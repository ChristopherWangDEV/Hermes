package ir

import (
	"hermes/internal/diag"
)

// Validator 累积诊断，**不抛不返回首个错误**。
//
// 为什么不 fail-fast：一次加载要能给全貌（"3 处悬空 + 1 处环"比"第一处悬空"
// 有用得多），而且诊断是**数据不是控制流** —— 它不能被某一层 catch 住改写。
// 这是刻意避开 LangChain 那类"错误被框架层重组"的做法。
type Validator struct {
	doc  *Doc
	idx  *index
	ds   []diag.Diag
	lang diag.Lang

	// maxDiags 防止恶意 IR 把诊断内存撑爆（C++ 前端也踩过同类问题：
	// `.h` 里一个带体 workflow 曾让诊断 JSON 超过 64 MiB）。
	maxDiags int
	dropped  int
}

// Option 调整校验行为。
type Option func(*Validator)

// WithLang 切换诊断语言（默认英文）。
func WithLang(l diag.Lang) Option {
	return func(v *Validator) { v.lang = l }
}

// WithMaxDiags 设置诊断上限（默认 100）。超出的被丢弃并汇总为一条警告。
func WithMaxDiags(n int) Option {
	return func(v *Validator) {
		if n > 0 {
			v.maxDiags = n
		}
	}
}

// Validate 跑完整三阶段校验，返回全部诊断（可能为空 = 合法）。
//
// 只接受**已经反序列化**的 Doc。若手上是字节，用 Load —— 它会先做 V-7
// （浮点扫描）再调这里，而浮点恰恰是 json.Unmarshal 看不出来的。
func Validate(d *Doc, opts ...Option) []diag.Diag {
	v := newValidator(opts)
	v.run(d)
	return v.finish()
}

func newValidator(opts []Option) *Validator {
	v := &Validator{lang: diag.LangFromEnv(), maxDiags: 100}
	for _, o := range opts {
		o(v)
	}
	return v
}

// run 是三阶段的主体。抽出来是为了让 Load 能在**同一个** Validator 上
// 先追加"阶段 0"（浮点扫描）的诊断，再接着跑 1–3，共用同一个上限计数。
func (v *Validator) run(d *Doc) {
	v.doc = d
	if d == nil {
		return
	}
	// 阶段顺序**不可交换**，理由见各阶段注释。
	//
	// 版本闸排在最前：**不做向前兼容**（2.X 只兼容 ≥2.0.0 的 IR，2.0.0 是 LTS）。
	// 版本不对就**不再往下走** —— 后续三阶段都在按 2.0 的字段语义解释这份产物，
	// 拿一份 1.x IR 跑出来的十条悬空诊断只会盖住唯一真正的原因。
	if !v.checkVersions() {
		return
	}
	v.stageShape()
	v.stageGraph()
	v.stageDerived()
}

// checkVersions 是版本闸：ir_version 与 language_version 都必须等于
// IRVersionSupported（2）。返回 false 表示已经报了错，调用方应停止后续阶段。
//
// 为什么连 language_version 一起卡：它描述「这份源码讲哪一版语言」，与
// ir_version 是两条独立的轴（ARCH §11 六轴），但**拒绝的门槛一致** ——
// 1.x 的源码语义与 2.0 不同（例如 `fallback` 在 1.x 里是合法标识符，
// 在 2.0 里是保留字），按 2.0 去解释它同样会给出"看起来对的错答案"。
func (v *Validator) checkVersions() bool {
	ok := true
	if got := v.doc.IRVersion; got != IRVersionSupported {
		v.add(diag.CodeIRVersion, diag.SeverityError,
			diag.Loc{Module: v.doc.Unit.Name},
			map[string]string{
				"field": "ir_version",
				"got":   itoa(got),
				"want":  itoa(IRVersionSupported),
			})
		ok = false
	}
	if got := v.doc.Unit.LanguageVersion; got != IRVersionSupported {
		v.add(diag.CodeIRVersion, diag.SeverityError,
			diag.Loc{Module: v.doc.Unit.Name},
			map[string]string{
				"field": "language_version",
				"got":   itoa(got),
				"want":  itoa(IRVersionSupported),
			})
		ok = false
	}
	return ok
}

func (v *Validator) add(code string, sev diag.Severity, l diag.Loc, args map[string]string) {
	if len(v.ds) >= v.maxDiags {
		v.dropped++
		return
	}
	v.ds = append(v.ds, diag.New(code, sev, l, v.lang, args))
}

func (v *Validator) finish() []diag.Diag {
	if v.dropped > 0 {
		// 汇总成一条，不是静默丢弃。"被吞掉 N 条"本身就是信息。
		v.ds = append(v.ds, diag.New(
			diag.CodePanelTruncated, diag.SeverityWarning, diag.Loc{Module: v.doc.Unit.Name},
			v.lang,
			map[string]string{"shown": itoa(len(v.ds)), "dropped": itoa(v.dropped)},
		))
	}
	return v.ds
}

// dloc 把 IR 的 Loc 翻成诊断位置。
//
// **必须容忍 l.M 越界**：V-6 在跑之前，任何一条 loc 都可能指向不存在的模块，
// 而这个函数会被所有诊断调用。越过界就是"报错时崩"，那比原错误更难查。
func (v *Validator) dloc(l Loc, path string) diag.Loc {
	out := diag.Loc{Path: path, Line: l.L, Col: l.C, EndLine: l.EL, EndCol: l.EC}
	if l.M >= 0 && l.M < len(v.doc.Unit.Modules) {
		out.Module = v.doc.Unit.Modules[l.M].Path
	} else if len(v.doc.Unit.Modules) > 0 {
		out.Module = v.doc.Unit.Modules[0].Path
	}
	return out
}

// ---------------------------------------------------------------------------
// 阶段 1：形状与位置（V-6 + 规模）
//
// **必须最先**：后面每一步报错都要用 loc 渲染，loc 越界会让渲染器自己崩。
// 这是"越界连坐"——校验器崩在错误路径上，把原始错误盖掉。
// ---------------------------------------------------------------------------
func (v *Validator) stageShape() {
	nmod := len(v.doc.Unit.Modules)
	check := func(l Loc, what, path string) {
		if nmod == 0 {
			v.add(diag.CodeBadLoc, diag.SeverityError, diag.Loc{Module: v.doc.Unit.Name, Path: path},
				map[string]string{"what": what, "m": itoa(l.M), "count": "0", "last": "none"})
			return
		}
		if l.M < 0 || l.M >= nmod {
			v.add(diag.CodeBadLoc, diag.SeverityError, diag.Loc{Module: v.doc.Unit.Name, Path: path},
				map[string]string{
					"what": what, "m": itoa(l.M),
					"count": itoa(nmod), "last": itoa(nmod - 1),
				})
		}
	}

	for ti := range v.doc.Types {
		check(v.doc.Types[ti].Loc, pathf("types[%d]", ti), pathf("types[%d].loc", ti))
	}
	for wi := range v.doc.Workflows {
		w := &v.doc.Workflows[wi]
		check(w.Loc, pathf("workflows[%d]", wi), pathf("workflows[%d].loc", wi))
		for ni := range w.Nodes {
			check(w.Nodes[ni].Loc, pathf("workflows[%d].nodes[%d]", wi, ni),
				pathf("workflows[%d].nodes[%d].loc", wi, ni))
		}
	}
}

// index 是阶段 2 建的关系索引。建一次，查所有引用。
type index struct {
	toolByName     map[string]int
	backendByName  map[string]int
	functionByName map[string]int
}

// ---------------------------------------------------------------------------
// 阶段 2：图不变量（V-1, V-2, V-3, V-4, V-5, V-9, V-10, V-11, V-12）
//
// 依赖阶段 1 已保证 loc 可安全渲染。
// ---------------------------------------------------------------------------
func (v *Validator) stageGraph() {
	v.buildIndex()
	v.checkFunctionCalls()
	for wi := range v.doc.Workflows {
		v.checkWorkflowIDs(wi)
	}
	v.resolveRefs(v.collectAllRefs())
	v.checkExportsUniqueness()
	for wi := range v.doc.Workflows {
		v.checkRoot(wi)
		v.checkVarBinding(wi)
		v.checkAcyclic(wi)
		v.checkRetryIdempotency(wi)
		for _, d := range checkMutAssign(&v.doc.Workflows[wi]) {
			v.ds = append(v.ds, d)
		}
	}
	v.checkExportBidirectional()
	v.checkEntry()
}

func (v *Validator) buildIndex() {
	v.idx = &index{
		toolByName:     make(map[string]int, len(v.doc.Tools)),
		backendByName:  make(map[string]int, len(v.doc.Backends)),
		functionByName: make(map[string]int, len(v.doc.Functions)),
	}
	for i := range v.doc.Tools {
		v.idx.toolByName[v.doc.Tools[i].Name] = i
	}
	for i := range v.doc.Backends {
		v.idx.backendByName[v.doc.Backends[i].Name] = i
	}
	for i := range v.doc.Functions {
		v.idx.functionByName[v.doc.Functions[i].Name] = i
	}
}

// collectAllRefs 汇总全部引用。抽出来的目的是让"引用条数"可断言（测试里用）。
func (v *Validator) collectAllRefs() []Ref { return collectRefs(v.doc) }

// checkFunctionCalls 守"非递归函数必须在编译期展开"。
//
// call_fn 只允许指向 recursive=true 的函数。这不是优化，是语言定义：
// 非递归函数的逻辑与资源边界都在编译期静态化，运行期不存在动态派发。
// 旧版编译器、手改 IR、或 codegen bug 都可能留下这种节点 —— 在加载期拒绝，
// 比运行期"照章派发"要诚实得多。
func (v *Validator) checkFunctionCalls() {
	fn := make(map[string]*Function, len(v.doc.Functions))
	for i := range v.doc.Functions {
		fn[v.doc.Functions[i].Name] = &v.doc.Functions[i]
	}
	for wi := range v.doc.Workflows {
		nodes := v.doc.Workflows[wi].Nodes
		for ni := range nodes {
			n := &nodes[ni]
			if n.K != "call_fn" || n.Fn == nil {
				continue
			}
			f, ok := fn[*n.Fn]
			if !ok {
				continue // workflow 里的悬空 fn 由 V-2（refs）报
			}
			if f.Recursive {
				continue
			}
			where := pathf("workflows[%d].nodes[%d]", wi, ni)
			v.add(diag.CodeDynamicCall, diag.SeverityError, v.dloc(n.Loc, where+".fn"),
				map[string]string{"fn": *n.Fn, "where": where})
		}
	}
	for fi := range v.doc.Functions {
		nodes := v.doc.Functions[fi].Nodes
		for ni := range nodes {
			n := &nodes[ni]
			if n.K != "call_fn" || n.Fn == nil {
				continue
			}
			f, ok := fn[*n.Fn]
			where := pathf("functions[%d].nodes[%d]", fi, ni)
			if !ok {
				// 函数体里的引用没有进 collectRefs（它只遍历 workflows），
				// 这里补一条悬空诊断，否则"函数表内部指错"完全看不见。
				v.add(diag.CodeDanglingRef, diag.SeverityError, v.dloc(n.Loc, where+".fn"),
					map[string]string{
						"what": where + ".fn", "id": *n.Fn, "table": "functions",
						"range": rangeStr(len(v.doc.Functions)), "path": where + ".fn",
					})
				continue
			}
			if f.Recursive {
				continue
			}
			v.add(diag.CodeDynamicCall, diag.SeverityError, v.dloc(n.Loc, where+".fn"),
				map[string]string{"fn": *n.Fn, "where": where})
		}
	}
}

// checkWorkflowIDs 是 V-1。
//
// IR.md 规定 nodes 是"扁平数组，id == 下标"，所以**唯一性与范围都归结为一个等式**：
// nodes[i].id == i。这比"用 map 查重"更强也更快，而且报错更精确（能直接说
// "第 i 个节点的 id 是 7，应该是 i"）。
func (v *Validator) checkWorkflowIDs(wi int) {
	w := &v.doc.Workflows[wi]
	seen := make(map[int]int, len(w.Nodes))
	for ni := range w.Nodes {
		n := &w.Nodes[ni]
		if n.ID != ni {
			v.add(diag.CodeDupID, diag.SeverityError,
				v.dloc(n.Loc, pathf("workflows[%d].nodes[%d].id", wi, ni)),
				map[string]string{
					"id": itoa(n.ID), "wf": w.Name,
					"first": pathf("nodes[%d]", ni), "second": pathf("nodes[%d]", ni),
					"last": itoa(len(w.Nodes) - 1),
				})
		}
		if prev, dup := seen[n.ID]; dup {
			v.add(diag.CodeDupID, diag.SeverityError,
				v.dloc(n.Loc, pathf("workflows[%d].nodes[%d].id", wi, ni)),
				map[string]string{
					"id": itoa(n.ID), "wf": w.Name,
					"first": pathf("nodes[%d]", prev), "second": pathf("nodes[%d]", ni),
					"last": itoa(len(w.Nodes) - 1),
				})
		} else {
			seen[n.ID] = ni
		}
	}
	// vars[].id 也要求唯一（IR.md §3.1：unit 内唯一）。
	vseen := make(map[int]int, len(w.Vars))
	for vi := range w.Vars {
		id := w.Vars[vi].ID
		if prev, dup := vseen[id]; dup {
			v.add(diag.CodeDupID, diag.SeverityError,
				v.dloc(w.Loc, pathf("workflows[%d].vars[%d]", wi, vi)),
				map[string]string{
					"id": itoa(id), "wf": w.Name,
					"first": pathf("vars[%d]", prev), "second": pathf("vars[%d]", vi),
					"last": itoa(len(w.Vars) - 1),
				})
		} else {
			vseen[id] = vi
		}
	}
}

// resolveRefs 是 V-2：**所有**引用只经过这一份解析逻辑。
func (v *Validator) resolveRefs(refs []Ref) {
	for _, r := range refs {
		if r.Table.IsNamespace() {
			var ok bool
			switch r.Table {
			case TableTool:
				_, ok = v.idx.toolByName[r.Name]
			case TableBackend:
				_, ok = v.idx.backendByName[r.Name]
			case TableFunction:
				_, ok = v.idx.functionByName[r.Name]
			case TableNative:
				// 反向融合（extern / #link native / call_native）：fn 指向一个
				// 由 #link native 归档提供的 C 符号。符号是否真实存在，要等
				// 链接期（internal/link）或 S2 的 cgo bridge 验证；而 requires[]
				// 当前不携带"头里声明了哪些符号"的清单，加载期无从核对。
				// 所以这里**从宽**：把它视为可解析，绝不报悬空 —— 否则一个
				// 还没接 S2 的合法 unit 会在加载期被判死。真正的门在链接期。
				ok = true
			}
			if !ok {
				v.add(diag.CodeDanglingRef, diag.SeverityError, v.dloc(r.Loc, r.From),
					danglingArgs(r, v.namespaceSize(r.Table), r.Name))
			}
			continue
		}

		// types[] 的引用按 **id** 解析，不按下标：IR.md §3.1 规定类型 id 从 1 递增
		// （"按声明顺序分配"），而 id 与下标因此相差 1。这里若不特判，
		// 只有 1 个类型的 unit 会把 `{"k":"struct","id":1}` 判成越界 —— 而它是错的
		// 判定：id=1 正是那唯一的类型。
		if r.Table == TableType {
			if !v.typeIDExists(r.Index) {
				v.add(diag.CodeDanglingRef, diag.SeverityError, v.dloc(r.Loc, r.From),
					danglingArgs(r, len(v.doc.Types), itoa(r.Index)))
			}
			continue
		}

		size := v.tableSize(r)
		// -1 是 input 哨兵，只在允许的位置放行（Node.Var）。
		if r.AllowInput && r.Index == -1 {
			continue
		}
		if r.Index < 0 || r.Index >= size {
			v.add(diag.CodeDanglingRef, diag.SeverityError, v.dloc(r.Loc, r.From),
				danglingArgs(r, size, itoa(r.Index)))
		}
	}
}

// danglingArgs 拼装悬空引用的文案参数。
//
// 这里刻意把"表名"和"范围"都拼成具体字符串再交给 catalog：
// 文案模板不该知道"这次查的是 nodes 还是 vars"，否则每加一张表就要改模板，
// 而且会出现 "vars ... has 0 node(s)" 这种单位错位的输出。
func danglingArgs(r Ref, size int, id string) map[string]string {
	return map[string]string{
		"what":  r.From,
		"id":    id,
		"table": r.Table.String(),
		"range": rangeStr(size),
		"path":  r.From,
	}
}

// rangeStr 把表大小说成人话。空表**不能**写成 "0..-1" —— 那是在用数字
// 表达"什么都没有"，读者要先做一次心算才明白表是空的。
func rangeStr(n int) string {
	switch n {
	case 0:
		return "no entries at all"
	case 1:
		return "1 entry (0)"
	}
	return itoa(n) + " entries (0.." + itoa(n-1) + ")"
}

func (v *Validator) namespaceSize(t Table) int {
	switch t {
	case TableTool:
		return len(v.doc.Tools)
	case TableBackend:
		return len(v.doc.Backends)
	case TableFunction:
		return len(v.doc.Functions)
	}
	return 0
}

// tableSize 返回这条引用所指向的表有多大。
//
// **必须用 r.WF**：nodes 与 vars 是**每个 workflow 各自的**表。用全局大小
// （比如"所有 workflow 的节点总数"）会让一个只有 3 个节点的 workflow 放过
// 指向 id 50 的悬空引用 —— 因为另一个 workflow 恰好有 100 个节点。
func (v *Validator) tableSize(r Ref) int {
	switch r.Table {
	case TableNode:
		if r.WF >= 0 && r.WF < len(v.doc.Workflows) {
			return len(v.doc.Workflows[r.WF].Nodes)
		}
	case TableVar:
		if r.WF >= 0 && r.WF < len(v.doc.Workflows) {
			return len(v.doc.Workflows[r.WF].Vars)
		}
	case TableType:
		return len(v.doc.Types)
	case TableSchema:
		return len(v.doc.Schemas)
	}
	return 0
}

// typeIDExists 判断类型 id 是否存在（id 从 1 起，不连续也不能当错——
// 连续性由 codegen 保证，加载器只关心"指得到"）。
func (v *Validator) typeIDExists(id int) bool {
	for i := range v.doc.Types {
		if v.doc.Types[i].ID == id {
			return true
		}
	}
	return false
}

// checkExportsUniqueness 是 V-11：四个命名空间**各自**唯一。
func (v *Validator) checkExportsUniqueness() {
	type ns struct {
		name string
		list []string
	}
	for _, n := range []ns{
		{"types", v.doc.Exports.Types},
		{"workflows", v.doc.Exports.Workflows},
		{"backends", v.doc.Exports.Backends},
		{"tools", v.doc.Exports.Tools},
	} {
		seen := make(map[string]int, len(n.list))
		for i, s := range n.list {
			if prev, dup := seen[s]; dup {
				v.add(diag.CodeSymConflict, diag.SeverityError,
					diag.Loc{Module: v.doc.Unit.Name, Path: pathf("exports.%s[%d]", n.name, i)},
					map[string]string{
						"name": s, "count": "2", "namespace": n.name,
						"places": pathf("exports.%s[%d] and [%d]", n.name, prev, i),
					})
			} else {
				seen[s] = i
			}
		}
	}
}

// checkRoot 是 V-4。
func (v *Validator) checkRoot(wi int) {
	w := &v.doc.Workflows[wi]
	p := pathf("workflows[%d].root", wi)
	if w.Root < 0 || w.Root >= len(w.Nodes) {
		v.add(diag.CodeBadRoot, diag.SeverityError, v.dloc(w.Loc, p),
			map[string]string{"wf": w.Name, "root": itoa(w.Root), "reason": "no such node"})
		return
	}
	if k := w.Nodes[w.Root].K; k != "seq" {
		v.add(diag.CodeBadRoot, diag.SeverityError, v.dloc(w.Loc, p),
			map[string]string{"wf": w.Name, "root": itoa(w.Root), "reason": "node " + itoa(w.Root) + " has k=" + k + ", not \"seq\""})
	}
}

// boundVars 返回一个节点**绑定**的变量 id（不含 -1 的 input 绑定，也不含 null 丢弃位）。
//
// hasBinding 与 ids 必须分开：`auto [] = parallel {}`（k==0）有绑定位但一个名字都不绑，
// 用 len(ids)==0 判"没有绑定"会把它误判成坏 IR。
//
// 三种形态在这里被抹平（IR.md §4.2 的 A/B/C）：写方只出 B，读方三种都认。
// checkMutAssign 守"赋值只能改 可变（非 `const auto`）声明的变量"。
//
// 这条必须在**装配期**查：运行期再查，就意味着一个错误的程序能被编译通过，
// 而错误只有在跑到那一步时才暴露。
func checkMutAssign(w *Workflow) []diag.Diag {
	// 语义：**默认可变**，只有 const 不可变。
	mut := map[int]bool{}
	for i := range w.Nodes {
		n := &w.Nodes[i]
		if n.K == "decl" && n.Var != nil {
			mut[*n.Var] = n.Const == nil || !*n.Const
		}
	}
	var ds []diag.Diag
	for i := range w.Nodes {
		n := &w.Nodes[i]
		if n.K != "assign" || n.Var == nil {
			continue
		}
		if !mut[*n.Var] {
			ds = append(ds, diag.Diag{
				Severity: diag.SeverityError,
				Code:     "E_IR_ASSIGN_IMMUTABLE",
				Message:  "Cannot mutate const variable",
				Loc:      diag.Loc{Line: n.Loc.L, Col: n.Loc.C, EndCol: n.Loc.EC},
				Why:      "Immutable bindings are the basis for static inference: only variables declared `mut` may change at runtime.",
				Fix:      []string{"Remove `const`, or assign to a new variable instead."},
			})
		}
	}
	return ds
}

func boundVars(n *Node) (ids []int, hasBinding bool) {
	add := func(p *int) {
		if p != nil && *p >= 0 {
			ids = append(ids, *p)
		}
	}
	switch n.K {
	case "decl":
		hasBinding = n.Var != nil || n.Vars != nil
		add(n.Var)
		for _, p := range n.Vars {
			add(p)
		}
	case "for":
		// for 的循环变量由**它自己**绑定（D-040）：没有 decl 节点，也不需要。
		// 循环体里没有任何赋值节点能改它，所以"只声明一次"仍然成立。
		hasBinding = n.Var != nil
		add(n.Var)
	case "destructure":
		hasBinding = true
		for _, p := range n.Vars {
			add(p)
		}
	case "parallel":
		hasBinding = n.Bind != nil
		for _, p := range n.Bind {
			add(p)
		}
	case "try":
		// `catch (T e)` 也**绑定变量**。不认它，装配阶段回填 decl 时就找不到
		// 绑定节点，而报错说的是"codegen 与装配对不上"——误导性极强。
		for _, c := range n.Catches {
			if c.V >= 0 {
				ids = append(ids, c.V)
				hasBinding = true
			}
		}
	case "fallback":
		// **不绑定任何变量**：降级块里的 `auto x = …` 是它自己的 decl 节点，
		// 由上面 "decl" 分支收；fallback 本身只持有 main + body 两个子节点
		// （悬空检查在 refs.go 的 "fallback" 分支，不在这个函数里）。
		// 写出来是为了让下一个人看到"这里确实想过"，而不是漏了一个 case。
	}
	return ids, hasBinding
}

// checkVarBinding 是 V-5，**双向**。
//
// 只查一向是这类 bug 的典型形态：`var.decl` 指向的节点被删了，反向没查就发现不了。
func (v *Validator) checkVarBinding(wi int) {
	w := &v.doc.Workflows[wi]
	p := pathf("workflows[%d]", wi)

	// 正向：vars[i].decl 指向的节点必须**绑定**了这个变量
	for vi := range w.Vars {
		vr := &w.Vars[vi]
		if vr.Decl < 0 || vr.Decl >= len(w.Nodes) {
			continue // 已由 V-2 报悬空
		}
		n := &w.Nodes[vr.Decl]
		ids, hasBinding := boundVars(n)
		// `input` 是**调用方绑定**的：它没有 decl 节点，decl 指向 root 表示
		// "从根开始可见"。这是唯一允许的例外，且名字固定 —— 用户写的 auto
		// 不会叫 input 却没有绑定（那会在装配期就被 found 命中）。
		if !hasBinding && vr.Name == "input" && vr.Decl == w.Root {
			continue
		}
		if !hasBinding {
			v.add(diag.CodeVarMismatch, diag.SeverityError,
				v.dloc(w.Loc, pathf("%s.vars[%d].decl", p, vi)),
				map[string]string{
					"name": vr.Name, "var": itoa(vr.ID),
					"detail": "decl points at node " + itoa(vr.Decl) + " (k=" + n.K + "), which binds no variable",
				})
			continue
		}
		found := false
		for _, id := range ids {
			if id == vr.ID {
				found = true
				break
			}
		}
		if !found {
			v.add(diag.CodeVarMismatch, diag.SeverityError,
				v.dloc(w.Loc, pathf("%s.vars[%d].decl", p, vi)),
				map[string]string{
					"name": vr.Name, "var": itoa(vr.ID),
					"detail": "the binding node " + itoa(vr.Decl) + " binds vars " + intsStr(ids) + ", expected to include " + itoa(vr.ID),
				})
		}
	}

	// 反向：每个绑定节点里的变量必须在 vars[] 里，且 decl 回指这个节点；
	// 同一次绑定里重复绑同一个变量也是错（它会让"哪一份是它"没有答案）。
	for ni := range w.Nodes {
		n := &w.Nodes[ni]
		ids, _ := boundVars(n)
		if len(ids) == 0 {
			continue
		}
		seen := map[int]bool{}
		for _, vid := range ids {
			if seen[vid] {
				v.add(diag.CodeVarMismatch, diag.SeverityError,
					v.dloc(n.Loc, pathf("%s.nodes[%d]", p, ni)),
					map[string]string{
						"name": "", "var": itoa(vid),
						"detail": "node " + itoa(ni) + " binds var " + itoa(vid) + " more than once",
					})
				continue
			}
			seen[vid] = true
			if vid >= len(w.Vars) {
				continue // 已由 V-2 报悬空
			}
			found := -1
			for i := range w.Vars {
				if w.Vars[i].ID == vid {
					found = i
					break
				}
			}
			if found < 0 {
				continue
			}
			if w.Vars[found].Decl != ni {
				v.add(diag.CodeVarMismatch, diag.SeverityError,
					v.dloc(n.Loc, pathf("%s.nodes[%d]", p, ni)),
					map[string]string{
						"name": w.Vars[found].Name, "var": itoa(vid),
						"detail": "var " + itoa(vid) + ".decl points at node " + itoa(w.Vars[found].Decl) + ", but this binding node is " + itoa(ni),
					})
			}
		}
	}

	// 形态互斥：decl 不许同时写 var 与 vars（写方只出 B，但读方必须拒绝歧义形状）。
	for ni := range w.Nodes {
		n := &w.Nodes[ni]
		if n.K == "decl" && n.Var != nil && n.Vars != nil {
			v.add(diag.CodeVarMismatch, diag.SeverityError,
				v.dloc(n.Loc, pathf("%s.nodes[%d]", p, ni)),
				map[string]string{
					"name": "", "var": itoa(*n.Var),
					"detail": "decl node " + itoa(ni) + " has both var and vars; exactly one binding shape is allowed",
				})
		}
	}
}

// checkRetryIdempotency 是 E5007 的**产物侧**对应物。
//
// E5007 挂在源码 AST 的 check_idem.cpp 上，只保护 hermes ir 产出的 IR。
// 但 IR 是可分发、可签名、可手改的产物：hermes run 接受任意 IR 文件，
// 而 engine 的 retry 只重跑 body、完全不读 tools[].idem.mode。手工把一份
// POST 挪进 retry，或把 mode 改成 none，事故就只差一次 5xx。
//
// 判据必须落在**节点图**上：IR 里已经没有词法块，「retry 内」的准确定义
// 是「从 retry.body 沿引用边可达」。边由 nodeSuccessors 给出，与 V-2 同源。
//
// 边界：这不是防恶意篡改 —— 攻击者可以把 mode 直接填成 readonly。它防的是
// 手改失误、旧编译器产物、codegen 把调用搬进 retry 的 bug。与「手写汇编
// 能绕过编译器」同类，已登记在 KNOWN-GAPS.md。
func (v *Validator) checkRetryIdempotency(wi int) {
	w := &v.doc.Workflows[wi]
	// node id == 数组下标由 V-1 保证；但 V-1 报错后本阶段仍会跑完，
	// 直接 w.Nodes[id] 会越界崩。按 id 建表，坏 id 交给 V-2 报，不抢活。
	byID := make(map[int]*Node, len(w.Nodes))
	for i := range w.Nodes {
		byID[w.Nodes[i].ID] = &w.Nodes[i]
	}

	// 嵌套 retry 会让同一个 call_tool 被多个 retry 覆盖。一个坏调用只报
	// 一条：报两次不是更严格，是噪声。
	reported := make(map[int]bool)

	for ri := range w.Nodes {
		r := &w.Nodes[ri]
		if r.K != "retry" || r.Body == nil {
			continue
		}
		seen := make(map[int]bool)
		stack := []int{*r.Body}
		for len(stack) > 0 {
			id := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[id] {
				continue
			}
			seen[id] = true
			n := byID[id]
			if n == nil {
				continue // 悬空 id：V-2 的 E_IR_DANGLING_REF 负责
			}
			if n.K == "call_tool" && !reported[id] {
				v.checkToolIdemAtRetry(wi, r, n, reported)
			}
			for _, s := range nodeSuccessors(n) {
				if !seen[s] {
					stack = append(stack, s)
				}
			}
		}
	}
}

// checkToolIdemAtRetry 判定一次调用是否有幂等证明。
// 只认三个「已证明」值；空串、none、以及任何未知值一律按无证明处理
// （保守方向：认不出的标签不能被人拿来当证明）。
func (v *Validator) checkToolIdemAtRetry(wi int, r, n *Node, reported map[int]bool) {
	name := ""
	if n.Tool != nil {
		name = *n.Tool
	}
	ti, ok := v.idx.toolByName[name]
	if !ok {
		return // tool 不存在：V-2 报 E_IR_DANGLING_REF，这里不抢活
	}
	mode := v.doc.Tools[ti].Idem.Mode
	switch mode {
	case "readonly", "idem_true", "idem_key":
		return
	}
	if mode == "" {
		mode = "(missing)"
	}
	reported[n.ID] = true
	v.add(diag.CodeRetryUnproven, diag.SeverityError,
		v.dloc(n.Loc, pathf("workflows[%d].nodes[%d]", wi, n.ID)),
		map[string]string{
			"retry": itoa(r.ID),
			"call":  itoa(n.ID),
			"tool":  name,
			"mode":  mode,
		})
}

// successors 返回一个节点的直接后继（用于环检测）。只走**控制流边**，
// 不走表达式边（表达式不会回到控制流，但保守起来一起算也能检出环）。
func successors(n *Node) []int {
	var out []int
	add := func(p *int) {
		if p != nil {
			out = append(out, *p)
		}
	}
	out = append(out, n.Kids...)
	out = append(out, n.Branches...)
	add(n.E)
	add(n.C)
	add(n.Then)
	add(n.Else)
	add(n.Subj)
	add(n.Default)
	add(n.Body)
	for i := range n.Elifs {
		out = append(out, n.Elifs[i].C, n.Elifs[i].Body)
	}
	for i := range n.Arms {
		out = append(out, n.Arms[i].Body)
	}
	for i := range n.Catches {
		out = append(out, n.Catches[i].Body)
	}
	add(n.Of)
	add(n.L)
	add(n.R)
	return out
}

// checkAcyclic 是 V-3。**迭代 DFS + 三色标记**，不用递归 ——
// 深链会爆栈，而 Go 的栈扩容在深递归上表现为 OOM 而不是一个干净的诊断。
func (v *Validator) checkAcyclic(wi int) {
	w := &v.doc.Workflows[wi]
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make([]int8, len(w.Nodes))
	type frame struct {
		node int
		next int
	}
	for start := 0; start < len(w.Nodes); start++ {
		if color[start] != white {
			continue
		}
		stack := []frame{{node: start}}
		color[start] = gray
		for len(stack) > 0 {
			top := &stack[len(stack)-1]
			succ := successors(&w.Nodes[top.node])
			if top.next >= len(succ) {
				color[top.node] = black
				stack = stack[:len(stack)-1]
				continue
			}
			nxt := succ[top.next]
			top.next++
			if nxt < 0 || nxt >= len(w.Nodes) {
				continue // 已由 V-2 报悬空
			}
			switch color[nxt] {
			case white:
				color[nxt] = gray
				stack = append(stack, frame{node: nxt})
			case gray:
				// 找到回边 top.node → nxt，环为 nxt … top.node → nxt
				chain := ""
				started := false
				for _, f := range stack {
					if f.node == nxt {
						started = true
					}
					if started {
						if chain != "" {
							chain += " -> "
						}
						chain += itoa(f.node)
					}
				}
				chain += " -> " + itoa(nxt)
				v.add(diag.CodeCycle, diag.SeverityError,
					v.dloc(w.Nodes[top.node].Loc, pathf("workflows[%d].nodes[%d]", wi, top.node)),
					map[string]string{
						"wf": w.Name, "chain": chain,
						"edge": itoa(top.node) + " -> " + itoa(nxt),
					})
				return // 一个 workflow 报一条环就够，链条已给出全貌
			}
		}
	}
}

// checkExportBidirectional 是 V-9，**双向**。
func (v *Validator) checkExportBidirectional() {
	exported := make(map[string]bool, len(v.doc.Exports.Workflows))
	for _, s := range v.doc.Exports.Workflows {
		exported[s] = true
	}
	wfByName := make(map[string]*Workflow, len(v.doc.Workflows))
	for wi := range v.doc.Workflows {
		wfByName[v.doc.Workflows[wi].Name] = &v.doc.Workflows[wi]
	}

	for wi := range v.doc.Workflows {
		w := &v.doc.Workflows[wi]
		inTable := exported[w.Name]
		if w.Exported == inTable {
			continue
		}
		where, verb, want, action, target := "", "", "", "", ""
		if w.Exported {
			where, verb = "exports.workflows", "does not list"
			want, action, target = "false", "remove", "from exports.workflows"
		} else {
			where, verb = "exports.workflows", "already lists"
			want, action, target = "true", "add", "to exports.workflows"
		}
		v.add(diag.CodeExportMismatch, diag.SeverityError,
			v.dloc(w.Loc, pathf("workflows[%d].exported", wi)),
			map[string]string{
				"name": w.Name, "exported": boolStr(w.Exported),
				"where": where, "verb": verb, "want": want,
				"action": action, "target": target,
			})
	}

	// 反向：exports.workflows 里出现了 IR 里不存在的 workflow 名。
	for i, s := range v.doc.Exports.Workflows {
		if _, ok := wfByName[s]; !ok {
			v.add(diag.CodeExportMismatch, diag.SeverityError,
				diag.Loc{Module: v.doc.Unit.Name, Path: pathf("exports.workflows[%d]", i)},
				map[string]string{
					"name": s, "exported": "?",
					"where": "workflows[]", "verb": "has no entry for",
					"want": "n/a", "action": "remove", "target": "from exports.workflows",
				})
		}
	}
}

// checkEntry 是 V-10：至多一个 is_entry，且它必须 exported。
func (v *Validator) checkEntry() {
	entries := []int{}
	for wi := range v.doc.Workflows {
		if v.doc.Workflows[wi].IsEntry {
			entries = append(entries, wi)
		}
	}
	switch {
	case len(entries) == 0:
		// **零个 entry 是合法的**：一个纯库 unit（只有 workflow/type/backend 导出、
		// 没有入口）是静态库的正常形态。IR.md 的 V-10 写的是"**至多**一个
		// is_entry"，实现一度要求"至少一个"——那让所有库 .hbc 都无法存在，
		// 症状是"链接时找不到成员"，而原因是它压根没能生成。
		//
		// 运行期选 workflow 时仍然要求恰好一个（engine 的 pick()），
		// 但这是**执行**的要求，不是**加载**的要求。
	case len(entries) > 1:
		names := ""
		for i, wi := range entries {
			if i > 0 {
				names += ", "
			}
			names += v.doc.Workflows[wi].Name
		}
		v.add(diag.CodeEntry, diag.SeverityError,
			v.dloc(v.doc.Workflows[entries[1]].Loc, pathf("workflows[%d].is_entry", entries[1])),
			map[string]string{"reason": itoa(len(entries)) + " workflows are marked as entry: " + names, "name": names})
	}
	for _, wi := range entries {
		if !v.doc.Workflows[wi].Exported {
			v.add(diag.CodeEntry, diag.SeverityError,
				v.dloc(v.doc.Workflows[wi].Loc, pathf("workflows[%d].is_entry", wi)),
				map[string]string{
					"reason": "the entry workflow is not exported",
					"name":   v.doc.Workflows[wi].Name,
				})
		}
	}
}

// ---------------------------------------------------------------------------
// 阶段 3：派生字段复算
//
// **必须最后**：复算要遍历图，而无环已由阶段 2 保证（有环时遍历不终止）。
//
// 为什么这层是安全问题而不是洁癖：bounds 存在的意义就是"封住一次运行能做什么"。
// 一个被篡改的 max_spans 如果被信任，等于给了攻击者绕过执行上限的入口。
// ---------------------------------------------------------------------------
func (v *Validator) stageDerived() {
	for wi := range v.doc.Workflows {
		w := &v.doc.Workflows[wi]
		b := w.Bounds

		// max_steps 不可能超过节点数（每个节点最多执行一次，在无环前提下）。
		if b.MaxSteps > len(w.Nodes) {
			v.add(diag.CodeRecomputeDrift, diag.SeverityError,
				v.dloc(w.Loc, pathf("workflows[%d].bounds.max_steps", wi)),
				map[string]string{
					"field": "bounds.max_steps", "claimed": itoa(b.MaxSteps),
					"actual": itoa(len(w.Nodes)) + " (node count) or less",
				})
		}
		// **真复算**（T37 ③ / IR.md §5.1 的代数）。上面那条不等式只是 sanity：
		// 它拦得住 100000 这种离谱值，拦不住"差 1"——而差 1 就是错的：
		// 运行期断言 len(spans) ≤ max_spans 会因此判错。
		//
		// 三种结果必须分开（见 bounds.go 的 boundUnrecognized / boundUnknown）：
		//
		//	boundUnrecognized → **在这里报**。这是编译器 bug：IR 里有一个节点种类
		//	                    复算不认识，于是 max_spans 根本没有被验证过。
		//	                    以前这条落在 `got >= 0 && …` 的短路里被静默吞掉，
		//	                    而唯一会报它的 CheckBounds 只有测试在调 —— 所以
		//	                    它在一个真实产物上永远不会红。msg 必须点名"编译器
		//	                    bug"与两种可能来源，否则读者只会看到一条无解的 drift。
		//	boundUnknown      → 跳过（合法地算不出来：while 轮数、unlimited 的 agent）
		//	真实数字          → 只在真漂移时报
		got := RecomputeMaxSpans(w)
		switch {
		case got == boundUnrecognized:
			v.add(diag.CodeRecomputeDrift, diag.SeverityError,
				v.dloc(w.Loc, pathf("workflows[%d].bounds.max_spans", wi)),
				map[string]string{
					"field":   "bounds.max_spans",
					"claimed": itoa(b.MaxSpans),
					"actual": "nothing — the recompute met a node kind it does not recognize, " +
						"so this bound was never verified. That is a Hermes compiler bug: " +
						"the frontend emitted a `kind` that internal/ir/bounds.go does not handle " +
						"(the other possibility is a hand-edited or corrupt IR). " +
						"Loading stops rather than trusting `claimed`",
				})
		case got >= 0 && got != b.MaxSpans && b.MaxSpans <= 64*b.MaxSteps+1:
			// 只在不等式通过时另报一条：两个都报会让同一行出现两条诊断，
			// 而它们的修复动作是同一件事。
			v.add(diag.CodeRecomputeDrift, diag.SeverityError,
				v.dloc(w.Loc, pathf("workflows[%d].bounds.max_spans", wi)),
				map[string]string{
					"field": "bounds.max_spans", "claimed": itoa(b.MaxSpans),
					"actual": itoa(got) + " (recomputed from the node graph)",
				})
		}
		// 调用次数一侧同一个哨兵，同一个理由：RecomputeCalls 是第二个独立实现，
		// 它也有一个"不认识这个 kind"的出口。不查它的话，新增 kind 时
		// max_model_calls / max_tool_calls 就是"没被复算过"却照样放行。
		//
		// **这里只报哨兵，不做等式校验**：调用次数的复算与 C++ 的代数在某些
		// 形状上刻意不同（见 bounds.go 里 assign / try 的注释），把它升级成
		// 加载期硬校验是另一个决定，不该夹在"让哨兵响起来"里顺手做。
		if mc, tc := RecomputeCalls(w); mc == boundUnrecognized || tc == boundUnrecognized {
			field, claimed := "bounds.max_model_calls", itoa(b.MaxModelCalls)
			if mc != boundUnrecognized {
				field, claimed = "bounds.max_tool_calls", itoa(b.MaxToolCalls)
			}
			v.add(diag.CodeRecomputeDrift, diag.SeverityError,
				v.dloc(w.Loc, pathf("workflows[%d].%s", wi, field)),
				map[string]string{
					"field":   field,
					"claimed": claimed,
					"actual": "nothing — the call-count recompute met a node kind it does not recognize, " +
						"so this bound was never verified. That is a Hermes compiler bug: " +
						"the frontend emitted a `kind` that internal/ir/bounds.go does not handle " +
						"(the other possibility is a hand-edited or corrupt IR)",
				})
		}
		// IR.md §5.1 / ARCH §2.2 bounds.cpp：max_spans ≤ 64×max_steps + 1。
		if b.MaxSpans > 64*b.MaxSteps+1 {
			v.add(diag.CodeRecomputeDrift, diag.SeverityError,
				v.dloc(w.Loc, pathf("workflows[%d].bounds.max_spans", wi)),
				map[string]string{
					"field": "bounds.max_spans", "claimed": itoa(b.MaxSpans),
					"actual": "at most " + itoa(64*b.MaxSteps+1) + " (= 64*max_steps+1)",
				})
		}
	}
}

// ---- 小工具 ----

// intsStr 只用于诊断文案（"binds vars [1 2]"），不追求可解析。
func intsStr(xs []int) string {
	out := "["
	for i, x := range xs {
		if i > 0 {
			out += " "
		}
		out += itoa(x)
	}
	return out + "]"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
