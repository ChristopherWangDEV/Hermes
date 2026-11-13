package lsp

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// newTest 造一个服务端，输入/输出都是内存缓冲。
func newTest() (*Server, *strings.Builder) {
	var out strings.Builder
	return New(strings.NewReader(""), &out), &out
}

// TestFraming 守分帧：头部按 Content-Length 读，**不按行切 JSON**。
//
// 消息体里可以有换行；按行读会把一条消息切成两半，而症状是"编辑器偶尔不响应"。
func TestFraming(t *testing.T) {
	body := "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\n\"x\":1}}"
	raw := "Content-Length: " + itoa(len(body)) + "\r\n\r\n" + body
	s := New(strings.NewReader(raw), io.Discard)

	msg, err := ReadMessage(s.in)
	if err != nil {
		t.Fatalf("读消息失败: %v", err)
	}
	if msg.Method != "initialize" {
		t.Fatalf("method 应为 initialize，实际 %q", msg.Method)
	}
	var p map[string]any
	if err := json.Unmarshal(msg.Params, &p); err != nil || p["x"] != float64(1) {
		t.Fatalf("参数解析不对: %v %v", p, err)
	}
}

// TestWriteMessage 守输出格式：头部 + 恰好一个空行 + JSON。
func TestWriteMessage(t *testing.T) {
	var b strings.Builder
	if err := WriteMessage(&b, &Message{ID: json.RawMessage("1"), Result: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.HasPrefix(out, "Content-Length: ") {
		t.Fatalf("缺 Content-Length: %q", out)
	}
	if !strings.Contains(out, "\r\n\r\n") {
		t.Fatalf("头部与体之间缺空行: %q", out)
	}
	if !strings.Contains(out, `"jsonrpc":"2.0"`) {
		t.Fatalf("缺 jsonrpc 版本: %q", out)
	}
}

// TestDiagnosticsForBadSource 守"坏代码会推诊断"。
//
// 这是插件里红色波浪线的来源；它坏掉的话，用户会以为"IDE 没反应"。
func TestDiagnosticsForBadSource(t *testing.T) {
	var out strings.Builder
	s := New(strings.NewReader(""), &out)
	s.docs["file:///bad.hermes"] = "export workflow f() entry -> string { return }\n"
	if err := s.publishDiagnostics("file:///bad.hermes"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "publishDiagnostics") {
		t.Fatalf("应推送诊断: %s", out.String())
	}
	if !strings.Contains(out.String(), `"severity":1`) {
		t.Fatalf("语法错误应是 error（severity 1）: %s", out.String())
	}
}

// TestCleanSourceHasNoDiagnostics 守"好代码不报错"。
func TestCleanSourceHasNoDiagnostics(t *testing.T) {
	var out strings.Builder
	s := New(strings.NewReader(""), &out)
	s.docs["file:///ok.hermes"] = "export workflow f() entry -> string { return \"x\"; }\n"
	if err := s.publishDiagnostics("file:///ok.hermes"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), `"severity":1`) {
		t.Fatalf("干净代码不该有 error: %s", out.String())
	}
}

// TestWordAtAndDottedAt 守 hover 的定位。
func TestWordAtAndDottedAt(t *testing.T) {
	text := "auto x = json.as_int(input.total);"
	if w := wordAt(text, position{Line: 0, Character: 14}); w != "as_int" {
		t.Fatalf("光标在 as_int 上应取到 as_int，实际 %q", w)
	}
	ns, member, ok := dottedAt(text, position{Line: 0, Character: 14})
	if !ok || ns != "json" || member != "as_int" {
		t.Fatalf("应取到 json.as_int，实际 %q.%q ok=%v", ns, member, ok)
	}
}

func itoa(n int) string { return strings.TrimSpace(strings.Join([]string{jsonNum(n)}, "")) }

func jsonNum(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
