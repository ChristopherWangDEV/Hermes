// 本文件用**真的 `hermes serve`** 校验 SDK 依赖的线格式。
//
// 为什么不能只靠各语言的假服务端：假服务端验的是"SDK 以为的契约"，
// 而这里验的是"服务端实际给的契约"。两者一旦分叉，SDK 会在用户那边才炸。
package guard_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startServe 编译一个 demo workflow 并起 `hermes serve`，返回 base URL。
func startServe(t *testing.T, dir string) string {
	t.Helper()
	root := repoRoot(t)
	bin := filepath.Join(root, "build", "hermes")
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("没有 %s（先跑 scripts/build.sh）", bin)
	}

	src := filepath.Join(dir, "sdk.hermes")
	if err := os.WriteFile(src, []byte(
		"export struct In { string text; }\n"+
			"export workflow echo(input: In) entry -> string { return \"echo: {input.text}\"; }\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	hbc := filepath.Join(dir, "sdk.hbc")
	if out, err := exec.Command(bin, "build", src, "-o", hbc).CombinedOutput(); err != nil {
		t.Fatalf("build 失败: %v\n%s", err, out)
	}

	// 选一个空闲端口：让内核分配，再立刻释放（够用，且比写死端口稳）。
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()

	cmd := exec.Command(bin, "serve", hbc, "--addr", addr, "--token", "t0ken")
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	base := "http://" + addr
	// 等它起来：/health 不需要 token，最适合探活。
	for i := 0; i < 100; i++ {
		resp, err := http.Get(base + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return base
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("hermes serve 没起来")
	return ""
}

// TestServeContractForSDKs 覆盖所有 SDK 依赖的四个行为。
func TestServeContractForSDKs(t *testing.T) {
	dir := t.TempDir()
	base := startServe(t, dir)

	client := &http.Client{Timeout: 10 * time.Second}

	t.Run("health 不需要 token", func(t *testing.T) {
		resp, err := client.Get(base + "/health")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("/health 应 200，实际 %d", resp.StatusCode)
		}
	})

	t.Run("models 需要 token", func(t *testing.T) {
		resp, err := client.Get(base + "/v1/models")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("无 token 应 401，实际 %d", resp.StatusCode)
		}

		req, _ := http.NewRequest("GET", base+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer t0ken")
		resp, err = client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var payload struct {
			Data []struct {
				ID      string `json:"id"`
				OwnedBy string `json:"owned_by"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Data) != 1 || payload.Data[0].ID != "echo" || payload.Data[0].OwnedBy != "hermes" {
			t.Fatalf("/v1/models 内容不对：%+v", payload.Data)
		}
	})

	t.Run("input 是显式字段", func(t *testing.T) {
		body := `{"model":"echo","input":{"text":"hi"}}`
		req, _ := http.NewRequest("POST", base+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer t0ken")
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var payload struct {
			Model   string `json:"model"`
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Choices) == 0 || payload.Choices[0].Message.Content != "echo: hi" {
			t.Fatalf("结果不对：%+v", payload)
		}
	})

	t.Run("流式是 SSE 且以 [DONE] 收尾", func(t *testing.T) {
		body := `{"model":"echo","input":{"text":"hi"},"stream":true}`
		req, _ := http.NewRequest("POST", base+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer t0ken")
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
			t.Fatalf("流式应 text/event-stream，实际 %q", ct)
		}
		raw, _ := io.ReadAll(resp.Body)
		text := string(raw)
		if !strings.Contains(text, "data: [DONE]") {
			t.Fatalf("缺 [DONE]：%s", text)
		}
		if !strings.Contains(text, `"object":"chat.completion.chunk"`) {
			t.Fatalf("缺 chunk 对象：%s", text)
		}
	})
}

// TestPythonSDKAgainstRealServe 用真 SDK 打真服务端 —— 这才是"SDK 能用"的证据。
//
// 没有 python3 时跳过。
func TestPythonSDKAgainstRealServe(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("没有 python3")
	}
	dir := t.TempDir()
	base := startServe(t, dir)

	root := repoRoot(t)
	script := filepath.Join(dir, "check.py")
	code := fmt.Sprintf(`
import sys
sys.path.insert(0, %q)
from hermes import Client
c = Client(%q, token="t0ken")
assert c.models() == ["echo"], c.models()
r = c.run("echo", {"text": "hi"})
assert r.content == "echo: hi", r.content
chunks = "".join(c.chat_stream([{"role":"user","content":"yo"}]))
assert "echo:" in chunks, chunks
print("python sdk ok")
`, filepath.Join(root, "packages", "python"), base)
	if err := os.WriteFile(script, []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(py, script)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("Python SDK 打真服务端失败: %v\n%s\n%s", err, out, stderr.String())
	}
	if !strings.Contains(string(out), "python sdk ok") {
		t.Fatalf("输出不对: %s", out)
	}
	_ = bufio.NewReader
}
