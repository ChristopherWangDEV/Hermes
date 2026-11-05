# hermes-sdk (Rust)

Both usages, one C ABI. **Zero crates** — `dlopen` is declared directly with `extern "C"`.

## In-process (like PyQt wrapping Qt)

```rust
use hermes_sdk::native::NativeRuntime;

auto rt = NativeRuntime::open("app.hbc", None)?;
println!("{}", rt.run(Some(r#"{"title":"printer is broken"}"#))?);
```

## HTTP

```rust
use hermes_sdk::Client;

auto c = Client::new("http://127.0.0.1:8080", "token");
println!("{}", c.health()?);
println!("{:?}", c.models()?);
```

**Honest limit**: the HTTP client speaks HTTP/1.1 by hand, so it supports `http://` only.
Connecting to `https://` needs TLS, which would mean pulling a crate — the deployment shape we
recommend is `hermes serve` on loopback behind a gateway that terminates TLS.
