# build.ps1 —— Windows 原生构建。
#
# 红线（与 build.sh 一致，且在脚本里**断言**，不靠自觉）：
#   · 编译器只用 clang；链接器只用 lld-link（LLVM）
#   · 不使用 GNU 工具链（gcc / mingw / GNU ld）
#   · 不调用 MSVC 的 cl.exe（只用它的头与库：UCRT / STL，许可为
#     Apache-2.0 WITH LLVM-exception 与 Microsoft 可再分发条款）
#   · 不引入任何 GPL 类传染性依赖
#
# 用法（普通 PowerShell 即可，不需要管理员）：
#   pwsh -File scripts\build.ps1
$ErrorActionPreference = "Stop"

$Root  = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
$Build = Join-Path $Root "build"
New-Item -ItemType Directory -Path $Build -Force | Out-Null

function Say($m) { Write-Host "==> $m" }

# ---------------------------------------------------------------- 1. 工具链门禁
Say "step 1/6 toolchain gate (clang only; no GNU, no cl.exe)"
$llvm = "C:\Program Files\LLVM\bin"
if (-not (Test-Path "$llvm\clang.exe")) { throw "找不到 clang.exe（期望 $llvm）" }
$env:PATH = "$llvm;" + $env:PATH
$clangVer = (& clang --version | Select-Object -First 1)
if ($clangVer -notmatch "clang") { throw "clang 不可用" }
# 机器上装有 GNU/MSVC 编译器是允许的 —— 红线管的是**用什么构建**，不是"装了什么"。
# 但要让"我没用它们"变成可验证的事实：这里警告，构建结束后再检查产物导入表，
# 确认没有链进 libgcc / libstdc++。
foreach ($bad in @("gcc", "g++", "cl")) {
  $p = Get-Command $bad -ErrorAction SilentlyContinue
  if ($p) { Write-Host "  warn: PATH 里存在 $bad（$($p.Source)）—— 本构建不会调用它" }
}
Say "clang: $clangVer"

# ---------------------------------------------------------------- 2. C 对象
Say "step 2/6 compile C (clang, integrated assembler)"
$cobjs = New-Object System.Collections.ArrayList
foreach ($f in @("abi_version.c","cycle.c","errno_map.c","sandbox.c","win_spawn.c")) {
  $src = Join-Path $Root "native\c\$f"
  if (-not (Test-Path $src)) { throw "缺源文件 $f" }
  $o = Join-Path $Build ([IO.Path]::GetFileNameWithoutExtension($f) + ".obj")
  & clang -std=c11 -O2 -fvisibility=hidden -fno-common -Wall -Wextra -Werror `
      -D_FORTIFY_SOURCE=2 -DNDEBUG -I "$Root\native\include" -c $src -o $o
  if ($LASTEXITCODE -ne 0) { throw "编译失败: $f" }
  [void]$cobjs.Add($o)
}

# ---------------------------------------------------------------- 3. Rust
Say "step 3/6 cargo build (staticlib, offline, locked)"
Push-Location "$Root\native\rust"
$env:CARGO_TARGET_X86_64_PC_WINDOWS_MSVC_LINKER = "$llvm\lld-link.exe"
cargo build --release --offline --locked
if ($LASTEXITCODE -ne 0) { Pop-Location; throw "cargo 构建失败" }
Pop-Location

# ---------------------------------------------------------------- 4. 归档
Say "step 4/6 archive native + rust（MSVC 命名：hermes_native.lib）"
$nativeLib = Join-Path $Build "hermes_native.lib"
if (Test-Path $nativeLib) { Remove-Item $nativeLib -Force }
& llvm-ar rcs $nativeLib $cobjs
Copy-Item "$Root\native\rust\target\release\hermes_core.lib" (Join-Path $Build "hermes_core.lib") -Force
if (-not (Test-Path $nativeLib)) { throw "归档失败" }

# ---------------------------------------------------------------- 5. C++ 前端
Say "step 5/6 compile C++ frontend (clang++, MSVC STL)"
$cpp = Join-Path $Build "cpp"; New-Item -ItemType Directory -Path $cpp -Force | Out-Null
$pobjs = New-Object System.Collections.ArrayList
foreach ($src in (Get-ChildItem "$Root\native\cpp" -Filter *.cpp | Where-Object { $_.Name -notlike "._*" })) {
  $o = Join-Path $cpp ($src.BaseName + ".obj")
  & clang++ -std=c++17 -O2 -fno-exceptions -fno-rtti -fvisibility=hidden `
      -fno-threadsafe-statics -fno-common -Wall -Wextra -Werror `
      -I "$Root\native\cpp" -I "$Root\native\include" -c $src.FullName -o $o
  if ($LASTEXITCODE -ne 0) { throw "C++ 编译失败: $($src.Name)" }
  [void]$pobjs.Add($o)
}
$parserLib = Join-Path $Build "hermes_parser.lib"
if (Test-Path $parserLib) { Remove-Item $parserLib -Force }
& llvm-ar rcs $parserLib $pobjs

# ---------------------------------------------------------------- 6. Go
Say "step 6/6 go build (cgo, clang driver, lld-link)"
$env:CGO_ENABLED = "1"
$env:CC = "$llvm\clang.exe"
$env:CGO_CFLAGS = "-I$Root\native\include"
# Go 的 cgo 白名单不允许 -fuse-ld，所以用一个薄包装：
# 只在**链接可执行文件**时追加 -fuse-ld=lld-link，编译期原样透传。
$wrap = Join-Path $Build "clang-lld.cmd"
@"
@echo off
setlocal enabledelayedexpansion
set "EXTRA="
echo %* | findstr /C:"-c" >nul
if errorlevel 1 set "EXTRA=-fuse-ld=lld-link"
"$llvm\clang.exe" %EXTRA% %*
"@ | Set-Content -Path $wrap -Encoding ascii
$env:CC = $wrap
$env:CGO_LDFLAGS = "-L$Build"
Push-Location $Root
go build -o (Join-Path $Build "hermes.exe") ./cmd/hermes
if ($LASTEXITCODE -ne 0) { Pop-Location; throw "go build 失败" }
Pop-Location

# Windows 不构建 libhermesrt.a：cgo 探针需要可重定位链接（-r），而 lld-link
# 不支持；能满足它的只有 MSVC link.exe 或 MinGW 的 GNU ld，两者都在红线之外。
# 因此 Windows 包只提供 CLI，C 嵌入能力留在 macOS / Linux。
Say "skip runtime archive (Windows: C embedding stays on macOS/Linux)"

# 产物验证：PE 导入表里不能出现 GNU 运行库（libgcc / libstdc++）。
$exe = Join-Path $Build "hermes.exe"
$imports = (& llvm-objdump -p $exe | Select-String -Pattern "DLL Name:") -join " "
foreach ($banned in @("libgcc", "libstdc++", "libwinpthread")) {
  if ($imports -match [regex]::Escape($banned)) { throw "产物链接了 GNU 运行库 $banned（红线）" }
}
Say "artifact check: no GNU runtime imported"

Say "build ok"
& (Join-Path $Build "hermes.exe") version
