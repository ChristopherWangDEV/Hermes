package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"hermes/internal/objfile"
)

// TestCCLinksUserProgram 是 T48 的验收：**用户自己的 C 程序**调 workflow 并跑出结果。
//
// 这是"可嵌入"这句话的可执行形式 —— 不是 hermes 自己的二进制能跑，
// 而是别人写的 C 程序能链上它。
func TestCCLinksUserProgram(t *testing.T) {
	root := "../.."
	lib := filepath.Join(root, "build", "libhermesrt.a")
	if _, err := os.Stat(lib); err != nil {
		t.Skipf("没有 %s（先跑 scripts/build.sh）", lib)
	}
	if _, err := exec.LookPath("clang"); err != nil {
		t.Skip("没有 clang")
	}

	dir := t.TempDir()
	hbc := filepath.Join(dir, "hello.hbc")
	if code := run([]string{"build", "testdata/hello.hermes", "-o", hbc}); code != 0 {
		t.Fatalf("build 退出码 %d", code)
	}

	src := filepath.Join(dir, "user.c")
	// 用户程序只 include 生成的头，不 include 任何 hermes 内部实现。
	prog := `#include "hello.h"

int main(void) {
    hermes_buf in;
    hermes_buf out;
    hermes_status st;
    in.ptr = 0; in.len = 0; out.ptr = 0; out.len = 0;
    st = hermes_hello_hello(&in, &out);
    if (st != HERMES_OK) { return 1; }
    return out.len == 2 ? 0 : 2;
}
`
	if err := os.WriteFile(src, []byte(prog), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "prog")
	if code := run([]string{"cc", src, "--unit", hbc, "-o", out, "--runtime-lib", lib}); code != 0 {
		t.Fatalf("cc 退出码 %d", code)
	}
	if err := exec.Command(out).Run(); err != nil {
		t.Fatalf("用户程序运行失败: %v", err)
	}
}

// TestCCRejectsMangledSymbols 守 D-025：mangled 符号是**硬失败**。
//
// 理由不是洁癖：修饰名意味着有 C++ 翻译单元被链了，而它通常也意味着 libstdc++
// 进来了 —— 那是许可问题，不只是兼容问题。
func TestCCRejectsMangledSymbols(t *testing.T) {
	root := "../.."
	lib := filepath.Join(root, "build", "libhermesrt.a")
	if _, err := os.Stat(lib); err != nil {
		t.Skip("没有运行时库")
	}
	if _, err := exec.LookPath("clang"); err != nil {
		t.Skip("没有 clang")
	}

	dir := t.TempDir()
	hbc := filepath.Join(dir, "hello.hbc")
	if code := run([]string{"build", "testdata/hello.hermes", "-o", hbc}); code != 0 {
		t.Fatal("build 失败")
	}
	// 用户程序里带一个 C++ 翻译单元（.cpp 会被 cc 以 -std=c89 编译而失败，
	// 所以这里用 C 写一个显式的 mangled 引用是构造不出来的）——
	// 改为直接验证判据函数本身，避免用"编译失败"冒充"符号检查生效"。
	if !objfile.Mangled("_Z3foov") {
		t.Fatal("_Z3foov 应被判为 mangled")
	}
	for _, ok := range []string{"hermes_hello_hello", "_hermes_hello_hello", "hermes_buf_free"} {
		if objfile.Mangled(ok) {
			t.Fatalf("%q 不该被判为 mangled", ok)
		}
	}
}
