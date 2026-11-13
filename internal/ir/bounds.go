package ir

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// 复算有两个**不同**的负数结果，不是一个。为什么必须分开：
//
//   - boundUnrecognized 说的是"这张图里有我不认识的节点种类"。这是**编译器
//     bug**：codegen 新增了一个 kind，而这里没跟上。它必须响亮 —— 上界无法
//     验证，不能当成"没有上界"糊过去。
//   - boundUnknown 说的是"这个值在静态上算不出来"，而这是**合法**的：循环次数
//     取决于运行期值、agent 的 max_steps 不限、递归 call_fn。它必须诚实地空着。
//
// 用一个哨兵表示两种意思，两种情况都会出错：
// 把"不认识"当成"不可知"，新增 kind 的那天会静默通过；把"不可知"当成
// "不认识"，一个合法的 while 会被判成编译器 bug。所以宁可多一个常量。
const (
	// boundUnrecognized：遇到本文件不认识的节点种类 —— 编译器 bug，必须报。
	boundUnrecognized = -2
	// boundUnknown：值合法地在静态上不可计算 —— 不编数，跳过校验。
	boundUnknown = -1
)

// maxBoundInt 是 int 的正上限，用 const 表达式取，避免依赖 math.MaxInt 的平台宽度。
const maxBoundInt = int(^uint(0) >> 1)

// absorbSum 把一个累计和与一个孩子的值合并：**编译器 bug 的哨兵压过合法的
// 未知，两者都压过真实数字**。
//
// 为什么必须有它：旧实现是裸的 `v += span(k)`，于是 seq 里有一个 while（旧
// 实现落到 default 得 -1）就变成 `1 + (-1) + 2 = 2` —— 一个**看起来很正常**的
// 正数，却根本不是上界。校验器拿到它只会报一条引用了错误期望值的 drift，
// 或者更糟：恰好等于声明值，绿灯放行。
func absorbSum(acc, s int) int {
	if acc == boundUnrecognized || s == boundUnrecognized {
		return boundUnrecognized
	}
	if acc == boundUnknown || s == boundUnknown {
		return boundUnknown
	}
	return acc + s
}

// absorbMax 与 absorbSum 同一条规则，只是合并方式是取最大（互斥分支用）。
func absorbMax(acc, s int) int {
	if acc == boundUnrecognized || s == boundUnrecognized {
		return boundUnrecognized
	}
	if acc == boundUnknown || s == boundUnknown {
		return boundUnknown
	}
	if s > acc {
		return s
	}
	return acc
}

// scaledBound 用乘法折算子树上界（for 的 N 轮、retry 的 N+1 次）。
// 哨兵原样传出：-1 乘任何数还是"不知道"，而不是某个数字。
//
// 溢出返回 boundUnknown，**不回绕**：回绕出来的负数会被下游当成"已经排除"，
// 那是这里最危险的一种错（上界凭空变小）。
func scaledBound(s, factor int) int {
	if s < 0 {
		return s
	}
	if factor < 0 {
		return boundUnknown
	}
	prod := int64(s) * int64(factor)
	if prod > int64(maxBoundInt) {
		return boundUnknown
	}
	return int(prod)
}

// nodeAt 取 id 对应的节点，越界返回 nil（悬空 id 由 V-2 报，这里不能崩）。
func nodeAt(w *Workflow, id int) *Node {
	if w == nil || id < 0 || id >= len(w.Nodes) {
		return nil
	}
	return &w.Nodes[id]
}

// litIntValue 按要求把节点读成整数字面量；不是"int 字面量"这个形状就返回 false。
//
// **形状判据必须和 C++ 一模一样**（`kind == LitExpr && text == "int"`，
// codegen.cpp:185）：放宽一点，Go 就会算出 C++ 标成 unbounded 的数字，
// 于是合法产物被报成 E_IR_RECOMPUTE_DRIFT。
func litIntValue(n *Node) (int64, bool) {
	if n == nil || n.K != "lit" || n.LitT == nil || *n.LitT != "int" {
		return 0, false
	}
	raw := strings.TrimSpace(string(n.LitV))
	if raw == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// forTripCount 静态推导 for 的迭代次数 N，推不出来返回 boundUnknown。
//
// **这是 native/cpp/codegen.cpp:183-203 `for_iterations` 的逐条镜像**，两边
// 必须给出同一个 N —— 不是"各自合理就行"：N 不一样，SP(for) 就不一样，
// 两份实现对同一张图报不同的上界，CheckBounds 会把合法产物判成编译器 bug。
// C++ 那边给出的放弃条件（推不出来就把整个 workflow 标 unbounded）在这里
// 对应 boundUnknown，同样不编数字。
//
// 镜像的判据（顺序与 C++ 一致，多一个条件就多一处漂移的机会）：
//
//	init 是整数字面量、cond 是 `<` / `<=` 的二元运算、右操作数是整数字面量、
//	步长为正。C++ 用语法层符号 `"<"` / `"<="`，IR 里已映射成算子名
//	`lt` / `le`（codegen.cpp:1735-1737），所以这里认的是 lt/le。
//	C++ **不检查**左操作数是不是循环变量，这里也不检查：多一条判据就可能
//	在 C++ 能算的图上退回未知，那正是"两份实现不一致"。
func forTripCount(w *Workflow, n *Node) int {
	if n == nil || n.Init == nil || n.Cond == nil {
		return boundUnknown
	}
	// 缺 delta 时执行器按 1 走（engine/run.go 的 for 分支），这里跟着执行器；
	// codegen 产出的 IR 永远带 delta。
	delta := int64(1)
	if n.Delta != nil {
		delta = *n.Delta
	}
	if delta <= 0 {
		// 步长为 0 是死循环（执行器直接报错），负步长的逆向循环暂不推导上界。
		return boundUnknown
	}
	start, ok := litIntValue(nodeAt(w, *n.Init))
	if !ok {
		return boundUnknown
	}
	cond := nodeAt(w, *n.Cond)
	if cond == nil || cond.K != "binop" || cond.Op == nil || cond.R == nil {
		return boundUnknown
	}
	inclusive := false
	switch *cond.Op {
	case "lt":
		inclusive = false
	case "le":
		inclusive = true
	default:
		return boundUnknown
	}
	stop, ok := litIntValue(nodeAt(w, *cond.R))
	if !ok {
		return boundUnknown
	}
	var trip int64
	if !inclusive {
		if stop <= start {
			return 0
		}
		trip = (stop - start + delta - 1) / delta
	} else {
		if stop < start {
			return 0
		}
		trip = (stop-start)/delta + 1
	}
	if trip < 0 || trip > int64(maxBoundInt) {
		return boundUnknown
	}
	return int(trip)
}

// agentStaticSteps 取 agent 循环的**静态**步数上界；bounded=false 表示这个循环
// 没有静态上界，两个上界都必须诚实地说 boundUnknown。
//
// 为什么单独抽成一个函数：`max_steps = unlimited` 时前端**省略整个键**
// （native/cpp c344ebd 刻意不写 null、也不写一个大数 —— 写假上限会撞上
// `max_spans ≤ 64×max_steps+1` 这条 sanity 断言）。span 与调用次数两个调用点
// 都走这里，于是"缺键"只有一处判断，不会出现一处改了一处忘。
//
// **nil/≤0 一律映射成 boundUnknown，不能映射成 1 或 (1+k)**：那个数字会
// 恰好等于"跑一轮"的值，看起来像上界，于是把"我们不知道"悄悄变成"我们知道"。
//
// ≤0 也算无上界而不是报错：`max_steps: 0` 只可能来自手改/损坏的 IR（schema 要求
// minimum 1），它同样不是上界。这里不编数字、也不越权报错 —— 报错是校验器的
// 事，复算只回答"这个值能不能静态定界"。
func agentStaticSteps(spec *AgentSpec) (steps int, bounded bool) {
	if spec == nil || spec.MaxSteps == nil || *spec.MaxSteps <= 0 {
		return 0, false
	}
	return *spec.MaxSteps, true
}

// RecomputeMaxSpans 按 IR.md §5.1 的代数**独立重算**一个 workflow 的 span 上界。
//
// 为什么校验器需要它：`bounds.max_spans` 是**派生字段**，而运行期断言
// （len(spans) ≤ max_spans）直接建立在它上面。算小了，正确的 trace 会被判成
// compiler bug；算大了，上界失去意义（trace 字节上界、预算预留全部跟着松）。
// 更糟的是**没有任何检查会红**：加载器只查"≤ 64×max_steps+1"这种不等式，
// 一个偏差 1 的值永远合法。
//
// 所以这条复算的用途不是"再算一遍求安心"，而是给 codegen 一个**独立实现**的
// 对照物：两边用不同语言、不同遍历顺序算出同一个数，才算真的钉住了。
//
// 返回值：真实数字，或者 boundUnrecognized（-2，见常量注释）/ boundUnknown
// （-1）。**永不返回"看起来正常的猜数"** —— 每一个聚合点都过 absorbSum /
// absorbMax，一个未知会毒化整个表达式。
func RecomputeMaxSpans(w *Workflow) int {
	memo := make(map[int]int, len(w.Nodes))
	var span func(id int) int
	span = func(id int) int {
		// 悬空 id 不在这里报错（V-2 已经报了），也**不能崩**：
		// 校验器崩在错误路径上会盖掉它本来要报的那条诊断。
		if id < 0 || id >= len(w.Nodes) {
			return 0
		}
		if v, ok := memo[id]; ok {
			return v
		}
		n := &w.Nodes[id]
		// 先占位，防止环路导致无限递归；环本身由 V-3 报，这里只要不死循环。
		// 占位值用 boundUnknown 而不是 0：万一真走到这个占位上（只可能因为
		// 环路），"不知道"是诚实的，0 是凭空给出的下界。
		memo[id] = boundUnknown
		v := 0
		switch n.K {
		case "lit", "var", "interp", "nop":
			v = 0
		case "field", "index":
			// 表达式节点必须把子树传上来：`m.generate(…).field` 里的调用
			// 真实执行了，丢掉就等于上界小于实际。
			if n.Of != nil {
				v = absorbSum(v, span(*n.Of))
			}
		// 2.0.1：stop 的 reason/data、mem_txn 的快照、emit 的负载都是表达式，
		// 必须把子树的上界传上来，丢掉就等于"上界小于实际"。
		case "stop":
			if n.E != nil {
				v = absorbSum(v, span(*n.E))
			}
			if n.Data != nil {
				v = absorbSum(v, span(*n.Data))
			}
		case "mem_txn":
			if n.E != nil {
				v = absorbSum(v, span(*n.E))
			}
		case "emit":
			if len(n.Args) > 0 {
				var ids []int
				if json.Unmarshal(n.Args, &ids) == nil {
					for _, id := range ids {
						v = absorbSum(v, span(id))
					}
				}
			}
		case "on":
			// 槽体在**定义处不执行**：它是 emit 命中时才跑的一段代码。
			// 若把它算进定义点的 span，上界会比实际大（而且与 codegen 写的
			// span_[on]=0 对不上，直接触发 E_IR_RECOMPUTE_DRIFT）。
			v = absorbSum(v, 0)
		case "sleep":
			if n.MS != nil {
				v = absorbSum(v, span(*n.MS))
			}
		case "until":
			if n.TimeoutMS != nil {
				v = absorbSum(v, span(*n.TimeoutMS))
			}
		case "cast", "unop", "len":
			if n.E != nil {
				v = absorbSum(v, span(*n.E))
			}
		case "binop":
			if n.L != nil {
				v = absorbSum(v, span(*n.L))
			}
			if n.R != nil {
				v = absorbSum(v, span(*n.R))
			}
		case "at":
			if n.Of != nil {
				v = absorbSum(v, span(*n.Of))
			}
			if n.I != nil {
				v = absorbSum(v, span(*n.I))
			}
		case "array_lit":
			for _, k := range n.Kids {
				v = absorbSum(v, span(k))
			}
		case "call_model", "call_tool":
			v = 1
		case "call_fn":
			// 一次调用算一个 span。函数体没有内联，所以这不是真正的上界 ——
			// codegen 会把含 call_fn 的 workflow 标 unbounded，让 explain 明说。
			// 两边必须用同一个数，否则 CheckBounds 会把合法产物判成编译器 bug。
			v = 1
		case "call_native":
			// 一次原生调用算一个 span。它与 call_fn / call_model / call_tool 不同：
			// C 函数体在 cgo 链接进来的归档里，Go 这边看不见，所以 SP 不是真正的
			// 上界 —— 含 call_native 的 workflow 会被 C++ 前端标 unbounded（unbounded 标志
			// 由前端写、Go 只消费），实际执行次数由运行期闸住。但 SP(call_native) 仍是
			// **一个真实可数的常数 1**，不是 boundUnknown：否则复算落到 default 拿不到 1，
			// stageDerived 的等式校验会被整段跳过，那个 workflow 的 max_spans 就再也没人验。
			// 这里必须返回真数字，与 call_fn 一侧的契约一致。
			v = 1
		case "store_op":
			// 一次存储操作 = 1 个 span（codegen.cpp:1584 那一行 `span_[id] = 1`）。
			//
			// **参数表达式的 span 不计**，与 call_model / call_tool 同一条先例：
			// args 里的节点是纯表达式（归约器保证），C++ 也没有把它们的 span 加上来。
			// 两边一起"漏"才是一致的；这里多算一个数，CheckBounds 就会把合法
			// 产物报成 drift。
			//
			// 这个 case 曾经**不存在** —— kind 是 store_op 的节点一路落到
			// default，复算返回 boundUnrecognized，而旧代码的 `got >= 0` 把哨兵
			// 吞掉了，所以"memory.get 的产物从未被复算验证过"这件事一直没人发现。
			// 它现在被加载期报了出来（validate.go 的 stageDerived），这就是那条
			// 哨兵存在的意义。
			v = 1
		case "decl", "destructure", "assign":
			// 这里按 **IR 语义**独立复算，不照抄 C++ 的代数简化：assign.e 在
			// engine/run.go 里会被 r.expr 真正求值，其中的 span 理应计入上界；
			// 而 codegen.cpp 的 AssignStmt 把 span_[assign] 直接置 0、不传播 e。
			// 当前归约器对每个分支表达式做纯度检查，e 恒纯，两边数值都是 0、
			// CheckBounds 全绿；但**故意不写成 v = 0**：将来若放宽纯度让 e 带上
			// 调用，Go 会先算出更大的数并报 drift，提前暴露 C++ 的漏算。照抄 0
			// 会让两边一起错、加载期静默通过、直到运行期断言 len(spans) ≤
			// max_spans 才炸。这条 case 既是独立复算，也是未来放宽纯度的守卫。
			if n.E != nil {
				v = absorbSum(v, span(*n.E))
			}
		case "return", "exprstmt":
			// 两者是同一套代数（codegen.cpp:2314-2320 / 2322-2328）：
			// span_[id] = span_[e]，表达式语句只是"不要这个值"。
			// `memory.put(...)` 这种有副作用的调用就是靠 exprstmt 被执行的
			// （codegen.cpp:2305-2317），所以它的 span 必须真的计进去。
			//
			// exprstmt 也曾是**漏掉的 kind**：`memory.put(...)` 单独成句时
			// 复算不认识它，而旧代码的 `got >= 0` 把"不认识"当成"跳过"。
			if n.E != nil {
				v = absorbSum(v, span(*n.E))
			}
		case "seq":
			for _, k := range n.Kids {
				v = absorbSum(v, span(k))
			}
		case "if":
			best := 0
			if n.Then != nil {
				best = absorbMax(best, span(*n.Then))
			}
			for _, a := range n.Elifs {
				best = absorbMax(best, span(a.Body))
			}
			if n.Else != nil {
				best = absorbMax(best, span(*n.Else))
			}
			v = absorbSum(1, best)
		case "match":
			best := 0
			for _, a := range n.Arms {
				best = absorbMax(best, span(a.Body))
			}
			if n.Default != nil {
				best = absorbMax(best, span(*n.Default))
			}
			v = absorbSum(1, best)
		case "parallel":
			v = 1
			for _, b := range n.Branches {
				v = absorbSum(v, span(b))
			}
		case "retry":
			// SP(retry) = 1 + (n+1) × SP(body)
			body := 0
			if n.Body != nil {
				body = span(*n.Body)
			}
			attempts := 1
			if n.N != nil && *n.N >= 0 {
				attempts = *n.N + 1
			}
			v = absorbSum(1, scaledBound(body, attempts))
		case "for":
			// SP(for) = 1 + N × SP(body)，N 由 forTripCount 静态推导。
			//
			// 推不出来（cond 依赖运行期值、init 不是字面量、步长非正）时必须
			// 是 boundUnknown：C++ 在同一个判据上把整个 workflow 标 unbounded
			// （codegen.cpp:2051-2057），它写的那个数**不是**上界。这里若照抄
			// 它的数字（1 + SP(body)），两份实现会一致地错；这里若编一个 N，
			// 两份实现会不一致地错。只有"不知道"是对的。
			body := 0
			if n.Body != nil {
				body = span(*n.Body)
			}
			v = absorbSum(1, scaledBound(body, forTripCount(w, n)))
		case "while":
			// 循环次数由运行期值决定，**按构造**就没有静态上界
			// （codegen.cpp:2013-2027 置 unbounded_ = true，写的数是"不真实的
			// 上界"）。这里返回 boundUnknown，让 CheckBounds 跳过等式校验，
			// 而不是拿一个数字去和声明值比。
			v = boundUnknown
		case "break", "continue":
			// 它们只表达"跳到哪"，本身不产生 span：C++ 是 span_[id] = 0
			// （codegen.cpp:1997-2011），执行器里也只置一个跳转意图
			// （engine/run.go 的 break/continue 分支）。这里同样是 0。
			v = 0
		case "agent":
			// 每步一次模型调用 + 最多 K 个工具调用，各自一个 span。
			// 上界因此是 (1+K)×max_steps —— 不是"看起来差不多"的常数。
			if n.Agent == nil {
				// kind 是 agent 却没有规格：codegen 不可能产出这种节点。
				// 这是编译器 bug，不是"不知道"。
				v = boundUnrecognized
				break
			}
			steps, bounded := agentStaticSteps(n.Agent)
			if !bounded {
				v = boundUnknown
				break
			}
			k := n.Agent.MaxParallelTools
			if k < 1 {
				k = 1
			}
			v = (1 + k) * steps
		case "try":
			v = 1
			if n.Body != nil {
				v = absorbSum(v, span(*n.Body))
			}
			best := 0
			for _, c := range n.Catches {
				best = absorbMax(best, span(c.Body))
			}
			v = absorbSum(v, best)
		case "fallback":
			// **main 与 body 相加**：main 失败前可能已经花掉一部分预算
			// （模型调了、工具超时了），body 才接着跑。取 max 会低估上界，
			// 而低估的上界会让运行期把合法行为判成超界。
			v = 1
			if n.Main != nil {
				v = absorbSum(v, span(*n.Main))
			}
			if n.Body != nil {
				v = absorbSum(v, span(*n.Body))
			}
		default:
			// 新增 kind 时**必须**在这里加分支。返回 0 是最坏的退化：
			// 上界变小 → 运行期正确行为被判成 compiler bug，而且没有测试会红。
			v = boundUnrecognized
		}
		memo[id] = v
		return v
	}

	if w.Root < 0 || w.Root >= len(w.Nodes) {
		// 根越界是结构错误（V-2 会报），这里保持"响亮"：用 unrecognized
		// 而不是 unknown，免得一张坏图被当成"合法地算不出来"而跳过校验。
		return boundUnrecognized
	}
	return span(w.Root)
}

// callPair 是一对调用次数上界（模型 / 工具）。两个分量**各自**带哨兵：
// 只有模型侧未知时，工具侧的数字仍然可以被验证。
type callPair struct{ m, t int }

// add 对应 seq / parallel / try 的求和语义：任一分量未知则该分量未知。
func (p callPair) add(s callPair) callPair {
	return callPair{absorbSum(p.m, s.m), absorbSum(p.t, s.t)}
}

// max 对应 if / match 的互斥分支语义：取最大，未知优先。
func (p callPair) max(s callPair) callPair {
	return callPair{absorbMax(p.m, s.m), absorbMax(p.t, s.t)}
}

// mul 对应 for / retry 的乘倍数语义。
func (p callPair) mul(n int) callPair {
	return callPair{scaledBound(p.m, n), scaledBound(p.t, n)}
}

// RecomputeCalls 独立复算"模型调用次数"与"工具调用次数"的上界。
//
// 与 max_spans 同构的代数（seq 求和 / 分支取最大 / retry 乘倍数），
// 只有叶节点不同。**为什么要单独复算**：这两个字段曾经在
// `parallel → destructure → arm seq` 这条链上少算了一半，而 SP 是对的 ——
// 只查 SP 的复算完全看不见它。复算一个字段只防住那个字段。
//
// 返回值语义与 RecomputeMaxSpans 一致：真实数字、boundUnrecognized、或
// boundUnknown，逐分量。
func RecomputeCalls(w *Workflow) (model, tool int) {
	memo := make(map[int]callPair, len(w.Nodes))
	var walk func(id int) callPair
	walk = func(id int) callPair {
		if id < 0 || id >= len(w.Nodes) {
			return callPair{}
		}
		if m, ok := memo[id]; ok {
			return m
		}
		// 占位：与 span 同理，环路只可能由 V-3 报，这里用"不知道"占位。
		memo[id] = callPair{boundUnknown, boundUnknown}
		n := &w.Nodes[id]
		var r callPair
		switch n.K {
		case "call_model":
			r = callPair{1, 0}
		case "call_tool":
			r = callPair{0, 1}
		// 2.0.1：控制流/事务/信号节点本身不发起模型或工具调用；它们只是把
		// 子表达式的结果搬来搬去，所以 0/0，与 break/continue 同类。
		case "stop", "mem_txn", "emit", "on", "sleep", "until":
			r = callPair{0, 0}
		case "lit", "var", "interp", "nop", "break", "continue":
			// 叶子上界（以及 break/continue 这类无 span 的跳转）都是 0/0。
			// **必须显式列出**：加了 default 之后，漏一个就是 unrecognized，
			// 而它们本来是安静的正常节点。
			r = callPair{0, 0}
		case "call_fn":
			// 与 C++ 一致：call_fn 记 span 1，但调用次数记 0/0
			// （codegen.cpp:1226-1228 只写 span_，model_/tool_ 保持 0）。
			r = callPair{0, 0}
		case "call_native":
			// 原生调用既不是模型调用也不是工具调用：它走 cgo 链接的 C 函数，
			// 不计入 model_ / tool_ 上界。两边都是 0/0（与 call_fn 同义）。
			r = callPair{0, 0}
		case "store_op":
			// codegen.cpp:1585-1586：
			//   model_[id] = (vector.put) || (query) ? 1 : 0;  tool_[id] = 0;
			// 理由：向量的写入与查询要调 embeddings，那是一次模型调用；
			// memory.* 只碰本地存储，不算模型调用。**工具调用恒为 0** ——
			// 存储子系统不经 tools[]，它走 Runtime 的 store 句柄。
			//
			// 这个分量错了不会崩，只会让 max_model_calls 少算（= 上界变小
			// = "预算是预留"不再成立），所以必须与 C++ 逐字一致。
			m := 0
			if n.Op != nil && (*n.Op == "vector.put" || *n.Op == "vector.query") {
				m = 1
			}
			r = callPair{m, 0}
		case "field", "index":
			if n.Of != nil {
				r = walk(*n.Of)
			}
		case "cast", "unop", "len":
			if n.E != nil {
				r = walk(*n.E)
			}
		case "binop":
			if n.L != nil {
				r = r.add(walk(*n.L))
			}
			if n.R != nil {
				r = r.add(walk(*n.R))
			}
		case "at":
			if n.Of != nil {
				r = r.add(walk(*n.Of))
			}
			if n.I != nil {
				r = r.add(walk(*n.I))
			}
		case "array_lit":
			for _, k := range n.Kids {
				r = r.add(walk(k))
			}
		case "decl", "destructure", "assign":
			// 与 RecomputeMaxSpans 同一条设计意图：按 IR 语义，assign.e 会被
			// 求值，其中的 call_model / call_tool 应计入调用次数上界；C++ 把
			// model_/tool_[assign] 固定为 0 是简化，这里不照抄。当前 e 恒纯，
			// 复算与声明都是 0；若将来放宽纯度，Go 会先报 max_model_calls /
			// max_tool_calls drift，成为 C++ 漏算的守卫。
			if n.E != nil {
				r = walk(*n.E)
			}
		case "return", "exprstmt":
			// 与 C++ 一致：两者的 model_/tool_ 都原样传自 e
			// （codegen.cpp:2314-2320 / 2322-2328）。exprstmt 里的
			// `vector.put` 会带一次 embeddings 模型调用，漏掉它就是把
			// max_model_calls 算小。
			if n.E != nil {
				r = walk(*n.E)
			}
		case "seq":
			for _, k := range n.Kids {
				r = r.add(walk(k))
			}
		case "if":
			if n.Then != nil {
				r = walk(*n.Then)
			}
			for _, a := range n.Elifs {
				r = r.max(walk(a.Body))
			}
			if n.Else != nil {
				r = r.max(walk(*n.Else))
			}
		case "match":
			for _, a := range n.Arms {
				r = r.max(walk(a.Body))
			}
			if n.Default != nil {
				r = r.max(walk(*n.Default))
			}
		case "parallel":
			for _, b := range n.Branches {
				r = r.add(walk(b))
			}
		case "for":
			// C++：能算出 N 就是 N×body，算不出来就标 unbounded
			// （codegen.cpp:2051-2062）。这里对应 N×body 或 boundUnknown。
			trip := forTripCount(w, n)
			if trip < 0 {
				r = callPair{boundUnknown, boundUnknown}
			} else if n.Body != nil {
				r = walk(*n.Body).mul(trip)
			} else {
				r = callPair{0, 0}
			}
		case "while":
			// 迭代次数运行期才知道 → 两个分量都不可静态定界。
			r = callPair{boundUnknown, boundUnknown}
		case "agent":
			if n.Agent == nil {
				r = callPair{boundUnrecognized, boundUnrecognized}
				break
			}
			steps, bounded := agentStaticSteps(n.Agent)
			if !bounded {
				// 无静态步数上界 → 每步的调用次数乘不上任何已知轮数。
				r = callPair{boundUnknown, boundUnknown}
				break
			}
			k := n.Agent.MaxParallelTools
			if k < 1 {
				k = 1
			}
			r.m = steps
			r.t = k * steps
		case "try":
			// **body 与 catch 相加**：体可能执行了一部分才抛，然后 catch 再跑。
			// 两者不是互斥的 —— 取大等于假设"要么全跑体、要么直接进 catch"，
			// 而那不是 try 的语义。多个 catch 之间才互斥，所以取最大。
			if n.Body != nil {
				r = walk(*n.Body)
			}
			best := callPair{0, 0}
			for _, c := range n.Catches {
				best = best.max(walk(c.Body))
			}
			r = r.add(best)
		case "retry":
			attempts := 1
			if n.N != nil && *n.N >= 0 {
				attempts = *n.N + 1
			}
			if n.Body != nil {
				r = walk(*n.Body).mul(attempts)
			}
		case "fallback":
			// main 与 body **相加**：两条路径都可能执行（与 span 一侧同一条理由）。
			if n.Main != nil {
				r = r.add(walk(*n.Main))
			}
			if n.Body != nil {
				r = r.add(walk(*n.Body))
			}
		default:
			// 旧实现**没有 default**，于是这里落成 pair{0,0} —— "没有模型调用、
			// 没有工具调用"，静默地把上界变小。现在与 span 一侧对齐：不认识
			// 就是 boundUnrecognized。
			r = callPair{boundUnrecognized, boundUnrecognized}
		}
		memo[id] = r
		return r
	}
	if w.Root < 0 || w.Root >= len(w.Nodes) {
		return boundUnrecognized, boundUnrecognized
	}
	r := walk(w.Root)
	return r.m, r.t
}

// CheckBounds 把复算结果与 IR 里声明的上界比对，返回一条可读的差异描述。
// 相等时返回空串。
//
// 三种结果必须分开处理，否则某个字段的诚实会毁掉另一个字段的可验证性：
//
//	boundUnrecognized → 报错（编译器 bug，不能静默）
//	boundUnknown      → **跳过该字段**的等式校验（合法地算不出来，不是错）
//	真实数字          → 只有两边都已知时才比，只在真漂移时报
func CheckBounds(w *Workflow) string {
	got := RecomputeMaxSpans(w)
	mc, tc := RecomputeCalls(w)
	if got == boundUnrecognized {
		return fmt.Sprintf("workflow %q: recompute hit an unrecognized node kind; upper bound cannot be verified", w.Name)
	}
	if mc == boundUnrecognized || tc == boundUnrecognized {
		return fmt.Sprintf("workflow %q: call-count recompute hit an unrecognized node kind", w.Name)
	}
	if got >= 0 && got != w.Bounds.MaxSpans {
		return fmt.Sprintf("workflow %q: bounds.max_spans declares %d, but §5.1 algebraic recompute yields %d",
			w.Name, w.Bounds.MaxSpans, got)
	}
	if mc >= 0 && mc != w.Bounds.MaxModelCalls {
		return fmt.Sprintf("workflow %q: bounds.max_model_calls declares %d, recompute yields %d",
			w.Name, w.Bounds.MaxModelCalls, mc)
	}
	if tc >= 0 && tc != w.Bounds.MaxToolCalls {
		return fmt.Sprintf("workflow %q: bounds.max_tool_calls declares %d, recompute yields %d",
			w.Name, w.Bounds.MaxToolCalls, tc)
	}
	return ""
}
