import dev.hermes.NativeRuntime;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;

/**
 * 进程内绑定的测试：编一个 .hbc，用 FFM 直接跑。
 *
 * 不引 JUnit：SDK 的测试也不该给使用者添依赖。失败即非零退出。
 */
public final class NativeRuntimeTest {
    public static void main(String[] args) throws Exception {
        Path root = Path.of("..", "..").normalize();
        Path hermes = root.resolve("build/hermes");
        if (!Files.exists(hermes)) {
            System.out.println("跳过：没有 build/hermes（先跑 scripts/build.sh）");
            return;
        }
        Path tmp = Files.createTempDirectory("hermes-java");
        Path src = tmp.resolve("echo.hermes");
        Files.writeString(src,
                "export struct In { string text; }\n"
                        + "export workflow echo(input: In) entry -> string { return \"echo: {input.text}\"; }\n",
                StandardCharsets.UTF_8);
        Path hbc = tmp.resolve("echo.hbc");
        Process p = new ProcessBuilder(hermes.toString(), "build", src.toString(), "-o", hbc.toString())
                .redirectErrorStream(true).start();
        String buildOut = new String(p.getInputStream().readAllBytes(), StandardCharsets.UTF_8);
        if (p.waitFor() != 0) {
            System.err.println("build 失败：" + buildOut);
            System.exit(1);
        }

        try (NativeRuntime rt = new NativeRuntime(hbc.toString())) {
            String got = rt.run("{\"text\":\"hi\"}");
            check(got.contains("echo: hi"), "run 返回 workflow 输出，实际：" + got);

            try {
                rt.runOn(tmp.resolve("nope.hbc").toString(), "{}");
                check(false, "缺失 unit 应抛异常");
            } catch (NativeRuntime.HermesException e) {
                check(e.code != 0, "异常应带 code");
            }
        }
        System.out.println("Java 原生绑定：全部通过");
    }

    private static void check(boolean cond, String what) {
        if (!cond) {
            System.err.println("FAIL: " + what);
            System.exit(1);
        }
    }
}
