package dev.hermes;

import java.lang.foreign.Arena;
import java.lang.foreign.FunctionDescriptor;
import java.lang.foreign.Linker;
import java.lang.foreign.MemorySegment;
import java.lang.foreign.SymbolLookup;
import java.lang.foreign.ValueLayout;
import java.lang.invoke.MethodHandle;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.List;

/**
 * Hermes 运行时的**进程内**绑定（像 PyQt 封装 Qt 那样）。
 *
 * <p>用 JDK 自带的 FFM（{@code java.lang.foreign}，JDK 22 起标准）直接调
 * {@code libhermesrt} 的 C ABI。不需要 JNI、不需要第三方库、不需要起服务。
 *
 * <p>边界（诚实说明）：只覆盖**执行**（run）。{@code serve} 的健康/模型端点
 * 在进程内没有对应物 —— 它们是网络面，不是运行时面。
 */
public final class NativeRuntime implements AutoCloseable {

    /** 原生层错误。code：2 = 输入/文件问题，3 = 执行失败。 */
    public static final class HermesException extends RuntimeException {
        public final int code;

        public HermesException(String message, int code) {
            super(message);
            this.code = code;
        }
    }

    private static final Linker LINKER = Linker.nativeLinker();
    private final Arena arena;
    private final MethodHandle runFile;
    private final MethodHandle free;
    private final String unit;

    public NativeRuntime(String unitPath) {
        this(unitPath, findLibrary(null));
    }

    public NativeRuntime(String unitPath, String libraryPath) {
        this.unit = unitPath;
        this.arena = Arena.ofShared();
        try {
            System.load(Path.of(libraryPath).toAbsolutePath().toString());
        } catch (UnsatisfiedLinkError alreadyLoaded) {
            // 同一进程里多次构造是常见用法（每个 unit 一个 Runtime），
            // 重复 load 同一个库会抛错，忽略即可。
        }
        SymbolLookup lookup = SymbolLookup.loaderLookup();
        this.runFile = LINKER.downcallHandle(
                lookup.find("hermes_rt_run_file").orElseThrow(
                        () -> new IllegalStateException("动态库里没有 hermes_rt_run_file")),
                FunctionDescriptor.of(ValueLayout.JAVA_INT,
                        ValueLayout.ADDRESS, ValueLayout.ADDRESS,
                        ValueLayout.JAVA_LONG, ValueLayout.ADDRESS, ValueLayout.ADDRESS));
        this.free = LINKER.downcallHandle(
                lookup.find("hermes_rt_free").orElseThrow(
                        () -> new IllegalStateException("动态库里没有 hermes_rt_free")),
                FunctionDescriptor.ofVoid(ValueLayout.ADDRESS));
    }

    /** 执行这个 unit；{@code inputJson} 为 null 表示不需要输入。 */
    public String run(String inputJson) {
        return runOn(unit, inputJson);
    }

    /** 执行指定的 unit（覆盖构造时给的那个）。 */
    public String runOn(String unitPath, String inputJson) {
        try (Arena call = Arena.ofConfined()) {
            MemorySegment pathSeg = call.allocateFrom(unitPath);
            byte[] payload = inputJson == null ? new byte[0] : inputJson.getBytes(StandardCharsets.UTF_8);
            MemorySegment inputSeg = payload.length == 0 ? MemorySegment.NULL : call.allocateFrom(
                    ValueLayout.JAVA_BYTE, payload);
            MemorySegment outPtr = call.allocate(ValueLayout.ADDRESS);
            MemorySegment errPtr = call.allocate(ValueLayout.ADDRESS);

            int code;
            try {
                code = (int) runFile.invokeExact(pathSeg, inputSeg, (long) payload.length, outPtr, errPtr);
            } catch (Throwable t) {
                throw new IllegalStateException("调用原生层失败", t);
            }
            // 指针由原生层分配，必须用它的 free 归还 —— 宿主 GC 不知道那块内存。
            String out = null;
            String err = null;
            try {
                err = readCString(errPtr.get(ValueLayout.ADDRESS, 0));
                out = readCString(outPtr.get(ValueLayout.ADDRESS, 0));
            } finally {
                freeIfSet(call, outPtr);
                freeIfSet(call, errPtr);
            }
            if (code != 0) {
                throw new HermesException(err == null || err.isEmpty() ? "执行失败(code=" + code + ")" : err, code);
            }
            return out == null ? "" : out;
        }
    }

    private void freeIfSet(Arena call, MemorySegment slot) {
        MemorySegment p = slot.get(ValueLayout.ADDRESS, 0);
        if (p.equals(MemorySegment.NULL)) {
            return;
        }
        try {
            free.invokeExact(p);
        } catch (Throwable t) {
            throw new IllegalStateException("释放原生内存失败", t);
        }
    }

    private static String readCString(MemorySegment p) {
        if (p == null || p.equals(MemorySegment.NULL)) {
            return null;
        }
        return p.reinterpret(Long.MAX_VALUE).getString(0);
    }

    /** 按 HERMES_HOME → 仓库 build/ → 包旁 lib/ 的顺序找动态库。 */
    public static String findLibrary(String explicit) {
        String name = System.getProperty("os.name", "").toLowerCase().contains("mac")
                ? "libhermesrt.dylib"
                : (System.getProperty("os.name", "").toLowerCase().contains("win")
                        ? "hermesrt.dll" : "libhermesrt.so");
        List<String> tried = new java.util.ArrayList<>();
        if (explicit != null) {
            if (Files.exists(Path.of(explicit))) {
                return explicit;
            }
            throw new HermesException("指定的库不存在: " + explicit, 0);
        }
        String home = System.getenv("HERMES_HOME");
        if (home != null) {
            tried.add(home + "/lib/" + name);
        }
        // 从包的测试目录、以及仓库根分别能找到的位置都试一遍。
        tried.add("build/" + name);
        tried.add("../build/" + name);
        tried.add("../../build/" + name);
        for (String p : tried) {
            if (Files.exists(Path.of(p))) {
                return p;
            }
        }
        throw new HermesException(
                "找不到 " + name + "。已尝试：" + String.join(", ", tried)
                        + "。发布包应自带 lib/；开发时先跑 scripts/build.sh，"
                        + "或用 HERMES_HOME 指向安装根。", 0);
    }

    @Override
    public void close() {
        arena.close();
    }
}
