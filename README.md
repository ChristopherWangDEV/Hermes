<div align="center">

# Hermes

**A compiled agent orchestration runtime** · **编译型 Agent 编排运行时**

[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![Third-party Go deps](https://img.shields.io/badge/third--party%20Go%20deps-0-brightgreen.svg)](NOTICE)
[![Toolchain](https://img.shields.io/badge/toolchain-clang%20only-orange.svg)](NOTICE)
[![CI](https://github.com/ChristopherWangDEV/Hermes/actions/workflows/ci.yml/badge.svg)](https://github.com/ChristopherWangDEV/Hermes/actions/workflows/ci.yml)
[![LTS](https://img.shields.io/badge/release-LTS%20%E9%95%BF%E6%9C%9F%E6%94%AF%E6%8C%81-2ea043.svg)](#长期支持lts)

[English](#english) · [简体中文](#简体中文)

</div>

---

<a id="english"></a>

# English

**A compiled agent orchestration runtime.** Describe a workflow in `.hermes`, compile it to IR,
then run it or compile it into a native executable — offline, no runtime to install, single binary.

```sh
hermes demo-runaway            # a bounded agent stops; a runtime-counting one does not
hermes demo                    # the whole chain in one command
hermes hello.hermes -o hello   # compile like clang does
./hello
```

---

## The problem

An agent that *looks* like it is making progress can keep calling tools forever. Runtime-counting
frameworks tell you after the bill arrives. Hermes fixes the bound **at compile time**.

```text
# runtime counting — you find out when it runs away
while (true) { const r = await agent.step(); }        // when does it stop?

# Hermes — the bound is a constant, and the compiler knows it
auto answer = model.agent(goal = "…", tools = ["lookup_order"], max_steps = 4);
```

`max_parallel_tools` (default 1) bounds how many tool calls one turn may contain, because models
do issue several in parallel. Bounds follow from both constants: `max_model_calls = max_steps`,
`max_tool_calls = max_parallel_tools × max_steps`, `max_spans = (1 + max_parallel_tools) × max_steps`.

Because they are constants, money is reserved before the request goes out —
so money is *reserved* before the request goes out, not reconciled after it lands.

```console
$ hermes demo-runaway
  An agent that will not stop on its own

    Every turn the model asks for the same tool: it thinks it is making progress,
    while it is going in circles. Runtime-counting frameworks keep going and you learn
    the cost afterwards; here max_steps is a compile-time constant.

    max_steps = 4 (a compile-time constant, not a setting)

    step 1  model call -> asks for lookup_order
    step 2  model call -> asks for lookup_order
    step 3  model call -> asks for lookup_order
    step 4  model call -> asks for lookup_order

    → ContractError
      the agent still asked for tools at max_steps=4 — the bound is a promise, not a starting line

    4 model calls, 4 tool calls — all within the bound

    It stopped — right after paying for step 4, not after reconciling a bill.
```

That is the whole pitch: **4, not "until you kill it"**.

## Table of contents

- [What it is](#what-it-is)
- [Quick start](#quick-start)
- [Install](#install)
- [Language tour](#language-tour)
- [CLI reference](#cli-reference)
- [Compile to a native binary](#compile-to-a-native-binary)
- [Embed in C / C++](#embed-in-c--c)
- [Serve over HTTP](#serve-over-http)
- [Sign and verify artifacts](#sign-and-verify-artifacts)
- [Editor support](#editor-support)
- [Client SDKs](#client-sdks)
- [Architecture](#architecture)
- [Status and limits](#status-and-limits)
- [License and compliance](#license-and-compliance)
- [Development](#development)
- [Documentation](#documentation)

---

## What it is

Not another agent framework. The difference is **how much is decided at compile time**:

| Question | Answered by | When |
|---|---|---|
| What is the input/output shape? | structs in `.h` | compile time |
| Worst-case steps, model calls, tool calls? | `bounds` | compile time |
| Can parallel branches race? | type checker | compile time |
| Who built this artifact, was it modified? | signature + cross-check | distribution time |
| Which step failed, how much did it cost? | trace hash chain | run time |

Anything that *can* be settled at compile time is not guessed at run time. That is why the
artifact can be small: a `.hbc` archive, or a standalone executable.

Three hard constraints run through the whole project:

- **Zero third-party Go dependencies** (`go list -m all` prints one line: `hermes`)
- **clang / libc++ by default** — the only exception is `HERMES_ALLOW_LIBSTDCXX=1`, an explicit opt-in recorded as `gpl_tools_detected=true` in `toolchain.json` and rejected by the release gate
- **No copyleft** — the third-party code Hermes links is MIT / Apache-2.0 / BSD (no copyleft), and Hermes itself is Apache-2.0; closed-source commercial use is permitted

---

## Quick start

```sh
hermes demo                                  # compile → execute → native binary
printf 'export workflow hi() entry -> string { return "hi"; }\n' > hi.hermes
hermes hi.hermes -o hi && ./hi               # clang-style compile
hermes doctor                                # environment check: what is missing, and how to fix it
```

`hermes demo` embeds its source in the binary, so it works from a release package with no
repository and no network.

---

## Install

### From a release package (recommended)

```sh
# macOS arm64 — either the installer or the portable tarball
sudo installer -pkg hermes-2.0.0-macos-arm64.pkg -target /    # installs to /usr/local
# or: ./scripts/install.sh hermes-2.0.0-darwin-arm64.tar.gz    # verifies SHA256, runs doctor

# Linux x86_64 — either the installer or the portable tarball
sudo dpkg -i hermes-2.0.0-linux-amd64.deb                      # installs to /usr/local
# or: ./scripts/install.sh hermes-2.0.0-linux-x86_64.tar.gz

# portable tarball only: put it on PATH (we never edit your shell config)
export PATH="$HOME/.local/hermes/bin:$PATH"
```

Windows: run `hermes-2.0.0-windows-x64.msi` (installs to `Program Files`, adds `PATH`), or
unpack the zip and run `install.ps1` for a per-user install with no admin rights.

The package is self-contained — **no Go, no Rust, no cargo**. What each platform ships:

| Path | macOS | Linux | Windows |
|---|:--:|:--:|:--:|
| `bin/hermes` (`hermes.exe` on Windows) | ✅ | ✅ | ✅ |
| `bin/hermes-shim` — sandbox child process | ✅ | ✅ | — |
| `lib/libhermesrt.a` — runtime for AOT and `hermes cc` | ✅ | ✅ | — |
| `lib/hermes_rt.h` — its C header | ✅ | ✅ | — |
| `lib/libhermesrt.dylib` / `.so` — runtime for in-process bindings (ctypes / FFM / P/Invoke / dlopen) | ✅ | ✅ | — |
| `lib/cmake/hermes/` — CMake package config (`find_package(hermes)`) | ✅ | ✅ | — |
| `include/hermes/hermes_core.h` — handwritten, frozen public C ABI header | ✅ | ✅ | ✅ |
| `include/hermes/hermes.h` — older alias header kept for source compatibility | ✅ | ✅ | ✅ |
| `SHA256SUMS` — checksums for every file in the package | ✅ | ✅ | ✅ |
| `licenses/` — third-party license texts (must travel with the binary) | ✅ | ✅ | ✅ |

Windows ships the CLI only: Go's cgo probe needs a relocatable link (`-r`), which `lld-link`
does not support, and the alternatives are outside this project's red line. See
[`docs/CHANGELOG.md`](docs/CHANGELOG.md).

The standard library is `go:embed`-ed into the binary, so the package does not ship a second
copy — one less "version does not match the binary" failure mode.

`hermes compile` / `hermes cc` need a local **clang** (it turns generated C into machine code).
Everything else works without it; `hermes doctor` says exactly what is missing.

### From source

```sh
# macOS / Linux
bash scripts/build.sh     # needs Go ≥1.23, clang, cargo
bash scripts/check.sh     # full acceptance
bash scripts/release.sh   # build the release package

# Windows (ordinary PowerShell, no admin needed)
powershell -File scripts\build.ps1
.\build\hermes.exe doctor
```

---

## Language tour

Every example below compiles; a guard test extracts the code blocks from this README and
compiles each one, so none of them can rot.

### 1. Hello

```hermes
export workflow hello() entry -> string {
    return "hello";
}
```

`entry` marks the workflow that `hermes run` and `hermes serve` pick by default.

### 2. Types

```hermes
export struct Ticket {
    string title;
    int priority;
}

export workflow label(input: Ticket) entry -> string {
    return "{input.title} #{input.priority}";
}
```

Six base types: `string` `int` `double` `bool` `json` `cost`. The only floating type is `double` (IEEE-754 binary64); there is no 32-bit `float`. Double literals (`1.5` / `1e3` / `1.5e2`) are allowed and compile to the IR bit pattern `{"k":"double","bits":"<16 lowercase hex>"}` — IR bytes never carry a JSON floating-point number, so V-7 / canonical / the signing chain are unaffected. `double` supports `+ - * /` and `< <= > >=`; `%` at the source level is restricted by `E4009` to `int` operands (IR / Go engine / C backend implement double remainder, but that is a value-layer capability, not a language feature); still rejected are `double == double` (`E4033`), implicit `int`/`cost` -> `double` mixing (`E4006` / `E4008`), and `tool.call<double>` (a tool's `T` must be a JSON structured type). `call_native` may return `double` (verified by a real compile-and-run test). `json.as_float(j)` deliberately keeps its name but now narrows to `double`; `cost` stays `int64` micro-USD, deliberately off the floating path: money and shortest-round-trip decimals are exactly where languages disagree.

### 3. Branching


`match` arms take string **or integer** literals. Integer arms exist so status codes and
enums can be matched directly, instead of turning them into strings first.

```hermes
export struct Ticket {
    int priority;
}

export workflow triage(input: Ticket) entry -> string {
    match (input.priority) {
        case 200: { return "ok"; }
        case 404: { return "not found"; }
        default:  { return "error"; }
    }
}

```


```hermes
#link <std/model>

export struct Ticket {
    string priority;
}

export workflow triage(input: Ticket) entry -> string {
    match (input.priority) {
        case "urgent": { return "now"; }
        case "high": { return "today"; }
        default: { return "later"; }
    }
}
```

`match` arms take string literals; `default` is mandatory.

### 3b. Arrays

```hermes
export workflow sizes() entry -> json {
    auto xs = [1, 2, 3];
    auto n = len(xs);
    return xs;
}
```

Arrays are **fixed-length values**: the literal's element count is the length, so `len()` is
known at compile time. Indexing is checked the same way — `xs[5]` on a three-element literal is
a **compile error**, not a runtime surprise:

```hermes
export workflow pick() entry -> int {
    auto xs = [10, 20, 30];
    return xs[1];
}
```

`xs[5]` on that three-element literal is a compile error (`数组下标越界：长度 3，下标 5`).

### 4. Calling a model

```hermes
#link <std/model>

export struct Intent {
    string action;
}

export backend gateway : openai = env("GATEWAY_URL", "http://127.0.0.1:8080");

export workflow classify(backend = gateway) entry -> string {
    auto i = model.generate<Intent>(prompt = "classify this ticket", precision = "exact");
    return i.action;
}
```

`<Intent>` is the **contract**: the schema is generated from the struct, and the model's answer
is validated against it **before** the workflow continues. An invalid answer fails with
`ModelError` and a field path, not a mystery `null` three steps later.

Addresses come from `env(...)` only — a library that hard-codes an address cannot be distributed.

### 5. Calling a tool

```hermes
#link <std/net>

export struct OrderInfo {
    string order_id;
    int status;
    int amount;
}

export workflow fetch() entry -> string {
    auto r = tool.call<OrderInfo>(
        url = "http://127.0.0.1:8080/orders/A1",
        method = "GET",
        allow_status = [404, 409]
    );
    if (r.status == 404) {
        return "no such order";
    }
    return "amount {r.body.amount}";
}
```

`allow_status` turns those status codes into **normal returns** shaped
`{status, body, headers}`. Without it, "order does not exist" would be an exception, and that
branch would be impossible to write — which is the whole reason the argument exists.

### 6. Retry

```hermes
export workflow flaky() entry -> int {
    retry (3) {
        return 42;
    }
}
```

Retry applies only to retryable errors. A contract violation retried three times is still wrong,
so it is not retried.

### 7. Errors

```hermes
#link <std/net>

export struct Order {
    string id;
}

export workflow fetch() entry -> string {
    try {
        auto o = tool.call<Order>(url = "http://127.0.0.1:9/o", method = "GET");
        return "ok";
    } catch (HttpError e) {
        return "upstream said no";
    }
}
```

Six error types: `NetworkError` `HttpError` `ModelError` `ContractError` `CircuitOpenError`
`Error`. Only language-level errors are catchable; programming mistakes (unbound variable,
bad node index) crash through on purpose.

### 8. Parallel

```hermes
#link <std/net>

export struct Order {
    string id;
}

export struct History {
    string last;
}

export workflow both() entry -> string {
    auto [o, h] = parallel {
        auto x = tool.call<Order>(url = "http://127.0.0.1:9/o", method = "GET");
        auto y = tool.call<History>(url = "http://127.0.0.1:9/h", method = "GET");
        return [x, y];
    };
    return "{o.id}/{h.last}";
}
```

Branches run **concurrently** and share no writable state. The `return [x, y]` is the join
clause — you must say explicitly how many values come out.

### 9. Narrowing JSON

```hermes
export workflow pick(input: json) entry -> int {
    return json.as_int(input.total);
}
```

`json.as_*` is the **only** place a runtime type error can enter the program. Failure raises
`ContractError` with `expect`/`got`; it never returns a zero value, because "amount failed to
parse" silently becoming `0` is how you ship a wrong refund.

### 10. What the compiler told you

```hermes
export workflow cheap(input: int) entry -> int {
    if (input > 10) { return input * 2; }
    return input + 1;
}
```

```console
$ hermes explain cheap.hbc
cheap  [entry exported unit=cheap]
  returns            int
  bounds（upper bounds, not expectations）:
    max_steps        4
    max_spans        0
    max_concurrency  1
    max_model_calls  0
    max_tool_calls   0
```

`bounds` drives stripping, trace limits and budget reservation. A wrong bound is a correctness
bug, not a cosmetic one — so a guard test runs every golden workflow and asserts the real
counts stay within the declared bounds.

### 11. Backends: what Hermes can talk to

`backend` has five kinds. Four of them are protocol faces, one is an escape hatch:

| kind | speaks | covers |
|---|---|---|
| `openai` | OpenAI Chat Completions | OpenAI, Azure OpenAI, DeepSeek, Moonshot, Qwen, OpenRouter, LiteLLM, vLLM, LM Studio, Together… |
| `anthropic` | Anthropic Messages | Claude |
| `ollama` | Ollama native API | local Ollama |
| `custom` | **any** OpenAI-compatible server | your own gateway, a proxy, anything above with a non-standard base URL |

Because `custom` and `openai` share one protocol face, "supporting another framework"
in practice means **that framework exposes an OpenAI-compatible endpoint** — which today is
almost all of them. Hermes deliberately does **not** implement other frameworks' orchestration
formats: importing LangChain chains or LangGraph graphs would create a second source of truth
that drifts .

What Hermes replaces is the **orchestration layer**, not the integration catalogue:

| | LangChain-style | Hermes |
|---|---|---|
| What you write | Python/JS code calling a framework | `.hermes` source, compiled |
| When mistakes surface | at run time, in production | at compile time (types, bounds, branch analysis) |
| Deployment | Python env + dependency tree | one binary, or a `.hbc` + C ABI |
| Debugging | logs you added yourself | a verifiable trace with a hash chain |
| API stability | framework-internal abstractions change | IR v1.4 is versioned and validated on load |

What Hermes does **not** give you: document loaders, an agent-tool marketplace, or a
managed vector database. It does ship **memory** (key/value) and **vector search**
(embeddings + cosine) as builtins — small enough to stay dependency-free, replaceable by
your own store if you need one.


### 12. An agent that picks its own tools

```hermes
#link <std/model>
#link <std/net>

export struct Order {
    string id;
}

export tool orders {
    description = "look up an order by id";
    url = "http://127.0.0.1:8080/orders/{id}";
    method = "GET";
}

export backend gw : openai = env("GATEWAY_URL", "http://127.0.0.1:8080");

export workflow support(input: Order, backend = gw) entry -> string {
    auto answer = model.agent(
        goal = "check the status of order {input.id}",
        tools = ["orders"],
        max_steps = 4
    );
    return answer;
}
```

`max_steps` is a **constant**, and that is the whole point: the model decides *which*
tool to call and how many rounds to take, but the loop bound is fixed at compile time.
So `max_model_calls = max_tool_calls = max_steps`, and the budget is still a reservation
rather than an after-the-fact statistic.

Two rules keep this from becoming the thing it replaces:
- The tool schema the model sees and the URL the runtime calls come from **one**
  `export tool` declaration — never two copies to keep in sync.
- Hitting `max_steps` while the model still wants tools is an **error**, not "try again".


---

### 13. Memory and vectors

```hermes
export struct Turn {
    string user;
    string text;
}

export workflow chat(input: Turn) entry -> string {
    auto prev = memory.get(key = "user:{input.user}");
    memory.put(key = "user:{input.user}", value = input.text);
    return input.text;
}
```

`memory.get` returns `null` when the key is absent — **missing is not an error**; the first
conversation always has no history, and making that an exception would push the normal path
into `try/catch`.

Vectors live next to memory, and use an OpenAI-compatible `/v1/embeddings` endpoint:

```hermes
#link <std/model>

export struct Doc {
    string id;
    string body;
}

export backend embedder : openai = env("EMBED_URL", "http://127.0.0.1:8080");

export workflow index(input: Doc, backend = embedder) entry -> json {
    vector.put(id = "{input.id}", text = input.body);
    return vector.query(text = input.body, k = 3);
}
```

Storage location is a **deployment fact**, not syntax: `hermes run --store <dir>` or
`$HERMES_STORE`. Nothing is written until a workflow actually uses one of these builtins.

A unit may hold **several** workflows; exactly one is the `entry`, and `hermes run --workflow`
picks the rest. `hermes new` scaffolds a project with the * contract / implementation * split
already in place:

```sh
hermes new myapp && cd myapp
hermes build myapp.hermes -o myapp.hbc
hermes run myapp.hbc --workflow ping --input in.json
```

## CLI reference

| Command | What it does |
|---|---|
| `hermes demo` | compile → execute → native binary, in one command |
| `hermes <src> -o <out>` | **clang-style**: compile and link to a native executable |
| `hermes <src> -c` | compile only (produces a `.hbc` archive) |
| `hermes <src> -E` | preprocess only (expand `#include`) |
| `ir` | `.hermes` → IR (canonical JSON, reviewable build input) |
| `build` | `.hbc` archive; `-k` also produces the signature and `sealed.hermes` |
| `run` | execute IR or `.hbc`; `--trace` writes a verifiable trace |
| `ar t` / `ar x` | list / extract archive members |
| `link` | merge libraries: symbol resolution, ODR, dependency byte check, dead-code stripping |
| `sign` | Ed25519 sign / verify (`keygen` / `hbc` / `verify`) |
| `verify` | verify both artifacts together (five cross-checks) |
| `compile` | AOT: produce a native executable |
| `cc` | link your own C program against a Hermes unit |
| `header` | generate the public C ABI header (`--c` / `--cpp`) |
| `serve` | expose workflows over HTTP (four endpoints, OpenAI-shaped) |
| `lsp` | language server (diagnostics / completion / hover / outline / semantic tokens) |
| `doctor` | environment check: ABI, stdlib, goldens, clang identity, runtime library |
| `fmt` | whitespace normalisation (`--check` for CI) |
| `explain` | print a workflow's contract and cost upper bounds |
| `trace` | `verify` (recompute the hash chain) / `summary` / `diff` |
| `licenses` | print the license texts that must ship with the binary |

Exit codes: `0` success, `2` usage error, `4` operation failed.

---

## Compile to a native binary

```sh
hermes app.hermes -o app
./app '{"title":"printer is broken","priority":"urgent"}'
```

The result **does not need Hermes installed**. `argv[1]` is the input JSON, the same contract as
`hermes run --input`.

Under the hood this is the D-025 driver model: **no bundled compiler** — it detects the local
clang and calls it. Bundling GCC would put the GPL into the distribution chain; calling a
compiler the user already has is not distribution under any license.

---

## Embed in C / C++

```sh
hermes header app.hbc --c  -o app.h       # C89, verified with clang -std=c89 -pedantic
hermes header app.hbc --cpp -o app.hpp    # zero C++ standard library by default (-nostdinc++)
hermes cc user.c --unit app.hbc -o prog   # links and audits the symbol table
```

`examples/c-minimal/` is the whole thing in three files, and it runs:

```sh
cd examples/c-minimal && ./build.sh && ./app     # -> printer is broken #3
```

Ownership, thread-safety and the compatibility promise are in [`docs/C-ABI.md`](docs/C-ABI.md).
A CMake module lives in `cmake/hermesConfig.cmake` — **not tested yet** (no cmake on the
development machine); `examples/c-minimal/build.sh` is the verified path.

Generated headers include only the handwritten `hermes/hermes_core.h`. Types are **opaque
pointers** (flattened only for `@frozen`) — flattening would turn "the user added a field" into
"the client reads the wrong offset", and that failure is silent.

`.hpp` deliberately pulls in **no C++ standard library**: on Linux `clang++` defaults to
libstdc++ (GPL), so the convenience layer only appears under `HERMES_CPP_WRAPPERS`, and even
then it uses no stdlib types.

`hermes cc` parses the symbol table itself (`debug/elf`, `debug/macho`) and never calls `nm` —
one less deployment prerequisite for a yes/no question. A mangled symbol is a hard failure: it
means a C++ translation unit, and usually libstdc++, got linked in.

---

## Serve over HTTP

```sh
hermes serve app.hbc --addr 127.0.0.1:8080 --token "$TOKEN" --max-inflight 64
```

Four endpoints, shaped like the widely used OpenAI surface , so clients can switch back ends silently:

```
GET  /health
GET  /v1/models
POST /v1/chat/completions     supports stream: true (real streaming, from the provider layer)
POST /v1/responses
```

- Bearer auth: on when configured, but startup **says loudly** when it is not.
- Concurrency cap: over the limit returns `429 + Retry-After` instead of queueing.
- **Per-model cap** (`--max-inflight-per-model`): a slow workflow cannot starve the others.
- **Tenants** (`--tenants tenants.json`): the bearer token decides *who* you are, and each tenant
  carries its own concurrency limits **and budget**. Tokens live in the file, never on the
  command line. A tenant whose budget is gone gets `402`, not a silent overspend.

  A tenant is a **type**, not a loose bag of keys: declare it once in `.h`, and the JSON file is
  checked against that struct at startup, with field-level errors.

  ```hermes
  export struct Tenant {
      string name;
      int max_inflight;
      int budget_micro_usd;
      string budget_reset;   // optional: none | daily | weekly | monthly
  }

  export workflow ping() entry -> string {
      return "pong";
  }
  ```

  **Budget reset is pure opt-in.** The default `none` (or an omitted field) never resets:
  cumulative spend keeps growing and an exhausted budget stays `402`, exactly as before.
  To opt in, pass `--budget-reset daily|weekly|monthly` as a process-wide default, or set
  `budget_reset` on one tenant (the tenant value wins). Windows use **UTC** calendar
  boundaries, so a machine's local timezone cannot move the first of the month. This is
  just a configuration shape, not a tenancy feature — local, air-gapped and
  single-machine deployments can use it without being "tenants".

  If the unit declares `export struct Tenant`, its generated schema is strict
  (`additionalProperties: false`, and every declared field is `required`): declare
  `string budget_reset;` there too and give it a value on every tenant (an empty string
  means "use the global default"), otherwise a JSON file containing `budget_reset` is
  rejected at startup. With no declared struct, simply writing the field in the tenant
  JSON is enough. The process-wide `--budget-reset` flag never requires a JSON field.
- Run failures return **503**, not 400: the request was fine; the upstream/budget/circuit was not.

---

## Sign and verify artifacts

`.hbc` is an archive: IR bytes plus an index. Building with `-k` also produces `sealed.hermes`;
the two are **both required**:

| Artifact | Proves | Cannot prove |
|---|---|---|
| `.hbc` | archive integrity, signer identity, which source the member came from | contains no source |
| `sealed.hermes` | source ↔ IR same origin, bounds/economics at build time | does not contain the whole `.hbc` |

`hermes verify` runs five cross-checks: structure → signature → `index_sha256` → `ir_sha256` →
archive/source hashes.

The signature covers **the index and member hashes, not member bodies** — otherwise `link` would
need the original private key to reuse someone else's library, and the whole distribution chain
would break.

---

## Editor support

```sh
hermes lsp                                     # any LSP-capable editor
code --install-extension hermes-vscode-0.1.3.vsix
```

The extension provides syntax highlighting (TextMate), diagnostics (with *why* and *how to fix*),
completion, hover, outline and **semantic tokens**.

Semantic tokens matter: in VS Code, C++ variable colours come from clangd's semantic tokens, not
from grammar. To feel the same, the server has to emit tokens itself.

The extension has **zero npm dependencies** — the LSP client is a hand-written framing
implementation, which is easier to audit than a dependency tree.

---

## Client SDKs

Eight languages, **two ways to call**, one C ABI (`hermes_rt_*`):

1. **In-process binding** (like PyQt wrapping Qt) — load `libhermesrt` and run `.hbc` directly.
   No server, no network.
2. **HTTP client** — talk to `hermes serve`. Good for cross-machine, multi-tenant, authenticated use.

| Language | In-process | HTTP |
|---|---|---|
| Python | `NativeRuntime`, `ctypes` (stdlib) | yes |
| JavaScript | N-API addon (Node has no built-in FFI) | yes |
| TypeScript | same addon, typed | yes |
| Go | `dlopen` via cgo | yes |
| Rust | `extern "C"` + `dlopen`, no crates | yes |
| Java | FFM API (JDK 22+), no JNI | yes |
| C# | P/Invoke (BCL) | yes |
| Kotlin | FFM, same as Java | yes |

All of them are exercised by `scripts/test_sdks.sh`, which is part of `check.sh`.

---

## Architecture

Three layers, each doing what it is good at:

| Layer | Language | Responsibility | Why not elsewhere |
|---|---|---|---|
| Orchestration | Go stdlib | DAG, state machine, circuit breaker, budget ledger, trace, CLI | no syscalls, pure logic, easy to test |
| Executor | C/C++ + assembly | `posix_spawn`, rlimit, kill, sandbox, cycle counter | needs precise control over processes and time |
| Native extension | Rust | FFI handle table (generational), `mlock`+zeroize, zero-copy views, hash chain | memory safety, explicit zeroing |

---

## Long-term support

**2.0.0 is an LTS release.** The 2.x line guarantees:

- **IR compatibility**: every 2.x runtime reads IR produced by any 2.x compiler
  (`ir_version` stays `2`; new node kinds are optional fields, never renames)
- **C ABI stability**: `include/hermes/hermes_core.h` is frozen within 2.x (`abi 2.0`)
- **Bug fixes backported** to the latest 2.x patch for the support window

Breaking changes ship only in a new major (3.0.0), never inside 2.x.

## Status and limits

**Works** (each item has an executable check):

- `.hermes` → IR: 21 node kinds, type table, JSON schemas, tool table, `requires`, 16 goldens
- Execution: all control flow, concurrent `parallel`, try/retry, cancellation, budgets, circuit breaker
- **Bounded agent loop**: `model.agent(goal, tools, max_steps, max_parallel_tools)` with a
  compile-time bound; `hermes demo-runaway` shows it stopping at the bound
- **Memory and vectors**: `memory.get/put`, `vector.put/query` — file-backed, zero dependencies
- Artifacts: `.hbc`, `sealed.hermes`, signatures, `link`, AOT executables, C/C++ headers
- Editor: LSP (diagnostics / completion / hover / outline / semantic tokens) + VS Code extension
- End to end: `examples/support_bot` compiles → archives → signs → verifies → runs;
  `examples/starter` is the smallest complete project; `examples/memory` shows persistence

**Does not work yet** (real gaps, not polish):

- **`serve` has only a concurrency cap** — no per-tenant or per-model quotas
- **Semantic tokens are a textual approximation** — same-named variables in different scopes are
  indistinguishable; the real fix is putting the type checker behind the LSP
- **Local `#link "x"` needs `-L` at compile time** to record `member_sha256`; without it the
  dependency is only warned about, never invented
- **Static glibc linking is refused on Linux** ; the musl path is **not tested on Linux**
- **Language coverage**: `match` arms take string literals only; there is no array value semantics
- **One *entry* per unit**: several workflows may live in one unit, but only one is the entry
- **No document loaders**: memory and vector search are builtins, but turning a PDF or a web page
  into text is left to you. There is also no MCP client

These limits have reasons, not excuses. The Linux one especially: the gate is in the code, but it
has never been tested on a real Linux host, so the docs will not claim it works.

## Fuel the Development

Building low-level systems takes a lot of brainpower and coffee. If Hermes helps you, feel free
to buy Chris a coffee — over on **Afdian** (爱发电), since Buy Me a Coffee doesn't work in
mainland China.

- **<https://afdian.com/a/ChrisWangDEV>**
- or scan the code below

<img src="docs/assets/afdian-qr.png" width="200" alt="Afdian QR code">

No pressure: support is optional, changes nothing about the license, and buys no queue
priority — your bug report and a supporter's land in the same pile. That pile is just a little
better caffeinated.

---

## License and compliance

**Hermes is open source under the Apache License, Version 2.0** (`LICENSE`). You may use,
modify, redistribute and sell it, including inside closed-source products, subject to the
Apache-2.0 conditions (keep the license and NOTICE, state changes).

The license covers this repository only — the compiler, runtime, CLI, standard library,
generated headers, editor extension and SDKs. It does **not** cover any sibling project in
the same workspace, and it does not cover what you produce with Hermes: **your output is yours**.

Binary distribution also carries third-party obligations: the `hermes` binary links the Go
runtime (BSD-3) and two Rust crates (`sha2`, `zeroize`, MIT OR Apache-2.0). The release package
ships `LICENSE`, `NOTICE`, `THIRD_PARTY_NOTICES.md` and every text under `licenses/`;
`hermes licenses` prints all of them.

> Note: executables produced by `hermes compile` embed the Go runtime. If you redistribute such a
> binary, that obligation travels with it — your distribution must carry Go's license text too,
> available in `licenses/`.

(This is not legal advice. It turns "what must ship" into a checkable fact.)

---

## Development

```sh
bash scripts/build.sh               # C/asm → Rust → C++ frontend → Go (cgo, external link)
bash scripts/check.sh               # build + go test -race + 116 frontend tests + all guards + SDKs
bash scripts/release.sh             # relocatable package + SHA256SUMS + license texts
bash scripts/package_extension.sh   # .vsix (zip assembled by hand, zero npm)
bash scripts/test_sdks.sh           # the eight SDKs
```

The bar is hard: `check.sh` must be green. Two dozen guard tests cover the license red line,
architecture boundaries, release layout, CLI language, LSP protocol, TextMate grammar, semantic
tokens and `.vsix` structure.

---

## Documentation

| Document | Contents |
|---|---|
| [`docs/LANGUAGE.md`](docs/LANGUAGE.md) | language spec: syntax, types, error types, builtins |
| [`docs/IR.md`](docs/IR.md) | IR spec: node kinds, SP/bounds algebra, load-time invariants |
| [`docs/C-ABI.md`](docs/C-ABI.md) | C ABI: exported symbols, ownership, thread-safety |
| [`docs/TRACE-SCHEMA.md`](docs/TRACE-SCHEMA.md) | trace format and hash chain |
| [`docs/AOT-ERROR-OBJECT.md`](docs/AOT-ERROR-OBJECT.md) | the `HermesError` object embedded in AOT output |
| [`docs/STD-VARIANT.md`](docs/STD-VARIANT.md) | optional standard-library variants |
| [`docs/KNOWN-GAPS.md`](docs/KNOWN-GAPS.md) | what this version does **not** do yet |

Internal design notes (decision log, architecture history, handoff) are kept out of this
repository on purpose — they are working notes, not documentation.
---

<a id="简体中文"></a>

# 简体中文

**编译型 Agent 编排运行时。** 用 `.hermes` 描述工作流，编译成 IR，执行或**编成原生可执行文件** —
不联网、不装运行时、单文件二进制。

```sh
hermes demo                    # 一条命令看完整条链
hermes hello.hermes -o hello   # 像 clang 一样编译
./hello
```

---

## 问题

一个"看起来在推进"的 agent，可以永远地调工具、永远地烧钱。运行期计数的框架，账单到了才知道。
Hermes 把上界定在**编译期**。

```text
# 运行期计数 —— 跑飞了才知道
while (true) { const r = await agent.step(); }        // 什么时候停？

# Hermes —— 上界是常量，编译器知道它
auto answer = model.agent(goal = "…", tools = ["lookup_order"], max_steps = 4);
```

`max_parallel_tools`（默认 1）限制一轮里最多有几个工具调用 —— 模型确实会并行发多个。
两个常量共同决定上界：`max_model_calls = max_steps`、`max_tool_calls = max_parallel_tools × max_steps`、
`max_spans = (1 + max_parallel_tools) × max_steps`。

因为是常量，钱在请求发出**之前**就被预留，而不是落地之后才对账。

```console
$ hermes demo-runaway
  一个不会自己停下来的 agent

    模型每一轮都要求调用同一个工具：它以为在推进，其实在原地打转。
    运行期计数的框架会一直转，账单事后才知道；Hermes 的 max_steps 是编译期常量。

    max_steps = 4（写死在编译期，不是配置项）

    step 1  模型调用 → 要求调用 lookup_order
    step 2  模型调用 → 要求调用 lookup_order
    step 3  模型调用 → 要求调用 lookup_order
    step 4  模型调用 → 要求调用 lookup_order

    → ContractError
      agent 达到 max_steps=4 仍在请求工具调用（上界是承诺，不是起跑线）

    模型调用 4 次，工具调用 4 次，全部落在上界内

    结果：停下了。而且是在**花掉第 4 步的钱之后**立刻停的，不是跑完再对账。
```

一句话：**是 4，不是"跑到你手动 kill"**。

## 目录

- [这是什么](#这是什么)
- [30 秒上手](#30-秒上手)
- [安装](#安装)
- [语言教程](#语言教程)
- [命令行](#命令行)
- [编译成原生程序](#编译成原生程序)
- [嵌入 C / C++](#嵌入-c--c)
- [对外提供 HTTP](#对外提供-http)
- [签名与验签](#签名与验签)
- [编辑器支持](#编辑器支持)
- [客户端 SDK](#客户端-sdk)
- [架构](#架构)
- [状态与边界](#状态与边界)
- [许可与合规](#许可与合规)
- [开发](#开发)
- [文档](#文档)

---

## 这是什么

它不是"再写一个 Agent 框架"。区别在**编译期就把能确定的都确定下来**：

| 问题 | 谁回答 | 什么时候 |
|---|---|---|
| 输入输出长什么样 | `.h` 里的 struct | 编译期 |
| 最坏执行多少步、调几次模型/工具 | `bounds` | 编译期 |
| 跨并行分支有没有数据竞争 | 类型检查 | 编译期 |
| 这份产物是谁编的、有没有被改过 | 签名 + 交叉校验 | 分发期 |
| 这次运行哪一步失败了、花了多少 | trace 哈希链 | 运行期 |

能编译的就不在运行期猜。所以产物可以很小：一个 `.hbc` 归档，或者一个独立可执行文件。

三条硬约束贯穿整个项目：

- **零第三方 Go 依赖**（`go list -m all` 只有 `hermes` 一行）
- **默认只用 clang / libc++**；唯一例外是 `HERMES_ALLOW_LIBSTDCXX=1`（显式 opt-in），会在 `toolchain.json` 记 `gpl_tools_detected=true`，发布门禁据此拒绝
- **零传染性依赖**：第三方组件全是 MIT / Apache-2.0 / BSD（无传染性），Hermes 本体为 Apache-2.0 授权，可闭源商用

---

## 30 秒上手

```sh
hermes demo                                  # 编译 → 执行 → 出原生二进制
printf 'export workflow hi() entry -> string { return "hi"; }\n' > hi.hermes
hermes hi.hermes -o hi && ./hi               # clang 风格编译
hermes doctor                                # 环境自检：缺什么、怎么补
```

`hermes demo` 的示例源码嵌在二进制里，安装包内直接可用，不依赖仓库、不依赖网络。

---

## 安装

### 从发布包（推荐）

```sh
# macOS arm64 —— 安装器或免安装包，二选一
sudo installer -pkg hermes-2.0.0-macos-arm64.pkg -target /    # 装到 /usr/local
# 或：./scripts/install.sh hermes-2.0.0-darwin-arm64.tar.gz    # 校验 SHA256 并跑 doctor

# Linux x86_64 —— 安装器或免安装包，二选一
sudo dpkg -i hermes-2.0.0-linux-amd64.deb                      # 装到 /usr/local
# 或：./scripts/install.sh hermes-2.0.0-linux-x86_64.tar.gz

# 只有免安装包才需要这一行（我们不替你改 shell 配置）
export PATH="$HOME/.local/hermes/bin:$PATH"
```

Windows：跑 `hermes-2.0.0-windows-x64.msi`（装到 `Program Files` 并加 `PATH`），
或解压 zip 后执行 `install.ps1`，装到当前用户目录、不需要管理员权限。

发布包自包含，**不需要** Go / Rust / cargo。三个平台各自带什么：

| 路径 | macOS | Linux | Windows |
|---|:--:|:--:|:--:|
| `bin/hermes`（Windows 上是 `hermes.exe`） | ✅ | ✅ | ✅ |
| `bin/hermes-shim` —— 沙箱子进程 | ✅ | ✅ | — |
| `lib/libhermesrt.a` —— AOT 与 `hermes cc` 的运行时 | ✅ | ✅ | — |
| `lib/hermes_rt.h` —— 它的 C 头 | ✅ | ✅ | — |
| `lib/libhermesrt.dylib` / `.so` —— 进程内绑定用（ctypes / FFM / P/Invoke / dlopen） | ✅ | ✅ | — |
| `lib/cmake/hermes/` —— CMake 包配置（`find_package(hermes)`） | ✅ | ✅ | — |
| `include/hermes/hermes_core.h` —— 手写、冻结的对外 C ABI 头 | ✅ | ✅ | ✅ |
| `include/hermes/hermes.h` —— 旧别名头，为源码兼容保留 | ✅ | ✅ | ✅ |
| `SHA256SUMS` —— 包内每个文件的校验和 | ✅ | ✅ | ✅ |
| `licenses/` —— 第三方许可原文（分发时必须随包） | ✅ | ✅ | ✅ |

Windows 只发 CLI：Go 的 cgo 探针需要可重定位链接（`-r`），`lld-link` 不支持，
能满足它的工具链都在本项目红线之外。详见 [`docs/CHANGELOG.md`](docs/CHANGELOG.md)。

标准库已经 `go:embed` 进二进制，包里不再带第二份 —— 少一类"版本与二进制不配套"的故障。

`hermes compile` / `hermes cc` 需要本机 **clang**；其余命令不需要。`hermes doctor` 会明确说缺什么。

### 从源码

```sh
# macOS / Linux
bash scripts/build.sh     # 需要 Go ≥1.23、clang、cargo
bash scripts/check.sh     # 全套验收
bash scripts/release.sh   # 打发布包

# Windows（普通 PowerShell，不需要管理员）
powershell -File scripts\build.ps1
.\build\hermes.exe doctor
```

---

## 语言教程

下面每段都能编译 —— 有一条守卫会从本 README 里抽出所有 `hermes` 代码块逐个编译，
所以示例不会腐烂。

### 1. 你好

```hermes
export workflow hello() entry -> string {
    return "hello";
}
```

`entry` 标记 `hermes run` 与 `hermes serve` 默认选中的那个 workflow。

### 2. 类型

```hermes
export struct Ticket {
    string title;
    int priority;
}

export workflow label(input: Ticket) entry -> string {
    return "{input.title} #{input.priority}";
}
```

六种基础类型：`string` `int` `double` `bool` `json` `cost`。唯一的浮点类型是 `double`（IEEE-754 binary64），没有 32 位 `float`。double 字面量（`1.5` / `1e3` / `1.5e2`）已允许，编成 IR 位模式 `{"k":"double","bits":"<16 位小写 hex>"}`；IR 字节里绝不放 JSON 浮点数字，V-7 / canonical / 签名链都不受影响。`double` 支持 `+ - * /` 与 `< <= > >=` 比较；`%` 在源码层由 `E4009` 限定为 int 操作数（值层 IR / Go 引擎 / C 后端虽实现了 double 取余，但那不是语言面能力）；仍被硬拒的三条是 `double == double`（`E4033`）、`int`/`cost` 与 double 混用不自动提升（`E4006` / `E4008`）、`tool.call<double>`（`call_tool` 的 T 必须是 JSON 结构类型）。`call_native` 的返回类型可以是 `double`（真编译真跑已验证）。`json.as_float(j)` 有意保留原名、只是窄化到 `double`；`cost` 仍是 `int64` micro_usd，刻意与浮点路径分开：钱与最短往返表示正是各语言分歧最大的地方。

### 3. 分支

`match` 的臂接受**字符串或整数**字面量。整数臂存在的理由：状态码与枚举可以直接匹配，
不用先转成字符串。

```hermes
export struct Ticket {
    int priority;
}

export workflow triage(input: Ticket) entry -> string {
    match (input.priority) {
        case 200: { return "ok"; }
        case 404: { return "not found"; }
        default:  { return "error"; }
    }
}

```


```hermes
#link <std/model>

export struct Ticket {
    string priority;
}

export workflow triage(input: Ticket) entry -> string {
    match (input.priority) {
        case "urgent": { return "now"; }
        case "high": { return "today"; }
        default: { return "later"; }
    }
}
```

`match` 的臂只接受字符串字面量；`default` 是必需的。

### 3b. 数组

```hermes
export workflow sizes() entry -> json {
    auto xs = [1, 2, 3];
    auto n = len(xs);
    return xs;
}
```

数组是**定长值**：字面量的元素个数就是长度，所以 `len()` 在编译期就知道结果。下标同样在
编译期检查——三元素字面量上的 `xs[5]` 是**编译错误**，不是运行期的意外：

```hermes
export workflow pick() entry -> int {
    auto xs = [10, 20, 30];
    return xs[1];
}
```

同一个三元素字面量上写 `xs[5]` 是编译错误（`数组下标越界：长度 3，下标 5`）。

### 4. 调模型

```hermes
#link <std/model>

export struct Intent {
    string action;
}

export backend gateway : openai = env("GATEWAY_URL", "http://127.0.0.1:8080");

export workflow classify(backend = gateway) entry -> string {
    auto i = model.generate<Intent>(prompt = "classify this ticket", precision = "exact");
    return i.action;
}
```

`<Intent>` 是**契约**：schema 由 struct 生成，模型的回答会在**进入编排之前**被校验。
不合规会报 `ModelError` 并指出字段路径，而不是三步之后拿到一个神秘的 `null`。

地址只能来自 `env(...)` —— 硬编码地址的库没法分发。

### 5. 调工具

```hermes
#link <std/net>

export struct OrderInfo {
    string order_id;
    int status;
    int amount;
}

export workflow fetch() entry -> string {
    auto r = tool.call<OrderInfo>(
        url = "http://127.0.0.1:8080/orders/A1",
        method = "GET",
        allow_status = [404, 409]
    );
    if (r.status == 404) {
        return "no such order";
    }
    return "amount {r.body.amount}";
}
```

`allow_status` 把这些状态码变成**正常返回**，形状是 `{status, body, headers}`。
没有它，"订单不存在"只能抛异常，那个分支就根本写不出来 —— 这正是这个参数存在的理由。

### 6. 重试

```hermes
export workflow flaky() entry -> int {
    retry (3) {
        return 42;
    }
}
```

重试只作用于**可重试**的错误。契约违规重试三次还是错的，所以不重试。

### 7. 错误

```hermes
#link <std/net>

export struct Order {
    string id;
}

export workflow fetch() entry -> string {
    try {
        auto o = tool.call<Order>(url = "http://127.0.0.1:9/o", method = "GET");
        return "ok";
    } catch (HttpError e) {
        return "upstream said no";
    }
}
```

六种错误类型：`NetworkError` `HttpError` `ModelError` `ContractError` `CircuitOpenError` `Error`。
只有语言层面的错误可被捕获；编程错误（变量未绑定、节点下标越界）会直接炸穿 —— 那是刻意的。

### 8. 并行

```hermes
#link <std/net>

export struct Order {
    string id;
}

export struct History {
    string last;
}

export workflow both() entry -> string {
    auto [o, h] = parallel {
        auto x = tool.call<Order>(url = "http://127.0.0.1:9/o", method = "GET");
        auto y = tool.call<History>(url = "http://127.0.0.1:9/h", method = "GET");
        return [x, y];
    };
    return "{o.id}/{h.last}";
}
```

分支**真并发**，且不共享可写状态。`return [x, y]` 是 join 子句 —— 出几个值必须显式写出来。

### 9. 窄化 JSON

```hermes
export workflow pick(input: json) entry -> int {
    return json.as_int(input.total);
}
```

`json.as_*` 是程序里**唯一**能引入运行时类型错误的地方。失败抛 `ContractError`，
带 `expect`/`got`；**不会返回零值** —— 因为"金额没解析出来"悄悄变成 `0`，正是退款打错的来源。

### 10. 编译器告诉了你什么

```hermes
export workflow cheap(input: int) entry -> int {
    if (input > 10) { return input * 2; }
    return input + 1;
}
```

```console
$ hermes explain cheap.hbc
cheap  [entry exported unit=cheap]
  returns            int
  bounds（上界，不是预期）:
    max_steps        4
    max_spans        0
    max_concurrency  1
    max_model_calls  0
    max_tool_calls   0
```

`bounds` 驱动死代码剥离、trace 限额与预算预留。上界写错是**正确性**问题，不是显示问题 ——
所以有一条守卫会把每个 golden workflow 跑一遍，断言实际计数不超过声明上界。

### 11. 后端：Hermes 能接什么

`backend` 有五类 kind，四类是协议面，一类是逃生口：

| kind | 说的是 | 覆盖 |
|---|---|---|
| `openai` | OpenAI Chat Completions | OpenAI、Azure OpenAI、DeepSeek、Moonshot、Qwen、OpenRouter、LiteLLM、vLLM、LM Studio、Together… |
| `anthropic` | Anthropic Messages | Claude |
| `ollama` | Ollama 原生 API | 本地 Ollama |
| `custom` | **任意** OpenAI 兼容服务 | 你自己的网关、代理、或上面任何一个用非常规 base URL 的部署 |

因为 `custom` 与 `openai` 共用同一套协议面，"支持别的框架"在实践中就等于
**那个框架暴露一个 OpenAI 兼容端点** —— 而今天几乎都是。Hermes 刻意**不实现**别家的
编排格式：导入 LangChain chain 或 LangGraph graph 会养出第二个事实源，而它必然漂移
。

Hermes 替代的是**编排层**，不是集成目录：

| | LangChain 式 | Hermes |
|---|---|---|
| 你写什么 | Python/JS 代码调框架 | `.hermes` 源码，编译 |
| 错误何时暴露 | 运行期、线上 | 编译期（类型、上界、分支分析） |
| 部署 | Python 环境 + 依赖树 | 一个二进制，或 `.hbc` + C ABI |
| 排障 | 自己加的日志 | 可校验的 trace，带哈希链 |
| API 稳定性 | 框架内部抽象会变 | IR v1.4 有版本号，加载时校验 |

Hermes **不提供**：文档加载器、Agent 工具市场、托管向量库。但它**内置**了
**记忆**（键值）与**向量检索**（embedding + 余弦）—— 小到可以零依赖，
需要时也能换成你自己的存储。


### 12. 让模型自己选工具的 agent

```hermes
#link <std/model>
#link <std/net>

export struct Order {
    string id;
}

export tool orders {
    description = "look up an order by id";
    url = "http://127.0.0.1:8080/orders/{id}";
    method = "GET";
}

export backend gw : openai = env("GATEWAY_URL", "http://127.0.0.1:8080");

export workflow support(input: Order, backend = gw) entry -> string {
    auto answer = model.agent(
        goal = "check the status of order {input.id}",
        tools = ["orders"],
        max_steps = 4
    );
    return answer;
}
```

`max_steps` 是**常量**，而这正是关键：调哪个工具、跑几轮由模型决定，
但循环上界在编译期写死。于是 `max_model_calls = max_tool_calls = max_steps`，
预算仍然是**预留**而不是事后统计。

两条规矩让它不会变成它要替代的东西：
- 模型看到的工具 schema 与运行期执行的 URL 来自**同一条** `export tool` 声明，
  不存第二份要同步的副本；
- 到达 `max_steps` 时模型还在要工具，是**错误**，不是"再试一次"。


---

一个 unit 可以有**多个** workflow，其中恰好一个是 `entry`，其余用
`hermes run --workflow` 指定。`hermes new` 会生成一个已经分好「契约 / 实现」的项目：

```sh
hermes new myapp && cd myapp
hermes build myapp.hermes -o myapp.hbc
hermes run myapp.hbc --workflow ping --input in.json
```

## 命令行

| 命令 | 干什么 |
|---|---|
| `hermes demo` | 一条命令跑通编译 → 执行 → 出原生二进制 |
| `hermes <src> -o <out>` | **clang 风格**：编译并链接成原生可执行文件 |
| `hermes <src> -c` | 只编译（产出 `.hbc` 归档） |
| `hermes <src> -E` | 只预处理（展开 `#include`） |
| `ir` | `.hermes` → IR（canonical JSON，可作为构建输入审阅） |
| `build` | `.hbc` 归档；`-k` 同时产出签名与 `sealed.hermes` |
| `run` | 执行 IR 或 `.hbc`；`--trace` 落盘可校验的 trace |
| `ar t` / `ar x` | 查看 / 拆开归档成员 |
| `link` | 多库合并：符号解析 + ODR + 依赖字节校验 + 死代码剥离 |
| `sign` | Ed25519 签名与验签（`keygen` / `hbc` / `verify`） |
| `verify` | 两个产物一起验（五步交叉校验） |
| `compile` | AOT：编成原生可执行文件 |
| `cc` | 把你的 C 程序链上 Hermes unit |
| `header` | 生成对外 C ABI 头（`--c` / `--cpp`） |
| `serve` | 把 workflow 暴露成 HTTP 面（四端点，OpenAI 同形） |
| `lsp` | 语言服务器（诊断 / 补全 / 悬浮 / 大纲 / 语义着色） |
| `doctor` | 环境自检：ABI、stdlib、golden、clang 身份、运行时库 |
| `fmt` | 空白级规范化（`--check` 给 CI 用） |
| `explain` | 打印 workflow 的契约与代价上界 |
| `trace` | `verify`（重算哈希链）/ `summary` / `diff` |
| `licenses` | 打印分发时必须随附的许可文本 |

退出码：`0` 成功、`2` 用法错误、`4` 操作失败。

---

## 编译成原生程序

```sh
hermes app.hermes -o app
./app '{"title":"打印机坏了","priority":"urgent"}'
```

产物**不依赖已安装的 hermes**。`argv[1]` 是输入 JSON，与 `hermes run --input` 同一个契约。

底层是 D-025 的 driver 模型：**不内置编译器**，探测本机 clang 并调用它。
内置 GCC 会把 GPL 写进分发链，而"调用用户机器上已有的编译器"在任何许可下都不构成分发。

---

## 嵌入 C / C++

```sh
hermes header app.hbc --c  -o app.h       # C89，clang -std=c89 -pedantic 实测通过
hermes header app.hbc --cpp -o app.hpp    # 默认零 C++ 标准库（-nostdinc++ 实测）
hermes cc user.c --unit app.hbc -o prog   # 链接并自解析符号表
```

`examples/c-minimal/` 就是整件事的三个文件，而且能跑：

```sh
cd examples/c-minimal && ./build.sh && ./app     # -> printer is broken #3
```

所有权、线程安全、兼容性承诺写在 [`docs/C-ABI.md`](docs/C-ABI.md)。
CMake 模块在 `cmake/hermesConfig.cmake` —— **未实测**（开发机没装 cmake），
实测过的路径是 `examples/c-minimal/build.sh`。

生成的头只 include 手写的 `hermes/hermes_core.h`。类型一律**不透明指针**（展平只在 `@frozen` 时）——
展平会让"用户加了一个字段"变成"客户读到错误偏移"，而那是静默的。

`.hpp` 默认**不引入任何 C++ 标准库**：Linux 上 `clang++` 默认链 libstdc++（GPL），
所以便利层只在 `HERMES_CPP_WRAPPERS` 下出现，且仍不依赖标准库。

`hermes cc` 链接后**自己解析符号表**（`debug/elf`、`debug/macho`），不调 `nm` ——
多一个外部工具就多一个部署前提，而我们要的只是一个布尔判断。修饰名即硬失败。

---

## 对外提供 HTTP

```sh
hermes serve app.hbc --addr 127.0.0.1:8080 --token "$TOKEN" --max-inflight 64
```

四个端点，与主流的 OpenAI 面**同形**，客户端可以无感切换：

```
GET  /health
GET  /v1/models
POST /v1/chat/completions     支持 stream: true（真流式，来自 provider 层）
POST /v1/responses
```

- Bearer 鉴权：配了才开，但启动时**大声说明**当前是不是裸的。
- 并发上限：超限回 `429 + Retry-After`，不排队。
- **按模型并发上限**（`--max-inflight-per-model`）：一个慢 workflow 不会把其他模型饿死。
- **租户**（`--tenants tenants.json`）：bearer token 决定**你是谁**，每个租户带自己的并发上限。
  token 放文件里，不放在命令行（`ps` 能看到命令行参数）。租户预算用完时回 `402`，
  而不是悄悄超支。

  租户是**一个类型**，不是一堆散键：在 `.h` 里声明一次，JSON 配置启动时按这个 struct
  校验，错误带字段路径。

  ```hermes
  export struct Tenant {
      string name;
      int max_inflight;
      int budget_micro_usd;
      string budget_reset;   // 可选：none | daily | weekly | monthly
  }

  export workflow ping() entry -> string {
      return "pong";
  }
  ```

  **预算重置是纯 opt-in。** 默认 `none`（不写这个字段也一样）永不重置：累计花费只增
  不减，预算用完依旧回 `402`，与之前完全一致。要启用，可以传
  `--budget-reset daily|weekly|monthly` 作为全局默认值，也可以在某个租户上写
  `budget_reset`（租户级优先）。窗口按 **UTC** 日历边界计算，机器本地时区不会让
  「月初」悄悄挪一天。它只是配置形状，不绑定「租户」语义：本地部署、内网封闭
  环境、单机配置同样适用。

  如果 unit 声明了 `export struct Tenant`，生成的 schema 是严格的
  （`additionalProperties: false`，且所有声明字段都进 `required`）：要在结构体里
  声明 `string budget_reset;`，并在每条租户 JSON 里给出该值（空串表示「用全局
  默认值」），否则带 `budget_reset` 的 JSON 会在启动时被拒。没有声明 struct 时，
  直接在租户 JSON 里写这个字段即可；全局参数 `--budget-reset` 不要求 JSON 里有字段。
- 运行失败回 **503** 而不是 400：请求本身没错，是上游/预算/熔断出了问题。

---

## 签名与验签

`.hbc` 是归档：IR 字节 + 索引。带 `-k` 构建会同时产出 `sealed.hermes`，两者**缺一不可**：

| 产物 | 能证明 | 不能证明 |
|---|---|---|
| `.hbc` | 归档完整、签名者身份、成员来自哪份源码 | 不含源码 |
| `sealed.hermes` | 源码 ↔ IR 同源、构建时的 bounds/economics | 不含 `.hbc` 全文 |

`hermes verify` 做五步交叉校验：结构 → 验签 → `index_sha256` → `ir_sha256` → 归档/源码哈希。

签名覆盖**索引与成员哈希，不覆盖成员全文** —— 否则 `link` 复用别人的库时必须用原私钥重签，
整条分发链会断。

---

## 编辑器支持

```sh
hermes lsp                                     # 任何支持 LSP 的编辑器
code --install-extension hermes-vscode-0.1.3.vsix
```

扩展提供：语法高亮（TextMate）、诊断（带「为什么」与「怎么修」）、补全、悬浮、大纲、**语义着色**。

语义着色是关键一条：VS Code 里 C++ 的变量颜色来自 clangd 的语义 token，不是语法高亮。
要让 Hermes 看起来一样，服务器必须自己给 token。

扩展**零 npm 依赖** —— LSP 客户端是手写分帧实现，自己写比背一棵依赖树更容易审计。

---

## 客户端 SDK

八个语言，**两种用法**，同一套 C ABI（`hermes_rt_*`）：

1. **进程内绑定**（像 PyQt 封装 Qt）—— 加载 `libhermesrt` 直接跑 `.hbc`，不起服务、不走网络。
2. **HTTP 客户端** —— 跟 `hermes serve` 说话，适合跨机器 / 多租户 / 需要鉴权。

| 语言 | 进程内 | HTTP |
|---|---|---|
| Python | `NativeRuntime` / `Runtime`，`ctypes`（标准库） | 是 |
| JavaScript | N-API 插件（Node 没有内建 FFI） | 是 |
| TypeScript | 同一插件 + 类型 | 是 |
| Go | `dlopen`（cgo） | 是 |
| Rust | `extern "C"` + `dlopen`，无 crate | 是 |
| Java | FFM API（JDK 22+），不用 JNI | 是 |
| C# | P/Invoke（BCL） | 是 |
| Kotlin | FFM，同 Java | 是 |

八个都由 `scripts/test_sdks.sh` 跑过，而它是 `check.sh` 的一步。

---

## 架构

三层分工，各做各擅长的事：

| 层 | 语言 | 负责 | 为什么不放别处 |
|---|---|---|---|
| 编排 | Go 标准库 | DAG、状态机、熔断、预算账本、trace、CLI | 不碰 syscall，纯逻辑，好测试 |
| 执行器 | C/C++ + 汇编 | `posix_spawn`、rlimit、kill、沙箱、周期计数器 | 需要精确控制进程与时间 |
| 原生扩展 | Rust | FFI handle 表（带 generation）、`mlock`+zeroize、零拷贝视图、哈希链 | 内存安全 + 明确的清零语义 |

---

## 长期支持（LTS）

**2.0.0 是长期支持版本。** 2.x 系列承诺：

- **IR 兼容**：任何 2.x 运行时都能读任何 2.x 编译器产出的 IR
  （`ir_version` 恒为 `2`；新节点种类只加可选字段，不改名）
- **C ABI 稳定**：`include/hermes/hermes_core.h` 在 2.x 期间冻结（`abi 2.0`）
- **修复会回合**到最新的 2.x 补丁版，支持窗口内不要求升级

破坏性改动只出现在新的大版本（3.0.0），绝不塞进 2.x。

## 状态与边界

**能做**（每条都有可执行验收）：

- `.hermes` → IR：21 种节点、类型表、JSON Schema、工具表、`requires`、16 份 golden
- 执行：全部控制流 + 真并发 `parallel` + try/retry + 取消 + 预算 + 熔断
- **有界 agent 循环**：`model.agent(goal, tools, max_steps, max_parallel_tools)`，
  上界是编译期常量；`hermes demo-runaway` 直接演示它在边界处停下
- **记忆与向量**：`memory.get/put`、`vector.put/query` —— 文件存储、零依赖
- 产物：`.hbc`、`sealed.hermes`、签名、`link`、AOT 可执行文件、C/C++ 头
- 编辑器：LSP（诊断/补全/悬浮/大纲/语义着色）+ VS Code 扩展
- 端到端：`examples/support_bot` 编译 → 归档 → 签名 → 验证 → 执行；
  `examples/starter` 是最小完整工程，`examples/memory` 演示跨进程持久化

**不能做**（功能缺口，不是打磨）：

- **`serve` 只有并发上限**，没有按租户/按模型的配额
- **语义着色是文本扫描的近似**：同名不同作用域区分不了（真正的解法是把类型检查接进 LSP）
- **本地 `#link "x"` 需要编译时 `-L`** 才能记录 `member_sha256`；不给只 warning，不编假哈希
- **Linux 静态链 glibc 默认拒绝**；musl 那条路**未在 Linux 实测**
- **语言覆盖**：`match` 的 `case` 只接受字符串字面量；没有数组值语义
- **没有文档加载器**：记忆与向量检索已内置，但把 PDF / 网页变成文本这一步留给你。
  也没有 MCP 客户端
- **一个 unit 只能有一个 entry**：可以有多个 workflow，但 `entry` 只能一个

这些边界是有理由的，不是"没做完"。尤其是 Linux 那条 —— 闸门在代码里，但没在真机上验过，
所以文档不会写"已验证"。

## 为开发充能

写底层系统很费脑子，也很费咖啡。如果你觉得 Hermes 很好用，可以在这里请 Chris 喝杯咖啡 ——
用**爱发电**（Buy Me a Coffee 在大陆用不了）。

- **<https://afdian.com/a/ChrisWangDEV>**
- 或者扫下面的码

<img src="docs/assets/afdian-qr.png" width="200" alt="爱发电二维码">

没压力：赞助是可选的，不影响许可，也换不来优先级 —— 你的 bug 和赞助者的 bug 进同一条队列。
只不过那条队列的咖啡因浓度高一点。

---

## 许可与合规

**Hermes 本体以 Apache License 2.0 开源**（见 `LICENSE`）。你可以使用、修改、再分发与**销售**它，
包括放进闭源产品里，但需遵守 Apache-2.0 的条件（保留许可证与 NOTICE、标明改动）。

授权仅限本仓库里的 Hermes：编译器、运行时、CLI、标准库、生成的头、编辑器扩展、客户端 SDK。
它**不覆盖**同一工作区里的其他项目；也**不覆盖你用 Hermes 产出的东西** —— **你的产物归你**。

二进制分发还带第三方义务：`hermes` 里链着 Go 运行时（BSD-3）与两个 Rust crate
（`sha2`、`zeroize`，MIT OR Apache-2.0）。发布包已带齐 `LICENSE`、`NOTICE`、
`THIRD_PARTY_NOTICES.md` 与 `licenses/` 下的全部原文；`hermes licenses` 一条命令打印它们。

> 注意：`hermes compile` 产出的可执行文件里嵌着 Go 运行时。你分发那个产物时，义务随它走 ——
> **你的分发也要带 Go 的许可文本**，原文在 `licenses/` 里。

（这不是法律意见。它做的是把"分发时必须带什么"变成可核对的事实。）

---

## 开发

```sh
bash scripts/build.sh               # C/asm → Rust → C++ 前端 → Go（cgo，外部链接）
bash scripts/check.sh               # 构建 + go test -race + 前端 116 项 + 全部守卫 + SDK
bash scripts/release.sh             # 可重定位发布包 + SHA256SUMS + 许可文本
bash scripts/package_extension.sh   # .vsix（手拼 zip，零 npm）
bash scripts/test_sdks.sh           # 八个 SDK
```

判据很硬：`check.sh` 必须全绿。二十余条守卫覆盖红线、架构边界、发布包布局、CLI 语言、
LSP 协议、TextMate 语法、语义 token 与 `.vsix` 结构。

---

## 文档

| 文档 | 内容 |
|---|---|
| [`docs/LANGUAGE.md`](docs/LANGUAGE.md) | 语言规范：语法、类型、错误类型、内建 |
| [`docs/IR.md`](docs/IR.md) | IR 规范：节点、SP/bounds 代数、加载期不变量 |
| [`docs/C-ABI.md`](docs/C-ABI.md) | C ABI：导出符号、所有权、线程安全 |
| [`docs/TRACE-SCHEMA.md`](docs/TRACE-SCHEMA.md) | trace 格式与哈希链 |
| [`docs/AOT-ERROR-OBJECT.md`](docs/AOT-ERROR-OBJECT.md) | AOT 产物内嵌的 `HermesError` 对象 |
| [`docs/STD-VARIANT.md`](docs/STD-VARIANT.md) | 标准库的可选变体 |
| [`docs/KNOWN-GAPS.md`](docs/KNOWN-GAPS.md) | 本版本**尚未提供**的能力 |

内部设计记录（决策日志、架构沿革、交接笔记）**刻意不放进这个仓库** —— 它们是工作笔记，不是文档。
