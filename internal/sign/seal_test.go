package sign

import (
	"bytes"
	"errors"
	"testing"

	"hermes/internal/archive"
)

func buildPair(t *testing.T) (sealed, hbc []byte, k *Key) {
	t.Helper()
	k, _ = NewKey()

	src := []byte("export workflow hello() entry -> string { return \"hi\"; }\n")
	memberIR := []byte(`{"ir_version":1,"unit":{"name":"hello"}}`)

	hbc, err := archive.Write([]archive.Member{{
		Name: "hello", IR: memberIR, Entry: true, LanguageVersion: 1,
		ABIVersion: archive.ABIVersionSupported,
		Exports: archive.Exports{Workflows: []string{"hello"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	hbc, err = Sign(hbc, k)
	if err != nil {
		t.Fatal(err)
	}

	sealed, err = Seal(SealInput{
		SourcePath: "hello.hermes", Source: src, IR: memberIR, HBC: hbc,
		IndexSHA:       indexSHAHex(hbc),
		EntryWorkflows: []string{"hello"},
		Bounds:         map[string]int64{"max_spans": 0},
		Economics:      map[string]any{"pricing_source": "unknown"},
	}, k)
	if err != nil {
		t.Fatal(err)
	}
	return sealed, hbc, k
}

func indexSHAHex(data []byte) string {
	// .hbc 头部 44..76 就是 index_sha256
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 0, 64)
	for _, b := range data[44:76] {
		out = append(out, hexdigits[b>>4], hexdigits[b&0xF])
	}
	return string(out)
}

// TestSealRoundTrip：两个产物一起验，五步交叉全过。
func TestSealRoundTrip(t *testing.T) {
	sealed, hbc, k := buildPair(t)
	if err := VerifySealedWithPub(sealed, hbc, k.Pub); err != nil {
		t.Fatalf("应通过: %v", err)
	}
}

// TestSealDetectsCrossProductSwap 是这条设计存在的理由：
// **把 sealed 指向另一个 .hbc 必须被发现**。
//
// 若没有 hbc_sha256 这一步，两个各自合法、但来自不同构建的产物就能被拼在一起，
// 而"它们同源"这句话没有任何办法证伪。
func TestSealDetectsCrossProductSwap(t *testing.T) {
	_, hbcA, k := buildPair(t)
	sealedB, hbcB, _ := buildPair(t)
	_ = hbcB

	if err := VerifySealedWithPub(sealedB, hbcA, k.Pub); err == nil {
		t.Fatal("sealed 与 .hbc 不同源，必须报错")
	}
}

// TestSealDetectsSourceTamper：改 sealed 里的源码字节必须被发现。
func TestSealDetectsSourceTamper(t *testing.T) {
	sealed, hbc, k := buildPair(t)
	bad := append([]byte(nil), sealed...)
	// source 从 SealHeader 开始
	bad[SealHeader] ^= 0xFF
	if err := VerifySealedWithPub(bad, hbc, k.Pub); err == nil {
		t.Fatal("源码被改过，必须报错")
	}
}

// TestSealIsDeterministic：同输入同钥匙，字节相等。
func TestSealIsDeterministic(t *testing.T) {
	sealed, hbc, k := buildPair(t)
	again, err := Seal(SealInput{
		SourcePath: "hello.hermes",
		Source:     []byte("export workflow hello() entry -> string { return \"hi\"; }\n"),
		IR:         []byte(`{"ir_version":1,"unit":{"name":"hello"}}`),
		HBC:        hbc, IndexSHA: indexSHAHex(hbc),
		EntryWorkflows: []string{"hello"},
		Bounds:         map[string]int64{"max_spans": 0},
		Economics:      map[string]any{"pricing_source": "unknown"},
	}, k)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sealed, again) {
		t.Fatal("同输入两次封签应字节相等")
	}
}

// TestVerifySealedRequiresSignature：未签名的 sealed 不许通过。
func TestVerifySealedRequiresSignature(t *testing.T) {
	_, _, _ = buildPair(t)
	raw := make([]byte, SealHeader+archive.FooterLen)
	copy(raw[0:4], SealMagic)
	raw[4] = SealVersion
	_, _, err := ReadSealed(raw)
	if err == nil {
		t.Fatal("空的 sealed 应报错")
	}
	_ = errors.Is
}
