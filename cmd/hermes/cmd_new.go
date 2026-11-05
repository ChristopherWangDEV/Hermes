package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// new 子命令：生成一个可以直接跑的项目骨架。
//
// 为什么需要它：语言允许一个 unit 有多个 workflow（D-037），但"多 workflow 怎么组织
// 文件、`#link` 放哪、契约与实现怎么分"这些**结构性决定**仍然费脑子。脚手架把
// 这些决定固化成一个能跑的例子，用户改内容而不是从头想结构。
func cmdNew(args []string) int {
	name := ""
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "-h", "--help", "help":
			fmt.Print(newUsage)
			return 0
		default:
			if name == "" {
				name = a
			} else {
				fmt.Fprintf(os.Stderr, tr("hermes new: unexpected argument %q\n\n")+newUsage, a)
				return 2
			}
		}
	}
	if name == "" {
		fmt.Fprint(os.Stderr, newUsage)
		return 2
	}

	if _, err := os.Stat(name); err == nil {
		fmt.Fprintf(os.Stderr, tr("hermes new: %s already exists\n"), name)
		return 4
	}
	unit := sanitizeUnit(name)
	files := map[string]string{
		filepath.Join(name, unit+".h"): `// 契约：类型、backend 声明、workflow 签名。
// 分发 .hbc 时，对方只需要这个文件。
#include <std/model.h>
#include <std/net.h>

export struct Input {
    string text;
}

export backend gateway : openai;

export workflow triage(input: Input, backend = gateway) entry -> string;
export workflow ping(input: Input) -> string;
`,
		filepath.Join(name, unit+".hermes"): `// 实现：#link、地址、workflow 体。
#include "` + unit + `.h"

#link <std/model>;
#link <std/net>;

// 地址走环境注入：硬编码地址的库没法分发。
backend gateway = env("GATEWAY_URL", "http://127.0.0.1:8080");

workflow triage(input: Input, backend = gateway) entry {
    return "triage: {input.text}";
}

// 第二个 workflow：一个 unit 可以有多个（D-037）。
// 每个 unit 只能有**一个** entry —— 它是 run / serve 的默认入口。
workflow ping(input: Input) {
    return "pong";
}
`,
		filepath.Join(name, "README.md"): "# " + unit + "\n\n" +
			"    hermes build " + unit + ".hermes -o " + unit + ".hbc\n" +
			"    hermes run " + unit + ".hbc --input in.json\n" +
			"    hermes run " + unit + ".hbc --workflow ping --input in.json\n" +
			"    hermes explain " + unit + ".hbc\n\n" +
			unit + ".h holds the contract (types, backend declarations, workflow signatures).\n" +
			unit + ".hermes holds the implementation (#link, addresses, bodies).\n" +
			"They are separate because the contract must be distributable on its own,\n" +
			"while addresses and link inputs are choices of this file.\n",
		filepath.Join(name, "in.json"): `{"text": "hello"}` + "\n",
	}
	for path, body := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "hermes new: %v\n", err)
			return 4
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "hermes new: %v\n", err)
			return 4
		}
	}
	fmt.Printf("%s\n", fmt.Sprintf(tr("created %s/"), name))
	fmt.Printf("  cd %s && hermes build %s.hermes -o %s.hbc\n", name, unit, unit)
	return 0
}

// sanitizeUnit 把目录名变成合法的 unit 名（标识符）。
func sanitizeUnit(name string) string {
	out := make([]rune, 0, len(name))
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
			out = append(out, r)
		case r >= '0' && r <= '9' && i > 0:
			out = append(out, r)
		case r == '-', r == ' ':
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "unit"
	}
	return string(out)
}

const newUsage = `hermes new <dir> - scaffold a project

Usage:
  hermes new myapp

Creates:

  myapp/myapp.h        contract: types, backend declarations, workflow signatures
  myapp/myapp.hermes   implementation: #link, addresses, bodies
  myapp/in.json        a sample input
  myapp/README.md      how to build and run

Notes:
  A unit may hold several workflows, but exactly one of them is the entry.
`
