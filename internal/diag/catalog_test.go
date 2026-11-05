package diag

import (
	"sort"
	"strings"
	"testing"
)

// 本文件的每条测试都是**报错质量的机器保证**。
//
// 为什么值得为一堆文案写测试：报错是产品的一部分，而且是最难靠人工回归的部分
// —— 没人会为了"检查错误消息还通顺"去手动触发 11 种坏 IR。LangChain 的报错
// 之所以难用，不是因为它没写文案，而是因为**没有任何机制保证文案完整**：
// 新增一条校验路径时顺手写一句 `errors.New("invalid")` 就上线了。
//
// 这里的机制是：**没有 why、没有两条改法、参数对不上、缺一种语言 → 测试红。**

// codeSet 返回代码里声明的全部错误码常量。
// 用字面量列举而不是反射，因为反射拿不到 const 的值，而用 go/parser 扫源码
// 又太重；这份清单本身由 TestCodeConstantsMatchCatalog 的双向比对保护
// —— 加了码忘了登记，这里就会缺、测试就会红。
func codeSet() []string {
	return []string{
		CodeDupID, CodeDanglingRef, CodeCycle, CodeBadRoot, CodeVarMismatch,
		CodeBadLoc, CodeFloat, CodeSchemaUnstable, CodeExportMismatch,
		CodeEntry, CodeSymConflict, CodePanelTruncated, CodeRecomputeDrift,
		CodeLoadFailed, CodeDynamicCall,
		CodeIncludeCycle, CodeIncludeHermes, CodePathEscape,
		CodeIncludeDepth, CodeIncludeTooMany, CodeLinkBadModule, CodeLinkNotStd,
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestCatalogCoversEveryCode 双向：每个码有文案，每条文案有码。
func TestCatalogCoversEveryCode(t *testing.T) {
	declared := map[string]bool{}
	for _, c := range codeSet() {
		if declared[c] {
			t.Errorf("错误码 %s 在 codeSet() 里重复声明", c)
		}
		declared[c] = true
	}
	for _, lang := range []Lang{LangEN, LangZH} {
		cat := catalog[lang]
		if len(cat) == 0 {
			t.Fatalf("语言 %s 的目录是空的", lang)
		}
		for _, c := range codeSet() {
			if _, ok := cat[c]; !ok {
				t.Errorf("语言 %s 缺错误码 %s 的文案（每种语言都必须齐全，否则合并输出时会露出英文）", lang, c)
			}
		}
		for _, c := range sortedKeys(cat) {
			if !declared[c] {
				t.Errorf("语言 %s 的目录里有 %s，但代码里没有这个常量（死条目，或常量名写错）", lang, c)
			}
		}
	}
}

// TestLanguagesHaveIdenticalKeys en 与 zh 的码集合必须完全相同。
func TestLanguagesHaveIdenticalKeys(t *testing.T) {
	en := sortedKeys(catalog[LangEN])
	zh := sortedKeys(catalog[LangZH])
	if strings.Join(en, ",") != strings.Join(zh, ",") {
		t.Errorf("en 与 zh 的码集合不一致：\n  en=%v\n  zh=%v", en, zh)
	}
}

// TestEveryMessageHasWhyAndTwoFixes 这是"比 Python 好"最核心的一条。
//
// Python 的 KeyError 只告诉你"缺了这个键"，不说"这里该是什么"，也不说"怎么改"。
// 我们的每条诊断必须给出：后果（Why）+ **至少两条**改法（Fix）。
//
// 为什么坚持两条：只有一种改法的错误，通常意味着"这个约束没想清楚"——
// 要么约束本身可以放宽，要么生产者根本产不出正确的东西。逼自己写出第二种改法，
// 会把这类设计问题提前暴露。
func TestEveryMessageHasWhyAndTwoFixes(t *testing.T) {
	for _, lang := range []Lang{LangEN, LangZH} {
		for _, c := range sortedKeys(catalog[lang]) {
			m := catalog[lang][c]
			if strings.TrimSpace(m.Text) == "" {
				t.Errorf("%s/%s: Text 为空", lang, c)
			}
			if strings.TrimSpace(m.Why) == "" {
				t.Errorf("%s/%s: Why 为空 —— 不说后果的报错等于没解释", lang, c)
			}
			if len(m.Fix) < 2 {
				t.Errorf("%s/%s: 只有 %d 条改法，至少要 2 条（只有一种改法通常说明约束没想清楚）",
					lang, c, len(m.Fix))
			}
			for i, f := range m.Fix {
				if strings.TrimSpace(f) == "" {
					t.Errorf("%s/%s: Fix[%d] 为空", lang, c, i)
				}
			}
		}
	}
}

// TestPlaceholdersMatchDeclaredArgs 占位符与 Args 声明必须**双向**一致。
//
// 单向会漏两件事：
//
//	· 文案里写了 {x} 但没人传 → 渲染出字面 "{x}"，用户看到一个没被替换的模板
//	· 声明了 Args 却没在文案里用 → 翻译者看不到这个参数，中英文会漂
func TestPlaceholdersMatchDeclaredArgs(t *testing.T) {
	for _, lang := range []Lang{LangEN, LangZH} {
		for _, c := range sortedKeys(catalog[lang]) {
			m := catalog[lang][c]

			// 收集模板里出现的全部占位符（Text + Why + 每条 Fix）
			used := map[string]bool{}
			for _, s := range append([]string{m.Text, m.Why}, m.Fix...) {
				for _, p := range placeholderNames(s) {
					used[p] = true
				}
			}

			declared := map[string]bool{}
			for _, a := range m.Args {
				if declared[a] {
					t.Errorf("%s/%s: Args 里 %q 重复声明", lang, c, a)
				}
				declared[a] = true
			}

			for p := range used {
				if !declared[p] {
					t.Errorf("%s/%s: 文案里用了 {%s}，但 Args 没声明它（Args=%v）", lang, c, p, m.Args)
				}
			}
			for a := range declared {
				if !used[a] {
					t.Errorf("%s/%s: Args 声明了 %q，但文案里一次都没用到", lang, c, a)
				}
			}
		}
	}
}

// TestLanguagesUseSameArgs 同一条码在两种语言下必须接受同一组参数。
// 否则切语言时会缺参，渲染出字面的 {x}。
func TestLanguagesUseSameArgs(t *testing.T) {
	for _, c := range codeSet() {
		en, ok1 := catalog[LangEN][c]
		zh, ok2 := catalog[LangZH][c]
		if !ok1 || !ok2 {
			continue // 已由 TestCatalogCoversEveryCode 报过
		}
		a := append([]string(nil), en.Args...)
		b := append([]string(nil), zh.Args...)
		sort.Strings(a)
		sort.Strings(b)
		if strings.Join(a, ",") != strings.Join(b, ",") {
			t.Errorf("%s: en 的 Args=%v 与 zh 的 Args=%v 不一致", c, en.Args, zh.Args)
		}
	}
}

// TestRenderIsStable 锁住渲染骨架。文案会改，骨架不能改 ——
// 骨架变了等于所有下游的日志解析、grep 脚本、文档示例全部失效。
func TestRenderIsStable(t *testing.T) {
	loc := Loc{Module: "support.hbc", Path: "workflows[0].nodes[3].kids[1]", Line: 7, Col: 9}
	args := map[string]string{
		"what": "workflows[0].nodes[3].kids[1]", "id": "99",
		"table": "nodes", "range": "5 entries (0..4)",
		"path": "workflows[0].nodes[3].kids[1]",
	}
	d := New(CodeDanglingRef, SeverityError, loc, LangEN, args)
	got := d.Render()

	for _, want := range []string{
		"error[E_IR_DANGLING_REF]: ",
		"  --> support.hbc",
		"      at workflows[0].nodes[3].kids[1]",
		"      line 7, col 9",
		"   why: ",
		"  help: ",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("渲染骨架缺 %q：\n%s", want, got)
		}
	}

	// 占位必须被替换干净：不能残留任何 {xxx}
	if idx := strings.IndexByte(got, '{'); idx >= 0 {
		t.Errorf("渲染结果里残留未替换的占位符（在偏移 %d）：%s", idx, got)
	}

	// 语言切换只换文案，不换骨架：行数与结构必须一致。
	zh := New(CodeDanglingRef, SeverityError, loc, LangZH, args)
	if nl(got) != nl(zh.Render()) {
		t.Errorf("en 与 zh 的行数不同（%d vs %d）：骨架漂了，diff 会变得不可读",
			nl(got), nl(zh.Render()))
	}
}

func nl(s string) int { return strings.Count(s, "\n") }

// TestUnknownCodeIsLoud 错误码拼错时必须自曝，不能静默给个空消息。
func TestUnknownCodeIsLoud(t *testing.T) {
	d := New("E_IR_TYPO", SeverityError, Loc{Module: "x"}, LangEN, nil)
	if !strings.Contains(d.Message, "no message registered") {
		t.Errorf("未知错误码没有自曝，Message=%q", d.Message)
	}
	if d.Code != "E_IR_TYPO" {
		t.Errorf("未知错误码应原样保留，得到 %q", d.Code)
	}
	if d.Why == "" || len(d.Fix) == 0 {
		t.Error("未知错误码的兜底诊断也必须有 why 与 fix —— 它本身也是一条给人看的消息")
	}
}

// TestMissingArgKeepsPlaceholder 缺参时原样保留占位，不吞掉。
// 吞掉会让"漏了一个参数"看起来像"文案写得怪"，而原样保留一眼可见。
func TestMissingArgKeepsPlaceholder(t *testing.T) {
	d := New(CodeEntry, SeverityError, Loc{Module: "x"}, LangEN, map[string]string{"name": "f"})
	if !strings.Contains(d.Message, "{reason}") {
		t.Errorf("缺参数时应保留 {reason}，实际 Message=%q", d.Message)
	}
	if strings.Contains(d.Message, "{name}") {
		t.Errorf("给了的参数 {name} 应该被替换，实际 Message=%q", d.Message)
	}
}

// TestBracesAreEscaped 文案里想写字面花括号（代码示例）时必须写成 `{{` / `}}`。
//
// 这条守卫来自一次真实踩坑：E_IR_FLOAT 的修复建议里写了
// `{"t":"float","v":"1.5"}`，被占位符解析器当成一个叫 `"t":"float"...` 的
// 占位符，测试直接红。没有这条守卫，这个 bug 会以"渲染出字面花括号"的形式
// 上线 —— 用户看到一个没被替换的模板片段。
func TestBracesAreEscaped(t *testing.T) {
	for _, lang := range []Lang{LangEN, LangZH} {
		for _, c := range sortedKeys(catalog[lang]) {
			m := catalog[lang][c]
			all := append([]string{m.Text, m.Why}, m.Fix...)
			for _, s := range all {
				if bad := unescapedBraces(s); len(bad) > 0 {
					t.Errorf("%s/%s: 文里有未转义的花括号 %q（代码示例要写成 {{...}}）", lang, c, bad)
				}
			}
		}
	}
}

// TestUnescapedBracesIsNotVacuous 证明上面的守卫真的能红。
// 守卫自己也要被守卫 —— 一个恒真的检查等于没有检查。
func TestUnescapedBracesIsNotVacuous(t *testing.T) {
	// 该报的：裸的代码示例
	if got := unescapedBraces(`write {"t":"float"}`); len(got) == 0 {
		t.Error("未转义的代码示例应该被抓出来，实际没报")
	}
	// 不该报的：合法占位符
	if got := unescapedBraces("hello {name}"); len(got) != 0 {
		t.Errorf("合法占位符被误报：%v", got)
	}
	// 不该报的：转义对
	if got := unescapedBraces("write {{\"t\":1}}"); len(got) != 0 {
		t.Errorf("转义对不该被报：%v", got)
	}
}

// TestSubstUnescapes 渲染时转义对必须还原成单个花括号。
func TestSubstUnescapes(t *testing.T) {
	got := subst(`write {{"t":"float","v":"1.5"}} with {name}`, map[string]string{"name": "x"})
	want := `write {"t":"float","v":"1.5"} with x`
	if got != want {
		t.Errorf("转义还原不对：\n got=%q\nwant=%q", got, want)
	}
}

// TestCodesAreUpperCaseWithPrefix 错误码格式守卫。
//
// 本项目**刻意并存两种形态**，因为它们服务两个不同的东西：
//
//	E3002       LANGUAGE.md §11.2 的数字码。文档里有锚点、用户手册里按它写，
//	            所以不能改名。编译器诊断（词法/语法/类型/include 图）用它。
//	E_IR_DUP_ID IR 加载期的语义码。IR.md 没有给这些检查编号，而语义名比
//	            "E7001 是哪个"好记，grep 也更精确。加载器用它。
//
// 守卫只要求**形态合法且可区分**，不强求统一 —— 强求统一会逼着我们去改
// 已经写进 LANGUAGE.md 的数字码，那是在用一个内部整洁度去破坏对外文档的稳定性。
func TestCodesAreUpperCaseWithPrefix(t *testing.T) {
	isNumericCode := func(c string) bool {
		// E3002 / W3013：一个大写字母 + 4 位数字
		if len(c) != 5 || (c[0] != 'E' && c[0] != 'W') {
			return false
		}
		for i := 1; i < len(c); i++ {
			if c[i] < '0' || c[i] > '9' {
				return false
			}
		}
		return true
	}
	isSemanticCode := func(c string) bool {
		if !strings.HasPrefix(c, "E_") && !strings.HasPrefix(c, "W_") {
			return false
		}
		for _, r := range c {
			if !(r == '_' || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
				return false
			}
		}
		return true
	}
	for _, c := range codeSet() {
		if !isNumericCode(c) && !isSemanticCode(c) {
			t.Errorf("错误码 %q 既不是数字码（E3002）也不是语义码（E_IR_DUP_ID）："+
				"只能是这两种形态之一，否则按码 grep 会漏", c)
		}
	}
}
