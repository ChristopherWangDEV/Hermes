package main

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestNativeHelpIsLocalized 守"C++ 主程序接语言包"这条决定：
// HERMES_LANG=zh 时，已搬命令的 help 必须是中文。
//
// 为什么用 help 当判据：它是最稳定的用户可见文案（不随实现变动），
// 而且它一旦回退成英文，用户立刻能看见——不像错误消息那样只在出错时暴露。
func TestNativeHelpIsLocalized(t *testing.T) {
	root := repoRootForBackend(t)
	bin := filepath.Join(root, "build", "hermes-native")

	cmd := exec.Command(bin, "ir", "--help")
	cmd.Env = append(cmd.Environ(), "HERMES_LANG=zh")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hermes-native ir --help 失败: %v\n%s", err, out)
	}
	if !hasNonASCII(string(out)) {
		t.Fatalf("help is not localized under HERMES_LANG=zh (C++ side bypasses the pack):\n%s", out)
	}

	// 英文模式必须回到英文，不能被中文残留污染。
	cmd = exec.Command(bin, "ir", "--help")
	cmd.Env = append(cmd.Environ(), "HERMES_LANG=en")
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hermes-native ir --help (en) failed: %v", err)
	}
	if hasNonASCII(string(out)) {
		t.Fatalf("help contains Chinese under HERMES_LANG=en:\n%s", out)
	}
}

// hasNonASCII 判断输出里是否含非 ASCII（中文译文的必要条件）。
// 用条件而不是比对具体中文，是为了让这条测试不依赖某一条译文的具体措辞。
func hasNonASCII(s string) bool {
	for _, r := range s {
		if r > 127 {
			return true
		}
	}
	return false
}
