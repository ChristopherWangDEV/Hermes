#!/usr/bin/env bash
#
# check_toolchain.sh —— 工具链门禁（R-LC-B1 / R-LC-B2 / R-LC-B4）
#
# 用法：
#   ./scripts/check_toolchain.sh          # 断言 + 打印版本
#   ./scripts/check_toolchain.sh --quiet  # 只断言，不打版本
#   ./scripts/check_toolchain.sh --json   # 输出 build/toolchain.json 的正文
#
# 退出码：0 全绿 / 64 工具链不合规（GPL 传染性许可证风险）。
#
# 为什么退出码是 64：这是本项目对"工具链不合规"约定的专用码，
# tests/guard/guards_test.go::TestBuildRejectsGxx 直接断言它。

set -euo pipefail

MODE="${1:-}"

# 找 lld：Debian/Ubuntu 的版本化包只提供 ld.lld-18 这类名字，
# 元包 lld 才建 /usr/bin/ld.lld。只认无后缀的名字会把"装了 lld"误判成"没装"。
hermes_find_lld() {
  for c in ld.lld lld; do
    if command -v "$c" >/dev/null 2>&1; then printf '%s' "$c"; return 0; fi
  done
  for c in ld.lld-21 ld.lld-20 ld.lld-19 ld.lld-18 ld.lld-17 ld.lld-16 lld-21 lld-20 lld-19 lld-18 lld-17; do
    if command -v "$c" >/dev/null 2>&1; then printf '%s' "$c"; return 0; fi
  done
  for p in /usr/lib/llvm-*/bin/ld.lld /usr/local/opt/llvm/bin/ld.lld /opt/homebrew/opt/llvm/bin/ld.lld; do
    if [ -x "$p" ]; then printf '%s' "$p"; return 0; fi
  done
  return 1
}


# ------------------------------------------------------------------ #
# 0. 让 go 能被找到（本机 Go 不在 PATH 上，这是环境事实，不是设计）
# ------------------------------------------------------------------ #
if ! command -v go >/dev/null 2>&1; then
  for cand in "${HOME}/.local/go/bin" "/usr/local/go/bin" "/opt/homebrew/opt/go/bin"; do
    if [ -x "${cand}/go" ]; then
      PATH="${cand}:${PATH}"
      export PATH
      break
    fi
  done
fi

# ------------------------------------------------------------------ #
# 1. 断言：CC / CXX / AS 必须以 clang 结尾
#
# 允许 clang 与 clang++ 两个 basename；其余（gcc / g++ / cc / GNU ld /
# binutils 系）一律拒绝。
# ------------------------------------------------------------------ #
gpl_fatal() {
  local var="$1" bin="$2"
  {
    printf 'FATAL: %s=%s is not clang.\n' "$var" "$bin"
    printf '\n'
    printf 'This project forbids the GCC / binutils toolchain on license grounds, not preference:\n'
    printf '  - gcc / g++ / GNU ld / binutils are GPL-licensed (copyleft);\n'
    printf '    static or dynamic linking against their output puts the whole deliverable under GPL obligations.\n'
    printf '  - The red line here is "free proprietary commercial use", which directly conflicts with GPL.\n'
    printf '  - nasm / yasm are license-compliant, but to keep a single toolchain,\n'
    printf '    all assembly goes through the clang integrated assembler (.s / .S).\n'
    printf '\n'
    printf 'Unset %s, or set it explicitly to clang / clang++.\n' "$var"
  } >&2
  exit 64
}

check_cc_var() {
  local var="$1"
  local raw="${!var:-}"
  local bin base

  if [ -z "$raw" ]; then
    # 未设置：用默认值，再确认默认值真的是 clang。
    case "$var" in
      AS)  bin="$(command -v clang || true)" ;;
      CXX) bin="$(command -v clang++ || true)" ;;
      *)   bin="$(command -v clang || true)" ;;
    esac
  else
    bin="$(command -v "${raw%% *}" 2>/dev/null || true)"
    if [ -z "$bin" ]; then
      # 变量指向一个不存在的程序：等价于不可用，按不合规处理。
      gpl_fatal "$var" "$raw"
    fi
  fi

  if [ -z "$bin" ]; then
    printf 'FATAL: no clang implementation found for %s; provide clang in PATH.\n' "$var" >&2
    exit 64
  fi

  base="$(basename "$bin")"
  case "$base" in
    clang|clang++|clang-[0-9]*|clang++-[0-9]*) : ;;
    *) gpl_fatal "$var" "$raw" ;;
  esac
}

# CXX=g++ 走的是同一条路径：command -v g++ 可能根本不存在，
# 两种情况都必须 exit 64 并提到 GPL。
check_cc_var CC
check_cc_var CXX
check_cc_var AS

# ------------------------------------------------------------------ #
# 2. 版本采集（doctor 的事实源，R-LC-B4）
# ------------------------------------------------------------------ #
CC_BIN="$(command -v "${CC:-clang}")"
CXX_BIN="$(command -v "${CXX:-clang++}")"
AS_BIN="$(command -v "${AS:-clang}")"

CLANG_VER="$("$CC_BIN" --version | head -1)"

# ------------------------------------------------------------------ #
# 1c. 断言：名字叫 clang，**实际也必须是 clang**
#
# 第 1 段只看 basename，而 basename 可以被伪造：PATH 里放一个名为 clang 的
# shell 包装脚本，里面 exec gcc，就能一路通过。这不是假想——交叉编译工具链、
# "统一入口"包装、CI 镜像里的 shim 都是这个形状。
#
# 判据只能是编译器自己说的话：`--version` 的第一行必须包含 "clang"。
# 不去解析版本号（格式会变），只认这一个词 —— 因为 GNU 的驱动会打印
# "gcc (GCC) x.y.z"，那是另一个词。
# ------------------------------------------------------------------ #
assert_actual_clang() {
  local var="$1" bin="$2" ver
  ver="$("$bin" --version 2>/dev/null | head -1 || true)"
  case "$ver" in
    *clang*) return 0 ;;
  esac
  {
    printf 'FATAL: %s=%s: basename is clang, but `--version` is not clang:\n' "$var" "$bin"
    printf '       %s\n' "${ver:-<no output>}"
    printf '\n'
    printf 'This usually means a clang wrapper script in PATH is actually invoking gcc/g++.\n'
    printf 'The red line requires the entire build to use clang and LLVM tools only; if any step falls back to the GNU toolchain,\n'
    printf 'the artifacts may carry GPL obligations, and such mixing is invisible in the build log\n'
    printf '(errors only say "clang: error: ...", because the driver is named clang).\n'
    printf '\n'
    printf 'Fix: point %s to a real clang (command -v clang should return a non-script file).\n' "$var"
  } >&2
  exit 64
}

assert_actual_clang CC "$CC_BIN"
assert_actual_clang CXX "$CXX_BIN"
assert_actual_clang AS "$AS_BIN"
GO_VER="$(go version 2>/dev/null | head -1 || echo 'go: missing')"
RUSTC_VER="$(rustc --version 2>/dev/null | head -1 || echo 'rustc: missing')"
CARGO_VER="$(cargo --version 2>/dev/null | head -1 || echo 'cargo: missing')"
AR_VER="$(ar --version 2>/dev/null | head -1 || echo 'ar: BSD (cctools / llvm-ar)')"

# ar 的许可证**必须探测，不能硬编码**。
#
# macOS 的 ar 是 Apple cctools / llvm-ar（APSL / Apache-2.0 WITH LLVM-exception），
# 而 Linux 上 ar 来自 binutils，是 **GPL-3.0-or-later**。过去这里把 ar 的许可
# 写死成 llvm-exception —— 那在 Linux 上是一句谎话，而"硬编码的许可证字段必然
# 说谎"这个教训本项目已经吃过一次（cpp_stdlib_license 就是这么被删掉的）。
#
# 但要说清它**不传染**：ar 只把我们的 .o 拼成 .a，不链接任何代码、也不随产物分发，
# 所以使用它不构成许可义务（§1.4「不分发即不触发」）。这里记录的是**事实**，
# 不是把它当成红线违规 —— 记录准确，下游才能自己判断。
AR_BIN="$(command -v ar || echo '')"
AR_LICENSE="Apache-2.0 WITH LLVM-exception"
GNU_BINUTILS=false
case "$AR_VER" in
  *"GNU ar"*)
    AR_LICENSE="GPL-3.0-or-later"
    GNU_BINUTILS=true
    ;;
esac
# ------------------------------------------------------------------ #
# 1c. 断言：链接器不能是 GNU ld（GPL）
#
# 第 1 段只查编译器、1b 只查 C++ 标准库 —— 而**链接器**是第三层：
# Linux 上 clang 默认调用 `/usr/bin/ld`（GNU binutils，GPL-3.0）。
# 编译器合规 + 标准库合规 + 链接器不合规，是个很容易漏的组合。
#
# 探测方式同样是**让工具自己回答**：`-fuse-ld=lld -Wl,--version` 会打印
# LLD 自己的版本行。拿不到 lld 就让门禁失败，而不是退回 GNU ld。
LD_LICENSE="(n/a: Darwin ld64)"
if [ "$(uname -s)" != "Darwin" ]; then
  if hermes_find_lld >/dev/null 2>&1; then
    if "$CC_BIN" -fuse-ld=lld -Wl,--version -x c /dev/null -o /dev/null 2>&1 | grep -qi "LLD"; then
      LD_LICENSE="Apache-2.0 WITH LLVM-exception (lld)"
    else
      printf 'FATAL: linker is not lld.\n\n' >&2
      printf '  On Linux clang defaults to GNU ld (GPL-3.0), which the red line forbids.\n' >&2
      printf '  Install lld (package lld-18 on Ubuntu) and ensure build.sh finds it via -fuse-ld=lld.\n' >&2
      exit 64
    fi
  else
    printf 'FATAL: ld.lld not found (on Linux the default linker is GNU ld, GPL).\n' >&2
    printf '  Install lld and retry.\n' >&2
    exit 64
  fi
fi

HOST_TRIPLE="$("$CC_BIN" -print-target-triple 2>/dev/null || echo unknown)"
UNAME_S="$(uname -s)"
UNAME_M="$(uname -m)"

# ------------------------------------------------------------------ #
# 1b. 断言：C++ 标准库必须是 libc++，不能是 libstdc++
#
# 为什么第 1 段不够：clang 只是编译器，它默认链接的 C++ 标准库是
# 平台相关的 —— macOS 上是 libc++（Apache-2.0 WITH LLVM-exception，
# 在白名单内），**但 Linux 上 clang++ 默认链 libstdc++**，而 libstdc++
# 是 GPL-3.0 WITH GCC Runtime Library Exception。
#
# 于是会出现一种很难看的状态：编译器合规、标准库不合规。第 1 段只看
# basename，抓不到这一层。这里用 probe 直接问"你现在到底在用哪个库"：
# 探测方式是让**编译器自己的退出码**回答，不解析任何输出文本。
# __GLIBCXX__ 由 libstdc++ 的任一头文件定义，_LIBCPP_VERSION 由 libc++
# 的 <__config> 定义 —— 两者互斥，不需要猜路径。
# ------------------------------------------------------------------ #
stdlib_fatal() {
  {
    printf 'FATAL: CXX (%s) is using a C++ standard library other than libc++.\n' "$CXX_BIN"
    printf '\n'
    printf 'libstdc++ is licensed GPL-3.0 WITH GCC Runtime Library Exception.\n'
    printf 'The Runtime Exception is widely read to allow proprietary distribution of the target program, but this red line\n'
    printf 'is strict by the letter: where there is a choice, pick the one that adds no obligation. On Linux clang++ defaults to it,\n'
    printf 'so this gate must block it explicitly rather than assume "clang is compliant anyway".\n'
    printf '\n'
    printf 'Fix: install the libc++ dev packages (Debian/Ubuntu: libc++-dev libc++abi-dev;\n'
    printf '      RHEL/Fedora: libcxx-devel), or set CXX="clang++ -stdlib=libc++" explicitly.\n'
  } >&2
  exit 64
}

PROBE_DIR="$(mktemp -d 2>/dev/null || echo /tmp/hermes-stdlib-probe-$$)"
mkdir -p "$PROBE_DIR" 2>/dev/null || true
PROBE_SRC="$PROBE_DIR/stdlib_probe.cpp"
cat >"$PROBE_SRC" <<'PROBE_EOF'
// 必须 include 一个头文件，否则标识宏根本没有定义的机会：
// _LIBCPP_VERSION 由 libc++ 的 <__config> 定义、__GLIBCXX__ 由 libstdc++ 的
// <bits/c++config.h> 定义，两者都只在**头文件被读入后**才存在。
// 第一版 probe 漏了这一行，于是在 macOS 上也报 "not libc++" ——
// 一个只会误报的门禁比没有门禁更糟（它会第一次就被人删掉），
// 所以这个坑写在这里：改 probe 时不要把 include 当成可选的开场白。
#include <cstddef>
#if defined(__GLIBCXX__) || defined(__GLIBCPP__)
#error "libstdc++ in use (GPL-3.0 WITH GCC Runtime Library Exception)"
#endif
#if !defined(_LIBCPP_VERSION)
#error "not libc++: C++ standard library is neither libc++ nor identifiable"
#endif
int main() { return 0; }
PROBE_EOF

if ! "$CXX_BIN" -std=c++17 -stdlib=libc++ -fsyntax-only "$PROBE_SRC" >/dev/null 2>&1; then
  rm -rf "$PROBE_DIR" 2>/dev/null || true
  # ---- 受控例外通道（取舍原则：默认最严 / 例外显式 opt-in / 例外留记录）----
  # 有些环境（尤其 Linux 最小安装）确实没有 libc++ 开发包，而用户愿意自己
  # 承担 libstdc++ 的许可判断。与其让他们去改脚本（那等于偷偷绕过、不留记录），
  # 不如给一个显式开关：开了就必须在 toolchain.json 里如实写出，
  # 下游 doctor / 发布门禁据此拒绝。
  if [ "${HERMES_ALLOW_LIBSTDCXX:-0}" = "1" ]; then
    CPP_STDLIB="libstdc++"
    CPP_STDLIB_EXCEPTION="HERMES_ALLOW_LIBSTDCXX=1"
    GPL_DETECTED=true
    printf 'WARN: libstdc++ allowed via HERMES_ALLOW_LIBSTDCXX=1 (GPL-3.0 WITH GCC Runtime Library Exception).\n' >&2
    printf '      This build records gpl_tools_detected=true in toolchain.json; the release gate should reject it.\n' >&2
  else
    stdlib_fatal
  fi
else
  CPP_STDLIB="libc++"
  CPP_STDLIB_EXCEPTION=""
  GPL_DETECTED=false
fi
rm -rf "$PROBE_DIR" 2>/dev/null || true

if [ "$MODE" = "--quiet" ]; then
  exit 0
fi

if [ "$MODE" = "--json" ]; then
  cat <<JSON
{
  "cc": {"path": "${CC_BIN}", "version": "${CLANG_VER}", "license": "Apache-2.0 WITH LLVM-exception"},
  "cxx": {"path": "${CXX_BIN}", "version": "${CLANG_VER}", "license": "Apache-2.0 WITH LLVM-exception"},
  "as": {"path": "${AS_BIN}", "version": "${CLANG_VER}", "license": "Apache-2.0 WITH LLVM-exception", "note": "clang integrated assembler, no nasm/yasm"},
  "ar": {"path": "${AR_BIN}", "version": "${AR_VER}", "license": "${AR_LICENSE}"},
  "go": {"version": "${GO_VER}", "license": "BSD-3-Clause"},
  "rustc": {"version": "${RUSTC_VER}", "license": "MIT OR Apache-2.0"},
  "cargo": {"version": "${CARGO_VER}", "license": "MIT OR Apache-2.0"},
  "target_triple": "${HOST_TRIPLE}",
  "os": "${UNAME_S}",
  "arch": "${UNAME_M}",
  "cpp_stdlib": "${CPP_STDLIB}",
  "cpp_stdlib_exception": "${CPP_STDLIB_EXCEPTION}",
  "gpl_tools_detected": ${GPL_DETECTED},
  "gnu_binutils_detected": ${GNU_BINUTILS}
}
JSON
  exit 0
fi

cat <<EOF
toolchain: ok (compiler and C++ standard library compliant; ar license on the line below)
  CC    = ${CC_BIN}
  CXX   = ${CXX_BIN}
  AS    = ${AS_BIN}
  clang = ${CLANG_VER}
  go    = ${GO_VER}
  rustc = ${RUSTC_VER}
  cargo = ${CARGO_VER}
  ar    = ${AR_BIN}  [${AR_LICENSE}]
  target= ${HOST_TRIPLE} (${UNAME_S}/${UNAME_M})
EOF

if [ "$GNU_BINUTILS" = true ]; then
  printf '\nWARN: ar comes from GNU binutils (GPL-3.0-or-later).\n' >&2
  printf '      This does not affect the artifact license: ar only bundles our .o files, links no code, and is not shipped with the artifacts,\n' >&2
  printf '      so it creates no license obligation (§1.4 "no distribution, no trigger").\n' >&2
  printf '      But the record must be accurate: written to ar.license and gnu_binutils_detected in toolchain.json.\n' >&2
  printf '      To avoid GPL even in dev tools: install llvm-ar (Apache-2.0 WITH LLVM-exception) and use it instead of ar.\n' >&2
fi
