# 已知限制

当前版本**尚未提供**的能力，按影响范围分类。这份清单用于让使用者判断
"我要做的东西现在能不能做"，而不是路线图承诺。

## 语言与运行时

| 缺口 | 影响 | 现状 | 下一步 |
|---|---|---|---|
| ~~「零解释器」红线未达成：`hermes compile` 不是真 AOT~~ → **已闭合** | `037a166` 删掉了「C 启动器 + 内嵌 IR 字节 + `libhermesrt.a`」路径；`cmd/hermes/cmd_compile.go` 现在走 `internal/backend`：`IR → main.c → clang → 机器码`，产物不含 IR 字节、不含解释器。`internal/header/gen_exe.go` **已不存在**，旧记录里的「hello 产物 6.6 MB / C 后端 33 KB」是当时实测，不是现状 | ✅ 真 AOT 已落地 | **剩三处收尾**：① `cmd/hermesrt/main.go` 仍导出 `hermes_rt_run`（Go c-archive `libhermesrt.a`，15.5 MB，内含 `ir.Load` + `engine.RunWith` 树遍历解释器），头注释仍称「`hermes compile` 生成的 C 程序内嵌 IR」，与 `cmd_compile.go` 直接矛盾；② `hermes run` / **`hermes serve`** / `hermes cc` / 7 语言 SDK 仍经 `internal/engine`（`cmd/hermes/cmd_serve.go:528,538,547,598` 四端点直接调 `engine.RunWith`）⇒ **「零解释器」只覆盖 `hermes compile` 的 AOT 产物，不能写成全仓成立**；其中 `hermes cc` 与 7 SDK 的 in-process 模式依赖该归档，删前要给迁移路径（`hermes run` 是开发期差分工具）。另 `internal/engine` 整包**不可删**：`internal/netcall`、`internal/runtime`、`libhermesnet` 编译期依赖它的 `ToolRequest`/`ModelRequest`/`RunError`/`Runtime` 接口，删了会摧毁 AI 兼容面；③ `cmd_compile.go:204` 无条件打印 `no Go runtime`，但 `needNet` 路径链了 `libhermesnet.a`（Go c-archive，带 Go runtime）⇒ 文案失真，应按 `needNet` 分支改。**注意 `libhermesnet` 不是解释器**：`cmd/hermesnet` → `internal/netcall` 只调 `runtime.Runtime.Tool/Model`，从 `internal/engine` 借的是类型定义，不调 `ir.Load` / `engine.RunWith`、不遍历 IR 图；它是 Anthropic / OpenAI / Ollama / custom 等 AI 接口兼容层在 AOT 产物里的**唯一载体**，清理 `libhermesrt` 时绝不能一起删。分层纪律：硬件/底层需要时可用 C/C++（或 LLVM 后端）自写，Go 保留在软件层接口；但兼容面是协议契约，与实现语言正交 |
| 数组值语义 | ✅ 已支持：字面量、`len()`、下标 `xs[i]`、返回数组、**编译期越界检查** | ✅ | 剩：动态长度数组（`json.as_array` 的结果）只有运行期越界检查 |
| ~~`serve` 租户预算没有周期性重置~~ → **✅ 已闭合（v2.0.0 收口）** | 预算用完回 402 的语义逐字不变 | ✅ `budget_reset` 纯 opt-in：租户级 > CLI `--budget-reset` > 默认 `none`，UTC 日历窗口惰性归零 | 无（语义着色仍是文本近似，另在核心层登记） |
| **语义着色是文本近似** | 同名不同作用域区分不了 | ⚠️ 近似 | 真正的解法是把类型检查接进 LSP；工作量等于半个前端 |
| **Linux 静态链** | 静态 glibc 被 D-033 主动拒绝；musl 路径未实测 | ⚠️ 仅 Linux 动态链 | 需要一台 Linux 机器实测，不能靠推演 |
| **运行期 agent 硬上限** | `max_steps = unlimited` 的 agent 只有编译期语义（发 `W2004`、标 `unbounded`），运行期那道"硬性迭代上限兜底"还没有实现：现在 `hermes run` 遇到没有静态上界的 agent 直接拒绝执行（`agent 节点 N 的 max_steps 必须 > 0`）。所以 `LANGUAGE.md` §6.4 里"运行期另有一道硬上限"这句目前描述的是契约，不是可观察行为 | ⚠️ 未实现（`internal/engine/run.go` 仍拒绝 `MaxSteps <= 0`） | 在 `runAgent` 里加一个**具名**上限常量（与 `for` 的迭代上限分开，两者混用会让"改了一个影响另一个"）+ 无静态上界时的循环上界；与 `AgentSpec.MaxSteps` 改 `*int` 一起落地 |
| **手写 / 篡改 IR 可绕过幂等证明** | `E5007` 是**编译期**闸门，只保护 `hermes ir` 产出的 IR。`hermes run` 接受任意 IR 文件，而 `internal/engine/run.go` 的 `retry` 语句不读 `tools[].idem.mode` —— 手工构造一份 `retry` 里放 `idem.mode = "none"` 的 `call_tool`，执行器仍会把它重试到上限 | ✅ 已补：IR.md **V-12** + `E_IR_RETRY_UNPROVEN` | `internal/ir/validate.go::checkRetryIdempotency` 在图阶段遍历 `retry.body` 可达子图（复用 `refs.go::nodeSuccessors`），`idem.mode ∈ {readonly, idem_true, idem_key}` 才算有证明；`none` / 缺失 / 未知值一律拒。中英文文案进 `internal/diag/catalog.go`。测试：`internal/ir/retry_idem_test.go`（18 个节点图形状 + 真实 golden 单点变异）、`cmd/hermes/cmd_run_idem_test.go`（`hermes run` 端到端退出码 4）。两轮变异验证（把 `none` 当证明 / 摘掉 `stageGraph` 调用）均转红 |
| ~~**`float` 的语义口径未收敛 / 改名未落地**~~ → **✅ 已闭合（v2.0.0 收口：float→double 完整 binary64）** | 语言里唯一的浮点类型名是 `double`（IEEE-754 binary64），没有 32 位 `float` | ✅ 字面量 `1.5` / `1e3` / `1.5e2` 编成 IR 位模式 `{"k":"double","bits":"<16 位小写 hex>"}`；`+ - * /` 与 `< <= > >=` 支持 double（源码层 `%` 由 `E4009` 限定为 int 操作数）；`call_native` 返回 double 真编译真跑已验证；`json.as_float` 保留 API 原名、窄化到 `double` | 无（`E4029` 收窄为旧 `float` 标签的纵深防线；仍被拒的三条见下「决定记录」） |
| **string FFI 返回是零拷贝借用，无生命周期保护** | 首版 AOT 子集把 `string` 映成 `const char*` / `hm_str`（只存 `char*` 不复制）：被调方返回栈上缓冲 ⇒ use-after-scope 且当前 rc=0 无诊断（实测 stdout 乱码）；内嵌 NUL 被截断；`hm_as_str` 对非 string `Val` 用共享静态缓冲（仅篡改 IR 可达，真实源被 E4030 拦） | ⚠️ 仅是约定，未强制 | 约定被调方返回静态存储期或调用方长期持有的缓冲；完整 `hermes_str{ptr,len}` 映射属 S2 |
| ~~**AOT 产物里读不到 `catch` 绑定的错误对象字段**~~ → **✅ 已闭合**（2026-11-10） | `hermes run` 与 `hermes compile`（C 后端）下 `catch (HttpError e) { if (e.status == 404) }` **都能读到真实 HTTP 状态码**（规范源见 `docs/AOT-ERROR-OBJECT.md`）。闭合方式：产物内部落地一个**无指针成员、编译期布局断言钉死**的标准错误对象，由 `hermes_net_*` 的「状态码 + 出参」填充；catch 臂在清错前**按值快照**，`try` 的 catch 臂 / `fallback` / `retry` **三处清错点统一清空整个对象**；真实 HTTP 404 / 503 的状态码经错误文本打通到 `status`，不再是静默 `null` | ✅ 已闭合：原先「`case "field"` 对 catch 绑定一律显式拒绝 + 帮助文案指向 `hermes run` / `allow_status`」的临时保护及其守卫 `TestGenerateRefusesCaughtErrorField` 已被**「放行 `status` / 拒绝未 ready 字段（`E4034`）/ 端到端真实 404·503」的守卫**取代；以 `internal/backend` 的生成结构守卫与端到端 AOT 测试为准 | **剩余缺口 ① 已闭合（v2.0.0 收口）**：`hermes_last_error()` 不再是恒空占位，改读一个 **external-linkage、无指针成员、静态存储期**的共享错误槽（`CoreImpl` 里的变量，不是新函数）；`hermes cc` 的 per-unit shim 在 `free(err)` **之前**把 `hermes_rt_run` 的错误文本 bounded-copy 进槽，unit AOT 的 `hm_fail` 把 `hm_e.code/status/msg` 同样写进槽（返回值仍恒为 `HERMES_ERR_INTERNAL`，AOT `code` 与 `hermes_status` 两套码表不混用）。`hermes_last_error()` 返回 `hermes_str{ptr,len}` 指向该静态数组，**调用方不 free、不长期持有**，每个入口调用先清槽 ⇒ 成功调用后 `len == 0`。闭合方式：**不改公开 ABI 签名、不动 4 个冻结头、不新增 `hermes_core.h` 导出函数**；7 语言 SDK 仍走 `err` 出参（错误文本本就完整），不在本次改动内。**如实登记的新限制**：`HERMES_THREAD_LOCAL` 在 C11+ / MSVC 下是线程局部，但 `hermes cc` 走 `-std=c89`、unit AOT 走 `-std=c99`，该宏**展开为空** ⇒ 槽退化为**进程级全局**，并发调用会串错误；且同一链接单元的所有 TU 必须用同一方言，否则宏不一致。详见 `docs/C-ABI.md`、`docs/AOT-ERROR-OBJECT.md` §9；② **32 位 target 从未实机编译 / 链接 / 运行** —— 只有「按构造可移植 + 编译期断言 + 双 target 编译探针（若 clang 支持）」，不能当成实机验证（见 `docs/AOT-ERROR-OBJECT.md` §6）。~~历史结论留档：静默错值比拒绝坏得多 —— 上一轮 `hm_field(hm_str(""), "status")` = 静默 `null`、`if (e.status == 404)` 永远不成立且没有任何报错，这正是「宁可拒绝、不要猜」的理由；当前实现有了真实值形态后该前提不再成立，留档备忘。~~ |

| **`parallel` 体内写体外变量未被拒绝** | 规范说"体外变量在体内可见**且只读**"，这是 `parallel` 不需要锁的全部理由。曾实测：前端接受 `parallel` 分支里对体外绑定的 `x++` / `x = …`，并编成对**同一个变量 id** 的 `assign` 节点；而运行期分支拿到的是父作用域的**副本**（`runBranches`），于是这个写操作**静默丢弃** —— 不报错、也不生效 | ✅ 已决定：硬错，无迁移期 | ✅ 已落地 `E5010`：判据落在**源码 AST** 的 `check_type.cpp::parallel_`（进入最内层 `parallel` 时的作用域水位），不在 codegen；读体外变量继续合法；带内联不误伤的回归测试。**控制流体（`for` / `while` / `match` 臂 / `retry` 体）由 `parallel_()` 内一次独立写语句子树扫描覆盖**（遍历 `child` / `args` / `body` / `else_branch` / `type_args` / `subs` / `next` 全部槽位，嵌套 `parallel` 压新水位去重），刻意不补齐 `stmt_()` 的既有类型检查遍历，避免语义扩张。三条理由与「显式 opt-in 不能支持」的结论见下方决定记录 |

### 决定记录：`parallel` 体内写体外变量（留档，避免重复讨论）

**结论：硬错 `E5010`，无迁移期警告，不提供 opt-in 开关。** 这行特意连同理由一起留在这里 ——
只留结论，下次还会有人把「警告 / 版本闸 / 显式开关」三个选项重新提一遍。

1. 这不是「行为变更」，而是**现在就是死代码**。每个分支拿到的是父作用域的副本，写入被丢弃，所以今天这行代码的效果恒等于零。迁移期能迁移的只有「依赖静默丢数据」的人，而那种依赖本身就是缺陷。
2. 它不是「可疑」，是**可证无效**。警告是给「可能不是你的本意」用的；这里编译器知道这次写入永远不生效。它和 `if (false) { ... }` 之后的不可达代码同类，不是 `unused variable` 同类。这个项目的判据一贯是「宁可拒绝，不要猜」—— 加一条没人读的警告，等于让它再静默一个版本。
3. 版本闸对 pre-1.0 产品是最坏选项：等于先发布一个已知静默错误 + 一条警告，再许一个没人会执行的期限。

**选项 3（显式 opt-in）不采纳，且这条同样是结论，不是「暂不支持」**：
「允许分支写体外变量」若要成立，必须先定义并发写的语义（last-writer-wins？合并？），而那按定义就是数据竞争 —— 直接推翻 §5.2「parallel 不需要锁」和「上界可证」这两条产品主张。它不是「暂不支持」，是**不能支持**。

实现约束也一并钉住：判据必须在**源码 AST** 的类型检查里，不能放 codegen。codegen 看到的是内联之后的 IR，路线 A/B 内联会给被内联的函数体合成指向 workflow 作用域临时变量（`__hm_inline_r_N`）的 `assign` 节点；在 IR 上判「目标必须声明在本分支内」，会把 `auto v = pick(3);`（`pick` 是 if/return 决策树）这种完全正常的源码判错 —— 用户一行赋值都没写。嵌套时只看**最内层**水位（内层分支同样拿到外层分支作用域的副本）；**读体外变量继续合法**。

### 决定记录：浮点类型 = `double`（IR 用位模式）

**结论（2026-11-09 定，v2.0.0 收口已落地）：类型名 `double`（IEEE-754 binary64），不提供 32 位 `float`；IR 里的浮点**常量**用位模式，绝不放 JSON 数字；隐式转换维持禁止。**

1. **为什么要改名（历史口径）**：改名之前 `LANGUAGE.md` §4.1 写的是 `float = binary64`，而 C++ 里 `float` 是 32 位、`double` 才是 64 位。这是"C 的名字 + C++ 的宽度"——同一个词表示两件事，与 `entry`→`root` 那次同类。用 `double` 之后，语义与 C++ **重合**，读者不用查表；§4.1 现已统一为 `double`。
2. **改名已落地（v2.0.0 收口）**：`E4029` 已从「所有浮点字面量被拒」收窄成**旧 `float` 标签的纵深防线**（`check_type.cpp::scan_float_` 全 AST 扫描 + `codegen.cpp::gen_expr` + 参数默认值折叠三道）；正常路径的标签是 `double`，字面量 `1.5` / `1e3` / `1.5e2` 编成位模式 `{"k":"double","bits":"<16 位小写 hex>"}`。`json.as_float` 是 **API 名**不是类型关键字，**有意保留原名**，只窄化到 `double`（cast 节点的 `to` 写 `"double"`）。
3. **为什么 IR 不许放浮点数字**：IR 的 canonical / 字节稳定 / 可签名三件事都建立在"同一个值只有一种写法"上。`0.1` 的十进制文本会随序列化实现、locale、round-trip 实现漂；而 `{"k":"double","bits":"3ff0000000000000"}`（16 位十六进制，8 字节原样）唯一且可逆，且**读方不需要浮点解析器**。引擎侧 `math.Float64frombits`，C 后端直接发 `double` 字面量。
4. **为什么不松隐式转换**：`E4006`（`int` 不隐式升 `double`）与 `E4008`（`cost` 不参与浮点）保持不动。金额红线的整条论证就是"跨类型必须显式"，松一个口子，`cost + double` 会跟着被推着松。这与 C++ 不一致，是**有意的偏离**，写进偏离清单。同理 `double == double` / `!=` 由 `E4033` 硬拒，`tool.call<double>` 由装配层拒（`call_tool` 的 T 必须是 JSON 结构类型）；`call_native` 的返回类型则可以是 `double`。

**两条已拍板（2026-11-10，用户裁决）**：
- (a) `double == double`：**升为硬错**，已落地为 `E4033`（`==` 与 `!=` 都拒；`< <= > >=` 与算术不受影响）。§4.4 已从"允许但 `W4001`"改成禁止。
  - 附带发现：`W4001` **从来没有实现过** —— 全仓只有文档提到它；改名前的 `float == float` 曾静默通过（能编、能跑，产出的是两个十进制文本的字符串比较）。所以这次不是"警告改错误"，是"补上一个从未存在的拒绝"。`W4001` 已进"已废弃的码"表，防复用。
- (b) `NaN` / `Inf`：**只在序列化边界拒绝**，已落地。判据是位置，不是值：
  - IR 内部、构造、传递、比较**都不拒** —— `0/0`、`Inf-Inf` 这类中间结果必须能存在，拒在中间等于把这些表达式本身禁掉；
  - 写出去的那一刻拒：`cmd/hermes/jsonboundary.go::rejectNonFinite`，在 `json.Marshal` 之前，报**位置**（`value.a.b[0]`）与改法；键排序遍历，同一份结果两次运行报同一项；
  - **结构守卫**：`tests/guard/nonfinite_boundary_test.go` 扫全仓 Go 源码，断言 `math.IsNaN` / `math.IsInf` 只出现在那个文件 —— 别处再加一道就变红。这类改动是"加代码"不是"改代码"，不会让任何现有测试红，只能靠结构守卫拦。
  - 为什么不用 `encoding/json` 自带的拒绝：它吐 `json: unsupported value: NaN`，既不说哪一项也不说怎么办。`TestEncodingJSONWordingIsWhyWeGuard` 把"我们换掉的是哪句话"钉住，免得后人觉得这层多余。
  - 现状（v2.0.0）：`double` 已是真 binary64，`1.0 / 0.0` 一类运算能产出 `+Inf`，这道边界检查在 `hermes run` / `hermes compile` 写结果 JSON 时会真实触发（`cmd/hermes/cmd_float_test.go::TestDoubleNonFiniteRejectedAtAOTJSONBoundary`）；不再是「为 Phase 2 预埋」。

**实现清单（v2.0.0 已逐项落地，一处不改就会半截）**：字面量（lexer 已支持 `1.5` / `1.5e2` / `1e3`，`1.` 不合法）→ `check_type`（`Ty::Double`、E4006/E4008 不动、`E4033` 已落地）→ `codegen`（lit 发位模式、binop 支持 double 的 `+ - * /` 与关系比较；`%` 仍由 `E4009` 限 int）→ `schemas/ir.schema.json`（新增 double 位模式 `oneOf`；`nodeLit.t.enum` **刻意保留**旧 `"float"` 标签）→ `internal/engine`（`float64`；序列化边界的非有限值检查已就位）→ C 后端（`double`，且必须 `-ffp-contract=off`、禁 `-ffast-math`，否则 FMA 收缩让结果依赖编译选项）→ 格式化（插值与 trace 用 **shortest round-trip**，否则 `0.1+0.2` 打成 `0.3`，对拍失真）。**当前实现另行闭合的三项**：`serve` 预算 opt-in 重置、AOT 错误对象经 `hermes_last_error()` 对外暴露、`W5005`；连同本决定记录的 float→double 完整 binary64 共四项，逐条留档见「已经补上的」。

## 接口层（我们开个口子，别人填）

| 缺口 | 需要的口子 | 谁来实现 |
|---|---|---|
| C 侧流式 trace | 一个回调注册函数 | 我们加一个 C 函数；上层封装（Python `on_step`、Node EventEmitter）交给社区 |
| C 侧流式输出 | 一个 `run_stream` + `next` 迭代器 | 同上 |
| MCP 客户端 | **不需要**：`export tool` 语法稳定，社区写 `mcp2hermes` 转换器即可 | 社区 |

## 外围层（不是这儿的缺口）

文档加载器（PDF / 网页 → 文本）、工具市场、更多 backend 适配器、模板库 ——
这些用 HTTP 边界或文件格式就能接，**不内建**。`examples/` 里有种子示例就够了。

## 已经补上的（留档，避免重复讨论）

| 曾经是缺口 | 现在 |
|---|---|
| **`HttpError.status` 读不出来**（§7.1 与 `examples/support_bot/README.md:78` 都在推荐的写法报 `E4016`） | **已修（2026-11-10，`hermes run` 路径）**：`check_type.cpp` 按 catch 的**类型名**建错误字段表（`ready` 一列区分「规范有」与「运行期已带上」），`PathExpr` 分支对错误对象做静态字段推导 ⇒ `e.status: int` 编得过、拼错的 `e.nope` 报 `E4016`、规范有但运行期没带上的 `e.url` 报 **`E4034`**（不骗人说「没这个字段」，也不放行到运行期炸）。`internal/engine/run.go::value()` **无条件**写 `status`（0 是规范给的哨兵）——有条件写会让「类型检查说能读、运行期 `field does not exist` 炸」，而那不是 `*RunError`，`catch` 接不住。守卫：`tests/compiler/parser_test.cpp::test_err_fields`（8 例）、`cmd/hermes/cmd_errfield_test.go`（端到端**换服务端真实状态码**：404 命中、503 不命中 404、503 命中 503 ⇒ 同时排除「写死成 404」与「什么都不命中」）、`internal/engine/errvalue_test.go`；变异三轮全红（status 条件写入 / 标成 not-ready / 类型改成 string）。顺带把 `W5004` 的 help 换回规范推荐的 `catch (HttpError e) { e.status == 404 }`。**AOT（`hermes compile`，C 后端）侧也已打通**：同一份 `e.status` 静态契约在两条路径下都读到真实 HTTP 状态码，规范源见 `docs/AOT-ERROR-OBJECT.md`。**剩余缺口**：对宿主暴露的 unit AOT / `hermes cc` 的错误文本已由核心层那一行闭合（`hermes_last_error()` 现在返回真实原因，**公开 ABI 签名与冻结头一个字未动**）；7 语言 SDK 仍走 `err` 出参、不在这次改动内；32 位 target 仍未实机验证，见核心层那一行 |
| **同一条诊断在终端印两遍**（`#link …;` 的 `W2003` 与 `if (x.status == 404)` 的 `W5004` 各出现 2 次） | **已修（2026-11-10，`cmd/hermes/ir_build.go::dedupeDiags`）**。根因不在前端某处重复发，而是**同一个模块被前端解析了好几遍**：`api.cpp::run_parse` 是每个 C 入口（`hm_codegen_types` / `_wfdecls` / `_backends` / `_tools` / `_functions` / `_ext`）的公共前置，每趟都跑完整 lex + parse + check_types + check_idempotency，且每趟都把**整袋**诊断交回来 —— 不只是自己那一趟产出的。于是**模块级**诊断随每一趟各出一份（`E4027`/`E4010`/`E4033` 只印一遍，是因为它们的发出点只在其中一条路径上）。修法只能在 Go 渲染层：各趟的 `DiagBag` 是分开建的，跨袋去重在前端做不到；而**不能只保留某一趟** —— 各趟的 codegen 诊断是互补的（函数体的错只从 `_functions` 来、workflow 体的错只从 `_ext` 来），只能取并集再去重。判据：按「模块 + 严重级 + 码 + 行列区间 + 消息」逐字合并，`seen` 表**每次构建一份**（跨构建共享会让第二次编译静默吞掉诊断）。守卫 `TestDedupeDiagsKeepsDistinct`（5 个反例：同位置不同模块 / 不同严重级 / 不同码都必须各留一条）+ `TestDedupeDiagsSeenIsSharedAcrossCalls` + `TestBuildDedupesCrossPassDiagnostics`（端到端，用 `strings.Count` 而不是 `Contains` —— 缺陷本身是"印两遍"，`Contains` 对它完全无感）。三轮变异（去重空转 / key 去掉模块名 / 只共享一半的 `seen`）均转红。**未修的**：前端仍然把同一个模块解析 5~6 遍（纯性能，与诊断无关） |
| `match` 只接受字符串 | 接受整数（`E4019` 校验类型一致） |
| 一个 unit 只能一个 workflow | 支持多个，`entry` 唯一 |
| `#link` 不给 `-L` 只警告 | 默认严格，`--no-strict-link` 显式放宽 |
| 记忆 / 向量 | 内建 `memory.*` / `vector.*`，文件存储、零依赖 |
| agent 循环 | `model.agent`；`max_steps` 可以是常量（上界可证明），也可以是 `unlimited`（明说没有编译期上界，发 `W2004`，workflow 标 `unbounded`） |
| 类型检查跳过 `match` 臂 / `while` 体 / `for` 体 / `retry` 体 | 阶段 B 起 `check_type.cpp::stmt_()` 下潜这四类体，阶段 C 用 11 形态表驱动守卫防复发；修复前实测基线与根因见下方「阶段 B 前置基线」小节 |
| 没有「一行调用的降级路径」：只能靠 `try` / `catch` 包一整段 | **2.0.0 已实现 `fallback`**（`expr fallback { ... }`，`E4025` / `E4026`）。它是**表达式**、产出一个值、零栈展开；`try` / `catch` 仍是语句、按错误类型分派。两者并存不互相替代，区别见 `LANGUAGE.md` §5.4.1 |
| `retry` 的幂等证明只写在文档里：`check_idem.cpp` 不存在，`E5007`/`E5009` 零引用，`retry(2){ tool.call(POST) }` 能编出合法 IR | **已落地 `check_idem.cpp`**（挂在 `api.cpp::run_parse` 类型检查之后，所有入口共用）：`E5007` 拦未证明的调用，`E5009` 拦不纯 `idem_key`；`if` / `parallel` / `fallback` / 嵌套 `retry` 子树全覆盖。同时修掉三处「假证明」：① 未证明的 POST 曾被无条件标成 `idem.mode = "readonly"`，现改为新增的 `none`；② `idem_key` 只记了个 bool，`tools[].idem.key` 与 `args.idem_key` 都没发；③ 引擎从不注入 `Idempotency-Key` 头。现在源 → IR → 执行器整条链闭合，由 `cmd/hermes/cmd_idem_test.go` 与 `internal/engine/idem_test.go` 回归 |
| `float` 字面量静默损坏（历史）：IR 值层没有浮点，`gen_expr` 的 `LitExpr` 没有 float 分支，`return 1.5;` 会落进字符串兜底，编出 `{"t":"string","v":"float"}` | **v2.0.0 已换解**：浮点类型名统一为 `double`，字面量编成 `{"k":"double","bits":"<16 位小写 hex>"}` 位模式，IR 字节里没有 JSON 浮点数字。`E4029` 从「全拒」收窄为**旧 `float` 标签的纵深防线**（check_type 全 AST 扫描 + codegen `gen_expr` + 参数默认值折叠三道），防的是手工把标签改回 `"float"` 或直调 C ABI 绕过诊断时又产出 `{"t":"string","v":"float"}` 这种合法但静默损坏的 IR。`json.as_float(j)` 保留 API 原名、窄化到 `double`；语言里已无 `float` 类型 |

| 用户函数**实参类型**零校验（`add("x", 1)` 静默编过；`type_of_` 对 CallExpr 一律返回 `unknown()`，无任何 `E4xxx` 实参校验，是全局既有松点，非仅四类语句体独有） | **已落地 `E4030`（`check_type.cpp` 在 `type_of_` 的 `CallExpr` 分支）**：`collect_` 阶段建函数声明表 `fn_table_`；调用点比对每个实参表达式类型与形参声明类型（复用 `same_type_`，覆盖 base / struct / cost-vs-int 等），不匹配即拒；`checked_calls_` 按 `CallExpr` 节点去重避免重复报（同 `scan_float_` 思路）；并返回函数声明返回类型（此前一律 `unknown`）。**元数仍由 codegen 的 `E_IR_CODEGEN` 负责**，两道不重叠，避免与装配期诊断重复。 |
| `match` 重复 case 字面量：文档（`LANGUAGE.md` §4.2 / 附录 A）声称「重复字面量 → `E4020`」，但 `check_type.cpp` 从未发出 `E4020` —— 纯 stale 声明；重复 `case "a":` 能静默编过，第二个分支永不可达，是纯死代码 | **已落地 `E4020`（`check_type.cpp` 的 `MatchStmt` handler）**：遍历 `s->args` 上的 `MatchArm`，跳过 `is_nullable`（default）臂，字符串臂（`text!=nullptr`）与整型臂（`text==nullptr`，值取 `i64`）分别以 `"s:<v>"` / `"i:<v>"` 为键计数，重复即报 `E4020`；字符串与整型类型不同（严格相等）故分键，避免 `"5"` 与 `5` 误判重复。`default` 缺失仍由 `E4018` 负责、臂/主语类型仍由 `E4019` 负责，三道不重叠。遍历范围同 E4030（仅覆盖 workflow 体内 match，函数体内 match 仍漏检）。 |
| `E4022`/`E4023` 被**误用**于数组下标（`cannot index a value that is not an array` / `array index must be an integer`），而文档（§7.1.1 / 附录 A）把它们定义为「`return` 了 tuple」/「错误对象逃出 catch」—— 结果「错误对象逃出 catch」这条**根本没实现**（`auto x = e;` 静默编过） | **已修**：下标两条错各归新码 `E4031`（下标非整数）/ `E4032`（被索引值非数组）；`E4022` 回归「return 了 tuple」（原就在 ReturnStmt 里正确使用）；`E4023` 在 `LetStmt` handler 落地 —— catch 绑定打 `VarSym.is_error`，`auto x = e;` 的 init 是**单段 PathExpr**（段在 `subs`、`n_subs==1`）且名字是错误对象即拒，多段 `e.status` 放行。回归见 `internal/native/catch_escape_test.go`。 |
| **变量名遮蔽**（内层同名声明）整类失效。`codegen.cpp::var_index` 只按**名字**查表，声明与引用共用它，于是两个绑定共用一个槽位：① workflow 层遮蔽（`if` / `for` / `catch` 里再声明一次同名变量）→ 装配期回填 `vars[].decl` 只能指向其中一个，V-5 反向当场判「坏 IR」，报 `E_IR_VAR_MISMATCH ... (compiler bug, not an input problem)`；② 函数体内联路径（路线 B）→ **静默编出错值**（`keep(1)*10` 得 20 而非 10，`acc(7)` 得 3 而非 8），因为 `inline_locals_[name]` 被内层声明覆盖后没有出块还原；③ `arr_len_` 按名字记定长数组长度 → 内层短数组顶掉外层长度，外层长度 3 的 `xs[2]` 被误判越界。原始报障形态是「catch 嵌 fallback 再嵌 try」——真因与 try 无关，只是两层 `catch (ModelError e)` 同名 | **已修（`native/cpp/codegen.cpp`）**：新增**作用域栈** `binds_`（源码名 → 槽位）+ `scope_marks_`，并把「声明」与「引用」彻底分开：`decl_var` 永远分配新槽（同名可重名）、`ref_var` 取最内层同名绑定（与 `check_type.cpp::lookup_` 同一条规则，两处不一致就会编出「读到另一个变量」的合法但错误的 IR）。作用域边界落在 `case Kind::Block`（if/else/while/for/try/catch/retry/parallel 分支的体全走它）、`ForStmt`（循环变量）、`TryStmt` 每个 catch 臂（错误对象只活在自己的块里）、`run_function`（形参 + 函数体）、`InlineGuard`（内联实例整体快照/还原，含嵌套内联）。`arr_len_` 改为按**槽位**索引。不遮蔽的程序变量 id 与产物**逐字节不变**（19 份 golden 全过）。回归 `cmd/hermes/cmd_shadow_test.go`（5 例，逐个做过前后对照：修复前全部转红，其中内联两条是**错值**而非报错）。**剩余**：trace 的 `<run_id>/<var_name>` 与插值模板路径首段（`run.go::rootOf`）仍按名字查找，重名时取第一个匹配 —— 只影响人读的名字，执行期一律按 `var.id`；见 IR.md §11。 |
| ~~**`float` → `double` 改名 / 完整 binary64 未落地**~~ | **✅ 已闭合（v2.0.0 收口）**：语言唯一浮点类型是 `double`（IEEE-754 binary64），无 32 位 `float`；字面量编成 IR 位模式 `{"k":"double","bits":"<16 位小写 hex>"}`，IR 字节不含 JSON 浮点数字；`+ - * /` 与 `< <= > >=` 支持 double（源码层 `%` 由 `E4009` 限定为 int 操作数）；`call_native` 返回 double 真编译真跑已验证；`json.as_float` 保留 API 原名、窄化到 `double`。仍被拒：`double == double`（`E4033`）、`int`/`cost` 与 double 混用（`E4006` / `E4008`）、`tool.call<double>` |
| ~~**`serve` 租户预算没有周期性重置**~~ | **✅ 已闭合（v2.0.0 收口）**：`budget_reset` 纯 opt-in，优先级租户 > CLI `--budget-reset` > 默认 `none`，`daily` / `weekly` / `monthly` 按 UTC 日历窗口惰性归零；402 语义与默认 `none` 下的累计花费口径逐字不变 |
| ~~**AOT 错误对象没有把错误文本暴露给宿主**~~ | **✅ 已闭合（v2.0.0 收口）**：`hermes_last_error()` 不再是恒空占位，改读真实共享错误槽 `hermes_last_error_slot`（调用方不 free，每次入口先清槽）；`hermes cc` shim 与 unit AOT 的 `hm_fail` 都把文本 bounded-copy 进槽；公开 ABI 签名、4 个冻结头、导出函数集合一字未动，7 语言 SDK 仍走 `err` 出参 |
| ~~**`W5005`（`input: json` 入口无结构契约）规范有、代码无产出点**~~ | **✅ 已闭合（v2.0.0 收口）**：`input: json` 入口现在编译期发 `W5005`（`LANGUAGE.md` §4.8 与附录 A 的实现状态表已同步），`--deny-warnings` 也能升级它 |

**v2.0.0 已闭合（四项，逐条留档）**：`float→double` 完整 binary64、`serve` 预算 opt-in 重置、AOT 错误对象经 `hermes_last_error()` 对外暴露、`W5005`（见上表四行）。

**v2.0.0 已知限制（冻结前如实登记；每条一句原因）**：

- **32 位 target**：只有「按构造可移植 + 编译期布局断言 + 双 target 编译探针」，从未在真实 32 位硬件上编译 / 链接 / 运行过，故不能当成实机验证。
- **Linux / musl 静态链接**：本仓只实测过 Linux 动态链，静态 glibc 被 D-033 主动拒绝、musl 路径未跑，缺一台真 Linux 机器故不能靠推演下结论。
- **`build/glibhermesnet.a`**：这份真实 Go c-archive 未入库，端到端网络联编需手工构建；缺它时相关测试是 skip 而不是失败，404 / 503 端到端用手写 C 桩注入 Go 边界产出的纯文本。
- **C89 / C99 的线程局部降级**：`HERMES_THREAD_LOCAL` 在 C11+ / MSVC 下是线程局部，但 `hermes cc` 走 `-std=c89`、unit AOT 走 `-std=c99`，宏展开为空 ⇒ `hermes_last_error_slot` 退化为进程级全局，并发调用可能串错误文本。
- **schema 层刻意保留旧 `float` 标签**：`schemas/ir.schema.json` 的 `nodeLit.t.enum` 同时收 `"float"` 与 `"double"`，所以 schema 不拦 `{"t":"float","v":"..."}`，靠 C++ 前端 `E4029` 三道纵深防线兜底，防手工造 IR / 直调 C ABI 绕过诊断拿走坏 IR。
- **手写 IR / 直调 C ABI 的 double 相等两层不一致**：Go 引擎 `equalJSON` 没有 double 分支（只比 string / bool / int），C 后端 `op_eq` 有 `a.k==4&&b.k==4` 的 double 分支；源码层 `double == double` 被 `E4033` 挡住，故正常路径不可达。
- **NaN 位模式不保证跨主机唯一**：IR 用 16 位小写 hex 存 binary64 位模式，NaN 的 payload 依赖平台 / 编译器，跨主机不保证字节唯一；语言也没有 NaN 字面量语法，非有限值只能由运算产生并在 JSON 边界被拒。
- **值层与源码层的能力差**：IR / Go 引擎（`math.Mod`）与 C 后端（`hm_fmod`）都实现了 double 取余，但源码层 `check_type.cpp` 的 `E4009` 把 `%` 限定为 int 操作数；这是**有意的语言面约束**，double 取余不作为 v2.0.0 语言特性对外承诺。

### 阶段 B 前置基线：类型检查不进入四类语句体（2026-11-08 实测）

**类型检查不进入 `match` 臂 / `while` 体 / `for` 体 / `retry` 体。这四个体内的一切源码类型错误零诊断通过。`E5010` 只是这条缺口的一个可见症状，不是它的边界。**

这不是「E5010 少覆盖一个边界」，而是**类型检查器跳过了一整类语句体**：这四个体里的字段访问错（`E4016`）、返回类型错（`E4021`）、实参类型错全部零诊断编成 IR，运行期才炸；而语言文档写的是静态检查。`if` / `else` / `try` / `catch` / `parallel` 分支都正常下潜。

探针（`n` 是 int，对 int 取字段，源码类型检查本应报 `E4016`）：

```hermes
auto n = 1;
auto bad = n.foo;
```

把这两行放进各语句形态的体里，复跑 `build/hermes ir`（HEAD `7e0b91e`，修复前）：

| 位置 | 用户复现 | 复跑 `IR_EXIT` | 精确诊断 |
|---|---|---|---|
| 顶层 | ✓ 报 E4016 | 4 | `1 × error[E4016]` |
| if | ✓ 报 E4016 | 4 | `1 × error[E4016]` |
| try | ✓ 报 E4016 | 4 | `1 × error[E4016]` |
| parallel | ✓ 报 E4016 | 4 | `1 × error[E4016]` |
| match 臂 | ✗ 0 诊断 | 0 | 0 诊断，写出 IR |
| while 体 | ✗ 0 诊断 | 0 | 0 诊断，写出 IR |
| for 体 | ✗ 0 诊断 | 0 | 0 诊断，写出 IR |
| retry 体 | ✗ 0 诊断 | 0 | 0 诊断，写出 IR |

（用户原文 7 行、`if / try` 合写；QA 按位置拆成 8 行分别复跑，逐行一致。0 诊断的 IR 内 `diag_counts={"error":0,"warning":0}`，且 `field name=foo` / `return` / `int` 字面量等坏构造确实编进 IR，不是被丢弃。）

扩到 11 种语句形态（阶段 C 表驱动守卫的用例源，同一处探针）：修复前 6 个报 `E4016`（顶层 / if / else / try / catch / parallel 分支），5 个 0 诊断（`match` 臂 / `match` default / `while` 体 / `for` 体 / `retry` 体）—— 5 个坏形态对应 4 类语句，`match` 的两个臂各算一个形态，缺一个守卫就复发。

**边界校准（QA 实测，防止「文档声称 ≠ 代码行为」）**：`hermes ir` 输出人类可读诊断而非 JSON；上面的「零诊断」指**源码类型诊断**（`error[E[0-9]{4}]`）。
- 函数调用**参数个数**错不是零诊断：`codegen.cpp:1085` 在 IR 装配期报 `error[E_IR_CODEGEN]: function add takes 2 argument(s), got 1`，`IR_EXIT=4`、**不写出 IR**；顶层与四个体内表现完全相同，与遍历缺口无关。
- 函数调用**实参类型**错在顶层同样 0 诊断（检查器没有任何实参类型校验的 `E4xxx`），是另一条全局既有松点，不是这四个体独有。
- 「变量类型全部退化为 json」**未观察到**可观察证据：`explain` 不输出变量类型（`vars 2` 只是计数）；IR 的 `workflows[].vars[].t` 在顶层、显式 `int n = 1;`、四个体内一律是 `{"k":"json"}`，是全局统一表示。准确说法是「这四个体内根本没做类型推导」——`E4016` 在顶层报 `on int`，在这些体里连类型都没算。
- 修复前 `examples/**/*.hermes`（5 个）、`cmd/hermes/testdata/**/*.hermes` 与 `internal/demo/demo.hermes`（18 个）共 23 个 `.hermes` 全部零诊断（`tests/**/*.hermes` 为 0 个文件）——阶段 B 冒出的任何源码诊断都是新增。

**根因 = 纯遍历遗漏 + 槽位读错，不是「为不动点选择不下潜」**。`Checker` 没有任何收敛机制：`scope_` 是一次性绑定表、`lookup_` 倒序线性查无 merge、`type_of_` 单次现算无缓存、`Ty` 无 lattice / join，全文件无 `converge / iterate / merge`。反证：`match` 臂体只执行一次却同样漏，`if` / `else` / `try` / `catch` 也只一次却被正常检查。所以这里是「从零补一次遍历」，不是修正收敛策略，也不需要「循环内变量退化 json」的降级。

**当前实现修法**：阶段 B 让 `check_type.cpp::stmt_()` **必须下潜** `match` 臂 / `while` 体 / `for` 体 / `retry` 体（跳过不行；仅当实测遇到「循环体内赋值导致误报」才允许把循环内被赋值的变量退化为 json，且仍必须下潜）；阶段 C 用 `internal/native/parse_test.go` 的 11 形态表驱动守卫 + 两条元守卫防复发。

## 1.2.2 新增（当前实现）

| 能力 | 说明 |
|---|---|
| `for (int i = 0; i < N; i++)` | C 风格四段式，支持 `i++`/`++i`/`i--`/`--i`/`i += k`/`i -= k`；循环变量由 `for` 独占，不破坏"变量只声明一次" |
| `unbounded` 标注 | 执行次数在编译期算不出来时（`while`、步进推不出次数的 `for`、递归 `call_fn`、`max_steps = unlimited` 的 agent），`explain` 明确显示"未知"，而不是给一个假数字 |
| 语言包 | `i18n/hermes.rp`（222 条，embed 进二进制）；`hermes i18n check/export/import`；中文只在包里 |
| 版本检测与升级 | `hermes version --check`、`hermes upgrade`（校验 SHA256 后才下载，不原地替换） |
| 记忆 TTL | `memory.put(..., ttl = "7d")`，惰性删除，旧文件不需要迁移 |
| 删除 | `memory.delete` / `vector.delete`，跨命名空间会被拦住 |
| 向量检索提速 | 预存模长（float64），分数与逐次重算**完全一致** |
| 诊断渲染 | `error[CODE]` + 源码片段 + `^`（含跨行）+ `why`/`help` |

## 开发流程

提交与推送走 `scripts/push.sh`（先跑 `check.sh`，不通过就停）：

```bash
bash scripts/push.sh            # 校验 → amend → 推 main 与标签
bash scripts/push.sh "消息"      # 自定义 commit 消息
```

理由：仓库长期只有一个 commit，推上去就是改写历史，没有"先推后修"的余地。
把顺序写进脚本比依赖记性可靠——本次开发里出现过两次红灯推送，原因都是
命令中 `;` 与 `&&` 的区别。
