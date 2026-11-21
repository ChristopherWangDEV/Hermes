package native

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"hermes/internal/header"
)

// TestHeaderGenMatchesGo：C++ 生成的 C 头必须与 Go 的 header.GenC **逐字节相同**。
//
// 头文件是**分发物**：用户程序 include 它。任何排版差异都会让"同一份 IR 在两台
// 机器上生成不同头文件"，而那种差异极难发现（两边都能编过）。
func TestHeaderGenMatchesGo(t *testing.T) {
	root := filepath.Join("..", "..")
	bin := filepath.Join(root, "build", "hermes")
	samples := []string{
		filepath.Join(root, "examples", "arrays", "length.hermes"),
		filepath.Join(root, "examples", "functions", "greet.hermes"),
	}
	compared := 0
	for _, src := range samples {
		ir, err := exec.Command(bin, "ir", src).Output()
		if err != nil {
			continue
		}
		doc, ds, lerr := LoadIR(ir)
		if lerr != nil || len(ds) != 0 || doc == nil {
			t.Fatalf("%s: 读 IR 失败", src)
		}
		want, err := header.GenC(doc)
		if err != nil {
			t.Fatalf("%s: header.GenC: %v", src, err)
		}
		got, err := HeaderGenC(ir)
		if err != nil {
			t.Fatalf("%s: C++ 生成失败: %v", src, err)
		}
		if string(got) != string(want) {
			os.WriteFile(filepath.Join(t.TempDir(), "x"), want, 0o644)
			t.Fatalf("%s: 头文件不同\n--- Go ---\n%s\n--- C++ ---\n%s", src, want, got)
		}
		compared++
	}
	if compared == 0 {
		t.Fatal("一个样本都没比到")
	}
	t.Logf("C 头逐字节一致：%d 个样本", compared)
}

// TestHeaderShimMatchesGo：shim 也必须逐字节相同——它是**链接层**，
// 差一个字符就可能让 per-unit 符号与运行时入口对不上。
func TestHeaderShimMatchesGo(t *testing.T) {
	root := filepath.Join("..", "..")
	bin := filepath.Join(root, "build", "hermes")
	src := filepath.Join(root, "examples", "arrays", "length.hermes")
	ir, err := exec.Command(bin, "ir", src).Output()
	if err != nil {
		t.Fatal(err)
	}
	doc, ds, lerr := LoadIR(ir)
	if lerr != nil || len(ds) != 0 || doc == nil {
		t.Fatal("读 IR 失败")
	}
	want, err := header.GenCShim(doc, ir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := HeaderGenShim(ir, ir)
	if err != nil {
		t.Fatalf("C++ shim 生成失败: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("shim 不同\n--- Go ---\n%.600s\n--- C++ ---\n%.600s", want, got)
	}
}

// TestHeaderCoreImplMatchesGo：运行时核心实现正文必须与 Go 的 header.CoreImpl 相同。
func TestHeaderCoreImplMatchesGo(t *testing.T) {
	got, err := HeaderCoreImpl()
	if err != nil {
		t.Fatalf("C++ 读取失败: %v", err)
	}
	if string(got) != header.CoreImpl {
		t.Fatalf("core impl 正文不同：C++ %d 字节 / Go %d 字节", len(got), len(header.CoreImpl))
	}
}

// TestHeaderGenCPPMatchesGo：C++ 入口头也必须逐字节相同。
func TestHeaderGenCPPMatchesGo(t *testing.T) {
	root := filepath.Join("..", "..")
	bin := filepath.Join(root, "build", "hermes")
	src := filepath.Join(root, "examples", "arrays", "length.hermes")
	ir, err := exec.Command(bin, "ir", src).Output()
	if err != nil {
		t.Fatal(err)
	}
	doc, ds, lerr := LoadIR(ir)
	if lerr != nil || len(ds) != 0 || doc == nil {
		t.Fatal("读 IR 失败")
	}
	want, err := header.GenCPP(doc)
	if err != nil {
		t.Fatal(err)
	}
	got, err := HeaderGenCPP(ir)
	if err != nil {
		t.Fatalf("C++ 生成失败: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf(".hpp 不同\n--- Go ---\n%.500s\n--- C++ ---\n%.500s", want, got)
	}
}
