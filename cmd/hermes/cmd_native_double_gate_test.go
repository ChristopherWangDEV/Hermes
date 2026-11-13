package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"hermes/internal/ir"
)

// cmd_native_double_gate_test.go —— C1 regression 的双闸门（v2.0.0 收口口径）。
//
// double 已是 AOT 子集成员：C 后端 nativeCTypes 含 double，`call_native.ret`
// 可以合法写 {"k":"double"}。`TestDoubleCallNativeCompilesAndRuns` 用
// `extern double dsqrt(int x)` 走 hermes ir -> ir.Load -> hermes compile
// --link-static -> 真跑，断言 stdout 的 value 恰为 13.5，证明双精度返回类型
// 真编译真运行（不是被 "not in the AOT subset" 拒绝，也不是被静默降级成整数）。
//
// 但 `call_tool` 的 T 必须是 JSON 结构类型，double 是标量：共享 helper
// resolveRetType 不许把 double 外溢到 call_tool。否则会静默产出
// call_tool.ret={"k":"double"}，而 emitExpr 不读 n.Ret，前端、装配、
// ir.Load、C 后端没有一层会拦。`TestDoubleToolCallRetIsRejected` 守住这条：
// `tool.call<double>(...)` 必须在 hermes ir 阶段非零退出并报装配层诊断。
//
// 两条测试从两侧把闸门钉死；测试代码本身不随文档口径变化。

const toolDoubleSrc = `#link <std/net>
export workflow w() entry -> string {
    auto r = tool.call<double>(url = "https://x/get", method = "GET");
    return "ok";
}
`

const nativeDoubleSrc = `#link native "math" from "math_tool.h"
extern double dsqrt(int x);
export workflow probe() entry -> double {
    return dsqrt(6);
}
`

// TestDoubleToolCallRetIsRejected 锁住「共享 helper 不许外溢」：
// tool.call<double> 必须在 hermes ir 阶段非零退出，且报原有的装配层诊断。
func TestDoubleToolCallRetIsRejected(t *testing.T) {
	code, msg, outPath := compileInlineSource(t, toolDoubleSrc)
	if code == 0 {
		t.Fatalf("tool.call<double> 必须被装配层拒绝，实际 hermes ir rc=0（会静默产出 schema-invalid IR）：\n%s", msg)
	}
	want := `ir.AssembleUnit: return type "double" is undeclared (typo?)`
	if !strings.Contains(msg, want) {
		t.Errorf("诊断应含 %q，实际：\n%s", want, msg)
	}
	if _, err := os.Stat(outPath); !os.IsNotExist(err) {
		t.Errorf("拒绝后不应写出 IR 文件，os.Stat = %v", err)
	}
	t.Logf("tool.call<double> 修复后诊断原文 = %s", strings.TrimSpace(msg))
}

// TestDoubleCallNativeCompilesAndRuns 锁住「call_native 返回 double 已进 AOT 子集」：
// hermes ir rc=0 证明前端与装配层没拦；ir.Load 0 诊断证明加载层没拦；
// hermes compile 真编译，并用 --link-static 链进真实 .a，跑出 double 结果。
// 硬证据是 13.5：它只能来自 dsqrt(6) 的 2.25*6，解释器 / 常量折叠都伪造不出。
func TestDoubleCallNativeCompilesAndRuns(t *testing.T) {
	code, msg, outPath := compileInlineSource(t, nativeDoubleSrc)
	if code != 0 {
		t.Fatalf("call_native 的 double 必须在装配层放行，实际 hermes ir rc=%d：\n%s", code, msg)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	doc, ds := ir.Load(data)
	if len(ds) != 0 {
		t.Fatalf("产物应通过加载期校验，实际 %d 条诊断：%v", len(ds), ds)
	}

	// 原始 IR 字节里的形状：ret={"k":"double"}、arg_types=[{"k":"int"}]。
	var raw struct {
		Workflows []struct {
			Nodes []map[string]any `json:"nodes"`
		} `json:"workflows"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("IR 不是合法 JSON: %v", err)
	}
	found := 0
	for _, wf := range raw.Workflows {
		for _, n := range wf.Nodes {
			if n["k"] != "call_native" {
				continue
			}
			found++
			ret, _ := n["ret"].(map[string]any)
			if ret["k"] != "double" {
				t.Errorf("call_native.ret 应为 {k:double}，实际 %v", ret)
			}
			ats, _ := n["arg_types"].([]any)
			if len(ats) != 1 {
				t.Fatalf("arg_types 应有 1 项，实际 %v", ats)
			}
			at, _ := ats[0].(map[string]any)
			if at["k"] != "int" {
				t.Errorf("arg_types[0] 应为 {k:int}，实际 %v", at)
			}
		}
	}
	if found != 1 {
		t.Fatalf("应恰好一个 call_native 节点，实际 %d", found)
	}
	t.Logf("call_native double 产物: ir_version=%d, ir.Load diagnostics=%d, ret={k:double}, arg_types=[{k:int}]",
		doc.IRVersion, len(ds))

	// v2.0.0：C 后端 nativeCTypes 已含 double，compile 必须成功，且产物跑出
	// 一个精确的 double 值（不是被 AOT 子集拒绝、也不是被静默降级成整数）。
	dir := t.TempDir()
	src := filepath.Join(dir, "probe.hermes")
	if err := os.WriteFile(src, []byte(nativeDoubleSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	lib := writeDoubleNativeMathLib(t, dir)
	prog := filepath.Join(dir, "prog")
	code2, msg2 := runCompileCaptured(t, src, "-o", prog, "--link-static", lib)
	if code2 != 0 {
		t.Fatalf("call_native 返回 double 现在应能真编译，实际 compile rc=%d：\n%s", code2, msg2)
	}
	out, runErr := exec.Command(prog).Output()
	if runErr != nil {
		t.Fatalf("运行 AOT 产物失败: %v\n%s", runErr, out)
	}
	var env struct {
		Value    *float64 `json:"value"`
		Steps    *int     `json:"steps"`
		Workflow string   `json:"workflow"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("stdout 不是完整 JSON 信封: %v，原文 %q", err, out)
	}
	if env.Value == nil || *env.Value != 13.5 {
		t.Fatalf("value 应为真 .a 里 2.25*6=13.5，实际 %v；原文 %q", env.Value, out)
	}
	if !strings.Contains(string(out), `"value":13.5`) {
		t.Errorf("stdout 应含 \"value\":13.5（最短往返，不是 13.500000），实际 %q", out)
	}
	t.Logf("call_native double 真编译输出 = %s", strings.TrimSpace(string(out)))
}

// writeDoubleNativeMathLib 编一份只有 dsqrt 的真实静态库（纯 C ABI，无三方依赖）。
// 形参是 long：C 后端把 IR 的 int 映射成 long，extern 原型也是 long。
func writeDoubleNativeMathLib(t *testing.T, dir string) string {
	t.Helper()
	if _, err := exec.LookPath("clang"); err != nil {
		t.Skip("没有 clang")
	}
	if _, err := exec.LookPath("ar"); err != nil {
		t.Skip("没有 ar")
	}
	cfile := filepath.Join(dir, "math_double.c")
	if err := os.WriteFile(cfile, []byte("double dsqrt(long x) { return 2.25 * (double)x; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	obj := filepath.Join(dir, "math_double.o")
	if o, err := exec.Command("clang", "-std=c99", "-O2", "-c", cfile, "-o", obj).CombinedOutput(); err != nil {
		t.Fatalf("clang -c math_double.c: %v\n%s", err, o)
	}
	lib := filepath.Join(dir, "libmath_double.a")
	if o, err := exec.Command("ar", "rcs", lib, obj).CombinedOutput(); err != nil {
		t.Fatalf("ar rcs libmath_double.a: %v\n%s", err, o)
	}
	return lib
}
