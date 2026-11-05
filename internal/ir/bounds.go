package ir

import "fmt"

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
// 返回 -1 表示遇到无法识别的节点种类（新增 kind 时**必须**在这里加分支，
// 否则会静默返回 0 并让上界变小 —— 这正是要防的那类退化）。
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
		memo[id] = 0
		v := 0
		switch n.K {
		case "lit", "var", "interp", "nop":
			v = 0
		case "field", "index":
			// 表达式节点必须把子树传上来：`m.generate(…).field` 里的调用
			// 真实执行了，丢掉就等于上界小于实际。
			if n.Of != nil {
				v = span(*n.Of)
			}
		case "cast", "unop", "len":
			if n.E != nil {
				v = span(*n.E)
			}
		case "binop":
			if n.L != nil {
				v += span(*n.L)
			}
			if n.R != nil {
				v += span(*n.R)
			}
		case "at":
			if n.Of != nil {
				v += span(*n.Of)
			}
			if n.I != nil {
				v += span(*n.I)
			}
		case "array_lit":
			for _, k := range n.Kids {
				v += span(k)
			}
		case "call_model", "call_tool":
			v = 1
		case "call_fn":
			// 一次调用算一个 span。函数体没有内联，所以这不是真正的上界 ——
			// codegen 会把含 call_fn 的 workflow 标 unbounded，让 explain 明说。
			// 两边必须用同一个数，否则 CheckBounds 会把合法产物判成编译器 bug。
			v = 1
		case "let", "destructure":
			if n.E != nil {
				v = span(*n.E)
			}
		case "return":
			if n.E != nil {
				v = span(*n.E)
			}
		case "seq":
			for _, k := range n.Kids {
				v += span(k)
			}
		case "if":
			best := 0
			if n.Then != nil {
				best = span(*n.Then)
			}
			for _, a := range n.Elifs {
				if s := span(a.Body); s > best {
					best = s
				}
			}
			if n.Else != nil {
				if s := span(*n.Else); s > best {
					best = s
				}
			}
			v = 1 + best
		case "match":
			best := 0
			for _, a := range n.Arms {
				if s := span(a.Body); s > best {
					best = s
				}
			}
			if n.Default != nil {
				if s := span(*n.Default); s > best {
					best = s
				}
			}
			v = 1 + best
		case "parallel":
			v = 1
			for _, b := range n.Branches {
				v += span(b)
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
			v = 1 + attempts*body
		case "agent":
			// 每步一次模型调用 + 最多 K 个工具调用，各自一个 span。
			// 上界因此是 (1+K)×max_steps —— 不是"看起来差不多"的常数。
			if n.Agent != nil {
				k := n.Agent.MaxParallelTools
				if k < 1 {
					k = 1
				}
				v = (1 + k) * n.Agent.MaxSteps
			} else {
				v = -1
			}
		case "try":
			v = 1
			if n.Body != nil {
				v += span(*n.Body)
			}
			best := 0
			for _, c := range n.Catches {
				if s := span(c.Body); s > best {
					best = s
				}
			}
			v += best
		default:
			v = -1
		}
		memo[id] = v
		return v
	}

	if w.Root < 0 || w.Root >= len(w.Nodes) {
		return -1
	}
	return span(w.Root)
}

// RecomputeCalls 独立复算"模型调用次数"与"工具调用次数"的上界。
//
// 与 max_spans 同构的代数（seq 求和 / 分支取最大 / retry 乘倍数），
// 只有叶节点不同。**为什么要单独复算**：这两个字段曾经在
// `parallel → destructure → arm seq` 这条链上少算了一半，而 SP 是对的 ——
// 只查 SP 的复算完全看不见它。复算一个字段只防住那个字段。
func RecomputeCalls(w *Workflow) (model, tool int) {
	memoM := make(map[int]int, len(w.Nodes))
	memoT := make(map[int]int, len(w.Nodes))
	type pair struct{ m, t int }
	var walk func(id int) pair
	walk = func(id int) pair {
		if id < 0 || id >= len(w.Nodes) {
			return pair{}
		}
		if m, ok := memoM[id]; ok {
			return pair{m, memoT[id]}
		}
		memoM[id], memoT[id] = 0, 0
		n := &w.Nodes[id]
		var r pair
		switch n.K {
		case "call_model":
			r = pair{1, 0}
		case "call_tool":
			r = pair{0, 1}
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
				s := walk(*n.L)
				r.m += s.m
				r.t += s.t
			}
			if n.R != nil {
				s := walk(*n.R)
				r.m += s.m
				r.t += s.t
			}
		case "at":
			if n.Of != nil {
				s := walk(*n.Of)
				r.m += s.m
				r.t += s.t
			}
			if n.I != nil {
				s := walk(*n.I)
				r.m += s.m
				r.t += s.t
			}
		case "array_lit":
			for _, k := range n.Kids {
				s := walk(k)
				r.m += s.m
				r.t += s.t
			}
		case "let", "destructure":
			if n.E != nil {
				r = walk(*n.E)
			}
		case "return":
			if n.E != nil {
				r = walk(*n.E)
			}
		case "seq":
			for _, k := range n.Kids {
				s := walk(k)
				r.m += s.m
				r.t += s.t
			}
		case "if":
			if n.Then != nil {
				r = walk(*n.Then)
			}
			for _, a := range n.Elifs {
				s := walk(a.Body)
				if s.m > r.m {
					r.m = s.m
				}
				if s.t > r.t {
					r.t = s.t
				}
			}
			if n.Else != nil {
				s := walk(*n.Else)
				if s.m > r.m {
					r.m = s.m
				}
				if s.t > r.t {
					r.t = s.t
				}
			}
		case "match":
			for _, a := range n.Arms {
				s := walk(a.Body)
				if s.m > r.m {
					r.m = s.m
				}
				if s.t > r.t {
					r.t = s.t
				}
			}
			if n.Default != nil {
				s := walk(*n.Default)
				if s.m > r.m {
					r.m = s.m
				}
				if s.t > r.t {
					r.t = s.t
				}
			}
		case "parallel":
			for _, b := range n.Branches {
				s := walk(b)
				r.m += s.m
				r.t += s.t
			}
		case "agent":
			if n.Agent != nil {
				k := n.Agent.MaxParallelTools
				if k < 1 {
					k = 1
				}
				r.m = n.Agent.MaxSteps
				r.t = k * n.Agent.MaxSteps
			}
		case "try":
			// **body 与 catch 相加**：体可能执行了一部分才抛，然后 catch 再跑。
			// 两者不是互斥的 —— 取大等于假设"要么全跑体、要么直接进 catch"，
			// 而那不是 try 的语义。多个 catch 之间才互斥，所以取最大。
			if n.Body != nil {
				r = walk(*n.Body)
			}
			bestM, bestT := 0, 0
			for _, c := range n.Catches {
				s := walk(c.Body)
				if s.m > bestM {
					bestM = s.m
				}
				if s.t > bestT {
					bestT = s.t
				}
			}
			r.m += bestM
			r.t += bestT
		case "retry":
			attempts := 1
			if n.N != nil && *n.N >= 0 {
				attempts = *n.N + 1
			}
			if n.Body != nil {
				s := walk(*n.Body)
				r = pair{s.m * attempts, s.t * attempts}
			}
		}
		memoM[id], memoT[id] = r.m, r.t
		return r
	}
	if w.Root < 0 || w.Root >= len(w.Nodes) {
		return -1, -1
	}
	r := walk(w.Root)
	return r.m, r.t
}

// CheckBounds 把复算结果与 IR 里声明的上界比对，返回一条可读的差异描述。
// 相等时返回空串。
func CheckBounds(w *Workflow) string {
	got := RecomputeMaxSpans(w)
	if got < 0 {
		return fmt.Sprintf("workflow %q: 复算遇到无法识别的节点种类，上界无法验证", w.Name)
	}
	if got != w.Bounds.MaxSpans {
		return fmt.Sprintf("workflow %q: bounds.max_spans 声明 %d，按 §5.1 代数复算应为 %d",
			w.Name, w.Bounds.MaxSpans, got)
	}
	mc, tc := RecomputeCalls(w)
	if mc < 0 {
		return fmt.Sprintf("workflow %q: 调用次数复算遇到无法识别的节点种类", w.Name)
	}
	if mc != w.Bounds.MaxModelCalls {
		return fmt.Sprintf("workflow %q: bounds.max_model_calls 声明 %d，复算应为 %d",
			w.Name, w.Bounds.MaxModelCalls, mc)
	}
	if tc != w.Bounds.MaxToolCalls {
		return fmt.Sprintf("workflow %q: bounds.max_tool_calls 声明 %d，复算应为 %d",
			w.Name, w.Bounds.MaxToolCalls, tc)
	}
	return ""
}
