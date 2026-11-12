package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"hermes/internal/engine"
	"hermes/internal/ir"
)

func toolBackend(u string) map[string]ir.Backend {
	return map[string]ir.Backend{"h": {Name: "h", Kind: "openai", Model: "m",
		Addr: ir.Addr{Src: "literal", Value: strp(u)}}}
}

// TestToolSendsHeadersAndBody 守 T24：header/body 真的到了线上。
//
// 这条必须**看服务端收到了什么**，而不是看客户端构造了什么：
// 请求头写错一个字母，客户端一切正常，服务端看到的是另一个请求。
func TestToolSendsHeadersAndBody(t *testing.T) {
	var gotHeader, gotBody, gotCT string
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Tenant")
		gotCT = r.Header.Get("Content-Type")
		gotMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"q":"ok"}`))
	}))
	defer srv.Close()

	rt := &Runtime{Backends: toolBackend(srv.URL)}
	_, err := rt.Tool(context.Background(), engine.ToolRequest{
		Workflow: "w", URL: srv.URL + "/search", Method: "POST",
		Header: map[string]string{"X-Tenant": "acme"},
		Body:   map[string]any{"q": "needle"},
	})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if gotMethod != "POST" {
		t.Fatalf("method 应是 POST，实际 %q", gotMethod)
	}
	if gotHeader != "acme" {
		t.Fatalf("服务端没收到 X-Tenant: acme，实际 %q", gotHeader)
	}
	if gotCT != "application/json" {
		t.Fatalf("有 body 时应设 Content-Type=application/json，实际 %q", gotCT)
	}
	if gotBody != `{"q":"needle"}` {
		t.Fatalf("body 不对: %q", gotBody)
	}
}

// TestToolAllowStatusReturnsEnvelope 守 allow_status 的语义：
// 列表内的状态码是**正常返回**，带 status/body/headers 三元组。
//
// 这不是便利：没有它，调用方无法写"订单不存在"这类分支 ——
// 404 会被当成异常抛出，而那是业务的一部分。
func TestToolAllowStatusReturnsEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Reason", "missing")
		w.WriteHeader(404)
		w.Write([]byte(`{"error":"not found"}`))
	}))
	defer srv.Close()

	rt := &Runtime{Backends: toolBackend(srv.URL)}

	// 不在列表里 → HttpError
	if _, err := rt.Tool(context.Background(), engine.ToolRequest{
		Workflow: "w", URL: srv.URL, Method: "GET",
	}); err == nil {
		t.Fatal("404 不在 allow_status 里应报错")
	}

	// 在列表里 → 正常返回三元组
	v, err := rt.Tool(context.Background(), engine.ToolRequest{
		Workflow: "w", URL: srv.URL, Method: "GET", AllowStatus: []int{404},
	})
	if err != nil {
		t.Fatalf("404 在 allow_status 里不该报错: %v", err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("应返回三元组对象，实际 %T", v)
	}
	if m["status"] != int64(404) {
		t.Fatalf("status 应是 404，实际 %v", m["status"])
	}
	body, _ := m["body"].(map[string]any)
	if body["error"] != "not found" {
		t.Fatalf("body 应原样带出，实际 %v", m["body"])
	}
	hdrs, _ := m["headers"].(map[string]any)
	if hdrs["X-Reason"] != "missing" {
		t.Fatalf("headers 应带出，实际 %v", m["headers"])
	}
}

// TestToolBodyIsJSONEncoded 确认 body 走 JSON 而不是被 fmt 成字符串。
func TestToolBodyIsJSONEncoded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var v map[string]any
		if err := json.Unmarshal(b, &v); err != nil {
			t.Errorf("服务端收到的不是 JSON: %v（%q）", err, b)
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	rt := &Runtime{Backends: toolBackend(srv.URL)}
	_, err := rt.Tool(context.Background(), engine.ToolRequest{
		Workflow: "w", URL: srv.URL, Method: "POST",
		Body: map[string]any{"n": int64(1)},
	})
	if err != nil {
		t.Fatal(err)
	}
}
