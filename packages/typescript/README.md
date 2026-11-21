# @hermes/runtime-ts (TypeScript)

Both usages, one C ABI. Zero runtime dependencies.

## In-process (like PyQt wrapping Qt)

```ts
import { NativeRuntime } from '@hermes/runtime-ts';

const rt = new NativeRuntime('app.hbc');
console.log(rt.run({ title: 'printer is broken' }));   // no server, no network
```

Needs the N-API addon (`scripts/build_node_addon.sh`) and `libhermesrt` — Node has no built-in FFI.

## HTTP

```ts
import { Client } from '@hermes/runtime-ts';

const c = new Client('http://127.0.0.1:8080', { token: '…' });
console.log(await c.models());
console.log((await c.run('triage', { title: 'x' })).content);

for await (const piece of c.chatStream([{ role: 'user', content: 'hi' }])) {
  process.stdout.write(piece);
}
```
