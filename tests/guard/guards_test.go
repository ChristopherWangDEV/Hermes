// Package guard 是项目的"不许退化"测试集。
//
// 与功能测试的区别：功能测试证明"现在能用"，守卫测试证明"以后也不能变坏"。
// 它们全部是可执行事实，不是注释里的承诺 —— 一条守卫被删掉或者被注释掉，
// 会立刻在 CI 里表现为少了一条 PASS，而不是安静地消失。
package guard_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// repoRoot 从 tests/guard 往上两级。
func repoRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	return abs
}

// goBin 保证测试里起的子进程能找到 go（本机 go 不在 PATH 的默认位置）。
func goPath() string {
	if p, err := exec.LookPath("go"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	cand := filepath.Join(home, ".local", "go", "bin", "go")
	if _, err := os.Stat(cand); err == nil {
		return cand
	}
	return "go"
}

// tail 截取末尾 n 字节，用于把长 stderr 压成可读的诊断。
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

func run(t *testing.T, dir, name string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+filepath.Dir(goPath())+":"+os.Getenv("PATH"))
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("%s %v: %v", name, args, err)
		}
	}
	return out.String(), errb.String(), code
}

// TestSymbolSurfaceMatchesManifest 是"Rust 只做 4 件事"的机器守卫。
//
// 双向 diff：少一个 = 功能缺失；**多一个** = 有人给原生层偷偷加了职责。
// 单向检查（只查少）会让这道守卫形同虚设 —— 语言分工被腐蚀的方式永远是"多"。
func TestSymbolSurfaceMatchesManifest(t *testing.T) {
	root := repoRoot(t)
	archive := filepath.Join(root, "build", "libhermes_native.a")
	if _, err := os.Stat(archive); err != nil {
		t.Skipf("build/libhermes_native.a 不存在，先跑 scripts/build.sh: %v", err)
	}

	// 注意：这里**不能**用 nm 的退出码判断成败。
	// Rust 1.98 产出 LLVM 22 的 DWARF，而 Apple 的 nm 读端更旧，会对
	// compiler_builtins 的每个 .o 报 "Unknown attribute kind" 并以非零退出，
	// 但符号表照常输出。判据改成"解析出的符号集合非空"：
	// 真的读不出符号时集合为空，仍然会红。
	out, errs, _ := run(t, root, "nm", "-gU", archive)
	got := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		sym := strings.TrimPrefix(fields[len(fields)-1], "_")
		if strings.HasPrefix(sym, "hm_") {
			got[sym] = true
		}
	}
	if len(got) == 0 {
		t.Fatalf("nm 未解析出任何 hm_* 符号，工具链可能真的坏了:\n%s", tail(errs, 800))
	}

	raw, err := os.ReadFile(filepath.Join(root, "scripts", "expected_symbols.txt"))
	if err != nil {
		t.Fatalf("read expected_symbols.txt: %v", err)
	}
	want := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		want[line] = true
	}
	if len(want) == 0 {
		t.Fatal("expected_symbols.txt 解析出 0 个符号 —— 守卫本身坏了")
	}

	var missing, extra []string
	for s := range want {
		if !got[s] {
			missing = append(missing, s)
		}
	}
	for s := range got {
		if !want[s] {
			extra = append(extra, s)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("缺失符号（功能没做完）: %v", missing)
	}
	if len(extra) > 0 {
		t.Errorf("多出符号（原生层长出了新职责，违背 D-011 的语言分工）: %v", extra)
	}
}

// TestGoDependenciesAreStdlibOnly 是 A 组许可门禁的第一道：Go 侧不许有第三方模块。
//
// 这条比"许可证合规"更严 —— 它连"合规的第三方"也不允许，
// 因为供应链面每多一个模块，审计和可重现就多一个变量。
func TestGoDependenciesAreStdlibOnly(t *testing.T) {
	root := repoRoot(t)
	out, errs, code := run(t, root, goPath(), "list", "-deps", "./...")
	if code != 0 {
		t.Fatalf("go list -deps failed: %s", errs)
	}
	bad := []string{}
	for _, p := range strings.Split(out, "\n") {
		p = strings.TrimSpace(p)
		if p == "" || p == "C" {
			continue
		}
		// 标准库没有点号（域名）在第一个路径段里。
		first := strings.SplitN(p, "/", 2)[0]
		if strings.Contains(first, ".") {
			bad = append(bad, p)
		}
	}
	if len(bad) > 0 {
		t.Fatalf("发现非标准库依赖（违反 R-LC-A2）: %v", bad)
	}
}

// TestBuildRejectsGNUCompiler 是 B 组门禁：CXX=g++ 必须失败，且说得清原因。
//
// 退出码 64 是专用码，不是随便的 1 —— 让 CI 能把它和"编译真的失败了"区分开。
func TestBuildRejectsGNUCompiler(t *testing.T) {
	root := repoRoot(t)
	_, stderr, code := run(t, root, "env", "CXX=g++", "./scripts/build.sh")
	if code != 64 {
		t.Fatalf("CXX=g++ 构建退出码 = %d, want 64. stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "GPL") {
		t.Fatalf("拒绝理由没提到 GPL（用户划的红线必须出现在错误信息里）: %s", stderr)
	}
}

// TestDebugPeekOnlyInTests：hm_debug_peek 是测试专用的观测口，
// 一旦产品代码路径引用它，就等于在生产里长期保留一块"上一次释放的明文残留"。
func TestDebugPeekOnlyInTests(t *testing.T) {
	root := repoRoot(t)
	// 允许：测试文件、native/ 的 C/Rust 侧、docs、scripts，
	// 以及 internal/native/ffi.go —— 那是**定义**它的地方，不是调用它的地方。
	allow := regexp.MustCompile(`(_test\.go$|^tests/|^native/|^docs/|^scripts/|^internal/native/ffi\.go$)`)
	var offenders []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			base := filepath.Base(path)
			if base == "build" || base == ".git" || base == "target" || base == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if allow.MatchString(rel) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if strings.Contains(string(data), "native.DebugPeek(") {
			offenders = append(offenders, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("产品代码引用了 DebugPeek: %v", offenders)
	}
}

// TestGoConstantsMatchHeader：Go 侧的 ABI 常量与头文件宏必须一致。
// 三处一起钉：头文件 HM_ABI_MAJOR/MINOR、internal/native 的 ABIMajor/ABIMinor、
// internal/archive 的 ABIVersionSupported（`.hbc` 成员 abi_version 的唯一来源）。
// 两边手改不报警的那一天，就是 ABI 开始悄悄漂移的那一天。
func TestGoConstantsMatchHeader(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "native", "include", "hermes_abi.h"))
	if err != nil {
		t.Fatalf("read header: %v", err)
	}
	hdr := string(raw)

	major := regexp.MustCompile(`#define\s+HM_ABI_MAJOR\s+(\d+)`).FindStringSubmatch(hdr)
	minor := regexp.MustCompile(`#define\s+HM_ABI_MINOR\s+(\d+)`).FindStringSubmatch(hdr)
	if major == nil || minor == nil {
		t.Fatal("头文件里找不到 HM_ABI_MAJOR/HM_ABI_MINOR")
	}

	goSrc, err := os.ReadFile(filepath.Join(root, "internal", "native", "ffi.go"))
	if err != nil {
		t.Fatalf("read ffi.go: %v", err)
	}
	gs := string(goSrc)
	if !strings.Contains(gs, "ABIMajor = "+major[1]) {
		t.Errorf("Go ABIMajor 与头文件 %s 不一致", major[1])
	}
	if !strings.Contains(gs, "ABIMinor = "+minor[1]) {
		t.Errorf("Go ABIMinor 与头文件 %s 不一致", minor[1])
	}

	// 第三处：`.hbc` 成员的 abi_version（archive.ABIVersionSupported）。
	// 头文件与 ffi.go 一起改、却漏掉链接器与 emitter 引用的那个常量，是 2.0.0
	// 这次升号最可能出的事故形态：产物声明 2、链接器只认 1（或反过来），
	// 而两边各自"看起来都对"。minor 恒为 0，所以它必须等于 major。
	arcSrc, err := os.ReadFile(filepath.Join(root, "internal", "archive", "archive.go"))
	if err != nil {
		t.Fatalf("read archive.go: %v", err)
	}
	if minor[1] != "0" {
		t.Errorf("HM_ABI_MINOR 是 %s：abi_version 是一个整数，minor 必须为 0 才能一一对应", minor[1])
	}
	if !strings.Contains(string(arcSrc), "ABIVersionSupported = "+major[1]) {
		t.Errorf("archive.ABIVersionSupported 与头文件 HM_ABI_MAJOR=%s 不一致", major[1])
	}

	// 第四处：Rust 侧。lib.rs 的 ABI_MAJOR/ABI_MINOR 是 hm_abi_version() 的
	// **实现**，也是 hm_runtime_init 的判据。它在 2.0.0 的升号里真的漏过：
	// 头文件与 ffi.go 都改成 2 了，Rust 还是 1，于是二进制一启动就报
	// "caller expects major 1, library is 2"。没有这条，下一次还会漏。
	rsSrc, err := os.ReadFile(filepath.Join(root, "native", "rust", "src", "lib.rs"))
	if err != nil {
		t.Fatalf("read lib.rs: %v", err)
	}
	if !strings.Contains(string(rsSrc), "ABI_MAJOR: u32 = "+major[1]) {
		t.Errorf("Rust ABI_MAJOR 与头文件 HM_ABI_MAJOR=%s 不一致", major[1])
	}
	if !strings.Contains(string(rsSrc), "ABI_MINOR: u32 = "+minor[1]) {
		t.Errorf("Rust ABI_MINOR 与头文件 HM_ABI_MINOR=%s 不一致", minor[1])
	}
}

// TestABIBuildIdMatchesHeaders 逐字复算 Rust 的 ABI_BUILD_ID。
//
// 算法与 native/rust/src/lib.rs 的注释一致：`native/include/` 下按文件名排序，
// 每个 `.h` 依次吃掉 basename、`\n`、内容、`\x00`，取 sha256 十六进制前 16 位。
//
// 为什么需要这条：`hm_abi_build_id()` 的承诺是"头文件集合的内容哈希"，改了头
// 却没更新常量时它就不是了 —— 没有任何类型系统能保证这句话，只能靠一条真的
// 会红的测试。这条测试此前只存在于 lib.rs 的注释里（"逐字复算"），而注释不会
// 变红：2.0.0 升 ABI 之前它的值和按算法算出来的结果就已经对不上。
func TestABIBuildIdMatchesHeaders(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "native", "include")
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read include dir: %v", err)
	}

	names := make([]string, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".h") {
			continue
		}
		names = append(names, e.Name())
	}
	if len(names) == 0 {
		t.Fatal("native/include 里一个 .h 都没有：路径变了就说明这条守卫失去了意义")
	}
	sort.Strings(names)

	h := sha256.New()
	for _, name := range names {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		h.Write([]byte(name))
		h.Write([]byte("\n"))
		h.Write(body)
		h.Write([]byte{0})
	}
	want := "hermes-abi-" + hex.EncodeToString(h.Sum(nil))[:16]

	rsSrc, err := os.ReadFile(filepath.Join(root, "native", "rust", "src", "lib.rs"))
	if err != nil {
		t.Fatalf("read lib.rs: %v", err)
	}
	// 两处（常量与 NUL 结尾的静态串）必须完全一致，否则 hm_abi_build_id()
	// 返回的串与常量对不上，Rust 自己的单测也会红。
	// 只认真正的常量定义（`= "..."` 与 `= b"..."`），注释里提及旧值不算 ——
	// 否则"解释旧值为什么漂移"的那句注释会把自己喂给测试、制造假红。
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`pub const ABI_BUILD_ID: &str = "(hermes-abi-[0-9a-f]{16})"`),
		regexp.MustCompile(`static ABI_BUILD_ID_CSTR: &\[u8\] = b"(hermes-abi-[0-9a-f]{16})\\0"`),
	} {
		for _, m := range re.FindAllStringSubmatch(string(rsSrc), -1) {
			if m[1] != want {
				t.Errorf("lib.rs 里的 ABI_BUILD_ID 是 %s，按当前头文件应重算为 %s", m[1], want)
			}
		}
	}
	if !strings.Contains(string(rsSrc), want) {
		t.Errorf("native/rust/src/lib.rs 里没有 %s（改了头文件就要同步这个常量）", want)
	}
}

// TestReproducibleBuild 是真跑两次构建比 sha256 —— 慢，因此默认跳过。
// HERMES_SLOW=1 时启用（CI 的发布门禁应该打开它）。
func TestReproducibleBuild(t *testing.T) {
	if os.Getenv("HERMES_SLOW") != "1" {
		t.Skip("慢测试：设 HERMES_SLOW=1 启用（两次完整构建 + 哈希比对）")
	}
	root := repoRoot(t)
	read := func() string {
		out, errs, code := run(t, root, "./scripts/build.sh")
		if code != 0 {
			t.Fatalf("build failed: %s%s", out, errs)
		}
		data, err := os.ReadFile(filepath.Join(root, "build", "artifacts.sha256"))
		if err != nil {
			t.Fatalf("read artifacts.sha256: %v", err)
		}
		return string(data)
	}
	first := read()
	second := read()
	if first != second {
		t.Fatalf("两次构建产物不一致（R-OPS-05 失效）:\n%s\n---\n%s", first, second)
	}
}
