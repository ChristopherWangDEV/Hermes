
# build.ps1 —— Windows 原生构建（**只支持 LLVM-MinGW**）。
#
# 红线（与 build.sh 一致，且在脚本里**断言**，不靠自觉）：
#   · 编译器只用 clang；目标是 x86_64-w64-windows-gnu
#   · C++ 标准库必须是 libc++（Apache-2.0 WITH LLVM-exception），
#     不是 libstdc++（GPL-3.0 WITH GCC Runtime Library Exception）
#   · 不使用 GCC、不使用 MSVC 编译器、不链 MSVC 的 STL/UCRT
#   · 不引入任何 GPL 类传染性依赖
#
# 为什么只剩一条路径：
#   Go 的 cgo 产 c-archive 需要可重定位链接（-r）。MSVC 目标的 lld-link
#   不支持它 —— 于是那条路径**永远给不出 C 嵌入库**，只能出 CLI。既然
#   Windows 的完整形态必须包含 C 嵌入，就只保留能做到这一点的工具链。
#
#   而 GNU MinGW 又不能选：它带 GCC 与 libstdc++（GPL）。
#   LLVM-MinGW 是 Clang/LLVM + MinGW-w64 运行时 + libc++，全程无 GCC ——
#   这是唯一同时满足"完整能力"与"红线"的 Windows 工具链。
#
# 安装 LLVM-MinGW：
#   1. 从 mstorsjo/llvm-mingw 的 release 下载 Windows 宿主包
#      （形如 llvm-mingw-<日期>-ucrt-x86_64.zip），解压到任意目录
#      （例如 C:\llvm-mingw）。ucrt 与 msvcrt 两种 CRT 变体都可用：
#      它们是**宿主**二进制链接的 C 运行时，与本项目的 GNU 目标无关。
#   2. 设置环境变量 LLVM_MINGW_PATH 指向该目录
#      （或把它的 bin 放进 PATH）
#   3. rustup target add x86_64-pc-windows-gnu
#
# 用法（普通 PowerShell 即可，不需要管理员）：
#   pwsh -File scripts\build.ps1
$ErrorActionPreference = "Stop"

$Root  = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
$Build = Join-Path $Root "build"
New-Item -ItemType Directory -Path $Build -Force | Out-Null

$Target        = "x86_64-w64-windows-gnu"
$RustTarget    = "x86_64-pc-windows-gnu"

function Say($m)  { Write-Host "==> $m" }
function Warn($m) { Write-Host "  warn: $m" -ForegroundColor Yellow }
function Die($m)  { Write-Host ""; Write-Host "FATAL: $m" -ForegroundColor Red; exit 1 }

# ------------------------------------------------------------------ 探测 ------
# 三级探测：显式环境变量 → PATH → 常见安装位置。
# 每一级都必须通过 Test-GnuTarget（真实编译一次），因为"目标在 clang 的支持
# 列表里"不等于"这台机器装了 MinGW-w64 的头与库"。
function Test-GnuTarget($clangPath) {
  $tmp = Join-Path ([IO.Path]::GetTempPath()) ("hmprobe-" + [guid]::NewGuid().ToString("N"))
  New-Item -ItemType Directory -Path $tmp -Force | Out-Null
  try {
    Set-Content -Path (Join-Path $tmp "p.c") -Value "int hm_probe(void){return 0;}" -Encoding ascii
    & $clangPath "--target=$Target" -c (Join-Path $tmp "p.c") -o (Join-Path $tmp "p.o") 2>$null
    return ($LASTEXITCODE -eq 0) -and (Test-Path (Join-Path $tmp "p.o"))
  } catch { return $false } finally {
    Remove-Item $tmp -Recurse -Force -ErrorAction SilentlyContinue
  }
}

# 红线断言：C++ 标准库必须是 libc++。
# GNU MinGW 会在这里失败（__GLIBCXX__ 已定义），于是被拒 —— 这正是
# "llvm-mingw" 与 "mingw" 的判据，由编译器自己回答，不靠目录名字猜。
function Test-Libcxx($clangxxPath) {
  $tmp = Join-Path ([IO.Path]::GetTempPath()) ("hmprobe-" + [guid]::NewGuid().ToString("N"))
  New-Item -ItemType Directory -Path $tmp -Force | Out-Null
  try {
    $src = @"
#include <cstddef>
#if defined(__GLIBCXX__)
#error libstdc++ in use (GPL)
#endif
#if !defined(_LIBCPP_VERSION)
#error not libc++
#endif
int main(){return 0;}
"@
    Set-Content -Path (Join-Path $tmp "p.cpp") -Value $src -Encoding ascii
    & $clangxxPath "--target=$Target" -std=c++17 -stdlib=libc++ -fsyntax-only (Join-Path $tmp "p.cpp") 2>$null
    return ($LASTEXITCODE -eq 0)
  } catch { return $false } finally {
    Remove-Item $tmp -Recurse -Force -ErrorAction SilentlyContinue
  }
}

function Find-LlvmMingw {
  $candidates = @()
  if ($env:LLVM_MINGW_PATH) {
    $candidates += (Join-Path $env:LLVM_MINGW_PATH "bin")
    $candidates += $env:LLVM_MINGW_PATH
  }
  foreach ($d in @(
      "C:\llvm-mingw\bin",
      "C:\Program Files\llvm-mingw\bin",
      "C:\Program Files (x86)\llvm-mingw\bin",
      "$env:LOCALAPPDATA\llvm-mingw\bin",
      "$env:USERPROFILE\llvm-mingw\bin"
  )) { $candidates += $d }
  $onPath = Get-Command "clang.exe" -ErrorAction SilentlyContinue
  if ($onPath) { $candidates += (Split-Path -Parent $onPath.Source) }

  foreach ($c in $candidates) {
    if (-not $c) { continue }
    $clang = Join-Path $c "clang.exe"
    if (Test-Path $clang) { if (Test-GnuTarget $clang) { return $c } }
  }
  return $null
}

$Bin = Find-LlvmMingw
if (-not $Bin) {
  Die @"
找不到支持 $Target 的 clang（LLVM-MinGW）。

Hermes 的 Windows 构建**只使用 LLVM-MinGW**，原因有两条：
  · Go 的 c-archive 需要可重定位链接（-r），MSVC 目标的 lld-link 不支持；
  · GNU MinGW 带 GCC 与 libstdc++（GPL），违反本项目的红线。

安装步骤：
  1. 下载 mstorsjo/llvm-mingw 的 Windows 宿主包（llvm-mingw-<日期>-ucrt-x86_64.zip；
     ucrt / msvcrt 两种变体都可，那是宿主 CRT，与 GNU 目标无关）
  2. 解压到任意目录，例如 C:\llvm-mingw
  3. setx LLVM_MINGW_PATH C:\llvm-mingw
  4. rustup target add $RustTarget
  5. 重开终端后重跑本脚本
"@
}

$Clang   = Join-Path $Bin "clang.exe"
$ClangXX = Join-Path $Bin "clang++.exe"
$Ar      = Join-Path $Bin "llvm-ar.exe"
$Objdump = Join-Path $Bin "llvm-objdump.exe"

Say "toolchain: LLVM-MinGW ($Bin)"
Say "target: $Target"
$env:PATH = "$Bin;" + $env:PATH

# 红线断言（一）：C++ 标准库是 libc++。
if (-not (Test-Libcxx $ClangXX)) {
  Die "该工具链的 C++ 标准库不是 libc++（可能是 GNU MinGW，其 libstdc++ 为 GPL）：$ClangXX"
}
Say "stdlib check: libc++"

# 红线断言（二）：Rust 的 GNU 目标必须已安装，否则产不出 GNU ABI 的静态库。
$rustTargets = (& rustup target list --installed 2>$null) -join " "
if ($rustTargets -notmatch [regex]::Escape($RustTarget)) {
  Die "Rust 缺 $RustTarget 目标。执行：rustup target add $RustTarget"
}

# ------------------------------------------------------------------ 1. C ------
Say "1/6 compile C (clang, integrated assembler)"
$cobjs = New-Object System.Collections.ArrayList
foreach ($f in @("abi_version.c","cycle.c","errno_map.c","sandbox.c","win_spawn.c")) {
  $src = Join-Path $Root "native\c\$f"
  if (-not (Test-Path $src)) { Die "缺源文件 $f" }
  $o = Join-Path $Build ([IO.Path]::GetFileNameWithoutExtension($f) + ".o")
  & $Clang "--target=$Target" -std=c11 -O2 -fvisibility=hidden -fno-common `
      -Wall -Wextra -Werror -D_FORTIFY_SOURCE=2 -DNDEBUG `
      -I "$Root\native\include" -c $src -o $o
  if ($LASTEXITCODE -ne 0) { Die "编译失败: $f" }
  [void]$cobjs.Add($o)
}

# ------------------------------------------------------------------ 2. Rust ---
Say "2/6 cargo build --target $RustTarget (staticlib, offline, locked)"
Push-Location "$Root\native\rust"
$env:CARGO_TARGET_X86_64_PC_WINDOWS_GNU_LINKER = $Clang
cargo build --release --offline --locked --target $RustTarget
$rc = $LASTEXITCODE
Pop-Location
if ($rc -ne 0) { Die "cargo 构建失败" }

# ------------------------------------------------------------------ 3. 归档 ---
Say "3/6 archive native + rust (libhermes_native.a)"
$nativeLib = Join-Path $Build "libhermes_native.a"
if (Test-Path $nativeLib) { Remove-Item $nativeLib -Force }
& $Ar rcs $nativeLib $cobjs
if ($LASTEXITCODE -ne 0) { Die "归档 native 失败" }
$rustLib = Join-Path $Root "native\rust\target\$RustTarget\release\libhermes_core.a"
if (-not (Test-Path $rustLib)) { Die "找不到 Rust 静态库（期望 $rustLib）" }
Copy-Item $rustLib (Join-Path $Build "libhermes_core.a") -Force

# ------------------------------------------------------------------ 4. C++ ----
Say "4/6 compile C++ frontend (clang++, libc++)"
$cpp = Join-Path $Build "cpp"
New-Item -ItemType Directory -Path $cpp -Force | Out-Null
$pobjs = New-Object System.Collections.ArrayList
foreach ($src in (Get-ChildItem "$Root\native\cpp" -Filter *.cpp | Where-Object { $_.Name -notlike "._*" })) {
  $o = Join-Path $cpp ($src.BaseName + ".o")
  & $ClangXX "--target=$Target" -std=c++17 -O2 -stdlib=libc++ -fno-exceptions -fno-rtti `
      -fvisibility=hidden -fno-threadsafe-statics -fno-common -Wall -Wextra -Werror `
      -I "$Root\native\cpp" -I "$Root\native\include" -c $src.FullName -o $o
  if ($LASTEXITCODE -ne 0) { Die "C++ 编译失败: $($src.Name)" }
  [void]$pobjs.Add($o)
}
$parserLib = Join-Path $Build "libhermes_parser.a"
if (Test-Path $parserLib) { Remove-Item $parserLib -Force }
& $Ar rcs $parserLib $pobjs
if ($LASTEXITCODE -ne 0) { Die "归档 parser 失败" }

# ------------------------------------------------------------------ 5. Go -----
# Go 的 cgo 白名单不接受 --target / -fuse-ld，所以用一个薄包装把目标参数
# 注入到每一次 clang 调用里 —— 这是把 GNU 目标透传给 cgo 的唯一办法。
$wrap = Join-Path $Build "clang-driver.cmd"
@"
@echo off
"$Clang" --target=$Target -fuse-ld=lld %*
"@ | Set-Content -Path $wrap -Encoding ascii

Say "5/6 go build hermes.exe (cgo)"
$env:CGO_ENABLED = "1"
$env:CC = $wrap
$env:CGO_CFLAGS = "-I$Root\native\include -g0"
$env:CGO_LDFLAGS = "-L$Build"
Push-Location $Root
go build -trimpath -buildvcs=false -o (Join-Path $Build "hermes.exe") ./cmd/hermes
$rc = $LASTEXITCODE
Pop-Location
if ($rc -ne 0) { Die "go build 失败" }

# ------------------------------------------------------------------ 6. 运行时 --
# GNU 味的 ld.lld 支持 -r，cgo 探针能过 —— 这就是选 LLVM-MinGW 的全部理由。
Say "6/6 go build -buildmode=c-archive (libhermesrt.a)"
Push-Location $Root
go build -a -trimpath -buildvcs=false -buildmode=c-archive `
  -o (Join-Path $Build "libhermesrt.a") ./cmd/hermesrt
$rc = $LASTEXITCODE
Pop-Location
if ($rc -ne 0) { Die "c-archive 构建失败" }
if (-not (Test-Path (Join-Path $Build "libhermesrt.h"))) { Die "c-archive 没生成 libhermesrt.h" }
Say "runtime: libhermesrt.a + libhermesrt.h"

# ------------------------------------------------------------------ 校验 ------
# 产物验证：不允许出现 libgcc / libstdc++。
# libwinpthread 是 MinGW-w64 的正常组成部分（MIT，非传染性），不在禁止列表。
$exe = Join-Path $Build "hermes.exe"
$imports = (& $Objdump -p $exe | Select-String -Pattern "DLL Name:") -join " "
foreach ($b in @("libgcc", "libstdc++")) {
  if ($imports -match [regex]::Escape($b)) { Die "产物链接了 $b（红线）" }
}
Say "artifact check: no GNU C++ runtime imported"

if ($imports -match "msvcrt|vcruntime") {
  Warn "产物导入了 MSVC 运行库 —— 说明工具链不是 LLVM-MinGW，请检查 LLVM_MINGW_PATH"
}

Say "build ok (LLVM-MinGW, full C embedding)"
& $exe version
