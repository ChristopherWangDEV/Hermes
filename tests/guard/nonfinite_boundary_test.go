package guard_test

// 守「NaN / ±Inf **只在序列化边界拒**」这条裁决的**结构侧**（LANGUAGE.md §4.4）。
//
// 裁决本身是一句关于"位置"的话：构造、传递、比较都不拒，只在写出去的那一刻拒。
// 关于位置的话最容易在后续重构里失效 —— 谁在 `json.as_float` 或 `binop` 里
// 顺手加一句 `if math.IsNaN(x) { return err }`，看起来都是"更严谨了"，
// 而实际上把语言改成了"浮点一旦非有限就不可传递"，那会让 `0/0`、`Inf-Inf`
// 这类中间结果直接变成不可表达的表达式。
//
// 所以判据不写在注释里，写成一个可执行的扫描：全仓 Go 源码里
// `math.IsNaN` / `math.IsInf` 只允许出现在序列化边界那一个文件。
//
// 为什么扫源码而不是靠 review：这类改动是**加**代码，不是改代码 ——
// 它不会让任何现有测试变红，只会让语言的语义范围悄悄收窄。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// nonFiniteBoundaryFile 是唯一允许出现非有限值判定的文件。
const nonFiniteBoundaryFile = "cmd/hermes/jsonboundary.go"

// TestNonFiniteOnlyRejectedAtBoundary 扫全仓，断言判定点只有一处。
func TestNonFiniteOnlyRejectedAtBoundary(t *testing.T) {
	root := filepath.Join("..", "..")
	var hits []string
	scanned := 0

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "build", "node_modules", "testdata", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		text := string(src)
		if strings.Contains(text, "math.IsNaN") || strings.Contains(text, "math.IsInf") {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				rel = path
			}
			hits = append(hits, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历仓库失败：%v", err)
	}

	// 守卫自己也要被守：扫到的文件太少说明根目录找错了，此时"没有额外命中"
	// 是个空结论。
	if scanned < 40 {
		t.Fatalf("只扫到 %d 个 .go 文件，明显没扫对仓库根 —— 守卫本身失效了", scanned)
	}
	if len(hits) == 0 {
		t.Fatalf("连 %s 都没扫到：要么文件被删了，要么扫描器失效（判定点不可能为零）",
			nonFiniteBoundaryFile)
	}

	var extra []string
	for _, h := range hits {
		if h != nonFiniteBoundaryFile {
			extra = append(extra, h)
		}
	}
	if len(extra) > 0 {
		t.Errorf("NaN/Inf 的判定只能出现在 %s（序列化边界），这些文件也加了：\n  %s\n"+
			"裁决是「构造/传递/比较都不拒，只在写出去时拒」——"+
			"别处再加一道，等于把语言改成浮点一旦非有限就不可传递（见 LANGUAGE.md §4.4）",
			nonFiniteBoundaryFile, strings.Join(extra, "\n  "))
	}
}
