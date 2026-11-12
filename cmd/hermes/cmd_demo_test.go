package main

import (
	"os"
	"os/exec"
	"testing"
)

// TestDemoRunsFromEmbeddedSource 守"一条命令看到整条链"。
//
// 它同时钉住一件重要的事：**示例源码是 go:embed 的**。用户装的是发布包，
// 没有仓库；如果 demo 读磁盘上的相对路径，它在用户机器上只会报"找不到文件"，
// 而那正是最不该出现的第一次体验。
func TestDemoRunsFromEmbeddedSource(t *testing.T) {
	if _, err := exec.LookPath("clang"); err != nil {
		t.Skip("没有 clang，AOT 那一步会被跳过（demo 本身仍应成功）")
	}
	wd, _ := os.Getwd()
	if err := os.Chdir("../.."); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)

	if code := run([]string{"demo"}); code != 0 {
		t.Fatalf("demo 退出码 %d", code)
	}
}
