package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"hermes/internal/errs"
)

// fake 起一个按路径回固定状态的服务器，并记录最后一次请求体。
type fake struct {
	srv      *httptest.Server
	lastPath string
	lastBody []byte
	lastHdr  http.Header
}

func newFake(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *fake {
	t.Helper()
	f := &fake{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.lastPath = r.URL.Path
		f.lastBody = b
		f.lastHdr = r.Header.Clone()
		handler(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) client() *Client { return &Client{BaseURL: f.srv.URL} }

// ① OpenAI 面：请求字段逐个对，响应归一化，用量来源是 provider。
func TestOpenAIChat(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cmpl-1","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7}}`))
	})
	resp, err := f.client().Chat(context.Background(),
		Target{Provider: "openai", Model: "qwen2.5-7b", EndpointKind: "chat"},
		Request{
			Model:          "qwen2.5-7b",
			Messages:       []Message{{Role: "user", Content: "hello"}},
			MaxTokens:      64,
			ResponseFormat: json.RawMessage(`{"type":"json_schema"}`),
			Tools:          json.RawMessage(`[{"type":"function","function":{"name":"f"}}]`),
			ToolChoice:     "auto",
		})
	if err != nil {
		t.Fatal(err)
	}
	if f.lastPath != "/v1/chat/completions" {
		t.Fatalf("路径错：%s", f.lastPath)
	}
	var sent map[string]any
	if err := json.Unmarshal(f.lastBody, &sent); err != nil {
		t.Fatal(err)
	}
	if sent["model"] != "qwen2.5-7b" {
		t.Fatalf("model 未透传：%v", sent["model"])
	}
	if sent["max_tokens"].(float64) != 64 {
		t.Fatalf("max_tokens 错：%v", sent["max_tokens"])
	}
	if _, ok := sent["response_format"]; !ok {
		t.Fatal("response_format 必须原样透传（运行期不重新生成，§1.7 ⑥）")
	}
	if _, ok := sent["tools"]; !ok {
		t.Fatal("tools 必须透传")
	}
	if _, ok := sent["temperature"]; ok {
		t.Fatal("不该发送 temperature（禁浮点）")
	}
	if resp.Content != "hi" || resp.Usage.In != 11 || resp.Usage.Out != 7 {
		t.Fatalf("解析错：%+v", resp)
	}
	if resp.Usage.Source != "provider" {
		t.Fatalf("用量来源应为 provider，got %q", resp.Usage.Source)
	}
}

// ② 同输入 ⇒ 同请求指纹（T22 ⑥）。
func TestRequestFingerprintStable(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"x"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})
	c := f.client()
	tg := Target{Provider: "openai", Model: "m", EndpointKind: "chat"}
	req := Request{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}, MaxTokens: 8}
	a, err := c.Chat(context.Background(), tg, req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Chat(context.Background(), tg, req)
	if err != nil {
		t.Fatal(err)
	}
	if a.RequestSHA256 != b.RequestSHA256 {
		t.Fatalf("同输入必须同指纹：%s vs %s", a.RequestSHA256, b.RequestSHA256)
	}
}

// ③ 错误映射：三类上游各一条关键行，且容量类**不计熔断**。
func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		style  errs.Style
		status int
		body   string
		wantA  errs.Attribution
		wantS  errs.Subtype
		counts bool
	}{
		{"openai 429 quota", errs.StyleOpenAI, 429,
			`{"error":{"code":"rate_limit_exceeded","type":"requests"}}`,
			errs.AttrPolicy, errs.SubQuota, false},
		{"openai 503 capacity", errs.StyleOpenAI, 503,
			`{"error":{"code":"server_busy"}}`,
			errs.AttrPolicy, errs.SubCapacity, false},
		{"anthropic 529 capacity", errs.StyleAnthropic, 529,
			`{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`,
			errs.AttrPolicy, errs.SubCapacity, false},
		{"ollama 429 无 code", errs.StyleOllama, 429,
			`{"error":"too many requests"}`,
			errs.AttrPolicy, errs.SubQuota, false},
		{"4xx 请求被拒", errs.StyleOpenAI, 404,
			`{"error":{"code":"model_not_found"}}`,
			errs.AttrUpstreamContractDrift, errs.SubRejected, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := classifyHTTP(c.style, c.status, http.Header{}, []byte(c.body))
			if e.Attr != c.wantA || e.Sub != c.wantS {
				t.Fatalf("归因错：want %s/%s got %s/%s", c.wantA, c.wantS, e.Attr, e.Sub)
			}
			if got := e.CountsAsBreakerFailure(); got != c.counts {
				t.Fatalf("计熔断应为 %v，got %v", c.counts, got)
			}
		})
	}
}

// ④ Retry-After 必须被保留（它是下界，丢了就会早重试）。
func TestRetryAfterPreserved(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "3")
	e := classifyHTTP(errs.StyleOpenAI, 429, h,
		[]byte(`{"error":{"code":"rate_limit_exceeded"}}`))
	if e.Detail["retry_after"] != "3" {
		t.Fatalf("Retry-After 丢了：%+v", e.Detail)
	}
}

// ⑤ 计数：Anthropic 走权威端点；OpenAI 明确退化为 upper_bound。
func TestCountTokens(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages/count_tokens" {
			_, _ = w.Write([]byte(`{"input_tokens":321}`))
			return
		}
		w.WriteHeader(404)
	})
	c := f.client()

	n, src, err := c.CountTokens(context.Background(),
		Target{Provider: "anthropic", Model: "claude", EndpointKind: "messages"},
		Request{Model: "claude", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if n != 321 || src != "provider" {
		t.Fatalf("权威计数错：n=%d src=%q", n, src)
	}
	if f.lastPath != "/v1/messages/count_tokens" {
		t.Fatalf("没有打到计数端点：%s", f.lastPath)
	}

	// OpenAI 面：不发请求，直接声明 upper_bound。
	f.lastPath = ""
	n2, src2, err := c.CountTokens(context.Background(),
		Target{Provider: "openai", Model: "m", EndpointKind: "chat"}, Request{})
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 || src2 != "upper_bound" {
		t.Fatalf("OpenAI 面应退化：n=%d src=%q", n2, src2)
	}
	if f.lastPath != "" {
		t.Fatalf("退化路径不该发请求，却打了 %s", f.lastPath)
	}
}

// ⑥ 不支持的能力必须有明确条目（T22 ⑨）。
func TestCapabilitiesExplicitUnsupported(t *testing.T) {
	c := CapabilitiesFor("openai")
	for _, ep := range []string{"rerank", "completions"} {
		v, ok := c.Endpoints[ep]
		if !ok {
			t.Fatalf("%s 必须有条目（缺席 = 上层读成'没问过'）", ep)
		}
		if v {
			t.Fatalf("%s 不该标支持", ep)
		}
	}
	if len(c.Unsupported) == 0 {
		t.Fatal("不支持项必须带理由")
	}
	a := CapabilitiesFor("anthropic")
	if a.CountTokens != "provider" {
		t.Fatalf("Anthropic 有权威计数端点，应标 provider，got %q", a.CountTokens)
	}
}

// ⑦ 连接失败必须归为 transport/refused（PRD §4.1.1 第 1 行），且不会被当成 4xx。
func TestConnectFailure(t *testing.T) {
	c := &Client{BaseURL: "http://127.0.0.1:1", Timeout: 200}
	_, err := c.Chat(context.Background(),
		Target{Provider: "openai", Model: "m", EndpointKind: "chat"},
		Request{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}})
	var e *errs.Error
	if !errors.As(err, &e) {
		t.Fatalf("应是 *errs.Error，got %T", err)
	}
	if e.Attr != errs.AttrTransport || e.Sub != errs.SubRefused {
		t.Fatalf("连接失败应为 transport/refused，got %s/%s", e.Attr, e.Sub)
	}
}
