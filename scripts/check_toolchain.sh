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
    printf 'FATAL: %s=%s 不是 clang。\n' "$var" "$bin"
    printf '\n'
    printf '本项目禁止 GCC / binutils 工具链，理由是许可证而不是偏好：\n'
    printf '  · gcc / g++ / GNU ld / binutils 的许可证是 GPL（传染性），\n'
    printf '    静态或动态链接它们产出的二进制会让整个交付物落入 GPL 义务。\n'
    printf '  · 本项目的红线是"允许免费闭源商用"，与 GPL 直接冲突。\n'
    printf '  · nasm / yasm 虽然许可证合规，但为了让工具链只有一条，\n'
    printf '    汇编一律走 clang 集成汇编器（.s / .S）。\n'
    printf '\n'
    printf '请 unset %s，或显式设为 clang / clang++。\n' "$var"
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
    printf 'FATAL: 找不到 %s 的 clang 实现，请在 PATH 中提供 clang。\n' "$var" >&2
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
    printf 'FATAL: %s=%s 的 basename 是 clang，但 `--version` 不是 clang：\n' "$var" "$bin"
    printf '       %s\n' "${ver:-<没有输出>}"
    printf '\n'
    printf '这通常意味着 PATH 里有一个 clang 包装脚本，实际调用的是 gcc/g++。\n'
    printf '红线要求整个构建只用 clang 与 LLVM 系工具；一旦某个环节落到 GNU 工具链\n'
    printf '上，产物就可能带上 GPL 义务，而且这种混用**在编译日志里完全看不出来**\n'
    printf '（错误信息只会说 "clang: error: ..."，因为驱动名就是 clang）。\n'
    printf '\n'
    printf '修复：把 %s 指向真正的 clang（command -v clang 应给出非脚本文件）。\n' "$var"
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
  if command -v ld.lld >/dev/null 2>&1; then
    if "$CC_BIN" -fuse-ld=lld -Wl,--version -x c /dev/null -o /dev/null 2>&1 | grep -qi "LLD"; then
      LD_LICENSE="Apache-2.0 WITH LLVM-exception (lld)"
    else
      printf 'FATAL: 链接器不是 lld。\n\n' >&2
      printf '  Linux 上 clang 默认调用 GNU ld（GPL-3.0），本项目红线不允许。\n' >&2
      printf '  安装 lld（Ubuntu 上是 lld-18 这个包），并确保 build.sh 能通过 -fuse-ld=lld 找到它。\n' >&2
      exit 64
    fi
  else
    printf 'FATAL: 找不到 ld.lld（Linux 上默认链接器是 GNU ld，GPL）。\n' >&2
    printf '  安装 lld 后重试。\n' >&2
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
    printf 'FATAL: CXX (%s) 正在使用非 libc++ 的 C++ 标准库。\n' "$CXX_BIN"
    printf '\n'
    printf 'libstdc++ 的许可证是 GPL-3.0 WITH GCC Runtime Library Exception。\n'
    printf 'Runtime Exception 在主流解读下允许闭源分发目标程序，但本项目红线\n'
    printf '按字面从严：凡可选，选不引入的那个。Linux 上 clang++ 默认就是它，\n'
    printf '所以这条必须在门禁里显式拦，不能靠"反正 clang 是合规的"推过去。\n'
    printf '\n'
    printf '修复：安装 libc++ 开发包（Debian/Ubuntu: libc++-dev libc++abi-dev；\n'
    printf '      RHEL/Fedora: libcxx-devel），或显式 CXX="clang++ -stdlib=libc++"。\n'
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
    printf 'WARN: 已按 HERMES_ALLOW_LIBSTDCXX=1 允许 libstdc++（GPL-3.0 WITH GCC Runtime Library Exception）。\n' >&2
    printf '      本次构建会在 toolchain.json 里如实记录 gpl_tools_detected=true，发布门禁应据此拒绝。\n' >&2
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
  "as": {"path": "${AS_BIN}", "version": "${CLANG_VER}", "license": "Apache-2.0 WITH LLVM-exception", "note": "clang 集成汇编器，不使用 nasm/yasm"},
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
toolchain: ok（编译器与 C++ 标准库合规；ar 的许可证见下方一行）
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
  printf '\nWARN: ar 来自 GNU binutils（GPL-3.0-or-later）。\n' >&2
  printf '      这不影响产物许可：ar 只拼接我们的 .o，不链接任何代码，也不随产物分发，\n' >&2
  printf '      因此不构成许可义务（§1.4「不分发即不触发」）。\n' >&2
  printf '      但记录必须准确 —— 已写进 toolchain.json 的 ar.license 与 gnu_binutils_detected。\n' >&2
  printf '      若想连开发工具也避开 GPL：安装 llvm-ar（Apache-2.0 WITH LLVM-exception）并用它替换 ar。\n' >&2
fi
