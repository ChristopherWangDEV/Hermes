#!/usr/bin/env bash
# 安装 Hermes：**下载即用**，不装 Go / Rust / cargo。
#
# 用法（用户视角）：
#   curl -fsSL https://example.invalid/install.sh | sh
# 或本地包：
#   ./scripts/install.sh ./build/release/hermes-*.tar.gz
#
# 它做的事只有三件：解包到 $HERMES_PREFIX、建一个 bin 软链、跑一次 doctor。
# **不改 PATH 文件**：改 shell 配置是"替用户做决定"，而我们不知道他用什么 shell。
set -euo pipefail

VERSION_DEFAULT="latest"
PREFIX="${HERMES_PREFIX:-$HOME/.local/hermes}"
SRC="${1:-}"

say() { printf '==> %s\n' "$*"; }

if [ -z "$SRC" ]; then
  echo "用法: install.sh <hermes-*.tar.gz>" >&2
  echo "      （或由发布页脚本带 URL 调用）" >&2
  exit 2
fi

if [ ! -f "$SRC" ]; then
  echo "找不到安装包: $SRC" >&2
  exit 4
fi

say "解包 $SRC 到 $PREFIX"
mkdir -p "$PREFIX"
tar -xzf "$SRC" -C "$PREFIX" --strip-components=1

# 校验：包里自带 SHA256SUMS，装完立刻验一遍 —— 这是唯一能证明"没被改过"的本地手段。
if [ -f "$PREFIX/SHA256SUMS" ]; then
  say "校验文件完整性"
  ( cd "$PREFIX" && shasum -a 256 -c SHA256SUMS --quiet ) || {
    echo "校验失败：安装包可能被篡改" >&2
    exit 4
  }
fi

BIN="$PREFIX/bin/hermes"
if [ ! -x "$BIN" ]; then
  echo "包里没有 bin/hermes，布局不对" >&2
  exit 4
fi

say "自检"
HERMES_HOME="$PREFIX" "$BIN" doctor || {
  echo "doctor 未通过；上面的 FAIL 行说明了缺什么" >&2
  exit 4
}

cat <<MSG

装好了。把下面这行加进你的 shell 配置（我们**不替你改**）：

  export PATH="$PREFIX/bin:\$PATH"

然后：

  hermes --version
  hermes hello.hermes -o hello && ./hello

AOT 编译需要一个 C 编译器（clang）。没有它时，除 \`hermes compile\` / \`hermes cc\`
以外的命令仍然可用 —— \`hermes doctor\` 会明确告诉你缺什么。
MSG
