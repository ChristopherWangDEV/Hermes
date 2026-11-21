package hermes

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fake 起一个假服务端，记录最后一次请求。
func fake(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *map[string]any, *http.Header) {
	t.Helper()
	var seen map[string]any
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		seen = body
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen, &hdr
}

func TestHealthAndModels(t *testing.T) {
	srv, _, _ := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Write([]byte(`{"status":"ok","workflows":1}`))
			return
		}
		w.Write([]byte(`{"data":[{"id":"triage"}]}`))
	})
	c := New(srv.URL, "")

	h, err := c.Health()
	if err != nil || h["status"] != "ok" {
		t.Fatalf("health: %v %v", h, err)
	}
	models, err := c.Models()
	if err != nil || len(models) != 1 || models[0] != "triage" {
		t.Fatalf("models: %v %v", models, err)
	}
}

func TestRunSendsExplicitInput(t *testing.T) {
	srv, seen, _ := fake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"model":"triage","choices":[{"message":{"content":"ok"}}]}`))
	})
	c := New(srv.URL, "")

	res, err := c.Run("triage", map[string]any{"title": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "ok" {
		t.Fatalf("content = %q", res.Content)
	}
	if (*seen)["input"] == nil {
		t.Fatalf("input 必须是显式字段，实际请求体：%v", *seen)
	}
	if (*seen)["messages"] != nil {
		t.Fatalf("不该把 input 塞进 messages：%v", *seen)
	}
}

func TestTokenIsBearer(t *testing.T) {
	srv, _, hdr := fake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok"}`))
	})
	if _, err := New(srv.URL, "s3cret").Health(); err != nil {
		t.Fatal(err)
	}
	if got := (*hdr).Get("Authorization"); got != "Bearer s3cret" {
		t.Fatalf("Authorization = %q", got)
	}
}

func TestChatStream(t *testing.T) {
	srv, _, _ := fake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, p := range []string{"你", "好"} {
			json.NewEncoder(w).Encode(map[string]any{})
			_ = p
			w.Write([]byte("data: " + `{"choices":[{"delta":{"content":"` + p + `"}}]}` + "\n\n"))
		}
		w.Write([]byte("data: [DONE]\n\n"))
	})
	var got strings.Builder
	err := New(srv.URL, "").ChatStream([]ChatMessage{{Role: "user", Content: "hi"}}, "", func(s string) error {
		got.WriteString(s)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "你好" {
		t.Fatalf("流式内容 = %q", got.String())
	}
}

func TestErrorCarriesStatus(t *testing.T) {
	srv, _, _ := fake(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		w.Write([]byte(`{"error":{"message":"upstream failed","type":"server_error"}}`))
	})
	_, err := New(srv.URL, "").Run("boom", nil)
	var he *Error
	if !errors.As(err, &he) {
		t.Fatalf("应是 *Error，实际 %T", err)
	}
	if he.Status != 503 || he.Type != "server_error" {
		t.Fatalf("status/type 不对：%+v", he)
	}
}
