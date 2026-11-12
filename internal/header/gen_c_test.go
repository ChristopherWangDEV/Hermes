package header

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"hermes/internal/ir"
)

func loadGolden(t *testing.T, name string) *ir.Doc {
	t.Helper()
	data, err := os.ReadFile("../../schemas/golden/" + name)
	if err != nil {
		t.Fatal(err)
	}
	d, ds := ir.Load(data)
	if len(ds) != 0 {
		t.Fatalf("golden 不合格: %d 条", len(ds))
	}
	return d
}

// TestGenCIsDeterministic 守"同 IR 两次生成字节全等"（T47 ①）。
//
// 头文件也会进版本库、进客户构建；不稳定意味着每次重新生成都是一次 diff。
func TestGenCIsDeterministic(t *testing.T) {
	d := loadGolden(t, "ir-support_bot.json")
	a, err := GenC(d)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		b, _ := GenC(d)
		if !bytes.Equal(a, b) {
			t.Fatalf("第 %d 次生成字节不同", i)
		}
	}
}

// TestGenCOnlyIncludesCoreHeader 守 ⑥：只允许 hermes_core.h 一个自有头。
//
// 多 include 一个系统头，就等于把"客户环境里有没有那个头"变成新的部署前提。
func TestGenCOnlyIncludesCoreHeader(t *testing.T) {
	d := loadGolden(t, "ir-support_bot.json")
	out, err := GenC(d)
	if err != nil {
		t.Fatal(err)
	}
	includeRE := regexp.MustCompile(`(?m)^#include\s+[<"]([^>"]+)[>"]`)
	got := includeRE.FindAllStringSubmatch(string(out), -1)
	if len(got) != 1 || got[0][1] != "hermes/hermes_core.h" {
		t.Fatalf("只允许 include hermes/hermes_core.h，实际 %v", got)
	}
	// 反向：不许出现 `//` 注释（C89 无行注释）。
	if strings.Contains(string(out), "//") {
		t.Fatal("生成的 C89 头里不许有 // 注释")
	}
}

// TestGenCCompilesAsC89 是本包最重要的一条：**真的用 C89 编译器吃一遍**。
//
// 断言"看起来像 C89"没有意义 —— 只有编译器说了算，而且必须是
// `-std=c89 -pedantic -Werror`：宽松模式会放过 `//` 注释与 C99 语法，
// 而那些在客户的 MSVC / 老编译器上就是编译失败。
func TestGenCCompilesAsC89(t *testing.T) {
	cc, err := exec.LookPath("clang")
	if err != nil {
		t.Skip("没有 clang，跳过编译验证")
	}
	d := loadGolden(t, "ir-support_bot.json")
	out, err := GenC(d)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	hdr := filepath.Join(dir, "support_gen.h")
	if err := os.WriteFile(hdr, out, 0o644); err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(dir, "probe.c")
	src := `#include "support_gen.h"
int main(void) {
    hermes_buf in;
    hermes_buf out;
    hermes_status st;
    in.ptr = NULL; in.len = 0;
    out.ptr = NULL; out.len = 0;
    st = hermes_support_support_bot(&in, &out);
    hermes_buf_free(&out);
    return (int)st;
}
`
	if err := os.WriteFile(probe, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(cc, "-std=c89", "-Wall", "-Wextra", "-Werror", "-pedantic",
		"-I", dir, "-I", "../../include", "-fsyntax-only", probe)
	combined, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("C89 编译失败：%v\n%s\n--- 生成的头 ---\n%s", err, combined, out)
	}
}
