package guard_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// C++ 标准库边界守卫（红线：不允许含 GPL 类传染性协议）
//
// 为什么需要它：scripts/check_toolchain.sh 的第 1b 段只能保证**构建时**
// 用的是 libc++。但静态库是要被别人链接的：
//
//   libhermes_parser.a 内部是 C++（用了 <vector>），它有 undefined 的
//   operator new/delete（实测 5 个）。这类符号由**链接方的**标准库解析 ——
//   复用者若用 g++ 链接，就会从 libstdc++（GPL-3.0 WITH GCC Runtime Library
//   Exception）解析它们，于是"我们构建时合规"变成"用户链接后不合规"。
//
// 构建时合规 ≠ 分发后合规。这一层只有扫符号表能看见。
//
// 两个产物因此有不同的契约，守卫分别断言：
//   · libhermes_native.a（C + Rust，staticlib + extern "C"）必须**零** C++
//     标准库符号 —— 它可以被任何编译器链，包括纯 C 项目。
//   · libhermes_parser.a 允许有 libc++ 符号，但**必须零 libstdc++ 符号**，
//     且复用约束要写进文档（见 TestParserArchiveConsumersMustUseLibcxx 上方的注释）。
// ---------------------------------------------------------------------------

// nmOn 对一个静态库跑 nm -C（demangle），返回全部符号行。
// nm 在 macOS 上来自 cctools/llvm（与 clang 同类许可），在 Linux CI 上是
// binutils —— **检测工具不属于分发物**，使用不构成许可义务。
func nmOn(t *testing.T, archive string, args ...string) []string {
	t.Helper()
	if _, err := os.Stat(archive); err != nil {
		t.Fatalf("%s 不存在（%v）：请先跑 scripts/build.sh。红线检查不能因为没构建就跳过。", archive, err)
	}
	argv := append([]string{"-C"}, args...)
	argv = append(argv, archive)
	out, err := exec.Command("nm", argv...).Output()
	if err != nil {
		// nm 对空归档会返回非 0；有输出时仍继续。
		if len(out) == 0 {
			t.Fatalf("nm %v 失败：%v", argv, err)
		}
	}
	return strings.Split(string(out), "\n")
}

func countContaining(lines []string, needle string) int {
	n := 0
	for _, l := range lines {
		if strings.Contains(l, needle) {
			n++
		}
	}
	return n
}

// TestNativeArchiveIsFreeOfCxxStdlib：运行时库必须能被纯 C 项目链接。
func TestNativeArchiveIsFreeOfCxxStdlib(t *testing.T) {
	root := repoRoot(t)
	a := filepath.Join(root, "build", "libhermes_native.a")
	lines := nmOn(t, a)
	if len(lines) < 10 {
		t.Fatalf("%s 的符号表只有 %d 行 —— 守卫空转，等于没有守卫", a, len(lines))
	}
	for _, needle := range []string{"std::__1::", "std::__cxx11::", "std::basic_string"} {
		if n := countContaining(lines, needle); n != 0 {
			t.Errorf("%s 含 %d 个 %q 符号：运行时库必须零 C++ 标准库依赖，否则纯 C 项目无法链接",
				a, n, needle)
		}
	}
}

// TestParserArchiveHasNoLibstdcxx：编译器前端库内部是 C++，可以依赖 libc++，
// 但绝不能已经带着 libstdc++ 的符号 —— 那意味着构建时就已经被污染了。
func TestParserArchiveHasNoLibstdcxx(t *testing.T) {
	root := repoRoot(t)
	a := filepath.Join(root, "build", "libhermes_parser.a")
	lines := nmOn(t, a)
	if len(lines) < 10 {
		t.Fatalf("%s 的符号表只有 %d 行 —— 守卫空转，等于没有守卫", a, len(lines))
	}
	// libstdc++ 的 inline namespace 与 libc++（__1）不同，两个库的符号可区分：
	//   libc++     → std::__1::
	//   libstdc++  → std::__cxx11:: 或裸 std::（旧 ABI）
	if n := countContaining(lines, "std::__cxx11::"); n != 0 {
		t.Errorf("%s 含 %d 个 libstdc++（GPL-3.0 + Runtime Exception）符号 —— 构建产物已被污染",
			a, n)
	}
	if n := countContaining(lines, "__GLIBCXX__"); n != 0 {
		t.Errorf("%s 含 %d 处 __GLIBCXX__ 引用（libstdc++ 标识宏）", a, n)
	}
	// 防空转的另一半：这个库**应该**有 C++ 成分，否则说明它变成了纯 C，
	// 上面两条就变成了恒真的假守卫。
	if n := countContaining(lines, "std::__1::"); n == 0 {
		t.Errorf("%s 一个 libc++ 符号都没有 —— 若它已改为纯 C 实现，请删掉本守卫的"+
			" \"必须有 C++ 成分\" 这条反向空转检查，并同步 ARCH §8.2 的复用约束", a)
	}
}

// TestNativeArchiveSourcesAreNotCxx 守住 D-024 的"三级形态"第一条：
// **L0（原生档）不因 L1/L2 的存在而变重**。
//
// libhermes_native.a 由 native/c/ 与 native/rust/ 构成，它的 C 侧必须始终是 C。
// 一旦有人在 native/c/ 里加了 .cpp、或在 native/include/（C ABI 头所在处）
// 放了 C++ 头，"纯 C 项目可链接"这个契约就静默消失了 —— 而它消失的方式
// 是"链接时才报一堆莫名其妙的 C++ 符号错误"，很难归因。
//
// native/cpp/ 不在扫描范围：那是 T09 编译器前端，本来就是 C++，已冻结。
func TestNativeArchiveSourcesAreNotCxx(t *testing.T) {
	root := repoRoot(t)
	for _, dir := range []string{"native/c", "native/include"} {
		abs := filepath.Join(root, dir)
		entries, err := os.ReadDir(abs)
		if err != nil {
			t.Fatalf("读不到 %s：%v（目录缺失本身就是红线检查的失败，不是跳过）", dir, err)
		}
		if len(entries) == 0 {
			t.Fatalf("%s 是空目录 —— 守卫空转，等于没有守卫", dir)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			ext := strings.ToLower(filepath.Ext(e.Name()))
			switch ext {
			case ".cpp", ".cc", ".cxx", ".c++", ".hpp", ".hh", ".hxx":
				t.Errorf("%s/%s 是 C++ 文件：%s 属于 L0 原生档，必须保持纯 C"+
					"（C++ 只允许出现在 native/cpp/ —— 那是 T09 前端，已冻结）",
					dir, e.Name(), dir)
			}
		}
	}
}

// TestCxxSourcesUseCppExtension 兑现 ARCH §11.1 的命名约定："C++ 源文件一律
// .cpp，不用 .cxx / .cc / .C"。
//
// native/cpp/ 是前端目录（2026-11-04 由 native/cxx/ 改名而来，见 ARCH §11.1；
// 不改名），但目录**内**的源文件后缀必须统一为 .cpp。后缀是给人扫目录看的，
// 一个项目里不该有两种写法。
//
// 这条同时是上一条守卫的补充：上一条说"L0 不许有 C++"，这一条说"允许有
// C++ 的那个目录里，后缀必须统一"。两条合起来，C++ 的出现位置和形态都被钉住。
func TestCxxSourcesUseCppExtension(t *testing.T) {
	root := repoRoot(t)
	abs := filepath.Join(root, "native", "cpp")
	entries, err := os.ReadDir(abs)
	if err != nil {
		t.Fatalf("读不到 native/cpp：%v", err)
	}
	checked := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := filepath.Ext(e.Name())
		switch ext {
		case ".h", ".hpp":
			// 头文件的后缀约定不在本条范围内（.h 既有，.hpp 是生成物）
			continue
		case ".cpp":
			checked++
		default:
			t.Errorf("native/cpp/%s 的后缀是 %q：ARCH §11.1 规定 C++ 源文件一律 .cpp"+
				"（不用 .cxx / .cc / .C）", e.Name(), ext)
		}
	}
	if checked == 0 {
		t.Fatalf("native/cpp/ 里一个 .cpp 都没有 —— 守卫空转，等于没有守卫")
	}
}

// TestParserArchiveConsumersMustUseLibcxx 不检查产物本身，而是检查**约束被写进文档**。
//
// 这是"红线条款必须能被 grep 到"的纪律：libhermes_parser.a 有 undefined 的
// operator new/delete，复用者用 g++ 链接就会解析到 libstdc++。这件事无法靠
// 改产物避免（除非放弃 <vector>），只能靠**文档约束 + 提供 hermes cc 门禁**。
// 因此约束文本本身必须存在，且必须在产物布局那一节。
func TestParserArchiveConsumersMustUseLibcxx(t *testing.T) {
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "docs", "ARCH.md"))
	if err != nil {
		// ARCH.md 是内部文档，已移出公开仓库；这条守卫在本地有它时才有意义。
		t.Skip("docs/ARCH.md 不在仓库里（内部文档）")
	}
	s := string(b)

	// 定位 §8.2 产物布局那一节，约束必须写在那里（而不是随便某处）。
	idx := strings.Index(s, "### 8.2")
	if idx < 0 {
		t.Fatalf("docs/ARCH.md 里找不到 §8.2 产物布局")
	}
	end := strings.Index(s[idx:], "### 8.3")
	if end < 0 {
		end = len(s) - idx
	}
	section := s[idx : idx+end]

	for _, needle := range []string{"libhermes_parser.a", "libc++", "libstdc++"} {
		if !strings.Contains(section, needle) {
			t.Errorf("§8.2 产物布局里缺少 %q：复用 libhermes_parser.a 必须用 clang++ + libc++ 链接，"+
				"用 g++ 会引入 libstdc++（GPL-3.0 + Runtime Exception）—— 这条约束必须写在产物旁边，"+
				"写在别处等于没人看得到", needle)
		}
	}
}
