// Package budget 是三级预算账本：global → workflow → step。
//
// 设计约束（ARCH §5.4）：
//   - 一次预留要**同时**推进三个量（token in/out、micro-USD）并检查三级限额，
//     所以用临界区而不是 CAS——账本调用频率是"每次下游请求一次"，不在热路径上。
//   - TOCTOU 的闭合靠**顺序**：Reserve 在构造任何请求之前完成，拒绝即不发请求。
//     因此"必超限的 workflow 只发出 k-1 次请求"是账本的性质，不是 engine 的自觉。
//   - 所有累加走饱和加法：溢出不是"变成负数"，而是把 run 标成 budget_overflow 并终止。
package budget

import (
	"errors"
	"fmt"
	"math"
	"sync"

	"hermes/internal/std"
	"hermes/internal/std/rtbudget"
)

// ErrDoubleSettle：同一次预留被结算两次。它一定是调用方的 bug（重试路径漏了状态判断），
// 静默幂等会让"钱被算了两遍"永远查不出来。
var ErrDoubleSettle = errors.New("budget: reservation settled twice")

// ErrPriceOverflow：单价乘 token 溢出 int64。
var ErrPriceOverflow = errors.New("budget: price overflow")

// HaltOverflow 是 `halt_reason` 的取值之一（另一个是调用方自己设的 cancelled 等）。
const HaltOverflow = "budget_overflow"

// Level 是三级限额的层次名，也是 Refusal 里告诉调用方"哪一级拦的"。
type Level uint8

const (
	LevelGlobal Level = iota
	LevelWorkflow
	LevelStep
)

func (l Level) String() string {
	switch l {
	case LevelGlobal:
		return "global"
	case LevelWorkflow:
		return "workflow"
	case LevelStep:
		return "step"
	}
	return "unknown_level"
}

// Limits 是**上界**。字段 <= 0 表示该维度不限（0 也当作不限，避免"默认零值把一切拦住"）。
type Limits struct {
	TokensIn  int64
	TokensOut int64
	MicroUSD  int64
}

func (l Limits) unlimited() bool {
	return l.TokensIn <= 0 && l.TokensOut <= 0 && l.MicroUSD <= 0
}

// Limits3 是三级限额的组合。
type Limits3 struct {
	Global   Limits
	Workflow Limits
	Step     Limits
}

// Upper 是一次请求的**上界估计**（不是预测）。名字刻意叫 Upper：
// 账本只跟上界打交道，真实花费在 Settle 时回落。
type Upper struct {
	In       int64 // prompt tokens 上界
	Out      int64 // completion tokens 上界
	MicroUSD int64
}

func (u Upper) zero() bool { return u.In == 0 && u.Out == 0 && u.MicroUSD == 0 }

// Scope 定位一次预留属于哪个 workflow / 哪个 step。
type Scope struct {
	Workflow string
	Step     string
}

// Refusal 是拒绝预留的证据。它必须带上"哪一级、要多少、还剩多少"，
// 否则用户只会看到"钱不够"而不知道该调哪个预算。
type Refusal struct {
	Scope      Scope
	Level      Level
	Requested  Upper
	Remaining  Upper
	HaltReason string // 非空表示账本已进入终止态
}

func (r *Refusal) Error() string {
	return fmt.Sprintf("budget refused at %s: requested {in:%d,out:%d,usd:%d} remaining {in:%d,out:%d,usd:%d}",
		r.Level, r.Requested.In, r.Requested.Out, r.Requested.MicroUSD,
		r.Remaining.In, r.Remaining.Out, r.Remaining.MicroUSD)
}

type state uint8

const (
	stateReserved state = iota
	stateSettled
)

// Reservation 是一张欠条。Settle 之前它一直挂在 open 里；
// run 结束时 open 非空即 invariant violation（有请求没结算 = 账本漏账）。
type Reservation struct {
	ledger *Ledger
	scope  Scope
	up     Upper
	state  state
}

// Ledger 是一次 run 的账本。并发安全（内部一把锁）。
type Ledger struct {
	mu     sync.Mutex
	limits Limits3

	global    Upper
	workflows map[string]*Upper
	steps     map[string]*Upper

	open map[*Reservation]struct{}
	halt string
}

// New 建账本。nil 的 limits 视为全不限（测试与"不设预算"的场景）。
func New(l Limits3) *Ledger {
	return &Ledger{
		limits:    l,
		workflows: map[string]*Upper{},
		steps:     map[string]*Upper{},
		open:      map[*Reservation]struct{}{},
	}
}

// HaltReason 返回终止原因（""=未终止）。
func (l *Ledger) HaltReason() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.halt
}

// Open 返回未结算的预留数。run 结束时应为 0。
func (l *Ledger) Open() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.open)
}

// Spent 返回当前已占用（含未结算）的三项合计。
func (l *Ledger) Spent() Upper {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.global
}

// Reserve 预留一次请求的上界。成功即占用三级额度；失败即**什么都没占用**。
// 检查与占用在同一临界区内，这就是 TOCTOU 闭合点。
func (l *Ledger) Reserve(scope Scope, up Upper) (*Reservation, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.halt != "" {
		return nil, &Refusal{Scope: scope, Level: LevelGlobal, Requested: up, HaltReason: l.halt}
	}
	if up.zero() {
		// 零上界也要留一张欠条：否则 Settle 无处落账，调用方会漏一次结算。
		r := &Reservation{ledger: l, scope: scope, up: up}
		l.open[r] = struct{}{}
		return r, nil
	}

	wf := l.workflows[scope.Workflow]
	if wf == nil {
		wf = &Upper{}
		l.workflows[scope.Workflow] = wf
	}
	st := l.steps[scope.Step]
	if st == nil {
		st = &Upper{}
		l.steps[scope.Step] = st
	}

	// 固定顺序 global → workflow → step，从不乱序，因此没有死锁也没有"先花后查"。
	chain := []struct {
		level Level
		used  *Upper
		lim   Limits
	}{
		{LevelGlobal, &l.global, l.limits.Global},
		{LevelWorkflow, wf, l.limits.Workflow},
		{LevelStep, st, l.limits.Step},
	}
	for _, c := range chain {
		if exceeds(c.used, up, c.lim) {
			return nil, &Refusal{
				Scope:     scope,
				Level:     c.level,
				Requested: up,
				Remaining: remaining(c.used, c.lim),
			}
		}
	}
	for _, c := range chain {
		charge(c.used, up, l)
	}

	r := &Reservation{ledger: l, scope: scope, up: up}
	l.open[r] = struct{}{}
	return r, nil
}

// Settle 用**实际**花费结算。实际可以小于上界（退款），也可以略大于上界
// （例如上游报的 usage 比我们估的高）——后者照实记账，但会重新检查限额。
func (r *Reservation) Settle(actual Upper) error {
	l := r.ledger
	l.mu.Lock()
	defer l.mu.Unlock()

	if r.state == stateSettled {
		return ErrDoubleSettle
	}
	r.state = stateSettled
	delete(l.open, r)

	delta := Upper{
		In:       actual.In - r.up.In,
		Out:      actual.Out - r.up.Out,
		MicroUSD: actual.MicroUSD - r.up.MicroUSD,
	}
	if delta.zero() {
		return nil
	}

	wf := l.workflows[r.scope.Workflow]
	st := l.steps[r.scope.Step]
	for _, u := range []*Upper{&l.global, wf, st} {
		if u == nil {
			continue
		}
		applyDelta(u, delta, l)
	}
	return nil
}

// exceeds 判断"再加 up 是否越过 lim"。<=0 的维度不限。
//
// App 层可选（g*/G*）：选 variant=c 时改走纯 C 实现（rtbudget.Exceeded），
// 不拖 Go 版 gruntime 的逻辑；默认 variant=go 走下方与 C 版逐字一致的 Go 实现。
// 两条路径语义相同，只是实现语言不同，因此默认行为与今天逐字节一致。
func exceeds(used *Upper, up Upper, lim Limits) bool {
	if std.SelectedVariant() == std.VariantC {
		return rtbudget.Exceeded(used.In, used.Out, used.MicroUSD,
			up.In, up.Out, up.MicroUSD,
			lim.TokensIn, lim.TokensOut, lim.MicroUSD)
	}
	if lim.TokensIn > 0 && used.In+up.In > lim.TokensIn {
		return true
	}
	if lim.TokensOut > 0 && used.Out+up.Out > lim.TokensOut {
		return true
	}
	if lim.MicroUSD > 0 && used.MicroUSD+up.MicroUSD > lim.MicroUSD {
		return true
	}
	return false
}

func remaining(used *Upper, lim Limits) Upper {
	r := Upper{}
	if lim.TokensIn > 0 {
		r.In = max64(0, lim.TokensIn-used.In)
	}
	if lim.TokensOut > 0 {
		r.Out = max64(0, lim.TokensOut-used.Out)
	}
	if lim.MicroUSD > 0 {
		r.MicroUSD = max64(0, lim.MicroUSD-used.MicroUSD)
	}
	return r
}

func charge(used *Upper, up Upper, l *Ledger) {
	used.In = addSat(used.In, up.In, l)
	used.Out = addSat(used.Out, up.Out, l)
	used.MicroUSD = addSat(used.MicroUSD, up.MicroUSD, l)
}

// applyDelta 应用可正可负的差额。负数（退款）不会把用量降到 0 以下。
func applyDelta(used *Upper, d Upper, l *Ledger) {
	used.In = addDelta(used.In, d.In)
	used.Out = addDelta(used.Out, d.Out)
	used.MicroUSD = addDelta(used.MicroUSD, d.MicroUSD)
	_ = l
}

func addDelta(a, d int64) int64 {
	if d >= 0 {
		return satAdd(a, d)
	}
	if a < -d { // a + d < 0
		return 0
	}
	return a + d
}

// addSat 是唯一允许的累加入口：溢出即饱和，并把 run 标记为终止。
// 饱和值本身不是错误状态，但出现饱和意味着输入非法，必须响亮。
func addSat(a, b int64, l *Ledger) int64 {
	s, over := satAddOK(a, b)
	if over {
		l.halt = HaltOverflow
	}
	return s
}

func satAdd(a, b int64) int64 {
	s, _ := satAddOK(a, b)
	return s
}

func satAddOK(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64, true
	}
	if b < 0 && a < math.MinInt64-b {
		return math.MinInt64, true
	}
	return a + b, false
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
