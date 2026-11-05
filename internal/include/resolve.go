package include

import (
	"os"
	"path/filepath"
	"strings"

	"hermes/internal/diag"
	"hermes/internal/native"
	"hermes/internal/std"
)

// 上限取自 LANGUAGE.md §9.3。
const (
	MaxDepth   = 32
	MaxModules = 256
)

// DefaultStdModules 是内置 stdlib 的成员表（`#include <std/xxx.h>` 与
// `#link <std/xxx>` 只查它，**不触文件系统**）。
//
// 现在写死四个是权宜：真正的表由 T42 从 internal/std/stdlib/*.h 生成并
// go:embed 进二进制。写死的好处是"内置解析不走磁盘"这条性质现在就能测；
// 坏处是它可能与 T42 的产物漂移 —— 所以 Options.StdModules 允许注入，
// 测试里用注入值，生产路径等 T42 接入。
var DefaultStdModules = std.Names()

// Options 是解析参数。
type Options struct {
	IncludeDirs []string // -I，按命令行顺序
	ProjectRoot string   // include 根的另一个候选；为空则用 entry 所在目录
	StdModules  []string // 为空则用 DefaultStdModules
	Lang        diag.Lang
	MaxDiags    int
}

// Module 是一个被展开的源文件。
type Module struct {
	// Path 是**相对 include root 的 POSIX 路径**。IR 记录的就是它（IR.md §9.2），
	// 因为绝对路径不可重现：源码树挪个位置，产物就变了。
	Path    string
	Kind    string // "hermes" | "h"
	AbsPath string // 实际读到的路径；**不进 IR**，只用于读文件与去重
	Source  []byte
	// Builtin 为 true 表示来自内置 stdlib（go:embed），不是磁盘上的文件。
	Builtin bool
}

// Link 是一条解析成功的 `#link`。
type Link struct {
	Spec       string
	Module     string // 模块名（"std/net" 或 "models"）
	Kind       string // "std" | "local"
	FromModule string // 写这条指令的模块（相对路径）
	Line, Col  int
}

// Result 是一次 include 图解析的全部产出。
type Result struct {
	Modules []Module // 按 include 到达顺序（深度优先，先序）
	Links   []Link
}

type resolver struct {
	opts    Options
	root    string
	std     map[string]bool
	res     *Result
	seen    map[string]bool // include-once：键 = 绝对路径
	stack   []string        // 当前 DFS 链（绝对路径），用于报环的完整链条
	ds      []diag.Diag
	dropped int
}

// Resolve 从 entry 开始展开整棵 include 图。
//
// 返回的 Modules **包含 entry 自己**（它排第一），因为 IR 的 unit.modules[]
// 是"展开后的全部模块，按 include 到达顺序"（IR.md §2）。
func Resolve(entryPath string, entrySrc []byte, opts Options) (*Result, []diag.Diag) {
	if opts.Lang == "" {
		opts.Lang = diag.LangFromEnv()
	}
	if opts.MaxDiags <= 0 {
		opts.MaxDiags = 100
	}
	std := opts.StdModules
	if len(std) == 0 {
		std = DefaultStdModules
	}

	abs, err := filepath.Abs(entryPath)
	if err != nil {
		abs = entryPath
	}
	root := opts.ProjectRoot
	if root == "" {
		root = filepath.Dir(abs)
	}
	if root, err = filepath.Abs(root); err != nil {
		root = filepath.Dir(abs)
	}

	r := &resolver{
		opts: opts,
		root: root,
		std:  map[string]bool{},
		res:  &Result{},
		seen: map[string]bool{},
	}
	for _, m := range std {
		r.std[m] = true
	}

	// entry 自己也是图里的一个节点：先登记再用，否则"某个头 include 回 entry"
	// 不会被环检测抓住，而会无限展开到深度上限。
	r.seen[abs] = true
	r.stack = append(r.stack, abs)
	r.visit(abs, entrySrc, "hermes", 0)
	r.stack = r.stack[:len(r.stack)-1]

	if r.dropped > 0 {
		r.add(diag.CodePanelTruncated, diag.SeverityWarning, diag.Loc{Module: relName(root, abs)},
			map[string]string{"shown": itoa(len(r.ds)), "dropped": itoa(r.dropped)})
	}
	return r.res, r.ds
}

func (r *resolver) add(code string, sev diag.Severity, loc diag.Loc, args map[string]string) {
	if len(r.ds) >= r.opts.MaxDiags {
		r.dropped++
		return
	}
	r.ds = append(r.ds, diag.New(code, sev, loc, r.opts.Lang, args))
}

// visit 读入一个模块，展开它的指令，然后递归。
//
// 用**递归**而不是迭代 DFS：深度上限是 32，Go 的栈会自动扩容，
// 这里不存在 C++ 侧那个"深链爆栈在 -fno-exceptions 下无法捕获"的问题。
// 把上限当作保护的是"恶意输入"，不是"调用栈"。
func (r *resolver) visit(abs string, src []byte, kind string, depth int) {
	name := relName(r.root, abs)

	if depth > MaxDepth {
		r.add(diag.CodeIncludeDepth, diag.SeverityError, diag.Loc{Module: name},
			map[string]string{"depth": itoa(depth), "limit": itoa(MaxDepth)})
		return
	}
	if len(r.res.Modules) >= MaxModules {
		// 只报一次，不逐层重复刷。
		if len(r.res.Modules) == MaxModules {
			r.add(diag.CodeIncludeTooMany, diag.SeverityError, diag.Loc{Module: name},
				map[string]string{"count": itoa(MaxModules + 1), "limit": itoa(MaxModules)})
		}
		return
	}

	r.res.Modules = append(r.res.Modules, Module{Path: name, Kind: kind, AbsPath: abs, Source: src})

	res, err := native.Parse(src, name, kind == "h")
	if err != nil {
		// 前端都跑不起来：这是环境/内存问题，不是用户源码问题。
		r.add(diag.CodeLoadFailed, diag.SeverityError, diag.Loc{Module: name},
			map[string]string{"detail": err.Error()})
		return
	}

	for _, d := range ScanDirectives(res.Dump) {
		loc := diag.Loc{Module: name, Path: d.Kind, Line: d.Line, Col: d.Col}
		if d.Kind == "Link" {
			r.handleLink(d, loc)
			continue
		}
		r.handleInclude(d, abs, loc, depth)
	}
}

func (r *resolver) handleLink(d Directive, loc diag.Loc) {
	spec := d.Path
	// 模块名 = Ident ("/" Ident)*，不能带 .hbc 后缀、不能是路径（E1010）。
	if d.Angle {
		// <std/NAME>
		if !strings.HasPrefix(spec, "std/") {
			r.add(diag.CodeLinkNotStd, diag.SeverityError, loc,
				map[string]string{"spec": d.Spec, "available": r.availableStd()})
			return
		}
		name := strings.TrimPrefix(spec, "std/")
		if !r.std[name] {
			r.add(diag.CodeLinkNotStd, diag.SeverityError, loc,
				map[string]string{"spec": d.Spec, "available": r.availableStd()})
			return
		}
		r.res.Links = append(r.res.Links, Link{
			Spec: d.Spec, Module: spec, Kind: "std",
			FromModule: loc.Module, Line: d.Line, Col: d.Col,
		})
		return
	}

	// 引号形式：本地模块名。
	if strings.HasSuffix(spec, ".hbc") || strings.Contains(spec, "..") ||
		strings.HasPrefix(spec, "/") || strings.Contains(spec, "\\") {
		r.add(diag.CodeLinkBadModule, diag.SeverityError, loc,
			map[string]string{"spec": d.Spec, "suggestion": stripHbc(spec)})
		return
	}
	r.res.Links = append(r.res.Links, Link{
		Spec: d.Spec, Module: spec, Kind: "local",
		FromModule: loc.Module, Line: d.Line, Col: d.Col,
	})
}

func (r *resolver) handleInclude(d Directive, fromAbs string, loc diag.Loc, depth int) {
	spec := d.Path

	// ① 路径不得逃出 include 根（E1009）。
	if hasDotSegment(spec) {
		r.add(diag.CodePathEscape, diag.SeverityError, loc,
			map[string]string{"path": d.Spec, "suggestion": cleanPath(spec)})
		return
	}
	// ② 只吃 .h（E3015）。这条必须在搜索之前 —— 否则"找到了 .hermes 但拒绝"
	//    会和"没找到"混成同一类错误，而它们的修复动作完全不同。
	if strings.HasSuffix(spec, ".hermes") {
		base := strings.TrimSuffix(spec, ".hermes")
		r.add(diag.CodeIncludeHermes, diag.SeverityError, loc, map[string]string{
			"path": d.Spec, "mod": base, "name": base,
		})
		return
	}
	if !strings.HasSuffix(spec, ".h") {
		r.add(diag.CodeIncludeHermes, diag.SeverityError, loc, map[string]string{
			"path": d.Spec, "mod": spec, "name": strings.TrimSuffix(spec, filepath.Ext(spec)),
		})
		return
	}

	// ③ 尖括号只查内置表，**不触文件系统**。
	if d.Angle {
		if !strings.HasPrefix(spec, "std/") {
			r.add(diag.CodeLinkNotStd, diag.SeverityError, loc,
				map[string]string{"spec": d.Spec, "available": r.availableStd()})
			return
		}
		mod := strings.TrimSuffix(strings.TrimPrefix(spec, "std/"), ".h")
		if !r.std[mod] {
			r.add(diag.CodeLinkNotStd, diag.SeverityError, loc,
				map[string]string{"spec": d.Spec, "available": r.availableStd()})
			return
		}
		// 内置头现在有**真实字节**（internal/std 的 go:embed）。
		// 登记进 modules[] 有两个理由：它要进 `unit.source_sha256` 的输入，
		// 也要让 `requires[].member_sha256` 有东西可算 —— 否则供应链那一环
		// 永远是"声明了依赖却说不清是哪份字节"。
		if src, ok := std.Source(mod); ok {
			path := "std/" + mod + ".h"
			if !r.seen[path] {
				r.seen[path] = true
				r.res.Modules = append(r.res.Modules, Module{
					Path: path, Kind: "h", AbsPath: path, Source: src, Builtin: true,
				})
			}
			return
		}
		return
	}

	// ④ 引号形式：按 当前目录 → -I → 项目根 的顺序找。
	found, candidates := r.search(spec, filepath.Dir(fromAbs))
	if found == "" {
		// 找不到时把**试过的每个路径**都列出来。
		// 只说"找不到 xxx.h"会让人反复猜 -I 该指哪。
		r.add(diag.CodeLoadFailed, diag.SeverityError, loc, map[string]string{
			"detail": "cannot find " + spec + "; tried: " + strings.Join(candidates, ", ") +
				`. If you meant the implementation, write #link "` + strings.TrimSuffix(spec, ".h") + `";`,
		})
		return
	}
	// ⑤ 环检测**必须在 seen 检查之前**。顺序反了就是真 bug：
	//    "已在 DFS 链上"是环，"已展开过"是 include-once，两者都用"见过这个文件"
	//    表达，但含义相反。先把 seen 短路掉，环就永远走不到检测那一步，
	//    结果是无限展开到深度上限 —— 报的是 E3003（太深）而不是 E3002（成环），
	//    而这两条的修复动作完全不同。
	for i, s := range r.stack {
		if s == found {
			chain := ""
			for _, x := range r.stack[i:] {
				if chain != "" {
					chain += " -> "
				}
				chain += relName(r.root, x)
			}
			chain += " -> " + relName(r.root, found)
			r.add(diag.CodeIncludeCycle, diag.SeverityError, loc, map[string]string{"chain": chain})
			return
		}
	}
	if r.seen[found] {
		return // include-once：重复 include 是 no-op（不在当前链上，所以不是环）
	}
	src, err := os.ReadFile(found)
	if err != nil {
		r.add(diag.CodeLoadFailed, diag.SeverityError, loc,
			map[string]string{"detail": "cannot read " + found + ": " + err.Error()})
		return
	}

	r.seen[found] = true
	r.stack = append(r.stack, found)
	r.visit(found, src, "h", depth+1)
	r.stack = r.stack[:len(r.stack)-1]
}

// search 按 LANGUAGE §9.2 的顺序找文件，返回命中路径与**全部候选**。
func (r *resolver) search(spec, fromDir string) (string, []string) {
	var candidates []string
	dirs := append([]string{fromDir}, r.opts.IncludeDirs...)
	dirs = append(dirs, r.root)
	for _, d := range dirs {
		p := filepath.Join(d, filepath.FromSlash(spec))
		candidates = append(candidates, p)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, candidates
		}
	}
	return "", candidates
}

func (r *resolver) availableStd() string {
	var parts []string
	for _, m := range r.opts.StdModules {
		parts = append(parts, "<std/"+m+">")
	}
	if len(parts) == 0 {
		for _, m := range DefaultStdModules {
			parts = append(parts, "<std/"+m+">")
		}
	}
	return strings.Join(parts, ", ")
}

// relName 把绝对路径转成相对 include root 的 POSIX 路径（可重现构建要求）。
func relName(root, abs string) string {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return filepath.ToSlash(abs)
	}
	return filepath.ToSlash(rel)
}

// hasDotSegment 判断路径里是否有 "." 或 ".." 段。
func hasDotSegment(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

// cleanPath 去掉 "." 段，给 E1009 的建议用。
func cleanPath(p string) string {
	var out []string
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." {
			continue
		}
		out = append(out, seg)
	}
	return strings.Join(out, "/")
}

func stripHbc(s string) string { return strings.TrimSuffix(s, ".hbc") }

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
