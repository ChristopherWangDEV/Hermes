package dev.hermes;

import java.io.IOException;
import java.io.InputStream;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

/**
 * Hermes 运行时的 Java 客户端。
 *
 * <p>它说 {@code hermes serve} 的 HTTP 面（四个端点，与 OpenAI 同形，D-021）：
 * 不需要装 hermes、不需要 JNI —— 只要能发 HTTP。
 *
 * <p>零第三方依赖：只用 JDK 自带的 {@code java.net.http}。SDK 引第三方库，
 * 等于把那棵依赖树带进每个使用者的构建里。
 */
public final class HermesClient {

    /** 服务端错误。保留 status：401 与 503 的处置完全不同。 */
    public static final class HermesException extends RuntimeException {
        public final int status;
        public final String type;

        public HermesException(String message, int status, String type) {
            super(message);
            this.status = status;
            this.type = type;
        }
    }

    /** 一次执行的结果。 */
    public static final class ChatResult {
        public final String content;
        public final String model;
        public final String raw;

        ChatResult(String content, String model, String raw) {
            this.content = content;
            this.model = model;
            this.raw = raw;
        }
    }

    private final String baseUrl;
    private final String token;
    private final HttpClient http;

    public HermesClient(String baseUrl, String token) {
        this.baseUrl = baseUrl.endsWith("/") ? baseUrl.substring(0, baseUrl.length() - 1) : baseUrl;
        this.token = token == null ? "" : token;
        this.http = HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(10)).build();
    }

    public HermesClient() {
        this("http://127.0.0.1:8080", "");
    }

    /** 存活与能力。 */
    public String health() throws IOException, InterruptedException {
        return send("GET", "/health", null);
    }

    /** 可用的 workflow 名（服务端只列 exported 的）。 */
    public List<String> models() throws IOException, InterruptedException {
        String body = send("GET", "/v1/models", null);
        List<String> out = new ArrayList<>();
        Matcher m = Pattern.compile("\"id\"\\s*:\\s*\"((?:[^\"\\\\]|\\\\.)*)\"").matcher(body);
        while (m.find()) {
            out.add(unescape(m.group(1)));
        }
        return out;
    }

    /**
     * 执行一个 workflow。
     *
     * <p>{@code inputJson} 是**显式**的（非 OpenAI 标准）：workflow 的输入契约由
     * {@code .h} 里的 struct 定义，把它映射到 messages 只能靠猜。传 {@code null}
     * 表示不需要输入。
     */
    public ChatResult run(String workflow, String inputJson) throws IOException, InterruptedException {
        String body = inputJson == null
                ? "{\"model\":\"" + escape(workflow) + "\"}"
                : "{\"model\":\"" + escape(workflow) + "\",\"input\":" + inputJson + "}";
        String raw = send("POST", "/v1/chat/completions", body);
        return toResult(raw, workflow);
    }

    /** 按 OpenAI 的 messages 形状调用（服务端取最后一条 user 消息当 text）。 */
    public ChatResult chat(String messagesJson, String model) throws IOException, InterruptedException {
        String body = "{\"model\":\"" + escape(model) + "\",\"messages\":" + messagesJson + "}";
        String raw = send("POST", "/v1/chat/completions", body);
        return toResult(raw, model);
    }

    private ChatResult toResult(String raw, String fallbackModel) {
        String content = firstString(raw, "content");
        String model = firstString(raw, "model");
        return new ChatResult(
                content == null ? "" : content,
                model == null ? fallbackModel : model,
                raw);
    }

    private String send(String method, String path, String body)
            throws IOException, InterruptedException {
        HttpRequest.Builder b = HttpRequest.newBuilder(URI.create(baseUrl + path))
                .timeout(Duration.ofSeconds(60))
                .header("Content-Type", "application/json");
        if (!token.isEmpty()) {
            b.header("Authorization", "Bearer " + token);
        }
        b.method(method, body == null
                ? HttpRequest.BodyPublishers.noBody()
                : HttpRequest.BodyPublishers.ofString(body, StandardCharsets.UTF_8));

        HttpResponse<InputStream> res = http.send(b.build(), HttpResponse.BodyHandlers.ofInputStream());
        String text;
        try (InputStream in = res.body()) {
            text = new String(in.readAllBytes(), StandardCharsets.UTF_8);
        }
        int status = res.statusCode();
        if (status / 100 != 2) {
            throw new HermesException(
                    firstString(text, "message") == null ? "HTTP " + status : firstString(text, "message"),
                    status,
                    firstString(text, "type") == null ? "" : firstString(text, "type"));
        }
        return text;
    }

    // ---- 极简 JSON 读取 ----
    //
    // 为什么不引 Jackson/Gson：那会把一棵依赖树带进每个使用者的构建。
    // 这里只需要"取第一个同名字符串字段"，够用且可审。

    static String firstString(String json, String field) {
        Matcher m = Pattern.compile("\"" + Pattern.quote(field) + "\"\\s*:\\s*\"((?:[^\"\\\\]|\\\\.)*)\"")
                .matcher(json);
        return m.find() ? unescape(m.group(1)) : null;
    }

    static String escape(String s) {
        StringBuilder sb = new StringBuilder(s.length() + 2);
        for (char c : s.toCharArray()) {
            switch (c) {
                case '"' -> sb.append("\\\"");
                case '\\' -> sb.append("\\\\");
                case '\n' -> sb.append("\\n");
                case '\r' -> sb.append("\\r");
                case '\t' -> sb.append("\\t");
                default -> sb.append(c);
            }
        }
        return sb.toString();
    }

    static String unescape(String s) {
        StringBuilder sb = new StringBuilder(s.length());
        for (int i = 0; i < s.length(); i++) {
            char c = s.charAt(i);
            if (c != '\\' || i + 1 >= s.length()) {
                sb.append(c);
                continue;
            }
            char n = s.charAt(++i);
            switch (n) {
                case 'n' -> sb.append('\n');
                case 'r' -> sb.append('\r');
                case 't' -> sb.append('\t');
                case 'u' -> {
                    if (i + 4 < s.length()) {
                        sb.append((char) Integer.parseInt(s.substring(i + 1, i + 5), 16));
                        i += 4;
                    }
                }
                default -> sb.append(n);
            }
        }
        return sb.toString();
    }

    /** 便于调用方构造请求体：把 Java Map 转成 JSON（仅字符串/数字/布尔/嵌套 Map/List）。 */
    public static String json(Object value) {
        StringBuilder sb = new StringBuilder();
        writeJson(sb, value);
        return sb.toString();
    }

    private static void writeJson(StringBuilder sb, Object v) {
        if (v == null) {
            sb.append("null");
        } else if (v instanceof String s) {
            sb.append('"').append(escape(s)).append('"');
        } else if (v instanceof Number || v instanceof Boolean) {
            sb.append(v);
        } else if (v instanceof Map<?, ?> m) {
            sb.append('{');
            boolean first = true;
            for (Map.Entry<?, ?> e : m.entrySet()) {
                if (!first) {
                    sb.append(',');
                }
                first = false;
                sb.append('"').append(escape(String.valueOf(e.getKey()))).append("\":");
                writeJson(sb, e.getValue());
            }
            sb.append('}');
        } else if (v instanceof Iterable<?> it) {
            sb.append('[');
            boolean first = true;
            for (Object o : it) {
                if (!first) {
                    sb.append(',');
                }
                first = false;
                writeJson(sb, o);
            }
            sb.append(']');
        } else {
            throw new IllegalArgumentException("不支持的类型: " + v.getClass());
        }
    }

    /** 测试用：LinkedHashMap 便捷构造。 */
    public static Map<String, Object> map(Object... kv) {
        Map<String, Object> m = new LinkedHashMap<>();
        for (int i = 0; i + 1 < kv.length; i += 2) {
            m.put(String.valueOf(kv[i]), kv[i + 1]);
        }
        return m;
    }
}
