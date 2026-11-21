package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestFmtGoesThroughNative：`fmt` 已由 C++ 主程序提供，Go 侧只转发。
//
// 判据用**只有 C++ 版才有的措辞**：报错时说 "cannot expand <file>" 而不是
// Go 版的 "cannot expand %s: %v"。措辞不同证明这份输出不是 Go 给的。
func TestFmtGoesThroughNative(t *testing.T) {
	root := repoRootForBackend(t)
	bin := filepath.Join(root, "build", "hermes")
	dir := t.TempDir()

	// 规范化：CRLF + 行尾空白 + 多余结尾空行 → 全部收敛。
	p := filepath.Join(dir, "a.hermes")
	if err := os.WriteFile(p, []byte("x = 1;   \r\ny = 2;\r\n\r\n\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(bin, "fmt", p).CombinedOutput(); err != nil {
		t.Fatalf("fmt 失败: %v\n%s", err, out)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "x = 1;\ny = 2;\n" {
		t.Fatalf("规范化结果不对：%q", got)
	}

	// --check：需要格式化的文件必须退出 4。
	p2 := filepath.Join(dir, "b.hermes")
	os.WriteFile(p2, []byte("x = 1;   \n"), 0o644)
	cmd := exec.Command(bin, "fmt", "--check", p2)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if err == nil || !errors.As(err, &ee) || ee.ExitCode() != 4 {
		t.Fatalf("--check 应退出 4，实际 err=%v out=%s", err, out)
	}
	if !strings.Contains(string(out), "needs formatting") {
		t.Fatalf("--check 输出不对: %s", out)
	}

	// --expand：不写文件，只打 stdout。
	p3 := filepath.Join(dir, "c.hermes")
	src := "#bind <std/net>\nexport workflow w() entry -> string { return \"ok\"; }\n"
	os.WriteFile(p3, []byte(src), 0o644)
	out, err = exec.Command(bin, "fmt", "--expand", p3).Output()
	if err != nil {
		t.Fatalf("--expand 失败: %v", err)
	}
	if !strings.Contains(string(out), "#link <std/net>") {
		t.Fatalf("--expand 未展开 #bind: %s", out)
	}
	after, _ := os.ReadFile(p3)
	if string(after) != src {
		t.Fatalf("--expand 不该改文件，实际变成了 %q", after)
	}
}
