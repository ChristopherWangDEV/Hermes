package main

import (
	"fmt"
	"os"
	"strings"
)

// fmt 子命令：**只做空白级规范化**（T36）。
//
// 为什么不做 AST 重排版：那需要一个完整的源码打印器，而打印器一旦与解析器
// 不同步，`fmt` 就会把能编的代码改成不能编的 —— 那是工具能犯的最严重错误之一。
// 所以在打印器成熟之前，这里只做**能证明安全**的三件事：
//
//	① 行尾空白去掉   ② CRLF → LF   ③ 文件末尾恰好一个换行
//
// 三件都是"改完还能编"的局部变换，不需要理解语法。
//
// 另一半职责更重要：**格式不对时能检查出来**（--check），这样 CI 能挡住
// "有人用 Windows 保存了一次"这种静默的整文件 diff。
func cmdFmt(args []string) int {
	check := false
	var files []string
	for _, a := range args {
		switch a {
		case "--check":
			check = true
		case "-h", "--help", "help":
			fmt.Print(fmtUsage)
			return 0
		default:
			if a != "" && a[0] == '-' {
				fmt.Fprintf(os.Stderr, tr("hermes fmt: unknown flag %q\n\n%s"), a, fmtUsage)
				return 2
			}
			files = append(files, a)
		}
	}
	if len(files) == 0 {
		fmt.Fprint(os.Stderr, tr("hermes fmt: missing source file\n\n")+fmtUsage)
		return 2
	}

	changed := 0
	for _, p := range files {
		src, err := os.ReadFile(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, tr("hermes fmt: cannot read %s: %v\n"), p, err)
			return 4
		}
		out := normalizeWhitespace(src)
		if string(out) == string(src) {
			continue
		}
		changed++
		if check {
			fmt.Fprintf(os.Stderr, tr("hermes fmt: %s needs formatting\n"), p)
			continue
		}
		if err := os.WriteFile(p, out, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, tr("hermes fmt: cannot write %s: %v\n"), p, err)
			return 4
		}
		fmt.Fprintf(os.Stderr, tr("hermes fmt: formatted %s\n"), p)
	}

	if check && changed > 0 {
		return 4
	}
	return 0
}

// normalizeWhitespace 做那三件局部变换。
//
// 刻意**不碰缩进**：缩进要理解块结构，而"看起来对了但语义没变"没法证明。
// 行尾空白与换行符不同 —— 它们在任何上下文里都不改变语义。
func normalizeWhitespace(src []byte) []byte {
	s := strings.ReplaceAll(string(src), "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")

	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	s = strings.Join(lines, "\n")

	// 末尾恰好一个换行：0 个会让某些工具报"no newline at end of file"，
	// 多个则是纯粹的空行堆积。
	s = strings.TrimRight(s, "\n") + "\n"
	return []byte(s)
}

const fmtUsage = `hermes fmt - normalise whitespace in .hermes / .h sources (T36)

Usage:
  hermes fmt <file...>
  hermes fmt --check <file...>

Notes:
  Only three transformations are applied, all of which provably cannot change meaning:
    - strip trailing whitespace
    - CRLF -> LF
    - exactly one trailing newline

  Indentation is deliberately NOT touched: it requires understanding block structure,
  and a formatter that cannot be proven safe can turn compiling code into non-compiling
  code -- the worst thing a tool can do. Full AST printing is a separate, larger job.

Exit codes:
  0  formatted (or already formatted)
  2  usage error
  4  unreadable, or --check found a file needing formatting
`
