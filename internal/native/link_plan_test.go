package native

import (
	"testing"

	"hermes/internal/toolchain"
)

// TestLinkPlanMatchesGo 是链接面决策的等价判据。
//
// 比三件事，缺一不可：
//   - 成败一致（哪些输入被拒）；
//   - **拒绝码一致**（一个码一种原因，码错了等于原因丢了）；
//   - 接受的输入上，静态/动态/框架/Go-runtime 四组完全一致。
func TestLinkPlanMatchesGo(t *testing.T) {
	host, err := ParseTarget("", "darwin", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	bare, err := ParseTarget("thumbv7em-none-eabi", "darwin", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	cross, err := ParseTarget("x86_64-unknown-linux-gnu", "darwin", "arm64")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		tgt      TargetInfo
		needNet  bool
		netLib   string
		statics  []string
		dynamics []string
	}{
		{"host-plain", host, false, "", nil, nil},
		{"host-net", host, true, "build/glibhermesnet.a", nil, nil},
		{"host-net-misnamed", host, true, "build/libnet.a", nil, nil},
		{"host-user-glib", host, false, "", []string{"build/glibfoo.a"}, nil},
		{"cross-net", cross, true, "build/glibhermesnet.a", nil, nil},
		{"bare-net", bare, true, "build/glibhermesnet.a", nil, nil},
		{"bare-glib", bare, false, "", []string{"build/glibfoo.a"}, nil},
		{"bare-not-a", bare, false, "", []string{"build/foo.so"}, nil},
		{"bare-dynamic", bare, false, "", nil, []string{"libfoo.so"}},
		{"cpp-abi", host, false, "", []string{"libstdc++.a"}, nil},
		{"forbidden-netlib", host, true, "build/libsupc++.a", nil, nil},
	}

	for _, c := range cases {
		goTgt, err := toolchain.ParseTarget(c.tgt.Raw, "darwin", "arm64")
		if err != nil {
			t.Fatalf("%s: Go target: %v", c.name, err)
		}
		wantPlan, werr := toolchain.BuildLinkPlan(toolchain.Input{
			Target: goTgt, NeedNet: c.needNet, NetLib: c.netLib,
			StaticLibs: c.statics, DynamicLibs: c.dynamics,
		})
		gotPlan, gerr := PlanLink(c.tgt, c.needNet, c.netLib, c.statics, c.dynamics)

		if (werr == nil) != (gerr == nil) {
			t.Fatalf("%s: 成败不同 Go=%v C++=%v", c.name, werr, gerr)
		}
		if werr != nil {
			if toolchain.CodeOf(werr) != refusalCode(gerr) {
				t.Fatalf("%s: 拒绝码不同 Go=%q C++=%q（C++ 文案：%v）",
					c.name, toolchain.CodeOf(werr), refusalCode(gerr), gerr)
			}
			if werr.Error() != gerr.Error() {
				t.Fatalf("%s: 拒绝文案不同\n  Go =%s\n  C++=%s", c.name, werr, gerr)
			}
			continue
		}
		if !eq(join(wantPlan.StaticLibs), join(gotPlan.Static)) ||
			!eq(join(wantPlan.DynamicLibs), join(gotPlan.Dynamic)) ||
			!eq(join(wantPlan.Frameworks), join(gotPlan.Frameworks)) ||
			!eq(join(wantPlan.GoRuntimeLibs), join(gotPlan.GoRuntime)) ||
			wantPlan.UsedNet != gotPlan.UsedNet {
			t.Fatalf("%s: 计划不同\n  Go =%v\n  C++=%v", c.name, wantPlan, gotPlan)
		}
	}
}

func refusalCode(err error) string {
	var r *LinkRefusal
	if err != nil && asRefusal(err, &r) {
		return r.Code
	}
	return ""
}

func asRefusal(err error, out **LinkRefusal) bool {
	if r, ok := err.(*LinkRefusal); ok {
		*out = r
		return true
	}
	return false
}

func join(xs []string) string {
	s := ""
	for i, x := range xs {
		if i > 0 {
			s += "|"
		}
		s += x
	}
	return s
}

func eq(a, b string) bool { return a == b }
