// Hermes 运行时的**进程内**绑定（像 PyQt 封装 Qt 那样）。
//
// 用 .NET 的 P/Invoke 直接调 libhermesrt 的 C ABI：不经过网络、不需要起服务。
// 零第三方依赖，只用 BCL。
//
// 边界（诚实说明）：只覆盖执行（run）。serve 的健康/模型端点是网络面，
// 在进程内没有对应物。

using System;
using System.Collections.Generic;
using System.IO;
using System.Runtime.InteropServices;

namespace Hermes;

/// <summary>原生层错误。Code：2 = 输入/文件问题，3 = 执行失败。</summary>
public sealed class HermesNativeException : Exception
{
    public int Code { get; }
    public HermesNativeException(string message, int code = 0) : base(message) => Code = code;
}

public sealed class NativeRuntime : IDisposable
{
    // 显式用函数指针而不是 DllImport("名字")：DllImport 的库名解析在不同平台/
    // 打包形态下很脆（尤其 "lib" 前缀与扩展名），而我们已经拿到了绝对路径。
    private delegate int RunFileFn(IntPtr unitPath, IntPtr input, UIntPtr inputLen, out IntPtr outPtr, out IntPtr errPtr);
    private delegate void FreeFn(IntPtr p);

    private readonly IntPtr _lib;
    private readonly RunFileFn _runFile;
    private readonly FreeFn _free;
    private readonly string _unit;

    public NativeRuntime(string unitPath, string? libraryPath = null)
    {
        _unit = unitPath;
        var path = FindLibrary(libraryPath);
        _lib = NativeLibrary.Load(path);
        _runFile = Marshal.GetDelegateForFunctionPointer<RunFileFn>(
            NativeLibrary.GetExport(_lib, "hermes_rt_run_file"));
        _free = Marshal.GetDelegateForFunctionPointer<FreeFn>(
            NativeLibrary.GetExport(_lib, "hermes_rt_free"));
    }

    /// <summary>执行这个 unit；inputJson 为 null 表示不需要输入。</summary>
    public string Run(string? inputJson = null) => RunOn(_unit, inputJson);

    /// <summary>执行指定的 unit（覆盖构造时给的那个）。</summary>
    public string RunOn(string unitPath, string? inputJson = null)
    {
        var unitPtr = Marshal.StringToCoTaskMemUTF8(unitPath);
        byte[] payload = inputJson is null ? Array.Empty<byte>() : System.Text.Encoding.UTF8.GetBytes(inputJson);
        var inputPtr = payload.Length == 0 ? IntPtr.Zero : Marshal.AllocHGlobal(payload.Length);
        if (payload.Length > 0)
        {
            Marshal.Copy(payload, 0, inputPtr, payload.Length);
        }
        try
        {
            var code = _runFile(unitPtr, inputPtr, (UIntPtr)payload.Length, out var outPtr, out var errPtr);
            string? outText = null, errText = null;
            try
            {
                errText = PtrToString(errPtr);
                outText = PtrToString(outPtr);
            }
            finally
            {
                // 两块内存都由原生层分配，必须用它的 free 归还。
                if (outPtr != IntPtr.Zero) _free(outPtr);
                if (errPtr != IntPtr.Zero) _free(errPtr);
            }
            if (code != 0)
            {
                throw new HermesNativeException(
                    string.IsNullOrEmpty(errText) ? $"执行失败(code={code})" : errText!, code);
            }
            return outText ?? "";
        }
        finally
        {
            Marshal.FreeCoTaskMem(unitPtr);
            if (inputPtr != IntPtr.Zero) Marshal.FreeHGlobal(inputPtr);
        }
    }

    private static string? PtrToString(IntPtr p) =>
        p == IntPtr.Zero ? null : Marshal.PtrToStringUTF8(p);

    /// <summary>按 HERMES_HOME → 仓库 build/ → 包旁 lib/ 的顺序找动态库。</summary>
    public static string FindLibrary(string? explicitPath = null)
    {
        if (!string.IsNullOrEmpty(explicitPath))
        {
            if (File.Exists(explicitPath)) return explicitPath!;
            throw new HermesNativeException("指定的库不存在: " + explicitPath);
        }
        var name = OperatingSystem.IsMacOS() ? "libhermesrt.dylib"
                 : OperatingSystem.IsWindows() ? "hermesrt.dll"
                 : "libhermesrt.so";
        var tried = new List<string>();
        var home = Environment.GetEnvironmentVariable("HERMES_HOME");
        if (!string.IsNullOrEmpty(home)) tried.Add(Path.Combine(home!, "lib", name));
        foreach (var d in new[] { "build", "../build", "../../build", "../../../build" })
        {
            tried.Add(Path.Combine(d, name));
        }
        foreach (var p in tried)
        {
            if (File.Exists(p)) return p;
        }
        throw new HermesNativeException(
            $"找不到 {name}。已尝试：{string.Join(", ", tried)}。" +
            "发布包应自带 lib/；开发时先跑 scripts/build.sh，或用 HERMES_HOME 指向安装根。");
    }

    public void Dispose() => NativeLibrary.Free(_lib);
}
