package toolchain

import (
	"strings"
	"testing"
)

func parseForTest(t *testing.T, raw string) Target {
	t.Helper()
	tgt, err := ParseTarget(raw, "darwin", "arm64")
	if err != nil {
		t.Fatalf("ParseTarget(%q): %v", raw, err)
	}
	return tgt
}

func TestLibStemTable(t *testing.T) {
	cases := []struct{ in, want string }{
		{"libstdc++.so.6", "libstdc++"},
		{"/usr/lib/x86_64-linux-gnu/libsupc++.a", "libsupc++"},
		{"@rpath/libc++.1.dylib", "libc++"},
		{"libc++abi.dylib", "libc++abi"},
		{"glibhermesnet.a", "glibhermesnet"},
		{"build/libhermesnet.so.1", "libhermesnet"},
		{"libm.so.1.2.3", "libm"},
	}
	for _, c := range cases {
		if got := libStem(c.in); got != c.want {
			t.Errorf("libStem(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestIsGoRuntimeLibTable pins both detectors: the g* declaration AND the
// legacy-name safety net. A pure C library must never be flagged.
func TestIsGoRuntimeLibTable(t *testing.T) {
	cases := []struct {
		lib  string
		want bool
	}{
		{"glibhermesnet.a", true},
		{"build/glibhermesnet.a", true},
		{"gnet.a", true},
		{"gnet.so.1", true},
		{"libhermesnet.a", true},    // legacy name: fail-closed, not "clean"
		{"libhermesrt.dylib", true}, // legacy interpreter archive
		{"libc.a", false},
		{"libm.so.1", false},
		{"@rpath/libSystem.B.dylib", false},
		{"libgcc_s.so.1", false},
	}
	for _, c := range cases {
		if got := IsGoRuntimeLib(c.lib); got != c.want {
			t.Errorf("IsGoRuntimeLib(%q) = %v, want %v", c.lib, got, c.want)
		}
	}
}

// TestLegacyGoRuntimeNameMutation proves the legacy-name net is wired: remove
// an entry and the un-renamed archive must stop being recognised. Without this
// a future "cleanup" could silently reopen the firmware hole.
func TestLegacyGoRuntimeNameMutation(t *testing.T) {
	lib := "build/libhermesnet.a"
	if !IsGoRuntimeLib(lib) {
		t.Fatal("baseline: the legacy Go network archive must be recognised")
	}
	saved := legacyGoRuntimeNames["libhermesnet"]
	delete(legacyGoRuntimeNames, "libhermesnet")
	defer func() { legacyGoRuntimeNames["libhermesnet"] = saved }()
	if IsGoRuntimeLib(lib) {
		t.Fatal("removing the legacy name must change the verdict; the table is not consulted")
	}
}

// TestCheckPureCABITable covers the C++ ABI closed set, including libc++ which
// is licence-legal but still an ABI boundary (it must NOT be folded into the
// licence closed set and must NOT share its error code).
func TestCheckPureCABITable(t *testing.T) {
	cases := []struct {
		lib     string
		wantHit bool
	}{
		{"libstdc++.so.6", true},
		{"libsupc++.a", true},
		{"@rpath/libc++.1.dylib", true},
		{"libc++abi.dylib", true},
		{"libcxxrt.a", true},
		{"libc.a", false},
		{"glibhermesnet.a", false},
		{"libm.so.1", false},
	}
	for _, c := range cases {
		err := CheckPureCABI("--link-static", c.lib)
		if c.wantHit {
			if err == nil {
				t.Fatalf("CheckPureCABI(%q) must refuse a C++ ABI runtime", c.lib)
			}
			if CodeOf(err) != CodeCppABIBridge {
				t.Errorf("CheckPureCABI(%q) code = %q, want %q", c.lib, CodeOf(err), CodeCppABIBridge)
			}
			if !strings.Contains(err.Error(), "extern \"C\"") {
				t.Errorf("refusal must tell the user to provide an extern \"C\" bridge: %q", err.Error())
			}
			continue
		}
		if err != nil {
			t.Errorf("CheckPureCABI(%q) false positive: %v", c.lib, err)
		}
	}
}

func TestCppABIMutation(t *testing.T) {
	lib := "@rpath/libc++.1.dylib"
	if CheckPureCABI("--link-dynamic", lib) == nil {
		t.Fatal("baseline: libc++ must be refused")
	}
	saved := cppABIRuntimes["libc++"]
	delete(cppABIRuntimes, "libc++")
	defer func() { cppABIRuntimes["libc++"] = saved }()
	if CheckPureCABI("--link-dynamic", lib) != nil {
		t.Fatal("removing libc++ from the closed set must change the verdict; the table is not consulted")
	}
}

// TestGoRuntimeBareMetalMessageIsVerbatim pins the exact user-specified text.
// It is printed with no prefix, so even a leading "hermes compile:" would be a
// contract break.
func TestGoRuntimeBareMetalMessageIsVerbatim(t *testing.T) {
	const want = "[ERROR] Go runtime libraries (g*) cannot be linked for bare-metal targets. Please use pure C/Rust static libraries."
	if GoRuntimeBareMetalMessage != want {
		t.Fatalf("message = %q\nwant     = %q", GoRuntimeBareMetalMessage, want)
	}
}

// TestBuildLinkPlanBareMetalRefusals is the hardware-isolation spec. Each row
// asserts a distinct *cause code*, so a single generic refusal cannot satisfy
// the table.
func TestBuildLinkPlanBareMetalRefusals(t *testing.T) {
	bare := parseForTest(t, "thumbv7em-none-eabi")
	cases := []struct {
		name     string
		in       Input
		wantCode string
		wantText string
	}{
		{
			name:     "built-in network archive (needNet) refused verbatim",
			in:       Input{Target: bare, NeedNet: true},
			wantCode: CodeGoRuntimeBareMetal,
			wantText: GoRuntimeBareMetalMessage,
		},
		{
			name:     "explicit g* static lib refused verbatim",
			in:       Input{Target: bare, StaticLibs: []string{"build/glibhermesnet.a"}},
			wantCode: CodeGoRuntimeBareMetal,
			wantText: GoRuntimeBareMetalMessage,
		},
		{
			name:     "un-renamed legacy Go archive still refused verbatim",
			in:       Input{Target: bare, StaticLibs: []string{"build/libhermesnet.a"}},
			wantCode: CodeGoRuntimeBareMetal,
			wantText: GoRuntimeBareMetalMessage,
		},
		{
			name:     "g* dynamic lib refused verbatim",
			in:       Input{Target: bare, DynamicLibs: []string{"gnet.so.1"}},
			wantCode: CodeGoRuntimeBareMetal,
			wantText: GoRuntimeBareMetalMessage,
		},
		{
			name:     "any dynamic lib refused (no dynamic loader)",
			in:       Input{Target: bare, DynamicLibs: []string{"libm.so.1"}},
			wantCode: CodeDynamicBareMetal,
			wantText: "no dynamic loader",
		},
		{
			name:     "non-.a static input refused",
			in:       Input{Target: bare, StaticLibs: []string{"libm.so.1"}},
			wantCode: CodeBareMetalLinkShape,
			wantText: "static .a archives only",
		},
		{
			name:     "C++ ABI refused even on bare metal",
			in:       Input{Target: bare, StaticLibs: []string{"libc++abi.a"}},
			wantCode: CodeCppABIBridge,
			wantText: "extern \"C\"",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := BuildLinkPlan(c.in)
			if err == nil {
				t.Fatal("must be refused")
			}
			if CodeOf(err) != c.wantCode {
				t.Fatalf("code = %q, want %q (err: %v)", CodeOf(err), c.wantCode, err)
			}
			if c.wantText == GoRuntimeBareMetalMessage {
				if err.Error() != c.wantText {
					t.Fatalf("must be printed verbatim, got %q", err.Error())
				}
			} else if !strings.Contains(err.Error(), c.wantText) {
				t.Fatalf("error %q does not name the cause %q", err.Error(), c.wantText)
			}
		})
	}
}

// TestBuildLinkPlanBareMetalClean proves the allowed path is not empty: pure
// C/Rust static archives must actually link, with a structurally empty Go
// runtime / dynamic / framework surface.
func TestBuildLinkPlanBareMetalClean(t *testing.T) {
	bare := parseForTest(t, "thumbv7em-none-eabi")
	plan, err := BuildLinkPlan(Input{
		Target:     bare,
		StaticLibs: []string{"build/libm.a", "build/librust_core.a"},
	})
	if err != nil {
		t.Fatalf("pure C/Rust static .a must be allowed: %v", err)
	}
	if len(plan.StaticLibs) != 2 || plan.StaticLibs[0] != "build/libm.a" {
		t.Fatalf("static libs lost or reordered: %v", plan.StaticLibs)
	}
	if len(plan.DynamicLibs) != 0 || len(plan.Frameworks) != 0 || len(plan.GoRuntimeLibs) != 0 || plan.UsedNet {
		t.Fatalf("bare-metal plan must carry no Go runtime / dynamic / framework: %+v", plan)
	}
	for _, lib := range append(append([]string{}, plan.StaticLibs...), plan.DynamicLibs...) {
		if err := CheckPureCABI("--link-static", lib); err != nil {
			t.Fatalf("pure C ABI violated by %q: %v", lib, err)
		}
	}
}

// TestBuildLinkPlanHost pins the software-layer rules, including the g*
// namespace contract and the cross-compilation ABI constraint.
func TestBuildLinkPlanHost(t *testing.T) {
	host := parseForTest(t, "")
	cross := parseForTest(t, "x86_64-unknown-linux-gnu")

	t.Run("g* net archive allowed and recorded", func(t *testing.T) {
		plan, err := BuildLinkPlan(Input{Target: host, NeedNet: true, NetLib: "build/glibhermesnet.a"})
		if err != nil {
			t.Fatal(err)
		}
		if !plan.UsedNet || len(plan.GoRuntimeLibs) != 1 || plan.StaticLibs[0] != "build/glibhermesnet.a" {
			t.Fatalf("Go runtime archive not recorded in the plan: %+v", plan)
		}
		if len(plan.Frameworks) == 0 {
			t.Fatal("darwin host must carry CoreFoundation/Security for a Go c-archive")
		}
	})

	t.Run("un-namespaced net archive refused", func(t *testing.T) {
		_, err := BuildLinkPlan(Input{Target: host, NeedNet: true, NetLib: "build/libhermesnet.a"})
		if CodeOf(err) != CodeGoRuntimeName {
			t.Fatalf("code = %q, want %q (err %v)", CodeOf(err), CodeGoRuntimeName, err)
		}
		if !strings.Contains(err.Error(), "glibhermesnet.a") {
			t.Fatalf("refusal must give the actionable rename: %v", err)
		}
	})

	t.Run("host-built Go archive cannot enter a cross-compiled artifact", func(t *testing.T) {
		_, err := BuildLinkPlan(Input{Target: cross, NeedNet: true, NetLib: "build/glibhermesnet.a"})
		if CodeOf(err) != CodeCrossNetArchive {
			t.Fatalf("code = %q, want %q (err %v)", CodeOf(err), CodeCrossNetArchive, err)
		}
	})

	t.Run("no net lib resolved refused", func(t *testing.T) {
		_, err := BuildLinkPlan(Input{Target: host, NeedNet: true})
		if CodeOf(err) != CodeNetLibMissing {
			t.Fatalf("code = %q, want %q (err %v)", CodeOf(err), CodeNetLibMissing, err)
		}
	})

	t.Run("pure computation host artifact has no Go runtime", func(t *testing.T) {
		plan, err := BuildLinkPlan(Input{Target: host, StaticLibs: []string{"build/libm.a"}})
		if err != nil {
			t.Fatal(err)
		}
		if plan.UsedNet || len(plan.GoRuntimeLibs) != 0 || len(plan.Frameworks) != 0 {
			t.Fatalf("pure computation must not pull in a Go runtime: %+v", plan)
		}
	})

	// A user-supplied g* c-archive is a Go-runtime link input even when the
	// program never calls tools/models (NeedNet=false). Its crypto/x509 code
	// needs the same darwin frameworks as the built-in network archive.
	frameworksOf := func(plan Plan) string { return strings.Join(plan.Frameworks, " ") }

	t.Run("user g* static archive without NeedNet still gets darwin frameworks", func(t *testing.T) {
		plan, err := BuildLinkPlan(Input{Target: host, StaticLibs: []string{"build/gnet.a"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.GoRuntimeLibs) != 1 || plan.GoRuntimeLibs[0] != "build/gnet.a" {
			t.Fatalf("user g* c-archive must be recorded as a Go runtime lib: %+v", plan)
		}
		if fw := frameworksOf(plan); !strings.Contains(fw, "CoreFoundation") || !strings.Contains(fw, "Security") {
			t.Fatalf("darwin host must carry CoreFoundation/Security for a user g* c-archive: %+v", plan)
		}
	})

	t.Run("user g* dynamic archive without NeedNet still gets darwin frameworks", func(t *testing.T) {
		plan, err := BuildLinkPlan(Input{Target: host, DynamicLibs: []string{"build/gnet.dylib"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.GoRuntimeLibs) != 1 || plan.GoRuntimeLibs[0] != "build/gnet.dylib" {
			t.Fatalf("user g* dylib must be recorded as a Go runtime lib: %+v", plan)
		}
		if fw := frameworksOf(plan); !strings.Contains(fw, "CoreFoundation") || !strings.Contains(fw, "Security") {
			t.Fatalf("darwin host must carry CoreFoundation/Security for a user g* dylib: %+v", plan)
		}
	})

	t.Run("non-darwin cross target with a g* archive carries no frameworks", func(t *testing.T) {
		plan, err := BuildLinkPlan(Input{Target: cross, StaticLibs: []string{"build/gnet.a"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.GoRuntimeLibs) != 1 {
			t.Fatalf("the g* archive must still be recorded on a cross target: %+v", plan)
		}
		if len(plan.Frameworks) != 0 {
			t.Fatalf("linux cross target must not carry darwin frameworks: %+v", plan)
		}
	})

	t.Run("host still refuses C++ ABI and copyleft runtimes", func(t *testing.T) {
		if _, err := BuildLinkPlan(Input{Target: host, StaticLibs: []string{"libc++.a"}}); CodeOf(err) != CodeCppABIBridge {
			t.Fatalf("C++ ABI must be refused on host too: %v", err)
		}
		if _, err := BuildLinkPlan(Input{Target: host, StaticLibs: []string{"libgomp.so.1"}}); CodeOf(err) != CodeForbiddenRuntime {
			t.Fatalf("licence closed set must be enforced on the plan surface: %v", err)
		}
	})
}
