package backend

import (
	"os"
	"path/filepath"
	"testing"

	"hermes/internal/native"
)

// TestUsesNetMatchesNative 是 UsesNet 判定的等价判据：对**全部 golden IR**，
// C++ 的判定与这里（Go 侧 `UsesNet`）必须一致。
//
// 为什么放在 backend 包里：`native` 不能反向依赖 `backend`（会成环），
// 而这条判据需要两边——放在"被搬的一方"（backend）最自然。
//
// 为什么重要：它决定"要不要把 Go 网络归档链进产物"。判错有两个方向：
// 说不需要 → 产物缺符号（链接期就炸，代价可控）；说需要 → 把 Go runtime
// 塞进一个本不需要它的产物（**红线问题**：裸机目标因此会被直接拒绝）。
func TestUsesNetMatchesNative(t *testing.T) {
	dir := filepath.Join("..", "..", "tests", "ir", "golden")
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("没有 golden 目录: %v", err)
	}
	compared, withNet := 0, 0
	for _, e := range ents {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		doc, ds, lerr := native.LoadIR(raw)
		if lerr != nil || len(ds) != 0 || doc == nil {
			continue
		}
		want := UsesNet(doc)
		got, gerr := native.IRUsesNet(raw)
		if gerr != nil {
			t.Fatalf("%s: C++ 判定失败: %v", e.Name(), gerr)
		}
		if got != want {
			t.Fatalf("%s: 判定不同 Go=%v C++=%v", e.Name(), want, got)
		}
		compared++
		if want {
			withNet++
		}
	}
	if compared == 0 {
		t.Fatal("一份 golden 都没比到")
	}
	if withNet == 0 {
		t.Fatal("样本里没有需要网络的 IR —— 这条测试对 true 分支是空转")
	}
	t.Logf("UsesNet 一致：%d 份（其中 %d 份需要网络）", compared, withNet)
}
