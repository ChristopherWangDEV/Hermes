// Package resilience 是三个反压/韧性机制的唯一实现点：
// 熔断器（breaker）、逐目标拥塞窗口（aimd）、退避与重试判定（retry）。
//
// 三者共享同一条事实源：`internal/errs` 的映射表。这里**不允许**再写一份
// "哪些失败算失败"的判断——熔断器只问 `err.CountsAsBreakerFailure()`。
package resilience

import (
	"sync"
	"time"

	"hermes/internal/errs"
)

// State 是熔断器状态。
type State uint8

const (
	StateClosed State = iota
	StateOpen
	StateHalfOpen
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half_open"
	}
	return "unknown_state"
}

// BreakerConfig 有合理默认值：零值即"5 次连续失败、冷却 5s、上限 5min"。
type BreakerConfig struct {
	Threshold   int           // 连续失败多少次开闸
	Cooldown    time.Duration // 初始冷却
	MaxCooldown time.Duration // 冷却倍增的上限
}

func (c BreakerConfig) withDefaults() BreakerConfig {
	if c.Threshold <= 0 {
		c.Threshold = 5
	}
	if c.Cooldown <= 0 {
		c.Cooldown = 5 * time.Second
	}
	if c.MaxCooldown <= 0 {
		c.MaxCooldown = 5 * time.Minute
	}
	if c.MaxCooldown < c.Cooldown {
		c.MaxCooldown = c.Cooldown
	}
	return c
}

// Breaker 是 per-target 的熔断器。并发安全。
//
// 两条不能妥协的性质：
//   - 开闸期间 Allow 返回 false，调用方**不发请求**——这是"第 6 次调用上游计数不增长"的实现方式，
//     不是靠调用方自觉。
//   - 半开只放**一个**探测：放多个等于在还没恢复时把并发全压回去。
type Breaker struct {
	mu sync.Mutex

	cfg BreakerConfig

	state       State
	consecutive int
	deferred    int // 被容量/配额拒绝的次数：它们**不是**失败，但必须可观测
	openedAt    time.Time
	cooldown    time.Duration
	probing     bool
}

// NewBreaker 构造熔断器。
func NewBreaker(cfg BreakerConfig) *Breaker {
	c := cfg.withDefaults()
	return &Breaker{cfg: c, state: StateClosed, cooldown: c.Cooldown}
}

// Allow 询问"现在能不能发"。返回 false 时**禁止**发起任何上游请求。
func (b *Breaker) Allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateClosed:
		return true
	case StateOpen:
		if now.Sub(b.openedAt) < b.cooldown {
			return false
		}
		// 冷却到期：进入半开，只放这一个探测。
		b.state = StateHalfOpen
		b.probing = true
		return true
	case StateHalfOpen:
		// 探测还在飞：谁也别再进。
		return !b.probing
	}
	return false
}

// Report 上报一次结果。err == nil 表示成功。
// 是否计入失败**完全**由 errs 的映射表决定（CountsAsBreakerFailure）。
func (b *Breaker) Report(now time.Time, err *errs.Error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if err == nil {
		b.consecutive = 0
		if b.state == StateHalfOpen {
			b.state = StateClosed
			b.probing = false
			b.cooldown = b.cfg.Cooldown // 恢复后冷却回到初值
		}
		return
	}

	if !err.CountsAsBreakerFailure() {
		// 容量/配额/审批/预算：不是上游坏了。记一次"被推迟"，绝不动 consecutive。
		// 这一行就是 R-CB-05 的全部实现——把它挪进失败分支，集群一忙就会踢掉健康节点。
		b.deferred++
		return
	}

	switch b.state {
	case StateHalfOpen:
		b.state = StateOpen
		b.probing = false
		b.openedAt = now
		b.cooldown = nextCooldown(b.cooldown, b.cfg.MaxCooldown)
	case StateClosed:
		b.consecutive++
		if b.consecutive >= b.cfg.Threshold {
			b.state = StateOpen
			b.openedAt = now
		}
	case StateOpen:
		// 开闸期间本来就该拒绝；这里只更新冷却窗口，不让 consecutive 无限涨。
		b.openedAt = now
	}
}

// nextCooldown 按倍增推进冷却，但不越过上限。
func nextCooldown(cur, max time.Duration) time.Duration {
	next := cur * 2
	if next < cur || next > max { // 兼管溢出
		return max
	}
	return next
}

// State 返回当前状态（会顺带把过期的 open 翻成 half_open 的**观察**，
// 但不占探测名额——只有 Allow 才占）。
func (b *Breaker) State(now time.Time) State {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == StateOpen && now.Sub(b.openedAt) >= b.cooldown {
		return StateHalfOpen
	}
	return b.state
}

// Consecutive 是当前连续失败数。
func (b *Breaker) Consecutive() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.consecutive
}

// Deferred 是被"不计失败"的拒绝推迟的次数（容量/配额）。
func (b *Breaker) Deferred() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.deferred
}

// Cooldown 返回当前冷却时长（用于 trace 与 status 输出）。
func (b *Breaker) Cooldown() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cooldown
}

// Reset 人工复位（`hermes reset-circuit`）。返回要写进 trace 的事件名。
func (b *Breaker) Reset() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state = StateClosed
	b.consecutive = 0
	b.probing = false
	b.cooldown = b.cfg.Cooldown
	return []string{"circuit.reset"}
}
