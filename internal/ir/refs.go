package ir

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Table 是引用指向的表。
type Table int

const (
	TableNode Table = iota
	TableVar
	TableType
	TableSchema
	TableTool
	TableBackend
	TableFunction
	TableNative
	numTables
)

func (t Table) String() string {
	switch t {
	case TableNode:
		return "nodes"
	case TableVar:
		return "vars"
	case TableType:
		return "types"
	case TableSchema:
		return "schemas"
	case TableTool:
		return "tools"
	case TableBackend:
		return "backends"
	case TableFunction:
		return "functions"
	case TableNative:
		return "natives"
	}
	return "unknown"
}

// IsNamespace 为 true 表示这张表按**名字**索引（tools / backends / functions /
// natives 是符号表）；其余按**下标**索引（nodes / vars / types / schemas 是数组）。
//
// 这个区分是 V-2 不再漏检的关键：IR 里两种引用形态混用，而它们的查法是
// 完全不同的（越界 vs 名字不存在），报错位置和错误码也不同。
func (t Table) IsNamespace() bool {
	return t == TableTool || t == TableBackend || t == TableFunction || t == TableNative
}

// Ref 是一条**归一化后的引用**。
//
// 归一化的意义：如果按字段逐个写检查（`if n.Kids != nil { 查 kids }`
// `if n.E != nil { 查 e }` ...），新增一个 node kind 时**必然漏**——
// 新 kind 的作者不会记得去改校验器。而这里 agent 只写一份解析逻辑，
// 新 kind 只需在 appendNodeRefs 里加一个 case。
type Ref struct {
	Table Table
	// WF 是这条引用所属的 workflow 下标（-1 = 不涉及 workflow，例如 types[] 内部）。
	// nodes / vars 是**每个 workflow 各自的**表，所以"表大小"必须靠 WF 才能确定；
	// 用全局大小会误报（一个 workflow 只有 3 个节点，另一个有 100 个）。
	WF    int
	Index int    // 下标表用
	Name  string // 名字表用
	From  string // JSON 路径，报错定位用（"workflows[0].nodes[3].kids[1]"）
	Loc   Loc

	// AllowInput 表示这个位置允许 -1。
	// 只有 `Node.Var` 需要：-1 是 input 隐式根变量的哨兵（IR.md §4.3），
	// 它不在 vars[] 里，所以不算悬空。漏了这条会把所有用到输入参数的 IR 判死。
	AllowInput bool
}

// pathf 是 Sprintf 的短别名，只为让下面的路径拼接读起来不那么吵。
func pathf(format string, args ...any) string { return fmt.Sprintf(format, args...) }

// collectRefs 遍历整份 IR，产出**全部**引用。
//
// 返回条数本身是断言目标：golden IR 的引用总数固定，新增 node kind 却没在
// appendNodeRefs 里登记时，条数会少 —— 这比"漏检一条悬空引用"更早暴露。
func collectRefs(d *Doc) []Ref {
	var refs []Ref

	for ti := range d.Types {
		td := &d.Types[ti]
		p := pathf("types[%d]", ti)
		for fi := range td.Fields {
			refs = appendTypeRefs(refs, -1, &td.Fields[fi].T, pathf("%s.fields[%d].t", p, fi), td.Loc)
		}
	}

	for wi := range d.Workflows {
		w := &d.Workflows[wi]
		p := pathf("workflows[%d]", wi)

		// backend 是**名字**引用（符号表），不是下标。
		// **空串不是引用**：没有默认 backend 的 workflow 合法（它必须在调用处指定
		// backend，或在 .h 里由声明约束）。把它当成引用会让所有不带 backend 参数的
		// workflow 被判悬空——而"没有"与"指向一个不存在的名字"是两件事。
		if w.Backend != "" {
			refs = append(refs, Ref{Table: TableBackend, WF: wi, Name: w.Backend, From: p + ".backend", Loc: w.Loc})
		}

		if w.Input != nil {
			refs = appendTypeRefs(refs, wi, w.Input, p+".input", w.Loc)
		}
		refs = appendTypeRefs(refs, wi, &w.Returns, p+".returns", w.Loc)

		refs = append(refs, Ref{Table: TableNode, WF: wi, Index: w.Root, From: p + ".root", Loc: w.Loc})

		for ni := range w.Nodes {
			refs = appendNodeRefs(refs, wi, &w.Nodes[ni], pathf("%s.nodes[%d]", p, ni))
		}

		for vi := range w.Vars {
			vp := pathf("%s.vars[%d]", p, vi)
			v := &w.Vars[vi]
			refs = appendTypeRefs(refs, wi, &v.T, vp+".t", w.Loc)
			// V-5 的一端：var.decl 指向绑定它的 `decl` 节点。
			// 另一端（decl.var 指回这个 var）在 appendNodeRefs 里。
			refs = append(refs, Ref{Table: TableNode, WF: wi, Index: v.Decl, From: vp + ".decl", Loc: w.Loc})
		}
	}
	return refs
}

// appendTypeRefs 递归展开类型表达式。array/httpresult 的 of 是单个表达式，
// tuple 的 of 是表达式数组 —— 同一个键两种形态，所以 of 先存 RawMessage。
func appendTypeRefs(refs []Ref, wf int, t *TypeExpr, p string, loc Loc) []Ref {
	if t == nil {
		return refs
	}
	switch t.K {
	case "struct":
		if t.ID != nil {
			refs = append(refs, Ref{Table: TableType, WF: wf, Index: *t.ID, From: p + ".id", Loc: loc})
		}
	case "array", "httpresult":
		if len(t.Of) > 0 {
			var sub TypeExpr
			if err := json.Unmarshal(t.Of, &sub); err == nil {
				refs = appendTypeRefs(refs, wf, &sub, p+".of", loc)
			}
		}
	case "tuple":
		if len(t.Of) > 0 {
			var subs []TypeExpr
			if err := json.Unmarshal(t.Of, &subs); err == nil {
				for i := range subs {
					refs = appendTypeRefs(refs, wf, &subs[i], pathf("%s.of[%d]", p, i), loc)
				}
			}
		}
	}
	// 6 个基础类型（string/int/double/bool/json/cost）没有附加字段；
	// double 常量用位模式表达，不写进这里的类型表达式。
	return refs
}

// appendNodeRefs 按 node kind 分出它的全部引用。**新增 kind 必须在这里加 case**，
// 否则那条 kind 的引用不会被校验（这是本文件唯一需要人工维护的地方）。
func appendNodeRefs(refs []Ref, wf int, n *Node, p string) []Ref {
	node := func(idx int, field string) []Ref {
		return append(refs, Ref{Table: TableNode, WF: wf, Index: idx, From: p + "." + field, Loc: n.Loc})
	}
	nodePtr := func(idx *int, field string) []Ref {
		if idx == nil {
			return refs
		}
		return append(refs, Ref{Table: TableNode, WF: wf, Index: *idx, From: p + "." + field, Loc: n.Loc})
	}
	varRef := func(idx int, field string, allowInput bool) []Ref {
		return append(refs, Ref{Table: TableVar, WF: wf, Index: idx, From: p + "." + field, Loc: n.Loc, AllowInput: allowInput})
	}
	// varPtrRef 处理解构绑定的元素：**null = `_` 丢弃位**，不是一条引用。
	// 若把 nil 当成 0 号变量，`auto [_, x] = …` 会静默变成"引用了第 0 个变量"。
	varPtrRef := func(refs []Ref, idx *int, wf int, loc Loc, path string) []Ref {
		if idx == nil {
			return refs
		}
		return append(refs, Ref{Table: TableVar, WF: wf, Index: *idx, From: path, Loc: loc, AllowInput: true})
	}

	switch n.K {
	// ---- 语句节点（9）----
	case "agent":
		// agent 的 goal 是节点引用；tools 是**名字**（不是节点），不进引用图。
		if n.Agent != nil {
			refs = append(refs, nodePtr(n.Agent.Goal, "agent.goal")...)
		}
	case "array_lit":
		for i, k := range n.Kids {
			refs = append(refs, Ref{Table: TableNode, WF: wf, Index: k,
				From: pathf("%s.kids[%d]", p, i), Loc: n.Loc})
		}
	case "assign":
		refs = append(refs, nodePtr(n.E, "e")...)
	case "break", "continue":
		// 没有引用：它们只表达"跳到哪"，跳转目标由所在的 for 决定。
	case "while":
		refs = append(refs, nodePtr(n.C, "c")...)
		refs = append(refs, nodePtr(n.Body, "body")...)
	case "for":
		refs = append(refs, nodePtr(n.Init, "init")...)
		refs = append(refs, nodePtr(n.Cond, "cond")...)
		refs = append(refs, nodePtr(n.Body, "body")...)
	case "at":
		refs = nodePtr(n.Of, "of")
		refs = append(refs, nodePtr(n.I, "i")...)
	case "len":
		refs = nodePtr(n.E, "e")
	case "store_op":
		// args 里存的是节点 id；不遍历它们，store_op 的引用就不会被 V-2 校验。
		if len(n.Args) > 0 {
			var args map[string]any
			if err := json.Unmarshal(n.Args, &args); err == nil {
				keys := make([]string, 0, len(args))
				for k := range args {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					if f, ok := args[k].(float64); ok && f >= 0 {
						refs = append(refs, Ref{Table: TableNode, WF: wf, Index: int(f),
							From: pathf("%s.args.%s", p, k), Loc: n.Loc})
					}
				}
			}
		}
	case "exprstmt":
		refs = nodePtr(n.E, "e")
	case "seq":
		for i, k := range n.Kids {
			refs = append(refs, Ref{Table: TableNode, WF: wf, Index: k, From: pathf("%s.kids[%d]", p, i), Loc: n.Loc})
		}
	case "decl":
		// **AllowInput=true**：decl 绑定到 input 是 -1（不改 vars[]）。
		if n.Var != nil {
			refs = varRef(*n.Var, "var", true)
		}
		// A 形态：`auto [a, b] = …`。位置即元组下标（LANGUAGE §5.2 / E-7）。
		for i, bp := range n.Vars {
			refs = varPtrRef(refs, bp, wf, n.Loc, pathf("%s.vars[%d]", p, i))
		}
		refs = nodePtr(n.E, "e")
	case "destructure":
		// B 形态：独立的解构节点。null 元素是 `_` 丢弃位，不是引用。
		for i, bp := range n.Vars {
			refs = varPtrRef(refs, bp, wf, n.Loc, pathf("%s.vars[%d]", p, i))
		}
		refs = nodePtr(n.E, "e")
	case "if":
		refs = nodePtr(n.C, "c")
		refs = nodePtr(n.Then, "then")
		for i := range n.Elifs {
			refs = node(n.Elifs[i].C, pathf("elifs[%d].c", i))
			refs = node(n.Elifs[i].Body, pathf("elifs[%d].body", i))
		}
		refs = nodePtr(n.Else, "else")
	case "match":
		refs = nodePtr(n.Subj, "subj")
		for i := range n.Arms {
			refs = node(n.Arms[i].Body, pathf("arms[%d].body", i))
		}
		refs = nodePtr(n.Default, "default")
	case "parallel":
		for i, b := range n.Branches {
			refs = append(refs, Ref{Table: TableNode, WF: wf, Index: b, From: pathf("%s.branches[%d]", p, i), Loc: n.Loc})
		}
		// join 的元素是**表达式节点**（D-031）：不收集它们，悬空 join 不会被 V-2 检查。
		for i, j := range n.Join {
			refs = append(refs, Ref{Table: TableNode, WF: wf, Index: j, From: pathf("%s.join[%d]", p, i), Loc: n.Loc})
		}
		// C 形态：绑定直接挂在 parallel 上。
		for i, b := range n.Bind {
			refs = varPtrRef(refs, b, wf, n.Loc, pathf("%s.bind[%d]", p, i))
		}
	case "retry":
		refs = nodePtr(n.Body, "body")
	case "try":
		refs = nodePtr(n.Body, "body")
		for i := range n.Catches {
			// catches[].v 是**变量** id（catch 绑定异常到变量），不是节点 id。
			refs = varRef(n.Catches[i].V, pathf("catches[%d].v", i), false)
			refs = node(n.Catches[i].Body, pathf("catches[%d].body", i))
		}
	case "fallback":
		// main 是**表达式**、body 是**语句**，两个都必须计入引用，否则
		// 悬空 id 不会被 V-2 抓到 —— 那会一路走到运行期才炸。
		refs = nodePtr(n.Main, "main")
		refs = nodePtr(n.Body, "body")
	case "return":
		refs = nodePtr(n.E, "e")
	case "nop":
		// 无引用
	case "stop":
		refs = nodePtr(n.E, "e")
		refs = nodePtr(n.Data, "data")
	case "mem_txn":
		// checkpoint 没有实参；rollback/commit 的快照在 e。
		refs = nodePtr(n.E, "e")
	case "emit":
		// route=2 时 to 是目标上下文表达式。
		refs = nodePtr(n.Target, "to")
		// args 是位置实参的节点 id 数组（与 call_json 同形）。
		if len(n.Args) > 0 {
			var ids []int
			if json.Unmarshal(n.Args, &ids) == nil {
				for i, id := range ids {
					refs = node(id, pathf("args[%d]", i))
				}
			}
		}
	case "on":
		refs = nodePtr(n.Body, "body")
	case "sleep":
		// ms 是**表达式 id**（时长可以不是字面量），所以它是一条节点引用。
		refs = nodePtr(n.MS, "ms")
	case "until":
		refs = nodePtr(n.TimeoutMS, "timeout_ms")

	// ---- 表达式节点（11）----
	case "lit":
		// v 是字面量，不是引用
	case "var":
		// 这里是**引用变量表**；-1 = input（哨兵，不在 vars[] 里）
		if n.Var != nil {
			refs = varRef(*n.Var, "var", true)
		}
	case "field", "index":
		refs = nodePtr(n.Of, "of")
	case "binop":
		refs = nodePtr(n.L, "l")
		refs = nodePtr(n.R, "r")
	case "unop":
		refs = nodePtr(n.E, "e")
	case "interp":
		// parts[].path 是已解析的字段路径（[]string），不是 id 引用
	case "call_model":
		if n.Schema != nil {
			refs = append(refs, Ref{Table: TableSchema, WF: wf, Index: *n.Schema, From: p + ".schema", Loc: n.Loc})
		}
		var a ModelArgs
		if len(n.Args) > 0 && json.Unmarshal(n.Args, &a) == nil {
			refs = nodePtr(a.Prompt, "args.prompt")
		}
		if n.Att != nil {
			refs = node(n.Att.RetryNode, "attempt_ctx.retry_node")
		}
	case "call_tool":
		// tool 是**名字**引用
		if n.Tool != nil {
			refs = append(refs, Ref{Table: TableTool, WF: wf, Name: *n.Tool, From: p + ".tool", Loc: n.Loc})
		}
		var a ToolArgs
		if len(n.Args) > 0 && json.Unmarshal(n.Args, &a) == nil {
			refs = nodePtr(a.URL, "args.url")
			refs = nodePtr(a.Header, "args.header")
			refs = nodePtr(a.Body, "args.body")
			refs = nodePtr(a.IdemKey, "args.idem_key")
		}
		if n.Ret != nil {
			refs = appendTypeRefs(refs, wf, n.Ret, p+".ret", n.Loc)
		}
		if n.Att != nil {
			refs = node(n.Att.RetryNode, "attempt_ctx.retry_node")
		}
	case "call_fn":
		// fn 是**名字**引用（函数表是符号表），不是下标：codegen 只看得见
		// 一个模块，函数表的下标是 unit 级的产物。
		if n.Fn != nil {
			refs = append(refs, Ref{Table: TableFunction, WF: wf, Name: *n.Fn, From: p + ".fn", Loc: n.Loc})
		}
		// args 是位置参数数组，与 call_json 同形。
		var fnArgs []int
		if len(n.Args) > 0 && json.Unmarshal(n.Args, &fnArgs) == nil {
			for i, a := range fnArgs {
				refs = append(refs, Ref{Table: TableNode, WF: wf, Index: a,
					From: pathf("%s.args[%d]", p, i), Loc: n.Loc})
			}
		}
	case "call_native":
		// 反向融合 / extern / #link native：调用一个 cgo 链接进来的 C 符号。
		// fn 是符号名（按名字索引，归到 TableNative）。真正的符号是否真实
		// 存在于归档，要等链接期 / S2 的 cgo bridge 才验证；加载期只归一化
		// 引用，不报悬空（见 validate.go 的 resolveRefs 分支）。
		if n.Fn != nil {
			refs = append(refs, Ref{Table: TableNative, WF: wf, Name: *n.Fn, From: p + ".fn", Loc: n.Loc})
		}
		// args 是位置参数数组，与 call_json / call_fn 同形。
		var nativeArgs []int
		if len(n.Args) > 0 && json.Unmarshal(n.Args, &nativeArgs) == nil {
			for i, a := range nativeArgs {
				refs = append(refs, Ref{Table: TableNode, WF: wf, Index: a,
					From: pathf("%s.args[%d]", p, i), Loc: n.Loc})
			}
		}
		// ret 是返回类型表达式（与 call_tool 同形）。
		if n.Ret != nil {
			refs = appendTypeRefs(refs, wf, n.Ret, p+".ret", n.Loc)
		}
		// arg_types 是位置形参的静态类型表；struct 形参的 id 引用同样要进
		// V-2 校验（与 ret 同一条理由）。漏掉它，一个指向不存在类型的
		// extern 形参会安静编过。
		for i := range n.ArgTypes {
			refs = appendTypeRefs(refs, wf, &n.ArgTypes[i],
				pathf("%s.arg_types[%d]", p, i), n.Loc)
		}
	case "call_json":
		// args 是**数组**（位置参数），不是对象 —— 与 call_model/call_tool 不同。
		var as []int
		if len(n.Args) > 0 && json.Unmarshal(n.Args, &as) == nil {
			for i, a := range as {
				refs = append(refs, Ref{Table: TableNode, WF: wf, Index: a, From: pathf("%s.args[%d]", p, i), Loc: n.Loc})
			}
		}
	case "cast":
		refs = nodePtr(n.E, "e")
		if n.To != nil {
			refs = appendTypeRefs(refs, wf, n.To, p+".to", n.Loc)
		}
	}
	return refs
}

// NativeCallRef 记录一个被实际调用的 native（C ABI）符号及其位置。
//
// 反向融合（extern / #link native / call_native）里，链接期必须用它与
// `requires[].kind=="native"` 对账：每个被调用的 native 符号都必须有 native 依赖
// 支撑，否则 E_LINK_UNRESOLVED。S1 只做"粗粒度"对账——只要 unit 里存在任一条
// native require 就视为全部 native 调用都可满足；精确的符号级头文件校验
// （确认符号确实在 from 头里声明）留给 S2 的 cgo 桥。
type NativeCallRef struct {
	Sym string
	Loc Loc
}

// NativeCallRefs 扫整份 IR（workflows + functions），返回所有 call_native 引用的
// 符号（按符号去重，保序）。同名符号多处调用只返回一次，位置取首次出现。
func NativeCallRefs(d *Doc) []NativeCallRef {
	seen := map[string]bool{}
	out := make([]NativeCallRef, 0)
	collect := func(nodes []Node) {
		for i := range nodes {
			n := &nodes[i]
			if n.K == "call_native" && n.Fn != nil {
				if seen[*n.Fn] {
					continue
				}
				seen[*n.Fn] = true
				out = append(out, NativeCallRef{Sym: *n.Fn, Loc: n.Loc})
			}
		}
	}
	for i := range d.Workflows {
		collect(d.Workflows[i].Nodes)
	}
	for i := range d.Functions {
		collect(d.Functions[i].Nodes)
	}
	return out
}

// nodeSuccessors 返回一个节点在**节点表**里的全部直接后继。
//
// 复用 appendNodeRefs 而不是另写一份字段 switch：那份是按 kind 归一化的
// 唯一事实源，V-2 悬空检查与引用总数断言都靠它。retry 幂等校验若自己再列
// 一遍 kids/body/args，新增 node kind 时必然漏一条边 —— 而漏一条边就等于
// 留一个「把未证明的 POST 藏进新 kind 里绕过重试检查」的口子。
//
// 排除 attempt_ctx.retry_node：它是编译期打的**反向注解**（指回包含这次
// 调用的 retry），不是执行时会走到的边。顺着它走会绕回 retry 节点本身，
// 在嵌套 retry 的 IR 里还会把别的 retry 子树误算进当前可达集。
func nodeSuccessors(n *Node) []int {
	refs := appendNodeRefs(nil, 0, n, "")
	out := make([]int, 0, len(refs))
	for i := range refs {
		if refs[i].Table != TableNode {
			continue // vars / tools / schemas 不是节点后继
		}
		if strings.HasSuffix(refs[i].From, "attempt_ctx.retry_node") {
			continue
		}
		out = append(out, refs[i].Index)
	}
	return out
}
