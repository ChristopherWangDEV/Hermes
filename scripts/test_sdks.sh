#!/usr/bin/env bash
# 跑各语言 SDK 的测试。**没有对应工具链的语言跳过并明确说"跳过"** ——
# 静默跳过会让人以为"都验过了"。
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail=0
ran=0
skipped=()

run() { # run <名字> <目录> <命令...>
  local name="$1" dir="$2"; shift 2
  printf '==> %s\n' "$name"
  if ( cd "$dir" && "$@" ); then
    ran=$((ran+1))
  else
    fail=$((fail+1))
  fi
}

have() { command -v "$1" >/dev/null 2>&1; }

if have python3; then
  run "Python" "$ROOT/packages/python" python3 -m unittest discover -s tests -q
else skipped+=("Python（无 python3）"); fi

if have node; then
  if [ -f "$ROOT/build/hermes.node" ]; then
    run "JavaScript（含原生绑定）" "$ROOT/packages/javascript" node --test
  else
    run "JavaScript（仅 HTTP，缺原生插件）" "$ROOT/packages/javascript" node --test
  fi
  if [ -d "$ROOT/packages/typescript/node_modules" ]; then
    run "TypeScript build" "$ROOT/packages/typescript" ./node_modules/.bin/tsc -p tsconfig.json
    run "TypeScript test"  "$ROOT/packages/typescript" node --test
  else
    skipped+=("TypeScript（工作区没有 tsc；自行准备编译器后重跑）")
  fi
else skipped+=("JavaScript/TypeScript（无 node）"); fi

if [ -x "$HOME/.local/go/bin/go" ]; then
  run "Go" "$ROOT/packages/go" "$HOME/.local/go/bin/go" test ./...
elif have go; then
  run "Go" "$ROOT/packages/go" go test ./...
else skipped+=("Go（无 go）"); fi

if have cargo; then
  run "Rust" "$ROOT/packages/rust" cargo test --offline -q
else skipped+=("Rust（无 cargo）"); fi

if have javac && have java; then
  rm -rf "$ROOT/packages/java/out"
  run "Java" "$ROOT/packages/java" bash -c \
    'javac -d out src/dev/hermes/*.java test/*.java && java -cp out HermesClientTest && java --enable-native-access=ALL-UNNAMED -cp out NativeRuntimeTest'
else skipped+=("Java（无 javac/java）"); fi

if have dotnet; then
  run "C#" "$ROOT/packages/csharp.tests" dotnet run -v q --nologo
else skipped+=("C#（无 dotnet）"); fi

if have kotlinc && have java; then
  # 两套测试都跑：原生绑定（FFM）+ HTTP 客户端。
  run "Kotlin 原生绑定" "$ROOT/packages/kotlin" bash -c \
    'mkdir -p out && kotlinc src/dev/hermes/NativeRuntime.kt test/NativeRuntimeTest.kt -include-runtime -d out/native.jar \
      && java --enable-native-access=ALL-UNNAMED -cp out/native.jar NativeRuntimeTestKt'
  run "Kotlin HTTP" "$ROOT/packages/kotlin" bash -c \
    'mkdir -p out && kotlinc src/dev/hermes/HermesClient.kt test/HermesClientTest.kt -include-runtime -d out/http.jar \
      && java -cp out/http.jar HermesClientTestKt'
else skipped+=("Kotlin（无 kotlinc 或 java）"); fi

echo
echo "==> SDK 测试：$ran 个通过，$fail 个失败"
if [ ${#skipped[@]} -gt 0 ]; then
  echo "==> 跳过："
  for s in "${skipped[@]}"; do echo "      - $s"; done
fi
[ "$fail" -eq 0 ]
