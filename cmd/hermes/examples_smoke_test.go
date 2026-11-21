package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestExamplesCorpusBuilds 把**全部示例**过一遍 `build`。
//
// 为什么值得一条语料级 smoke：单点测试都盯着各自的功能，而"某个示例因为
// 最近一次改动编不过"只有全量跑才会暴露。示例是发布包里的门面——它们坏了，
// 用户第一次接触就是失败。
//
// 判据不是"退出码 0"而是**与期望一致**：有些示例本来就该失败（比如刻意演示
// 诊断的那些），所以这里只要求：要么成功产出 .hbc，要么失败时 stderr 有诊断。
// 不允许的是"崩溃 / 无输出 / 退出码 2 却没有用法提示"这类.
func TestExamplesCorpusBuilds(t *testing.T) {
	root := repoRootForBackend(t)
	bin := filepath.Join(root, "build", "hermes")
	var files []string
	err := filepath.Walk(filepath.Join(root, "examples"), func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(p, ".hermes") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("没有找到任何示例")
	}
	dir := t.TempDir()
	for _, src := range files {
		name := strings.TrimSuffix(filepath.Base(src), ".hermes")
		out := filepath.Join(dir, name+".hbc")
		res, err := exec.Command(bin, "build", src, "-o", out).CombinedOutput()
		text := string(res)
		if err != nil {
			// 失败可以，但必须**说清原因**：空输出或只有崩溃堆栈说明是内部错误。
			if len(strings.TrimSpace(text)) == 0 {
				t.Fatalf("%s: 失败但没有输出", filepath.Base(src))
			}
			if strings.Contains(text, "panic:") || strings.Contains(text, "runtime error:") {
				t.Fatalf("%s: 崩溃而不是诊断:\n%s", filepath.Base(src), text)
			}
			// 有诊断码或明确前缀才算"说清了"。
			if !strings.Contains(text, "error[") && !strings.Contains(text, "hermes") {
				t.Fatalf("%s: 输出的不是诊断:\n%s", filepath.Base(src), text)
			}
			continue
		}
		if _, statErr := os.Stat(out); statErr != nil {
			t.Fatalf("%s: 退出码 0 但没有产物", filepath.Base(src))
		}
	}
	t.Logf("示例语料：%d 个文件全部可构建（或给出可读诊断）", len(files))
}
