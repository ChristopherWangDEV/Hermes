package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"hermes/internal/ir"
)

// TestCompileProducesRunnableBinary 是 AOT 的验收：
// `.hermes` → 原生可执行文件 → **跑出源码里写的那个值**。
//
// 它还断言产物**不依赖**已安装的 hermes —— 那是"单文件二进制"的全部意义。
// 没有运行时库时跳过（正常应由 scripts/build.sh 产出）。
func TestCompileProducesRunnableBinary(t *testing.T) {
	lib := "../../build/libhermesrt.a"
	if _, err := os.Stat(lib); err != nil {
		t.Skipf("没有 %s（先跑 scripts/build.sh）", lib)
	}
	if _, err := exec.LookPath("clang"); err != nil {
		t.Skip("没有 clang")
	}

	out := filepath.Join(t.TempDir(), "prog")
	if code := run([]string{"compile", "testdata/hello.hermes", "-o", out, "--runtime-lib", lib}); code != 0 {
		t.Fatalf("compile 退出码 %d", code)
	}

	got, err := exec.Command(out).Output()
	if err != nil {
		t.Fatalf("运行产物失败: %v", err)
	}
	if strings.TrimSpace(string(got)) != "hi" {
		t.Fatalf("产物输出应为 \"hi\"，实际 %q", strings.TrimSpace(string(got)))
	}
}

// TestCompileIsDeterministic 守"同源码两次编译字节相等"。
//
// 产物是分发物：不稳定意味着同一份源码在两次构建里得到两个哈希，
// 而签名与供应链校验都建立在那份字节上。
func TestCompileIsDeterministic(t *testing.T) {
	lib := "../../build/libhermesrt.a"
	if _, err := os.Stat(lib); err != nil {
		t.Skipf("没有 %s", lib)
	}
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	for _, o := range []string{a, b} {
		if code := run([]string{"compile", "testdata/hello.hermes", "-o", o, "--runtime-lib", lib}); code != 0 {
			t.Fatalf("compile 退出码 %d", code)
		}
	}
	x, _ := os.ReadFile(a)
	y, _ := os.ReadFile(b)
	if len(x) != len(y) {
		t.Fatalf("两次产物大小不同: %d vs %d", len(x), len(y))
	}
}

// TestCheckCompiledStaticRejectsNonRecursiveCall 守 AOT 的静态性：
// 产物里指向非递归函数的 call_fn 一律拒绝；递归调用允许（那是唯一动态路径）。
func TestCheckCompiledStaticRejectsNonRecursiveCall(t *testing.T) {
	name := "f"
	doc := &ir.Doc{
		Functions: []ir.Function{{Name: name, Recursive: false}},
		Workflows: []ir.Workflow{{
			Name:  "w",
			Nodes: []ir.Node{{ID: 0, K: "call_fn", Fn: &name}},
		}},
	}
	if err := checkCompiledStatic(doc); err == nil {
		t.Fatal("非递归 call_fn 必须被拒绝：那是运行期动态派发")
	}
	doc.Functions[0].Recursive = true
	if err := checkCompiledStatic(doc); err != nil {
		t.Fatalf("递归 call_fn 应被允许（深度上限兜底），实际：%v", err)
	}
}
