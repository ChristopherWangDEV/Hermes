package header

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestGenCPPIncludesNoCxxHeaders 守红线：生成的 .hpp **一个 C++ 标准库头都不 include**。
//
// Linux 上 clang++ 默认链 libstdc++（GPL-3.0 + Runtime Exception）。默认模式若引入
// <string>/<string_view>/<system_error>，就等于把那条许可问题写进默认路径。
func TestGenCPPIncludesNoCxxHeaders(t *testing.T) {
	d := loadGolden(t, "ir-support_bot.json")
	out, err := GenCPP(d)
	if err != nil {
		t.Fatal(err)
	}
	includeRE := regexp.MustCompile(`(?m)^#include\s+[<"]([^>"]+)[>"]`)
	got := includeRE.FindAllStringSubmatch(string(out), -1)
	if len(got) != 1 || got[0][1] != "hermes/hermes_core.h" {
		t.Fatalf("只允许 include hermes/hermes_core.h，实际 %v", got)
	}
	for _, bad := range []string{"<string>", "<string_view>", "<system_error>", "<vector>", "<memory>"} {
		if strings.Contains(string(out), bad) {
			t.Fatalf("生成的 .hpp 引用了 %s —— 那会把 libstdc++ 的许可问题带进默认路径", bad)
		}
	}
}

// TestGenCPPCompilesWithoutStdlib 用 **-nostdinc++** 真编一遍。
//
// `-nostdinc++` 让编译器找不到任何 C++ 标准库头：能编过，就证明这份头确实
// 不依赖标准库 —— 而不是"恰好没 include 到"。两种模式都要过：
// 默认（纯 C 声明）与 HERMES_CPP_WRAPPERS（便利层）。
func TestGenCPPCompilesWithoutStdlib(t *testing.T) {
	cxx, err := exec.LookPath("clang++")
	if err != nil {
		t.Skip("没有 clang++")
	}
	d := loadGolden(t, "ir-support_bot.json")
	out, err := GenCPP(d)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "support_gen.hpp"), out, 0o644); err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(dir, "probe.cpp")
	src := `#include "support_gen.hpp"
int main() {
    hermes_buf in; hermes_buf out;
    in.ptr = 0; in.len = 0; out.ptr = 0; out.len = 0;
    hermes_status st = hermes_support_support_bot(&in, &out);
    hermes_buf_free(&out);
    return (int)st;
}
`
	if err := os.WriteFile(probe, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, mode := range []struct {
		name string
		def  string
	}{{"默认（纯 C 声明）", ""}, {"HERMES_CPP_WRAPPERS", "HERMES_CPP_WRAPPERS"}} {
		args := []string{"-std=c++17", "-nostdinc++", "-Wall", "-Wextra", "-Werror", "-pedantic",
			"-I", dir, "-I", "../../include", "-fsyntax-only"}
		if mode.def != "" {
			args = append(args, "-D"+mode.def)
		}
		args = append(args, probe)
		if b, err := exec.Command(cxx, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s: -nostdinc++ 编译失败: %v\n%s", mode.name, err, b)
		}
	}
}
