// Package engine 是 IR 驱动执行器（T25）。
//
// 第一刀的范围刻意很小：seq / let / lit / var / field / interp / return。
// 其余节点**明确报错**，不静默跳过——一份"少执行了一个分支"的 IR 看起来
// 完全正常，而它的产物是错的。
//
// 这一段**不产生 span**：trace 是下一刀（T20 的 span 树已经就绪，接上去即可）。
// 先证明"IR 能算出正确的值"，再证明"能说清楚它怎么算的"。
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"hermes/internal/ir"
	"hermes/internal/provider"
	"hermes/internal/schema"
	"hermes/internal/trace"
)

// context 引入位置：外部调用要能被取消（T25 的 ctx 取消验收项）。

// RunError 是**可被 try/catch 捕获**的运行期错误。
//
// 为什么不能直接用 Go 的 error：语言里有 6 种错误类型与一条继承关系
// （Error 是根），而 Go 的 error 只是一条链。把"哪个类型"塞进字符串，
// `catch (HttpError e)` 就只能靠字符串匹配 —— 那正是把类型系统退化成约定。
//
// 反过来，**编程错误**（节点越界、类型不符、变量未赋值）不是 RunError：
// 它们必须直接炸穿，不能被 catch 住当业务错误处理。
type RunError struct {
	// Kind 是语言里的错误类型名：NetworkError / HttpError / ModelError /
	// ContractError / CircuitOpenError / Error。
	Kind string
	Msg  string
	// Status 只有 HttpError 有值（§7.1：否则"订单不存在"这个分支写不出来）。
	Status int
}

func (e *RunError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("%s: %s (status %d)", e.Kind, e.Msg, e.Status)
	}
	return e.Kind + ": " + e.Msg
}

// value 是 `catch (T e)` 里绑定给 e 的值。
func (e *RunError) value() map[string]any {
	m := map[string]any{"type": e.Kind, "message": e.Msg}
	if e.Status != 0 {
		m["status"] = int64(e.Status)
	}
	return m
}

// Result 是一次运行的产出。
type Result struct {
	Workflow string
	Value    any
	// Steps 是实际执行过的节点数（第一刀的"预算/上界"占位）。
	Steps int
	// Spans 是这次运行产生的 span（IR.md §11 的映射）。
	Spans []trace.Span
}

// Runtime 是执行器对"外部世界"的唯一依赖面：模型调用与工具调用。
//
// 为什么要接口而不是直接调 provider/HTTP：
//
//	· engine 的单测不该联网（R-TR-05：排查时不该还要连上游）；
//	· 预算、熔断、重试将来都要挂在**这一层**，而不是散在 engine 里；
//	· 传 nil 就是"这个 workflow 不许有外部调用"，越界会明确报错，不是静默跳过。
type Runtime interface {
	Model(ctx context.Context, req ModelRequest) (any, error)
	Tool(ctx context.Context, req ToolRequest) (any, error)
}

// StoreRuntime 是可选的存储能力。
//
// 为什么不并进 Runtime：不是每个执行环境都有存储（纯计算、只读镜像）。
// 做成可选接口，缺了就在用到的那一步报错，而不是让所有 Runtime 实现
// 都被迫实现一个用不到的 方法。
type StoreRuntime interface {
	Store(ctx context.Context, req StoreRequest) (any, error)
}

// StoreRequest 是一次存储操作。Op ∈ memory.get/memory.put/vector.put/vector.query。
type StoreRequest struct {
	Workflow string
	// Unit 是 unit 名：**记忆与向量的命名空间**。
	// 两个不同的 unit 共用一个 stores 目录时，不会互相看见对方的键。
	Unit  string
	Op    string
	Key   string
	Text  string
	Value any
	K     int
	// Backend 是 workflow 的默认 backend 名；vector.* 用它决定去哪里算 embedding。
	Backend string
	// TTL 是 memory.put 的存活时长（0 表示永不过期）。
	TTL time.Duration
}

// ModelRequest 是一次模型调用的全部输入（已折叠的常量 + 已求值的 prompt）。
type ModelRequest struct {
	Workflow string
	// Backend 是 workflow 的默认 backend 名（IR.md §4.1）。空串表示该 workflow
	// 没有默认 backend，运行期必须由配置指定——不许悄悄挑一个。
	Backend     string
	Prompt      string
	Precision   string
	MaxTokens   int
	Temperature string
	// Schema 是 schemas[] 里那份 JSON Schema 的原文；nil 表示无约束（返回 json）。
	Schema []byte
	// OnChunk 非空时启用**真流式**：每个增量文本块回调一次。
	OnChunk func(delta string) error
	// Messages 非空时走**多轮**（agent 循环）：与 Prompt 互斥，Priority 归 Messages。
	Messages []provider.Message
	// Tools 是 function calling 的 schema（JSON 数组）；nil 表示不提供工具。
	Tools json.RawMessage
}

// ModelReply 是模型的一次回复，区分"说话"与"要求调工具"。
//
// 为什么不让 Runtime.Model 继续返回 any：agent 循环需要读出 tool_calls，
// 而 any 里塞一个 map 会让"这是文本还是调用请求"变成运行期猜。
type ModelReply struct {
	Content   string
	ToolCalls []provider.ToolCall
}

// ToolRequest 是一次工具调用的全部输入。
type ToolRequest struct {
	Workflow string
	Tool     string
	URL      string
	Method   string
	// Header 与 Body 是**求值后**的运行时值（不是节点）。
	// Header 只接受 map[string]any 且叶子必须是字符串 —— 别的形态明确报错，
	// 因为"把 ``{a:1}`` 悄悄转成 ``"a": "1"``"会让请求头在服务端变成完全不同的东西。
	Header map[string]string
	Body   any
	// AllowStatus 非空时，这些状态码属于**正常返回**而不是 HttpError。
	AllowStatus []int
	// TimeoutMS 是单次调用的超时（0 表示用 Runtime 的默认值）。
	TimeoutMS int
}

// Run 执行指定 workflow。空 workflow 名表示用 is_entry 那一个。
func Run(doc *ir.Doc, workflowName string, input any) (*Result, error) {
	return RunWithID(doc, workflowName, input, "")
}

// RunWithID 允许指定 run_id（trace 的根 span 用它；空则用 "run"）。
func RunWithID(doc *ir.Doc, workflowName string, input any, runID string) (*Result, error) {
	return RunWith(doc, workflowName, input, RunOptions{RunID: runID})
}

// RunOptions 把"运行期依赖"收成一个结构体，避免参数表继续变长。
type RunOptions struct {
	RunID string
	// Runtime 为 nil 时，任何 call_model / call_tool 都会明确报错。
	Runtime Runtime
	// Context 为 nil 时用 context.Background()。
	Context context.Context
	// OnModelChunk 非空时，模型调用走**真流式**：每个增量文本块回调一次。
	//
	// 注意语义：回调只影响"怎么给"，不影响"算什么" —— 最终仍以完整内容
	// 做 schema 校验与后续编排。流式是传输形态，不是语义。
	OnModelChunk func(delta string) error
	// Concurrency 是**自适应并发**的注入点：给定期望上限，返回本次实际允许的并发。
	//
	// 为什么由外面给而不是 engine 自己算：上限的依据是"上游现在能接多少"
	// （AIMD 窗口、反压信号），那属于运行时策略，不该混进控制流。
	// 为 nil 时不动上限 —— 即按 IR 的 max_par 执行。
	Concurrency func(want int) int
}

// RunWith 是执行的唯一入口。
func RunWith(doc *ir.Doc, workflowName string, input any, opt RunOptions) (*Result, error) {
	runID := opt.RunID
	ctx := opt.Context
	if ctx == nil {
		ctx = context.Background()
	}
	w, err := pick(doc, workflowName)
	if err != nil {
		return nil, err
	}
	if runID == "" {
		runID = "run"
	}
	base := &frame{nodes: w.Nodes, vars: w.Vars, vals: make(map[int]any, len(w.Vars))}
	r := &runner{doc: doc, wf: w, unitName: doc.Unit.Name, input: input, frames: []*frame{base}, runID: runID,
		rt: opt.Runtime, ctx: ctx, conc: opt.Concurrency, onChunk: opt.OnModelChunk}
	// `input` 也占一个变量槽（codegen 会给它 id），而它不是 let 绑定的：
	// 由调用方在入口处赋值。不写这一行，`json.as_int(input.total)` 这种
	// **非插值**的用法会在运行期报"变量 input 在读取前没有被赋值" ——
	// 而插值写法 `{input.total}` 却能正常工作，两者看起来应该等价。
	for i := range w.Vars {
		if w.Vars[i].Name == "input" {
			base.vals[w.Vars[i].ID] = input
			break
		}
	}

	// 根 span：一次运行一条。IR.md §11 说它的 kind 是 `run`，而 trace.schema.json
	// 的枚举里只有 workflow/step/provider/tool/admission/budget/event —— 此处按
	// **schema**（机器可校验的那份）取 workflow，冲突记在 DECISIONS D-030。
	root := trace.Span{
		ID: runID, Parent: "", StepID: w.Name, Kind: "workflow", Name: w.Name, Status: "ok",
	}
	r.spans = append(r.spans, root)

	v, returned, err := r.stmt(w.Root)
	if err != nil {
		return nil, err
	}
	if !returned {
		return nil, fmt.Errorf("workflow %q 执行结束但没有 return：语言要求每条路径都有返回值", w.Name)
	}
	// 运行期断言（IR.md §5.1）：span 数不得超上界，超了就是编译器 bug，必须响亮。
	//
	// `+1` 是根 span：SP 代数**不把根算进去**（SP(seq)=Σ kids），而 trace 里根必然
	// 存在。不加这个 1，每个简单 workflow 都会在跑起来的第一秒被判成 compiler bug。
	//
	// **unbounded 时跳过**：此时 max_spans 是"未知"的占位，不是承诺。递归函数的
	// 调用次数无法静态定界，真正的闸是 maxCallDepth（512）与循环上限 —— 拿一个
	// 占位的数字去断言，会把正确的执行判成编译器 bug。
	if !w.Bounds.Unbounded && len(r.spans) > w.Bounds.MaxSpans+1 {
		return nil, fmt.Errorf(
			"internal.invariant_violation: workflow %q 产生了 %d 个 span，超过上界 max_spans=%d（+1 根）"+
				"——这是编译器 bug，不是用户输入的问题", w.Name, len(r.spans), w.Bounds.MaxSpans)
	}
	return &Result{Workflow: w.Name, Value: v, Steps: r.steps, Spans: r.spans}, nil
}

func pick(doc *ir.Doc, name string) (*ir.Workflow, error) {
	if name != "" {
		for i := range doc.Workflows {
			if doc.Workflows[i].Name == name {
				return &doc.Workflows[i], nil
			}
		}
		return nil, fmt.Errorf("没有名为 %q 的 workflow", name)
	}
	var found *ir.Workflow
	for i := range doc.Workflows {
		if doc.Workflows[i].IsEntry {
			if found != nil {
				return nil, fmt.Errorf("有多个 is_entry workflow，必须显式指定")
			}
			found = &doc.Workflows[i]
		}
	}
	if found == nil {
		return nil, fmt.Errorf("没有 is_entry workflow，必须显式指定")
	}
	return found, nil
}

// frame 是一个执行作用域：workflow 或某次函数调用。
//
// nodes/vars 来自 IR（workflow 或 functions[]），vals 是**这次调用**的变量值。
// 把三者绑在一起是调用帧的全部意义：函数有自己的节点编号与变量表，
// 不能拿 workflow 的表去解释它的 nodes。
type frame struct {
	nodes []ir.Node
	vars  []ir.Var
	vals  map[int]any
}

type runner struct {
	conc    func(int) int
	onChunk func(string) error
	rt      Runtime
	ctx     context.Context
	runID   string
	spans   []trace.Span
	doc     *ir.Doc
	wf      *ir.Workflow
	// unitName 是存储命名空间：键只在自己的 unit 里可见。
	unitName string
	// jump 是**非 return** 的跳转意图："" / "break" / "continue"。
	// return 用 stmt 的 jumped 返回值表达（它带着值），所以不放在这里。
	jump  string
	input any
	// frames 是调用栈：frames[0] 是 workflow，最后一个是当前函数。
	// 深度上限见 maxCallDepth。
	frames []*frame
	steps  int
	// toolCalls 统计整个 workflow 的工具调用总数，用来兜底核对 MaxToolCalls。
	// 之前 MaxToolCalls 只在加载校验器里被读，运行期一次都没读过 ——
	// 于是它只是"一句声明"，不是"被执行的约束"。
	toolCalls int
}

// cur 返回当前作用域。调用栈永远非空：RunWith 建栈，callFn push/pop 成对。
func (r *runner) cur() *frame { return r.frames[len(r.frames)-1] }

// stmt 执行一个语句节点，返回 (值, 是否已 return, 错误)。
//
// 用"是否已 return"而不是 Go 的 panic/recover 做提前返回：recover 会把真正的
// 编程错误（越界、nil）一起吞掉，而那类错误必须炸。
// ErrCancelled 是取消时的错误。调用方用它把 halt_reason 记成 "cancelled"，
// 而不是把取消混进"执行失败"——两者的修复动作完全不同。
var ErrCancelled = errors.New("run cancelled")

// checkCancel 在每个节点入口查一次取消。
//
// 为什么查得这么勤：取消的价值在于**尽快**停止花钱与占用并发。隔几个节点才查，
// 一次慢的工具调用就可能已经在飞了。这次检查是两个 select 一个 default，够便宜。
func (r *runner) checkCancel() error {
	if r.ctx == nil {
		return nil
	}
	select {
	case <-r.ctx.Done():
		return fmt.Errorf("%w: %v", ErrCancelled, r.ctx.Err())
	default:
		return nil
	}
}

// maxForIterations 是运行期兜底上限：静态上界算不出来的循环
// 不能真的无限跑下去，否则一次请求就能把进程挂住。
const maxForIterations = 1_000_000

func (r *runner) stmt(id int) (any, bool, error) {
	if err := r.checkCancel(); err != nil {
		return nil, false, err
	}
	nodes := r.cur().nodes
	if id < 0 || id >= len(nodes) {
		return nil, false, fmt.Errorf("节点 id %d 越界（nodes=%d）", id, len(nodes))
	}
	n := &nodes[id]
	r.steps++

	switch n.K {
	case "seq":
		for _, k := range n.Kids {
			v, ret, err := r.stmt(k)
			if err != nil {
				return nil, false, err
			}
			if ret {
				return v, true, nil
			}
		}
		return nil, false, nil
	case "assign":
		// 目标必须来自 `let mut`；这条在装配期已校验，这里只做写入。
		if n.Var == nil || n.E == nil {
			return nil, false, fmt.Errorf("assign 节点 %d 缺 var/e", id)
		}
		v, err := r.expr(*n.E)
		if err != nil {
			return nil, false, err
		}
		r.cur().vals[*n.Var] = v
		return nil, false, nil
	case "break", "continue":
		// 只设置意图并返回 jumped；由 for 解释成"退出本轮"还是"退出循环"。
		if n.K == "break" {
			r.jump = "break"
		} else {
			r.jump = "continue"
		}
		return nil, true, nil
	case "let":
		if n.Var == nil || n.E == nil {
			return nil, false, fmt.Errorf("let 节点 %d 缺 var/e", id)
		}
		v, err := r.expr(*n.E)
		if err != nil {
			return nil, false, err
		}
		r.cur().vals[*n.Var] = v
		return nil, false, nil
	case "while":
		// 无静态上界的循环：靠运行期上限兜底，不允许真的死循环。
		if n.C == nil || n.Body == nil {
			return nil, false, fmt.Errorf("while 节点 %d 缺 c/body", id)
		}
		for iter := 0; ; iter++ {
			if iter >= maxForIterations {
				return nil, false, fmt.Errorf("循环超过 %d 次（上界无法静态确定）", maxForIterations)
			}
			keep, err := r.expr(*n.C)
			if err != nil {
				return nil, false, err
			}
			if b, _ := keep.(bool); !b {
				return nil, false, nil
			}
			_, jumped, err := r.stmt(*n.Body)
			if err != nil {
				return nil, false, err
			}
			if jumped {
				switch r.jump {
				case "break":
					r.jump = ""
					return nil, false, nil
				case "continue":
					r.jump = ""
				default:
					return nil, true, nil
				}
			}
		}
	case "for":
		// for 循环：循环变量由这个节点独占，每轮重新绑定。
		// 体里没有任何赋值节点能改它 —— 所以"变量只被声明一次"这条不变量不破。
		if n.Var == nil || n.Init == nil || n.Cond == nil || n.Body == nil {
			return nil, false, fmt.Errorf("for 节点 %d 字段不全", id)
		}
		start, err := r.expr(*n.Init)
		if err != nil {
			return nil, false, err
		}
		delta := int64(1)
		if n.Delta != nil {
			delta = *n.Delta
		}
		if delta == 0 {
			return nil, false, fmt.Errorf("for 节点 %d 的步长为 0（会死循环）", id)
		}
		cur := start
		for iter := 0; ; iter++ {
			// 兜底：静态上界算不出来时也不允许真的无限跑下去。
			if iter >= maxForIterations {
				return nil, false, fmt.Errorf("循环超过 %d 次（上界无法静态确定）", maxForIterations)
			}
			r.cur().vals[*n.Var] = cur
			keep, err := r.expr(*n.Cond)
			if err != nil {
				return nil, false, err
			}
			if b, _ := keep.(bool); !b {
				return nil, false, nil
			}
			_, jumped, err := r.stmt(*n.Body)
			if err != nil {
				return nil, false, err
			}
			if jumped {
				switch r.jump {
				case "break":
					r.jump = ""
					return nil, false, nil
				case "continue":
					r.jump = "" // 清掉后继续走步进，不退出循环
				default:
					return nil, true, nil // return：往外传
				}
			}
			v, ok := r.cur().vals[*n.Var]
			if !ok {
				return nil, false, fmt.Errorf("for 的循环变量在迭代中被清掉")
			}
			iv, ok := v.(int64)
			if !ok {
				return nil, false, fmt.Errorf("for 的循环变量不是整数: %T", v)
			}
			cur = iv + delta
		}
	case "exprstmt":
		// 求值并丢弃：副作用已经发生（写存储、调模型）。
		if n.E == nil {
			return nil, false, nil
		}
		if _, err := r.expr(*n.E); err != nil {
			return nil, false, err
		}
		return nil, false, nil
	case "return":
		if n.E == nil {
			return nil, false, fmt.Errorf("return 节点 %d 缺 e", id)
		}
		v, err := r.expr(*n.E)
		if err != nil {
			return nil, false, err
		}
		return v, true, nil
	case "if":
		if n.C == nil || n.Then == nil {
			return nil, false, fmt.Errorf("if 节点 %d 缺 c/then", id)
		}
		cond, err := r.expr(*n.C)
		if err != nil {
			return nil, false, err
		}
		b, ok := cond.(bool)
		if !ok {
			return nil, false, fmt.Errorf("if 节点 %d 的条件不是 bool，是 %T", id, cond)
		}
		r.block(n, "if")
		if b {
			return r.stmt(*n.Then)
		}
		// elif 链按顺序求值，第一个为真的胜出。
		for _, a := range n.Elifs {
			cv, err := r.expr(a.C)
			if err != nil {
				return nil, false, err
			}
			if bv, ok := cv.(bool); ok && bv {
				return r.stmt(a.Body)
			}
		}
		if n.Else != nil {
			return r.stmt(*n.Else)
		}
		return nil, false, nil
	case "match":
		if n.Subj == nil {
			return nil, false, fmt.Errorf("match 节点 %d 缺 subj", id)
		}
		subj, err := r.expr(*n.Subj)
		if err != nil {
			return nil, false, err
		}
		// 被匹配的值可以是字符串或整数。两种都按**同类型**比较：
		// 跨类型比较（"404" == 404）永远是 false，而那种静默不匹配很难查。
		r.block(n, "match")
		switch sv := subj.(type) {
		case string:
			for _, a := range n.Arms {
				if a.Num == nil && a.Lit == sv {
					return r.stmt(a.Body)
				}
			}
		case int64:
			for _, a := range n.Arms {
				if a.Num != nil && *a.Num == sv {
					return r.stmt(a.Body)
				}
			}
		case json.Number:
			nv, err := sv.Int64()
			if err != nil {
				return nil, false, fmt.Errorf("match 节点 %d 的被匹配值不是整数: %v", id, err)
			}
			for _, a := range n.Arms {
				if a.Num != nil && *a.Num == nv {
					return r.stmt(a.Body)
				}
			}
		default:
			return nil, false, fmt.Errorf("match 节点 %d 的被匹配值类型不支持: %T", id, subj)
		}
		if n.Default == nil {
			return nil, false, fmt.Errorf("match 节点 %d 没有匹配到 %v 且没有 default", id, subj)
		}
		return r.stmt(*n.Default)
	case "destructure":
		if n.E == nil || n.Vars == nil {
			return nil, false, fmt.Errorf("destructure 节点 %d 缺 e/vars", id)
		}
		v, err := r.expr(*n.E)
		if err != nil {
			return nil, false, err
		}
		// parallel 的值是**按位置**配对的元组（LANGUAGE §5.2 / E-7）。
		tup, ok := v.([]any)
		if !ok {
			return nil, false, fmt.Errorf("解构的值不是元组，是 %T", v)
		}
		if len(tup) != len(n.Vars) {
			return nil, false, fmt.Errorf("解构了 %d 个名字但元组有 %d 个值", len(n.Vars), len(tup))
		}
		for i, vp := range n.Vars {
			if vp == nil {
				continue // `_` 丢弃位：不绑定，也不报错
			}
			r.cur().vals[*vp] = tup[i]
		}
		return nil, false, nil
	case "try":
		if n.Body == nil {
			return nil, false, fmt.Errorf("try 节点 %d 缺 body", id)
		}
		v, ret, err := r.stmt(*n.Body)
		if err == nil {
			return v, ret, nil
		}
		re, ok := err.(*RunError)
		if !ok {
			return nil, false, err // 编程错误：不许被 catch 住
		}
		for _, c := range n.Catches {
			if !catchMatches(c.T, re.Kind) {
				continue
			}
			if c.V >= 0 {
				r.cur().vals[c.V] = re.value()
			}
			return r.stmt(c.Body)
		}
		return nil, false, err // 没有匹配的 catch：继续向外抛
	case "retry":
		if n.Body == nil {
			return nil, false, fmt.Errorf("retry 节点 %d 缺 body", id)
		}
		attempts := 1
		if n.N != nil {
			attempts = *n.N + 1
		}
		var lastErr error
		for i := 0; i < attempts; i++ {
			v, ret, err := r.stmt(*n.Body)
			if err == nil {
				return v, ret, nil
			}
			lastErr = err
			if _, ok := err.(*RunError); !ok {
				return nil, false, err // 编程错误：重试它只会重复同一个 bug
			}
		}
		return nil, false, lastErr
	case "nop":
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("engine 尚不支持语句节点 %q（id=%d）", n.K, id)
	}
}

func (r *runner) expr(id int) (any, error) {
	if err := r.checkCancel(); err != nil {
		return nil, err
	}
	nodes := r.cur().nodes
	if id < 0 || id >= len(nodes) {
		return nil, fmt.Errorf("节点 id %d 越界（nodes=%d）", id, len(nodes))
	}
	n := &nodes[id]
	// **表达式不计入 steps**：steps 是"执行了多少个语句节点"，
	// 与 codegen 算上界时数的东西必须一致。把表达式也算进来，
	// 上界永远偏小，而作业里那意味着运行期被截断。
	switch n.K {
	case "lit":
		return literal(n)
	case "var":
		if n.Var == nil {
			return nil, fmt.Errorf("var 节点 %d 缺 var", id)
		}
		if *n.Var == -1 { // input 哨兵（IR.md §4.3）
			return r.input, nil
		}
		v, ok := r.cur().vals[*n.Var]
		if !ok {
			name := ""
			cur := r.cur()
			if *n.Var < len(cur.vars) {
				name = cur.vars[*n.Var].Name
			}
			return nil, fmt.Errorf("变量 %d（%s）在读取前没有被赋值", *n.Var, name)
		}
		return v, nil
	case "field":
		if n.Of == nil || n.Name == nil {
			return nil, fmt.Errorf("field 节点 %d 缺 of/name", id)
		}
		base, err := r.expr(*n.Of)
		if err != nil {
			return nil, err
		}
		return field(base, *n.Name)
	case "interp":
		return r.interp(n)
	case "cast":
		return r.cast(n)
	case "agent":
		// agent 是**表达式**：`let answer = agent { … }`。它有副作用（多轮模型/工具调用），
		// 但返回值就是一个值 —— 与 `parallel` 同一种形状，所以放在 expr 分支。
		return r.runAgent(n)
	case "array_lit":
		// 定长数组：元素按 kids 顺序求值。长度 = kids 个数，编译期已知。
		out := make([]any, 0, len(n.Kids))
		for _, k := range n.Kids {
			v, err := r.expr(k)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case "at":
		// 数组下标。运行期也检查边界：编译期只能覆盖"字面量数组 + 字面量下标"，
		// 剩下的组合必须在这里有明确错误，而不是越界读到别的东西。
		base, err := r.expr(*n.Of)
		if err != nil {
			return nil, err
		}
		iv, err := r.expr(*n.I)
		if err != nil {
			return nil, err
		}
		idx, ok := iv.(int64)
		if !ok {
			if num, ok2 := iv.(json.Number); ok2 {
				if v, e := num.Int64(); e == nil {
					idx, ok = v, true
				}
			}
		}
		if !ok {
			return nil, fmt.Errorf("数组下标不是整数: %T", iv)
		}
		arr, ok := base.([]any)
		if !ok {
			return nil, fmt.Errorf("只能对数组取下标，实际 %T", base)
		}
		if idx < 0 || idx >= int64(len(arr)) {
			return nil, fmt.Errorf("数组下标越界: %d（长度 %d）", idx, len(arr))
		}
		return arr[idx], nil
	case "len":
		if n.E == nil {
			return nil, fmt.Errorf("len 节点 %d 缺 e", n.ID)
		}
		v, err := r.expr(*n.E)
		if err != nil {
			return nil, err
		}
		switch t := v.(type) {
		case []any:
			return int64(len(t)), nil
		case string:
			// 字节长度：确定、可重现。按"字符数"数会让同一个字符串在不同
			// 编码假设下得到不同答案。
			return int64(len(t)), nil
		case map[string]any:
			return int64(len(t)), nil
		default:
			return nil, fmt.Errorf("len() 不支持 %T", v)
		}
	case "store_op":
		return r.storeOp(n)
	case "binop":
		return r.binop(n)
	case "call_model":
		return r.callModel(n)
	case "call_tool":
		return r.callTool(n)
	case "call_fn":
		return r.callFn(n)
	case "parallel":
		if err := r.runBranches(n); err != nil {
			return nil, err
		}
		tuple := make([]any, 0, len(n.Join))
		for _, j := range n.Join {
			v, err := r.expr(j)
			if err != nil {
				return nil, err
			}
			tuple = append(tuple, v)
		}
		return tuple, nil
	default:
		return nil, fmt.Errorf("engine 尚不支持表达式节点 %q（id=%d）", n.K, id)
	}
}

// maxCallDepth 是函数调用栈的硬上限（docs/FUNCTIONS.md）。
//
// 递归深度不可静态定界，所以这是运行期的最后一道闸：超了回一个普通错误，
// 进程不崩、也不无限吃内存。512 是规格值，不是"大概够用"。
const maxCallDepth = 512

// function 按名字查 unit 级函数表。线性查找：一个 unit 的函数通常是个位数，
// 为它建 map 反而多一份需要同步的状态。
func (r *runner) function(name string) *ir.Function {
	for i := range r.doc.Functions {
		if r.doc.Functions[i].Name == name {
			return &r.doc.Functions[i]
		}
	}
	return nil
}

// callFn 执行一次用户函数调用：求值实参 → 绑定形参 → 新帧执行 → 弹帧返回值。
//
// 为什么用**新帧**而不是复用 workflow 的 vars：函数的节点编号与变量表都是
// 自己的（codegen 从 0 开始编），用 workflow 的表去解释会静默读错变量。
func (r *runner) callFn(n *ir.Node) (any, error) {
	if n.Fn == nil {
		return nil, fmt.Errorf("call_fn node %d has no fn", n.ID)
	}
	fn := r.function(*n.Fn)
	if fn == nil {
		return nil, fmt.Errorf("call_fn targets unknown function %q", *n.Fn)
	}
	// 深度 = 已有帧数 - 1（根 workflow 不算一次调用）。
	if len(r.frames)-1 >= maxCallDepth {
		return nil, fmt.Errorf("stack depth exceeded: calling %q more than %d levels deep", *n.Fn, maxCallDepth)
	}

	// 实参在**调用方**作用域里求值，然后才换帧 —— 顺序反过来会读到被调函数的
	// 同名变量，而那种错误看起来像"参数传错了"。
	var args []int
	if len(n.Args) > 0 {
		if err := json.Unmarshal(n.Args, &args); err != nil {
			return nil, fmt.Errorf("call_fn node %d: args is not an integer array: %w", n.ID, err)
		}
	}
	if len(args) != len(fn.Params) {
		return nil, fmt.Errorf("function %q takes %d argument(s), got %d", fn.Name, len(fn.Params), len(args))
	}
	vals := make([]any, len(args))
	for i, a := range args {
		v, err := r.expr(a)
		if err != nil {
			return nil, err
		}
		vals[i] = v
	}

	f := &frame{nodes: fn.Nodes, vars: fn.Vars, vals: make(map[int]any, len(fn.Vars))}
	// 按**名字**绑定，不假设形参的变量 id 恰好是 0..n-1：id 是 codegen 的
	// 分配顺序，把它当协议会让"以后加一个隐式变量"静默错位。
	for i := range fn.Params {
		id := -1
		for j := range fn.Vars {
			if fn.Vars[j].Name == fn.Params[i].N {
				id = fn.Vars[j].ID
				break
			}
		}
		if id < 0 {
			return nil, fmt.Errorf("function %q: parameter %q is not in vars[] (compiler bug)", fn.Name, fn.Params[i].N)
		}
		f.vals[id] = vals[i]
	}

	r.frames = append(r.frames, f)
	v, returned, err := r.stmt(fn.Root)
	r.frames = r.frames[:len(r.frames)-1]
	if err != nil {
		return nil, err
	}
	if !returned {
		return nil, fmt.Errorf("function %q ended without return: every path must return a value", fn.Name)
	}
	return v, nil
}

func literal(n *ir.Node) (any, error) {
	if n.LitT == nil {
		return nil, fmt.Errorf("lit 节点 %d 缺 t", n.ID)
	}
	var v any
	if len(n.LitV) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(n.LitV)))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("lit 节点 %d 的 v 不是合法 JSON: %w", n.ID, err)
		}
	}
	switch *n.LitT {
	case "string":
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("lit 节点 %d 声明 string，值是 %T", n.ID, v)
		}
		return s, nil
	case "bool":
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("lit 节点 %d 声明 bool，值是 %T", n.ID, v)
		}
		return b, nil
	case "int", "cost":
		num, ok := v.(json.Number)
		if !ok {
			return nil, fmt.Errorf("lit 节点 %d 声明 %s，值是 %T", n.ID, *n.LitT, v)
		}
		i, err := strconv.ParseInt(num.String(), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("lit 节点 %d 的整数越界: %w", n.ID, err)
		}
		return i, nil
	default:
		return nil, fmt.Errorf("lit 节点 %d 的类型 %q 尚不支持", n.ID, *n.LitT)
	}
}

func field(base any, name string) (any, error) {
	m, ok := base.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("对 %T 取字段 %q：只有 object/json 才有字段", base, name)
	}
	v, ok := m[name]
	if !ok {
		return nil, fmt.Errorf("字段 %q 不存在", name)
	}
	return v, nil
}

func (r *runner) interp(n *ir.Node) (any, error) {
	var b strings.Builder
	for _, p := range n.Parts {
		if p.Lit != "" || p.Path == nil {
			b.WriteString(p.Lit)
			continue
		}
		// 路径的第一段是**根**：要么是 `input`，要么是一个变量名。
		// 把第一段也当字段去查 input，会让所有 `{intent.action}` 都报"字段不存在"。
		base, err := r.rootOf(p.Path[0])
		if err != nil {
			return nil, fmt.Errorf("插值路径 %v: %w", p.Path, err)
		}
		for _, seg := range p.Path[1:] {
			v, err := field(base, seg)
			if err != nil {
				return nil, fmt.Errorf("插值路径 %v: %w", p.Path, err)
			}
			base = v
		}
		cur := base
		b.WriteString(render(cur))
	}
	return b.String(), nil
}

// rootOf 解析插值路径的第一段：`input` 或某个变量名。
func (r *runner) rootOf(seg string) (any, error) {
	if seg == "input" {
		return r.input, nil
	}
	cur := r.cur()
	for i := range cur.vars {
		if cur.vars[i].Name == seg {
			v, ok := cur.vals[cur.vars[i].ID]
			if !ok {
				return nil, fmt.Errorf("变量 %q 在读取前没有被赋值", seg)
			}
			return v, nil
		}
	}
	return nil, fmt.Errorf("路径的第一段 %q 既不是 input 也不是已知变量", seg)
}

func render(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case int64:
		return strconv.FormatInt(t, 10)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case nil:
		return ""
	default:
		return fmt.Sprint(t)
	}
}

// block 为 if/match 产出一条 span。
//
// span_id 规则来自 IR.md §11：`<run>/L<line>` —— **行号**是可 grep 回去的锚，
// 比自增编号有用得多：拿着 trace 能直接跳到源码那一行。
func (r *runner) block(n *ir.Node, kind string) {
	r.spans = append(r.spans, trace.Span{
		ID:     fmt.Sprintf("%s/L%d", r.runID, n.Loc.L),
		Parent: r.runID,
		StepID: kind,
		Kind:   "event",
		Name:   kind,
		Status: "ok",
	})
}

// callModel 执行一次模型调用。schema 由 schemas[] 下标索引（IR.md §3.5）。
func (r *runner) callModel(n *ir.Node) (any, error) {
	if r.rt == nil {
		return nil, fmt.Errorf(
			"workflow %q 里有模型调用，但执行器没有接 Runtime（测试里注入 fake，生产里接 provider）",
			r.wf.Name)
	}
	var a ir.ModelArgs
	if len(n.Args) > 0 {
		if err := json.Unmarshal(n.Args, &a); err != nil {
			return nil, fmt.Errorf("call_model 节点 %d 的 args 形状不对: %w", n.ID, err)
		}
	}
	if a.Prompt == nil {
		return nil, fmt.Errorf("call_model 节点 %d 缺 prompt", n.ID)
	}
	pv, err := r.expr(*a.Prompt)
	if err != nil {
		return nil, err
	}
	prompt, ok := pv.(string)
	if !ok {
		return nil, fmt.Errorf("call_model 节点 %d 的 prompt 不是 string，是 %T", n.ID, pv)
	}

	req := ModelRequest{
		Workflow: r.wf.Name, Backend: r.wf.Backend, Prompt: prompt,
		Precision: a.Precision, Temperature: a.Temperature, MaxTokens: -1,
	}
	if a.MaxTokens != nil {
		req.MaxTokens = *a.MaxTokens
	}
	// schema: null 表示无约束；非 null 是 schemas[] 下标。
	if n.Schema != nil {
		if *n.Schema < 0 || *n.Schema >= len(r.doc.Schemas) {
			return nil, fmt.Errorf("call_model 节点 %d 的 schema 下标 %d 越界（schemas=%d）",
				n.ID, *n.Schema, len(r.doc.Schemas))
		}
		req.Schema = r.doc.Schemas[*n.Schema].JSON
	}

	sp := trace.Span{
		ID: fmt.Sprintf("%s/L%d", r.runID, n.Loc.L), Parent: r.runID,
		StepID: "model", Kind: "provider", Name: r.wf.Name, Status: "ok",
	}
	req.OnChunk = r.onChunk
	v, err := r.rt.Model(r.ctx, req)
	if err != nil {
		sp.Status = "failed"
		r.spans = append(r.spans, sp)
		// **取消优先**：Runtime 很可能把取消报成 NetworkError（它只看到 ctx 结束了）。
		// 直接透传会把"用户按了 Ctrl-C"伪装成"上游连不上" —— 而两者的修复动作
		// 完全不同：一个是重跑，一个是去查网。
		if cerr := r.checkCancel(); cerr != nil {
			return nil, cerr
		}
		return nil, asRunError("ModelError", err)
	}
	// **schema 校验在这里**：拿到响应、进入编排之前。
	//
	// 为什么不等 json.get 取字段时才发现：那时模型的钱已经花了、也可能已经在
	// 错误的分支上走了一段。校验放在边界上，失败就是"上游照做了但内容不对"，
	// 与"上游没响应"（NetworkError）分开 —— 两者的重试策略不同，
	// 后者重试有意义，前者重试通常还是错的。
	if len(req.Schema) > 0 {
		if verr := schema.Validate(req.Schema, v); verr != nil {
			sp.Status = "failed"
			r.spans = append(r.spans, sp)
			return nil, &RunError{Kind: "ModelError",
				Msg: "模型输出不满足 schema（" + r.wf.Name + "）: " + verr.Error()}
		}
	}
	r.spans = append(r.spans, sp)
	return v, nil
}

// callTool 执行一次工具调用。
func (r *runner) callTool(n *ir.Node) (any, error) {
	if r.rt == nil {
		return nil, fmt.Errorf("workflow %q 里有工具调用，但执行器没有接 Runtime", r.wf.Name)
	}
	var a ir.ToolArgs
	if len(n.Args) > 0 {
		if err := json.Unmarshal(n.Args, &a); err != nil {
			return nil, fmt.Errorf("call_tool 节点 %d 的 args 形状不对: %w", n.ID, err)
		}
	}
	if a.URL == nil {
		return nil, fmt.Errorf("call_tool 节点 %d 缺 url", n.ID)
	}
	uv, err := r.expr(*a.URL)
	if err != nil {
		return nil, err
	}
	url, ok := uv.(string)
	if !ok {
		return nil, fmt.Errorf("call_tool 节点 %d 的 url 不是 string，是 %T", n.ID, uv)
	}
	tool := ""
	if n.Tool != nil {
		tool = *n.Tool
	}

	// header / body 是表达式节点，在这里求值 —— 它们可以引用 workflow 的变量，
	// 而"请求体里带什么"通常是上游算出来的，不是常量。
	var hdr map[string]string
	if a.Header != nil {
		hv, err := r.expr(*a.Header)
		if err != nil {
			return nil, err
		}
		h, err := toStringMap(hv)
		if err != nil {
			return nil, fmt.Errorf("call_tool 节点 %d 的 header: %w", n.ID, err)
		}
		hdr = h
	}
	var body any
	if a.Body != nil {
		bv, err := r.expr(*a.Body)
		if err != nil {
			return nil, err
		}
		body = bv
	}
	timeout := 0
	if a.TimeoutMS != nil {
		timeout = *a.TimeoutMS
	}

	sp := trace.Span{
		ID: fmt.Sprintf("%s/L%d", r.runID, n.Loc.L), Parent: r.runID,
		StepID: "tool", Kind: "tool", Name: tool, Status: "ok",
	}
	v, err := r.rt.Tool(r.ctx, ToolRequest{
		Workflow: r.wf.Name, Tool: tool, URL: url, Method: a.Method,
		Header: hdr, Body: body, AllowStatus: a.AllowStatus, TimeoutMS: timeout,
	})
	if err != nil {
		sp.Status = "failed"
		r.spans = append(r.spans, sp)
		if cerr := r.checkCancel(); cerr != nil {
			return nil, cerr
		}
		return nil, asRunError("NetworkError", err)
	}
	r.spans = append(r.spans, sp)
	return v, nil
}

// catchMatches 判断一条 catch 子句是否接得住这个错误类型。
//
// 语言只有**一层**继承：`Error` 是唯一的根，6 种错误并列（LANGUAGE §7.1）。
// 所以"能不能接住"只有两种情况：类型相同，或写的是根类型 Error。
// 不做多层继承的猜测——层数一旦猜错，`catch (Error e)` 会接住本来该往外的错误。
func catchMatches(catchType, errKind string) bool {
	if catchType == "" {
		return false
	}
	if catchType == errKind {
		return true
	}
	return catchType == "Error"
}

// asRunError 把 Runtime 返回的错误包成可捕获的运行期错误。
// Runtime 若自己给的是 *RunError，原样保留（它更清楚是 NetworkError 还是 HttpError）。
func asRunError(kind string, err error) error {
	if re, ok := err.(*RunError); ok {
		return re
	}
	return &RunError{Kind: kind, Msg: err.Error()}
}

// runBranches 并发执行 parallel 的分支，然后回到父作用域求 join 表达式。
//
// 三个必须守住的点：
//  1. **每个分支一份 vars 副本**。分支之间不许共享可写状态（语言层面也只允许
//     读外层变量），共享 map 会变成数据竞争，而 `-race` 未必能稳定复现。
//  2. **错误按最小分支索引取胜**（T25 ⑥）。并发完成顺序不确定，按"先完成"报错
//     会让同一份 IR 的失败信息随机变化 —— 那对排查是灾难。
//  3. **span 与 steps 按分支顺序合并**，不由完成顺序决定。否则 trace 的形状
//     每次都不同，而 trace 要被 diff。
//
// cloneFrames 给并行分支一份独立的**当前**作用域：读到的是父变量的快照，
// 写不到别人。外层函数帧按指针共享（分支不会改写它们）。
//
// 切片必须整体复制：直接 append 到 r.frames 可能复用底层数组，分支 push
// 函数帧时就会覆盖父级的栈 —— 那是数据竞争，而且只在嵌套调用时出现。
func cloneFrames(frames []*frame) []*frame {
	out := make([]*frame, len(frames))
	copy(out, frames)
	cur := frames[len(frames)-1]
	c := &frame{nodes: cur.nodes, vars: cur.vars, vals: make(map[int]any, len(cur.vals))}
	for k, v := range cur.vals {
		c.vals[k] = v
	}
	out[len(out)-1] = c
	return out
}

func (r *runner) runBranches(n *ir.Node) error {
	maxPar := len(n.Branches)
	if n.MaxPar != nil && *n.MaxPar > 0 && *n.MaxPar < maxPar {
		maxPar = *n.MaxPar
	}
	if r.conc != nil {
		if got := r.conc(maxPar); got > 0 && got < maxPar {
			maxPar = got
		}
	}
	sem := make(chan struct{}, maxPar)

	type outcome struct {
		vars  map[int]any
		spans []trace.Span
		steps int
		err   error
	}
	out := make([]outcome, len(n.Branches))
	var wg sync.WaitGroup

	for i, id := range n.Branches {
		// 父级已取消时不再启动新分支：启动了它们也只会立刻返回取消错误，
		// 白白多一轮调度。
		if r.ctx != nil {
			select {
			case <-r.ctx.Done():
				// 已经起来的分支要等它们退（defer wg.Wait 会等），这里只是不再加新的。
			default:
			}
		}
		wg.Add(1)
		go func(i, id int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// 分支拿到的是父变量的**副本**：读到的是快照，写不到别人。
			// 外层函数帧按指针共享（只读），只有当前作用域被复制。
			child := &runner{
				rt: r.rt, ctx: r.ctx, runID: r.runID,
				doc: r.doc, wf: r.wf, input: r.input,
				frames: cloneFrames(r.frames),
			}
			_, ret, err := child.stmt(id)
			if err != nil {
				out[i].err = err
			} else if ret {
				out[i].err = fmt.Errorf("parallel 的分支 %d 里出现了 return：分支不该提前结束整个 workflow", id)
			}
			out[i].vars = child.cur().vals
			out[i].spans = child.spans
			out[i].steps = child.steps
		}(i, id)
	}
	wg.Wait()

	// 按分支下标合并（不是按完成顺序）。
	for _, o := range out {
		if o.err != nil {
			// 先把所有 span 落进父级，再返回错误：失败路径也要有 trace。
			for _, idx := range []int{} {
				_ = idx
			}
		}
	}
	cur := r.cur()
	for i := range out {
		for k, v := range out[i].vars {
			if _, existed := cur.vals[k]; !existed {
				cur.vals[k] = v
			}
		}
		r.spans = append(r.spans, out[i].spans...)
		r.steps += out[i].steps
	}

	// **取消优先于一切错误**：取消是外部意图，其它错误多半是它的后果。
	for i := range out {
		if out[i].err != nil && errors.Is(out[i].err, ErrCancelled) {
			return out[i].err
		}
	}
	// 最小索引的错误胜出。
	for i := range out {
		if out[i].err != nil {
			return out[i].err
		}
	}

	// join 表达式的求值留在调用点（case "parallel"）：它需要父级作用域，
	// 而此时分支变量已经并回当前作用域。放在这里求值会让同一组表达式被算两次，
	// steps 也会跟着多记一遍。
	return nil
}

// toStringMap 把运行期值收敛成请求头。
//
// 只接受 map[string]any，且**每个叶子必须是字符串**：数字/布尔会自动变成
// "1"/"true"，而服务端读到的就是另一个值 —— 那种错在客户端完全看不出来。
func toStringMap(v any) (map[string]string, error) {
	if v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("必须是对象，实际 %T", v)
	}
	out := make(map[string]string, len(m))
	for k, raw := range m {
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("字段 %q 必须是字符串，实际 %T（自动转换会让请求头悄悄变形）", k, raw)
		}
		out[k] = s
	}
	return out, nil
}

// cast 实现 `json.as_int/as_string/as_bool/as_float`（LANGUAGE §5.6）。
//
// 这是**语言里唯一能引入运行时类型错误的地方**，所以失败必须报
// `ContractError`（带 expect/got），而不是 panic 或返回零值：
//
//	· 返回零值会静默改变业务含义（"金额没解析出来"变成"金额是 0"）；
//	· panic 会穿透 catch，让用户无法处理。
func (r *runner) cast(n *ir.Node) (any, error) {
	if n.E == nil || n.To == nil {
		return nil, fmt.Errorf("cast 节点 %d 缺参数", n.ID)
	}
	v, err := r.expr(*n.E)
	if err != nil {
		return nil, err
	}
	to := *n.To
	got := describeJSON(v)

	switch to.K {
	case "string":
		if s, ok := v.(string); ok {
			return s, nil
		}
	case "bool":
		if b, ok := v.(bool); ok {
			return b, nil
		}
	case "int":
		if i, ok := toInt64(v); ok {
			return i, nil
		}
	case "float":
		// IR 禁浮点（§3.2 P-1），所以 float 在语言里以**十进制文本**流转；
		// 这里把它规约成最短往返表示，不做二进制转换。
		switch x := v.(type) {
		case string:
			return x, nil
		case json.Number:
			return x.String(), nil
		case int64:
			return strconv.FormatInt(x, 10), nil
		}
	}
	return nil, &RunError{
		Kind: "ContractError",
		Msg:  "json.as_" + to.K + " 失败：期望 " + to.K + "，实际 " + got,
	}
}

// describeJSON 给错误信息用：说清"实际是什么"，而不是只说"失败"。
func describeJSON(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "bool"
	case json.Number, int64, float64:
		return "number"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// toInt64 把 JSON 数字收敛成 int64。**非整数一律拒绝**：
// 1.5 变成 1 是静默的数据损坏，而窄化的语义是"我要 int"。
func toInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case json.Number:
		i, err := x.Int64()
		return i, err == nil
	case int64:
		return x, true
	case float64:
		if x == float64(int64(x)) {
			return int64(x), true
		}
		return 0, false
	}
	return 0, false
}

// binop 求二元运算。
//
// 类型规则很窄，而且**窄是故意的**：
//
//	· `eq`/`ne` 只比较同类型的两个值（string/string、int/int、bool/bool）；
//	· `lt`/`le`/`gt`/`ge` 只比 int（字符串比较的排序规则跨语言不一致，不做）；
//	· `and`/`or` 只接受 bool，且**短路**；
//	· 算术只接受 int（IR 禁浮点，§3.2 P-1）。
//
// 跨类型直接报 ContractError，而不是"试着转一下"：静默的强制转换会让
// `"404" == 404` 这种写法在某些输入上碰巧为真，而那是最难查的一类 bug。
func (r *runner) binop(n *ir.Node) (any, error) {
	if n.L == nil || n.R == nil {
		return nil, fmt.Errorf("binop 节点 %d 缺操作数", n.ID)
	}
	op := ""
	if n.Op != nil {
		op = *n.Op
	}
	lv, err := r.expr(*n.L)
	if err != nil {
		return nil, err
	}
	// 短路：`false && x` 不该求值 x（x 可能是个会抛错的调用）。
	if op == "and" || op == "or" {
		lb, ok := lv.(bool)
		if !ok {
			return nil, &RunError{Kind: "ContractError",
				Msg: "binop " + op + " 需要 bool，左边实际 " + describeJSON(lv)}
		}
		if op == "and" && !lb {
			return false, nil
		}
		if op == "or" && lb {
			return true, nil
		}
	}
	rv, err := r.expr(*n.R)
	if err != nil {
		return nil, err
	}

	switch op {
	case "and", "or":
		rb, ok := rv.(bool)
		if !ok {
			return nil, &RunError{Kind: "ContractError",
				Msg: "binop " + op + " 需要 bool，右边实际 " + describeJSON(rv)}
		}
		if op == "and" {
			return rb, nil
		}
		return rb, nil
	case "eq", "ne":
		eq, err := equalJSON(lv, rv)
		if err != nil {
			return nil, &RunError{Kind: "ContractError", Msg: "binop " + op + ": " + err.Error()}
		}
		if op == "eq" {
			return eq, nil
		}
		return !eq, nil
	case "lt", "le", "gt", "ge":
		li, ok1 := toInt64(lv)
		ri, ok2 := toInt64(rv)
		if !ok1 || !ok2 {
			return nil, &RunError{Kind: "ContractError",
				Msg: "binop " + op + " 只接受 int（" + describeJSON(lv) + " vs " + describeJSON(rv) + "）"}
		}
		switch op {
		case "lt":
			return li < ri, nil
		case "le":
			return li <= ri, nil
		case "gt":
			return li > ri, nil
		default:
			return li >= ri, nil
		}
	case "add", "sub", "mul", "div", "mod":
		// 字符串拼接也走 add：这是语言里唯一的隐式转换通道之外的显式写法。
		if op == "add" {
			ls, ok1 := lv.(string)
			rs, ok2 := rv.(string)
			if ok1 && ok2 {
				return ls + rs, nil
			}
		}
		li, ok1 := toInt64(lv)
		ri, ok2 := toInt64(rv)
		if !ok1 || !ok2 {
			return nil, &RunError{Kind: "ContractError",
				Msg: "binop " + op + " 只接受 int（" + describeJSON(lv) + " vs " + describeJSON(rv) + "）"}
		}
		switch op {
		case "add":
			return li + ri, nil
		case "sub":
			return li - ri, nil
		case "mul":
			return li * ri, nil
		case "div":
			if ri == 0 {
				return nil, &RunError{Kind: "ContractError", Msg: "整除的除数为 0"}
			}
			return li / ri, nil
		default:
			if ri == 0 {
				return nil, &RunError{Kind: "ContractError", Msg: "取模的除数为 0"}
			}
			return li % ri, nil
		}
	}
	return nil, fmt.Errorf("binop 节点 %d 的算子 %q 未知", n.ID, op)
}

// equalJSON 只比较**同类型**的值。跨类型不是"转一下就行"：
// `"404" == 404` 在某些输入上碰巧为真，而那是这类 bug 最难查的形态。
func equalJSON(a, b any) (bool, error) {
	switch av := a.(type) {
	case string:
		if bv, ok := b.(string); ok {
			return av == bv, nil
		}
	case bool:
		if bv, ok := b.(bool); ok {
			return av == bv, nil
		}
	}
	ai, ok1 := toInt64(a)
	bi, ok2 := toInt64(b)
	if ok1 && ok2 {
		return ai == bi, nil
	}
	return false, fmt.Errorf("两边的类型不同（%s vs %s）", describeJSON(a), describeJSON(b))
}

// runAgent 执行一次**有界** agent 循环（D-034）。
//
// 语义：
//   - 把 goal 作为第一条 user 消息，把 tools[] 里声明的工具作为 function schema
//     发给模型；
//   - 模型若要求调用工具，就执行、把结果按 `role:"tool"` 回填，进入下一轮；
//   - 模型不再要求调用时返回其内容；
//   - 达到 MaxSteps 仍在要求调用 → **报错**，而不是"再试一次"。
//
// 为什么上界到了要报错：继续跑意味着花掉不可预留的钱，而这个循环存在的意义
// 正是"上界可算"。把它悄悄放宽，等于把 D-034 的取舍作废。
func (r *runner) runAgent(n *ir.Node) (any, error) {
	if n.Agent == nil {
		return nil, fmt.Errorf("agent 节点 %d 缺规格", n.ID)
	}
	if n.Agent.MaxSteps <= 0 {
		return nil, fmt.Errorf("agent 节点 %d 的 max_steps 必须 > 0", n.ID)
	}
	goal, err := r.expr(*n.Agent.Goal)
	if err != nil {
		return nil, err
	}
	goalText, _ := goal.(string)

	// 工具 schema 从 doc.Tools 取：**按名字引用，定义只有一份**。
	tools, err := r.agentTools(n.Agent.Tools)
	if err != nil {
		return nil, err
	}

	// 兜底上界取自 IR 声明值（编译期算出来的那个数），不是这里再猜一个。
	maxTools := r.wf.Bounds.MaxToolCalls
	messages := []provider.Message{{Role: "user", Content: goalText}}
	for step := 1; step <= n.Agent.MaxSteps; step++ {
		req := ModelRequest{
			Workflow: r.wf.Name,
			Backend:  firstNonEmpty(n.Agent.Backend, r.wf.Backend),
			Messages: messages,
			Tools:    tools,
		}
		sp := trace.Span{
			ID: fmt.Sprintf("%s/L%d/agent%d", r.runID, n.Loc.L, step), Parent: r.runID,
			StepID: "agent", Kind: "provider", Name: r.wf.Name, Status: "ok",
		}
		v, err := r.rt.Model(r.ctx, req)
		if err != nil {
			sp.Status = "failed"
			r.spans = append(r.spans, sp)
			if cerr := r.checkCancel(); cerr != nil {
				return nil, cerr
			}
			return nil, asRunError("ModelError", err)
		}
		r.spans = append(r.spans, sp)

		res, ok := v.(*ModelReply)
		if !ok {
			return nil, fmt.Errorf("agent 节点 %d：Runtime.Model 未返回 ModelReply", n.ID)
		}
		if len(res.ToolCalls) == 0 {
			return res.Content, nil
		}
		// 一轮里的并行调用数有**声明上限**（默认 1）。模型真的并行发多个时，
		// 不在这里拦，max_tool_calls 就不再是上界，而"预算是预留"立刻变成空话。
		k := n.Agent.MaxParallelTools
		if k < 1 {
			k = 1
		}
		if len(res.ToolCalls) > k {
			return nil, &RunError{
				Kind: "ContractError",
				Msg: fmt.Sprintf("模型一轮要求了 %d 个工具调用，超过声明的 max_parallel_tools=%d"+
					"（要让并行生效，把它写成需要的值；上界会随之变成 (1+K)×max_steps）",
					len(res.ToolCalls), k),
			}
		}

		// 把"模型要求调用"记进消息，再按**原顺序**回填结果 —— 顺序错了模型会张冠李戴。
		messages = append(messages, provider.Message{
			Role: "assistant", Content: res.Content, ToolCalls: res.ToolCalls,
		})
		if r.toolCalls > maxTools {
			// 兜底：即使前面的检查被绕过，总量也不会越过声明的上界。
			return nil, &RunError{Kind: "ContractError",
				Msg: fmt.Sprintf("工具调用总数 %d 超过声明的上界 %d", r.toolCalls, maxTools)}
		}
		for _, call := range res.ToolCalls {
			r.toolCalls++
			out, terr := r.execAgentTool(n, call)
			tsp := trace.Span{
				ID:     fmt.Sprintf("%s/L%d/agent%d/tool/%s", r.runID, n.Loc.L, step, call.ID),
				Parent: r.runID, StepID: "agent.tool", Kind: "tool",
				Name: call.Name, Status: "ok",
			}
			if terr != nil {
				tsp.Status = "failed"
				r.spans = append(r.spans, tsp)
				return nil, terr
			}
			r.spans = append(r.spans, tsp)
			messages = append(messages, provider.Message{
				Role: "tool", ToolCallID: call.ID, Name: call.Name, Content: out,
			})
		}
	}
	return nil, &RunError{
		Kind: "ContractError",
		Msg: fmt.Sprintf("agent 达到 max_steps=%d 仍在请求工具调用（上界是承诺，不是起跑线）",
			n.Agent.MaxSteps),
	}
}

// agentTools 把工具名解析成 function calling 的 schema。
func (r *runner) agentTools(names []string) (json.RawMessage, error) {
	if len(names) == 0 {
		return nil, nil
	}
	byName := make(map[string]*ir.Tool, len(r.doc.Tools))
	for i := range r.doc.Tools {
		byName[r.doc.Tools[i].Name] = &r.doc.Tools[i]
	}
	out := make([]any, 0, len(names))
	for _, name := range names {
		t, ok := byName[name]
		if !ok {
			// 名字写错必须报错：静默少一个工具会让模型永远调不到它，
			// 而症状是"agent 不干活"，离原因很远。
			return nil, fmt.Errorf("agent 引用了不存在的工具 %q（tools[] 里有 %d 个）", name, len(r.doc.Tools))
		}
		params := t.Params
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  params,
			},
		})
	}
	return json.Marshal(out)
}

// execAgentTool 按工具声明执行一次调用：URL 模板里 `{param}` 用模型给的参数填。
//
// 为什么在这里做字符串插值：model 给的参数是运行期 JSON，不在 IR 节点图里，
// 所以这是**运行期**的填空，而不是编译期的插值节点。
func (r *runner) execAgentTool(n *ir.Node, call provider.ToolCall) (string, error) {
	var t *ir.Tool
	for i := range r.doc.Tools {
		if r.doc.Tools[i].Name == call.Name {
			t = &r.doc.Tools[i]
			break
		}
	}
	if t == nil {
		return "", fmt.Errorf("agent 节点 %d：模型要求调用未声明的工具 %q", n.ID, call.Name)
	}

	args := map[string]any{}
	if call.Arguments != "" {
		dec := json.NewDecoder(strings.NewReader(call.Arguments))
		dec.UseNumber()
		if err := dec.Decode(&args); err != nil {
			// 模型给了坏 JSON：这是契约错误，重试只会再错一次。
			return "", &RunError{Kind: "ContractError",
				Msg: "工具 " + call.Name + " 的参数不是合法 JSON: " + err.Error()}
		}
	}

	var url strings.Builder
	for _, part := range t.URLTemplate {
		if part.Lit != "" {
			url.WriteString(part.Lit)
			continue
		}
		if len(part.Path) > 0 {
			if v, ok := args[part.Path[0]]; ok {
				url.WriteString(fmt.Sprint(v))
			}
		}
	}

	out, err := r.rt.Tool(r.ctx, ToolRequest{
		Workflow: r.wf.Name, Tool: call.Name, URL: url.String(), Method: t.Method,
		AllowStatus: t.AllowStatus, TimeoutMS: t.TimeoutMS,
	})
	if err != nil {
		if cerr := r.checkCancel(); cerr != nil {
			return "", cerr
		}
		return "", asRunError("NetworkError", err)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("工具 %s 的结果无法序列化: %w", call.Name, err)
	}
	return string(b), nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// storeOp 执行 memory/vector 内建。
//
// 语义上它是一次**副作用求值**：读有返回值，写返回 true。上界上它算一个 span；
// vector.* 还要调 embeddings，所以各算一次模型调用（编译期已计入 bounds）。
func (r *runner) storeOp(n *ir.Node) (any, error) {
	sr, ok := r.rt.(StoreRuntime)
	if !ok {
		return nil, &RunError{Kind: "Error",
			Msg: "这个执行环境没有存储能力（memory.* / vector.* 需要带存储的运行时）"}
	}
	args := map[string]any{}
	if len(n.Args) > 0 {
		dec := json.NewDecoder(bytes.NewReader(n.Args))
		dec.UseNumber()
		if err := dec.Decode(&args); err != nil {
			return nil, fmt.Errorf("store_op 的 args 不是对象: %w", err)
		}
	}
	// args 里的值是**节点 id**，要逐个求值。
	str := func(name string) (string, error) {
		id, ok := args[name]
		if !ok {
			return "", nil
		}
		num, _ := id.(json.Number)
		idx, err := num.Int64()
		if err != nil {
			return "", fmt.Errorf("store_op 的 %s 不是节点 id", name)
		}
		v, err := r.expr(int(idx))
		if err != nil {
			return "", err
		}
		s, _ := v.(string)
		return s, nil
	}

	op := ""
	if n.Op != nil {
		op = *n.Op
	}
	req := StoreRequest{Workflow: r.wf.Name, Unit: r.unitName, Op: op, Backend: r.wf.Backend}
	if op == "memory.get" || op == "memory.put" || op == "memory.delete" ||
		op == "vector.put" || op == "vector.query" || op == "vector.delete" {
		k, err := str("key")
		if err != nil {
			return nil, err
		}
		if k == "" {
			if id, err := str("id"); err == nil {
				k = id
			}
		}
		req.Key = k
	}
	if op == "vector.put" || op == "vector.query" {
		t, err := str("text")
		if err != nil {
			return nil, err
		}
		req.Text = t
	}
	if op == "memory.put" {
		if id, ok := args["value"]; ok {
			num, _ := id.(json.Number)
			idx, err := num.Int64()
			if err != nil {
				return nil, fmt.Errorf("store_op 的 value 不是节点 id")
			}
			v, err := r.expr(int(idx))
			if err != nil {
				return nil, err
			}
			req.Value = v
		}
	}
	if op == "memory.put" {
		// 可选 ttl：`ttl = "24h"` / `ttl = "7d"` / `ttl = "30m"`。
		if v, ok := args["ttl"]; ok {
			num, _ := v.(json.Number)
			idx, err := num.Int64()
			if err != nil {
				return nil, fmt.Errorf("store_op 的 ttl 不是节点 id")
			}
			raw, err := r.expr(int(idx))
			if err != nil {
				return nil, err
			}
			s, _ := raw.(string)
			d, perr := parseTTL(s)
			if perr != nil {
				return nil, perr
			}
			req.TTL = d
		}
	}
	if op == "vector.query" {
		if id, ok := args["k"]; ok {
			num, _ := id.(json.Number)
			if i, err := num.Int64(); err == nil {
				req.K = int(i)
			}
		}
	}

	sp := trace.Span{ID: fmt.Sprintf("%s/L%d/store%d", r.runID, n.Loc.L, n.ID), Parent: r.runID,
		StepID: "store", Kind: "store", Name: op, Status: "ok"}
	v, err := sr.Store(r.ctx, req)
	if err != nil {
		sp.Status = "failed"
		r.spans = append(r.spans, sp)
		if cerr := r.checkCancel(); cerr != nil {
			return nil, cerr
		}
		return nil, asRunError("Error", err)
	}
	r.spans = append(r.spans, sp)
	return v, nil
}

// parseTTL 解析 "30m" / "24h" / "7d"。
//
// 为什么自己解析而不是只用 time.ParseDuration：`7d` 是运维最自然的写法，
// 而 Go 的解析器不认"天"。少支持这一种写法，用户就会写成 `168h` —— 那
// 是同一个数，但没人会一眼认得出来。
func parseTTL(v string) (time.Duration, error) {
	if v == "" {
		return 0, nil
	}
	if strings.HasSuffix(v, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(v, "d"))
		if err != nil || n < 0 {
			return 0, fmt.Errorf("ttl %q 不是合法的天数", v)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("ttl %q 不是合法时长（例：30m、24h、7d）", v)
	}
	if d < 0 {
		return 0, fmt.Errorf("ttl 不能是负数")
	}
	return d, nil
}
