# Hermes client SDKs

Eight languages, **two ways to call**, one C ABI (`hermes_rt_*`):

1. **In-process binding** (like PyQt wrapping Qt) — load `libhermesrt` and run `.hbc` directly.
   No server, no network, no serialization round trip.
2. **HTTP client** — talk to `hermes serve`. For cross-machine, multi-tenant, authenticated use.

| Language | In-process binding | HTTP | How it binds |
|---|---|---|---|
| Python | `Runtime` | `Client` | `ctypes` (stdlib) |
| JavaScript | `NativeRuntime` | `Client` | N-API addon (Node has no built-in FFI) |
| TypeScript | `NativeRuntime` | `Client` | same addon, typed |
| Go | `NativeRuntime` | `Client` | `dlopen` via cgo |
| Rust | `NativeRuntime` | `Client` | `extern "C"` + `dlopen`, no crates |
| Java | `NativeRuntime` | `HermesClient` | FFM API (JDK 22+) |
| C# | `NativeRuntime` | `Client` | P/Invoke (BCL) |
| Kotlin | `NativeRuntime` | `Client` | FFM API (same as Java) |

## Zero third-party dependencies — enforced, not promised

A dependency in an SDK becomes a dependency in **your** build, your lock file and your license
audit. So every SDK uses only its language's standard library:

| Language | Allowed | Manifest |
|---|---|---|
| Python | stdlib (`ctypes`, `urllib`, `json`) | `dependencies = []` |
| JavaScript / TypeScript | `node:` builtins | no `dependencies`; `typescript` is dev-only |
| Go | stdlib | no `require` in `go.mod` |
| Rust | std (`dlopen` declared via `extern "C"`) | empty `[dependencies]` |
| Java / Kotlin | JDK (`java.net.http`, `java.lang.foreign`) | no Maven/Gradle file at all |
| C# | BCL | no `PackageReference` |

`tests/guard/sdk_redline_test.go` checks all of this mechanically — including that no manifest
starts requiring something, and that no README tells you to install anything.

## Shared contract (both usages)

- `run(input)` takes an **explicit input object**; it is never squeezed into `messages`.
  A workflow's input contract is the struct in `.h`, and mapping it to messages would be guessing.
- Errors carry **code/status + type**: "file not found" (code 2) and "execution failed" (code 3),
  or 401 vs 503, need different handling.
- Memory allocated by the native layer is freed with `hermes_rt_free` — the host GC does not know
  about it.
- Streaming is **real streaming** (the server forwards upstream SSE), not a pre-computed result
  chopped into chunks.

## Talking to other model servers

The SDKs talk to Hermes; Hermes talks to model servers. `backend` kinds cover
`openai` / `anthropic` / `ollama` / `homonoia` / `custom`, where `custom` is **any
OpenAI-compatible endpoint** — vLLM, LM Studio, OpenRouter, LiteLLM, and most vendors.
See the "Backends" section of the main README.

## Verifying

```sh
bash scripts/test_sdks.sh      # runs whatever toolchains are present, lists what it skipped
```

A skipped language is always **named**, never silently counted as passing.
