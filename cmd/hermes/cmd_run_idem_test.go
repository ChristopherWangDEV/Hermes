package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunRejectsTamperedRetryIR 守整条链路：手写 IR 文件 → hermes run →
// ir.Load → checkRetryIdempotency → 退出 4。
//
// 编译器侧的 E5007 只覆盖 hermes ir 的源码路径；这里刻意**绕过编译器**，
// 把一份合法 golden 的 tools[0].idem.mode 直接改成 none 再交给 run。
// 修复前 engine 的 retry 会照常重跑 body，POST 被重复发出。
func TestRunRejectsTamperedRetryIR(t *testing.T) {
	raw, err := os.ReadFile("../../schemas/golden/ir-idemkey.json")
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(raw), `"mode":"idem_key"`, `"mode":"none"`, 1)
	if tampered == string(raw) {
		t.Fatal("golden 里没找到 idem_key，变异没生效")
	}

	dir := t.TempDir()
	irPath := filepath.Join(dir, "tampered.ir.json")
	if err := os.WriteFile(irPath, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}

	stderrPath := filepath.Join(dir, "stderr.txt")
	f, err := os.Create(stderrPath)
	if err != nil {
		t.Fatal(err)
	}
	code := func() int {
		old := os.Stderr
		os.Stderr = f
		defer func() { os.Stderr = old }()
		return run([]string{"run", irPath})
	}()
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	msg, err := os.ReadFile(stderrPath)
	if err != nil {
		t.Fatal(err)
	}

	if code != 4 {
		t.Fatalf("未证明的 retry 必须阻塞执行（退出 4），实际 %d：\n%s", code, msg)
	}
	if !strings.Contains(string(msg), "E_IR_RETRY_UNPROVEN") {
		t.Fatalf("诊断里没有 E_IR_RETRY_UNPROVEN：\n%s", msg)
	}
	// 参数替换真的生效：诊断必须点名工具与 mode，而不是留着 {tool}/{mode}。
	if strings.Contains(string(msg), "{tool}") || strings.Contains(string(msg), "{mode}") {
		t.Fatalf("诊断模板占位符没替换：\n%s", msg)
	}
}
