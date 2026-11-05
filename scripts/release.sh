#!/usr/bin/env bash
# 生成**下载即用**的发布包（不需要用户装 Go / Rust / cargo）。
#
# 发布包布局（与 internal/layout 的解析顺序一致）：
#   hermes-<version>-<os>-<arch>/
#     bin/hermes            编译器本体（静态链接了 Rust 扩展与 C++ 前端）
#     bin/hermes-shim       沙箱子进程
#     lib/libhermesrt.a     AOT / hermes cc 需要的运行时
#     lib/hermes_rt.h       它的 C 头
#     include/hermes/hermes_core.h
#     README.md  LICENSE  SHA256SUMS
#
# 为什么不打包源码、不打 `.hermes` 的 stdlib：stdlib 已经 go:embed 进二进制，
# 安装包不需要它 —— 少了它，包里就少了"版本与二进制不配套"的一整类故障。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUILD="$ROOT/build"
# 版本串来自 `hermes version` 的第一行（`hermes 0.1.0`）。
# 之前用 `awk '/semver/'`，而那一行根本不存在 —— 于是包名成了 `hermes--darwin-arm64`
# 这种没有版本号的东西，而**没有版本号的发布包**无法回答"这是哪个版本"。
VERSION="$("$BUILD/hermes" version 2>/dev/null | awk 'NR==1 {print $2}' || echo "0.0.0")"
[ -z "$VERSION" ] && VERSION="0.0.0"
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
NAME="hermes-${VERSION}-${OS}-${ARCH}"
OUT="$BUILD/release/$NAME"

echo "==> release: $NAME"

# 先构建：发布包里的每个文件都必须来自同一次构建，否则版本校验没有意义。
"$ROOT/scripts/build.sh" >/dev/null

rm -rf "$OUT"
mkdir -p "$OUT/bin" "$OUT/lib" "$OUT/include/hermes"

cp "$BUILD/hermes" "$OUT/bin/hermes"
[ -f "$BUILD/hermes-shim" ] && cp "$BUILD/hermes-shim" "$OUT/bin/hermes-shim"
cp "$BUILD/libhermesrt.a" "$OUT/lib/libhermesrt.a"

# CMake 模块：让 C/C++ 项目一行接入。没装 cmake 也能带着 —— 它不参与构建。
if [ -d "$ROOT/cmake" ]; then
  mkdir -p "$OUT/lib/cmake/hermes"
  cp "$ROOT"/cmake/*.cmake "$OUT/lib/cmake/hermes/" 2>/dev/null || true
fi

# 动态库：**进程内绑定**（Python ctypes / Java FFM / C# P/Invoke / Go dlopen）靠它。
# 之前只拷了 .a，于是"装了这个包的人"根本用不了 Python SDK 的原生绑定 ——
# 而 README 明明写着两种用法。静态库只能在链接期用，运行期加载需要 .dylib/.so。
for ext in dylib so; do
  if [ -f "$BUILD/libhermesrt.$ext" ]; then
    cp "$BUILD/libhermesrt.$ext" "$OUT/lib/libhermesrt.$ext"
  fi
done

# 运行时头：go build -buildmode=c-archive 生成的头跟着 -o 走，
# 发布包里统一叫 hermes_rt.h（internal/layout 两种都认）。
if [ -f "$BUILD/hermes_rt.h" ]; then
  cp "$BUILD/hermes_rt.h" "$OUT/lib/hermes_rt.h"
elif [ -f "$BUILD/libhermesrt.h" ]; then
  cp "$BUILD/libhermesrt.h" "$OUT/lib/hermes_rt.h"
fi

cp "$ROOT/include/hermes/hermes_core.h" "$OUT/include/hermes/hermes_core.h"
cp "$ROOT/include/hermes/hermes.h" "$OUT/include/hermes/hermes.h"
cp "$ROOT/README.md" "$OUT/README.md"

# 合规文件必须随包走：分发二进制时，这些**不是可选项**。
# 缺了它们，"Apache-2.0 开源"就只对了本体那一半，第三方部分没有履行义务。
cp "$ROOT/LICENSE" "$OUT/LICENSE"
cp "$ROOT/NOTICE" "$OUT/NOTICE"
cp "$ROOT/THIRD_PARTY_NOTICES.md" "$OUT/THIRD_PARTY_NOTICES.md"
mkdir -p "$OUT/licenses"
cp "$ROOT"/licenses/* "$OUT/licenses/"

# 校验和：用户下载后**先验再跑**，这是唯一能证明"没被中间人改过"的本地手段。
( cd "$OUT" && find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 shasum -a 256 > SHA256SUMS )

TAR="$BUILD/release/$NAME.tar.gz"
tar -C "$BUILD/release" -czf "$TAR" "$NAME"

echo "==> $TAR"
echo "==> 包内布局："
( cd "$OUT" && find . -type f | sort | sed 's/^/      /' )
echo
echo "验证（模拟用户）："
echo "  tar -xzf $NAME.tar.gz && \\"
echo "    HERMES_HOME=\$PWD/$NAME $PWD/$NAME/bin/hermes doctor"
