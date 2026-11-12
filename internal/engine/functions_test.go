package engine

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"hermes/internal/ir"
)

// loadFunctionsGolden 读 2.0.0 的函数 golden。
//
// 为什么用 golden 而不是在测试里手搭 Doc：函数表与 call_fn 的 id 都来自 codegen。
// 手搭一份"我以为的形状"只能证明测试自己自洽，证明不了 codegen 与 engine 对得上。
func loadFunctionsGolden(t *testing.T) *ir.Doc {
	t.Helper()
	data, err := os.ReadFile("../../schemas/golden/ir-functions.json")
	if err != nil {
		t.Fatalf("读不到函数 golden：%v", err)
	}
	doc, ds := ir.Load(data)
	if len(ds) != 0 {
		t.Fatalf("函数 golden 不是合法 IR（%d 条诊断）", len(ds))
	}
	if len(doc.Functions) == 0 {
		t.Fatal("golden 里没有 functions[]：函数表没进 IR")
	}
	return doc
}

// TestUserFunctionsCallFrames 验调用帧与参数绑定：
//
//	main = double_it(20) + fib(10) = 40 + 55 = 95
//
// 它同时覆盖"非递归函数"与"递归函数"，以及两条不同的调用点。
func TestUserFunctionsCallFrames(t *testing.T) {
	doc := loadFunctionsGolden(t)

	res, err := RunWith(doc, "main", nil, RunOptions{})
	if err != nil {
		t.Fatalf("执行 main 失败：%v", err)
	}
	if res.Value != int64(95) {
		t.Fatalf("期望 95（double_it(20)=40 + fib(10)=55），实际 %#v", res.Value)
	}
}

// TestRecursionDepthLimit 守 docs/FUNCTIONS.md 的"深度上限 512"：
// 递归深度不可静态定界，所以运行期必须有一道闸，超了回**普通错误**，不是崩溃。
//
// down(100000) 在 513 层停下；如果这条测试卡住或 panic，说明闸没生效。
func TestRecursionDepthLimit(t *testing.T) {
	doc := loadFunctionsGolden(t)

	_, err := RunWith(doc, "deep", nil, RunOptions{})
	if err == nil {
		t.Fatal("down(100000) 应该被深度上限拦下，实际成功返回")
	}
	if !strings.Contains(err.Error(), "stack depth exceeded") {
		t.Fatalf("期望 stack depth exceeded，实际：%v", err)
	}
}

// TestNonRecursiveFunctionsAreInlined 守"编译期展开"这条硬要求：
//
//	mild 只调用 double_it（非递归）→ 产物里必须**没有 call_fn**，
//	并且 unbounded 不出现（上界是静态的 0 个 span）。
//
// 运行期只执行展开后的普通节点；如果这里出现 call_fn，说明 codegen 退回了
// 动态派发 —— 那正是"编译型语言"不能接受的部分。
func TestNonRecursiveFunctionsAreInlined(t *testing.T) {
	doc := loadFunctionsGolden(t)

	var mild *ir.Workflow
	for i := range doc.Workflows {
		if doc.Workflows[i].Name == "mild" {
			mild = &doc.Workflows[i]
			break
		}
	}
	if mild == nil {
		t.Fatal("golden 里没有 mild workflow")
	}
	if mild.Bounds.Unbounded {
		t.Fatalf("mild 只调用非递归函数，上界必须静态；实际 bounds=%+v", mild.Bounds)
	}
	for i := range mild.Nodes {
		if mild.Nodes[i].K == "call_fn" {
			t.Fatalf("mild 里还有 call_fn（id=%d）：非递归函数没有在编译期展开", mild.Nodes[i].ID)
		}
	}

	res, err := RunWith(doc, "mild", nil, RunOptions{})
	if err != nil {
		t.Fatalf("执行 mild 失败：%v", err)
	}
	if res.Value != int64(40) {
		t.Fatalf("double_it(20) 应得 40，实际 %#v", res.Value)
	}
}

// TestIfReturnDecisionTreeIsInlined 守 docs/FUNCTIONS.md 的路线 A：
//
//	branch = pick(3) * 10 + pick(0) = 1 * 10 + 0 = 10
//
// pick 的函数体是 if/return 决策树，codegen 在调用点归约成
// 「__hm_inline_r_N 结果变量 + if 语句」。四条断言缺一不可：值对了但留了
// call_fn 是退回动态派发；值对了但 max_spans=0 是上界撒谎（运行期断言
// len(spans) ≤ max_spans 会把正确 trace 判成 compiler bug）。
func TestIfReturnDecisionTreeIsInlined(t *testing.T) {
	doc := loadFunctionsGolden(t)

	var branch *ir.Workflow
	for i := range doc.Workflows {
		if doc.Workflows[i].Name == "branch" {
			branch = &doc.Workflows[i]
			break
		}
	}
	if branch == nil {
		t.Fatal("golden 里没有 branch workflow")
	}
	if branch.Bounds.Unbounded {
		t.Fatalf("branch 只调用非递归函数，上界必须静态；实际 bounds=%+v", branch.Bounds)
	}
	// 两个调用点各归约出一个 if；if 产生 1 个 trace span（IR.md §5.1）。
	if branch.Bounds.MaxSpans != 2 {
		t.Fatalf("归约后的两个 if 应让 max_spans=2，实际 %d（bounds=%+v）",
			branch.Bounds.MaxSpans, branch.Bounds)
	}
	for i := range branch.Nodes {
		if branch.Nodes[i].K == "call_fn" {
			t.Fatalf("branch 里还有 call_fn（id=%d）：if/return 函数没有在编译期展开",
				branch.Nodes[i].ID)
		}
	}
	// 合成结果变量必须真的有绑定它的 decl 节点（装配期按节点 var 字段的 id
	// 回填 decl；只生成 var 表达式会让装配期报「编译器 bug」）。
	resultVars := 0
	for i := range branch.Vars {
		v := &branch.Vars[i]
		if !strings.HasPrefix(v.Name, "__hm_inline_r_") {
			continue
		}
		resultVars++
		if v.Decl < 0 || v.Decl >= len(branch.Nodes) || branch.Nodes[v.Decl].K != "decl" {
			t.Fatalf("合成结果变量 %q 的 decl=%d 没有指向 decl 节点", v.Name, v.Decl)
		}
	}
	if resultVars != 2 {
		t.Fatalf("branch 有两个调用点，应有两个 __hm_inline_r_N 变量；实际 %d", resultVars)
	}

	res, err := RunWith(doc, "branch", nil, RunOptions{})
	if err != nil {
		t.Fatalf("执行 branch 失败：%v", err)
	}
	if res.Value != int64(10) {
		t.Fatalf("pick(3)*10 + pick(0) 应得 10，实际 %#v", res.Value)
	}
	// 上界对账：branch 是两个内联调用点的**非入口、有界** workflow，此前被
	// bounds_test.go 的双闸门（跳过非入口 + 跳过 unbounded）永远漏测。引擎
	// run.go 的 stmt() 对每个语句节点（含归约出的 wrapper seq）逐节点计步，
	// codegen 在 reduce_chain 挂出无条件执行的 wrapper seq 后恰好补一次
	// ++stmtNodes_，使 max_steps 与运行期步数同源。这里直接堵这道盲区。
	if res.Steps > branch.Bounds.MaxSteps {
		t.Fatalf("branch 实际 steps %d 超过声明上界 %d", res.Steps, branch.Bounds.MaxSteps)
	}
	if len(res.Spans) > branch.Bounds.MaxSpans+1 {
		t.Fatalf("branch 实际 spans %d 超过声明上界 %d(+1)", len(res.Spans), branch.Bounds.MaxSpans)
	}
}

// TestInlineReductionStepsWithinBounds 守「运行期无 steps 断言」的盲区，覆盖
// 4 调用点 / 嵌套 if/return 函数 / else-if 链这三类归约形态——确保任何一类都不会
// 让"声明上界 < 实际步数"。branch 已在 TestIfReturnDecisionTreeIsInlined 直接对账，
// 这里是它的补强。
//
// 用例从源码编译（走 ../../build/hermes，与 cmd_ir_test.go 同一份构建产物），
// 不手搭 IR——否则只能证明测试自洽，证明不了 codegen 与引擎对得上。
func TestInlineReductionStepsWithinBounds(t *testing.T) {
	type tc struct {
		name string
		src  string
		want float64
	}
	cases := []tc{
		{
			// 4 个内联调用点 ⇒ 4 个无条件执行的 wrapper seq，每个净欠 1 步。
			name: "four_call_sites",
			src: `int pick(int x) { if (x > 0) { return 1; } return 0; }
export workflow main() entry -> int {
    return pick(1) + pick(0)*10 + pick(3)*100 + pick(0 - 1)*1000;
}`,
			want: 101,
		},
		{
			// 嵌套 if/return 函数：外层 if 的两个分支各含一个 return。
			name: "nested_if_return",
			src: `int f(int x) {
    if (x > 0) {
        if (x > 100) { return 123; }
        return 12;
    }
    return 0;
}
export workflow main() entry -> int { return f(200); }`,
			want: 123,
		},
		{
			// else-if 链：归约器同时认 Block 与 IfStmt 形式的 else 分支。
			name: "else_if_chain",
			src: `int grade(int x) {
    if (x < 0) { return 1; }
    else if (x == 0) { return 2; }
    else { return 3; }
}
export workflow main() entry -> int {
    return grade(0 - 1)*100 + grade(0)*10 + grade(5);
}`,
			want: 123,
		},
	}

	const bin = "../../build/hermes"
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			srcPath := filepath.Join(dir, "case.hermes")
			irPath := filepath.Join(dir, "out.ir.json")
			if err := os.WriteFile(srcPath, []byte(c.src), 0o644); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command(bin, "ir", srcPath, "-o", irPath).CombinedOutput(); err != nil {
				t.Fatalf("%s: hermes ir 失败：%v\n%s", c.name, err, out)
			}
			data, err := os.ReadFile(irPath)
			if err != nil {
				t.Fatal(err)
			}
			doc, ds := ir.Load(data)
			if len(ds) != 0 {
				t.Fatalf("%s: IR 不合法：%v", c.name, ds)
			}
			var w *ir.Workflow
			for i := range doc.Workflows {
				if doc.Workflows[i].IsEntry {
					w = &doc.Workflows[i]
					break
				}
			}
			if w == nil {
				t.Fatalf("%s: 没有入口 workflow", c.name)
			}
			resBytes, err := exec.Command(bin, "run", irPath).CombinedOutput()
			if err != nil {
				t.Fatalf("%s: hermes run 失败：%v\n%s", c.name, err, resBytes)
			}
			var res struct {
				Value any   `json:"value"`
				Steps int   `json:"steps"`
				Spans []any `json:"spans"`
			}
			if err := json.Unmarshal(resBytes, &res); err != nil {
				t.Fatalf("%s: 解析 run 输出失败：%v\n%s", c.name, err, resBytes)
			}
			got, ok := res.Value.(float64)
			if !ok || got != c.want {
				t.Fatalf("%s: 值期望 %v，实际 %#v", c.name, c.want, res.Value)
			}
			if res.Steps > w.Bounds.MaxSteps {
				t.Fatalf("%s: 实际 steps %d 超过声明上界 %d", c.name, res.Steps, w.Bounds.MaxSteps)
			}
			if len(res.Spans) > w.Bounds.MaxSpans+1 {
				t.Fatalf("%s: 实际 spans %d 超过声明上界 %d(+1)", c.name, len(res.Spans), w.Bounds.MaxSpans)
			}
		})
	}
}
