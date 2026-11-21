package linkpolicy

import (
	"strings"
	"testing"
)

// wantNames is the closed set the licence red line MUST cover. Keeping this
// list in the test (rather than deriving it from ForbiddenRuntimes) is what
// makes the guard able to fail: if someone deletes a row from
// ForbiddenRuntimes, this test goes red instead of silently shrinking the
// red line. There is no "derive from the implementation" shortcut here.
var wantNames = []string{
	"libstdc++",
	"libsupc++",
	"libgcc_s",
	"libgomp",
	"libquadmath",
}

// TestClosedSetIsComplete fails when any required entry is missing, has an
// empty licence/reason/source, or is duplicated. It is the constructive
// mutation check: deleting one row from ForbiddenRuntimes must turn this red.
func TestClosedSetIsComplete(t *testing.T) {
	byName := make(map[string]Rule, len(ForbiddenRuntimes))
	for _, r := range ForbiddenRuntimes {
		if r.Name == "" {
			t.Fatal("a rule with an empty Name matches everything via Contains(\"\", ...)")
		}
		if r.Licence == "" || r.Reason == "" || r.Source == "" {
			t.Errorf("rule %q is missing licence/reason/source: %+v", r.Name, r)
		}
		if _, dup := byName[r.Name]; dup {
			t.Errorf("duplicate rule %q", r.Name)
		}
		byName[r.Name] = r
	}
	for _, want := range wantNames {
		if _, ok := byName[want]; !ok {
			t.Errorf("closed set is missing %q -- deleting a row must make this test red", want)
		}
	}
	if len(ForbiddenRuntimes) < len(wantNames) {
		t.Errorf("closed set has %d entries, want at least %d", len(ForbiddenRuntimes), len(wantNames))
	}
}

// TestMatchTable drives every closed-set entry plus realistic path spellings,
// and a set of libraries that MUST pass. A false positive here (e.g. matching
// libc++) would reject legitimate C++ users; a false negative lets GPL in.
func TestMatchTable(t *testing.T) {
	cases := []struct {
		lib     string
		wantHit bool
		want    string
	}{
		{"@rpath/libstdc++.6.dylib", true, "libstdc++"},
		{"/usr/lib/x86_64-linux-gnu/libstdc++.so.6", true, "libstdc++"},
		{"libsupc++.a", true, "libsupc++"},
		{"/usr/lib/libgcc_s.so.1", true, "libgcc_s"},
		{"libgomp.so.1", true, "libgomp"},
		{"/opt/gcc/lib/libquadmath.0.dylib", true, "libquadmath"},
		{"LIBSTDC++.SO.6", true, "libstdc++"}, // case-insensitive
		{"@rpath/libc++.1.dylib", false, ""},
		{"/usr/lib/libc++abi.dylib", false, ""},
		{"libc++.so.1", false, ""},
		{"/usr/lib/libSystem.B.dylib", false, ""},
		{"glibhermesnet.a", false, ""}, // Go runtime archive: licence-clean, gated by toolchain instead
		{"libhermesnet.a", false, ""},  // legacy spelling, same licence verdict
		{"", false, ""},
	}
	for _, c := range cases {
		r, got := Match(c.lib)
		if got != c.wantHit {
			t.Errorf("Match(%q) hit=%v, want %v (rule %+v)", c.lib, got, c.wantHit, r)
			continue
		}
		if c.wantHit && r.Name != c.want {
			t.Errorf("Match(%q) rule=%q, want %q", c.lib, r.Name, c.want)
		}
	}
}

// TestFirstForbidden pins the "first match wins, original spelling preserved"
// contract used by both cc and compile diagnostics.
func TestFirstForbidden(t *testing.T) {
	libs := []string{
		"/usr/lib/libSystem.B.dylib",
		"@rpath/libc++.1.dylib",
		"@rpath/libstdc++.6.dylib",
		"libgcc_s.so.1",
	}
	bad, rule, ok := FirstForbidden(libs)
	if !ok {
		t.Fatal("FirstForbidden must find the libstdc++ entry")
	}
	if bad != "@rpath/libstdc++.6.dylib" {
		t.Errorf("FirstForbidden returned %q, want the original libstdc++ spelling", bad)
	}
	if rule.Name != "libstdc++" {
		t.Errorf("FirstForbidden rule = %q, want libstdc++", rule.Name)
	}
	if !strings.Contains(rule.Reason, "must not enter the link") {
		t.Errorf("rule reason is not actionable: %q", rule.Reason)
	}
	if _, _, ok := FirstForbidden([]string{"libc++.dylib", "libSystem.B.dylib"}); ok {
		t.Error("a clean link surface must not report a forbidden runtime")
	}
	if _, _, ok := FirstForbidden(nil); ok {
		t.Error("nil deps must be clean")
	}
}
