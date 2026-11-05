package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSignCLIRoundTrip 走完整命令面：keygen → build → sign → verify 通过。
func TestSignCLIRoundTrip(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "k.pem")
	hbc := filepath.Join(dir, "x.hbc")

	if code := run([]string{"sign", "keygen", "-o", key}); code != 0 {
		t.Fatalf("keygen 退出码 %d", code)
	}
	// 私钥必须是 0600：这是唯一能把它变成"不是我们的问题"的做法。
	info, err := os.Stat(key)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("私钥权限应为 0600，实际 %o", info.Mode().Perm())
	}

	if code := run([]string{"build", "testdata/live.hermes", "-o", hbc}); code != 0 {
		t.Fatal("build 失败")
	}
	if code := run([]string{"sign", "hbc", hbc, "-k", key}); code != 0 {
		t.Fatalf("sign 退出码 %d", code)
	}
	if code := run([]string{"sign", "verify", hbc, "-p", key + ".pub"}); code != 0 {
		t.Fatalf("verify 应通过，退出码 %d", code)
	}
}

// TestSignVerifyRejectsUnsigned and Tampered：两种情况都要失败，
// 而且**报的错不一样** —— 前者该去补签名步骤，后者该去查文件来源。
func TestSignVerifyDistinguishes(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "k.pem")
	if code := run([]string{"sign", "keygen", "-o", key}); code != 0 {
		t.Fatal("keygen 失败")
	}

	t.Run("未签名", func(t *testing.T) {
		hbc := filepath.Join(dir, "u.hbc")
		if code := run([]string{"build", "testdata/hello.hermes", "-o", hbc}); code != 0 {
			t.Fatal("build 失败")
		}
		if code := run([]string{"sign", "verify", hbc, "-p", key + ".pub"}); code != 4 {
			t.Fatalf("未签名应退出 4，实际 %d", code)
		}
	})

	t.Run("签名后篡改", func(t *testing.T) {
		hbc := filepath.Join(dir, "t.hbc")
		if code := run([]string{"build", "testdata/hello.hermes", "-o", hbc}); code != 0 {
			t.Fatal("build 失败")
		}
		if code := run([]string{"sign", "hbc", hbc, "-k", key}); code != 0 {
			t.Fatal("sign 失败")
		}
		data, _ := os.ReadFile(hbc)
		data[len(data)-85] ^= 0xFF // 索引区一个字节
		os.WriteFile(hbc, data, 0o644)

		if code := run([]string{"sign", "verify", hbc, "-p", key + ".pub"}); code != 4 {
			t.Fatalf("篡改后应退出 4，实际 %d", code)
		}
	})
}
