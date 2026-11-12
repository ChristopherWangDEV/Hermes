package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDoctorRuns 是冒烟：doctor 在仓库根目录下必须全部通过。
//
// 它守的其实是"仓库自身的完整性"：stdlib 内嵌、golden 可加载、clang 身份、
// 运行时库存在。任何一项坏了，这条会红 —— 而人不必等到某个功能失败才发现。
func TestDoctorRuns(t *testing.T) {
	// 测试的工作目录是 cmd/hermes，doctor 按仓库根找 schemas/ 与 build/。
	wd, _ := os.Getwd()
	if err := os.Chdir(filepath.Join(wd, "..", "..")); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)

	if code := run([]string{"doctor"}); code != 0 {
		t.Fatalf("doctor 退出码 %d", code)
	}
}

// TestDoctorFailsOutsideRepo 守"读不到就报，不假装通过"。
//
// 在空目录里跑 doctor 必须失败：缺 stdlib/goldens 的判定若写反，
// 会给出"环境正常"的假结论 —— 那比没有 doctor 更糟。
func TestDoctorFailsOutsideRepo(t *testing.T) {
	wd, _ := os.Getwd()
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)

	// native ABI / stdlib / clang 仍可能通过（它们不依赖工作目录），
	// 所以这里只断言 goldens 一项被判失败：退出码应为 4。
	if code := run([]string{"doctor"}); code != 4 {
		t.Fatalf("仓库外应退出 4（goldens 判失败），实际 %d", code)
	}
}
