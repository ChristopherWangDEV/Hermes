// 本文件守一条：**发布物里不许有本机信息。**
//
// 起因：`libhermesrt.a` 里出现过 5 处
//
//	/Users/<名字>/Library/Developer/Toolchains/swift-6.3.3-RELEASE.xctoolchain/…
//
// 那是 cgo 编译 C 代码时记进 DWARF 的绝对路径。它会随发布包发给所有人：
// 泄露构建者的用户名与工具链布局，也让"同一个 tag 编出同样的东西"不成立。
package guard_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestReleaseHasNoLocalPaths 检查构建产物与发布包里没有构建机的绝对路径。
func TestReleaseHasNoLocalPaths(t *testing.T) {
	root := repoRoot(t)

	targets := []string{
		filepath.Join(root, "build", "hermes"),
		filepath.Join(root, "build", "libhermesrt.a"),
	}
	if p, _ := filepath.Glob(filepath.Join(root, "build", "libhermesrt.*")); len(p) > 0 {
		targets = append(targets, p...)
	}

	// 这些前缀说明"这里有构建机的路径"。
	markers := []string{"/Users/", "/home/", `C:\Users\`}

	scanned := 0
	for _, f := range targets {
		if _, err := os.Stat(f); err != nil || strings.HasSuffix(f, ".h") {
			continue
		}
		scanned++
		out, err := exec.Command("strings", "-a", f).Output()
		if err != nil {
			t.Skipf("没有 strings 命令: %v", err)
		}
		text := string(out)
		for _, m := range markers {
			if strings.Contains(text, m) {
				// 打印第一条命中的上下文，便于定位。
				idx := strings.Index(text, m)
				end := idx + 120
				if end > len(text) {
					end = len(text)
				}
				t.Fatalf("%s 里有本机路径 —— 发布物不该带构建机的信息：\n%s",
					filepath.Base(f), strings.ReplaceAll(text[idx:end], "\x00", " "))
			}
		}
	}
	if scanned == 0 {
		t.Skip("没有构建产物（先跑 scripts/build.sh）")
	}
}
