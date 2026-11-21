package errs

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Error 是全仓唯一的失败载体。它是**值语义**的：构造后不再改动，
// 因此可以安全地跨 goroutine 读取（trace 写入与 engine 判定会并发读同一个错误）。
type Error struct {
	Attr   Attribution
	Sub    Subtype
	Op     string            // "provider.Call" / "engine.step" / "native.spawn"
	StepID string            // R-TR-02：trace 的 failure.span_id 由此派生，必填
	Target string            // "(provider,model,endpoint_kind)" 的字符串形式
	Status int               // HTTP 状态；0 表示非 HTTP
	Detail map[string]string // 结构化细节：expect/got sha256、schema 关键字 …
	cause  error
	// rule 指向映射表里命中的那一行。重试策略与"是否计熔断"都从它读，
	// 保证同一件事只有一个来源；本地构造的错误没有 rule，走 Lookup 反查。
	rule *Rule
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.Op)
	if e.StepID != "" {
		b.WriteString("[")
		b.WriteString(e.StepID)
		b.WriteString("]")
	}
	b.WriteString(": ")
	b.WriteString(e.Attr.String())
	if e.Sub != SubNone {
		b.WriteString("/")
		b.WriteString(e.Sub.String())
	}
	if e.Status != 0 {
		b.WriteString(" status=")
		b.WriteString(strconv.Itoa(e.Status))
	}
	if e.Target != "" {
		b.WriteString(" target=")
		b.WriteString(e.Target)
	}
	// Detail 必须**有序**输出：map 迭代顺序随机，会让同一份失败产生两种日志文本，
	// 而日志文本会进 trace、被 diff、被拿去判回归。
	if len(e.Detail) > 0 {
		keys := make([]string, 0, len(e.Detail))
		for k := range e.Detail {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString(" {")
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(k)
			b.WriteString("=")
			b.WriteString(e.Detail[k])
		}
		b.WriteString("}")
	}
	if e.cause != nil {
		b.WriteString(": ")
		b.WriteString(e.cause.Error())
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.cause }

// Is 让 errors.Is 按 (Attr, Sub) 命中：调用方关心的是"这是容量问题还是模型问题"，
// 而不是"这两条错误是不是同一个变量"。
func (e *Error) Is(target error) bool {
	var t *Error
	if !errors.As(target, &t) {
		return false
	}
	return e.Attr == t.Attr && e.Sub == t.Sub
}

// WithDetail 返回副本并合并一条细节（值语义，不修改原对象）。
func (e *Error) WithDetail(k, v string) *Error {
	c := *e
	c.Detail = make(map[string]string, len(e.Detail)+1)
	for kk, vv := range e.Detail {
		c.Detail[kk] = vv
	}
	c.Detail[k] = v
	return &c
}

// WithCause 返回副本并挂上底层错误。
func (e *Error) WithCause(err error) *Error {
	c := *e
	c.cause = err
	return &c
}

// WithStep 返回副本并补上 step_id（engine 在拿到步骤上下文后才能填）。
func (e *Error) WithStep(stepID string) *Error {
	c := *e
	c.StepID = stepID
	return &c
}

// New 构造一条已分类的错误，并挂上映射表里对应的那一行（若表里有）。
//
// 为什么要在这里 Lookup：本包的第一性契约是"表是唯一事实源"（见 Error.rule 的注释）。
// 若本地构造的错误不挂 rule，Retry()/Class()/CountsAsBreakerFailure() 就各自
// 退回自己的默认值——同一件事出现两条来源，而两条来源一旦分叉，分叉是**静默**的：
// 错误照常产生，只是策略错了。挂上 rule 之后，"本地构造"与"Classify 分类"
// 走的是同一行，改表即改本地行为。
//
// status 传 0：本地构造的错误没有 HTTP 状态。Lookup 在 status==0 时短路返回
// 第一条 (attr, sub) 匹配的行，这正是想要的语义（例如 policy/denied 命中
// 401/403 那条，从而继承它的 Retry:none 与不计熔断）。
func New(attr Attribution, sub Subtype, op string) *Error {
	e := &Error{Attr: attr, Sub: sub, Op: op}
	if r, ok := Lookup(attr, sub, 0); ok {
		e.rule = &r
	}
	return e
}

// IsCapacity 是反压路径上最常问的问题：这次失败该不该退避？
// 注意它同时覆盖 draining —— 两者对调用方的动作一样（让路），语义不同而已。
func (e *Error) IsCapacity() bool {
	return e.Attr == AttrPolicy && (e.Sub == SubCapacity || e.Sub == SubQuota)
}

// CountsAsBreakerFailure 决定这次失败要不要记进熔断窗口。
// 它与 IsCapacity 不是反义：denied / budget / circuit 都"不计熔断"但也不是容量问题。
//
// 判据只有一个：映射表里那一行的 CountsAsFailure。
// 这里**不再**看 Attr 猜——旧实现对本地构造的错误走 `switch e.Attr`，
// 完全不查表，于是 "表是唯一事实源" 对它是假的：表里写着模型违约不计熔断，
// 本地构造的模型违约却按 Attr 兜底算成故障，两个答案并存且没有任何报警。
func (e *Error) CountsAsBreakerFailure() bool {
	if e.rule != nil {
		return e.rule.CountsAsFailure
	}
	if r, ok := Lookup(e.Attr, e.Sub, e.Status); ok {
		return r.CountsAsFailure
	}
	// 查不到 = 表里没有这个 (attr, sub)。保守选择"不踢节点"：
	// 在没有证据说这是上游故障时就摘掉 target，代价是健康节点被隔离，
	// 而隔离的代价远大于多等一个周期。New() 已经尽力挂上 rule，
	// 走到这里说明有人绕过了 New 直接构造 Error ——
	// internal/errs/local_coverage_test.go 的守卫会拦住这类组合进仓库。
	return false
}

// Retry 返回这条错误对应的重试策略。策略来源只有一个：映射表。
func (e *Error) Retry() RetryPolicy {
	if e.rule != nil {
		return e.rule.Retry
	}
	return retryFor(e.Attr, e.Sub, e.Status)
}

// Class 返回这条错误在映射表里的分类（trace 里给人看的那一列）。
func (e *Error) Class() Class {
	if e.rule != nil {
		return e.rule.Class
	}
	if r, ok := Lookup(e.Attr, e.Sub, e.Status); ok {
		return r.Class
	}
	return ClassTransport
}

// Key 是稳定的机读标识，用于 trace 与指标聚合（不含任何会变的文本）。
func (e *Error) Key() string {
	return e.Attr.String() + "/" + e.Sub.String()
}

func (e *Error) GoString() string { return fmt.Sprintf("errs.Error(%s)", e.Error()) }
