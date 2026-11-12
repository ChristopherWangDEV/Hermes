# 标准库的实现可选性（g\*/G* 机制）

> 状态：**规范性**。本文是 g\*/G* 命名约定与「App 层可选标准库」机制的**唯一规范源**。  
> Hermes 语言主体 = **LLVM / Rust / C / C++**，标准库是 **App 层可选件**。
> 上游：`HANDOFF_TO_INTL.md` §四/§五、`docs/LANGUAGE.md` 的 `#link <std/...>` 语义。

## §1 一句话结论

- **语言主体** = LLVM / Rust / C / C++。Go 只用于工具链与**标准库的可选实现**，绝不作为语言主体。
- **标准库是 App 层可选件**：每个 `std` 成员都可有多个实现语言变体；涉及 Go 特性且 Go 版更优时提供 *g/G* &#x7248;**（Go 实现），同时始终提供**非 Go 版\*\*（纯 C / Rust 主体实现），由 App 层选择。
- *g/G* 命名约定\*\*：Go 版实现一律带 `g`/`G` 前缀，与纯 C / Rust 静态库做命名空间隔离（与 `internal/toolchain` 的 `g*` 策略一致）：
  | std 成员        | Go 版（g\*/G\*） | 非 Go 版（成员本名） |
  | ------------- | ------------- | ------------ |
  | `std/net`     | `gnet`        | `net`        |
  | `std/model`   | `gmodel`      | `model`      |
  | `std/runtime` | `gruntime`    | `runtime`    |
  | `std/json`    | `gjson`       | `json`       |
- **绝不能写成「App 层 = Go」或「全仓 Go 化」**。标准库的实现语言是选修课，不是必修课。

## §2 App 层变体开关（按选择取实现）

变体通过环境变量 **`HERMES_STD_VARIANT`** 在**运行期**选择：

| 取值        | 含义             | 走哪条实现         |
| --------- | -------------- | ------------- |
| 缺省 / 非法   | 回退到 `go`（默认变体） | Go 版（g\*/G\*） |
| `c` / `C` | 选纯 C 主体实现      | 非 Go 版（成员本名）  |

- **默认变体 = `go`，等于今天的行为**。不显式选择时，解析、哈希、运行期行为都与今天**逐字节一致**——这是向后兼容的硬保证。
- **为什么是运行期、不写进 IR、不改 `requires` 哈希**：std 依赖的 `member_sha256` 是**供应链防投毒**的核心判据（`internal/link/link.go` 的 `VerifyRequires`）。若把变体写进 IR 或用于哈希，就会改变每条依赖的摘要，逼整个基线重算、破坏可重现性。运行期开关让「选哪个实现」与「依赖了哪份字节」彻底解耦：无论选 go 还是 c，IR 与 `requires` 哈希都不变。
- **链接层仍按选择校验**（`internal/link/link.go`）：链接期对每条 `<std/x>` 依赖，调用 `std.ImplOf(mod, std.SelectedVariant())` 确认该变体切实提供了该成员，否则报 `E_LINK_STD_VARIANT`。默认变体下四个成员都有实现，因此不引入新错误。

## §3 非 Go 版的具体落地（已提供）

至少一个非 Go 版标准库实现必须真实存在，否则 g\*/G* 只是空名。当前已落地：

- **`std/runtime` 的预算检查 = 纯 C 非 Go 实现**：`internal/std/rtbudget/rtbudget.c`（配 `rtbudget.h`，经 cgo 桥 `rtbudget_cgo.go` 暴露）。当 `HERMES_STD_VARIANT=c` 时，`internal/budget.exceeds` 改走 `hermes_rt_budget_exceeded`，语义与 Go 版 `internal/budget.exceeds` **逐字一致**（三维预算、`<0` 维度不限、任一维度越过即拒绝）。
- 该 C 实现刻意**不放在 `native/include/`**：那里是 4 个冻结头，受 `ABI_BUILD_ID` 哈希守卫，改一个字节都会逼 `native/rust/src/lib.rs` 重算。`rtbudget.h` 是独立的、非冻结的纯 C 声明。
- **行为等价是被测试钉死的**：`internal/budget/budget_variant_test.go` 断言——`c` 变体下 C 计数器严格增加（证明真走到了非 Go 实现），且 `c` 与 `go` 对同一预算场景给出**完全一致**的接受/拒绝决策。

> 其余成员（`net` / `model` / `json`）的 Go 版（gnet / gmodel / gjson）已就位；它们的纯 C / Rust 非 Go 版是同一机制的自然扩展点，新增时只改 `internal/std/variant.go` 的 `impls` 表与对应实现，不改 IR、不改链接哈希。

## §4 不变式（验收口径）

1. 开关可在 Go 版 / 非 Go 版间选择，**默认行为与今天一致**。
2. g\*/G* 命名自洽，有本文件作为规范源；`internal/std/variant.go` 的 `impls` 表是唯一事实源。
3. 至少一条端到端测试证明「选非 Go 版」真走非 Go 实现、「选 Go 版」走 Go 实现，且决策一致。
4. 四条基线（`go build ./...`、`go test -race -count=1 ./...`、`bash tests/compiler/run.sh`、`./build/hermes i18n check` / `doctor`）全绿；`gofmt` 干净；不引入第三方依赖；不动两条冻结头。

