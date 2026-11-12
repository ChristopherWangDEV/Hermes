// Package diag 是 Hermes 的**编译期 / 加载期诊断**。
//
// 与 internal/errs 的分工是一条硬边界，不许合并：
//
//	errs.Error   一次请求为什么失败。单行、稳定、**不本地化**
//	             （它进日志、进 trace、被 diff 比对；翻成两种语言就等于
//	              同一份失败产生两种文本，回归测试立刻崩）
//	diag.Diag    一次编译/加载为什么被拒。多行、**可本地化**、带源码定位
//	             （它给人看，所以必须说清 期望 / 实际 / 后果 / 两种改法）
//
// 设计参照 rustc，不是参照 Python：
//
//	Python:  KeyError: 'a'              ← 只说"哪里错了"，不说"该是什么"
//	rustc:   error[E0308]: mismatched types
//	           --> src/main.rs:3:9
//	            expected `i32`, found `&str`
//	         ← 错误码可查 + 精确定位 + 期望/实际 + 后果 + 改法
//
// 这里做到 rustc 的六件事，一件不少：
//  1. 稳定错误码（可 grep、可挂文档锚点）
//  2. 精确位置（文件 + IR 里的 JSON 路径 + 回指源码行）
//  3. 期望 vs 实际（写在同一句里）
//  4. 后果（Why：不修会怎样 —— 这条 Python 几乎没有）
//  5. 改法（Fix：**至少两种**，因为"只有一种改法"的错误通常是设计错了）
//  6. 关联位置（Note：指回另一处定义，"你说的这个东西在哪"）
package diag

import (
	"os"
	"strings"
)

// Severity 与 C++ 前端 native/cpp/ast.h 的 Severity 一一对应，
// 因为两边诊断最终会合流成同一份输出。
type Severity int

const (
	SeverityError Severity = iota
	SeverityWarning
	SeverityNote
)

func (s Severity) String() string {
	switch s {
	case SeverityError:
		return "error"
	case SeverityWarning:
		return "warning"
	case SeverityNote:
		return "note"
	}
	return "unknown"
}

// Lang 是文案语言。默认英文（go-zero 风格：代码与默认输出用英文，
// 免得非中文使用者在日志里看到乱码）。
type Lang string

const (
	LangEN Lang = "en"
	LangZH Lang = "zh"
)

// DefaultLang 是可执行文件的默认诊断语言：**英文**。
//
// 覆盖方式是 HERMES_LANG（见 LangFromEnv）。这条注释以前写着"可通过 --lang /
// HERMES_LANG 覆盖"，而 HERMES_LANG 当时**根本没实现** —— 那种"文档说有、
// 实际没有"的注释比没有注释更糟：它让人以为自己已经设对了。
const DefaultLang = LangEN

// Loc 是一处位置。三个字段互相独立，允许部分缺失：
//
//	Module  文件名（"support.hermes"），必需
//	Path    IR 里的 JSON 路径（"workflows[0].nodes[3].kids[1]"），加载期诊断用它
//	Line/Col 回指源码位置，0 表示未知
//
// 为什么要同时有 Path 和 Line：IR 是**编译产物**，出错时用户手上可能只有 .hbc
// 没有源码。此时 Path 仍然精确（能指出是哪个 workflow 的哪个节点的哪个字段），
// 而 Line 只能说"未知"。反过来源码在手上时，Line 更直观。两者都给，让人自己选。
type Loc struct {
	Module  string
	Path    string
	Line    int
	Col     int
	EndLine int
	EndCol  int
}

// WithLine 返回带上源码行号的副本（链式构造用）。
func (l Loc) WithLine(line, col int) Loc {
	l.Line, l.Col = line, col
	return l
}

// Note 是关联位置：指向另一处相关的地方（"这个符号在这里声明过"）。
type Note struct {
	Loc     Loc
	Message string
}

// Diag 是一条已经本地化的诊断快照。
//
// 它是**值语义**的：构造后不再改动，可以安全地跨 goroutine 传递与排序。
// 文案在构造时从 catalog 取定（见 New），所以同一个 Diag 反复渲染结果稳定 ——
// 这一点对 golden 比对是必需的。
type Diag struct {
	Severity Severity
	Code     string
	Loc      Loc
	Message  string
	Why      string
	Fix      []string
	// Snippet 是已渲染好的源码片段（含 `^` 标记）。渲染层不读文件：
	// 它不该知道文件从哪来（内存里的、磁盘上的、还是测试构造的）。
	Snippet string
	Notes   []Note
	// Edits 是机器可应用的修正编辑（M2：修正提示）。对应 C++ 前端在 Diag 上序列化的
	// edits[]：每条 = 一处 span + 一个 Replacement。Render() 不打印它，以保住
	// error[CODE] / --> / why: / help: / note: 的骨架稳定（catalog_test 锁死）。
	// 它由 LSP CodeAction / 工具链读走做自动修复。当前只承载单条主编辑，
	// 多编辑（同一诊断多处改写）超出本期范围。
	Edits []Edit
}

// Edit 是一条机器可应用的修正编辑（M2：修正提示）。
//
// 与 C++ 前端 Diag.edits 的 JSON 形状对齐：span 是源码位置，Replacement 是要
// 替换进去的文本。它让"语法纠正"从一句人读的建议变成一条工具可执行的改写。
type Edit struct {
	Loc         Loc
	Replacement string
}

// Explain 取一条码的**离线说明**，占位符已渲染成元变量 `<name>`。
//
// 与 New 的区别只有一件事：New 拿的是**实例值**（`{id}` → `7`），Explain 没有实例
// 可填 —— 它解释的是一类错误。三种做法里只有一种对：
//
//	留着 {name}   → 读者以为是没替换的 bug（曾经就是这样：`explain E_IR_DANGLING_REF`
//	                的头行印出 "{what} points at {id}"，等于什么也没说）
//	填个样例值    → 暗示"这种情况就是那个值"，把一类说成一个例子
//	换成 <name>   → 与文法书里的 EBNF 记法一致，读起来就是"这里放一个 id"
//
// 所以 Explain 是 explain 子命令的唯一入口：调用方不该自己碰模板，否则
// "{name} 该怎么显示"这条判断会散落在每个调用点上。
func Explain(code string, lang Lang) (text, why string, fixes []string, docs string, ok bool) {
	m, ok := Lookup(code, lang)
	if !ok {
		return "", "", nil, "", false
	}
	metavars := make(map[string]string, len(m.Args))
	for _, a := range m.Args {
		metavars[a] = "<" + a + ">"
	}
	text = subst(m.Text, metavars)
	why = subst(m.Why, metavars)
	fixes = make([]string, len(m.Fix))
	for i, f := range m.Fix {
		fixes[i] = subst(f, metavars)
	}
	return text, why, fixes, m.Docs, true
}

// New 按错误码构造一条诊断，文案从 catalog 取，{name} 占位按 args 替换。
//
// 未知 code 不会静默通过：会产出一条**自曝的诊断**（code 原样保留 + 明确文案），
// 因为"错误码拼错了"必须比"报错文案不好看"更早被发现。
// 守卫测试会断言 catalog 与代码里用到的 code 集合完全一致，所以这条兜底
// 在正常构建里永远不会出现。
func New(code string, sev Severity, loc Loc, lang Lang, args map[string]string) Diag {
	d := Diag{Severity: sev, Code: code, Loc: loc}
	m, ok := Lookup(code, lang)
	if !ok {
		d.Message = "internal: no message registered for code " + code
		d.Why = "The diagnostic catalog has no entry for this code. This is a bug in Hermes, not in your input."
		d.Fix = []string{"report this code at the issue tracker"}
		return d
	}
	d.Message = subst(m.Text, args)
	d.Why = subst(m.Why, args)
	if len(m.Fix) > 0 {
		d.Fix = make([]string, len(m.Fix))
		for i, f := range m.Fix {
			d.Fix[i] = subst(f, args)
		}
	}
	return d
}

// WithNote 追加一条关联位置，返回自身（便于链式调用）。
func (d Diag) WithNote(loc Loc, msg string) Diag {
	d.Notes = append(d.Notes, Note{Loc: loc, Message: msg})
	return d
}

// WithSourceLine 补上源码行号（加载期先有 Path，解析到源码后再补 Line）。
func (d Diag) WithSourceLine(line, col int) Diag {
	d.Loc.Line, d.Loc.Col = line, col
	return d
}

// Render 把诊断排成人读的多行文本。格式固定，与语言无关（只换文案不换骨架），
// 因此同一条错误在 en/zh 下的行数相同 —— 这一点让 diff 可读。
func (d Diag) Render() string {
	var b strings.Builder

	b.WriteString(d.Severity.String())
	b.WriteString("[")
	b.WriteString(d.Code)
	b.WriteString("]: ")
	b.WriteString(d.Message)
	b.WriteString("\n")

	// --> 位置行：文件 + JSON 路径 + 源码行（有则给）
	b.WriteString("  --> ")
	b.WriteString(d.Loc.Module)
	if d.Loc.Path != "" {
		b.WriteString("\n      at ")
		b.WriteString(d.Loc.Path)
	}
	if d.Loc.Line > 0 {
		b.WriteString("\n      line ")
		b.WriteString(itoa(d.Loc.Line))
		b.WriteString(", col ")
		b.WriteString(itoa(d.Loc.Col))
	}
	// 源码片段（有 Snippet 时才画）。Rust 的 `^` 标记之所以有用，是因为
	// 它把"错在哪"变成一眼可见；只有行号的话，人还得自己去数第几列。
	if d.Snippet != "" {
		b.WriteString("\n\n")
		b.WriteString(d.Snippet)
	}
	b.WriteString("\n")

	if d.Why != "" {
		b.WriteString("   why: ")
		b.WriteString(d.Why)
		b.WriteString("\n")
	}
	for _, f := range d.Fix {
		b.WriteString("  help: ")
		b.WriteString(f)
		b.WriteString("\n")
	}
	for _, n := range d.Notes {
		b.WriteString("  note: ")
		b.WriteString(n.Message)
		if n.Loc.Module != "" {
			b.WriteString(" (")
			b.WriteString(n.Loc.Module)
			if n.Loc.Path != "" {
				b.WriteString(" ")
				b.WriteString(n.Loc.Path)
			}
			if n.Loc.Line > 0 {
				b.WriteString(" line ")
				b.WriteString(itoa(n.Loc.Line))
			}
			b.WriteString(")")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// RenderAll 渲染一批诊断，每条之间空一行。顺序由调用方决定（不在这里排序）：
// 排序规则属于"什么顺序对人最有用"，那是调用方的判断，不是渲染层的。
func RenderAll(ds []Diag) string {
	if len(ds) == 0 {
		return ""
	}
	var b strings.Builder
	for i, d := range ds {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(d.Render())
	}
	return b.String()
}

// subst 用 {name} 占位替换，`{{` / `}}` 转义为字面的花括号。
//
// **不用 fmt**：fmt 的位置参数在改文案时极易错位（往前插一个 %s，后面全偏），
// 命名占位错不了；缺参数时 fmt 会在输出里留下 %!s(MISSING)，而命名占位由守卫
// 测试在构建前就抓住。
//
// **为什么必须有转义**：诊断文案里经常要举例（`{"t":"float","v":"1.5"}` 这种），
// 没有转义的话这些花括号会被当成占位符。第一版就踩了 —— 守卫测试报
// "文案里用了 {"t":"float","v":"1.5"}，但 Args 没声明它"。一个没有转义的模板
// 系统等于禁止在文案里写代码示例，那是荒谬的。
func subst(tmpl string, args map[string]string) string {
	if tmpl == "" {
		return tmpl
	}
	var b strings.Builder
	b.Grow(len(tmpl))
	for i := 0; i < len(tmpl); {
		c := tmpl[i]

		// 转义：`{{` → `{`，`}}` → `}`
		if c == '{' && i+1 < len(tmpl) && tmpl[i+1] == '{' {
			b.WriteByte('{')
			i += 2
			continue
		}
		if c == '}' && i+1 < len(tmpl) && tmpl[i+1] == '}' {
			b.WriteByte('}')
			i += 2
			continue
		}

		if c != '{' {
			b.WriteByte(c)
			i++
			continue
		}

		j := strings.IndexByte(tmpl[i:], '}')
		if j < 0 {
			b.WriteString(tmpl[i:])
			break
		}
		name := tmpl[i+1 : i+j]
		if v, ok := args[name]; ok {
			b.WriteString(v)
		} else {
			// 缺参时**原样保留占位**，不吞掉。吞掉会让"漏了一个参数"
			// 变成"文案读起来怪怪的"，而原样保留一眼就能看出来。
			b.WriteString(tmpl[i : i+j+1])
		}
		i += j + 1
	}
	return b.String()
}

// isPlaceholderName 判断 `{...}` 里的内容是否是合法占位符名。
// 只有 [a-z_][a-z0-9_]* 才算占位符；其余（比如 `{"t":"float"...`）必须写成转义。
// 这条规则让"忘了转义"从"渲染出怪东西"变成"守卫测试当场报错"。
func isPlaceholderName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c == '_' || (c >= 'a' && c <= 'z') || (i > 0 && c >= '0' && c <= '9')
		if !ok {
			return false
		}
	}
	return true
}

// placeholderNames 抽出模板里全部合法占位符名（跳过 `{{` 转义）。
// 放在非测试文件里，是为了让 catalog 的自检能力也能被产品代码复用
// （例如 --lint-catalog 这类运维命令）。
func placeholderNames(tmpl string) []string {
	var out []string
	for i := 0; i < len(tmpl); {
		if tmpl[i] != '{' {
			i++
			continue
		}
		if i+1 < len(tmpl) && tmpl[i+1] == '{' {
			i += 2 // 转义对，跳过
			continue
		}
		j := strings.IndexByte(tmpl[i:], '}')
		if j < 0 {
			break
		}
		name := tmpl[i+1 : i+j]
		if isPlaceholderName(name) {
			out = append(out, name)
		}
		i += j + 1
	}
	return out
}

// unescapedBraces 找出**没转义也没构成合法占位符**的花括号位置。
// 守卫测试用它抓"忘了转义代码示例"这类问题。
func unescapedBraces(tmpl string) []string {
	var out []string
	for i := 0; i < len(tmpl); {
		if tmpl[i] != '{' {
			i++
			continue
		}
		if i+1 < len(tmpl) && tmpl[i+1] == '{' {
			i += 2
			continue
		}
		j := strings.IndexByte(tmpl[i:], '}')
		if j < 0 {
			out = append(out, tmpl[i:])
			break
		}
		if name := tmpl[i+1 : i+j]; !isPlaceholderName(name) {
			out = append(out, tmpl[i:i+j+1])
		}
		i += j + 1
	}
	return out
}

// itoa 避免为几处拼接引入 strconv 依赖链之外的包。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
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

// FromTag 把语言标签收敛成 Lang。
//
// 只认**前缀**：`zh`、`zh-CN`、`zh-Hans` 都归到中文；其余一律回落英文。
// 为什么不做完整 BCP-47：诊断只有两种语言，多解析一层只会多一类"标签写错所以
// 悄悄变英文"的困惑。认不出就是英文 —— 那是默认值，也是默认行为。
func FromTag(tag string) Lang {
	t := strings.ToLower(strings.TrimSpace(tag))
	if strings.HasPrefix(t, "zh") {
		return LangZH
	}
	return LangEN
}

// LangFromEnv 读 HERMES_LANG。空值或认不出 → DefaultLang。
//
// 为什么是环境变量而不是命令行参数：语言是**会话级**的偏好，不是某条命令的参数。
// 每敲一次 `hermes ir` 都带 `--lang zh` 不现实，而漏带的后果是诊断突然变英文。
func LangFromEnv() Lang {
	if v, ok := os.LookupEnv("HERMES_LANG"); ok && v != "" {
		return FromTag(v)
	}
	return DefaultLang
}
