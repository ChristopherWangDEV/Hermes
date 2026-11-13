// 本文件守的是 **D-027：三层分工定稿**。
//
//	Go   —— 编排引擎：DAG 调度、状态机、熔断、预算记账、Trace、CLI。**不碰 syscall 细节**
//	C/C++—— 执行器/督导：posix_spawn、rlimit、硬超时、进程组强杀、沙箱、循环级 deadline。**不做调度决策**
//	Rust —— 原生扩展：FFI handle 表、敏感缓冲、零拷贝视图、哈希链。**不做编排、不做记账**
//
// 为什么这些边界需要守卫：三条线**都不会在编译期报错**。Go 里写一行 syscall.Kill 能编过，
// Rust 里加个"顺手调度一下"的函数也能编过。越界之后唯一的症状是"以后没人说得清这段
// 逻辑该测谁"，而跨语言故障面长出来就收不回去。
package guard_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestDivisionDecisionIsRecorded 先证明裁决本身存在。
//
// 没有这条，下面的守卫会变成"两条孤立的技术检查"——将来有人删掉 D-027 的正文，
// 守卫仍然绿着，而它守的东西已经没人知道是什么了。
func TestDivisionDecisionIsRecorded(t *testing.T) {
	root := repoRoot(t)
	// DECISIONS.md 是**内部文档**，已移出公开仓库（.internal/docs/）。
	// 本地有就验，没有就跳过 —— 公开仓库里不该为了这条守卫再放一份内部决策记录。
	data, err := os.ReadFile(filepath.Join(root, "docs/DECISIONS.md"))
	if err != nil {
		t.Skip("内部文档 DECISIONS.md 不在仓库里")
	}
	text := string(data)
	if !strings.Contains(text, "## D-027") {
		t.Fatal("DECISIONS.md 里没有 D-027（三层分工定稿）——守卫失去了它守的那条裁决")
	}
	for _, layer := range []string{"编排引擎", "执行器 / 督导", "原生扩展"} {
		if !strings.Contains(text, layer) {
			t.Errorf("D-027 里缺少「%s」这一层", layer)
		}
	}
}

// TestGoDoesNotTouchSyscalls 守 Go 层的"不做什么"。
//
// 判据是**import 与选择子**，不是文本搜索：注释里写 "syscall" 是允许的
// （解释为什么不碰它，本来就要提它），真正越界的只有两种形态。
func TestGoDoesNotTouchSyscalls(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()

	var offenders []string
	for _, path := range walkGoFiles(t, root) {
		rel, _ := filepath.Rel(root, path)

		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("解析 %s 失败: %v", rel, err)
		}

		for _, spec := range f.Imports {
			p, uerr := strconv.Unquote(spec.Path.Value)
			if uerr != nil {
				continue
			}
			if p == "syscall" || strings.HasPrefix(p, "golang.org/x/sys") {
				offenders = append(offenders, rel+" → import "+p)
			}
		}

		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "syscall" {
				offenders = append(offenders, rel+" → syscall."+sel.Sel.Name)
			}
			return true
		})
	}
	if len(offenders) == 0 {
		return
	}
	t.Fatalf("Go 层碰了 syscall 细节（D-027：Go 是编排引擎，不碰 syscall）：\n  %s\n\n"+
		"这不是洁癖：一旦 Go 直接 syscall，跨语言故障面就从\"C 负责的执行细节\"扩到两处，\n"+
		"而 syscall 的语义在三个平台上并不一致——那正是 C 层存在的理由。",
		strings.Join(offenders, "\n  "))
}

// forbiddenRustTokens 是编排/记账层才该出现的概念。Rust 只做内存安全与密码学。
var forbiddenRustTokens = []string{
	"budget", "breaker", "schedul", "workflow", "ledger", "max_par", "dag",
}

// TestRustStaysAtMemoryAndCrypto 守 Rust 层的"不做什么"。
//
// 只扫**非注释行**：注释里解释"记账为什么不在 Rust"是应该的，那不是在记账。
// 这条守卫第一次跑就抓到过自己——如果一个扫描器不区分注释，它抓到的第一条
// 一定是文档。
func TestRustStaysAtMemoryAndCrypto(t *testing.T) {
	root := repoRoot(t)
	rustSrc := filepath.Join(root, "native/rust/src")
	if _, err := os.Stat(rustSrc); err != nil {
		t.Fatalf("找不到 Rust 源码目录: %v", err)
	}

	var offenders []string
	err := filepath.Walk(rustSrc, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".rs") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			code := strings.TrimSpace(line)
			if code == "" || strings.HasPrefix(code, "//") {
				continue
			}
			lower := strings.ToLower(code)
			for _, tok := range forbiddenRustTokens {
				if strings.Contains(lower, tok) {
					offenders = append(offenders,
						rel+":"+strconv.Itoa(i+1)+" → "+tok)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 Rust 源码失败: %v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("Rust 层出现了编排/记账概念（D-027：Rust 只做内存与密码学）：\n  %s",
			strings.Join(offenders, "\n  "))
	}
}
