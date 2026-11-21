package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// cmd_compile_goarchive_test.go —— S2 milestone 1 的端到端回归：用户提供的
// Go c-archive（g* 命名空间）是 call_native 的一等 AOT 链接输入。
//
// 这条路径以前有两处错：
//   ① 只有 NeedNet=true 才加 darwin frameworks，于是 NeedNet=false 的 g*
//      c-archive 在链接期报 undefined _CFStringCreateWithBytes；
//   ② 成功文案写死 "no Go runtime"，与产物里真实存在的 Go runtime 矛盾。
//
// 下面用真正的 go build -buildmode=c-archive 造一份带 crypto/x509 的归档，
// 走真实 `hermes compile`、跑真实可执行文件，并核对 stderr 文案。

// goArchiveBareMetalMessage 是裸机拒绝的逐字契约，独立于被测常量再钉一份，
// 防止「改了常量、改了实现、测试跟着一起绿」。
const goArchiveBareMetalMessage = "[ERROR] Go runtime libraries (g*) cannot be linked for bare-metal targets. Please use pure C/Rust static libraries."

const goArchiveGoMod = "module gprobe\n\ngo 1.23\n"

// 前言只放声明（`//export` 所在文件的 preamble 会被 cgo 拷进两份 C 文件，
// 放定义会重复符号）。crypto/x509 的 macOS 根证书路径会调
// CoreFoundation/Security，这正是 frameworks 必须存在的原因。
const goArchiveMainGo = `package main

/*
#include <stdlib.h>
*/
import "C"

import "crypto/x509"

var _ = x509.SystemCertPool

//export gprobe_add
func gprobe_add(a, b C.long) C.long { return a + b }

//export gprobe_is_pos
func gprobe_is_pos(a C.long) C.int {
	if a > 0 {
		return 1
	}
	return 0
}

//export gprobe_echo_len
func gprobe_echo_len(p *C.char) C.long {
	if p == nil {
		return 0
	}
	return C.long(len(C.GoString(p)))
}

//export gprobe_mix
func gprobe_mix(a, b, c C.long) C.long { return a*100 + b*10 + c }

func main() {}
`

// 注意：#link 行末尾没有分号（W2003）。`from "gprobe.h"` 只是 FFI 元数据，
// 不会触发头文件读取；真正的符号由 clang/ld 在链接期解析。
const goArchiveAddSrc = `#link native "gprobe" from "gprobe.h"
extern int gprobe_add(int a, int b);
export workflow probe() entry -> int {
    return gprobe_add(6, 7);
}
`

const goArchiveIsPosSrc = `#link native "gprobe" from "gprobe.h"
extern bool gprobe_is_pos(int a);
export workflow probe() entry -> bool {
    return gprobe_is_pos(5);
}
`

const goArchiveEchoLenSrc = `#link native "gprobe" from "gprobe.h"
extern int gprobe_echo_len(string s);
export workflow probe() entry -> int {
    return gprobe_echo_len("abcdef");
}
`

const goArchiveMixSrc = `#link native "gprobe" from "gprobe.h"
extern int gprobe_mix(int a, int b, int c);
export workflow probe() entry -> int {
    return gprobe_mix(6, 7, 8);
}
`

// buildGoArchiveForTest 在 t.TempDir() 里的独立 module 中造一份真实 Go
// c-archive，返回 .a 的绝对路径。只依赖 Go 标准库与本机 clang，不联网、
// 不引入第三方依赖、不写仓库目录。
func buildGoArchiveForTest(t *testing.T) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("没有 go 工具链: %v", err)
	}
	if _, err := exec.LookPath("clang"); err != nil {
		t.Skipf("没有 clang: %v", err)
	}

	modDir := filepath.Join(t.TempDir(), "gprobe")
	if err := os.MkdirAll(modDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modDir, "go.mod"), []byte(goArchiveGoMod), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modDir, "main.go"), []byte(goArchiveMainGo), 0o644); err != nil {
		t.Fatal(err)
	}

	lib := filepath.Join(modDir, "gprobe.a")
	// 本机 Go 装在 ~/.local/go/bin；显式补进子进程 PATH，保证 cgo 找得到它
	// 与 clang。CGO_ENABLED=1 是 c-archive 的硬前提。
	env := append(os.Environ(),
		"PATH="+filepath.Join(os.Getenv("HOME"), ".local/go/bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
		"CGO_ENABLED=1",
	)
	cmd := exec.Command(goBin, "build", "-buildmode=c-archive", "-o", lib, ".")
	cmd.Dir = modDir
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build -buildmode=c-archive: %v\n%s", err, out)
	}
	if st, err := os.Stat(lib); err != nil || st.IsDir() {
		t.Fatalf("c-archive 未生成或不是普通文件: %v", err)
	}
	return lib
}

// TestCompileGoArchiveIsFirstClassLinkInput 用四个真实调用覆盖 int / bool /
// string 三种 ABI 映射与三参数混合调用，并核对成功文案不再谎报 "no Go runtime"。
func TestCompileGoArchiveIsFirstClassLinkInput(t *testing.T) {
	lib := buildGoArchiveForTest(t)
	dir := t.TempDir()

	cases := []struct {
		name string
		file string
		src  string
		want string
	}{
		{"int add", "add.hermes", goArchiveAddSrc, `"value":13`},
		{"bool is_pos", "is_pos.hermes", goArchiveIsPosSrc, `"value":true`},
		{"string echo_len", "echo_len.hermes", goArchiveEchoLenSrc, `"value":6`},
		{"int mix 3 args", "mix.hermes", goArchiveMixSrc, `"value":678`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := writeSource(t, dir, c.file, c.src)
			prog := filepath.Join(dir, "prog_"+strings.TrimSuffix(c.file, ".hermes"))

			code, msg := runCompileCaptured(t, src, "-o", prog, "--link-static", lib)
			if code != 0 {
				t.Fatalf("带 Go c-archive 的 compile 应成功，实际退出 %d：\n%s", code, msg)
			}
			if strings.Contains(msg, "no Go runtime") {
				t.Errorf("产物确实链了 Go runtime，成功文案不得再写 \"no Go runtime\"：\n%s", msg)
			}
			if !strings.Contains(msg, "Go-runtime libraries") {
				t.Errorf("成功文案应如实点名 Go-runtime libraries：\n%s", msg)
			}

			got, err := exec.Command(prog).Output()
			if err != nil {
				t.Fatalf("运行 AOT 产物失败: %v", err)
			}
			if !strings.Contains(string(got), c.want) {
				t.Errorf("stdout 应含 %s，实际 %q", c.want, string(got))
			}
		})
	}
}

// TestCompileGoArchiveBareMetalRefusal 证明 g* c-archive 在裸机目标上仍被
// 用户指定的那句 [ERROR] 逐字拒绝，且发生在任何链接动作之前。
func TestCompileGoArchiveBareMetalRefusal(t *testing.T) {
	lib := buildGoArchiveForTest(t)
	dir := t.TempDir()
	src := writeSource(t, dir, "bare.hermes", goArchiveAddSrc)
	prog := filepath.Join(dir, "prog_bare")

	code, msg := runCompileCaptured(t, src, "-o", prog,
		"--link-static", lib, "--target", "thumbv7em-none-eabi")
	if code != 4 {
		t.Fatalf("裸机目标链 g* 应退出 4，实际 %d：\n%s", code, msg)
	}
	if !strings.Contains(msg, goArchiveBareMetalMessage) {
		t.Fatalf("stderr 必须逐字含指定的 [ERROR]，实际 %q", msg)
	}
	if strings.TrimSpace(msg) != goArchiveBareMetalMessage {
		t.Fatalf("stderr 必须只有那一条 [ERROR]，实际 %q", msg)
	}
	if strings.HasPrefix(msg, "hermes compile:") {
		t.Fatal("[ERROR] 行不得带命令前缀")
	}
}
