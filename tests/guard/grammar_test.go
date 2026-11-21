// 本文件验 TextMate 语法的**匹配行为**（不是"文件是合法 JSON"）。
//
// 为什么不能只验 JSON：语法错一处，VSCode 的表现是"某个词没颜色" —— 那是
// 沉默的失败，用户不会报 bug，只会觉得"这个插件的配色有点怪"。
// 这里用 Go 的正则把 grammar 里的规则跑一遍，断言典型代码片段被分到正确的 scope。
package guard_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

type capture struct {
	Name string `json:"name"`
}

type pattern struct {
	Match    string             `json:"match"`
	Name     string             `json:"name"`
	Captures map[string]capture `json:"captures"`
}

type rule struct {
	Patterns []pattern `json:"patterns"`
}

type grammar struct {
	Repository map[string]rule `json:"repository"`
}

// loadGrammar 读语法文件。
func loadGrammar(t *testing.T) grammar {
	t.Helper()
	p := filepath.Join(repoRoot(t), "editor", "vscode", "syntaxes", "hermes.tmLanguage.json")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var g grammar
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

// TestGrammarPatternsCompile 先保证所有正则是合法的。
//
// 一条坏正则在 TextMate 里可能被静默忽略 —— 于是那条规则从此不再生效，而没人知道。
func TestGrammarPatternsCompile(t *testing.T) {
	g := loadGrammar(t)
	n := 0
	for rule, block := range g.Repository {
		for _, p := range block.Patterns {
			if p.Match == "" {
				continue
			}
			if _, err := regexp.Compile(p.Match); err != nil {
				t.Fatalf("规则 %s 的正则不合法: %v\n  %s", rule, err, p.Match)
			}
			n++
		}
	}
	if n < 8 {
		t.Fatalf("只找到 %d 条匹配规则，语法文件可能被截断了", n)
	}
}

// TestGrammarScopes 断言典型片段被分到正确的 scope。
//
// 这里不模拟 TextMate 的完整优先级（那是编辑器的事），只验证"这条规则能匹配到这段代码"
// —— 覆盖的是最常见的沉默失败：规则写错、名字写错、漏了关键字。
func TestGrammarScopes(t *testing.T) {
	g := loadGrammar(t)

	find := func(name string) []pattern { return g.Repository[name].Patterns }

	type tc struct {
		rule  string
		input string
		scope string
	}
	cases := []tc{
		{"keywords", "export workflow f() entry -> string {", "keyword.control.hermes"},
		{"keywords", "if (true) { return \"x\"; }", "keyword.control.hermes"},
		{"types", "struct S { string a; int b; }", "storage.type.hermes"},
		{"builtins", "auto n = json.as_int(x);", "support.function.builtin.hermes"},
		{"builtins", "catch (ModelError e) {", "support.class.error.hermes"},
		{"directives", "#link <std/net>;", "keyword.control.directive.hermes"},
		{"numbers", "retry (2) {", "constant.numeric.integer.hermes"},
		// 变量声明：`auto label = ...` 里的 label 必须有 scope，否则编辑器里
		// 它就是一片白 —— 用户会以为"变量没被识别"。
		{"variables", "    auto label = json.as_string(x);", "variable.other.readwrite.hermes"},
		// 调用处的方法名：`json.as_string(` 里的 as_string。
		{"functions", "    auto n = json.as_string(x);", "entity.name.function.hermes"},
	}

	for _, c := range cases {
		found := false
		for _, p := range find(c.rule) {
			re, err := regexp.Compile(p.Match)
			if err != nil {
				continue
			}
			m := re.FindStringSubmatch(c.input)
			if m == nil {
				continue
			}
			// 有 captures 时，看被命名的那一组；否则看整条规则的名字。
			if len(p.Captures) > 0 {
				for _, cap := range p.Captures {
					if cap.Name == c.scope {
						found = true
					}
				}
			} else if p.Name == c.scope {
				found = true
			}
		}
		if !found {
			t.Errorf("规则 %s 没把 %q 分到 %s", c.rule, c.input, c.scope)
		}
	}
}
