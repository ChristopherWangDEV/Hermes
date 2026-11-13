package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"hermes/internal/ir"
	"hermes/internal/provider"
)

// agentRuntime 模拟一个"先要工具、再回答"的模型。
type agentRuntime struct {
	models    atomic.Int32
	tools     atomic.Int32
	lastTools json.RawMessage
	lastMsgs  []provider.Message
}

func (a *agentRuntime) Model(_ context.Context, req ModelRequest) (any, error) {
	n := a.models.Add(1)
	a.lastTools = req.Tools
	a.lastMsgs = req.Messages
	if n == 1 {
		// 第一轮：要求查订单。
		return &ModelReply{ToolCalls: []provider.ToolCall{{
			ID: "call_1", Name: "orders", Arguments: `{"id":"A1"}`,
		}}}, nil
	}
	// 第二轮：给答案。
	return &ModelReply{Content: "订单 A1 已发货"}, nil
}

func (a *agentRuntime) Tool(_ context.Context, req ToolRequest) (any, error) {
	a.tools.Add(1)
	if !strings.Contains(req.URL, "A1") {
		return nil, errors.New("URL 里没有填进参数: " + req.URL)
	}
	return map[string]any{"id": "A1", "status": "shipped"}, nil
}

// agentDoc 手写一份含 agent 节点的 IR —— 语法层还没接，先用 IR 证明循环能跑。
//
// maxSteps 是**指针**：nil 表示源码里写的 `max_steps = unlimited`（键被省略，
// 见 internal/ir/types.go）。那时 bounds 必须如实标 unbounded，并写"跑一轮"的量
// —— 与前端 codegen 在 unlimited 时的产出一致（不是上界，但不撒谎说 0）。
func agentDoc(maxSteps *int) *ir.Doc {
	goalleaf := 3 // goal 指向那条字符串字面量；指向 agent 自己会无限递归
	bounds := ir.Bounds{MaxSteps: 2, MaxSpans: 2, MaxConcurrency: 1,
		MaxModelCalls: 1, MaxToolCalls: 1, Unbounded: true}
	if maxSteps != nil {
		bounds = ir.Bounds{MaxSteps: 2, MaxSpans: 2 * *maxSteps, MaxConcurrency: 1,
			MaxModelCalls: *maxSteps, MaxToolCalls: *maxSteps}
	}
	mk := func(nodes []ir.Node) *ir.Doc {
		return &ir.Doc{
			IRVersion: 1,
			Unit: ir.Unit{Name: "a", EntryPath: "a.hermes", LanguageVersion: 1,
				Modules: []ir.Module{{Path: "a.hermes", SHA256: strings.Repeat("0", 64)}}},
			Compiler: ir.Compiler{Name: "p", Version: "1", ParserABI: 1},
			Types:    []ir.TypeDef{}, Backends: []ir.Backend{}, Schemas: []ir.SchemaDoc{},
			Tools: []ir.Tool{{
				Name: "orders", Description: "按订单号查订单", Method: "GET",
				URLTemplate: []ir.TemplatePart{
					{Lit: "http://127.0.0.1:9/orders/"},
					{Path: []string{"id"}},
				},
			}},
			Workflows: []ir.Workflow{{
				Name: "a", Exported: true, IsEntry: true,
				Returns: ir.TypeExpr{K: "string"}, Root: 0,
				Nodes:     nodes,
				Bounds:    bounds,
				Economics: []byte(`{}`), InterpDeps: []ir.InterpDep{},
				Loc: ir.Loc{M: 0, L: 1, C: 1, EL: 1, EC: 2},
			}},
			Imports: []ir.ImportRef{}, Requires: []ir.RequireItem{},
			Exports: ir.Exports{Workflows: []string{"a"}},
		}
	}
	// 形状：seq → return(agent)。agent 作为**表达式**求值（`return agent{…}` 是最简形态），
	// 这样它只被执行一次 —— 若把它同时挂在 seq 的 kids 上，会被当语句再执行一遍。
	return mk([]ir.Node{
		{ID: 0, K: "seq", Kids: []int{2}, Loc: ir.Loc{M: 0, L: 1, C: 1, EL: 1, EC: 2}},
		{ID: 1, K: "agent", Loc: ir.Loc{M: 0, L: 2, C: 5, EL: 2, EC: 10},
			Agent: &ir.AgentSpec{Goal: &goalleaf, Tools: []string{"orders"}, MaxSteps: maxSteps}},
		{ID: 2, K: "return", E: intp(1), Loc: ir.Loc{M: 0, L: 2, C: 5, EL: 2, EC: 11}},
		{ID: 3, K: "lit", LitT: strp("string"), LitV: json.RawMessage(`"查一下 A1"`),
			Loc: ir.Loc{M: 0, L: 2, C: 12, EL: 2, EC: 20}},
	})
}

func intp(i int) *int       { return &i }
func strp(s string) *string { return &s }

// TestAgentLoop 守 agent 循环的完整语义：
// 调模型 → 执行工具 → 回填 → 再调模型 → 返回答案。
func TestAgentLoop(t *testing.T) {
	doc := agentDoc(intp(4))
	doc.Workflows[0].Vars = []ir.Var{}
	rt := &agentRuntime{}

	_, err := RunWith(doc, "", nil, RunOptions{Runtime: rt})
	if err != nil {
		t.Fatalf("agent 执行失败: %v", err)
	}
	if got := rt.models.Load(); got != 2 {
		t.Fatalf("应调用模型 2 次（要工具 + 给答案），实际 %d", got)
	}
	if got := rt.tools.Load(); got != 1 {
		t.Fatalf("应执行工具 1 次，实际 %d", got)
	}
	// 工具 schema 必须真的发给模型，否则它无从选择。
	if !strings.Contains(string(rt.lastTools), `"orders"`) {
		t.Fatalf("tools schema 没发出去: %s", rt.lastTools)
	}
	// 第二轮消息里必须带上工具结果，且 role 是 tool。
	found := false
	for _, m := range rt.lastMsgs {
		if m.Role == "tool" && m.ToolCallID == "call_1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("工具结果没回填: %+v", rt.lastMsgs)
	}
}

// TestAgentRespectsMaxSteps 守"上界到了就报错，而不是继续跑"。
func TestAgentRespectsMaxSteps(t *testing.T) {
	doc := agentDoc(intp(1)) // 只允许一步，而模型永远在要工具
	rt := &alwaysTools{}
	_, err := RunWith(doc, "", nil, RunOptions{Runtime: rt})
	if err == nil {
		t.Fatal("到达 max_steps 必须报错")
	}
	var re *RunError
	if !errors.As(err, &re) || re.Kind != "ContractError" {
		t.Fatalf("应是 ContractError，实际 %v", err)
	}
	if !strings.Contains(re.Msg, "max_steps") {
		t.Fatalf("错误应点明 max_steps：%s", re.Msg)
	}
	if got := rt.models.Load(); got != 1 {
		t.Fatalf("不该多跑一轮，实际调用 %d 次", got)
	}
}

type alwaysTools struct{ models atomic.Int32 }

func (a *alwaysTools) Model(_ context.Context, _ ModelRequest) (any, error) {
	a.models.Add(1)
	return &ModelReply{ToolCalls: []provider.ToolCall{{ID: "c", Name: "orders", Arguments: "{}"}}}, nil
}
func (a *alwaysTools) Tool(_ context.Context, _ ToolRequest) (any, error) {
	return map[string]any{}, nil
}

// TestAgentStepCap 钉住"有静态上界就用它，没有就用运行期硬上限"。
//
// 这条测试**替代**"跑满 1e6 次"那种测试：真跑一百万轮要几分钟，而它证明的事情
// 和这里一样 —— 上限就是那个常量。cap 是唯一的判定点（校验与循环都用它），
// 所以钉住它就钉住了两边不会各说各话。
func TestAgentStepCap(t *testing.T) {
	r := &runner{}
	if got := r.agentStepCap(nil); got != maxUnboundedAgentSteps {
		t.Fatalf("nil spec 应退回运行期硬上限 %d，实际 %d", maxUnboundedAgentSteps, got)
	}
	if got := r.agentStepCap(&ir.AgentSpec{}); got != maxUnboundedAgentSteps {
		t.Fatalf("MaxSteps 缺失应退回运行期硬上限 %d，实际 %d", maxUnboundedAgentSteps, got)
	}
	if got := r.agentStepCap(&ir.AgentSpec{MaxSteps: intp(0)}); got != maxUnboundedAgentSteps {
		t.Fatalf("手改出的 max_steps=0 不是上界，应退回运行期硬上限 %d，实际 %d",
			maxUnboundedAgentSteps, got)
	}
	if got := r.agentStepCap(&ir.AgentSpec{MaxSteps: intp(4)}); got != 4 {
		t.Fatalf("声明 4 步就应只允许 4 步，实际 %d", got)
	}
}

// TestAgentUnlimitedRunsAndAnswers 证明 `max_steps = unlimited`（键缺失）
// 的 agent 循环**真的会跑**，且模型第一轮就给出答案时正常返回。
//
// 这条测试守的是一个具体的旧症状：引擎以前用 `if MaxSteps <= 0` 直接拒绝，
// 于是 unlimited 的产物在 `hermes run` 上永远得到
// "agent 节点 N 的 max_steps 必须 > 0" —— 而 W2004 承诺的是"运行期有兜底上限，
// 不会真的无限跑"。承诺与行为对不上，比没有承诺更糟。
func TestAgentUnlimitedRunsAndAnswers(t *testing.T) {
	doc := agentDoc(nil) // 键缺失 == max_steps = unlimited
	rt := &immediateAnswer{}
	res, err := RunWith(doc, "", nil, RunOptions{Runtime: rt})
	if err != nil {
		t.Fatalf("unlimited 的 agent 应能执行（只有运行期硬上限），实际失败: %v", err)
	}
	if res.Value != "好了" {
		t.Fatalf("最终答案不对: %#v", res.Value)
	}
	if got := rt.models.Load(); got != 1 {
		t.Fatalf("模型第一轮就给了答案，应只调用 1 次，实际 %d", got)
	}
}

// immediateAnswer 第一次（也是唯一一次）就回答，不要求工具。
type immediateAnswer struct{ models atomic.Int32 }

func (a *immediateAnswer) Model(_ context.Context, _ ModelRequest) (any, error) {
	a.models.Add(1)
	return &ModelReply{Content: "好了"}, nil
}

func (a *immediateAnswer) Tool(_ context.Context, _ ToolRequest) (any, error) {
	return map[string]any{}, nil
}

// TestAgentWithoutRuntimeFailsLoudly 守"没有 provider 时报清楚，而不是 panic"。
//
// runAgent 曾经漏掉 callModel / callTool 都有的 `r.rt == nil` 检查：
// 一个没有 backend 的 workflow 里放一个 agent，进程直接收到 Go 的
// nil 指针 panic（exit 2 + 一堆 goroutine 栈）。对一个把"失败要说清楚"
// 当卖点的运行时来说，这是最差的失败方式 —— 用户的问题被表现成我们的 bug。
// 而这条路径在 unlimited 之后变成了默认路径（以前 MaxSteps<=0 会更早拦下）。
func TestAgentWithoutRuntimeFailsLoudly(t *testing.T) {
	doc := agentDoc(nil)
	_, err := RunWith(doc, "", nil, RunOptions{})
	if err == nil {
		t.Fatal("没有 Runtime 时必须报错")
	}
	if !strings.Contains(err.Error(), "Runtime") {
		t.Fatalf("错误应说明缺 Runtime/provider：%v", err)
	}
}

// TestAgentRejectsTooManyParallelToolCalls 守"并行调用有声明上限"。
//
// OpenAI 与 Anthropic 都会在一轮响应里并行发多个 tool_call。若引擎照单全收，
// `max_tool_calls = max_steps` 当场不成立，而"预算是预留"整个叙事跟着崩。
func TestAgentRejectsTooManyParallelToolCalls(t *testing.T) {
	doc := agentDoc(intp(4)) // 默认 max_parallel_tools = 1
	rt := &twoToolsPerTurn{}
	_, err := RunWith(doc, "", nil, RunOptions{Runtime: rt})
	if err == nil {
		t.Fatal("一轮两个工具调用、上限为 1 时必须报错")
	}
	var re *RunError
	if !errors.As(err, &re) || re.Kind != "ContractError" {
		t.Fatalf("应是 ContractError，实际 %v", err)
	}
	if !strings.Contains(re.Msg, "max_parallel_tools") {
		t.Fatalf("错误要点名 max_parallel_tools：%s", re.Msg)
	}
	// 一个都不该执行：先拒绝，再执行 —— 顺序反了就成了"执行完再报错"。
	if got := rt.tools.Load(); got != 0 {
		t.Fatalf("被拒绝的那一轮不该执行任何工具，实际 %d 次", got)
	}
}

// TestAgentAllowsDeclaredParallelism 守"声明了就放行，且上界跟着变"。
func TestAgentAllowsDeclaredParallelism(t *testing.T) {
	doc := agentDoc(intp(4))
	// 声明 K=2：上界应为 (1+2)×4 个 span、2×4 次工具调用。
	doc.Workflows[0].Nodes[1].Agent.MaxParallelTools = 2
	doc.Workflows[0].Bounds.MaxSpans = 3 * 4
	doc.Workflows[0].Bounds.MaxToolCalls = 2 * 4

	rt := &twoToolsPerTurn{}
	if _, err := RunWith(doc, "", nil, RunOptions{Runtime: rt}); err != nil {
		t.Fatalf("声明 K=2 后应放行: %v", err)
	}
	if got := rt.tools.Load(); got != 2 {
		t.Fatalf("一轮执行 2 个工具，实际 %d", got)
	}
}

type twoToolsPerTurn struct {
	tools  atomic.Int32
	models atomic.Int32
}

func (a *twoToolsPerTurn) Model(_ context.Context, _ ModelRequest) (any, error) {
	if a.models.Add(1) == 1 {
		return &ModelReply{ToolCalls: []provider.ToolCall{
			{ID: "c1", Name: "orders", Arguments: `{"id":"A1"}`},
			{ID: "c2", Name: "orders", Arguments: `{"id":"A2"}`},
		}}, nil
	}
	return &ModelReply{Content: "done"}, nil
}

func (a *twoToolsPerTurn) Tool(_ context.Context, _ ToolRequest) (any, error) {
	a.tools.Add(1)
	return map[string]any{"ok": true}, nil
}
