package native

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"hermes/internal/archive"
)

// TestArchiveIndexHashMatchesGo 是 M2 的判据：对同一批 `.hbc` 样本，
// C++ 算出的 member_sha256 必须与 Go 的 `archive.IndexBytes` + sha256 相同。
//
// 为什么这条必须有：`#link "lib"` 的 requires 里那个哈希就是它。算错了不会
// 立刻报错——产物照样生成，只有到分发链路上验签时才发现"对不上"。
func TestArchiveIndexHashMatchesGo(t *testing.T) {
	cases := []struct {
		name    string
		members []archive.Member
	}{
		{"single", []archive.Member{{Name: "a", IR: []byte(`{"k":1}`)}}},
		{"two", []archive.Member{
			{Name: "b", IR: []byte(`{"k":2}`)},
			{Name: "a", IR: []byte(`{"k":1}`)},
		}},
		{"empty-ir", []archive.Member{{Name: "z", IR: []byte(``)}}},
	}
	for _, c := range cases {
		raw, err := archive.Write(c.members)
		if err != nil {
			t.Fatalf("%s: archive.Write: %v", c.name, err)
		}
		// Go 参照实现
		ar, err := archive.Read(raw)
		if err != nil {
			t.Fatalf("%s: archive.Read: %v", c.name, err)
		}
		sum := sha256.Sum256(ar.IndexBytes(raw))
		want := hex.EncodeToString(sum[:])

		got, err := ArchiveIndexSHA256(raw)
		if err != nil {
			t.Fatalf("%s: C++ 计算失败: %v", c.name, err)
		}
		if got != want {
			t.Fatalf("%s: 索引哈希不同\n  C++=%s\n  Go =%s", c.name, got, want)
		}
	}
}

// TestArchiveIndexRejectsBadInput 是**反向变异**：坏输入必须报错，不能崩溃、
// 更不能"截断着算一个哈希"（那会给出看起来正常但错误的指纹）。
func TestArchiveIndexRejectsBadInput(t *testing.T) {
	raw, err := archive.Write([]archive.Member{{Name: "a", IR: []byte(`{"k":1}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ArchiveIndexSHA256(nil); err != ErrNotArchive {
		t.Fatalf("空输入应报 ErrNotArchive，实际 %v", err)
	}
	// 截断到各种长度：都必须报错而不是返回哈希。
	for _, n := range []int{1, 4, 64, 127, 128, 129, len(raw) - 1} {
		if n <= 0 || n >= len(raw) {
			continue
		}
		if _, err := ArchiveIndexSHA256(raw[:n]); err == nil {
			t.Fatalf("截断到 %d 字节必须报错", n)
		}
	}
	// 魔数坏掉
	bad := append([]byte(nil), raw...)
	bad[0] = 'X'
	if _, err := ArchiveIndexSHA256(bad); err != ErrNotArchive {
		t.Fatalf("坏魔数应报 ErrNotArchive，实际 %v", err)
	}
	// 索引长度越界（把 index_len 写成巨大值）
	over := append([]byte(nil), raw...)
	for i := 36; i < 44; i++ {
		over[i] = 0xff
	}
	if _, err := ArchiveIndexSHA256(over); err != ErrArchiveMalformed {
		t.Fatalf("索引越界应报 ErrArchiveMalformed，实际 %v", err)
	}
}
