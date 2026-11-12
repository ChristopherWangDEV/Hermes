# C ABI 约定

生成的头（`hermes header --c`）里只有一份核心头：`hermes/hermes_core.h`。
本文写清楚**谁分配、谁释放、什么能跨线程**——C API 最容易被踩的就是这三件事。

## 所有权表

| 对象 | 谁分配 | 谁释放 | 备注 |
|---|---|---|---|
| `char*`（`hermes_rt_run` / `hermes_rt_run_file` 的 `out_ptr` / `err_ptr`） | **库** | 调用方，用 `hermes_rt_free` | 用 `free()` 是错的：分配器不一定相同 |
| `err_ptr` 的错误文本 | 由 `hermes_rt_run` / `hermes_rt_run_file` 返回，属于**本次调用** | 用 `hermes_rt_free` 归还 | 与 `hermes_last_error()` 是**两条独立通道**，不要混为一谈 |
| `hermes_last_error()` 的错误文本 | 库拥有的**静态共享错误槽**：不分配、不转移所有权 | **调用方不 free**，也不长期持有指针 | 返回 `hermes_str{ptr,len}` 指向静态 `char[256]`，`ptr` 恒非 NULL；无错误时 `len == 0` 且 `msg[0] == 0`。**在下次调用前有效**。C11+ / MSVC 下槽是线程局部；`-std=c89` / `-std=c99` 下 `HERMES_THREAD_LOCAL` 展开为空 ⇒ **进程级全局**，并发调用会串错误（见「错误处理」与「线程安全」） |
| `hermes_buf input_json` | 调用方 | 调用方 | 库只读，不持有 |
| `hermes_app_*`（不透明类型） | 库 | 库（通过 `*_free`） | 默认不透明指针过边界（见上方「`@frozen`/`@opaque` 是布局承诺，不是注解」）；当前无字段展平 |

**一句话**：从库里**分配出来**的字节，必须用库的 `hermes_rt_free` 还；
`hermes_last_error()` 返回的视图不属于这一类 —— 它借的是静态槽，不 free。
这跟 `malloc`/`free` 不能跨分配器是同一个道理。

## 标准库实现的 g*/G* 命名（App 层可选）

> 规范性细则见 **`docs/STD-VARIANT.md`**。

- **语言主体 = LLVM / Rust / C / C++，不是 Go**；标准的 `std` 库实现是 **App 层可选件**。
- 每个 `std` 成员的 **Go 版实现**走 `g*`/`G*` 命名空间（`gnet` / `gmodel` / `gruntime` / `gjson`），与纯 C / Rust 的**非 Go 版**（成员本名 `net` / `model` / `runtime` / `json`）做命名隔离。
- App 层经 `HERMES_STD_VARIANT` 运行期二选一，**默认 `go` = 今天的行为**；该开关不写进 IR、不改 `requires` 哈希，否则破坏供应链防投毒与可重现性。
- 引用这些符号时按所选变体取对应名字：**绝不能把 App 层写成「= Go」或「全仓 Go 化」**。

> **`@frozen` / `@opaque` 是布局承诺的两种状态的描述，不是可写的注解。**
> 当前语法（§4.7）没有任何 `@frozen` / `@opaque` 注解——注解闭集只有
> `@enum` / `@pattern`，且只作用于 `string` 字段，未知注解一律 `E4015`
> （可自证：`grep -nE "@frozen|@opaque" docs/LANGUAGE.md` 无任何命中）。
> 所以今天**所有**跨边界的 struct 一律按不透明指针过（这也是 `gen_c.go`
> 的实际行为：所有导出类型生成 `typedef struct hermes_<unit>_<T> hermes_<unit>_<T>;`）。
> 等 §4.7 正式引入这两个注解，本节里涉及 `@frozen` / `@opaque` 的三条规则
> （"加字段即 break"、"默认只过指针对"）才生效。下面各处出现这两个词时，
> 都指向这一段，不再逐处重复。

## 错误处理

- 所有函数返回 `hermes_status`：`HERMES_OK == 0`，其余见 `hermes_core.h` 的枚举
- 失败时 `out_json` **不被赋值**（不是置为半成品）
- 失败详情由 `hermes_rt_run` 的 `err_ptr` 返回，**属于本次调用**，调用方自己持有
- `hermes cc` 的 per-unit 入口（`hermes_<unit>_<wf>`）失败时不回传 `char*`：
  shim 在 `free(err)` **之前**把错误文本 bounded-copy 进共享错误槽，宿主随后调
  `hermes_last_error()` 读取；unit AOT 成员在 `hm_fail` 出口同样发布错误对象。
  两条写方的返回值都恒为 `HERMES_ERR_INTERNAL`，**不把 AOT `code` 映射成
  `hermes_status`**（两套码表不混用，见 `docs/AOT-ERROR-OBJECT.md` §5）

### `hermes_last_error()` 的槽、所有权与线程局部降级

`include/hermes/hermes_core.h:74` 承诺「线程局部」，其实现是
`internal/header/core_impl.go` 里一个 **external-linkage 变量**（不是函数，故没有
新增 `hermes_core.h` 导出函数、没有触发公开 ABI minor bump）：

```c
#define HERMES_LAST_ERROR_CAP 256
struct hermes_error_slot {
  int32_t code;
  int32_t status;
  size_t len;
  char msg[HERMES_LAST_ERROR_CAP];
};
HERMES_THREAD_LOCAL struct hermes_error_slot hermes_last_error_slot = { 0, 0, 0, { 0 } };
```

- **无指针成员、静态存储期**：沿用 `HermesError` 那套价值观。没有 `malloc` / `strdup`，
  错误文本按固定 CAP bounded-copy（`memcpy` + 显式 `len` + NUL 终止），不会复制出悬空
  指针，也不会出现「`strlen` 静默截断后 `len` 还声称原长度」。内嵌 NUL 由 `len` 表达。
- **所有权**：`hermes_last_error()` 返回指向 `msg[]` 的 `hermes_str{ptr,len}`，
  调用方 **不 free**（也没有对应的 free 函数）；`ptr` 恒非 NULL，无错误时 `len == 0`
  且 `msg[0] == 0`，`printf("%.*s")` 安全。每个入口调用先清槽，所以视图只在
  **下次调用前有效**，成功调用后 `len == 0`。
- **跨语言可读**：Python / Go / Java / C# / Node / Rust 的 FFI 只需读 `ptr` 前 `len`
  个字节；不需要匹配分配器、不需要释放、不需要复制后才敢用。
- **线程局部的真实边界**：`HERMES_THREAD_LOCAL` 来自 `hermes_core.h`，C11 `_Thread_local`
  / MSVC `__declspec(thread)` 下槽是线程局部；但 **`hermes cc` 用 `-std=c89`、unit AOT
  用 `-std=c99`，这两种方言下宏展开为空 ⇒ 槽退化为进程级全局，并发调用会串错误**。
  这与头文件「线程局部」的承诺存在**方言相关的降级**，不是"错误不再有线程局部缓冲"。
  同一链接单元里的所有翻译单元必须用同一方言，否则宏展开不一致。
- **链接**：`hermes cc` 每次都把 `CoreImpl` 写出的 `hermes_core_impl.c` 编进产物（强定义）；
  unit AOT 产物自带一份 **weak 定义**，单独编译链接也能解析槽符号；同时链接两者时
  强定义胜出，所有 TU 共享同一个槽。
- **7 语言 SDK 不变**：它们走 `hermes_rt_run_file` 的 `err` 出参，错误文本本就完整；
  本次没有改 `packages/` 下任何 SDK。

## 字符串

`hermes_str` 带 `len`，不是裸 `const char*`。因为值里可以有 NUL，
而"跨语言边界上被静默截断"是最难查的一类 bug。
`hermes_last_error()` 正是按这条约定返回视图：长度只以 `len` 为准。

## 线程安全

| 对象 | 线程安全 |
|---|---|
| `hermes_rt_free` | 是（对不同的指针） |
| `hermes_rt_run` | 是（无全局可变状态） |
| `hermes_last_error()` 的共享槽 | **C11+ / MSVC：线程局部**（每线程一份）；**`-std=c89` / `-std=c99`：进程级全局**（`HERMES_THREAD_LOCAL` 展开为空），并发调用会互相覆盖。本仓 `hermes cc` 与 unit AOT 分别走 c89 / c99，故**当前产物是后者** |
| 同一个 unit 的并发调用 | **否** —— unit 内部有可变的执行状态 |
| 不同 unit 之间 | 是 |

需要并发就开多个 unit，或者用 `hermes serve` 走 HTTP 面。

## 兼容性承诺

先分清是哪一层的"版本"——本文件后面「C ABI 有两层」那一节把两层摊开讲，
混着看必然算错版本号：

| 层 | 版本号 | 判据 | 现在是多少 |
|---|---|---|---|
| 编译器内部 ABI（`native/include/hermes_abi.h`，只有 Go 看） | `HM_ABI_MAJOR.MINOR`，`hermes version` 的 `abi` 字段 | `hm_runtime_init` 要求 **major 相等**（`HM_E_ABI_VERSION`） | **2.0** |
| 产物契约：`.hbc` 成员的 `abi_version` 与 IR 里 `requires[].abi_version` | 一个整数（= 上面那个 major，minor 恒 0） | 链接器要求**完全相等**（`E_LINK_ABI_MISMATCH`） | **2** |
| 运行期公开 ABI（`include/hermes/hermes_core.h`，所有宿主语言看） | 产品 semver（布局承诺注解见上方说明，当前未引入） | 加字段就是 break（注解引入后；见上方说明） | 未变（当前一律不透明指针） |

- **`abi_version` 必须完全相等，不是 major 相等。** 这条 2.0.0 之后依然成立，
  而且正是它让"只改一半"变得不可能被忽略：前端写 2、链接器还认 1，产物当场
  被拒。（`major 相等就行`只适用于 `hm_runtime_init` 那道闸——那里比较的是
  同一个 `.so` 的编译期常量，不是跨越分发链的产物声明。）
- （以下两条是未来 `@frozen` / `@opaque` 注解引入后才生效的规则，见上方「`@frozen`/`@opaque` 是布局承诺，不是注解」；今天一律不透明指针过边界。）
- `@frozen` 结构体是**布局承诺**：加字段就是 ABI break，必须重新编译所有使用方
- `@opaque` 类型（默认）只有指针对过边界，加字段安全

### 2.0.0：布局没变，为什么还是把 `HM_ABI_MAJOR` 从 1 升到 2

结构体字段、函数签名、导出符号一个都没动——`hm_spawn_req` / `hm_spawn_res` /
`hm_sensitive_*` 与 1.0 逐字节相同。升号的是**语义**：

- 2.0.0 的 IR 用 `decl` / `const` / `fallback` 描述程序（`let` / `mut` 两个
  节点被替换），`ir_version` 从 1 升到 2，且**明确不做向前兼容**
  （`internal/ir/validate.go` 的 `checkVersions`：不是 2 就报 `E_IR_VERSION`
  并只报这一条）。
- `.hbc` 成员的 `abi_version` 与 `requires[].abi_version` 描述的不是"结构体
  多大"，而是**"这份字节消费哪一版 IR 形状"**。v2 的 IR 带 `decl` / `const` /
  `fallback`，1.x 的运行时按旧字段语义去读，得到的是"看起来对的错答案"。
- 所以继续打印 `abi 1.0` 的唯一后果是：**让 `abi_version=1` 这句话变成假话**。
  版本号的价值全在"它承诺了什么"，不在这串数字好看。

这条也顺带校准了一个容易读反的旧结论：**"IR 的结构变化不影响 ABI"是分层结论，
不是普遍结论**——对运行期公开层（C 面只看字节）成立，对编译器内部层与
`abi_version` 不成立。详见下一节。

### 版本号的唯一事实源（升版必须一次改全）

| 位置 | 内容 |
|---|---|
| `native/include/hermes_abi.h` | `HM_ABI_MAJOR` / `HM_ABI_MINOR` |
| `internal/native/ffi.go` | `ABIMajor` / `ABIMinor`（`tests/guard` 逐字比对头文件） |
| `internal/archive/archive.go` | `archive.ABIVersionSupported` —— `.hbc` 成员级与 `requires[]` 级 `abi_version` 的唯一来源；链接器与两个 emitter（`hermes ir`、`hermes build`）都引用它 |
| `tests/guard` | `TestGoConstantsMatchHeader` 同时钉住头文件 ↔ `ffi.go` ↔ `archive.ABIVersionSupported` |

`internal/link` 与两个 emitter 里**不许**再出现裸数字：散落的 `1` 是"下次升版
只改一半"的唯一原因。


## 已知的坑

**`#include` 报错一次，用户就走了。** 所以生成的头**只** include `hermes/hermes_core.h`，
不依赖任何系统头（`<stddef.h>` / `<stdint.h>` / `<stdbool.h>` 都不用）。
如果你在嵌入式工具链上发现缺某个 typedef，那是一个 bug，报给我们。

**不要在外面再包 `extern "C"`。** 生成的头已经包了；嵌套在标准里合法，
但某些 clang 版本在 `-pedantic` 下会警告。

## 测试状态

| 项 | 状态 |
|---|---|
| C89 + `clang -std=c89 -pedantic -Werror` | ✅ 实测（`examples/c-minimal`） |
| `hermes cc` 链接与符号自检 | ✅ 实测 |
| CMake 集成（`cmake/hermesConfig.cmake`） | ✅ 实测：`cmake -S . -B build -DHERMES_ROOT=…` → 构建 → 运行 |
| `-fno-exceptions` 的 C++ 头 | ⚠️ 未实测 |
| 交叉编译（ARM / RISC-V） | ❌ 未支持，也没试过 |


## CMake 集成为什么不能直接链静态库

`hermes cc` 会**为每个 unit 生成一份 C shim**（把 per-unit 符号接到
`hermes_rt_run`，并在失败时把错误文本发布进共享错误槽），同时把 `CoreImpl`
写出的 `hermes_core_impl.c`（`hermes_buf_free` / `hermes_last_error` / 错误槽的
强定义）一起编译链接。所以 `target_link_libraries(app hermes::runtime)` 是不完整的 ——
手写 clang 会漏掉 shim 与 `hermes_core_impl.c`，症状是链接期报
`hermes_<unit>_<wf>` / `hermes_last_error` 找不到。

`cmake/hermesConfig.cmake` 因此提供两个函数：

```cmake
hermes_add_unit(headers units/label.hermes)   # 编 .hbc + 生成 C 头 + include 路径
hermes_link("${CMAKE_CURRENT_BINARY_DIR}/app" main.c units/label.hermes)  # 走 hermes cc
```

`examples/c-minimal/` 是完整的可运行例子（实测通过）。

## IR 兼容性 ≠ ABI 兼容性

这两件事容易混，混了就会算错版本。**先问一句：这个变化对哪一层可见？**
"IR 变了 ABI 就不变"是分层结论，不是普遍结论：

| 变化 | 编译器内部 ABI（`HM_ABI_MAJOR` / `abi_version`） | 运行期公开 ABI（`hermes_core.h`） | 影响什么 |
|---|---|---|---|
| IR 新增字段/节点（如 `functions[]`、`call_fn`、`auth`） | **不影响** | **不影响** | `ir_version` |
| IR **改名/改变语义**（2.0.0：`let`→`decl`、`mut`→`const`、新增 `fallback`） | **影响：1 → 2** | 不影响（C 面只收到字节与 JSON） | `ir_version` + `abi_version` |
| `hermes_core.h` 新增导出函数 | 不影响（不同头文件） | 影响（1.0 → 1.1） | 运行期公开 ABI |
| `hermes_str` / `hermes_buf` 加字段 | 不影响 | 影响（未来 `@frozen` 注解引入后；当前一律不透明，见上方说明） | 运行期公开 ABI |
| `hm_spawn_req` / `hm_spawn_res` 加字段 | 影响（`abi_size` + major） | 不影响 | 编译器内部 ABI |

为什么"新增"不影响而"改名/改语义"影响：C 面看到的是**字节**，IR 多一个可选
字段对调用方不可见（旧运行时读到新 IR 会报"未知节点种类"，那是 `ir_version`
的事）。但当**节点种类被替换、字段含义被改**，同一个 `.hbc` 字节流就需要另一版
解释器——`abi_version` 承诺的正是"这份产物消费哪一版 IR 形状"，所以它必须跟着升。
2.0.0 就是这一种。

所以：

- 加**可选字段**：`include/hermes/hermes_core.h` 不改、`abi` 不动，`ir_version`
  提一档，并在本文件记录"旧运行时的行为"
- 改**节点语义或节点种类**：`abi_version` 必须跟着 `HM_ABI_MAJOR` 一起升，
  三处常量（头文件 / `ffi.go` / `archive.ABIVersionSupported`）一次改全；
  golden（`schemas/golden/ir-*.json`）里的 `abi_version` 同步重生
- 若将来需要 `hermes_run_function` 这类**新入口**，才谈运行期公开层的 ABI bump


## 真实导出符号（唯一事实源）

导出符号来自**两种提供机制**，不要混成一处来源：

| 符号 | 谁提供 | 什么时候进产物 |
|---|---|---|
| `hermes_rt_free` / `hermes_rt_run` / `hermes_rt_run_file` | `libhermesrt.a`（来自 `cmd/hermesrt/main.go` 的 `//export`） | 链运行时库时 |
| `hermes_buf_free` / `hermes_last_error` | `internal/header/core_impl.go` 生成的 C 文件（`hermes cc` 每次都编译进用户程序） | 每次 `hermes cc` |
| `hermes_abi_major` | **不存在**（头文件 `:94` 明说；ABI 版本由 `hermes version` 的 `abi` 字段报告） | —— |

**共享错误槽不是导出函数**：`hermes_last_error_slot` 是 `CoreImpl` 里定义的
**external-linkage 变量**（`struct hermes_error_slot`，无指针成员、静态存储期），
不是 `hermes_core.h` 新增的声明。这正是本次能填实 `hermes_last_error()` 却
**不触发公开 ABI minor bump**（本文件「IR 兼容性 ≠ ABI 兼容性」表里
「`hermes_core.h` 新增导出函数 → 1.0 → 1.1」那一行）的原因。shim 与 unit AOT
产物以 `extern` 引用它，unit.c 另带一份 weak 定义以便单独链接；所有权规则与
`-std=c89` / `-std=c99` 下的线程局部降级见上文「错误处理」。

`libhermesrt.a` 那三条（`hermes_rt_free` / `hermes_rt_run` / `hermes_rt_run_file`）来自
`cmd/hermesrt/main.go` 的 `//export`，是静态库里实际存在的符号，链运行时库时进来。

`hermes_buf_free` 与 `hermes_last_error` 不是库符号——它们是 `core_impl.go` 里 `CoreImpl`
这个**字符串常量**生成出来的 C 源文件，`hermes cc` 每次都会把它和用户程序一起编译。
这件事是刻意的：**为什么不做成库**？因为 `hermes_buf_free` 的行为里含 `free()`
（见 `core_impl.go:10` 的注释），跨模块各带一份 `free` 是自找麻烦；做成库反而多一道
"库版本要和头版本配套" 的约束，而它们之间根本没有会变的东西。所以它们是
"生成成 C 文件" 而不是 "做成库"——这正是上表分两行存在的意义。

> **历史说明（只点一个名字，不连坐）：** 更早的文档里确实写过 `hermes_abi_major`，
> 且**它从未被实现**——头文件 `include/hermes/hermes_core.h:94` 明确写着"没有
> `hermes_abi_major()`"，ABI 版本由 `hermes version` 的 `abi` 字段报告；
> `internal/header/core_impl.go` 曾经定义过它、返回 `1u`，既与头文件矛盾、返回值也和
> 现在的 `HM_ABI_MAJOR = 2`（`native/include/hermes_abi.h:62`）不符，已删除。
> **注意**：`hermes_buf_free` 与 `hermes_last_error` 不在"已删除"之列——
> 它们既在 `hermes_core.h` 里声明（`:69`、`:75`），也在 `CoreImpl` 里实现，
> `examples/c-minimal/main.c` 就直接调 `hermes_buf_free`。照旧文档把这三个名字
> 捆在一起说"从未实现"，会让 C 用户误以为没有 `hermes_buf_free`，从而用 `free()`
> 去还库分配的字节——那正是「所有权表」明令禁止的未定义行为。

## C ABI 有两层：编译器内部层 + 运行期公开层（产品面）

**不要把它理解成"Go 和 C++ 之间的桥"。** 它有两层，别混：

| 层 | 头文件 | 消费方 | 符号面钉在 |
|---|---|---|---|
| 编译器内部 ABI | `native/include/hermes_parser.h`、`hermes_abi.h` | **只有 Go**（cgo 调 C++ 前端） | `scripts/expected_symbols_parser.txt` |
| 运行期公开 ABI（产品面） | `include/hermes/hermes_core.h`（手写、冻结）+ 每个 unit 由 `hermes header --c` 生成的 `<unit>.h` | **所有宿主语言** | `scripts/expected_symbols.txt` |

运行期公开层是**所有宿主语言共用的一份契约**，逐个核过（不是推测）：

- **C/C++ 宿主程序**：`hermes cc` 直接把用户自己的 C 程序链到 `libhermesrt`（`cmd/hermes/cmd_cc.go`）
- **Go**：cgo（`packages/go/native.go`）
- **Python**：ctypes 加载 `libhermesrt`（`packages/python/hermes/native.py`，注释写明"用 ctypes 因为它是标准库"，故意不引 pybind11/sip）
- **Java**：JDK 22 的 `java.lang.foreign`（FFM），**不用 JNI**（`packages/java/src/dev/hermes/NativeRuntime.java`）
- **C#**：P/Invoke（`packages/csharp/NativeRuntime.cs`）
- **Node/JS**：N-API 插件 `packages/javascript/native/hermes_native.c`，`scripts/build_node_addon.sh` 一条 clang 命令编出，**故意不引 node-gyp / node-addon-api**（它们要 npm 拉依赖）
- **Rust**：FFI

这层存在的原因是产品承诺：宿主语言不装 Hermes 运行时、不起 `hermes serve`，进程内直调 `.hbc`/IR。改它等于改对外承诺，**比改语言语法更重**。

### 不可违反的几条（都有源码里的理由，别凭直觉改）

- `hermes_core.h` 是**手写、进版本库、冻结**的。**不许**改成由生成器产出核心类型 —— 两个 unit 的头被同一个翻译单元 include 时就会重复定义。
- 方言是 **C89 + `<stdint.h>`**：不许 `//` 注释、不许声明放块中间、不许 `long long`、不许 `_Thread_local`（用头里的 `HERMES_THREAD_LOCAL` 宏吸收 MSVC/C89 差异）。
- 生成的头**只 include `hermes/hermes_core.h`**，不依赖任何系统头（理由见上文「已知的坑」：`#include` 报错一次，用户就走了）。
- `hermes_str` **必须带 len**，不是裸 `const char*`（见上文「字符串」：值里可以有 NUL，跨语言边界静默截断是最难查的一类 bug）。
- 所有权：从库里出来的字节必须用 `hermes_rt_free` 还，不能用 `free()`（见上文「所有权表」：分配器不一定相同）。
- 兼容：`hermes version` 的 `abi` 现在是 **2.0**（2.0.0 起；布局没变但承诺的 IR 形状变了，见上文），`hm_runtime_init` 要求 major 相等；而产物里的 `abi_version` 要求**完全相等**。（未来 `@frozen` / `@opaque` 注解引入后）`@frozen` 结构体加字段就是 ABI break，`@opaque`（默认）只有指针对过边界——当前一律不透明指针过边界，见上方「`@frozen`/`@opaque` 是布局承诺，不是注解」。
- 符号面漂移会被 `scripts/expected_symbols.txt` / `expected_symbols_parser.txt` 拦住，**别去改这两个文件来让构建变绿**。

### 工程纪律

改任何 C ABI 相关的东西（`include/hermes/`、`libhermesrt` 的导出、`hermes header` 的产出），受影响的是**七个语言的 SDK + 第三方 C 程序**，不是"只有 Go"。改完要跑 `scripts/test_sdks.sh`。
