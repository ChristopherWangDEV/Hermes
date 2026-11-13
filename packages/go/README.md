# Go SDK

```sh
go get github.com/ChristopherWangDEV/Hermes/packages/go/v2
```

```go
import hermes "github.com/ChristopherWangDEV/Hermes/packages/go"
```

Both usages, one C ABI. Zero third-party modules.

## In-process (like PyQt wrapping Qt)

```go
rt, err := hermes.OpenRuntime("app.hbc", "")
if err != nil {
    log.Fatal(err)
}
defer rt.Close()

out, err := rt.Run(`{"title":"printer is broken"}`)
```

Loads `libhermesrt` with `dlopen` — so the SDK follows wherever the user installed Hermes
(`HERMES_HOME`, then the repository layout).

## HTTP

```go
c := hermes.New("http://127.0.0.1:8080", "token")
models, _ := c.Models()
res, _ := c.Run("triage", map[string]any{"title": "x"})
fmt.Println(res.Content)

_ = c.ChatStream([]hermes.ChatMessage{{Role: "user", Content: "hi"}}, "",
    func(piece string) error { fmt.Print(piece); return nil })
```
