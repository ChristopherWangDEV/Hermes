package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestNativeCompileMatchesGo 是 `compile` 默认路径的端到端判据：
// 同一份 .hermes，Go 与 native 各编一次，**产物跑起来输出相同**。
//
// 为什么必须真跑：compile 的失败模式是"能编过、结果不对"——IR→C 那一步
// 一旦少了个字段，链接照样成功，只有运行时才看得出。
func TestNativeCompileMatchesGo(t *testing.T) {
	root := repoRootForBackend(t)
	goBin := filepath.Join(root, "build", "hermes")
	nativeBin := filepath.Join(root, "build", "hermes-native")
	dir := t.TempDir()

	samples := []string{
		filepath.Join(root, "examples", "functions", "greet.hermes"),
		filepath.Join(root, "examples", "json", "coerce.hermes"),
		filepath.Join(root, "examples", "signals", "budget.hermes"),
	}
	compared := 0
	for _, src := range samples {
		if _, err := os.Stat(src); err != nil {
			continue
		}
		run := func(bin, outName string) (string, error) {
			out := filepath.Join(dir, outName)
			cmd := exec.Command(bin, "compile", src, "-o", out)
			cmd.Dir = root
			if b, err := cmd.CombinedOutput(); err != nil {
				return string(b), err
			}
			got, err := exec.Command(out).CombinedOutput()
			if err != nil {
				return string(got), err
			}
			return strings.TrimSpace(string(got)), nil
		}
		want, err := run(goBin, "go.bin")
		if err != nil {
			t.Fatalf("%s: Go compile 失败: %v\n%s", filepath.Base(src), err, want)
		}
		got, err := run(nativeBin, "native.bin")
		if err != nil {
			t.Fatalf("%s: native compile 失败: %v\n%s", filepath.Base(src), err, got)
		}
		if got != want {
			t.Fatalf("%s: 产物行为不同\n  Go =%s\n  C++=%s", filepath.Base(src), want, got)
		}
		compared++
	}
	if compared == 0 {
		t.Fatal("一个样本都没比到")
	}
	t.Logf("compile 端到端一致：%d 个样本", compared)
}

// TestNativeCompileBareMetalGate：裸机目标上，宿主 OS 专属语义必须被**同一条定稿文案**
// 拒绝——Go 与 native 两侧逐字相同。
//
// 为什么比文案：这句话是用户指定的，改一个字就变成另一条规则；而"两边都拒了"
// 并不代表拒的理由一样（可能是 clang 先炸了）。
func TestNativeCompileBareMetalGate(t *testing.T) {
	root := repoRootForBackend(t)
	goBin := filepath.Join(root, "build", "hermes")
	nativeBin := filepath.Join(root, "build", "hermes-native")

	// 找一个真的含 sleep/until 的样本
	var src string
	for _, cand := range []string{
		filepath.Join(root, "examples", "sync", "pacing.hermes"),
		filepath.Join(root, "examples", "signals", "budget.hermes"),
	} {
		if b, err := os.ReadFile(cand); err == nil {
			text := string(b)
			if strings.Contains(text, "sleep") || strings.Contains(text, "until") {
				src = cand
				break
			}
		}
	}
	if src == "" {
		t.Skip("没有含 sleep/until 的样本")
	}
	const want = "[ERROR] Blocking synchronization or network routing requires a host OS."

	out := filepath.Join(t.TempDir(), "x.bin")
	for _, bin := range []string{goBin, nativeBin} {
		res, err := exec.Command(bin, "compile", src, "--target", "thumbv7em-none-eabi", "-o", out).CombinedOutput()
		if err == nil {
			t.Fatalf("%s: 裸机上 sleep/until 应被拒绝", filepath.Base(bin))
		}
		if !strings.Contains(string(res), want) {
			t.Fatalf("%s: 拒绝文案不对\n  期望包含：%s\n  实际：%s", filepath.Base(bin), want, res)
		}
	}
}

