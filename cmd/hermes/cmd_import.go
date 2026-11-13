package main

import (
	"fmt"
	"os"
	"path/filepath"

	"hermes/internal/importhomo"
)

// import 子命令：把 `homonoia compile --emit --json` 的产物转成 `.hermes` 源码。
//
// 这是 D-023 路 A 允许的**唯一**接触面。三条硬边界（都在 tests/guard 里盯着）：
//  1. 输入是 JSON 计划，不是部署源文件——Hermes 不解析那个语言；
//  2. 输出是 `.hermes` 源码，不是旁路数据，所以导入产物天然 `origin:"source"`；
//  3. 不解释 argv 语义，只认 `--http` / `--model`，其余丢弃并 warn。
//
// 退出码沿用全局约定：0 成功 / 2 用法错 / 4 运行失败（读不到、不是合法计划、写不出）。
func cmdImport(args []string) int {
	var planPath, outPath string

	// 手工解析而不是 flag 包：Go 的 flag 遇到第一个位置参数就停止解析，
	// `hermes import plan.json -o x.hermes` 会把 `-o` 当成第二个位置参数。
	// 这个坑在 cmd_trace.go 里已经踩过一次，不要踩第二次。
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--from-plan":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, tr("hermes import: --from-plan needs a file argument\n\n")+importUsage)
				return 2
			}
			planPath = args[i+1]
			i++
		case "-o", "--out":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, tr("hermes import: -o needs a file argument\n\n")+importUsage)
				return 2
			}
			outPath = args[i+1]
			i++
		case "-h", "--help", "help":
			fmt.Print(importUsage)
			return 0
		default:
			// 允许把计划文件当位置参数给，兼容 `hermes import plan.json`。
			if planPath == "" && a != "" && a[0] != '-' {
				planPath = a
				continue
			}
			fmt.Fprintf(os.Stderr, "hermes import: %s %q\n\n%s", tr("unknown argument"), a, importUsage)
			return 2
		}
	}

	if planPath == "" {
		fmt.Fprint(os.Stderr, tr("hermes import: missing --from-plan\n\n")+importUsage)
		return 2
	}

	data, err := os.ReadFile(planPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes import: cannot read plan %s: %v\n"), planPath, err)
		return 4
	}

	steps, err := importhomo.Parse(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes import: %s is not a usable plan: %v\n"), planPath, err)
		return 4
	}

	res := importhomo.Emit(steps)

	// warning 走 stderr：产物 stdout 必须只有源码，否则 `> x.hermes` 会把
	// 警告一起写进文件，而那是一个会被提交上去的坏文件。
	for _, w := range res.Warnings {
		fmt.Fprintf(os.Stderr, tr("hermes import: note: %s\n"), w)
	}

	if outPath == "" {
		os.Stdout.Write(res.Source)
		return 0
	}

	if dir := filepath.Dir(outPath); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, tr("hermes import: cannot create directory %s: %v\n"), dir, err)
			return 4
		}
	}
	if err := os.WriteFile(outPath, res.Source, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes import: cannot write %s: %v\n"), outPath, err)
		return 4
	}

	fmt.Fprintf(os.Stderr, tr("hermes import: wrote %s (%d bytes, dropped %d kinds of unrecognised flags)\n"),
		outPath, len(res.Source), len(res.Warnings))
	return 0
}

var importUsage = pickUsage(`hermes import - turn a Homonoia deployment plan into .hermes source

Usage:
  hermes import --from-plan <plan.json> [-o <out.hermes>]

Notes:
  The input must be the output of: homonoia compile --emit --json (a JSON array).
  This is not parsing .homo: that language belongs to Homonoia, Hermes only reads
  its output (D-023).

  Only two things are understood: the gateway --http (becomes the backend address)
  and --model (comments only; at runtime /v1/models is authoritative). Every other
  flag is dropped with a note on stderr.

Exit codes:
  0  success
  2  usage error
  4  unreadable / not a valid plan / cannot write
`,
	`hermes import —— 把 Homonoia 的部署计划转成 .hermes 源码

用法:
  hermes import --from-plan <plan.json> [-o <out.hermes>]

说明:
  输入必须是 homonoia compile --emit --json 的输出（JSON 数组）。
  这不是解析部署脚本 —— 那个语言归 Homonoia，Hermes 只读它的产物（D-023）。

  导入器只认两件事：网关的 --http（变成 backend 地址）与 --model（只作注释，
  运行期一律以 /v1/models 为准）。其余参数一律丢弃并在 stderr 提示。

  产物是一个**占位** workflow：部署计划描述「起什么」，编排描述「做什么」，
  后者必须由人来写。导入器不替你发明它。

退出码:
  0  成功
  2  用法错误
  4  读不到 / 不是合法计划 / 写不出
`)
