package main

import (
	"os"
	"path/filepath"
	"testing"
)

// buildSigned 造一对已签名产物，返回 (hbc, sealed)。
func buildSigned(t *testing.T, dir string) (string, string) {
	t.Helper()
	key := filepath.Join(dir, "k.pem")
	if code := run([]string{"sign", "keygen", "-o", key}); code != 0 {
		t.Fatalf("keygen 退出码 %d", code)
	}
	hbc := filepath.Join(dir, "x.hbc")
	if code := run([]string{"build", "testdata/live.hermes", "-o", hbc, "-k", key}); code != 0 {
		t.Fatalf("build -k 退出码 %d", code)
	}
	sealed := filepath.Join(dir, "x.sealed.hermes")
	if _, err := os.Stat(sealed); err != nil {
		t.Fatalf("应产出 sealed：%v", err)
	}
	return hbc, sealed
}

// TestVerifyTwoProducts 是 T41 的完整验收：两个产物一起验通过。
func TestVerifyTwoProducts(t *testing.T) {
	dir := t.TempDir()
	hbc, sealed := buildSigned(t, dir)
	pub := filepath.Join(dir, "k.pem.pub")
	if code := run([]string{"verify", hbc, sealed, "-p", pub}); code != 0 {
		t.Fatalf("verify 应通过，退出码 %d", code)
	}
}

// TestVerifyDetectsProductSwap 是这条设计存在的理由：
// **把另一对构建的 sealed 配这个 .hbc，必须被发现**。
func TestVerifyDetectsProductSwap(t *testing.T) {
	d1 := t.TempDir()
	d2 := t.TempDir()
	hbc1, _ := buildSigned(t, d1)
	_, sealed2 := buildSigned(t, d2)
	pub := filepath.Join(d1, "k.pem.pub")

	if code := run([]string{"verify", hbc1, sealed2, "-p", pub}); code != 4 {
		t.Fatalf("跨构建混搭两个产物应退出 4，实际 %d", code)
	}
}

// TestVerifyRejectsWrongKey：换一把公钥必须失败。
func TestVerifyRejectsWrongKey(t *testing.T) {
	d1 := t.TempDir()
	d2 := t.TempDir()
	hbc, sealed := buildSigned(t, d1)
	buildSigned(t, d2) // 生成另一把钥匙

	if code := run([]string{"verify", hbc, sealed, "-p", filepath.Join(d2, "k.pem.pub")}); code != 4 {
		t.Fatalf("换成别人的公钥应退出 4，实际 %d", code)
	}
}
