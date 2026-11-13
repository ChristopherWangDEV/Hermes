package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"hermes/internal/demo"
	"hermes/internal/engine"
)

// demo 子命令：一条命令跑通"编译 → 执行 → 出原生二进制"（T50）。
//
// 它存在的理由是**第一次体验**：用户装完 Hermes，先敲 `hermes demo`，
// 应该在一屏里看到"这东西能干什么"，而不是先去读文档、找示例、配 API key。
// 所以演示**不依赖网络**：没有模型、没有工具调用，只有编译器和运行时。
func cmdDemo(args []string) int {
	keep := false
	for _, a := range args {
		switch a {
		case "-h", "--help", "help":
			fmt.Print(demoUsage)
			return 0
		case "--keep":
			keep = true
		}
	}

	dir, err := os.MkdirTemp("", "hermes-demo-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermes demo: %v\n", err)
		return 4
	}
	if !keep {
		defer os.RemoveAll(dir)
	} else {
		fmt.Printf(tr("demo directory (--keep): %s\n"), dir)
	}

	src := filepath.Join(dir, demo.Name)
	if err := os.WriteFile(src, demo.Source, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "hermes demo: %v\n", err)
		return 4
	}

	fmt.Println(tr("1. source -> IR"))
	doc, err := buildIR(src, demo.Source)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermes demo: %v\n", err)
		return 4
	}
	fmt.Printf(tr("   workflow %q, %d nodes, upper bound max_steps=%d\n"),
		doc.Workflows[0].Name, len(doc.Workflows[0].Nodes), doc.Workflows[0].Bounds.MaxSteps)

	fmt.Println(tr("2. IR -> run"))
	res, err := engine.RunWith(doc, "", demo.Input, engine.RunOptions{RunID: "demo"})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermes demo: %s: %v\n", tr("run failed"), err)
		return 4
	}
	fmt.Printf(tr("   input  %s\n"), jsonOneLine(demo.Input))
	fmt.Printf(tr("   output %v\n"), res.Value)

	fmt.Println(tr("3. trace (what each step cost)"))
	failed := 0
	for _, sp := range res.Spans {
		if sp.Status == "failed" {
			failed++
		}
	}
	fmt.Printf(tr("   %d spans, %d failed\n"), len(res.Spans), failed)

	fmt.Println(tr("4. same source -> a native executable"))
	bin := filepath.Join(dir, "demo")
	if code := cmdCompile([]string{src, "-o", bin}); code != 0 {
		fmt.Println(tr("   (skipped: needs clang; `hermes doctor` explains)"))
		return 0
	}
	// 产物的输入走 argv[1]（JSON），与生成的 main 的约定一致。
	out, runErr := exec.Command(bin, jsonOneLine(demo.Input)).Output()
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "hermes demo: %s: %v\n", tr("running the built artifact failed"), runErr)
		return 4
	}
	fmt.Printf("   %s → %s", bin, out)
	fmt.Println()
	fmt.Println()
	fmt.Println(tr("done. this executable does not depend on an installed hermes."))
	return 0
}

func jsonOneLine(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

const demoUsage = `hermes demo - see the whole chain in one command

Usage:
  hermes demo [--keep]

What it does:
  1. compile the embedded .hermes source to IR
  2. execute it (no network, no API key)
  3. show the trace it produced
  4. compile the same source into a native executable and run it

Notes:
  The demo source is embedded in the binary, so it works from a release package
  where no repository exists. --keep leaves the temporary directory for inspection.

Exit codes:
  0  demo completed
  4  something in the chain failed
`
