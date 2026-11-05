package budget

import (
	"errors"
	"math"
	"sync"
	"testing"
)

// ① 三级各自都能拦住，且 Refusal 带 scope/requested/remaining。
func TestThreeLevelsRefuse(t *testing.T) {
	cases := []struct {
		name  string
		lim   Limits3
		first Upper
		next  Upper
		level Level
	}{
		{
			name:  "global 拦",
			lim:   Limits3{Global: Limits{MicroUSD: 100}},
			first: Upper{MicroUSD: 60},
			next:  Upper{MicroUSD: 60},
			level: LevelGlobal,
		},
		{
			name:  "workflow 拦",
			lim:   Limits3{Workflow: Limits{TokensIn: 100}},
			first: Upper{In: 60},
			next:  Upper{In: 60},
			level: LevelWorkflow,
		},
		{
			name:  "step 拦",
			lim:   Limits3{Step: Limits{TokensOut: 100}},
			first: Upper{Out: 60},
			next:  Upper{Out: 60},
			level: LevelStep,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := New(c.lim)
			sc := Scope{Workflow: "wf", Step: "s1"}
			if _, err := l.Reserve(sc, c.first); err != nil {
				t.Fatalf("第一次预留不该失败：%v", err)
			}
			_, err := l.Reserve(sc, c.next)
			if err == nil {
				t.Fatal("第二次预留必须被拦")
			}
			var ref *Refusal
			if !errors.As(err, &ref) {
				t.Fatalf("必须是 *Refusal，got %T", err)
			}
			if ref.Level != c.level {
				t.Fatalf("拦在 %s，期望 %s", ref.Level, c.level)
			}
			if ref.Scope != sc {
				t.Fatalf("Refusal 丢了 scope：%+v", ref.Scope)
			}
			if ref.Requested != c.next {
				t.Fatalf("Refusal 的 requested 不对：%+v", ref.Requested)
			}
			// 被拒绝的那次**什么都没占用**：用量仍是第一次的量。
			if got := l.Spent(); got != c.first && got.MicroUSD != c.first.MicroUSD {
				t.Fatalf("拒绝路径污染了用量：%+v", got)
			}
		})
	}
}

// ② 并发：100 goroutine 混合 reserve/settle 后 spent ≤ limit 且 open == 0。
// 这条是账本唯一不能妥协的性质——钱不能超，账不能漏。
func TestConcurrentReserveSettle(t *testing.T) {
	const n = 100
	lim := Limits3{Global: Limits{MicroUSD: 1_000_000}}
	l := New(lim)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var refused int
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sc := Scope{Workflow: "wf", Step: "s"}
			r, err := l.Reserve(sc, Upper{MicroUSD: 20_000})
			if err != nil {
				mu.Lock()
				refused++
				mu.Unlock()
				return
			}
			// 实际花得比上界少一半（退款路径）
			if err := r.Settle(Upper{MicroUSD: 10_000}); err != nil {
				t.Errorf("Settle 失败：%v", err)
			}
		}(i)
	}
	wg.Wait()

	if got, want := l.Open(), 0; got != want {
		t.Fatalf("open 必须为 0，got %d（有预留没结算）", got)
	}
	if got := l.Spent().MicroUSD; got > lim.Global.MicroUSD {
		t.Fatalf("spent 越界：%d > %d", got, lim.Global.MicroUSD)
	}
	if refused == 0 {
		t.Fatal("100×20k 超过 1M 了，应当有被拒的（否则限额没生效）")
	}
}

// ③ 双次 Settle 必须报错。
func TestDoubleSettle(t *testing.T) {
	l := New(Limits3{})
	r, err := l.Reserve(Scope{Workflow: "wf", Step: "s"}, Upper{In: 10})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Settle(Upper{In: 10}); err != nil {
		t.Fatal(err)
	}
	if err := r.Settle(Upper{In: 10}); !errors.Is(err, ErrDoubleSettle) {
		t.Fatalf("必须返回 ErrDoubleSettle，got %v", err)
	}
}

// ④ 饱和：在 MaxInt64 附近置 halt_reason=budget_overflow，且不变成负数。
func TestSaturationSetsHalt(t *testing.T) {
	l := New(Limits3{})
	sc := Scope{Workflow: "wf", Step: "s"}
	if _, err := l.Reserve(sc, Upper{In: math.MaxInt64 - 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Reserve(sc, Upper{In: 10}); err != nil {
		t.Fatal(err)
	}
	if got := l.HaltReason(); got != HaltOverflow {
		t.Fatalf("halt_reason 应为 %q，got %q", HaltOverflow, got)
	}
	if got := l.Spent().In; got < 0 {
		t.Fatalf("饱和后不许变负：%d", got)
	}
	if got := l.Spent().In; got != math.MaxInt64 {
		t.Fatalf("应饱和到 MaxInt64，got %d", got)
	}
	// 进入终止态后一切预留都被拒
	if _, err := l.Reserve(sc, Upper{In: 1}); err == nil {
		t.Fatal("终止态必须拒绝一切预留")
	}
}

// ⑤ pricing.source=unknown 时，金额不参与预留与限额。
func TestUnknownPriceExcludesMoney(t *testing.T) {
	p := Price{InPerMTok: 15_000_000, OutPerMTok: 60_000_000, Source: SourceUnknown}
	u, err := UpperFor(p, 1_000, 500)
	if err != nil {
		t.Fatal(err)
	}
	if u.MicroUSD != 0 {
		t.Fatalf("unknown 单价必须把金额置 0，got %d", u.MicroUSD)
	}
	// 有单价时才算钱：15 USD/Mtok × 1000 tok = 15,000 micro-USD
	q := Price{InPerMTok: 15_000_000, Source: SourceDeclared}
	u2, err := UpperFor(q, 1_000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if u2.MicroUSD != 15_000 {
		t.Fatalf("金额算错：want 15000, got %d", u2.MicroUSD)
	}
}

// ⑥ 结算小于上界要退款；大于上界要补记（真实 usage 高于估计是常态）。
func TestSettleRefundsAndTops(t *testing.T) {
	l := New(Limits3{})
	sc := Scope{Workflow: "wf", Step: "s"}

	r, _ := l.Reserve(sc, Upper{In: 100, Out: 100})
	if err := r.Settle(Upper{In: 40, Out: 40}); err != nil {
		t.Fatal(err)
	}
	if got := l.Spent().In; got != 40 {
		t.Fatalf("退款失败：want 40, got %d", got)
	}

	r2, _ := l.Reserve(sc, Upper{In: 10})
	if err := r2.Settle(Upper{In: 30}); err != nil {
		t.Fatal(err)
	}
	if got := l.Spent().In; got != 70 {
		t.Fatalf("补记失败：want 70, got %d", got)
	}
}

// 价格乘法溢出必须报错，不能悄悄回绕。
func TestPriceOverflow(t *testing.T) {
	p := Price{InPerMTok: math.MaxInt64, Source: SourceDeclared}
	if _, err := p.Cost(1_000_000_000, 0); !errors.Is(err, ErrPriceOverflow) {
		t.Fatalf("必须 ErrPriceOverflow，got %v", err)
	}
}
