package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"hermes/internal/ir"
	"hermes/internal/native"
)

// cmd_float_test.go —— v2.0.0 浮点落地的 Go 侧可执行契约。
//
// 定案（KNOWN-GAPS.md「决定记录：浮点类型 = double」）：语言里唯一的浮点
// 类型名是 `double`（IEEE-754 binary64），没有 32 位 `float`。IR 里的浮点
// **常量**用位模式对象，绝不放 JSON 数字：
//
//	{"k":"double","bits":"3ff8000000000000"}
//
// `bits` 是 16 位**小写** hex 的 JSON **字符串**（big-endian 数值十六进制，
// 即 math.Float64bits(x) 的 uint64 用 %016x 格式化），schema 用
// `^[0-9a-f]{16}$` 钉死宽度与端序。字符串形态是刻意的：V-7
// （internal/ir/load.go::scanFloats）只查 json.Number 里的 .eE，
// internal/canon/json.go::FromAny 对带小数点的 json.Number 一律 ErrFloat，
// 签名链要求同一份值只有一种字节 —— 放进 JSON 数字会让三件事同时崩。
//
// E4029 不再是「所有浮点字面量被拒」。三道扫描器
// （check_type.cpp::scan_float_ / codegen.cpp 的 gen_expr 硬拒 /
// codegen.cpp 的 workflow 参数默认值折叠硬拒）**结构原样保留**，判据仍只
// 盯旧的 "float" 标签：它们是「旧标签万一漏进来」的纵深防线，防 C ABI
// 直调绕过诊断拿走坏 IR。正常路径的标签是 "double"，不命中。
// 手工把 AST 标签改回 "float" 后的拒绝由
// tests/compiler/parser_test.cpp::test_e4029_float_literal 覆盖；本文件
// 守的是 Go/CLI 侧：double 可编、位模式正确、C ABI 直调拿到**合法**位模式 IR。

// doubleBitsPattern 与 schemas/ir.schema.json 的 bits.pattern 同源。
// 它是端序/宽度唯一机器可执行契约，测试里再钉一次，防止 schema 与产物分叉。
var doubleBitsPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// collectBitPatterns 递归收集一段 IR JSON 里所有位模式，并顺带数出
// t=="double" 的 lit 节点个数。收集时把宽度/端序契约一起验掉。
func collectBitPatterns(t *testing.T, raw []byte) ([]string, int) {
	t.Helper()
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("IR 不是合法 JSON: %v\n%.400s", err, raw)
	}
	var bits []string
	doubleLits := 0
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if tag, _ := x["t"].(string); tag == "double" {
				doubleLits++
			}
			if b, ok := x["bits"].(string); ok {
				bits = append(bits, b)
			}
			for _, child := range x {
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	walk(root)
	for _, b := range bits {
		if !doubleBitsPattern.MatchString(b) {
			t.Errorf("位模式 %q 不满足 ^[0-9a-f]{16}$（宽度/端序契约）", b)
		}
	}
	return bits, doubleLits
}

// assertNoFloatJSONNumbers 用与 V-7 同源的判据（UseNumber 后查 .eE）确认
// IR 字节里没有浮点 JSON 数字。C ABI 直调拿到的是 workflow 对象、不是完整
// unit，不能直接走 ir.Load，所以这条判据在 C ABI 用例里补上 P-1 的保证。
func assertNoFloatJSONNumbers(t *testing.T, raw []byte) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		t.Fatalf("IR 不是合法 JSON: %v\n%.400s", err, raw)
	}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case json.Number:
			if strings.ContainsAny(x.String(), ".eE") {
				t.Errorf("IR 字节里出现浮点 JSON 数字 %q（违反 P-1）", x.String())
			}
		case map[string]any:
			for _, child := range x {
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	walk(root)
}

// assertBitPatterns 断言 IR 里恰好出现 want 列出的位模式、每个都满足 pattern、
// 且没有把类型名当值写进 lit 节点（旧形态静默损坏的唯一症状）。
func assertBitPatterns(t *testing.T, raw []byte, want ...string) {
	t.Helper()
	got, doubleLits := collectBitPatterns(t, raw)
	if len(got) != len(want) {
		t.Fatalf("位模式个数：期望 %d，实际 %d（%v）\n%.500s", len(want), len(got), got, raw)
	}
	for _, w := range want {
		if !doubleBitsPattern.MatchString(w) {
			t.Fatalf("测试用例自身写错了位模式 %q", w)
		}
		found := false
		for _, g := range got {
			if g == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("缺位模式 %q，实际 %v", w, got)
		}
	}
	if len(want) > 0 && doubleLits == 0 {
		t.Errorf("IR 里没有 t==\"double\" 的 lit 节点：%.500s", raw)
	}
	// 修复前的确切症状：类型名被当成值。任何一条合法用例都不该出现。
	// "v":"float" 是旧标签兜底，"v":"double" 是同一 bug 换个名字。
	if strings.Contains(string(raw), `"v":"float"`) ||
		strings.Contains(string(raw), `"v":"double"`) {
		t.Fatalf("IR 里出现了类型名当值的坏 lit：%.500s", raw)
	}
	assertNoFloatJSONNumbers(t, raw)
}

// TestDoubleLiteralCompilesToBitPattern 是 E4029 语义变化的可执行形式：
// v2.0.0 起 `double` 字面量必须编过、不报 E4029，且每个词法形态都编出
// 正确的 IEEE-754 binary64 位模式（而不是十进制文本或 JSON 数字）。
// 覆盖 lexer 的三种形态（D.D / D e D / D.D e D）、let 初始化、同一表达式
// 两个字面量、parallel 的 return [..]（subs 路径）、以及 0.1+0.2 的
// shortest-round-trip 规格钉子。
func TestDoubleLiteralCompilesToBitPattern(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{
			name: "return 值位 1.5",
			src: `export workflow w() entry -> double {
    return 1.5;
}
`,
			want: []string{"3ff8000000000000"},
		},
		{
			name: "指数形式 D e D（没有小数点）1e3",
			src: `export workflow w() entry -> double {
    return 1e3;
}
`,
			want: []string{"408f400000000000"},
		},
		{
			name: "小数 + 指数形式 1.5e2",
			src: `export workflow w() entry -> double {
    return 1.5e2;
}
`,
			want: []string{"4062c00000000000"},
		},
		{
			name: "let 初始化位",
			src: `export workflow w() entry -> string {
    auto x = 1.5;
    return "ok";
}
`,
			want: []string{"3ff8000000000000"},
		},
		{
			name: "同一表达式里两个字面量",
			src: `export workflow w() entry -> double {
    return 1.5 * 2.0;
}
`,
			want: []string{"3ff8000000000000", "4000000000000000"},
		},
		{
			name: "parallel 的 return [..] 走 subs",
			// parse_parallel_return 用 to_subs 把表达式列表落成定长数组，
			// codegen 再逐个送进 gen_expr。位模式必须覆盖 subs，漏掉一个
			// 就会退回字符串兜底。
			src: `export workflow par() entry -> string {
    auto [a, b] = parallel {
        auto x = "one";
        auto y = "two";
        return [1.5, 2.5];
    };
    return "done";
}
`,
			want: []string{"3ff8000000000000", "4004000000000000"},
		},
		{
			name: "0.1 + 0.2 的两个位模式（shortest round-trip 的规格钉子）",
			src: `export workflow w() entry -> double {
    return 0.1 + 0.2;
}
`,
			want: []string{"3fb999999999999a", "3fc999999999999a"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, msg, outPath := compileInlineSource(t, c.src)
			if code != 0 {
				t.Fatalf("double 字面量必须能编过（v2.0.0），实际退出码 %d：\n%s", code, msg)
			}
			if strings.Contains(msg, "E4029") {
				t.Fatalf("double 字面量不该再报 E4029（E4029 只盯旧 float 标签）：\n%s", msg)
			}
			data, err := os.ReadFile(outPath)
			if err != nil {
				t.Fatal(err)
			}
			// 位模式的 bits 是 ASCII 字符串，V-7 / canonical / 签名都不该误伤。
			if _, ds := ir.Load(data); len(ds) != 0 {
				t.Fatalf("位模式 IR 不该有 ir.Load 诊断：%v\n%.500s", ds, data)
			}
			assertBitPatterns(t, data, c.want...)
		})
	}
}

// TestDoubleNonLiteralUsesStillCompile 守「合法用法不误伤」：E4029 撤掉硬拒
// 之后，非浮点字面量的写法仍要能编过，且 IR 里不能冒出旧形态的坏 lit。
// `json.as_float` 是 API 名，保留原名；cast 节点的 to 已改成 "double"。
func TestDoubleNonLiteralUsesStillCompile(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "int 字面量",
			src: `export workflow w() entry -> int {
    return 42;
}
`,
		},
		{
			name: "字符串里的小数点不是浮点字面量",
			src: `export workflow w() entry -> string {
    return "1.5";
}
`,
		},
		{
			name: "cost 金额字面量带点也不受影响",
			src: `export workflow w() entry -> cost {
    return $1.50;
}
`,
		},
		{
			name: "double 类型 + json.as_float 边界窄化",
			src: `export workflow f(input: json) entry -> double {
    return json.as_float(input.total);
}
`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, msg, outPath := compileInlineSource(t, c.src)
			if code != 0 {
				t.Fatalf("合法写法不该被拒（退出码 %d）：\n%s", code, msg)
			}
			if strings.Contains(msg, "E4029") {
				t.Fatalf("合法写法被误判成 E4029：\n%s", msg)
			}
			data, err := os.ReadFile(outPath)
			if err != nil {
				t.Fatal(err)
			}
			if _, ds := ir.Load(data); len(ds) != 0 {
				t.Fatalf("IR 不合法：%v", ds)
			}
			// 这条用例不产生浮点常量，但仍不能有类型名当值的坏 lit。
			assertBitPatterns(t, data)
		})
	}
}

// TestDoubleEqualityRejected 是 E4033 的可执行形式（§4.4），必须保留。
//
// `double == double` 的精确相等不是稳定谓词：舍入让数学上相等的两个值比出
// 不相等，NaN 与自己也不相等。编译期证不出来就拒，而不是静默返回 false。
// W4001（「允许但警告」）从未实现，2026-11-10 拍板升为硬错。
//
// 用 `hermes ir`（不经 cmdBuild 的诊断截断）而不是 `hermes build`，
// 所以能精确数条数：每个比较恰好一条，不多不少。
func TestDoubleEqualityRejected(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "== 同一字段自比",
			src: `export struct S { double x; }
export workflow w(input: S) -> bool {
    return input.x == input.x;
}
`,
		},
		{
			name: "!= （曾是唯一的 NaN 判据）",
			src: `export struct S { double x; }
export workflow w(input: S) -> bool {
    return input.x != input.x;
}
`,
		},
		{
			name: "json.as_float 边界窄化后相等",
			src: `export workflow w(input: json) entry -> bool {
    return json.as_float(input.total) == json.as_float(input.subtotal);
}
`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, msg, _ := compileInlineSource(t, c.src)
			if code == 0 {
				t.Fatalf("double 相等必须阻塞构建并报 E4033，实际编过了。stderr:\n%s", msg)
			}
			if n := strings.Count(msg, "error[E4033]"); n != 1 {
				t.Fatalf("E4033 应恰好 1 条，实际 %d 条:\n%s", n, msg)
			}
		})
	}
}

// TestDoubleComparisonStillCompiles 是 E4033 的**反例**，与上面同等重要。
//
// 只测正例的守卫会放过"把所有浮点比较都拦掉"这种过度修复 —— 而那会直接
// 废掉 E4033 自己推荐的改法（容差比较靠的就是 `<`）。所以这里钉住：
// 关系比较、以及按容差写出来的相等，必须继续编过。
func TestDoubleComparisonStillCompiles(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "关系比较仍合法",
			src: `export struct S { double x; }
export workflow w(input: S) -> bool {
    return input.x < input.x;
}
`,
		},
		{
			// 名字里刻意不写 "E4033"：compileInlineSource 用 t.TempDir()，
			// 子测试名会进路径、再进成功消息，断言 strings.Contains(msg, "E4033")
			// 就会匹配到自己 —— 一条恒真的假断言。
			name: "推荐的容差写法（epsilon 来自数据）",
			src: `export workflow w(input: json) entry -> bool {
    return json.as_float(input.x) - json.as_float(input.y) < json.as_float(input.eps);
}
`,
		},
		{
			name: "double 字面量也能写容差（v2.0.0 放开后）",
			src: `export workflow w() entry -> bool {
    return 1.5 - 1.25 < 0.5;
}
`,
		},
		{
			name: "int 与 string 的相等不受影响",
			src: `export workflow w(input: json) -> bool {
    auto a = 1;
    auto s = "k";
    return a == a && s == s && input == input;
}
`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, msg, _ := compileInlineSource(t, c.src)
			if code != 0 {
				t.Fatalf("合法写法被 E4033 误伤（退出码 %d）：\n%s", code, msg)
			}
			if strings.Contains(msg, "error[E4033]") {
				t.Fatalf("合法写法不该报 E4033：\n%s", msg)
			}
		})
	}
}

// TestDoubleLiteralViaCABIIsBitPattern 守的是「C ABI 直调」这一层。
//
// CLI 靠 nativeHasError 拦诊断，但 hm_codegen / hm_codegen_ext 是公开 C ABI，
// 完全可以不看 out_diag 只用 out。这两条入口在 api.cpp::run_parse 之后
// **不会因为 DiagBag 里有 error 就短路**：所以三道扫描器留在 codegen 里，
// 判据只盯旧 "float" 标签 —— 手工造 AST 塞旧标签的拒绝由 C++ 单测覆盖。
//
// v2.0.0 起，正常源码（标签是 "double"）直调 C ABI 必须拿到**合法的位模式
// IR**，而不是空 JSON：宿主拿到的这份 workflow 对象要能过 P-1（无浮点 JSON
// 数字）、bits 要满足 ^[0-9a-f]{16}$、且不能把类型名当值。
func TestDoubleLiteralViaCABIIsBitPattern(t *testing.T) {
	const src = `export workflow w() entry -> double {
    return 1.5;
}
`
	cases := []struct {
		name string
		call func() (*native.CodegenResult, error)
	}{
		{"hm_codegen", func() (*native.CodegenResult, error) {
			return native.Codegen([]byte(src), "f.hermes", false)
		}},
		{"hm_codegen_ext", func() (*native.CodegenResult, error) {
			return native.CodegenWithExternals([]byte(src), "f.hermes", false, "")
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := c.call()
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(res.Diag, "E4029") {
				t.Fatalf("%s 不该再报 E4029（double 字面量已放开）：%s", c.name, res.Diag)
			}
			if res.JSON == "" {
				t.Fatalf("%s 必须产出合法位模式 IR（v2.0.0 不再拒 double 字面量）", c.name)
			}
			assertBitPatterns(t, []byte(res.JSON), "3ff8000000000000")
		})
	}
}

// ---------------------------------------------------------------------------
// 真编译真运行的 double 端到端契约
//
// 上面 5 个用例都只停在 `compileInlineSource`（产 IR JSON 再验字符串）。这一组
// 补的是执行侧：同一个 .hermes 程序分别由解释器与 AOT 产物跑一遍，钉住
// shortest round-trip 的文本，以及非有限值在 JSON 边界被拦下的运行期行为。
// ---------------------------------------------------------------------------

// doubleRoundTripSrc 是 T50 shortest round-trip 的规格钉子：0.1 + 0.2 在
// IEEE-754 binary64 下必须是 0.30000000000000004，不是 0.3。
const doubleRoundTripSrc = `export workflow w() entry -> double {
    return 0.1 + 0.2;
}
`

// doubleNonFiniteSrc 产生 +Inf：构造 / 传递 / 比较都不拒，只在写出去时拒。
const doubleNonFiniteSrc = `export workflow w() entry -> double {
    return 1.0 / 0.0;
}
`

// floatRunCaptured 走真实 CLI 的 `hermes run`，分别收回 stdout / stderr。
// 只查退出码拿不到 value 的文本形态，而这条契约钉的就是文本。
func floatRunCaptured(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	dir := t.TempDir()
	outPath := filepath.Join(dir, "stdout.txt")
	errPath := filepath.Join(dir, "stderr.txt")
	of, err := os.Create(outPath)
	if err != nil {
		t.Fatal(err)
	}
	ef, err := os.Create(errPath)
	if err != nil {
		of.Close()
		t.Fatal(err)
	}
	code := func() int {
		oldOut, oldErr := os.Stdout, os.Stderr
		os.Stdout, os.Stderr = of, ef
		defer func() { os.Stdout, os.Stderr = oldOut, oldErr }()
		return run(append([]string{"run"}, args...))
	}()
	if err := of.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ef.Close(); err != nil {
		t.Fatal(err)
	}
	stdout, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.ReadFile(errPath)
	if err != nil {
		t.Fatal(err)
	}
	return code, string(stdout), string(stderr)
}

// assertShortestRoundTripValue 断言 stdout 是完整 JSON 信封，且 value 的**文本**
// 精确等于 0.30000000000000004。用 json.RawMessage 而不是 float64：解成
// float64 会把记法差异抹掉，而这里钉的正是 encoding/json 的记法。
func assertShortestRoundTripValue(t *testing.T, where, stdout string) {
	t.Helper()
	var env struct {
		Steps    *int            `json:"steps"`
		Value    json.RawMessage `json:"value"`
		Workflow string          `json:"workflow"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("%s stdout 不是合法 JSON 信封: %v，原文 %q", where, err, stdout)
	}
	if env.Steps == nil || len(env.Value) == 0 || env.Workflow == "" {
		t.Fatalf("%s 信封缺 steps / value / workflow: %q", where, stdout)
	}
	const want = "0.30000000000000004"
	got := string(env.Value)
	if got != want {
		t.Fatalf("%s value 文本必须精确等于 %q，实际 %q（原文 %q）", where, want, got, stdout)
	}
	if got == "0.3" {
		t.Fatalf("%s value 退化成 0.3，shortest round-trip 规格被破坏：%q", where, stdout)
	}
}

// TestDoubleShortestRoundTripRunAndCompile 是 shortest round-trip 的可执行钉子：
// 同一个 .hermes 程序的 value 文本在 `hermes run` 与 `hermes compile`（真 clang
// 编译 + 真执行）两侧都必须逐字等于 0.30000000000000004，且不得退化成 0.3。
func TestDoubleShortestRoundTripRunAndCompile(t *testing.T) {
	// 两份输入来自同一个文件的同一份字节：run 侧先编 IR，compile 侧直接吃源码。
	dir := t.TempDir()
	src := filepath.Join(dir, "roundtrip.hermes")
	if err := os.WriteFile(src, []byte(doubleRoundTripSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	srcBytes, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}

	// ① hermes run：真实 CLI 编 IR → 真实 CLI 解释执行。
	code, msg, irPath := compileInlineSource(t, string(srcBytes))
	if code != 0 {
		t.Fatalf("0.1 + 0.2 必须能编成 IR，实际 rc=%d：\n%s", code, msg)
	}
	runCode, runOut, runErrText := floatRunCaptured(t, irPath)
	if runCode != 0 {
		t.Fatalf("hermes run 应成功，实际 rc=%d，stderr:\n%s", runCode, runErrText)
	}
	assertShortestRoundTripValue(t, "hermes run", runOut)

	// ② hermes compile：真 clang 编译 + 执行。缺 clang 时按既有模式 t.Skip。
	if _, err := exec.LookPath("clang"); err != nil {
		t.Skip("没有 clang，跳过真编译真运行")
	}
	prog := filepath.Join(dir, "prog")
	cc, ccMsg := runCompileCaptured(t, src, "-o", prog)
	if cc != 0 {
		t.Fatalf("hermes compile 应成功，实际 rc=%d：\n%s", cc, ccMsg)
	}
	got, runErr := exec.Command(prog).Output()
	if runErr != nil {
		t.Fatalf("运行 AOT 产物失败: %v\n%s", runErr, got)
	}
	assertShortestRoundTripValue(t, "hermes compile", string(got))
	t.Logf("hermes run    stdout = %s", strings.TrimSpace(runOut))
	t.Logf("hermes compile stdout = %s", strings.TrimSpace(string(got)))
}

// TestDoubleNonFiniteRejectedAtAOTJSONBoundary 是「只在写出去时拒」的运行期证明：
// 1.0 / 0.0 必须能真编译真运行（构造不拒），但 AOT 产物在 JSON 序列化边界
// 必须以非零码退出、stderr 点名 non-finite，且 stdout 不得出现 inf / nan。
func TestDoubleNonFiniteRejectedAtAOTJSONBoundary(t *testing.T) {
	if _, err := exec.LookPath("clang"); err != nil {
		t.Skip("没有 clang，跳过真编译真运行")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "nonfinite.hermes")
	if err := os.WriteFile(src, []byte(doubleNonFiniteSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	prog := filepath.Join(dir, "prog")
	cc, ccMsg := runCompileCaptured(t, src, "-o", prog)
	if cc != 0 {
		t.Fatalf("1.0 / 0.0 必须能真编译（非有限值只在序列化边界拒），实际 rc=%d：\n%s", cc, ccMsg)
	}

	cmd := exec.Command(prog)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if runErr == nil {
		t.Fatalf("AOT 产物必须在 JSON 边界拒掉 +Inf 并非零退出，stdout=%q stderr=%q",
			stdout.String(), stderr.String())
	}
	if ee, ok := runErr.(*exec.ExitError); ok && ee.ExitCode() == 0 {
		t.Fatalf("退出码必须非 0，实际 %d", ee.ExitCode())
	}
	if !strings.Contains(stderr.String(), "non-finite") {
		t.Errorf("stderr 应提到 non-finite，实际 %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "non-finite double at JSON boundary") {
		t.Errorf("stderr 应给出边界诊断原文，实际 %q", stderr.String())
	}
	lowOut := strings.ToLower(stdout.String())
	for _, bad := range []string{"inf", "nan"} {
		if strings.Contains(lowOut, bad) {
			t.Errorf("stdout 不该出现非法 JSON token %q，实际 %q", bad, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), `"value"`) {
		t.Errorf("stdout 在任何 printf 之前就该被拦下，不该有 value 字段：%q", stdout.String())
	}
	t.Logf("AOT 非有限边界: rc=%v, stdout=%q, stderr=%q",
		runErr, stdout.String(), strings.TrimSpace(stderr.String()))
}
