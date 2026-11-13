
# build.ps1 —— Windows 原生构建。
#
# 红线（与 build.sh 一致，且在脚本里**断言**，不靠自觉）：
#   · 编译器只用 clang；不使用 GNU 工具链（gcc / GNU ld）
#   · 不调用 MSVC 的 cl.exe（只用它的头与库）
#   · C++ 标准库必须是 libc++，不是 libstdc++（GPL）
#   · 不引入任何 GPL 类传染性依赖
#
# 两条构建路径：
#
#   gnu  —— 检测到 LLVM-MinGW 时。clang + libc++ + ld.lld，目标
#           x86_64-w64-windows-gnu。GNU 味的 lld 支持可重定位链接（-r），
#           所以 Go 的 c-archive 能跑通，**产物包含完整 C 嵌入支持**。
#
#   msvc —— 未检测到 LLVM-MinGW 时的温和降级。目标 x86_64-pc-windows-msvc，
#           lld-link 不支持 -r，因此**不产 C 嵌入库**，只出 hermes.exe。
#           这不是错误，只是能力少一块；doctor 会给出明确提示。
#
# 为什么 LLVM-MinGW 而不是 GNU MinGW：
#   GNU MinGW 带 GCC 与 libstdc++（GPL-3.0 WITH GCC Runtime Library Exception）。
#   LLVM-MinGW 是 Clang/LLVM + MinGW-w64 运行时 + **libc++**，全程无 GCC。
#   脚本里的 libc++ 探针就是为了把这两者区分开：探针失败 = 不是 LLVM-MinGW。
#
# 用法（普通 PowerShell 即可，不需要管理员）：
#   pwsh -File scripts\build.ps1                  # 自动探测
#   pwsh -File scripts\build.ps1 -Toolchain gnu   # 强制 GNU 路径（找不到就报错）
#   pwsh -File scripts\build.ps1 -Toolchain msvc  # 强制降级路径
param(
  [ValidateSet("auto", "gnu", "msvc")]
  [string]$Toolchain = "auto"
)

$ErrorActionPreference = "Stop"

$Root  = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
$Build = Join-Path $Root "build"
New-Item -ItemType Directory -Path $Build -Force | Out-Null

function Say($m) { Write-Host "==> $m" }
function Warn($m) { Write-Host "  warn: $m" -ForegroundColor Yellow }

# ------------------------------------------------------------------ 探测 ------
# Find-LlvmMingw 按优先级找 LLVM-MinGW 的 bin 目录：
#   1. 环境变量 LLVM_MINGW_PATH（用户显式指定，最可靠）
#   2. PATH 里的 clang，且它支持 GNU 目标
#   3. 几个常见安装位置
# 找不到返回 $null —— **不抛异常**，降级由调用方决定。
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

  # PATH 里的 clang 也可能就是 LLVM-MinGW 的（它自带 bin/clang.exe）
  $onPath = Get-Command "clang.exe" -ErrorAction SilentlyContinue
  if ($onPath) { $candidates += (Split-Path -Parent $onPath.Source) }

  foreach ($c in $candidates) {
    if (-not $c) { continue }
    $clang = Join-Path $c "clang.exe"
    if (Test-Path $clang) {
      # 只有真正支持 GNU 目标才算（普通 LLVM 的 clang 也「支持」，但
      # 没有 MinGW 的头与库就跑不通编译 —— 所以下一步还要试编一次）。
      if (Test-GnuTarget $clang) { return $c }
    }
  }
  return $null
}

# Test-GnuTarget：用一次真实编译确认这个 clang 能出 GNU 目标的 .o。
# 只问 --print-targets 是不够的：目标在列表里不代表装了 MinGW 的头与库。
function Test-GnuTarget($clangPath) {
  $tmp = Join-Path ([IO.Path]::GetTempPath()) ("hmprobe-" + [guid]::NewGuid().ToString("N"))
  New-Item -ItemType Directory -Path $tmp -Force | Out-Null
  try {
    Set-Content -Path (Join-Path $tmp "p.c") -Value "int hm_probe(void){return 0;}" -Encoding ascii
    & $clangPath --target=x86_64-w64-windows-gnu -c (Join-Path $tmp "p.c") -o (Join-Path $tmp "p.o") 2>$null
    return ($LASTEXITCODE -eq 0) -and (Test-Path (Join-Path $tmp "p.o"))
  } catch { return $false } finally {
    Remove-Item $tmp -Recurse -Force -ErrorAction SilentlyContinue
  }
}

# Test-Libcxx：红线断言 —— 这个 C++ 编译器用的必须是 libc++，不是 libstdc++。
# GNU MinGW 会在这里失败（__GLIBCXX__ 已定义），于是被拒。
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
    & $clangxxPath --target=x86_64-w64-windows-gnu -std=c++17 -stdlib=libc++ -fsyntax-only (Join-Path $tmp "p.cpp") 2>$null
    return ($LASTEXITCODE -eq 0)
  } catch { return $false } finally {
    Remove-Item $tmp -Recurse -Force -ErrorAction SilentlyContinue
  }
}

# Rust 的 GNU 目标需要 rustup 装过；没装就没法产 GNU ABI 的静态库。
function Test-RustGnuTarget {
  $out = (& rustup target list --installed 2>$null) -join "`n"
  return ($out -match "x86_64-pc-windows-gnu")
}

# ------------------------------------------------------------------ 决策 ------
$Mode = $null
$Bin = $null

if ($Toolchain -ne "msvc") {
  $gnuBin = Find-LlvmMingw
  if ($gnuBin) {
    if (Test-Libcxx (Join-Path $gnuBin "clang++.exe")) {
      if (Test-RustGnuTarget) {
        $Mode = "gnu"; $Bin = $gnuBin
      } else {
        Warn "LLVM-MinGW 已找到，但 Rust 缺 x86_64-pc-windows-gnu 目标 —— 降级为 msvc 路径"
        Warn "补齐后可开启 C 嵌入：rustup target add x86_64-pc-windows-gnu"
        if ($Toolchain -eq "gnu") { throw "指定了 -Toolchain gnu，但 Rust 的 GNU 目标未安装" }
      }
    } else {
      Warn "找到的 clang++ 用的不是 libc++（可能是 GNU MinGW，其 libstdc++ 为 GPL）—— 拒绝使用"
      if ($Toolchain -eq "gnu") { throw "指定了 -Toolchain gnu，但该工具链的 C++ 标准库不是 libc++" }
    }
  } elseif ($Toolchain -eq "gnu") {
    throw "指定了 -Toolchain gnu，但没找到 LLVM-MinGW。安装后设置 LLVM_MINGW_PATH 指向它的解压目录。"
  }
}

if (-not $Mode) {
  $Mode = "msvc"
  $Bin = "C:\Program Files\LLVM\bin"
  if (-not (Test-Path (Join-Path $Bin "clang.exe"))) { throw "找不到 clang.exe（期望 $Bin，或设置 LLVM_MINGW_PATH 指向 LLVM-MinGW）" }
}

# 两种模式共用的差异表。放在一处，避免下面五个阶段各写一遍 if。
switch ($Mode) {
  "gnu" {
    $Clang     = Join-Path $Bin "clang.exe"
    $ClangXX   = Join-Path $Bin "clang++.exe"
    $Ar        = Join-Path $Bin "llvm-ar.exe"
    $TargetArg = @("--target=x86_64-w64-windows-gnu")
    $RustTarget = "x86_64-pc-windows-gnu"
    $NativeLibName = "libhermes_native.a"
    $ParserLibName = "libhermes_parser.a"
    $CoreLibName   = "libhermes_core.a"
  }
  "msvc" {
    $Clang     = Join-Path $Bin "clang.exe"
    $ClangXX   = Join-Path $Bin "clang++.exe"
    $Ar        = Join-Path $Bin "llvm-ar.exe"
    $TargetArg = @()
    $RustTarget = $null
    $NativeLibName = "hermes_native.lib"
    $ParserLibName = "hermes_parser.lib"
    $CoreLibName   = "hermes_core.lib"
  }
}

Say "toolchain mode: $Mode"
Say "compiler: $Clang"
$env:PATH = "$Bin;" + $env:PATH

# 机器上装有 GNU/MSVC 编译器是允许的 —— 红线管的是**用什么构建**，不是"装了什么"。
foreach ($bad in @("gcc", "g++", "cl")) {
  $p = Get-Command $bad -ErrorAction SilentlyContinue
  if ($p) { Warn "PATH 里存在 $bad（$($p.Source)）—— 本构建不会调用它" }
}

# ------------------------------------------------------------------ 1. C ------
Say "1/6 compile C (clang, integrated assembler)"
$cobjs = New-Object System.Collections.ArrayList
foreach ($f in @("abi_version.c","cycle.c","errno_map.c","sandbox.c","win_spawn.c")) {
  $src = Join-Path $Root "native\c\$f"
  if (-not (Test-Path $src)) { throw "缺源文件 $f" }
  $o = Join-Path $Build ([IO.Path]::GetFileNameWithoutExtension($f) + ".o")
  & $Clang @TargetArg -std=c11 -O2 -fvisibility=hidden -fno-common -Wall -Wextra -Werror `
      -D_FORTIFY_SOURCE=2 -DNDEBUG -I "$Root\native\include" -c $src -o $o
  if ($LASTEXITCODE -ne 0) { throw "编译失败: $f" }
  [void]$cobjs.Add($o)
}

# ------------------------------------------------------------------ 2. Rust ---
Say "2/6 cargo build (staticlib, offline, locked)"
Push-Location "$Root\native\rust"
if ($Mode -eq "gnu") {
  $env:CARGO_TARGET_X86_64_PC_WINDOWS_GNU_LINKER = $Clang
  cargo build --release --offline --locked --target $RustTarget
} else {
  $env:CARGO_TARGET_X86_64_PC_WINDOWS_MSVC_LINKER = (Join-Path $Bin "lld-link.exe")
  cargo build --release --offline --locked
}
if ($LASTEXITCODE -ne 0) { Pop-Location; throw "cargo 构建失败" }
Pop-Location

$rustOut = if ($Mode -eq "gnu") {
  Join-Path $Root "native\rust\target\$RustTarget\release"
} else {
  Join-Path $Root "native\rust\target\release"
}

# ------------------------------------------------------------------ 3. 归档 ---
Say "3/6 archive native + rust ($NativeLibName)"
$nativeLib = Join-Path $Build $NativeLibName
if (Test-Path $nativeLib) { Remove-Item $nativeLib -Force }
& $Ar rcs $nativeLib $cobjs
if ($LASTEXITCODE -ne 0) { throw "归档 native 失败" }
$rustLib = if ($Mode -eq "gnu") { Join-Path $rustOut "libhermes_core.a" } else { Join-Path $rustOut "hermes_core.lib" }
if (-not (Test-Path $rustLib)) { throw "找不到 Rust 静态库（期望 $rustLib）" }
Copy-Item $rustLib (Join-Path $Build $CoreLibName) -Force

# ------------------------------------------------------------------ 4. C++ ----
Say "4/6 compile C++ frontend (clang++, libc++)"
$cpp = Join-Path $Build "cpp"; New-Item -ItemType Directory -Path $cpp -Force | Out-Null
$pobjs = New-Object System.Collections.ArrayList
foreach ($src in (Get-ChildItem "$Root\native\cpp" -Filter *.cpp | Where-Object { $_.Name -notlike "._*" })) {
  $o = Join-Path $cpp ($src.BaseName + ".o")
  & $ClangXX @TargetArg -std=c++17 -O2 -stdlib=libc++ -fno-exceptions -fno-rtti `
      -fvisibility=hidden -fno-threadsafe-statics -fno-common -Wall -Wextra -Werror `
      -I "$Root\native\cpp" -I "$Root\native\include" -c $src.FullName -o $o
  if ($LASTEXITCODE -ne 0) { throw "C++ 编译失败: $($src.Name)" }
  [void]$pobjs.Add($o)
}
$parserLib = Join-Path $Build $ParserLibName
if (Test-Path $parserLib) { Remove-Item $parserLib -Force }
& $Ar rcs $parserLib $pobjs
if ($LASTEXITCODE -ne 0) { throw "归档 parser 失败" }

# ------------------------------------------------------------------ 5. Go -----
# Go 的 cgo 白名单不接受 --target / -fuse-ld，所以用一个薄包装把目标参数
# 注入到每一次 clang 调用里。这是唯一能把 GNU 目标透传给 cgo 的办法。
$wrap = Join-Path $Build "clang-driver.cmd"
$targetLine = ($TargetArg -join " ")
if ($Mode -eq "gnu") {
  @"
@echo off
"$Clang" $targetLine -fuse-ld=lld %*
"@ | Set-Content -Path $wrap -Encoding ascii
} else {
  @"
@echo off
setlocal enabledelayedexpansion
set "EXTRA="
echo %* | findstr /C:"-c" >nul
if errorlevel 1 set "EXTRA=-fuse-ld=lld-link"
"$Clang" %EXTRA% %*
"@ | Set-Content -Path $wrap -Encoding ascii
}

Say "5/6 go build hermes.exe (cgo)"
$env:CGO_ENABLED = "1"
$env:CC = $wrap
$env:CGO_CFLAGS = "-I$Root\native\include"
$env:CGO_LDFLAGS = "-L$Build"
Push-Location $Root
go build -trimpath -buildvcs=false -o (Join-Path $Build "hermes.exe") ./cmd/hermes
if ($LASTEXITCODE -ne 0) { Pop-Location; throw "go build 失败" }
Pop-Location

# ------------------------------------------------------------------ 6. 运行时 --
if ($Mode -eq "gnu") {
  # GNU 味的 ld.lld 支持可重定位链接（-r），Go 的 cgo 探针能过 —— 于是
  # C 嵌入库可用。这一步正是引入 LLVM-MinGW 的全部理由。
  Say "6/6 go build -buildmode=c-archive (libhermesrt.a)"
  Push-Location $Root
  $env:CGO_CFLAGS = "-I$Root\native\include -g0"
  go build -a -trimpath -buildvcs=false -buildmode=c-archive `
    -o (Join-Path $Build "libhermesrt.a") ./cmd/hermesrt
  $rc = $LASTEXITCODE
  Pop-Location
  if ($rc -ne 0) { throw "c-archive 构建失败（GNU 目标下应当可用）" }
  Say "runtime archive: libhermesrt.a + libhermesrt.h"
} else {
  # 降级路径：msvc 目标的 lld-link 不支持 -r，cgo 探针过不去。
  # 这里不报错 —— 能力少一块而已，doctor 会提示怎么开启。
  Say "6/6 skip runtime archive (msvc target: lld-link has no -r; install LLVM-MinGW for C embedding)"
}

# ------------------------------------------------------------------ 校验 ------
# 产物验证：导入表里不能出现 GNU 运行库。gnu 模式下 libwinpthread 是
# MinGW-w64 的正常组成部分（MIT，非传染性），所以那一条只在 msvc 模式检查。
$exe = Join-Path $Build "hermes.exe"
$imports = (& (Join-Path $Bin "llvm-objdump.exe") -p $exe | Select-String -Pattern "DLL Name:") -join " "
$banned = @("libgcc", "libstdc++")
if ($Mode -eq "msvc") { $banned += "libwinpthread" }
foreach ($b in $banned) {
  if ($imports -match [regex]::Escape($b)) { throw "产物链接了 $b（红线）" }
}
Say "artifact check: no GNU C++ runtime imported"

# libc++ 探针的最终断言：无论是哪条路径，C++ 标准库都必须是 libc++。
if (-not (Test-Libcxx $ClangXX)) {
  throw "C++ 标准库不是 libc++（红线）：$ClangXX"
}
Say "stdlib check: libc++"

Say "build ok (mode=$Mode)"
& $exe version
if ($Mode -ne "gnu") {
  Warn "本机未用 LLVM-MinGW：产物只有 CLI，无 C 嵌入库。"
  Warn "开启方式：安装 LLVM-MinGW，设 LLVM_MINGW_PATH，然后 rustup target add x86_64-pc-windows-gnu"
}
