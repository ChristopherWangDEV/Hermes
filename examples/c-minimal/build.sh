#!/usr/bin/env bash
# 三步：编 unit → 生成 C 头 → 用 hermes cc 编译并链接。
#
# 为什么用 `hermes cc` 而不是手写 clang：它知道要链接哪些东西
# （运行时静态库、沙箱 shim、平台框架），并在链接后自检符号表。
# 手写 clang 会漏掉 shim，症状是链接期报 "symbol not found"。
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$HERE"

HERMES="${HERMES:-hermes}"
command -v "$HERMES" >/dev/null 2>&1 || { echo "找不到 hermes（设 HERMES=/path/to/hermes）" >&2; exit 1; }

echo "==> 1/3 编译 unit"
"$HERMES" build units/label.hermes -o label.hbc

echo "==> 2/3 生成 C 头"
"$HERMES" header label.hbc --c -o label.h

echo "==> 3/3 编译并链接 C 程序"
"$HERMES" cc main.c --unit label.hbc -o app

echo
echo "构建完成："
echo "  ./app          # 运行"
echo "  输出应当是一行：printer is broken #3"
