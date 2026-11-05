package archive

import (
	"bytes"
	"errors"
	"testing"
)

func sample() []Member {
	return []Member{
		{Name: "support", IR: []byte(`{"ir_version":1,"unit":{"name":"support"}}`),
			Exports: Exports{Workflows: []string{"support_bot"}, Types: []string{"Intent"}},
			Entry:   true, LanguageVersion: 1, ABIVersion: 1},
		{Name: "base", IR: []byte(`{"ir_version":1,"unit":{"name":"base"}}`),
			Exports: Exports{Types: []string{"OrderInfo"}}, LanguageVersion: 1, ABIVersion: 1},
	}
}

// TestRoundTrip 是最基本的性质：写出来的读得回来，且成员字节一致。
func TestRoundTrip(t *testing.T) {
	data, err := Write(sample())
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	a, err := Read(data)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(a.Members) != 2 {
		t.Fatalf("应有 2 个成员，实际 %d", len(a.Members))
	}
	// 成员按名字典序：base 在前。
	if a.Members[0].Name != "base" || a.Members[1].Name != "support" {
		t.Fatalf("成员应按名字典序，实际 %q, %q", a.Members[0].Name, a.Members[1].Name)
	}
	for _, m := range a.Members {
		for _, want := range sample() {
			if want.Name == m.Name && !bytes.Equal(want.IR, m.IR) {
				t.Fatalf("成员 %q 的字节往返后不一致", m.Name)
			}
		}
	}
}

// TestWriteIsDeterministic 是 R-HF-03：同输入两次写，字节相等。
//
// 这条不是"锦上添花"：`.hbc` 的 `ir_sha256` 进签名链，字节不稳等于签名不稳。
func TestWriteIsDeterministic(t *testing.T) {
	a, _ := Write(sample())
	for i := 0; i < 20; i++ {
		b, _ := Write(sample())
		if !bytes.Equal(a, b) {
			t.Fatalf("第 %d 次写出的字节不同（长度 %d vs %d）", i, len(a), len(b))
		}
	}
	// 调用方给的顺序不该影响产物。
	rev := sample()
	rev[0], rev[1] = rev[1], rev[0]
	c, _ := Write(rev)
	if !bytes.Equal(a, c) {
		t.Fatal("成员顺序应被规范化（名字典序），否则 link 的字节可重现性依赖调用方")
	}
}

// TestReadRejectsCorruption 逐项验证加载期的结构检查。
func TestReadRejectsCorruption(t *testing.T) {
	good, _ := Write(sample())

	cases := []struct {
		name string
		mut  func([]byte) []byte
		want error
	}{
		{"magic", func(b []byte) []byte { b[0] = 'X'; return b }, ErrBadMagic},
		{"version", func(b []byte) []byte { b[4] = 9; return b }, ErrBadVersion},
		{"reserved", func(b []byte) []byte { b[100] = 1; return b }, ErrReserved},
		{"index_sha", func(b []byte) []byte {
			// 改索引区一个字节（索引在末尾 footer 之前）
			b[len(b)-FooterLen-1] ^= 0xFF
			return b
		}, ErrIndexSHA},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := append([]byte(nil), good...)
			_, err := Read(c.mut(b))
			if err == nil {
				t.Fatal("应报错")
			}
			if c.want != nil && !errIs(err, c.want) {
				t.Fatalf("期望 %v，实际 %v", c.want, err)
			}
		})
	}
}

// TestMemberHashMismatch 证明成员内容被改过会被抓（即使索引没动）。
func TestMemberHashMismatch(t *testing.T) {
	data, _ := Write(sample())
	// 改 payload 区第一个字节（成员 base 的内容）
	data[HeaderLen] ^= 0xFF
	if _, err := Read(data); !errors.Is(err, ErrMemberHash) {
		t.Fatalf("成员哈希不符应报 ErrMemberHash，实际 %v", err)
	}
}

func errIs(err, target error) bool { return errors.Is(err, target) }
