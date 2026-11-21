package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestExplainWhyPrintsOnlyRationale 是 `explain --why` 的判据：
// 只给 why + fixes，**不给**完整条目（message/docs 是 `explain` 的活）。
//
// 为什么要分开：用户已经在错误输出里看过 message，`--why` 是"这条规则为什么
// 存在"的单独查询。把完整条目再打一遍等于没做这个开关。
func TestExplainWhyPrintsOnlyRationale(t *testing.T) {
	root := repoRootForBackend(t)
	bin := filepath.Join(root, "build", "hermes")

	out, err := exec.Command(bin, "explain", "E4036", "--why").CombinedOutput()
	if err != nil {
		t.Fatalf("explain --why 失败: %v\n%s", err, out)
	}
	got := string(out)
	if !strings.Contains(got, "who supplies the implementation") {
		t.Fatalf("缺 why 内容：%s", got)
	}
	if strings.Count(got, "fix: ") < 2 {
		t.Fatalf("应给出至少两条改法：%s", got)
	}
	// 不该出现完整条目才有的东西。
	for _, unwanted := range []string{"docs:", "error[E4036]"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("--why 不应输出 %q：%s", unwanted, got)
		}
	}
}

// TestExplainWhyChinese 守中文路径：文案仍来自 C++ catalog，Go 只挑字段。
func TestExplainWhyChinese(t *testing.T) {
	root := repoRootForBackend(t)
	bin := filepath.Join(root, "build", "hermes")
	cmd := exec.Command(bin, "explain", "E3028", "--why")
	cmd.Env = append(cmd.Environ(), "HERMES_LANG=zh")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("中文 explain --why 失败: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "指令不是语句") {
		t.Fatalf("中文 why 缺失：%s", out)
	}
}
