# HermesSdk (C#)

Both usages, one C ABI. **BCL only** — no `PackageReference`.

## In-process (like PyQt wrapping Qt)

```csharp
using var rt = new NativeRuntime("app.hbc");
Console.WriteLine(rt.Run("{\"title\":\"printer is broken\"}"));
```

Uses `NativeLibrary` + `Marshal.GetDelegateForFunctionPointer` instead of `DllImport("name")`:
library-name resolution is brittle across platforms, and we already have an absolute path.

## HTTP

```csharp
var c = new Client("http://127.0.0.1:8080", "token");
Console.WriteLine(await c.HealthAsync());
foreach (var m in await c.ModelsAsync()) Console.WriteLine(m);
var r = await c.RunAsync("triage", new Dictionary<string, object?> { ["title"] = "x" });
```

Requires .NET 8.
