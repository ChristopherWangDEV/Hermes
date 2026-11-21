package main

import (
	"strings"
	"os/exec"
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

// TestDoctorOutsideRepoDoesNotFakeGoldens 守"读不到就如实说，不假装通过"。
//
// 仓库外没有 schemas/golden，doctor 必须把那一项标成"发布包不带它们"，
// 而不是报"19 goldens all loadable"（那才是假的通过）。
//
// **为什么不断言退出码**：原先这条断言"仓库外退 4"，但当时失败的是
// "runtime lib"——因为 Go 版在进程内跑，看到的是**测试二进制**的路径。
// 搬运到 C++ 后 doctor 由独立进程执行，看到的是 build/hermes-native 的路径，
// 于是那一项通过了。退出码因此变成了"取决于谁在执行"，不是判据该钉的东西。
func TestDoctorOutsideRepoDoesNotFakeGoldens(t *testing.T) {
	root := repoRootForBackend(t)
	bin := filepath.Join(root, "build", "hermes")
	dir := t.TempDir()
	cmd := exec.Command(bin, "doctor")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	text := string(out)
	if err != nil {
		// 允许失败：机器上缺 clang 等真实缺失本就该失败。
		t.Logf("doctor 在仓库外退出非零（%v）", err)
	}
	if strings.Contains(text, "goldens all loadable") {
		t.Fatalf("仓库外不该报 \"goldens all loadable\"：\n%s", text)
	}
	if !strings.Contains(text, "not shipped") && !strings.Contains(text, "no goldens") {
		t.Fatalf("仓库外应说明 goldens 不适用或缺失：\n%s", text)
	}
}
