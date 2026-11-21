# 变更记录

## 2.0.1

> **2.x 整条线是 LTS。** 2.0.1 是这条线上的第二个版本；版本号本身不带 LTS 后缀，
> 承诺写在 README 的长期支持一节里。

### 语言：控制流与事件模型

| 特性 | 说明 |
|---|---|
| `void` | 函数可声明返回 `void`；`auto a = voidf();` 之类**当值使用**在编译期直接拒绝（E4101–E4104） |
| `stop` | `workflow.stop(reason, data)` 提前结束任务。与 `break` 语义正交：`break` 只跳出循环，`stop` 只终止 workflow |
| `signal` / `on` / `emit` | 信号声明、槽绑定、发射。冒泡**短路**：槽返回已处理即截断 |
| `sleep` | `sleep(1500)` / `sleep(1.5s)`；单位在编译期折算成毫秒 |
| `until` | 等待信号，**边沿触发**：等的是"接下来的任意一次 emit"，历史 emit 不会让它立刻返回 |

四个新关键字（`stop` / `signal` / `on` / `emit`）都是**上下文关键字**：词法器不新增保留字，
旧代码里作为变量名的 `stop` 照常工作。

**声明优先**：如果本单元定义了同名函数（`emit` / `stop` / `sleep` / `until`），
语法器在解析前先扫一遍顶层函数名，遇到同名就让位给普通调用——不是静默忽略用户函数。
单向的判据由 `cmd/hermes/contextual_keyword_test.go` 钉住（有同名 → 不出内置节点；
没有 → 必须是内置节点）。

### 闭包转换与零开销派发

槽编译成独立函数，捕获的外层局部变量打包进上下文结构体：

```c
struct hm_ctx_7 { Val v0; Val v1; };
static int hm_slot_7(void* raw) { struct hm_ctx_7* c = ...; c->v1 = ...; return 1; }
```

- **写回**：调用返回后把 ctx 字段写回调用方变量——不写回则"能编能跑、结果永远不变"
- **短路**：认领即 `goto` 跨过全局派发那一行，靠**控制流**而不是标志位保证不重复调用
- **静态优先**：同单元的 `emit`/`on` 在编译期配对，连查表都没有
- **全局兜底**：只有 `export signal` 的槽进全局表，静态未命中时才查表

### PAL：平台抽象层

生成代码里的平台差异收口在一处。**承诺范围**：Windows（LLVM-MinGW）/ macOS / Linux /
**FreeBSD**（按 macOS 的方式做交叉编译，不依赖 BSD 真机链接）；**OpenBSD / NetBSD /
DragonFly 仅 best-effort**——代码里有对应分支，但不承诺可用、也未真机验证。
未知平台 `#error` 而不是静默落到 POSIX 分支。

| 能力 | Windows | POSIX |
|---|---|---|
| 睡眠 | `Sleep` | `nanosleep` |
| 单调时钟 | `QueryPerformanceCounter` | `mach_absolute_time` / `clock_gettime` |
| 条件变量 | `CONDITION_VARIABLE` + `CRITICAL_SECTION` | `pthread_cond_timedwait` |
| 互斥量 | `CRITICAL_SECTION` | `pthread_mutex_t` |
| 线程 | `CreateThread` + 蹦床 | `pthread_create` |

单位写进函数名（`_ms` / `_ns`）。POSIX 的绝对 deadline 与 Windows 的相对毫秒各自只换算一次。

### 零解释器是**可验证**的性质

生成的 C 里不含 `dlopen` / `dlsym`——有一条守卫扫描产物文本。动态加载能力因此不进默认
prelude：需要时以可选 prelude 引入，让"所有符号在链接期解析"成为可 `grep` 的事实，
而不是一句承诺。

### 编译器：Go → C++ 迁移（本版最大的一处结构性变化）

`hermes` 的**命令层与编译链路已整体搬进 C++**，Go 只剩三类必须留在它那边的能力：
TLS（`serve` / `run` 的网络部分）、Ed25519（`sign` / `verify` / `build -k`）、
以及解释器本身（`run` 的求值循环）。

| 已搬到 C++ | 判据 |
|---|---|
| `version` `fmt` `deps` `ir` `build` `cc` `header` `ar` `doctor` `compile` `new` `import` `licenses` `lsp` `explain` `link` `trace` | 与搬迁前的 Go 实现**逐字节/逐字**比对 |
| clang 风格入口（`hermes app.hermes -o app` / `-c` / `-E`）| `-c` 的产物与 `hermes build` 逐字节相同 |
| 诊断渲染（片段、文案、三种截断口径）| 224 条诊断 golden |
| `.hbc` 读 / 写 / 索引哈希、许可闭集、目标分类、链接计划 | 与 Go 侧逐条等价，含**拒绝码与拒绝文案** |
| `--target` / `--sysroot` / `--static` / `--link-*` / 产物侧红线 | 拒绝文案逐字一致 |

**这对用户意味着什么**：命令行行为不变（同一批旗标、同一批退出码、同一批文案），
但 `check.sh` 现在有 4600+ 条 C++ 测试在盯这些等价性；`hermes build` 产出的 `.hbc`
与 2.0.0 逐字节相同。

**三类"永久留在 Go"的能力**（不是没搬完，是不该搬）：

- **TLS**：Go 标准库自带；C++ 侧要么引第三方（违反零依赖红线），要么自己写 TLS（不该写）。
- **Ed25519**：同上。`build -k` 因此拆成两半——编译在 C++，签名在 Go。
- **解释器**：`hermes run` 的求值循环。

**没有变的红线**：`go.mod` 零 `require`；Rust 只有 `sha2` + `zeroize`；只用
clang / clang++ / libc++；零费用。

### 契约编程：`require` / `ensure`（D-036）

签名区可写契约子句，位置固定在**参数列表之后、`with` / `entry` / `-> T` 之前**：

```hermes
export workflow analyze(input: Req)
  require(input.budget > 0)
  ensure(result != "")
  entry -> string { ... }
```

- 条件必须是 `bool`（`E4116`）；`require` 里引用 `result` 是错（`E4117`）；
  子句必须**纯**（`E4118`：`tool.*` / `model.*` / `memory.*` / 本 unit 用户函数一律拒）；
  子句只允许在签名区（`E4119`）。
- 失败抛 **`ContractError`**（既有错误类型，不新增），可被 `fallback` / `retry` /
  `try { } catch (ContractError e)` 原样接住。
- **AOT 与 `hermes run` 是同一套语义**：同一个 `.hbc`，两侧拦同样的东西。
- 函数上的子句 2.0.1 **尚未支持**，写在那里会得到 `E4120`（点名限制，而不是被忽略）。

### 修复：`max_steps = unlimited` 不再有人为硬上限

`unlimited` 的字面含义就是无上限。以前运行期塞了一个 1e6 的硬顶——那等于把
"作者说无限"改成"作者说无限但我们不同意"，用户看到的是循环跑了很多轮之后
莫名失败，而失败理由与真实原因无关。

现在没有静态上界时**不设人为步数/工具调用上限**；"一定会停"改由不属于步数预算的
机制保证：上下文取消/超时（每轮都查）、作者声明的 cost/token 预算、进程外限制。

### 平台

- **FreeBSD**：编译器取自身路径改用 `sysctl(KERN_PROC_PATHNAME)`（FreeBSD 的
  procfs 是可选的，`/proc/self/exe` 不可靠）。三个平台分支在 `#if` 下互斥，
  因此在 macOS 上编译时 FreeBSD 那段不参与编译——**未在真机验证**，这一点如实记录。

### 兼容性

- `ir_version` 保持 `2`；新节点（`stop` / `mem_txn` / `emit` / `on` / `sleep` / `until`）都是**增量**
- `include/hermes/hermes_core.h` 未改动，C ABI 仍是 `2.0`
- 2.0.0 的源码无需任何修改即可编译

## 2.0.0

> **2.x 整条线是 LTS。** 2.0.0 是这条线的起点；版本号本身不带 LTS 后缀，
> 承诺写在 README 的长期支持一节里。

### 平台支持

| 平台 | 产物 | 状态 |
|---|---|---|
| macOS arm64 | `hermes-2.0.0-darwin-arm64.tar.gz` | 全功能（含 C 嵌入库） |
| Linux x86_64 | `hermes-2.0.0-linux-x86_64.tar.gz` | 全功能（含 C 嵌入库） |
| Windows x64 | `hermes-2.0.0-windows-x64.zip` | CLI 全功能；**不含 C 嵌入库**（原因见下） |

### Windows：只用 LLVM-MinGW

Go 的 cgo 在构建运行时库（`-buildmode=c-archive`）时会先做一次
**可重定位链接**（`-r`）来生成探针对象 `_cgo_.o`：

- MSVC 目标的 `lld-link` **不支持** `-r`，因此那条路径永远产不出 C 嵌入库；
- **LLVM-MinGW** 走 GNU 味的 `ld.lld`，它支持。

既然 Windows 的完整形态必须包含 C 嵌入，就只保留能做到这一点的工具链：

| 工具链 | 目标三元组 | 链接器 | `-r` | C 嵌入 |
|---|---|---|---|---|
| **LLVM-MinGW** | `x86_64-w64-windows-gnu` | `ld.lld` | ✅ | ✅ |
| ~~MSVC 目标~~ | ~~`x86_64-pc-windows-msvc`~~ | ~~`lld-link`~~ | ❌ | 已删除 |
| ~~GNU MinGW~~ | — | — | — | 红线禁止（libstdc++ 为 GPL） |

**为什么不是 GNU MinGW**：它带 GCC 与 `libstdc++`。LLVM-MinGW 是
Clang/LLVM + MinGW-w64 运行时 + **libc++**（Apache-2.0 WITH LLVM-exception），
全程无 GCC。`scripts/build.ps1` 里的 libc++ 探针就是这个判据，由编译器
自己回答：

```c
#if defined(__GLIBCXX__)
#error libstdc++ in use (GPL)
#endif
#if !defined(_LIBCPP_VERSION)
#error not libc++
#endif
```

探针失败 = 不是 LLVM-MinGW = 拒绝构建。不看目录名字，只看编译器实际用哪个库。

### Windows 构建前提

1. 安装 LLVM-MinGW（`mstorsjo/llvm-mingw` 的 release，解压即用）
2. `setx LLVM_MINGW_PATH <解压目录>`
3. `rustup target add x86_64-pc-windows-gnu`
4. `pwsh -File scripts\build.ps1`

缺任何一项，脚本会在**开始编译之前**停下并给出上面这几步，而不是编到一半
报一个看不懂的链接错误。

### 红线核对（可验证的事实）

| 项 | 结论 | 怎么验证 |
|---|---|---|
| 编译器 | 只用 clang（Windows 上实测 23.1.4） | `hermes doctor` 的 clang 行；`clang --version` |
| 链接器 | 只用 LLVM `lld-link` | `clang -v` 的输出里出现 `lld-link`，不出现 `link.exe` / `ld` |
| 产物依赖 | 不含 GNU 运行库 | `llvm-objdump -p hermes.exe` 的导入表无 `libgcc` / `libstdc++` / `libwinpthread` |
| 依赖数量 | Go 侧零第三方（`go list -m all` 只有自身）；Rust 仅 `sha2`、`zeroize` | `scripts/check.sh` 的守卫测试 |
| 许可 | Apache-2.0，允许闭源商用 | `LICENSE`、`NOTICE`、`THIRD_PARTY_NOTICES.md`、`licenses/` |
| 费用 | 构建链全部是系统已装的开源工具，无付费服务依赖 | `scripts/build.sh` / `scripts/build.ps1` 无网络安装步骤 |

`scripts/build.ps1` 把上表的前三条写成了**脚本里的断言**：构建前检查 clang
可用，构建后解析 PE 导入表，一旦出现 GNU 运行库就直接失败。

### 验证命令

```sh
# macOS / Linux
bash scripts/build.sh && bash scripts/check.sh

# Windows（普通 PowerShell，无需管理员）
powershell -File scripts\build.ps1
.\build\hermes.exe version
.\build\hermes.exe doctor
```
