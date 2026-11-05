package main

import (
	"os/exec"
	"strings"
	"testing"
)

// TestRunawayStopsAtMaxSteps 把"有界"这件事变成 CI 里的一条断言。
//
// 演示命令本身可以坏（它只是打印），但"到了上界就停下"不行 —— 那是产品的核心承诺。
func TestRunawayStopsAtMaxSteps(t *testing.T) {
	// 两种语言各验一次：输出跟随 HERMES_LANG，而"在边界处停下"两种语言都得成立。
	for _, tc := range []struct{ lang, wantCalls, wantMsg string }{
		{"en", "4 model calls", "max_steps=4"},
		{"zh", "模型调用 4 次", "max_steps=4"},
	} {
		cmd := exec.Command("../../build/hermes", "demo-runaway")
		cmd.Env = append(cmd.Environ(), "HERMES_LANG="+tc.lang)
		out, err := cmd.CombinedOutput()
		text := string(out)
		if err != nil {
			t.Skipf("跑不起 demo-runaway（先跑 build.sh）: %v\n%s", err, text)
		}
		if !strings.Contains(text, tc.wantMsg) {
			t.Fatalf("%s: 输出里没提到 max_steps=4：\n%s", tc.lang, text)
		}
		if !strings.Contains(text, tc.wantCalls) {
			t.Fatalf("%s: 调用次数不对（想要 %q）：\n%s", tc.lang, tc.wantCalls, text)
		}
		if strings.Contains(text, "FAILED") || strings.Contains(text, "实验失败") {
			t.Fatalf("%s: 没有在上界处停下：\n%s", tc.lang, text)
		}
	}
	return
}
