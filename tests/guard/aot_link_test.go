// 本文件守 AOT 产物的**链接面**：产物不许把 GPL 类库带进分发链。
//
// 为什么需要单独一条守卫：`hermes compile` 是一条新的链接路径
// （Go c-archive + 用户机器上的 clang + 系统框架），而前面那些守卫
// （check_toolchain、stdlib_boundary）盯的是**构建产物**与**编译器**，
// 盯不到"最终可执行文件链了什么"。
package guard_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenLibs 是绝对不该出现在分发产物动态依赖里的库。
//
// 只列"因为许可而必须拒绝"的那些：
//   - libstdc++：GPL-3.0 WITH GCC Runtime Library Exception（红线按字面从严）
//   - libgcc_s：同上（GCC 运行时）
//   - libgomp / libquadmath：GPL 家族
var forbiddenLibs = []string{"libstdc++", "libgcc_s", "libgomp", "libquadmath"}

// TestAOTBinaryHasNoCopyleftLibs 检查 `hermes compile` 产物的动态依赖。
//
// 没有 clang 或没有运行时库时跳过（CI 上由 build.sh 保证存在）。
func TestAOTBinaryHasNoCopyleftLibs(t *testing.T) {
	root := repoRoot(t)
	hermesBin := filepath.Join(root, "build", "hermes")
	if _, err := os.Stat(hermesBin); err != nil {
		t.Skipf("没有 %s（先跑 scripts/build.sh）", hermesBin)
	}
	cc, err := exec.LookPath("clang")
	if err != nil {
		t.Skip("没有 clang")
	}

	out := filepath.Join(t.TempDir(), "prog")
	// 直接调 clang 而不是 hermes compile：这一步只验证**链接面**，
	// 走命令面会把"命令坏了"和"链路带进了 GPL"混成一个失败。
	gen := exec.Command(hermesBin,
		"compile", filepath.Join(root, "cmd", "hermes", "testdata", "hello.hermes"),
		"-o", out)
	gen.Dir = root
	if b, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("hermes compile 失败: %v\n%s", err, b)
	}

	deps := dynamicDeps(t, out, cc)
	if len(deps) == 0 {
		t.Skip("拿不到动态依赖（非 darwin 或 otool 不可用）")
	}
	for _, d := range deps {
		for _, bad := range forbiddenLibs {
			if strings.Contains(d, bad) {
				t.Fatalf("AOT 产物链接了 %s（%s）：红线禁止把 GPL 类库带进分发链", bad, d)
			}
		}
	}
	// 反向对照：确认我们**确实**读到了依赖，否则上面的循环是空转。
	if !strings.Contains(strings.Join(deps, "\n"), "libSystem") {
		t.Fatalf("依赖列表里没有 libSystem，说明扫描没生效：%v", deps)
	}
}

// dynamicDeps 用 otool 读动态依赖（仅 darwin）。
func dynamicDeps(t *testing.T, path, cc string) []string {
	t.Helper()
	if !strings.Contains(strings.ToLower(cc), "clang") {
		return nil
	}
	out, err := exec.Command("otool", "-L", path).Output()
	if err != nil {
		return nil
	}
	lines := strings.Split(string(out), "\n")
	deps := make([]string, 0, len(lines))
	for _, l := range lines[1:] {
		if f := strings.Fields(l); len(f) > 0 {
			deps = append(deps, f[0])
		}
	}
	return deps
}
