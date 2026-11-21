package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestNativeCCMatchesGo 是 `cc` 的端到端判据：同一个 unit + 同一份 C 程序，
// `hermes cc` 与 `hermes-native cc` 都必须**成功产出可执行文件**，且两个产物
// 跑起来输出相同。
//
// 为什么必须比"跑起来"而不只是"退出码 0"：cc 的失败模式是"能编过、运行期才炸"
// ——shim 与运行时入口对不上时，符号都存在，但结果不对。
func TestNativeCCMatchesGo(t *testing.T) {
	root := repoRootForBackend(t)
	goBin := filepath.Join(root, "build", "hermes")
	nativeBin := filepath.Join(root, "build", "hermes-native")
	dir := t.TempDir()

	unit := filepath.Join(dir, "length.hbc")
	if out, err := exec.Command(goBin, "build", filepath.Join(root, "examples", "arrays", "length.hermes"),
		"-o", unit).CombinedOutput(); err != nil {
		t.Fatalf("build unit: %v\n%s", err, out)
	}

	prog := filepath.Join(dir, "prog.c")
	src := `#include <stdio.h>
#include "length.h"

int main(void) {
  hermes_buf in = {0}, out = {0};
  const char* body = "{\"tags\":[\"a\",\"b\"]}";
  in.ptr = (char*)body;
  in.len = (size_t)(sizeof("{\"tags\":[\"a\",\"b\"]}") - 1);
  hermes_status st = hermes_length_main(&in, &out);
  printf("st=%d len=%zu\n", (int)st, (size_t)out.len);
  return 0;
}
`
	if err := os.WriteFile(prog, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	run := func(bin, outName string) (string, error) {
		out := filepath.Join(dir, outName)
		cmd := exec.Command(bin, "cc", prog, "--unit", unit, "-o", out,
			"--runtime-lib", filepath.Join(root, "build", "libhermesrt.a"))
		cmd.Dir = dir
		cmd.Env = append(cmd.Environ(), "HERMES_HOME="+root)
		if b, err := cmd.CombinedOutput(); err != nil {
			return string(b), err
		}
		if _, err := os.Stat(out); err != nil {
			return "", err
		}
		// 产物必须能跑（这是 cc 的真正判据：链接正确）
		got, err := exec.Command(out).CombinedOutput()
		if err != nil {
			return string(got), err
		}
		return strings.TrimSpace(string(got)), nil
	}

	want, err := run(goBin, "go.bin")
	if err != nil {
		t.Fatalf("Go cc 失败: %v\n%s", err, want)
	}
	got, err := run(nativeBin, "native.bin")
	if err != nil {
		t.Fatalf("native cc 失败: %v\n%s", err, got)
	}
	if got != want {
		t.Fatalf("产物行为不同\n  Go =%q\n  C++=%q", want, got)
	}
	t.Logf("cc 端到端一致：%q", got)
}

// TestNativeCCCPPParity：C++ 路径要两遍编译（shim 与 core impl 必须**当 C** 编，
// 否则 `_Z…` 修饰名与头里声明的名字当场链不上）。这条测试把它跑通并比行为。
func TestNativeCCCPPParity(t *testing.T) {
	root := repoRootForBackend(t)
	goBin := filepath.Join(root, "build", "hermes")
	nativeBin := filepath.Join(root, "build", "hermes-native")
	dir := t.TempDir()

	unit := filepath.Join(dir, "length.hbc")
	if out, err := exec.Command(goBin, "build", filepath.Join(root, "examples", "arrays", "length.hermes"),
		"-o", unit).CombinedOutput(); err != nil {
		t.Fatalf("build unit: %v\n%s", err, out)
	}
	prog := filepath.Join(dir, "prog.cpp")
	src := `#include <cstdio>
#include "length.hpp"

int main() {
  hermes_buf in = {0}, out = {0};
  const char* body = "{\"tags\":[\"a\"]}";
  in.ptr = const_cast<char*>(body);
  in.len = 13;
  hermes_status st = hermes_length_main(&in, &out);
  std::printf("st=%d len=%zu\n", (int)st, (size_t)out.len);
  return 0;
}
`
	if err := os.WriteFile(prog, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	run := func(bin, outName string) (string, error) {
		out := filepath.Join(dir, outName)
		cmd := exec.Command(bin, "cc", prog, "--unit", unit, "-o", out,
			"--runtime-lib", filepath.Join(root, "build", "libhermesrt.a"))
		cmd.Dir = dir
		cmd.Env = append(cmd.Environ(), "HERMES_HOME="+root)
		if b, err := cmd.CombinedOutput(); err != nil {
			return string(b), err
		}
		got, err := exec.Command(out).CombinedOutput()
		if err != nil {
			return string(got), err
		}
		return strings.TrimSpace(string(got)), nil
	}

	want, err := run(goBin, "go_cpp.bin")
	if err != nil {
		t.Fatalf("Go cc（C++）失败: %v\n%s", err, want)
	}
	got, err := run(nativeBin, "native_cpp.bin")
	if err != nil {
		t.Fatalf("native cc（C++）失败: %v\n%s", err, got)
	}
	if got != want {
		t.Fatalf("C++ 产物行为不同\n  Go =%q\n  C++=%q", want, got)
	}
	t.Logf("C++ 路径端到端一致：%q", got)
}
