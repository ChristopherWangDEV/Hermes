package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"hermes/internal/objfile"
)

// ccEnv 准备一次 cc 验收需要的东西：一个 hello.hbc + 运行时库路径。
//
// 缺任何一件都 skip（不是失败）：两样都由 scripts/build.sh 产出，而
// "没先构建"不是这个测试要证明的事。
func ccEnv(t *testing.T) (dir, hbc, lib string) {
	t.Helper()
	root := "../.."
	lib = filepath.Join(root, "build", "libhermesrt.a")
	if _, err := os.Stat(lib); err != nil {
		t.Skipf("没有 %s（先跑 scripts/build.sh）", lib)
	}
	if _, err := exec.LookPath("clang"); err != nil {
		t.Skip("没有 clang")
	}
	dir = t.TempDir()
	hbc = filepath.Join(dir, "hello.hbc")
	if code := run([]string{"build", "testdata/hello.hermes", "-o", hbc}); code != 0 {
		t.Fatalf("build 退出码 %d", code)
	}
	return dir, hbc, lib
}

// TestCCLinksUserProgram 是 T48 的验收：**用户自己的 C 程序**调 workflow 并跑出结果。
//
// 这是"可嵌入"这句话的可执行形式 —— 不是 hermes 自己的二进制能跑，
// 而是别人写的 C 程序能链上它。
func TestCCLinksUserProgram(t *testing.T) {
	dir, hbc, lib := ccEnv(t)

	src := filepath.Join(dir, "user.c")
	// 用户程序只 include 生成的头，不 include 任何 hermes 内部实现。
	prog := `#include "hello.h"

int main(void) {
    hermes_buf in;
    hermes_buf out;
    hermes_status st;
    in.ptr = 0; in.len = 0; out.ptr = 0; out.len = 0;
    st = hermes_hello_hello(&in, &out);
    if (st != HERMES_OK) { return 1; }
    return out.len == 2 ? 0 : 2;
}
`
	if err := os.WriteFile(src, []byte(prog), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "prog")
	if code := run([]string{"cc", src, "--unit", hbc, "-o", out, "--runtime-lib", lib}); code != 0 {
		t.Fatalf("cc 退出码 %d", code)
	}
	if err := exec.Command(out).Run(); err != nil {
		t.Fatalf("用户程序运行失败: %v", err)
	}
}

// TestCCCompilesCPPSource 是这次改动的正面验收：C++ 翻译单元能编、能链、能跑。
//
// 源文件**故意**用 `std::string`：那是旧判据（"未定义符号里有 mangled 即硬失败"）
// 必然误杀的形状 —— macOS 只有 libc++，任何用标准库的 C++ TU 都会带
// `__ZNSt3__112basic_string…`。所以这条测试同时钉住"假阳性已经消失"。
func TestCCCompilesCPPSource(t *testing.T) {
	dir, hbc, lib := ccEnv(t)

	src := filepath.Join(dir, "user.cpp")
	prog := `#include "hello.h"
#include <string>
#include <cstring>

int main() {
    hermes_buf in;
    hermes_buf out;
    in.ptr = 0; in.len = 0; out.ptr = 0; out.len = 0;
    if (hermes_hello_hello(&in, &out) != HERMES_OK) { return 1; }
    // 用 std::string 是刻意的：它把 libc++ 的符号带进链接，也把
    // "mangled 符号到底说明了什么"这件事变成可执行事实。
    std::string body(out.ptr ? out.ptr : "", out.len);
    hermes_buf_free(&out);
    return body.size() == 2 ? 0 : 2;
}
`
	if err := os.WriteFile(src, []byte(prog), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "prog_cpp")
	if code := run([]string{"cc", src, "--unit", hbc, "-o", out, "--runtime-lib", lib}); code != 0 {
		t.Fatalf("cc 退出码 %d（C++ 源）", code)
	}
	if err := exec.Command(out).Run(); err != nil {
		t.Fatalf("C++ 用户程序运行失败: %v", err)
	}
}

// TestCCArgsCPPFlags 断言 C++ 路径的开关集合。
//
// 纯函数断言的理由：许可红线就落在这几个开关上，而"顺手加个 -lstdc++ 让某个
// 用户编过"是这类红线最典型的死法 —— 它不会让任何功能测试变红。
func TestCCArgsCPPFlags(t *testing.T) {
	args := ccArgsCPP("/tmp/x.cpp",
		[]string{"/tmp/shim.o", "/tmp/core.o"},
		"/tmp/libhermesrt.a", "/tmp/prog", "/tmp/dir", "/tmp/inc", "/tmp/rt")
	joined := strings.Join(args, " ")

	for _, want := range []string{"--driver-mode=g++", "-std=c++17", "-stdlib=libc++"} {
		if !strings.Contains(joined, want) {
			t.Errorf("缺少 %q：%v", want, args)
		}
	}
	for _, bad := range []string{"-lstdc++", "-lsupc++", "-static-libstdc++", "-std=c89", "-std=gnu++"} {
		if strings.Contains(joined, bad) {
			t.Errorf("不该出现 %q（GPL 运行时或语言版本由红线固定）：%v", bad, args)
		}
	}

	// `-x c++` 必须只罩住源文件：它后面的 -x none 一旦被人删掉，.o 会被当
	// C++ 源码再编一遍 —— 那个错误信息完全看不出根因。
	xi := indexOf(args, "-x")
	if xi < 0 || xi+1 >= len(args) || args[xi+1] != "c++" {
		t.Fatalf("没有 `-x c++`：%v", args)
	}
	if args[xi+2] != "/tmp/x.cpp" {
		t.Fatalf("`-x c++` 之后必须紧跟源文件：%v", args)
	}
	if args[xi+3] != "-x" || args[xi+4] != "none" {
		t.Fatalf("`-x c++` 之后必须紧跟 `-x none`（否则 .o 会被当源码）：%v", args)
	}
}

// TestCCRejectsUnknownSourceExtension：语言由扩展名决定，不认识的扩展名要说清
// 支持哪些，而不是猜一个语言硬编。
func TestCCRejectsUnknownSourceExtension(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "user.mm")
	if err := os.WriteFile(src, []byte("int main(){return 0;}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"cc", src, "--unit", filepath.Join(dir, "x.hbc")}); code != 2 {
		t.Fatalf("不支持的扩展名应退 2，实际 %d", code)
	}
}

// TestHermesExportsAreUnmangled 守真正该守的那条 ABI 承诺：
// 头里声明的 `hermes_<unit>_<wf>` 必须以**未修饰**形式定义在产物里。
//
// 这条替换了旧的 TestCCRejectsMangledSymbols：旧测试断言"任何 mangled 未定义
// 符号即硬失败"，而它本身就是那个缺口的化石（注释里写着"`.cpp` 会被 cc 以
// -std=c89 编译而失败，所以构造不出 mangled 引用"——于是 cc 支持 C++ 这件事
// 从来没被验证过）。
func TestHermesExportsAreUnmangled(t *testing.T) {
	dir, hbc, lib := ccEnv(t)

	src := filepath.Join(dir, "user.c")
	prog := `#include "hello.h"
int main(void) {
    hermes_buf in; hermes_buf out;
    in.ptr = 0; in.len = 0; out.ptr = 0; out.len = 0;
    return hermes_hello_hello(&in, &out) == HERMES_OK ? 0 : 1;
}
`
	if err := os.WriteFile(src, []byte(prog), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "prog_sym")
	if code := run([]string{"cc", src, "--unit", hbc, "-o", out, "--runtime-lib", lib}); code != 0 {
		t.Fatalf("cc 退出码 %d", code)
	}

	syms, err := objfile.Read(out)
	if err != nil {
		t.Fatalf("读符号表: %v", err)
	}
	want := "hermes_hello_hello"
	if runtime.GOOS == "darwin" {
		want = objfile.MachOName(want)
	}
	if !syms.Defined[want] {
		t.Fatalf("%s 未定义：头里承诺的符号没有落地", want)
	}
	if objfile.Mangled(want) {
		t.Fatalf("%s 不该被判为 mangled", want)
	}
	// 同一批导出符号一个都不许以修饰名出现（shim 被当 C++ 编时的症状）。
	for name := range syms.Defined {
		if objfile.Mangled(name) && strings.Contains(name, "hermes_hello_hello") {
			t.Fatalf("导出符号以修饰名出现：%s", name)
		}
	}
}

// TestGPLRuntimeDetection 是判据本身的单测：抓 libstdc++/libsupc++，放行 libc++。
func TestGPLRuntimeDetection(t *testing.T) {
	cases := []struct {
		lib  string
		want bool
	}{
		{"@rpath/libstdc++.6.dylib", true},
		{"/usr/lib/x86_64-linux-gnu/libstdc++.so.6", true},
		{"libsupc++.a", true},
		{"@rpath/libc++.1.dylib", false},
		{"/usr/lib/libc++abi.dylib", false},
		{"libc++.so.1", false},
		{"/usr/lib/libSystem.B.dylib", false},
	}
	for _, c := range cases {
		got := gplRuntime([]string{c.lib}) != ""
		if got != c.want {
			t.Errorf("gplRuntime(%q) = %v, want %v", c.lib, got, c.want)
		}
	}
	if gplRuntime(nil) != "" {
		t.Error("空列表不该报 GPL")
	}
}

// TestCXXStdlibHint：C++ 编译失败时必须补一句人话，而不是让人自己猜
// "为什么不用系统默认的 libstdc++"。
func TestCXXStdlibHint(t *testing.T) {
	if h := cxxStdlibHint([]byte("fatal error: 'cstddef' file not found")); h == "" {
		t.Fatal("找不到 libc++ 头时应给出提示")
	}
	if h := cxxStdlibHint([]byte("error: use of undeclared identifier 'x'")); h != "" {
		t.Fatalf("普通语法错不该被当成本机缺 libc++：%s", h)
	}
}

func indexOf(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

// ccErrHost 是 examples/c-minimal/main.c 那种宿主形状的最小自验收程序：
// 一次故意失败（非法 JSON 输入）+ 一次成功，两次都读 hermes_last_error()。
// 同一份源码分别以 .c / .cpp 交给 `hermes cc`，覆盖 C 与 C++ 两条编译路径；
// C++ 侧刻意用 std::string，把 libc++ 符号带进链接。
//
// 失败时的 error 视图**不 free**：它指向共享静态槽，调用方只借不还。
const ccErrHost = `#include <stdio.h>
#include <string.h>
#ifdef __cplusplus
#include <string>
#endif
#include "hello.h"

int main(void) {
  hermes_buf in;
  hermes_buf out;
  hermes_status st;
  hermes_str e;
  const char* bad = "{not-json";

  in.ptr = (char*)bad; in.len = strlen(bad);
  out.ptr = 0; out.len = 0;
  st = hermes_hello_hello(&in, &out);
  if (st == HERMES_OK) { printf("BAD_FAIL_STATUS\n"); return 1; }
  e = hermes_last_error();
  if (e.ptr == 0 || e.len == 0 || e.ptr[0] == 0) { printf("BAD_EMPTY\n"); return 1; }
  printf("FAIL_REASON:%.*s\n", (int)e.len, e.ptr);
  printf("FAIL_LEN_OK:%d\n", (int)(e.len > 0));

  /* 入口清槽：成功调用后必须是 len == 0 且 ptr 指向非 NULL 的空串。 */
  in.ptr = 0; in.len = 0;
  out.ptr = 0; out.len = 0;
  st = hermes_hello_hello(&in, &out);
  if (st != HERMES_OK) { printf("BAD_OK_STATUS\n"); return 1; }
  e = hermes_last_error();
  printf("OK_LEN_ZERO:%d\n", (int)(e.len == 0 && e.ptr != 0 && e.ptr[0] == 0));
#ifdef __cplusplus
  { std::string body(out.ptr ? out.ptr : "", out.len); printf("CPP_BODY:%d\n", (int)body.size()); }
#endif
  hermes_buf_free(&out);
  return 0;
}
`

// TestCCLastErrorReportsRealReason 是 `hermes cc` 路径的端到端验收：宿主真编译、
// 真链接、真运行，失败调用拿到非空且含真实原因的文本，成功调用后 len == 0。
func TestCCLastErrorReportsRealReason(t *testing.T) {
	for _, c := range []struct{ name, ext string }{{"C", ".c"}, {"C++", ".cpp"}} {
		t.Run(c.name, func(t *testing.T) {
			dir, hbc, lib := ccEnv(t)
			src := filepath.Join(dir, "user"+c.ext)
			if err := os.WriteFile(src, []byte(ccErrHost), 0o644); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(dir, "prog_last_error")
			if code := run([]string{"cc", src, "--unit", hbc, "-o", out, "--runtime-lib", lib}); code != 0 {
				t.Fatalf("cc 退出码 %d（%s 源）", code, c.name)
			}
			got, err := exec.Command(out).CombinedOutput()
			if err != nil {
				t.Fatalf("宿主程序运行失败: %v\n%s", err, got)
			}
			s := string(got)
			if strings.Contains(s, "BAD_") {
				t.Fatalf("宿主自检失败:\n%s", s)
			}
			for _, want := range []string{
				"FAIL_REASON:input is not valid JSON",
				"FAIL_LEN_OK:1",
				"OK_LEN_ZERO:1",
			} {
				if !strings.Contains(s, want) {
					t.Errorf("输出缺少 %q，实际:\n%s", want, s)
				}
			}
			if c.ext == ".cpp" && !strings.Contains(s, "CPP_BODY:2") {
				t.Errorf("C++ 路径的 std::string 分支没跑到:\n%s", s)
			}
		})
	}
}
