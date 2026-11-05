package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"hermes/internal/archive"
)

// ar 子命令族：查看与拆开 `.hbc`。名字沿用 `ar`，因为这两件事是同构的
// ——「归档里有什么」「把其中一个拿出来」。
func cmdAr(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, arUsage)
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "t":
		return cmdArList(rest)
	case "x":
		return cmdArExtract(rest)
	case "-h", "--help", "help":
		fmt.Print(arUsage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "hermes ar: %s %q\n\n%s", tr("unknown subcommand"), sub, arUsage)
		return 2
	}
}

func readArchive(path string) (*archive.Archive, int) {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes ar: cannot read %s: %v\\n"), path, err)
		return nil, 4
	}
	a, err := archive.Read(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes ar: %s is not a usable archive: %v\\n"), path, err)
		return nil, 4
	}
	return a, 0
}

func cmdArList(args []string) int {
	if len(args) < 1 {
		fmt.Fprint(os.Stderr, tr("hermes ar t: missing .hbc file\\n\\n")+arUsage)
		return 2
	}
	a, code := readArchive(args[0])
	if a == nil {
		return code
	}
	for _, m := range a.Members {
		fmt.Printf("%s\n", m.Name)
		// 符号按四个命名空间分别列出：它们彼此独立，混在一起会让人以为同名冲突。
		for _, ns := range []string{"workflows", "types", "backends", "tools"} {
			exp, _ := m.Index["exports"].(map[string]any)
			list, _ := exp[ns].([]any)
			for _, s := range list {
				name, _ := s.(string)
				fmt.Printf("  %-10s %s\n", ns[:len(ns)-1], name)
			}
		}
	}
	return 0
}

func cmdArExtract(args []string) int {
	var hbcPath, member, outPath string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "-o", "--out":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, tr("hermes ar x: -o needs a file\\n\\n")+arUsage)
				return 2
			}
			outPath = args[i+1]
			i++
		default:
			if hbcPath == "" {
				hbcPath = a
			} else if member == "" {
				member = a
			} else {
				fmt.Fprintf(os.Stderr, "hermes ar x: %s %q\n\n%s", tr("unexpected argument"), a, arUsage)
				return 2
			}
		}
	}
	if hbcPath == "" || member == "" {
		fmt.Fprint(os.Stderr, tr("hermes ar x: need <file.hbc> <member>\\n\\n")+arUsage)
		return 2
	}
	a, code := readArchive(hbcPath)
	if a == nil {
		return code
	}
	for _, m := range a.Members {
		if m.Name != member {
			continue
		}
		if outPath == "" {
			outPath = filepath.Base(hbcPath) + "." + m.Name + ".ir.json"
		}
		if err := os.WriteFile(outPath, m.IR, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, tr("hermes ar x: cannot write %s: %v\\n"), outPath, err)
			return 4
		}
		fmt.Fprintf(os.Stderr, tr("hermes ar x: extracted %s (%d bytes)\\n"), outPath, len(m.IR))
		return 0
	}
	fmt.Fprintf(os.Stderr, tr("hermes ar x: no member named %q\\n"), member)
	return 4
}

var _ = json.Marshal

var arUsage = pickUsage(`hermes ar - inspect or extract .hbc members

Usage:
  hermes ar t <file.hbc>                    list members and symbols
  hermes ar x <file.hbc> <member> [-o <f>]  extract a member IR

Exit codes:
  0  success
  2  usage error
  4  unreadable or not a valid archive, or no such member
`,
	`hermes ar —— 查看与拆开 .hbc 归档

用法:
  hermes ar t <file.hbc>                    列出成员与符号
  hermes ar x <file.hbc> <member> [-o <f>]  拆出成员的 IR 字节

退出码:
  0  成功
  2  用法错误
  4  读不到 / 不是合法归档 / 没有该成员
`)
