package native

import (
	"os"
	"testing"

	"hermes/internal/objfile"
)

// SymbolState 是 C 侧三态的可读形式。
const (
	symMissing   = 0
	symDefined   = 1
	symUndefined = 2
)

// TestSymbolStateMatchesGo 是产物符号检查的等价判据：同一个符号在同一个文件里，
// C++（Rust 实现）与 Go 的 `objfile.Read` 必须给出相同状态。
//
// 为什么需要它：`hermes cc` 要证明产物**确实定义**了 per-unit 符号
// （`hermes_<unit>_<wf>`）。判错的两种后果都很糟——把"未定义"当成"已定义"
// 会交出运行期才炸的产物；反过来会把能用的产物判死。
func TestSymbolStateMatchesGo(t *testing.T) {
	bins := []string{"/bin/ls", "/usr/bin/env"}
	if exe, err := os.Executable(); err == nil {
		bins = append(bins, exe)
	}
	// 我们自己的产物：符号表里有 _main / main
	if _, err := os.Stat("../../build/hermes"); err == nil {
		bins = append(bins, "../../build/hermes")
	}

	checked := 0
	for _, bin := range bins {
		if _, err := os.Stat(bin); err != nil {
			continue
		}
		syms, err := objfile.Read(bin)
		if err != nil {
			continue
		}
		// 挑几个必然存在的符号 + 一个必然不存在的。
		names := []string{"main", "_main", "printf", "hermes_definitely_not_a_symbol_zzz"}
		for _, n := range names {
			want := symMissing
			switch {
			case syms.Defined[n]:
				want = symDefined
			case syms.Undefined[n]:
				want = symUndefined
			}
			got, err := SymbolState(bin, n)
			if err != nil {
				t.Fatalf("%s/%s: C++ 失败: %v", bin, n, err)
			}
			if got != want {
				t.Fatalf("%s/%s: 状态不同 Go=%d C++=%d", bin, n, want, got)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("一个样本都没比到 —— 这条测试会空转")
	}
	t.Logf("符号三态一致：%d 组", checked)
}
