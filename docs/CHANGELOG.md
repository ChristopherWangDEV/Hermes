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

生成代码里的平台差异收口在一处，覆盖 **Windows / macOS / Linux / FreeBSD / OpenBSD /
NetBSD / DragonFly**，未知平台 `#error` 而不是静默落到 POSIX 分支。

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

### Windows 的 C 嵌入：LLVM-MinGW

Go 的 cgo 在构建运行时库（`-buildmode=c-archive`）时会先做一次
**可重定位链接**（`-r`）来生成探针对象 `_cgo_.o`：

- LLVM 的 `lld-link`（MSVC 目标）**不支持** `-r`；
- 而 LLVM-MinGW 走的是 **GNU 味的 `ld.lld`**，它支持。

所以 Windows 上能否提供 C 嵌入，取决于**用哪条工具链**，而不是平台本身：

| 构建路径 | 目标三元组 | 链接器 | C 嵌入库 |
|---|---|---|---|
| LLVM-MinGW | `x86_64-w64-windows-gnu` | `ld.lld`（GNU 味） | ✅ 有 |
| 纯 LLVM | `x86_64-pc-windows-msvc` | `lld-link` | ❌ 跳过（温和降级） |

**为什么是 LLVM-MinGW 而不是 GNU MinGW**：GNU MinGW 带 GCC 与
`libstdc++`（GPL-3.0 WITH GCC Runtime Library Exception）。LLVM-MinGW 是
Clang/LLVM + MinGW-w64 运行时 + **libc++**（Apache-2.0 WITH LLVM-exception），
全程无 GCC。`scripts/build.ps1` 里的 libc++ 探针就是用来把两者分开的：
探针失败 = 不是 LLVM-MinGW，拒绝使用。

未检测到 LLVM-MinGW 时脚本**不报错**，而是只产 `hermes.exe` 并在
`hermes doctor` 里给出开启方式：

```
warn  runtime lib   not built on Windows with the msvc target (lld-link has no -r).
                    CLI is fully functional; to enable C embedding, build with LLVM-MinGW:
                    set LLVM_MINGW_PATH, run `rustup target add x86_64-pc-windows-gnu`,
                    then `pwsh -File scripts\build.ps1 -Toolchain gnu`
```

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
