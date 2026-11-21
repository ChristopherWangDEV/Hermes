// Package guard 是项目的"不许退化"测试集。
//
// 本文件守的是 docs/ARCH.md §5.7 的"映射表关键行"摘要与 internal/errs/table.go
// 的真实规则之间的一致性。
//
// 为什么需要它：`ARCH.md:1338` 自己写着"摘要与代码的任何不一致都是缺陷，因为读者会
// 拿摘要当契约"。而 v3.4 首次落地这张表时，同一天就漂了 5 处：`shutting_down` 的
// 重试列被压成"立即换节点"、四行 `Style` 抄成 `native`/`local`、两行漏抄。
// **手抄的表一定会漂**，所以让它由机器逐行比。
//
// # 只比四列 + 一条弱校验，这个取舍是刻意的
//
// `状态` 与 `code / type` 两列是自然语言（"5xx（非上述）"、"*（无 code）*"、
// "响应体 schema 校验失败"），硬啃只能写出一堆脆弱的正则，最后因为误报被删掉 ——
// 一条会误报的守卫等于没有守卫。所以：
//
//   - **逐行必须相等**的四列：`Style`、`Class`、`Attr/Sub`、`重试`、`计熔断`
//     （这五列都是枚举或布尔，机器可比，且都直接对应 Rule 字段）。
//   - `状态` 只做**弱校验**：单元格里出现的数字必须能在该行的 `WhenStatus` 里找到
//     （`4xx`/`5xx`/`任意`/`—` 这类不含具体状态码的写法跳过）。
//     这能抓住"表写 503、代码写 502"这种最伤人的抄错，又不逼人把自然语言拆成枚举。
//   - `code / type` **不比**：它是上游的字符串字面量，改一个字的差别由
//     internal/errs 与 internal/provider 的行为测试负责，不是摘要的职责。
//
// 另外三条结构断言（行数、连续、表头）比任何单元格都重要：它们守的是
// "这张表还能不能被当作表读"。
package guard_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"hermes/internal/errs"
)

// wantHeader 是 §5.7 映射表的表头。列被改名时守卫必须红，
// 而不是"找不到列就默默比错列"。
var wantHeader = []string{"Style", "状态", "code / type", "Class", "Attr/Sub", "重试", "计熔断"}

// reRangeish 匹配 `4xx` / `5xx` 这类**区间**写法：它不是具体状态码，
// 不能拿去做成员检查，否则 `4xx` 里的 4 会被当成"状态码 4 不在 WhenStatus 里"。
var reRangeish = regexp.MustCompile(`\d+[xX]{2}`)

// reNumber 提取状态单元格里剩下的数字串。
var reNumber = regexp.MustCompile(`\d+`)

// mdTable 是解析出来的 markdown 表格。
type mdTable struct {
	header  []string
	rows    [][]string
	lineNos []int
}

// stripMarkup 去掉单元格里的 markdown 强调与行内代码标记。
//
// 表里存在 `**poisoned**`、“ `HM_E_SANDBOX_DENIED` “ 这类写法，
// 它们是**排版**而不是语义：把星号当值去比，会因为一处加粗就红。
// 只去 `*` 与反引号，不动其它字符。
func stripMarkup(cell string) string {
	s := strings.TrimSpace(cell)
	s = strings.ReplaceAll(s, "*", "")
	s = strings.ReplaceAll(s, "`", "")
	return strings.TrimSpace(s)
}

// splitRow 把一行 markdown 表格拆成单元格（已去排版标记）。
func splitRow(line string) []string {
	s := strings.TrimSpace(line)
	s = strings.TrimPrefix(s, "|")
	s = strings.TrimSuffix(s, "|")
	parts := strings.Split(s, "|")
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = stripMarkup(p)
	}
	return out
}

// isSeparatorRow 判断是否是 `|---|---|` 那行。
func isSeparatorRow(cells []string) bool {
	if len(cells) == 0 {
		return false
	}
	for _, c := range cells {
		if strings.Trim(c, "-: ") != "" {
			return false
		}
		if !strings.Contains(c, "-") {
			return false
		}
	}
	return true
}

// readMappingTable 定位并解析 §5.7 的映射表。
//
// 定位方式是"按表头精确匹配"而不是"§5.7 之后第一张表"：前者在章节里
// 新增别的表格时不会比错对象，后者会。
func readMappingTable(t *testing.T) mdTable {
	t.Helper()
	path := filepath.Join(repoRoot(t), "docs", "ARCH.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skip("docs/ARCH.md 不在仓库里（内部文档）")
	}
	lines := strings.Split(string(raw), "\n")

	// 表头：第一个与 wantHeader 完全一致的表格行。
	start := -1
	for i, ln := range lines {
		if !strings.HasPrefix(strings.TrimSpace(ln), "|") {
			continue
		}
		if equalCells(splitRow(ln), wantHeader) {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("docs/ARCH.md 里找不到表头为 %q 的表格：\n"+
			"要么 §5.7 的映射表被删/被重写，要么表头列被改名。"+
			"守卫不能靠“猜列”工作——找不到就响亮地失败，别默默比错列。", wantHeader)
	}

	// 分隔行必须紧跟表头。
	if start+1 >= len(lines) || !isSeparatorRow(splitRow(lines[start+1])) {
		t.Fatalf("表头下一行不是 markdown 分隔行（`|---|`），表格结构已坏：\n  %q",
			strings.TrimSpace(lines[start+1]))
	}

	tbl := mdTable{header: wantHeader}
	for i := start + 2; i < len(lines); i++ {
		ln := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(ln, "|") {
			break
		}
		cells := splitRow(lines[i])
		if len(cells) != len(wantHeader) {
			t.Fatalf("第 %d 行的单元格数是 %d，表头是 %d 列：\n  %s\n"+
				"列数不齐会让后面的行整体错位，渲染出来也是残表",
				i+1, len(cells), len(wantHeader), ln)
		}
		tbl.rows = append(tbl.rows, cells)
		tbl.lineNos = append(tbl.lineNos, i+1)
	}

	// 防空转：解析出 0 行时，"没有不一致"是假的绿灯。
	if len(tbl.rows) == 0 {
		t.Fatal("§5.7 映射表解析出 0 行数据 —— 表格被删、格式变了或解析失效；" +
			"此时“逐行一致”是空集上的真理，守卫必须在这里失败")
	}

	// 连续性：表格数据行之后、下一个 `###` 标题之前，不许再出现表格行。
	//
	// 这是渲染层缺陷，机器能抓：v3.4 真实发生过——一段 `>` 引用块插在表格中间，
	// markdown 会在此终止表格，于是引用块下面那三行在渲染时**掉出表外**，
	// 谁读文档都看不到它们，而源文件看起来"表里明明有"。
	lastLine := tbl.lineNos[len(tbl.lineNos)-1]
	var orphan []int
	for i := lastLine; i < len(lines); i++ {
		ln := strings.TrimSpace(lines[i])
		if strings.HasPrefix(ln, "### ") {
			break
		}
		if strings.HasPrefix(ln, "|") {
			orphan = append(orphan, i+1)
		}
	}
	if len(orphan) > 0 {
		t.Fatalf("表格被插入了非表格行，渲染时后面几行会掉出表外（v3.4 真实发生过）：\n"+
			"  表数据到第 %d 行结束，但第 %v 行又出现了表格行。\n"+
			"  把引用块/空行/说明文字移到整张表**之后**，或者把这些行接回表内。",
			lastLine, orphan)
	}

	return tbl
}

// normIdent 把标识符归一到可比形态：去下划线 + 转小写。
//
// 只用于**同一枚举值的两种排版**（表里写 Go 常量名 `RespectRetryAfter`，
// 代码 `String()` 给 `respect_retry_after`）。不用于 Class/Style 之类
// 已经同形的列——那里做归一化等于放宽契约。
func normIdent(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "_", ""))
}

// equalCells 逐格比较（大小写敏感：表头是人工约定的字面量）。
func equalCells(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// styleFromCell 把 `Style` 列的文字翻成 errs.Style。
//
// 出现不认识的写法一律失败，**不静默跳过**：静默跳过就是给漂移留门——
// v3.4 正是把 `全部` 抄成 `native`/`local` 才让摘要与代码分叉。
func styleFromCell(t *testing.T, cell, where string) errs.Style {
	t.Helper()
	switch {
	case cell == "全部":
		return errs.StyleAny
	case strings.EqualFold(cell, "local"):
		return errs.StyleLocal
	case strings.EqualFold(cell, "openai"):
		return errs.StyleOpenAI
	case strings.EqualFold(cell, "anthropic"):
		return errs.StyleAnthropic
	case strings.EqualFold(cell, "ollama"):
		return errs.StyleOllama
	}
	t.Fatalf("%s 的 Style 列写的是 %q，无法映射到 errs.Style。\n"+
		"允许的写法只有：`全部`(=StyleAny)、`local`(=StyleLocal)、`openai`、`anthropic`、`ollama`。\n"+
		"特别注意 `native` 不是合法值：那七条 WhenKind 规则在代码里是 StyleAny，"+
		"写成 `native` 会让读者以为跨风格信号会落 ErrUnmapped。", where, cell)
	return errs.StyleAny // 不可达：t.Fatalf 已终止
}

// countsFromCell 把 `计熔断` 列的 ❌/✅ 翻成布尔。
func countsFromCell(t *testing.T, cell, where string) bool {
	t.Helper()
	switch cell {
	case "❌":
		return false
	case "✅":
		return true
	}
	t.Fatalf("%s 的计熔断列写的是 %q，只允许 ❌ 或 ✅。\n"+
		"别的字符（例如空着、写“否”、写“-”）会让这一列失去机读性，"+
		"下一个抄表的人只能靠猜。", where, cell)
	return false
}

// attrSubFromCell 把 `Attr/Sub` 列拆成 (attr, sub) 的字符串形式。
func attrSubFromCell(t *testing.T, cell, where string) (string, string) {
	t.Helper()
	parts := strings.Split(cell, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		t.Fatalf("%s 的 Attr/Sub 列写的是 %q，期望 `attr/sub` 形状（两段都非空）。\n"+
			"只写一半（例如 `budget`）等于丢掉 Sub 的语义：SubNone 与 SubRejected "+
			"在同一个 Attr 下是完全不同的失败。", where, cell)
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
}

// checkStatusCell 是 `状态` 列的弱校验：单元格里出现的**具体数字**必须能在
// 该行的 WhenStatus 里找到。
//
// 跳过 `4xx`/`5xx` 这类区间写法（它们不是状态码）、`—`/`任意` 这类纯文字。
// 这条能抓住"表写 503、代码写 502"，又不强迫把自然语言列改成枚举。
func checkStatusCell(t *testing.T, cell string, r errs.Rule, where string) {
	t.Helper()
	s := reRangeish.ReplaceAllString(cell, " ")
	nums := reNumber.FindAllString(s, -1)
	for _, n := range nums {
		v, err := strconv.Atoi(n)
		if err != nil {
			continue
		}
		hit := false
		for _, st := range r.WhenStatus {
			if st == v {
				hit = true
				break
			}
		}
		if !hit {
			t.Fatalf("%s 的状态列写着 %q，但代码这一行的 WhenStatus 是 %v —— %d 不在里面。\n"+
				"摘要把状态码抄错是最伤人的一类：读者会按表去构造 Signal，然后永远命中不了这一行。",
				where, cell, r.WhenStatus, v)
		}
	}
}

// ruleKey 生成与 *Error.Key() 同形的机读标识（Rule 本身没有 Key 方法）。
func ruleKey(r errs.Rule) string {
	return r.Attr.String() + "/" + r.Sub.String()
}

// TestMappingTableMatchesRules 逐行比对 §5.7 摘要与 table.go。
func TestMappingTableMatchesRules(t *testing.T) {
	tbl := readMappingTable(t)
	rules := errs.Rules()

	// 先把表行按 (Style, Attr/Sub) 对齐到规则上。用两组键而不是单一键，
	// 是因为 (policy/capacity)、(policy/quota) 这些组合在表里出现多次，
	// 单靠 Attr/Sub 无法区分是哪一行。
	rowRule := make([]int, len(tbl.rows)) // 表行 → 规则下标
	for i := range rowRule {
		rowRule[i] = -1
	}
	usedRule := make([]bool, len(rules))

	for i, cells := range tbl.rows {
		where := "ARCH.md:" + strconv.Itoa(tbl.lineNos[i])
		st := styleFromCell(t, cells[0], where)
		attr, sub := attrSubFromCell(t, cells[4], where)
		for j := range rules {
			if usedRule[j] {
				continue
			}
			if rules[j].Style == st &&
				rules[j].Attr.String() == attr &&
				rules[j].Sub.String() == sub {
				rowRule[i] = j
				usedRule[j] = true
				break
			}
		}
	}

	var missing, extra []string
	for j, r := range rules {
		if !usedRule[j] {
			missing = append(missing, ruleKey(r)+"（Class="+r.Class.String()+", Style="+r.Style.String()+"）")
		}
	}
	for i, j := range rowRule {
		if j < 0 {
			extra = append(extra,
				"ARCH.md:"+strconv.Itoa(tbl.lineNos[i])+" 风格="+tbl.rows[i][0]+" "+tbl.rows[i][4])
		}
	}

	// ① 行数：两个方向都要报。摘要允许"少"是因为它自称"关键行"——
	// 但 v3.4 之后它已经承诺逐条列出（见 ARCH.md:1338），所以少一行就是缺陷。
	if len(tbl.rows) != len(rules) {
		t.Fatalf("§5.7 摘要有 %d 行，table.go 有 %d 条规则：\n"+
			"  摘要缺（代码里有、表里没有）：%v\n"+
			"  摘要多（表里有、代码里没有）：%v\n"+
			"若某条同时出现在两侧，说明它的 Style 或 Attr/Sub 抄错了，不是真的缺失。\n"+
			"ARCH.md:1338 承诺“逐条列出”，所以行数必须相等。",
			len(tbl.rows), len(rules), missing, extra)
	}
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("摘要与规则无法一一对齐：\n  代码有而表里没有：%v\n  表里有而代码里没有：%v",
			missing, extra)
	}

	// ② 顺序：表必须按 rules 顺序（= Classify 的优先级）排。
	// 顺序错了，读者会以为优先级不同——而优先级正是这张表的一半信息。
	for i, j := range rowRule {
		if j != i {
			t.Fatalf("第 %d 行（ARCH.md:%d）对应的是 rules[%d]，不是 rules[%d]。\n"+
				"表必须按 rules 顺序排列——那是 Classify 的优先级，不是任意顺序。\n"+
				"第 %d 行应是 %s。",
				i+1, tbl.lineNos[i], j, i, i+1, ruleKey(rules[i]))
		}
	}

	// ③ 逐行比四列 + 状态弱校验。
	for i, cells := range tbl.rows {
		r := rules[i]
		where := "ARCH.md:" + strconv.Itoa(tbl.lineNos[i])

		if got := styleFromCell(t, cells[0], where); got != r.Style {
			t.Errorf("%s（%s）Style 列写 %q → %s，代码是 %s。\n"+
				"常见抄错：把 `全部`(StyleAny) 写成 `native`/`local`。",
				where, ruleKey(r), cells[0], got, r.Style)
		}
		if got := cells[3]; !strings.EqualFold(got, r.Class.String()) {
			t.Errorf("%s（%s）Class 列写 %q，代码是 %q", where, ruleKey(r), got, r.Class.String())
		}
		attr, sub := attrSubFromCell(t, cells[4], where)
		if attr != r.Attr.String() || sub != r.Sub.String() {
			t.Errorf("%s（%s）Attr/Sub 列写 %q，代码是 %q",
				where, ruleKey(r), cells[4], ruleKey(r))
		}
		// RetryPolicy.String() 是 snake_case（none / once / idempotent_only /
		// respect_retry_after），表里写的是 Go 常量名（None / Once / IdempotentOnly /
		// RespectRetryAfter）。两者是同一个值的两种排版，不是语义差异，
		// 所以去掉下划线 + 大小写不敏感后再比。
		if got, want := normIdent(cells[5]), normIdent(r.Retry.String()); got != want {
			t.Errorf("%s（%s）重试列写 %q，代码是 %q。\n"+
				"注意这一列必须是 RetryPolicy 的枚举值；把 prose 写进来（例如“立即换节点”）"+
				"会让读者去找一个不存在的策略。", where, ruleKey(r), cells[5], r.Retry.String())
		}
		if got, want := countsFromCell(t, cells[6], where), r.CountsAsFailure; got != want {
			t.Errorf("%s（%s）计熔断列写 %q → %v，代码是 %v", where, ruleKey(r), cells[6], got, want)
		}
		checkStatusCell(t, cells[1], r, where)
	}
}
