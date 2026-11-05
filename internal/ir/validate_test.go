package ir

import (
	"encoding/json"
	"strings"
	"testing"

	"hermes/internal/diag"
)

// baseIR 是一份**最小合法 IR**。所有反例都是在它上面做单点变异，
// 这样每个测试只证明"这一条 V 检查"有效，不会因为 fixture 本身别处不合法
// 而假红（一个测试同时踩两个约束，红了也说不清是哪条）。
const baseIR = `{
  "ir_version": 1,
  "unit": {"name":"t","entry_path":"t.hermes","language_version":1,
           "modules":[{"path":"t.hermes","sha256":"0000000000000000000000000000000000000000000000000000000000000000","builtin":false}]},
  "compiler": {"name":"hermes-parse","version":"0.1","parser_abi":1},
  "types": [],
  "backends": [{"name":"b","kind":"openai","addr":{"src":"literal","value":"http://127.0.0.1:1"}}],
  "tools": [],
  "schemas": [],
  "workflows": [{
    "name":"f","exported":true,"is_entry":true,"backend":"b",
    "input":null,
    "returns":{"k":"string"},
    "params":[],
    "root":0,
    "nodes":[{"id":0,"k":"seq","loc":{"m":0,"l":1,"c":1,"el":1,"ec":10},"kids":[]}],
    "vars":[],
    "bounds":{"max_steps":1,"max_spans":0,"max_model_calls":0,
              "max_tool_calls":0,"max_concurrency":1,"max_retry_expansion":0},
    "economics":{},
    "interp_deps":[],
    "loc":{"m":0,"l":1,"c":1,"el":1,"ec":10}
  }],
  "exports":{"workflows":["f"],"types":[],"backends":["b"],"tools":[]},
  "imports":[],
  "requires":[],
  "diag_counts":{"error":0,"warning":0}
}`

func loadBase(t *testing.T) *Doc {
	t.Helper()
	var d Doc
	if err := json.Unmarshal([]byte(baseIR), &d); err != nil {
		t.Fatalf("baseIR 反序列化失败（types.go 的 json tag 可能有错）：%v", err)
	}
	return &d
}

func hasCode(ds []diag.Diag, code string) bool {
	for _, d := range ds {
		if d.Code == code {
			return true
		}
	}
	return false
}

func codes(ds []diag.Diag) string {
	var out []string
	for _, d := range ds {
		out = append(out, d.Code)
	}
	return strings.Join(out, ",")
}

// TestBaseIRIsValid 先证明 fixture 干净 —— 否则下面所有反例都不可信。
func TestBaseIRIsValid(t *testing.T) {
	ds := Validate(loadBase(t))
	if len(ds) != 0 {
		t.Fatalf("baseIR 应无诊断，实际 %d 条：%s", len(ds), diag.RenderAll(ds))
	}
}

// V-1：nodes[i].id 必须等于 i（IR.md：扁平数组，id == 下标）。
func TestVDupID(t *testing.T) {
	d := loadBase(t)
	d.Workflows[0].Nodes = append(d.Workflows[0].Nodes,
		Node{ID: 7, K: "nop", Loc: Loc{M: 0, L: 2}})
	ds := Validate(d)
	if !hasCode(ds, diag.CodeDupID) {
		t.Errorf("期望 E_IR_DUP_ID（节点 id 7 出现在下标 1），实际：%s", codes(ds))
	}
}

// V-2：kids 指向不存在的节点。
func TestVDanglingRef(t *testing.T) {
	d := loadBase(t)
	d.Workflows[0].Nodes[0].Kids = []int{99}
	ds := Validate(d)
	if !hasCode(ds, diag.CodeDanglingRef) {
		t.Errorf("期望 E_IR_DANGLING_REF（kids 指向 99），实际：%s", codes(ds))
	}
}

// V-2 哨兵：`var: -1` 是 input（隐式根变量，不在 vars[] 里），**必须放行**。
// 这条如果错，所有用到输入参数的 IR 都会被判悬空 —— 那是灾难级的误报。
func TestVInputSentinelIsAllowed(t *testing.T) {
	d := loadBase(t)
	// 加一个 var 节点读 input（-1），再加一个 return 引用它
	d.Workflows[0].Nodes = []Node{
		{ID: 0, K: "seq", Loc: Loc{M: 0, L: 1}, Kids: []int{1, 2}},
		{ID: 1, K: "var", Loc: Loc{M: 0, L: 2}, Var: intp(-1)},
		{ID: 2, K: "return", Loc: Loc{M: 0, L: 3}, E: intp(1)},
	}
	ds := Validate(d)
	if hasCode(ds, diag.CodeDanglingRef) {
		t.Fatalf("var=-1（input 哨兵）被误判为悬空：%s", diag.RenderAll(ds))
	}
	if len(ds) != 0 {
		t.Errorf("应无诊断，实际：%s", codes(ds))
	}
}

// V-3：环。seq(0) -> seq(1) -> seq(0)
func TestVCycle(t *testing.T) {
	d := loadBase(t)
	d.Workflows[0].Nodes = []Node{
		{ID: 0, K: "seq", Loc: Loc{M: 0, L: 1}, Kids: []int{1}},
		{ID: 1, K: "seq", Loc: Loc{M: 0, L: 2}, Kids: []int{0}},
	}
	ds := Validate(d)
	if !hasCode(ds, diag.CodeCycle) {
		t.Errorf("期望 E_IR_CYCLE，实际：%s", codes(ds))
	}
}

// V-4：root 不是 seq。
func TestVBadRoot(t *testing.T) {
	d := loadBase(t)
	d.Workflows[0].Nodes[0].K = "nop"
	ds := Validate(d)
	if !hasCode(ds, diag.CodeBadRoot) {
		t.Errorf("期望 E_IR_BAD_ROOT（root 是 nop），实际：%s", codes(ds))
	}
}

// V-5：var.decl 指向的节点不是 let。
func TestVVarMismatch(t *testing.T) {
	d := loadBase(t)
	d.Workflows[0].Vars = []Var{{ID: 0, Name: "x", T: TypeExpr{K: "int"}, Decl: 0, Scope: 0}}
	ds := Validate(d)
	if !hasCode(ds, diag.CodeVarMismatch) {
		t.Errorf("期望 E_IR_VAR_MISMATCH（decl 指向 seq 不是 let），实际：%s", codes(ds))
	}
}

// V-6：loc.m 越界。**这条必须最先跑**，否则渲染诊断时取 modules[m] 会崩。
func TestVBadLoc(t *testing.T) {
	d := loadBase(t)
	d.Workflows[0].Nodes[0].Loc.M = 9
	ds := Validate(d)
	if !hasCode(ds, diag.CodeBadLoc) {
		t.Errorf("期望 E_IR_BAD_LOC（m=9 但只有 1 个模块），实际：%s", codes(ds))
	}
	// 关键：校验器不能因为 loc 越界而崩，且诊断仍可渲染。
	for _, x := range ds {
		_ = x.Render()
	}
}

// V-9：exported 与 exports.workflows 不一致（**双向**）。
func TestVExportMismatchForward(t *testing.T) {
	d := loadBase(t)
	d.Exports.Workflows = nil // 表里没有，但 workflow 上 exported=true
	ds := Validate(d)
	if !hasCode(ds, diag.CodeExportMismatch) {
		t.Errorf("期望 E_IR_EXPORT_MISMATCH（exported=true 但表里没有），实际：%s", codes(ds))
	}
}

func TestVExportMismatchBackward(t *testing.T) {
	d := loadBase(t)
	d.Exports.Workflows = append(d.Exports.Workflows, "ghost")
	ds := Validate(d)
	if !hasCode(ds, diag.CodeExportMismatch) {
		t.Errorf("期望 E_IR_EXPORT_MISMATCH（表里有 ghost 但 IR 里没有），实际：%s", codes(ds))
	}
}

// V-10：两个 entry。
func TestVTwoEntries(t *testing.T) {
	d := loadBase(t)
	w2 := d.Workflows[0]
	w2.Name = "g"
	w2.IsEntry = true
	d.Workflows = append(d.Workflows, w2)
	d.Exports.Workflows = []string{"f", "g"}
	ds := Validate(d)
	if !hasCode(ds, diag.CodeEntry) {
		t.Errorf("期望 E_IR_ENTRY（两个 entry），实际：%s", codes(ds))
	}
}

// V-10：entry 未导出。
func TestVEntryNotExported(t *testing.T) {
	d := loadBase(t)
	d.Workflows[0].Exported = false
	d.Exports.Workflows = nil
	ds := Validate(d)
	if !hasCode(ds, diag.CodeEntry) {
		t.Errorf("期望 E_IR_ENTRY（entry 未 export），实际：%s", codes(ds))
	}
}

// V-11：四命名空间各自唯一。
func TestVSymConflict(t *testing.T) {
	d := loadBase(t)
	d.Exports.Workflows = []string{"f", "f"}
	ds := Validate(d)
	if !hasCode(ds, diag.CodeSymConflict) {
		t.Errorf("期望 E_IR_SYM_CONFLICT（exports.workflows 里有重复），实际：%s", codes(ds))
	}
}

// 阶段 3：派生字段与复算不符（bounds 被抬高）。
func TestVBoundsDrift(t *testing.T) {
	d := loadBase(t)
	d.Workflows[0].Bounds.MaxSpans = 100000 // 远超 64*max_steps+1
	ds := Validate(d)
	if !hasCode(ds, diag.CodeRecomputeDrift) {
		t.Errorf("期望 E_IR_RECOMPUTE_DRIFT（max_spans 超上界），实际：%s", codes(ds))
	}
}

// 诊断上限：恶意 IR 不能让诊断无限膨胀。
func TestDiagCap(t *testing.T) {
	d := loadBase(t)
	// 造 30 个悬空引用（每个 kids 一条）
	kids := make([]int, 30)
	for i := range kids {
		kids[i] = 900 + i
	}
	d.Workflows[0].Nodes[0].Kids = kids
	ds := Validate(d, WithMaxDiags(5))
	dangling := 0
	for _, x := range ds {
		if x.Code == diag.CodeDanglingRef {
			dangling++
		}
	}
	if dangling > 5 {
		t.Errorf("WithMaxDiags(5) 下报了 %d 条悬空，超上限", dangling)
	}
	if !hasCode(ds, diag.CodePanelTruncated) {
		t.Errorf("被丢弃的诊断必须汇总成一条 W_IR_PANEL_TRUNCATED，实际：%s", codes(ds))
	}
}

// 中文诊断也要能出（可本地化是 diag 包的设计目标）。
func TestLangZH(t *testing.T) {
	d := loadBase(t)
	d.Workflows[0].Nodes[0].Kids = []int{99}
	ds := Validate(d, WithLang(diag.LangZH))
	if len(ds) == 0 {
		t.Fatal("应至少一条诊断")
	}
	found := false
	for _, x := range ds {
		if strings.Contains(x.Message, "指向") {
			found = true
		}
	}
	if !found {
		t.Errorf("WithLang(zh) 没有得到中文文案：%s", diag.RenderAll(ds))
	}
}

// 引用条数断言：新增 node kind 却没在 appendNodeRefs 里登记时，条数会少。
// 这个数字来自 baseIR 的手工核对（root 1 条 + backend 1 条 + returns 是 string 无引用）。
func TestRefCountIsPinned(t *testing.T) {
	d := loadBase(t)
	refs := collectRefs(d)
	// baseIR: workflows[0].backend(1) + workflows[0].root(1) = 2
	if len(refs) != 2 {
		t.Errorf("baseIR 的引用条数 = %d，期望 2（backend + root）。"+
			"若你新增了 node kind 却没在 appendNodeRefs 里登记，这里会变。", len(refs))
	}
}

func intp(v int) *int { return &v }

// TestNonRecursiveCallFnIsRejectedAtLoad 守：call_fn 只允许指向递归函数。
//
// 非递归函数必须在编译期展开；加载期放行一条指向它的 call_fn，等于允许
// 旧产物或手改 IR 悄悄恢复动态派发 —— 而那正是"编译型/静态上界"要排除的。
func TestNonRecursiveCallFnIsRejectedAtLoad(t *testing.T) {
	d := loadBase(t)
	fname := "g"
	loc := Loc{M: 0, L: 1, C: 1, EL: 1, EC: 2}
	d.Functions = []Function{{
		Name: fname, Recursive: false,
		Params:  []FuncParam{{N: "x", T: TypeExpr{K: "int"}}},
		Returns: TypeExpr{K: "int"}, Root: 0,
		Nodes: []Node{
			{ID: 0, K: "return", E: intp(1), Loc: loc},
			{ID: 1, K: "lit", LitT: strptr("int"), LitV: json.RawMessage("1"), Loc: loc},
		},
		Vars: []Var{},
	}}
	d.Workflows[0].Nodes = []Node{{ID: 0, K: "call_fn", Fn: &fname, Loc: loc}}
	d.Workflows[0].Root = 0

	ds := Validate(d)
	if !hasCode(ds, diag.CodeDynamicCall) {
		t.Fatalf("非递归 call_fn 必须在加载期被拒绝，实际诊断：%s", codes(ds))
	}

	d.Functions[0].Recursive = true
	ds = Validate(d)
	if hasCode(ds, diag.CodeDynamicCall) {
		t.Fatalf("递归 call_fn 应放行（深度上限兜底），实际：%s", codes(ds))
	}
}
