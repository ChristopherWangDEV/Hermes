# Third-Party Notices

Hermes 的**分发**里包含第三方代码。这份文件说明它们是什么、什么许可、
以及分发时必须随附哪些文本。

> 这不是法律意见。它做的是把"分发时该带什么"变成可核对的事实：
> 每一项都指向 `licenses/` 里的原文。

## 链接进 `hermes` 二进制的第三方代码

| 组件 | 版本 | 许可 | 分发要求 |
|---|---|---|---|
| Go 运行时（标准库） | 见 `hermes version` 的 go 行 | BSD-3-Clause | 随附版权与许可文本（本仓库 `licenses/go-LICENSE`） |
| `sha2` | 0.10.9 | MIT OR Apache-2.0 | 随附 MIT 文本与版权行（`licenses/sha2-LICENSE-MIT`） |
| `zeroize` | 1.9.1 | MIT OR Apache-2.0 | 随附 MIT 文本与版权行（`licenses/zeroize-LICENSE-MIT`） |
| libc++（仅当静态链入时） | Apple/LLVM 版本 | Apache-2.0 WITH LLVM-exception | 随附 Apache-2.0 与例外文本 |

**注意**：`hermes compile` 产出的可执行文件里**嵌了 Go 运行时**。
你分发那个产物时，义务随它走 —— 也就是说，**你的分发也要带 Go 的许可文本**。
本仓库在 `licenses/` 里提供了原文，直接复制即可。这一条是"用户产物"与
"Hermes 本体"之间的界线，值得单独记住。

## 客户端 SDK

八个 SDK（`packages/`）**零第三方运行时依赖**：Python 用 `ctypes`、Java/Kotlin 用 JDK 的
FFM、C# 用 BCL 的 P/Invoke、Go 用 cgo 的 `dlopen`、Rust 用 `extern "C"`、Node 用官方
N-API 头自己写插件。它们只依赖各自语言的标准库，因此不引入新的许可义务。

Kotlin 的测试产物用 `-include-runtime` 把 kotlin-stdlib（Apache-2.0）打进 jar ——
**那是测试用，不是分发形态**；作为库发布时应由使用者的构建解析 stdlib。

`tests/guard/sdk_redline_test.go` 机器检查这一点：任何 SDK 的清单里出现运行期依赖即红。

## 不随分发的东西

- 开发期工具：`git`、`ar`、`python3`、`node`、`cargo` —— **不分发即不触发**（§1.4）。
- macOS 系统框架（CoreFoundation / Security / libSystem）：系统库，不随 Hermes 分发。
- Node 头文件（`node_api.h`）：只在构建插件时读取，不随分发。

## 机器化保证

这些声明不是靠自觉维持的：

- `tests/guard/license_sweep_test.go`：`go.sum` 必须为空、Cargo 依赖走白名单、禁 `vendor/`
- `tests/guard/license_scope_test.go`：LICENSE 是 Apache-2.0 全文、NOTICE 划清范围
- `tests/guard/redline_test.go`：脚本里出现获取类命令（curl/pip/npm install/git clone）即红
- `tests/guard/release_layout_test.go`：发布包必须带齐许可文件才能算通过

## 如果你要再分发

最少带这些（发布包里已经都在）：

```
LICENSE                 Hermes 本体（Apache-2.0）
NOTICE                  归属与范围
THIRD_PARTY_NOTICES.md  这份文件
licenses/               所有第三方许可原文
```
