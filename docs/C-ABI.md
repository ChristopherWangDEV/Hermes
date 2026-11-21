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

结构体字段、函数签名、导出符号一个都没动——`hm_spawn_req` / `hm_spawn_res` / `hm_sensitive_*` 与 1.0 逐字节相同
（`hm_spawn_res` 已在 F-11b 删除，见下方「F-11 那一版的 `hm_spawn_res.ignored`——留档」；
这句话说的是 2.0.0 当时的形状，不做追溯修改）。升号的是**语义**：

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
| `hm_spawn_req` **中间插字段 / 改字段含义** | 影响（偏移变了，必须升 major） | 不影响 | 编译器内部 ABI |
| 结果结构体**尾部追加字段**（F-11 的 `hm_spawn_res.ignored`，该结构体 F-11b 已删除） | 不必升 major：R8 的 `abi_size` 按 min 读写，旧读方忽略尾部；新读方用 `abi_size` 守卫认出旧库 | 不影响 | 编译器内部 ABI |

> 第二行是**历史规则示例，不是现行形状**：`hm_spawn_res` 在 F-11b（v4）被整体删除，
> 结果面改成不透明句柄 `hm_handle` + 逐项 getter，今天**没有**任何结果结构体可供
> 尾部追加。保留这行是因为它描述的规则（尾部追加 ≠ 改偏移）仍然适用于请求面
> `hm_spawn_req` 与将来任何带 `abi_size` 的 struct。详见下方
> 「F-11 那一版的 `hm_spawn_res.ignored`——留档」。

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


## F-11 那一版的 `hm_spawn_res.ignored`——留档（该结构体已删除）

> **这一节是历史，不是现行形状。** F-11b（v4，不透明句柄 + Getter）已经把
> `hm_spawn_res` / `hm_setup_status` 两个结果类型，以及 `hm_spawn_wait` /
> `hm_spawn_res_free` / `hm_spawn_cancel` / `hm_spawn_free` 四个符号**整条删除**，
> 结果面改成「一个 `hm_handle`（8 字节，本来就过边界）+ 13 个 out-of-line getter」。
> 下面留的是**论证过程**：它解释了「尾部追加 + `abi_size` 守卫」当时为什么是
> 合格答案，以及这个答案最终为什么仍被否决（结论见节末）。
> **别照着下面写新代码**；现行形状见下一节「F-11b 已闭合（v4：不透明句柄 + Getter）」。

2026-11 的 F-11 修复曾把「平台请求了但没能施加的沙箱位」结构化带回 Go 侧：
`hm_spawn_res` 在 `stderr_len` **之后**追加一个字段（offset 88）：

```c
uint32_t ignored; /* bitmask of HM_SB_* flags the platform could not apply */
```

值由 `hermes-shim` 经 fd 3 的 8 字节带 tag 记录
（`[0x484D4947][ignored_flags]`，两者都是小端 u32）回报，
`supervisor.c::pump_setup` 累加后填进 `hm_spawn_res.ignored`，Go 侧
`Result.Ignored` 再参与 fail-closed 判据。既有语义不变：fd 3 的
「4 字节 errno = 失败」「EOF = execve 成功」两条都保留，新记录是**加性**的。

- **当时的 `HM_ABI_MAJOR` / `HM_ABI_MINOR` 不动（仍是 2 / 0）。** 那次既不是
  「改字段含义」也不是「中间插字段」：R8 规定每个 struct 首字段是 `abi_size`、
  收发双方按 `min(自身 sizeof, 传入 abi_size)` 读写，所以**尾部追加**对旧读方
  完全不可见，不需要升 major。上表里「加字段 → 影响 major」那一行指的是
  **改偏移 / 改语义**，与尾部追加是两件事，别混着读。
- **当时的 `abi_size` 全部由 `sizeof` 派生。** `sizeof(hm_spawn_res)` 从 88 变成
  96，`supervisor.c` / `win_spawn.c` / `internal/native/exec.go` 里的 `abi_size`
  都跟着自动走。**如实补记**：后续 A 组第 2 项为消除 Windows `abi_size=88` 的
  越界写，单独把 `win_spawn.c::hm_spawn_wait` 的写回改成 `tmp` +
  `memset(out,0,nbytes)` + `memcpy(out,&tmp,nbytes)` —— 那是**越界写加固**，
  与字段追加是两件事，当时不能写成「`abi_size` 相关代码全程一行未改」。
  **v4 之后整条写回路径消失了**：`hm_proc_wait(proc)` 一个字节都不写回给调用方，
  「按调用方声明的 `abi_size` 往回写」这个失效模式**按构造不可能发生**。
- **字段必须追加在尾部，不能塞进 offset 52 的 padding 空洞。** 塞空洞时
  `sizeof` 仍是 88，新消费者无法凭 `abi_size` 认出旧库，也就无法触发那道守卫。
- **build id 必须重算——这条今天仍然生效，且与 v4 无关。**
  `native/rust/src/lib.rs` 的 `ABI_BUILD_ID` / `ABI_BUILD_ID_CSTR` 是
  `native/include/` 下**全部** `.h`（当前 4 个：`hermes_abi.h`、`hermes_net.h`、
  `hermes_parser.h`、`hm_internal.h`）的内容哈希：按 `LC_ALL=C` 排序后逐文件喂
  `basename + "\n" + 内容 + "\x00"`，取 sha256 前 16 个十六进制字符，前缀
  `hermes-abi-`。由 `tests/guard::TestABIBuildIdMatchesHeaders` 逐字复算
  （该测试直接 `os.ReadDir` 目录，**不写死文件名清单**——所以往目录里加头文件
  也会自动被算进去）。改了头却忘记更新常量，那条守卫会报出 `want`；
  把 `want` 同时写进两处即可。
  **本轮不许照抄文档里的旧值**：更新后必须重新跑那条守卫（或按上面的算法手算）
  确认，不能「看着像就填」。`hermes version` 的 `build_id` 与
  `hermes-shim --self-test` 的 layout fingerprint 都由构建自动注入，不用手改。

### F-11 当时的混合版本矩阵（历史；v4 已把它整张作废）

下表是 **F-11 当时**的 QA 步骤，整张读作过去时：行文里的「现在」指的是 F-11 那个
commit 之后、F-11b 之前。`hm_spawn_res` 已不存在，这些格子今天无法复现。

| Go 二进制 | 原生库 / shim | 期望 |
|---|---|---|
| 新（读 96） | 新（`abi_size=96`） | 正常；`ignored` 参与按位交集判据 |
| 新（读 96） | 旧库（`abi_size=88`） | **硬拒**：`min` 只拷 88 字节，尾部 `ignored` 保持 Go 零值 0；`abi_size < sizeof` 守卫把它当「ignored 未知」拒绝，绝不「没忽略 → 放行」 |
| 旧（读 88） | 新库（`abi_size=96`） | **min-size 读写成立，但须如实区分两侧成立的时间点，不得无条件宣称两侧一直成立**：**POSIX 一直成立** —— `supervisor.c::hm_spawn_wait` 从来都把结果填在局部变量 `tmp` 后按 `nbytes = min(out->abi_size, sizeof(hm_spawn_res))` 写回；**Windows 是本次 A 组第 2 项修复后才成立** —— 修复前 `win_spawn.c::hm_spawn_wait` 在 `memcpy(out, &tmp, nbytes)` 之后还直接赋值 `out->abi_size`，`abi_size=88` 时写 offset 88 即越界写。现在 **POSIX 与 Windows 两侧均**先填 `tmp`（含 `tmp.abi_size=96`、`tmp.ignored=0`），再做一次 `memset(out, 0, nbytes)` + `memcpy(out, &tmp, nbytes)`，之后不对任何 `out->` 字段赋值；旧消费者（88）只读旧字段、新字段留在 `tmp` 不写出，故无越界写；混合版本 `abi_size` 语义见下方矩阵 |

第二行是 F-11 当时最重要的回归：新 Go + 旧库时**不能**静默放行。
第三行的「min-size 读写」在 Windows 上**不是** F-11 就有的能力，而是要
A 组第 2 项（第二个 commit 及其后续对齐）把写回改成 `tmp` + `memset`/`memcpy`
之后才成立；本文件当时就不把修复前的 Windows 行为写成「一直成立」。

### v4 之后：这张矩阵缩成一格——**链接期硬失败**

v4 不是「把上面三行都修好」，而是**把那个决策点删掉**。结果面没有任何结构体，
也就没有「按谁的 `abi_size` 读」这回事：

| 组合 | 结果 |
|---|---|
| 新 Go（调用 13 个 getter）+ 新库 | 正常 |
| 新 Go（调用 13 个 getter）+ **旧库**（没有那 13 个符号） | **链接期 `undefined reference`**。不是运行期拒绝，是产物根本链不出来 |
| 旧 Go **源码**重新链接到新库（调 `hm_spawn_wait`，符号已删） | 同样**链接期失败** |
| 已经编好的旧二进制 + 新库放在同一目录 | 没有"换库"这回事：Go 静态链接，二进制自带旧库副本，不受影响 |

这是 D1/D3 的全部意义：判据从**代码里的 `if`** 挪到了**链接器**。
守卫写漏一次就永久失效，而「符号不存在」不可能被忘记检查。

- F-11 矩阵里「旧 shim ⇒ 静默成功」那一格的风险也随之收口：新 Go + **旧 shim** 时
  旧 shim 不写 `[HMIA]` ACK ⇒ `HM_SETUP_EXEC_CONFIRMED` 恒 0 ⇒ 请求关键位时
  `reason=no_ack` 拒绝；被信号杀死时则落到 `ErrToolNotStarted`。
  **仍然只是「事后拒绝」，不是「事前阻止」** —— 旧 shim 已经把目标 execve
  掉了。这条边界在任何版本的 Go 侧都无法靠 Go 自己解决，见
  `docs/KNOWN-GAPS.md`。

### F-11b 已闭合（v4：不透明句柄 + Getter，2026-11）

F-11 登记的三项残余已由 F-11b 闭合。闭合手段**不是**给结果结构体加字段，也不是
再发明一个更小的结构体 —— 而是**把「结构体承载返回值」这个契约本身废掉**：

- `hm_spawn` 返回的 `hm_handle` 是**唯一**的结果容器；
- 一次调用发生了什么，全部经 `hm_proc_get_*` / `hm_proc_copy_*` 从 handle 读；
- 删除 `hm_spawn_res`、`hm_setup_status` 两个类型，删除 `hm_spawn_wait` /
  `hm_spawn_res_free` / `hm_spawn_cancel` / `hm_spawn_free` 四个符号，
  新增 13 个 `hm_proc_*`（导出面 25 → 34）。

这是 `hermes_abi.h` 顶部 **R8 的一次性例外 2b**，成立条件只有一个：**已发布
消费者集合为空**。依据是 `scripts/release.sh:33-65` 的发布包内容清单 —— 该区间
只拷 `bin/hermes`、`bin/hermes-shim`、`lib/libhermesrt.a`、
`lib/libhermesrt.{dylib,so}`、`lib/hermes_rt.h`、`include/hermes/{hermes.h,hermes_core.h}`；
**`libhermes_native.a` 与 `native/include/hermes_abi.h` 从不进入发布包**，
`include/hermes/` 下的两个头也不引用被删符号。所以破坏形态是**编译/链接期硬失败**，
不是运行期静默误读。`HM_ABI_MAJOR` 保持 2（跨边界字节的**解释**未变），
`HM_ABI_BUILD_ID` 必然变化（**形状**变了）。未来任何删除必须提交**同等强度**的证据。

闭合方式（逐条对应原登记）：

1. **P0 正面握手（不合作 shim）** -> shim 在关键位全部生效后写 8 字节
   `[HMIA][applied]`；父进程 `pump_setup` 按位累加进 `hm_proc.sb_acked`，getter
   （`hm_proc_get_sb_acked`）带回 Go。判据：请求了关键位就必须
   `acked ⊇ requested & critical`，否则 `reason=no_ack` 拒绝。
   「没有 ACK」不再等于「没问题」，而是「没有证据」。
2. **B 组负面语义信号（`exit 40` 误拒）** -> setup 失败改由 8 字节
   `[HMIF][errno]` 表达（-> `HM_SETUP_FAILED`），**`exit_code == 40` 这条启发式
   整条删除**，`hmShimExitSetup` 常量只允许出现在守卫测试里；execve 失败仍走
   既有的裸 4 字节 errno（语义未变，且**不置** `setup_failed`，改置
   `HM_SETUP_EXEC_FAILED`）。shim 的解析失败 / 未知选项 / 缺 `--` 也全部改走
   `report_and_exit`，所以「setup 阶段失败」与「业务自己 `exit(40)`」在协议层可分。
3. **fd 3 的 EOF 语义耦合（exec 成功 vs shim 死亡）** -> 不改通道，改读**时序**：
   `pump_setup` 之后、`waitpid(WNOHANG)` 之前取 `setup_eof`；若 EOF 已到而子进程
   仍存活，则 `setup_eof_while_alive = 1`（getter 里是 `HM_SETUP_EXEC_CONFIRMED`）。
   v4 把它从**拒绝依据**降级为**结局依据**（见下面两节）。

判据顺序（顺序本身是判据的一部分；v4 由 7 条组成，**deny 4 条**）：

```text
1. state != HM_PROC_REAPED                     -> ErrInvalidArgument（程序错误，不是安全事件）
2. sb_requested != requested                   -> reject reason=requested
3. setup_bits & HM_SETUP_FAILED                -> reject reason=setup_failed
4. setup_bits & HM_SETUP_EXEC_FAILED           -> allow（目标从未启动，无未沙箱执行）
5. (requested & critical) &^ sb_acked != 0     -> reject reason=no_ack
6. sb_ignored & requested & critical != 0      -> reject reason=ignored_critical
7. allow
critical = HM_SB_NO_NET | HM_SB_READONLY_TMP
```

**第一条不再是 `abi_size`**。没有大小可协商，就没有「旧库按旧大小写」；新 Go + 旧库
在**链接期**就 undeclared/undefined reference。删掉那条守卫是**设计收益**，不是
覆盖缺口（`internal/native` 有一条 `TestF11ABISizeGuardIsGoneByDesign` 专门记录
"它真的不在了"）。`setup_errno` 明确不参与（函数体内 `_ = setupErrno`，有元守卫）。

第 3 条早于第 5/6 条：真实 shim 里「setup 阶段失败」必然同时没有 ACK，两条同时
为真；先说清是哪一阶段失败，归因更有用。

**第 6 条（`ignored_critical`）今天几乎只在"自相矛盾的 shim"上可达** —— 这一点在
v4 的早期文档里被写反过，必须说清：

真实 shim 发的 ACK 是 `acked = requested & ~ignored`（`shim.c` 里
`shim_report_acked(sandbox_flags & ~ignored_flags)`）。也就是说，**一个诚实的
shim 只要把某个关键位报成 ignored，它同时就不会把该位放进 ACK**，于是判据⑤
（`critical &^ acked`）先命中，reason 是 `no_ack`。第 6 条要命中，必须出现
"既报 ignored、又在 ACK 里声称施加了"的同一位 —— 那是自相矛盾的输入。

所以第 6 条是**纵深防御**，不是与 ACK 互补的第二份证据。早期写法"`ignored` 让
『只撒谎说全施加了』的 shim 撒不圆"是**假的**：QA 实测一个骗子 shim（吞
`--sandbox`、什么都不施加、往 fd 3 只写 `[HMIA][0x3]`、一个 `[HMIG]` 字节都不写）
得到了 `Acked=0x3 Ignored=0x0` 并被放行，目标在**完全无沙箱**状态下跑完 ——
它内部完全自洽，`ignored` 没有任何交叉校验能力。详见下面「信任边界」。

**残差 `F-11b-R1`（**已实测，不再是"静态推演"**）：`HM_SETUP_EXEC_CONFIRMED`
是**采样**出来的，不是明示的。supervisor 每 2ms 跑一次
`pump_setup → 读 setup_eof → waitpid(WNOHANG)`，而快工具的 `execve → exit`
常常落在同一个 tick 里，采样点晚于死亡就确认不了。

替身 shim（shell 脚本，30 次/轮）：

| 目标 | `ExecConfirmed=1` |
|---|---|
| `/usr/bin/true` | 8/30、18/30、11/30、11/30 |
| `/bin/echo hi` | 1/30、18/30、13/30、16/30 |
| `/usr/bin/false` | 22/30（补测一轮） |
| `/bin/sh -c 'exit 40'` | 30/30 ×4（sh 启动够慢，跨得过一个 tick） |

**生产路径（真实 `build/hermes-shim`，20 次/轮，各轮独立）**：

| 目标 | 各轮 `ExecConfirmed=1` | 跨轮区间 |
|---|---|---|
| `/bin/echo hi` | 0/20、0/20、8/20（实现者）；2/20、4/20、18/20（QA，C 探针）；5/20（QA，Go 路径） | **0/20 ~ 18/20** |
| `/usr/bin/true` | 0/20、0/20、1/20（实现者）；1~2/20（QA） | **0 ~ 2/20** |
| `/bin/sh -c 'exit 40'` | 20/20 ×3 + 20/20（QA） | **80/80** |
| `/bin/sh -c 'sleep 0.05; exit 0'` | 20/20 ×3 + 10/10（QA） | **恒 20/20** |

**波动本身就是"采样推断"的证据**：同一目标、同一构建，跨轮能从 0/20 跳到 18/20 ——
这个位承载不了"没执行"，只能承载"没观测到"。所以去掉 `Signaled` 的后果不是"偶尔
误报"而是**常态化**误报（echo 类快工具在真实 shim 下大多数轮次几乎一次都确认不到）。
**这是 R1 的量化形态，不是 flake。**

`ExecConfirmed` 的缺失因此有两种截然不同的含义，判据必须只认其中一种：

- **可判定**（判据命中，正确）：「被信号杀死 + 没确认 exec」⇒ 工具没启动。
  `Signaled` 的作用是把命题收窄到这一半 —— 它是判据的**正当性来源**，
  不是"因为会误报所以先留着"的临时妥协。
- **不可判定**（不归任何判据）：「干净退出 + 没确认 exec」。父进程侧没有任何
  信息能区分「跑得太快没采样到」（常态）与「shim 违约没 exec」，所以
  `Wait()` 接受它 —— 这不是漏报，是**归入信任边界**。

用一句话写死这条纪律：

> `ExecConfirmed=0` 只应在 `requested & 关键位 != 0` 且需要**归因**时被解释为
> 「**未确认**」，**不得**解释为「**未执行**」。要「未执行」这个事实，只有两条
> 路：信号死因（判据已覆盖），或协议级 fact（`F-11c`：shim 在 `execve` 前写一条
> dedicated 记录）。

## 信任边界：ACK 是 **shim 自报**的

这一节是 F-11b 最容易被读过头的地方，必须写在判据旁边而不是塞进附录。

**整套 fail-closed 只在 `hermes-shim` 二进制本身可信时成立。** fd 3 上的三个记录
（`[HMIG]` ignored / `[HMIA]` acked / `[HMIF]` setup 失败）全部由 shim **自己**
写。父进程没有任何独立手段验证"沙箱真的施加了" —— 它只能读 shim 的自述。

因此三种 shim 的命运不同：

| shim | 行为 | 父进程能做什么 |
|---|---|---|
| 合作 | 施加 → ACK → `execve` | 正常：ACK 覆盖请求的关键位，放行 |
| 不合作 | 吞掉 `--sandbox`、**一字不写** fd 3、直接 `execve` | **事后识破**：`reason=no_ack` 拒绝。但目标**已经 execve 了**，拒绝只是"不把它报告成成功" |
| **撒谎** | 什么都不施加、写 `[HMIA][requested]`、一个 `[HMIG]` 都不写、然后 `execve` | **完全无法识破**：`Acked=requested`、`Ignored=0`，判据⑤⑥都满足，目标在无沙箱状态下跑完并返回成功 |

**撒谎 shim 是实测过的，不是设想**（QA 独立实验）：一个脚本替身吞掉 `--sandbox`、
不施加任何沙箱、只往 fd 3 写 `[HMIA][0x3]`，得到 `Acked=0x3 Ignored=0x0`，
`Wait()` 返回成功，目标标记文件存在。它内部**完全自洽** —— `ignored` 提供不了
任何交叉校验，"判据再加一条"也解决不了：父进程没有任何一条信息是 shim 没告诉它的。

**根治手段不是判据，是身份与完整性**：
- 起进程前校验 `hermes-shim` 的身份（签名 / 路径白名单 / build id 与 `hermes`
  二进制一致）—— 属于 F-11c 与独立的"shim 可信启动"专项；
- 或者不依赖 shim 自述，改由内核侧证明（Linux 上 user namespace + 只读挂载能
  让"没施加"直接表现为目标起不来）。

**在那之前，唯一的保证是**：本仓库自己构建、自己安装的那对
`hermes` + `hermes-shim` 是可信的。**运维要求（硬性）**：
> **升级 `hermes` 必须同时升级 `hermes-shim`，两者版本不一致时不启动工具调用。**
> 判据只在"同版本"这一前提下有意义：旧 shim 不写 ACK（会被 `no_ack` 拒绝，
> 方向安全）；新 shim 配旧 `hermes` 则根本链不出来（缺符号）。混装不是"可能
> 有点问题"，是**保证没有防线**。

**混合版本矩阵（F-11b/v4 之后）**：

| 组合 | 期望 |
|---|---|
| 新 Go + 新库 + 新 shim | 正常；ACK 到达，判据 5/6 生效 |
| 新 Go + **旧库**（没有 13 个新符号） | **链接期硬失败**（`undefined reference`）⇒ 构建期 fail-closed，不可能带病出厂 |
| 新 Go + 新库 + **旧 / 不合作 shim**（不写 ACK） | 请求关键位 -> **拒绝**（`reason=no_ack`）。必须如实理解这是**事后判定**：不合作 shim 已经把目标 execve 掉了，F-11b 阻止不了它，只能拒绝把这次调用报告成成功 |
| 旧 Go + 新库 | **不可能存在**：Go 静态链接，旧二进制里没有新符号的引用 |
| 外部 C 消费者链 `hm_spawn_wait` | 编译/链接期硬失败（响亮失败，不是静默误读）；全仓无此消费者 |
| `dlsym("hm_spawn_wait")` | 返回 NULL。SDK 只 dlsym `hermes_rt_*` ⇒ 零影响 |
| Windows（新 Go + 新库） | `HM_SETUP_CHANNEL_MEASURED` 恒不置位 ⇒ `ErrToolNotStarted` **永不触发**；`Signaled` = cancelled 原样进 `Result` |

**被否决的方案（留档，别再提）**：把状态塞进 `hm_spawn_res` offset 52 的 padding
空洞；「把 ack 塞进 `ignored` 高位 / 复用任何现有字段的备用位」；再加一个更小的
结果结构体（v3 的 `hm_setup_status` 就是这么被推翻的）；固定大小的 builder API
（输入侧）。前两条会让两个字段的语义永久纠缠，且被 `hermes_abi.h` 的明文禁令与
用户明确否决挡住；第三条被用户**三次**否决（"不要固定大小结构体承载返回值"）。
「全局 last-call / thread-local 状态」同样被否决：它把「这一次 spawn 的证据」变成
进程级可变状态，并发下互相覆盖。

## Getter 面：一次调用发生了什么只能从 handle 读

13 个符号，全部 `HM_EXPORT` + **out-of-line**（`native/c/supervisor.c` 与
`native/c/win_spawn.c`）。**禁止**写成 `static inline` 或宏：那样不产生导出符号，
"新 Go + 旧库"就从链接期硬失败退化成静默误读，F-11 的 fail-open 洞原地重开。
这条有守卫（`TestF11bG1HeaderStaysClean`）。

| 函数 | 出参 | 说明 |
|---|---|---|
| `hm_proc_wait(proc)` | 无 | 阻塞收割；**无出参、无 abi_size** |
| `hm_proc_cancel(proc)` | 无 | 线程安全，立即向 -pgid 发 SIGTERM |
| `hm_proc_free(proc)` | 无 | `refcnt>0 \|\| waiting` ⇒ `HM_E_INVALID_ARG`；**已释放/非法句柄 ⇒ `HM_OK`（幂等无操作）**；wait 之前调用 = 取消 + 收尸 |
| `hm_proc_get_state` | `int32_t*` ∈ {1,2} | 唯一**不要求**已收割的 getter |
| `hm_proc_get_term` | 4 个出参 | 见下（D2） |
| `hm_proc_get_deadline` | `uint64_t*` | Windows 恒 0 |
| `hm_proc_copy_deadline_source` | `char*`,cap,len* | `"cycle"` / `"wall"` / `""` |
| `hm_proc_get_setup` | `uint32_t*`,`int32_t*` | `HM_SETUP_*` 位 + errno |
| `hm_proc_get_sb_requested` | `uint32_t*` | C 侧回读的请求位（判据 2） |
| `hm_proc_get_sb_acked` | `uint32_t*` | 正面握手位（判据 5） |
| `hm_proc_get_sb_ignored` | `uint32_t*` | 「请求了但没施加」（判据 6） |
| `hm_proc_copy_stdout` / `_stderr` | `uint8_t*`,cap,len* | 两阶段字节流拷贝 |

**位定义（位面是唯一的增长维度）**：`HM_PROC_{RUNNING,REAPED}`、
`HM_TERM_{EXITED,SIGNALED,ESCALATED,DEADLINE_HIT}`、
`HM_SETUP_{FAILED,EXEC_FAILED,EXEC_CONFIRMED,CHANNEL_MEASURED}`。
加一个新安全信号 = 加一个 `#define` + 在 C 侧置位；**0 新符号、0 结构体、0 签名
改动**（罕见时才加一个 getter）。位没有布局。

**`hm_proc_get_term` 为什么要四合一（D2）**：`exited` / `exit_code` /
`signaled` / `term_signal` / `escalated` / `deadline_hit` / `killed_pgid` 七项
全部派生自**同一枚 `waitpid` 状态字**。拆成多个 getter 会让「读到跨字段撕裂的
组合」成为可能（例如 `EXITED` 配一个属于另一次 wait 的 `exit_code`），那不是省
符号的问题，是原子性问题。实现上先全部读到局部变量、再统一写出。

**出参 NULL 规则（统一、无例外）**：任何标量出参为 NULL ⇒ `HM_E_INVALID_ARG`，
**一个出参都不写**；`get_term` 的 4 个、`get_setup` 的 2 个是"全有或全无"。
唯一例外是 `copy_*` 的 `buf`：NULL 是合法的两阶段第一阶段。

**未就绪真值表**：

| 时刻 | `get_state` | 标量 getter | `copy_*`（`buf==NULL`） | `copy_*`（`buf!=NULL`） |
|---|---|---|---|---|
| spawn 后、wait 前 | `RUNNING` | `HM_E_NOT_READY`，不写出参 | `HM_E_NOT_READY`，不写 `*out_len` | 同左 |
| wait 进行中（另一 goroutine） | `RUNNING` | 同左 | 同左 | 同左 |
| wait 返回后、free 前 | `REAPED` | 值 | `HM_OK` + 需要长度 | `HM_OK` / `HM_E_TOO_LARGE` |
| free 后 / 非法 handle | `HM_E_HANDLE_STALE` | 同左 | 同左 | 同左 |

"还没算出来"**在任何一格都不表现为 0**：要么 `HM_E_NOT_READY`，要么 `RUNNING`。
没有 `HM_PROC_FREED` 是刻意的：free 后 generation 自增，句柄变成"过期的名字"，
要把它表达成可读状态需要一张永不清理的墓碑表（无界内存）；而 R2 的 generation
用 O(1) 空间表达了同一件事，并把"对象不存在"正确归类为**错误**而不是**状态**。

**两条拷贝容量语义必须分开记（D6，混用就差一个字节的溢出）**：

- 字符串（`copy_deadline_source`）：`*out_len` 成功时 = 字符数（不含 `'\0'`）；
  需要 `cap = *out_len + 1`；`cap` 不足 ⇒ `HM_E_TOO_LARGE` 且 `*out_len` 写
  "所需 cap"，`buf` 一个字节都不写（R3：绝不截断）；`buf == NULL` ⇒ 只写长度。
- 字节流（`copy_stdout` / `copy_stderr`）：**没有** `+1`，没有终止符；
  `*out_len` 在所有路径上都是"需要的/已写的字节数"；`out_len == NULL` ⇒
  `HM_E_INVALID_ARG`。

R5 的"跨分配器 free"规则因此**整个消失**：内存始终在 C 侧槽位里，调用方只往里
`memcpy`，从不接手所有权，也没有任何 free 函数要配对。

**生命周期与发布屏障**：`hm_proc_wait` 就地写槽位，`p->reaped = 1` 是**最后一次
写**（原来是先发布再排空 stdout/stderr 约 140 行；那会让并发 getter 看到
`REAPED` + 半成品数据 —— "用 0 冒充"的另一种形态）。getter 一律
`hermes_proc_acquire`（`refcnt++`）→ 表锁内校验 `reaped` → 读到局部 → 解锁 →
`hermes_proc_put`；`hm_proc_free` 在 `refcnt > 0 || waiting` 时返回
`HM_E_INVALID_ARG`，所以 getter 在飞时被 free 不会产生 use-after-free。

**Windows 侧（D4）**：`win_slot` 之前**没有** `refcnt`/`waiting`，`hm_proc_free`
也不检查"是否有人在用" —— getter 化之后每次读都解引用槽位，那就是
free-during-getter 的 use-after-free。现已补齐 `slot_acquire` / `slot_put`，
语义与 POSIX 的 `hermes_proc_acquire` / `hermes_proc_put` **逐行镜像**。
⚠ **诚实标注**：本机没有 MinGW，`win_spawn.c` 整文件被 `#if defined(_WIN32)` 包住，
Windows 分支本轮是"**镜像式实现 + 代码审查**"，**无法编译、无法执行验证**。
任何"Windows 侧已验证"的表述都是假的。

## 「永不因为加字段而改 ABI 的形状」

`hermes_abi.h` 的 R8 第 2 条列了四条新增路径，其中 (c)——"新增**不接收也不返回
布局结构体**的导出函数"——是 F-11b 之后的主力路径。原因是结构性的：

- **请求面**已经是位掩码：`hm_spawn_req.sandbox_flags` 是 `uint32_t`，当前只用
  3 位，**剩 29 位**。"启用某安全能力" = 置一位，零结构体改动。
- **结果面**在 v4 变成位掩码 + getter：`sb_requested` / `sb_acked` / `sb_ignored`
  三个 `uint32_t`，合计覆盖 **96 个安全信号状态**。

于是演进阶梯变成：

| 级别 | 形态 | 加第 N 个安全信号的代价 |
|---|---|---|
| L0（本轮） | 命名 getter + 位面 getter | 加一个 `#define`，置位；0 新符号 |
| L1（默认未来路径） | 只加位 | 0 新符号、0 结构体、0 签名改动、清单 0 改动 |
| L2（标量 getter > 30 个时） | `hm_proc_copy_out(proc, kind, buf, cap, &len)`，`kind` 是开放枚举 | 一次性收敛；旧 getter 保留为一行 wrapper（R8 禁删） |
| L3 | `hm_proc_copy_out_v2` | —— |

**关键判据**：v1/v2/v3 每加一个字段都要做一次**布局决策**（往哪追加？padding 洞
用不用？abi_size 抬不抬？）；v4 之后 L0→L1 是零成本，L2 之后 `kind` 是开放枚举、
再无演进需求 —— **决策点被消灭了，这才是根治**。

## fd 3 协议（v3/v4 逐字相同）

| 记录 | 字节 | tag | 时机 | 语义 |
|---|---|---|---|---|
| IGNORED | 8 | `0x484D4947` "HMIG" | 施加之后 | 请求了但没施加的位 |
| ACK | 8 | `0x484D4941` "HMIA" | **施加成功之后、`execve` 之前** | 实际施加的位（正面证据） |
| SETUP_FAILED | 8 | `0x484D4946` "HMIF" | `report_and_exit` 的 setup 分支 | setup 失败的 errno |
| 裸 errno | 4 | 无 | execve 失败分支 | errno |

判别靠量级：三个 tag 都 > `0x40000000`，远大于任何真实 errno。16 字节累加缓冲
够容纳"ACK 8B + FAILED 8B"或"任一条 + 尾部裸 4B"的粘连。`FD_CLOEXEC` 让 fd 3
的 EOF 在"execve 成功"与"shim 死亡"上同形 —— 这是 E3/E4 的全部难点，也是为什么
`HM_SETUP_EXEC_CONFIRMED` 只能靠**时序**（EOF 时 `waitpid(WNOHANG) == 0`）判定，
而不是靠"读到 EOF 就算成功"。

### `hm_proc_get_setup`：`EXEC_CONFIRMED` **缺失 ≠ 目标没有 execve**

这一条要单独写成硬话，否则下一个人一定把它当"未执行"用：

> `HM_SETUP_EXEC_CONFIRMED` 为 0 只等于「**我们没观测到**」，不等于「工具没跑」。
> 它是父进程**采样**出来的：`native/c/supervisor.c` 每 `sleep_ms(2)`（`:550`）
> 跑一次 `pump_setup`（`:454`）→ 读 `setup_eof`（`:469`）→ `waitpid(WNOHANG)`
> （`:471`），只有采样点恰好落在"fd3 已关、子进程还没死"这个窗口里才置位。
> 快工具的 `execve → exit` 常常整个落在同一个 2ms tick 内 ⇒ **跑完了也确认不到**：
> 真实 `build/hermes-shim` 下 `/bin/echo hi` **跨轮 0/20 ~ 18/20**（`/usr/bin/true`
> 0~2/20，`/bin/sh -c 'exit 40'` 累计 80/80），而 `Wait()` 一次都没报错。
> **波动本身就是证据**：同一目标跨轮差一个数量级 ⇒ 它承载不了"没执行"。

允许的用法只有一种：**归因**（"这次没确认到 exec"），且仅在
`requested & 关键位 != 0` 时需要用它。禁止的用法：把它当成"工具跑没跑"的**功能
性事实**（计费、审计、外部审计日志）。那需要协议级 fact（shim 在 `execve` 前写
一条 dedicated 记录），登记在 `docs/KNOWN-GAPS.md` 的 `F-11c` 条目下，**不立项**。

这也正是结局判据必须是三合一（`MEASURED && !EXEC_CONFIRMED && Signaled`）的原因：
`!EXEC_CONFIRMED` 混着"没跑"与"跑太快没采样到"两件事，只有"**被信号杀死**"这一
死因能把后者排除，判据宣称的命题才收窄成一句说得准的话。

## 结局判定：`ErrToolNotStarted`（D5）

安全性判定与结局判定是**两步**，顺序不能反（安全优先于正确性）：

```text
safety := sandboxDecision(...)          // 4 条 deny，见上
if safety != nil { return nil, safety }
if !execConfirmed && CHANNEL_MEASURED && signaled { return nil, ErrToolNotStarted }
```

三个合取项缺一不可：

| 合取项 | 不写它会怎样 |
|---|---|
| `CHANNEL_MEASURED` | Windows 上 `EXEC_CONFIRMED` 恒 0、`Signaled` = cancelled ⇒ 每次主动取消都被误判成"未启动"。**这就是 Windows 的豁免开关** |
| `signaled` | 快速正常退出的工具（`exited=1, signaled=0`）不受影响；挡掉"sub-2ms 正常退出"的误报 |
| `!execConfirmed` | 这条本身就是判据 |

**为什么必须给"没启动"一个独立结局**：ACK 之后，"signaled + 未确认 exec"的三种
真相必须分开 ——

| 真相 | 安全结论 | 结局 |
|---|---|---|
| shim 施加沙箱 → 写 ACK → **被 SIGKILL** | 安全（沙箱确实施加、目标确实没跑） | `ErrToolNotStarted`（执行失败，不是安全事件） |
| shim **施加前**被 SIGKILL | 不安全 | `policy/denied`（判据 5：缺 ACK） |
| 目标 exec 后被杀 | 安全 | 正常 `Result`（`EXEC_CONFIRMED=1`，判据 5 通过） |

若把第一行与第三行都返回"成功的空结果"，那么"工具被杀了"和"工具跑了但什么都没
打印"在结果上完全同形 —— 那是 F-11 要消灭的同一类静默错误，只是从"安全"挪到了
"正确性"。

`ErrToolNotStarted` 是 `internal/native` 的**包内哨兵**（普通 error），**不是**
`internal/errs` 错误类（`AttrAll`/`SubAll` 是闭集），也**不是** `policy/denied`：
后者的 Class=policy / Retry=None / 不该重试，而它是"链路没走完"的瞬时失败、可重试
—— 两者的分类与重试策略相反，绝不能合并。它的判定被抽成纯函数
`toolNotStarted(execConfirmed, setupBits, signaled)`，于是"Windows 上永不触发"
这件事可以被直接断言，而不必跑一个 Windows 进程。

## Windows 终止面与 setup 面语义

Windows 没有 POSIX 信号，也没有 `hermes-shim`。getter 的语义因此是**逐项定义**的，
不是"同 POSIX"：

| 项 | Windows 取值 |
|---|---|
| `HM_TERM_EXITED` | **恒置**（Win32 的 wait 只返回"进程结束了"） |
| `exit_code` | `GetExitCodeProcess` |
| `HM_TERM_SIGNALED` | `= cancelled` |
| `term_signal` | **恒 0**（没有 POSIX 信号可报） |
| `HM_TERM_ESCALATED` | `= cancelled`（`TerminateJobObject` 就是这个平台的"升级"） |
| `HM_TERM_DEADLINE_HIT` | deadline 触发 |
| `killed_pgid` | `(cancelled \|\| deadline_hit) ? pid : 0` |
| `fired_cycles` | **恒 0**（只有 wall 兜底，无周期计数） |
| `deadline_source` | `deadline_hit ? "wall" : ""` |
| `HM_SETUP_*` 全部位 | **恒 0**，含 `CHANNEL_MEASURED` **不置位** |
| `sb_requested` / `sb_acked` / `sb_ignored` | **恒 0** |

为什么 setup 面恒 0 而不是返回一个"不支持"错误：`win_spawn.c` 在 `CreateProcessA`
**之前**就 `if (req->sandbox_flags != 0) return HM_E_SANDBOX_DENIED;`，所以能走到
wait 的请求 `sandbox_flags` 必然为 0 —— "没有请求、没有施加、没有 ACK"的诚实表示
就是全 0，天然 fail-closed。`CHANNEL_MEASURED` 不置位则把"**没测量**"与"**测出来
是 0**"分开，这是"缺证据 ≠ 证据缺席"在平台差异上的落地。

⚠ Windows 分支本轮为**镜像式实现 + 代码审查，无执行验证**（本机无 MinGW）。


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
  **唯一例外是 R8 授权的符号面变更**：按 `hermes_abi.h` 顶部 R8 新增一个独立导出
  符号（例如 F-11b 的 `hm_proc_get_setup`，或同批的 `hm_proc_copy_stderr`）时，
  实现、清单、`docs/C-ABI.md` 必须在**同一次变更**里一起改——清单与实现是双向
  diff，只改一侧必红。清单里的分组计数注释也要跟着改，"改了符号却忘了改计数"
  同样会被 review 当成漂移（`tests/guard::TestExpectedSymbolsCountMatchesComment`
  会直接算总条数与分组之和）。
  **但要知道这条守卫的天花板**：双向 diff 是**集合对称差**，它能抓「一侧改了
  另一侧没改」，**抓不到「两侧都没了这一项」**——实现里少了 `hm_proc_get_sb_acked`、
  清单里也少了，两边一致 ⇒ 守卫全绿。这正是 F-11b 另加 G2
  （`TestF11bG2GetterNamesHardcoded`：13 个名字**在测试里写死**，逐个查
  头文件声明 / 清单 / `nm` 三处）的原因。**任何"清单 ↔ 实现"的对称守卫都有
  这个盲区**，写新守卫时别重复踩。

### 工程纪律

改任何 C ABI 相关的东西（`include/hermes/`、`libhermesrt` 的导出、`hermes header` 的产出），受影响的是**七个语言的 SDK + 第三方 C 程序**，不是"只有 Go"。改完要跑 `scripts/test_sdks.sh`。
