package ir

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"hermes/internal/diag"
)

// E5007 的**产物侧**防线（validate.go::checkRetryIdempotency）的可执行形式。
//
// 编译器闸门挂在源码 AST 上，只保护 hermes ir 产出的 IR。IR 却是可分发、
// 可签名、可手改的产物：hermes run 接受任意 IR 文件，而 engine 的 retry
// 只重跑 body、完全不读 tools[].idem.mode。所以「retry 内」必须在**节点图**
// 上判定 —— 从 retry.body 沿引用边可达。
//
// 这些用例直接调 checkRetryIdempotency，不跑完整 Validate：最小 Doc 过不了
// V-1..V-12（bounds / exports / var 双向等），那样红了也说不清是哪条约束。
// 完整链路另由 TestLoadRejectsTamperedRetryIR 用真实 golden 覆盖。

func riInt(x int) *int       { return &x }
func riStr(x string) *string { return &x }

// riDoc 固定给两个 tool：pay 的幂等模式由用例指定，safe 恒 readonly。
// 这样「外层证明过、内层没证明」这类嵌套形状才能精确只报一条。
func riDoc(mode string, nodes []Node) *Doc {
	return &Doc{
		Tools: []Tool{
			{Name: "pay", Method: "POST", Idem: Idem{Mode: mode}},
			{Name: "safe", Method: "GET", Idem: Idem{Mode: "readonly"}},
		},
		Workflows: []Workflow{{Nodes: nodes}},
	}
}

func riCheck(d *Doc) []diag.Diag {
	v := newValidator(nil)
	v.doc = d
	v.buildIndex()
	v.checkRetryIdempotency(0)
	return v.finish()
}

func riSeq(id int, kids ...int) Node  { return Node{ID: id, K: "seq", Kids: kids} }
func riDecl(id, e int) Node           { return Node{ID: id, K: "decl", E: riInt(e)} }
func riCall(id int, tool string) Node { return Node{ID: id, K: "call_tool", Tool: riStr(tool)} }
func riRetry(id, body int) Node {
	return Node{ID: id, K: "retry", N: riInt(2), DelayMS: riInt(0), Body: riInt(body)}
}

func countRetryUnproven(ds []diag.Diag) int {
	n := 0
	for _, d := range ds {
		if d.Code == diag.CodeRetryUnproven {
			n++
		}
	}
	return n
}

func TestRetryIdempotencyOnNodeGraph(t *testing.T) {
	// 基本形状：retry(0) → body seq(1) → decl(2, e=3) → call_tool(3)。
	base := func(tool string) []Node {
		return []Node{riRetry(0, 1), riSeq(1, 2), riDecl(2, 3), riCall(3, tool)}
	}

	// call_tool 的 args.body 里再嵌一个 call_tool：外层已证明、内层没有。
	outer := Node{
		ID: 3, K: "call_tool", Tool: riStr("safe"),
		Args: json.RawMessage(`{"body":4}`),
	}

	cases := []struct {
		name  string
		mode  string
		nodes []Node
		want  int
	}{
		{"none 必须报", "none", base("pay"), 1},
		{"mode 缺失（手改删掉）必须报", "", base("pay"), 1},
		{"不认识的 mode 按无证明处理", "garbage", base("pay"), 1},
		{"readonly 是证明", "readonly", base("pay"), 0},
		{"idem_true 是证明", "idem_true", base("pay"), 0},
		{"idem_key 是证明", "idem_key", base("pay"), 0},
		{"已证明的 tool 不受 pay 的模式影响", "none", base("safe"), 0},

		{
			name: "call_tool 在 retry 子树外不报",
			mode: "none",
			nodes: []Node{
				riSeq(0, 1, 3), riRetry(1, 2), riSeq(2), riCall(3, "pay"),
			},
			want: 0,
		},
		{
			name: "藏在 fallback.main（表达式槽位）",
			mode: "none",
			nodes: []Node{
				riRetry(0, 1), riSeq(1, 2),
				{ID: 2, K: "fallback", Main: riInt(3), Body: riInt(4)},
				riCall(3, "pay"), riSeq(4),
			},
			want: 1,
		},
		{
			name: "藏在 parallel.branches",
			mode: "none",
			nodes: []Node{
				riRetry(0, 1), riSeq(1, 2),
				{ID: 2, K: "parallel", Branches: []int{3}},
				riCall(3, "pay"),
			},
			want: 1,
		},
		{
			name: "藏在 if.then",
			mode: "none",
			nodes: []Node{
				riRetry(0, 1), riSeq(1, 2),
				{ID: 2, K: "if", Then: riInt(3), Else: riInt(4)},
				riCall(3, "pay"), riSeq(4),
			},
			want: 1,
		},
		{
			name:  "藏在 call_tool.args.body（表达式参数里嵌套）",
			mode:  "none",
			nodes: []Node{riRetry(0, 1), riSeq(1, 3), outer, riCall(4, "pay")},
			want:  1,
		},
		{
			name:  "嵌套 retry 只报一条（不是更严格，是噪声）",
			mode:  "none",
			nodes: []Node{riRetry(0, 1), riRetry(1, 2), riSeq(2, 3), riCall(3, "pay")},
			want:  1,
		},
		{
			name:  "两个不同的坏调用各报一条",
			mode:  "none",
			nodes: []Node{riRetry(0, 1), riSeq(1, 2, 3), riCall(2, "pay"), riCall(3, "pay")},
			want:  2,
		},
		{
			name:  "tool 名字不存在：交给 V-2，不抢活",
			mode:  "none",
			nodes: base("ghost"),
			want:  0,
		},
		{
			name:  "retry 缺 body 不能崩",
			mode:  "none",
			nodes: []Node{{ID: 0, K: "retry"}},
			want:  0,
		},
		{
			name: "attempt_ctx.retry_node 是反向注解，不是执行边",
			mode: "none",
			nodes: []Node{
				riRetry(0, 1), riSeq(1),
				{ID: 2, K: "call_tool", Tool: riStr("pay"),
					Att: &AttemptCtx{RetryNode: 0}},
			},
			want: 0,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ds := riCheck(riDoc(c.mode, c.nodes))
			if got := countRetryUnproven(ds); got != c.want {
				t.Fatalf("E_IR_RETRY_UNPROVEN 应恰好 %d 条，实际 %d 条：\n%s",
					c.want, got, diag.RenderAll(ds))
			}
		})
	}
}

// TestLoadRejectsTamperedRetryIR 用**真实 golden** 走完整 Load 链路。
//
// 只做一次字符串替换而不是 Unmarshal→Marshal：后者会重排所有键，可能触发
// V-8 schema canonical 化或哈希复核，那样红了就说不清是不是这条新闸。
// 单点变异保证「原产物合法、只改一个字段就必须被拦」。
func TestLoadRejectsTamperedRetryIR(t *testing.T) {
	raw, err := os.ReadFile("../../schemas/golden/ir-idemkey.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, ds := Load(raw); len(ds) != 0 {
		t.Fatalf("原 golden 应完全合法，实际：\n%s", diag.RenderAll(ds))
	}

	const from = `"mode":"idem_key"`
	if strings.Count(string(raw), from) != 1 {
		t.Fatalf("golden 里 %s 应恰好出现 1 次，实际 %d 次",
			from, strings.Count(string(raw), from))
	}

	checkLoad := func(label string, data []byte, want int) {
		t.Helper()
		_, ds := Load(data)
		if got := countRetryUnproven(ds); got != want {
			t.Fatalf("%s：E_IR_RETRY_UNPROVEN 应 %d 条，实际 %d 条：\n%s",
				label, want, got, diag.RenderAll(ds))
		}
		if want == 0 && len(ds) != 0 {
			t.Fatalf("%s 不该有任何诊断，实际：\n%s", label, diag.RenderAll(ds))
		}
	}

	checkLoad("改成另一个证明值（idem_true）", []byte(
		strings.Replace(string(raw), from, `"mode":"idem_true"`, 1)), 0)
	checkLoad("改成 none（未证明）", []byte(
		strings.Replace(string(raw), from, `"mode":"none"`, 1)), 1)
	checkLoad("整个删掉 mode（未证明）", []byte(
		strings.Replace(string(raw), `,"mode":"idem_key"`, ``, 1)), 1)
}
