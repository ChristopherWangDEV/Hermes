package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hermes/internal/diag"
	"hermes/internal/engine"
	"hermes/internal/ir"
)

// goldenIR 是 T34 的第一份产物：`schemas/golden/ir-hello.json`。
//
// 它由这条命令生成，而不是手抄：
//
//	hermes ir cmd/hermes/testdata/hello.hermes -o schemas/golden/ir-hello.json
//
// 手抄的 golden 会把"我随手选的编号与顺序"固化成规格；由 codegen 产出的 golden
// 才记录"这条流水线现在给出什么"，于是它一变就说明流水线变了。
var goldenCases = []struct {
	name    string
	fixture string
	golden  string
}{
	{"hello", "testdata/hello.hermes", "../../schemas/golden/ir-hello.json"},
	{"parallel", "testdata/parallel.hermes", "../../schemas/golden/ir-parallel.json"},
	{"control", "testdata/control.hermes", "../../schemas/golden/ir-control.json"},
	{"typed", "testdata/typed.hermes", "../../schemas/golden/ir-typed.json"},
	{"model", "testdata/model.hermes", "../../schemas/golden/ir-model.json"},
	{"tool", "testdata/tool.hermes", "../../schemas/golden/ir-tool.json"},
	{"multi", "testdata/multi/app.hermes", "../../schemas/golden/ir-multi.json"},
	{"support_bot", "../../examples/support_bot/support.hermes", "../../schemas/golden/ir-support_bot.json"},
	{"try", "testdata/trycatch.hermes", "../../schemas/golden/ir-try.json"},
	{"retry", "testdata/retry.hermes", "../../schemas/golden/ir-retry.json"},
	{"live", "testdata/live.hermes", "../../schemas/golden/ir-live.json"},
	{"arrays", "testdata/arrays.hermes", "../../schemas/golden/ir-arrays.json"},
	{"allowstatus", "testdata/allowstatus.hermes", "../../schemas/golden/ir-allowstatus.json"},
	{"toolwire", "testdata/toolwire.hermes", "../../schemas/golden/ir-toolwire.json"},
	{"idemkey", "testdata/idemkey.hermes", "../../schemas/golden/ir-idemkey.json"},
	{"cast", "testdata/cast.hermes", "../../schemas/golden/ir-cast.json"},
	{"binop", "testdata/binop.hermes", "../../schemas/golden/ir-binop.json"},
	{"agent", "testdata/agent.hermes", "../../schemas/golden/ir-agent.json"},
	{"functions", "testdata/functions.hermes", "../../schemas/golden/ir-functions.json"},
}

// TestIROutputMatchesGolden 是 T34 验收 ⑨ 的最小版本：
// 同一份源码两次编译 ⇒ 与 golden **逐字节**相等。
func TestIROutputMatchesGolden(t *testing.T) {
	for _, c := range goldenCases {
		t.Run(c.name, func(t *testing.T) {
			want, err := os.ReadFile(c.golden)
			if err != nil {
				t.Fatalf("读不到 golden %s: %v（先跑 hermes ir 生成它）", c.golden, err)
			}

			out := filepath.Join(t.TempDir(), "out.ir.json")
			if code := run([]string{"ir", c.fixture, "-o", out}); code != 0 {
				t.Fatalf("hermes ir 退出码 %d", code)
			}

			got, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("IR 与 golden 不一致（%d vs %d 字节）\n--- got ---\n%s\n--- want ---\n%s",
					len(got), len(want), got, want)
			}
		})
	}
}

// TestIRIsDeterministic 连跑三次，证明"同源码 ⇒ 同字节"不是一次巧合。
//
// 单次比对只能证明"这次等于 golden"；三次才排掉"某处有 map 迭代序 / 时间戳"
// 这类概率性问题（Go 的 map 顺序随机化跑一次抓不到，本项目已经因此吃过一次）。
func TestIRIsDeterministic(t *testing.T) {
	var first []byte
	for i := 0; i < 3; i++ {
		out := filepath.Join(t.TempDir(), "a.json")
		if code := run([]string{"ir", goldenCases[1].fixture, "-o", out}); code != 0 {
			t.Fatalf("第 %d 次退出码非 0", i)
		}
		got, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = got
			continue
		}
		if !bytes.Equal(first, got) {
			t.Fatalf("第 %d 次的字节与第一次不同", i)
		}
	}
}

// TestIRRejectsUnsupportedConstruct 证明不支持的构造**报错**而不是产出半份 IR。
//
// 这条比"能编"更重要：一份缺了 return 的 IR 依然能通过形状校验，
// 而它到运行期才会表现为"workflow 什么都不做"。
func TestIRRejectsUnsupportedConstruct(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "bad.hermes")
	// 从数组里取字段：field 节点这一刀还不支持（需要类型表），必须报错而不是跳过。
	body := "export workflow f() entry -> string {\n" +
		"    auto a = json.get(\"k\");\n" +
		"    return \"b\";\n}\n"
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"ir", src}); code != 4 {
		t.Fatalf("不支持的构造应退出 4，实际 %d", code)
	}
}

// compileInlineSource 把一段源码编成 IR，返回 (退出码, stderr 全文, IR 路径)。
// 临时替换 os.Stderr 是为了逐字断言诊断：只查退出码 4 会把「语法写错」
// 与「形状被正确拒绝」混成同一件事。
func compileInlineSource(t *testing.T, src string) (int, string, string) {
	t.Helper()
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "case.hermes")
	outPath := filepath.Join(dir, "out.ir.json")
	if err := os.WriteFile(srcPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	stderrPath := filepath.Join(dir, "stderr.txt")
	ef, err := os.Create(stderrPath)
	if err != nil {
		t.Fatal(err)
	}
	code := func() int {
		old := os.Stderr
		os.Stderr = ef
		defer func() { os.Stderr = old }()
		return run([]string{"ir", srcPath, "-o", outPath})
	}()
	if err := ef.Close(); err != nil {
		t.Fatal(err)
	}
	msg, err := os.ReadFile(stderrPath)
	if err != nil {
		t.Fatal(err)
	}
	return code, string(msg), outPath
}

// TestInlineIfReturnDecisionTree 把 docs/FUNCTIONS.md 路线 A 的验收用例固化成测试。
//
// 源码里的 `0 - N` 是 BinaryExpr 写法，保留它让用例顺带覆盖二元减法。一元负号
// `-N` 与逻辑非 `!b` 现已实现，另由 TestUnaryOperators 单独回归（早前它们未实现，
// 用例才刻意避开 `-N`）。
func TestInlineIfReturnDecisionTree(t *testing.T) {
	boolStringSrc := `bool yes(int x) {
    if (x > 0) { return true; }
    return false;
}
string label(int x) {
    if (x > 0) { return "pos"; }
    return "neg";
}
export workflow m1() entry -> bool { return yes(3); }
export workflow m2() -> string { return label(0 - 1); }
`
	cases := []struct {
		name     string
		src      string
		workflow string
		want     any
	}{
		{
			name:     "basic",
			workflow: "main",
			want:     int64(2), // pick(3) + 1
			src: `int pick(int x) {
    if (x > 0) { return 1; }
    return 0;
}
export workflow main() entry -> int {
    return pick(3) + 1;
}
`,
		},
		{
			name:     "if_else",
			workflow: "main",
			want:     int64(30), // side(-5) + side(7)
			src: `int side(int x) {
    if (x < 0) { return 10; } else { return 20; }
}
export workflow main() entry -> int {
    return side(0 - 5) + side(7);
}
`,
		},
		{
			name:     "multi_call_site",
			workflow: "main",
			want:     int64(80), // band(-1) + band(50) + band(200)*2
			src: `int band(int x) {
    if (x < 0) { return 10; }
    if (x > 100) { return 20; }
    return 30;
}
export workflow main() entry -> int {
    int a = band(0 - 1) + band(50);
    int b = band(200) * 2;
    return a + b;
}
`,
		},
		{
			name:     "bool_return",
			src:      boolStringSrc,
			workflow: "m1",
			want:     true,
		},
		{
			name:     "string_return",
			src:      boolStringSrc,
			workflow: "m2",
			want:     "neg",
		},
		{
			name:     "else_if_chain",
			workflow: "main",
			want:     int64(123),
			src: `int grade(int x) {
    if (x < 0) { return 1; } else if (x == 0) { return 2; } else { return 3; }
}
export workflow main() entry -> int {
    return grade(0 - 1) * 100 + grade(0) * 10 + grade(5);
}
`,
		},
		{
			name:     "nested_if",
			workflow: "main",
			want:     int64(123),
			src: `int classify(int x) {
    if (x < 0) {
        if (x < 0 - 10) { return 1; }
        return 2;
    } else {
        if (x > 10) { return 3; }
        return 4;
    }
}
export workflow main() entry -> int {
    return classify(0 - 20) * 100 + classify(0 - 1) * 10 + classify(20);
}
`,
		},
		{
			name:     "nested_inline",
			workflow: "main",
			want:     int64(83), // outer(9)*10 + outer(-1) = 8*10 + 3
			src: `int inner(int y) {
    if (y > 5) { return 7; }
    return 3;
}
int outer(int x) {
    if (x > 0) { return inner(x) + 1; }
    return inner(0);
}
export workflow main() entry -> int {
    return outer(9) * 10 + outer(0 - 1);
}
`,
		},
		{
			// 归约出的 if 的**条件里**再触发一次内联：内层合成语句必须排在
			// 外层 if 之前，否则条件会读到零值。
			name:     "nested_inline_in_reduced_condition",
			workflow: "main",
			want:     int64(30), // outer(5) + outer(0) = 10 + 20
			src: `int inner(int y) {
    if (y > 0) { return 1; }
    return 0;
}
int outer(int x) {
    if (inner(x) > 0) { return 10; }
    return 20;
}
export workflow main() entry -> int {
    return outer(5) + outer(0);
}
`,
		},
		{
			// 归约出的函数被用在**普通 workflow if 的条件**里：合成语句排在
			// 引用它的那条 if 语句之前（cur_block_ 的 saved/restore 生效）。
			name:     "inline_in_normal_if_condition",
			workflow: "main",
			want:     int64(10),
			src: `int pick(int x) {
    if (x > 0) { return 1; }
    return 0;
}
export workflow main() entry -> int {
    if (pick(3) == 1) { return 10; }
    return 20;
}
`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, msg, outPath := compileInlineSource(t, c.src)
			if code != 0 {
				t.Fatalf("hermes ir 退出码 %d：%s", code, msg)
			}
			data, err := os.ReadFile(outPath)
			if err != nil {
				t.Fatal(err)
			}
			doc, ds := ir.Load(data)
			if len(ds) != 0 {
				t.Fatalf("归约产物不是合法 IR：%s", diag.RenderAll(ds))
			}
			// 非递归函数不能退回动态派发，静态上界也不能被标成 unbounded。
			for wi := range doc.Workflows {
				w := &doc.Workflows[wi]
				if w.Bounds.Unbounded {
					t.Fatalf("workflow %q 被标成 unbounded", w.Name)
				}
				for ni := range w.Nodes {
					if w.Nodes[ni].K == "call_fn" {
						t.Fatalf("workflow %q 里还有 call_fn（id=%d）", w.Name, w.Nodes[ni].ID)
					}
				}
			}
			var target *ir.Workflow
			for wi := range doc.Workflows {
				if doc.Workflows[wi].Name == c.workflow {
					target = &doc.Workflows[wi]
					break
				}
			}
			if target == nil {
				t.Fatalf("找不到 workflow %q", c.workflow)
			}
			// 归约出的 if 产生 1 个 trace span，必须如实进 max_spans。
			if target.Bounds.MaxSpans <= 0 {
				t.Fatalf("归约出的 if 必须计入 max_spans，实际 %d", target.Bounds.MaxSpans)
			}
			for vi := range target.Vars {
				v := &target.Vars[vi]
				if !strings.HasPrefix(v.Name, "__hm_inline_r_") {
					continue
				}
				if v.Decl < 0 || v.Decl >= len(target.Nodes) ||
					target.Nodes[v.Decl].K != "decl" {
					t.Fatalf("合成结果变量 %q 的 decl=%d 没指向 decl 节点", v.Name, v.Decl)
				}
			}
			res, err := engine.RunWith(doc, c.workflow, nil, engine.RunOptions{})
			if err != nil {
				t.Fatalf("执行 %q 失败：%v", c.workflow, err)
			}
			if res.Value != c.want {
				t.Fatalf("%q 期望 %#v，实际 %#v", c.workflow, c.want, res.Value)
			}
		})
	}
}

// TestUnaryOperators 一元负号 `-x` 与逻辑非 `!b` 必须能编、能跑。
//
// 这两者此前**完全没实现**（gen_expr 无 UnaryExpr 分支、engine 无 "unop" case），
// 于是 `-x` / `!flag` 根本编不出来，报错还说"尚不支持这种表达式"。现在补上了
// unop 的生成与求值，这里钉成回归。
func TestUnaryOperators(t *testing.T) {
	src := `export workflow neg() entry -> int {
    int x = 5;
    return -x;
}
export workflow neglit() -> int {
    return -1;
}
export workflow nottrue() -> bool {
    bool flag = true;
    return !flag;
}
`
	code, msg, outPath := compileInlineSource(t, src)
	if code != 0 {
		t.Fatalf("hermes ir 退出码 %d：%s", code, msg)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	doc, ds := ir.Load(data)
	if len(ds) != 0 {
		t.Fatalf("IR 不合法：%s", diag.RenderAll(ds))
	}
	cases := []struct {
		wf   string
		want any
	}{
		{"neg", int64(-5)},
		{"neglit", int64(-1)},
		{"nottrue", false},
	}
	for _, c := range cases {
		res, err := engine.RunWith(doc, c.wf, nil, engine.RunOptions{})
		if err != nil {
			t.Fatalf("执行 %q 失败：%v", c.wf, err)
		}
		if res.Value != c.want {
			t.Fatalf("%q 期望 %#v，实际 %#v", c.wf, c.want, res.Value)
		}
	}
}

// TestInlineInsideRecursiveFunction 验 codegen_functions 的独立 Gen：
// 递归函数 rec 的函数表里，对非递归 pick 的调用同样必须在编译期展开。
func TestInlineInsideRecursiveFunction(t *testing.T) {
	src := `int pick(int x) {
    if (x > 0) { return 1; }
    return 0;
}
int rec(int n) {
    if (n <= 0) { return 0; }
    return pick(n) + rec(n - 1);
}
export workflow main() entry -> int { return rec(3); }
`
	code, msg, outPath := compileInlineSource(t, src)
	if code != 0 {
		t.Fatalf("hermes ir 退出码 %d：%s", code, msg)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	doc, ds := ir.Load(data)
	if len(ds) != 0 {
		t.Fatalf("IR 不合法：%s", diag.RenderAll(ds))
	}
	var rec *ir.Function
	for i := range doc.Functions {
		if doc.Functions[i].Name == "rec" {
			rec = &doc.Functions[i]
			break
		}
	}
	if rec == nil || !rec.Recursive {
		t.Fatalf("rec 应该是递归函数：%+v", rec)
	}
	found := false
	for i := range rec.Vars {
		v := &rec.Vars[i]
		if !strings.HasPrefix(v.Name, "__hm_inline_r_") {
			continue
		}
		found = true
		if v.Decl < 0 || v.Decl >= len(rec.Nodes) || rec.Nodes[v.Decl].K != "decl" {
			t.Fatalf("函数表里合成结果变量 %q 的 decl=%d 没指向 decl 节点", v.Name, v.Decl)
		}
	}
	if !found {
		t.Fatal("rec 的函数表里没有 __hm_inline_r_N：递归函数体里的非递归调用没被内联")
	}
	res, err := engine.RunWith(doc, "main", nil, engine.RunOptions{})
	if err != nil {
		t.Fatalf("执行 main 失败：%v", err)
	}
	if res.Value != int64(3) {
		t.Fatalf("rec(3) 应得 3，实际 %#v", res.Value)
	}
}

// TestInlineRejectsUnsupportedShapes 守「不要为了支持所有情况把语义做糊」：
// 中间语句、循环、无 return 的路径、double 返回类型、else 后还有语句、
// 分支里的中间语句，全部必须**继续**在编译期拒绝。
//
// v2.0.0：类型名从 `float` 同步成 `double`。零值门本身没变 —— 用户函数内联
// 仍只支持 int / cost / bool / string 四种有零值的返回类型，double 没有零值，
// 所以这一条守卫的正例（E_IR_CODEGEN "has no zero value"）原样保留。
func TestInlineRejectsUnsupportedShapes(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "double_return_type",
			want: "has no zero value",
			src: `double bad(int x) {
    if (x > 0) { return 1; }
    return 0;
}
export workflow main() entry -> double { return bad(1); }
`,
		},
		{
			name: "path_without_return",
			want: "cannot be inlined at compile time",
			src: `int bad(int x) {
    if (x > 0) { return 1; }
}
export workflow main() entry -> int { return bad(1); }
`,
		},
		{
			name: "statement_after_else",
			want: "cannot be inlined at compile time",
			src: `int bad(int x) {
    if (x > 0) { return 1; } else { return 2; }
    return 3;
}
export workflow main() entry -> int { return bad(1); }
`,
		},
		{
			name: "middle_let_in_branch",
			want: "cannot be inlined at compile time",
			src: `int bad(int x) {
    if (x > 0) { int y = x + 1; return y; }
    return 0;
}
export workflow main() entry -> int { return bad(1); }
`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, msg, outPath := compileInlineSource(t, c.src)
			if code != 4 {
				t.Fatalf("不支持的形状应退出 4，实际 %d：%s", code, msg)
			}
			if !strings.Contains(msg, "E_IR_CODEGEN") {
				t.Fatalf("诊断里没有 E_IR_CODEGEN：%s", msg)
			}
			if !strings.Contains(msg, c.want) {
				t.Fatalf("诊断里没有 %q：%s", c.want, msg)
			}
			if _, err := os.Stat(outPath); err == nil {
				t.Fatalf("编译失败时不应产出 IR：%s", outPath)
			}
		})
	}
}

// TestRouteBInlinesStatementsAndLoops 守路线 B（2026-11-07）：含中间语句/循环的
// 非递归函数现在编译期内联（call_fn==0，零开销）。直线代码上界精确（有限），
// 含循环则诚实标 unbounded —— 绝不写一个偏小的有限 max_steps。
func TestRouteBInlinesStatementsAndLoops(t *testing.T) {
	cases := []struct {
		name      string
		src       string
		wantValue int64
		unbounded bool
	}{
		{
			name:      "straight_line",
			wantValue: 14, // f(3,4) = (3+4)*2
			src: `int f(int a, int b) {
    int x = a + b;
    int y = x * 2;
    return y;
}
export workflow main() entry -> int { return f(3, 4); }
`,
		},
		{
			name:      "loop_body",
			wantValue: 10, // 0+1+2+3+4
			unbounded: true,
			src: `int f(int n) {
    int s = 0;
    for (auto i = 0; i < n; i++) { s = s + i; }
    return s;
}
export workflow main() entry -> int { return f(5); }
`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, msg, outPath := compileInlineSource(t, c.src)
			if code != 0 {
				t.Fatalf("路线 B 应编译成功，实际 exit=%d：%s", code, msg)
			}
			data, err := os.ReadFile(outPath)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), `"call_fn"`) {
				t.Fatalf("路线 B 必须内联，产物里出现了 call_fn")
			}
			doc, ds := ir.Load(data)
			if len(ds) != 0 {
				t.Fatalf("IR 不合法（%d 条诊断）", len(ds))
			}
			found, gotUnbounded := false, false
			for i := range doc.Workflows {
				if doc.Workflows[i].Name == "main" {
					gotUnbounded = doc.Workflows[i].Bounds.Unbounded
					found = true
				}
			}
			if !found {
				t.Fatal("找不到 main workflow")
			}
			if gotUnbounded != c.unbounded {
				t.Fatalf("unbounded 期望 %v，实际 %v", c.unbounded, gotUnbounded)
			}
			res, err := engine.RunWith(doc, "main", nil, engine.RunOptions{})
			if err != nil {
				t.Fatalf("执行失败：%v", err)
			}
			if res.Value != c.wantValue {
				t.Fatalf("期望 %d，实际 %#v", c.wantValue, res.Value)
			}
		})
	}
}
