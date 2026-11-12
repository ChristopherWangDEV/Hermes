package resilience

import "sync"

// AIMD 是 per-target 的并发窗口。target = "(provider, model, endpoint_kind)"。
//
// 为什么必须 per-target（ARCH §5.5）：`/health` 的队列深度是**跨模型聚合**的。
// 用聚合值去缩窗口，会让 A 模型的拥塞压掉 B 模型的并发——A 生病、B 吃药。
// 因此本类型没有任何"全局窗口"，只有 map；隔离性由数据结构本身保证。
type AIMD struct {
	mu sync.Mutex

	initial int
	ceil    int
	targets map[string]*window
}

type window struct {
	n         int // 当前窗口
	successes int // 本窗口内已完成的成功数
}

// NewAIMD 构造。initial <= 0 取 1；ceil <= 0 取 initial。
func NewAIMD(initial, ceil int) *AIMD {
	if initial <= 0 {
		initial = 1
	}
	if ceil <= 0 || ceil < initial {
		ceil = initial
	}
	return &AIMD{initial: initial, ceil: ceil, targets: map[string]*window{}}
}

func (a *AIMD) get(key string) *window {
	w := a.targets[key]
	if w == nil {
		w = &window{n: a.initial}
		a.targets[key] = w
	}
	return w
}

// Window 返回某 target 的当前窗口（未出现过则返回 initial，且**不**创建条目）。
func (a *AIMD) Window(key string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if w := a.targets[key]; w != nil {
		return w.n
	}
	return a.initial
}

// OnSuccess 记一次成功。返回 (新窗口, 是否变化)。
//
// 增速是 D-006 的"每个拥塞避免窗口 +1"：凑满 **当前窗口值** 个成功才 +1。
// 逐请求 +1 会在拥塞点造成 4↔8 的持续锯齿，把上游打成脉冲负载。
func (a *AIMD) OnSuccess(key string) (int, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	w := a.get(key)
	w.successes++
	if w.successes < w.n || w.n >= a.ceil {
		return w.n, false
	}
	w.successes = 0
	w.n++
	return w.n, true
}

// OnCongestion 记一次拥塞。返回值新窗口：`n → ⌈n/2⌉`，且恒 ≥ 1。
// 乘法下降是硬要求：宁可掉一半，也不要逐请求回落（那等于没有减速）。
func (a *AIMD) OnCongestion(key string) (int, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	w := a.get(key)
	next := (w.n + 1) / 2 // ⌈n/2⌉
	if next < 1 {
		next = 1
	}
	changed := next != w.n
	w.n = next
	w.successes = 0
	return w.n, changed
}

// Snapshot 返回全部 target 的窗口副本（写进 trace 用）。顺序由调用方排序。
func (a *AIMD) Snapshot() map[string]int {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]int, len(a.targets))
	for k, w := range a.targets {
		out[k] = w.n
	}
	return out
}

// BoundedSlots 是有界并发槽：满了就**立即失败**，绝不排队等待。
//
// 为什么 fail-fast（R-BP-03）：等待会把"上游已经过载"变成"我们自己也在积压"，
// 两头一起崩。并且在墙钟上无法区分"排队久"与"上游慢"，trace 就失去归因能力。
type BoundedSlots struct {
	mu   sync.Mutex
	cap  int
	used int
}

// NewBoundedSlots 构造。cap <= 0 视为 1。
func NewBoundedSlots(capacity int) *BoundedSlots {
	if capacity <= 0 {
		capacity = 1
	}
	return &BoundedSlots{cap: capacity}
}

// Acquire 立即尝试占一个槽。第二个返回值 false 表示"满了"，调用方必须放弃本次请求
// （写 admission 类失败，而不是 upstream_wait 的 span）。
func (s *BoundedSlots) Acquire() (release func(), ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.used >= s.cap {
		return nil, false
	}
	s.used++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			s.used--
			s.mu.Unlock()
		})
	}, true
}

// Used 返回已占用槽数。
func (s *BoundedSlots) Used() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.used
}
