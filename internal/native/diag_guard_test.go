package native

import (
	"strings"
	"testing"
)

// TestDiagDoesNotExplodeOnHeaderBodyWorkflow 是诊断规模的守卫。
//
// 背景：一条 `.h` 里出现带体 workflow 只该报**一条** E3005。
// 但实测（2026-11-03）发现 `.h` 模式下这条路径的诊断 JSON 会涨到 1 MiB 以上，
// 让 Parse 的扩容重试耗尽 —— 说明错误恢复在刷诊断而不是"报一条、跳到下一个声明"。
//
// 本测试用足够大的缓冲把它量出来，并按条数报警。它同时是 Parse 的
// "扩容重试"逻辑的上游症状监测：体积异常 → 条数异常 → 恢复逻辑有问题。
func TestDiagDoesNotExplodeOnHeaderBodyWorkflow(t *testing.T) {
	src := []byte("export workflow f() -> string { return \"x\"; }\n")

	const bigCap = 64 << 20 // 64 MiB：足够装下任何"正常"诊断，只是为了让测试不挂
	buf, n, err := ParseDiagRaw(src, "x.h", true, bigCap)
	if err != nil {
		t.Fatalf("ParseDiagRaw 失败：%v", err)
	}
	if n >= bigCap {
		t.Fatalf("诊断 JSON 连 64 MiB 都装不下（n=%d）：错误恢复几乎肯定不收敛", n)
	}
	body := string(buf[:n])
	codes := strings.Count(body, `"code"`)
	t.Logf("诊断 JSON = %d 字节，条数 ≈ %d", n, codes)

	// 一条带体 workflow 在 .h 里只应有一条 E3005。
	// 这里放宽到 20，是为了容忍"顺带报了解析恢复的诊断"，但绝不容忍上百条。
	if codes > 20 {
		t.Errorf("诊断条数 %d 远超预期（应为 1 条 E3005）：.h 模式的错误恢复不收敛。"+
			"前 800 字节：%s", codes, firstN(body, 800))
	}
	if !strings.Contains(body, "E3005") {
		t.Errorf("诊断里没有 E3005，前 800 字节：%s", firstN(body, 800))
	}
}
