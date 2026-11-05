package main

import (
	"fmt"
	"os"

	"hermes/internal/lsp"
)

// lsp 子命令：语言服务器（T49）。
//
// 为什么它比 CLI 更值得先做：编译器的体验有一大半在编辑器里。用户判断
// "这个工具能不能用"，看的是按下 `.` 之后有没有补全、写错时有没有红线。
func cmdLSP(args []string) int {
	for _, a := range args {
		switch a {
		case "-h", "--help", "help":
			fmt.Print(lspUsage)
			return 0
		}
	}
	if err := lsp.New(os.Stdin, os.Stdout).Run(); err != nil {
		fmt.Fprintf(os.Stderr, "hermes lsp: %v\n", err)
		return 4
	}
	return 0
}

const lspUsage = `hermes lsp - language server over stdio (T49)

Usage:
  hermes lsp

Notes:
  Speaks LSP over stdin/stdout. Editors launch it; humans do not.

  Implemented: diagnostics (parse-level), completion, hover, document symbols.
  Diagnostics are deliberately parse-level: a full build needs the include graph
  and type checking, which is too slow for every keystroke -- and a slow linter
  gets turned off.

Exit codes:
  0  client disconnected
  4  protocol error
`
