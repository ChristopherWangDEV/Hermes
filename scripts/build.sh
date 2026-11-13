#!/usr/bin/env bash
#
# build.sh —— Hermes 的唯一构建入口。
#
# 顺序（ARCH §8.1）：
#   ① 工具链断言（最先执行，早于任何编译）
#   ② 版本采集 → build/toolchain.json
#   ③ clang 编译 C 与汇编（可重现标志）
#   ④ cargo 构建 Rust staticlib（--locked --offline）
#   ⑤ 归档成单个 build/libhermes_native.a
#   ⑥ go build（cgo，静态链接上面的 .a）
#   ⑦ 构建 hermes-shim（独立小二进制）
#   ⑧ 产物校验：sha256 + 符号双向 diff + shim 自检 + 可重现性
#
# 硬约束：
#   · 只用 clang / clang++（clang 集成汇编器处理 .S），不使用 gcc/g++/ld/nasm。
#   · CXX=g++ ./scripts/build.sh 必须退出 64 且 stderr 含 "GPL"。
#   · 两次构建产物 sha256 必须相等。

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# 动态库扩展名按平台定：macOS 是 .dylib，其余（Linux 与各 BSD）是 .so。
#
# BSD 与 Linux 共用同一条构建路径：clang + libc++ + lld 在 FreeBSD / OpenBSD /
# NetBSD / DragonFly 上都可用。**尚未在真机验证** —— 这条分支存在是为了让
# "支持 BSD" 在脚本里成为可执行的路径，而不是一句宣传；真机验证排在后续版本。
case "$(uname -s)" in
  Darwin)  SHLIB_EXT=".dylib" ;;
  FreeBSD|OpenBSD|NetBSD|DragonFly) SHLIB_EXT=".so" ;;
  *)       SHLIB_EXT=".so" ;;
esac
cd "$ROOT"

# 确定性：不让 locale / 时区 / 时间戳渗进任何产物。
export LC_ALL=C
export LANG=C
export TZ=UTC
export SOURCE_DATE_EPOCH=0
export CGO_ENABLED=1

BUILD="$ROOT/build"
mkdir -p "$BUILD"

say() { printf '==> %s\n' "$*"; }
die() { printf 'FATAL: %s\n' "$*" >&2; exit 1; }

# 找 lld：Debian/Ubuntu 的版本化包只提供 ld.lld-18 这类名字，
# 元包 lld 才建 /usr/bin/ld.lld。只认无后缀的名字会把"装了 lld"误判成"没装"，
# 于是门禁在 CI 上直接 die —— 这是名字问题，不是缺组件。
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
# ① 工具链断言（R-LC-B1 / R-LC-B2）—— 必须最先执行
# ------------------------------------------------------------------ #
say "step 1/8 toolchain gate"
# 这一行是三重保障的第一重：check_toolchain.sh 内部对 CC/CXX/AS 逐个断言，
# 不合规直接 exit 64（专用退出码，被 guards_test 断言）。
"$ROOT/scripts/check_toolchain.sh" --quiet

CC_BIN="$(command -v "${CC:-clang}")"
CXX_BIN="$(command -v "${CXX:-clang++}")"
case "$(basename "$CC_BIN")" in
  clang|clang++|clang-[0-9]*) : ;;
  *) printf 'FATAL: CC is not clang (GPL toolchain forbidden)\n' >&2; exit 64 ;;
esac
case "$(basename "$CXX_BIN")" in
  clang|clang++|clang++-[0-9]*) : ;;
  *) printf 'FATAL: CXX is not clang++ (GPL toolchain forbidden)\n' >&2; exit 64 ;;
esac

# ------------------------------------------------------------------ #
# ② 版本采集 → build/toolchain.json（doctor 的事实源）
# ------------------------------------------------------------------ #
say "step 2/8 collect toolchain versions"
"$ROOT/scripts/check_toolchain.sh" --json > "$BUILD/toolchain.json"

HOST_ARCH="$(uname -m)"
HOST_OS="$(uname -s)"
LD_FLAGS=()   # Darwin 用 clang 自带的 ld64，无需指定
if [ "$HOST_OS" = "Darwin" ]; then
  case "$HOST_ARCH" in
    arm64)  ARCH_AS_FLAGS=(-arch arm64);  FOREIGN_ARCH=x86_64;  FOREIGN_FLAGS=(-arch x86_64) ;;
    x86_64) ARCH_AS_FLAGS=(-arch x86_64); FOREIGN_ARCH=arm64;   FOREIGN_FLAGS=(-arch arm64) ;;
    *) die "unsupported host arch: $HOST_ARCH" ;;
  esac
  AS_MIN_FLAGS=(-mmacosx-version-min=13.0)
else
  # Linux：`-arch` 与 `-mmacosx-version-min` 都是 Darwin 专用标志。
  # 另一架构用 --target 做交叉汇编检查（clang 自带汇编器，不需要交叉工具链）。
  case "$HOST_ARCH" in
    x86_64) ARCH_AS_FLAGS=(); FOREIGN_ARCH=arm64;  FOREIGN_FLAGS=(--target=aarch64-linux-gnu) ;;
    aarch64|arm64) ARCH_AS_FLAGS=(); FOREIGN_ARCH=x86_64; FOREIGN_FLAGS=(--target=x86_64-linux-gnu) ;;
    *) die "unsupported host arch: $HOST_ARCH" ;;
  esac
  AS_MIN_FLAGS=()
  # 链接器：Linux 上 clang 默认调 GNU ld（GPL）。红线要求换成 lld
  # （Apache-2.0 WITH LLVM-exception）—— 这跟"编译器用 clang"是同一条规矩，
  # 只是链接器容易被忘掉。
  if hermes_find_lld >/dev/null; then
    LD_FLAGS=(-fuse-ld=lld)
  else
    die "lld not found (on Linux clang defaults to GNU ld, GPL, forbidden by the red line); install lld and retry."
  fi
fi

# 可重现标志。$ROOT 被映射成 "."，绝对路径因此不会进 .o / .a / 二进制。
CFLAGS=(
  -O2 -std=c11
  -fvisibility=hidden -fno-common
  -Wall -Wextra -Werror
  -ffile-prefix-map="$ROOT"=.
  -fdebug-prefix-map="$ROOT"=.
  -fno-ident
  -D_FORTIFY_SOURCE=2
  -DNDEBUG
  # 内存安全加固：栈保护（canary）+ 只在需要时执行栈。
  # 这不是"性能换安全"的取舍 —— 这两个开关的开销在解析器这种
  # 短函数密集的代码里可以忽略，而溢出/ROP 的门槛完全不同。
  -fstack-protector-strong
  -I"$ROOT/native/include"
)
ASFLAGS=("${ARCH_AS_FLAGS[@]}" "${AS_MIN_FLAGS[@]}")

# ------------------------------------------------------------------ #
# ③ clang 编译 C 与汇编
# ------------------------------------------------------------------ #
say "step 3/8 compile C and assembly (clang integrated assembler)"
rm -f "$BUILD"/*.o

C_SOURCES=(
  native/c/abi_version.c
  native/c/cycle.c
  native/c/errno_map.c
  native/c/sandbox.c
  native/c/shim.c
  native/c/exec.c
  native/c/supervisor.c
)
for src in "${C_SOURCES[@]}"; do
  [ -f "$src" ] || die "missing source: $src"
  "$CC_BIN" "${CFLAGS[@]}" -c "$src" -o "$BUILD/$(basename "${src%.c}").o"
done

# 宿主机架构的 .S 真正参与链接
for src in native/c/cycle_arm64.S native/c/cycle_x86_64.S; do
  base="$(basename "${src%.S}")"
  if [ "$HOST_ARCH" = "arm64" ] && [ "$base" = "cycle_arm64" ]; then
    "$CC_BIN" "${ASFLAGS[@]}" -c "$src" -o "$BUILD/$base.o"
  elif [ "$HOST_ARCH" = "x86_64" ] && [ "$base" = "cycle_x86_64" ]; then
    "$CC_BIN" "${ASFLAGS[@]}" -c "$src" -o "$BUILD/$base.o"
  else
    # R-NF-03：另一条汇编分支也必须被 clang 汇编器接受（不要求能跑）。
    # 交叉汇编可行（clang 是多目标汇编器），因此这里是真的汇编而非仅语法检查。
    "$CC_BIN" "${FOREIGN_FLAGS[@]}" -mmacosx-version-min=13.0 -c "$src" -o "$BUILD/$base.foreign.o"
    rm -f "$BUILD/$base.foreign.o"
  fi
done

# R-NF-03 的另一半：cycle.c 的 #if 分支必须两侧都能通过编译。
# 直接用 -D__x86_64__ 会污染系统头，因此用显式的 HM_FORCE_ARCH_* 开关
# （cycle.c 里两者等价，见该文件的注释）。
"$CC_BIN" "${CFLAGS[@]}" -fsyntax-only -DHM_FORCE_ARCH_X86_64 native/c/cycle.c
"$CC_BIN" "${CFLAGS[@]}" -fsyntax-only -DHM_FORCE_ARCH_ARM64  native/c/cycle.c

# ------------------------------------------------------------------ #
# ④ Rust staticlib
# ------------------------------------------------------------------ #
say "step 4/8 cargo build (staticlib, offline, locked)"
export RUSTFLAGS="--remap-path-prefix=$ROOT=."
cargo build \
  --manifest-path "$ROOT/native/rust/Cargo.toml" \
  --release --locked --offline \
  --target-dir "$BUILD/cargo"

RUST_A="$BUILD/cargo/release/libhermes_core.a"
[ -f "$RUST_A" ] || die "rust staticlib not produced: $RUST_A"

# ------------------------------------------------------------------ #
# ④b T09：C++ 编译器前端 → build/libhermes_parser.a
#
# 独立静态库（不并入 libhermes_native.a）。只依赖 clang++，不依赖 cargo / go / cgo。
# 严格标志：无异常 / 无 RTTI / 无 IO / 无全局可变状态（ARCH §5.9）。
# 与 ⑤ 同样做可重现归档（钉 mtime，arfix 消除 ar/ranlib 的非确定字节）。
# ------------------------------------------------------------------ #
say "step 4b/8 compile C++ parser frontend (libhermes_parser.a)"
PARSER_CXXFLAGS=(
  -O2 -std=c++17
  # -stdlib=libc++ 是**许可证要求不是口味**（见 check_toolchain.sh 第 1b 段）：
  # macOS 上 clang++ 默认就是 libc++，写不写都一样；Linux 上默认是 libstdc++
  # （GPL-3.0 WITH Runtime Exception），不显式指定就会被静默选中。
  # 这里显式写出，是为了让"三个平台编出来的东西依赖同一个库"成为脚本里的事实，
  # 而不是依赖各发行版的默认值。门禁已先行断言它能用，此处不需要兜底。
  -stdlib=libc++
  -fno-exceptions -fno-rtti
  -fvisibility=hidden -fvisibility-inlines-hidden
  -fno-threadsafe-statics -fno-common -fno-ident
  -Wall -Wextra -Werror
  -ffile-prefix-map="$ROOT"=.
  -fdebug-prefix-map="$ROOT"=.
  -I"$ROOT/native/cpp" -I"$ROOT/native/include"
)
PARSER_SRCS=(native/cpp/lexer.cpp native/cpp/parser.cpp native/cpp/diag.cpp \
  native/cpp/ast_dump.cpp native/cpp/check_type.cpp native/cpp/check_idem.cpp \
  native/cpp/json_write.cpp native/cpp/codegen.cpp native/cpp/api.cpp)
rm -f "$BUILD"/*.parser.o
for src in "${PARSER_SRCS[@]}"; do
  [ -f "$src" ] || die "missing parser source: $src"
  obj="$BUILD/$(basename "${src%.cpp}").parser.o"
  "$CXX_BIN" "${PARSER_CXXFLAGS[@]}" -c "$src" -o "$obj"
done
rm -f "$BUILD/libhermes_parser.a"
ar rc "$BUILD/libhermes_parser.a" \
  "$BUILD/lexer.parser.o" "$BUILD/parser.parser.o" "$BUILD/diag.parser.o" \
  "$BUILD/ast_dump.parser.o" "$BUILD/check_type.parser.o" "$BUILD/check_idem.parser.o" \
  "$BUILD/json_write.parser.o" "$BUILD/codegen.parser.o" "$BUILD/api.parser.o"
ranlib "$BUILD/libhermes_parser.a"
go run ./tools/arfix "$BUILD/libhermes_parser.a" >/dev/null

# 机器守卫 ①：前端严禁引入异常 / RTTI 符号（违反 -fno-exceptions -fno-rtti）。
# 仅当符号被**定义**（非 U / 非弱未定义 w）时才判定失败。
say "step 4b/8 guard: no exception/RTTI symbols"
# 注意 `|| true`：守卫的**通过**形态是"一条都不匹配"，而 grep 无匹配时退出 1；
# 在 `set -o pipefail` 下这会让命令替换赋值失败、脚本静默退出 1 —— 守卫自己成了故障源。
PARSER_EXC="$(nm "$BUILD/libhermes_parser.a" 2>/dev/null \
  | grep -iE '__cxa_throw|__gxx_personality_v0|_ZTI|typeinfo' \
  | awk '$2!="U" && $2!="w"{print}' || true)"
if [ -n "$PARSER_EXC" ]; then
  printf '%s\n' "$PARSER_EXC" >&2
  die "libhermes_parser.a contains exception/RTTI symbols (violates -fno-exceptions -fno-rtti)"
fi

# 机器守卫 ②：公开 ABI 符号面必须双向等于 scripts/expected_symbols_parser.txt。
say "step 4b/8 verify parser symbol surface"
nm "$BUILD/libhermes_parser.a" 2>/dev/null \
  | awk '$2=="T"{print $3}' | sed 's/^_//' \
  | grep -E '^hm_(parse|codegen)' | sort -u > "$BUILD/parser_symbols.txt" || true
grep -vE '^\s*(#|$)' "$ROOT/scripts/expected_symbols_parser.txt" | sort -u \
  > "$BUILD/parser_symbols.expected.txt"
if ! diff -u "$BUILD/parser_symbols.expected.txt" "$BUILD/parser_symbols.txt"; then
  die "parser symbol surface drifted from scripts/expected_symbols_parser.txt"
fi

# ------------------------------------------------------------------ #
# ⑤ 归档：Rust 在前，C 在后（C 依赖 Rust 符号）
#
say "step 5/8 archive libhermes_native.a"
rm -rf "$BUILD/rust-obj"
mkdir -p "$BUILD/rust-obj"
( cd "$BUILD/rust-obj" && ar x "$RUST_A" )

# 不使用 GNU ld / libtool：ar 是唯一需要的归档工具。
#
# 可重现性：Apple 的 ar 没有 GNU 的 -D（deterministic）开关，而 ar 会把成员的
# mtime 写进归档头。因此先把所有输入的时间戳钉到 epoch 0 再归档 —— 否则两次
# 构建的 .a 必然不同，R-OPS-05 当场失效。
# （uid/gid 不需要钉：构建始终以同一用户执行，天然一致。）
ARCHIVE_INPUTS=("$BUILD"/rust-obj/*.o "$BUILD"/abi_version.o "$BUILD"/cycle.o \
  "$BUILD"/cycle_"$HOST_ARCH".o "$BUILD"/errno_map.o "$BUILD"/sandbox.o "$BUILD"/exec.o \
  "$BUILD"/supervisor.o)
touch -t 197001010000.00 "${ARCHIVE_INPUTS[@]}"
rm -f "$BUILD/libhermes_native.a"
ar rc "$BUILD/libhermes_native.a" "${ARCHIVE_INPUTS[@]}"

# 单独跑一次 ranlib 重写符号索引。
#
# 这一行不是冗余：Apple 的 `ar rcs`（-s = 顺便建索引）产出的 __.SYMDEF 是
# **非确定性**的 —— 同样输入两次归档，字节不同。实测 `ar rc` + 独立 ranlib
# 两次的 sha256 完全相等，而 `ar rcs` 两次不等、`libtool -static` 两次也不等。
# 这是 R-OPS-05（可重现构建）在这台机器上唯一需要绕的坑。
#
# 用的是 Xcode cctools 的 ranlib，不是 GNU binutils —— 许可证问题上与 clang 同类。
ranlib "$BUILD/libhermes_native.a"

# ranlib 会把**当前时间**写进 __.SYMDEF 成员的 mtime 字段 —— 这是 macOS 上
# 唯一残留的不可重现来源（实测：只差 1 个字节，位置正是该成员头的 date 字段）。
# arfix 把所有成员的 date/uid/gid/mode 钉成常量。ar 头没有校验和，改写是安全的。
go run ./tools/arfix "$BUILD/libhermes_native.a" >/dev/null

# ------------------------------------------------------------------ #
# ⑥ go build
# ------------------------------------------------------------------ #
say "step 6/8 go build (cgo, external link against libhermes_native.a)"
# 链接器选择只在**链接期**生效；放进编译 flags 会被 -Werror 当成未使用参数。
if [ ${#LD_FLAGS[@]} -gt 0 ]; then
  export CGO_LDFLAGS="${CGO_LDFLAGS:-} ${LD_FLAGS[*]}"
fi

# buildID 必须是**确定性**的：它参与 sha256，任何随机/时间戳都会破坏
# R-OPS-05。这里的取值方式 = 头文件 + go.mod 的内容哈希。
BUILD_ID="$(
  {
    cat "$ROOT/native/include/hermes_abi.h" "$ROOT/native/include/hm_internal.h"
    cat "$ROOT/go.mod"
  } | shasum -a 256 | cut -c1-16
)"

# `-a` 不是保守，是**必要**：Go 只对 Go 源码做增量，不跟踪外部静态库
# （libhermes_native.a / libhermes_parser.a）的内容变化。少了它，改完 C++/Rust
# 再构建，二进制里跑的仍是旧代码 —— 而症状是"改了没用"，极难定位。
# 代价实测约 5 秒，比一次误判便宜得多。
go build -a -trimpath -buildvcs=false \
  -ldflags "-linkmode external -X main.buildID=${BUILD_ID}" \
  -o "$BUILD/hermes" ./cmd/hermes

# ------------------------------------------------------------------ #
# ⑥b 运行时静态库（AOT 用）
#
# `-buildmode=c-archive` 把运行时导出成 **C 可链接**的 .a，并顺带生成同名 .h。
# 这是 `hermes compile` 的依赖：产物内嵌 IR + 链接它 => 不需要目标机器装 hermes。
# 不生成它，AOT 路径在所有别的机器上都是"找不到运行时库"。
# ------------------------------------------------------------------ #
say "step 6b/8 go build -buildmode=c-archive (libhermesrt.a)"
# CGO_CFLAGS=-g0：cgo 编 C 代码时默认带调试信息，而 DWARF 里会记下
# **本机绝对路径**（例如 /Users/<名字>/.../clang/21/include）。这些字节会随发布包
# 发给所有人 —— 既是信息泄露，也让"同一个 tag 编出同样的东西"变得不成立。
CGO_CFLAGS="-g0" go build -a -trimpath -buildvcs=false -buildmode=c-archive \
  -o "$BUILD/libhermesrt.a" ./cmd/hermesrt

# 同一份运行时的**动态库**版本：给进程内绑定用（Python 的 ctypes、以及
# 任何能加载 .dylib/.so 的语言）。静态库只能链接进 C 程序，而"像 PyQt 那样封装"
# 需要一个能在运行时加载的东西。
say "step 6d/8 node N-API addon (hermes.node, optional)"

# Node 没有内建 FFI，JS/TS 的进程内绑定只能走原生插件。没有 node 时跳过 ——
# 跳过要说出来，不能静默当成"没有这个能力"。
if command -v node >/dev/null 2>&1; then
  bash "$ROOT/scripts/build_node_addon.sh" || echo "    (Node addon not built: does not affect other languages)"
else
  echo "    (node not found, skipping Node addon)"
fi

# 去掉 DWARF：cgo 编 C 代码时会把**本机绝对路径**记进调试信息
# （例如 /Users/<名字>/.../clang/21/include）。这些字节会随发布包发给所有人，
# 既是信息泄露，也让构建不可复现。strip -S 只删调试段，不动符号。
strip_debug() {
  local f="$1"
  [ -f "$f" ] || return 0
  if [ "$SHLIB_EXT" = ".dylib" ]; then
    strip -S "$f" 2>/dev/null || true
  else
    strip --strip-debug "$f" 2>/dev/null || true
  fi
}



say "step 6c/8 go build -buildmode=c-shared (libhermesrt shared library)"
CGO_CFLAGS="-g0" go build -a -trimpath -buildvcs=false -buildmode=c-shared \
  -o "$BUILD/libhermesrt$SHLIB_EXT" ./cmd/hermesrt

strip_debug "$BUILD/libhermesrt$SHLIB_EXT"

# ------------------------------------------------------------------ #
# ⑦ hermes-shim（独立小二进制；D-005 明确它是交付物之一）
#
# 不链接 Rust、不链接 libhermes_native.a —— shim 必须是能被审的最小程序。
# ------------------------------------------------------------------ #
say "step 7/8 build hermes-shim"
# abi_version.c 必须一起编：shim 的 --self-test 要打印布局指纹，定义在那里。
# 它是纯 C、零依赖，不会把 Rust 拖进来。
"$CC_BIN" "${CFLAGS[@]}" \
  native/c/shim.c native/c/sandbox.c native/c/errno_map.c native/c/abi_version.c \
  -o "$BUILD/hermes-shim"

# ------------------------------------------------------------------ #
# ⑧ 产物校验
#
# 这里刻意把"shim 缺失"变成构建失败：D-005 说得很清楚，两个二进制是一个
# 交付单元，只产出 hermes 的构建是坏的构建。
# ------------------------------------------------------------------ #
say "step 8/8 verify artifacts"

for f in "$BUILD/hermes" "$BUILD/hermes-shim" "$BUILD/libhermes_native.a"; do
  [ -f "$f" ] || die "missing artifact: $f"
done

"$BUILD/hermes-shim" --self-test || die "hermes-shim --self-test failed"

# 符号双向 diff：少一个 = 功能缺失；多一个 = 原生层偷偷长出新职责。
nm -gU "$BUILD/libhermes_native.a" 2>/dev/null \
  | awk '{print $NF}' \
  | sed 's/^_//' \
  | grep -E '^hm_' \
  | sort -u > "$BUILD/symbols.txt" || true
grep -vE '^\s*(#|$)' "$ROOT/scripts/expected_symbols.txt" | sort -u > "$BUILD/symbols.expected.txt"
if ! diff -u "$BUILD/symbols.expected.txt" "$BUILD/symbols.txt"; then
  die "ABI symbol surface drifted from scripts/expected_symbols.txt"
fi

"$BUILD/hermes" version >/dev/null || die "hermes version failed"

# T09 的 libhermes_parser.a 必须在这张清单里：可重现性断言只覆盖清单内的产物，
# 把它漏在外面等于"新产物永不参与两次构建字节比较"。
ARTIFACTS=("$BUILD/hermes" "$BUILD/hermes-shim" \
           "$BUILD/libhermes_native.a" "$BUILD/libhermes_parser.a")
shasum -a 256 "${ARTIFACTS[@]}" | sed "s|$BUILD/||" > "$BUILD/artifacts.sha256"

say "artifacts"
for a in "${ARTIFACTS[@]}"; do
  printf '  %-28s %s\n' "$(basename "$a")" "$(shasum -a 256 "$a" | cut -d' ' -f1)"
done
printf '  %-28s %s\n' "build_id"      "$BUILD_ID"
say "build ok"
