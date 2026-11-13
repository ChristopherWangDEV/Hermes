// Hermes 运行时的 JavaScript 客户端。
//
// 它说 `hermes serve` 的 HTTP 面（与 OpenAI 同形，D-021），所以不需要安装 hermes、
// 不需要 FFI —— 只要能发 HTTP。
//
// 零依赖：只用内建 `fetch`（Node ≥18）与 `ReadableStream`。SDK 引第三方库，
// 就等于把那棵依赖树带进每个使用者的构建里。

/**
 * 服务端错误。保留 type 与 status：调用方要能区分"没带凭证（401）"和
 * "上游/预算出了问题（503）"——两者的处置完全不同。
 */
export class HermesError extends Error {
  constructor(message, { type = '', status = 0 } = {}) {
    super(message);
    this.name = 'HermesError';
    this.type = type;
    this.status = status;
  }
}

/** workflow 名就是 model 名；空串表示"用 entry workflow"。 */
export const DEFAULT_MODEL = '';

export class Client {
  /**
   * @param {string} baseUrl 指向 `hermes serve --addr`
   * @param {{token?: string, timeoutMs?: number}} [opts]
   */
  constructor(baseUrl = 'http://127.0.0.1:8080', { token = '', timeoutMs = 60000 } = {}) {
    this.baseUrl = baseUrl.replace(/\/$/, '');
    this.token = token;
    this.timeoutMs = timeoutMs;
  }

  headers() {
    const h = { 'Content-Type': 'application/json' };
    if (this.token) h.Authorization = `Bearer ${this.token}`;
    return h;
  }

  async #request(method, path, body) {
    const ctl = new AbortController();
    const timer = setTimeout(() => ctl.abort(), this.timeoutMs);
    try {
      const res = await fetch(this.baseUrl + path, {
        method,
        headers: this.headers(),
        body: body === undefined ? undefined : JSON.stringify(body),
        signal: ctl.signal,
      });
      const text = await res.text();
      const json = text ? JSON.parse(text) : {};
      if (!res.ok) {
        const err = json.error || {};
        throw new HermesError(err.message || `HTTP ${res.status}`, {
          type: err.type || '',
          status: res.status,
        });
      }
      return json;
    } finally {
      clearTimeout(timer);
    }
  }

  /** 存活与能力。 */
  health() {
    return this.#request('GET', '/health');
  }

  /** 可用的 workflow 名（服务端只列 exported 的）。 */
  async models() {
    const data = await this.#request('GET', '/v1/models');
    return (data.data || []).map((m) => m.id);
  }

  /**
   * 执行一个 workflow。
   *
   * `input` 是**显式**的（非 OpenAI 标准）：workflow 的输入契约由 `.h` 里的 struct
   * 定义，把它映射到 messages 只能靠猜。传 `undefined` 表示不需要输入。
   */
  async run(workflow, input) {
    const body = { model: workflow };
    if (input !== undefined) body.input = input;
    const raw = await this.#request('POST', '/v1/chat/completions', body);
    const content = raw.choices?.[0]?.message?.content ?? '';
    return { content, model: raw.model ?? workflow, raw };
  }

  /**
   * 按 OpenAI 的 messages 形状调用。
   *
   * 服务端会把**最后一条 user 消息**当成 `text` 传给 workflow —— 只在没有显式
   * `input` 时发生，所以适合"对话式"的 workflow。
   */
  chat(messages, model = DEFAULT_MODEL) {
    return this.run0({ model, messages }, model);
  }

  async run0(body, model) {
    const raw = await this.#request('POST', '/v1/chat/completions', body);
    const content = raw.choices?.[0]?.message?.content ?? '';
    return { content, model: raw.model ?? model, raw };
  }

  /**
   * 流式调用：逐个产出文本增量（async iterator）。
   *
   * 服务端是真流式（provider 层接上游 SSE），第一个块通常最快 —— 那正是流式的意义。
   */
  async *chatStream(messages, model = DEFAULT_MODEL) {
    const ctl = new AbortController();
    const timer = setTimeout(() => ctl.abort(), this.timeoutMs);
    try {
      const res = await fetch(this.baseUrl + '/v1/chat/completions', {
        method: 'POST',
        headers: this.headers(),
        body: JSON.stringify({ model, messages, stream: true }),
        signal: ctl.signal,
      });
      if (!res.ok) {
        const text = await res.text();
        let err = {};
        try { err = JSON.parse(text).error || {}; } catch { /* 非 JSON 错误页 */ }
        throw new HermesError(err.message || `HTTP ${res.status}`, {
          type: err.type || '',
          status: res.status,
        });
      }
      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      let buf = '';
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        buf += decoder.decode(value, { stream: true });
        let idx;
        while ((idx = buf.indexOf('\n')) >= 0) {
          const line = buf.slice(0, idx).trimEnd();
          buf = buf.slice(idx + 1);
          if (!line.startsWith('data:')) continue;
          const payload = line.slice(5).trim();
          if (payload === '[DONE]') return;
          const chunk = JSON.parse(payload);
          if (chunk.error) {
            throw new HermesError(chunk.error.message || 'stream error', {
              type: chunk.error.type || '',
            });
          }
          for (const ch of chunk.choices || []) {
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
