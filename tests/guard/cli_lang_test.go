// 本文件守"CLI 默认英文、HERMES_LANG 可切中文"这条**用户可见**的承诺。
//
// 为什么要在 guard 里跑真二进制：`tr()`/`pickUsage()` 只是约定，
// 约定会被漏用 —— 只要有一个命令忘了包，用户就会在英文界面里撞见一句中文，
// 而那种不一致比纯中文更难读。静态扫描做不到（中文串在 pickUsage 的多行块里），
// 所以只能真的跑一遍看输出。
package guard_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var cjk = regexp.MustCompile(`[\p{Han}]`)

// TestCLIHelpIsEnglishByDefault 与 TestCLIHelpIsChineseWithEnv 是一对：
// 只测一边会得出"全英文"或"全中文"的错误结论，而要求的是**可切换**。
func TestCLIHelpIsEnglishByDefault(t *testing.T) {
	bin := cliBinary(t)
	cases := helpCommands()
	for _, c := range cases {
		out := runCLI(t, bin, c, "")
		if cjk.MatchString(out) {
			t.Errorf("默认（英文）模式下 `hermes %s` 的输出含中文：\n%s", strings.Join(c, " "), out)
		}
	}
}

func TestCLIHelpIsChineseWithEnv(t *testing.T) {
	bin := cliBinary(t)
	for _, c := range helpCommands() {
		out := runCLI(t, bin, c, "zh-CN")
		if !cjk.MatchString(out) {
			t.Errorf("HERMES_LANG=zh-CN 时 `hermes %s` 的输出不含中文：\n%s", strings.Join(c, " "), out)
		}
	}
}

// cliBinary 返回 build.sh 产出的二进制；没有就跳过（正常由 check.sh 保证存在）。
func cliBinary(t *testing.T) string {
	t.Helper()
	p := filepath.Join(repoRoot(t), "build", "hermes")
	if _, err := os.Stat(p); err != nil {
		t.Skipf("没有 %s（先跑 scripts/build.sh）", p)
	}
	return p
}

func helpCommands() [][]string {
	return [][]string{
		{"--help"},
		{"ir", "--help"}, {"ar", "--help"}, {"build", "--help"}, {"run", "--help"},
		{"link", "--help"}, {"sign", "--help"}, {"verify", "--help"}, {"serve", "--help"},
		{"header", "--help"}, {"compile", "--help"}, {"import", "--help"}, {"trace", "--help"},
	}
}

func runCLI(t *testing.T, bin string, args []string, lang string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	// 显式清掉再设：否则开发机上的 HERMES_LANG 会让"默认"这一侧失真。
	cmd.Env = append(os.Environ(), "HERMES_LANG="+lang)
	out, _ := cmd.CombinedOutput()
	return string(out)
}
