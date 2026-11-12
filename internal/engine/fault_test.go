package engine

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

// 本文件是**故障注入**（T29）：不是"功能能不能用"，而是"坏的时候会怎样"。
//
// 每条故障都断言三件事：错误类型对不对、错误信息指不指得回步骤、
// 以及**有没有在错误发生前多花钱**（多一次调用就多一次钱）。

// faultRuntime 在指定的调用序号上失败。
type faultRuntime struct {
	modelFailAt int // 第 n 次模型调用失败（1 起；0 表示不失败）
	toolFailAt  int
	// 计数器必须原子：parallel 的分支**真并发**调用 Tool，
	// 普通 int 会构成数据竞争（`-race` 立刻报）。这也再次说明
	// `Runtime` 的实现必须能被并发调用 —— 假件也不例外。
	models atomic.Int32
	tools  atomic.Int32
}

func (f *faultRuntime) Model(_ context.Context, _ ModelRequest) (any, error) {
	n := f.models.Add(1)
	if f.modelFailAt > 0 && int(n) == f.modelFailAt {
		return nil, &RunError{Kind: "NetworkError", Msg: "injected model failure"}
	}
	// action 必须是 "refund"：support_bot 用 match 分派，其它值会走默认分支、
	// 根本到不了工具调用 —— 那样"工具失败"这条测试会因为**没调用**而失败，
	// 而失败信息看起来像"错误被吞了"，方向完全错。
	return map[string]any{"action": "refund",
		"order": map[string]any{"order_id": "A1", "status": int64(200), "amount": int64(1)}}, nil
}

func (f *faultRuntime) Tool(_ context.Context, _ ToolRequest) (any, error) {
	tn := f.tools.Add(1)
	if f.toolFailAt > 0 && int(tn) == f.toolFailAt {
		return nil, &RunError{Kind: "HttpError", Status: 500, Msg: "injected tool failure"}
	}
	return map[string]any{"order_id": "A1", "status": int64(200), "amount": int64(1)}, nil
}

// TestFaultModelFailureStopsEarly 守"模型失败后不再继续调用"。
func TestFaultModelFailureStopsEarly(t *testing.T) {
	d := loadGolden(t, "ir-support_bot.json")
	rt := &faultRuntime{modelFailAt: 1}
	_, err := RunWith(d, "", map[string]any{"text": "x", "user_id": "u"}, RunOptions{Runtime: rt})
	if err == nil {
		t.Fatal("模型失败必须让 run 失败")
	}
	if int(rt.tools.Load()) != 0 {
		t.Fatalf("模型失败后不该再调工具，实际调了 %d 次", int(rt.tools.Load()))
	}
	if int(rt.models.Load()) != 1 {
		t.Fatalf("失败后不该重试（没有 retry 包裹），实际 %d 次", int(rt.models.Load()))
	}
}

// TestFaultToolFailureCarriesStatus 守 HttpError 带状态码。
//
// 不带状态码的错误在排障时几乎没用："上游 500" 和 "上游 404" 的处置完全不同。
func TestFaultToolFailureCarriesStatus(t *testing.T) {
	d := loadGolden(t, "ir-support_bot.json")
	rt := &faultRuntime{toolFailAt: 1}
	_, err := RunWith(d, "", map[string]any{"text": "x", "user_id": "u"}, RunOptions{Runtime: rt})
	if err == nil {
		t.Fatalf("工具失败必须让 run 失败（tools=%d, models=%d, err=%v）", int(rt.tools.Load()), int(rt.models.Load()), err)
	}
	var re *RunError
	if !errors.As(err, &re) {
		t.Logf("实际错误: %v", err)
		t.Fatalf("应是 RunError，实际 %T", err)
	}
	if re.Status != 500 {
		t.Fatalf("应带上游状态码 500，实际 %d", re.Status)
	}
}

// TestFaultRespectsContextCancellation 守"取消优先于上游错误"。
func TestFaultRespectsContextCancellation(t *testing.T) {
	d := loadGolden(t, "ir-support_bot.json")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立刻取消

	rt := &faultRuntime{}
	_, err := RunWith(d, "", map[string]any{"text": "x", "user_id": "u"},
		RunOptions{Runtime: rt, Context: ctx})
	if err == nil {
		t.Fatal("已取消的 ctx 必须让 run 失败")
	}
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("应是 ErrCancelled，实际 %v", err)
	}
	if int(rt.models.Load()) != 0 || int(rt.tools.Load()) != 0 {
		t.Fatalf("取消后不该发出任何调用，实际 model=%d tool=%d", int(rt.models.Load()), int(rt.tools.Load()))
	}
}

// TestFaultNilRuntimeFailsLoudly 守"没有 Runtime 时明确报错，而不是假装成功"。
//
// 静默成功是最坏的一种：workflow 看起来跑完了，但模型调用根本没发生。
func TestFaultNilRuntimeFailsLoudly(t *testing.T) {
	d := loadGolden(t, "ir-hello.json")
	res, err := RunWith(d, "", nil, RunOptions{})
	if err != nil {
		t.Fatalf("hello 不需要 Runtime，不该失败: %v", err)
	}
	if res.Value != "hi" {
		t.Fatalf("hello 应返回 hi，实际 %v", res.Value)
	}

	// 需要模型的 workflow 在没有 Runtime 时必须报错。
	dm := loadGolden(t, "ir-model.json")
	_, err = RunWith(dm, "", map[string]any{"text": "x"}, RunOptions{})
	if err == nil {
		t.Fatal("需要模型但没有 Runtime，必须报错")
	}
	if !strings.Contains(err.Error(), "Runtime") && !strings.Contains(err.Error(), "runtime") {
		t.Fatalf("错误应说明缺 Runtime：%v", err)
	}
}

// TestFaultBadSchemaIsModelError 守 schema 违规被归到 ModelError 而不是网络错误。
//
// 归错类会让重试策略变错：网络错值得重试，schema 违规重试通常还是错的。
func TestFaultBadSchemaIsModelError(t *testing.T) {
	d := loadGolden(t, "ir-support_bot.json")
	rt := &faultRuntime{}
	// 用一个返回“缺字段”的 runtime 覆盖
	bad := &badSchemaRuntime{}
	_, err := RunWith(d, "", map[string]any{"text": "x", "user_id": "u"}, RunOptions{Runtime: bad})
	if err == nil {
		t.Fatal("schema 违规必须失败")
	}
	var re *RunError
	if !errors.As(err, &re) {
		t.Fatalf("应是 RunError，实际 %T", err)
	}
	if re.Kind != "ModelError" {
		t.Fatalf("schema 违规应归 ModelError，实际 %s", re.Kind)
	}
	_ = rt
}

type badSchemaRuntime struct{}

func (b *badSchemaRuntime) Model(_ context.Context, _ ModelRequest) (any, error) {
	return map[string]any{"action": "ok"}, nil // 缺 order
}
func (b *badSchemaRuntime) Tool(_ context.Context, _ ToolRequest) (any, error) {
	return map[string]any{}, nil
}
