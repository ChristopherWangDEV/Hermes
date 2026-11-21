package resilience

import (
	"testing"
	"time"

	"hermes/internal/errs"
)

func capacityErr() *errs.Error  { return errs.New(errs.AttrPolicy, errs.SubCapacity, "provider.Call") }
func transportErr() *errs.Error { return errs.New(errs.AttrTransport, errs.SubNone, "provider.Call") }

// ① 连续 5 次失败开闸，且第 6 次 Allow 直接拒绝（= 上游请求计数不再增长）。
func TestBreakerOpensAfterThreshold(t *testing.T) {
	b := NewBreaker(BreakerConfig{Threshold: 5, Cooldown: time.Minute})
	now := time.Unix(0, 0)
	for i := 0; i < 5; i++ {
		if !b.Allow(now) {
			t.Fatalf("第 %d 次不该被拒", i+1)
		}
		b.Report(now, transportErr())
	}
	if b.State(now) != StateOpen {
		t.Fatalf("5 次连续失败后应为 open，got %s", b.State(now))
	}
	if b.Allow(now) {
		t.Fatal("开闸期间必须拒绝——这就是'上游请求数不再增长'的实现")
	}
}

// ② 冷却到期只放**一个**探测；探测在飞时第二个请求被拒。
func TestHalfOpenAllowsExactlyOneProbe(t *testing.T) {
	b := NewBreaker(BreakerConfig{Threshold: 1, Cooldown: time.Second})
	now := time.Unix(0, 0)
	b.Report(now, transportErr())

	now = now.Add(2 * time.Second)
	if !b.Allow(now) {
		t.Fatal("冷却到期应放行一个探测")
	}
	if b.Allow(now) {
		t.Fatal("探测已在飞，第二个必须被拒（否则等于没恢复就全量打回去）")
	}
}

// ③ 探测成功 → closed 且连续失败清零。
func TestProbeSuccessCloses(t *testing.T) {
	b := NewBreaker(BreakerConfig{Threshold: 1, Cooldown: time.Second})
	now := time.Unix(0, 0)
	b.Report(now, transportErr())
	now = now.Add(2 * time.Second)
	if !b.Allow(now) {
		t.Fatal("应放行探测")
	}
	b.Report(now, nil)
	if b.State(now) != StateClosed {
		t.Fatalf("探测成功后应为 closed，got %s", b.State(now))
	}
	if b.Consecutive() != 0 {
		t.Fatalf("consecutive 应清零，got %d", b.Consecutive())
	}
}

// ④ 探测失败 → 回到 open，冷却倍增且不越上限。
func TestProbeFailureDoublesCooldown(t *testing.T) {
	b := NewBreaker(BreakerConfig{Threshold: 1, Cooldown: time.Second, MaxCooldown: 4 * time.Second})
	now := time.Unix(0, 0)
	b.Report(now, transportErr())
	if got := b.Cooldown(); got != time.Second {
		t.Fatalf("初始冷却 1s，got %v", got)
	}
	for i := 0; i < 4; i++ {
		now = now.Add(b.Cooldown() + time.Second)
		if !b.Allow(now) {
			t.Fatalf("第 %d 轮应放行探测", i+1)
		}
		b.Report(now, transportErr())
	}
	if got := b.Cooldown(); got != 4*time.Second {
		t.Fatalf("冷却应倍增到上限 4s，got %v", got)
	}
	if b.State(now) != StateOpen {
		t.Fatalf("探测失败后应为 open，got %s", b.State(now))
	}
	if b.Cooldown() > 4*time.Second {
		t.Fatal("冷却越过上限")
	}
}

// ⑤ 注入 20 次 429（容量拒绝）：consecutive 必须为 0，deferred 必须为 20。
// 这是 R-CB-05 的全部意义：集群忙不能把健康节点踢掉。
func TestCapacityRejectionsNeverCountAsFailure(t *testing.T) {
	b := NewBreaker(BreakerConfig{Threshold: 5})
	now := time.Unix(0, 0)
	for i := 0; i < 20; i++ {
		if !b.Allow(now) {
			t.Fatalf("容量拒绝不该导致开闸（第 %d 次）", i+1)
		}
		b.Report(now, capacityErr())
	}
	if b.Consecutive() != 0 {
		t.Fatalf("consecutive 必须为 0，got %d", b.Consecutive())
	}
	if b.Deferred() != 20 {
		t.Fatalf("deferred 必须为 20，got %d", b.Deferred())
	}
	if b.State(now) != StateClosed {
		t.Fatalf("应为 closed，got %s", b.State(now))
	}
}

// ⑥ 人工复位后为 closed，并产出可写进 trace 的事件。
func TestReset(t *testing.T) {
	b := NewBreaker(BreakerConfig{Threshold: 1})
	now := time.Unix(0, 0)
	b.Report(now, transportErr())
	if b.State(now) != StateOpen {
		t.Fatal("前置条件失败")
	}
	events := b.Reset()
	if b.State(now) != StateClosed {
		t.Fatalf("复位后应为 closed，got %s", b.State(now))
	}
	if len(events) != 1 || events[0] != "circuit.reset" {
		t.Fatalf("应产出 circuit.reset 事件，got %v", events)
	}
}

// ⑦ 拥塞按 ⌈n/2⌉ 乘法下降，恒 ≥ 1。
func TestAIMDHalves(t *testing.T) {
	a := NewAIMD(8, 64)
	seq := []int{}
	for i := 0; i < 6; i++ {
		n, _ := a.OnCongestion("t")
		seq = append(seq, n)
	}
	want := []int{4, 2, 1, 1, 1, 1}
	for i := range want {
		if seq[i] != want[i] {
			t.Fatalf("减半序列错：want %v, got %v", want, seq)
		}
	}
}

// ⑧ 增速是"每个拥塞避免窗口 +1"，不是逐请求 +1（D-006）。
func TestAIMDGrowsPerWindow(t *testing.T) {
	a := NewAIMD(4, 64)
	// 窗口是 4：前 3 次成功都不该动，第 4 次凑满一个窗口才 +1。
	for i := 1; i <= 3; i++ {
		if _, ch := a.OnSuccess("t"); ch {
			t.Fatalf("第 %d 次成功就 +1 了（窗口是 4，应凑满 4 次）", i)
		}
	}
	if n, ch := a.OnSuccess("t"); !ch || n != 5 {
		t.Fatalf("第 4 次成功应把窗口推到 5，got %d changed=%v", n, ch)
	}
	// 从 50 起，100 次成功内变化必须 ≤ 2（D-006 的振荡上界）
	b := NewAIMD(50, 1024)
	ch := 0
	for i := 0; i < 100; i++ {
		if _, ok := b.OnSuccess("t2"); ok {
			ch++
		}
	}
	if ch > 2 {
		t.Fatalf("100 次成功内窗口变化 %d 次，超过 2（会形成锯齿）", ch)
	}
}

// ⑨ 隔离性：A 反复拥塞时 B 的窗口序列必须逐项不变。
func TestAIMDIsolation(t *testing.T) {
	a := NewAIMD(8, 64)
	for i := 0; i < 8; i++ {
		a.OnSuccess("modelB")
	}
	baseB := a.Window("modelB")

	for i := 0; i < 5; i++ {
		a.Window("modelA")
		a.OnCongestion("modelA")
	}
	if got := a.Window("modelB"); got != baseB {
		t.Fatalf("A 的拥塞污染了 B：%d → %d", baseB, got)
	}
	if got := a.Window("modelA"); got != 1 {
		t.Fatalf("A 应降到 1，got %d", got)
	}
}

// ⑩ 有界槽满了立即失败，不排队。
func TestBoundedSlotsFailFast(t *testing.T) {
	s := NewBoundedSlots(2)
	r1, ok1 := s.Acquire()
	r2, ok2 := s.Acquire()
	if !ok1 || !ok2 {
		t.Fatal("前两个应成功")
	}
	if _, ok := s.Acquire(); ok {
		t.Fatal("槽满必须立即失败，不许排队等待")
	}
	r1()
	if _, ok := s.Acquire(); !ok {
		t.Fatal("释放后应能再次占用")
	}
	r2()
	r2() // 重复释放必须是 no-op，不能把 used 减成负数
	if s.Used() != 1 {
		t.Fatalf("used 应为 1，got %d", s.Used())
	}
}

// ⑪ 同种子两次运行退避序列逐项相等；上界单调不减且 ≤ Cap。
func TestBackoffDeterministicAndCapped(t *testing.T) {
	b1 := NewBackoff(100*time.Millisecond, time.Second, 42)
	b2 := NewBackoff(100*time.Millisecond, time.Second, 42)
	b3 := NewBackoff(100*time.Millisecond, time.Second, 43)

	differsFromOtherSeed := false
	for i := 0; i < 20; i++ {
		d1 := b1.Delay(i, 0)
		d2 := b2.Delay(i, 0)
		d3 := b3.Delay(i, 0)
		if d1 != d2 {
			t.Fatalf("同种子第 %d 次不等：%v vs %v", i, d1, d2)
		}
		if d1 != d3 {
			differsFromOtherSeed = true
		}
	}
	if !differsFromOtherSeed {
		t.Fatal("不同种子序列完全一样，说明种子没生效")
	}

	prev := time.Duration(0)
	for i := 0; i < 10; i++ {
		c := b1.CapFor(i)
		if c < prev {
			t.Fatalf("上界应单调不减：第 %d 次 %v < 前一次 %v", i, c, prev)
		}
		if c > time.Second {
			t.Fatalf("上界越过 Cap：%v", c)
		}
		prev = c
	}
}

// ⑫ Retry-After 是下界：抖动值低于它时必须抬到它。
func TestBackoffRespectsRetryAfterFloor(t *testing.T) {
	b := NewBackoff(100*time.Millisecond, time.Second, 7)
	for i := 0; i < 30; i++ {
		d := b.Delay(0, 3*time.Second)
		if d < 3*time.Second {
			t.Fatalf("Retry-After 是下界，got %v", d)
		}
	}
}

// ⑬ 非幂等调用即使遇到可重试的 5xx，也只允许一次（R-VD-01）。
func TestShouldRetryNonIdempotent(t *testing.T) {
	e := transportErr()
	if d := ShouldRetry(e, 0, 3, false); d.Retry {
		t.Fatalf("非幂等调用不许重试，got %+v", d)
	}
	if d := ShouldRetry(e, 0, 3, true); !d.Retry {
		t.Fatalf("幂等调用应允许重试，got %+v", d)
	}
	// 模型违约只允许一次，用完就停
	m := errs.New(errs.AttrModelContractViolation, errs.SubNone, "provider.Call")
	if d := ShouldRetry(m, 0, 3, true); !d.Retry {
		t.Fatal("模型违约应允许重试一次")
	}
	if d := ShouldRetry(m, 1, 3, true); d.Retry {
		t.Fatal("模型违约只允许一次")
	}
	// 预算拒绝不重试
	bu := errs.New(errs.AttrBudget, errs.SubNone, "ledger")
	if d := ShouldRetry(bu, 0, 3, true); d.Retry {
		t.Fatal("预算拒绝重试没有意义")
	}
}
