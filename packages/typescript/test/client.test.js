// 编译产物（dist/）的线格式测试：与 JS 版同一套断言，验的是"类型编译后行为不变"。
import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { Client, HermesError } from '../dist/index.js';

let server, port;
const seen = {};

before(async () => {
  server = http.createServer((req, res) => {
    seen.auth = req.headers.authorization;
    let body = '';
    req.on('data', (c) => (body += c));
    req.on('end', () => {
      seen.body = body ? JSON.parse(body) : {};
      if (req.url === '/health') {
        res.writeHead(200, { 'Content-Type': 'application/json' });
        return res.end(JSON.stringify({ status: 'ok', workflows: 1 }));
      }
      if (req.url === '/v1/models') {
        res.writeHead(200, { 'Content-Type': 'application/json' });
        return res.end(JSON.stringify({ data: [{ id: 'triage', object: 'model', owned_by: 'hermes' }] }));
      }
      if (seen.body.stream) {
        res.writeHead(200, { 'Content-Type': 'text/event-stream' });
        for (const p of ['你', '好']) {
          res.write('data: ' + JSON.stringify({ choices: [{ delta: { content: p } }] }) + '\n\n');
        }
        res.write('data: [DONE]\n\n');
        return res.end();
      }
      if (seen.body.model === 'boom') {
        res.writeHead(503, { 'Content-Type': 'application/json' });
        return res.end(JSON.stringify({ error: { message: 'upstream failed', type: 'server_error' } }));
      }
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ model: seen.body.model, choices: [{ message: { content: 'ok' } }] }));
    });
  });
  await new Promise((r) => server.listen(0, '127.0.0.1', r));
  port = server.address().port;
});

after(() => server.close());

const client = (token) => new Client(`http://127.0.0.1:${port}`, token ? { token } : {});

test('health / models', async () => {
  assert.equal((await client().health()).status, 'ok');
  assert.deepEqual(await client().models(), ['triage']);
});

test('run 用显式 input', async () => {
  await client().run('triage', { title: 'x' });
  assert.deepEqual(seen.body.input, { title: 'x' });
  assert.equal(seen.body.messages, undefined);
});

test('Bearer 头', async () => {
  await client('s3cret').health();
  assert.equal(seen.auth, 'Bearer s3cret');
});

test('流式增量', async () => {
  const parts = [];
  for await (const p of client().chatStream([{ role: 'user', content: 'hi' }])) parts.push(p);
  assert.equal(parts.join(''), '你好');
});

test('错误带 status/type', async () => {
  await assert.rejects(() => client().run('boom'),
    (e) => e instanceof HermesError && e.status === 503 && e.type === 'server_error');
});

// 进程内绑定：与 HTTP 客户端共存，但语义不同（不走网络）。
import { NativeRuntime } from '../dist/index.js';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, writeFileSync, existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

test('进程内执行 workflow', { skip: !existsSync(join(import.meta.dirname, '..', '..', '..', 'build', 'hermes')) }, () => {
  const root = join(import.meta.dirname, '..', '..', '..');
  const hermes = join(root, 'build', 'hermes');
  const dir = mkdtempSync(join(tmpdir(), 'hermes-ts-'));
  const src = join(dir, 'echo.hermes');
  writeFileSync(src,
    'export struct In { string text; }\n' +
    'export workflow echo(input: In) entry -> string { return "echo: {input.text}"; }\n');
  const hbc = join(dir, 'echo.hbc');
  execFileSync(hermes, ['build', src, '-o', hbc]);

  const rt = new NativeRuntime(hbc);
  assert.equal(rt.run({ text: 'hi' }), 'echo: hi');
});
