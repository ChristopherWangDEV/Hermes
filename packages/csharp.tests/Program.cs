// C# SDK 的测试：**进程内绑定**（P/Invoke → libhermesrt）与 HTTP 客户端两条路径都验。
//
// 不引 xUnit/NUnit：SDK 的测试也不该给使用者添依赖。失败即非零退出。
using System;
using System.Collections.Generic;
using System.Diagnostics;
using System.IO;
using System.Net;
using System.Net.Sockets;
using System.Text;
using System.Text.Json;
using System.Threading.Tasks;
using Hermes;

internal static class Program
{
    private static string _lastAuth = "";
    private static string _lastBody = "";

    private static async Task<int> Main()
    {
        var dir = Directory.CreateTempSubdirectory("hermes-cs-");
        var unit = Path.Combine(dir.FullName, "echo.hbc");
        if (!await BuildUnit(dir.FullName, unit).ConfigureAwait(false))
        {
            Console.WriteLine("跳过：没有 build/hermes（先跑 scripts/build.sh）");
            return 0;
        }

        NativeRuntimeTest(unit);
        await HttpTest().ConfigureAwait(false);
        Console.WriteLine("C# SDK：全部通过");
        return 0;
    }

    private static void NativeRuntimeTest(string unit)
    {
        using var rt = new NativeRuntime(unit);
        Check(rt.Run("{\"text\":\"hi\"}").Contains("echo: hi"), "原生绑定 run 返回 workflow 输出");
        try
        {
            rt.RunOn(Path.Combine(Path.GetDirectoryName(unit)!, "nope.hbc"), "{}");
            Check(false, "缺失 unit 应抛异常");
        }
        catch (HermesNativeException e)
        {
            Check(e.Code != 0, "异常应带 code");
        }
    }

    private static async Task<bool> BuildUnit(string dir, string unit)
    {
        var root = Path.GetFullPath(Path.Combine(AppContext.BaseDirectory, "../../../../../"));
        var hermes = Path.Combine(root, "build", "hermes");
        if (!File.Exists(hermes)) return false;
        var src = Path.Combine(dir, "echo.hermes");
        await File.WriteAllTextAsync(src,
            "export struct In { string text; }\n" +
            "export workflow echo(input: In) entry -> string { return \"echo: {input.text}\"; }\n")
            .ConfigureAwait(false);
        var psi = new ProcessStartInfo(hermes) { RedirectStandardError = true, RedirectStandardOutput = true };
        psi.ArgumentList.Add("build");
        psi.ArgumentList.Add(src);
        psi.ArgumentList.Add("-o");
        psi.ArgumentList.Add(unit);
        using var p = Process.Start(psi)!;
        await p.WaitForExitAsync().ConfigureAwait(false);
        if (p.ExitCode != 0)
        {
            Console.Error.WriteLine("build 失败：" + await p.StandardError.ReadToEndAsync().ConfigureAwait(false));
            Environment.Exit(1);
        }
        return true;
    }

    private static async Task HttpTest()
    {
        var port = FreePort();
        var listener = new HttpListener();
        listener.Prefixes.Add($"http://127.0.0.1:{port}/");
        listener.Start();
        _ = Task.Run(async () =>
        {
            while (listener.IsListening)
            {
                HttpListenerContext ctx;
                try { ctx = await listener.GetContextAsync().ConfigureAwait(false); }
                catch { return; }
                _lastAuth = ctx.Request.Headers["Authorization"] ?? "";
                using (var reader = new StreamReader(ctx.Request.InputStream, Encoding.UTF8))
                {
                    _lastBody = await reader.ReadToEndAsync().ConfigureAwait(false);
                }
                var path = ctx.Request.Url!.AbsolutePath;
                var (code, body) = path switch
                {
                    "/health" => (200, "{\"status\":\"ok\"}"),
                    "/v1/models" => (200, "{\"data\":[{\"id\":\"triage\"}]}"),
                    _ when _lastBody.Contains("\"model\":\"boom\"") =>
                        (503, "{\"error\":{\"message\":\"upstream failed\",\"type\":\"server_error\"}}"),
                    _ => (200, "{\"model\":\"triage\",\"choices\":[{\"message\":{\"content\":\"ok\"}}]}"),
                };
                var bytes = Encoding.UTF8.GetBytes(body);
                ctx.Response.StatusCode = code;
                ctx.Response.ContentType = "application/json";
                await ctx.Response.OutputStream.WriteAsync(bytes).ConfigureAwait(false);
                ctx.Response.Close();
            }
        });

        var client = new Client($"http://127.0.0.1:{port}", "s3cret");
        Check((await client.HealthAsync().ConfigureAwait(false)).Contains("ok"), "health");
        var models = await client.ModelsAsync().ConfigureAwait(false);
        Check(models.Count == 1 && models[0] == "triage", "models");
        var r = await client.RunAsync("triage", new Dictionary<string, object?> { ["title"] = "x" }).ConfigureAwait(false);
        Check(r.Content == "ok", "run 返回内容");
        Check(_lastBody.Contains("\"input\":{\"title\":\"x\"}"), "run 必须显式发 input");
        Check(_lastAuth == "Bearer s3cret", "Bearer 头");
        listener.Stop();
    }

    private static int FreePort()
    {
        var l = new TcpListener(IPAddress.Loopback, 0);
        l.Start();
        var port = ((IPEndPoint)l.LocalEndpoint).Port;
        l.Stop();
        return port;
    }

    private static void Check(bool cond, string what)
    {
        if (!cond)
        {
            Console.Error.WriteLine("FAIL: " + what);
            Environment.Exit(1);
        }
    }
}
