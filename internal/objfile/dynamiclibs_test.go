package objfile

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestDynamicLibsOnSystemBinary：真实二进制上先把"能读出来"这件事钉住。
//
// 拿 /usr/bin/true 当样本：它存在于 macOS 与所有主流 Linux，且是动态链接的
// （Alpine 那种 musl 静态 busybox 世界里会退化成 skip —— 静态产物没有
// DT_NEEDED，那不是错误）。
func TestDynamicLibsOnSystemBinary(t *testing.T) {
	path := "/usr/bin/true"
	if _, err := os.Stat(path); err != nil {
		t.Skipf("%s 不存在", path)
	}
	libs, err := DynamicLibs(path)
	if err != nil {
		t.Fatalf("DynamicLibs(%s): %v", path, err)
	}
	if len(libs) == 0 {
		t.Skip("本机 /usr/bin/true 是静态链接，没有动态依赖可查")
	}
	for _, l := range libs {
		if l == "" {
			t.Fatalf("返回了空库名：%v", libs)
		}
	}
	// 系统二进制不该被判成 GPL 运行时。这条同时守住"判据不会见谁都喊狼"。
	if bad := gplLike(libs); bad != "" {
		t.Fatalf("/usr/bin/true 被判成链了 GPL C++ 运行时（%s）：判据太宽", bad)
	}
}

// TestDynamicLibsSeesLibcxx：用 libc++ 编一个**真的用 std::string** 的程序，
// 断言动态依赖里能看到 libc++、看不到 libstdc++。
//
// 这正是旧判据（"mangled 即失败"）会误杀的形状：std::string 必然产生
// __ZNSt3__112basic_string… 这类未定义符号，而它们完全合法。
func TestDynamicLibsSeesLibcxx(t *testing.T) {
	cc, err := exec.LookPath("clang")
	if err != nil {
		t.Skip("没有 clang")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "uses_string.cpp")
	prog := `#include <string>
#include <cstdio>
int main() {
    std::string s("hi");
    std::printf("%zu\n", s.size());
    return s.size() == 2 ? 0 : 1;
}
`
	if err := os.WriteFile(src, []byte(prog), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "uses_string")
	args := []string{"-std=c++17", "-stdlib=libc++", "-O0", src, "-o", out}
	if b, err := exec.Command(cc, args...).CombinedOutput(); err != nil {
		// Linux 上没装 libc++ 时这一定会失败。那是环境缺件，不是判据有问题：
		// Hermes 宁可让 C++ 用户装 libc++，也不回落到 GPL 的 libstdc++。
		t.Skipf("本机无法用 -stdlib=libc++ 编译 C++（%v）\n%s", err, b)
	}

	libs, err := DynamicLibs(out)
	if err != nil {
		t.Fatalf("DynamicLibs: %v", err)
	}
	if len(libs) == 0 {
		t.Fatalf("用 libc++ 编出来的 C++ 程序竟然没有动态依赖：%v", libs)
	}
	if bad := gplLike(libs); bad != "" {
		t.Fatalf("libc++ 产物被判成 GPL 运行时（%s）", bad)
	}
	found := false
	for _, l := range libs {
		if strings.Contains(l, "libc++") {
			found = true
		}
	}
	if !found {
		t.Fatalf("动态依赖里没有 libc++ —— 判据漏掉了它想放行的那一份：%v", libs)
	}
}

// TestDynamicLibsRejectsUnknownFormat：不是 ELF 也不是 Mach-O 时必须报错，
// 而不是"返回空列表 + nil"—— 后者会让调用方把"读不懂"当成"很干净"。
func TestDynamicLibsRejectsUnknownFormat(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "not-a-binary")
	if err := os.WriteFile(p, []byte("### not a binary ###"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := DynamicLibs(p); err == nil {
		t.Fatal("非 ELF/Mach-O 必须报错")
	}
}

// gplLike 是测试侧的独立实现：**不**复用 cmd/hermes 里的 gplRuntime，
// 否则判据写错时两边一起错、测试照样绿。
func gplLike(libs []string) string {
	for _, l := range libs {
		b := strings.ToLower(filepath.Base(strings.ReplaceAll(l, "\\", "/")))
		if strings.Contains(b, "libstdc++") || strings.Contains(b, "libsupc++") {
			return l
		}
	}
	return ""
}

// TestFatMagicIsRecognised 守住 0xcafebabe 那一族：fat 产物如果被当 thin 读，
// 报错只会在多架构二进制上出现，而那种错误最容易被加 t.Skip 糊过去。
func TestFatMagicIsRecognised(t *testing.T) {
	cases := []struct {
		m    [4]byte
		fat  bool
		thin bool
	}{
		{[4]byte{0xca, 0xfe, 0xba, 0xbe}, true, false},
		{[4]byte{0xbe, 0xba, 0xfe, 0xca}, true, false},
		{[4]byte{0xca, 0xfe, 0xba, 0xbf}, true, false},
		{[4]byte{0xcf, 0xfa, 0xed, 0xfe}, false, true},
	}
	for _, c := range cases {
		if got := isFatMachOMagic(c.m); got != c.fat {
			t.Errorf("isFatMachOMagic(% x) = %v, want %v", c.m, got, c.fat)
		}
		if got := isMachOMagic(c.m); got != c.thin {
			t.Errorf("isMachOMagic(% x) = %v, want %v", c.m, got, c.thin)
		}
	}
}
