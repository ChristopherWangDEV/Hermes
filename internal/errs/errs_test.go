package errs

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// ① errors.Is 必须按 (Attr, Sub) 命中，而不是按指针同一个。
func TestIsMatchesByAttrAndSub(t *testing.T) {
	a := New(AttrPolicy, SubCapacity, "provider.Call")
	b := New(AttrPolicy, SubCapacity, "engine.step") // 不同 op、不同 step，同一分类
	if !errors.Is(a, b) {
		t.Fatalf("同一个 (Attr,Sub) 必须互相 Is；got a=%v b=%v", a, b)
	}
	c := New(AttrPolicy, SubQuota, "provider.Call")
	if errors.Is(a, c) {
		t.Fatal("Sub 不同不许 Is 命中")
	}
	d := New(AttrTransport, SubCapacity, "provider.Call")
	if errors.Is(a, d) {
		t.Fatal("Attr 不同不许 Is 命中")
	}
	// 包装后仍要能命中（errors.Is 会 unwrap）
	wrapped := errors.Join(errors.New("outer"), a)
	if !errors.Is(wrapped, b) {
		t.Fatal("包装后的错误仍必须按分类命中")
	}
}

// ② 归因是 6 值闭集：长度 6，每个值 String() 可读且互不相同。
func TestAttributionClosure(t *testing.T) {
	if len(AttrAll) != 6 {
		t.Fatalf("AttrAll 长度必须是 6（闭集），got %d", len(AttrAll))
	}
	seen := map[string]bool{}
	for _, a := range AttrAll {
		if a == AttrNone {
			t.Fatal("AttrNone 是零值哨兵，不属于闭集")
		}
		s := a.String()
		if s == "" || strings.HasPrefix(s, "unknown_") {
			t.Fatalf("归因 %d 的 String() 未实现：%q", uint8(a), s)
		}
		if seen[s] {
			t.Fatalf("归因字符串重复：%q", s)
		}
		seen[s] = true
	}
	// 零值必须是"未分类"的可读形态，不能伪装成某个真实归因。
	if !strings.HasPrefix(AttrNone.String(), "unknown_") {
		t.Fatalf("AttrNone.String() 应为 unknown_*，got %q", AttrNone.String())
	}

	// ④ 声明数必须等于 AttrAll 的长度 —— 这是"加了归因却忘了更新闭集"的唯一检测。
	//
	// 归因是**整个错误模型的第一性契约**（6 个真实值，没有 "other" 桶），
	// 所以它比 Subtype 更该守住：漏更新时 AttrAll 会悄悄少一项，
	// 而穷尽性断言照样绿，于是"新增归因"与"忘记新增"在测试层面无法区分。
	//
	// 注意 AttrNone 必须**排除**：它是零值哨兵，不属于闭集（见 AttrAll 的注释），
	// 而它在声明处确实存在，所以声明数会比成员数多 1。
	names := declConstNames(t, "code.go", "Attribution")
	sawNone := false
	realNames := make([]string, 0, len(names))
	for _, n := range names {
		if n == "AttrNone" {
			sawNone = true
			continue
		}
		realNames = append(realNames, n)
	}
	if !sawNone {
		t.Fatalf("code.go 的 Attribution const 块里找不到 AttrNone（声明为 %v）：\n"+
			"零值哨兵是这套模型的一部分（'忘记赋值'要在测试里立即可见），"+
			"它消失说明闭集与哨兵的约定被改了，守卫的计数前提不再成立", names)
	}
	if len(realNames) != len(AttrAll) {
		t.Fatalf("code.go 声明了 %d 个真实归因 %v（AttrNone 是哨兵，不计入），"+
			"但 AttrAll 只有 %d 项：\n"+
			"加了归因却忘了更新 AttrAll，此时穷尽性断言（含 AttrAll 不含哨兵这条）"+
			"对其他包完全不设防。请把新值加进 AttrAll，并确认它的 String() 可读。",
			len(realNames), realNames, len(AttrAll))
	}

	// ⑤ 值必须从 1 开始逐一对应 AttrAll 的下标：
	// AttrNone 占 0，真实归因从 1 起，AttrAll[0] == Attribution(1)、AttrAll[1] == 2 …
	// 这条防的是"中间漏列"——漏一项时 AttrAll 长度已经不对（④ 会红），
	// 但如果有人既漏一项又塞进一个错值，只有这里能看出来。
	for i, a := range AttrAll {
		if a != Attribution(i+1) {
			t.Fatalf("AttrAll[%d] 的值是 %d，期望 %d：\n"+
				"AttrNone 占 0，真实归因必须从 1 起连续排列；"+
				"不连续意味着 AttrAll 里混进了错误的成员或顺序被打乱", i, uint8(a), i+1)
		}
	}
}

// ③ §5.7 的 15 行逐行断言。表变一行，这里就多一条。
func TestMappingTableRowByRow(t *testing.T) {
	cases := []struct {
		name   string
		sig    Signal
		class  Class
		attr   Attribution
		sub    Subtype
		retry  RetryPolicy
		counts bool
	}{
		{"openai 429 quota",
			Signal{Style: StyleOpenAI, Status: 429, Code: "rate_limit_exceeded"},
			ClassQuota, AttrPolicy, SubQuota, RetryRespectRetryAfter, false},
		{"openai 503 capacity",
			Signal{Style: StyleOpenAI, Status: 503, Code: "server_busy"},
			ClassCapacity, AttrPolicy, SubCapacity, RetryRespectRetryAfter, false},
		{"openai 503 draining",
			Signal{Style: StyleOpenAI, Status: 503, Code: "shutting_down"},
			ClassDraining, AttrPolicy, SubCapacity, RetryIdempotentOnly, false},
		{"openai 503 model_poisoned",
			Signal{Style: StyleOpenAI, Status: 503, Code: "model_poisoned"},
			ClassModel, AttrModelContractViolation, SubPoisoned, RetryOnce, true},
		{"anthropic 429 quota",
			Signal{Style: StyleAnthropic, Status: 429, Type: "rate_limit_error"},
			ClassQuota, AttrPolicy, SubQuota, RetryRespectRetryAfter, false},
		{"anthropic 503 capacity",
			Signal{Style: StyleAnthropic, Status: 503, Type: "overloaded_error"},
			ClassCapacity, AttrPolicy, SubCapacity, RetryRespectRetryAfter, false},
		{"anthropic 529 capacity",
			Signal{Style: StyleAnthropic, Status: 529, Type: "overloaded_error"},
			ClassCapacity, AttrPolicy, SubCapacity, RetryRespectRetryAfter, false},
		{"ollama 429 无 code",
			Signal{Style: StyleOllama, Status: 429},
			ClassQuota, AttrPolicy, SubQuota, RetryRespectRetryAfter, false},
		{"ollama 503 无 code",
			Signal{Style: StyleOllama, Status: 503},
			ClassCapacity, AttrPolicy, SubCapacity, RetryRespectRetryAfter, false},
		{"4xx 请求被拒",
			Signal{Style: StyleOpenAI, Status: 404},
			ClassContract, AttrUpstreamContractDrift, SubRejected, RetryNone, true},
		{"401 认证失败（第 16 行）",
			Signal{Style: StyleOpenAI, Status: 401},
			ClassPolicy, AttrPolicy, SubDenied, RetryNone, false},
		{"5xx 兜底",
			Signal{Style: StyleAnthropic, Status: 500},
			ClassServer, AttrTransport, SubNone, RetryIdempotentOnly, true},
		{"schema 漂移",
			Signal{Kind: KindSchemaDrift},
			ClassDrift, AttrUpstreamContractDrift, SubNone, RetryNone, true},
		{"模型违约",
			Signal{Kind: KindModelViolation},
			ClassModel, AttrModelContractViolation, SubNone, RetryOnce, false},
		{"连接失败",
			Signal{Kind: KindConnectFailure},
			ClassTransport, AttrTransport, SubRefused, RetryIdempotentOnly, true},
		{"执行器 deadline",
			Signal{Kind: KindNativeDeadline},
			ClassTransport, AttrTransport, SubTimeout, RetryIdempotentOnly, true},
		{"沙箱拒绝",
			Signal{Kind: KindSandboxDenied},
			ClassPolicy, AttrPolicy, SubDenied, RetryNone, false},
		{"预算拒绝",
			Signal{Kind: KindBudgetRefused},
			ClassBudget, AttrBudget, SubNone, RetryNone, false},
		{"熔断开",
			Signal{Kind: KindCircuitOpen},
			ClassCircuit, AttrCircuit, SubNone, RetryNone, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, err := Classify(c.sig)
			if err != nil {
				t.Fatalf("不该 unmapped：%v", err)
			}
			if e == nil {
				t.Fatal("非 2xx 必须产出 *Error")
			}
			if got := e.Class(); got != c.class {
				t.Fatalf("class: want %s, got %s", c.class, got)
			}
			if e.Attr != c.attr || e.Sub != c.sub {
				t.Fatalf("attr/sub: want %s/%s, got %s/%s",
					c.attr, c.sub, e.Attr, e.Sub)
			}
			if got := e.Retry(); got != c.retry {
				t.Fatalf("retry: want %s, got %s", c.retry, got)
			}
			if got := e.CountsAsBreakerFailure(); got != c.counts {
				t.Fatalf("counts_as_failure: want %v, got %v", c.counts, got)
			}
		})
	}
}

// ④b 七条 Kind 规则必须与协议无关（Style: StyleAny）。
//
// 这条测试存在的唯一理由：Style 的零值是 StyleOpenAI（iota 0），而 match() 的
// 第一条就是 style 检查。省略 Style 不是"无关"而是"只对 OpenAI 面生效"——
// 于是 `Signal{Style: StyleNative, Kind: KindNativeDeadline}` 会一路落空到
// ErrUnmapped，把"执行器被杀"报成"上游出现了表里没见过的组合"，
// 让人去查一个根本不存在的上游。
//
// 笛卡尔积形状是故意的：漏写任何一行的 Style，这里就有一格红。
// StyleLocal 那一列断言的是"拒绝"而不是"命中"，理由见 TestClassifyRejectsLocalStyle：
// 本地决策没有 HTTP 形状，它不该走到分类路径上来——但这仍然是这一格必须守住的契约，
// 所以留在矩阵里，而不是悄悄跳过。
func TestKindRulesAreStyleAgnostic(t *testing.T) {
	kinds := []struct {
		kind  Kind
		class Class
		attr  Attribution
		sub   Subtype
	}{
		{KindSchemaDrift, ClassDrift, AttrUpstreamContractDrift, SubNone},
		{KindModelViolation, ClassModel, AttrModelContractViolation, SubNone},
		{KindConnectFailure, ClassTransport, AttrTransport, SubRefused},
		{KindNativeDeadline, ClassTransport, AttrTransport, SubTimeout},
		{KindSandboxDenied, ClassPolicy, AttrPolicy, SubDenied},
		{KindBudgetRefused, ClassBudget, AttrBudget, SubNone},
		{KindCircuitOpen, ClassCircuit, AttrCircuit, SubNone},
	}

	// 守卫自检：表里必须正好有 7 条 Kind 规则，否则下面的矩阵会漏格子。
	nKind := 0
	for _, r := range Rules() {
		if r.WhenKind != KindNone {
			nKind++
		}
	}
	if nKind != len(kinds) {
		t.Fatalf("表里有 %d 条 WhenKind 规则，但本测试列了 %d 种 Kind —— "+
			"新增 Kind 而没加进这里，守卫会漏掉它", nKind, len(kinds))
	}

	styles := []Style{StyleOpenAI, StyleAnthropic, StyleOllama, StyleNative, StyleLocal}
	for _, st := range styles {
		for _, c := range kinds {
			t.Run(st.String()+"/"+c.kind.String(), func(t *testing.T) {
				e, err := Classify(Signal{Style: st, Kind: c.kind})

				if st == StyleLocal {
					// 本地决策只能由 New() 构造（见 TestClassifyRejectsLocalStyle）。
					if err == nil {
						t.Fatalf("StyleLocal 的 %s 不该被分类成功，got %s/%s —— "+
							"本地决策没有 HTTP 形状，被分类成功说明它撞上了一张伪造的表行",
							c.kind, e.Attr, e.Sub)
					}
					if !errors.Is(err, ErrUnmapped) {
						t.Fatalf("StyleLocal 必须返回 ErrUnmapped，got %v", err)
					}
					return
				}

				if err != nil {
					t.Fatalf("%s 面上的 %s 落到了 ErrUnmapped：%v\n"+
						"这就是漏写 Style: StyleAny 的症状——"+
						"Style 零值是 StyleOpenAI，不写等于只对 OpenAI 面生效", st, c.kind, err)
				}
				if e == nil {
					t.Fatalf("%s 的 %s 必须产出 *Error", st, c.kind)
				}
				if e.Class() != c.class || e.Attr != c.attr || e.Sub != c.sub {
					t.Fatalf("%s 的 %s 应归成 %s/%s/%s，got %s/%s/%s",
						st, c.kind, c.class, c.attr, c.sub, e.Class(), e.Attr, e.Sub)
				}
			})
		}
	}
}

// ④c 本地决策不许走 HTTP 分类。
//
// 本地信号（账本拒绝、熔断器自己、资源上限、密钥缺失）没有 HTTP 形状：
// 它们没有 status、没有 code、没有 type，只有一个 Kind 或什么都没有。
// 拿它去 Classify，最好的结果是撞上一条为别的目的写的规则——比如
// `Signal{Style: StyleLocal, Status: 500}` 会命中没有任何 When* 条件的
// `policy/resource_limit` 行，得到一张**伪造的表行**（"资源上限"）。
// 伪造比报错难查得多：它看起来完全正常，还能一路写进 trace。
func TestClassifyRejectsLocalStyle(t *testing.T) {
	cases := []struct {
		name string
		sig  Signal
	}{
		{"本地裸状态码", Signal{Style: StyleLocal, Status: 500}},
		{"本地 503", Signal{Style: StyleLocal, Status: 503}},
		{"本地 Kind", Signal{Style: StyleLocal, Kind: KindBudgetRefused}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, err := Classify(c.sig)
			if err == nil {
				t.Fatalf("本地决策不是上游信号：它没有 HTTP 形状，拿它去分类只会得到一张伪造的表行。\n"+
					"本例被分类成了 %s/%s（class=%s），而它的真实来源是本地决策。",
					e.Attr, e.Sub, e.Class())
			}
			if !errors.Is(err, ErrUnmapped) {
				t.Fatalf("必须返回 ErrUnmapped（开发期错误，要响亮），got %v", err)
			}
			if e != nil {
				t.Fatalf("出错时不得同时返回 *Error，got %v", e)
			}
			// 诊断信息里要能看出这是本地信号，否则排查时只能猜。
			if !strings.Contains(err.Error(), "local") {
				t.Fatalf("ErrUnmapped 信息应指明 style=local，got %q", err.Error())
			}
		})
	}

	// 2xx 是"成功"而不是"分类"：本地路径也会有成功信号，不该被这条拒绝规则误伤。
	if e, err := Classify(Signal{Style: StyleLocal, Status: 200}); e != nil || err != nil {
		t.Fatalf("StyleLocal 的 2xx 应返回 (nil,nil)，got (%v,%v)", e, err)
	}

	// 反向守卫：**只**堵 StyleLocal。StyleNative 是执行器在说话，它必须能被分类
	// （KindNativeDeadline 那条就是给它的）。顺手把 Native 也堵掉，
	// 会让执行器超时永远无法归因。
	e, err := Classify(Signal{Style: StyleNative, Kind: KindNativeDeadline})
	if err != nil {
		t.Fatalf("StyleNative 的执行器失败必须能分类，got %v —— 这条拒绝规则只该针对 StyleLocal", err)
	}
	if e.Sub != SubTimeout {
		t.Fatalf("StyleNative 的 KindNativeDeadline 应归成 %s，got %s", SubTimeout, e.Sub)
	}
}

// ④d 4xx 是"请求被拒"，不是"该归因不再细分"。
//
// 修的是一个结构性矛盾：(drift, none) 曾经同时指向两件不同的事——
//  1. 4xx：我们发出去的请求**形状**不被接受（ClassContract）；
//  2. KindSchemaDrift：2xx 回来了，但响应过不了我们自己声明的 schema（ClassDrift）。
//
// 于是 provider 用 `New(AttrUpstreamContractDrift, SubNone, …)` 报"解不出预期结构"时，
// Lookup 取到的是 4xx 那一行，Class 变成 contract（"请求被拒"）——
// 把"外部 API 变了"说成了"我们发错了"，与这套表存在的理由正相反。
//
// 修法是让结构自己说话：4xx 有 SubRejected 这个子类，(drift, none) 于是只剩
// schema 漂移一行。歧义消在结构里，不靠 Lookup 的顺序去掩盖。
func TestFourXXIsRejectedNotNone(t *testing.T) {
	for _, status := range []int{400, 404, 422} {
		e, err := Classify(Signal{Style: StyleOpenAI, Status: status})
		if err != nil {
			t.Fatalf("%d 应已映射，got %v", status, err)
		}
		if e.Sub != SubRejected {
			t.Fatalf("%d 的 sub 必须是 %s（请求形状不被接受），got %s —— "+
				"用 SubNone 会让 (drift, none) 同时指向 4xx 拒绝与 schema 漂移两件事",
				status, SubRejected, e.Sub)
		}
		if e.Class() != ClassContract {
			t.Fatalf("%d 的 class 必须是 %s，got %s", status, ClassContract, e.Class())
		}
		if got := e.Key(); got != "upstream_contract_drift/rejected" {
			t.Fatalf("%d 的 Key() 应为 upstream_contract_drift/rejected，got %q", status, got)
		}
	}

	// 这一半才是修复的验收点：**同一件事不再有两个答案**。
	// 响应形状不符合我们的预期，无论从哪条路径进来，Class 都必须是 drift。
	viaKind, err := Classify(Signal{Style: StyleOpenAI, Kind: KindSchemaDrift})
	if err != nil {
		t.Fatalf("KindSchemaDrift 应已映射，got %v", err)
	}
	viaNew := New(AttrUpstreamContractDrift, SubNone, "provider.Call")

	if got := viaKind.Class(); got != ClassDrift {
		t.Fatalf("Classify(KindSchemaDrift) 的 class 必须是 %s，got %s", ClassDrift, got)
	}
	if got := viaNew.Class(); got != ClassDrift {
		t.Fatalf("New(AttrUpstreamContractDrift, SubNone, …) 的 class 必须是 %s，got %s：\n"+
			"provider 用这条错误报'2xx 但解不出预期结构'，它是**响应**形状变了，不是请求被拒。\n"+
			"若这里是 %s，说明 New 又查到了 4xx 那一行（(drift, none) 的歧义回来了）。",
			ClassDrift, got, ClassContract)
	}
	if viaKind.Class() != viaNew.Class() {
		t.Fatalf("同一种失败出现两个 Class：KindSchemaDrift→%s，New(drift/none)→%s",
			viaKind.Class(), viaNew.Class())
	}
	// (drift, none) 不许再被 4xx 行占着：查到的必须是 schema 漂移那一行。
	if r, ok := Lookup(AttrUpstreamContractDrift, SubNone, 400); ok && r.Class != ClassDrift {
		t.Fatalf("(drift, none) 在 status=400 下仍命中 class=%s 的行：4xx 行必须改用 %s",
			r.Class, SubRejected)
	}
}

// ④ R-CB-05 的机器化守卫：容量/配额/draining 三类的行**必须**不计熔断。
// 这条独立于上表，因为它是"集群忙时不要把健康节点踢掉"的唯一保险。
func TestCapacityQuotaDrainingNeverCountAsFailure(t *testing.T) {
	n := 0
	for _, r := range Rules() {
		switch r.Class {
		case ClassCapacity, ClassQuota, ClassDraining:
			n++
			if r.CountsAsFailure {
				t.Fatalf("class %s 必须 CountsAsFailure=false（R-CB-05）；违反行：%s",
					r.Class, r.Why)
			}
			if r.Attr != AttrPolicy {
				t.Fatalf("class %s 的归因应是 policy，got %s", r.Class, r.Attr)
			}
		}
	}
	if n < 5 {
		t.Fatalf("容量/配额/draining 相关行数异常少（%d），映射表可能被削", n)
	}
}

// ⑤ 未知组合必须返回 ErrUnmapped，**不得**被静默归成 transport。
func TestUnknownSignalIsUnmappedNotTransport(t *testing.T) {
	// 418：表里没有，且不是 4xx 契约行覆盖的三个码。
	_, err := Classify(Signal{Style: StyleOpenAI, Status: 418})
	if err == nil {
		t.Fatal("未映射的状态码必须报错，不能返回一条'看起来正常'的 Error")
	}
	if !errors.Is(err, ErrUnmapped) {
		t.Fatalf("必须是 ErrUnmapped，got %v", err)
	}
	// 反向守卫：错误信息里要能看见原始证据，否则排查时只能猜。
	for _, want := range []string{"418", "openai"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("ErrUnmapped 信息缺少证据 %q：%s", want, err.Error())
		}
	}
}

// ⑥ 三套上游各 ≥3 条样本（R-IO-05），含 529 与 Ollama 无 code 的情形。
func TestThreeUpstreamsHaveSamples(t *testing.T) {
	samples := map[string][]Signal{
		"openai": {
			{Style: StyleOpenAI, Status: 429, Code: "rate_limit_exceeded"},
			{Style: StyleOpenAI, Status: 503, Code: "server_busy"},
			{Style: StyleOpenAI, Status: 500},
		},
		"anthropic": {
			{Style: StyleAnthropic, Status: 429, Type: "rate_limit_error"},
			{Style: StyleAnthropic, Status: 529, Type: "overloaded_error"},
			{Style: StyleAnthropic, Status: 400},
		},
		"ollama": {
			{Style: StyleOllama, Status: 429},
			{Style: StyleOllama, Status: 503},
			{Style: StyleOllama, Status: 400},
		},
	}
	for name, sigs := range samples {
		if len(sigs) < 3 {
			t.Fatalf("%s 样本不足 3 条", name)
		}
		for _, s := range sigs {
			if _, err := Classify(s); err != nil {
				t.Fatalf("%s 样本 %+v 未映射：%v", name, s, err)
			}
		}
	}
}

// 2xx 不是失败。Classify 必须返回 (nil, nil)，否则调用方会在成功路径上写错误 span。
func TestSuccessIsNil(t *testing.T) {
	for _, st := range []int{200, 201, 204} {
		e, err := Classify(Signal{Style: StyleOpenAI, Status: st})
		if e != nil || err != nil {
			t.Fatalf("status %d 应返回 (nil,nil)，got (%v,%v)", st, e, err)
		}
	}
}

// 错误文本必须稳定：Detail 是 map，但输出要有序，否则同一失败每次日志都不同。
func TestErrorTextIsDeterministic(t *testing.T) {
	e := New(AttrUpstreamContractDrift, SubNone, "provider.Call").
		WithStep("s3").
		WithDetail("expect_sha256", "aa").
		WithDetail("got_sha256", "bb").
		WithDetail("field", "intent.action")
	first := e.Error()
	for i := 0; i < 50; i++ {
		if got := e.Error(); got != first {
			t.Fatalf("错误文本不稳定：\n%s\n%s", first, got)
		}
	}
	if !strings.Contains(first, "expect_sha256=aa, field=intent.action, got_sha256=bb") {
		t.Fatalf("Detail 未按字典序输出：%s", first)
	}
}

// 值语义：With* 不许改动原对象（并发读同一错误是常态）。
func TestWithIsValueSemantics(t *testing.T) {
	base := New(AttrPolicy, SubCapacity, "provider.Call")
	step := base.WithStep("s1")
	if base.StepID != "" || step.StepID != "s1" {
		t.Fatal("WithStep 修改了原对象")
	}
	d := base.WithDetail("k", "v")
	if len(base.Detail) != 0 || len(d.Detail) != 1 {
		t.Fatal("WithDetail 修改了原对象")
	}
}

// ⑦ poisoned 是**模型**问题，不是传输问题。
//
// 这条测试守的是映射表存在的全部理由：把"模型文件坏了"归成 transport，
// 等于告诉运维"网络抖了，重试就行"，而真实原因是那颗模型在 Homonoia 里
// 已经被驱逐（gateway.py:2464-2466），重试只会被同一个坏文件再答一次。
func TestModelPoisonedIsModelNotTransport(t *testing.T) {
	e, err := Classify(Signal{Style: StyleOpenAI, Status: 503, Code: "model_poisoned"})
	if err != nil {
		t.Fatalf("model_poisoned 必须在映射表里有专门一行，不该 unmapped：%v", err)
	}
	if e == nil {
		t.Fatal("非 2xx 必须产出 *Error")
	}
	// 若归成 transport 则本测试失败，因为 poisoned 不是网络故障。
	if e.Attr != AttrModelContractViolation {
		t.Fatalf("attr 必须是 %s（模型违约），got %s —— "+
			"若这里是 %s，说明 poisoned 掉进了 5xx 兜底，把模型文件坏了说成了网络故障",
			AttrModelContractViolation, e.Attr, AttrTransport)
	}
	if e.Sub != SubPoisoned {
		t.Fatalf("sub 必须是 %s，got %s", SubPoisoned, e.Sub)
	}
	if got := e.Class(); got != ClassModel {
		t.Fatalf("class 必须是 %s，got %s", ClassModel, got)
	}
	// RetryOnce：换一次采样/探测机会即可，不能无限重试同一个坏文件。
	if got := e.Retry(); got != RetryOnce {
		t.Fatalf("retry 必须是 %s（封顶一次），got %s —— 无限重试会变成 gateway.py:2464 说的 retry loop",
			RetryOnce, got)
	}
	// 计熔断是**故意**的：熔断键是 (provider,model,endpoint_kind)，
	// 所以只隔离这一颗坏模型，同网关上的其他模型不受影响。
	if !e.CountsAsBreakerFailure() {
		t.Fatal("poisoned 必须计熔断：否则每次请求都先撞一次 503 再重试，正是 retry loop")
	}
	// 顺带守住可读性：Key() 会进 trace 与指标聚合，不能出现 unknown_*。
	if e.Key() != "model_contract_violation/poisoned" {
		t.Fatalf("Key() 应稳定可读，got %q", e.Key())
	}
}

// ⑧ 同一个 503 有三种含义，必须靠 body 里的 code 分开。
//
// 这条是防止将来有人"简化"成按状态码分类：一旦按状态码分，
// 容量拒绝会被算成故障（集群忙时踢掉健康节点），或者 poisoned 会被
// 当成容量（无限重试一颗坏模型）。两者都是生产事故。
func TestSameStatusThreeMeanings(t *testing.T) {
	sigs := []struct {
		name   string
		code   string
		counts bool
	}{
		{"server_busy（容量）", "server_busy", false},
		{"shutting_down（节点下线）", "shutting_down", false},
		{"model_poisoned（模型文件坏了）", "model_poisoned", true},
	}
	type classKey struct {
		class Class
		attr  Attribution
		sub   Subtype
	}
	seen := make(map[classKey]string, len(sigs))
	for _, c := range sigs {
		e, err := Classify(Signal{Style: StyleOpenAI, Status: 503, Code: c.code})
		if err != nil {
			t.Fatalf("%s：503/%s 必须已映射，got %v", c.name, c.code, err)
		}
		if e == nil {
			t.Fatalf("%s：非 2xx 必须产出 *Error", c.name)
		}
		k := classKey{e.Class(), e.Attr, e.Sub}
		if prev, dup := seen[k]; dup {
			t.Fatalf("分类撞车：%s 与 %s 都归成 %s/%s/%s；"+
				"503 的三种含义必须互不相同，否则只能靠状态码分类",
				prev, c.name, k.class, k.attr, k.sub)
		}
		seen[k] = c.name
		if got := e.CountsAsBreakerFailure(); got != c.counts {
			t.Fatalf("%s：CountsAsBreakerFailure 应为 %v，got %v", c.name, c.counts, got)
		}
	}
	if len(seen) != 3 {
		t.Fatalf("三种含义应产出三份不同分类，got %d 份", len(seen))
	}
}

// ⑨ Anthropic 面上 poisoned 与容量过载在**字节层无法区分**——这是上游的信息丢失，
// 不是我们分错。
//
// compat.py:96-111 的 error_body 在 anthropic 分支丢掉 code，503 只留下
// _ANTHROPIC_ERROR_TYPE[503] == "overloaded_error"。因此 Hermes 在 Anthropic 面上
// 会持续重试一颗 poisoned 模型（按容量让路）。这**不能**靠改映射表修，
// 改表等于用假信息补真缺口；这里只把已知的信息损失记录成断言。
func TestAnthropicFaceLosesTheCode(t *testing.T) {
	// Code 故意为空：上游根本没把这个信息传下来。
	e, err := Classify(Signal{Style: StyleAnthropic, Status: 503, Type: "overloaded_error"})
	if err != nil {
		t.Fatalf("anthropic 503 必须已映射，got %v", err)
	}
	if e == nil {
		t.Fatal("非 2xx 必须产出 *Error")
	}
	if e.Attr != AttrPolicy || e.Sub != SubCapacity {
		t.Fatalf("anthropic 503 只能归成 policy/capacity（信息丢失后的唯一安全猜测），"+
			"got %s/%s —— 若这里变成 model/poisoned，说明有人在没有证据的情况下猜了 code",
			e.Attr, e.Sub)
	}
	if got := e.Class(); got != ClassCapacity {
		t.Fatalf("class 必须是 %s，got %s", ClassCapacity, got)
	}
	if e.CountsAsBreakerFailure() {
		t.Fatal("anthropic 503 不得计熔断：这可能是容量过载，熔断会踢掉健康节点")
	}
}

// ⑩ Ollama 的错误体只有 {"error": text}（compat.py:107-110），没有 code 也没有 type，
// 所以 503 只能按状态码归成 capacity。同样，这是上游的信息缺失，不是我们分错：
// 一颗 poisoned 的 Ollama 模型在 Hermes 眼里与"忙"完全一样，会被持续重试。
// 不得通过对 body 做子串匹配来"补救"——那是脆弱的猜测，不是分类。
func TestOllamaFaceHasOnlyStatus(t *testing.T) {
	e, err := Classify(Signal{Style: StyleOllama, Status: 503})
	if err != nil {
		t.Fatalf("ollama 503 必须已映射，got %v", err)
	}
	if e == nil {
		t.Fatal("非 2xx 必须产出 *Error")
	}
	if e.Attr != AttrPolicy || e.Sub != SubCapacity {
		t.Fatalf("ollama 503 只能归成 policy/capacity，got %s/%s", e.Attr, e.Sub)
	}
	if got := e.Class(); got != ClassCapacity {
		t.Fatalf("class 必须是 %s，got %s", ClassCapacity, got)
	}
	if e.CountsAsBreakerFailure() {
		t.Fatal("ollama 503 不得计熔断：状态码里看不出是容量还是模型坏，按容量处理")
	}
}

// declConstNames 从**声明处**读出某个枚举类型（Attribution / Subtype）的
// const 块里有哪些常量名。Attribution 与 Subtype 共用它，不复制两遍。
//
// 为什么必须读源码而不能只断言 len(AttrAll) / len(SubAll)：这两个都是数值枚举，
// 运行期没有任何办法枚举"声明了哪些值"。只查长度的话，有人加一个
// `AttrFoo` 而忘了更新 AttrAll，len 仍然是 6，测试会安静通过 ——
// 那正是这两条守卫要防的事，所以必须拿声明数去对。
func declConstNames(t *testing.T, file, typeName string) []string {
	t.Helper()
	fset := token.NewFileSet()
	// 测试的 cwd 是包目录，直接读同目录的源文件。
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("解析 %s 失败：穷尽性守卫依赖它的结构，读不到就无法判定: %v", file, err)
	}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST || len(gd.Specs) == 0 {
			continue
		}
		// 被定位的 const 块以 `XxxNone <Type> = iota`（或 `= 0`）开头：
		// 第一个 ValueSpec 显式写了类型，后续 spec 的类型/值由它继承。
		first, ok := gd.Specs[0].(*ast.ValueSpec)
		if !ok || first.Type == nil {
			continue
		}
		ident, ok := first.Type.(*ast.Ident)
		if !ok || ident.Name != typeName {
			continue
		}
		names := make([]string, 0, len(gd.Specs))
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, nm := range vs.Names {
				names = append(names, nm.Name)
			}
		}
		return names
	}
	t.Fatalf("%s 里找不到 `%s` 的 const 块 —— 守卫的定位逻辑失效了，"+
		"要么块被改名，要么类型不再显式声明；两种情况下穷尽性断言都已空转", file, typeName)
	return nil
}

// ⑪ Subtype 是闭集，且必须和 Attribution 一样有穷尽性守卫。
//
// 在此之前它是唯一没有守卫的闭集：加一个子类不需要更新任何地方，
// 于是"新增子类"与"忘记新增子类"在测试层面完全无法区分 ——
// 这正是"加值容易、漏测容易"的形状。
func TestSubtypeClosedSet(t *testing.T) {
	// ① 长度钉死。加子类时这里会红，提醒你确认下游（trace 的 String()、
	// docs/TRACE-SCHEMA.md）也已经跟上，而不是悄悄多一个值。
	if len(SubAll) != 9 {
		t.Fatalf("SubAll 长度必须是 9（闭集），got %d。加子类请同步更新 "+
			"docs/TRACE-SCHEMA.md 与本节注释里的数字，别只改常量", len(SubAll))
	}

	// ② 每一项都必须可读、且两两不重复。
	// 重复会让 trace 里两种不同失败长得一模一样，排查时无法区分。
	seen := map[string]bool{}
	for _, s := range SubAll {
		str := s.String()
		if str == "" || strings.HasPrefix(str, "unknown_") {
			t.Fatalf("子类 %d 的 String() 未实现：%q —— "+
				"它会被写进 trace 与 Error.Key()，必须可读", uint8(s), str)
		}
		if seen[str] {
			t.Fatalf("子类字符串重复：%q —— trace 里两种失败会长得一样", str)
		}
		seen[str] = true
	}
	if len(seen) != len(SubAll) {
		t.Fatalf("SubAll 去重后只剩 %d 项（%d 项），存在重复值", len(seen), len(SubAll))
	}

	// ③ 值必须按声明顺序连续覆盖 0..len-1：漏列中间某个值会表现为数值空洞。
	for i, s := range SubAll {
		if int(s) != i {
			t.Fatalf("SubAll[%d] 的数值是 %d，闭集必须按声明顺序逐一列出；"+
				"中间漏项意味着某个子类没被穷尽性断言覆盖（它会是 %s）", i, int(s), s)
		}
	}

	// ④ 声明数必须等于 SubAll 的长度 —— 这条才是真正的"忘了更新"检测。
	names := declConstNames(t, "code.go", "Subtype")
	if len(names) != len(SubAll) {
		t.Fatalf("code.go 声明了 %d 个 Subtype 常量 %v，但 SubAll 只有 %d 项：\n"+
			"加了子类却忘了更新 SubAll，此时穷尽性断言对其他包完全不设防。\n"+
			"请把新值加进 SubAll，并确认它的 String() 可读。", len(names), names, len(SubAll))
	}
}

// ⑫ 本地构造的 policy/denied 必须继承表里 401/403 那条的重试与熔断策略。
//
// 背景（D-023 末节）：`backend.auth` 的密钥只从 env 来、**没有 `default`**；
// 环境变量缺失时 Hermes 必须在**发请求之前**报 policy/denied + Retry:none。
// 这条错误不是 HTTP 失败（Status 为 0），所以它不走 Classify 的 HTTP 分支，
// 而是靠 `Lookup(attr, sub)` 反查到 `401/403 → policy/denied` 那一行。
//
// 也就是说：这个"巧合"（本地构造的分类恰好命中表里一条 HTTP 规则）
// 实际上是一条**契约** —— 表是唯一的策略事实源，本地决策不许自己另定一套。
// 本测试把它钉住，理由见下面的失败消息。
func TestLocalDeniedInheritsTheAuthRow(t *testing.T) {
	// op 用配置解析时的真实名字，让失败消息指向真实调用点。
	e := New(AttrPolicy, SubDenied, "resolve_auth")
	// Status 保持 0：这不是"上游返回了 401"，是我们自己发现配置缺失，
	// 一个请求都还没发出去。若这里非 0，说明有人把本地决策伪装成了 HTTP 失败。
	if e.Status != 0 {
		t.Fatalf("本地拒绝的 Status 必须为 0（非 HTTP），got %d", e.Status)
	}

	if got := e.Retry(); got != RetryNone {
		t.Fatalf("Retry() 必须是 %s，got %s：\n"+
			"本地构造的 policy/denied 必须继承表里 401/403 那条的重试策略。"+
			"走的是 Lookup(attr,sub) 而不是 HTTP 分支；若这里断了，说明 Lookup 的匹配条件被改窄"+
			"（例如要求 Status 必填），后果是\"忘了配密钥\"会退化成可重试错误，"+
			"把一个配置失误变成重试风暴（D-023 末节第 3 条）。", RetryNone, got)
	}
	if got := e.CountsAsBreakerFailure(); got != false {
		t.Fatalf("CountsAsBreakerFailure() 必须是 false，got %v：\n"+
			"配置缺失不是上游故障，记进熔断会让熔断器为我们的配置失误背锅，"+
			"把该 (provider,model) 的健康节点一起断路（R-CB-05 的同一条道理）。", got)
	}
	if got := e.Key(); got != "policy/denied" {
		t.Fatalf("Key() 必须是 \"policy/denied\"，got %q —— "+
			"它是机读标识，会进 trace 与指标聚合，改了就断历史数据", got)
	}

	// Class 是这条测试里**唯一能真正红**的断言（见下方注释）：
	// 本地错误的 e.rule 为 nil，Class() 走 Lookup；Lookup 失配时
	// Class() 会回落到 ClassTransport —— 那正是本测试要抓的退化。
	if got := e.Class(); got != ClassPolicy {
		t.Fatalf("Class() 必须是 %s，got %s：\n"+
			"本地 policy/denied 的 class 只能来自 Lookup 命中的那一行；"+
			"落到 %s 说明 Lookup 已经查不到了（而 %s 会让 trace 与 '网络故障' 无法区分）。",
			ClassPolicy, got, ClassTransport, ClassTransport)
	}

	// 直接对表行断言：Retry()/CountsAsBreakerFailure() 各有自己的默认值兜底
	// （Retry 默认 RetryNone、AttrPolicy 默认不计熔断），单看它们无法区分
	// "查到了那一行" 与 "查不到但默认值恰好相同"。所以这里必须把行本身钉住。
	r, ok := Lookup(AttrPolicy, SubDenied, 0)
	if !ok {
		t.Fatalf("Lookup(AttrPolicy, SubDenied, status=0) 查不到任何一行：\n" +
			"本地拒绝必须继承表里 401/403 那条的策略；查不到说明匹配条件被改窄" +
			"（例如要求 Status 必填），或那条规则被删了。")
	}
	if r.Retry != RetryNone {
		t.Fatalf("命中行的 Retry 必须是 %s，got %s（行：%s）", RetryNone, r.Retry, r.Why)
	}
	if r.CountsAsFailure {
		t.Fatalf("命中行必须 CountsAsFailure=false，got true（行：%s）—— "+
			"认证/授权失败是政策决定，不该让熔断器背锅", r.Why)
	}
}
