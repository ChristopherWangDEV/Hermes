package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestNativeDriverMatchesSubcommands 守 clang 风格入口与子命令**走同一套实现**：
// `hermes x.hermes -c` 的产物必须与 `hermes build x.hermes` 逐字节相同，
// 链接路径的产物必须能跑。
//
// 为什么比字节而不是比"都成功"：driver 一旦自己拼一遍编译流程，两份实现就会
// 各自漂移——而漂移的第一个症状就是产物不同。
func TestNativeDriverMatchesSubcommands(t *testing.T) {
	root := repoRootForBackend(t)
	bin := filepath.Join(root, "build", "hermes")
	dir := t.TempDir()
	src := filepath.Join(dir, "greet.hermes")
	body, err := os.ReadFile(filepath.Join(root, "examples", "functions", "greet.hermes"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatal(err)
	}

	// -c 与 build 的产物必须相同
	a := filepath.Join(dir, "a.hbc")
	b := filepath.Join(dir, "b.hbc")
	if out, err := exec.Command(bin, src, "-c", "-o", a).CombinedOutput(); err != nil {
		t.Fatalf("driver -c 失败: %v\n%s", err, out)
	}
	if out, err := exec.Command(bin, "build", src, "-o", b).CombinedOutput(); err != nil {
		t.Fatalf("build 失败: %v\n%s", err, out)
	}
	ab, err := os.ReadFile(a)
	if err != nil {
		t.Fatal(err)
	}
	bb, err := os.ReadFile(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(ab) != string(bb) {
		t.Fatal("driver -c 的产物与 build 不同：两条路径已经开始漂移")
	}

	// 链接路径：产物必须能跑
	prog := filepath.Join(dir, "app")
	if out, err := exec.Command(bin, src, "-o", prog).CombinedOutput(); err != nil {
		t.Fatalf("driver 链接失败: %v\n%s", err, out)
	}
	got, err := exec.Command(prog).Output()
	if err != nil {
		t.Fatalf("产物跑不起来: %v", err)
	}
	if !strings.Contains(string(got), "same") {
		t.Fatalf("产物输出不对: %s", got)
	}

	// -E：展开结果必须带行标记，且只打印、不产生产物
	res, err := exec.Command(bin, src, "-E").Output()
	if err != nil {
		t.Fatalf("driver -E 失败: %v", err)
	}
	if !strings.HasPrefix(string(res), "# ") {
		t.Fatalf("-E 输出缺少行标记: %.80s", res)
	}
}
