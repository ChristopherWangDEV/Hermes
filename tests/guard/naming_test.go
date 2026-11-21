// 本文件守 ARCH §11.1 的 C++ 命名约定：**一律 `cpp`，不用 `cxx`**。
//
// 这条约定曾经有个例外（"既有目录 native/cxx 保留不动，因为改名收益为零"）。
// 2026-11-04 用户要求改名，例外消失了 —— 而**例外消失的那一刻最值得加一条守卫**：
// 没有它，下次有人手滑新建 native/cxx2 或 abc.cxx，没人会发现，直到两份命名并存。
package guard_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// skipNamingDirs 是遍历时跳过的目录：构建产物与依赖缓存里出现什么名字都不算数。
var skipNamingDirs = map[string]bool{
	"build": true, ".git": true, "target": true, "node_modules": true,
}

// TestNoCxxNaming 扫两个东西：叫 `cxx` 的目录，与后缀是 `.cxx` 的文件。
//
// 注意**不禁止**字符串 `cxx` 出现在代码里：`std::__cxx11::` 是 libstdc++ 的
// 符号前缀，正是我们要检测的对象（`TestParserArchiveHasNoLibstdcxx`）。
// 禁的是**路径名**，不是文本。
func TestNoCxxNaming(t *testing.T) {
	root := repoRoot(t)

	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		base := filepath.Base(path)
		if info.IsDir() {
			if skipNamingDirs[base] {
				return filepath.SkipDir
			}
			if base == "cxx" {
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders, "目录 "+rel)
				return filepath.SkipDir
			}
			return nil
		}
		if strings.EqualFold(filepath.Ext(path), ".cxx") {
			rel, _ := filepath.Rel(root, path)
			offenders = append(offenders, "文件 "+rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历失败: %v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("发现 cxx 命名（ARCH §11.1：一律用 cpp）：\n  %s",
			strings.Join(offenders, "\n  "))
	}

	// 反向保险：确认前端目录**确实**叫 cpp 且非空。
	// 没有这一条，把 native/cpp 整个删掉也会让上面的检查通过。
	dir := filepath.Join(root, "native", "cpp")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读不到 native/cpp：%v", err)
	}
	if len(entries) == 0 {
		t.Fatal("native/cpp 是空的 —— 扫了个寂寞")
	}
}
