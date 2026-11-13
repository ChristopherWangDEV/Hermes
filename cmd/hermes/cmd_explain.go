package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"hermes/internal/archive"
	"hermes/internal/diag"
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
	// explainCode 非空表示本命令在"解释一个诊断码"（rustc --explain 等价物），
	// 此时不走 IR / .hbc 文件路径。
	explainCode := ""
	// colorSet 记下用户是否显式要了颜色。IR 契约路径是纯文本表格、没有上色实现，
	// 所以带了 --color 却在解释 IR 时必须报错 —— 静默吞掉一个开关，
	// 会让用户以为它生效了，下次照着写进 CI 也不会有人发现它一直没作用。
	colorSet := false
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
		case "--color":
			colorSet = true
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, tr("hermes explain: --color needs auto|always|never\n\n")+explainUsage)
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
				fmt.Fprintf(os.Stderr, tr("hermes explain: unknown --color %q (want auto|always|never)\n\n%s"), args[i+1], explainUsage)
				return 2
			}
			i++
		case "--color=auto":
			colorSet = true
			diagColor = diag.ColorAuto
		case "--color=always":
			colorSet = true
			diagColor = diag.ColorAlways
		case "--color=never":
			colorSet = true
			diagColor = diag.ColorNever
		case "-h", "--help", "help":
			fmt.Print(explainUsage)
			return 0
		default:
			if isDiagCode(a) {
				explainCode = a
				continue
			}
			if irPath == "" && a != "" && a[0] != '-' {
				irPath = a
				continue
			}
			fmt.Fprintf(os.Stderr, tr("hermes explain: unknown argument %q\n\n%s"), a, explainUsage)
			return 2
		}
	}
	if explainCode != "" {
		return runExplainCode(explainCode, asJSON)
	}
	if irPath == "" {
		fmt.Fprint(os.Stderr, tr("hermes explain: missing IR / .hbc / DIAG_CODE\n\n")+explainUsage)
		return 2
	}
	if colorSet {
		// 见 colorSet 的注释：宁可报错，也不要让一个没作用的开关看起来生效了。
		fmt.Fprint(os.Stderr, tr("hermes explain: --color only applies to a DIAG_CODE; an IR contract is printed as plain text\n\n")+explainUsage)
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
		//
		// `unbounded` 说"这几个数字不是上界"，但它**不区分原因**：while 的迭代次数、
		// 递归函数的调用深度、`max_steps = unlimited` 的 agent 步数是三件不同的事。
		// 机器要做判断就得知道是哪一件，所以另给一个精确信号
		// （agent_steps_unbounded），而不是让它去解析 unbounded 旁边的散文。
		b := target.Bounds
		out := map[string]any{
			"workflow":              target.Name,
			"is_entry":              target.IsEntry,
			"unbounded":             b.Unbounded,
			"agent_steps_unbounded": agentStepsUnbounded(target),
			"max_steps":             b.MaxSteps,
			"max_spans":             b.MaxSpans,
			"max_model_calls":       b.MaxModelCalls,
			"max_tool_calls":        b.MaxToolCalls,
			"max_concurrency":       b.MaxConcurrency,
			"nodes":                 len(target.Nodes),
			"vars":                  len(target.Vars),
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
	// 「为什么不是数字」只讲一次。
	//
	// 以前两个字段各重复同一句 "unknown (a loop or a function call whose cost is
	// not known at compile time)"，讲了**两遍**（其实就是一句），而真正新增的
	// 第三种原因（max_steps = unlimited）根本没提 —— 于是 unlimited 的产物
	// 看起来像是"有个循环"，用户会去图里找一个并不存在的循环。
	// 现在：一行 unbounded 给理由（三种可能都列出来），下面字段只答"是多少"。
	if b.Unbounded {
		fmt.Printf("    %-16s %s\n", "unbounded",
			tr("yes (cost not statically bounded: a loop, a recursive call, or max_steps = unlimited)"))
	}
	switch {
	case agentStepsUnbounded(target):
		// 单独一行点名 unlimited：它的后果最多（max_spans 不再是 trace 上界、
		// 预算无法预留、只剩运行期硬上限），不能和"有个 while"共用一句话。
		fmt.Printf("    %-16s %s\n", "max_steps",
			tr("unlimited (no compile-time bound; the runtime step cap is the only gate)"))
	case b.Unbounded:
		fmt.Printf("    %-16s %s\n", "max_steps", tr("unknown"))
	default:
		fmt.Printf("    %-16s %d\n", "max_steps", b.MaxSteps)
	}
	if b.Unbounded {
		fmt.Printf("    %-16s %s\n", "max_spans", tr("unknown"))
	} else {
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

// runExplainCode 实现 `hermes explain <DIAG_CODE>`（rustc --explain 等价物）。
//
// 把一个诊断码的长说明（Text/Why/Fix）连同文档锚点（docs）打印出来。
// 找不到该码时返回 4，并提示：这类码由编译器在出错时内联给出 why/fix，
// 直接重跑构建即可看到，或查阅 docs/LANGUAGE.md。
func runExplainCode(code string, asJSON bool) int {
	// 走 diag.Explain 而不是 diag.Lookup：explain 没有实例值可填，
	// 模板里的 {id} 必须变成元变量 <id>，否则头行会印出 "{what} points at {id}"。
	text, why, fixes, docs, ok := diag.Explain(code, diag.LangFromEnv())
	if !ok {
		fmt.Fprintf(os.Stderr, tr("hermes explain: no explanation registered for %s\n"), code)
		fmt.Fprint(os.Stderr, tr("  this diagnostic is produced by the compiler with an inline why/fix; re-run the build to see it, or consult docs/LANGUAGE.md\n"))
		return 4
	}
	if asJSON {
		out := map[string]any{
			"code":    code,
			"message": text,
			"why":     why,
			"fixes":   fixes,
			"docs":    docs,
		}
		blob, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "hermes explain: %v\n", err)
			return 4
		}
		fmt.Println(string(blob))
		return 0
	}
	body := diag.RenderExplain(code, text, why, fixes, diagColor, os.Stdout)
	if docs != "" {
		body += "  docs: " + docs + "\n"
	}
	fmt.Print(body)
	return 0
}

// isDiagCode 判断参数是否像一个诊断码：E3002 / W3013（大写字母 + 4 位数字），
// 或 E_IR_DUP_ID / W_IR_*（语义码）。用于区分"解释一个诊断码"与"解释一个 IR 文件"。
func isDiagCode(s string) bool {
	if len(s) < 2 || (s[0] != 'E' && s[0] != 'W') {
		return false
	}
	if len(s) == 5 && s[1] >= '0' && s[1] <= '9' {
		for i := 1; i < len(s); i++ {
			if s[i] < '0' || s[i] > '9' {
				return false
			}
		}
		return true
	}
	if len(s) > 3 && s[1] == '_' && (strings.HasPrefix(s, "E_IR_") || strings.HasPrefix(s, "W_IR_")) {
		return true
	}
	return false
}

// agentStepsUnbounded 报告这个 workflow 里是否存在**没有编译期步数上界**的
// agent 节点（源码里写的 `max_steps = unlimited`）。
//
// 为什么需要它：bounds.unbounded 只说明"这几个数字不是上界"，不说明为什么。
// 而 unlimited 的后果与"有个 while 循环"完全不同 —— 后者至少要循环条件收敛，
// 前者连"循环多少次"都没有声明。explain 的价值就在于说清这个差别。
//
// nil 与 ≤0 都算无上界：nil 是正常产物的形状（键被省略），≤0 只可能来自
// 手改的 IR，它同样不是上界。
func agentStepsUnbounded(w *ir.Workflow) bool {
	for i := range w.Nodes {
		n := &w.Nodes[i]
		if n.K != "agent" || n.Agent == nil {
			continue
		}
		if n.Agent.MaxSteps == nil || *n.Agent.MaxSteps <= 0 {
			return true
		}
	}
	return false
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

const explainUsage = `hermes explain - print a workflow contract, its cost upper bounds (T36), or a long-form diagnostic explanation (rustc --explain equivalent)

Usage:
  hermes explain <unit.ir.json | unit.hbc> [--workflow <name>]   explain a workflow's contract
  hermes explain <DIAG_CODE> [--json] [--color ...]              explain a diagnostic code (E3002, E_IR_DUP_ID, ...)

Notes:
  The bounds are UPPER bounds, not expectations: max_steps / max_spans /
  max_concurrency / max_model_calls / max_tool_calls. They also drive stripping and
  trace limits, so a wrong bound is a correctness problem, not just a display one.

  When a cost cannot be statically bounded the row says "unknown" instead of a
  number, and an "unbounded" row explains why: a loop, a recursive call, or
  max_steps = unlimited. For an unlimited agent the runtime still enforces a
  hard iteration cap — that number is deliberately NOT in the IR, so it is not
  printed here as if it were one of the declarable bounds.

  A DIAG_CODE argument (e.g. E3002 or E_IR_DANGLING_REF) prints the long-form
  explanation registered for that code, plus a docs anchor. Codes not yet in the
  catalog fall back to the inline why/fix the compiler prints when the error occurs.

Exit codes:
  0  success
  2  usage error
  4  unreadable / invalid IR / unknown workflow / unknown diagnostic code
`
