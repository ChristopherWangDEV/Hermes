#!/usr/bin/env bash
# 编译并运行 T09 编译器前端 C++ 单测（不依赖 cgo，纯 clang++ 驱动）。
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

CLANG="${CLANG:-clang++}"

# 名字叫 clang 不等于实际是 clang（PATH 里的包装脚本可以 exec gcc）。
# 这里与 scripts/check_toolchain.sh 的 1c 段同一条判据：只认编译器自己说的那句话。
CLANG_FIRST_LINE="$("$CLANG" --version 2>/dev/null | head -1 || true)"
case "$CLANG_FIRST_LINE" in
  *clang*) ;;
  *)
    printf 'FATAL: CLANG=%s 的 --version 不是 clang：\n       %s\n' \
      "$CLANG" "${CLANG_FIRST_LINE:-<没有输出>}" >&2
    printf '红线要求只用 clang 编译；GNU 工具链产物会带上 GPL 义务。\n' >&2
    exit 64
    ;;
esac
CXXFLAGS=(-std=c++17 -O1
  -fno-exceptions -fno-rtti
  -fvisibility=hidden -fvisibility-inlines-hidden
  -fno-threadsafe-statics -fno-common -fno-ident
  -Wall -Wextra -Werror
  -ffile-prefix-map="$ROOT"=. -fdebug-prefix-map="$ROOT"=. -D_FORTIFY_SOURCE=2)
INC=(-I native/cpp -I native/include)

mkdir -p build/cpp
"$CLANG" "${CXXFLAGS[@]}" "${INC[@]}" \
  native/cpp/lexer.cpp native/cpp/parser.cpp native/cpp/diag.cpp native/cpp/ast_dump.cpp \
  native/cpp/check_type.cpp native/cpp/json_write.cpp native/cpp/codegen.cpp \
  native/cpp/api.cpp \
  tests/compiler/parser_test.cpp \
  -o build/cpp/parser_test

exec ./build/cpp/parser_test "$ROOT"
