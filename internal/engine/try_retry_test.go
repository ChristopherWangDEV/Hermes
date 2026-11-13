package engine

import (
	"context"
	"errors"
	"testing"
)

// flakyRuntime 前 N 次抛可捕获错误，之后成功。
type flakyRuntime struct {
	failures int
	calls    int
}

func (f *flakyRuntime) Model(_ context.Context, _ ModelRequest) (any, error) {
	f.calls++
	if f.calls <= f.failures {
		return nil, &RunError{Kind: "ModelError", Msg: "model exploded"}
	}
	// 这个 workflow 的模型输出有 schema（object，必填 action）——
	// 返回一个字符串会被校验器拦下，而那是**正确**行为：假件也得守契约。
	return map[string]any{"action": "ok"}, nil
}

func (f *flakyRuntime) Tool(_ context.Context, _ ToolRequest) (any, error) {
	return nil, errors.New("not used")
}

// TestTryCatchesRunError 守"catch 接得住"。
func TestTryCatchesRunError(t *testing.T) {
	d := loadGolden(t, "ir-try.json")
	f := &flakyRuntime{failures: 1}
	res, err := RunWith(d, "", nil, RunOptions{Runtime: f})
	if err != nil {
		t.Fatalf("catch 应接住 ModelError，实际抛出: %v", err)
	}
	if res.Value != "caught" {
		t.Fatalf("期望 \"caught\"，实际 %#v", res.Value)
	}
}

// TestTryDoesNotCatchProgrammingErrors 守"编程错误不许被 catch 住"。
//
// 把 catch 写成万能兜底是最常见的错误用法。若执行器把编程错误也当业务错误
// 处理，真 bug 就永久隐身了。
func TestTryDoesNotCatchProgrammingErrors(t *testing.T) {
	d := loadGolden(t, "ir-try.json")
	// 制造一个编程错误：把 try 的 body 换成越界节点。
	for i := range d.Workflows[0].Nodes {
		if d.Workflows[0].Nodes[i].K == "try" {
			bad := 999
			d.Workflows[0].Nodes[i].Body = &bad
		}
	}
	f := &flakyRuntime{}
	if _, err := RunWith(d, "", nil, RunOptions{Runtime: f}); err == nil {
		t.Fatal("编程错误必须炸穿，不能被 catch 住")
	} else if _, ok := err.(*RunError); ok {
		t.Fatalf("编程错误不该是 RunError（它不可捕获），实际 %v", err)
	}
}

// TestRetryRetriesUntilSuccess 守"重试到成功为止，且次数是 n+1 次"。
func TestRetryRetriesUntilSuccess(t *testing.T) {
	d := loadGolden(t, "ir-retry.json")
	f := &flakyRuntime{failures: 2} // retry(2) = 最多 3 次尝试
	res, err := RunWith(d, "", nil, RunOptions{Runtime: f})
	if err != nil {
		t.Fatalf("第 3 次应成功，实际: %v", err)
	}
	if res.Value != "ok" {
		t.Fatalf("期望 \"ok\"，实际 %#v", res.Value)
	}
	if f.calls != 3 {
		t.Fatalf("retry(2) 应尝试 3 次，实际 %d 次", f.calls)
	}
}

// TestRetryGivesUpAfterNAttempts 守"用完次数就如实失败"，不无限重试。
func TestRetryGivesUpAfterNAttempts(t *testing.T) {
	d := loadGolden(t, "ir-retry.json")
	f := &flakyRuntime{failures: 99}
	_, err := RunWith(d, "", nil, RunOptions{Runtime: f})
	if err == nil {
		t.Fatal("重试次数用尽后必须报错")
	}
	if f.calls != 3 {
		t.Fatalf("retry(2) 最多 3 次尝试，实际 %d 次", f.calls)
	}
}
