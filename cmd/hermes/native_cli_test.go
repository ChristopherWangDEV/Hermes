package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestVersionComesFromNative 守"version 只有一份实现"：
// `hermes version` 的输出必须来自 C++ 主程序（hermes-native），而不是 Go。
//
// 判据用**只有 C++ 版才有的那个字段**：C++ 版没有 `go` 行（去 Go 之后
// "依赖哪个 Go 运行时"不再成立）。Go 版曾经有，所以"没有 go 行"恰好证明
// 这次请求没有走 Go。
func TestVersionComesFromNative(t *testing.T) {
	root := repoRootForBackend(t)
	bin := filepath.Join(root, "build", "hermes")
	out, err := exec.Command(bin, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("hermes version 失败: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{"hermes ", "build_id", "abi", "native_id", "platform", "cycle_arch"} {
		if !strings.Contains(got, want) {
			t.Errorf("输出缺少 %q\n%s", want, got)
		}
	}
	if strings.Contains(got, "\n  go ") {
		t.Fatalf("输出里还有 go 行 —— 说明这份输出不是 C++ 主程序给的\n%s", got)
	}
}

// TestNativeBinaryExists：Go 侧转发依赖它存在，缺了必须能被明确发现。
func TestNativeBinaryExists(t *testing.T) {
	root := repoRootForBackend(t)
	bin := filepath.Join(root, "build", "hermes-native")
	out, err := exec.Command(bin, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("hermes-native 不可用: %v\n%s", err, out)
	}
}
