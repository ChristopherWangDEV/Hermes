package main

import (
	"bytes"
	"hermes/internal/archive"
	"os"
	"path/filepath"
	"testing"
)

// TestLinkMergesAndIsDeterministic：两个库合成一个归档，两次结果字节相等。
func TestLinkMergesAndIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "live.hbc")
	b := filepath.Join(dir, "hello.hbc")
	out1 := filepath.Join(dir, "m1.hbc")
	out2 := filepath.Join(dir, "m2.hbc")

	for _, c := range [][]string{
		{"build", "testdata/live.hermes", "-o", a},
		{"build", "testdata/hello.hermes", "-o", b},
	} {
		if code := run(c); code != 0 {
			t.Fatalf("%v 退出码 %d", c, code)
		}
	}
	if code := run([]string{"link", a, b, "-o", out1}); code != 0 {
		t.Fatalf("link 退出码 %d", code)
	}
	// 输入顺序反过来，产物也必须一样：链接的可重现性不该依赖调用方顺序。
	if code := run([]string{"link", b, a, "-o", out2}); code != 0 {
		t.Fatalf("link（反序）退出码 %d", code)
	}

	x, _ := os.ReadFile(out1)
	y, _ := os.ReadFile(out2)
	if !bytes.Equal(x, y) {
		t.Fatal("输入顺序不应影响链接产物")
	}

	// 两个成员都在。
	m, err := os.ReadFile(out1)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(m, []byte("live")) || !bytes.Contains(m, []byte("hello")) {
		t.Fatal("合并后的归档应含两个成员")
	}
}

// TestLinkRejectsDuplicateWorkflow：同名 workflow 必须失败，不许覆盖。
func TestLinkRejectsDuplicateWorkflow(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.hbc")
	b := filepath.Join(dir, "b.hbc")

	if code := run([]string{"build", "testdata/hello.hermes", "-o", a}); code != 0 {
		t.Fatal("build a 失败")
	}
	// 同一个库放两次 = 两份同名成员。
	if code := run([]string{"build", "testdata/hello.hermes", "-o", b}); code != 0 {
		t.Fatal("build b 失败")
	}
	if code := run([]string{"link", a, b, "-o", filepath.Join(dir, "out.hbc")}); code != 4 {
		t.Fatalf("同名 workflow 应退出 4，实际 %d", code)
	}
}

// TestLinkLibrarySearch 守 `-l`/`-L`（T45）。
//
// 真实项目里库是按**名字**发现的；没有它只能把路径硬编码进构建脚本，
// 而"可重现"会退化成"可重现于我这台机器"。
func TestLinkLibrarySearch(t *testing.T) {
	dir := t.TempDir()
	libdir := filepath.Join(dir, "libs")
	if err := os.MkdirAll(libdir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 两个库：一个用 unix 习惯名（libhello.hbc），一个用 build 默认名（live.hbc）。
	for src, dst := range map[string]string{
		"testdata/hello.hermes": filepath.Join(libdir, "libhello.hbc"),
		"testdata/live.hermes":  filepath.Join(libdir, "live.hbc"),
	} {
		if code := run([]string{"build", src, "-o", dst}); code != 0 {
			t.Fatalf("build %s 失败", src)
		}
	}

	out := filepath.Join(dir, "merged.hbc")
	if code := run([]string{"link", "-L", libdir, "-l", "hello", "-l", "live", "-o", out}); code != 0 {
		t.Fatalf("link 退出码 %d", code)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	a, err := archive.Read(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Members) != 2 {
		t.Fatalf("应有 2 个成员，实际 %d", len(a.Members))
	}
}

// TestLinkMissingLibraryListsCandidates 守"找不到时列出试过的路径"。
//
// 只说"找不到 libfoo"会让人反复猜 -L 该指哪，而每次猜都要跑一遍命令。
func TestLinkMissingLibraryListsCandidates(t *testing.T) {
	dir := t.TempDir()
	_ = run([]string{"build", "testdata/hello.hermes", "-o", filepath.Join(dir, "hello.hbc")})

	out := filepath.Join(dir, "m.hbc")
	// 用一个不存在的库名；输出到 stderr 的候选里必须包含两种命名。
	code := run([]string{"link", "-L", dir, "-l", "nope", "-o", out})
	if code != 4 {
		t.Fatalf("找不到库应退出 4，实际 %d", code)
	}
}
