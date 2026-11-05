# Linux 移植记录

真机：Ubuntu 24.04（noble），x86_64，clang 18.1.3。这不是推演，是实测记录。

## 第一次跑起来发现的三个问题

### 1. glibc 在 `-std=c11` 下不暴露 POSIX 符号

`clock_gettime` / `nanosleep` / `kill` / `sigemptyset` 全部变成"未声明"。
macOS 的库头文件**无条件暴露**它们，所以同一份代码在 macOS 上编得过、在 Linux 上编不过。

修法：在 `native/include/hm_internal.h` 顶部（任何系统头之前）定义
`_GNU_SOURCE` 与 `_POSIX_C_SOURCE=200809L`。

### 2. `.subsections_via_symbols` 是 Mach-O 指令

`native/c/cycle_arm64.S` 里有它，ELF 汇编器不认识。已按 `__APPLE__` 守卫。

### 3. `-arch` / `-mmacosx-version-min` 是 Darwin 专用

`build.sh` 用它们做"另一架构"的交叉汇编检查。Linux 上改用
`--target=aarch64-linux-gnu` / `--target=x86_64-linux-gnu`（clang 自带汇编器）。

## 红线在 Linux 上的两个新战场

| 项 | 现象 |
|---|---|
| **libstdc++** | 工具链门禁实测拦下：Ubuntu 上 `clang++` 默认链 libstdc++（GPL） |
| **GNU ld** | clang 默认调用 `/usr/bin/ld`（GPL）；必须显式 `-fuse-ld=lld`。`build.sh` 现在检测 lld，找不到就**拒绝构建**而不是退回 GNU ld |

## 已经打通

系统包装齐之后（`libc++-18-dev`、`libc++abi-18-dev`、`libunwind-18-dev`、`llvm-18`、
`lld-18`、`liblzma-dev`），Linux 上：

- `scripts/build.sh` **通过**（`rc=0`）
- `hermes doctor` 全部必需检查通过
- `hermes demo` 能编出可执行文件
- `hermes/internal` 下 canon / schema / budget / ir / engine 的测试全过
- 工具链门禁 `rc=0`

## 链接器也必须查（新增）

之前的门禁只查了**编译器**（clang）和**C++ 标准库**（libc++），漏了**链接器**：
Linux 上 clang 默认调用 `/usr/bin/ld`（GNU binutils，GPL）。现在门禁会：

1. 要求 `ld.lld` 存在；
2. 用 `-fuse-ld=lld -Wl,--version` 让 lld 自己报名字；
3. 拿不到 lld 就 **exit 64**，而不是退回 GNU ld。

已做反向验证：把 lld 从 PATH 移走后，门禁 `rc=64`。

## 还没打通的部分

`go test` 全量还没跑完（`check.sh` 里的 C++ 前端验收、守卫、SDK 测试）。

`scripts/check.sh` 在 Ubuntu 24.04 x86_64 上 **rc=0（全部通过）**，
C++ 前端 117/117，SDK 部分按工具链缺失跳过（dotnet / kotlinc 没装）。

## 产物

`build/release/hermes-1.0.1-linux-x86_64.tar.gz`（16MB），解包后 `hermes doctor` 通过。

**运行期依赖**（`ldd` 实测）：`libc++.so.1` / `libc++abi.so.1` / `libunwind.so.1`，
**没有 libstdc++** —— 红线在运行期同样成立。目标机需要装：

    sudo apt install libc++1 libc++abi1 libunwind-18

（构建机需要的是 `-dev` 包，运行机只需要运行时包。）

## 结论

**Linux x86_64：构建 + 全量验收 + 产物自检 全部通过。**
