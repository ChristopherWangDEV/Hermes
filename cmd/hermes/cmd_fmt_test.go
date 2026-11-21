package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFmtNormalisesWhitespace 守三件局部变换。
func TestFmtNormalisesWhitespace(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.hermes")
	dirty := "export workflow f() entry -> string {\r\n    return \"x\";   \r\n}\r\n\r\n\r\n"
	if err := os.WriteFile(p, []byte(dirty), 0o644); err != nil {
		t.Fatal(err)
	}

	if code := run([]string{"fmt", p}); code != 0 {
		t.Fatalf("fmt 退出码 %d", code)
	}
	got, _ := os.ReadFile(p)
	want := "export workflow f() entry -> string {\n    return \"x\";\n}\n"
	if string(got) != want {
		t.Fatalf("格式化结果不对：\n%q\n期望：\n%q", got, want)
	}

	// 幂等：再跑一次不该再改。
	if code := run([]string{"fmt", p}); code != 0 {
		t.Fatalf("第二次 fmt 退出码 %d", code)
	}
	again, _ := os.ReadFile(p)
	if string(again) != want {
		t.Fatal("fmt 不幂等")
	}
}

// TestFmtCheckMode 守 --check：CI 用它挡住"有人用 Windows 保存了一次"。
func TestFmtCheckMode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.hermes")
	os.WriteFile(p, []byte("a  \r\n\r\n"), 0o644)

	if code := run([]string{"fmt", "--check", p}); code != 4 {
		t.Fatalf("未格式化的文件 --check 应退出 4，实际 %d", code)
	}
	// --check 不能改文件。
	got, _ := os.ReadFile(p)
	if string(got) != "a  \r\n\r\n" {
		t.Fatal("--check 不该修改文件")
	}

	os.WriteFile(p, []byte("a\n"), 0o644)
	if code := run([]string{"fmt", "--check", p}); code != 0 {
		t.Fatalf("已格式化的文件 --check 应退出 0，实际 %d", code)
	}
}
