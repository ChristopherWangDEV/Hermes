package dev.hermes

import java.lang.foreign.Arena
import java.lang.foreign.FunctionDescriptor
import java.lang.foreign.Linker
import java.lang.foreign.MemorySegment
import java.lang.foreign.SymbolLookup
import java.lang.foreign.ValueLayout
import java.lang.invoke.MethodHandle
import java.nio.file.Files
import java.nio.file.Path

/**
 * Hermes 运行时的**进程内**绑定（像 PyQt 封装 Qt 那样）。
 *
 * 用 JDK 的 FFM API 直接调 `libhermesrt` 的 C ABI：不需要 JNI、不需要第三方库、
 * 不需要起服务。
 *
 * 边界：只覆盖执行（run）。`serve` 的健康/模型端点是网络面。
 */
class NativeRuntime @JvmOverloads constructor(
    private val unit: String?,
    libraryPath: String? = null,
) : AutoCloseable {

    /** 原生层错误。code：2 = 输入/文件问题，3 = 执行失败。 */
    class HermesException(message: String, val code: Int) : RuntimeException(message)

    private val arena: Arena = Arena.ofShared()
    private val runFile: MethodHandle
    private val free: MethodHandle

    init {
        val path = findLibrary(libraryPath)
        try {
            System.load(Path.of(path).toAbsolutePath().toString())
        } catch (_: UnsatisfiedLinkError) {
            // 同一进程里重复 load 会抛错：多个 unit 共用一个库是常见用法。
        }
        val linker = Linker.nativeLinker()
        val lookup: SymbolLookup = SymbolLookup.loaderLookup()
        runFile = linker.downcallHandle(
            lookup.find("hermes_rt_run_file").orElseThrow {
                IllegalStateException("动态库里没有 hermes_rt_run_file")
            },
            FunctionDescriptor.of(
                ValueLayout.JAVA_INT,
                ValueLayout.ADDRESS,
                ValueLayout.ADDRESS,
                ValueLayout.JAVA_LONG,
                ValueLayout.ADDRESS,
                ValueLayout.ADDRESS,
            ),
        )
        free = linker.downcallHandle(
            lookup.find("hermes_rt_free").orElseThrow {
                IllegalStateException("动态库里没有 hermes_rt_free")
            },
            FunctionDescriptor.ofVoid(ValueLayout.ADDRESS),
        )
    }

    /** 执行构造时给的那个 unit。 */
    fun run(inputJson: String? = null): String = runOn(unit, inputJson)

    /** 执行指定 unit。 */
    fun runOn(unitPath: String?, inputJson: String?): String {
        val target = unitPath ?: throw HermesException("没有指定 unit", 0)
        Arena.ofConfined().use { call ->
            val pathSeg = call.allocateFrom(target)
            val bytes = inputJson?.toByteArray(Charsets.UTF_8) ?: ByteArray(0)
            // `*bytes` 是展开运算符：漏了它只会分配 1 个字节，而原生层读到的
            // 就是一段带 \x00 的残垃圾 —— 报错是 "invalid character '\x00'"，
            // 离"参数没展开"这个真实原因很远。
            val inputSeg = if (bytes.isEmpty()) MemorySegment.NULL
            else call.allocateFrom(ValueLayout.JAVA_BYTE, *bytes)
            val outPtr = call.allocate(ValueLayout.ADDRESS)
            val errPtr = call.allocate(ValueLayout.ADDRESS)

            // 注意：`invokeExact` 是**签名多态**方法，Kotlin 无法像 Java 那样精确调用；
            // 用 `invokeWithArguments`（慢一点，但语义正确）。写错会得到
            // WrongMethodTypeException，而错误信息完全指不到"FFM 调用方式不对"。
            val code = try {
                runFile.invokeWithArguments(pathSeg, inputSeg, bytes.size.toLong(), outPtr, errPtr) as Int
            } catch (t: Throwable) {
                throw IllegalStateException("调用原生层失败: ${t.message}", t)
            }

            val outText = readCString(outPtr)
            val errText = readCString(errPtr)
            if (code != 0) {
                throw HermesException(errText ?: "执行失败(code=$code)", code)
            }
            return outText ?: ""
        }
    }

    /** 读 C 字符串并立刻归还原生内存（用原生层的 free，不用宿主 GC 猜）。 */
    private fun readCString(slot: MemorySegment): String? {
        val p = slot.get(ValueLayout.ADDRESS, 0)
        if (p == MemorySegment.NULL) return null
        val text = p.reinterpret(Long.MAX_VALUE).getString(0)
        free.invokeWithArguments(p)
        return text
    }

    override fun close() = arena.close()

    companion object {
        /** 按 HERMES_HOME → 仓库 build/ 的顺序找动态库，找不到时列出试过的路径。 */
        fun findLibrary(explicit: String? = null): String {
            val os = System.getProperty("os.name", "").lowercase()
            val name = when {
                os.contains("mac") -> "libhermesrt.dylib"
                os.contains("win") -> "hermesrt.dll"
                else -> "libhermesrt.so"
            }
            if (explicit != null) {
                if (Files.exists(Path.of(explicit))) return explicit
                throw HermesException("指定的库不存在: $explicit", 0)
            }
            val tried = mutableListOf<String>()
            System.getenv("HERMES_HOME")?.let { tried.add("$it/lib/$name") }
            tried.addAll(listOf("build/$name", "../build/$name", "../../build/$name"))
            for (p in tried) if (Files.exists(Path.of(p))) return p
            throw HermesException(
                "找不到 $name。已尝试：${tried.joinToString(", ")}。" +
                    "发布包应自带 lib/；开发时先跑 scripts/build.sh，或用 HERMES_HOME 指向安装根。",
                0,
            )
        }
    }
}
