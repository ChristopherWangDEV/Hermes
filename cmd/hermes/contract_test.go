package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// contractSrc 是一个带 require/ensure 的 unit：budget > 0 才通过前置条件，
// 且返回值不能为空。
const contractSrc = `export struct Req { int budget; }

export workflow w(input: Req)
  require(input.budget > 0)
  ensure(result != "")
  entry -> string {
  if (input.budget > 1) { return "ok"; }
  return "";
}
`

// TestContractEnforcedOnBothPaths 守 AOT 与 run **同一套语义**。
//
// 为什么必须两边都测：契约判断如果只在一侧生效，用户会得到"本地 run 过了、
// 上线产物却拒绝请求"（或反过来）——那种分叉比没有契约更糟，因为它给了
// 一个不成立的保证。
func TestContractEnforcedOnBothPaths(t *testing.T) {
	root := repoRootForBackend(t)
	bin := filepath.Join(root, "build", "hermes")
	dir := t.TempDir()
	src := filepath.Join(dir, "c.hermes")
	if err := os.WriteFile(src, []byte(contractSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(dir, "c.hbc")
	if out, err := exec.Command(bin, "build", src, "-o", unit).CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	writeJSON := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	pass := writeJSON("pass.json", `{"budget":2}`)
	failRequire := writeJSON("req.json", `{"budget":0}`)
	failEnsure := writeJSON("ens.json", `{"budget":1}`)

	// run：通过 / 违反 require / 违反 ensure
	if out, err := exec.Command(bin, "run", unit, "--input", pass).Output(); err != nil {
		t.Fatalf("run 通过用例失败: %v\n%s", err, out)
	} else if !strings.Contains(string(out), `"ok"`) {
		t.Fatalf("run 通过用例返回值不对: %s", out)
	}
	for _, tc := range []struct{ name, input, want string }{
		{"require", failRequire, "contract violation: require"},
		{"ensure", failEnsure, "contract violation: ensure"},
	} {
		out, err := exec.Command(bin, "run", unit, "--input", tc.input).CombinedOutput()
		if err == nil {
			t.Fatalf("run %s 应失败，实际成功: %s", tc.name, out)
		}
		if !strings.Contains(string(out), tc.want) {
			t.Fatalf("run %s 的错误不对: %s", tc.name, out)
		}
	}

	// AOT：同样的三组输入，产物必须给出同样的结论
	prog := filepath.Join(dir, "c.bin")
	if out, err := exec.Command(bin, "compile", src, "-o", prog).CombinedOutput(); err != nil {
		t.Fatalf("compile: %v\n%s", err, out)
	}
	if out, err := exec.Command(prog, `{"budget":2}`).Output(); err != nil || !strings.Contains(string(out), "ok") {
		t.Fatalf("AOT 通过用例失败: %v %s", err, out)
	}
	for _, tc := range []struct{ name, arg, want string }{
		{"require", `{"budget":0}`, "contract violation: require"},
		{"ensure", `{"budget":1}`, "contract violation: ensure"},
	} {
		out, err := exec.Command(prog, tc.arg).CombinedOutput()
		if err == nil {
			t.Fatalf("AOT %s 应失败，实际成功: %s", tc.name, out)
		}
		if !strings.Contains(string(out), tc.want) {
			t.Fatalf("AOT %s 的错误不对: %s", tc.name, out)
		}
	}
}
