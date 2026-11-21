# dev.hermes (Kotlin)

Both usages, one C ABI. **JDK only** — no Gradle/Maven file, no third-party jar.

## In-process (like PyQt wrapping Qt)

```kotlin
NativeRuntime("app.hbc").use { rt ->
    println(rt.run("""{"title":"printer is broken"}"""))
}
```

Uses the FFM API. Compile and run with the local JDK:

```sh
kotlinc src/dev/hermes/NativeRuntime.kt test/NativeRuntimeTest.kt -include-runtime -d out/native.jar
java --enable-native-access=ALL-UNNAMED -cp out/native.jar NativeRuntimeTestKt
```

`--enable-native-access` silences the restricted-method warning; FFM will require it in a
future JDK.

## HTTP

```kotlin
val c = Client("http://127.0.0.1:8080", "token")
println(c.models())
println(c.run("triage", """{"title":"x"}""").content)
```

⚠️ `-include-runtime` bundles the Kotlin stdlib **into the jar, for the test only**.
A published Kotlin library declares `kotlin("stdlib")` and lets the consumer's build resolve it —
otherwise every user gets a second copy of the stdlib.
