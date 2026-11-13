package main

import (
	"fmt"
	"os"

	"hermes/internal/diag"
	"hermes/internal/ir"
)

// ir 子命令：把一份 `.hermes` 编成 IR（canonical JSON）。
//
// 这一刀的范围：单个文件、单个 workflow、无 include、无 backend 表。
// 它存在的意义是**把整条链打通并可复现**（codegen → 装配 → canonical 字节），
// 而不是"支持完整语言"—— 不支持的构造会明确报错，不会产出缺斤少两的 IR。
//
// 退出码沿用全局约定：0 成功 / 2 用法错 / 4 编译失败（源码有问题或构造不支持）。
func cmdIR(args []string) int {
	var srcPath, outPath string
	var libDirs []string
	strictLink := true

	// 手工解析（同 cmd_trace / cmd_import）：flag 包遇到位置参数就停。
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "-L":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, tr("hermes ir: -L needs a directory\n\n")+irUsage)
				return 2
			}
			libDirs = append(libDirs, args[i+1])
			i++
		case "--no-strict-link":
			strictLink = false
		case "--strict-link":
			strictLink = true
		case "--color":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, tr("hermes ir: --color needs auto|always|never\n\n")+irUsage)
				return 2
			}
			switch args[i+1] {
			case "auto":
				diagColor = diag.ColorAuto
			case "always":
				diagColor = diag.ColorAlways
			case "never":
				diagColor = diag.ColorNever
			default:
				fmt.Fprintf(os.Stderr, tr("hermes ir: unknown --color %q (want auto|always|never)\n\n%s"), args[i+1], irUsage)
				return 2
			}
			i++
		case "--color=auto":
			diagColor = diag.ColorAuto
		case "--color=always":
			diagColor = diag.ColorAlways
		case "--color=never":
			diagColor = diag.ColorNever
		case "-o", "--out":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, tr("hermes ir: -o needs a file argument\n\n")+irUsage)
				return 2
			}
			outPath = args[i+1]
			i++
		case "-h", "--help", "help":
			fmt.Print(irUsage)
			return 0
		default:
			if srcPath == "" && a != "" && a[0] != '-' {
				srcPath = a
				continue
			}
			fmt.Fprintf(os.Stderr, tr("hermes ir: unknown argument %q\n\n%s"), a, irUsage)
			return 2
		}
	}
	if srcPath == "" {
		fmt.Fprint(os.Stderr, tr("hermes ir: missing source file\n\n")+irUsage)
		return 2
	}

	src, err := os.ReadFile(srcPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes ir: cannot read %s: %v\n"), srcPath, err)
		return 4
	}

	doc, err := buildIRWith(srcPath, src, &libSearch{dirs: libDirs, strict: strictLink})
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes ir: %v\n"), err)
		return 4
	}

	out, err := ir.Emit(doc)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes ir: %v\n"), err)
		return 4
	}

	if outPath == "" {
		os.Stdout.Write(out)
		return 0
	}
	if err := os.WriteFile(outPath, out, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes ir: cannot write %s: %v\n"), outPath, err)
		return 4
	}
	fmt.Fprintf(os.Stderr, tr("hermes ir: wrote %s (%d bytes)\n"), outPath, len(out))
	return 0
}

var irUsage = pickUsage(`hermes ir - compile .hermes into IR (canonical JSON)

Usage:
  hermes ir <file.hermes> [-o <out.json>] [-L <dir>]

Notes:
  -L <dir> resolves local #link "x" dependencies: the archive found there
  supplies the member_sha256 recorded in requires[]. Without it the dependency
  is NOT recorded (only a warning) -- a declared dependency must name its bytes.

  The output is the execution plan defined by IR.md: no floats, every node carries a
  source location, byte-stable (same source compiles to the same bytes).

  Supported today: single file or include graph, control flow, parallel, try/retry,
  types, schemas, tools, backends. Anything else fails loudly rather than producing
  a partial IR.

Exit codes:
  0  success
  2  usage error
  4  compilation failed
`,
	`hermes ir —— 把 .hermes 编成 IR（canonical JSON）

用法:
  hermes ir <file.hermes> [-o <out.json>] [-L <dir>]

说明:
  产物是 IR.md 定义的执行计划：无浮点、每个节点带源码位置、字节稳定
  （同一份源码两次编译逐字节相等）。

  这一刀只支持单文件、单个 workflow、无 include、无 backend 表；
  其他构造会**明确报错**而不是产出一份缺斤少两的 IR。

退出码:
  0  成功
  2  用法错误
  4  编译失败（源码有问题，或构造尚不支持）
`)
