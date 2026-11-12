package lsp

import (
	"encoding/json"
	"sort"
	"strings"

	"hermes/internal/native"
)

// keywords 是补全与 hover 的词表。
//
// 与 `docs/LANGUAGE.md` 的语法保持一致；**不从这里推断语义** —— 语义归解析器，
// 这张表只负责"打字时有东西可补、悬停时有话说"。
var keywords = []string{
	"export", "workflow", "struct", "backend", "entry", "const", "auto", "return",
	"if", "else", "match", "retry", "try", "catch", "parallel", "true", "false",
	"input", "string", "int", "double", "bool", "json", "cost",
}

// builtins 是保留命名空间（LANGUAGE §3.3）。补全里要带上点号，用户打 `json.`
// 时才能直接看到 `as_int` 之类。
var builtins = map[string]string{
	"model.generate": "Call the model. Requires `#link <std/model>`. The return type is given by `<T>`.",
	"tool.call":      "Call an HTTP tool. Requires `#link <std/net>`. allow_status can make certain status codes a normal return.",
	"json.as_int":    "Narrowing: on failure it throws ContractError (with expect/got); it never returns a zero value.",
	"json.as_string": "Narrowing: on failure it throws ContractError.",
	"json.as_bool":   "Narrowing: on failure it throws ContractError.",
	"json.as_float":  "Narrowing to `double` (IEEE-754 binary64); the API name `json.as_float` is kept. IR constants use a 16-hex-digit bit pattern, never a JSON number.",
}

// completion 提供关键字与内建命名空间。
//
// 不做上下文感知（"这里能不能写 auto"）：那需要完整的语法分析，而补全的价值
// 在于**不出错** —— 给太多候选只是稍微啰嗦，给错候选会让用户写出编不过的代码。
func (s *Server) completion(m *Message) error {
	items := make([]any, 0, len(keywords)+len(builtins))
	for _, k := range keywords {
		items = append(items, map[string]any{
			"label": k, "kind": 14, // 14 = Keyword
		})
	}
	names := make([]string, 0, len(builtins))
	for k := range builtins {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		items = append(items, map[string]any{
			"label": k, "kind": 3, // 3 = Function
			"detail": "builtin", "documentation": builtins[k],
		})
	}
	return s.reply(m, map[string]any{"isIncomplete": false, "items": items})
}

// hover 给出光标下这个词的说明。
//
// 三种来源，按可靠性排序：内建（表里有）、关键字（表里有）、用户定义的 struct
// （从 AST dump 里找）。找不到就**不回复** —— 返回空 hover 会让编辑器弹一个空框。
func (s *Server) hover(m *Message) error {
	uri, text, pos, ok := s.posParams(m)
	if !ok {
		return s.reply(m, nil)
	}
	word := wordAt(text, pos)
	if word == "" {
		return s.reply(m, nil)
	}
	// 带命名空间的形式（`json.as_int`）优先：光标常在点号右侧。
	if ns, member, found := dottedAt(text, pos); found {
		if doc, hit := builtins[ns+"."+member]; hit {
			return s.reply(m, hoverText(doc))
		}
	}
	if doc, hit := builtins[word]; hit {
		return s.reply(m, hoverText(doc))
	}
	if isKeyword(word) {
		return s.reply(m, hoverText(keywordDoc(word)))
	}
	if doc, found := structDoc(uri, text, word); found {
		return s.reply(m, hoverText(doc))
	}
	return s.reply(m, nil)
}

func hoverText(s string) map[string]any {
	return map[string]any{
		"contents": map[string]any{"kind": "markdown", "value": s},
	}
}

func isKeyword(w string) bool {
	for _, k := range keywords {
		if k == w {
			return true
		}
	}
	return false
}

func keywordDoc(w string) string {
	switch w {
	case "export":
		return "`export` — makes the symbol appear in `exports` so other units can use it."
	case "entry":
		return "`entry` — this workflow is the unit's entry; `hermes run` / `serve` select it by default."
	case "parallel":
		return "`parallel` — runs branches concurrently, throttled by `max_par`; branches **do not share writable state**."
	case "retry":
		return "`retry` — retries. Only applies to **retryable** errors (retrying a contract error is usually still wrong)."
	case "try":
		return "`try`/`catch` — catches the language's 6 error types; the body may run partly before throwing."
	case "json":
		return "`json` — an unconstrained JSON value. Use `json.as_*` narrowing to extract a concrete type."
	case "cost":
		return "`cost` — an amount, always `int64` micro_usd (no implicit promotion to `double`; IR constants use bit patterns, never JSON float numbers)."
	}
	return "`" + w + "` — language keyword."
}

// structDoc 从 AST dump 里找 struct 声明并列出字段。
//
// 为什么用 dump 而不是再解析一次：前端已经把 AST 算出来了，dump 是它的
// 确定性文本形式；自己再写一个解析器等于把"语言的形状"实现两遍。
func structDoc(uri, text, name string) (string, bool) {
	isHeader := strings.HasSuffix(uri, ".h")
	res, err := native.Parse([]byte(text), uri, isHeader)
	if err != nil {
		return "", false
	}
	lines := strings.Split(res.Dump, "\n")
	for i, line := range lines {
		if !strings.Contains(line, "StructDecl") || !strings.HasSuffix(strings.TrimSpace(line), `"`+name+`"`) {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		var fields []string
		for _, sub := range lines[i+1:] {
			subIndent := len(sub) - len(strings.TrimLeft(sub, " "))
			if subIndent <= indent {
				break
			}
			if strings.Contains(sub, "Field ") {
				if nm := quoted(sub); nm != "" {
					fields = append(fields, nm)
				}
			}
		}
		doc := "**struct " + name + "**"
		if len(fields) > 0 {
			doc += "\n\nFields: " + strings.Join(fields, ", ")
		}
		return doc, true
	}
	return "", false
}

// quoted 取 dump 行末尾的引号内容（dump 的格式是 `Kind n=N l:c-el:ec "text"`）。
func quoted(line string) string {
	end := strings.LastIndex(line, `"`)
	if end <= 0 {
		return ""
	}
	start := strings.LastIndex(line[:end], `"`)
	if start < 0 {
		return ""
	}
	return line[start+1 : end]
}

// dottedAt 取光标处的 `ns.member`（若有）。
//
// 两种光标位置都要支持：
//
//	· 停在成员上（`json.as_|int`）—— 点号在左边
//	· 停在命名空间上（`json|.as_int`）—— 点号在右边
//
// 只支持一种会让 hover 在另一种位置上退回"关键字说明"，而用户明明指着内建。
func dottedAt(text string, pos position) (ns, member string, ok bool) {
	lines := strings.Split(text, "\n")
	if pos.Line < 0 || pos.Line >= len(lines) {
		return "", "", false
	}
	line := lines[pos.Line]
	c := pos.Character
	if c > len(line) {
		c = len(line)
	}
	isIdent := func(b byte) bool {
		return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
	}
	// 把光标落在词上：光标在词尾时向左看一格。
	if c == len(line) || !isIdent(line[c]) {
		if c > 0 && isIdent(line[c-1]) {
			c--
		}
	}
	if c >= len(line) || !isIdent(line[c]) {
		return "", "", false
	}
	start, end := c, c
	for start > 0 && isIdent(line[start-1]) {
		start--
	}
	for end < len(line) && isIdent(line[end]) {
		end++
	}

	// 右边是点号：当前词是命名空间。
	if end < len(line) && line[end] == '.' {
		mStart := end + 1
		mEnd := mStart
		for mEnd < len(line) && isIdent(line[mEnd]) {
			mEnd++
		}
		if mEnd > mStart {
			return line[start:end], line[mStart:mEnd], true
		}
	}
	// 左边是点号：当前词是成员，往左取命名空间。
	if start > 1 && line[start-1] == '.' {
		nsEnd := start - 1
		nsStart := nsEnd
		for nsStart > 0 && isIdent(line[nsStart-1]) {
			nsStart--
		}
		if nsEnd > nsStart {
			return line[nsStart:nsEnd], line[start:end], true
		}
	}
	return "", "", false
}

// documentSymbol 提供大纲：workflow / struct / backend。
//
// 数据来自 AST dump —— 和 hover 同一条理由：前端已经算过一遍了。
func (s *Server) documentSymbol(m *Message) error {
	var p struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil {
		return s.reply(m, nil)
	}
	text, ok := s.docs[p.TextDocument.URI]
	if !ok {
		return s.reply(m, nil)
	}
	res, err := native.Parse([]byte(text), p.TextDocument.URI, strings.HasSuffix(p.TextDocument.URI, ".h"))
	if err != nil {
		return s.reply(m, nil)
	}

	syms := make([]any, 0, 8)
	for _, line := range strings.Split(res.Dump, "\n") {
		kind := ""
		lspKind := 0
		switch {
		case strings.Contains(line, "WorkflowDecl "):
			kind, lspKind = "workflow", 12 // Function
		case strings.Contains(line, "StructDecl "):
			kind, lspKind = "struct", 23 // Struct
		case strings.Contains(line, "BackendDef "), strings.Contains(line, "BackendSig "):
			kind, lspKind = "backend", 13 // Variable
		default:
			continue
		}
		name := quoted(line)
		if name == "" {
			continue
		}
		syms = append(syms, map[string]any{
			"name": name, "detail": kind, "kind": lspKind,
			"range":          zeroRange(),
			"selectionRange": zeroRange(),
		})
	}
	return s.reply(m, syms)
}

func zeroRange() map[string]any {
	return map[string]any{
		"start": map[string]int{"line": 0, "character": 0},
		"end":   map[string]int{"line": 0, "character": 0},
	}
}
