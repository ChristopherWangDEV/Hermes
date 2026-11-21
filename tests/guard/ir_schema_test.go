// 本文件守的是 IR.md §12 与代码之间的**双向**一致：
// 规格说有 20 种节点，schema 必须有、refs 收集器必须逐种处理、Go 结构体必须能装下。
//
// 为什么这值得一条守卫：本项目反复出现的失败模式是"文档按目标状态写，读者按已完成读"。
// schema 是最容易漂移的那种产物 —— 它不像代码会编译失败，改错了只会安静地少一个分支，
// 而后果要等到 T34 产出 IR 才暴露。这里的做法是把三处清单相互对照：
//
//	schemas/ir.schema.json —— 机器可读的规格
//	internal/ir/refs.go    —— 引用收集器（新增 kind 必须加 case，见文件头注释）
//	docs/IR.md §4.2        —— 人读的规格
package guard_test

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// wantNodeKinds 是 IR.md §4.2 的节点清单（21 种 + agent）；改这里必须同时改 IR.md。
//
// 注意标题写的是"语句/结构节点（8）"，但表里实际有 9 行（含 nop）；
// 9 + 11 = 20 与总数对得上，所以标题的那个 8 是文档笔误。
// 这份清单按"表里的行"而不是"标题里的数"来，否则守卫会替一个笔误背书。
var wantNodeKinds = []string{
	// 语句/结构（10）：解构绑定在 IR 里有一个独立节点（B 形态，见 IR.md §4.2）
	"seq", "decl", "if", "match", "parallel", "retry", "try", "return", "nop", "destructure",
	// 表达式（11）
	"lit", "var", "field", "index", "binop", "unop", "interp",
	"call_model", "call_tool", "call_json", "cast",
	// 用户函数调用（2.0.0）：与 call_json 同形，但指向 functions[]。
	"call_fn",
	// agent（D-034）：有界循环。加它时必须同时更新 IR.md §4.2 的节点表。
	"agent",
	// 存储子系统（D-035）、表达式语句、数组字面量与 len（D-038）。
	"store_op", "exprstmt", "array_lit", "len", "at", "for", "while", "break", "continue", "assign",
	// fallback（2.0.0）：错误边界，不是异常。main=主表达式，body=降级块。
	// 与 try 同构但**是表达式**（产出一个值）。加它时必须同时更新 IR.md §4.2。
	"fallback",
	// call_native（反向融合 / extern / #link native）：调用原生 C 函数。
	// 加它时必须同时更新 IR.md §4.2 的节点表。
	"call_native",
	// 2.0.1 控制流扩展（4）：stop / mem_txn / emit / on。
	// 加它们时必须同时更新 IR.md §4.2 的节点表与 schemas/ir.schema.json。
	"stop", "mem_txn", "emit", "on", "sleep", "until",
}

func readJSONFile(t *testing.T, rel string) map[string]any {
	t.Helper()
	path := filepath.Join(repoRoot(t), rel)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读不到 %s: %v", rel, err)
	}
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber() // 与 internal/ir 的约定一致：不让浮点悄悄进来
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("%s 不是合法 JSON: %v", rel, err)
	}
	return m
}

// TestIRSchemaRefsResolve 检查每条 $ref 都指得到 $defs 里的东西。
// 一条断了并不可怕，可怕的是它断在某个没人走的分支里 —— 校验器会静默放过那一类节点。
func TestIRSchemaRefsResolve(t *testing.T) {
	schema := readJSONFile(t, "schemas/ir.schema.json")
	defs, _ := schema["$defs"].(map[string]any)
	if len(defs) == 0 {
		t.Fatal("ir.schema.json 没有 $defs")
	}

	var refs []string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if r, ok := x["$ref"].(string); ok {
				refs = append(refs, r)
			}
			for _, vv := range x {
				walk(vv)
			}
		case []any:
			for _, vv := range x {
				walk(vv)
			}
		}
	}
	walk(schema)

	if len(refs) == 0 {
		t.Fatal("没有扫到任何 $ref，说明扫描逻辑本身坏了")
	}
	for _, r := range refs {
		const p = "#/$defs/"
		if !strings.HasPrefix(r, p) {
			t.Errorf("$ref %q 不是本地 $defs 引用（本 schema 不许引用外部文件）", r)
			continue
		}
		if _, ok := defs[strings.TrimPrefix(r, p)]; !ok {
			t.Errorf("$ref %q 指向的 $defs 条目不存在", r)
		}
	}
}

// nodeDefNames 是 kind → $defs 条目名。显式写出而不是由 kind 推导：
// call_json 的规范拼法是 nodeCallJSON（不是 nodeCallJson），推导会在这类缩写上出错，
// 而"守卫因为自己推导错而报假红"比没有守卫更糟。
var nodeDefNames = map[string]string{
	"seq": "nodeSeq", "decl": "nodeDecl", "if": "nodeIf", "match": "nodeMatch",
	"parallel": "nodeParallel", "retry": "nodeRetry", "try": "nodeTry",
	"return": "nodeReturn", "nop": "nodeNop", "destructure": "nodeDestructure",
	"lit": "nodeLit", "var": "nodeVar", "field": "nodeField", "index": "nodeIndex",
	"binop": "nodeBinop", "unop": "nodeUnop", "interp": "nodeInterp",
	"call_model": "nodeCallModel", "call_tool": "nodeCallTool",
	"call_json": "nodeCallJSON", "call_fn": "nodeCallFn", "cast": "nodeCast", "agent": "nodeAgent",
	"store_op": "nodeStoreOp", "exprstmt": "nodeExprStmt",
	"array_lit": "nodeArrayLit", "len": "nodeLen", "at": "nodeAt", "for": "nodeFor", "while": "nodeWhile", "break": "nodeBreak", "continue": "nodeContinue", "assign": "nodeAssign",
	// fallback（2.0.0）：错误边界，不是异常。
	"fallback": "nodeFallback",
	// call_native（反向融合 / extern / #link native）：调用原生 C 函数。
	"call_native": "nodeCallNative",
	// 2.0.1 控制流扩展（4）。显式写出，不靠驼峰推导。
	"stop": "nodeStop", "mem_txn": "nodeMemTxn", "emit": "nodeEmit", "on": "nodeOn",
	"sleep": "nodeSleep", "until": "nodeUntil",
}

// TestIRSchemaNodeKindsMatchSpec 把 schema 的节点清单与 IR.md §4.2 的清单对照。
func TestIRSchemaNodeKindsMatchSpec(t *testing.T) {
	schema := readJSONFile(t, "schemas/ir.schema.json")
	defs, _ := schema["$defs"].(map[string]any)

	// 正向：规格里的 20 种，每种都有一个定义，且 k 用 const 钉死。
	for _, name := range wantNodeKinds {
		defName, ok := nodeDefNames[name]
		if !ok {
			t.Fatalf("守卫自己漏了 %q 的 def 名映射", name)
		}
		d, ok := defs[defName].(map[string]any)
		if !ok {
			t.Errorf("$defs 缺少 %s（节点 %q）", defName, name)
			continue
		}
		props, _ := d["properties"].(map[string]any)
		k, _ := props["k"].(map[string]any)
		if c, _ := k["const"].(string); c != name {
			t.Errorf("%s 的 k const 是 %q，应为 %q", defName, c, name)
		}
		// 每种节点都必须能被 oneOf 认出来：const 而不是 enum，才谈得上判别。
		if _, ok := k["const"]; !ok {
			t.Errorf("%s 的 k 没有 const，oneOf 判别不成立", defName)
		}
	}

	// 反向：$defs 里不许有规格外的 node* 定义。
	var extra []string
	for name := range defs {
		// "node" 是 oneOf 的聚合分发器，不是某一种节点。
		if name == "node" || !strings.HasPrefix(name, "node") {
			continue
		}
		known := false
		for _, defName := range nodeDefNames {
			if defName == name {
				known = true
				break
			}
		}
		if !known {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	for _, name := range extra {
		t.Errorf("$defs 里有规格外的节点定义 %q（IR.md §4.2 只有 20 种）", name)
	}
}

// TestIRSchemaKindsCoveredByRefs 是这条守卫里最值钱的一条：
// **schema 认识的每一种节点，refs.go 的 appendNodeRefs 都必须有 case**。
//
// 反过来的失效是真事故型的：新增一种节点、在 refs 里忘了加 case，
// 那个节点的引用就永远不会被 V-2 检查 —— 悬空引用会一路走到运行期。
func TestIRSchemaKindsCoveredByRefs(t *testing.T) {
	fset := token.NewFileSet()
	path := filepath.Join(repoRoot(t), "internal/ir/refs.go")
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("解析 refs.go 失败: %v", err)
	}

	cases := map[string]bool{}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "appendNodeRefs" {
			return true
		}
		found = true
		ast.Inspect(fn, func(n ast.Node) bool {
			cc, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, e := range cc.List {
				if bl, ok := e.(*ast.BasicLit); ok && bl.Kind == token.STRING {
					cases[strings.Trim(bl.Value, `"`)] = true
				}
			}
			return true
		})
		return false
	})
	if !found {
		t.Fatal("refs.go 里找不到 appendNodeRefs（改名了？那这条守卫就失效了）")
	}
	if len(cases) < 15 {
		t.Fatalf("只从 appendNodeRefs 里扫到 %d 个 case，扫描逻辑可疑", len(cases))
	}

	for _, k := range wantNodeKinds {
		if !cases[k] {
			t.Errorf("节点 %q 在 refs.go 的 appendNodeRefs 里没有 case："+
				"它的引用不会被校验，悬空引用会一路走到运行期", k)
		}
	}
	// 反向：refs 里有但规格没有 = 规格漏了一种节点。
	want := map[string]bool{}
	for _, k := range wantNodeKinds {
		want[k] = true
	}
	var extra []string
	for k := range cases {
		if !want[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		t.Errorf("refs.go 处理了规格外的节点 %q（IR.md §4.2 没有它）", k)
	}
}

// TestIRSchemaAgentMaxStepsIsOptional 守 1.2.2 的一处**形状**变更：
// agent.max_steps 从必填变成可选，且 bounds 多声明了一个 unbounded。
//
// 为什么这两件事必须由守卫钉住：
//
//	agent.max_steps 可选 —— 源码里 `max_steps = unlimited` 时前端**省略整个键**
//	  （不写 null、不写假数字）。若 schema 还把它列为 required，这份产物就是
//	  "schema 说必填、编译器故意不填"——两边永远打架，而打架的结果通常是有人
//	  去改产物（把键加回来，写个 0 或 1000000），那正是要禁止的假数字。
//	  **键出现时**仍然 minimum 1：null / 0 不是"没有上界"的表达方式，
//	  "没有"只有一种表达方式，就是没有。
//
//	bounds.unbounded —— 它早就在产物里（ir-functions.json 的 golden 就有
//	  "unbounded": true），而 bounds 是 additionalProperties: false。也就是说
//	  任何按 schema 校验产物的人都会把**合法**产物判成非法。schema 与产物
//	  对不上时，schema 是错的。
func TestIRSchemaAgentMaxStepsIsOptional(t *testing.T) {
	schema := readJSONFile(t, "schemas/ir.schema.json")
	defs, _ := schema["$defs"].(map[string]any)

	agent, ok := defs["nodeAgent"].(map[string]any)
	if !ok {
		t.Fatal("$defs 里没有 nodeAgent")
	}
	props, _ := agent["properties"].(map[string]any)
	aprops, _ := props["agent"].(map[string]any)
	if aprops == nil {
		t.Fatal("nodeAgent 没有 agent 属性")
	}
	req := map[string]bool{}
	reqList, _ := aprops["required"].([]any)
	for _, r := range reqList {
		if name, ok := r.(string); ok {
			req[name] = true
		}
	}
	if req["max_steps"] {
		t.Error("agent.max_steps 不能是 required：`max_steps = unlimited` 时前端故意省略这个键")
	}
	// goal / tools 仍然必填：它们是 agent 的语义核心，缺了就没法执行。
	for _, must := range []string{"goal", "tools"} {
		if !req[must] {
			t.Errorf("agent.%s 必须仍然是 required（这是 agent 的语义核心）", must)
		}
	}
	// 出现时仍然是一个 ≥1 的整数：0 / null 不是"没有上界"的表达方式。
	ms, ok := aprops["properties"].(map[string]any)["max_steps"].(map[string]any)
	if !ok {
		t.Fatal("agent.max_steps 没有 properties 定义")
	}
	if ms["type"] != "integer" {
		t.Errorf("agent.max_steps 的 type 应为 integer，实际 %v", ms["type"])
	}
	if fmtNum(ms["minimum"]) != 1 {
		t.Errorf("agent.max_steps 的 minimum 应为 1，实际 %v", ms["minimum"])
	}

	// bounds.unbounded：布尔，且**不**放宽 additionalProperties。
	bounds, ok := defs["bounds"].(map[string]any)
	if !ok {
		t.Fatal("$defs 里没有 bounds")
	}
	if ap, ok := bounds["additionalProperties"].(bool); !ok || ap {
		t.Error("bounds 必须保持 additionalProperties: false（伪造字段要能被 schema 拦住），" +
			"要加字段就显式声明它")
	}
	bprops, _ := bounds["properties"].(map[string]any)
	unb, ok := bprops["unbounded"].(map[string]any)
	if !ok {
		t.Fatal("bounds 没有声明 unbounded，而产物里已经在写它了" +
			"（additionalProperties: false 会因此把合法产物判成非法）")
	}
	if unb["type"] != "boolean" {
		t.Errorf("bounds.unbounded 的 type 应为 boolean，实际 %v", unb["type"])
	}
	// 说明"为什么这个字段存在"不能为空：unbounded 的存在理由不是"前端顺手写的"，
	// 而是"这几个数字不再是上界"。描述缺失时读 schema 的人只知道有它，不知道为什么。
	if d, _ := unb["description"].(string); len(d) < 20 {
		t.Errorf("bounds.unbounded 缺少足够的 description（它解释这几个 bound 何时不再是上界）")
	}
}

// fmtNum 把 json.Number / float64 / int 收敛成 int，只为断言 minimum。
func fmtNum(v any) int {
	switch x := v.(type) {
	case json.Number:
		n, _ := x.Int64()
		return int(n)
	case float64:
		return int(x)
	case int:
		return x
	}
	return -1
}

// TestIRDocFieldsMatchSchema 检查顶层文档的字段集：Go 结构体能装的 == schema 要求的。
//
// 少了：加载器会把一个合法字段静默丢掉（最坏的兼容性 bug）。
// 多了：Go 结构体里有个没人定义的字段，说明规格已改而代码没跟。
func TestIRDocFieldsMatchSchema(t *testing.T) {
	fset := token.NewFileSet()
	path := filepath.Join(repoRoot(t), "internal/ir/types.go")
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("解析 types.go 失败: %v", err)
	}

	goFields := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "Doc" {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		for _, fld := range st.Fields.List {
			if len(fld.Names) == 0 {
				continue
			}
			tag := ""
			if fld.Tag != nil {
				tag = fld.Tag.Value
			}
			if i := strings.Index(tag, `json:"`); i >= 0 {
				rest := tag[i+len(`json:"`):]
				if j := strings.Index(rest, `"`); j >= 0 {
					name := rest[:j]
					if k := strings.Index(name, ","); k >= 0 {
						name = name[:k]
					}
					if name != "" && name != "-" {
						goFields[name] = true
					}
				}
			}
		}
		return false
	})
	if len(goFields) == 0 {
		t.Fatal("没能从 types.go 的 Doc 里解析出任何 json tag")
	}

	schema := readJSONFile(t, "schemas/ir.schema.json")
	props, _ := schema["properties"].(map[string]any)
	req, _ := schema["required"].([]any)

	for k := range goFields {
		if _, ok := props[k]; !ok {
			t.Errorf("Doc 有字段 %q，schema 顶层没有", k)
		}
	}
	for k := range props {
		if !goFields[k] {
			t.Errorf("schema 顶层有 %q，Doc 装不下（加载时会静默丢弃）", k)
		}
	}
	for _, r := range req {
		name, _ := r.(string)
		if !goFields[name] {
			t.Errorf("schema 把 %q 标为必填，但 Doc 没有这个字段", name)
		}
	}
}
