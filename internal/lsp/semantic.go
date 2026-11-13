package lsp

import (
	"encoding/json"
	"sort"
	"strings"
)

// 语义着色（semantic tokens）。
//
// 为什么需要它：C++ 在 VSCode 里的变量颜色**不是 TextMate 语法给的**，
// 是 clangd 通过语义着色给的。语法只能做"行内"匹配，跨行追踪不到作用域；
// 而用户对"C++ 那样的体验"的期待，很大一部分就是"变量、函数、类型各是各的颜色"。
//
// 这里的着色是基于**文本扫描**的近似，不是完整的名字解析 —— 诚实说明边界：
// 它能覆盖 decl 绑定、调用点、类型名、字段声明、关键字与数字；
// 它不能区分同名变量的不同作用域（那需要符号表，等类型检查接进 LSP 再做）。
var (
	semanticTokenTypes = []string{
		"variable", "function", "parameter", "type", "property",
		"keyword", "number", "string",
	}
	tokenIndexOf = map[string]int{}
)

func init() {
	for i, t := range semanticTokenTypes {
		tokenIndexOf[t] = i
	}
}

// semanticLegend 是对客户端声明的图例。**必须与 extension.js 里的一致** ——
// 两边不一致时，VSCode 会把 variable 画成 function 的颜色，而不会报错。
func semanticLegend() map[string]any {
	return map[string]any{
		"tokenTypes":     semanticTokenTypes,
		"tokenModifiers": []string{},
	}
}

// tokenize 产出行内的语义 token。
func tokenize(text string) [][]int {
	lines := strings.Split(text, "\n")
	type tok struct {
		line, start, length, typ int
	}
	var toks []tok
	add := func(line, start, length int, typ string) {
		idx, ok := tokenIndexOf[typ]
		if !ok || length <= 0 {
			return
		}
		toks = append(toks, tok{line: line, start: start, length: length, typ: idx})
	}

	for ln, raw := range lines {
		// 注释整行跳过：注释里的词不该着色（否则注释里写 auto x 也会变色）。
		if i := strings.Index(raw, "//"); i >= 0 {
			raw = raw[:i]
		}
		words := scanWords(raw)
		for i, w := range words {
			switch {
			case isKeyword(w.text):
				add(ln, w.start, len(w.text), "keyword")
			// C++ 风格声明：`int x` / `auto x` / `const string X`。
			// 判定方式是**看前一个词**：它是类型关键字或 auto 时，这个词是变量名。
			case i > 0 && (words[i-1].text == "auto" || isTypeKeyword(words[i-1].text)):
				add(ln, w.start, len(w.text), "variable")
			case i > 2 && words[i-2].text == "const" &&
				(words[i-1].text == "auto" || isTypeKeyword(words[i-1].text)):
				add(ln, w.start, len(w.text), "variable")
			case i > 1 && words[i-2].text == "auto" && words[i-1].text == "[":
				add(ln, w.start, len(w.text), "variable")
			case isStructKeyword(words, i):
				add(ln, w.start, len(w.text), "type")
			case i+1 < len(words) && words[i+1].text == "(":
				add(ln, w.start, len(w.text), "function")
			case i > 0 && isTypeKeyword(words[i-1].text):
				add(ln, w.start, len(w.text), "property")
			case isUpperStart(w.text):
				add(ln, w.start, len(w.text), "type")
			}
		}
		for _, n := range scanNumbers(raw) {
			add(ln, n.start, len(n.text), "number")
		}
	}

	// LSP 要求按位置排序后做**增量编码**。
	sort.Slice(toks, func(i, j int) bool {
		if toks[i].line != toks[j].line {
			return toks[i].line < toks[j].line
		}
		return toks[i].start < toks[j].start
	})
	data := make([]int, 0, len(toks)*5)
	prevLine, prevStart := 0, 0
	for _, t := range toks {
		dl := t.line - prevLine
		ds := t.start
		if dl == 0 {
			ds = t.start - prevStart
		}
		data = append(data, dl, ds, t.length, t.typ, 0)
		prevLine, prevStart = t.line, t.start
	}
	return [][]int{data}
}

type word struct {
	text  string
	start int
}

// scanWords 取一行里的标识符及其列号。
func scanWords(line string) []word {
	var out []word
	for i := 0; i < len(line); {
		if !isIdentByte(line[i]) {
			i++
			continue
		}
		start := i
		for i < len(line) && isIdentByte(line[i]) {
			i++
		}
		out = append(out, word{text: line[start:i], start: start})
	}
	return out
}

func isIdentByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func scanNumbers(line string) []word {
	var out []word
	for i := 0; i < len(line); {
		if line[i] < '0' || line[i] > '9' {
			i++
			continue
		}
		// 前一个字符是标识符字符时，这不是一个独立数字（比如 foo2）。
		if i > 0 && isIdentByte(line[i-1]) {
			i++
			continue
		}
		start := i
		for i < len(line) && line[i] >= '0' && line[i] <= '9' {
			i++
		}
		out = append(out, word{text: line[start:i], start: start})
	}
	return out
}

func isStructKeyword(words []word, i int) bool {
	if i == 0 {
		return false
	}
	prev := words[i-1].text
	return prev == "struct" || prev == "workflow" || prev == "backend"
}

func isTypeKeyword(s string) bool {
	switch s {
	case "string", "int", "double", "bool", "json", "cost":
		return true
	}
	return false
}

func isUpperStart(s string) bool {
	return len(s) > 0 && s[0] >= 'A' && s[0] <= 'Z'
}

// semanticTokens 处理 `textDocument/semanticTokens/full`。
func (s *Server) semanticTokens(m *Message) error {
	var p struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil {
		return s.reply(m, map[string]any{"data": []int{}})
	}
	text, ok := s.docs[p.TextDocument.URI]
	if !ok {
		return s.reply(m, map[string]any{"data": []int{}})
	}
	data := tokenize(text)[0]
	return s.reply(m, map[string]any{"data": data})
}
