package std

import (
	"testing"
)

// g*/G* 命名表自洽：每个成员都必须同时提供 go 与 c 两个变体，
// 且 go 版带 g/G 前缀、非 go 版用成员本名。
func TestImplNamingTable(t *testing.T) {
	cases := map[string]struct{ goName, cName string }{
		"net":     {"gnet", "net"},
		"model":   {"gmodel", "model"},
		"runtime": {"gruntime", "runtime"},
		"json":    {"gjson", "json"},
	}
	for mod, want := range cases {
		goImpl, ok := ImplOf(mod, VariantGo)
		if !ok {
			t.Fatalf("成员 %q 缺少 go 变体", mod)
		}
		if goImpl.Name != want.goName || !goImpl.GoImpl {
			t.Fatalf("成员 %q 的 go 实现应为 %q(GoImpl=true)，got %+v", mod, want.goName, goImpl)
		}
		cImpl, ok := ImplOf(mod, VariantC)
		if !ok {
			t.Fatalf("成员 %q 缺少 c 变体", mod)
		}
		if cImpl.Name != want.cName || cImpl.GoImpl {
			t.Fatalf("成员 %q 的 c 实现应为 %q(GoImpl=false)，got %+v", mod, want.cName, cImpl)
		}
	}
}

// SelectedVariant 缺省回退到 go（=今天的行为），仅 c/C 命中非 Go 变体。
func TestSelectedVariantDefault(t *testing.T) {
	cases := []struct {
		env  string
		want Variant
	}{
		{"", VariantGo},
		{"go", VariantGo},
		{"Go", VariantGo},
		{"anything-else", VariantGo},
		{"c", VariantC},
		{"C", VariantC},
	}
	for _, c := range cases {
		if c.env == "" {
			t.Setenv("HERMES_STD_VARIANT", "")
		} else {
			t.Setenv("HERMES_STD_VARIANT", c.env)
		}
		if got := SelectedVariant(); got != c.want {
			t.Fatalf("env=%q: SelectedVariant()=%q, want %q", c.env, got, c.want)
		}
	}
}

func TestIsKnownMember(t *testing.T) {
	if !IsKnownMember("std/net") || !IsKnownMember("net") {
		t.Fatal("std/net 与 net 都应是已知成员")
	}
	if IsKnownMember("std/nope") || IsKnownMember("nope") {
		t.Fatal("std/nope 不应是已知成员")
	}
}

func TestVariantsListsBoth(t *testing.T) {
	for _, mod := range []string{"net", "model", "runtime", "json"} {
		vs := Variants(mod)
		if len(vs) != 2 {
			t.Fatalf("成员 %q 应提供 2 个变体，got %d", mod, len(vs))
		}
		seen := map[Variant]bool{}
		for _, v := range vs {
			seen[v] = true
		}
		if !seen[VariantGo] || !seen[VariantC] {
			t.Fatalf("成员 %q 的变体集合必须含 go 与 c，got %v", mod, vs)
		}
	}
}

// 未知成员 ImplOf 返回 (Impl{}, false)。
func TestImplOfUnknown(t *testing.T) {
	if _, ok := ImplOf("nope", VariantGo); ok {
		t.Fatal("未知成员不应有实现")
	}
	if _, ok := ImplOf("net", Variant("x")); ok {
		t.Fatal("未知变体不应有实现")
	}
}

// exceedsGo 与 internal/budget.exceeds 的语义镜像（这里不依赖 budget 包，
// 只验证本包的纯 Go 镜像逻辑：<=0 维度不限、任一维度越过即 true）。
func TestExceedsGoMirror(t *testing.T) {
	cases := []struct {
		name   string
		used   BudgetProbe
		up     BudgetProbe
		lim    BudgetProbe
		expect bool
	}{
		{"不限", BudgetProbe{}, BudgetProbe{In: 1}, BudgetProbe{}, false},
		{"in 越过", BudgetProbe{In: 60}, BudgetProbe{In: 60}, BudgetProbe{In: 100}, true},
		{"out 越过", BudgetProbe{}, BudgetProbe{Out: 60}, BudgetProbe{Out: 50}, true},
		{"usd 越过", BudgetProbe{}, BudgetProbe{MicroUSD: 60}, BudgetProbe{MicroUSD: 50}, true},
		{"恰好顶满不算越", BudgetProbe{In: 40}, BudgetProbe{In: 60}, BudgetProbe{In: 100}, false},
	}
	for _, c := range cases {
		if got := exceedsGo(c.used, c.up, c.lim); got != c.expect {
			t.Fatalf("%s: exceedsGo=%v, want %v", c.name, got, c.expect)
		}
	}
}
