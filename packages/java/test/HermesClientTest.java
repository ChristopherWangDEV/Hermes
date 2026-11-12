import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;
import dev.hermes.HermesClient;

import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;

/**
 * 用 JDK 自带的 HttpServer 起假服务端，验线格式。
 *
 * 不引 JUnit：SDK 的测试也不该给使用者添依赖。失败即非零退出。
 */
public final class HermesClientTest {
    private static String lastAuth;
    private static String lastBody;

    public static void main(String[] args) throws Exception {
        HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/", ex -> {
            lastAuth = ex.getRequestHeaders().getFirst("Authorization");
            String body = new String(ex.getRequestBody().readAllBytes(), StandardCharsets.UTF_8);
            lastBody = body;
            String path = ex.getRequestURI().getPath();
            String resp;
            int code = 200;
            if (path.equals("/health")) {
                resp = "{\"status\":\"ok\"}";
            } else if (path.equals("/v1/models")) {
                resp = "{\"data\":[{\"id\":\"triage\"}]}";
            } else if (body.contains("\"model\":\"boom\"")) {
                code = 503;
                resp = "{\"error\":{\"message\":\"upstream failed\",\"type\":\"server_error\"}}";
            } else {
                resp = "{\"model\":\"triage\",\"choices\":[{\"message\":{\"content\":\"ok\"}}]}";
            }
            byte[] bytes = resp.getBytes(StandardCharsets.UTF_8);
            ex.getResponseHeaders().add("Content-Type", "application/json");
            ex.sendResponseHeaders(code, bytes.length);
            try (OutputStream os = ex.getResponseBody()) {
                os.write(bytes);
            }
        });
        server.start();
        int port = server.getAddress().getPort();
        String base = "http://127.0.0.1:" + port;

        try {
            HermesClient c = new HermesClient(base, "s3cret");
            check(c.health().contains("ok"), "health");
            check(c.models().equals(java.util.List.of("triage")), "models");

            HermesClient.ChatResult r = c.run("triage", "{\"title\":\"x\"}");
            check(r.content.equals("ok"), "run 返回内容");
            check(lastBody.contains("\"input\":{\"title\":\"x\"}"), "run 必须显式发 input");
            check(!lastBody.contains("messages"), "run 不该把 input 塞进 messages");
            check("Bearer s3cret".equals(lastAuth), "Bearer 头");

            try {
                c.run("boom", null);
                check(false, "503 应当抛异常");
            } catch (HermesClient.HermesException e) {
                check(e.status == 503, "异常应带 status");
                check("server_error".equals(e.type), "异常应带 type");
            }

            // json() 是给调用方构造请求体用的，顺手验一下转义。
            String body = HermesClient.json(HermesClient.map("title", "he said \"hi\"\n"));
            check(body.contains("\\\"hi\\\""), "json 转义引号");
            check(body.contains("\\n"), "json 转义换行");

            System.out.println("Java SDK：全部通过");
        } finally {
            server.stop(0);
        }
    }

    private static void check(boolean cond, String what) {
        if (!cond) {
            System.err.println("FAIL: " + what);
            System.exit(1);
        }
    }
}
