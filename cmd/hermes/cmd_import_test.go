package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// planFixture 是真实的 `homonoia compile --emit --json` 输出。
const planFixture = "../../internal/importhomo/testdata/plan-two-nodes.json"

// TestImportWritesFile 走一遍真实的 CLI 路径：读计划 → 写 .hermes。
func TestImportWritesFile(t *testing.T) {
	out := filepath.Join(t.TempDir(), "nested", "imported.hermes")

	if code := run([]string{"import", "--from-plan", planFixture, "-o", out}); code != 0 {
		t.Fatalf("exit=%d，期望 0", code)
	}

	src, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("没有生成 %s: %v", out, err)
	}
	text := string(src)

	for _, want := range []string{
		"backend homonoia",
		"workflow imported",
		"entry",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("产物里缺少 %q：\n%s", want, text)
		}
	}

	// 模型清单只能是注释：可执行部分里出现模型路径，运行期就会以它为准，
	// 而事实会与 /v1/models 分叉（D-023 ⑧）。
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.Contains(trimmed, ".gguf") {
			t.Errorf("模型路径进了可执行部分：%s", trimmed)
		}
	}
}

// TestImportUsageAndFailures 钉住退出码矩阵（0/2/4），不依赖具体文案。
func TestImportUsageAndFailures(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"缺 --from-plan", []string{"import"}, 2},
		{"未知参数", []string{"import", "--nope"}, 2},
		{"-o 缺值", []string{"import", "--from-plan", planFixture, "-o"}, 2},
		{"文件不存在", []string{"import", "--from-plan", "no-such-plan.json"}, 4},
		{"不是 JSON", []string{"import", "--from-plan", "cmd_import.go"}, 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := run(c.args); got != c.want {
				t.Fatalf("exit=%d，期望 %d", got, c.want)
			}
		})
	}
}

// TestImportStdoutIsOnlySource 证明 stdout 只有源码。
//
// 这条不是洁癖：`hermes import plan.json > x.hermes` 是最自然的用法，
// 一旦 warning 走了 stdout，那个文件里就混进了注释之外的诊断文本。
func TestImportStdoutIsOnlySource(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Skipf("当前环境不支持 pipe: %v", err)
	}
	old := os.Stdout
	os.Stdout = w
	code := run([]string{"import", "--from-plan", planFixture})
	os.Stdout = old
	w.Close()

	buf := make([]byte, 1<<16)
	n, _ := r.Read(buf)
	r.Close()

	if code != 0 {
		t.Fatalf("exit=%d，期望 0", code)
	}
	out := string(buf[:n])
	if !strings.Contains(out, "workflow imported") {
		t.Fatalf("stdout 里没有源码：\n%s", out)
	}
	if strings.Contains(out, "hermes import: 注意") {
		t.Fatalf("warning 混进了 stdout（`> x.hermes` 会把它写进文件）：\n%s", out)
	}
}
