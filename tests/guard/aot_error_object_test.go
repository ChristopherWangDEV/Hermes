package guard_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// sliceGoRegion 从 startMark 起截到其后**最先**出现的任一 endMark（不含）。
// 用来把 c.go 里某个函数体 / case 块单独取出来：只在整文件里 grep 一次
// hm_clear_error(); 会被 main() 入口那一处喂饱，即使 try/fallback/retry
// 三处被改回「只清 code」也照样绿 —— 必须逐块各自验。
func sliceGoRegion(src, startMark string, endMarks ...string) string {
	i := strings.Index(src, startMark)
	if i < 0 {
		return ""
	}
	rest := src[i+len(startMark):]
	best := len(rest)
	for _, m := range endMarks {
		if j := strings.Index(rest, m); j >= 0 && j < best {
			best = j
		}
	}
	return rest[:best]
}

// TestAOTErrorObjectClearPointsAreWholeObject 是 AOT 标准错误对象的结构守卫：
//   - lowerFallback / case "try" / case "retry" 三处清错点必须都用
//     hm_clear_error();（清整个 HermesError，而不是只清 code）；
//   - errKindCond 读 hm_error.code，不许残留旧的 `hm_err ==` 判据；
//   - 整份 c.go 不许有独立 hm_err 记号（显式替换，不留 #define 别名宏）。
//
// 这与 internal/backend/c_test.go 的「扫生成源码」互补：那条看产物，这条看
// 生成器本体，避免"生成产物里对、但生成器里被改坏"或反之的漏网。
func TestAOTErrorObjectClearPointsAreWholeObject(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "internal", "backend", "c.go"))
	if err != nil {
		t.Fatalf("read c.go: %v", err)
	}
	src := string(raw)

	regions := []struct {
		name  string
		start string
		ends  []string
	}{
		{"lowerFallback", "func (g *cGen) lowerFallback(", []string{"\nfunc "}},
		{`case "try"`, "\tcase \"try\":", []string{"\n\tcase \"", "\n\tdefault:"}},
		{`case "retry"`, "\tcase \"retry\":", []string{"\n\tcase \"", "\n\tdefault:"}},
		{"errKindCond", "func errKindCond(", []string{"\nfunc "}},
	}
	got := map[string]string{}
	for _, r := range regions {
		got[r.name] = sliceGoRegion(src, r.start, r.ends...)
		if got[r.name] == "" {
			t.Fatalf("c.go 里找不到 %s 的函数体/case 块（守卫本身失效，先修守卫）", r.name)
		}
	}
	for _, name := range []string{"lowerFallback", `case "try"`, `case "retry"`} {
		if !strings.Contains(got[name], "hm_clear_error();") {
			t.Errorf("%s 必须用 hm_clear_error(); 清整个错误对象（不得改回只清 code）", name)
		}
	}
	if regexp.MustCompile(`\bhm_err\b\s*==`).MatchString(got["errKindCond"]) {
		t.Errorf("errKindCond 仍含旧的 hm_err == 判据，应改读 hm_error.code")
	}
	if regexp.MustCompile(`\bhm_err\b`).MatchString(src) {
		t.Errorf("c.go 仍含旧的独立 hm_err 记号（应显式替换为 hm_error.code / hm_clear_error）")
	}
}

// TestAOTNetBoundaryOutParamIsConsumed 从生成器本体（c.go 的 cNetPrelude）钉住
// 「出参是网络错误的唯一来源」：hm_net_invoke 函数体不得出现 hm_set_error（即
// 不写全局错误槽），必须把局部 HermesError 写回 *out_err；两个调用方必须以
// hm_publish_error(&out_err) 消费出参，且不得残留 (void)out_err;。与
// c_test.go 里「扫生成产物」的守卫互补：那条看产物，这条看生成器本体。
func TestAOTNetBoundaryOutParamIsConsumed(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "internal", "backend", "c.go"))
	if err != nil {
		t.Fatalf("read c.go: %v", err)
	}
	src := string(raw)
	invoke := sliceGoRegion(src, "static int hm_net_invoke(", "static Val hm_net_tool_call(")
	if invoke == "" {
		t.Fatal("c.go 里找不到 hm_net_invoke 函数体（守卫本身失效，先修守卫）")
	}
	if strings.Contains(invoke, "hm_set_error") {
		t.Errorf("hm_net_invoke 不得调用 hm_set_error 写全局错误槽（错误须只经 *out_err 回传）:\n%s", invoke)
	}
	if !strings.Contains(invoke, "*out_err=e") {
		t.Errorf("hm_net_invoke 必须把局部错误对象写回 *out_err:\n%s", invoke)
	}
	if !strings.Contains(src, "static void hm_publish_error(const HermesError* e)") {
		t.Error("缺少 hm_publish_error helper（把出参按值发布到全局 hm_error）")
	}
	if n := strings.Count(src, "hm_publish_error(&out_err);"); n != 2 {
		t.Errorf("两个调用方都必须以 hm_publish_error(&out_err); 消费出参，实际 %d 处", n)
	}
	if strings.Contains(src, "(void)out_err;") {
		t.Error("不得出现 (void)out_err;（出参被丢弃 = 死参数 / 装饰性 out param）")
	}
}
