// Hermes VSCode 扩展：**零 npm 依赖**。
//
// 为什么自己实现 LSP 客户端而不引 vscode-languageclient：红线要求可闭源商用、
// 零付费、无传染性许可 —— 自己写这 300 行比背一个依赖树更容易审计。
// 传输层就是 `Content-Length` 头 + JSON，和语言服务器那边对称。
'use strict';

const vscode = require('vscode');
const cp = require('child_process');
const fs = require('fs');
const path = require('path');

let client = null;

function activate(context) {
  const output = vscode.window.createOutputChannel('Hermes');
  const collection = vscode.languages.createDiagnosticCollection('hermes');
  context.subscriptions.push(output, collection);

  const cfg = vscode.workspace.getConfiguration('hermes');
  const serverPath = findServer(cfg.get('serverPath'));
  if (!serverPath) {
    // **不静默降级**：没有服务器就没有诊断/补全，必须让用户知道原因与修法。
    vscode.window.showWarningMessage(
      '找不到 hermes 可执行文件：诊断、补全、悬停都不可用。' +
      '安装 Hermes 或设置 hermes.serverPath。', '打开设置'
    ).then((pick) => {
      if (pick === '打开设置') {
        vscode.commands.executeCommand('workbench.action.openSettings', 'hermes.serverPath');
      }
    });
    return;
  }

  client = new LspClient(serverPath, output, collection, cfg.get('trace.server') === 'messages');
  client.start();

  // 文档事件 → 转发给服务器。
  context.subscriptions.push(
    vscode.workspace.onDidOpenTextDocument((d) => client && client.open(d)),
    vscode.workspace.onDidChangeTextDocument((e) => client && client.change(e.document)),
    vscode.workspace.onDidCloseTextDocument((d) => client && client.close(d)),
    vscode.workspace.onDidSaveTextDocument((d) => client && client.open(d))
  );

  // 补全 / 悬停 / 大纲都走服务器，这里只做转接。
  context.subscriptions.push(
    vscode.languages.registerCompletionItemProvider({ language: 'hermes' }, {
      provideCompletionItems: (doc, pos) => client.completion(doc, pos)
    }, '.', '<', '"'),
    vscode.languages.registerHoverProvider({ language: 'hermes' }, {
      provideHover: (doc, pos) => client.hover(doc, pos)
    }),
    vscode.languages.registerDocumentSymbolProvider({ language: 'hermes' }, {
      provideDocumentSymbols: (doc) => client.documentSymbols(doc)
    }),
    // 语义着色：与 C++ 在 VSCode 里的体验对齐的关键一项。
    // 图例（legend）必须与服务器声明的**完全一致**，否则颜色会整体错位。
    vscode.languages.registerDocumentSemanticTokensProvider({ language: 'hermes' }, {
      provideDocumentSemanticTokens: (doc) => client.semanticTokens(doc)
    }, {
      tokenTypes: ['variable', 'function', 'parameter', 'type', 'property', 'keyword', 'number', 'string'],
      tokenModifiers: []
    })
  );

  // 已经打开的文件补一次 didOpen（扩展可能在文件打开后才激活）。
  for (const doc of vscode.workspace.textDocuments) {
    if (doc.languageId === 'hermes') client.open(doc);
  }
}

function deactivate() {
  if (client) client.stop();
  client = null;
}

// findServer 依次找：设置 → 扩展自带 server/ → PATH。
//
// 顺序是刻意的：**用户设置的优先**（他们可能要用自己构建的版本），
// 其次扩展自带（"装上就能用"），最后 PATH。
function findServer(configured) {
  if (configured && fs.existsSync(configured)) return configured;
  const bundled = path.join(__dirname, '..', 'server', process.platform === 'win32' ? 'hermes.exe' : 'hermes');
  if (fs.existsSync(bundled)) return bundled;
  const paths = (process.env.PATH || '').split(path.delimiter);
  for (const p of paths) {
    const cand = path.join(p, process.platform === 'win32' ? 'hermes.exe' : 'hermes');
    if (fs.existsSync(cand)) return cand;
  }
  return '';
}

// LspClient 是最小 LSP 客户端：分帧、请求、通知、诊断。
class LspClient {
  constructor(serverPath, output, collection, trace) {
    this.serverPath = serverPath;
    this.output = output;
    this.collection = collection;
    this.trace = trace;
    this.seq = 1;
    this.pending = new Map();
    this.buf = Buffer.alloc(0);
    this.proc = null;
  }

  start() {
    this.proc = cp.spawn(this.serverPath, ['lsp'], { stdio: ['pipe', 'pipe', 'pipe'] });
    this.proc.stdout.on('data', (chunk) => this.onData(chunk));
    // 输出通道在窗口关闭时会被销毁，之后再 appendLine 会抛 "Channel has been closed"。
    // 那不是程序错误，但会在扩展主机日志里留一条红色堆栈 —— 而红色堆栈会让人
    // 以为插件坏了。所以写日志一律走 safeLog。
    this.proc.stderr.on('data', (chunk) => this.safeLog(chunk.toString().trimEnd()));
    this.proc.on('exit', (code) => {
      this.safeLog(`hermes lsp 退出（code=${code}）`);
      this.collection.clear();
    });
    this.request('initialize', {
      processId: process.pid,
      rootUri: vscode.workspace.workspaceFolders && vscode.workspace.workspaceFolders.length
        ? vscode.workspace.workspaceFolders[0].uri.toString() : null,
      capabilities: { textDocument: { hover: {}, completion: {}, documentSymbol: {} } }
    }).then(() => this.notify('initialized', {}));
  }

  // safeLog 吞掉"通道已关闭"这类**窗口关闭时必然发生**的写失败。
  //
  // 为什么值得一个方法：这些失败在日志里是红色堆栈，而它们发生在用户关窗口的时候 ——
  // 用户会以为插件崩了，然后来报 bug。
  safeLog(text) {
    try {
      this.output.appendLine(text);
    } catch (e) {
      /* 通道已销毁，忽略 */
    }
  }

  stop() {
    if (!this.proc) return;
    try { this.notify('shutdown', null); this.notify('exit', null); } catch (e) { /* 进程可能已退出 */ }
    this.proc.kill();
    this.proc = null;
  }

  // onData 处理分帧。**必须按 Content-Length 读**，不能按行切 JSON：
  // 消息体里可以有换行，按行切会把一条消息切成两半。
  onData(chunk) {
    this.buf = Buffer.concat([this.buf, chunk]);
    for (;;) {
      const sep = this.buf.indexOf('\r\n\r\n');
      if (sep < 0) return;
      const head = this.buf.slice(0, sep).toString('ascii');
      const m = /Content-Length:\s*(\d+)/i.exec(head);
      if (!m) { this.buf = this.buf.slice(sep + 4); continue; }
      const len = parseInt(m[1], 10);
      const start = sep + 4;
      if (this.buf.length < start + len) return;
      const body = this.buf.slice(start, start + len).toString('utf8');
      this.buf = this.buf.slice(start + len);
      try { this.onMessage(JSON.parse(body)); } catch (e) { this.safeLog('解析失败: ' + e.message); }
    }
  }

  onMessage(msg) {
    if (this.trace) this.safeLog('← ' + JSON.stringify(msg));
    if (msg.id !== undefined && this.pending.has(msg.id)) {
      const { resolve, reject } = this.pending.get(msg.id);
      this.pending.delete(msg.id);
      msg.error ? reject(new Error(msg.error.message)) : resolve(msg.result);
      return;
    }
    if (msg.method === 'textDocument/publishDiagnostics') {
      this.publishDiagnostics(msg.params);
    }
  }

  publishDiagnostics(params) {
    const uri = vscode.Uri.parse(params.uri);
    const diags = (params.diagnostics || []).map((d) => {
      const r = new vscode.Range(
        d.range.start.line, d.range.start.character,
        d.range.end.line, d.range.end.character
      );
      // 消息里已经带了 why/fix（服务器拼好的），这里不再加工。
      const diag = new vscode.Diagnostic(r, d.message, d.severity || 1);
      diag.source = d.source || 'hermes';
      diag.code = d.code;
      return diag;
    });
    this.collection.set(uri, diags);
  }

  send(obj) {
    if (!this.proc || !this.proc.stdin.writable) return;
    const body = Buffer.from(JSON.stringify(obj), 'utf8');
    if (this.trace) this.safeLog('→ ' + body.toString());
    this.proc.stdin.write(`Content-Length: ${body.length}\r\n\r\n`);
    this.proc.stdin.write(body);
  }

  notify(method, params) {
    this.send({ jsonrpc: '2.0', method, params });
  }

  request(method, params) {
    const id = this.seq++;
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      this.send({ jsonrpc: '2.0', id, method, params });
      // 超时兜底：服务器卡住时不该让编辑器永远等下去。
      setTimeout(() => {
        if (this.pending.has(id)) {
          this.pending.delete(id);
          reject(new Error(`lsp 请求超时: ${method}`));
        }
      }, 5000);
    });
  }

  docId(doc) { return doc.uri.toString(); }

  open(doc) {
    if (doc.languageId !== 'hermes') return;
    this.notify('textDocument/didOpen', {
      textDocument: { uri: this.docId(doc), languageId: 'hermes', version: doc.version, text: doc.getText() }
    });
  }

  change(doc) {
    if (doc.languageId !== 'hermes') return;
    // textDocumentSync=Full：每次发全文。文件不大，换来"永远没有增量补丁算错"。
    this.notify('textDocument/didChange', {
      textDocument: { uri: this.docId(doc), version: doc.version },
      contentChanges: [{ text: doc.getText() }]
    });
  }

  close(doc) {
    if (doc.languageId !== 'hermes') return;
    this.notify('textDocument/didClose', { textDocument: { uri: this.docId(doc) } });
  }

  async completion(doc, pos) {
    try {
      const res = await this.request('textDocument/completion', {
        textDocument: { uri: this.docId(doc) },
        position: { line: pos.line, character: pos.character }
      });
      const items = (res && res.items) || [];
      return items.map((it) => {
        const kind = it.kind || 1;
        const ci = new vscode.CompletionItem(it.label, kind);
        if (it.detail) ci.detail = it.detail;
        if (it.documentation) ci.documentation = new vscode.MarkdownString(it.documentation);
        return ci;
      });
    } catch (e) {
      return [];
    }
  }

  async hover(doc, pos) {
    try {
      const res = await this.request('textDocument/hover', {
        textDocument: { uri: this.docId(doc) },
        position: { line: pos.line, character: pos.character }
      });
      if (!res || !res.contents) return undefined;
      const value = typeof res.contents === 'string' ? res.contents : res.contents.value;
      return new vscode.Hover(new vscode.MarkdownString(value));
    } catch (e) {
      return undefined;
    }
  }

  // semanticTokens 把服务器返回的增量数组原样交给 VSCode。
  //
  // 服务器已经按 LSP 要求做了增量编码（deltaLine/deltaStart/length/type/modifiers），
  // 客户端**不要再做任何换算** —— 换错一次，整份文件的颜色会全乱。
  async semanticTokens(doc) {
    try {
      const res = await this.request('textDocument/semanticTokens/full', {
        textDocument: { uri: this.docId(doc) }
      });
      const data = (res && res.data) || [];
      return new vscode.SemanticTokens(new Uint32Array(data));
    } catch (e) {
      return new vscode.SemanticTokens(new Uint32Array(0));
    }
  }

  async documentSymbols(doc) {
    try {
      const res = await this.request('textDocument/documentSymbol', {
        textDocument: { uri: this.docId(doc) }
      });
      const syms = (res || []).map((s) => new vscode.SymbolInformation(
        s.name, s.kind || 12, s.detail || '',
        new vscode.Location(doc.uri, new vscode.Position(0, 0))
      ));
      return syms;
    } catch (e) {
      return [];
    }
  }
}

module.exports = { activate, deactivate };
