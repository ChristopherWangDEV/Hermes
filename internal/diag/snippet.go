package diag

import (
	"strconv"
	"strings"
)

// Snippet 把一行源码与它的 `^` 标记渲染成 Rust 风格的片段：
//
//	3 |     return x + 1;
//	  |     ^^^^^^^^^^^^
//
// 为什么渲染层做这件事：所有诊断（C++ 前端、Go 装配、IR 校验）都需要同样的
// 形状。各写一遍必然出现"有的地方画了波浪线、有的地方只有行号"。
func Snippet(src []byte, line, col, endLine, endCol int) string {
	if line <= 0 || len(src) == 0 {
		return ""
	}
	lines := strings.Split(string(src), "\n")
	if line > len(lines) {
		return ""
	}
	if endLine < line {
		endLine = line
	}
	if endLine > len(lines) {
		endLine = len(lines)
	}
	num := strconv.Itoa(endLine)
	pad := strings.Repeat(" ", len(num))

	var b strings.Builder
	b.WriteString(pad + " |\n")
	for n := line; n <= endLine; n++ {
		text := strings.TrimRight(lines[n-1], "\r")
		b.WriteString(strconv.Itoa(n) + " | " + text + "\n")
		b.WriteString(pad + " | ")
		switch {
		case line == endLine:
			// 单行：从起始列画到结束列。
			b.WriteString(strings.Repeat(" ", maxInt(col-1, 0)))
			b.WriteString(strings.Repeat("^", clampWidth(col, endCol)))
		case n == line:
			// 首行：从起始列画到行尾。
			b.WriteString(strings.Repeat(" ", maxInt(col-1, 0)))
			width := len(text) - (col - 1)
			if width < 1 {
				width = 1
			}
			b.WriteString(strings.Repeat("^", width))
		case n == endLine:
			// 末行：从行首画到结束列（没有结束列就整行）。
			width := endCol - 1
			if width < 1 {
				width = len(text)
			}
			b.WriteString(strings.Repeat("^", width))
		default:
			// 中间行：整行。
			b.WriteString(strings.Repeat("^", maxInt(len(text), 1)))
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// clampWidth 返回单行标记的宽度：至少 1，至多到行尾。
func clampWidth(col, endCol int) int {
	w := endCol - col
	if w < 1 {
		w = 1
	}
	return w
}
