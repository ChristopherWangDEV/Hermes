package tracetest

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestNativeTraceMatchesGolden 用冻结的期望值校验 C++ 的 trace。
//
// `verify` 是安全语义：把被篡改的 trace 判成干净比不检查更糟。所以判据不是
// "能跑"，而是 stdout / stderr / 退出码**逐字相同**——包括篡改时指出的块号。
func TestNativeTraceMatchesGolden(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	nativeBin := filepath.Join(root, "build", "hermes-native")
	if _, err := os.Stat(nativeBin); err != nil {
		t.Skip("hermes-native 未构建")
	}
	blob, err := os.ReadFile("golden.json")
	if err != nil {
		t.Fatalf("读 golden.json: %v", err)
	}
	var cases []traceCase
	if err := jsonUnmarshal(blob, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("golden.json 是空的")
	}
	dir := t.TempDir()
	makeTraces(t, root, dir)
	for _, c := range cases {
		cmd := exec.Command(nativeBin, append([]string{"trace"}, c.Args...)...)
		cmd.Dir = dir
		var so, se strings.Builder
		cmd.Stdout, cmd.Stderr = &so, &se
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		if code != c.Exit {
			t.Fatalf("%s: 退出码 %d, 期望 %d\nstderr:\n%s", c.Name, code, c.Exit, se.String())
		}
		if so.String() != c.Stdout {
			t.Fatalf("%s: stdout 不同\n got: %q\nwant: %q", c.Name, so.String(), c.Stdout)
		}
		if se.String() != c.Stderr {
			t.Fatalf("%s: stderr 不同\n got: %q\nwant: %q", c.Name, se.String(), c.Stderr)
		}
	}
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
