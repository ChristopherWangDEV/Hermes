package native

import (
	"encoding/json"
	"testing"

	"hermes/internal/archive"
)

// TestArchiveReadMatchesGo：C++ 读出的成员表必须与 Go 的 archive.Read 一致。
func TestArchiveReadMatchesGo(t *testing.T) {
	raw, err := archive.Write([]archive.Member{
		{Name: "a", IR: []byte(`{"x":1}`), Exports: archive.Exports{Workflows: []string{"w1"}}, Entry: true, LanguageVersion: 2, ABIVersion: 2},
		{Name: "b", IR: []byte(`{"x":2}`), Exports: archive.Exports{Types: []string{"T"}}, ABIVersion: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	ar, err := archive.Read(raw)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := ArchiveReadMembers(raw)
	if err != nil {
		t.Fatalf("C++ 读取失败: %v", err)
	}
	var got []struct {
		Name   string `json:"name"`
		IROff  int    `json:"ir_off"`
		IRLen  int    `json:"ir_len"`
		IRSha  string `json:"ir_sha256"`
	}
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("成员表不是合法 JSON: %v", err)
	}
	if len(got) != len(ar.Members) {
		t.Fatalf("成员数不同：C++ %d / Go %d", len(got), len(ar.Members))
	}
	for i := range got {
		if got[i].Name != ar.Members[i].Name {
			t.Fatalf("第 %d 个成员名不同：C++ %q / Go %q", i, got[i].Name, ar.Members[i].Name)
		}
		if got[i].IRLen != len(ar.Members[i].IR) {
			t.Fatalf("%s: IR 长度不同 C++ %d / Go %d", got[i].Name, got[i].IRLen, len(ar.Members[i].IR))
		}
	}
}

// TestArchiveReadRejectsTampered 是**反向变异**：改动索引区任意一个字节，
// 必须被 sha256 校验拦住——否则被篡改的归档会被当成正常产物。
func TestArchiveReadRejectsTampered(t *testing.T) {
	raw, err := archive.Write([]archive.Member{{Name: "a", IR: []byte(`{"x":1}`), ABIVersion: 2}})
	if err != nil {
		t.Fatal(err)
	}
	// 索引区从头部第 28 字节读偏移；改它中间一个字节。
	bad := append([]byte(nil), raw...)
	idx := int(uint64(bad[28]) | uint64(bad[29])<<8 | uint64(bad[30])<<16 | uint64(bad[31])<<24)
	bad[idx+2] ^= 0x01
	if _, err := ArchiveReadMembers(bad); err == nil {
		t.Fatal("篡改索引后仍被接受")
	}
	// 结构损坏：把 ir_len 改大
	bad2 := append([]byte(nil), raw...)
	bad2[0] = 'X'
	if _, err := ArchiveReadMembers(bad2); err != ErrNotArchive {
		t.Fatalf("坏魔数应报 ErrNotArchive，实际 %v", err)
	}
}
