package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"hermes/internal/archive"
)

// TestNativeIRMatchesGo 是 M4 的主判据：仓库里每个 `.hermes` 样本，
// `hermes-native ir` 的输出必须与 `hermes ir` **逐字节相同**。
//
// 为什么要求全覆盖而不是"挑能跑的"：挑样本等于自己给自己出题；只要有一个
// 形态没对齐，产物字节就会不同，而那正是迁移唯一能证明的东西。
func TestNativeIRMatchesGo(t *testing.T) {
	root := repoRootForBackend(t)
	goBin := filepath.Join(root, "build", "hermes")
	nativeBin := filepath.Join(root, "build", "hermes-native")

	var compared, skipped int
	for _, sub := range []string{"examples", "cmd"} {
		err := filepath.Walk(filepath.Join(root, sub), func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(p, ".hermes") {
				return nil
			}
			dir, base := filepath.Dir(p), filepath.Base(p)
			want, werr := runIn(dir, goBin, "ir", base)
			if werr != nil || len(want) == 0 {
				skipped++ // Go 自己都编不过的样本（负例）
				return nil
			}
			got, gerr := runIn(dir, nativeBin, "ir", base)
			if gerr != nil {
				t.Fatalf("%s: native ir 失败: %v\n%s", p, gerr, got)
			}
			compared++
			if string(got) != string(want) {
				t.Fatalf("%s: IR 不同\n--- Go ---\n%.300s\n--- C++ ---\n%.300s", p, want, got)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if compared < 30 {
		t.Fatalf("只比到 %d 个样本 —— 这条测试会空转", compared)
	}
	t.Logf("IR 逐字节一致：%d 个（跳过 Go 也编不过的 %d 个）", compared, skipped)
}

// TestNativeIRHandlesLocalIncludeAndLibrary 守"本地 include 与 -L 库"这条路径。
// 它们是 M4 最后接上的部分，也是最容易在 return 值/顺序上出错的部分。
func TestNativeIRHandlesLocalIncludeAndLibrary(t *testing.T) {
	root := repoRootForBackend(t)
	goBin := filepath.Join(root, "build", "hermes")
	nativeBin := filepath.Join(root, "build", "hermes-native")
	dir := t.TempDir()
	libDir := filepath.Join(dir, "libs")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// 用一个真实的 .hbc 当本地库：直接用 archive.Write 造（`hermes ar` 只读不写，
	// 写归档是 `hermes link` 的事）。这样样本与判据都不依赖别的命令。
	depIR, derr := exec.Command(goBin, "ir", filepath.Join(root, "examples", "arrays", "length.hermes")).Output()
	if derr != nil {
		t.Fatalf("生成依赖 IR 失败: %v", derr)
	}
	hbc, werr := archive.Write([]archive.Member{{Name: "dep", IR: depIR}})
	if werr != nil {
		t.Fatalf("archive.Write: %v", werr)
	}
	if err := os.WriteFile(filepath.Join(libDir, "libdep.hbc"), hbc, 0o644); err != nil {
		t.Fatal(err)
	}

	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("dep.h", "export struct Dep { int n; }\n")
	write("m.hermes", "#include \"dep.h\"\n#link \"dep\"\nexport workflow w() input Dep entry -> string { return \"ok\"; }\n")

	args := []string{"ir", filepath.Join(dir, "m.hermes"), "-L", libDir}
	want, werr := exec.Command(goBin, args...).Output()
	got, gerr := exec.Command(nativeBin, args...).Output()
	// 判据是**与 Go 一致**，不是"必须成功"：Go 侧对本地库有自己的解析规则，
	// 这里不假设它一定成功——两侧同进同退才是等价。
	if (werr == nil) != (gerr == nil) {
		t.Fatalf("成败不一致：Go err=%v C++ err=%v\nGo 输出: %.300s\nC++ 输出: %.300s",
			werr, gerr, want, got)
	}
	if werr == nil && string(got) != string(want) {
		t.Fatalf("本地 include + -L 库的 IR 不同\n--- Go ---\n%.300s\n--- C++ ---\n%.300s", want, got)
	}
	if werr == nil {
		t.Log("本地 include + -L 库：两侧都成功且 IR 相同")
	} else {
		t.Logf("本地 include + -L 库：两侧都拒绝（Go 的规则如此）: %v", werr)
	}

	// 去掉 -L：两侧都必须失败（严格模式下不允许"悄悄少一条依赖"）。
	noLib := []string{"ir", filepath.Join(dir, "m.hermes")}
	if _, err := exec.Command(goBin, noLib...).Output(); err == nil {
		t.Fatal("Go 侧缺 -L 应失败")
	}
	if _, err := exec.Command(nativeBin, noLib...).Output(); err == nil {
		t.Fatal("native 侧缺 -L 应失败")
	}
}

func runIn(dir, bin string, args ...string) ([]byte, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	return cmd.Output()
}
