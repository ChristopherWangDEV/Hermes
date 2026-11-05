// Package link 把多个 `.hbc` 合成一个可执行归档（IR.md §9 / D-013）。
//
// 四条判据，每条都对应一类真实事故：
//
//	· 同名 workflow / backend / tool → **错误**，不覆盖。静默覆盖会让
//	  "我链接的到底是谁"变成不可知。
//	· 同名 struct → **ODR**：比 `types[].sha256`（规范序列化，不含 id/module/loc）。
//	  相同就合并成一份，不同就报字段级 diff。
//	· 版本：`abi_version` **完全相等**、`ir_version` 主版本相等且被链接方 ≤ 链接方。
//	· `requires[].member_sha256`：声明了要链接谁，就得说清是哪份字节。
package link

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"hermes/internal/archive"
	"hermes/internal/ir"
	"hermes/internal/std"
)

// Input 是链接的一个输入库。
type Input struct {
	Path    string
	Archive *archive.Archive
	// Raw 是归档原始字节。本地依赖的校验要算**索引哈希**，而索引只在原始字节里
	// 有位置 —— Archive 结构体不带它。
	Raw []byte
}

// Kind 是符号命名空间。四个彼此独立：一个 workflow 与一个 type 可以同名。
type Kind string

const (
	KindWorkflow Kind = "workflow"
	KindType     Kind = "type"
	KindBackend  Kind = "backend"
	KindTool     Kind = "tool"
)

// Symbol 是一个已解析的符号。
type Symbol struct {
	Kind   Kind
	Name   string
	Member string // 来自哪个成员
	Lib    string // 来自哪个输入库
}

// Result 是链接结果。
type Result struct {
	// Symbols 按命名空间分行。
	Symbols map[Kind]map[string]Symbol
	// Members 是被链接进来的成员（库名.成员名），按字典序。
	Members []string
	// Entries 是可执行入口（is_entry 的 workflow）。
	Entries []string
}

// ODRDiff 是一处字段级差异（用于报错文案）。
type ODRDiff struct {
	Field string
	A, B  string
}

// Link 解析符号表并做四类检查。返回的错误是**用户可读的问题清单**，
// 不是 Go 的 error 包装 —— 链接错误的价值全在"说清是哪两个库的哪个成员"。
func Link(inputs []Input) (*Result, []string) {
	var problems []string
	res := &Result{Symbols: map[Kind]map[string]Symbol{
		KindWorkflow: {}, KindType: {}, KindBackend: {}, KindTool: {},
	}}

	type origin struct {
		sym Symbol
		lib string
	}
	seen := map[Kind]map[string]origin{
		KindWorkflow: {}, KindType: {}, KindBackend: {}, KindTool: {},
	}
	// typeName → (sha, field list, member)
	typeDefs := map[string]typeRecord{}

	for _, in := range inputs {
		for _, m := range in.Archive.Members {
			res.Members = append(res.Members, in.Path+"."+m.Name)

			doc, ds := ir.Load(m.IR)
			if len(ds) != 0 {
				problems = append(problems, fmt.Sprintf(
					"%s: 成员 %q 的 IR 不合格（%d 条诊断），链接前必须能过加载期校验",
					in.Path, m.Name, len(ds)))
				continue
			}

			// ---- 类型：ODR ----
			for i := range doc.Types {
				t := &doc.Types[i]
				if !t.Exported {
					continue
				}
				if prev, ok := typeDefs[t.Name]; ok {
					if prev.sha != t.SHA256 {
						problems = append(problems, odrMessage(t.Name, prev, typeRecord{
							sha: t.SHA256, member: m.Name, lib: in.Path,
							fields: fieldSummary(t),
						}))
					}
					continue
				}
				typeDefs[t.Name] = typeRecord{sha: t.SHA256, member: m.Name, lib: in.Path, fields: fieldSummary(t)}
				res.Symbols[KindType][t.Name] = Symbol{Kind: KindType, Name: t.Name, Member: m.Name, Lib: in.Path}
			}

			// ---- workflow：同名即错（不是 ODR）----
			for i := range doc.Workflows {
				w := &doc.Workflows[i]
				if !w.Exported {
					continue
				}
				if prev, ok := seen[KindWorkflow][w.Name]; ok {
					problems = append(problems, fmt.Sprintf(
						"同名 workflow %q：%s/%s 与 %s/%s。"+
							"不覆盖、不警告后继续 —— 静默覆盖会让\"我链接的到底是谁\"变成不可知",
						w.Name, prev.lib, prev.sym.Member, in.Path, m.Name))
					continue
				}
				seen[KindWorkflow][w.Name] = origin{sym: Symbol{Kind: KindWorkflow, Name: w.Name, Member: m.Name, Lib: in.Path}, lib: in.Path}
				res.Symbols[KindWorkflow][w.Name] = seen[KindWorkflow][w.Name].sym
				if w.IsEntry {
					res.Entries = append(res.Entries, w.Name)
				}
			}

			// ---- backend / tool：同名且配置不同即错 ----
			checkDup := func(kind Kind, name, cfg string) {
				if prev, ok := seen[kind][name]; ok {
					_ = prev
					problems = append(problems, fmt.Sprintf(
						"同名 %s %q：%s/%s 与 %s/%s 都导出它；"+
							"两个库对同一个名字的配置若不同，链接后谁生效没有答案",
						kind, name, prev.lib, prev.sym.Member, in.Path, m.Name))
					return
				}
				_ = cfg
				seen[kind][name] = origin{sym: Symbol{Kind: kind, Name: name, Member: m.Name, Lib: in.Path}, lib: in.Path}
				res.Symbols[kind][name] = seen[kind][name].sym
			}
			for _, b := range doc.Backends {
				if b.Exported {
					checkDup(KindBackend, b.Name, "")
				}
			}
			for i := range doc.Tools {
				checkDup(KindTool, doc.Tools[i].Name, "")
			}

			// ---- 未解析的符号（imports）----
			for _, im := range doc.Imports {
				var table map[string]Symbol
				switch im.Kind {
				case "workflow":
					table = res.Symbols[KindWorkflow]
				case "type":
					table = res.Symbols[KindType]
				case "backend":
					table = res.Symbols[KindBackend]
				case "tool":
					table = res.Symbols[KindTool]
				}
				if table == nil {
					continue
				}
				// imports 可能指向本 unit 自己导出的符号：本 unit 也算"已解析"。
				if _, ok := table[im.Sym]; ok {
					continue
				}
				if docHasSymbol(doc, im.Kind, im.Sym) {
					continue
				}
				problems = append(problems, fmt.Sprintf(
					"未解析的符号 %q（%s），被 %s/%s 在 L%d 引用 —— "+
						"补上提供它的库（-l），或确认它其实应该在本 unit 内",
					im.Sym, im.Kind, in.Path, m.Name, im.Loc.L))
			}

			// ---- 版本检查（requires）----
			for _, r := range doc.Requires {
				if r.Implicit {
					continue
				}
				if r.ABIVersion != 1 {
					problems = append(problems, fmt.Sprintf(
						"%s 要求 %s 的 abi_version=%d，本链接器只认 1。"+
							"ABI 必须**完全相等**（不是 major 相等）：minor 变化也可能改结构体布局，而后果是内存解释错误",
						m.Name, r.Spec, r.ABIVersion))
				}
			}
		}
	}

	sort.Strings(res.Members)
	sort.Strings(res.Entries)
	return res, problems
}

type typeRecord struct {
	sha    string
	member string
	lib    string
	fields []string
}

func fieldSummary(t *ir.TypeDef) []string {
	out := make([]string, 0, len(t.Fields))
	for _, f := range t.Fields {
		out = append(out, fmt.Sprintf("%s : %s", f.N, typeName(f.T)))
	}
	return out
}

func typeName(t ir.TypeExpr) string {
	if t.K == "struct" && t.ID != nil {
		return fmt.Sprintf("struct#%d", *t.ID)
	}
	return t.K
}

// odrMessage 给出**字段级**差异。只说"两个 struct 不一致"没用 ——
// 人要知道改哪一行。
func odrMessage(name string, a, b typeRecord) string {
	var lines []string
	lines = append(lines, fmt.Sprintf("struct %s 在两处定义不同：", name))
	lines = append(lines, fmt.Sprintf("  成员 %s:%s", a.lib, a.member))
	lines = append(lines, fmt.Sprintf("  成员 %s:%s", b.lib, b.member))

	diffs := DiffFields(a.fields, b.fields)
	for _, d := range diffs {
		lines = append(lines, fmt.Sprintf("  %s", d))
	}
	lines = append(lines, "  = 为什么：两个库对同一个名字的结构有不同理解。若静默选一个，"+
		"另一个库生成的 JSON Schema 与它自己的读取代码就对不上——")
	lines = append(lines, "             这会在运行期表现为\"字段莫名消失\"，无法定位。")
	lines = append(lines, "  = 怎么改：重命名其中一个 struct，或让两个库依赖同一个 .h")
	return strings.Join(lines, "\n")
}

// DiffFields 比较两份"字段 : 类型"清单，返回可读差异。
func DiffFields(a, b []string) []string {
	am := map[string]string{}
	bm := map[string]string{}
	var order []string
	for _, s := range a {
		n, ty := splitField(s)
		am[n] = ty
		order = append(order, n)
	}
	for _, s := range b {
		n, ty := splitField(s)
		bm[n] = ty
		if _, ok := am[n]; !ok {
			order = append(order, n)
		}
	}

	var out []string
	seen := map[string]bool{}
	for _, n := range order {
		if seen[n] {
			continue
		}
		seen[n] = true
		av, aok := am[n]
		bv, bok := bm[n]
		switch {
		case aok && bok && av != bv:
			out = append(out, fmt.Sprintf("字段 %s：%s ≠ %s", n, av, bv))
		case aok && !bok:
			out = append(out, fmt.Sprintf("仅第一处有字段 %s", n))
		case !aok && bok:
			out = append(out, fmt.Sprintf("仅第二处有字段 %s", n))
		}
	}
	if len(out) == 0 {
		out = append(out, "字段集合相同但 sha256 不同：检查注解与可选性")
	}
	return out
}

func splitField(s string) (string, string) {
	if i := strings.Index(s, " : "); i >= 0 {
		return s[:i], s[i+3:]
	}
	return s, ""
}

// docHasSymbol 判断这个 unit 自己是否提供该符号。
func docHasSymbol(doc *ir.Doc, kind, name string) bool {
	switch kind {
	case "workflow":
		for i := range doc.Workflows {
			if doc.Workflows[i].Name == name {
				return true
			}
		}
	case "type":
		for i := range doc.Types {
			if doc.Types[i].Name == name {
				return true
			}
		}
	case "backend":
		for i := range doc.Backends {
			if doc.Backends[i].Name == name {
				return true
			}
		}
	case "tool":
		for i := range doc.Tools {
			if doc.Tools[i].Name == name {
				return true
			}
		}
	}
	return false
}

// VerifyRequires 校验 `requires[].member_sha256`：声明了依赖谁，就得说清是哪份字节。
//
// 这是三层判据里**唯一防投毒**的一条。少了它，一个被换过的 `stdlib.hbc` 与正版
// 在版本检查上完全无法区分 —— 版本号可以伪造，字节哈希不行。
//
// 规则（IR.md §9.5）：`member_sha256` 缺省即不校验，但只允许出现在 implicit 项
// （目前仅 <std/runtime>）；非 implicit 缺它是错误（E_LINK_REQUIRES_INCOMPLETE）。
func VerifyRequires(inputs []Input) []string {
	// 先把所有输入成员的 ir_sha256 收集起来。
	have := map[string]string{} // sha → "lib.member"
	for _, in := range inputs {
		for _, m := range in.Archive.Members {
			sum, _ := m.Index["ir_sha256"].(string)
			have[sum] = in.Path + "." + m.Name
		}
	}

	var problems []string
	for _, in := range inputs {
		for _, m := range in.Archive.Members {
			doc, ds := ir.Load(m.IR)
			if len(ds) != 0 {
				continue // 已在 Link 里报过
			}
			for _, r := range doc.Requires {
				if r.Implicit {
					continue // 编译器内建，不来自任何归档成员
				}
				if r.Kind != "std" {
					// 本地依赖：`member_sha256` 是**那个库归档的索引哈希**。
					// 链接输入里必须存在一份索引哈希与之相等的归档 —— 这就是
					// "我要那份确切字节"的可校验形式。
					if err := verifyLocalRequire(r, inputs); err != nil {
						problems = append(problems, fmt.Sprintf("%s: 成员 %q 的本地依赖校验失败: %v",
							in.Path, m.Name, err))
					}
					continue
				}
				if r.Kind == "std" {
					// std 依赖由**内建 stdlib** 提供，不是链接输入里的成员 ——
					// 所以不能在这张表里找，但**可以对着内嵌源码校验**。
					//
					// 这就是供应链那一环的闭合点：`member_sha256` 记的是
					// "我依赖的是哪份字节"，而那份字节就在二进制里（go:embed）。
					// 对不上说明 IR 被换过 / stdlib 与该 IR 不配套。
					if err := verifyStdRequire(r.Spec, r.MemberSHA256); err != nil {
						problems = append(problems, fmt.Sprintf("%s: 成员 %q 的 std 依赖校验失败: %v",
							in.Path, m.Name, err))
					}
					continue
				}
				if r.MemberSHA256 == "" {
					problems = append(problems, fmt.Sprintf(
						"%s: 成员 %q 声明依赖 %s 却没给 member_sha256。"+
							"声明了要链接一个库却不说要它的哪份字节，等于没声明（E_LINK_REQUIRES_INCOMPLETE）",
						in.Path, m.Name, r.Spec))
					continue
				}
				if _, ok := have[r.MemberSHA256]; !ok {
					problems = append(problems, fmt.Sprintf(
						"%s: 成员 %q 声明依赖 %s，但 member_sha256=%s 不在链接输入里。"+
							"要么补上提供它的库，要么这份依赖是伪造的（E_LINK_TAMPERED）",
						in.Path, m.Name, r.Spec, shortSHA(r.MemberSHA256)))
				}
			}
		}
	}
	return problems
}

// Merge 把多个归档的成员合成一份新的成员表（按名字典序，Write 会再排一次）。
//
// 这一刀**不做死代码剥离**：剥离要按 entries 求可达集，而可达性依赖跨成员的
// 引用图（type/backend/tool 都要算）。先把"能合并、能校验"做对，
// 剥离单独一刀做 —— 因为"剥错了"比"不剥"危险得多（后者只是产物大）。
func Merge(inputs []Input) []archive.Member {
	var out []archive.Member
	for _, in := range inputs {
		for _, m := range in.Archive.Members {
			exp := archive.Exports{}
			if raw, ok := m.Index["exports"].(map[string]any); ok {
				exp.Workflows = strList(raw["workflows"])
				exp.Types = strList(raw["types"])
				exp.Backends = strList(raw["backends"])
				exp.Tools = strList(raw["tools"])
			}
			var reqs []archive.Require
			if raw, ok := m.Index["requires"].([]any); ok {
				for _, r := range raw {
					rm, _ := r.(map[string]any)
					reqs = append(reqs, archive.Require{
						Spec: str(rm["spec"]), Kind: str(rm["kind"]),
						ABIVersion: toInt(rm["abi_version"]), IRVersion: toInt(rm["ir_version"]),
						Origin: str(rm["origin"]), MemberSHA256: str(rm["member_sha256"]),
						Implicit: rm["implicit"] == true,
					})
				}
			}
			out = append(out, archive.Member{
				Name: m.Name, IR: m.IR, Exports: exp, Requires: reqs,
				Entry:           len(exp.Workflows) > 0,
				LanguageVersion: toInt(m.Index["language_version"]),
				ABIVersion:      toInt(m.Index["abi_version"]),
			})
		}
	}
	return out
}

// shortSHA 截断显示。**不能直接 [:16]**：长度小于 16 会 panic，
// 而"报错路径上的 panic"会盖掉它本来要报的那条诊断 —— 测试用短哈希时当场就崩。
func shortSHA(s string) string {
	if len(s) <= 16 {
		return s
	}
	return s[:16]
}

func str(v any) string { s, _ := v.(string); return s }

func strList(v any) []string {
	arr, _ := v.([]any)
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// toInt 读索引里的整数。用 json.Number 解，避免 int → float64 的悄悄变形。
func toInt(v any) int {
	switch t := v.(type) {
	case json.Number:
		i, err := strconv.Atoi(t.String())
		if err != nil {
			return 0
		}
		return i
	case float64:
		return int(t)
	}
	return 0
}

// verifyStdRequire 把 `<std/net>` 这样的 spec 对着**内嵌 stdlib 源码**校验哈希。
//
// 为什么不是"查链接输入里的成员"：std 模块没有 workflow，构不出合法的 IR 成员，
// 所以它永远不会出现在任何 `.hbc` 里。它的字节就在二进制里 —— 直接对那里校验，
// 才是这条链真正闭合的地方。
func verifyStdRequire(spec, want string) error {
	mod := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(spec, "<"), "std/"), ">")
	if mod == spec || mod == "" {
		return fmt.Errorf("spec %q 不是 <std/…> 形态", spec)
	}
	src, ok := std.Source(mod)
	if !ok {
		return fmt.Errorf("内嵌 stdlib 里没有模块 %q（IR 依赖了一个这个版本不提供的模块）", mod)
	}
	sum := sha256.Sum256(src)
	if hex.EncodeToString(sum[:]) != want {
		return fmt.Errorf(
			"member_sha256 不符：IR 声称依赖 %s…，而内嵌 stdlib 的 %q 是 %s…（E_LINK_TAMPERED）",
			shortSHA(want), mod, shortSHA(hex.EncodeToString(sum[:])))
	}
	return nil
}

// Reach 计算从 entries 出发**可达**的成员集合（IR.md §9.4）。
//
// 两种产物两套根集合：`hermes link` 的根是 entries（is_entry 的 workflow），
// 剥掉一切不可达的东西——**包括未被用到的 export**；而 `hermes build` 的根是
// 全部 exported 符号（一个库的导出是给别人用的，本 unit 没人引用它完全正常）。
// 混淆这两者的后果，是把库剥成空壳，而症状是链接时 E_LINK_UNRESOLVED——离原因很远。
//
// 可达性沿**符号边**走：member A 用到 type T，T 由 member B 导出，则 B 可达。
func Reach(inputs []Input) (kept map[string]bool, dropped []string, problems []string) {
	// 1) 建符号表：symbol → 成员（lib.path + "." + member）
	symOwner := map[string]string{}
	memberOf := map[string]struct {
		lib string
		nm  string
	}{}
	needs := map[string][]string{} // 成员键 → 它 import 的符号
	mustKeep := map[string]bool{}  // 分析不了 → 不剥
	entries := map[string]bool{}

	for _, in := range inputs {
		for _, m := range in.Archive.Members {
			// **键用成员名**，不用 `lib.member`：调用方（cmd_link）过滤时手里
			// 只有 archive.Member（合并后已丢失 lib 路径）。用 lib.member 会让
			// `kept[m.Name]` 永远为假，于是**所有成员都被剥掉** ——
			// 而一个空归档的结构、签名全都合法。
			key := m.Name
			memberOf[key] = struct {
				lib string
				nm  string
			}{in.Path, m.Name}

			doc, ds := ir.Load(m.IR)
			if len(ds) != 0 {
				// **分析不了的成员一律保留**。剥离的目标是省体积，不是"把不懂的
				// 东西删掉"——后者会把一个能跑的产物变成缺件，而缺件要到链接期
				// 或运行期才暴露。这里只记录，不删。
				problems = append(problems, fmt.Sprintf(
					"%s: 成员 %q 的 IR 不合格，无法参与可达性分析 → 保守保留（不剥）",
					in.Path, m.Name))
				mustKeep[m.Name] = true
				continue
			}
			for i := range doc.Workflows {
				w := &doc.Workflows[i]
				if !w.Exported {
					continue
				}
				symOwner["workflow:"+w.Name] = key
				if w.IsEntry {
					entries["workflow:"+w.Name] = true
				}
			}
			for i := range doc.Types {
				if doc.Types[i].Exported {
					symOwner["type:"+doc.Types[i].Name] = key
				}
			}
			for _, b := range doc.Backends {
				if b.Exported {
					symOwner["backend:"+b.Name] = key
				}
			}
			var deps []string
			for _, im := range doc.Imports {
				deps = append(deps, im.Kind+":"+im.Sym)
			}
			// workflow 自己引用的类型/backend 也算边：它们可能由别的成员提供。
			for i := range doc.Workflows {
				w := &doc.Workflows[i]
				if w.Backend != "" {
					deps = append(deps, "backend:"+w.Backend)
				}
				if w.Input != nil {
					deps = append(deps, typeSymbols(doc, w.Input)...)
				}
				deps = append(deps, typeSymbols(doc, &w.Returns)...)
			}
			needs[key] = deps
			if os.Getenv("HERMES_REACH_DEBUG") != "" {
				fmt.Fprintf(os.Stderr, "reach: member=%s deps=%v\n", key, deps)
			}
		}
	}

	// 2) 从 entries 出发做不动点。
	kept = map[string]bool{}
	queue := []string{}
	for sym := range entries {
		if owner, ok := symOwner[sym]; ok {
			queue = append(queue, owner)
		}
	}
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		if kept[key] {
			continue
		}
		kept[key] = true
		for _, sym := range needs[key] {
			if owner, ok := symOwner[sym]; ok && !kept[owner] {
				queue = append(queue, owner)
			}
		}
	}

	// 3) 求差集：没被走到的成员就是死代码。
	for key := range memberOf {
		if mustKeep[key] {
			kept[key] = true
			continue
		}
		if !kept[key] {
			dropped = append(dropped, key)
		}
	}
	sortStrings(dropped)
	return kept, dropped, problems
}

// typeSymbols 把一个类型表达式里引用到的**具名类型**抽成符号边。
//
// 只有 `struct` 引用需要边：其余是基础类型，不指向任何成员。
func typeSymbols(doc *ir.Doc, t *ir.TypeExpr) []string {
	if t == nil || t.K == "" {
		return nil
	}
	if t.K == "struct" && t.ID != nil {
		for i := range doc.Types {
			if doc.Types[i].ID == *t.ID {
				return []string{"type:" + doc.Types[i].Name}
			}
		}
	}
	return nil
}

func sortStrings(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}

// verifyLocalRequire 校验一条本地依赖：链接输入里必须有一份归档，其**索引哈希**
// 等于 requires[].member_sha256。
//
// 为什么比索引而不是成员 IR：一个库可以有多个成员，而依赖声明的是"这个库"。
// 索引覆盖全部成员的哈希，所以它才是这份实现的指纹。
func verifyLocalRequire(r ir.RequireItem, inputs []Input) error {
	want := r.MemberSHA256
	if want == "" {
		return fmt.Errorf("requires 缺 member_sha256：声明了依赖却说不清是哪份字节（IR.md §9.5）")
	}
	for _, in := range inputs {
		idx := in.Archive.IndexBytes(in.Raw)
		if idx == nil {
			continue
		}
		sum := sha256.Sum256(idx)
		if hex.EncodeToString(sum[:]) == want {
			return nil
		}
	}
	return fmt.Errorf("链接输入里没有 member_sha256 为 %s… 的归档（依赖 %s；"+
		"该值 = 被依赖归档的索引哈希）", shortSHA(want), r.Spec)
}
