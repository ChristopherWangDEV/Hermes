package backend

import (
	"os"
	"path/filepath"
	"testing"

	"hermes/internal/native"
)

// TestCBackendEquivalence 是"去 Go 化"的驱动测试：C 后端的 C++ 实现
// （迁移目标）一旦声称支持某份 IR，它的输出必须与 Go 实现**逐字节相同**。
//
// 这条测试为什么必须先于移植工作存在：IR → C 这一千多行的输出代码，
// 靠"重写一遍然后肉眼比对"是不可能的。等价性是唯一可信的判据。
//
// 迁移期间它大部分时候是 skip（C++ 侧返回"未覆盖"）。每覆盖一类节点，
// 对应的 golden 就会真的进入比对 —— 于是进度是**可见的**，而不是靠感觉。
func TestCBackendEquivalence(t *testing.T) {
	root := repoRootEquiv(t)
	goldens, err := filepath.Glob(filepath.Join(root, "schemas", "golden", "ir-*.json"))
	if err != nil || len(goldens) == 0 {
		t.Fatalf("找不到 golden IR（schemas/golden/ir-*.json）: %v", err)
	}

	compared, skipped := 0, 0
	for _, path := range goldens {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("读不到 %s: %v", path, err)
			}
			doc, diags, loadErr := native.LoadIR(raw)
			if loadErr != nil {
				t.Fatalf("%s 校验失败: %v", name, loadErr)
			}
			if len(diags) > 0 {
				t.Fatalf("%s 加载失败: %v", name, diags)
			}
			goBE, ok := For("c")
			if !ok {
				t.Fatal("找不到 c 后端")
			}
			want, err := goBE.Generate(doc)
			if err != nil {
				t.Skipf("Go 版不支持这份 IR（%v）—— 等价性无从谈起", err)
			}
			got, err := NativeC().Generate(doc)
			if err != nil {
				if ErrNotCovered(err) {
					skipped++
					t.Skipf("C++ 版尚未覆盖 %s：%v", name, err)
				}
				t.Fatalf("C++ 版报错（不是'未覆盖'）: %v", err)
			}
			compared++
			if string(got) != string(want) {
				t.Fatalf("%s: C++ 版输出与 Go 版不一致（长度 %d vs %d）\n--- Go 前 400 字节 ---\n%s\n--- C++ 前 400 字节 ---\n%s",
					name, len(want), len(got), firstN(want, 400), firstN(got, 400))
			}
		})
	}
	t.Logf("已比对 %d 份，未覆盖 %d 份（总数 %d）", compared, skipped, len(goldens))
}

func firstN(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	return string(b)
}

func repoRootEquiv(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("找不到仓库根（go.mod）")
		}
		dir = parent
	}
}
