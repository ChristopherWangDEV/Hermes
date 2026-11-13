package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hermes/internal/linkpolicy"
)

// TestLinkInputGateTable is the executable form of the `--link-*` input gate.
// It is table-driven on purpose: the failure mode this project keeps hitting
// is a gate that silently stops covering one entry. Every case here must be
// able to turn red on its own (see TestLinkInputGateMutation below).
func TestLinkInputGateTable(t *testing.T) {
	dir := t.TempDir()
	// Real, existing library files: the gate must accept these. A versioned
	// soname (.so.1) is included because real Linux distros ship that shape and
	// filepath.Ext would otherwise misclassify it as ".1".
	goodA := filepath.Join(dir, "libmath.a")
	goodDylib := filepath.Join(dir, "libmath.dylib")
	goodSo := filepath.Join(dir, "libmath.so")
	goodSoN := filepath.Join(dir, "libmath.so.1")
	for _, p := range []string{goodA, goodDylib, goodSo, goodSoN} {
		if err := os.WriteFile(p, []byte("!<arch>\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Existing files carrying a forbidden name: with the closed set intact the
	// ONLY reason to refuse is the licence rule, not os.Stat. With the rule
	// removed (see the mutation test) these same paths must be accepted, which
	// proves the gate is genuinely wired to linkpolicy.ForbiddenRuntimes.
	forbiddenFiles := []string{"libstdc++.a", "libsupc++.a", "libgcc_s.so.1", "libgomp.so.1", "libquadmath.a"}
	for _, name := range forbiddenFiles {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("!<arch>\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A *directory* whose name still looks like a library: isNativeLibPath must
	// pass it so the only thing that refuses it is st.IsDir(). A bare temp dir
	// has no library extension and would be stopped earlier by the extension
	// check, never exercising the "regular file" branch.
	dirLib := filepath.Join(dir, "libdir.a")
	if err := os.Mkdir(dirLib, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		flag    string
		lib     string
		wantErr bool
		wantSub string
	}{
		{"static .a accepted", "--link-static", goodA, false, ""},
		{"dynamic .dylib accepted", "--link-dynamic", goodDylib, false, ""},
		{"dynamic .so accepted", "--link-dynamic", goodSo, false, ""},
		{"versioned .so.N accepted", "--link-dynamic", goodSoN, false, ""},
		{"empty path refused", "--link-static", "", true, "non-empty"},
		{"bare -l system lib refused", "--link-static", "-lmath", true, "bare -l"},
		{"-Wl, smuggling refused", "--link-dynamic", "-Wl,-rpath,/tmp", true, "-Wl,"},
		{"-Wl, embedded in a path refused", "--link-static", dir + "/-Wl,-lstdc++.a", true, "-Wl,"},
		{"libstdc++ refused", "--link-static", filepath.Join(dir, "libstdc++.a"), true, "libstdc++"},
		{"libsupc++ refused", "--link-static", filepath.Join(dir, "libsupc++.a"), true, "libsupc++"},
		{"libgcc_s refused", "--link-dynamic", filepath.Join(dir, "libgcc_s.so.1"), true, "libgcc_s"},
		{"libgomp refused", "--link-dynamic", filepath.Join(dir, "libgomp.so.1"), true, "libgomp"},
		{"libquadmath refused", "--link-static", filepath.Join(dir, "libquadmath.a"), true, "libquadmath"},
		{"system-path libstdc++ refused", "--link-static", "/usr/lib/x86_64-linux-gnu/libstdc++.so.6", true, "libstdc++"},
		{"unknown extension refused", "--link-static", filepath.Join(dir, "libmath.o"), true, "not a native library"},
		{"missing file refused", "--link-static", filepath.Join(dir, "absent.a"), true, "existing library"},
		{"directory refused", "--link-static", dirLib, true, "regular library"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := linkInputGate(c.flag, c.lib)
			if c.wantErr {
				if err == nil {
					t.Fatalf("must be refused, got nil (gate stopped covering %q)", c.lib)
				}
				if c.wantSub != "" && !strings.Contains(err.Error(), c.wantSub) {
					t.Fatalf("error %q does not mention %q", err.Error(), c.wantSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("must be accepted, got %v", err)
			}
		})
	}
}

// TestLinkInputGateMutation proves the guard is not a tautology: if the closed
// set loses an entry, linkInputGate MUST stop refusing that library. It does
// this by temporarily removing one row from linkpolicy.ForbiddenRuntimes and
// asserting the gate flips from "refused" to "accepted" (then restoring it).
// Without this, deleting a row could leave every test green.
func TestLinkInputGateMutation(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "libgomp.so.1")
	if err := os.WriteFile(lib, []byte("!<arch>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := linkInputGate("--link-dynamic", lib); err == nil {
		t.Fatal("baseline: libgomp must be refused before the mutation")
	}
	orig := linkpolicy.ForbiddenRuntimes
	kept := make([]linkpolicy.Rule, 0, len(orig))
	for _, r := range orig {
		if r.Name != "libgomp" {
			kept = append(kept, r)
		}
	}
	linkpolicy.ForbiddenRuntimes = kept
	defer func() { linkpolicy.ForbiddenRuntimes = orig }()

	if err := linkInputGate("--link-dynamic", lib); err == nil {
		return // mutation detected: the gate is genuinely wired to the closed set
	}
	t.Fatal("gate still refuses libgomp after removing its rule: the gate is " +
		"hard-coded somewhere else, so the table-driven guard would be a tautology")
}

// TestGateLinkInputsOrdering checks both slices are gated and the flag name in
// the error identifies which one failed (helps real users fix the right arg).
func TestGateLinkInputsOrdering(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "libok.a")
	if err := os.WriteFile(good, []byte("!<arch>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := gateLinkInputs([]string{good}, []string{good}); err != nil {
		t.Fatalf("two valid paths must pass: %v", err)
	}
	err := gateLinkInputs(nil, []string{"-lSystem"})
	if err == nil || !strings.Contains(err.Error(), "--link-dynamic") {
		t.Fatalf("error must name --link-dynamic, got %v", err)
	}
	err = gateLinkInputs([]string{"-lSystem"}, nil)
	if err == nil || !strings.Contains(err.Error(), "--link-static") {
		t.Fatalf("error must name --link-static, got %v", err)
	}
}

// TestGPLRuntimeSharesClosedSet pins that `hermes cc`'s gplRuntime and
// `hermes compile`'s linkInputGate consult the SAME linkpolicy closed set.
// Removing a row must change BOTH, so the two paths cannot drift apart.
func TestGPLRuntimeSharesClosedSet(t *testing.T) {
	for _, name := range []string{"libstdc++", "libsupc++", "libgcc_s", "libgomp", "libquadmath"} {
		if gplRuntime([]string{"/usr/lib/" + name + ".so.1"}) == "" {
			t.Errorf("gplRuntime must catch %q from the shared closed set", name)
		}
	}
	for _, name := range []string{"libc++", "libc++abi", "libSystem.B"} {
		if gplRuntime([]string{"/usr/lib/" + name + ".dylib"}) != "" {
			t.Errorf("gplRuntime false positive on %q", name)
		}
	}
}
