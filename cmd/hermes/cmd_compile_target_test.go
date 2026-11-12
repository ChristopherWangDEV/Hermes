package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hermes/internal/toolchain"
)

// captureStderr 捕获一次 run() 写到 os.Stderr 的全部字节。fmt.Fprintf 在调用时
// 才解引用 os.Stderr，所以替换全局 fd 是可靠的；输出很小，不会撑满管道缓冲。
func captureStderr(t *testing.T, fn func() int) (int, string) {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	code := fn()
	_ = w.Close()
	os.Stderr = orig
	b, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatal(err)
	}
	return code, string(b)
}

func writeFakeLib(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("!<arch>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestBareMetalGoRuntimeRefusalIsVerbatim 是硬件层隔离的**命令面**验收：
// 不问 clang 在不在、不问 sysroot 有没有，用户指定的那句 [ERROR] 必须逐字
// 出现在 stderr 里，且不带 "hermes compile:" 前缀、不被本地化。
func TestBareMetalGoRuntimeRefusalIsVerbatim(t *testing.T) {
	gnetA := writeFakeLib(t, "gnet.a")
	gnetSo := writeFakeLib(t, "gnet.so.1")
	cases := []struct {
		name string
		flag string
		lib  string
	}{
		{"--link-static g*", "--link-static", gnetA},
		{"--link-dynamic g*", "--link-dynamic", gnetSo},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "prog")
			code, stderr := captureStderr(t, func() int {
				return run([]string{"compile", "testdata/hello.hermes", "-o", out,
					"--target", "thumbv7em-none-eabi", c.flag, c.lib})
			})
			if code != 4 {
				t.Fatalf("exit = %d, want 4 (stderr: %q)", code, stderr)
			}
			if strings.TrimSpace(stderr) != toolchain.GoRuntimeBareMetalMessage {
				t.Fatalf("stderr must be exactly the specified message, got %q", stderr)
			}
			if strings.HasPrefix(stderr, "hermes compile:") {
				t.Fatal("the [ERROR] line must not carry a command prefix")
			}
		})
	}
}

// TestBareMetalNeedNetRefusalIsVerbatim 证"程序自己需要 Go 网络库"这条隐式
// 路径同样被斩断，而不是只有用户显式 --link-* 才拦得住。
func TestBareMetalNeedNetRefusalIsVerbatim(t *testing.T) {
	// 这里必须用 struct 契约，不能写 input: json：后者会触发 W5005，
	// 警告会先落到 stderr，把下面「拒绝消息逐字且不夹带别的输出」的断言污染掉。
	// 本用例验的是 needNet（由 tool.call 触发）与入口契约无关，用 W5005 推荐的
	// 写法既保持断言强度，也让 fixture 本身是合规示例。
	src := `#link <std/net>

export struct OrderInfo {
    string id;
}

export struct SupportInput {
    string url;
}

export workflow te(input: SupportInput) entry -> string {
    auto u = input.url;
    try {
        auto r = tool.call<OrderInfo>(url = "{u}", method = "GET");
        return "ok";
    } catch (HttpError e) {
        return "caught";
    }
}
`
	dir := t.TempDir()
	file := filepath.Join(dir, "t.hermes")
	if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	// 即使环境变量指向一个 g* 归档，裸机目标也绝不读取它。
	t.Setenv("HERMES_NET_LIB", filepath.Join(dir, "glibhermesnet.a"))
	code, stderr := captureStderr(t, func() int {
		return run([]string{"compile", file, "-o", filepath.Join(dir, "t"),
			"--target", "thumbv7em-none-eabi"})
	})
	if code != 4 {
		t.Fatalf("exit = %d, want 4 (stderr: %q)", code, stderr)
	}
	if strings.TrimSpace(stderr) != toolchain.GoRuntimeBareMetalMessage {
		t.Fatalf("needNet on bare metal must produce the verbatim [ERROR], got %q", stderr)
	}
}

// TestCrossHostRefusesBuiltinGoArchive 证交叉编译确实被支持（不再 "not
// supported yet"），但宿主机构建的 Go c-archive 不能被塞进异架构产物。
func TestCrossHostRefusesBuiltinGoArchive(t *testing.T) {
	root, _ := filepath.Abs("../..")
	lib := filepath.Join(root, "build", "glibhermesnet.a")
	if _, err := os.Stat(lib); err != nil {
		t.Skipf("没有 %s（go build -buildmode=c-archive -o build/glibhermesnet.a ./cmd/hermesnet）", lib)
	}
	t.Setenv("HERMES_NET_LIB", lib)
	t.Setenv("HERMES_NET_HEADER", filepath.Join(root, "native", "include", "hermes_net.h"))
	src := `#link <std/net>

export struct OrderInfo { string id; }

export workflow te(input: json) entry -> string {
    auto u = json.as_string(input.url);
    try {
        auto r = tool.call<OrderInfo>(url = "{u}", method = "GET");
        return "ok";
    } catch (HttpError e) {
        return "caught";
    }
}
`
	dir := t.TempDir()
	file := filepath.Join(dir, "t.hermes")
	if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stderr := captureStderr(t, func() int {
		return run([]string{"compile", file, "-o", filepath.Join(dir, "t"),
			"--target", "x86_64-unknown-linux-gnu"})
	})
	if code != 4 {
		t.Fatalf("exit = %d, want 4 (stderr: %q)", code, stderr)
	}
	if !strings.Contains(stderr, "cross-compiled") {
		t.Fatalf("refusal must name the cross-compilation cause, got %q", stderr)
	}
	if strings.Contains(stderr, "not supported yet") {
		t.Fatal("cross-compilation must no longer be blanket-refused")
	}
}

// TestStaticLibcGateSeesLinuxTriple 钉住一个会空转的旧判据：静态链 glibc 的
// 闸门比较的是 GOOS，若把原始三元组字符串喂进去，比较恒不相等、闸门全绿。
func TestStaticLibcGateSeesLinuxTriple(t *testing.T) {
	t.Setenv("HERMES_ALLOW_GLIBC_STATIC", "")
	if err := staticLibcGate("linux", true); err == nil {
		t.Fatal("static linking on linux must be refused")
	}
	tgt, err := toolchain.ParseTarget("x86_64-unknown-linux-gnu", "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if err := staticLibcGate(tgt.GOOS, true); err == nil {
		t.Fatal("normalised GOOS from a linux triple must still be refused")
	}
	// 反向对照：证明这条断言确实能失败 —— 传原始 Raw 就会漏过去。
	if err := staticLibcGate(tgt.Raw, true); err != nil {
		t.Fatalf("raw triple would bypass the gate; this is exactly why cmdCompile passes tgt.GOOS: %v", err)
	}
	if err := staticLibcGate("darwin", true); err != nil {
		t.Fatalf("darwin must be allowed: %v", err)
	}
}

// TestProductLinkGateBareMetalRawFirmware 证产物侧闸门在裸机 raw firmware
// 上既不误杀（那类产物没有动态段概念），又能抓住计划里混入的 Go runtime。
func TestProductLinkGateBareMetalRawFirmware(t *testing.T) {
	raw := filepath.Join(t.TempDir(), "firmware.bin")
	if err := os.WriteFile(raw, []byte{0x00, 0x01, 0x02, 0x03}, 0o644); err != nil {
		t.Fatal(err)
	}
	bare := toolchain.Target{Class: toolchain.ClassBareMetal, GOOS: "none"}
	clean := toolchain.Plan{StaticLibs: []string{"libm.a"}}
	if err := productLinkGate(raw, false, bare, clean); err != nil {
		t.Fatalf("raw firmware must not be failed by the ELF/Mach-O probe: %v", err)
	}
	poisoned := toolchain.Plan{GoRuntimeLibs: []string{"glibhermesnet.a"}}
	if err := productLinkGate(raw, false, bare, poisoned); err == nil {
		t.Fatal("a plan carrying a Go runtime must be rejected on bare metal")
	}
	host := toolchain.Target{Class: toolchain.ClassHost, GOOS: "darwin"}
	if err := productLinkGate(raw, false, host, clean); err == nil {
		t.Fatal("hosted artifacts must stay fail-closed when dependencies are unreadable")
	}
}

// TestCppABIInputGateGivesBridgeGuidance 证纯 C ABI 守卫落在输入侧，并给出
// 可照抄的 extern "C" 修法，而不是把 ABI 原因和许可原因混成一句话。
func TestCppABIInputGateGivesBridgeGuidance(t *testing.T) {
	for _, name := range []string{"libc++.1.dylib", "libstdc++.so.6", "libc++abi.a"} {
		lib := writeFakeLib(t, name)
		err := linkInputGate("--link-static", lib)
		if err == nil {
			t.Fatalf("%s must be refused", name)
		}
		if toolchain.CodeOf(err) != toolchain.CodeCppABIBridge {
			t.Fatalf("%s code = %q, want %q", name, toolchain.CodeOf(err), toolchain.CodeCppABIBridge)
		}
		if !strings.Contains(err.Error(), `extern "C"`) {
			t.Fatalf("%s refusal must name the extern \"C\" bridge: %v", name, err)
		}
	}
}
