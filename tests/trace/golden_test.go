package tracetest

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// traceCase 是一条 `hermes trace ...` 的**冻结期望**。
//
// `trace verify` 的语义是安全语义：把被篡改的 trace 判成干净，比不检查更糟。
// 所以移植前先把 Go 实现的输出（stdout / stderr / 退出码）逐字录下来，
// 之后任何实现都对照这批数字。
type traceCase struct {
	Name   string   `json:"name"`
	Args   []string `json:"args"`
	Exit   int      `json:"exit"`
	Stdout string   `json:"stdout"`
	Stderr string   `json:"stderr"`
}

// makeTraces 产出测试用的 trace：一次正常运行、一次"内容不同"的运行、
// 以及一份被篡改的副本（改一个事件字段，链就对不上）。
func makeTraces(t *testing.T, root, dir string) {
	t.Helper()
	bin := filepath.Join(root, "build", "hermes")
	src := filepath.Join(dir, "greet.hermes")
	if err := os.WriteFile(src, []byte(`export struct In { string name; }
export workflow main(input: In) entry -> string { return "hi"; }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(dir, "greet.hbc")
	if b, err := exec.Command(bin, "build", src, "-o", unit).CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, b)
	}
	in := filepath.Join(dir, "in.json")
	if err := os.WriteFile(in, []byte(`{"name":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"t1.json", "t2.json"} {
		out := filepath.Join(dir, name)
		if b, err := exec.Command(bin, "run", unit, "--input", in, "--trace", out).CombinedOutput(); err != nil {
			t.Fatalf("run: %v\n%s", err, b)
		}
	}
	// 篡改：把第一个事件的 status 改掉（链的根就对不上了）。
	raw, err := os.ReadFile(filepath.Join(dir, "t1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	evs, _ := doc["events"].([]any)
	if len(evs) == 0 {
		t.Fatal("trace 没有事件")
	}
	if m, ok := evs[0].(map[string]any); ok {
		m["status"] = "failed"
	}
	bad, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tampered.json"), bad, 0o644); err != nil {
		t.Fatal(err)
	}
}

func cases() []traceCase {
	return []traceCase{
		{Name: "verify-ok", Args: []string{"verify", "t1.json"}},
		{Name: "verify-ok-json", Args: []string{"verify", "t1.json", "--json"}},
		{Name: "verify-tampered", Args: []string{"verify", "tampered.json"}},
		{Name: "verify-tampered-json", Args: []string{"verify", "tampered.json", "--json"}},
		{Name: "summary", Args: []string{"summary", "t1.json"}},
		{Name: "summary-json", Args: []string{"summary", "t1.json", "--json"}},
		{Name: "diff", Args: []string{"diff", "t1.json", "t2.json"}},
		{Name: "diff-json", Args: []string{"diff", "t1.json", "t2.json", "--json"}},
	}
}

func TestRecordGolden(t *testing.T) {
	if os.Getenv("HERMES_RECORD_GOLDEN") != "1" {
		t.Skip("set HERMES_RECORD_GOLDEN=1 to record")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	makeTraces(t, root, dir)
	bin := filepath.Join(root, "build", "hermes")
	var out []traceCase
	for _, c := range cases() {
		cmd := exec.Command(bin, append([]string{"trace"}, c.Args...)...)
		cmd.Dir = dir
		var so, se stringsBuilder
		cmd.Stdout, cmd.Stderr = &so, &se
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		c.Exit, c.Stdout, c.Stderr = code, so.s, se.s
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

type stringsBuilder struct{ s string }

func (b *stringsBuilder) Write(p []byte) (int, error) { b.s += string(p); return len(p), nil }
