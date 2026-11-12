package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"hermes/internal/archive"
	"hermes/internal/diag"
	"hermes/internal/engine"
	"hermes/internal/ir"
	"hermes/internal/runtime"
	"hermes/internal/trace"
)

// run 子命令：执行一份 IR。
//
// 第一刀只支持"没有外部调用"的 IR（seq/decl/lit/var/field/interp/if/match/return）。
// 遇到不支持的节点**明确失败**，不跳过——少执行一个分支的产物看起来完全正常。
func cmdRun(args []string) int {
	var irPath, wfName, inputPath, tracePath, storeDir string

	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--workflow":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, tr("hermes run: --workflow needs a name\n\n")+runUsage)
				return 2
			}
			wfName = args[i+1]
			i++
		case "--input":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, tr("hermes run: --input needs a file\n\n")+runUsage)
				return 2
			}
			inputPath = args[i+1]
			i++
		case "--trace":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, tr("hermes run: --trace needs a file\n\n")+runUsage)
				return 2
			}
			tracePath = args[i+1]
			i++
		case "--store":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, tr("hermes run: --store needs a directory\n\n")+runUsage)
				return 2
			}
			storeDir = args[i+1]
			i++
		case "--color":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, tr("hermes run: --color needs auto|always|never\n\n")+runUsage)
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
				fmt.Fprintf(os.Stderr, tr("hermes run: unknown --color %q (want auto|always|never)\n\n%s"), args[i+1], runUsage)
				return 2
			}
			i++
		case "--color=auto":
			diagColor = diag.ColorAuto
		case "--color=always":
			diagColor = diag.ColorAlways
		case "--color=never":
			diagColor = diag.ColorNever
		case "-h", "--help", "help":
			fmt.Print(runUsage)
			return 0
		default:
			if irPath == "" && a != "" && a[0] != '-' {
				irPath = a
				continue
			}
			fmt.Fprintf(os.Stderr, "hermes run: %s %q\n\n%s", fmt.Sprintf(tr("unknown argument")), a, runUsage)
			return 2
		}
	}
	if irPath == "" {
		fmt.Fprint(os.Stderr, tr("hermes run: missing IR file\n\n")+runUsage)
		return 2
	}

	data, err := os.ReadFile(irPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes run: cannot read %s: %v\n"), irPath, err)
		return 4
	}
	// 允许直接跑 `.hbc`：读归档、取入口成员的 IR。这不是便利功能 ——
	// "分发出去的那份能不能跑"是 T44 的验收项，而分发出去的是 .hbc，
	// 不是 IR JSON。只能跑 JSON 的话，那条验收永远是在跑另一份东西。
	if len(data) >= 4 && string(data[0:4]) == archive.Magic {
		a, err := archive.Read(data)
		if err != nil {
			fmt.Fprintf(os.Stderr, tr("hermes run: %s is not a usable archive: %v\n"), irPath, err)
			return 4
		}
		if len(a.Members) == 0 {
			fmt.Fprintf(os.Stderr, tr("hermes run: %s has no members\n"), irPath)
			return 4
		}
		data = a.Members[0].IR
	}

	doc, ds := ir.Load(data)
	if len(ds) != 0 {
		fmt.Fprintf(os.Stderr, tr("hermes run: IR is invalid (%d diagnostics):\n%s\n"), len(ds), diag.RenderAllColored(ds, diagColor, os.Stderr))
		return 4
	}

	var input any
	if inputPath != "" {
		raw, err := os.ReadFile(inputPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, tr("hermes run: cannot read %s: %v\n"), inputPath, err)
			return 4
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&input); err != nil {
			fmt.Fprintf(os.Stderr, tr("hermes run: %s is not valid JSON: %v\n"), inputPath, err)
			return 4
		}
	}

	// 从 IR 的 backends[] 构造真实 Runtime。没有 backend 时给 nil ——
	// 那样任何外部调用都会**明确报错**，而不是悄悄跳过。
	//
	// 存储也在这里接：`--store` 优先，其次是 HERMES_STORE。*惰性打开* ——
	// 不用 memory/vector 的 workflow 不该被要求配置一个存储目录。
	var rt engine.Runtime
	byName := make(map[string]ir.Backend, len(doc.Backends))
	for _, b := range doc.Backends {
		byName[b.Name] = b
	}
	if len(doc.Backends) > 0 || storeDir != "" {
		r := &runtime.Runtime{Backends: byName}
		if storeDir != "" {
			s, err := runtime.OpenStore(storeDir)
			if err != nil {
				fmt.Fprintf(os.Stderr, tr("hermes run: %v\n"), err)
				return 4
			}
			r.StoreHandle = s
		}
		rt = r
	}

	res, err := engine.RunWith(doc, wfName, input, engine.RunOptions{Runtime: rt})
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes run: %v\n"), err)
		return 4
	}

	// 序列化边界：NaN / ±Inf 只在这里拒（见 jsonboundary.go 的裁决说明）。
	// 必须放在 Marshal 之前 —— 让 encoding/json 先撞上，用户看到的就是
	// `json: unsupported value: NaN`，既不知道哪一项也不知道怎么办。
	if err := rejectNonFinite(res.Value); err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes run: cannot serialise result: %v\n"), err)
		return 4
	}
	out, err := json.Marshal(map[string]any{
		"workflow": res.Workflow,
		"value":    res.Value,
		"steps":    res.Steps,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes run: cannot serialise result: %v\n"), err)
		return 4
	}
	fmt.Println(string(out))

	if tracePath != "" {
		if err := writeTrace(tracePath, res.Spans); err != nil {
			fmt.Fprintf(os.Stderr, tr("hermes run: cannot write trace: %v\n"), err)
			return 4
		}
		fmt.Fprintf(os.Stderr, tr("hermes run: wrote trace %s (%d spans)\n"), tracePath, len(res.Spans))
	}
	return 0
}

// writeTrace 把 span 写成 trace 文件（带哈希链）。
//
// 用 trace.Writer 而不是自己拼 JSON：哈希链是这个文件的价值所在——
// 一份能被改的 trace 不能当证据，而"能不能当证据"正是它存在的理由。
func writeTrace(path string, spans []trace.Span) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := trace.NewWriter(f, trace.Options{})
	for _, s := range spans {
		if err := w.Emit(s); err != nil {
			return err
		}
	}
	_, err = w.Close()
	return err
}

var runUsage = pickUsage(`hermes run - execute an IR document or a .hbc archive

Usage:
  hermes run <unit.ir.json | unit.hbc> [--workflow <name>] [--input <input.json>]
             [--trace <out.json>] [--store <dir>]

Notes:
  Without --workflow the is_entry workflow is used. Model and tool calls go through
  the backends declared in the IR; without them any external call fails loudly.
  memory.* / vector.* use --store (or $HERMES_STORE); without one they fail loudly.

Exit codes:
  0  success
  2  usage error
  4  execution failed
`,
	`hermes run —— 执行一份 IR

用法:
  hermes run <unit.ir.json> [--workflow <名字>] [--input <input.json>] [--trace <out.json>]

说明:
  省略 --workflow 时用 is_entry 标记的那个。

  这一刀只支持不依赖外部调用的 IR（seq/decl/lit/var/field/interp/if/match/return），
  其余节点会明确报错——不会跳过任何分支。
`)
