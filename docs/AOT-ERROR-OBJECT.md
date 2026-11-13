# AOT 标准错误对象（`HermesError`）规范

> 本文件是 AOT 错误对象的**规范源**。它描述的是 AOT 产物**内部**的表示，
> 不是运行期公开 ABI。仓库文档叙述一律用中文；产品用户可见输出（CLI 打印、stderr）用英文，
> 本文件引用到的输出仅为示意，不写死具体文案。

## §1 定位与适用范围

- `HermesError` 是 `hermes compile --backend=c` 生成的 `main.c` **内部**的错误对象结构体，
  定义在嵌进每份生成 `main.c` 的最小 C 运行期里，unit AOT 成员复用同一份运行期。
- 它**不是运行期公开 ABI**。两层冻结头**未改动**：`native/include/*.h`
  （受 `ABI_BUILD_ID` 哈希守卫）与 `include/hermes/hermes_core.h`（7 语言 SDK 的公开承诺）。
- 影响面只有两处：`hermes compile --backend=c` 生成的 `main.c`，以及复用同一份 C 运行期的
  unit AOT 成员。
- **状态更新（v2.0.0 收口）**：生成的 `main.c` 仍是独立可执行文件；但 unit AOT 成员
  与 `hermes cc` 的 per-unit shim 现在把错误文本发布到一个**共享错误槽**，宿主可用
  **既有**的 `hermes_last_error()` 读到真实原因（不再是恒空串）。公开 ABI 签名、
  4 个冻结头与 `include/hermes/hermes_core.h` 一个字未动，也没有新增导出函数；
  7 语言 SDK 仍走 `err` 出参、错误文本本就完整，不在本次改动内。详见 §9。

## §2 用户口径校准

用户诉求里的四个说法，在本仓链路里按下表落地：

| 用户口径 | 本仓落地形态 |
|---|---|
| 「标准错误对象结构体」 | AOT 产物内部的 C 结构体 `HermesError`：固定宽度整数 + 定长字符数组，**无指针成员**，带编译期布局断言 |
| 「返回状态码 + Out Parameter 指针」 | 冻结头 `hermes_net_*` 本就返回 `int` 状态码 + `char**` 出参（签名不变）；产物内部收口成「返回 `int` 状态码 + `HermesError*` 出参填错误对象」的 helper，形态示意 `int ...(..., HermesError* out_err)` |
| 「LLVM IR 层确保 32/64 位布局对齐」 | 本仓**没有 LLVM IR 层**。AOT 链路是 `IR JSON → 生成 main.c → clang -std=c99 → 机器码`，等价落地在 **C 层**：固定宽度整数 + 定长数组 + 编译期布局断言。断言用 `typedef char …[(cond) ? 1 : -1]` 形式，因为产物按 **C99** 编译（`cmd/hermes/cmd_compile.go`、`cmd/hermes/cmd_compile_test.go:260`），而 `_Static_assert` 是 C11 才有的。**不要用「生成的 C 要兼容 C89」当理由**：C89 严格模式只约束公开 ABI 头 / 生成头 / `hermes cc` shim；`cPrelude` 本身含 `snprintf` 与「声明在后」写法，在 `-std=c89 -pedantic -Werror` 下本来就编译不过，两条产物路径实际都是 `-std=c99`。新增 helper 仍按 C89 风格书写（不用 `//`、不用块中声明、不用 `long long`），这是风格约束不是全绿承诺 |
| 「`struct HermesError { int code; const char* msg; }`」 | `const char* msg` 是**指针成员**：ILP32 下 4 字节、LP64 下 8 字节，物理上无法逐字节一致。故改为定长 `char msg[256]`，整个结构体**无指针成员**，跨位宽布局才可对齐 |

## §3 字段与布局

`HermesError` 三个字段，均为固定宽度 / 定长，无指针、不依赖指针宽度：

| 字段 | 类型 | 语义 |
|---|---|---|
| `code` | `int32_t` | 错误类型枚举：`0` = none（无错误），`1`..`5` 对应 CatchType 分类，`8` = Internal。码表见 §5 |
| `status` | `int32_t` | 上游 HTTP 状态码；`0` 是 §7.1.1 规定的哨兵（上游无状态码，如网络错误 / 模型错误上游无码） |
| `msg` | `char[256]` | 错误文本（定长，不是 `const char*`）。来自 `hermes_net_*` 的 `char**` 出参，携带 `RunError.Error()` 已有的 `(status %d)` 片段，由产物内部解析出 `status` |

布局（`sizeof == 264`，字段偏移 `0 / 4 / 8`）：

```
offset  0 : int32_t code      (4 bytes)
offset  4 : int32_t status    (4 bytes)
offset  8 : char    msg[256]  (256 bytes)
sizeof(HermesError) == 264
```

**为何无指针成员**：只要结构体里出现指针，其宽度与对齐就随 ILP32 / LP64 变化，`sizeof`
与字段偏移无法在两个 target 上逐字节一致；改用定长 `char[256]` 后，布局只由固定宽度整数
与数组长度决定，与指针宽度无关。

**4 条编译期布局断言**（`typedef char <名字>[(cond) ? 1 : -1]` 形式，C99 下可用、不依赖 C11 的 `_Static_assert`），分别钉死：

1. `sizeof(HermesError) == 264`
2. `offsetof(HermesError, code) == 0`
3. `offsetof(HermesError, status) == 4`
4. `offsetof(HermesError, msg) == 8`

> 断言名、helper 名等具体实现标识以产物为准，本规范只约束上述等式与形式。

## §4 生命周期与闭环

错误从 C ABI 边界进入，到 `main()` 收尾，流程如下（简单 ASCII，非 mermaid）：

```
hermes_net_tool / hermes_net_model(req, len, &out, &err)
    |  冻结头：返回 int 状态码 + char** 出参（签名 / 注释约定未改）
    v
产物内部收口：int ...(..., HermesError* out_err)
    |  由 err 文本里的 (status %d) 片段解析出 status
    |  填 code（类型枚举）/ status（HTTP 码）/ msg（错误文本）
    v
HermesError（code / status / msg）
    |
    +-- 错误类型分派：按 code 选 catch 臂
    |     catch 臂在「清错之前」按值快照错误对象 -> 供 e.status 读真实状态码
    |
    +-- 三处清错点统一清「整个」对象：
          try 的 catch 臂 / fallback / retry
    v
main() 失败出口：以错误对象的 code 为退出码、msg 为 stderr 文本
```

**当前实现补的那条对外出口**（不改变上面的 `main()` 行为）：

```
unit.c 的 hm_fail / hermes cc shim 的 rc != 0 分支
    |  失败时先把错误文本 bounded-copy 进共享错误槽（静态存储期、无 malloc）
    v
hermes_last_error() -> hermes_str{ptr,len}（调用方不 free，下次调用前有效）
    |
    +-- Python / Go / Java / C# / Node / Rust 的 FFI 按 len 读 ptr 前 N 字节
```

要点：

- **出参必须被消费，否则就是死参数**：`hm_net_invoke` 只构造并填写 `*out_err`，**不写
  全局 `hm_error`**；两个调用方（tool / model）必须各自把出参发布到全局错误槽，错误才能
  进入 `try` / `catch` / `fallback` / `retry` 分派。判据是「删掉任一消费点，真实 404 / 503
  端到端测试必须转红」—— 仅是「helper 声明了 `HermesError*` 形参并写了值」不构成闭环，
  那是形式上的 Out Parameter。
- **闭环不等于让 `fallback` 绑定错误对象**：`LANGUAGE.md` §5.4.1「`fallback` 内不绑定错误
  对象」这条语言设计不变。闭环只发生在**清错语义**——`try` 的 catch 臂、`fallback`、
  `retry` 三处清错点统一清整个错误对象；要在降级块里看错误字段，仍请用 `try` / `catch`。
- **catch 臂按值快照**：清错发生在 catch 臂读字段之前会丢值，故 catch 臂在清错**前**按值
  快照一份错误对象供 `e.status` 读；`e.status` 因此是真实 HTTP 状态码，不再是静默 `null`。
- **状态码通道**：真实 HTTP 404 / 503 的状态码此前在文本里被丢掉，当前实现让错误文本携带
  `RunError.Error()` 已有的 `(status %d)` 片段，由产物内部解析回 `status`；`err` 仍是纯文本，
  冻结头签名与注释约定不变。

## §5 `code` 码表

| `code` | 含义 |
|---|---|
| `0` | none（无错误） |
| `1` | Contract |
| `2` | Network |
| `3` | Http |
| `4` | Model |
| `5` | Circuit |
| `8` | Internal |

- `1`..`5` 对应 CatchType 的分类，`8` 为内部错误；`0` 是「无错误」哨兵。
- **这与 `include/hermes/hermes_core.h` 的 `hermes_status` 不是同一套码表**：后者是 7 语言
  SDK 的公开状态码（按 Attribution 六个桶分组，含 `BUDGET` / `CIRCUIT` / `INTERNAL` 等独立
  取值）。两套码表不要混用。
- **既有文档漂移登记**：`native/include/hermes_net.h` 的头注释称「状态码与 hermes_status
  同义：0=OK 1=Contract 2=Network 3=Http 4=Model 5=Circuit 8=Internal」，这串取值实际对应
  上表（AOT `code`），并非 `hermes_status`。该头受 `ABI_BUILD_ID` 哈希守卫冻结，当前实现
  **一个字未改**，故在此登记，不改动冻结头；留待后续解冻时一并校正。

## §6 32 / 64 位对齐的诚实边界

- 结构体**无指针成员** ⇒ `sizeof` 与 `offsetof` 只由固定宽度整数和定长数组决定，
  **与指针宽度无关**：从构造上就是跨位宽可移植的。
- §3 的编译期断言只在**当前编译 target** 上钉死那 4 个等式；它证明的是「这一份生成的 C
  在当前 target 下布局符合预期」，不是对另一个 target 的实测。
- `docs/C-ABI.md` 明写交叉编译（ARM / RISC-V）**未支持、也没试过**。
- **本仓从未在 32 位 target 真编译 / 链接 / 运行**。当前实现提供的保障是三层：**按构造可移植
  + 当前 target 的编译期布局断言 + 双 target 编译探针（若 clang 支持）**。
- 探针的**实测能力边界**（Apple clang 21 / arm64-darwin，2026-11-10）：`i386-unknown-linux-gnu`、
  `i686-unknown-linux-gnu`、`armv7-unknown-linux-gnueabihf` 与 `-m32` 四种 32 位 target 的
  `-fsyntax-only` 均可用，故测试可以真跑一次 32 位语法层探针；但 `-fsyntax-only` **只做
  语法与语义检查，不链接、不生成目标文件、不运行**，因此它验证的是「4 条布局断言在
  32 位数据模型下也成立」，**不是** 32 位产物能加载 / 能跑。在 clang 不支持该 target 的
  机器上探针会跳过，并明确输出「32 位 target 未实测」。
- 这三层保障**不构成任何 32 位 target 的实机验证结论**。

## §7 源码层字段集合与 `ir_version`

- 源码层错误对象的字段集合是**封闭**的（`LANGUAGE.md` §7.1.1）：当前实现**不新增**
  `e.code` / `e.msg`，新增字段属 `ir_version` 的 minor 变更，**当前实现不升 `ir_version`**。
- `code` / `msg` / 结构体布局只是 AOT 产物**内部**的 ABI 字段，**不是源码可访问的字段**。
- 源码层仍只有 `HttpError.status` 为 ready；规范里写了但运行期未带上的字段（如 `e.url`）
  仍按 `E4034` 拒绝，AOT 侧也显式拒绝而非静默给 `null`。

## §8 剩余缺口（含已闭合项）

1. **✅ 已闭合（v2.0.0 收口）：错误对象已对外暴露错误文本**。宿主现在可通过
   **既有**的 `hermes_last_error()` 读到 unit AOT / `hermes cc` 失败时的真实原因
   （见 §9）。闭合方式：一个共享错误槽 + 两处写方 + 每个入口清槽；**不改公开 ABI
   签名、不动 4 个冻结头、不动 `hermes_core.h`、不新增导出函数、不动 7 语言 SDK**。
   **新登记的限制**：`HERMES_THREAD_LOCAL` 在 C11+ / MSVC 下是线程局部，但本仓
   `hermes cc` 走 `-std=c89`、unit AOT 走 `-std=c99`，宏展开为空 ⇒ 槽是
   **进程级全局**，并发调用会串错误（不是"线程局部缓冲已取消"）。
2. **32 位实机未测**：见 §6，本仓从未在 32 位 target 真编译 / 链接 / 运行。
3. **`hermes_net.h` 注释漂移**：见 §5，因该头冻结当前实现未改，登记在此。

## §9 对外错误文本：`hermes_last_error()` 的共享错误槽

当前实现只填实 `internal/header/core_impl.go` 里 `hermes_last_error()` 的实现，
**没有新增任何 `hermes_core.h` 导出函数**（按 `docs/C-ABI.md` 那张兼容性表，
新增导出函数属 1.0 → 1.1 的 minor bump）。

### 槽的定义与所有权

槽是 `CoreImpl` 里的一个 **external-linkage 变量**（不是函数，因此绕开
`internal/header/core_impl_test.go` 的「CoreImpl 里每个函数都必须在核心头有声明」
守卫）：

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

- **无指针成员、静态存储期**，与 §3 的 `HermesError` 同一套价值观。
- **绝不 `malloc` / `strdup`**：`hermes_last_error()` 返回 `hermes_str{ptr,len}`
  指向静态 `msg[]`；`ptr` 恒非 NULL，无错误时 `len == 0` 且 `msg[0] == 0`。
  **调用方不 free**（也没有对应的 free 函数）、不长期持有指针，视图只在
  **下次调用前有效** —— 每个入口调用先清槽。无泄漏、无悬空指针复制。
- 填槽一律 **bounded-copy**：`memcpy` 按 `min(len, CAP-1)` 复制，显式写 `len`，
  并 NUL 终止。**不许 `strlen` 静默截断后还声称有长度**；内嵌 NUL 由 `len` 表达，
  `hermes_str` 带 `len` 正是为了跨语言边界不被截断。

### 两处写方

| 写方 | 生成位置 | 清槽 | 失败发布 | 返回值 |
|---|---|---|---|---|
| `hermes cc` 的 per-unit shim | `internal/header/gen_shim.go`（生成 C 按 **C89** 书写：无 `//` 注释、声明集中在块首） | 函数入口 | `rc != 0` 时，在 `free(err)` **之前**把 `hermes_rt_run` 的 `err` 文本复制进槽 | 恒 `HERMES_ERR_INTERNAL` |
| unit AOT 成员 | `internal/backend/unit.go` 的 `hm_fail` 出口 | 入口 `hm_clear_error()` 之后 | 把 `hm_e.code` / `hm_e.status` / `hm_e.msg` bounded-copy 进槽 | 恒 `HERMES_ERR_INTERNAL` |

两处都**不改** `hermes_status hermes_<unit>_<fn>(const hermes_buf*, hermes_buf*)`
签名，也不把 AOT `code`（CatchType）映射成 `hermes_status` —— §5 已明令两套码表
不混用，且不存在正向映射规范。槽里的 `code` / `status` 只是**原始诊断元数据，
不对宿主暴露**；宿主唯一能读到的是 `hermes_str{ptr,len}`。

### 链接方案

- `hermes cc` 的 C 与 C++ 两条路径都把 `CoreImpl` 写出的 `hermes_core_impl.c`
  和用户程序、shim 一起编译链接（`cmd/hermes/cmd_cc.go`），槽在那里**强定义**；
  shim 只 `extern` 引用。
- unit AOT 的链接单元原本**没有** `hermes_core_impl.c`（`cmd_compile_test.go`）。
  因此 `internal/backend/unit.go` 生成的 `unit.c` 额外带一份
  `HERMES_THREAD_LOCAL struct hermes_error_slot __attribute__((weak)) ...` **weak 定义**
  （仅 clang/GCC）：单独编译链接时槽符号自洽；同时链接 `hermes_core_impl.c` 时
  强定义胜出，所有翻译单元共享同一个槽。端到端测试真的编、链、跑了这两种链接形态。
- `cmd_compile_test.go` 的 unit AOT driver 也带上了由 `header.CoreImpl` 写出的
  `hermes_core_impl.c`，用于取得 `hermes_last_error()` 并验证失败文本 / 入口清槽 /
  内嵌 NUL 的 `len` 语义。

### 线程局部的真实边界（必须如实登记）

`HERMES_THREAD_LOCAL` 来自 `include/hermes/hermes_core.h`：C11 `_Thread_local`、
MSVC `__declspec(thread)`，其余（含 `-std=c89` / `-std=c99`）**展开为空**。
本仓两条产物路径分别用 c89 与 c99，所以**当前产物里槽是进程级全局**：
并发调用会互相覆盖错误文本，不是"每个线程一份"。头文件 `:74` 的「线程局部」
承诺在这两种方言下存在降级；`docs/C-ABI.md` 与 `docs/KNOWN-GAPS.md` 已同步登记。
同一链接单元里的所有 TU 必须使用同一方言，否则宏展开不一致。
`include/hermes/hermes_core.h` 当前实现**一个字未动**。

### 验收

- `hermes cc` 的 **C 与 C++** 两条路径：失败工作流调用拿到非空、含真实原因
  （`input is not valid JSON: …`）的文本且 `len > 0`；成功调用后 `len == 0`。
  `examples/c-minimal` 的 `hermes cc` 路径实测同样通过。
- unit AOT：失败产物（`x / 0`）后同进程调 `hermes_last_error()` 读到
  `division by zero`；成功调用后 `len == 0`；直接写槽验证内嵌 NUL 由 `len` 表达；
  `unit.c` 不带 `hermes_core_impl.c` 也能单独链接运行。
- 结构守卫：`internal/header/core_impl_test.go` 断言 `CoreImpl` 里不再出现
  `static const char empty` 占位、`hermes_last_error()` 函数体引用槽、槽无指针成员、
  且 CoreImpl 里恰好只有两个已在头中声明的函数。
