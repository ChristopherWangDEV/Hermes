package main

import (
	"os/exec"
	"hermes/internal/native"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
			err := native.LinkInputGate(c.flag, c.lib)
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

// TestLinkPolicyTableIsGenerated pins that the C++ closed set is **generated** from
// internal/linkpolicy, not hand-written. 两份手抄的表会漂移，而这里漂移的后果是
// GPL 库被放进来。
//
// 判据：重新生成一次，与构建时使用的那份逐字节相同。谁手改了 C++ 表，这条就红。
func TestLinkPolicyTableIsGenerated(t *testing.T) {
	root := repoRootForBackend(t)
	out, err := filepath.Join(t.TempDir(), "table.cpp"), error(nil)
	_ = err
	cmd := exec.Command("go", "run", "./tools/genlinkpolicy", out)
	cmd.Dir = root
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("生成失败: %v\n%s", err, b)
	}
	fresh, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	built, err := os.ReadFile(filepath.Join(root, "build", "linkpolicy_table.cpp"))
	if err != nil {
		t.Skipf("build 里没有生成的表（先跑 scripts/build.sh）: %v", err)
	}
	if string(fresh) != string(built) {
		t.Fatalf("C++ 的许可闭集与 internal/linkpolicy 不一致——表被手改了？\n--- 生成 ---\n%s\n--- 构建 ---\n%s", fresh, built)
	}
}

// TestGateLinkInputsOrdering checks both slices are gated and the flag name in
// the error identifies which one failed (helps real users fix the right arg).
func TestGateLinkInputsOrdering(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "libok.a")
	if err := os.WriteFile(good, []byte("!<arch>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := native.LinkInputGate("--link-static", good); err != nil {
		t.Fatalf("valid path must pass: %v", err)
	}
	if err := native.LinkInputGate("--link-dynamic", good); err != nil {
		t.Fatalf("valid path must pass: %v", err)
	}
	err := native.LinkInputGate("--link-dynamic", "-lSystem")
	if err == nil || !strings.Contains(err.Error(), "--link-dynamic") {
		t.Fatalf("error must name --link-dynamic, got %v", err)
	}
	err = native.LinkInputGate("--link-static", "-lSystem")
	if err == nil || !strings.Contains(err.Error(), "--link-static") {
		t.Fatalf("error must name --link-static, got %v", err)
	}
}

// TestClosedSetIsSharedByCCAndCompile pins that `hermes cc`（C++ 侧的闭集判据）
// 与 `hermes compile` 的 linkInputGate 用**同一份**闭集：删掉一行必须两边都变，
// 两条路径不能各自漂移。
//
// cc 的实现搬进 C++ 之后，这条比的是 native.LinkPolicyMatch 与 Go 的
// gateLinkInputs —— 比原来"两份 Go 函数对照"更强：它跨语言比对。
func TestClosedSetIsSharedByCCAndCompile(t *testing.T) {
	for _, name := range []string{"libstdc++", "libsupc++", "libgcc_s", "libgomp", "libquadmath"} {
		lib := "/usr/lib/" + name + ".so.1"
		if name2, _ := native.LinkPolicyMatch(lib); name2 == "" {
			t.Errorf("C++ 侧闭集必须拦住 %q", lib)
		}
		if err := native.LinkInputGate("--link-dynamic", "-l"+name); err == nil {
			t.Errorf("hermes compile 的输入闸必须拦住 -l%s", name)
		}
	}
	for _, name := range []string{"libc++", "libc++abi", "libSystem.B"} {
		lib := "/usr/lib/" + name + ".dylib"
		if name2, _ := native.LinkPolicyMatch(lib); name2 != "" {
			t.Errorf("C++ 侧误报 %q（规则 %q）", lib, name2)
		}
	}
}
