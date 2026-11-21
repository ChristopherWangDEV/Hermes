import dev.hermes.NativeRuntime
import java.nio.file.Files
import java.nio.file.Path

/**
 * 进程内绑定的测试：编一个 .hbc，用 FFM 直接跑。
 *
 * 不引测试框架：SDK 的测试也不该给使用者添依赖。失败即非零退出。
 */
fun main() {
    val root = Path.of("..", "..").toAbsolutePath().normalize()
    val hermes = root.resolve("build/hermes")
    if (!Files.exists(hermes)) {
        println("跳过：没有 build/hermes（先跑 scripts/build.sh）")
        return
    }
    val dir = Files.createTempDirectory("hermes-kt")
    val src = dir.resolve("echo.hermes")
    Files.writeString(
        src,
        "export struct In { string text; }\n" +
            "export workflow echo(input: In) entry -> string { return \"echo: {input.text}\"; }\n",
    )
    val hbc = dir.resolve("echo.hbc")
    val p = ProcessBuilder(hermes.toString(), "build", src.toString(), "-o", hbc.toString())
        .redirectErrorStream(true).start()
    val out = p.inputStream.readAllBytes().toString(Charsets.UTF_8)
    if (p.waitFor() != 0) {
        System.err.println("build 失败：$out")
        kotlin.system.exitProcess(1)
    }

    NativeRuntime(hbc.toString()).use { rt ->
        val got = rt.run("""{"text":"hi"}""")
        check(got.contains("echo: hi")) { "run 返回 workflow 输出，实际：$got" }

        try {
            rt.runOn(dir.resolve("nope.hbc").toString(), "{}")
            error("缺失 unit 应抛异常")
        } catch (e: NativeRuntime.HermesException) {
            check(e.code != 0) { "异常应带 code" }
        }
    }
    println("Kotlin 原生绑定：全部通过")
}
