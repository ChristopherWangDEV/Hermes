# @hermes/runtime (JavaScript)

Both usages, one C ABI. Zero dependencies.

## In-process (like PyQt wrapping Qt)

```js
import { NativeRuntime } from '@hermes/runtime';

const rt = new NativeRuntime('app.hbc');
console.log(rt.run({ title: 'printer is broken' }));   // no server, no network
```

Node has no built-in FFI, so this needs the N-API addon built by
`scripts/build_node_addon.sh` (a single clang command against Node's own headers).

## HTTP

```js
import { Client } from '@hermes/runtime';

const c = new Client('http://127.0.0.1:8080', { token: '…' });
console.log(await c.models());
console.log((await c.run('triage', { title: 'x' })).content);

for await (const piece of c.chatStream([{ role: 'user', content: 'hi' }])) {
  process.stdout.write(piece);
}
```
