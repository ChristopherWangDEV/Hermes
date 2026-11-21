package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)


// lspScriptedSession 是**两版 LSP 共用**的脚本化会话。
//
// 共用一份而不是各写一份：两版的 golden 若各自维护，"两边一起漂移"就永远
// 不会被发现（都跟着改，测试都绿）。
func lspScriptedSession() []string {
	src := "export workflow w() entry -> string {\n  auto x = ;\n  return \"ok\";\n}\n"
	uri := "file:///tmp/hermes-lsp-probe.hermes"
	srcJSON := lspMustJSON(src)
	return []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"initialized","params":{}}`,
		`{"jsonrpc":"2.0","method":"textDocument/didOpen","params":{"textDocument":{"uri":"` + uri + `","languageId":"hermes","version":1,"text":` + srcJSON + `}}}`,
		`{"jsonrpc":"2.0","method":"textDocument/didChange","params":{"textDocument":{"uri":"` + uri + `","version":2},"contentChanges":[{"text":` + srcJSON + `}]}}`,
		// 补全：纯表驱动（关键字 + 内建命名空间），不依赖编辑位置。
		`{"jsonrpc":"2.0","id":3,"method":"textDocument/completion","params":{"textDocument":{"uri":"` + uri + `"},"position":{"line":1,"character":10}}}`,
		// hover：光标落在 `parallel` 之外的**关键字**上（`return`），走 keywordDoc。
		`{"jsonrpc":"2.0","id":4,"method":"textDocument/hover","params":{"textDocument":{"uri":"` + uri + `"},"position":{"line":1,"character":3}}}`,
		// documentSymbol：从 AST dump 里挑 WorkflowDecl / StructDecl / Backend*。
		`{"jsonrpc":"2.0","id":5,"method":"textDocument/documentSymbol","params":{"textDocument":{"uri":"` + uri + `"}}}`,
		// semanticTokens：行内词法 + legend 索引，按 LSP 要求增量编码。
		`{"jsonrpc":"2.0","id":6,"method":"textDocument/semanticTokens/full","params":{"textDocument":{"uri":"` + uri + `"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"shutdown","params":{}}`,
		`{"jsonrpc":"2.0","method":"exit","params":{}}`,
	}
}

// TestLSPGoldenSession 是 LSP 迁移的**判据**：把一段脚本化的 JSON-RPC 会话
// 喂给 `hermes lsp`，把服务端的**全部输出帧**记成 golden。
//
// 为什么要先建这条：LSP 是"输入一段消息、输出若干帧"的交互进程，任何协议细节
// （帧格式、能力声明、诊断内容、响应顺序）都不能靠肉眼 review 保证不变。
// C++ 版实现必须与这份 golden 逐帧相同，否则就不是"迁移"，是"换了个服务端"。
//
// 生成/更新：HERMES_UPDATE_LSP_GOLDEN=1。
func TestLSPGoldenSession(t *testing.T) {
	root := repoRootForBackend(t)
	bin := filepath.Join(root, "build", "hermes")
	dir := filepath.Join(root, "tests", "lsp", "golden")
	if os.Getenv("HERMES_UPDATE_LSP_GOLDEN") == "1" {
		_ = os.MkdirAll(dir, 0o755)
	}

	// 一份**故意带错**的源码：`auto x = ;` 会触发 E1001，从而产出 diagnostics 帧。
	msgs := lspScriptedSession()

	var in bytes.Buffer
	for _, m := range msgs {
		fmt.Fprintf(&in, "Content-Length: %d\r\n\r\n%s", len(m), m)
	}

	cmd := exec.Command(bin, "lsp")
	cmd.Stdin = &in
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("hermes lsp 退出码非零: %v", err)
	}

	frames, ferr := splitFrames(out)
	if ferr != nil {
		t.Fatalf("输出不是合法 LSP 帧: %v\n%s", ferr, out)
	}
	if len(frames) < 3 {
		t.Fatalf("只收到 %d 帧 —— 会话没有真正跑起来", len(frames))
	}
	// 版本号来自 semver，随发布变；其余字节必须稳定。
	for i := range frames {
		frames[i] = strings.ReplaceAll(frames[i], `"version":"`+semver+`"`, `"version":"<SEMVER>"`)
	}
	got := strings.Join(frames, "\n") + "\n"

	golden := filepath.Join(dir, "session.jsonl")
	if os.Getenv("HERMES_UPDATE_LSP_GOLDEN") == "1" {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("已写 %s（%d 帧）", golden, len(frames))
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("缺 golden（用 HERMES_UPDATE_LSP_GOLDEN=1 生成）: %v", err)
	}
	if strings.TrimSpace(string(want)) != strings.TrimSpace(got) {
		t.Fatalf("LSP 输出与 golden 不一致\n--- golden ---\n%s\n--- 现在 ---\n%s", want, got)
	}
	t.Logf("LSP golden：%d 帧一致", len(frames))
}

// TestCppLSPSessionMatchesGolden 是 **C++ 版 LSP 的迁移判据**：同一段脚本化
// 会话喂给 `build/hermes-lsp`，输出必须与 Go 版 golden 逐帧相同。
//
// 为什么两版共用一个 golden：LSP 的行为是"对同一输入的一组输出帧"，两版各自
// 的 golden 会掩盖"两边一起漂移"。共用一个，任何一侧变了都红。
func TestCppLSPSessionMatchesGolden(t *testing.T) {
	root := repoRootForBackend(t)
	bin := filepath.Join(root, "build", "hermes-lsp")
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("没有 build/hermes-lsp（先跑 scripts/build.sh）")
	}
	golden, err := os.ReadFile(filepath.Join(root, "tests", "lsp", "golden", "session.jsonl"))
	if err != nil {
		t.Fatalf("缺 golden: %v", err)
	}
	msgs := lspScriptedSession()
	var in strings.Builder
	for _, m := range msgs {
		fmt.Fprintf(&in, "Content-Length: %d\r\n\r\n%s", len(m), m)
	}
	cmd := exec.Command(bin)
	cmd.Stdin = strings.NewReader(in.String())
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("hermes-lsp 退出码非零: %v", err)
	}
	frames, ferr := splitFrames(out)
	if ferr != nil {
		t.Fatalf("输出不是合法 LSP 帧: %v\n%s", ferr, out)
	}
	for i := range frames {
		frames[i] = strings.ReplaceAll(frames[i], `"version":"`+semver+`"`, `"version":"<SEMVER>"`)
	}
	got := strings.Join(frames, "\n") + "\n"
	if strings.TrimSpace(string(golden)) != strings.TrimSpace(got) {
		t.Fatalf("C++ LSP 与 Go golden 不一致\n--- golden ---\n%s\n--- C++ ---\n%s", golden, got)
	}
	t.Logf("C++ LSP：%d 帧与 Go golden 一致", len(frames))
}

// splitFrames 把 "Content-Length: N\r\n\r\n<body>" 反复切出来。
func splitFrames(b []byte) ([]string, error) {
	r := bufio.NewReader(bytes.NewReader(b))
	var out []string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if strings.TrimSpace(line) == "" {
				return out, nil
			}
			return nil, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		const p = "Content-Length:"
		if !strings.HasPrefix(line, p) {
			return nil, fmt.Errorf("expected Content-Length, got %q", line)
		}
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(strings.TrimPrefix(line, p)), "%d", &n); err != nil {
			return nil, fmt.Errorf("bad length %q", line)
		}
		// 吃空行
		if _, err := r.ReadString('\n'); err != nil {
			return nil, err
		}
		body := make([]byte, n)
		if _, err := ioReadFull(r, body); err != nil {
			return nil, err
		}
		out = append(out, strings.TrimSpace(string(body)))
	}
}

func ioReadFull(r *bufio.Reader, b []byte) (int, error) {
	read := 0
	for read < len(b) {
		n, err := r.Read(b[read:])
		read += n
		if err != nil {
			return read, err
		}
	}
	return read, nil
}

func lspMustJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}
