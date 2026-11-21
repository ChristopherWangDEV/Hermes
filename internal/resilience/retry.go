package resilience

import (
	"math/rand"
	"time"

	"hermes/internal/errs"
)

// Backoff 生成退避时长。**全抖动**（full jitter）：delay = uniform(0, cap_n)。
//
// 为什么不用固定指数退避：多个客户端会在同一时刻撞同一个上游，固定退避等于
// 让它们永远同步重试（惊群）。全抖动把重试摊平，代价是"单次延迟不可预测"——
// 这正是我们要的：可预测的是**上界**，不是每一次的落点。
type Backoff struct {
	Base time.Duration // 首次退避的上界
	Cap  time.Duration // 上界的天花板
	rnd  *rand.Rand
}

// NewBackoff 构造。seed 相同 ⇒ 序列逐项相同（测试与 replay 都靠这条）。
func NewBackoff(base, cap time.Duration, seed int64) *Backoff {
	if base <= 0 {
		base = 100 * time.Millisecond
	}
	if cap <= 0 || cap < base {
		cap = base
	}
	return &Backoff{Base: base, Cap: cap, rnd: rand.New(rand.NewSource(seed))}
}

// CapFor 返回第 attempt 次重试的**上界**（attempt 从 0 开始）。
// 这是唯一"单调不减且 ≤ Cap"的序列——全抖动后的实际值不单调，别拿它做断言。
func (b *Backoff) CapFor(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	d := b.Base
	for i := 0; i < attempt; i++ {
		if d >= b.Cap/2 {
			return b.Cap
		}
		d *= 2
	}
	if d > b.Cap {
		return b.Cap
	}
	return d
}

// Delay 返回本次实际等待时长：先全抖动，再用上游给的 Retry-After 抬下界。
//
// Retry-After 是**下界**不是建议值：早于它重试，上游会再拒一次，等于白花一次往返。
func (b *Backoff) Delay(attempt int, retryAfter time.Duration) time.Duration {
	upper := b.CapFor(attempt)
	var d time.Duration
	if upper > 0 {
		d = time.Duration(b.rnd.Int63n(int64(upper) + 1))
	}
	if retryAfter > d {
		d = retryAfter
	}
	return d
}

// Decision 是一次重试判定的结果。把它写进 trace 的 attempt 事件，
// 事后才能回答"为什么这次没重试"。
type Decision struct {
	Retry      bool
	Reason     string
	MaxAttempt int
}

// ShouldRetry 按映射表的重试策略决定要不要再来一次。
//
// idempotent 是**调用方声明**的幂等性（来自 IR 的 idem.mode）：
// 非幂等调用即使遇到 5xx 也只允许一次——重试一个已经产生了副作用的 POST，
// 是在制造重复订单，不是在提高可用性。
func ShouldRetry(err *errs.Error, attempt, maxAttempts int, idempotent bool) Decision {
	if err == nil {
		return Decision{Retry: false, Reason: "no_error", MaxAttempt: maxAttempts}
	}
	if attempt+1 >= maxAttempts {
		return Decision{Retry: false, Reason: "max_attempts", MaxAttempt: maxAttempts}
	}
	switch err.Retry() {
	case errs.RetryNone:
		return Decision{Retry: false, Reason: "policy_none", MaxAttempt: maxAttempts}
	case errs.RetryOnce:
		if attempt == 0 {
			return Decision{Retry: true, Reason: "once", MaxAttempt: maxAttempts}
		}
		return Decision{Retry: false, Reason: "once_used", MaxAttempt: maxAttempts}
	case errs.RetryIdempotentOnly:
		if !idempotent {
			return Decision{Retry: false, Reason: "not_idempotent", MaxAttempt: maxAttempts}
		}
		return Decision{Retry: true, Reason: "idempotent", MaxAttempt: maxAttempts}
	case errs.RetryRespectRetryAfter:
		return Decision{Retry: true, Reason: "respect_retry_after", MaxAttempt: maxAttempts}
	}
	return Decision{Retry: false, Reason: "unknown_policy", MaxAttempt: maxAttempts}
}
