package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sseServer 吐一个最小的 OpenAI 风格流。
func sseServer(t *testing.T, lines []string, stop bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for _, l := range lines {
			w.Write([]byte("data: " + l + "\n\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if stop {
			w.Write([]byte("data: [DONE]\n\n"))
		}
	}))
}

// TestStreamCallsOnChunkPerDelta 守"每个非空增量回调一次"。
//
// 首块只带 role（content 为空），**不回调** —— 否则调用方会收到一串空增量，
// 而那在 SSE 客户端看来就是"模型吐了一堆空字符串"。
func TestStreamCallsOnChunkPerDelta(t *testing.T) {
	srv := sseServer(t, []string{
		`{"id":"x","choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"id":"x","choices":[{"delta":{"content":"你"},"finish_reason":null}]}`,
		`{"id":"x","choices":[{"delta":{"content":"好"},"finish_reason":null}]}`,
		`{"id":"x","choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"x","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":2}}`,
	}, true)
	defer srv.Close()

	var got []string
	c := &Client{BaseURL: srv.URL}
	res, err := c.Chat(context.Background(), Target{Provider: "openai", Model: "m"}, Request{
		OnChunk: func(d string) error { got = append(got, d); return nil },
	})
	if err != nil {
		t.Fatalf("流式调用失败: %v", err)
	}
	if strings.Join(got, "") != "你好" {
		t.Fatalf("增量拼接应是 你好，实际 %q", got)
	}
	if res.Content != "你好" {
		t.Fatalf("最终内容应是 你好，实际 %q", res.Content)
	}
	if res.FinishReason != "stop" {
		t.Fatalf("finish_reason 应是 stop，实际 %q", res.FinishReason)
	}
	// usage 来自最后一块：不带它，流式调用的 token 只能估，而预算要真实数字。
	if res.Usage.In != 7 || res.Usage.Out != 2 {
		t.Fatalf("usage 应来自上游（7/2），实际 %d/%d", res.Usage.In, res.Usage.Out)
	}
}

// TestStreamStopsWhenCallbackErrors 守"调用方不要了就别再读"。
func TestStreamStopsWhenCallbackErrors(t *testing.T) {
	srv := sseServer(t, []string{
		`{"choices":[{"delta":{"content":"a"}}]}`,
		`{"choices":[{"delta":{"content":"b"}}]}`,
	}, false)
	defer srv.Close()

	stop := errors.New("client gone")
	c := &Client{BaseURL: srv.URL}
	res, err := c.Chat(context.Background(), Target{Provider: "openai", Model: "m"}, Request{
		OnChunk: func(string) error { return stop },
	})
	if err != nil {
		t.Fatalf("回调中止不该报错（应返回已有内容）: %v", err)
	}
	if res.Content != "a" {
		t.Fatalf("中止后应保留已收到的内容 a，实际 %q", res.Content)
	}
}

// TestStreamRejectsMalformedChunk 守"单块解不出来 = 契约漂移"。
//
// 不能把它当成"模型胡说"忽略：契约漂移意味着上游改了形状，
// 而形状变了之后所有解析都不可信。
func TestStreamRejectsMalformedChunk(t *testing.T) {
	srv := sseServer(t, []string{`{"choices":`}, false)
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	_, err := c.Chat(context.Background(), Target{Provider: "openai", Model: "m"}, Request{
		OnChunk: func(string) error { return nil },
	})
	if err == nil {
		t.Fatal("畸形块必须报错")
	}
}
