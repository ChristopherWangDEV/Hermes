package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"hermes/internal/diag"
	"hermes/internal/include"
)

// 本文件实现**clang 风格的直接用法**：
//
//	hermes hello.hermes -o hello      # 一条命令出可执行文件（等价 clang a.c -o a）
//	hermes hello.hermes -c            # 只要"目标文件"——对 Hermes 就是 .hbc 归档
//	hermes hello.hermes -E            # 只做预处理（展开 #include），打印到 stdout
//
// 为什么必须这样：用户对编译器的肌肉记忆是 `cc file -o out`。要求先 `hermes build`
// 再 `hermes compile`，等于要求每个用户先读一遍我们的命令表 —— 而 clang 从没
// 要求过这件事。子命令仍然保留（`build`/`link`/`sign` 这些是多出来的能力，
// 不是多说一道手续）。
//
// 判定规则很简单：**第一个参数不是已知子命令、且以 .hermes/.h 结尾**就走 driver。
// 这样 `hermes build x.hermes` 与 `hermes x.hermes -o x` 都成立，不会打架。

// knownCommands 是现有的子命令集合（与 main.go 的 switch 一致）。
func knownCommands() map[string]bool {
	return map[string]bool{
		"version": true, "trace": true, "import": true, "ir": true, "run": true,
		"build": true, "ar": true, "link": true, "sign": true, "verify": true,
		"serve": true, "header": true, "compile": true, "cc": true, "doctor": true,
		"fmt": true, "explain": true, "lsp": true,
	}
}

// isDriverInvocation 判断 argv 是否该按 clang 风格解释。
func isDriverInvocation(args []string) bool {
	if len(args) == 0 {
		return false
	}
	first := args[0]
	if strings.HasPrefix(first, "-") {
		// `hermes -o x` 这种缺源文件的，交给 driver 报用法错 —— 比"未知命令"清楚。
		return first == "-o" || first == "-c" || first == "-E"
	}
	if knownCommands()[first] {
		return false
	}
	return strings.HasSuffix(first, ".hermes") || strings.HasSuffix(first, ".h")
}

// cmdDriver 是 clang 风格入口。
//
// 它**不重新实现**编译流程，而是把 flag 翻译成已有的子命令调用 ——
// 这样 driver 的每条路径都走同一套经过测试的代码。两套实现意味着两份 bug。
func cmdDriver(args []string) int {
	var (
		out     string
		objOnly bool
		preproc bool
		libDirs []string
		srcs    []string
		verbose bool
	)
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-o":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, "hermes: -o needs a file name\n")
				return 2
			}
			out = args[i+1]
			i++
		case a == "-c":
			objOnly = true
		case a == "-E":
			preproc = true
		case a == "-v" || a == "--verbose":
			verbose = true
		case a == "--version":
			return cmdVersion()
		case a == "-h" || a == "--help":
			fmt.Print(driverUsage)
			return 0
		case a == "-I":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, "hermes: -I needs a directory\n")
				return 2
			}
			i++ // include 搜索路径目前由 include.Resolve 从源文件目录推导
		case a == "-L":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, "hermes: -L needs a directory\n")
				return 2
			}
			libDirs = append(libDirs, args[i+1])
			i++
		case a == "-l":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, "hermes: -l needs a library name\n")
				return 2
			}
			i++ // 链接阶段由 requires 驱动；这里只接受语法
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "hermes: unknown flag %q\n\n%s", a, driverUsage)
			return 2
		default:
			srcs = append(srcs, a)
		}
	}
	if len(srcs) == 0 {
		fmt.Fprint(os.Stderr, "hermes: no input files\n\n"+driverUsage)
		return 2
	}
	if len(srcs) > 1 {
		// 多源文件需要"分别编译再链接"，而现在一次构建产出一个 unit。
		// 明确拒绝，不假装支持 —— 假装支持会产出只含第一个文件的二进制。
		fmt.Fprintf(os.Stderr,
			"hermes: only one source file is accepted (got %d).\n"+
				"compile multiple files into separate .hbc archives, then merge them with `hermes link`\n", len(srcs))
		return 2
	}
	src := srcs[0]

	if preproc {
		return runPreprocess(src, out)
	}

	base := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
	if objOnly {
		if out == "" {
			out = base + ".hbc"
		}
		// 注意：`cmdBuild` 接收的是**参数**，不含子命令名 —— 传进去的话
		// 它会把 "build" 当成源文件，随后把真正的源文件报成"未知参数"。
		argv := []string{src, "-o", out}
		for _, d := range libDirs {
			argv = append(argv, "-L", d)
		}
		if verbose {
			fmt.Fprintf(os.Stderr, "hermes: %s\n", strings.Join(argv, " "))
		}
		return cmdBuild(argv)
	}

	if out == "" {
		out = base // 像 clang：不给 -o 时叫 a.out？不 —— clang 叫 a.out
		out = "a.out"
	}
	argv := []string{src, "-o", out}
	if verbose {
		fmt.Fprintf(os.Stderr, "hermes: %s\n", strings.Join(argv, " "))
	}
	return cmdCompile(argv)
}

// runPreprocess 展开 #include 并打印，等价 `clang -E`。
//
// 输出顺序是**拓扑序**（被包含的在前），因为那正是编译器看到的顺序；
// 每个文件前加一行 `# <path>` 便于人读，且与 C 预处理器的行标记习惯一致。
func runPreprocess(src, out string) int {
	data, err := os.ReadFile(src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermes: cannot read %s: %v\n", src, err)
		return 4
	}
	res, ds := include.Resolve(src, data, include.Options{})
	for _, d := range ds {
		if d.Severity == diag.SeverityError {
			fmt.Fprintf(os.Stderr, "%s: %s: %s\n", d.Code, src, d.Message)
		}
	}
	var b strings.Builder
	for _, m := range res.Modules {
		fmt.Fprintf(&b, "# %s\n%s\n", m.Path, m.Source)
	}
	if out == "" {
		os.Stdout.WriteString(b.String())
		return 0
	}
	if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "hermes: cannot write %s: %v\n", out, err)
		return 4
	}
	return 0
}

const driverUsage = `hermes - compile .hermes like clang compiles .c

Usage:
  hermes <file.hermes> [-o <out>]      compile AND link into an executable
  hermes <file.hermes> -c [-o <out>]   compile only (produces a .hbc archive)
  hermes <file.hermes> -E [-o <out>]   preprocess only (expand #include)

Flags:
  -o <file>    output name (default: a.out for link, <base>.hbc for -c)
  -c           compile to .hbc instead of linking
  -E           preprocess only
  -L <dir>     library search directory (for local #link dependencies)
  -l <name>    library name (accepted; dependencies come from requires[])
  -v           echo the underlying command
  --version    print version

Subcommands still exist for what a compiler driver does not cover:
  build link sign verify ar run serve header cc doctor fmt explain lsp trace import

Exit codes:
  0  success
  2  usage error
  4  compilation or link failed
`
