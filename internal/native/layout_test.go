package native

import (
	"os"
	"path/filepath"
	"testing"

	"hermes/internal/layout"
)

// TestLayoutMatchesGo 是资源定位的等价判据：同一组 (可执行文件路径, 当前目录)，
// C++ 与 Go 必须找到**同一批**文件。
//
// 为什么这条重要：定位顺序错了不会报错，只会在"用户的机器上"找不到运行时库
// ——而开发机上两种布局都命中，测试会全绿。所以样本里必须同时包含
// "发布包布局"（bin/ + lib/）与"开发布局"（build/ + include/）。
func TestLayoutMatchesGo(t *testing.T) {
	root := t.TempDir()

	// 发布包布局：bin/hermes + lib/libhermesrt.a + lib/hermes_rt.h + include/hermes/
	rel := filepath.Join(root, "release")
	mustMkdir(t, filepath.Join(rel, "bin"))
	mustMkdir(t, filepath.Join(rel, "lib"))
	mustMkdir(t, filepath.Join(rel, "include", "hermes"))
	mustWrite(t, filepath.Join(rel, "bin", "hermes"), "x")
	mustWrite(t, filepath.Join(rel, "lib", "libhermesrt.a"), "x")
	mustWrite(t, filepath.Join(rel, "lib", "hermes_rt.h"), "x")
	mustWrite(t, filepath.Join(rel, "include", "hermes", "hermes_core.h"), "x")

	// 开发布局：build/libhermesrt.a + include/hermes/（当前目录命中）
	dev := filepath.Join(root, "devrepo")
	mustMkdir(t, filepath.Join(dev, "build"))
	mustMkdir(t, filepath.Join(dev, "include", "hermes"))
	mustWrite(t, filepath.Join(dev, "build", "libhermesrt.a"), "x")
	mustWrite(t, filepath.Join(dev, "include", "hermes", "hermes_core.h"), "x")

	cases := []struct{ name, exec, cwd string }{
		{"release", filepath.Join(rel, "bin", "hermes"), ""},
		{"dev", "", dev},
		{"nothing", "", filepath.Join(root, "empty")},
	}
	for _, c := range cases {
		want := layout.Resolve(c.exec, c.cwd)
		got := LayoutResolve(c.exec, c.cwd)
		if got.RuntimeLib != want.RuntimeLib {
			t.Errorf("%s: RuntimeLib 不同\n  Go =%q\n  C++=%q", c.name, want.RuntimeLib, got.RuntimeLib)
		}
		if got.IncludeDir != want.IncludeDir {
			t.Errorf("%s: IncludeDir 不同\n  Go =%q\n  C++=%q", c.name, want.IncludeDir, got.IncludeDir)
		}
		if got.Shim != want.Shim {
			t.Errorf("%s: Shim 不同\n  Go =%q\n  C++=%q", c.name, want.Shim, got.Shim)
		}
	}
}

// TestLayoutHeaderForMatchesGo：运行库头的推导（先 hermes_rt.h，再 <stem>.h）。
func TestLayoutHeaderForMatchesGo(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "libhermesrt.a"), "x")
	// 只有 <stem>.h
	mustWrite(t, filepath.Join(dir, "libhermesrt.h"), "x")
	if got, want := LayoutHeaderFor(filepath.Join(dir, "libhermesrt.a")),
		layout.HeaderFor(filepath.Join(dir, "libhermesrt.a")); got != want {
		t.Fatalf("<stem>.h 情形不同\n  Go =%q\n  C++=%q", want, got)
	}
	// 再放 hermes_rt.h：它优先于 <stem>.h
	mustWrite(t, filepath.Join(dir, "hermes_rt.h"), "x")
	if got, want := LayoutHeaderFor(filepath.Join(dir, "libhermesrt.a")),
		layout.HeaderFor(filepath.Join(dir, "libhermesrt.a")); got != want {
		t.Fatalf("hermes_rt.h 优先级不同\n  Go =%q\n  C++=%q", want, got)
	}
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, p, body string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
