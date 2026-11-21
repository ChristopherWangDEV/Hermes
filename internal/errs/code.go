// Package errs 是 Hermes 的单一错误模型：闭集归因 + 数据驱动映射表。
//
// 设计约束（ARCH §5.7）：
//   - 归因是**闭集**，共 6 个真实值（另有 AttrNone 作零值哨兵）。没有 "other" 桶：
//     一个无法归因的失败必须表现为 ErrUnmapped（响亮的开发期错误），
//     而不是被悄悄塞进 transport —— 那会让"到底是模型胡说还是外部 API 变了"永远答不出来。
//   - 映射表只有一处（table.go），三套上游 + native + local 共用同一张。
package errs

// Attribution 是失败的责任归属。闭集，不要加值。
type Attribution uint8

const (
	// AttrNone 是零值哨兵，不参与归因闭集。用零值表示"未分类"，
	// 让"忘记赋值"在测试里立即可见（AttrAll 不含它）。
	AttrNone Attribution = 0

	// AttrModelContractViolation：模型输出违反了它自己声明过的契约。
	// 典型：tool_call 参数过不了 schema、JSON 解不出。
	AttrModelContractViolation Attribution = 1

	// AttrUpstreamContractDrift：上游的**形状**变了（字段改名、删除、类型变）。
	// 这是"外部 API 变了"的那一桶，必须与模型胡说严格分开。
	AttrUpstreamContractDrift Attribution = 2

	// AttrTransport：连接/超时/5xx，或执行器进程非零退出。
	AttrTransport Attribution = 3

	// AttrPolicy：准入类决定（容量、配额、审批、资源上限）。
	AttrPolicy Attribution = 4

	// AttrBudget：本地账本拒绝，请求根本没发出去。
	AttrBudget Attribution = 5

	// AttrCircuit：熔断器开着，请求根本没发出去。
	AttrCircuit Attribution = 6
)

// AttrAll 是闭集本身。长度必须为 6，且每个值的 String() 必须可读。
// 测试拿它做穷尽性断言——加值而不更新它，测试立刻红。
var AttrAll = [...]Attribution{
	AttrModelContractViolation,
	AttrUpstreamContractDrift,
	AttrTransport,
	AttrPolicy,
	AttrBudget,
	AttrCircuit,
}

func (a Attribution) String() string {
	switch a {
	case AttrModelContractViolation:
		return "model_contract_violation"
	case AttrUpstreamContractDrift:
		return "upstream_contract_drift"
	case AttrTransport:
		return "transport"
	case AttrPolicy:
		return "policy"
	case AttrBudget:
		return "budget"
	case AttrCircuit:
		return "circuit"
	}
	// 不 panic：错误类型的 String() 在诊断路径上被调用，它自己再崩就无从诊断。
	return "unknown_attribution"
}

// Subtype 是归因下的细分。零值为 SubNone（"该归因不再细分"）。
type Subtype uint8

const (
	SubNone          Subtype = iota
	SubTimeout               // transport：deadline 到 / 执行器被杀
	SubRefused               // transport：连接被拒
	SubCapacity              // policy：server_busy / overloaded / draining —— **不计熔断**
	SubQuota                 // policy：配额 —— **不计熔断**
	SubDenied                // policy：allowlist / 审批 / 认证
	SubResourceLimit         // policy：内存 / trace 上界
	// SubPoisoned：model_contract_violation 下的细分 —— 模型文件处于 poisoned 态
	// （Homonoia 在连续违约之后把它驱逐出池）。归因**不是** AttrTransport：
	// 模型文件坏了不是网络故障，这正是这张表存在的理由。
	SubPoisoned
	// SubRejected：请求**形状**不被上游接受（4xx）。
	//
	// 它属于 AttrUpstreamContractDrift，是"拒绝"语义里 policy 之外的那一种：
	// 我们发出去的请求被对方拒收，与"模型胡说"、"网络故障"、
	// "我们自己声明的 schema 没过"是三件不同的事，必须能分开。
	//
	// 之所以不能借用 SubNone：SubNone 的语义是"该归因不再细分"，
	// 拿它表示"请求被拒"是偷懒，而且会让 (drift, none) 同时指向
	// "4xx 请求被拒"（ClassContract）与"响应 schema 漂移"（ClassDrift）两组语义，
	// 同一个归因下出现两个 Class —— 那正是这套表最该避免的自相矛盾。
	SubRejected
)

// SubAll 是子类闭集本身。长度必须为 9。
//
// SubNone **在**里面，这是它与 AttrNone 的关键区别：AttrNone 是"未分类"的零值哨兵，
// 不属于闭集；而 SubNone 是**合法**零值，含义明确——"该归因不再细分"（例如
// transport 下的裸 5xx、model_contract_violation 下的普通违约）。
// 因此穷尽性断言必须把它算作一个真实成员。
//
// 测试拿它做穷尽性断言：加值而不更新它，测试立刻红。
var SubAll = [...]Subtype{
	SubNone,
	SubTimeout,
	SubRefused,
	SubCapacity,
	SubQuota,
	SubDenied,
	SubResourceLimit,
	SubPoisoned,
	SubRejected,
}

func (s Subtype) String() string {
	switch s {
	case SubNone:
		return "none"
	case SubTimeout:
		return "timeout"
	case SubRefused:
		return "refused"
	case SubCapacity:
		return "capacity"
	case SubQuota:
		return "quota"
	case SubDenied:
		return "denied"
	case SubResourceLimit:
		return "resource_limit"
	case SubPoisoned:
		return "poisoned"
	case SubRejected:
		return "rejected"
	}
	return "unknown_subtype"
}

// Class 是映射表的输入分类，也是 trace 里给人看的那一列。
type Class uint8

const (
	ClassSuccess Class = iota
	ClassTransport
	ClassServer
	ClassCapacity
	ClassQuota
	ClassDraining
	ClassContract
	ClassDrift
	ClassModel
	ClassBudget
	ClassPolicy
	ClassCircuit
)

func (c Class) String() string {
	switch c {
	case ClassSuccess:
		return "success"
	case ClassTransport:
		return "transport"
	case ClassServer:
		return "server"
	case ClassCapacity:
		return "capacity"
	case ClassQuota:
		return "quota"
	case ClassDraining:
		return "draining"
	case ClassContract:
		return "contract"
	case ClassDrift:
		return "drift"
	case ClassModel:
		return "model"
	case ClassBudget:
		return "budget"
	case ClassPolicy:
		return "policy"
	case ClassCircuit:
		return "circuit"
	}
	return "unknown_class"
}

// RetryPolicy 是"这一类失败该怎么重试"的唯一事实源。
type RetryPolicy uint8

const (
	// RetryNone：不重试（确定性失败，重试只会再错一次并多花钱）。
	RetryNone RetryPolicy = iota
	// RetryOnce：最多一次（模型违约：换一次采样可能就好了）。
	RetryOnce
	// RetryIdempotentOnly：只对幂等调用重试（5xx / 超时）。
	RetryIdempotentOnly
	// RetryRespectRetryAfter：按上游给的 Retry-After 让路（容量/配额）。
	RetryRespectRetryAfter
)

func (r RetryPolicy) String() string {
	switch r {
	case RetryNone:
		return "none"
	case RetryOnce:
		return "once"
	case RetryIdempotentOnly:
		return "idempotent_only"
	case RetryRespectRetryAfter:
		return "respect_retry_after"
	}
	return "unknown_retry_policy"
}

// Style 区分"谁在说话"。同一个状态码在不同协议下含义不同，靠它分流。
type Style uint8

const (
	StyleOpenAI Style = iota
	StyleAnthropic
	StyleOllama
	StyleNative // 执行器（shim / 子进程）
	StyleLocal  // 本地决策（账本、熔断器）
)

func (s Style) String() string {
	switch s {
	case StyleOpenAI:
		return "openai"
	case StyleAnthropic:
		return "anthropic"
	case StyleOllama:
		return "ollama"
	case StyleNative:
		return "native"
	case StyleLocal:
		return "local"
	}
	return "unknown_style"
}

// Kind 是**非 HTTP** 失败的种类。HTTP 失败走 Status/Code/Type，非 HTTP 走这里。
// 两套信号分开，避免"用 0 状态码表示本地决策"这种含糊。
type Kind uint8

const (
	KindNone           Kind = iota
	KindSchemaDrift         // 响应体过不了我们声明的 schema
	KindModelViolation      // 模型输出违反契约
	KindConnectFailure      // 连接失败 / 重置 / TLS 失败（PRD §4.1.1 第 1 行）
	KindNativeDeadline      // 执行器 deadline / 被 kill / 非零退出
	KindSandboxDenied       // HM_E_SANDBOX_DENIED
	KindBudgetRefused       // 账本拒绝
	KindCircuitOpen         // 熔断器开着
	KindSuccess
)

func (k Kind) String() string {
	switch k {
	case KindNone:
		return "none"
	case KindSchemaDrift:
		return "schema_drift"
	case KindModelViolation:
		return "model_violation"
	case KindConnectFailure:
		return "connect_failure"
	case KindNativeDeadline:
		return "native_deadline"
	case KindSandboxDenied:
		return "sandbox_denied"
	case KindBudgetRefused:
		return "budget_refused"
	case KindCircuitOpen:
		return "circuit_open"
	case KindSuccess:
		return "success"
	}
	return "unknown_kind"
}
