package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"hermes/internal/trace"
)

// trace 子命令族。两条命令都**只读一个文件**：
//   - 不触网（R-TR-05：排查时不该还要连上游）
//   - 不读源码（trace 必须自足，否则拿去给别人看就没法自证）
//
// 退出码沿用全局约定：0 成功 / 2 用法错 / 4 运行失败（含"被篡改"）。
func cmdTrace(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, traceUsage)
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "verify":
		return cmdTraceVerify(rest)
	case "summary":
		return cmdTraceSummary(rest)
	case "diff":
		return cmdTraceDiff(rest)
	case "-h", "--help", "help":
		fmt.Print(traceUsage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "hermes trace: %s %q\n\n%s", tr("unknown subcommand"), sub, traceUsage)
		return 2
	}
}

var traceUsage = pickUsage(`hermes trace - answer "which step failed, what it cost, how many calls" from one file

Usage:
  hermes trace verify  <trace.json> [--json]
  hermes trace summary <trace.json> [--json]
  hermes trace diff    <a.json> <b.json> [--json]

Notes:
  verify   recompute the hash chain, proving the trace was not edited; names the block if it was
  summary  aggregate failed steps, attribution, cost and call counts

Exit codes:
  0  passed
  2  usage error
  4  trace tampered or unparseable
`,
	`hermes trace —— 单文件回答"哪步失败、花多少、调了几次"

用法:
  hermes trace verify  <trace.json> [--json]
  hermes trace summary <trace.json> [--json]
  hermes trace diff    <a.json> <b.json> [--json]

说明:
  verify   重算哈希链，证明这份 trace 没被改过；改过则指出第 k 块
  summary  聚合失败步骤、归因、花费与调用次数

退出码:
  0  通过
  2  用法错误
  4  trace 被篡改或无法解析
`)

func readTrace(path string) ([]byte, int) {
	if path == "" {
		fmt.Fprint(os.Stderr, tr("hermes trace: missing trace file path\n"))
		return nil, 2
	}
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermes trace: %s %s: %v\n", tr("cannot read"), path, err)
		return nil, 4
	}
	return data, 0
}

// splitArgs 手工分离 `--json` 与位置参数。
//
// 不用 flag 包：Go 的 flag 在遇到第一个非 flag 参数时就停止解析，
// 于是 `trace summary trace.json --json` 会把 --json 当成第二个文件路径。
// 使用者不会记住这种顺序约定，命令行也不该要求他记。
func splitArgs(args []string) (pos []string, asJSON bool, bad bool) {
	for _, a := range args {
		switch a {
		case "--json", "-json":
			asJSON = true
		case "-h", "--help", "help":
			bad = true
			return
		default:
			pos = append(pos, a)
		}
	}
	return
}

func cmdTraceVerify(args []string) int {
	pos, asJSON, help := splitArgs(args)
	if help {
		fmt.Print(traceUsage)
		return 0
	}
	if len(pos) != 1 {
		fmt.Fprint(os.Stderr, tr("hermes trace verify: needs exactly one file path\n"))
		return 2
	}
	data, code := readTrace(pos[0])
	if data == nil {
		return code
	}

	info, err := trace.Verify(data)
	if err != nil {
		var m *trace.Mismatch
		if errors.As(err, &m) {
			// 篡改是**结论**，不是内部错误：退出码 4，并明确指出第几块。
			fmt.Fprintf(os.Stderr, "FAIL: %s\n", err)
			if asJSON {
				_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
					"verified": false,
					"block":    m.Block,
					"want":     m.Want,
					"got":      m.Got,
					"detail":   m.Detail,
				})
			}
			return 4
		}
		fmt.Fprintf(os.Stderr, "hermes trace: %v\n", err)
		return 4
	}

	if asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"verified":  true,
			"root":      info.Root,
			"blocks":    info.Blocks,
			"total_len": info.TotalLen,
			"truncated": info.Truncated,
		})
		return 0
	}
	fmt.Printf("OK\n")
	fmt.Printf("  root      %s\n", info.Root)
	fmt.Printf("  blocks    %d\n", info.Blocks)
	fmt.Printf("  total_len %d\n", info.TotalLen)
	if info.Truncated {
		fmt.Printf(tr("  truncated       true (this trace was cut by max_bytes: incomplete, but not tampered with)\n"))
	}
	return 0
}

func cmdTraceSummary(args []string) int {
	pos, asJSON, help := splitArgs(args)
	if help {
		fmt.Print(traceUsage)
		return 0
	}
	if len(pos) != 1 {
		fmt.Fprint(os.Stderr, tr("hermes trace summary: needs exactly one file path\n"))
		return 2
	}
	data, code := readTrace(pos[0])
	if data == nil {
		return code
	}

	s, err := trace.Summarize(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermes trace: %v\n", err)
		return 4
	}
	if asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(s)
		return 0
	}

	fmt.Printf("run        %s\n", s.RunID)
	fmt.Printf("spans      %d\n", s.Spans)
	if s.FailedStep != "" {
		fmt.Printf("failed_at  %s (span %s)\n", s.FailedStep, s.FailedSpan)
		fmt.Printf("attribution %s\n", s.Attribution)
	} else {
		fmt.Printf(tr("failed_at  - (no failed step)\n"))
	}
	fmt.Printf("calls      provider=%d tool=%d\n", s.Calls, s.Tools)
	fmt.Printf("tokens     in=%d out=%d\n", s.TokensIn, s.TokensOut)
	fmt.Printf("cost       %d micro-USD\n", s.MicroUSD)
	if s.Truncated {
		fmt.Printf("truncated  true\n")
	}
	return 0
}

// cmdTraceDiff：比较两次运行的 trace。
//
// 用途很具体："上次好的、这次坏的，差在哪一步"。所以按 step_id 对齐，
// 并把 token/成本的差值一并给出 —— 只看状态不看成本会漏掉"没报错但贵了十倍"。
func cmdTraceDiff(args []string) int {
	pos, asJSON, help := splitArgs(args)
	if help {
		fmt.Print(traceUsage)
		return 0
	}
	if len(pos) != 2 {
		fmt.Fprint(os.Stderr, tr("hermes trace diff: needs exactly two trace files\n"))
		return 2
	}
	var spans [2][]trace.Span
	for i, p := range pos {
		data, code := readTrace(p)
		if data == nil {
			return code
		}
		t, err := trace.Parse(data)
		if err != nil {
			fmt.Fprintf(os.Stderr, tr("hermes trace diff: %s: %v\n"), p, err)
			return 4
		}
		spans[i] = t.Spans
	}

	d := trace.Diff(spans[0], spans[1])
	if asJSON {
		out, err := json.MarshalIndent(d, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "hermes trace diff: %v\n", err)
			return 4
		}
		fmt.Println(string(out))
		return 0
	}

	for _, id := range d.OnlyLeft {
		fmt.Printf("- %s\n", id)
	}
	for _, id := range d.OnlyRight {
		fmt.Printf("+ %s\n", id)
	}
	for _, c := range d.Changed {
		fmt.Printf("~ %s: %s -> %s\n", c.StepID, c.Want, c.Got)
	}
	fmt.Printf("tokens %+d, cost micro_usd %+d, failed %d -> %d\n",
		d.TokenDelta, d.CostDelta, d.FailedLeft, d.FailedRight)
	return 0
}
