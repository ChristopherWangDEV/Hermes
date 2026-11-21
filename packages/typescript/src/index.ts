/**
 * Hermes 运行时的 TypeScript 客户端。
 *
 * 与 JavaScript 版**同一套线格式**（`hermes serve` 的四个端点，OpenAI 同形），
 * 差别只在类型：把 `unknown` 换成具体形状，让调用方在编译期就知道
 * "input 是显式字段"而不是"塞进 messages"。
 *
 * 零运行时依赖。
 */

export { NativeRuntime, NativeError } from './native.js';

/** 服务端错误。status 与 type 都要保留：401 和 503 的处置完全不同。 */
export class HermesError extends Error {
  readonly type: string;
  readonly status: number;

  constructor(message: string, opts: { type?: string; status?: number } = {}) {
    super(message);
    this.name = 'HermesError';
    this.type = opts.type ?? '';
    this.status = opts.status ?? 0;
  }
}

export interface ClientOptions {
  token?: string;
  timeoutMs?: number;
}

export interface Health {
  status: string;
  workflows: number;
}

export interface ModelInfo {
  id: string;
  object: string;
  owned_by: string;
}

export interface ChatMessage {
  role: 'system' | 'user' | 'assistant';
  content: string;
}

export interface ChatResult {
  content: string;
  model: string;
  raw: unknown;
}

/** workflow 名就是 model 名；空串表示"用 entry workflow"。 */
export const DEFAULT_MODEL = '';

export class Client {
  private readonly baseUrl: string;
  private readonly token: string;
  private readonly timeoutMs: number;

  constructor(baseUrl = 'http://127.0.0.1:8080', opts: ClientOptions = {}) {
    this.baseUrl = baseUrl.replace(/\/$/, '');
    this.token = opts.token ?? '';
    this.timeoutMs = opts.timeoutMs ?? 60_000;
  }

  private headers(): Record<string, string> {
    const h: Record<string, string> = { 'Content-Type': 'application/json' };
    if (this.token) h['Authorization'] = `Bearer ${this.token}`;
    return h;
  }

  private async request<T>(method: string, path: string, body?: unknown): Promise<T> {
    const ctl = new AbortController();
    const timer = setTimeout(() => ctl.abort(), this.timeoutMs);
    try {
      const res = await fetch(this.baseUrl + path, {
        method,
        headers: this.headers(),
        ...(body === undefined ? {} : { body: JSON.stringify(body) }),
        signal: ctl.signal,
      });
      const text = await res.text();
      const json = text ? (JSON.parse(text) as Record<string, unknown>) : {};
      if (!res.ok) {
        const err = (json['error'] as { message?: string; type?: string } | undefined) ?? {};
        throw new HermesError(err.message ?? `HTTP ${res.status}`, {
          type: err.type ?? '',
          status: res.status,
        });
      }
      return json as T;
    } finally {
      clearTimeout(timer);
    }
  }

  health(): Promise<Health> {
    return this.request<Health>('GET', '/health');
  }

  async models(): Promise<string[]> {
    const data = await this.request<{ data?: ModelInfo[] }>('GET', '/v1/models');
    return (data.data ?? []).map((m) => m.id);
  }

  /**
   * 执行一个 workflow。
   *
   * `input` 是**显式**的（非 OpenAI 标准）：输入契约由 `.h` 里的 struct 定义，
   * 映射到 messages 只能靠猜。省略表示不需要输入。
   */
  async run(
    workflow: string,
    input?: Record<string, unknown>,
  ): Promise<ChatResult> {
    const body: Record<string, unknown> = { model: workflow };
    if (input !== undefined) body['input'] = input;
    return this.chatLike(body, workflow);
  }

  /** 按 OpenAI 的 messages 形状调用（服务端取最后一条 user 消息当 text）。 */
  chat(messages: ChatMessage[], model: string = DEFAULT_MODEL): Promise<ChatResult> {
    return this.chatLike({ model, messages }, model);
  }

  private async chatLike(body: Record<string, unknown>, model: string): Promise<ChatResult> {
    const raw = await this.request<{
      model?: string;
      choices?: { message?: { content?: string } }[];
    }>('POST', '/v1/chat/completions', body);
    const content = raw.choices?.[0]?.message?.content ?? '';
    return { content, model: raw.model ?? model, raw };
  }

  /** 流式调用：逐个产出文本增量（真流式，来自 provider 层的上游 SSE）。 */
  async *chatStream(
    messages: ChatMessage[],
    model: string = DEFAULT_MODEL,
  ): AsyncGenerator<string, void, void> {
    const ctl = new AbortController();
    const timer = setTimeout(() => ctl.abort(), this.timeoutMs);
    try {
      const res = await fetch(this.baseUrl + '/v1/chat/completions', {
        method: 'POST',
        headers: this.headers(),
        body: JSON.stringify({ model, messages, stream: true }),
        signal: ctl.signal,
      });
      if (!res.ok || !res.body) {
        throw new HermesError(`HTTP ${res.status}`, { status: res.status });
      }
      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      let buf = '';
      for (;;) {
        const { value, done } = await reader.read();
        if (done) return;
        buf += decoder.decode(value, { stream: true });
        let idx: number;
        while ((idx = buf.indexOf('\n')) >= 0) {
          const line = buf.slice(0, idx).trimEnd();
          buf = buf.slice(idx + 1);
          if (!line.startsWith('data:')) continue;
          const payload = line.slice(5).trim();
          if (payload === '[DONE]') return;
          const chunk = JSON.parse(payload) as {
            error?: { message?: string; type?: string };
            choices?: { delta?: { content?: string } }[];
          };
          if (chunk.error) {
            throw new HermesError(chunk.error.message ?? 'stream error', {
              type: chunk.error.type ?? '',
            });
          }
          for (const ch of chunk.choices ?? []) {
            const text = ch.delta?.content;
            if (text) yield text;
          }
        }
      }
    } finally {
      clearTimeout(timer);
    }
  }
}

export default Client;
