package linktest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// goldenCase 是一次 `hermes link` 调用的**冻结期望**。
//
// 为什么要有它：`link` 搬错不会报错——它会产出一个结构合法、符号解析却错了的
// 归档。没有冻结的期望值，"搬对了"就只是自我感觉。所以先用 Go 实现跑一遍，
// 把退出码 / stdout / stderr / 产物 sha256 全部记下来，之后任何实现都对照这些数字。
type goldenCase struct {
	Name    string   `json:"name"`
	Args    []string `json:"args"`     // 相对 fixture 目录的参数
	Exit    int      `json:"exit"`     // 期望退出码
	Stdout  string   `json:"stdout"`   // 期望 stdout（通常为空）
	Stderr  string   `json:"stderr"`   // 期望 stderr（逐字）
	OutSHA  string   `json:"out_sha"`  // 产物 sha256（没有产物时为空）
}

const update = "" // 由 -update 标志控制，见下

var fixtures = map[string]string{
	"base.hermes": `export struct T { string id; }

export workflow one(input: T) entry -> string {
  return helper();
}

string helper() { return "h"; }
`,
	"dup.hermes": `export struct T { string id; }

export workflow one(input: T) entry -> string {
  return "other";
}
`,
	"odr.hermes": `export struct T { string id; int n; }

export workflow two(input: T) entry -> string {
  return "two";
}
`,
	"extra.hermes": `export struct U { string k; }

export workflow side(input: U) -> string {
  return "side";
}
`,
	"be1.hermes": `backend b = "http://127.0.0.1:1";

export struct B1 { string s; }
export workflow wb1(input: B1) entry -> string { return "b1"; }
`,
	"be2.hermes": `backend b = "http://127.0.0.1:2";

export struct B2 { string s; }
export workflow wb2(input: B2) entry -> string { return "b2"; }
`,
	"calc.h": `export struct R { int v; }
export workflow add(input: R) -> string;
`,
	"calc.hermes": `#include "calc.h"

workflow add(input: R) -> string { return "added"; }
`,
	"app.h": `#include "calc.h"

export workflow main2(input: R) entry -> string;
`,
	"app.hermes": `#include "app.h"

#link "calc"

workflow main2(input: R) -> string { return "app"; }
`,
}

func buildFixtures(t *testing.T, root, dir string) {
	t.Helper()
	bin := filepath.Join(root, "build", "hermes")
	// calc 必须先建：app 通过 #link "calc" 依赖它。
	for _, name := range []string{"calc", "app"} {
		_ = name
	}
	for name, body := range fixtures {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 先 build 不依赖别人的，再 build 依赖别人的（-L 指向同目录）。
	buildOne := func(stem string, extra ...string) {
		src := filepath.Join(dir, stem+".hermes")
		out := filepath.Join(dir, stem+".hbc")
		args := append([]string{"build", src, "-o", out}, extra...)
		if b, err := exec.Command(bin, args...).CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", stem, err, b)
		}
	}
	buildOne("calc")
	buildOne("app", "-L", dir)
	buildOne("base")
	buildOne("dup")
	buildOne("odr")
	buildOne("extra")
	buildOne("be1")
	buildOne("be2")
}

func cases() []goldenCase {
	return []goldenCase{
		{Name: "strip-unreachable", Args: []string{"base.hbc", "extra.hbc", "-o", "out1.hbc"}},
		{Name: "no-strip", Args: []string{"base.hbc", "extra.hbc", "-o", "out2.hbc", "--no-strip"}},
		{Name: "dup-workflow", Args: []string{"base.hbc", "dup.hbc", "-o", "out3.hbc"}},
		{Name: "odr-mismatch", Args: []string{"base.hbc", "odr.hbc", "-o", "out4.hbc"}},
		{Name: "missing-lib", Args: []string{"-l", "nosuch", "-o", "out5.hbc"}},
		{Name: "local-dep-tampered", Args: []string{"app.hbc", "-o", "out6.hbc"}},
		{Name: "local-dep-ok", Args: []string{"app.hbc", "calc.hbc", "-o", "out7.hbc"}},
		{Name: "dup-backend", Args: []string{"be1.hbc", "be2.hbc", "-o", "out8.hbc"}},
	}
}

// TestRecordGolden 只在显式 -update 时写 golden（不是常规测试）。
func TestRecordGolden(t *testing.T) {
	if os.Getenv("HERMES_RECORD_GOLDEN") != "1" {
		t.Skip("set HERMES_RECORD_GOLDEN=1 to record")
	}
	root, err0 := filepath.Abs(filepath.Join("..", ".."))
	if err0 != nil {
		t.Fatal(err0)
	}
	dir := t.TempDir()
	buildFixtures(t, root, dir)
	bin := filepath.Join(root, "build", "hermes")
	var out []goldenCase
	for _, c := range cases() {
		cmd := exec.Command(bin, append([]string{"link"}, c.Args...)...)
		cmd.Dir = dir
		var stdout, stderr string
		cmd.Stdout = &stringWriter{&stdout}
		cmd.Stderr = &stringWriter{&stderr}
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		c.Exit, c.Stdout, c.Stderr = code, stdout, stderr
		if outPath := lastOut(c.Args); outPath != "" {
			if data, err := os.ReadFile(filepath.Join(dir, outPath)); err == nil {
				sum := sha256.Sum256(data)
				c.OutSHA = hex.EncodeToString(sum[:])
			}
		}
		out = append(out, c)
	}
	blob, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("golden.json"), append(blob, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("recorded %d cases", len(out))
}

type stringWriter struct{ s *string }

func (w *stringWriter) Write(p []byte) (int, error) { *w.s += string(p); return len(p), nil }

func lastOut(args []string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "-o" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
