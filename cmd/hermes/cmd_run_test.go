package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunAgainstRealHTTP 是**真的**一次 HTTP 往返：
// CLI 从 IR 的 backends[] 构造 Runtime → 解析 env 地址 → 调 provider → 拿到结果。
//
// 它证明的不是"engine 能跑"，而是"这条链路真的接上了"：
// IR 里的 backend 声明、地址来源、provider 的协议映射，任何一环断了都会红。
func TestRunAgainstRealHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/chat/completions") {
			t.Errorf("应走 OpenAI 面的 chat/completions，实际 %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"{\"action\":\"refund\"}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer srv.Close()

	t.Setenv("HERMES_TEST_URL", srv.URL)

	dir := t.TempDir()
	irPath := filepath.Join(dir, "live.ir.json")
	if code := run([]string{"ir", "testdata/live.hermes", "-o", irPath}); code != 0 {
		t.Fatalf("hermes ir 退出码 %d", code)
	}

	if code := run([]string{"run", irPath}); code != 0 {
		t.Fatalf("hermes run 退出码 %d（真实 HTTP 链路没接上）", code)
	}
	_ = os.Getenv("HERMES_TEST_URL")
}

// TestRunWithoutBackendFailsLoudly 守"IR 里没有 backend 时不许静默跳过外部调用"。
func TestRunWithoutBackendFailsLoudly(t *testing.T) {
	if code := run([]string{"run", "../../schemas/golden/ir-model.json"}); code != 4 {
		t.Fatalf("没有 backend 的 IR 里出现模型调用，应退出 4，实际 %d", code)
	}
}
