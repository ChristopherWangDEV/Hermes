package header

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"hermes/internal/ir"
)

// funcDefRe 匹配一个函数定义的签名行：类型部分（可含空格与 '*'）+ 空白 +
// 函数名 + '('。用于从 CoreImpl 字符串与 hermes_core.h 里提取函数名，
// 不引入任何外部依赖（不引 objfile / 不读 .a）。
var funcDefRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_ \*]*\s+(\w+)\s*\(`)

// headerDeclaredNames 从 hermes_core.h 里抽出真正被声明的函数名（跳过注释与
// #include），避免把注释里出现的 `hermes_abi_major()` 当成声明。
func headerDeclaredNames(t *testing.T) []string {
	t.Helper()
	header := readFileRel(t, "include/hermes/hermes_core.h")
	return extractFuncNames(header)
}

// isSkippable 跳过 #include 行、空行，以及 // 与 /* ... */ 注释行。
func isSkippable(line string) bool {
	t := strings.TrimSpace(line)
	if t == "" {
		return true
	}
	if strings.HasPrefix(t, "#") { // #include ...
		return true
	}
	if strings.HasPrefix(t, "//") {
		return true
	}
	if strings.HasPrefix(t, "/*") || t == "*/" || strings.HasPrefix(t, "*") {
		return true
	}
	return false
}

// extractFuncNames 从一段 C 源码里抽出所有被定义/声明的函数名。
func extractFuncNames(src string) []string {
	names := make([]string, 0)
	for _, line := range strings.Split(src, "\n") {
		if isSkippable(line) {
			continue
		}
		m := funcDefRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		names = append(names, m[1])
	}
	return names
}

// readFileRel 从仓库根（internal/header 的上两级）读一个文件。
func readFileRel(t *testing.T, rel string) string {
	t.Helper()
	p := filepath.Join("..", "..", rel)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读文件失败 %s: %v", p, err)
	}
	return string(data)
}

// TestCoreImplFuncsAreDeclaredInCoreHeader 守住「CoreImpl 里定义的每个函数，
// 必须在 hermes_core.h 里有声明」。这条守卫本应抓到 core_impl.go 里那个头文件
// 未声明的死符号 hermes_abi_major——它曾被编进每一个 hermes cc 产物，却无人能
// 正确调用，返回值还和 HM_ABI_MAJOR=2 矛盾。
func TestCoreImplFuncsAreDeclaredInCoreHeader(t *testing.T) {
	declared := headerDeclaredNames(t)
	declaredSet := make(map[string]bool, len(declared))
	for _, n := range declared {
		declaredSet[n] = true
	}

	implNames := extractFuncNames(CoreImpl)
	if len(implNames) == 0 {
		t.Fatal("CoreImpl 里没解析到任何函数，正则可能失效")
	}

	for _, name := range implNames {
		if !declaredSet[name] {
			t.Fatalf("CoreImpl 定义了 %q，但 include/hermes/hermes_core.h 里没有它的声明", name)
		}
	}
}

// TestCoreHeaderFuncsHaveAnImplementation 反向守：hermes_core.h 声明的每个函数，
// 必须在 CoreImpl（生成进用户程序的 C 源）或被 //export 的 libhermesrt 符号里有着落。
// 简化实现：在 CoreImpl 里能找到，或在 cmd/hermesrt/main.go 里以 `//export <name>` 出现，
// 不引 Go 的 objfile 包（那是另一个 worker 正在改的文件，避免冲突）。
func TestCoreHeaderFuncsHaveAnImplementation(t *testing.T) {
	rtMain := readFileRel(t, "cmd/hermesrt/main.go")

	declared := headerDeclaredNames(t)
	if len(declared) == 0 {
		t.Fatal("hermes_core.h 里没解析到任何声明，正则可能失效")
	}

	// 去重。
	seen := make(map[string]bool)
	for _, name := range declared {
		if seen[name] {
			continue
		}
		seen[name] = true
		inImpl := strings.Contains(CoreImpl, name+"(") || strings.Contains(CoreImpl, name+" (")
		inRT := regexp.MustCompile(`//export\s+` + regexp.QuoteMeta(name) + `\b`).MatchString(rtMain)
		if !inImpl && !inRT {
			t.Fatalf("hermes_core.h 声明了 %q，但既不在 CoreImpl 也不在 libhermesrt 的导出里", name)
		}
	}
}

// TestCoreImplLastErrorIsBackedBySharedSlot 是结构守卫：防止 hermes_last_error()
// 哪天又退回「恒返回空串」的占位实现。它只检查 CoreImpl 的形状，真正的端到端
// 读值由 cmd/hermes/cmd_cc_test.go 与 cmd_compile_test.go 真编译链接运行验证。
func TestCoreImplLastErrorIsBackedBySharedSlot(t *testing.T) {
	// 恒空实现的两件遗物：`static const char empty[1]` 与把 s.ptr 指向它。
	for _, bad := range []string{"static const char empty", "s.ptr = empty", "malloc(", "strdup("} {
		if strings.Contains(CoreImpl, bad) {
			t.Errorf("CoreImpl 又出现了 %q（错误槽必须用静态数组、不得分配堆内存）", bad)
		}
	}

	// 槽：external linkage、HERMES_THREAD_LOCAL、定长数组、无指针成员。
	for _, want := range []string{
		"#define HERMES_LAST_ERROR_CAP 256",
		"char msg[HERMES_LAST_ERROR_CAP];",
		"HERMES_THREAD_LOCAL struct hermes_error_slot hermes_last_error_slot = { 0, 0, 0, { 0 } };",
		"hermes_str hermes_last_error(void) {",
	} {
		if !strings.Contains(CoreImpl, want) {
			t.Errorf("CoreImpl 缺少 %q", want)
		}
	}
	for _, bad := range []string{"char* msg", "const char* msg"} {
		if strings.Contains(CoreImpl, bad) {
			t.Errorf("共享错误槽不许有指针成员 %q", bad)
		}
	}

	// 函数体必须真的读槽，而不是再糊一个常量空串。
	start := strings.Index(CoreImpl, "hermes_str hermes_last_error(void) {")
	if start < 0 {
		t.Fatal("找不到 hermes_last_error 的函数定义")
	}
	body := CoreImpl[start:]
	if end := strings.Index(body, "\n}"); end >= 0 {
		body = body[:end]
	}
	for _, want := range []string{"hermes_last_error_slot.msg", "hermes_last_error_slot.len"} {
		if !strings.Contains(body, want) {
			t.Errorf("hermes_last_error 函数体没有引用 %q:\n%s", want, body)
		}
	}

	// CoreImpl 里只能有 hermes_core.h 声明的那两个函数；新增函数属于公开 ABI
	// minor bump（docs/C-ABI.md:155），不是这次能做的事。
	names := extractFuncNames(CoreImpl)
	if len(names) != 2 {
		t.Fatalf("CoreImpl 定义了 %d 个函数 %v，期望恰好 2 个（不得新增导出函数）", len(names), names)
	}
	wantSet := map[string]bool{"hermes_buf_free": true, "hermes_last_error": true}
	for _, n := range names {
		if !wantSet[n] {
			t.Errorf("CoreImpl 出现了未在 hermes_core.h 声明的函数 %q", n)
		}
	}
}

// TestGenCShimPublishesErrorBeforeFree 钉住 shim 这处写方的形状：入口清槽、
// 在 free(err) 之前 bounded-copy 发布、不靠 strlen 取错误长度、返回码不变。
func TestGenCShimPublishesErrorBeforeFree(t *testing.T) {
	doc := &ir.Doc{
		Unit:      ir.Unit{Name: "u"},
		Workflows: []ir.Workflow{{Name: "w", Exported: true}},
	}
	shim, err := GenCShim(doc, []byte{1, 2, 3})
	if err != nil {
		t.Fatalf("GenCShim: %v", err)
	}
	s := string(shim)
	for _, want := range []string{
		"HERMES_THREAD_LOCAL extern struct hermes_error_slot hermes_last_error_slot;",
		"memcpy(hermes_last_error_slot.msg, msg, n)",
		"hermes_last_error_clear();",
		"hermes_last_error_publish(",
		"hermes_last_error_bounded_len(err)",
		"return HERMES_ERR_INTERNAL;",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("shim 缺少 %q", want)
		}
	}
	// 发布必须发生在 free(err) 之前，否则宿主读到的会是悬空指针的复制来源。
	pub, fre := strings.Index(s, "hermes_last_error_publish("), strings.Index(s, "free(err);")
	if pub < 0 || fre < 0 || pub > fre {
		t.Errorf("错误文本必须在 free(err) 之前复制进槽（publish=%d free=%d）", pub, fre)
	}
	if strings.Contains(s, "strlen(err)") {
		t.Error("shim 不得用 strlen(err) 取错误长度（应按 CAP bounded-copy 并用 len 表达）")
	}
}
