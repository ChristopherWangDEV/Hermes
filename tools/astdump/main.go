// astdump 打印一份 .hermes/.h 的确定性 AST dump。
//
// 为什么需要它：本项目的反复教训是"先看实际 AST，不要猜字段"——Pattern 的名字在
// child 不在 text、EnvExpr 的默认值在 args 不在 child->next、TryStmt 的 catch 体挂在
// else_branch 上。猜了三次全错，一次 dump 就定位了。所以给它一个一行命令的入口。
//
// 纯开发工具，**不进分发物**，零第三方依赖（只用标准库）。
//
//	go run ./tools/astdump examples/support_bot/support.hermes
//	go run ./tools/astdump --header examples/support_bot/support.h
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"hermes/internal/native"
)

func main() {
	header := flag.Bool("header", false, "按 .h（只有声明、无体）模式解析")
	count := flag.Bool("count", false, "只打印节点数")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "用法: astdump [--header] [--count] <file.hermes|file.h>\n")
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}

	path := flag.Arg(0)
	src, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读不到 %s: %v\n", path, err)
		os.Exit(1)
	}

	if *count {
		n, err := native.ParseCount(src, filepath.Base(path), *header)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		fmt.Println(n)
		return
	}

	r, err := native.Parse(src, filepath.Base(path), *header)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	fmt.Print(r.Dump)
	if r.Diag != "" && r.Diag != "[]" {
		fmt.Fprintf(os.Stderr, "\n--- 诊断 ---\n%s\n", r.Diag)
	}
}
