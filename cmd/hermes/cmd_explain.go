package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"hermes/internal/archive"
	"hermes/internal/ir"
)

// explain 子命令：把一个 workflow 的**契约与代价上界**打印出来（T36）。
//
// 它回答的问题很具体："这东西跑起来最坏什么样、要花多少"。
// 这些数字在 IR 里都有（bounds / economics），但没人会去读 JSON ——
// 而"上界"这个东西只有被看见才有意义：看不见的上界等于没有上界。
func cmdExplain(args []string) int {
	var irPath, wfName string
	// --json：给 CI 与工具用的机器可读输出。人读的那份保留，两者不互相替代：
	// 人类要"一眼看出上界"，机器要"不用解析散文"。
	asJSON := false
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--workflow":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, tr("hermes explain: --workflow needs a name\n\n")+explainUsage)
				return 2
			}
			wfName = args[i+1]
			i++
		case "--json":
			asJSON = true
		case "-h", "--help", "help":
			fmt.Print(explainUsage)
			return 0
		default:
			if irPath == "" && a != "" && a[0] != '-' {
				irPath = a
				continue
			}
			fmt.Fprintf(os.Stderr, tr("hermes explain: unknown argument %q\n\n%s"), a, explainUsage)
			return 2
		}
	}
	if irPath == "" {
		fmt.Fprint(os.Stderr, tr("hermes explain: missing IR / .hbc\n\n")+explainUsage)
		return 2
	}

	data, err := os.ReadFile(irPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes explain: cannot read %s: %v\n"), irPath, err)
		return 4
	}
	if len(data) >= 4 && string(data[0:4]) == archive.Magic {
		a, err := archive.Read(data)
		if err != nil {
			fmt.Fprintf(os.Stderr, tr("hermes explain: %s is not a usable archive: %v\n"), irPath, err)
			return 4
		}
		data = a.Members[0].IR
	}
	doc, ds := ir.Load(data)
	if len(ds) != 0 {
		fmt.Fprintf(os.Stderr, tr("hermes explain: IR is invalid (%d diagnostics)\n"), len(ds))
		return 4
	}

	target, err := pickWorkflow(doc, wfName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermes explain: %v\n", err)
		return 4
	}

	if asJSON {
		// 机器可读：字段名与 IR 的 bounds 保持一致，工具不用另记一套。
		b := target.Bounds
		out := map[string]any{
			"workflow":        target.Name,
			"is_entry":        target.IsEntry,
			"unbounded":       b.Unbounded,
			"max_steps":       b.MaxSteps,
			"max_spans":       b.MaxSpans,
			"max_model_calls": b.MaxModelCalls,
			"max_tool_calls":  b.MaxToolCalls,
			"max_concurrency": b.MaxConcurrency,
			"nodes":           len(target.Nodes),
			"vars":            len(target.Vars),
		}
		blob, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "hermes explain: %v\n", err)
			return 4
		}
		fmt.Println(string(blob))
		return 0
	}
	fmt.Printf("%s  %s\n", target.Name, unitSuffix(doc, target))
	fmt.Printf("  %-18s %s\n", "returns", typeName(target.Returns))
	if target.Input != nil {
		fmt.Printf("  %-18s %s\n", "input", typeName(*target.Input))
	}
	fmt.Printf("  %-18s %s\n", "backend", orDash(target.Backend))
	fmt.Printf("  %-18s %d\n", "nodes", len(target.Nodes))
	fmt.Printf("  %-18s %d\n", "vars", len(target.Vars))
	fmt.Println(tr("  bounds (upper bounds, not expectations):"))
	b := target.Bounds
	if b.Unbounded {
		// unbounded 时两个"次数"类上界都只是占位：递归函数的调用次数与
		// 未知循环的迭代次数都定不了。把它们显示成数字等于给出一个假承诺。
		fmt.Printf("    %-16s %s\n", "max_steps", tr("unknown (a loop or a function call whose cost is not known at compile time)"))
		fmt.Printf("    %-16s %s\n", "max_spans", tr("unknown (a loop or a function call whose cost is not known at compile time)"))
	} else {
		fmt.Printf("    %-16s %d\n", "max_steps", b.MaxSteps)
		fmt.Printf("    %-16s %d\n", "max_spans", b.MaxSpans)
	}
	fmt.Printf("    %-16s %d\n", "max_concurrency", b.MaxConcurrency)
	fmt.Printf("    %-16s %d\n", "max_model_calls", b.MaxModelCalls)
	fmt.Printf("    %-16s %d\n", "max_tool_calls", b.MaxToolCalls)

	if len(target.InterpDeps) > 0 {
		fmt.Println(tr("  interp_deps (the interpretive semantics of these nodes is hashed; changing the evaluator requires regeneration):"))
		for _, d := range target.InterpDeps {
			paths := make([]string, 0, len(d.Paths))
			for _, p := range d.Paths {
				paths = append(paths, strings.Join(p, "."))
			}
			fmt.Printf("    node %-10d %s\n", d.Node, strings.Join(paths, ", "))
		}
	}
	if len(target.Economics) > 0 && string(target.Economics) != "{}" && string(target.Economics) != "null" {
		fmt.Println("  economics:")
		var m map[string]any
		// **UseNumber**：默认解码把 1048576 变成 1.048576e+06，
		// 而这些是钱与 token 的上界 —— 用科学计数法显示等于让人自己去换算。
		dec := json.NewDecoder(strings.NewReader(string(target.Economics)))
		dec.UseNumber()
		if err := dec.Decode(&m); err == nil {
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Printf("    %-16s %v\n", k, m[k])
			}
		}
	}
	return 0
}

func pickWorkflow(doc *ir.Doc, name string) (*ir.Workflow, error) {
	if name == "" {
		for i := range doc.Workflows {
			if doc.Workflows[i].IsEntry {
				return &doc.Workflows[i], nil
			}
		}
		return nil, fmt.Errorf(tr("no entry workflow; use --workflow <name>"))
	}
	for i := range doc.Workflows {
		if doc.Workflows[i].Name == name {
			return &doc.Workflows[i], nil
		}
	}
	names := make([]string, 0, len(doc.Workflows))
	for i := range doc.Workflows {
		names = append(names, doc.Workflows[i].Name)
	}
	sort.Strings(names)
	return nil, fmt.Errorf(tr("no workflow named %q (available: %s)"), name, strings.Join(names, ", "))
}

func unitSuffix(doc *ir.Doc, w *ir.Workflow) string {
	tags := make([]string, 0, 3)
	if w.IsEntry {
		tags = append(tags, "entry")
	}
	if w.Exported {
		tags = append(tags, "exported")
	}
	if doc.Unit.Name != "" {
		tags = append(tags, "unit="+doc.Unit.Name)
	}
	return "[" + strings.Join(tags, " ") + "]"
}

func typeName(t ir.TypeExpr) string {
	name := t.K
	if t.K == "struct" && t.ID != nil {
		name = fmt.Sprintf("struct#%d", *t.ID)
	}
	for len(t.Of) > 0 {
		var inner ir.TypeExpr
		if err := json.Unmarshal(t.Of, &inner); err != nil {
			break
		}
		name = name + "<" + typeName(inner) + ">"
		break
	}
	return name
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

const explainUsage = `hermes explain - print a workflow contract and its cost upper bounds (T36)

Usage:
  hermes explain <unit.ir.json | unit.hbc> [--workflow <name>]

Notes:
  The bounds are UPPER bounds, not expectations: max_steps / max_spans /
  max_concurrency / max_model_calls / max_tool_calls. They also drive stripping and
  trace limits, so a wrong bound is a correctness problem, not just a display one.

Exit codes:
  0  success
  2  usage error
  4  unreadable / invalid IR / unknown workflow
`
