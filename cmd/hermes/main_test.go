package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain 给整个包找一个可用的 hermes-native。
//
// 为什么需要：包内测试大量走**进程内**的 run()，而 `explain`/`version` 已经
// 转发给 C++ 主程序。进程内时 os.Executable() 指向 Go 的测试二进制，同目录下
// 当然没有 hermes-native —— 测试会以"找不到二进制"失败，而不是真的验到东西。
// 所以在这里统一指到仓库的 build/ 产物。
func TestMain(m *testing.M) {
	if os.Getenv("HERMES_NATIVE_BIN") == "" {
		if wd, err := os.Getwd(); err == nil {
			cand := filepath.Join(wd, "..", "..", "build", "hermes-native")
			if st, err := os.Stat(cand); err == nil && !st.IsDir() {
				os.Setenv("HERMES_NATIVE_BIN", cand)
			}
		}
	}
	os.Exit(m.Run())
}
