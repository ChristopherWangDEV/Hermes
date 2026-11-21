package guard_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// 诊断码棘轮：码与文案必须双向登记。
//
// 两个对称漏洞必须同时堵：
//
//	① 有码无文档：发射了但附录 A 没索引 —— 宽表里少几行看不出来；
//	② 有文档无实现（纸面码）：附录 A 列了但零实现 —— 静默放行的假契约。
//
// 判据是**双向**的，且"预留清单"是人工决策的存档：新增一个"文档有、实现无"
// 的码必须写进 tests/diag/reserved_codes.txt；实现了就必须从清单里删掉。
// 测试不让它漂移。
//
// 为什么用 C++ catalog 当"已实现"的权威：catalog 是"这个码能被渲染/解释"的
// 唯一登记处（2b 之后 Go catalog 已不再是权威）。发射点字面量另有一条独立断言
// （见 TestEmittedCodesAreAllCatalogued），防"发射了但没登记文案"。

var (
	appendixLiteralRE = regexp.MustCompile("`([EW][0-9]{4})`")
	appendixRangeRE   = regexp.MustCompile("`([EW][0-9]{4})`[–-]`?([EW][0-9]{4})`?")
	appendixFamilyRE  = regexp.MustCompile("`([EW]_[A-Z]+_\\*)`")
	catalogEntryRE    = regexp.MustCompile(`\{"([EW][A-Z0-9_]+)",\s*"`)
	emittedLiteralRE  = regexp.MustCompile(`"(E[0-9]{4}|E_[A-Z0-9_]+|W_[A-Z0-9_]+)"`)
)

// appendices 解析附录 A 的字面码、区间展开码与通配族。
func appendices(t *testing.T, root string) (literal map[string]bool, families []string) {
	t.Helper()
	md, err := os.ReadFile(filepath.Join(root, "docs", "LANGUAGE.md"))
	if err != nil {
		t.Fatalf("read LANGUAGE.md: %v", err)
	}
	text := string(md)
	i := strings.Index(text, "## 附录 A")
	if i < 0 {
		t.Fatal("LANGUAGE.md 里找不到附录 A —— 这条测试会空跑")
	}
	appA := text[i:]
	literal = map[string]bool{}
	for _, m := range appendixLiteralRE.FindAllStringSubmatch(appA, -1) {
		literal[m[1]] = true
	}
	for _, m := range appendixRangeRE.FindAllStringSubmatch(appA, -1) {
		a := 0
		b := 0
		if _, err := fmt.Sscanf(m[1][1:], "%d", &a); err != nil {
			continue
		}
		if _, err := fmt.Sscanf(m[2][1:], "%d", &b); err != nil {
			continue
		}
		for n := a; n <= b; n++ {
			literal[fmt.Sprintf("%s%04d", m[1][:1], n)] = true
		}
	}
	for _, m := range appendixFamilyRE.FindAllStringSubmatch(appA, -1) {
		families = append(families, strings.TrimSuffix(m[1], "*"))
	}
	if len(literal) < 50 {
		t.Fatalf("附录 A 只解析出 %d 个字面码 —— 解析器退化了", len(literal))
	}
	return literal, families
}

// catalogCodes 读出 C++ catalog 里已登记的码。
func catalogCodes(t *testing.T, root string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "native", "cpp", "diag_catalog.cpp"))
	if err != nil {
		t.Fatalf("read diag_catalog.cpp: %v", err)
	}
	out := map[string]bool{}
	for _, m := range catalogEntryRE.FindAllStringSubmatch(string(raw), -1) {
		out[m[1]] = true
	}
	if len(out) < 50 {
		t.Fatalf("catalog 只解析出 %d 个码 —— 解析器退化了", len(out))
	}
	return out
}

// reservedCodes 读人工维护的预留清单。
func reservedCodes(t *testing.T, root string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "tests", "diag", "reserved_codes.txt"))
	if err != nil {
		t.Fatalf("read reserved_codes.txt: %v", err)
	}
	out := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out[line] = true
		}
	}
	return out
}

// familyCovered 报告码是否被附录 A 的通配族覆盖（如 E_IR_* / E_LINK_*）。
func familyCovered(code string, families []string) bool {
	for _, f := range families {
		if strings.HasPrefix(code, f) {
			return true
		}
	}
	return false
}

// TestEveryCatalogCodeIsDocumented 是正向：已登记的码必须在附录 A 有索引
// （或命中通配族）。
func TestEveryCatalogCodeIsDocumented(t *testing.T) {
	root := repoRoot(t)
	literal, families := appendices(t, root)
	cat := catalogCodes(t, root)
	var missing []string
	for code := range cat {
		if !literal[code] && !familyCovered(code, families) {
			missing = append(missing, code)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("这些码已登记进 catalog，但附录 A 既没有索引也不在通配族里：\n  %s",
			strings.Join(missing, "\n  "))
	}
}

// TestEveryDocumentedCodeIsImplementedOrReserved 是反向：附录 A 的字面码必须
// 要么已实现（在 catalog），要么在预留清单里。否则就是"纸面码"——文档承诺了
// 一条永远不会出现的错误。
func TestEveryDocumentedCodeIsImplementedOrReserved(t *testing.T) {
	root := repoRoot(t)
	literal, _ := appendices(t, root)
	cat := catalogCodes(t, root)
	reserved := reservedCodes(t, root)
	var orphans []string
	for code := range literal {
		if cat[code] || reserved[code] {
			continue
		}
		orphans = append(orphans, code)
	}
	sort.Strings(orphans)
	if len(orphans) > 0 {
		t.Errorf("这些码在附录 A 里，但既没有实现（不在 catalog），也不在预留清单：\n  %s\n"+
			"要么实现它，要么把它写进 tests/diag/reserved_codes.txt。",
			strings.Join(orphans, "\n  "))
	}
}

// TestReservedCodesAreNotImplemented 是诚实性检查：预留清单里的码不许已有实现。
// 有实现还留在清单里 = 清单在撒谎，棘轮就失去意义。
func TestReservedCodesAreNotImplemented(t *testing.T) {
	root := repoRoot(t)
	cat := catalogCodes(t, root)
	reserved := reservedCodes(t, root)
	var liars []string
	for code := range reserved {
		if cat[code] {
			liars = append(liars, code)
		}
	}
	sort.Strings(liars)
	if len(liars) > 0 {
		t.Errorf("这些码已在 catalog 实现，却仍留在预留清单里：\n  %s",
			strings.Join(liars, "\n  "))
	}
}

// TestEmptyShellCodesAreBanned 是用户红线：E_LINK_CYCLE 与 E_LINK_SIG_MISMATCH
// 是**空壳码**（没有任何检查逻辑），禁止把它们登记进 catalog 造假。
func TestEmptyShellCodesAreBanned(t *testing.T) {
	root := repoRoot(t)
	cat := catalogCodes(t, root)
	for _, code := range []string{"E_LINK_CYCLE", "E_LINK_SIG_MISMATCH"} {
		if cat[code] {
			t.Errorf("%s 没有实现，却被登记进 catalog —— 这是把空壳码包装成\"已验证\"，属红线", code)
		}
	}
}

// TestEmittedCodesAreAllCatalogued 防另一个方向的漏登记：源码里发射了某个码，
// 但 catalog 里没有它的文案 —— 用户会看到一条没有 why/help 的裸诊断。
func TestEmittedCodesAreAllCatalogued(t *testing.T) {
	root := repoRoot(t)
	cat := catalogCodes(t, root)
	emitted := map[string]bool{}
	for _, dir := range []string{"native/cpp", "internal", "cmd"} {
		filepath.Walk(filepath.Join(root, dir), func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if strings.HasSuffix(path, "_test.go") || strings.Contains(path, "parser_test") {
				return nil
			}
			isSource := strings.HasSuffix(path, ".go") || strings.HasSuffix(path, ".cpp") ||
				strings.HasSuffix(path, ".h") || strings.HasSuffix(path, ".c")
			if !isSource {
				return nil
			}
			raw, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			for _, m := range emittedLiteralRE.FindAllStringSubmatch(string(raw), -1) {
				code := m[1]
				// 跳过前缀常量（"E_IR_" / "W_IR_"）与明显不是码的名字。
				if strings.HasSuffix(code, "_") {
					continue
				}
				emitted[code] = true
			}
			return nil
		})
	}
	var missing []string
	for code := range emitted {
		if !cat[code] {
			missing = append(missing, code)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("这些码在源码里有发射点，但 catalog 里没有文案（用户会看到没有 why/help 的裸诊断）：\n  %s",
			strings.Join(missing, "\n  "))
	}
}
