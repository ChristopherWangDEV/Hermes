package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"hermes/internal/archive"
	"hermes/internal/engine"
	"hermes/internal/ir"
	"hermes/internal/runtime"
)

// TestT44SupportBotEndToEnd 是 T44 的验收：这条路径上的每一环都是真实产物。
//
//	examples/support_bot/support.hermes
//	  → hermes build -k        → .hbc + .sealed.hermes（真签名）
//	  → hermes verify          → 五步交叉校验
//	  → hermes run <hbc>       → 从**归档**里取 IR 跑，不是从 IR JSON 跑
//	  → 真实 HTTP              → 意图识别 + 两次工具调用 + 生成话术
//
// 为什么坚持"从 .hbc 跑"：分发出去的是归档，不是 IR JSON。只能跑 JSON 的话，
// 这条验收永远在跑另一份东西 —— 而"分发的那份能不能跑"才是这条验收要回答的。
func TestT44SupportBotEndToEnd(t *testing.T) {
	dir := t.TempDir()

	// ---- 假上游：只实现 OpenAI 面的 chat/completions ----
	var calls, toolCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 一个假服务器同时扮演两个角色：模型面与工具面。
		// 按**路径**分流，而不是按调用顺序 —— 顺序会在加分支时静默错位。
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			n := atomic.AddInt32(&calls, 1)
			content := `{"action":"refund","order":{"order_id":"A10231","status":200,"amount":4999}}`
			if n > 1 {
				content = "您的退款已提交，预计 3 个工作日到账。"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "x",
				"choices": []any{map[string]any{
					"message":       map[string]any{"content": content},
					"finish_reason": "stop",
				}},
				"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
			})
		case strings.Contains(r.URL.Path, "/orders/"):
			atomic.AddInt32(&toolCalls, 1)
			// 按 models/order.h 的 OrderInfo 返回：三个字段一个都不能少
			_ = json.NewEncoder(w).Encode(map[string]any{
				"order_id": "A10231", "status": 200, "amount": 4999,
			})
		case strings.Contains(r.URL.Path, "/history/"):
			atomic.AddInt32(&toolCalls, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{"history": []any{"login", "order"}})
		default:
			t.Errorf("意外的路径 %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	// 后端地址来自 IR 里的 `env("HOMONOIA_URL", …)`；这里注入假上游。
	t.Setenv("HOMONOIA_URL", srv.URL)

	key := filepath.Join(dir, "k.pem")
	if code := run([]string{"sign", "keygen", "-o", key}); code != 0 {
		t.Fatalf("keygen 退出码 %d", code)
	}

	hbc := filepath.Join(dir, "support_bot.hbc")
	if code := run([]string{"build", "../../examples/support_bot/support.hermes", "-o", hbc, "-k", key}); code != 0 {
		t.Fatalf("build 退出码 %d", code)
	}
	sealed := filepath.Join(dir, "support_bot.sealed.hermes")
	if _, err := os.Stat(sealed); err != nil {
		t.Fatalf("应产出 sealed: %v", err)
	}
	if code := run([]string{"verify", hbc, sealed, "-p", key + ".pub"}); code != 0 {
		t.Fatalf("verify 退出码 %d", code)
	}

	// ---- 跑归档 ----
	//
	// 这里不能用 `hermes run`：验收要求**零外网**，而 support_bot 的工具 URL 是
	// `https://api.internal.com/...`，命令行路径会把请求真的发出去（DNS 直接失败）。
	// 所以由测试自己注入 Runtime，并把**所有域名**重写到本地假服务 ——
	// 拦截而不是放行，才能证明"没有真外网"。
	fake := &rewritingClient{target: srv.URL}

	a, err := archive.Read(mustRead(t, hbc))
	if err != nil {
		t.Fatal(err)
	}
	doc, ds := ir.Load(a.Members[0].IR)
	if len(ds) != 0 {
		t.Fatalf("归档里的 IR 不合格: %v", ds)
	}

	input := map[string]any{}
	if raw, err := os.ReadFile("../../examples/support_bot/input.json"); err == nil {
		_ = json.Unmarshal(raw, &input)
	}

	backends := map[string]ir.Backend{}
	for _, b := range doc.Backends {
		backends[b.Name] = b
	}
	res, err := engine.RunWith(doc, "", input, engine.RunOptions{
		Runtime: &runtime.Runtime{Backends: backends, HTTPClient: fake.client()},
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if res.Value != "您的退款已提交，预计 3 个工作日到账。" {
		t.Fatalf("最终值应来自第二次模型调用，实际 %#v", res.Value)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("应有 2 次模型调用，实际 %d", got)
	}
	if got := atomic.LoadInt32(&toolCalls); got != 2 {
		t.Fatalf("parallel 的两个分支都应执行（订单 + 历史），实际 %d", got)
	}
}

// rewritingClient 把所有请求重写到本地假服务，并分别计数。
//
// 为什么需要它：support_bot 的工具 URL 是真实域名。离线验收的正确做法是
// **拦截**，而不是允许它发出去再等失败 —— 后者在联网机器上会真的发请求，
// 那就不再是"零外网"的验收了。
type rewritingClient struct {
	target string
}

func (r *rewritingClient) client() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		u, _ := url.Parse(r.target)
		req.URL.Scheme = u.Scheme
		req.URL.Host = u.Host
		if len(req.URL.Path) == 0 || req.URL.Path == "/" {
			req.URL.Path = "/v1/chat/completions"
		}
		return http.DefaultTransport.RoundTrip(req)
	})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
