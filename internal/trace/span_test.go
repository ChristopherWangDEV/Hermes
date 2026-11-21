package trace

import "testing"

// TestDiffAlignsByStepID 守"按 step_id 对齐"。
//
// 按 span_id 对齐等于每次都全变了（它含 run_id 与行号），
// 而人想问的是"上一次好的、这一次坏的，差在哪一步"。
func TestDiffAlignsByStepID(t *testing.T) {
	left := []Span{
		{ID: "r/L1", StepID: "intent", Status: "ok", TokensIn: 10, TokensOut: 5, MicroUSD: 100},
		{ID: "r/L3", StepID: "fetch", Status: "ok"},
	}
	right := []Span{
		{ID: "r2/L1", StepID: "intent", Status: "failed", TokensIn: 20, TokensOut: 5, MicroUSD: 200},
		{ID: "r2/L5", StepID: "reply", Status: "ok"},
	}
	d := Diff(left, right)

	if len(d.Changed) != 1 || d.Changed[0].StepID != "intent" || d.Changed[0].Got != "failed" {
		t.Fatalf("应报 intent 的状态变化，实际 %+v", d.Changed)
	}
	if len(d.OnlyLeft) != 1 || d.OnlyLeft[0] != "fetch" {
		t.Fatalf("fetch 应只在左边，实际 %v", d.OnlyLeft)
	}
	if len(d.OnlyRight) != 1 || d.OnlyRight[0] != "reply" {
		t.Fatalf("reply 应只在右边，实际 %v", d.OnlyRight)
	}
	// 成本差：右(200) − 左(100) = +100；token 差：25 − 15 = +10
	if d.CostDelta != 100 || d.TokenDelta != 10 {
		t.Fatalf("差值不对：cost=%d token=%d", d.CostDelta, d.TokenDelta)
	}
	if d.FailedLeft != 0 || d.FailedRight != 1 {
		t.Fatalf("失败计数不对：%d -> %d", d.FailedLeft, d.FailedRight)
	}
}

// TestDiffKeepsFailedSpanOnRetry 守"重试不掩盖失败"。
//
// 同一个 step 有多条 span 时保留失败那条：否则重试成功后的 ok 会盖掉失败，
// 而失败才是人要看的东西。
func TestDiffKeepsFailedSpanOnRetry(t *testing.T) {
	left := []Span{
		{ID: "a", StepID: "call", Status: "failed"},
		{ID: "b", StepID: "call", Status: "ok"},
	}
	want := "failed"
	right := []Span{{ID: "c", StepID: "call", Status: "failed"}}
	d := Diff([]Span{left[0], left[1]}, right)
	if len(d.Changed) != 0 {
		t.Fatalf("两边都该是 %s，不该报变化：%+v", want, d.Changed)
	}
}
