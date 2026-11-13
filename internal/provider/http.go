package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"hermes/internal/errs"
)

// Client 是所有面共用的 HTTP 管道。它只做四件事：
// 拼 URL、发请求、把非 2xx 翻成 errs.Error、把 2xx body 交回去。
//
// 它**不**重试、不熔断、不管预算 —— 那些是 internal/resilience 与 internal/budget 的职责。
// 混进来就会变成"重试在 HTTP 层、预算在引擎层"，而两者的失败归因再也对不齐。
type Client struct {
	HTTP    *http.Client
	BaseURL string
	APIKey  string
	Timeout time.Duration
}

func (c *Client) do(ctx context.Context, style errs.Style, method, path string,
	body []byte, headers map[string]string) ([]byte, error) {

	url := c.BaseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, errs.New(errs.AttrTransport, errs.SubRefused, "provider.Call").
			WithCause(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		// 三套协议的鉴权头不同，由调用方在 headers 里给；这里只兜底一个常见的。
		if style == errs.StyleOpenAI {
			req.Header.Set("Authorization", "Bearer "+c.APIKey)
		}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: timeout}
	}

	resp, err := hc.Do(req)
	if err != nil {
		// 连接失败 / 重置 / TLS：PRD §4.1.1 第 1 行。
		return nil, errs.New(errs.AttrTransport, errs.SubRefused, "provider.Call").
			WithCause(err)
	}
	defer resp.Body.Close()

	// 限读：上游偶尔会吐一个巨大的错误页，全读进内存是最容易被打爆的地方。
	body2, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if readErr != nil {
		return nil, errs.New(errs.AttrTransport, errs.SubNone, "provider.Call").
			WithCause(readErr)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, classifyHTTP(style, resp.StatusCode, resp.Header, body2)
	}
	return body2, nil
}

// classifyHTTP 把上游错误翻成 errs.Error。**只调用 Classify**，不自己判类。
func classifyHTTP(style errs.Style, status int, hdr http.Header, body []byte) *errs.Error {
	code, etype := extractError(body, style)
	sig := errs.Signal{
		Style:  style,
		Status: status,
		Code:   code,
		Type:   etype,
		Header: map[string]string{},
	}
	if ra := hdr.Get("Retry-After"); ra != "" {
		sig.Header["Retry-After"] = ra
	}
	e, err := errs.Classify(sig)
	if err != nil {
		// 未映射：保留原始证据，但**不**伪装成 transport。
		return errs.New(errs.AttrTransport, errs.SubNone, "provider.Call").
			WithDetail("unmapped", fmt.Sprintf("status=%d body=%s", status, truncate(body, 256))).
			WithCause(err)
	}
	e.Status = status
	e.Target = ""
	if ra := hdr.Get("Retry-After"); ra != "" {
		e = e.WithDetail("retry_after", ra)
	}
	return e
}

// extractError 从三套错误体里取 (code, type)。
// Ollama 恒返回空 —— 它只有 {"error": "text"}，没有机读码（D-004）。
func extractError(body []byte, style errs.Style) (code, etype string) {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return "", ""
	}
	switch style {
	case errs.StyleOpenAI:
		var inner struct {
			Code string `json:"code"`
			Type string `json:"type"`
		}
		if raw, ok := m["error"]; ok {
			_ = json.Unmarshal(raw, &inner)
		}
		return inner.Code, inner.Type
	case errs.StyleAnthropic:
		// {"type":"error","error":{"type":"overloaded_error","message":"..."}}
		var inner struct {
			Type string `json:"type"`
		}
		if raw, ok := m["error"]; ok {
			_ = json.Unmarshal(raw, &inner)
		}
		return "", inner.Type
	case errs.StyleOllama:
		return "", ""
	}
	return "", ""
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

// doStream 与 do 相同，但**不缓冲响应体** —— 流式解析必须边读边处理。
//
// 只在状态码 2xx 时把 body 交出去；非 2xx 仍然整读（错误页通常很小，
// 而分类需要完整 body）。
func (c *Client) doStream(ctx context.Context, style errs.Style, method, path string,
	body []byte, headers map[string]string) (io.ReadCloser, error) {

	url := c.BaseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, errs.New(errs.AttrTransport, errs.SubRefused, "provider.Call").WithCause(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" && style == errs.StyleOpenAI {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// 流式不能设整体超时：那是"整个响应必须在 N 秒内读完"，
	// 而流式响应可以合理地持续很久。超时交给 ctx。
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, errs.New(errs.AttrTransport, errs.SubRefused, "provider.Call").WithCause(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, classifyHTTP(style, resp.StatusCode, resp.Header, b)
	}
	return resp.Body, nil
}
