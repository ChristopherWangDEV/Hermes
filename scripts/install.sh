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
  echo "Usage: install.sh <hermes-*.tar.gz>" >&2
  echo "      (or invoked by the release script with a URL)" >&2
  exit 2
fi

if [ ! -f "$SRC" ]; then
  echo "package not found: $SRC" >&2
  exit 4
fi

say "unpacking $SRC to $PREFIX"
mkdir -p "$PREFIX"
tar -xzf "$SRC" -C "$PREFIX" --strip-components=1

# 校验：包里自带 SHA256SUMS，装完立刻验一遍 —— 这是唯一能证明"没被改过"的本地手段。
if [ -f "$PREFIX/SHA256SUMS" ]; then
  say "verifying file integrity"
  ( cd "$PREFIX" && shasum -a 256 -c SHA256SUMS --quiet ) || {
    echo "verification failed: package may be tampered" >&2
    exit 4
  }
fi

BIN="$PREFIX/bin/hermes"
if [ ! -x "$BIN" ]; then
  echo "package has no bin/hermes: invalid layout" >&2
  exit 4
fi

say "self-check"
HERMES_HOME="$PREFIX" "$BIN" doctor || {
  echo "doctor did not pass; the FAIL lines above show what is missing" >&2
  exit 4
}

cat <<MSG

Installed. Add the following line to your shell config (we do NOT edit it for you):

  export PATH="$PREFIX/bin:\$PATH"

Then:

  hermes --version
  hermes hello.hermes -o hello && ./hello

AOT compilation needs a C compiler (clang). Without it, all commands except \`hermes compile\` / \`hermes cc\`
remain available; \`hermes doctor\` tells you exactly what is missing.
MSG
