// 对着假服务端验线格式：请求体形状、Bearer 头、SSE 解析、错误映射。
import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { Client, HermesError } from '../index.js';

let server;
let port;
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
        return res.end(JSON.stringify({ status: 'ok' }));
      }
      if (req.url === '/v1/models') {
        res.writeHead(200, { 'Content-Type': 'application/json' });
        return res.end(JSON.stringify({ data: [{ id: 'triage' }] }));
      }
      if (seen.body.stream) {
        res.writeHead(200, { 'Content-Type': 'text/event-stream' });
        for (const piece of ['你', '好']) {
          res.write('data: ' + JSON.stringify({ choices: [{ delta: { content: piece } }] }) + '\n\n');
        }
        res.write('data: [DONE]\n\n');
        return res.end();
      }
      if (seen.body.model === 'boom') {
        res.writeHead(503, { 'Content-Type': 'application/json' });
        return res.end(JSON.stringify({ error: { message: 'upstream failed', type: 'server_error' } }));
      }
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({
        model: seen.body.model,
        choices: [{ message: { role: 'assistant', content: 'ok' } }],
      }));
    });
  });
  await new Promise((r) => server.listen(0, '127.0.0.1', r));
  port = server.address().port;
});

after(() => server.close());

const client = (token) => new Client(`http://127.0.0.1:${port}`, { token });

test('health', async () => {
  assert.equal((await client().health()).status, 'ok');
});

test('models', async () => {
  assert.deepEqual(await client().models(), ['triage']);
});

test('run 发的是显式 input，不是 messages', async () => {
  const r = await client().run('triage', { title: 'x' });
  assert.equal(r.content, 'ok');
  assert.deepEqual(seen.body.input, { title: 'x' });
  assert.equal(seen.body.messages, undefined);
});

test('token 以 Bearer 发送', async () => {
  await client('s3cret').health();
  assert.equal(seen.auth, 'Bearer s3cret');
});

test('chatStream 逐个产出增量', async () => {
  const parts = [];
  for await (const p of client().chatStream([{ role: 'user', content: 'hi' }])) parts.push(p);
  assert.equal(parts.join(''), '你好');
});

test('错误带 status 与 type', async () => {
  await assert.rejects(
    () => client().run('boom'),
    (e) => e instanceof HermesError && e.status === 503 && e.type === 'server_error'
  );
});
