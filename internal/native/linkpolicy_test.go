package native

import (
	"testing"

	"hermes/internal/linkpolicy"
)

// TestLinkPolicyMatchesGo 是**许可红线**的等价判据：C++ 的闭集判据必须与
// Go 的 `linkpolicy.Match` 对同一批输入给出相同结论。
//
// 为什么这条必须存在：红线判据有两份实现是危险信号。判据（而不是注释）
// 是唯一能保证两份不漂移的东西——而这里漂移的后果是"GPL 库被放进来"。
func TestLinkPolicyMatchesGo(t *testing.T) {
	inputs := []string{
		// 表里的每一条（含路径与大写变体）
		"/usr/lib/libstdc++.so.6", "libstdc++.a", "/usr/lib/libsupc++.dylib",
		"libgcc_s.so.1", "LIBGOMP.so", "libquadmath.a",
		// 合法库：不能被误判
		"/usr/lib/libc++.1.dylib", "libc++.a", "libSystem.B.dylib", "libc.so.6",
		"libm.so", "libresolv.so", "libcrypto.a", "libssl.so",
		// 边界：名字只含一部分
		"mylibstdc++x.so", "xlibgomp", "gomp", "stdc++",
	}
	for _, in := range inputs {
		wantRule, wantOK := linkpolicy.Match(in)
		gotName, gotLic := LinkPolicyMatch(in)
		if wantOK != (gotName != "") {
			t.Fatalf("%q: Go=%v C++=%q（结论不一致）", in, wantOK, gotName)
		}
		if wantOK {
			if gotName != wantRule.Name {
				t.Fatalf("%q: 规则名不同 Go=%q C++=%q", in, wantRule.Name, gotName)
			}
			if gotLic != wantRule.Licence {
				t.Fatalf("%q: 许可不同 Go=%q C++=%q", in, wantRule.Licence, gotLic)
			}
		}
	}
}
