package engine

import (
	"os"
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
