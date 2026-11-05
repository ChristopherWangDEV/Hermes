#!/usr/bin/env bash
# 编 Hermes 的 Node 原生插件（N-API）。
#
# 不引 node-gyp / node-addon-api：它们要 npm 拉依赖，而这里只需要一条 clang 命令。
# 头文件由 Node 自带。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="$ROOT/build/hermes.node"
SRC="$ROOT/packages/javascript/native/hermes_native.c"

NODE_INC="$(node -p "require('path').join(process.execPath, '..', '..', 'include', 'node')" 2>/dev/null || true)"
if [ ! -f "$NODE_INC/node_api.h" ]; then
  NODE_INC="/opt/homebrew/include/node"
fi
if [ ! -f "$NODE_INC/node_api.h" ]; then
  echo "找不到 node_api.h；跳过 Node 原生插件（JS 的进程内绑定需要它）" >&2
  exit 3
fi

# 红线：只用 clang。`cc` 在 Linux 上通常是 gcc 的软链 —— 名字一样，工具链不一样。
CLANG="$(command -v clang || true)"
if [ -z "$CLANG" ]; then
  echo "找不到 clang（红线要求只用 clang 编译）" >&2
  exit 3
fi
if ! "$CLANG" --version 2>&1 | grep -q clang; then
  echo "$CLANG 的 --version 不是 clang，拒绝使用" >&2
  exit 3
fi

mkdir -p "$ROOT/build"
# N-API 的符号由 Node 进程在加载时提供，所以**不能**链接 libnode：
# 用 -undefined dynamic_lookup（macOS）/ -Wl,--allow-shlib-undefined（Linux）。
if [ "$(uname -s)" = "Darwin" ]; then
  "$CLANG" -O2 -shared -fPIC -I"$NODE_INC" -undefined dynamic_lookup -o "$OUT" "$SRC"
else
  "$CLANG" -O2 -shared -fPIC -I"$NODE_INC" -Wl,--allow-shlib-undefined -o "$OUT" "$SRC"
fi
echo "==> $OUT"
