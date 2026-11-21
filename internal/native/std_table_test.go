package native

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"testing"

	"hermes/internal/std"
)

// TestStdTableMatchesGo 是 M1 的判据：C++ 侧生成的内置模块表必须与
// `internal/std` **逐个同名、逐字节同内容**。
//
// 为什么钉到 sha256 而不是只比名字：名字对、内容错的表会让 `#link <std/net>`
// 的 member_sha256 与 Go 版不同 —— 产物字节变了，而错误表现为"签名对不上"。
func TestStdTableMatchesGo(t *testing.T) {
	n := StdCount()
	if n == 0 {
		t.Fatal("C++ 侧没有内置模块表（生成步骤漏了？）")
	}
	var gotNames []string
	for i := 0; i < n; i++ {
		name := StdName(i)
		gotNames = append(gotNames, name)
	}
	sort.Strings(gotNames)

	wantNames := append([]string(nil), std.Names()...)
	sort.Strings(wantNames)
	if len(gotNames) != len(wantNames) {
		t.Fatalf("模块数不同：C++=%v Go=%v", gotNames, wantNames)
	}
	for i := range wantNames {
		if gotNames[i] != wantNames[i] {
			t.Fatalf("第 %d 个模块名不同：C++=%q Go=%q", i, gotNames[i], wantNames[i])
		}
	}

	for _, name := range wantNames {
		want, ok := std.Source(name)
		if !ok {
			t.Fatalf("Go 侧读不到模块 %q", name)
		}
		got, err := StdSource(name)
		if err != nil {
			t.Fatalf("C++ 侧读不到模块 %q：%v", name, err)
		}
		if len(got) != len(want) {
			t.Fatalf("模块 %q 长度不同：C++=%d Go=%d", name, len(got), len(want))
		}
		gh := sha256.Sum256(got)
		wh := sha256.Sum256(want)
		if hex.EncodeToString(gh[:]) != hex.EncodeToString(wh[:]) {
			t.Fatalf("模块 %q 内容不同：C++=%s Go=%s", name,
				hex.EncodeToString(gh[:])[:16], hex.EncodeToString(wh[:])[:16])
		}
	}
	t.Logf("内置模块表一致：%d 个（%v）", n, gotNames)
}
