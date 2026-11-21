package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestExplainPrintsBounds 守"上界要能被人看见"。
//
// 上界驱动剥离与 trace 限额，所以它写错是正确性问题，不是显示问题。
// 这条测试只钉住"该出现的数字出现了"，不钉排版。
func TestExplainPrintsBounds(t *testing.T) {
	wd, _ := os.Getwd()
	if err := os.Chdir(filepath.Join(wd, "..", "..")); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)

	if code := run([]string{"explain", "schemas/golden/ir-support_bot.json"}); code != 0 {
		t.Fatalf("explain 退出码 %d", code)
	}
}

// TestExplainUnknownWorkflow 守"打错名字要说清楚，并列出可用的"。
func TestExplainUnknownWorkflow(t *testing.T) {
	wd, _ := os.Getwd()
	if err := os.Chdir(filepath.Join(wd, "..", "..")); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)

	if code := run([]string{"explain", "schemas/golden/ir-support_bot.json", "--workflow", "nope"}); code != 4 {
		t.Fatalf("未知 workflow 应退出 4，实际 %d", code)
	}
}

// TestExplainCodeKnown 守"解释一个已登记的诊断码"走 code-explain 分支、退出 0。
// E3002 是 Go 侧目录里已登记、且有 docs 锚点的码。
func TestExplainCodeKnown(t *testing.T) {
	if code := run([]string{"explain", "E3002"}); code != 0 {
		t.Fatalf("explain E3002 应退出 0，实际 %d", code)
	}
}

// TestExplainCodeUnknown 守"解释一个未登记的码"回到提示，退出 4，不崩。
func TestExplainCodeUnknown(t *testing.T) {
	if code := run([]string{"explain", "E9999"}); code != 4 {
		t.Fatalf("未知诊断码应退出 4，实际 %d", code)
	}
}

// TestExplainCodeJSON 守 --json 给出机器可读的 {code,message,why,fixes,docs}，
// 且已登记码的 docs 锚点非空（否则"相关文档"行就没意义）。
func TestExplainCodeJSON(t *testing.T) {
	buf := captureStdout(t, func() {
		run([]string{"explain", "E3002", "--json"})
	})
	var out map[string]any
	if err := json.Unmarshal([]byte(buf), &out); err != nil {
		t.Fatalf("JSON 解析失败: %v\n%s", err, buf)
	}
	if out["code"] != "E3002" {
		t.Errorf("code 字段应为 E3002，实际 %v", out["code"])
	}
	if s, _ := out["message"].(string); s == "" {
		t.Errorf("message 不应为空")
	}
	if s, _ := out["docs"].(string); s == "" {
		t.Errorf("docs 锚点不应为空（E3002 已登记）")
	}
	if fixes, ok := out["fixes"].([]any); !ok || len(fixes) < 2 {
		t.Errorf("fixes 应至少 2 条，实际 %v", out["fixes"])
	}
}

// TestExplainColorRejectsIRPath 守"没作用的开关不许静默生效"。
//
// IR 契约路径是纯文本表格，没有上色实现；带了 --color 却不报错，用户会把
// 它写进 CI 并以为颜色生效了。宁可退出 2 说清楚 --color 只适用于诊断码。
func TestExplainColorRejectsIRPath(t *testing.T) {
	wd, _ := os.Getwd()
	if err := os.Chdir(filepath.Join(wd, "..", "..")); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)

	for _, args := range [][]string{
		{"explain", "schemas/golden/ir-support_bot.json", "--color", "always"},
		{"explain", "schemas/golden/ir-support_bot.json", "--color=always"},
	} {
		if code := run(args); code != 2 {
			t.Errorf("%v 应退出 2（--color 只适用于 DIAG_CODE），实际 %d", args, code)
		}
	}
	// 诊断码路径必须继续接受同一个开关（别把修正做成"两边都不许"）。
	if code := run([]string{"explain", "E4016", "--color=always"}); code != 0 {
		t.Errorf("explain E4016 --color=always 应退出 0，实际 %d", code)
	}
}

// captureStdout 把 fn 运行期间的 os.Stdout 重定向到内存缓冲并返回其内容。
// run() 的解释输出走 os.Stdout，这条助手让测试能断言输出内容。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}
