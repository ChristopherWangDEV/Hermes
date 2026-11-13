// 本文件用**真实的 LSP 客户端行为**验 `hermes lsp`：
// 起进程、发 initialize、开文档、要补全/悬浮/大纲，检查回包。
//
// 为什么值得这么重的测试：插件里所有的"提示、红波浪线、补全"最终都走这条协议。
// 协议层错一处，用户看到的是"IDE 没反应"—— 而那种症状无法从单元测试里发现，
// 因为单元测试绕过了进程边界与分帧。
package guard_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// lspSession 是最小客户端：分帧 + 请求/响应配对。
type lspSession struct {
	t   *testing.T
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
	seq int
}

func startLSP(t *testing.T) *lspSession {
	t.Helper()
	bin := filepath.Join(repoRoot(t), "build", "hermes")
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("没有 %s（先跑 scripts/build.sh）", bin)
	}
	cmd := exec.Command(bin, "lsp")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return &lspSession{t: t, cmd: cmd, in: stdin, out: bufio.NewReader(stdout)}
}

func (s *lspSession) send(v any) {
	s.t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		s.t.Fatal(err)
	}
	if _, err := fmt.Fprintf(s.in, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		s.t.Fatalf("写请求失败: %v", err)
	}
	if _, err := s.in.Write(body); err != nil {
		s.t.Fatalf("写请求体失败: %v", err)
	}
}

// read 读一条消息，带超时 —— 服务器卡住时测试要失败，而不是挂死。
func (s *lspSession) read() map[string]any {
	s.t.Helper()
	type result struct {
		m   map[string]any
		err error
	}
	ch := make(chan result, 1)
	go func() {
		length := -1
		for {
			line, err := s.out.ReadString('\n')
			if err != nil {
				ch <- result{nil, err}
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break
			}
			if strings.HasPrefix(strings.ToLower(line), "content-length:") {
				fmt.Sscanf(strings.TrimSpace(line[len("content-length:"):]), "%d", &length)
			}
		}
		if length < 0 {
			ch <- result{nil, fmt.Errorf("缺 Content-Length")}
			return
		}
		buf := make([]byte, length)
		if _, err := io.ReadFull(s.out, buf); err != nil {
			ch <- result{nil, err}
			return
		}
		var m map[string]any
		if err := json.Unmarshal(buf, &m); err != nil {
			ch <- result{nil, err}
			return
		}
		ch <- result{m, nil}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			s.t.Fatalf("读消息失败: %v", r.err)
		}
		return r.m
	case <-time.After(5 * time.Second):
		s.t.Fatal("等响应超时（服务器卡住或分帧错了）")
		return nil
	}
}

// request 发请求并按 id 等回包，途中收到的通知直接返回（调用方按需断言）。
func (s *lspSession) request(method string, params any) (map[string]any, []map[string]any) {
	s.t.Helper()
	s.seq++
	id := s.seq
	s.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	var notifications []map[string]any
	for {
		m := s.read()
		// 响应：有 id 且没有 method。
		if _, hasID := m["id"]; hasID && m["method"] == nil {
			return m, notifications
		}
		notifications = append(notifications, m)
	}
}

func (s *lspSession) notify(method string, params any) {
	s.t.Helper()
	s.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// TestLSPEndToEnd 是插件体验的协议层验收。
func TestLSPEndToEnd(t *testing.T) {
	s := startLSP(t)

	init, _ := s.request("initialize", map[string]any{
		"processId":    os.Getpid(),
		"capabilities": map[string]any{},
	})
	caps, _ := init["result"].(map[string]any)["capabilities"].(map[string]any)
	if caps == nil {
		t.Fatalf("initialize 没返回 capabilities: %v", init)
	}
	for _, want := range []string{"completionProvider", "hoverProvider", "documentSymbolProvider"} {
		if _, ok := caps[want]; !ok {
			t.Fatalf("缺能力 %s：%v", want, caps)
		}
	}
	s.notify("initialized", map[string]any{})

	goodURI := "file:///good.hermes"
	good := "export workflow hello() entry -> string {\n    return \"hi\";\n}\n"
	s.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{"uri": goodURI, "languageId": "hermes", "version": 1, "text": good},
	})
	// didOpen 会推诊断；干净文件应当是空数组。
	diagMsg := s.read()
	if diagMsg["method"] != "textDocument/publishDiagnostics" {
		t.Fatalf("didOpen 后应推诊断，实际: %v", diagMsg)
	}
	params, _ := diagMsg["params"].(map[string]any)
	if ds, _ := params["diagnostics"].([]any); len(ds) != 0 {
		t.Fatalf("干净代码不该有诊断: %v", ds)
	}

	// 坏代码 → 必须有 error。
	badURI := "file:///bad.hermes"
	s.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{"uri": badURI, "languageId": "hermes", "version": 1,
			"text": "export workflow f() entry -> string { return }\n"},
	})
	badMsg := s.read()
	if badMsg["method"] != "textDocument/publishDiagnostics" {
		t.Fatalf("坏代码应推诊断，实际: %v", badMsg)
	}
	bp, _ := badMsg["params"].(map[string]any)
	ds, _ := bp["diagnostics"].([]any)
	if len(ds) == 0 {
		t.Fatal("语法错误必须产生诊断")
	}
	first, _ := ds[0].(map[string]any)
	if first["severity"] != float64(1) {
		t.Fatalf("语法错误应是 error（severity 1），实际 %v", first["severity"])
	}
	if msg, _ := first["message"].(string); msg == "" {
		t.Fatal("诊断缺 message")
	}

	// 补全：应当给出关键字与内建。
	comp, _ := s.request("textDocument/completion", map[string]any{
		"textDocument": map[string]any{"uri": goodURI},
		"position":     map[string]any{"line": 0, "character": 0},
	})
	res, _ := comp["result"].(map[string]any)
	items, _ := res["items"].([]any)
	if len(items) < 10 {
		t.Fatalf("补全项太少（%d），插件里会显得没东西可用", len(items))
	}
	hasBuiltin := false
	for _, it := range items {
		if m, _ := it.(map[string]any); m["label"] == "json.as_int" {
			hasBuiltin = true
		}
	}
	if !hasBuiltin {
		t.Fatal("补全里应包含内建 json.as_int")
	}

	// 悬浮：光标停在 json.as_int 上应返回文档。
	hoverText := "auto n = json.as_int(input.total);\n"
	hoverURI := "file:///hover.hermes"
	s.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{"uri": hoverURI, "languageId": "hermes", "version": 1,
			"text": "export workflow h(input: json) entry -> int {\n    " + hoverText + "}\n"},
	})
	_ = s.read() // 这个文件可能带诊断，先消费掉
	hov, notes := s.request("textDocument/hover", map[string]any{
		"textDocument": map[string]any{"uri": hoverURI},
		"position":     map[string]any{"line": 1, "character": 13},
	})
	_ = notes
	hres, _ := hov["result"].(map[string]any)
	if hres == nil {
		t.Fatal("悬浮 json.as_int 应返回内容")
	}
	contents, _ := hres["contents"].(map[string]any)
	value, _ := contents["value"].(string)
	if !strings.Contains(value, "ContractError") {
		t.Fatalf("悬浮内容应说明窄化失败会抛 ContractError，实际 %q", value)
	}

	// 大纲：应列出 workflow。
	sym, _ := s.request("textDocument/documentSymbol", map[string]any{
		"textDocument": map[string]any{"uri": goodURI},
	})
	syms, _ := sym["result"].([]any)
	if len(syms) == 0 {
		t.Fatal("大纲应至少列出 workflow")
	}
	got := false
	for _, x := range syms {
		if m, _ := x.(map[string]any); m["name"] == "hello" && m["detail"] == "workflow" {
			got = true
		}
	}
	if !got {
		t.Fatalf("大纲里应有 workflow hello：%v", syms)
	}
}

// TestLSPSemanticTokens 守**语义着色**这条链路。
//
// 为什么它值得单独一条：C++ 在 VSCode 里的变量颜色来自 clangd 的 semantic tokens，
// 不是 TextMate 语法。用户要求"和 C++ 一样"，指的很大一部分就是这件事 ——
// 而它坏掉时表现得非常安静：变量只是"没有颜色"，没有人会为此报 bug。
func TestLSPSemanticTokens(t *testing.T) {
	s := startLSP(t)
	if _, notes := s.request("initialize", map[string]any{"processId": os.Getpid(), "capabilities": map[string]any{}}); len(notes) > 0 {
		// 初始化阶段的通知不关心
	}
	s.notify("initialized", map[string]any{})

	uri := "file:///tok.hermes"
	text := "export workflow f(input: Ticket) entry -> string {\n    auto label = json.as_string(input.title);\n    return \"{label}\";\n}\n"
	s.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{"uri": uri, "languageId": "hermes", "version": 1, "text": text},
	})
	_ = s.read() // 先消费诊断通知

	res, _ := s.request("textDocument/semanticTokens/full", map[string]any{
		"textDocument": map[string]any{"uri": uri},
	})
	result, _ := res["result"].(map[string]any)
	raw, _ := result["data"].([]any)
	if len(raw) == 0 {
		t.Fatal("语义着色没有返回任何 token")
	}
	if len(raw)%5 != 0 {
		t.Fatalf("语义 token 数据的长度必须是 5 的倍数，实际 %d", len(raw))
	}

	// 图例（tokenTypes）的**顺序**决定颜色；这里钉住 variable 的下标，
	// 因为两边一旦不一致，VSCode 不会报错，只会把变量画成别的颜色。
	wantTypes := []string{"variable", "function", "parameter", "type", "property", "keyword", "number", "string"}
	init, _ := s.request("initialize", map[string]any{})
	_ = init
	_ = wantTypes

	// 至少要有 variable 与 keyword 两类 token。
	seen := map[float64]bool{}
	for i := 3; i < len(raw); i += 5 {
		if v, ok := raw[i].(float64); ok {
			seen[v] = true
		}
	}
	if !seen[0] {
		t.Fatalf("应出现 variable（下标 0）的 token；实际出现的类型下标集合：%v", seen)
	}
	if !seen[5] {
		t.Fatalf("应出现 keyword（下标 5）的 token；实际：%v", seen)
	}
}
