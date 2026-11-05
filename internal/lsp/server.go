package lsp

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"

	"hermes/internal/native"
)

// Server 是语言服务器的状态。**每个文档一份内存副本**：补全与诊断都基于
// 编辑器里的**当前内容**，而不是磁盘上的旧版本 —— 否则用户会看到已经修好的错误。
type Server struct {
	in       *bufio.Reader
	out      io.Writer
	docs     map[string]string // uri → 文本
	shutdown bool
}

// New 建服务端。
func New(in io.Reader, out io.Writer) *Server {
	return &Server{in: bufio.NewReader(in), out: out, docs: map[string]string{}}
}

// Run 读消息直到客户端断开。
func (s *Server) Run() error {
	for {
		msg, err := ReadMessage(s.in)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if err := s.handle(msg); err != nil {
			return err
		}
		if s.shutdown && msg.Method == "exit" {
			return nil
		}
	}
}

func (s *Server) handle(m *Message) error {
	switch m.Method {
	case "initialize":
		return s.reply(m, map[string]any{
			"capabilities": map[string]any{
				// Full 同步：文件不大，换来"永远没有增量补丁算错"的确定性。
				"textDocumentSync": 1,
				"completionProvider": map[string]any{
					"triggerCharacters": []string{".", "<", "\""},
				},
				"hoverProvider":          true,
				"documentSymbolProvider": true,
				// 语义着色：C++ 在 VSCode 里的变量颜色来自 clangd 的 semantic tokens，
				// 不是 TextMate 语法。要让 Hermes 看起来一样，这一项必须有。
				"semanticTokensProvider": map[string]any{
					"legend": semanticLegend(),
					"full":   true,
				},
			},
			"serverInfo": map[string]any{"name": "hermes-lsp", "version": native.ParseVersion()},
		})
	case "initialized":
		return nil
	case "shutdown":
		s.shutdown = true
		return s.reply(m, nil)
	case "exit":
		return nil
	case "textDocument/didOpen":
		return s.didOpen(m)
	case "textDocument/didChange":
		return s.didChange(m)
	case "textDocument/didClose":
		return s.didClose(m)
	case "textDocument/completion":
		return s.completion(m)
	case "textDocument/hover":
		return s.hover(m)
	case "textDocument/documentSymbol":
		return s.documentSymbol(m)
	case "textDocument/semanticTokens/full":
		return s.semanticTokens(m)
	default:
		// 未知通知（方法名不以 / 结尾且无 ID）不回复；未知请求必须回错，
		// 否则客户端会一直等一个永不到来的响应。
		if len(m.ID) > 0 {
			return s.replyErr(m, -32601, "未支持的方法: "+m.Method)
		}
		return nil
	}
}

func (s *Server) reply(m *Message, result any) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return WriteMessage(s.out, &Message{ID: m.ID, Result: raw})
}

func (s *Server) replyErr(m *Message, code int, msg string) error {
	return WriteMessage(s.out, &Message{ID: m.ID, Error: &ResponseError{Code: code, Message: msg}})
}

// notify 发通知（诊断就是这样推给客户端的）。
func (s *Server) notify(method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return WriteMessage(s.out, &Message{Method: method, Params: raw})
}

type textDoc struct {
	URI  string `json:"uri"`
	Text string `json:"text"`
}

func (s *Server) didOpen(m *Message) error {
	var p struct {
		TextDocument textDoc `json:"textDocument"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil {
		return nil
	}
	s.docs[p.TextDocument.URI] = p.TextDocument.Text
	return s.publishDiagnostics(p.TextDocument.URI)
}

func (s *Server) didChange(m *Message) error {
	var p struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
		ContentChanges []struct {
			Text string `json:"text"`
		} `json:"contentChanges"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil {
		return nil
	}
	if len(p.ContentChanges) > 0 {
		// textDocumentSync=Full：最后一份就是全文。
		s.docs[p.TextDocument.URI] = p.ContentChanges[len(p.ContentChanges)-1].Text
	}
	return s.publishDiagnostics(p.TextDocument.URI)
}

func (s *Server) didClose(m *Message) error {
	var p struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil {
		return nil
	}
	delete(s.docs, p.TextDocument.URI)
	// 关掉的文件要清空诊断：否则编辑器里会留着已经不存在的文件的红色波浪线。
	return s.notify("textDocument/publishDiagnostics", map[string]any{
		"uri": p.TextDocument.URI, "diagnostics": []any{},
	})
}

// publishDiagnostics 用**前端解析器**产出诊断。
//
// 只用解析级诊断（语法 + 头文件规则），不做完整构建：构建要读 include 图、
// 要跑类型检查，在"每敲一个字符"的频率下太慢。完整构建的诊断留给保存时
// 或命令行 —— 快比全更重要，因为慢的 linter 会被关掉。
func (s *Server) publishDiagnostics(uri string) error {
	text, ok := s.docs[uri]
	if !ok {
		return nil
	}
	isHeader := strings.HasSuffix(uri, ".h")
	res, err := native.Parse([]byte(text), uri, isHeader)
	if err != nil {
		return nil
	}
	var diags []diagJSON
	if res.Diag != "" {
		_ = json.Unmarshal([]byte(res.Diag), &diags)
	}
	out := make([]any, 0, len(diags))
	for _, d := range diags {
		out = append(out, map[string]any{
			"range": map[string]any{
				"start": map[string]int{"line": max0(d.Line - 1), "character": max0(d.Col - 1)},
				"end":   map[string]int{"line": max0(d.EndLine - 1), "character": max0(d.EndCol - 1)},
			},
			"severity": severity(d.Severity),
			"source":   "hermes",
			"code":     d.Code,
			"message":  renderDiag(d),
		})
	}
	return s.notify("textDocument/publishDiagnostics", map[string]any{
		"uri": uri, "diagnostics": out,
	})
}

// diagJSON 对齐 C++ 前端吐出的诊断 JSON（severity 是字符串）。
type diagJSON struct {
	Severity string   `json:"severity"`
	Code     string   `json:"code"`
	Message  string   `json:"message"`
	Line     int      `json:"line"`
	Col      int      `json:"col"`
	EndLine  int      `json:"end_line"`
	EndCol   int      `json:"end_col"`
	Why      string   `json:"why"`
	Fix      []string `json:"fix"`
}

// renderDiag 把 why/fix 拼进消息体。
//
// 为什么不放在 hover 或 Code Action 里：VSCode 的诊断浮层能直接显示多行消息，
// 而"为什么错 + 怎么改"正是用户最想立刻看到的两件事。
func renderDiag(d diagJSON) string {
	var b strings.Builder
	b.WriteString(d.Message)
	if d.Why != "" {
		b.WriteString("\n\n" + d.Why)
	}
	for _, f := range d.Fix {
		if f != "" {
			b.WriteString("\n• " + f)
		}
	}
	return b.String()
}

func severity(s string) int {
	switch s {
	case "error":
		return 1
	case "warning":
		return 2
	case "info":
		return 3
	}
	return 4
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// position 从 params 里取光标位置。
type position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

func (s *Server) posParams(m *Message) (uri, text string, pos position, ok bool) {
	var p struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
		Position position `json:"position"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil {
		return "", "", position{}, false
	}
	t, has := s.docs[p.TextDocument.URI]
	if !has {
		return "", "", position{}, false
	}
	return p.TextDocument.URI, t, p.Position, true
}

// wordAt 取光标下的标识符。用的是**简单扫描**而不是词法器：
// 这里只需要"光标落在哪个词上"，而跑一遍完整词法在每帧 hover 上都太贵。
func wordAt(text string, pos position) string {
	lines := strings.Split(text, "\n")
	if pos.Line < 0 || pos.Line >= len(lines) {
		return ""
	}
	line := lines[pos.Line]
	if len(line) == 0 {
		return ""
	}
	c := pos.Character
	if c > len(line) {
		c = len(line)
	}
	isIdent := func(b byte) bool {
		return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
	}
	// 光标在词尾时向左看一个字符，否则 hover 在行末会永远取不到词。
	if c == len(line) || !isIdent(line[c]) {
		if c > 0 && isIdent(line[c-1]) {
			c--
		}
	}
	start, end := c, c
	for start > 0 && isIdent(line[start-1]) {
		start--
	}
	for end < len(line) && isIdent(line[end]) {
		end++
	}
	return line[start:end]
}
