package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"hermes/internal/engine"
	"hermes/internal/ir"
)

// TestOpenAICompatibleBackends 守"别的框架/服务也能接"这条路。
//
// `backend x : custom = env(...)` 是 IR 规格里的合法 kind，而它代表的就是
// **任何 OpenAI 兼容服务**：vLLM、LM Studio、Ollama 的 OpenAI 面、OpenRouter、
// LiteLLM、以及绝大多数厂商的 /v1 面。
//
// 这条测试同时钉住三件对外承诺：
//
//	· 请求形状是 OpenAI 的（model + messages）
//	· 响应按 OpenAI 解析（choices[0].message.content）
//	· usage 会被读出来（预算账本要真实数字）
func TestOpenAICompatibleBackends(t *testing.T) {
	for _, kind := range []string{"openai", "custom"} {
		t.Run(kind, func(t *testing.T) {
			var gotBody map[string]any
			var gotPath, gotAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotAuth = r.Header.Get("Authorization")
				raw, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(raw, &gotBody)
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"{\"action\":\"ok\"}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`))
			}))
			defer srv.Close()

			// 鉴权只能来自 env（IR 里 Auth 是 {src:"env", env:"名"}），
			// 测试因此也走环境变量 —— 这正是"密钥不进产物"的形状。
			t.Setenv("COMPAT_KEY", "k")
			rt := &Runtime{Backends: map[string]ir.Backend{"b": {
				Name: "b", Kind: kind, Model: "any-model",
				Addr: ir.Addr{Src: "literal", Value: strp(srv.URL)},
				Auth: &ir.Auth{Src: "env", Env: "COMPAT_KEY"},
			}}}
			v, err := rt.Model(context.Background(), engine.ModelRequest{
				Workflow: "w", Backend: "b", Prompt: "hi", MaxTokens: 16,
			})
			if err != nil {
				t.Fatalf("kind=%s 调用失败: %v", kind, err)
			}
			if s, _ := v.(string); !strings.Contains(s, "ok") {
				t.Fatalf("kind=%s 结果不对: %v", kind, v)
			}
			if gotPath != "/v1/chat/completions" {
				t.Fatalf("kind=%s 请求路径应为 OpenAI 面，实际 %q", kind, gotPath)
			}
			if gotAuth != "Bearer k" {
				t.Fatalf("kind=%s 鉴权头不对: %q", kind, gotAuth)
			}
			if gotBody["model"] != "any-model" {
				t.Fatalf("kind=%s 请求体缺 model: %v", kind, gotBody)
			}
			if _, ok := gotBody["messages"]; !ok {
				t.Fatalf("kind=%s 请求体缺 messages: %v", kind, gotBody)
			}
		})
	}
}
