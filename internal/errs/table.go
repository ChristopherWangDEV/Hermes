package errs

import (
	"errors"
	"fmt"
)

// StyleAny 让"与协议无关"的规则（4xx/5xx 兜底）不必为三套上游各写一遍。
// 它不是 Style 的一个真实值时才有意义——只出现在 Rule.Style 里。
const StyleAny Style = 255

// Signal 是一次失败的**全部可用证据**。分类只依赖它，不依赖任何全局状态。
type Signal struct {
	Style  Style
	Status int               // HTTP 状态；0 = 非 HTTP
	Code   string            // 上游机读 code（ollama 恒为空）
	Type   string            // anthropic 的 error.type
	Header map[string]string // 需要时看 Retry-After 之类
	Kind   Kind              // 非 HTTP 失败的种类
}

// Rule 是映射表的一行。表是**唯一**的失败分类事实源：
// 归因、子类、重试策略、是否计熔断，四件事在同一行里，不许分家。
type Rule struct {
	Style      Style
	WhenStatus []int
	WhenCode   string
	WhenType   string
	WhenHeader string
	WhenKind   Kind

	Class           Class
	Attr            Attribution
	Sub             Subtype
	Retry           RetryPolicy
	CountsAsFailure bool
	Why             string
}

func (r Rule) match(s Signal) bool {
	if r.Style != StyleAny && r.Style != s.Style {
		return false
	}
	if r.WhenKind != KindNone {
		return r.WhenKind == s.Kind
	}
	if s.Kind != KindNone || s.Status == 0 {
		return false
	}
	if len(r.WhenStatus) > 0 {
		hit := false
		for _, st := range r.WhenStatus {
			if st == s.Status {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	if r.WhenCode != "" && r.WhenCode != s.Code {
		return false
	}
	if r.WhenType != "" && r.WhenType != s.Type {
		return false
	}
	if r.WhenHeader != "" {
		if s.Header == nil {
			return false
		}
		if _, ok := s.Header[r.WhenHeader]; !ok {
			return false
		}
	}
	return true
}

// rules 的顺序即优先级：**具体规则在前，兜底在后**。
// 顺序被改动会立刻被 internal/errs/errs_test.go 抓出来（503 capacity 会退化成 5xx server），
// 以及 tests/guard/mapping_table_test.go 的逐行比对（它要求 docs/ARCH.md §5.7 的摘要
// 与这张表逐条同序同值——摘要是读者的契约，漂了比代码错更难发现）。
var rules = []Rule{
	// —— OpenAI 面 ——
	{Style: StyleOpenAI, WhenStatus: []int{429}, WhenCode: "rate_limit_exceeded",
		Class: ClassQuota, Attr: AttrPolicy, Sub: SubQuota,
		Retry: RetryRespectRetryAfter, CountsAsFailure: false,
		Why: "配额不是故障，重试按上游给的 Retry-After 让路"},
	{Style: StyleOpenAI, WhenStatus: []int{503}, WhenCode: "server_busy",
		Class: ClassCapacity, Attr: AttrPolicy, Sub: SubCapacity,
		Retry: RetryRespectRetryAfter, CountsAsFailure: false,
		Why: "gateway.py:5373-5389；容量拒绝记进熔断会在集群忙时踢掉健康节点"},
	{Style: StyleOpenAI, WhenStatus: []int{503}, WhenCode: "shutting_down",
		Class: ClassDraining, Attr: AttrPolicy, Sub: SubCapacity,
		Retry: RetryIdempotentOnly, CountsAsFailure: false,
		Why: "节点正在下线：允许重试但**换节点**是 engine 的路由决定，不是退避；不计熔断"},
	{Style: StyleOpenAI, WhenStatus: []int{503}, WhenCode: "model_poisoned",
		Class: ClassModel, Attr: AttrModelContractViolation, Sub: SubPoisoned,
		Retry: RetryOnce, CountsAsFailure: true,
		Why: "gateway.py:2458-2466：模型连续违约后进入 poisoned 态，请求本身没错、" +
			"模型还在内存里，但重试会被同一个坏文件再答一次。RetryOnce 封顶，" +
			"计熔断是为了让该 (provider,model) 停止被喂请求；半开时给一次探测，" +
			"运维 reload 之后流量自然恢复"},

	// —— Anthropic 面 ——
	{Style: StyleAnthropic, WhenStatus: []int{429}, WhenType: "rate_limit_error",
		Class: ClassQuota, Attr: AttrPolicy, Sub: SubQuota,
		Retry: RetryRespectRetryAfter, CountsAsFailure: false,
		Why: "Anthropic 靠 error.type 而非 code"},
	{Style: StyleAnthropic, WhenStatus: []int{503, 529}, WhenType: "overloaded_error",
		Class: ClassCapacity, Attr: AttrPolicy, Sub: SubCapacity,
		Retry: RetryRespectRetryAfter, CountsAsFailure: false,
		Why: "529 是 Anthropic 的过载码（compat.py:84-91），必须与 503 同桶"},

	// —— Ollama 面（无 code，只能按状态码） ——
	{Style: StyleOllama, WhenStatus: []int{429},
		Class: ClassQuota, Attr: AttrPolicy, Sub: SubQuota,
		Retry: RetryRespectRetryAfter, CountsAsFailure: false,
		Why: "Ollama 错误体只有 {\"error\": text}，不得对 body 做子串匹配"},
	{Style: StyleOllama, WhenStatus: []int{503},
		Class: ClassCapacity, Attr: AttrPolicy, Sub: SubCapacity,
		Retry: RetryRespectRetryAfter, CountsAsFailure: false,
		Why: "同上：只有状态码是机读的"},

	// —— 与协议无关的 HTTP 兜底 ——
	// 4xx 用 SubRejected 而不是 SubNone：SubNone 的含义是"该归因不再细分"，
	// 而 (drift, none) 必须留给 KindSchemaDrift（响应形状变了）。
	// 两者若共用 SubNone，同一归因下会出现两个 Class —— 结构性矛盾，
	// 靠"Lookup 取第一条"去掩盖它只是把错误藏进顺序里。
	{Style: StyleAny, WhenStatus: []int{400, 404, 422},
		Class: ClassContract, Attr: AttrUpstreamContractDrift, Sub: SubRejected,
		Retry: RetryNone, CountsAsFailure: true,
		Why: "请求形状不被接受 = 上游契约变了，重试只会再错一次"},
	{Style: StyleAny, WhenStatus: []int{401, 403},
		Class: ClassPolicy, Attr: AttrPolicy, Sub: SubDenied,
		Retry: RetryNone, CountsAsFailure: false,
		Why: "认证/授权失败是政策决定：换 key 才有用，且不该让熔断器背锅。" +
			"这一行是 §5.7 那 15 行之外补的第 16 行——401 不是「上游新增状态码」，" +
			"它是每个部署都会遇到的一等错误，落进 ErrUnmapped 是设计缺陷不是严格"},
	{Style: StyleAny, WhenStatus: []int{500, 502, 503, 504, 529},
		Class: ClassServer, Attr: AttrTransport, Sub: SubNone,
		Retry: RetryIdempotentOnly, CountsAsFailure: true,
		Why: "兜底：走到这里说明没有更具体的规则命中，按服务端故障处理"},

	// —— 本地决策（无 HTTP 信号；Classify 永远选不中它，只有 Lookup 会） ——
	//
	// 这一行没有任何 When* 条件，是**故意**的：它唯一的入口是
	// `New(AttrPolicy, SubResourceLimit, …)` → Lookup(attr, sub)，没有 HTTP 证据可匹配。
	// 也正因为它没有条件，Style 必须是 StyleLocal 而不是 StyleAny：
	// match() 只比较 style，若改成 StyleAny，这一行会吞掉**一切**没有更具体规则
	// 命中的 HTTP 失败（例如 400 会从 contract/drift 变成 policy/resource_limit）。
	// 位置同样不能提前：Lookup 返回第一条 (attr, sub) 匹配的行，
	// 虽然 SubResourceLimit 目前只有这一行，仍按"具体在前、本地在后"的既定顺序放。
	{Style: StyleLocal,
		Class: ClassPolicy, Attr: AttrPolicy, Sub: SubResourceLimit,
		Retry: RetryNone, CountsAsFailure: false,
		Why: "stdout_cap / trace 上界由本地截断判定，不是上游故障；重试只会再超一次"},

	// —— 非 HTTP 信号 ——
	//
	// 这七行**必须**显式写 Style: StyleAny。Style 的零值是 StyleOpenAI（iota 0），
	// 而 match() 的第一条就是 style 检查——省略 Style 等于把"枚举声明顺序"当成语义，
	// 后果是只有 StyleOpenAI 的信号能匹配：StyleNative 的执行器超时
	// （KindNativeDeadline）会一路落空到 ErrUnmapped，把"执行器被杀"报成
	// "上游出现了表里没见过的组合"，让人去查一个不存在的上游。
	//
	// 语义上本来就该是无关协议：一次执行器超时就是执行器超时，
	// 与"哪个协议在说话"没有关系。
	{Style: StyleAny, WhenKind: KindSchemaDrift,
		Class: ClassDrift, Attr: AttrUpstreamContractDrift, Sub: SubNone,
		Retry: RetryNone, CountsAsFailure: true,
		Why: "响应体过不了我们自己声明的 schema：这是形状变了，不是模型胡说"},
	{Style: StyleAny, WhenKind: KindModelViolation,
		Class: ClassModel, Attr: AttrModelContractViolation, Sub: SubNone,
		Retry: RetryOnce, CountsAsFailure: false,
		Why: "换一次采样可能就好了，且不是上游的错"},
	{Style: StyleAny, WhenKind: KindConnectFailure,
		Class: ClassTransport, Attr: AttrTransport, Sub: SubRefused,
		Retry: RetryIdempotentOnly, CountsAsFailure: true,
		Why: "PRD §4.1.1 第 1 行：连接失败/重置/TLS 失败——重试且计熔断"},
	{Style: StyleAny, WhenKind: KindNativeDeadline,
		Class: ClassTransport, Attr: AttrTransport, Sub: SubTimeout,
		Retry: RetryIdempotentOnly, CountsAsFailure: true,
		Why: "执行器 deadline / 被杀 / 非零退出"},
	{Style: StyleAny, WhenKind: KindSandboxDenied,
		Class: ClassPolicy, Attr: AttrPolicy, Sub: SubDenied,
		Retry: RetryNone, CountsAsFailure: false,
		Why: "沙箱拒绝是策略，不是故障"},
	{Style: StyleAny, WhenKind: KindBudgetRefused,
		Class: ClassBudget, Attr: AttrBudget, Sub: SubNone,
		Retry: RetryNone, CountsAsFailure: false,
		Why: "钱不够，重试没有意义"},
	{Style: StyleAny, WhenKind: KindCircuitOpen,
		Class: ClassCircuit, Attr: AttrCircuit, Sub: SubNone,
		Retry: RetryNone, CountsAsFailure: false,
		Why: "熔断器自己的决定，不能再喂回自己"},
}

// ErrUnmapped 是**开发期**错误：上游出现了表里没有的组合。
// 它必须响亮——静默归成 transport 会让"到底是模型胡说还是外部 API 变了"永远答不出来。
var ErrUnmapped = errors.New("errs: unmapped failure signal")

// Classify 把一次失败证据翻成一条 *Error。2xx 返回 (nil, nil)。
//
// StyleLocal 一律拒绝：本地决策（账本拒绝、熔断器自己、资源上限）没有 HTTP 形状，
// 它们只能由 New() 按 (attr, sub) 构造。拿一个本地信号来走 HTTP 分类，
// 最好的结果也是撞上一条为别的目的写的规则，得到一张伪造的表行——
// 那比报错更难查，因为它看起来完全正常。
func Classify(s Signal) (*Error, error) {
	if s.Kind == KindSuccess || (s.Kind == KindNone && s.Status >= 200 && s.Status < 300) {
		return nil, nil
	}
	// 放在成功判定之后：2xx 不是"分类"，本地路径也可能有成功信号，
	// 成功不需要表行，不该被这条拒绝规则误伤。
	if s.Style == StyleLocal {
		return nil, fmt.Errorf("%w: style=local 不是上游信号（status=%d kind=%s）——"+
			"本地决策请用 errs.New(attr, sub, op) 构造，它经由 Lookup 取表行，不走 HTTP 分类",
			ErrUnmapped, s.Status, s.Kind)
	}
	for i := range rules {
		r := &rules[i]
		if !r.match(s) {
			continue
		}
		e := &Error{
			Attr:   r.Attr,
			Sub:    r.Sub,
			Op:     "",
			Status: s.Status,
			rule:   r,
		}
		return e, nil
	}
	return nil, fmt.Errorf("%w: style=%s status=%d code=%q type=%q kind=%s",
		ErrUnmapped, s.Style, s.Status, s.Code, s.Type, s.Kind)
}

// Rules 暴露只读副本，供穷尽性测试与 `hermes doctor` 的映射表自检使用。
func Rules() []Rule {
	out := make([]Rule, len(rules))
	copy(out, rules)
	return out
}

// Lookup 按归因+子类反查规则（供本地构造的 *Error 走同一套重试/熔断策略）。
func Lookup(attr Attribution, sub Subtype, status int) (Rule, bool) {
	for i := range rules {
		r := &rules[i]
		if r.Attr == attr && r.Sub == sub {
			if status == 0 || len(r.WhenStatus) == 0 {
				return *r, true
			}
			for _, st := range r.WhenStatus {
				if st == status {
					return *r, true
				}
			}
		}
	}
	return Rule{}, false
}

func retryFor(attr Attribution, sub Subtype, status int) RetryPolicy {
	if r, ok := Lookup(attr, sub, status); ok {
		return r.Retry
	}
	return RetryNone
}
