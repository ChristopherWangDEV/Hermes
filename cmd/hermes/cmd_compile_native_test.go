package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"hermes/internal/backend"
	"hermes/internal/ir"
)

// cmd_compile_native_test.go —— call_native 的 AOT 最后一公里验收。
//
// 之前 call_native 只能产出 IR，C 后端 emitExpr 没有这个 case，compile 一律
// 报 "expression node \"call_native\" is not in the S0 subset"。这里按真实
// `hermes compile` 路径证明：
//   ① 形参静态类型表 arg_types 落进 IR；
//   ② C 后端编译期写死 extern 原型（不是 dlsym / 运行期符号表）；
//   ③ 符号由 clang/ld 在**链接期**解析，跑出真 .a 里的结果；
//   ④ 首版类型子集外的类型一律明确报错，不静默、不假支持。
//
// 硬证据是 42：它只能来自 libmath.a 里 `long imul(long,long)` 的乘法，
// 任何 IR JSON 桩、常量折叠或解释器都伪造不出这个数。

// nativeProbeSrc 与 cmd_ffi_e2e_test.go:21-26 同形。math_tool.h 不必存在：
// `#link native ... from ...` 是元数据，不触发头文件读取。
const nativeProbeSrc = `#link native "math" from "math_tool.h"
extern int imul(int a, int b);
export workflow probe() entry -> int {
    return imul(6, 7);
}
`

// runCompileCaptured 走真实 CLI 并把 stderr 收回来。compile 的成功文案与
// clang 的失败输出都写在 stderr，只看退出码拿不到"为什么失败"。
// 与 cmd_ir_test.go::compileInlineSource 同一套文件重定向（本包无 t.Parallel，
// 替换 os.Stderr 不会和其他测试打架）。
func runCompileCaptured(t *testing.T, args ...string) (int, string) {
	t.Helper()
	dir := t.TempDir()
	stderrPath := filepath.Join(dir, "stderr.txt")
	ef, err := os.Create(stderrPath)
	if err != nil {
		t.Fatal(err)
	}
	code := func() int {
		old := os.Stderr
		os.Stderr = ef
		defer func() { os.Stderr = old }()
		return run(append([]string{"compile"}, args...))
	}()
	if err := ef.Close(); err != nil {
		t.Fatal(err)
	}
	msg, err := os.ReadFile(stderrPath)
	if err != nil {
		t.Fatal(err)
	}
	return code, string(msg)
}

// writeNativeMathLib 用 clang -std=c99 编出真实静态库 libmath.a。
// 只用 clang / ar（Xcode cctools），不引入 GNU 工具链。
func writeNativeMathLib(t *testing.T, dir string) string {
	t.Helper()
	if _, err := exec.LookPath("clang"); err != nil {
		t.Skip("没有 clang")
	}
	if _, err := exec.LookPath("ar"); err != nil {
		t.Skip("没有 ar")
	}
	cfile := filepath.Join(dir, "math_tool.c")
	if err := os.WriteFile(cfile, []byte("long imul(long a, long b) { return a * b; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	obj := filepath.Join(dir, "math_tool.o")
	if out, err := exec.Command("clang", "-std=c99", "-O2", "-c", cfile, "-o", obj).CombinedOutput(); err != nil {
		t.Fatalf("clang -c math_tool.c: %v\n%s", err, out)
	}
	lib := filepath.Join(dir, "libmath.a")
	if out, err := exec.Command("ar", "rcs", lib, obj).CombinedOutput(); err != nil {
		t.Fatalf("ar rcs libmath.a: %v\n%s", err, out)
	}
	return lib
}

// writeNativeProbe 把 nativeProbeSrc 落到磁盘，返回 .hermes 路径。
func writeNativeProbe(t *testing.T, dir string) string {
	t.Helper()
	src := filepath.Join(dir, "case.hermes")
	if err := os.WriteFile(src, []byte(nativeProbeSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	return src
}

// writeSource 写一份临时 .hermes，给类型子集拒绝的用例用。
func writeSource(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestCompileNativeRunsRealStaticLib 是步骤 1~4：
// 建 .a → 写 .hermes → compile --link-static → 跑产物 → 断言 42 与完整信封。
func TestCompileNativeRunsRealStaticLib(t *testing.T) {
	dir := t.TempDir()
	lib := writeNativeMathLib(t, dir)
	src := writeNativeProbe(t, dir)
	prog := filepath.Join(dir, "prog")

	code, msg := runCompileCaptured(t, src, "-o", prog, "--link-static", lib)
	if code != 0 {
		t.Fatalf("带 --link-static 的 compile 应成功，实际退出 %d：\n%s", code, msg)
	}
	if _, err := os.Stat(prog); err != nil {
		t.Fatalf("产物不存在: %v", err)
	}

	got, err := exec.Command(prog).Output()
	if err != nil {
		t.Fatalf("运行 AOT 产物失败: %v", err)
	}
	t.Logf("AOT 可执行文件 stdout 原文 = %q", string(got))

	// 信封完整：必须是合法 JSON，且 steps / value / workflow 三键齐全。
	var env struct {
		Steps    *int   `json:"steps"`
		Value    *int   `json:"value"`
		Workflow string `json:"workflow"`
	}
	if err := json.Unmarshal(got, &env); err != nil {
		t.Fatalf("stdout 不是完整 JSON 信封: %v，原文 %q", err, got)
	}
	if env.Steps == nil || env.Value == nil {
		t.Fatalf("信封缺 steps / value: %q", got)
	}
	if !strings.Contains(string(got), `"value":42`) {
		t.Errorf("stdout 应含 \"value\":42，实际 %q", got)
	}
	if *env.Value != 42 {
		t.Errorf("value 必须是真 .a 里 6*7=42，实际 %d", *env.Value)
	}
	if env.Workflow != "probe" {
		t.Errorf("workflow 应为 probe，实际 %q", env.Workflow)
	}
}

// TestCompileNativeRequiresLinkTimeSymbol 是步骤 5 的反向对照：
// 不给 --link-static，clang/ld 必须报未解析的 _imul —— 证明符号是**链接期**
// 静态解析，不是运行期查表、更不是后端自己塞了个桩函数。
func TestCompileNativeRequiresLinkTimeSymbol(t *testing.T) {
	dir := t.TempDir()
	src := writeNativeProbe(t, dir)
	prog := filepath.Join(dir, "prog_nolib")

	code, msg := runCompileCaptured(t, src, "-o", prog)
	if code == 0 {
		t.Fatalf("不带 --link-static 必须链接失败（符号无处解析），实际成功：\n%s", msg)
	}
	low := strings.ToLower(msg)
	if !strings.Contains(low, "imul") {
		t.Errorf("链接错误应点名缺失符号 imul，实际：\n%s", msg)
	}
	if !strings.Contains(low, "undefined") && !strings.Contains(low, "unresolved") {
		t.Errorf("链接错误应含 undefined / unresolved，实际：\n%s", msg)
	}
	if _, err := os.Stat(prog); !os.IsNotExist(err) {
		t.Errorf("链接失败后不应留下可执行文件，os.Stat = %v", err)
	}
}

// TestCompileNativeGeneratedCIsStatic 是步骤 6 + arg_types 的真实例子：
// 直接对 backend.For("c").Generate 的产物做静态断言。
func TestCompileNativeGeneratedCIsStatic(t *testing.T) {
	dir := t.TempDir()
	src := writeNativeProbe(t, dir)

	doc, err := buildIR(src, []byte(nativeProbeSrc))
	if err != nil {
		t.Fatalf("buildIR: %v", err)
	}
	if doc.IRVersion != 2 {
		t.Errorf("ir_version 必须仍为 2，实际 %d", doc.IRVersion)
	}

	// arg_types 是这一轮新增的编译期静态形参类型表，必须与 args 一一对应。
	found := 0
	for wi := range doc.Workflows {
		for ni := range doc.Workflows[wi].Nodes {
			n := &doc.Workflows[wi].Nodes[ni]
			if n.K != "call_native" {
				continue
			}
			found++
			var argIDs []int
			if err := json.Unmarshal(n.Args, &argIDs); err != nil {
				t.Fatalf("call_native.args 不是节点 id 数组: %v", err)
			}
			if len(n.ArgTypes) != len(argIDs) {
				t.Fatalf("arg_types 与 args 必须一一对应，实际 %d vs %d",
					len(n.ArgTypes), len(argIDs))
			}
			types := make([]string, 0, len(n.ArgTypes))
			for i := range n.ArgTypes {
				types = append(types, n.ArgTypes[i].K)
			}
			t.Logf("call_native 真实例子: fn=%s args=%v arg_types=%v ret.k=%s",
				*n.Fn, argIDs, types, n.Ret.K)
			for _, k := range types {
				if k != "int" {
					t.Errorf("Form B 的形参类型应都是 int，实际 %v", types)
				}
			}
		}
	}
	if found != 1 {
		t.Fatalf("应恰好一个 call_native 节点，实际 %d", found)
	}

	be, ok := backend.For("c")
	if !ok {
		t.Fatal("找不到 c 后端")
	}
	csrc, err := be.Generate(doc)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	c := string(csrc)

	// 编译期写死的静态原型 + 编译期写死的类型转换。
	if !strings.Contains(c, "extern long imul(long, long);") {
		t.Errorf("生成的 C 应含编译期静态原型 extern long imul(long, long);")
	}
	if !strings.Contains(c, "imul(hm_as_int(") {
		t.Errorf("生成的 C 应含 imul(hm_as_int(...)) 的编译期转换调用")
	}
	for _, banned := range []string{"dlsym", "dlopen", "hermes_rt_run", "hermes_ir_bytes"} {
		if strings.Contains(c, banned) {
			t.Errorf("零解释器：生成的 C 不应含 %q", banned)
		}
	}
	for _, line := range strings.Split(c, "\n") {
		if strings.Contains(line, "imul(hm_as_int(") {
			t.Logf("生成的 C 里 call_native 那一行原文 = %s", strings.TrimSpace(line))
		}
	}
}

// TestCompileNativeRejectsUnsupportedTypes 是步骤 7：AOT 子集外一律明确报错。
//
// v2.0.0 起子集是 int / bool / string / **double**：double 的位模式常量、
// 引擎 float64、C 后端 Val.d 与 hm_as_double / hm_double 已同源落地，所以
// 对 double 的拒绝断言换成了「真编译真跑出精确 double 值」。其余
// cost / float(旧标签) / struct / array / httpresult / json / vector 仍全部
// 必须报 "not in the AOT subset"，不许静默降级、不许假支持 —— 一个都没删。
func TestCompileNativeRejectsUnsupportedTypes(t *testing.T) {
	dir := t.TempDir()
	probePath := writeNativeProbe(t, dir)

	// ① 真实 compile：double 返回（v2.0.0 已进 AOT 子集）。形参用 int 是为了
	// 避开与 AOT 无关的前端限制（extern 实参不允许隐式转换 E4030）。
	// 被检验的是**返回类型 double 这条新契约**：必须真编译、链进真实 .a、
	// 跑出一个精确 double 值，而不是 "not in the AOT subset" 或静默降级成 int。
	doubleSrc := `#link native "math" from "math_tool.h"
extern double dsqrt(int x);
export workflow probe() entry -> double {
    return dsqrt(6);
}
`
	if _, err := exec.LookPath("clang"); err == nil {
		lib := writeDoubleNativeMathLib(t, dir)
		prog := filepath.Join(dir, "p_dbl")
		code, msg := runCompileCaptured(t, writeSource(t, dir, "dbl.hermes", doubleSrc),
			"-o", prog, "--link-static", lib)
		if code != 0 {
			t.Fatalf("double 现在应能真编译（已进 AOT 子集），实际退出 %d：\n%s", code, msg)
		}
		if strings.Contains(msg, "not in the AOT subset") {
			t.Errorf("double 不该再被 AOT 子集拒绝：\n%s", msg)
		}
		out, runErr := exec.Command(prog).Output()
		if runErr != nil {
			t.Fatalf("运行 double AOT 产物失败: %v\n%s", runErr, out)
		}
		if !strings.Contains(string(out), `"value":13.5`) {
			t.Errorf("double 产物应跑出 13.5（2.25*6 的最短往返），实际 %q", out)
		}
		t.Logf("double 真编译输出 = %s", strings.TrimSpace(string(out)))
	} else {
		t.Skip("没有 clang，跳过 double 真编译")
	}

	// ② 真实 compile：`extern double dsqrt(double x);` 传 int 字面量。
	// 前端 E4030（语言无隐式转换，E4006 的"无隐式提升"同源）会**先于**
	// AOT 后端拦下，诊断文案是 "expects double"。这一条只证明"不静默、
	// 不 panic"；double 已进 AOT 子集的正向证明由 ① 与 ④ 覆盖。
	doubleParamSrc := `#link native "math" from "math_tool.h"
extern double dsqrt(double x);
export workflow probe() entry -> double {
    return dsqrt(6);
}
`
	code, msg := runCompileCaptured(t, writeSource(t, dir, "dbl_param.hermes", doubleParamSrc),
		"-o", filepath.Join(dir, "p_dbl_param"))
	if code == 0 {
		t.Fatalf("int 字面量传给 double 形参必须被 E4030 拒绝，实际编译成功")
	}
	if !strings.Contains(msg, "E4030") {
		t.Errorf("应给出明确的 E4030 诊断，实际：\n%s", msg)
	}
	t.Logf("double 形参原样源码的拒绝原文 = %s", strings.TrimSpace(msg))

	// ③ 真实 compile：Form A struct 返回（多文件 fixture）。
	formA := "testdata/ffi_multi/form_a/main.hermes"
	if _, err := os.Stat(formA); err == nil {
		code, msg = runCompileCaptured(t, formA, "-o", filepath.Join(dir, "p_form_a"))
		if code == 0 {
			t.Fatalf("struct 返回必须被 AOT 子集拒绝，实际编译成功")
		}
		if !strings.Contains(msg, `call_native type "struct" is not in the AOT subset`) {
			t.Errorf("struct 应明确报 not in the AOT subset，实际：\n%s", msg)
		}
		t.Logf("Form A struct 拒绝原文 = %s", strings.TrimSpace(msg))
	} else {
		t.Errorf("Form A fixture 缺失: %v", err)
	}

	// ④ 直接对后端 Generate 做类型矩阵：把 ret / arg_types 逐个换成子集外
	// 类型，每一个都必须由 nativeCTypeOf 明确拒绝（不许 panic、不许静默）。
	// "float" 是 v2.0.0 之前的旧类型名标签，留在矩阵里当**纵深防线**：
	// 手工篡改 IR 把旧标签塞回来时，后端也必须明确拒绝，不能认成 double。
	be, ok := backend.For("c")
	if !ok {
		t.Fatal("找不到 c 后端")
	}
	badTypes := []string{"cost", "float", "struct", "array", "httpresult", "json", "vector"}
	for _, bad := range badTypes {
		t.Run("ret_"+bad, func(t *testing.T) {
			d, err := buildIR(probePath, []byte(nativeProbeSrc))
			if err != nil {
				t.Fatalf("buildIR: %v", err)
			}
			patched := false
			for wi := range d.Workflows {
				for ni := range d.Workflows[wi].Nodes {
					n := &d.Workflows[wi].Nodes[ni]
					if n.K == "call_native" {
						n.Ret = &ir.TypeExpr{K: bad}
						patched = true
					}
				}
			}
			if !patched {
				t.Fatal("没有 call_native 节点可改")
			}
			_, err = be.Generate(d)
			if err == nil {
				t.Fatalf("ret=%q 应被拒绝，实际生成成功", bad)
			}
			if !strings.Contains(err.Error(), "not in the AOT subset") {
				t.Errorf("ret=%q 应明确报 not in the AOT subset，实际 %v", bad, err)
			}
		})
		t.Run("arg_"+bad, func(t *testing.T) {
			d, err := buildIR(probePath, []byte(nativeProbeSrc))
			if err != nil {
				t.Fatalf("buildIR: %v", err)
			}
			patched := false
			for wi := range d.Workflows {
				for ni := range d.Workflows[wi].Nodes {
					n := &d.Workflows[wi].Nodes[ni]
					if n.K == "call_native" && len(n.ArgTypes) > 0 {
						n.ArgTypes[0].K = bad
						patched = true
					}
				}
			}
			if !patched {
				t.Fatal("没有 call_native.arg_types 可改")
			}
			_, err = be.Generate(d)
			if err == nil {
				t.Fatalf("arg_types[0]=%q 应被拒绝，实际生成成功", bad)
			}
			if !strings.Contains(err.Error(), "not in the AOT subset") {
				t.Errorf("arg_types[0]=%q 应明确报 not in the AOT subset，实际 %v", bad, err)
			}
		})
	}
}

// TestCompileNativeBoolAndStringMapping 正向覆盖子集里另外两种映射：
// bool → int / hm_truthy / hm_bool，string → const char* / hm_as_str / hm_str。
// 只测 int 会让另两行映射表形同虚设（改错了也没有测试变红）。
func TestCompileNativeBoolAndStringMapping(t *testing.T) {
	dir := t.TempDir()
	src := writeNativeProbe(t, dir)
	be, ok := backend.For("c")
	if !ok {
		t.Fatal("找不到 c 后端")
	}
	doc, err := buildIR(src, []byte(nativeProbeSrc))
	if err != nil {
		t.Fatalf("buildIR: %v", err)
	}
	for wi := range doc.Workflows {
		for ni := range doc.Workflows[wi].Nodes {
			n := &doc.Workflows[wi].Nodes[ni]
			if n.K != "call_native" {
				continue
			}
			n.Ret = &ir.TypeExpr{K: "string"}
			n.ArgTypes[0].K = "bool"
			n.ArgTypes[1].K = "string"
		}
	}
	csrc, err := be.Generate(doc)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	c := string(csrc)
	for _, want := range []string{
		"extern const char* imul(int, const char*);",
		"hm_str(imul(hm_truthy(",
		"hm_as_str(",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("生成 C 应含 %q", want)
		}
	}
	for _, line := range strings.Split(c, "\n") {
		if strings.Contains(line, "hm_str(imul(") {
			t.Logf("bool/string 映射生成的 C 行 = %s", strings.TrimSpace(line))
		}
	}
}
