package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"hermes/internal/trace"
)

// 造一份落盘的 trace，供 CLI 测试用（不经过 native.Init，直接调 cmdTrace）。
func writeTraceFile(t *testing.T, tamper bool) string {
	t.Helper()
	var buf bytes.Buffer
	w := trace.NewWriter(&buf, trace.Options{RunID: "run-cli"})
	spans := []trace.Span{
		{ID: "s1", Parent: "", StepID: "fetch", Kind: "step", Name: "n1", Status: "ok"},
		{ID: "s2", Parent: "s1", StepID: "call_model", Kind: "provider",
			Name: "openai/qwen/chat", Status: "failed", Attr: "upstream_contract_drift",
			TokensIn: 100, TokensOut: 20, MicroUSD: 1234},
	}
	for _, s := range spans {
		if err := w.Emit(s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}

	data := buf.Bytes()
	if tamper {
		var doc map[string]any
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		if err := dec.Decode(&doc); err != nil {
			t.Fatal(err)
		}
		doc["events"].([]any)[1].(map[string]any)["status"] = "ok" // 把 failed 改成 ok
		var err error
		data, err = json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "trace.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCLIVerifyClean(t *testing.T) {
	path := writeTraceFile(t, false)
	if code := cmdTrace([]string{"verify", path}); code != 0 {
		t.Fatalf("未篡改应退出 0，got %d", code)
	}
}

func TestCLIVerifyTampered(t *testing.T) {
	path := writeTraceFile(t, true)
	if code := cmdTrace([]string{"verify", path}); code != 4 {
		t.Fatalf("篡改应退出 4，got %d", code)
	}
}

func TestCLISummary(t *testing.T) {
	path := writeTraceFile(t, false)
	if code := cmdTrace([]string{"summary", path, "--json"}); code != 0 {
		t.Fatalf("summary 应退出 0，got %d", code)
	}
}

func TestCLIUsageErrors(t *testing.T) {
	if code := cmdTrace(nil); code != 2 {
		t.Fatalf("缺子命令应退出 2，got %d", code)
	}
	if code := cmdTrace([]string{"nope"}); code != 2 {
		t.Fatalf("未知子命令应退出 2，got %d", code)
	}
	if code := cmdTrace([]string{"verify"}); code != 2 {
		t.Fatalf("缺文件应退出 2，got %d", code)
	}
	if code := cmdTrace([]string{"verify", "/nonexistent/trace.json"}); code != 4 {
		t.Fatalf("文件不存在应退出 4，got %d", code)
	}
}
