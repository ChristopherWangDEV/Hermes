package layout

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveReleaseLayout 守**发布包布局**：bin/hermes 与 lib/、include/ 平级。
//
// 这是"下载即用"的技术前提：二进制不能依赖"从仓库根目录运行"。
func TestResolveReleaseLayout(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "bin"))
	mustMkdir(t, filepath.Join(root, "lib"))
	mustMkdir(t, filepath.Join(root, "include", "hermes"))
	mustWrite(t, filepath.Join(root, "lib", "libhermesrt.a"), "x")
	mustWrite(t, filepath.Join(root, "lib", "hermes_rt.h"), "x")
	mustWrite(t, filepath.Join(root, "include", "hermes", "hermes_core.h"), "x")

	a := Resolve(filepath.Join(root, "bin", "hermes"), "")
	if a.RuntimeLib != filepath.Join(root, "lib", "libhermesrt.a") {
		t.Fatalf("应找到发布包的运行时库，实际 %q", a.RuntimeLib)
	}
	if a.RuntimeHeader != filepath.Join(root, "lib", "hermes_rt.h") {
		t.Fatalf("应找到 hermes_rt.h，实际 %q", a.RuntimeHeader)
	}
	if a.IncludeDir != filepath.Join(root, "include") {
		t.Fatalf("应找到 include 根，实际 %q", a.IncludeDir)
	}
	if err := Require(a); err != nil {
		t.Fatalf("资源齐全时不该报错: %v", err)
	}
}

// TestResolveDevLayout 守**开发布局**：仓库根下有 build/ 与 include/。
func TestResolveDevLayout(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "build"))
	mustMkdir(t, filepath.Join(root, "include", "hermes"))
	mustWrite(t, filepath.Join(root, "build", "libhermesrt.a"), "x")
	mustWrite(t, filepath.Join(root, "build", "libhermesrt.h"), "x") // 默认名
	mustWrite(t, filepath.Join(root, "include", "hermes", "hermes_core.h"), "x")

	a := Resolve("", root)
	if a.RuntimeLib != filepath.Join(root, "build", "libhermesrt.a") {
		t.Fatalf("应找到开发布局的运行时库，实际 %q", a.RuntimeLib)
	}
	// 头也可能叫 libhermesrt.h（跟着 -o 走的那种命名）。
	if a.RuntimeHeader != filepath.Join(root, "build", "libhermesrt.h") {
		t.Fatalf("应认出 libhermesrt.h，实际 %q", a.RuntimeHeader)
	}
}

// TestHermesHomeWins 守"显式指定优先"。
func TestHermesHomeWins(t *testing.T) {
	home := t.TempDir()
	mustMkdir(t, filepath.Join(home, "lib"))
	mustMkdir(t, filepath.Join(home, "include", "hermes"))
	mustWrite(t, filepath.Join(home, "lib", "libhermesrt.a"), "x")
	mustWrite(t, filepath.Join(home, "include", "hermes", "hermes_core.h"), "x")
	t.Setenv("HERMES_HOME", home)

	a := Resolve("", t.TempDir())
	if a.RuntimeLib != filepath.Join(home, "lib", "libhermesrt.a") {
		t.Fatalf("HERMES_HOME 应优先，实际 %q", a.RuntimeLib)
	}
}

// TestRequireGivesActionableError 守"缺资源时给可操作的错误"。
func TestRequireGivesActionableError(t *testing.T) {
	err := Require(Assets{})
	if err == nil {
		t.Fatal("什么都没有时必须报错")
	}
	msg := err.Error()
	for _, want := range []string{"libhermesrt.a", "build.sh", "HERMES_HOME"} {
		if !contains(msg, want) {
			t.Fatalf("错误应包含 %q：%s", want, msg)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}
