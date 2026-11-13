#!/usr/bin/env bash
#
# check.sh —— 一条命令跑完全部检查。CI 与本机共用同一条路径。
#
# 为什么需要它：完整验证分散在三处（build.sh 的产物校验、go test 的单元与守卫、
# tests/compiler 的 C++ 前端验收）。只跑其中一部分，会得到"绿的假象"——
# 例如 build.sh 绿但 go test 红，或两者都绿而 C++ 前端其实没编过。
#
# 顺序刻意如此：先构建（产物不存在则后面的检查没有意义），再跑单测，
# 最后跑不依赖 cgo 的前端验收（它自己编 clang++，能独立于 Go 侧失败）。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# Go 不在默认 PATH 上的机器（本项目的既有约定：~/.local/go）
if ! command -v go >/dev/null 2>&1; then
  if [ -x "$HOME/.local/go/bin/go" ]; then
    PATH="$HOME/.local/go/bin:$PATH"
  else
    printf 'FATAL: go not found (also not in ~/.local/go/bin)\n' >&2
    exit 1
  fi
fi

say() { printf '\n==> %s\n' "$*"; }

# 链接器：Linux 上 go test 也会调 GNU ld（GPL）。门禁已断言 lld 存在，
# 这里把它真正用起来 —— 否则"构建用 lld、测试用 GNU ld"照样越线。
if [ "$(uname -s)" != "Darwin" ] && command -v ld.lld >/dev/null 2>&1; then
  export CGO_LDFLAGS="${CGO_LDFLAGS:-} -fuse-ld=lld"
fi

say "1/4 build (toolchain gate + native layer + C++ frontend + artifact hashes)"
bash "$ROOT/scripts/build.sh"

say "2/4 go test (units + guards, with -race)"
go test -race ./...

say "3/4 C++ frontend acceptance (T09)"
bash "$ROOT/tests/compiler/run.sh"

# SDK 测试：八个语言共用一套契约，缺工具链的语言会**明确列出"跳过"**，
# 而不是静默当成通过。
say "4/4 client SDKs (Python / JS / TS / Go / Rust / Java / C# / Kotlin)"
bash "$ROOT/scripts/test_sdks.sh"

say "i18n check (language pack matches source)"
"$ROOT/build/hermes" i18n check >/dev/null || die "language pack out of sync with source: run hermes i18n export"

say "all passed"
