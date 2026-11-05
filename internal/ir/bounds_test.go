package ir

import (
	"os"
	"path/filepath"
	"testing"
)

// TestGoldenBoundsMatchRecompute 用**独立实现**复核 golden 里的 max_spans。
//
// codegen 在 C++ 里按 §5.1 算，这里在 Go 里再算一遍。两边用不同语言、
// 不同遍历顺序得到同一个数，才算真的钉住了那条代数；只靠 golden 逐字节
// 比对做不到这件事 —— 换了实现后 golden 会跟着变，而"变对了吗"没人知道。
func TestGoldenBoundsMatchRecompute(t *testing.T) {
	goldens, err := filepath.Glob("../../schemas/golden/ir-*.json")
	if err != nil || len(goldens) == 0 {
		t.Fatalf("扫不到 IR golden: %v", err)
	}

	for _, path := range goldens {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			d, ds := Load(data)
			if len(ds) != 0 {
				t.Fatalf("golden 自己不合格：%s", renderDiags(ds))
			}
			for i := range d.Workflows {
				if msg := CheckBounds(&d.Workflows[i]); msg != "" {
					t.Errorf("%s", msg)
				}
			}
		})
	}
}

// TestRecomputeRejectsUnknownKind 证明新增节点种类时复算会**报错**而不是返回 0。
//
// 返回 0 是最坏的退化：上界变小 → 运行期正确行为被判成 compiler bug，
// 而且没有任何测试会红。
func TestRecomputeRejectsUnknownKind(t *testing.T) {
	w := &Workflow{
		Name: "x",
		Root: 0,
		Nodes: []Node{{
			ID: 0, K: "future_kind", Loc: Loc{M: 0, L: 1, C: 1, EL: 1, EC: 2},
		}},
	}
	if got := RecomputeMaxSpans(w); got != -1 {
		t.Fatalf("未知节点种类应返回 -1，实际 %d", got)
	}
	if msg := CheckBounds(w); msg == "" {
		t.Fatal("未知节点种类时 CheckBounds 必须报出，而不是静默通过")
	}
}
