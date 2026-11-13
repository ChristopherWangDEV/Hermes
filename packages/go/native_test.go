package hermes

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestNativeRuntime 用真动态库跑一个真 workflow —— 这是"进程内"的证据：
// 全程没有监听端口，也没有任何 HTTP 客户端参与。
func TestNativeRuntime(t *testing.T) {
	lib, err := FindLibrary("")
	if err != nil {
		t.Skip(err)
	}
	root, _ := filepath.Abs("../..")
	bin := filepath.Join(root, "build", "hermes")
	if _, err := os.Stat(bin); err != nil {
		t.Skip("没有 build/hermes")
	}

	dir := t.TempDir()
	src := filepath.Join(dir, "echo.hermes")
	if err := os.WriteFile(src, []byte(
		"export struct In { string text; }\n"+
			"export workflow echo(input: In) entry -> string { return \"echo: {input.text}\"; }\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	hbc := filepath.Join(dir, "echo.hbc")
	if out, err := exec.Command(bin, "build", src, "-o", hbc).CombinedOutput(); err != nil {
		t.Fatalf("build 失败: %v\n%s", err, out)
	}

	rt, err := OpenRuntime(hbc, lib)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	defer rt.Close()

	got, err := rt.Run(`{"text":"hi"}`)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !strings.Contains(got, "echo: hi") {
		t.Fatalf("输出不对: %q", got)
	}

	// 缺失 unit 必须报错，且带 code。
	if _, err := rt.RunOn(filepath.Join(dir, "nope.hbc"), "{}"); err == nil {
		t.Fatal("缺失 unit 应报错")
	} else if ne, ok := err.(*NativeError); !ok || ne.Code == 0 {
		t.Fatalf("应带 code: %v", err)
	}
}
