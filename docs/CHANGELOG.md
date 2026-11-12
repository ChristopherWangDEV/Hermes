# 变更记录

## 2.0.0

> **2.x 整条线是 LTS。** 2.0.0 是这条线的起点；版本号本身不带 LTS 后缀，
> 承诺写在 README 的长期支持一节里。

### 平台支持

| 平台 | 产物 | 状态 |
|---|---|---|
| macOS arm64 | `hermes-2.0.0-darwin-arm64.tar.gz` | 全功能（含 C 嵌入库） |
| Linux x86_64 | `hermes-2.0.0-linux-x86_64.tar.gz` | 全功能（含 C 嵌入库） |
| Windows x64 | `hermes-2.0.0-windows-x64.zip` | CLI 全功能；**不含 C 嵌入库**（原因见下） |

### Windows：为什么这个包不含 `libhermesrt.a`

这是一个**受工具链约束的取舍**，不是遗漏。

Go 的 cgo 在构建运行时库（`-buildmode=c-archive`）时会先做一次
**可重定位链接**（`link -r`）来生成探针对象 `_cgo_.o`。而：

- LLVM 的 `lld-link` **不支持** `-r`（实测报 `subsystem must be defined`）；
- 能满足它的只有 MSVC 的 `link.exe` 或 MinGW 的 GNU `ld`。

本项目的红线是"编译只用 clang、不用 GNU、不用 MSVC 编译器"。既然
`lld-link` 做不到、另外两个又被排除，Windows 上就不发布运行时库。

**影响**：Windows 上不提供"C 程序嵌 hermes 运行时"这条路径。CLI
（`ir` / `run` / `build` / `serve` / `compile` / SDK）完整可用。
`hermes doctor` 在 Windows 上把该项降级为 `warn`，并直接写明原因，而不是
假装通过：

```
warn  runtime lib   not shipped on Windows (C embedding stays on macOS/Linux); CLI is fully functional
```

C 嵌入能力保留在 macOS 与 Linux 两个平台上。

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
