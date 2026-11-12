package engine

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeRuntime 是测试用的 Runtime：不联网、结果由调用顺序决定。
//
// 为什么用"第几次调用"而不是匹配 prompt 文本：匹配文本会让测试与文案绑定，
// 改一句提示词就红；而这里要证明的是**执行顺序与数据流**，不是文案。
type fakeRuntime struct {
	// 计数器必须是原子的：parallel 之后分支**真并发**，普通 int 会构成数据竞争。
	// 这不只是测试问题 —— 它同时说明 `Runtime` 的实现必须能被并发调用。
	modelCalls atomic.Int32
	toolCalls  atomic.Int32
}

func (f *fakeRuntime) Model(_ context.Context, req ModelRequest) (any, error) {
	n := f.modelCalls.Add(1)
	if n == 1 {
		return map[string]any{
			"action": "refund",
			// Intent 的 schema 要求 order 的三个字段齐全 —— 校验器现在会在
			// 模型输出进入编排前拦下，所以假件必须照 schema 返回。
			"order": map[string]any{"order_id": "A10231", "status": int64(200), "amount": int64(4999)},
		}, nil
	}
	return "您的退款已提交，预计 3 个工作日到账。", nil
}

func (f *fakeRuntime) Tool(_ context.Context, req ToolRequest) (any, error) {
	f.toolCalls.Add(1)
	// **按 URL 分流，不按调用顺序**。parallel 是真并发之后，两个分支的
	// 完成顺序不确定 —— 按顺序返回会让 `o` 有时候拿到订单、有时候拿到历史，
	// 而失败信息是"字段 amount 不存在"，看起来像类型问题，实际是假件的假设错了。
	if strings.Contains(req.URL, "/orders/") {
		// 按 models/order.h 的 OrderInfo 返回：order_id / status / amount。
		return map[string]any{"order_id": "A10231", "status": int64(200), "amount": int64(4999)}, nil
	}
	return map[string]any{"history": []any{"login", "order"}}, nil
}

// TestSupportBotEndToEnd 是第一条**完整业务流**：
// `examples/support_bot` 的 IR 从入口跑到返回值，中途经过
// 模型调用 → 分支 → fork/join → 两次工具调用 → 再次模型调用。
//
// 它同时钉住了三件事：match 走的是 "refund" 臂（不是 default）、
// parallel 的两个分支都真的执行了（toolCalls==2）、
// 最终返回值来自第二次模型调用（不是占位字符串）。
func TestSupportBotEndToEnd(t *testing.T) {
	d := loadGolden(t, "ir-support_bot.json")
	fake := &fakeRuntime{}

	res, err := RunWith(d, "", map[string]any{
		"text": "我要退款，订单 A10231 一直没到", "user_id": "u_1001",
	}, RunOptions{Runtime: fake})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	if res.Value != "您的退款已提交，预计 3 个工作日到账。" {
		t.Fatalf("最终值应来自第二次模型调用，实际 %#v", res.Value)
	}
	if got := fake.modelCalls.Load(); got != 2 {
		t.Errorf("应有 2 次模型调用（意图识别 + 生成话术），实际 %d", got)
	}
	if got := fake.toolCalls.Load(); got != 2 {
		t.Errorf("parallel 的两个分支都应执行（订单 + 历史），实际 %d 次工具调用", got)
	}
	if len(res.Spans) > d.Workflows[0].Bounds.MaxSpans+1 {
		t.Errorf("span 数 %d 超过上界 %d(+1)", len(res.Spans), d.Workflows[0].Bounds.MaxSpans)
	}
}

// TestExternalCallWithoutRuntimeFails 守"没有 Runtime 就明确报错"。
//
// 静默跳过外部调用会让整个 workflow 返回一个"看起来成功"的空值 ——
// 那比直接失败危险得多。
func TestExternalCallWithoutRuntimeFails(t *testing.T) {
	d := loadGolden(t, "ir-support_bot.json")
	if _, err := Run(d, "", map[string]any{"text": "x", "user_id": "u"}); err == nil {
		t.Fatal("没有 Runtime 时，模型/工具调用必须报错")
	}
}
