package diag

import (
	"io"
	"os"
	"strings"
)

// ColorMode 控制诊断输出是否上色。
//
// auto = 仅在交互终端且未设 NO_COLOR 时上色（与 rustc 默认行为一致：
// 管道 / CI 里是纯文本，不会在日志里塞 ANSI 转义，下游 grep / 日志解析不会炸）。
type ColorMode int

const (
	ColorAuto ColorMode = iota
	ColorAlways
	ColorNever
)

// ShouldColor 决定对给定 writer 是否上色。
//
// 红线相关：零第三方依赖。TTY 探测只用标准库（靠 os.File 的 ModeCharDevice），
// 不引入 golang.org/x/term 之类。NO_COLOR 环境变量（https://no-color.org）优先尊重。
func ShouldColor(mode ColorMode, w io.Writer) bool {
	switch mode {
	case ColorNever:
		return false
	case ColorAlways:
		return true
	case ColorAuto:
		if os.Getenv("NO_COLOR") != "" {
			return false
		}
		if os.Getenv("TERM") == "dumb" {
			return false
		}
		return isatty(w)
	}
	return false
}

// isatty 用标准库判断 writer 是否是交互终端（字符设备）。
// Windows 上 os.ModeCharDevice 恒为 0，于是永远返回 false —— 退化为纯文本。
func isatty(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// RenderAllColored 与 RenderAll 同义，但当 ShouldColor 为真时按严重程度给诊断上色。
//
// 上色是**整段外包 ANSI**，不改变任何可见文本 —— RenderAll 返回的纯文本仍是它的子串，
// 因此下游日志解析、grep 脚本、文档示例都不受影响。颜色关闭时逐字节等于 RenderAll，
// 既有测试（含 cmd 侧用 RenderAll 的断言）全部不受影响。
func RenderAllColored(ds []Diag, mode ColorMode, w io.Writer) string {
	if !ShouldColor(mode, w) {
		return RenderAll(ds)
	}
	var b strings.Builder
	for i, d := range ds {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(colorize(d.Render(), d.Severity))
	}
	return b.String()
}

// SeverityOfCode 按诊断码前缀判定严重程度：`W` 开头是警告，其余按错误。
//
// 码的首字母就是严重程度的唯一真相（LANGUAGE.md 起草时就按 E / W 划分），
// 所以这里**不设 severity 入参** —— 让调用方另传一个 severity，等于允许
// `warning[E4016]` 这种自相矛盾的输出存在。explain 与 LSP 都走这条判据。
func SeverityOfCode(code string) Severity {
	if strings.HasPrefix(code, "W") {
		return SeverityWarning
	}
	return SeverityError
}

// RenderExplain 渲染一条诊断的长说明（不含源码定位行），供 `hermes explain <code>` 使用。
//
// 与 Render 的区别：explain 解释的是"这一类错误长什么样"，没有具体源码位置，
// 所以省掉 `  --> ` 定位行。为何/怎么改 的暗化与 RenderAllColored 一致，复用同一个
// colorize，保证开关色时文本字节不变（下游 grep / 日志解析不受影响）。
//
// 头行的严重程度由 SeverityOfCode(code) 定：`explain W2003` 必须打出
// `warning[W2003]:`，打 `error[W2003]:` 是把警告谎报成错误。
func RenderExplain(code, message, why string, fixes []string, mode ColorMode, w io.Writer) string {
	sev := SeverityOfCode(code)
	var b strings.Builder
	b.WriteString(sev.String())
	b.WriteString("[")
	b.WriteString(code)
	b.WriteString("]: ")
	b.WriteString(message)
	b.WriteString("\n")
	if why != "" {
		b.WriteString("   why: ")
		b.WriteString(why)
		b.WriteString("\n")
	}
	for _, f := range fixes {
		b.WriteString("  help: ")
		b.WriteString(f)
		b.WriteString("\n")
	}
	if !ShouldColor(mode, w) {
		return b.String()
	}
	return colorize(b.String(), sev)
}

// colorize 把一段已渲染的诊断按严重程度上色：头行（severity[CODE]: message）
// 按错误/警告上红/黄，why:/help:/note: 标签做暗化，让"为什么/怎么改"衬在错误之后，
// 层次与 rustc 一致。所有 ANSI 转义都包在可见文本之外，故下游客服文本子串不变。
func colorize(s string, sev Severity) string {
	var head, body string
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		head = s[:i]
		body = s[i:]
	} else {
		head = s
	}
	switch sev {
	case SeverityError:
		head = "\x1b[1;31m" + head + "\x1b[0m"
	case SeverityWarning:
		head = "\x1b[1;33m" + head + "\x1b[0m"
	}
	body = strings.ReplaceAll(body, "   why: ", "\x1b[2m   why: \x1b[0m")
	body = strings.ReplaceAll(body, "  help: ", "\x1b[2m  help: \x1b[0m")
	body = strings.ReplaceAll(body, "  note: ", "\x1b[2m  note: \x1b[0m")
	return head + body
}
