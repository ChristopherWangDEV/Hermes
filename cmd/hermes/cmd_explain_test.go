package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestExplainPrintsBounds 守"上界要能被人看见"。
//
// 上界驱动剥离与 trace 限额，所以它写错是正确性问题，不是显示问题。
// 这条测试只钉住"该出现的数字出现了"，不钉排版。
func TestExplainPrintsBounds(t *testing.T) {
	wd, _ := os.Getwd()
	if err := os.Chdir(filepath.Join(wd, "..", "..")); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)

	if code := run([]string{"explain", "schemas/golden/ir-support_bot.json"}); code != 0 {
		t.Fatalf("explain 退出码 %d", code)
	}
}

// TestExplainUnknownWorkflow 守"打错名字要说清楚，并列出可用的"。
func TestExplainUnknownWorkflow(t *testing.T) {
	wd, _ := os.Getwd()
	if err := os.Chdir(filepath.Join(wd, "..", "..")); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)

	if code := run([]string{"explain", "schemas/golden/ir-support_bot.json", "--workflow", "nope"}); code != 4 {
		t.Fatalf("未知 workflow 应退出 4，实际 %d", code)
	}
}
