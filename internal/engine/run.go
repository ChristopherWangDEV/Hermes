// Package engine 是 IR 驱动执行器（T25）。
//
// 第一刀的范围刻意很小：seq / decl / lit / var / field / interp / return。
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
	"math"
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
//
// `status` **无条件**写进去，即使它是 0。原因是静态契约：LANGUAGE §7.1.1 说
// `HttpError.status: int`、`ModelError.status: int`（上游无状态码时为 0），
// 而 `field()` 对缺键是硬错（`field "status" does not exist`，且它不是
// *RunError，catch 接不住）。有条件地写会让"类型检查说能读、运行期炸"——
// 那正是本项目最不能接受的一类失败。0 是规范给的哨兵值，不是编造。
func (e *RunError) value() map[string]any {
	return map[string]any{
		"type":    e.Kind,
		"message": e.Msg,
		"status":  int64(e.Status),
	}
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
	// `input` 也占一个变量槽（codegen 会给它 id），而它不是 decl 绑定的：
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
		return nil, fmt.Errorf("workflow %q ended without return: every path must return a value", w.Name)
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
			"internal.invariant_violation: workflow %q produced %d spans, exceeding upper bound max_spans=%d (+1 root)"+
				"; this is a compiler bug, not a user input problem", w.Name, len(r.spans), w.Bounds.MaxSpans)
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
		return nil, fmt.Errorf("no workflow named %q", name)
	}
	var found *ir.Workflow
	for i := range doc.Workflows {
		if doc.Workflows[i].IsEntry {
			if found != nil {
				return nil, fmt.Errorf("multiple is_entry workflows found; specify one explicitly")
			}
			found = &doc.Workflows[i]
		}
	}
	if found == nil {
		return nil, fmt.Errorf("no is_entry workflow found; specify one explicitly")
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

// maxUnboundedAgentSteps 是 `max_steps = unlimited` 的 agent 循环的运行期硬上限。
//
// 为什么另起一个常量而不是复用 maxForIterations：两者**限的不是同一件事**，
// 而且会各自合理地漂移。maxForIterations 限的是"一个 while/for 迭代几次"，
// maxUnboundedAgentSteps 限的是"一个 agent 循环调几次模型"——后者每一次都可能
// 是一次付费的模型调用 + 若干次工具调用，所以将来若要收紧，先收紧的一定是
// 这一个。共用一个数会让"调循环上限"顺手改掉 agent 的可迭代预算，反之亦然。
//
// 为什么是 1e6：作者写 `max_steps = unlimited` 是**主动放弃编译期上界**
// （W2004 会如实告知），但没有因此放弃"一定会停"——运行期仍然要有一道闸，
// 否则一次请求就能把进程和钱包都挂住。数量级沿用 maxForIterations 的先例：
// 同一个语言里两个"兜底"应该同等宽松，免得解释成本比收益还高。
const maxUnboundedAgentSteps = 1_000_000

// agentStepCap 是 agent 循环**实际**允许的迭代次数。
//
// 有静态上界就照用（作者写多少就是多少，不替它加码）；没有静态上界
// （`max_steps = unlimited` → 键缺失 → nil，或手改出的 ≤0）才退回运行期硬上限。
//
// 为什么收成一个方法：校验与循环**必须**用同一个数。两处各写一遍
// `if spec.MaxSteps == nil { … }`，迟早会一处改了一处没改，症状是
// "校验说 1e6、循环跑 4 次" 或反过来 —— 前者是假承诺，后者是提前失败。
func (r *runner) agentStepCap(spec *ir.AgentSpec) int {
	if spec != nil && spec.MaxSteps != nil && *spec.MaxSteps > 0 {
		return *spec.MaxSteps
	}
	return maxUnboundedAgentSteps
}

func (r *runner) stmt(id int) (any, bool, error) {
	if err := r.checkCancel(); err != nil {
		return nil, false, err
	}
	nodes := r.cur().nodes
	if id < 0 || id >= len(nodes) {
		return nil, false, fmt.Errorf("node id %d out of range (nodes=%d)", id, len(nodes))
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
		// 目标必须来自 可变声明；这条在装配期已校验，这里只做写入。
		if n.Var == nil || n.E == nil {
			return nil, false, fmt.Errorf("assign node %d missing var/e", id)
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
	case "decl":
		if n.Var == nil || n.E == nil {
			return nil, false, fmt.Errorf("decl node %d missing var/e", id)
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
			return nil, false, fmt.Errorf("while node %d missing c/body", id)
		}
		for iter := 0; ; iter++ {
			if iter >= maxForIterations {
				return nil, false, fmt.Errorf("loop exceeded %d iterations (upper bound cannot be determined statically)", maxForIterations)
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
			return nil, false, fmt.Errorf("for node %d has incomplete fields", id)
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
			return nil, false, fmt.Errorf("for node %d has a step of 0 (would loop forever)", id)
		}
		cur := start
		for iter := 0; ; iter++ {
			// 兜底：静态上界算不出来时也不允许真的无限跑下去。
			if iter >= maxForIterations {
				return nil, false, fmt.Errorf("loop exceeded %d iterations (upper bound cannot be determined statically)", maxForIterations)
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
				return nil, false, fmt.Errorf("for loop variable was cleared during iteration")
			}
			iv, ok := v.(int64)
			if !ok {
				return nil, false, fmt.Errorf("for loop variable is not an integer: %T", v)
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
			return nil, false, fmt.Errorf("return node %d missing e", id)
		}
		v, err := r.expr(*n.E)
		if err != nil {
			return nil, false, err
		}
		return v, true, nil
	case "if":
		if n.C == nil || n.Then == nil {
			return nil, false, fmt.Errorf("if node %d missing c/then", id)
		}
		cond, err := r.expr(*n.C)
		if err != nil {
			return nil, false, err
		}
		b, ok := cond.(bool)
		if !ok {
			return nil, false, fmt.Errorf("if node %d condition is not bool, got %T", id, cond)
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
			return nil, false, fmt.Errorf("match node %d missing subj", id)
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
				return nil, false, fmt.Errorf("match node %d subject is not an integer: %v", id, err)
			}
			for _, a := range n.Arms {
				if a.Num != nil && *a.Num == nv {
					return r.stmt(a.Body)
				}
			}
		default:
			return nil, false, fmt.Errorf("match node %d subject has unsupported type: %T", id, subj)
		}
		if n.Default == nil {
			return nil, false, fmt.Errorf("match node %d found no arm for %v and has no default", id, subj)
		}
		return r.stmt(*n.Default)
	case "destructure":
		if n.E == nil || n.Vars == nil {
			return nil, false, fmt.Errorf("destructure node %d missing e/vars", id)
		}
		v, err := r.expr(*n.E)
		if err != nil {
			return nil, false, err
		}
		// parallel 的值是**按位置**配对的元组（LANGUAGE §5.2 / E-7）。
		tup, ok := v.([]any)
		if !ok {
			return nil, false, fmt.Errorf("destructured value is not a tuple, got %T", v)
		}
		if len(tup) != len(n.Vars) {
			return nil, false, fmt.Errorf("destructured %d names but tuple has %d values", len(n.Vars), len(tup))
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
			return nil, false, fmt.Errorf("try node %d missing body", id)
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
			return nil, false, fmt.Errorf("retry node %d missing body", id)
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
		return nil, false, fmt.Errorf("engine does not yet support statement node %q (id=%d)", n.K, id)
	}
}

func (r *runner) expr(id int) (any, error) {
	if err := r.checkCancel(); err != nil {
		return nil, err
	}
	nodes := r.cur().nodes
	if id < 0 || id >= len(nodes) {
		return nil, fmt.Errorf("node id %d out of range (nodes=%d)", id, len(nodes))
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
			return nil, fmt.Errorf("var node %d missing var", id)
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
			return nil, fmt.Errorf("variable %d (%s) was read before being assigned", *n.Var, name)
		}
		return v, nil
	case "field":
		if n.Of == nil || n.Name == nil {
			return nil, fmt.Errorf("field node %d missing of/name", id)
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
		// agent 是**表达式**：`auto answer = agent { … }`。它有副作用（多轮模型/工具调用），
		// 但返回值就是一个值 —— 与 `parallel` 同一种形状，所以放在 expr 分支。
		return r.runAgent(n)
	case "fallback":
		// 错误边界，**不是异常**：先求 main，失败（C ABI 非零状态码）再求 body
		// 并取它的返回值。全程没有栈展开 —— 失败一直是返回值。
		//
		// 只有 *RunError 触发降级。引擎内部的编程错误（节点越界、缺字段…）
		// 必须继续向外冒：否则一个编译器 bug 会被降级块吃成一个"看起来正常
		// 的答案"，而这类错误恰恰最需要被看见。与 try 的纪律一致。
		if n.Main == nil || n.Body == nil {
			return nil, fmt.Errorf("fallback node %d missing main/body", id)
		}
		v, err := r.expr(*n.Main)
		if err == nil {
			return v, nil
		}
		if _, ok := err.(*RunError); !ok {
			return nil, err // 编程错误：不降级
		}
		// 降级块是**语句**（Block → seq）：它的值靠 return 带出来。
		// 没有 return 是合法的（`expr fallback { log.warn(…) };` 这种只要
		// 副作用、不要值的形态），此时表达式本身没有值 → null。
		bv, ret, berr := r.stmt(*n.Body)
		if berr != nil {
			return nil, berr
		}
		if !ret {
			return nil, nil
		}
		return bv, nil
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
			return nil, fmt.Errorf("array index is not an integer: %T", iv)
		}
		arr, ok := base.([]any)
		if !ok {
			return nil, fmt.Errorf("can only index an array, got %T", base)
		}
		if idx < 0 || idx >= int64(len(arr)) {
			return nil, fmt.Errorf("array index out of range: %d (length %d)", idx, len(arr))
		}
		return arr[idx], nil
	case "len":
		if n.E == nil {
			return nil, fmt.Errorf("len node %d missing e", n.ID)
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
			return nil, fmt.Errorf("len() does not support %T", v)
		}
	case "store_op":
		return r.storeOp(n)
	case "binop":
		return r.binop(n)
	case "unop":
		return r.unop(n)
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
		return nil, fmt.Errorf("engine does not yet support expression node %q (id=%d)", n.K, id)
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
		return nil, fmt.Errorf("lit node %d missing t", n.ID)
	}
	var v any
	if len(n.LitV) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(n.LitV)))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("lit node %d has invalid JSON in v: %w", n.ID, err)
		}
	}
	switch *n.LitT {
	case "string":
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("lit node %d declares string, value is %T", n.ID, v)
		}
		return s, nil
	case "bool":
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("lit node %d declares bool, value is %T", n.ID, v)
		}
		return b, nil
	case "int", "cost":
		num, ok := v.(json.Number)
		if !ok {
			return nil, fmt.Errorf("lit node %d declares %s, value is %T", n.ID, *n.LitT, v)
		}
		i, err := strconv.ParseInt(num.String(), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("lit node %d integer out of range: %w", n.ID, err)
		}
		return i, nil
	case "double":
		// 位模式：{"k":"double","bits":"<16 位小写 hex>"}。bits 必须是字符串，
		// 否则 V-7 / canonical.ErrFloat / 签名链三件事全崩。非法 hex 必须明确报错，
		// 不能悄悄当成 0 —— 那又是一次静默损坏。
		var bp struct {
			K    string `json:"k"`
			Bits string `json:"bits"`
		}
		if err := json.Unmarshal(n.LitV, &bp); err != nil {
			return nil, fmt.Errorf("lit node %d has invalid double bit pattern: %w", n.ID, err)
		}
		if bp.K != "double" {
			return nil, fmt.Errorf("lit node %d double bit pattern has k=%q, want \"double\"", n.ID, bp.K)
		}
		if len(bp.Bits) != 16 {
			return nil, fmt.Errorf("lit node %d double bits must be 16 lowercase hex digits, got %q", n.ID, bp.Bits)
		}
		for i := 0; i < len(bp.Bits); i++ {
			c := bp.Bits[i]
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				return nil, fmt.Errorf("lit node %d double bits %q is not 16 lowercase hex digits", n.ID, bp.Bits)
			}
		}
		u, err := strconv.ParseUint(bp.Bits, 16, 64)
		if err != nil {
			return nil, fmt.Errorf("lit node %d double bits %q is not valid hex: %w", n.ID, bp.Bits, err)
		}
		return math.Float64frombits(u), nil
	default:
		return nil, fmt.Errorf("lit node %d has unsupported type %q", n.ID, *n.LitT)
	}
}

func field(base any, name string) (any, error) {
	m, ok := base.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("value of type %T has no field %q; only object/json have fields", base, name)
	}
	v, ok := m[name]
	if !ok {
		return nil, fmt.Errorf("field %q does not exist", name)
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
			return nil, fmt.Errorf("interpolation path %v: %w", p.Path, err)
		}
		for _, seg := range p.Path[1:] {
			v, err := field(base, seg)
			if err != nil {
				return nil, fmt.Errorf("interpolation path %v: %w", p.Path, err)
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
				return nil, fmt.Errorf("variable %q was read before being assigned", seg)
			}
			return v, nil
		}
	}
	return nil, fmt.Errorf("first path segment %q is neither input nor a known variable", seg)
}

func render(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		// 插值口径必须与 `value` 字段（走 json.Marshal）一致：encoding/json
		// 也是最短往返，但固定/指数记法的选择不同 —— 'g'/-1/64 会把 1e6
		// 写成 1e+06，而 encoding/json 写 1000000，于是 `hermes run` 与
		// `hermes compile` 的插值结果分叉。直接 Marshal 这个 float 最稳妥。
		// 非有限值 Marshal 会报错，此时安全退回原格式，绝不 panic：它们只在
		// 序列化边界被拒，插值本身不该成为另一道判定点。
		if b, err := json.Marshal(t); err == nil {
			return string(b)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
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
			"workflow %q contains a model call, but no Runtime is attached (inject a fake in tests, wire up a provider in production)",
			r.wf.Name)
	}
	var a ir.ModelArgs
	if len(n.Args) > 0 {
		if err := json.Unmarshal(n.Args, &a); err != nil {
			return nil, fmt.Errorf("call_model node %d has malformed args: %w", n.ID, err)
		}
	}
	if a.Prompt == nil {
		return nil, fmt.Errorf("call_model node %d missing prompt", n.ID)
	}
	pv, err := r.expr(*a.Prompt)
	if err != nil {
		return nil, err
	}
	prompt, ok := pv.(string)
	if !ok {
		return nil, fmt.Errorf("call_model node %d prompt is not a string, got %T", n.ID, pv)
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
			return nil, fmt.Errorf("call_model node %d schema index %d out of range (schemas=%d)",
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
				Msg: "model output does not satisfy schema (" + r.wf.Name + "): " + verr.Error()}
		}
	}
	r.spans = append(r.spans, sp)
	return v, nil
}

// callTool 执行一次工具调用。
func (r *runner) callTool(n *ir.Node) (any, error) {
	if r.rt == nil {
		return nil, fmt.Errorf("workflow %q contains a tool call, but no Runtime is attached", r.wf.Name)
	}
	var a ir.ToolArgs
	if len(n.Args) > 0 {
		if err := json.Unmarshal(n.Args, &a); err != nil {
			return nil, fmt.Errorf("call_tool node %d has malformed args: %w", n.ID, err)
		}
	}
	if a.URL == nil {
		return nil, fmt.Errorf("call_tool node %d missing url", n.ID)
	}
	uv, err := r.expr(*a.URL)
	if err != nil {
		return nil, err
	}
	url, ok := uv.(string)
	if !ok {
		return nil, fmt.Errorf("call_tool node %d url is not a string, got %T", n.ID, uv)
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
			return nil, fmt.Errorf("call_tool node %d header: %w", n.ID, err)
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
	// §5.7 条件 ② 的运行期一半：编译期证明了 idem_key 只由 input 决定，
	// 这里把求值结果注入同名头。缺了这一步，`idem.mode = "idem_key"` 就只是
	// 一个标签 —— 服务端收不到 Idempotency-Key，重试照样重复扣款。
	//
	// hdr 可能为 nil（没写 header 参数），此时才创建；不覆盖作者显式写的同名头
	// 之外的任何东西。值必须是字符串：悄悄把数字转成 "1" 会让服务端读到
	// 另一个键，与 header 的整体纪律一致。
	if a.IdemKey != nil {
		kv, err := r.expr(*a.IdemKey)
		if err != nil {
			return nil, err
		}
		key, ok := kv.(string)
		if !ok {
			return nil, fmt.Errorf("call_tool node %d idem_key is not a string, got %T", n.ID, kv)
		}
		if hdr == nil {
			hdr = map[string]string{}
		}
		hdr["Idempotency-Key"] = key
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
				out[i].err = fmt.Errorf("return found in parallel branch %d: a branch must not end the whole workflow early", id)
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
		return nil, fmt.Errorf("must be an object, got %T", v)
	}
	out := make(map[string]string, len(m))
	for k, raw := range m {
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("field %q must be a string, got %T (auto-coercion would silently corrupt the request header)", k, raw)
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
		return nil, fmt.Errorf("cast node %d missing arguments", n.ID)
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
	case "char":
		// C 风格宽度截断：char = signed 8-bit。
		if i, ok := toInt64(v); ok {
			return int64(int8(i)), nil
		}
	case "short":
		// short = signed 16-bit。
		if i, ok := toInt64(v); ok {
			return int64(int16(i)), nil
		}
	case "long":
		// long = 64-bit，与 int 同宽（Hermes int 本就是 int64）。
		if i, ok := toInt64(v); ok {
			return i, nil
		}
	case "double":
		// v2.0.0：`json.as_float` 是 API 名保留原名，但窄化结果是一个真正的
		// float64（IEEE-754 binary64），不再以十进制文本流转。IR 常量用位模式，
		// 而这里是运行期值——两者不冲突。显式 cast 允许 int→double（E4006 禁的是
		// **隐式**提升），解析失败的十进制文本落到下面的 ContractError。
		switch x := v.(type) {
		case float64:
			return x, nil
		case json.Number:
			if f, err := strconv.ParseFloat(x.String(), 64); err == nil {
				return f, nil
			}
		case string:
			if f, err := strconv.ParseFloat(x, 64); err == nil {
				return f, nil
			}
		case int64:
			return float64(x), nil
		}
	}
	return nil, &RunError{
		Kind: "ContractError",
		Msg:  "json.as_" + to.K + " failed: expected " + to.K + ", got " + got,
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
//	· 关系比较与算术：int⊗int 或 double⊗double；**混合 int/double 一律拒**
//	  （E4006 禁隐式提升，引擎侧再兜一次）。IR 常量用位模式，不写 JSON 数字。
//
// 跨类型直接报 ContractError，而不是"试着转一下"：静默的强制转换会让
// `"404" == 404` 这种写法在某些输入上碰巧为真，而那是最难查的一类 bug。
func (r *runner) binop(n *ir.Node) (any, error) {
	if n.L == nil || n.R == nil {
		return nil, fmt.Errorf("binop node %d missing operands", n.ID)
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
				Msg: "binop " + op + " requires bool, left operand is " + describeJSON(lv)}
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
				Msg: "binop " + op + " requires bool, right operand is " + describeJSON(rv)}
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
		// double 关系比较（E4033 只拒 == / !=，< <= > >= 与算术不受影响）。
		lf, lok := lv.(float64)
		rf, rok := rv.(float64)
		if lok || rok {
			if !lok || !rok {
				return nil, &RunError{Kind: "ContractError",
					Msg: "binop " + op + " does not mix int and double (" +
						describeJSON(lv) + " vs " + describeJSON(rv) + "): no implicit promotion"}
			}
			switch op {
			case "lt":
				return lf < rf, nil
			case "le":
				return lf <= rf, nil
			case "gt":
				return lf > rf, nil
			default:
				return lf >= rf, nil
			}
		}
		li, ok1 := toInt64(lv)
		ri, ok2 := toInt64(rv)
		if !ok1 || !ok2 {
			return nil, &RunError{Kind: "ContractError",
				Msg: "binop " + op + " accepts only int (" + describeJSON(lv) + " vs " + describeJSON(rv) + ")"}
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
		lf, lok := lv.(float64)
		rf, rok := rv.(float64)
		if lok || rok {
			if !lok || !rok {
				return nil, &RunError{Kind: "ContractError",
					Msg: "binop " + op + " does not mix int and double (" +
						describeJSON(lv) + " vs " + describeJSON(rv) + "): no implicit promotion (E4006)"}
			}
			switch op {
			case "add":
				return lf + rf, nil
			case "sub":
				return lf - rf, nil
			case "mul":
				return lf * rf, nil
			case "div":
				// IEEE-754：除零得到 ±Inf / NaN，不在中间拒 —— NaN/Inf 只在
				// 序列化边界拒（cmd/hermes/jsonboundary.go 是唯一判定点）。
				return lf / rf, nil
			default:
				return math.Mod(lf, rf), nil
			}
		}
		li, ok1 := toInt64(lv)
		ri, ok2 := toInt64(rv)
		if !ok1 || !ok2 {
			return nil, &RunError{Kind: "ContractError",
				Msg: "binop " + op + " accepts only int (" + describeJSON(lv) + " vs " + describeJSON(rv) + ")"}
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
				return nil, &RunError{Kind: "ContractError", Msg: "integer division by zero"}
			}
			return li / ri, nil
		default:
			if ri == 0 {
				return nil, &RunError{Kind: "ContractError", Msg: "modulo by zero"}
			}
			return li % ri, nil
		}
	}
	return nil, fmt.Errorf("binop node %d has unknown operator %q", n.ID, op)
}

// unop 求一元运算：neg（取负，int / double）/ not（逻辑非，bool）。
// 与 binop 同一套窄类型纪律：不做隐式转换，类型不符报 ContractError。
func (r *runner) unop(n *ir.Node) (any, error) {
	if n.E == nil {
		return nil, fmt.Errorf("unop node %d missing operands", n.ID)
	}
	op := ""
	if n.Op != nil {
		op = *n.Op
	}
	v, err := r.expr(*n.E)
	if err != nil {
		return nil, err
	}
	switch op {
	case "not":
		b, ok := v.(bool)
		if !ok {
			return nil, &RunError{Kind: "ContractError",
				Msg: "unop not requires bool, got " + describeJSON(v)}
		}
		return !b, nil
	case "neg":
		if f, ok := v.(float64); ok {
			return -f, nil
		}
		i, ok := toInt64(v)
		if !ok {
			return nil, &RunError{Kind: "ContractError",
				Msg: "unop neg requires int, got " + describeJSON(v)}
		}
		return -i, nil
	}
	return nil, fmt.Errorf("unop node %d has unknown operator %q", n.ID, op)
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
	return false, fmt.Errorf("operands have different types (%s vs %s)", describeJSON(a), describeJSON(b))
}

// runAgent 执行一次 agent 循环（D-034）。
//
// 语义：
//   - 把 goal 作为第一条 user 消息，把 tools[] 里声明的工具作为 function schema
//     发给模型；
//   - 模型若要求调用工具，就执行、把结果按 `role:"tool"` 回填，进入下一轮；
//   - 模型不再要求调用时返回其内容；
//   - 达到步数上界仍在要求调用 → **报错**，而不是"再试一次"。
//
// 为什么上界到了要报错：继续跑意味着花掉不可预留的钱，而这个循环存在的意义
// 正是"上界可算"。把它悄悄放宽，等于把 D-034 的取舍作废。
//
// `max_steps = unlimited`（键缺失 → MaxSteps == nil）不是"没有闸"：作者放弃的是
// **编译期**上界，运行期仍然有 agentStepCap 的硬上限（见 maxUnboundedAgentSteps）。
// 两者报的是**不同的话**：一个说"你写的上界到了"，一个说"你把上界让给了运行期"。
// 混成一句会让 unlimited 的用户以为自己声明了 max_steps = 1000000。
func (r *runner) runAgent(n *ir.Node) (any, error) {
	if n.Agent == nil {
		return nil, fmt.Errorf("agent node %d missing spec", n.ID)
	}
	// 与 callModel / callTool 同一条前置检查。**这不是防御性代码**：agent 循环
	// 的每一步都是一次模型调用，没有 Runtime 时 r.rt.Model 会在 nil 接口上
	// 解引用 —— 进程直接收到一个 Go panic（exit 2），而不是"这个 workflow 需要
	// 一个 provider"。对一个把"失败要说清楚"当卖点的运行时来说，
	// nil 解引用是最差的失败方式：它把用户的问题变成了我们的 bug。
	if r.rt == nil {
		return nil, fmt.Errorf(
			"workflow %q agent node %d needs a model call, but no Runtime is attached"+
				" (inject a fake in tests, wire up a provider in production)", r.wf.Name, n.ID)
	}
	stepCap := r.agentStepCap(n.Agent)
	// staticallyBounded 决定"到顶了"该怎么解释，所以它必须看 IR 的实际形状，
	// 而不是拿 stepCap 和 maxUnboundedAgentSteps 比 —— 作者完全可以真的写
	// max_steps = 1000000，那是有界，不是 unlimited。
	staticallyBounded := n.Agent.MaxSteps != nil && *n.Agent.MaxSteps > 0

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
	// 一轮里模型最多能要求多少个工具调用（默认 1）。循环外先算一次：
	// 工具预算与循环内的并行检查必须用同一个 k。
	k := n.Agent.MaxParallelTools
	if k < 1 {
		k = 1
	}

	// 兜底上界：有静态上界时取自 IR 声明值（编译期算出来的那个数），不是这里再猜一个。
	//
	// unlimited 时不能这么做：那种 workflow 的 bounds 只是"跑一轮"的量
	// （unbounded=true），拿它当闸会让运行期的硬上限**永远到不了** —— 第二、
	// 三次工具调用就会被判"超过声明的上界"，而声明里根本没有上界。
	// 此时的真上界是 stepCap × k，它才是这道闸的算术后果。
	maxTools := r.wf.Bounds.MaxToolCalls
	if !staticallyBounded {
		maxTools = stepCap * k
	}
	messages := []provider.Message{{Role: "user", Content: goalText}}
	for step := 1; step <= stepCap; step++ {
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
			return nil, fmt.Errorf("agent node %d: Runtime.Model did not return a ModelReply", n.ID)
		}
		if len(res.ToolCalls) == 0 {
			return res.Content, nil
		}
		// 一轮里的并行调用数有**声明上限**（默认 1，循环外已算好）。模型真的
		// 并行发多个时，不在这里拦，max_tool_calls 就不再是上界，而"预算是预留"
		// 立刻变成空话。
		if len(res.ToolCalls) > k {
			return nil, &RunError{
				Kind: "ContractError",
				Msg: fmt.Sprintf("model requested %d tool calls in one turn, exceeding declared max_parallel_tools=%d"+
					" (to enable parallelism, set it to the value you need; the upper bound becomes (1+K)*max_steps)",
					len(res.ToolCalls), k),
			}
		}

		// 把"模型要求调用"记进消息，再按**原顺序**回填结果 —— 顺序错了模型会张冠李戴。
		messages = append(messages, provider.Message{
			Role: "assistant", Content: res.Content, ToolCalls: res.ToolCalls,
		})
		if r.toolCalls > maxTools {
			// 兜底：即使前面的检查被绕过，总量也不会越过上界（有界时是 IR 声明值，
			// unlimited 时是 stepCap×k —— 见上面 maxTools 的取值）。
			return nil, &RunError{Kind: "ContractError",
				Msg: fmt.Sprintf("total tool calls %d exceed the upper bound %d", r.toolCalls, maxTools)}
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
	if !staticallyBounded {
		// 这条消息必须读起来像"运行期闸"，而不是"你声明了 1000000"：
		// 作者的源码里写的是 unlimited，IR 里**没有** max_steps 这个键。
		// 说成"达到 max_steps=1000000"会让用户去源码里找那个数字（找不到），
		// 也会让这道闸看起来像是他选的。
		return nil, &RunError{
			Kind: "ContractError",
			Msg: fmt.Sprintf("agent node %d hit the runtime hard cap of %d consecutive calls without a final answer"+
				" (the source says max_steps = unlimited: no compile-time upper bound, only this runtime cap)"+
				"; give it a real upper bound, or make it converge within a bounded number of steps", n.ID, stepCap),
		}
	}
	return nil, &RunError{
		Kind: "ContractError",
		Msg: fmt.Sprintf("agent reached max_steps=%d still requesting tool calls (the upper bound is a promise, not a starting line)",
			stepCap),
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
			return nil, fmt.Errorf("agent references unknown tool %q (tools[] has %d entries)", name, len(r.doc.Tools))
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
		return "", fmt.Errorf("agent node %d: model requested undeclared tool %q", n.ID, call.Name)
	}

	args := map[string]any{}
	if call.Arguments != "" {
		dec := json.NewDecoder(strings.NewReader(call.Arguments))
		dec.UseNumber()
		if err := dec.Decode(&args); err != nil {
			// 模型给了坏 JSON：这是契约错误，重试只会再错一次。
			return "", &RunError{Kind: "ContractError",
				Msg: "tool " + call.Name + " arguments are not valid JSON: " + err.Error()}
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
		return "", fmt.Errorf("tool %s result cannot be serialized: %w", call.Name, err)
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
			Msg: "this execution environment has no storage capability (memory.* / vector.* require a runtime with storage)"}
	}
	args := map[string]any{}
	if len(n.Args) > 0 {
		dec := json.NewDecoder(bytes.NewReader(n.Args))
		dec.UseNumber()
		if err := dec.Decode(&args); err != nil {
			return nil, fmt.Errorf("store_op args are not an object: %w", err)
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
			return "", fmt.Errorf("store_op: %s is not a node id", name)
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
				return nil, fmt.Errorf("store_op: value is not a node id")
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
				return nil, fmt.Errorf("store_op: ttl is not a node id")
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
			return 0, fmt.Errorf("ttl %q is not a valid number of days", v)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("ttl %q is not a valid duration (e.g. 30m, 24h, 7d)", v)
	}
	if d < 0 {
		return 0, fmt.Errorf("ttl cannot be negative")
	}
	return d, nil
}
