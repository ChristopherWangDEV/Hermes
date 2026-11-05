package dev.hermes

import java.net.URI
import java.net.http.HttpClient
import java.net.http.HttpRequest
import java.net.http.HttpResponse
import java.nio.charset.StandardCharsets
import java.time.Duration

/** 服务端错误。保留 status：401 与 503 的处置完全不同。 */
class HermesException(
    message: String,
    val status: Int = 0,
    val type: String = "",
) : RuntimeException(message)

/** 一次执行的结果。 */
data class ChatResult(val content: String, val model: String, val raw: String)

/**
 * Hermes 运行时的 Kotlin 客户端。
 *
 * 它说 `hermes serve` 的 HTTP 面（四个端点，与 OpenAI 同形，D-021）：
 * 不需要装 hermes、不需要 JNI —— 只要能发 HTTP。
 *
 * 零第三方依赖：只用 JDK 自带的 `java.net.http`（所以运行在 JVM 上，
 * Kotlin/JS 与 Kotlin/Native 不在覆盖范围）。
 */
class Client(
    baseUrl: String = "http://127.0.0.1:8080",
    private val token: String = "",
) {
    private val baseUrl: String = baseUrl.trimEnd('/')
    private val http: HttpClient = HttpClient.newBuilder()
        .connectTimeout(Duration.ofSeconds(10))
        .build()

    /** 存活与能力。 */
    fun health(): String = send("GET", "/health", null)

    /** 可用的 workflow 名（服务端只列 exported 的）。 */
    fun models(): List<String> {
        val body = send("GET", "/v1/models", null)
        val re = Regex("\"id\"\\s*:\\s*\"((?:[^\"\\\\]|\\\\.)*)\"")
        return re.findAll(body).map { unescape(it.groupValues[1]) }.toList()
    }

    /**
     * 执行一个 workflow。
     *
     * `inputJson` 是**显式**的（非 OpenAI 标准）：workflow 的输入契约由 `.h` 里的
     * struct 定义，把它映射到 messages 只能靠猜。传 null 表示不需要输入。
     */
    fun run(workflow: String, inputJson: String? = null): ChatResult {
        val body = if (inputJson == null) {
            """{"model":"${escape(workflow)}"}"""
        } else {
            """{"model":"${escape(workflow)}","input":$inputJson}"""
        }
        val raw = send("POST", "/v1/chat/completions", body)
        return toResult(raw, workflow)
    }

    /** 按 OpenAI 的 messages 形状调用（服务端取最后一条 user 消息当 text）。 */
    fun chat(messagesJson: String, model: String = ""): ChatResult {
        val body = """{"model":"${escape(model)}","messages":$messagesJson}"""
        val raw = send("POST", "/v1/chat/completions", body)
        return toResult(raw, model)
    }

    private fun toResult(raw: String, fallbackModel: String): ChatResult =
        ChatResult(
            content = firstString(raw, "content") ?: "",
            model = firstString(raw, "model") ?: fallbackModel,
            raw = raw,
        )

    private fun send(method: String, path: String, body: String?): String {
        val builder = HttpRequest.newBuilder(URI.create(baseUrl + path))
            .timeout(Duration.ofSeconds(60))
            .header("Content-Type", "application/json")
        if (token.isNotEmpty()) {
            builder.header("Authorization", "Bearer $token")
        }
        builder.method(
            method,
            if (body == null) HttpRequest.BodyPublishers.noBody()
            else HttpRequest.BodyPublishers.ofString(body, StandardCharsets.UTF_8),
        )
        val res = http.send(builder.build(), HttpResponse.BodyHandlers.ofString(StandardCharsets.UTF_8))
        if (res.statusCode() / 100 != 2) {
            val message = firstString(res.body(), "message") ?: "HTTP ${res.statusCode()}"
            throw HermesException(message, res.statusCode(), firstString(res.body(), "type") ?: "")
        }
        return res.body()
    }

    companion object {
        /** 取第一个同名字符串字段。不引 JSON 库：SDK 的依赖就是使用者的依赖。 */
        fun firstString(json: String, field: String): String? {
            val re = Regex("\"${Regex.escape(field)}\"\\s*:\\s*\"((?:[^\"\\\\]|\\\\.)*)\"")
            return re.find(json)?.let { unescape(it.groupValues[1]) }
        }

        fun escape(s: String): String = buildString {
            for (c in s) {
                when (c) {
                    '"' -> append("\\\"")
                    '\\' -> append("\\\\")
                    '\n' -> append("\\n")
                    '\r' -> append("\\r")
                    '\t' -> append("\\t")
                    else -> append(c)
                }
            }
        }

        fun unescape(s: String): String = buildString {
            var i = 0
            while (i < s.length) {
                val c = s[i]
                if (c != '\\' || i + 1 >= s.length) {
                    append(c)
                    i++
                    continue
                }
                when (val n = s[i + 1]) {
                    'n' -> append('\n')
                    'r' -> append('\r')
                    't' -> append('\t')
                    'u' -> {
                        if (i + 5 < s.length) {
                            append(s.substring(i + 2, i + 6).toInt(16).toChar())
                            i += 6
                            continue
                        }
                        append(n)
                    }
                    else -> append(n)
                }
                i += 2
            }
        }
    }
}
