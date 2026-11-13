// Package hermes 是与 Hermes 运行时通信的客户端。
//
// 它说 `hermes serve` 的 HTTP 面（四个端点，与 OpenAI 同形，D-021）：
// 不需要安装 hermes、不需要 cgo、不需要链接任何东西 —— 只要能发 HTTP。
//
// 零第三方依赖：只用标准库。SDK 引第三方库，等于把那棵依赖树带进每个使用者的
// 构建里，而这里要做的事标准库全都够。
package hermes

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Error 是服务端返回的错误。
//
// 保留 Type 与 Status：调用方要能区分"没带凭证（401）"与"上游/预算出了问题（503）"
// —— 两者的处置完全不同。
type Error struct {
	Message string
	Type    string
	Status  int
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("hermes: %s (status=%d type=%s)", e.Message, e.Status, e.Type)
	}
	return "hermes: " + e.Message
}

// ChatMessage 是 OpenAI 形状的一条消息。
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatResult 是一次执行的结果。
type ChatResult struct {
	Content string
	Model   string
	Raw     json.RawMessage
}

// Client 是服务端客户端。
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// New 建客户端。baseURL 指向 `hermes serve --addr`，token 对应 `--token`。
func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *Client) do(method, path string, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.BaseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	return hc.Do(req)
}

// decodeError 把非 2xx 的响应变成 *Error。
func decodeError(resp *http.Response) error {
	defer resp.Body.Close()
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = json.Unmarshal(raw, &payload)
	msg := payload.Error.Message
	if msg == "" {
		msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return &Error{Message: msg, Type: payload.Error.Type, Status: resp.StatusCode}
}

// Health 返回存活与能力。
func (c *Client) Health() (map[string]any, error) {
	resp, err := c.do(http.MethodGet, "/health", nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, decodeError(resp)
	}
	defer resp.Body.Close()
	var out map[string]any
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

// Models 列出可用的 workflow 名（服务端只列 exported 的）。
func (c *Client) Models() ([]string, error) {
	resp, err := c.do(http.MethodGet, "/v1/models", nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, decodeError(resp)
	}
	defer resp.Body.Close()
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(payload.Data))
	for _, m := range payload.Data {
		out = append(out, m.ID)
	}
	return out, nil
}

// Run 执行一个 workflow。
//
// input 是**显式**的（非 OpenAI 标准）：workflow 的输入契约由 `.h` 里的 struct
// 定义，把它映射到 messages 只能靠猜。传 nil 表示不需要输入。
func (c *Client) Run(workflow string, input map[string]any) (*ChatResult, error) {
	body := map[string]any{"model": workflow}
	if input != nil {
		body["input"] = input
	}
	return c.chatLike(body, workflow)
}

// Chat 按 OpenAI 的 messages 形状调用（服务端取最后一条 user 消息当 text）。
func (c *Client) Chat(messages []ChatMessage, model string) (*ChatResult, error) {
	return c.chatLike(map[string]any{"model": model, "messages": messages}, model)
}

func (c *Client) chatLike(body map[string]any, model string) (*ChatResult, error) {
	resp, err := c.do(http.MethodPost, "/v1/chat/completions", body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, decodeError(resp)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	content := ""
	if len(parsed.Choices) > 0 {
		content = parsed.Choices[0].Message.Content
	}
	m := parsed.Model
	if m == "" {
		m = model
	}
	return &ChatResult{Content: content, Model: m, Raw: raw}, nil
}

// ChatStream 流式调用：每个增量调用一次 onChunk。
//
// 服务端是真流式（provider 层接上游 SSE），所以 onChunk 会在响应还没结束时被调用 ——
// 这正是流式的意义。onChunk 返回错误表示调用方不想再要了，此时中止读取。
func (c *Client) ChatStream(messages []ChatMessage, model string, onChunk func(string) error) error {
	resp, err := c.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": model, "messages": messages, "stream": true,
	})
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return decodeError(resp)
	}
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			return nil
		}
		var chunk struct {
			Error *struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return fmt.Errorf("hermes: stream chunk is not valid JSON: %w", err)
		}
		if chunk.Error != nil {
			return &Error{Message: chunk.Error.Message, Type: chunk.Error.Type}
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.Content == "" {
				continue
			}
			if err := onChunk(ch.Delta.Content); err != nil {
				return nil // 调用方不要了：保留已收到的内容，正常返回。
			}
		}
	}
	return sc.Err()
}
