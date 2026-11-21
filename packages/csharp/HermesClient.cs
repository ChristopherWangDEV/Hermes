// Hermes 运行时的 C# 客户端。
//
// 它说 `hermes serve` 的 HTTP 面（四个端点，与 OpenAI 同形，D-021）：
// 不需要装 hermes、不需要 P/Invoke —— 只要能发 HTTP。
//
// 零第三方依赖：只用 BCL 的 HttpClient / System.Text.Json。

using System;
using System.Collections.Generic;
using System.Net.Http;
using System.Text;
using System.Text.Json;
using System.Threading.Tasks;

namespace Hermes;

/// <summary>服务端错误。保留 Status：401 与 503 的处置完全不同。</summary>
public sealed class HermesException : Exception
{
    public int Status { get; }
    public string Type { get; }

    public HermesException(string message, int status = 0, string type = "") : base(message)
    {
        Status = status;
        Type = type;
    }
}

/// <summary>一次执行的结果。</summary>
public sealed record ChatResult(string Content, string Model, string Raw);

public sealed class Client
{
    private readonly string _baseUrl;
    private readonly string _token;
    private readonly HttpClient _http;

    public Client(string baseUrl = "http://127.0.0.1:8080", string token = "")
    {
        _baseUrl = baseUrl.TrimEnd('/');
        _token = token ?? "";
        _http = new HttpClient { Timeout = TimeSpan.FromSeconds(60) };
    }

    /// <summary>存活与能力。</summary>
    public async Task<string> HealthAsync()
    {
        using var req = NewRequest(HttpMethod.Get, "/health", null);
        return await SendAsync(req).ConfigureAwait(false);
    }

    /// <summary>可用的 workflow 名（服务端只列 exported 的）。</summary>
    public async Task<List<string>> ModelsAsync()
    {
        var body = await HealthLikeAsync("/v1/models").ConfigureAwait(false);
        var outList = new List<string>();
        using var doc = JsonDocument.Parse(body);
        if (doc.RootElement.TryGetProperty("data", out var data) && data.ValueKind == JsonValueKind.Array)
        {
            foreach (var item in data.EnumerateArray())
            {
                if (item.TryGetProperty("id", out var id) && id.ValueKind == JsonValueKind.String)
                {
                    outList.Add(id.GetString()!);
                }
            }
        }
        return outList;
    }

    private async Task<string> HealthLikeAsync(string path)
    {
        using var req = NewRequest(HttpMethod.Get, path, null);
        return await SendAsync(req).ConfigureAwait(false);
    }

    /// <summary>
    /// 执行一个 workflow。
    ///
    /// <paramref name="input"/> 是**显式**的（非 OpenAI 标准）：workflow 的输入契约
    /// 由 `.h` 里的 struct 定义，把它映射到 messages 只能靠猜。
    /// 传 null 表示不需要输入。
    /// </summary>
    public async Task<ChatResult> RunAsync(string workflow, object? input = null)
    {
        var payload = new Dictionary<string, object?> { ["model"] = workflow };
        if (input is not null)
        {
            payload["input"] = input;
        }
        return await ChatLikeAsync(payload, workflow).ConfigureAwait(false);
    }

    /// <summary>按 OpenAI 的 messages 形状调用（服务端取最后一条 user 消息当 text）。</summary>
    public Task<ChatResult> ChatAsync(IEnumerable<object> messages, string model = "")
    {
        var payload = new Dictionary<string, object?>
        {
            ["model"] = model,
            ["messages"] = messages,
        };
        return ChatLikeAsync(payload, model);
    }

    private async Task<ChatResult> ChatLikeAsync(object payload, string model)
    {
        var json = JsonSerializer.Serialize(payload);
        using var req = NewRequest(HttpMethod.Post, "/v1/chat/completions", json);
        var raw = await SendAsync(req).ConfigureAwait(false);
        using var doc = JsonDocument.Parse(raw);
        var content = "";
        if (doc.RootElement.TryGetProperty("choices", out var choices) &&
            choices.ValueKind == JsonValueKind.Array && choices.GetArrayLength() > 0)
        {
            var first = choices[0];
            if (first.TryGetProperty("message", out var msg) &&
                msg.TryGetProperty("content", out var c) && c.ValueKind == JsonValueKind.String)
            {
                content = c.GetString()!;
            }
        }
        var m = model;
        if (doc.RootElement.TryGetProperty("model", out var mm) && mm.ValueKind == JsonValueKind.String)
        {
            m = mm.GetString()!;
        }
        return new ChatResult(content, m, raw);
    }

    private HttpRequestMessage NewRequest(HttpMethod method, string path, string? body)
    {
        var req = new HttpRequestMessage(method, _baseUrl + path);
        if (!string.IsNullOrEmpty(_token))
        {
            req.Headers.Add("Authorization", "Bearer " + _token);
        }
        if (body is not null)
        {
            req.Content = new StringContent(body, Encoding.UTF8, "application/json");
        }
        return req;
    }

    private async Task<string> SendAsync(HttpRequestMessage req)
    {
        var res = await _http.SendAsync(req).ConfigureAwait(false);
        var text = await res.Content.ReadAsStringAsync().ConfigureAwait(false);
        var status = (int)res.StatusCode;
        if (status / 100 != 2)
        {
            var message = "HTTP " + status;
            var type = "";
            try
            {
                using var doc = JsonDocument.Parse(text);
                if (doc.RootElement.TryGetProperty("error", out var err))
                {
                    if (err.TryGetProperty("message", out var msg)) message = msg.GetString() ?? message;
                    if (err.TryGetProperty("type", out var t)) type = t.GetString() ?? "";
                }
            }
            catch (JsonException)
            {
                // 非 JSON 错误页：保留状态码就够了。
            }
            throw new HermesException(message, status, type);
        }
        return text;
    }
}
