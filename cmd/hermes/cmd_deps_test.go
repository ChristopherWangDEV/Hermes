package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestDepsResolveShowsBindForms 是 `deps --resolve` 的判据：
// 三种 #bind 形态都要出现在解析结果里，并且**分类正确**。
//
// 为什么这条重要：`#bind models` 与 `#bind <std/net>` 在"编译成功"时看不出
// 区别，只有把解析结果摊开，用户才知道自己链的是什么、有没有解析到。
func TestDepsResolveShowsBindForms(t *testing.T) {
	root := repoRootForBackend(t)
	bin := filepath.Join(root, "build", "hermes")
	dir := t.TempDir()
	p := filepath.Join(dir, "d.hermes")
	src := "#bind <std/net>\n" +
		"#bind native m \"libm.a\" via \"m.h\"\n" +
		"#bind models\n" +
		"export workflow w() entry -> string { return \"ok\"; }\n"
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(bin, "deps", "--resolve", p).CombinedOutput()
	if err != nil {
		t.Fatalf("deps 失败: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"<std/net>", "builtin", // std
		"native \"libm.a\" from \"m.h\"", "archive=libm.a header=m.h", // native
		"\"models\"", "needs -L", // local
	} {
		if !strings.Contains(got, want) {
			t.Errorf("输出缺少 %q\n%s", want, got)
		}
	}
	// 稳定性：同一份输入两次输出必须一致（排序不是"碰巧"）。
	again, err := exec.Command(bin, "deps", "--resolve", p).CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != got {
		t.Fatalf("两次输出不同（排序不稳定）\n--- 1 ---\n%s\n--- 2 ---\n%s", got, again)
	}
}

// TestDepsRequiresResolveFlag：缺 --resolve 必须报用法错误（退出码 2），
// 而不是默默什么都不打印——"看起来成功但什么也没做"是最坏的 CLI 行为。
func TestDepsRequiresResolveFlag(t *testing.T) {
	root := repoRootForBackend(t)
	bin := filepath.Join(root, "build", "hermes")
	cmd := exec.Command(bin, "deps", "x.hermes")
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if err == nil || !strings.Contains(string(out), "--resolve") {
		t.Fatalf("缺 --resolve 应报用法错误，实际 err=%v out=%s", err, out)
	}
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() != 2 {
		t.Fatalf("退出码应 2，实际 %d", ee.ExitCode())
	}
	_ = ee
}
