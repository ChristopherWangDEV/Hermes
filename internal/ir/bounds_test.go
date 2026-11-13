package ir

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGoldenBoundsMatchRecompute 用**独立实现**复核 golden 里的 max_spans。
//
// codegen 在 C++ 里按 §5.1 算，这里在 Go 里再算一遍。两边用不同语言、
// 不同遍历顺序得到同一个数，才算真的钉住了那条代数；只靠 golden 逐字节
// 比对做不到这件事 —— 换了实现后 golden 会跟着变，而"变对了吗"没人知道。
func TestGoldenBoundsMatchRecompute(t *testing.T) {
	goldens, err := filepath.Glob("../../schemas/golden/ir-*.json")
	if err != nil || len(goldens) == 0 {
		t.Fatalf("扫不到 IR golden: %v", err)
	}

	for _, path := range goldens {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			d, ds := Load(data)
			if len(ds) != 0 {
				t.Fatalf("golden 自己不合格：%s", renderDiags(ds))
			}
			for i := range d.Workflows {
				if msg := CheckBounds(&d.Workflows[i]); msg != "" {
					t.Errorf("%s", msg)
				}
			}
		})
	}
}

// TestRecomputeRejectsUnknownKind 证明新增节点种类时复算会**报错**而不是返回 0。
//
// 返回 0 是最坏的退化：上界变小 → 运行期正确行为被判成 compiler bug，
// 而且没有任何测试会红。
//
// 注意断言的是 boundUnrecognized（-2）而**不是** -1：这里必须区分两种"非数字"。
// "我不认识这个 kind" 是编译器 bug（codegen 加了 kind，复算没跟上），必须响亮；
// "这个值静态算不出来"（while 的轮数、不限步数的 agent）是合法的，只能是
// boundUnknown（-1）。混用任何一个，另一类就会被误判：把前者当 -1，新增 kind
// 那天会静默跳过校验；把后者当 -2，一个合法的 while 会被报成编译器 bug。
func TestRecomputeRejectsUnknownKind(t *testing.T) {
	w := &Workflow{
		Name: "x",
		Root: 0,
		Nodes: []Node{{
			ID: 0, K: "future_kind", Loc: Loc{M: 0, L: 1, C: 1, EL: 1, EC: 2},
		}},
	}
	if got := RecomputeMaxSpans(w); got != boundUnrecognized {
		t.Fatalf("未知节点种类应返回 boundUnrecognized(%d)，实际 %d",
			boundUnrecognized, got)
	}
	if msg := CheckBounds(w); msg == "" {
		t.Fatal("未知节点种类时 CheckBounds 必须报出，而不是静默通过")
	}
}

// TestRecomputeAssignSpanIsZero 把「assign 被独立复算认识」单独钉住，不依赖
// golden 的形态间接覆盖，也不破坏 TestRecomputeRejectsUnknownKind。
//
// 形状就是路线 A 的归约产物：root=seq(decl, if)，if 的 then/else 各是
// seq[assign]。两个 assign.e 都指向纯 lit，按 bounds.go 的独立复算
// （span(assign) = span(e) = 0）与 C++ 的代数（span_[assign] = 0）结果都是 0，
// 所以 max_spans = 1 + max(分支) = 1。
// 若复算不认识 assign（落到 default 取 boundUnrecognized），CheckBounds 会
// 直接报"无法识别"，与声明的 1 对不上 —— 这条守卫钉的正是「认识这个 kind」。
// 它**不**钉「assign 的 span 恒 0」：bounds.go 故意按 IR 语义遍历 e，
// 将来放宽纯度时它会先报 drift（见该文件注释）。
func TestRecomputeAssignSpanIsZero(t *testing.T) {
	ptr := func(i int) *int { return &i }
	w := &Workflow{
		Name: "assign_span",
		Root: 0,
		Nodes: []Node{
			{ID: 0, K: "seq", Kids: []int{1, 3}},
			{ID: 1, K: "decl", Var: ptr(0), E: ptr(2)},
			{ID: 2, K: "lit"},
			{ID: 3, K: "if", C: ptr(4), Then: ptr(5), Else: ptr(7)},
			{ID: 4, K: "lit"},
			{ID: 5, K: "seq", Kids: []int{6}},
			{ID: 6, K: "assign", Var: ptr(0), E: ptr(2)},
			{ID: 7, K: "seq", Kids: []int{8}},
			{ID: 8, K: "assign", Var: ptr(0), E: ptr(2)},
		},
		Vars: []Var{{ID: 0, Name: "__hm_inline_r_0", T: TypeExpr{K: "int"}, Decl: 1}},
		Bounds: Bounds{
			MaxSteps:       9,
			MaxSpans:       1,
			MaxConcurrency: 1,
		},
	}
	if got := RecomputeMaxSpans(w); got != 1 {
		t.Fatalf("assign.e 是纯 lit、if 取 1+max(分支)，复算应为 1；实际 %d", got)
	}
	if msg := CheckBounds(w); msg != "" {
		t.Fatalf("CheckBounds 应通过：%s", msg)
	}
}

// TestSeqDoesNotSwallowUnknown 是这次改动的**动机本身**的回归测试。
//
// 旧实现里 seq 是裸的 `v += span(k)`，而 while 落到 default 得 -1，于是
// seq[call_model, while, call_tool] 算成 1 + (-1) + 1 = 1 —— 一个看起来完全
// 正常的正数，却根本不是上界。validate.go 拿它去比声明值，只会在"恰好不等"
// 时报一条引用了错误期望值的 drift，否则静默通过。
//
// 现在 while 是 boundUnknown，且每一个聚合点都过 absorbSum：未知必须毒化
// 整个表达式。**显式断言结果不是 >= 0**，因为那才是这个 bug 的真实形态。
func TestSeqDoesNotSwallowUnknown(t *testing.T) {
	w := &Workflow{
		Name: "seq_with_while",
		Root: 0,
		Nodes: []Node{
			{ID: 0, K: "seq", Kids: []int{1, 2, 3}},
			{ID: 1, K: "call_model"},
			{ID: 2, K: "while", C: intp(4), Body: intp(1)},
			{ID: 3, K: "call_tool"},
			{ID: 4, K: "var"},
		},
		Bounds: Bounds{MaxSteps: 9, MaxSpans: 1, MaxConcurrency: 1},
	}
	got := RecomputeMaxSpans(w)
	if got >= 0 {
		t.Fatalf("seq 里含 while 时复算不得给出正数（那会被当成上界）：实际 %d", got)
	}
	if got != boundUnknown {
		t.Fatalf("while 是合法地不可静态定界，应为 boundUnknown(%d)；实际 %d",
			boundUnknown, got)
	}
	if msg := CheckBounds(w); msg != "" {
		t.Fatalf("未知不等于出错，CheckBounds 应跳过而不是报错：%s", msg)
	}
	if m, tl := RecomputeCalls(w); m >= 0 || tl >= 0 {
		t.Fatalf("seq 里的 while 也必须把两个调用上界拉成未知；实际 %d/%d", m, tl)
	}
}

// TestWhileCallsAreUnknown 单独钉住"while 的调用次数也不可静态定界"。
//
// 两个字段必须各自诚实：只把 span 改成未知、调用次数却照旧返回 body 的数字，
// 是最容易漏的一半 —— RecomputeCalls 旧实现根本没有 default，任何漏掉的
// kind 都静默变成 0/0（"没有模型调用"）。
func TestWhileCallsAreUnknown(t *testing.T) {
	w := &Workflow{
		Name: "while_only",
		Root: 0,
		Nodes: []Node{
			{ID: 0, K: "while", C: intp(1), Body: intp(2)},
			{ID: 1, K: "var"},
			{ID: 2, K: "call_model"},
		},
		Bounds: Bounds{MaxSteps: 3, MaxSpans: 2, MaxConcurrency: 1},
	}
	if m, tl := RecomputeCalls(w); m != boundUnknown || tl != boundUnknown {
		t.Fatalf("while 的调用次数应为 boundUnknown/boundUnknown；实际 %d/%d", m, tl)
	}
	if msg := CheckBounds(w); msg != "" {
		t.Fatalf("两个字段都未知时应跳过校验：%s", msg)
	}
}

// TestUnrecognizedPropagatesThroughSeq 证明**编译器 bug 的哨兵不会被兄弟节点
// 吞掉**：seq 里混一个不认识的 kind，结果必须是 boundUnrecognized，而不是
// 剩下几个已知兄弟的和。
func TestUnrecognizedPropagatesThroughSeq(t *testing.T) {
	w := &Workflow{
		Name: "seq_with_bogus",
		Root: 0,
		Nodes: []Node{
			{ID: 0, K: "seq", Kids: []int{1, 2, 3}},
			{ID: 1, K: "call_model"},
			{ID: 2, K: "future_kind"},
			{ID: 3, K: "call_tool"},
		},
		Bounds: Bounds{MaxSteps: 9, MaxSpans: 2, MaxConcurrency: 1},
	}
	if got := RecomputeMaxSpans(w); got != boundUnrecognized {
		t.Fatalf("不认识的 kind 应把整个 seq 拉成 boundUnrecognized(%d)；实际 %d",
			boundUnrecognized, got)
	}
	if msg := CheckBounds(w); msg == "" {
		t.Fatal("含不认识的 kind 时必须报出，而不是静默通过")
	}
}

// TestRecomputeCallsRejectsUnknownKind 钉住 RecomputeCalls 一侧的同一件事。
//
// 旧实现**没有 default**：不认识的 kind 落成零值 pair{0,0}，即"没有模型调用、
// 没有工具调用"—— 一个把上界变小的静默退化。现在它是 boundUnrecognized。
// 断言写成"不是 0 且就是那个哨兵"：只断言"负数"会让 -1 和 -2 混过去，
// 而两者混用正是这次要修的东西。
func TestRecomputeCallsRejectsUnknownKind(t *testing.T) {
	w := &Workflow{
		Name:  "bogus_calls",
		Root:  0,
		Nodes: []Node{{ID: 0, K: "future_kind"}},
	}
	m, tl := RecomputeCalls(w)
	if m != boundUnrecognized || tl != boundUnrecognized {
		t.Fatalf("不认识的 kind 应返回 boundUnrecognized(%d)/boundUnrecognized；实际 %d/%d",
			boundUnrecognized, m, tl)
	}
	if m == 0 || tl == 0 {
		t.Fatalf("返回 0 等于宣称「没有调用」，那是静默缩小上界；实际 %d/%d", m, tl)
	}
}

// TestUnlimitedAgentStepsIsUnknown 钉住 `max_steps = unlimited` 在 Go 侧的契约。
//
// 前端（native/cpp c344ebd）在 unlimited 时**省略整个 max_steps 键**，并把
// 该 workflow 标 unbounded；它仍然写数字（本例是 max_spans=2 / 1 / 1，
// 即"只跑一轮"的 (1+k)×1），但那**不是**上界。所以复算必须说 boundUnknown，
// 而 CheckBounds 必须跳过 —— 不能拿"跑一轮"的数字去和声明值比，也不能因为
// 声明值不等于我们的推演就报 drift。
//
// AgentSpec.MaxSteps 现在是 *int，所以"缺键"有两种形态，**两种都必须未知**：
// nil（正常产物：键根本不存在）与 ptr(0)（手改/损坏的 IR）。后者不能变成
// "跑 0 步"这种看起来正常的上界。
func TestUnlimitedAgentStepsIsUnknown(t *testing.T) {
	goal := 1
	build := func(steps *int) *Workflow {
		return &Workflow{
			Name: "agent_unlimited",
			Root: 0,
			Nodes: []Node{
				{ID: 0, K: "agent", Agent: &AgentSpec{
					Goal:     &goal,
					Tools:    []string{"orders"},
					MaxSteps: steps,
					Backend:  "",
					// MaxParallelTools 缺省 → k = 1
				}},
				{ID: 1, K: "lit", LitT: strptr("string"), LitV: json.RawMessage(`"hi"`)},
			},
			Bounds: Bounds{
				// 前端实测值：一轮的量，不是上界。
				MaxSteps:       2,
				MaxSpans:       2,
				MaxModelCalls:  1,
				MaxToolCalls:   1,
				MaxConcurrency: 1,
			},
		}
	}
	for _, c := range []struct {
		name  string
		steps *int
	}{
		{"absent_key", nil}, // 正常产物：`max_steps = unlimited` 时键整个不存在
		{"hand_edited_zero", intp(0)},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := build(c.steps)
			if got := RecomputeMaxSpans(w); got != boundUnknown {
				t.Fatalf("max_steps 不限时 span 上界应为 boundUnknown(%d)；实际 %d",
					boundUnknown, got)
			}
			if m, tl := RecomputeCalls(w); m != boundUnknown || tl != boundUnknown {
				t.Fatalf("max_steps 不限时调用上界应为 boundUnknown；实际 %d/%d", m, tl)
			}
			if msg := CheckBounds(w); msg != "" {
				t.Fatalf("不可知必须跳过而不是报错（前端写的 2/1/1 不该被判 drift）：%s", msg)
			}
		})
	}
}

// TestAgentMaxStepsRoundTripsAsAbsent 守"缺键在 IR 往返后仍然缺键"。
//
// 这是 `AgentSpec.MaxSteps` 从 int 改成 *int 的**唯一动机**：非指针 int 会把
// 缺键解成 0，再编码回 `"max_steps":0` —— 一个凭空出现的假数字写进 .hbc。
// 断言用字节层面的 `max_steps` 出现次数，而不是断言字段等于 0：
// 后者在两版实现下都能通过。
//
// 序列化的是**节点**而不是整个 workflow：workflow.bounds 里也有一个必填的
// max_steps，拿整个文档去数字符串会把两者混在一起，测试就算错了也看不出来。
func TestAgentMaxStepsRoundTripsAsAbsent(t *testing.T) {
	goal := 1
	cases := []struct {
		name  string
		steps *int
		want  int // 期望在编码后的字节里出现 `"max_steps":` 的次数
	}{
		{"absent", nil, 0},
		{"present", intp(4), 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := Node{ID: 0, K: "agent", Agent: &AgentSpec{
				Goal: &goal, Tools: []string{}, MaxSteps: c.steps,
			}}
			blob, err := json.Marshal(n)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Count(string(blob), `"max_steps":`); got != c.want {
				t.Fatalf("编码后 `\"max_steps\":` 出现 %d 次，应为 %d：%s", got, c.want, blob)
			}
			// 回读一遍：缺失必须还是缺失（而不是变成 0）。
			var back Node
			if err := json.Unmarshal(blob, &back); err != nil {
				t.Fatal(err)
			}
			if (back.Agent.MaxSteps == nil) != (c.steps == nil) {
				t.Fatalf("往返后指针的 nil 性变了：%v → %v",
					c.steps == nil, back.Agent.MaxSteps == nil)
			}
		})
	}
}

// TestBoundedAgentSpanIsExact 是上面那条的对照片：max_steps 是常量时必须
// **精确**给出 (1+k)×max_steps 与 k×max_steps，否则"跳过"就成了万能挡箭牌。
func TestBoundedAgentSpanIsExact(t *testing.T) {
	goal := 1
	w := &Workflow{
		Name: "agent_bounded",
		Root: 0,
		Nodes: []Node{
			{ID: 0, K: "agent", Agent: &AgentSpec{
				Goal:             &goal,
				Tools:            []string{"orders"},
				MaxSteps:         intp(4),
				MaxParallelTools: 2,
			}},
			{ID: 1, K: "lit", LitT: strptr("string"), LitV: json.RawMessage(`"hi"`)},
		},
		Bounds: Bounds{
			MaxSteps:       2,
			MaxSpans:       (1 + 2) * 4,
			MaxModelCalls:  4,
			MaxToolCalls:   8,
			MaxConcurrency: 1,
		},
	}
	if got := RecomputeMaxSpans(w); got != 12 {
		t.Fatalf("(1+k)*max_steps = 3*4 = 12；实际 %d", got)
	}
	if m, tl := RecomputeCalls(w); m != 4 || tl != 8 {
		t.Fatalf("调用上界应为 4/8；实际 %d/%d", m, tl)
	}
	if msg := CheckBounds(w); msg != "" {
		t.Fatalf("CheckBounds 应干净：%s", msg)
	}
}

// int64p 是 *int64 的小工具（Delta 用）。
func int64p(v int64) *int64 { return &v }

// buildForWorkflow 造一张"根就是一个 for"的图，形状与 codegen 产出的一致
// （codegen.cpp:2029-2065 的 ForStmt）：
//
//	for { var, init, cond = binop(lt|le, l = var, r = 字面量), body, delta }
//
// 循环体固定是 seq[call_model, call_tool]，所以 SP(body) = 2、
// 模型/工具调用各 1 —— 用 N × 2 与 N × 1 就能把"重复次数"和"每次的量"
// 分开验证。literalStop=false 时把 cond 的右操作数换成 `field`（非字面量），
// 专门走"推不出来"那条路径。
func buildForWorkflow(name string, start, stop int64, op string, delta int64,
	literalStop bool, bounds Bounds) *Workflow {
	stopNode := Node{ID: 5, K: "lit", LitT: strptr("int"),
		LitV: json.RawMessage(itoa64(stop))}
	if !literalStop {
		// field(of = 循环变量)：运行期才知道值，静态不可判。
		stopNode = Node{ID: 5, K: "field", Of: intp(3)}
	}
	return &Workflow{
		Name: name,
		Root: 0,
		Nodes: []Node{
			{ID: 0, K: "for", Var: intp(0), Init: intp(1), Cond: intp(2),
				Body: intp(4), Delta: int64p(delta)},
			{ID: 1, K: "lit", LitT: strptr("int"), LitV: json.RawMessage(itoa64(start))},
			{ID: 2, K: "binop", Op: strptr(op), L: intp(3), R: intp(5)},
			{ID: 3, K: "var", Var: intp(0)},
			{ID: 4, K: "seq", Kids: []int{6, 7}},
			stopNode,
			{ID: 6, K: "call_model"},
			{ID: 7, K: "call_tool"},
		},
		Vars:   []Var{{ID: 0, Name: "i", T: TypeExpr{K: "int"}, Decl: 0}},
		Bounds: bounds,
	}
}

// itoa64 把 int64 写成 IR 里的字面量 JSON（lit.v 是裸数字）。
func itoa64(v int64) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// TestForWithLiteralBoundsIsExact 钉住 SP(for) = 1 + N × SP(body) 的**精确值**。
//
// N 由 init/cond/delta 静态推导（镜像 codegen.cpp:183-203 的 for_iterations）。
// 这里 N=5、SP(body)=2，所以 max_spans 必须正好是 11：既不能是 1+2=3（把循环
// 当一次），也不能是 1+5=6（把 body 的 span 当 1）。差 1 就会让运行期断言
// len(spans) ≤ max_spans 判错，而没有任何检查会红。
func TestForWithLiteralBoundsIsExact(t *testing.T) {
	w := buildForWorkflow("for_exact", 0, 5, "lt", 1, true, Bounds{
		MaxSteps:       9,
		MaxSpans:       11,
		MaxModelCalls:  5,
		MaxToolCalls:   5,
		MaxConcurrency: 1,
	})
	if got := RecomputeMaxSpans(w); got != 11 {
		t.Fatalf("for(0; i<5; i+=1){seq(model,tool)} 的 SP 应为 1+5*2=11；实际 %d", got)
	}
	if m, tl := RecomputeCalls(w); m != 5 || tl != 5 {
		t.Fatalf("调用次数应为 5/5（N×body）；实际 %d/%d", m, tl)
	}
	if msg := CheckBounds(w); msg != "" {
		t.Fatalf("声明值与复算一致时 CheckBounds 必须干净：%s", msg)
	}
}

// TestForTripCountMirrorsCPP 把 for_iterations 的算术逐条钉住。
//
// 两个实现给出的 N 不一样，SP(for) 就不一样，CheckBounds 会把**合法**产物报成
// E_IR_RECOMPUTE_DRIFT。所以这里不测"大概对"，而是把 C++ 那两个公式
// （向上取整的除法 / 闭区间 +1）在不同 start/stop/delta 上的结果列出来。
func TestForTripCountMirrorsCPP(t *testing.T) {
	// body 的 SP 恒为 2（seq[call_model, call_tool]）。
	const bodySpan = 2
	cases := []struct {
		name       string
		start      int64
		stop       int64
		op         string
		delta      int64
		wantTrip   int
		wantSpans  int
		wantModelC int
	}{
		{"lt_half_open", 0, 5, "lt", 1, 5, 1 + 5*bodySpan, 5},
		{"le_inclusive", 0, 5, "le", 1, 6, 1 + 6*bodySpan, 6},
		{"lt_delta2_ceil", 0, 5, "lt", 2, 3, 1 + 3*bodySpan, 3},
		{"le_delta2", 0, 5, "le", 2, 3, 1 + 3*bodySpan, 3},
		{"lt_offset_start", 1, 6, "lt", 2, 3, 1 + 3*bodySpan, 3},
		{"lt_empty_range", 5, 0, "lt", 1, 0, 1, 0},
		{"lt_equal_bound", 5, 5, "lt", 1, 0, 1, 0},
		{"le_equal_bound", 5, 5, "le", 1, 1, 1 + 1*bodySpan, 1},
		{"gt_operator_unknown", 0, 5, "gt", 1, -1, -1, -1},
		{"eq_operator_unknown", 0, 5, "eq", 1, -1, -1, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := buildForWorkflow(c.name, c.start, c.stop, c.op, c.delta, true,
				Bounds{MaxSteps: 64, MaxSpans: 0, MaxConcurrency: 1})
			// 直接验 trip count（唯一真源），再验由它推出的 SP 与调用次数。
			if got := forTripCount(w, &w.Nodes[0]); got != c.wantTrip {
				t.Fatalf("trip count：期望 %d，实际 %d", c.wantTrip, got)
			}
			if got := RecomputeMaxSpans(w); got != c.wantSpans {
				t.Fatalf("max_spans：期望 %d，实际 %d", c.wantSpans, got)
			}
			if m, _ := RecomputeCalls(w); m != c.wantModelC {
				t.Fatalf("max_model_calls：期望 %d，实际 %d", c.wantModelC, m)
			}
		})
	}
}

// TestForWithDynamicCondIsUnknown 钉住"推不出来就不许编数字"。
//
// cond 比的是 field（运行期值）→ 轮数不可静态定界 → boundUnknown。
// 断言 CheckBounds 返回**空串**：这不是错误，只是这一项跳过。
// 为了证明它真的被跳过了（而不是碰巧相等），声明的三个值全填成错的。
func TestForWithDynamicCondIsUnknown(t *testing.T) {
	w := buildForWorkflow("for_dynamic", 0, 5, "lt", 1, false, Bounds{
		MaxSteps:       64,
		MaxSpans:       99, // 故意错：不可知时不得参与等式校验
		MaxModelCalls:  42, // 同上
		MaxToolCalls:   42, // 同上
		MaxConcurrency: 1,
	})
	if got := RecomputeMaxSpans(w); got != boundUnknown {
		t.Fatalf("cond 依赖运行期值时应为 boundUnknown(%d)；实际 %d", boundUnknown, got)
	}
	if m, tl := RecomputeCalls(w); m != boundUnknown || tl != boundUnknown {
		t.Fatalf("推不出 N 时调用次数也应为 boundUnknown；实际 %d/%d", m, tl)
	}
	if msg := CheckBounds(w); msg != "" {
		t.Fatalf("不可知应跳过而不是报错，且不得因声明值错误而报 drift：%s", msg)
	}
}
