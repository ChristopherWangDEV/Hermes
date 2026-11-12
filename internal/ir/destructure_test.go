package ir

import (
	"fmt"
	"strings"
	"testing"

	"hermes/internal/diag"
)

// tuple 解构的三种编码（IR.md §4.2）：
//
//	A 形态  auto { vars: [0,1], e }          —— 读兼容
//	B 形态  destructure { vars: [0,1], e }  —— **codegen 唯一写出的规范形**
//	C 形态  parallel { …, bind: [0,1] }     —— 读兼容
//
// 本文件的目的是证明"读方宽容"是真的：三种形状都能装进 Doc、都建立正确的
// var.decl ↔ 绑定节点双向关系（V-5），并且歧义形状会被拒。
//
// 为什么要专门测它：这三种形状在**语义上完全相同**，所以任何一处接漏了都不会崩，
// 只会表现得像"这个变量没有声明"——那类失效在读别人的 IR 时最难归因。

// destructureIR 按指定形态拼一份最小合法 IR。
//
//	"A" 绑定在 decl（id 2）上，vars[].decl = 2
//	"B" 绑定在 destructure（id 2）上，vars[].decl = 2
//	"C" 绑定在 parallel（id 1）的 bind 上，id 2 是 nop 占位，vars[].decl = 1
//
// varsFor 接收"正确的 decl"，返回变量表 JSON —— 调用方据此造出单变量、丢弃位等变体，
// 而不必在模板里塞可变个数的占位符。
func destructureIR(shape string, varsFor func(decl int) string) string {
	var bindSeq string
	// max_spans 按形态不同：A/B 里绑定节点引用 parallel（SP=1），C 把绑定挂在
	// parallel 上、seq 里只剩一个 nop（SP=0）。写死一个值就会有一边被复算判红 ——
	// 这正好说明"复算"这条检查是活的。
	maxSpans := 1
	decl := 2
	bind := ""
	switch shape {
	case "A":
		bindSeq = `{"id":2,"k":"decl","loc":{"m":0,"l":2,"c":5,"el":2,"ec":9},"vars":[0,1],"e":1}`
	case "B":
		bindSeq = `{"id":2,"k":"destructure","loc":{"m":0,"l":2,"c":5,"el":2,"ec":9},"vars":[0,1],"e":1}`
	case "C":
		bindSeq = `{"id":2,"k":"nop","loc":{"m":0,"l":2,"c":5,"el":2,"ec":6}}`
		bind = `,"bind":[0,1]`
		decl = 1
		maxSpans = 0
	default:
		panic("shape 只能是 A/B/C")
	}
	return fmt.Sprintf(`{
  "ir_version": 2,
  "unit": {"name":"t","entry_path":"t.hermes","language_version":2,
           "modules":[{"path":"t.hermes","sha256":"0000000000000000000000000000000000000000000000000000000000000000","builtin":false}]},
  "compiler": {"name":"hermes-parse","version":"0.1","parser_abi":1},
  "types": [],
  "backends": [{"name":"b","kind":"openai","addr":{"src":"literal","value":"http://127.0.0.1:1"}}],
  "tools": [], "schemas": [],
  "workflows": [{
    "name":"f","exported":true,"is_entry":true,"backend":"b",
    "input":null,"returns":{"k":"string"},"params":[],
    "root":0,
    "nodes":[
      {"id":0,"k":"seq","loc":{"m":0,"l":1,"c":1,"el":1,"ec":10},"kids":[2]},
      {"id":1,"k":"parallel","loc":{"m":0,"l":1,"c":1,"el":1,"ec":10},
       "branches":[3,4],"join_k":2,"max_par":2%s},
      %s,
      {"id":3,"k":"seq","loc":{"m":0,"l":1,"c":1,"el":1,"ec":2},"kids":[]},
      {"id":4,"k":"seq","loc":{"m":0,"l":1,"c":1,"el":1,"ec":2},"kids":[]}
    ],
    "vars": %s,
    "bounds":{"max_steps":2,"max_spans":%d,"max_model_calls":0,
              "max_tool_calls":0,"max_concurrency":2,"max_retry_expansion":0},
    "economics":{},
    "interp_deps":[],
    "loc":{"m":0,"l":1,"c":1,"el":3,"ec":2}
  }],
  "exports":{"workflows":["f"],"types":[],"backends":[],"tools":[]},
  "imports":[],"requires":[],
  "diag_counts":{"error":0,"warning":0}
}`, bind, bindSeq, varsFor(decl), maxSpans)
}

// twoVars 是两个变量的变量表，decl 由形态决定。
func twoVars(decl int) string {
	return fmt.Sprintf(`[{"id":0,"name":"a","t":{"k":"string"},"decl":%d,"scope":0},
                         {"id":1,"name":"b","t":{"k":"string"},"decl":%d,"scope":0}]`, decl, decl)
}

// TestDestructureAllThreeShapesLoad 是三种编码的对照测试。
//
// 三份 IR 只差"绑定挂在哪"。若某一形态在 collectRefs 或 checkVarBinding 里没接上，
// 只有对应那一行会红、另外两行仍绿 —— 这正是我们要的定位精度。
func TestDestructureAllThreeShapesLoad(t *testing.T) {
	for _, shape := range []string{"A", "B", "C"} {
		t.Run(shape, func(t *testing.T) {
			d, ds := Load([]byte(destructureIR(shape, twoVars)))
			if len(ds) != 0 {
				t.Fatalf("%s 形态应无诊断，实际 %d 条：%s", shape, len(ds), diag.RenderAll(ds))
			}
			if d == nil {
				t.Fatalf("%s 形态 Load 返回了 nil Doc", shape)
			}
			if got := len(d.Workflows[0].Vars); got != 2 {
				t.Fatalf("变量表应 2 条，实际 %d", got)
			}
		})
	}
}

// TestDestructureDiscardSlot 证明 `_` 丢弃位不是一条引用。
//
// 失效方式很隐蔽：若把 null 当成 0，`auto [_, x]` 会静默变成"引用了第 0 个变量"，
// 于是校验器检查的是一条根本不存在的边，而真正的 x 绑定反而没人查。
func TestDestructureDiscardSlot(t *testing.T) {
	ir := destructureIR("B", func(decl int) string {
		return fmt.Sprintf(`[{"id":0,"name":"x","t":{"k":"string"},"decl":%d,"scope":0}]`, decl)
	})
	// 第 0 位丢弃，第 1 位绑 x。
	ir = replaceOnce(t, ir, `"vars":[0,1],"e":1`, `"vars":[null,0],"e":1`)

	_, ds := Load([]byte(ir))
	if len(ds) != 0 {
		t.Fatalf("含丢弃位的 IR 应无诊断，实际 %d 条：%s", len(ds), diag.RenderAll(ds))
	}
}

// TestLetWithBothShapesRejected 证明歧义形状会被拒。
//
// 读方宽容不等于读方糊涂：同时给 var 与 vars 的 decl，"到底绑了几个"没有答案，
// 必须拒绝而不是挑一个用。写方只出 B，所以这条只在读别人的 IR 时会遇到。
func TestLetWithBothShapesRejected(t *testing.T) {
	ir := destructureIR("A", twoVars)
	ir = replaceOnce(t, ir,
		`"k":"decl","loc":{"m":0,"l":2,"c":5,"el":2,"ec":9},"vars":[0,1]`,
		`"k":"decl","loc":{"m":0,"l":2,"c":5,"el":2,"ec":9},"var":0,"vars":[0,1]`)

	_, ds := Load([]byte(ir))
	if !hasCode(ds, diag.CodeVarMismatch) {
		t.Fatalf("auto 同时写 var 与 vars 应报 %s，实际：%s", diag.CodeVarMismatch, codes(ds))
	}
}

// TestDestructureBindMustMatchDecl 证明双向关系不是只查一向。
//
// 反向漏检的形态：把 vars[0].decl 指向 parallel 而不是绑定节点。
func TestDestructureBindMustMatchDecl(t *testing.T) {
	ir := destructureIR("B", twoVars)
	ir = replaceOnce(t, ir,
		`"id":0,"name":"a","t":{"k":"string"},"decl":2`,
		`"id":0,"name":"a","t":{"k":"string"},"decl":1`)

	_, ds := Load([]byte(ir))
	if !hasCode(ds, diag.CodeVarMismatch) {
		t.Fatalf("var.decl 与绑定节点不一致应报 %s，实际：%s", diag.CodeVarMismatch, codes(ds))
	}
}

// TestDestructureVarRefsAreCollected 证明三种形态的绑定位**都进了引用集合**。
//
// 这一条是前面几条的"反向保险"：上面证明"合法 IR 不报错"，而合法 IR 在接线漏掉时
// 同样不会报错——只有造一个越界的变量下标，才能区分"检查过且通过"与"根本没检查"。
func TestDestructureVarRefsAreCollected(t *testing.T) {
	for _, shape := range []string{"A", "B", "C"} {
		t.Run(shape, func(t *testing.T) {
			// 变量表只留 1 个，但绑定位仍是 [0,1] ⇒ 1 号变量悬空。
			ir := destructureIR(shape, func(decl int) string {
				return fmt.Sprintf(`[{"id":0,"name":"a","t":{"k":"string"},"decl":%d,"scope":0}]`, decl)
			})
			_, ds := Load([]byte(ir))
			if !hasCode(ds, diag.CodeDanglingRef) {
				t.Fatalf("%s 形态的越界变量下标应报 %s，实际：%s", shape, diag.CodeDanglingRef, codes(ds))
			}
		})
	}
}

// replaceOnce 在锚点不唯一时直接 Fatal：静默返回原串会让测试变绿，
// 而"守卫自己被改坏所以永远绿"是本项目最想避免的事。
func replaceOnce(t *testing.T, s, old, new string) string {
	t.Helper()
	if n := strings.Count(s, old); n != 1 {
		t.Fatalf("锚点出现 %d 次（应为 1）：%q", n, old)
	}
	return strings.Replace(s, old, new, 1)
}
