package linktest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestNativeLinkMatchesGolden 用**冻结的期望值**校验 C++ 的 link。
//
// 这些期望值是在 Go 实现还存在时录下来的（见 TestRecordGolden）：退出码、
// stdout、stderr（逐字）、以及产物 sha256。删掉 Go 实现之后，它们是唯一能
// 证明"搬对了"的东西——尤其是 link 这种**搬错不会报错**的命令：
// 它会产出结构合法、符号解析却错了的归档，能跑，"跑的是谁"却说不清。
func TestNativeLinkMatchesGolden(t *testing.T) {
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
	var cases []goldenCase
	if err := json.Unmarshal(blob, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("golden.json 是空的")
	}

	dir := t.TempDir()
	buildFixtures(t, root, dir)

	for _, c := range cases {
		cmd := exec.Command(nativeBin, append([]string{"link"}, c.Args...)...)
		cmd.Dir = dir
		var stdout, stderr strings.Builder
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		if strings.Contains(stderr.String(), "is not ported yet") {
			t.Skip("native 还没有 link：先移植")
		}
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		if code != c.Exit {
			t.Fatalf("%s: 退出码 %d, 期望 %d\nstderr:\n%s", c.Name, code, c.Exit, stderr.String())
		}
		if stdout.String() != c.Stdout {
			t.Fatalf("%s: stdout 不同\n got: %q\nwant: %q", c.Name, stdout.String(), c.Stdout)
		}
		if stderr.String() != c.Stderr {
			t.Fatalf("%s: stderr 不同\n got: %q\nwant: %q", c.Name, stderr.String(), c.Stderr)
		}
		if c.OutSHA != "" {
			outPath := lastOut(c.Args)
			data, err := os.ReadFile(filepath.Join(dir, outPath))
			if err != nil {
				t.Fatalf("%s: 产物读不到: %v", c.Name, err)
			}
			sum := sha256.Sum256(data)
			if got := hex.EncodeToString(sum[:]); got != c.OutSHA {
				t.Fatalf("%s: 产物 sha256 不同\n got: %s\nwant: %s", c.Name, got, c.OutSHA)
			}
		}
	}
}
