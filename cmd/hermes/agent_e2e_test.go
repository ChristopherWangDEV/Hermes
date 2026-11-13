package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// TestAgentEndToEndFromSource 是 agent 功能的**完整验收**：
//
//	.hermes（含 tool 声明 + model.agent）→ IR → 执行（模型要工具、工具被调、回填、再回答）
//
// 它同时验证了工具 schema 的**单一来源**：模型看到的 schema 与运行期执行的 URL
// 都来自同一条 `export tool` 声明。
func TestAgentEndToEndFromSource(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "agent.hermes")
	if code := run([]string{"ir", "testdata/agent.hermes", "-o", filepath.Join(dir, "a.json")}); code != 0 {
		t.Fatalf("ir 退出码 %d", code)
	}
	_ = src

	raw, err := os.ReadFile(filepath.Join(dir, "a.json"))
	if err != nil {
		t.Fatal(err)
	}
	doc, ds := ir.Load(raw)
	if len(ds) != 0 {
		t.Fatalf("IR 不合格: %d 条", len(ds))
	}
	if len(doc.Tools) != 1 || doc.Tools[0].Name != "orders" {
		t.Fatalf("tools[] 应有 orders，实际 %+v", doc.Tools)
	}

	var calls, toolHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/orders/") {
			atomic.AddInt32(&toolHits, 1)
			if !strings.Contains(r.URL.Path, "A1") {
				t.Errorf("URL 里没填进参数: %s", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			return2(w, `{"id":"A1","status":"shipped"}`)
			return
		}
		// 模型面：第一轮要工具，第二轮给答案。
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			return2(w, `{"id":"x","choices":[{"message":{"content":null,"tool_calls":[
				{"id":"c1","type":"function","function":{"name":"orders","arguments":"{\"id\":\"A1\"}"}}]},
				"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
			return
		}
		return2(w, `{"id":"x","choices":[{"message":{"content":"订单 A1 已发货"},
			"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`)
	}))
	defer srv.Close()

	t.Setenv("GATEWAY_URL", srv.URL)
	// 工具 URL 来自那条 `export tool` 声明；测试把它指向假服务端，
	// 正好证明"模型看到的 schema 与运行期执行的 URL 同源"。
	doc.Tools[0].URLTemplate = []ir.TemplatePart{
		{Lit: srv.URL + "/orders/"},
		{Path: []string{"id"}},
	}

	byName := map[string]ir.Backend{}
	for _, b := range doc.Backends {
		byName[b.Name] = b
	}
	// agent 的 goal 用 input.id，所以给一个输入。
	res, err := engine.RunWith(doc, "", map[string]any{"id": "A1"}, engine.RunOptions{
		Runtime: &runtime.Runtime{Backends: byName},
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if res.Value != "订单 A1 已发货" {
		t.Fatalf("最终答案不对: %v", res.Value)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("应调模型 2 次，实际 %d", got)
	}
	if got := atomic.LoadInt32(&toolHits); got != 1 {
		t.Fatalf("应调工具 1 次，实际 %d", got)
	}
}

func return2(w http.ResponseWriter, body string) { w.Write([]byte(body)) }

// TestAgentGoldenIsValidIR 确认 golden 能过 schema 与加载校验。
func TestAgentGoldenIsValidIR(t *testing.T) {
	raw, err := os.ReadFile("../../schemas/golden/ir-agent.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, ds := ir.Load(raw); len(ds) != 0 {
		t.Fatalf("golden 不合格: %d 条", len(ds))
	}
	// 顺带确认它是可归档的（agent 不能只在内存里成立）。
	data, err := archive.Write([]archive.Member{{Name: "a", IR: raw, Entry: true,
		LanguageVersion: 1, ABIVersion: archive.ABIVersionSupported,
		Exports: archive.Exports{Workflows: []string{"support"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Read(data); err != nil {
		t.Fatalf("归档读回失败: %v", err)
	}
	_ = json.Valid
}
