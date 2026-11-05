package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"hermes/internal/errs"
)

// Chat 按 target.Provider 分派到对应协议面。返回的 Response 是归一化的。
func (c *Client) Chat(ctx context.Context, t Target, r Request) (*Response, error) {
	switch t.Provider {
	case "openai", "custom":
		// `custom` 与 `openai` 同一套协议面。放在这里而不只在 runtime 里映射，
		// 是因为 provider 是**可独立使用**的库：直接调它的人不该因为传了 custom 就炸。
		return c.chatOpenAI(ctx, t, r)
	case "anthropic":
		return c.chatAnthropic(ctx, t, r)
	case "ollama":
		return c.chatOllama(ctx, t, r)
	}
	return nil, fmt.Errorf("provider: 未知 provider %q", t.Provider)
}

// ---------------- OpenAI ----------------

func (c *Client) chatOpenAI(ctx context.Context, t Target, r Request) (*Response, error) {
	// 不设 temperature：那是浮点参数，而本项目的 API 面刻意不接受浮点。
	streaming := r.OnChunk != nil
	body := map[string]any{
		"model":    t.Model,
		"messages": r.Messages,
		"stream":   streaming,
	}
	if streaming {
		// 让上游在最后一块里带 usage：不带的话流式调用的 token 数只能估，
		// 而预算账本要的是真实数字。
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	if r.MaxTokens > 0 {
		body["max_tokens"] = r.MaxTokens
	}
	if len(r.ResponseFormat) > 0 {
		body["response_format"] = r.ResponseFormat
	}
	if len(r.Tools) > 0 {
		body["tools"] = r.Tools
		if r.ToolChoice != "" {
			body["tool_choice"] = r.ToolChoice
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	if streaming {
		return c.chatOpenAIStreaming(ctx, t, r, raw)
	}
	respBody, err := c.do(ctx, errs.StyleOpenAI, "POST", "/v1/chat/completions", raw, nil)
	if err != nil {
		return nil, err
	}

	var out struct {
		ID      string `json:"id"`
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		// 2xx 但解不出来 = 形状变了，不是模型胡说。
		return nil, errs.New(errs.AttrUpstreamContractDrift, errs.SubNone, "provider.Call").
			WithDetail("body", truncate(respBody, 256)).WithCause(err)
	}
	res := &Response{
		ID:            out.ID,
		RequestSHA256: sha256Hex(raw),
		Usage:         Usage{In: out.Usage.PromptTokens, Out: out.Usage.CompletionTokens, Source: "provider"},
	}
	if len(out.Choices) > 0 {
		res.Content = out.Choices[0].Message.Content
		res.FinishReason = out.Choices[0].FinishReason
		// 工具调用按返回顺序保留：回填时必须同序，否则模型会把结果张冠李戴。
		for _, tc := range out.Choices[0].Message.ToolCalls {
			res.ToolCalls = append(res.ToolCalls, ToolCall{
				ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments,
			})
		}
	}
	return res, nil
}

// ---------------- Anthropic ----------------

func (c *Client) chatAnthropic(ctx context.Context, t Target, r Request) (*Response, error) {
	maxTok := r.MaxTokens
	if maxTok <= 0 {
		maxTok = 4096 // Anthropic 必须给 max_tokens；缺了会被上游拒
	}
	body := map[string]any{
		"model":      t.Model,
		"messages":   r.Messages,
		"max_tokens": maxTok,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	hdr := map[string]string{"anthropic-version": "2023-06-01"}
	if c.APIKey != "" {
		hdr["x-api-key"] = c.APIKey
	}
	respBody, err := c.do(ctx, errs.StyleAnthropic, "POST", "/v1/messages", raw, hdr)
	if err != nil {
		return nil, err
	}

	var out struct {
		ID      string `json:"id"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, errs.New(errs.AttrUpstreamContractDrift, errs.SubNone, "provider.Call").
			WithDetail("body", truncate(respBody, 256)).WithCause(err)
	}
	res := &Response{
		ID:            out.ID,
		FinishReason:  out.StopReason,
		RequestSHA256: sha256Hex(raw),
		Usage:         Usage{In: out.Usage.InputTokens, Out: out.Usage.OutputTokens, Source: "provider"},
	}
	for _, cb := range out.Content {
		if cb.Type == "text" {
			res.Content += cb.Text
		}
	}
	return res, nil
}

// ---------------- Ollama ----------------

func (c *Client) chatOllama(ctx context.Context, t Target, r Request) (*Response, error) {
	body := map[string]any{"model": t.Model, "messages": r.Messages, "stream": false}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	respBody, err := c.do(ctx, errs.StyleOllama, "POST", "/api/chat", raw, nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		DoneReason      string `json:"done_reason"`
		PromptEvalCount int64  `json:"prompt_eval_count"`
		EvalCount       int64  `json:"eval_count"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, errs.New(errs.AttrUpstreamContractDrift, errs.SubNone, "provider.Call").
			WithDetail("body", truncate(respBody, 256)).WithCause(err)
	}
	return &Response{
		Content:       out.Message.Content,
		FinishReason:  out.DoneReason,
		RequestSHA256: sha256Hex(raw),
		Usage:         Usage{In: out.PromptEvalCount, Out: out.EvalCount, Source: "provider"},
	}, nil
}

// ---------------- 计数（§1.7） ----------------

// CountTokens 返回 (token 数, 来源, err)。
//
// 能拿到权威计数就用权威计数：Homonoia 的 count 端点与真实请求共用同一 renderer，
// 因此它描述的一定是模型真会看到的 prompt。拿不到就明确返回 "upper_bound"，
// 由调用方用声明的上界去算 —— **不拿本地估算冒充精确值**。
func (c *Client) CountTokens(ctx context.Context, t Target, r Request) (int64, string, error) {
	switch t.Provider {
	case "anthropic":
		body := map[string]any{"model": t.Model, "messages": r.Messages}
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, "", err
		}
		hdr := map[string]string{"anthropic-version": "2023-06-01"}
		if c.APIKey != "" {
			hdr["x-api-key"] = c.APIKey
		}
		respBody, err := c.do(ctx, errs.StyleAnthropic, "POST", "/v1/messages/count_tokens", raw, hdr)
		if err != nil {
			return 0, "", err
		}
		var out struct {
			InputTokens int64 `json:"input_tokens"`
		}
		if err := json.Unmarshal(respBody, &out); err != nil {
			return 0, "", errs.New(errs.AttrUpstreamContractDrift, errs.SubNone, "provider.CountTokens").
				WithCause(err)
		}
		return out.InputTokens, "provider", nil

	case "openai":
		// OpenAI chat 面在 Homonoia 上没有对应计数端点（§1.7）。
		// 返回 upper_bound 而不是编一个数：编出来的数会让预算与上游计费长期对不上。
		return 0, "upper_bound", nil
	case "ollama":
		return 0, "upper_bound", nil
	}
	return 0, "upper_bound", nil
}

// Capabilities 描述某个 provider 的能力。**不支持也必须有条目**（T22 ⑨）。
func CapabilitiesFor(provider string) Capabilities {
	switch provider {
	case "openai":
		return Capabilities{
			Endpoints: map[string]bool{
				"chat": true, "responses": true, "embeddings": true, "images": true,
				"completions": false, "rerank": false,
			},
			Streaming: true, Tools: true, JSONSchema: true,
			CountTokens: "upper_bound",
			Unsupported: []string{
				"/v1/rerank：语言层没有 rerank 原语，作为普通工具调用更清楚",
				"/v1/completions：legacy text completion 无 workflow 语义",
			},
		}
	case "anthropic":
		return Capabilities{
			Endpoints: map[string]bool{"messages": true, "count_tokens": true},
			Streaming: true, Tools: true, JSONSchema: false,
			CountTokens: "provider",
			Unsupported: []string{
				"/v1/embeddings：Anthropic 面不提供，走 OpenAI 兼容面",
			},
		}
	case "ollama":
		return Capabilities{
			Endpoints: map[string]bool{"chat": true, "generate": true},
			Streaming: true, Tools: false, JSONSchema: false,
			CountTokens: "upper_bound",
			Unsupported: []string{
				"tools：Ollama 无 tool_choice，流式工具调用当前不可达",
			},
		}
	}
	return Capabilities{Endpoints: map[string]bool{}}
}

// chatOpenAIStreaming 解析 OpenAI 的 SSE 流。
//
// 三件事必须做对：
//  1. 每块 `data: {...}` 里的 `choices[0].delta.content` 可能为空（首块只带 role），
//     空的**不要**回调 —— 否则调用方会收到一串空增量。
//  2. `data: [DONE]` 是结束哨兵，不是 JSON。
//  3. usage 在最后一块（因为请求里带了 stream_options），要累加进 Response，
//     否则流式调用的 token 数只能靠估，而预算账本要真实数字。
func (c *Client) chatOpenAIStreaming(ctx context.Context, t Target, r Request, raw []byte) (*Response, error) {
	bodyRC, err := c.doStream(ctx, errs.StyleOpenAI, "POST", "/v1/chat/completions", raw, nil)
	if err != nil {
		return nil, err
	}
	defer bodyRC.Close()

	res := &Response{RequestSHA256: sha256Hex(raw), Usage: Usage{Source: "provider"}}
	var content strings.Builder

	sc := bufio.NewScanner(bodyRC)
	// 单块可能很长（模型偶尔一次吐很多），默认 64KB 会截断。
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk struct {
			ID      string `json:"id"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			// 单块解不出来：这是上游契约漂移，不是"模型胡说"，必须报出来。
			return nil, errs.New(errs.AttrUpstreamContractDrift, errs.SubNone, "provider.Call").
				WithDetail("chunk", truncate([]byte(payload), 256)).WithCause(err)
		}
		if chunk.ID != "" {
			res.ID = chunk.ID
		}
		if chunk.Usage != nil {
			res.Usage.In = chunk.Usage.PromptTokens
			res.Usage.Out = chunk.Usage.CompletionTokens
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.Content != "" {
				content.WriteString(ch.Delta.Content)
				if err := r.OnChunk(ch.Delta.Content); err != nil {
					// 调用方不要了（客户端断开）：中止读取，返回已有内容。
					res.Content = content.String()
					return res, nil
				}
			}
			if ch.FinishReason != "" {
				res.FinishReason = ch.FinishReason
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, errs.New(errs.AttrTransport, errs.SubNone, "provider.Call").WithCause(err)
	}
	res.Content = content.String()
	return res, nil
}
