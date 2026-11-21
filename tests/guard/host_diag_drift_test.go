package guard_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// TestHostDiagConstantsMatchCppCatalog 防的是**副本漂移**：
// `internal/ir/host_diag.go` 里那份 E_IR_LOAD_FAILED 文案是 AOT 运行期的
// 唯一来源（该路径必须 cgo-free，不能调 C++ 渲染），所以它必然会与 C++
// catalog 各存一份。两份一旦不一致，用户看到的就是两套话术 —— 而且只在
// "运行期加载坏 IR"这条冷路径上出现，日常构建永远碰不到。
//
// 注意：这条守的是副本一致性，**不是**渲染权证明（那是
// cmd/hermes 的 TestDiagTextComesFromCppCatalog + 反向变异的事）。
func TestHostDiagConstantsMatchCppCatalog(t *testing.T) {
	root := repoRoot(t)
	hostRaw, err := os.ReadFile(filepath.Join(root, "internal", "ir", "host_diag.go"))
	if err != nil {
		t.Fatalf("read host_diag.go: %v", err)
	}
	catRaw, err := os.ReadFile(filepath.Join(root, "native", "cpp", "diag_catalog.cpp"))
	if err != nil {
		t.Fatalf("read diag_catalog.cpp: %v", err)
	}
	catalog := string(catRaw)

	// 从 C++ catalog 里取 E_IR_LOAD_FAILED 的 en / zh 两条（各是一整行）。
	lineRE := regexp.MustCompile(`(?m)^\s*\{"E_IR_LOAD_FAILED",\s*"((?:[^"\\]|\\.)*)",\s*"((?:[^"\\]|\\.)*)",`)
	matches := lineRE.FindAllStringSubmatch(catalog, -1)
	if len(matches) != 2 {
		t.Fatalf("catalog 里 E_IR_LOAD_FAILED 应有 en/zh 两条，实际 %d", len(matches))
	}
	// Go 侧取**实际值**（源码里是多段字面量拼接，直接 Contains 会假红）。
	hostTexts, hostWhys := hostDiagValues(t, hostRaw)

	var catEN, catZH, whyEN, whyZH string
	for _, m := range matches {
		text := cppUnquote(t, m[1])
		why := cppUnquote(t, m[2])
		if containsHan(text) {
			catZH, whyZH = text, why
		} else {
			catEN, whyEN = text, why
		}
	}
	if catEN == "" || catZH == "" {
		t.Fatalf("catalog 的 en/zh 判定失败（en=%q zh=%q）", catEN, catZH)
	}
	if hostTexts[0] != catEN {
		t.Errorf("英文 Text 与 C++ catalog 不一致\nhost:    %q\ncatalog: %q", hostTexts[0], catEN)
	}
	if hostTexts[1] != catZH {
		t.Errorf("中文 Text 与 C++ catalog 不一致\nhost:    %q\ncatalog: %q", hostTexts[1], catZH)
	}
	if hostWhys[0] != whyEN {
		t.Errorf("英文 Why 与 C++ catalog 不一致\nhost:    %q\ncatalog: %q", hostWhys[0], whyEN)
	}
	if hostWhys[1] != whyZH {
		t.Errorf("中文 Why 与 C++ catalog 不一致\nhost:    %q\ncatalog: %q", hostWhys[1], whyZH)
	}
}

// cppUnquote 还原 C 字符串字面量里的转义（两份文件都只用到 \" 与 \n）。
func cppUnquote(t *testing.T, s string) string {
	t.Helper()
	out := strings.ReplaceAll(s, `\"`, `"`)
	out = strings.ReplaceAll(out, `\n`, "\n")
	return out
}

// hostExcerpt 给失败信息一段上下文（只取英文那段）。
func hostExcerpt(host string) string {
	i := strings.Index(host, "the IR bytes are")
	if i < 0 {
		return "(host_diag.go 里找不到英文文案起点)"
	}
	end := i + 120
	if end > len(host) {
		end = len(host)
	}
	return host[i:end]
}

// containsHan 报告字符串里有没有汉字。
func containsHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

// hostDiagValues 从 host_diag.go 的 AST 里取出 [en, zh] 的 Text 与 Why 实际值。
func hostDiagValues(t *testing.T, src []byte) (texts, whys []string) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "host_diag.go", src, 0)
	if err != nil {
		t.Fatalf("parse host_diag.go: %v", err)
	}
	texts = make([]string, 2)
	whys = make([]string, 2)
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
				continue
			}
			var idx int
			switch vs.Names[0].Name {
			case "hostLoadFailedEN":
				idx = 0
			case "hostLoadFailedZH":
				idx = 1
			default:
				continue
			}
			cl, ok := vs.Values[0].(*ast.CompositeLit)
			if !ok {
				continue
			}
			for _, el := range cl.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok {
					continue
				}
				val := evalStringExpr(kv.Value)
				switch key.Name {
				case "Text":
					texts[idx] = val
				case "Why":
					whys[idx] = val
				}
			}
		}
	}
	if texts[0] == "" || texts[1] == "" || whys[0] == "" || whys[1] == "" {
		t.Fatalf("host_diag.go 的常量解析不全（texts=%q whys=%q）", texts, whys)
	}
	return texts, whys
}

// evalStringExpr 求一个"字符串字面量或它们的 + 拼接"的值。
func evalStringExpr(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind == token.STRING {
			s, err := strconv.Unquote(v.Value)
			if err == nil {
				return s
			}
		}
	case *ast.BinaryExpr:
		if v.Op == token.ADD {
			return evalStringExpr(v.X) + evalStringExpr(v.Y)
		}
	}
	return ""
}
