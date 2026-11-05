package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSendsToolsAndParsesToolCalls 守 function calling 的两端：
// 请求里带 tools/tool_choice，响应里的 tool_calls 被解出来。
//
// 这是 agent 循环的地基：模型"要求调工具"这件事，只有在协议层被正确表达，
// 上层才可能去执行并回填。
func TestSendsToolsAndParsesToolCalls(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"id":"x",
			"choices":[{
				"message":{"content":null,"tool_calls":[
					{"id":"call_1","type":"function","function":{"name":"orders","arguments":"{\"id\":\"A1\"}"}},
					{"id":"call_2","type":"function","function":{"name":"history","arguments":"{}"}}
				]},
				"finish_reason":"tool_calls"
			}],
			"usage":{"prompt_tokens":5,"completion_tokens":6}
		}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	tools := json.RawMessage(`[{"type":"function","function":{"name":"orders"}}]`)
	res, err := c.Chat(context.Background(), Target{Provider: "openai", Model: "m"}, Request{
		Messages:   []Message{{Role: "user", Content: "查订单"}},
		Tools:      tools,
		ToolChoice: "auto",
	})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}

	// 请求侧：tools 必须真的发出去，否则模型无从选择。
	if _, ok := got["tools"]; !ok {
		t.Fatalf("请求体缺 tools：%v", got)
	}
	if got["tool_choice"] != "auto" {
		t.Fatalf("tool_choice 未透传：%v", got["tool_choice"])
	}

	// 响应侧：两个调用按顺序保留，参数保持原文。
	if len(res.ToolCalls) != 2 {
		t.Fatalf("应解出 2 个 tool_call，实际 %d", len(res.ToolCalls))
	}
	if res.ToolCalls[0].Name != "orders" || res.ToolCalls[0].ID != "call_1" {
		t.Fatalf("第一个调用不对：%+v", res.ToolCalls[0])
	}
	if res.ToolCalls[0].Arguments != `{"id":"A1"}` {
		t.Fatalf("参数应保留原文，实际 %q", res.ToolCalls[0].Arguments)
	}
	if res.FinishReason != "tool_calls" {
		t.Fatalf("finish_reason 应为 tool_calls，实际 %q", res.FinishReason)
	}
}
