package netcall

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"hermes/internal/runtime"
)

// TestToolGET 真走一遍 HTTP：netcall.Tool → runtime.Runtime.Tool → httptest。
func TestToolGET(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("method = %q, want GET", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	defer srv.Close()
	rt := &runtime.Runtime{HTTPClient: srv.Client()}

	out, msg, st := Tool(rt, ToolReq{URL: srv.URL, Method: "GET"})
	if st != OK {
		t.Fatalf("status=%d msg=%q", st, msg)
	}
	var v map[string]any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("bad out JSON: %v", err)
	}
	if v["id"] != "x" {
		t.Fatalf("unexpected body: %v", v)
	}
}

// TestToolHTTPError 守 D-010.2：非 2xx（且未 allow）→ HttpError(3)。
func TestToolHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()
	rt := &runtime.Runtime{HTTPClient: srv.Client()}

	if _, _, st := Tool(rt, ToolReq{URL: srv.URL, Method: "GET"}); st != HTTP {
		t.Fatalf("expected HttpError(3), got %d", st)
	}
}

// TestToolAllowStatus：allow_status 里的码算正常返回，不报错。
func TestToolAllowStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer srv.Close()
	rt := &runtime.Runtime{HTTPClient: srv.Client()}

	if _, msg, st := Tool(rt, ToolReq{URL: srv.URL, Method: "GET", AllowStatus: []int{404}}); st != OK {
		t.Fatalf("allow_status 404 应为 OK，得到 %d (%s)", st, msg)
	}
}

// TestToolSendsHeaderAndBody：头与 body 真的发出去了（不是"编过就行"）。
func TestToolSendsHeaderAndBody(t *testing.T) {
	var gotHeader, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Test")
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		if b, ok := m["k"].(string); ok {
			gotBody = b
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	rt := &runtime.Runtime{HTTPClient: srv.Client()}

	req := ToolReq{URL: srv.URL, Method: "POST", Header: map[string]string{"X-Test": "v1"}, Body: map[string]any{"k": "v2"}}
	if _, msg, st := Tool(rt, req); st != OK {
		t.Fatalf("status=%d msg=%q", st, msg)
	}
	if gotHeader != "v1" {
		t.Fatalf("header 未送达：got %q", gotHeader)
	}
	if gotBody != "v2" {
		t.Fatalf("body 未送达：got %q", gotBody)
	}
}
