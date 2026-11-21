package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestNativeBuildMatchesGo 是 `build` 搬 C++ 的判据：同一份源码，
// `hermes-native build` 产出的 `.hbc` 必须与 `hermes build` **逐字节相同**。
//
// 归档里有偏移、索引哈希与 canonical JSON——任何一处不同都会让"同一份源码
// 两次构建产出不同产物"，而症状（签名对不上 / 分发链断开）出现在很远的地方。
func TestNativeBuildMatchesGo(t *testing.T) {
	root := repoRootForBackend(t)
	goBin := filepath.Join(root, "build", "hermes")
	nativeBin := filepath.Join(root, "build", "hermes-native")
	dir := t.TempDir()

	samples := []string{
		filepath.Join(root, "examples", "arrays", "length.hermes"),
		filepath.Join(root, "examples", "functions", "greet.hermes"),
		filepath.Join(root, "examples", "json", "coerce.hermes"),
	}
	compared := 0
	for _, src := range samples {
		goOut := filepath.Join(dir, "go.hbc")
		cppOut := filepath.Join(dir, "cpp.hbc")
		if out, err := exec.Command(goBin, "build", src, "-o", goOut).CombinedOutput(); err != nil {
			t.Fatalf("go build %s: %v\n%s", src, err, out)
		}
		if out, err := exec.Command(nativeBin, "build", src, "-o", cppOut).CombinedOutput(); err != nil {
			t.Fatalf("native build %s: %v\n%s", src, err, out)
		}
		a, err := os.ReadFile(goOut)
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(cppOut)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Fatalf("%s: 归档字节不同（go %d / cpp %d）", src, len(a), len(b))
		}
		compared++
	}
	if compared == 0 {
		t.Fatal("一个样本都没比到")
	}
	t.Logf("归档逐字节一致：%d 个样本", compared)
}

// TestNativeBuildRefusesSigning：`-k` 需要 Ed25519（仍在 Go），
// 必须**明确拒绝**而不是产出未签名的产物——那会让用户以为签过了。
func TestNativeBuildRefusesSigning(t *testing.T) {
	root := repoRootForBackend(t)
	nativeBin := filepath.Join(root, "build", "hermes-native")
	src := filepath.Join(root, "examples", "arrays", "length.hermes")
	out, err := exec.Command(nativeBin, "build", src, "-k", "key.txt").CombinedOutput()
	if err == nil {
		t.Fatalf("带 -k 应拒绝，实际成功：%s", out)
	}
	if !bytes.Contains(out, []byte("Ed25519")) {
		t.Fatalf("拒绝时必须说明原因：%s", out)
	}
}

// TestNativeBuildKeepGoingParity: --keep-going must reproduce Go's two rules --
// default renders one error and says some were suppressed; keep-going renders
// all recoverable ones (cap 50) and says nothing about suppression.
func TestNativeBuildKeepGoingParity(t *testing.T) {
	root := repoRootForBackend(t)
	nativeBin := filepath.Join(root, "build", "hermes-native")
	dir := t.TempDir()
	src := filepath.Join(dir, "bad.hermes")
	body := "workflow \"a\" ();\nstruct { int x; };\nbackend foo\n"
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(nativeBin, "build", src, "-o", filepath.Join(dir, "a.hbc")).CombinedOutput()
	if err == nil {
		t.Fatalf("default build should fail: %s", out)
	}
	if n := bytes.Count(out, []byte("error[")); n != 1 {
		t.Fatalf("default must render exactly 1 error, got %d:\n%s", n, out)
	}
	if !bytes.Contains(out, []byte("suppressed")) {
		t.Fatalf("default must say diagnostics were suppressed:\n%s", out)
	}

	out, err = exec.Command(nativeBin, "build", src, "-o", filepath.Join(dir, "b.hbc"), "--keep-going").CombinedOutput()
	if err == nil {
		t.Fatalf("keep-going build should still fail: %s", out)
	}
	if n := bytes.Count(out, []byte("error[")); n < 3 {
		t.Fatalf("keep-going must render >= 3 errors, got %d:\n%s", n, out)
	}
	if bytes.Contains(out, []byte("suppressed")) {
		t.Fatalf("keep-going must not mention suppression:\n%s", out)
	}
}
