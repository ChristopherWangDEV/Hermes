# dev.hermes (Java)

Both usages, one C ABI. **JDK only** — no JNI, no Maven/Gradle file, no third-party jar.

## In-process (like PyQt wrapping Qt)

```java
try (var rt = new NativeRuntime("app.hbc")) {
    System.out.println(rt.run("{\"title\":\"printer is broken\"}"));
}
```

Uses the FFM API (`java.lang.foreign`, JDK 22+). Run with
`--enable-native-access=ALL-UNNAMED` to silence the restricted-method warning.

## HTTP

```java
var c = new HermesClient("http://127.0.0.1:8080", "token");
System.out.println(c.models());
System.out.println(c.run("triage", "{\"title\":\"x\"}").content);
```

The generated C header (`hermes header --c`) is for C/C++; this SDK calls the runtime directly.
