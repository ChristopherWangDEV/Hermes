import com.sun.net.httpserver.HttpServer
import dev.hermes.Client
import dev.hermes.HermesException
import java.net.InetSocketAddress

/**
 * 用 JDK 自带的 HttpServer 起假服务端，验线格式。
 *
 * **未在本机运行过**：本机没有 kotlinc。保留它是为了"有编译器的机器上一条命令能验"，
 * 而不是假装它已经验证过。
 */
fun main() {
    val server = HttpServer.create(InetSocketAddress("127.0.0.1", 0), 0)
    var lastAuth = ""
    var lastBody = ""
    server.createContext("/") { ex ->
        lastAuth = ex.requestHeaders.getFirst("Authorization") ?: ""
        lastBody = ex.requestBody.readAllBytes().toString(Charsets.UTF_8)
        val path = ex.requestURI.path
        val (code, resp) = when {
            path == "/health" -> 200 to """{"status":"ok"}"""
            path == "/v1/models" -> 200 to """{"data":[{"id":"triage"}]}"""
            lastBody.contains("\"model\":\"boom\"") ->
                503 to """{"error":{"message":"upstream failed","type":"server_error"}}"""
            else -> 200 to """{"model":"triage","choices":[{"message":{"content":"ok"}}]}"""
        }
        val bytes = resp.toByteArray()
        ex.responseHeaders.add("Content-Type", "application/json")
        ex.sendResponseHeaders(code, bytes.size.toLong())
        ex.responseBody.use { it.write(bytes) }
    }
    server.start()
    val base = "http://127.0.0.1:${server.address.port}"

    try {
        val c = Client(base, "s3cret")
        check(c.health().contains("ok")) { "health" }
        check(c.models() == listOf("triage")) { "models" }

        val r = c.run("triage", """{"title":"x"}""")
        check(r.content == "ok") { "run 内容" }
        check(lastBody.contains("\"input\":{\"title\":\"x\"}")) { "run 必须显式发 input" }
        check(!lastBody.contains("messages")) { "run 不该塞 messages" }
        check(lastAuth == "Bearer s3cret") { "Bearer 头" }

        try {
            c.run("boom")
            error("503 应当抛异常")
        } catch (e: HermesException) {
            check(e.status == 503) { "异常带 status" }
            check(e.type == "server_error") { "异常带 type" }
        }
        println("Kotlin SDK：全部通过")
    } finally {
        server.stop(0)
    }
}
