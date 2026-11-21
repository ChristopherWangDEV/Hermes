package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"hermes/internal/ir"
)

// 本文件守 `fallback`（2.0.0）的**运行期**语义：错误边界，不是异常。
//
// 三条不变量：
//  1. main 成功 → 直接返回 main 的值，**降级块不执行**（不能白花钱）；
//  2. main 失败（*RunError）→ 跑降级块并取它的返回值；
//  3. 引擎内部的编程错误**不被降级吞掉** —— 否则一个编译器 bug 会伪装成
//     "降级答案"，而那类错误最需要被看见。
//
// 为什么用 binop 除零当"失败源"而不是工具调用：ContractError 是确定的
// *RunError，不依赖假件、不依赖网络，测试不会 flaky。

func fbLit(t *testing.T, id int, typ string, v any) ir.Node {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return ir.Node{ID: id, K: "lit", Loc: ir.Loc{L: 1, C: 1, EC: 2}, LitT: &typ, LitV: raw}
}

func fbInt(t *testing.T, id int, n int64) ir.Node { return fbLit(t, id, "int", n) }

func fbStr(t *testing.T, id int, s string) ir.Node { return fbLit(t, id, "string", s) }

func fbPtr(i int) *int { return &i }

func fbOp(s string) *string { return &s }

// fbDoc 造一份最小 IR，语义等价于：
//
//	auto x = <main> fallback { return <bodyVal> };
//	return x;
//
// 节点布局（id 必须等于下标 —— engine 的引用就是下标，错一位就会"读到别的节点"）：
//
//	0 seq[kids 1,2]   1 decl[var 0, e 3]   2 return[e 4]   3 fallback[main 5, body 6]
//	4 var[0]          5 main（调用方给）   6 seq[kids 7]   7 return[e 8]
//	8 bodyVal（调用方给）   9.. extra
//
// `extra` 里的节点从 9 起编号（给 main 当子表达式用）。
func fbDoc(t *testing.T, mainNode ir.Node, bodyVal ir.Node, extra []ir.Node) *ir.Doc {
	t.Helper()

	mainNode.ID = 5
	bodyVal.ID = 8
	for i := range extra {
		extra[i].ID = 9 + i
	}

	nodes := []ir.Node{
		{ID: 0, K: "seq", Kids: []int{1, 2}},
		{ID: 1, K: "decl", Var: fbPtr(0), E: fbPtr(3)},
		{ID: 2, K: "return", E: fbPtr(4)},
		{ID: 3, K: "fallback", Main: fbPtr(5), Body: fbPtr(6)},
		{ID: 4, K: "var", Var: fbPtr(0)},
		mainNode,
		{ID: 6, K: "seq", Kids: []int{7}},
		{ID: 7, K: "return", E: fbPtr(8)},
		bodyVal,
	}
	nodes = append(nodes, extra...)
	// loc 全部给一个合法值：V-6 只在 Load 路径跑，但 RunWith 会渲染 loc，
	// 留零值会让失败信息指向"1:0"，排障时看不出是哪儿。
	for i := range nodes {
		if nodes[i].Loc.EC == 0 {
			nodes[i].Loc = ir.Loc{L: 1, C: 1, EC: 2}
		}
	}

	return &ir.Doc{
		IRVersion: ir.IRVersionSupported,
		Unit: ir.Unit{
			Name:            "fb",
			EntryPath:       "fb.hermes",
			LanguageVersion: ir.IRVersionSupported,
			Modules: []ir.Module{{
				Path:   "fb.hermes",
				SHA256: strings.Repeat("0", 64),
			}},
		},
		Workflows: []ir.Workflow{{
			Name:     "f",
			Exported: true,
			IsEntry:  true,
			Returns:  ir.TypeExpr{K: "string"},
			Root:     0,
			Vars:     []ir.Var{{Name: "x", Decl: 1}},
			Nodes:    nodes,
		}},
	}
}

// main 成功时降级块**不执行**：返回值来自 main。
func TestFallbackReturnsMainWhenItSucceeds(t *testing.T) {
	d := fbDoc(t, fbStr(t, 0, "from-main"), fbStr(t, 0, "from-fallback"), nil)

	res, err := RunWith(d, "", nil, RunOptions{})
	if err != nil {
		t.Fatalf("main 成功时不该失败：%v", err)
	}
	if res.Value != "from-main" {
		t.Fatalf("期望主路径的值 from-main，实际 %v（降级块被执行了？）", res.Value)
	}
}

// main 失败（ContractError）时跑降级块并取它的返回值。
func TestFallbackRunsBodyWhenMainFails(t *testing.T) {
	// main = 1 / 0 → ContractError（*RunError），可被错误边界接住。
	main := ir.Node{K: "binop", Op: fbOp("div"), L: fbPtr(9), R: fbPtr(10)}
	d := fbDoc(t, main, fbStr(t, 0, "from-fallback"), []ir.Node{
		fbInt(t, 0, 1),
		fbInt(t, 0, 0),
	})

	res, err := RunWith(d, "", nil, RunOptions{})
	if err != nil {
		t.Fatalf("错误边界必须接住 ContractError，实际仍失败：%v", err)
	}
	if res.Value != "from-fallback" {
		t.Fatalf("期望降级值 from-fallback，实际 %v", res.Value)
	}
}

// 编程错误**不**被降级吞掉。
//
// 构造方式：让 main 读一个从未被赋值的变量。engine 对这种情况返回
// fmt.Errorf（不是 *RunError）—— 这正是"调用本身坏了"，必须向外冒。
func TestFallbackDoesNotSwallowProgrammingErrors(t *testing.T) {
	// var 1 在 vars[] 里不存在、也从未赋值：读它走的是编程错误分支。
	main := ir.Node{K: "var", Var: fbPtr(77)}
	d := fbDoc(t, main, fbStr(t, 0, "from-fallback"), nil)

	_, err := RunWith(d, "", nil, RunOptions{})
	if err == nil {
		t.Fatal("编程错误必须冒出来，不能被降级块吃成 from-fallback")
	}
	if strings.Contains(err.Error(), "from-fallback") {
		t.Fatalf("错误信息不该是降级结果：%v", err)
	}
}
