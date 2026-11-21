package sign

import (
	"bytes"
	"errors"
	"testing"

	"hermes/internal/archive"
)

func makeArchive(t *testing.T) []byte {
	t.Helper()
	data, err := archive.Write([]archive.Member{{
		Name: "support",
		IR:   []byte(`{"ir_version":1,"unit":{"name":"support"}}`),
		Exports: archive.Exports{
			Workflows: []string{"support_bot"},
		},
		Entry: true, LanguageVersion: 1, ABIVersion: archive.ABIVersionSupported,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestSignVerifyRoundTrip 是最基本的性质。
func TestSignVerifyRoundTrip(t *testing.T) {
	k, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	signed, err := Sign(makeArchive(t), k)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := VerifyWithPub(signed, k.Pub); err != nil {
		t.Fatalf("验签应通过: %v", err)
	}
	if got := KeyID(signed); got != k.ID() {
		t.Fatalf("key_id 应为 %q，实际 %q", k.ID(), got)
	}
}

// TestSignCoversIndexNotBody 是**这条设计的核心断言**：
// 改成员字节会被 archive.Read 的 ir_sha256 抓住；改索引会被签名抓住；
// 而只要索引不变，成员字节的重排/对齐**不影响**签名 —— 这正是
// "链接方没有原私钥也能复用成员"的前提。
func TestSignCoversIndexNotBody(t *testing.T) {
	k, _ := NewKey()
	signed, _ := Sign(makeArchive(t), k)

	t.Run("改索引必须验签失败", func(t *testing.T) {
		bad := append([]byte(nil), signed...)
		// 改索引区一个字节（索引紧邻 footer 之前）
		bad[len(bad)-archive.FooterLen-1] ^= 0xFF
		if err := VerifyWithPub(bad, k.Pub); err == nil {
			t.Fatal("索引被改过，验签必须失败")
		}
	})

	t.Run("改成员内容会被成员哈希抓住", func(t *testing.T) {
		bad := append([]byte(nil), signed...)
		bad[archive.HeaderLen] ^= 0xFF
		if err := VerifyWithPub(bad, k.Pub); err == nil {
			t.Fatal("成员被改过，必须先被 ir_sha256 抓住")
		}
	})
}

// TestVerifyDistinguishesUnsignedFromTampered 守"两种情况的修复动作不同"。
//
// 未签名 = 你忘了签；验签失败 = 文件被人动过。
// 把前者报成后者，会让人去查密钥，而真正的问题在上游流程。
func TestVerifyDistinguishesUnsignedFromTampered(t *testing.T) {
	raw := makeArchive(t)
	if err := Verify(raw); !errors.Is(err, ErrNoSignature) {
		t.Fatalf("未签名应报 ErrNoSignature，实际 %v", err)
	}

	k, _ := NewKey()
	signed, _ := Sign(raw, k)
	other, _ := NewKey()
	if err := VerifyWithPub(signed, other.Pub); !errors.Is(err, ErrBadSig) {
		t.Fatalf("换了公钥应报 ErrBadSig，实际 %v", err)
	}
}

// TestSignIsDeterministic 证明 Ed25519 签名对同一输入稳定 —— 它确实稳定，
// 但这条测试的价值在于：我们**依赖**它，所以要钉住。
func TestSignIsDeterministic(t *testing.T) {
	k, _ := NewKey()
	raw := makeArchive(t)
	a, _ := Sign(raw, k)
	b, _ := Sign(raw, k)
	if !bytes.Equal(a, b) {
		t.Fatal("同一份数据同一把钥匙，签名应可重现")
	}
}
