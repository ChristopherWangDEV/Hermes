// Package lsp 是 Hermes 的语言服务器（T49）。
//
// 为什么自己写而不是用现成的 LSP 库：红线要求零第三方 Go 依赖，而 LSP 的传输层
// 简单得惊人 —— `Content-Length` 头 + JSON 体。为了这一层引一个库，
// 等于把一个永远升级不动的依赖绑进产品。
package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Message 是一条 JSON-RPC 消息。请求与通知都长这样，区别在于有没有 ID。
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *ResponseError  `json:"error,omitempty"`
}

// ResponseError 是 JSON-RPC 的错误对象。
type ResponseError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ReadMessage 读一条消息。
//
// LSP 的帧格式是 HTTP 风格的头部 + 空行 + JSON 体。头部**必须**按长度读，
// 不能按行读 JSON：JSON 体里可能有换行，按行切会把一条消息切成两半。
func ReadMessage(r *bufio.Reader) (*Message, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break // 头部结束
		}
		if name, value, ok := strings.Cut(line, ":"); ok {
			if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
				n, err := strconv.Atoi(strings.TrimSpace(value))
				if err != nil {
					return nil, fmt.Errorf("lsp: 非法的 Content-Length: %q", value)
				}
				length = n
			}
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("lsp: 缺 Content-Length")
	}
	if length > 64<<20 {
		// 上限不是洁癖：没有它，一个坏掉的客户端能让服务端分配任意大的内存。
		return nil, fmt.Errorf("lsp: 消息过大（%d 字节）", length)
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	var m Message
	if err := json.Unmarshal(buf, &m); err != nil {
		return nil, fmt.Errorf("lsp: 消息不是合法 JSON: %w", err)
	}
	return &m, nil
}

// WriteMessage 写一条消息并 flush。
func WriteMessage(w io.Writer, m *Message) error {
	m.JSONRPC = "2.0"
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	// 头部与体之间必须恰好一个空行 —— 少一个空行，客户端会一直等下去。
	if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}
