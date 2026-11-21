package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"hermes/internal/budget"
	"hermes/internal/engine"
	"hermes/internal/ir"
	"hermes/internal/resilience"
)

func okServer(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	return srv, &hits
}

func backendAt(u string) map[string]ir.Backend {
	return map[string]ir.Backend{"h": {
		Name: "h", Kind: "openai", Model: "m",
		Addr: ir.Addr{Src: "literal", Value: strp(u)},
	}}
}

// TestBudgetRefusalSendsNoRequest 守"钱不够时一个请求都不发"。
//
// 若先发再结算，预算就只是个事后统计 —— 而"花超了"这件事不可逆。
func TestBudgetRefusalSendsNoRequest(t *testing.T) {
	srv, hits := okServer(t)
	defer srv.Close()

	led := budget.New(budget.Limits3{Global: budget.Limits{TokensIn: 1, TokensOut: 1}})
	rt := &Runtime{Backends: backendAt(srv.URL), Budget: led}

	_, err := rt.Model(context.Background(), engine.ModelRequest{
		Workflow: "w", Backend: "h", Prompt: "a rather long prompt that exceeds the budget",
	})
	if err == nil {
		t.Fatal("预算不足必须报错")
	}
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Fatalf("预算拒绝时不该发出任何请求，实际 %d 个", n)
	}
}

// TestBudgetSettlesActualUsage 证明成功调用后按**实际**用量结算（而不是上界）。
func TestBudgetSettlesActualUsage(t *testing.T) {
	srv, _ := okServer(t)
	defer srv.Close()

	led := budget.New(budget.Limits3{Global: budget.Limits{TokensIn: 1000, TokensOut: 1000}})
	rt := &Runtime{Backends: backendAt(srv.URL), Budget: led}

	if _, err := rt.Model(context.Background(), engine.ModelRequest{
		Workflow: "w", Backend: "h", Prompt: "hi",
	}); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	spent := led.Spent()
	if spent.In != 10 || spent.Out != 5 {
		t.Fatalf("结算应按实际用量（10/5），实际 %d/%d", spent.In, spent.Out)
	}
	if led.Open() != 0 {
		t.Fatalf("结算后不该有未结清的预留，实际 %d", led.Open())
	}
}

// TestBreakerStopsSending 守"开闸期间不发请求"。
func TestBreakerStopsSending(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(500)
	}))
	defer srv.Close()

	br := resilience.NewBreaker(resilience.BreakerConfig{
		Threshold: 1, Cooldown: time.Minute, MaxCooldown: time.Minute,
	})
	now := time.Unix(1000, 0)
	rt := &Runtime{Backends: backendAt(srv.URL), Breaker: br, Now: func() time.Time { return now }}

	// 第一次：真的发出去，失败并计入熔断。
	_, _ = rt.Model(context.Background(), engine.ModelRequest{Workflow: "w", Backend: "h", Prompt: "x"})
	first := atomic.LoadInt32(&hits)

	// 第二次：已开闸，不该再发。
	_, err := rt.Model(context.Background(), engine.ModelRequest{Workflow: "w", Backend: "h", Prompt: "x"})
	if err == nil {
		t.Fatal("开闸后应报 CircuitOpenError")
	}
	re, ok := err.(*engine.RunError)
	if !ok || re.Kind != "CircuitOpenError" {
		t.Fatalf("应为 CircuitOpenError，实际 %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != first {
		t.Fatalf("开闸期间不该再发请求：之前 %d，现在 %d", first, got)
	}
}
