package budget

import (
	"errors"
	"testing"

	"hermes/internal/std"
	"hermes/internal/std/rtbudget"
)

// 选非 Go 版（variant=c）时，预算检查必须真走到 C 实现。
// 证据：rtbudget.Calls() 在 Reserve 之后严格增加。
func TestVariantCDispatchesToCRuntime(t *testing.T) {
	t.Setenv("HERMES_STD_VARIANT", "c")
	if std.SelectedVariant() != std.VariantC {
		t.Fatal("env 已设为 c，SelectedVariant 应返回 c")
	}

	l := New(Limits3{Global: Limits{MicroUSD: 100}})
	sc := Scope{Workflow: "wf", Step: "s1"}

	before := rtbudget.Calls()
	if _, err := l.Reserve(sc, Upper{MicroUSD: 60}); err != nil {
		t.Fatalf("首次预留不该失败：%v", err)
	}
	after := rtbudget.Calls()
	if after <= before {
		t.Fatalf("variant=c 下预算检查必须走到 C 实现：Calls 应增加，before=%d after=%d", before, after)
	}
}

// 选 Go 版（variant=go，也是默认值）时，预算检查走 Go 实现，C 计数器不变。
func TestVariantGoDoesNotTouchCRuntime(t *testing.T) {
	t.Setenv("HERMES_STD_VARIANT", "go")
	if std.SelectedVariant() != std.VariantGo {
		t.Fatal("env 已设为 go，SelectedVariant 应返回 go")
	}

	l := New(Limits3{Global: Limits{MicroUSD: 100}})
	sc := Scope{Workflow: "wf", Step: "s1"}

	before := rtbudget.Calls()
	if _, err := l.Reserve(sc, Upper{MicroUSD: 60}); err != nil {
		t.Fatalf("首次预留不该失败：%v", err)
	}
	after := rtbudget.Calls()
	if after != before {
		t.Fatalf("variant=go 下不应调用 C 实现：Calls 应不变，before=%d after=%d", before, after)
	}
}

// 关键不变式：同一个预算场景，选 c 与选 go 必须给出**完全一致**的接受/拒绝决策。
// 这是"可选而非另起炉灶"的硬保证——非 Go 版只是实现语言不同，语义逐字一致。
func TestVariantCAndGoDecideIdentically(t *testing.T) {
	cases := []struct {
		name   string
		lim    Limits3
		first  Upper
		next   Upper
		refuse bool
		level  Level
	}{
		{name: "在限额内，两次都接受", lim: Limits3{Global: Limits{MicroUSD: 100}}, first: Upper{MicroUSD: 60}, next: Upper{MicroUSD: 30}, refuse: false},
		{name: "第二次越过 in，global 拦", lim: Limits3{Global: Limits{TokensIn: 100}}, first: Upper{In: 60}, next: Upper{In: 60}, refuse: true, level: LevelGlobal},
		{name: "第二次越过 out，step 拦", lim: Limits3{Step: Limits{TokensOut: 100}}, first: Upper{Out: 60}, next: Upper{Out: 60}, refuse: true, level: LevelStep},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			decide := func(v std.Variant) (bool, Level) {
				t.Setenv("HERMES_STD_VARIANT", string(v))
				l := New(c.lim)
				sc := Scope{Workflow: "wf", Step: "s1"}
				if _, err := l.Reserve(sc, c.first); err != nil {
					t.Fatalf("首次预留不该失败：%v", err)
				}
				_, err := l.Reserve(sc, c.next)
				if err == nil {
					return false, LevelGlobal
				}
				var ref *Refusal
				if !errors.As(err, &ref) {
					t.Fatalf("拒绝必须是 *Refusal，got %T", err)
				}
				return true, ref.Level
			}

			refuseGo, levelGo := decide(std.VariantGo)
			refuseC, levelC := decide(std.VariantC)

			if refuseGo != refuseC {
				t.Fatalf("variant=go 与 c 决策不一致：go refuse=%v, c refuse=%v", refuseGo, refuseC)
			}
			if refuseGo && levelGo != levelC {
				t.Fatalf("拒绝层级不一致：go=%v, c=%v", levelGo, levelC)
			}
		})
	}
}

// 默认（不设 env）与显式 go 必须等价，且都走 Go 实现（C 计数器不变）。
func TestDefaultVariantEqualsGo(t *testing.T) {
	t.Setenv("HERMES_STD_VARIANT", "")
	if std.SelectedVariant() != std.VariantGo {
		t.Fatal("缺省应回退到 go（今天的行为）")
	}
	before := rtbudget.Calls()
	l := New(Limits3{Global: Limits{TokensIn: 100}})
	if _, err := l.Reserve(Scope{Workflow: "wf", Step: "s1"}, Upper{In: 10}); err != nil {
		t.Fatalf("预留不该失败：%v", err)
	}
	if rtbudget.Calls() != before {
		t.Fatal("默认变体不应调用 C 实现")
	}
}
