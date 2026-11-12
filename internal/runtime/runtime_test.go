package runtime

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"hermes/internal/engine"
	"hermes/internal/ir"
)

func strp(s string) *string { return &s }

// TestModelCallsProviderAndParsesSchema 走一遍真实的 HTTP 往返：
// 地址来源解析 → provider 请求 → 响应按 schema 解析成 JSON 值。
func TestModelCallsProviderAndParsesSchema(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer k1" {
			t.Errorf("密钥应从 auth 子句解析后注入，实际 %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"{\"action\":\"refund\"}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`))
	}))
	defer srv.Close()

	rt := &Runtime{
		Backends: map[string]ir.Backend{
			"h": {
				Name: "h", Kind: "openai", Model: "m",
				Addr: ir.Addr{Src: "env", Env: "HERMES_TEST_URL"},
				Auth: &ir.Auth{Src: "env", Env: "HERMES_TEST_KEY"},
			},
		},
		Getenv: func(k string) string {
			switch k {
			case "HERMES_TEST_URL":
				return srv.URL
			case "HERMES_TEST_KEY":
				return "k1"
			}
			return ""
		},
	}

	v, err := rt.Model(context.Background(), engine.ModelRequest{
		Workflow: "w", Backend: "h", Prompt: "classify",
		Schema: []byte(`{"type":"object"}`),
	})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	m, ok := v.(map[string]any)
	if !ok || m["action"] != "refund" {
		t.Fatalf("有 schema 时应解析成 JSON 值，实际 %#v", v)
	}
}

// TestMissingAPIKeyFailsBeforeAnyRequest 守 D-023 末节：
// 缺密钥时**一个请求都不发**。把"忘了配"伪装成"上游拒绝我"，
// 会让两件事的修复动作混在一起。
func TestMissingAPIKeyFailsBeforeAnyRequest(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
	}))
	defer srv.Close()

	rt := &Runtime{
		Backends: map[string]ir.Backend{
			"h": {Name: "h", Kind: "openai", Model: "m",
				Addr: ir.Addr{Src: "literal", Value: strp(srv.URL)},
				Auth: &ir.Auth{Src: "env", Env: "HERMES_TEST_KEY"}},
		},
		Getenv: func(string) string { return "" },
	}

	_, err := rt.Model(context.Background(), engine.ModelRequest{Backend: "h"})
	if err == nil {
		t.Fatal("缺密钥必须报错")
	}
	var re *engine.RunError
	if !errors.As(err, &re) {
		t.Fatalf("缺密钥应是可捕获的 RunError，实际 %T", err)
	}
	if requests != 0 {
		t.Fatalf("缺密钥时不该发出任何请求，实际发了 %d 个", requests)
	}
}

// TestToolMapsHTTPError 守 D-010.2：非 2xx → HttpError，带 status。
func TestToolMapsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"error":"not found"}`))
	}))
	defer srv.Close()

	rt := &Runtime{}
	_, err := rt.Tool(context.Background(), engine.ToolRequest{URL: srv.URL, Method: "GET"})
	if err == nil {
		t.Fatal("404 应报错")
	}
	var re *engine.RunError
	if !errors.As(err, &re) {
		t.Fatalf("应是 RunError，实际 %T", err)
	}
	if re.Kind != "HttpError" || re.Status != 404 {
		t.Fatalf("应为 HttpError/404，实际 %s/%d", re.Kind, re.Status)
	}
	if !strings.Contains(re.Msg, "not found") {
		t.Fatalf("错误正文应保留上游内容，实际 %q", re.Msg)
	}
}
