# Hermes for VS Code

[HermesScript](https://example.invalid/hermes) 的语言支持：语法高亮、诊断、补全、悬浮提示、大纲。

## 它能做什么

- **语法高亮**：`#include` / `#link` 指令、关键字、类型、内建（`model.generate`、`tool.call`、`json.as_*`）、字符串插值
- **诊断**：写错的语法立刻出红线，消息里带「为什么」与「怎么改」
- **补全**：关键字与内建命名空间（输入 `.` 后触发）
- **悬浮**：光标停在关键字/内建/struct 上显示说明与字段
- **大纲**：workflow / struct / backend 列表

## 安装

1. 先装 Hermes 本体（`hermes` 需要在 `PATH` 里）：
   ```sh
   ./scripts/install.sh hermes-<version>-<os>-<arch>.tar.gz
   ```
2. 安装本扩展（`.vsix`）：
   ```sh
   code --install-extension hermes-vscode-0.1.0.vsix
   ```

## 设置

| 设置 | 默认 | 说明 |
|---|---|---|
| `hermes.serverPath` | 空 | `hermes` 可执行文件路径。留空则依次查找扩展自带 `server/`、`PATH` |
| `hermes.trace.server` | `off` | 设为 `messages` 可把 LSP 收发消息打到输出面板（排查插件问题） |

## 找不到 `hermes` 时

扩展会弹一条**明确**的提示，而不是默默不工作 —— 「诊断、补全、悬停都不可用」这类状态必须让人知道，
否则用户会以为是自己写错了。

## 许可

扩展本身 MIT；它调用你本机的 `hermes`，不捆绑任何第三方运行时。
