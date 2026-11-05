package errs

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// 本文件是"表是唯一事实源"的可执行防线。
//
// 为什么需要它：`New(attr, sub, op)` 允许任何包在没有 HTTP 证据的情况下
// 造出一条错误。如果表里没有对应的 (attr, sub) 行，`New` 挂不上 rule，
// 三个策略方法就各自退回默认值 —— 而默认值是静默的：
//   - Class() 回落成 ClassTransport（把本地决策说成"网络故障"）；
//   - Retry() 回落成 RetryNone；
//   - CountsAsBreakerFailure() 回落成 false。
// 错误照常产生，trace 照常写入，只是语义错了，且没有任何报警。
// 所以"New() 造得出表里没有的错误"这件事必须被机器拦住。

// pair 是一个本地构造出来的 (归因, 子类) 组合。
type pair struct {
	attr Attribution
	sub  Subtype
}

func (p pair) String() string { return p.attr.String() + "/" + p.sub.String() }

// repoCoverageRoot 从 internal/errs 往上两级得到仓库根。
func repoCoverageRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	return abs
}

// constNameMaps 把 const 块里的**声明名字**映射回枚举值。
//
// 不手写一份 name → value 的映射表：那等于把 code.go 的知识复制一份，
// 两份一旦分叉，守卫就会用错的值去查表（然后"通过"）。
// 这里用声明顺序推出值，而 AttrNone / SubNone 恰好是 0 号，
// TestAttributionClosure 与 TestSubtypeClosedSet 已经钉死了"按声明顺序从 0 连续"
// 这条前提，所以索引即值。
func constNameMaps(t *testing.T) (map[string]Attribution, map[string]Subtype) {
	t.Helper()
	attrNames := declConstNames(t, "code.go", "Attribution")
	subNames := declConstNames(t, "code.go", "Subtype")

	attrByName := make(map[string]Attribution, len(attrNames))
	for i, n := range attrNames {
		attrByName[n] = Attribution(i)
	}
	subByName := make(map[string]Subtype, len(subNames))
	for i, n := range subNames {
		subByName[n] = Subtype(i)
	}

	// 自检：锚点对不上就说明"索引即值"的前提失效，此时任何结论都不成立。
	if v, ok := attrByName["AttrNone"]; !ok || v != AttrNone {
		t.Fatalf("AttrNone 的解析结果异常（存在=%v，值=%d）：索引即值的前提失效", ok, uint8(v))
	}
	if v, ok := attrByName["AttrCircuit"]; !ok || v != AttrCircuit {
		t.Fatalf("AttrCircuit 的解析结果异常（%d，期望 %d）", uint8(v), uint8(AttrCircuit))
	}
	if v, ok := subByName["SubNone"]; !ok || v != SubNone {
		t.Fatalf("SubNone 的解析结果异常（存在=%v，值=%d）", ok, uint8(v))
	}
	if v, ok := subByName["SubPoisoned"]; !ok || v != SubPoisoned {
		t.Fatalf("SubPoisoned 的解析结果异常（%d，期望 %d）", uint8(v), uint8(SubPoisoned))
	}
	return attrByName, subByName
}

// resolveEnumRef 把 `AttrPolicy` 或 `errs.AttrPolicy` 解析成常量名。
// 只看最后一段标识符：包限定与非限定两种写法都要支持（包内测试用非限定）。
func resolveEnumRef(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name, true
	case *ast.SelectorExpr:
		return v.Sel.Name, true
	}
	return "", false
}

// pendingLocalPairs 是**已由设计定下、但生产调用点还没落地**的本地组合。
//
// AST 扫描扫不到它们（目前没有代码调用 New 造这类错误），所以显式列在这里，
// 好让"表里少一行"现在就红，而不是等调用点落地那天才发现。
//
// 这是一份**临时**清单：调用点落地后请把条目删掉，届时 AST 扫描会自动覆盖。
// 不删的代价是它会慢慢变成第二份真相——正是这个包最反对的东西。
var pendingLocalPairs = []pair{
	// T24 ⑤：执行器 stdout 超过 stdout_cap → 本地截断。
	{attr: AttrPolicy, sub: SubResourceLimit},
	// D-023 末节：backend.auth 的密钥缺失 → policy/denied，且一个请求都不发。
	{attr: AttrPolicy, sub: SubDenied},
}

// TestLocalCoverageEveryConstructedPairIsMapped 是覆盖守卫本体。
//
// 扫全仓生产代码里的 `New(AttrX, SubY, …)` 调用，收集去重的 (attr, sub)，
// 逐个断言 `Lookup(attr, sub, 0)` 能命中。
func TestLocalCoverageEveryConstructedPairIsMapped(t *testing.T) {
	root := repoCoverageRoot(t)
	attrByName, subByName := constNameMaps(t)

	fset := token.NewFileSet()
	found := map[pair][]string{} // pair → 出现位置（file:line）
	var unresolved []string      // 静态解析不出来的实参
	var callSites int

	skipDirs := map[string]bool{
		"build": true, ".git": true, "target": true, "node_modules": true, "testdata": true,
	}

	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if info.IsDir() {
			if skipDirs[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// 只扫**生产代码**：测试文件会故意构造现实中不存在的组合，
		// 用来验证身份语义（例如 TestIsMatchesByAttrAndSub 里的
		// AttrTransport/SubCapacity —— 它证明 errors.Is 会比较 Attr，
		// 而 Hermes 永远不会真的产生"传输故障/容量"这种组合）。
		// 把它们算进覆盖面，只会逼着表里长出毫无意义的行。
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}

		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("解析 %s 失败，覆盖守卫无法判定: %v", path, perr)
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if !isNewCall(call) {
				return true
			}
			callSites++
			pos := fset.Position(call.Pos())
			where := rel + ":" + strconv.Itoa(pos.Line)

			if len(call.Args) < 2 {
				unresolved = append(unresolved, where+" → New(...) 实参少于 2 个")
				return true
			}
			attrName, okA := resolveEnumRef(call.Args[0])
			subName, okB := resolveEnumRef(call.Args[1])
			attr, okAV := attrByName[attrName]
			sub, okBV := subByName[subName]
			if !okA || !okB || !okAV || !okBV {
				unresolved = append(unresolved,
					where+" → New("+attrName+", "+subName+", …) 不是可解析的常量引用")
				return true
			}
			p := pair{attr: attr, sub: sub}
			found[p] = append(found[p], where)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	// 空集守卫：扫不到任何 New(...) 说明守卫的定位逻辑失效（改了包名/改了函数名/
	// 仓库挪了位置），此时"没有违规"是假的绿灯。
	if callSites == 0 {
		t.Fatal("全仓生产代码里一个 New(...) 调用都没扫到 —— 守卫的扫描条件失效了，不许在空集上通过")
	}

	if len(unresolved) > 0 {
		t.Fatalf("这些 New(...) 调用无法静态解析，覆盖守卫对它们失明：\n  %s\n"+
			"本包的契约是「本地构造的错误必须能在表里查到对应行」，"+
			"而这条契约只能靠静态分析来守。请把 attr/sub 写成常量字面量"+
			"（errs.AttrPolicy / errs.SubDenied 这种），不要用变量或函数返回值；"+
			"否则表里是否有一行覆盖它，就没有任何机器在看了。",
			strings.Join(unresolved, "\n  "))
	}

	var offenders []string
	// 自清断言：清单条目一旦被生产代码覆盖，就必须从清单里删掉。
	//
	// 不留这条的话，pendingLocalPairs 会慢慢变成第二份真相——本包最反对的东西：
	// 表里有一行、清单里也有一份"待办"，两边一旦分叉，没有人知道该信谁。
	// 这条断言让"落地"这件事自动逼人清理。
	for _, p := range pendingLocalPairs {
		if wheres, ok := found[p]; ok {
			t.Fatalf("%s 已落地（生产代码已构造该组合：%s），请从 pendingLocalPairs 删除该条目：\n"+
				"这份清单只该装「设计已定、生产调用点还没落地」的组合；"+
				"调用点落地后它已被 AST 扫描覆盖，再留着就变成第二份真相了。",
				p, strings.Join(wheres, ", "))
		}
	}

	// 并入"设计已定、调用点未落地"的组合。
	for _, p := range pendingLocalPairs {
		found[p] = []string{"（设计已定，生产调用点尚未落地）"}
	}

	for p, wheres := range found {
		if _, ok := Lookup(p.attr, p.sub, 0); !ok {
			offenders = append(offenders, p.String()+"  ←  "+strings.Join(wheres, ", "))
		}
	}
	if len(offenders) > 0 {
		// 排序让失败消息稳定：map 迭代顺序随机会让同一次失败产出两种文本，
		// 而这条消息会被贴进 issue 与 diff。
		sort.Strings(offenders)
		t.Fatalf("这些 (attr,sub) 组合被本地构造出来，但映射表里没有对应行：\n  %s\n"+
			"后果不是报错，是静默降级：Class() 会回落成 transport（把本地决策说成网络故障），\n"+
			"Retry() 回落成 none。表是这个包唯一的事实源，New() 造得出表里没有的错误，等于事实源有洞。\n"+
			"请在 table.go 补一行 Style: StyleLocal 的规则（挂在 HTTP 规则之后）。",
			strings.Join(offenders, "\n  "))
	}
}

// isNewCall 判断一个调用是不是 `errs.New(...)` 或包内 `New(...)`。
func isNewCall(call *ast.CallExpr) bool {
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		if fn.Sel.Name != "New" {
			return false
		}
		pkg, ok := fn.X.(*ast.Ident)
		return ok && pkg.Name == "errs"
	case *ast.Ident:
		return fn.Name == "New"
	}
	return false
}

// TestLocalCoverageResourceLimitRow 钉住 T24 ⑤ 那一行：
// "执行器 stdout 超 cap → policy/resource_limit"。
//
// 没有这一行时，`New(AttrPolicy, SubResourceLimit, …)` 的 Class() 会回落成
// ClassTransport —— 把"我们自己截断了一个过大的输出"说成"网络故障"，
// 而这正是这套映射表存在的全部理由所反对的那种混淆。
func TestLocalCoverageResourceLimitRow(t *testing.T) {
	// ① 表里有这一行，且它的 Style 必须是 StyleLocal。
	//
	// 这一条不是形式主义：该行没有任何 When* 条件，match() 只比较 style。
	// 若 Style 被改成 StyleAny，它会吞掉一切"没有更具体规则命中"的 HTTP 失败
	// （400 会从 contract/drift 变成 policy/resource_limit）。
	r, ok := Lookup(AttrPolicy, SubResourceLimit, 0)
	if !ok {
		t.Fatalf("表里没有 AttrPolicy/SubResourceLimit 这一行：\n" +
			"New(AttrPolicy, SubResourceLimit, …) 会因此挂不上 rule，" +
			"Class() 回落成 transport，把'响应体超 stdout_cap'报成网络故障")
	}
	if r.Style != StyleLocal {
		t.Fatalf("该行的 Style 必须是 %s，got %s：\n"+
			"它没有 When* 条件，Style 是唯一的匹配约束；改成 StyleAny 会吞掉所有兜底 HTTP 失败",
			StyleLocal, r.Style)
	}
	if r.Class != ClassPolicy || r.Retry != RetryNone || r.CountsAsFailure {
		t.Fatalf("该行应为 class=policy retry=none counts=false，got class=%s retry=%s counts=%v",
			r.Class, r.Retry, r.CountsAsFailure)
	}

	// ①b 这一行必须**唯一**。
	//
	// Lookup 返回第一条 (attr, sub) 匹配的行；若将来有人在它前面插入一行
	// 同 (policy, resource_limit) 的规则，这一行就永远查不到（死行）。
	// 位置在 HTTP 规则之后无所谓（StyleLocal 到不了 Classify），但重复一定有害。
	dup := 0
	for _, row := range Rules() {
		if row.Attr == AttrPolicy && row.Sub == SubResourceLimit {
			dup++
		}
	}
	if dup != 1 {
		t.Fatalf("(policy, resource_limit) 应恰好有一行，got %d —— "+
			"Lookup 只取第一条匹配，多出来的行永远查不到，是死行", dup)
	}

	// ② 经由 New 构造时，三个策略方法都必须来自这一行。
	e := New(AttrPolicy, SubResourceLimit, "native.exec")
	if got := e.Class(); got != ClassPolicy {
		t.Fatalf("Class() 必须是 %s，got %s —— 回落成 transport 就是事实源有洞", ClassPolicy, got)
	}
	if got := e.Retry(); got != RetryNone {
		t.Fatalf("Retry() 必须是 %s，got %s —— 截断是确定性的，重试只会再超一次", RetryNone, got)
	}
	if e.CountsAsBreakerFailure() {
		t.Fatal("资源上限是本地判定，不是上游故障，不许计熔断（会把健康节点踢掉）")
	}
	if e.Key() != "policy/resource_limit" {
		t.Fatalf("Key() 应为 policy/resource_limit，got %q", e.Key())
	}

	// ③ 反向守卫：这一行不许影响真实 HTTP 信号。
	// 400 仍必须是 contract/drift，不能被本地行抢走。
	httpErr, err := Classify(Signal{Style: StyleOpenAI, Status: 400})
	if err != nil {
		t.Fatalf("400 应已映射，got %v", err)
	}
	if httpErr.Class() != ClassContract {
		t.Fatalf("400 的 class 必须是 %s，got %s —— "+
			"证明 StyleLocal 那行没有在 Classify 路径上抢走真实 HTTP 失败",
			ClassContract, httpErr.Class())
	}
}

// TestLocalCoverageNewAttachesRule 保证 `New()` 真的把表行挂上。
//
// 为什么需要单独一条：`Class()` / `Retry()` / `CountsAsBreakerFailure()` 各自都还有
// 一次 `Lookup` 兜底，所以**即使 New() 不挂 rule，行为也看不出来**——
// 三个方法各自能查到同一行。这让 "New 挂 rule" 这件修复变成不可观测的：
// 删掉它，所有行为测试照样绿（我实测过）。
//
// 但它是"单一事实源"的形状保证而不是重复劳动：
//   - 挂上 rule 后，将来新增任何策略方法只要读 e.rule 就自动正确，不必再各写一次 Lookup；
//   - `e.rule != nil` 也让"这条错误来自表"成为可断言的事实，而不是隐含推断。
//
// 判定方式不硬编码组合：遍历 `Rules()` 自己，对每一行的 (attr, sub) 断言
// `New(...).rule != nil`。这样不会因为表里加行而失效，也不会变成第二份真相。
func TestLocalCoverageNewAttachesRule(t *testing.T) {
	rows := Rules()
	if len(rows) < 10 {
		t.Fatalf("Rules() 只有 %d 行，映射表可能被削；本断言会近乎空转", len(rows))
	}
	for _, r := range rows {
		e := New(r.Attr, r.Sub, "probe")
		if e.rule == nil {
			t.Fatalf("New(%s, %s, …) 没有挂上 rule：\n"+
				"表里明明有这一行（Why: %s）—— 说明 New() 里的 Lookup 失效了。\n"+
				"后果：本地构造的错误丢掉表策略，Retry/Class/CountsAsBreakerFailure 各自退回默认值，"+
				"同一件事出现两条来源，而分叉是静默的。", r.Attr, r.Sub, r.Why)
		}
		if e.rule.Why == "" {
			t.Fatalf("New(%s, %s, …) 挂上的 rule 没有 Why：表行的理由必须写下来，否则后人无从判断该不该改",
				r.Attr, r.Sub)
		}
	}
}

// TestLocalCoverageModelViolationDoesNotTripBreaker 钉住一次**有意的行为变更**。
//
// 变更前：CountsAsBreakerFailure() 对本地构造的错误走 `switch e.Attr`，
// AttrModelContractViolation 不在豁免名单里，于是落到 default `true`——
// 也就是说"模型违约"会被算成上游故障。而表里明确写着模型违约不计熔断
// （KindModelViolation 行的 Why："换一次采样可能就好了，且不是上游的错"）。
// 两个答案并存，且没人会发现。
//
// 变更后：一律以表为准 → 模型违约不计熔断。这是**行为变更**，所以单独钉一条，
// 免得将来有人"顺手"把 Attr 兜底改回来。
func TestLocalCoverageModelViolationDoesNotTripBreaker(t *testing.T) {
	e := New(AttrModelContractViolation, SubNone, "provider.Call")
	if got := e.Class(); got != ClassModel {
		t.Fatalf("Class() 必须是 %s，got %s", ClassModel, got)
	}
	if got := e.Retry(); got != RetryOnce {
		t.Fatalf("Retry() 必须是 %s（换一次采样可能就好了），got %s", RetryOnce, got)
	}
	if e.CountsAsBreakerFailure() {
		t.Fatal("模型违约不得计熔断：\n" +
			"表里 KindModelViolation 行明确写着 CountsAsFailure:false（'不是上游的错'）；\n" +
			"按 Attr 兜底会把它算成上游故障，从而在模型自己胡说时隔离一颗健康的 (provider,model)。\n" +
			"若这条红了，说明 CountsAsBreakerFailure() 又退回了不看表的老实现。")
	}
	// 对照：真正的传输故障仍必须计熔断，否则"表是事实源"会被理解成"什么都豁免"。
	if !New(AttrTransport, SubNone, "provider.Call").CountsAsBreakerFailure() {
		t.Fatal("传输故障必须计熔断——豁免模型违约不等于豁免上游故障")
	}
}
