package guard_test

// 守 SDK 包（packages/rust、packages/javascript）里**非注释**的字符串字面量恒英文。
//
// 这些串是 SDK 抛给 Rust / JS 开发者的报错与状态文案，产品默认英文，不该出现
// 中文。它们之前既不在 TestNoChineseInGoUserVisibleStrings（那只扫 .go）也不在
// TestNoChineseInCppDiagnostics（那只扫 native/cpp）的覆盖范围内，是一个静默的
// 中文后门 —— 改坏也不会有任何测试报红。
//
// Rust 与 C 的字符串/注释语法不同，必须用各自能正确跳注释、生命周期、原始串的
// 扫描器，否则会像「把中文注释误吞进一个没闭合的字符串」那样产生假阳性。注释 /
// doc-comment（// /// //! /* */）不是输出，放行。
//
// 元守卫见 TestSdkStringScannerCatchesChinese（Rust 版）与
// TestSdkStringScannerCatchesChineseC（C 版）：分别喂含中文串 + 中文注释的假
// 源码，证明扫描器抓得到串、且不误伤注释。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func isRustIdentStart(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isRustIdentPart(b byte) bool {
	return isRustIdentStart(b) || (b >= '0' && b <= '9')
}

// rustStringLiterals 返回 src 中全部字符串字面量（含两端引号），正确跳过：
//   - 行注释 //、///、//! 与块注释 /* */
//   - 生命周期 / 标签 'a、'static、'_
//   - 字符字面量 'x'、'\”、'"'
//   - 普通串 "…"、字节串 b"…"、原始串 r"…" 与 r#"…"#（含任意数量 #）
//
// 原始字符串与字节串的内容不进检查（它们通常是 JSON 模板/协议常量，非诊断文案），
// 但仍作为字面量返回以保持「扫到过」的可观测性。
func rustStringLiterals(src string) []string {
	var out []string
	i, n := 0, len(src)
	for i < n {
		c := src[i]
		// 字节串前缀 b：跳过 b，让后续 " 或 r 分支接手。
		if c == 'b' && i+1 < n && (src[i+1] == '"' || (src[i+1] == 'r' && i+2 < n && src[i+2] == '#')) {
			i++
			continue
		}
		if c == '/' && i+1 < n && src[i+1] == '/' { // 行注释（含 /// //!）
			i += 2
			for i < n && src[i] != '\n' {
				i++
			}
			continue
		}
		if c == '/' && i+1 < n && src[i+1] == '*' { // 块注释
			i += 2
			for i+1 < n && !(src[i] == '*' && src[i+1] == '/') {
				i++
			}
			if i+1 < n {
				i += 2
			}
			continue
		}
		if c == '\'' { // 生命周期 'a / 'static / '_  vs 字符字面量 'x'
			if i+1 < n && isRustIdentStart(src[i+1]) {
				i++ // 跳过 '，把后面的 ident 当 token 跳掉，不找闭合 '
				for i < n && isRustIdentPart(src[i]) {
					i++
				}
				continue
			}
			i++ // 字符字面量
			for i < n {
				if src[i] == '\\' {
					i += 2
					continue
				}
				if src[i] == '\'' {
					i++
					break
				}
				i++
			}
			continue
		}
		if c == 'r' && i+1 < n && src[i+1] == '"' { // r"..." 原始串
			i += 2
			for i < n && src[i] != '"' {
				i++
			}
			if i < n {
				i++
			}
			continue
		}
		if c == 'r' && i+1 < n && src[i+1] == '#' && i+2 < n && src[i+2] == '"' { // r#"..."#（# 数任意）
			j := i + 1
			for j < n && src[j] == '#' {
				j++
			}
			hashes := j - (i + 1)
			i = j + 1 // 跳到开头 "
			for i+hashes < n {
				if src[i] == '"' {
					k := 0
					for k < hashes && i+1+k < n && src[i+1+k] == '#' {
						k++
					}
					if k == hashes {
						i += 1 + hashes
						break
					}
				}
				i++
			}
			continue
		}
		if c == '"' { // 普通 / 字节串 "..."
			start := i
			i++
			for i < n {
				if src[i] == '\\' {
					i += 2
					continue
				}
				if src[i] == '"' {
					i++
					break
				}
				i++
			}
			out = append(out, src[start:min(i, n)])
			continue
		}
		i++
	}
	return out
}

// TestNoChineseInSdkUserVisibleStrings 守 packages/rust/src/*.rs 与
// packages/javascript/native/*.c 里所有非注释字符串字面量默认英文。
func TestNoChineseInSdkUserVisibleStrings(t *testing.T) {
	root := repoRoot(t)
	type target struct {
		dir    string
		scan   func(string) []string
		suffix string
	}
	targets := []target{
		{filepath.Join("packages", "rust", "src"), rustStringLiterals, ".rs"},
		{filepath.Join("packages", "javascript", "native"), cppStringLiterals, ".c"},
	}
	var bad []string
	for _, tg := range targets {
		dir := filepath.Join(root, tg.dir)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", tg.dir, err)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if !strings.HasSuffix(e.Name(), tg.suffix) {
				continue
			}
			path := filepath.Join(dir, e.Name())
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, lit := range tg.scan(string(raw)) {
				if hanInDiag.MatchString(lit) {
					bad = append(bad, filepath.Join(tg.dir, e.Name())+": "+lit)
				}
			}
		}
	}
	if len(bad) > 0 {
		t.Fatalf("SDK 用户可见字符串必须默认英文（注释/doc-comment 不算）：\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// TestSdkStringScannerCatchesChinese 是 Rust 扫描器的元守卫：证明它抓得到中文串、
// 且不把中文注释 / 生命周期误算成串。
func TestSdkStringScannerCatchesChinese(t *testing.T) {
	const fixture = `// 中文注释，不该被算作输出
fn f() -> Result<(), ()> {
    // 行尾注释里有 "假引号" 也不该被吞
    let _ = 'a; // 'a 是生命周期，不是字符字面量
    let _ = format!("base_url 缺少 scheme: {}", x);
    let _ = "plain english";
    /* 中文块注释 "里面还有假引号" */
}
`
	var withHan []string
	for _, lit := range rustStringLiterals(fixture) {
		if hanInDiag.MatchString(lit) {
			withHan = append(withHan, lit)
		}
	}
	if len(withHan) == 0 {
		t.Fatal("Rust 扫描器没抓到任何中文串 —— 这条守卫已失效，TestNoChineseInSdkUserVisibleStrings 会恒绿")
	}
	joined := strings.Join(withHan, "\n")
	if !strings.Contains(joined, "base_url 缺少 scheme") {
		t.Fatalf("Rust 扫描器应抓到中文串，实际：%s", joined)
	}
	for _, lit := range rustStringLiterals(fixture) {
		if strings.Contains(lit, "中文块注释") {
			t.Fatalf("块注释被误判成串：%q", lit)
		}
	}
}

// TestSdkStringScannerCatchesChineseC 是 C 扫描器的元守卫（复用 C++ 扫描器）。
func TestSdkStringScannerCatchesChineseC(t *testing.T) {
	const fixture = `// 中文注释，不该被算作输出
const char* a = "expected ']' after array type";
const char* b = "数组类型缺少 ']'";
/* 中文块注释 "里面还有假引号" */
`
	var withHan []string
	for _, lit := range cppStringLiterals(fixture) {
		if hanInDiag.MatchString(lit) {
			withHan = append(withHan, lit)
		}
	}
	if len(withHan) == 0 {
		t.Fatal("C 扫描器没抓到任何中文串 —— 这条守卫已失效")
	}
	joined := strings.Join(withHan, "\n")
	if !strings.Contains(joined, "数组类型缺少") {
		t.Fatalf("C 扫描器应抓到中文串，实际：%s", joined)
	}
	for _, lit := range cppStringLiterals(fixture) {
		if strings.Contains(lit, "中文块注释") {
			t.Fatalf("块注释被误判成串：%q", lit)
		}
	}
}
