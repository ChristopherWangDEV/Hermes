package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildProducesStableArchive 是 R-HF-03 的端到端版本：
// 同一份源码两次 build，`.hbc` 字节相等。
//
// 这条比"IR 字节相等"更强：归档里还有索引、偏移、对齐填充 ——
// 任何一处不稳定都会让"同一份源码 ⇒ 同一份可分发产物"失效。
func TestBuildProducesStableArchive(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.hbc")
	b := filepath.Join(dir, "b.hbc")

	if code := run([]string{"build", "testdata/live.hermes", "-o", a}); code != 0 {
		t.Fatalf("第一次 build 退出码 %d", code)
	}
	if code := run([]string{"build", "testdata/live.hermes", "-o", b}); code != 0 {
		t.Fatalf("第二次 build 退出码 %d", code)
	}
	x, _ := os.ReadFile(a)
	y, _ := os.ReadFile(b)
	if !bytes.Equal(x, y) {
		t.Fatalf("两次 build 的 .hbc 字节不同（%d vs %d）", len(x), len(y))
	}
}

// TestArExtractMatchesGolden：拆出来的成员字节必须与 golden IR 一致。
//
// 这条把"归档只是容器"钉死：容器可以加索引、加填充、加签名，
// 但成员字节不许被它改动 —— 否则 ir_sha256 与签名会跟着变。
func TestArExtractMatchesGolden(t *testing.T) {
	dir := t.TempDir()
	hbc := filepath.Join(dir, "x.hbc")
	if code := run([]string{"build", "testdata/live.hermes", "-o", hbc}); code != 0 {
		t.Fatalf("build 退出码 %d", code)
	}
	out := filepath.Join(dir, "m.json")
	if code := run([]string{"ar", "x", hbc, "live", "-o", out}); code != 0 {
		t.Fatalf("ar x 退出码 %d", code)
	}
	got, _ := os.ReadFile(out)
	want, err := os.ReadFile("../../schemas/golden/ir-live.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("拆出的成员与 golden 不一致（%d vs %d 字节）", len(got), len(want))
	}
}

// TestArRejectsCorruptFile 守"损坏的归档不许被当成好的读".
func TestArRejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	hbc := filepath.Join(dir, "x.hbc")
	if code := run([]string{"build", "testdata/live.hermes", "-o", hbc}); code != 0 {
		t.Fatal("build 失败")
	}
	data, _ := os.ReadFile(hbc)
	data[0] = 'X' // 破坏 magic
	if err := os.WriteFile(hbc, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"ar", "t", hbc}); code != 4 {
		t.Fatalf("损坏的归档应退出 4，实际 %d", code)
	}
}

// TestArListShowsSymbols 证明 `ar t` 真的列了符号（而不只是成员名）。
func TestArListShowsSymbols(t *testing.T) {
	dir := t.TempDir()
	hbc := filepath.Join(dir, "x.hbc")
	if code := run([]string{"build", "testdata/live.hermes", "-o", hbc}); code != 0 {
		t.Fatal("build 失败")
	}
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	code := run([]string{"ar", "t", hbc})
	os.Stdout = old
	w.Close()

	buf := make([]byte, 1<<16)
	n, _ := r.Read(buf)
	r.Close()
	out := string(buf[:n])

	if code != 0 {
		t.Fatalf("ar t 退出码 %d", code)
	}
	for _, want := range []string{"live", "workflow", "type"} {
		if !strings.Contains(out, want) {
			t.Fatalf("ar t 输出缺少 %q：\n%s", want, out)
		}
	}
}
