package include

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hermes/internal/diag"
)

// 测试全部在 t.TempDir() 里搭真目录树，**不用 mock 文件系统**：
// 这个包的全部价值就是"按顺序找文件"，mock 掉 IO 等于把被测对象删了。

func write(t *testing.T, dir, rel, content string) string {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func hasCode(ds []diag.Diag, code string) bool {
	for _, d := range ds {
		if d.Code == code {
			return true
		}
	}
	return false
}

func codes(ds []diag.Diag) string {
	var out []string
	for _, d := range ds {
		out = append(out, d.Code)
	}
	return strings.Join(out, ",")
}

// ScanDirectives 必须能切开带引号的 spec —— dump 里是 `""support.h""`，
// 非贪婪匹配会在内层引号处停住，切出半个路径。
func TestScanDirectivesHandlesQuotedSpec(t *testing.T) {
	dump := `Module n=3 1:1-1:1 ""
  Include n=0 8:11-8:21 ""support.h""
  Link n=0 11:8-11:17 "<std/net>"
  Link n=0 12:8-12:19 ""models/order""
`
	ds := ScanDirectives(dump)
	if len(ds) != 3 {
		t.Fatalf("应解析出 3 条指令，实际 %d：%+v", len(ds), ds)
	}
	if ds[0].Path != "support.h" || ds[0].Angle {
		t.Errorf("第 1 条应是 quote 形式 support.h，实际 %+v", ds[0])
	}
	if ds[1].Path != "std/net" || !ds[1].Angle {
		t.Errorf("第 2 条应是 angle 形式 std/net，实际 %+v", ds[1])
	}
	if ds[2].Path != "models/order" {
		t.Errorf("第 3 条应以 models/order 为目标，实际 %q", ds[2].Path)
	}
	// 行号来自 dump，是回指源码的唯一线索。
	if ds[0].Line != 8 || ds[1].Line != 11 {
		t.Errorf("行号不对：%+v", ds)
	}
}

// 端到端：examples/support_bot 应该能完整展开，零诊断。
func TestResolveSupportBot(t *testing.T) {
	root := repoRoot(t)
	entry := filepath.Join(root, "examples", "support_bot", "support.hermes")
	src, err := os.ReadFile(entry)
	if err != nil {
		t.Fatalf("读不到 %s：%v", entry, err)
	}
	res, ds := Resolve(entry, src, Options{})
	if len(ds) != 0 {
		t.Fatalf("examples/support_bot 应零诊断，实际 %d 条：\n%s", len(ds), diag.RenderAll(ds))
	}
	// 4 个模块（support.hermes + support.h + models/intent.h + models/order.h）
	if len(res.Modules) < 4 {
		t.Errorf("展开出 %d 个模块，期望至少 4：%+v", len(res.Modules), res.Modules)
	}
	if res.Modules[0].Kind != "hermes" {
		t.Errorf("第一个模块应是 entry（kind=hermes），实际 %q", res.Modules[0].Kind)
	}
	// 模块路径必须是相对的 POSIX 路径（IR 可重现构建要求）。
	for _, m := range res.Modules {
		if filepath.IsAbs(m.Path) || strings.Contains(m.Path, "\\") {
			t.Errorf("模块路径 %q 不是相对 POSIX 路径", m.Path)
		}
	}
	// `#link <std/net>` 与 `<std/model>` 各一条。
	if len(res.Links) < 2 {
		t.Errorf("应解析出至少 2 条 #link，实际 %d：%+v", len(res.Links), res.Links)
	}
	for _, l := range res.Links {
		if l.Kind != "std" {
			t.Errorf("support.hermes 只该有 std 链接，实际 %+v", l)
		}
	}
}

// E3015：`#include "x.hermes"` —— 文案必须给出可复制的 #link 那一行。
func TestIncludeHermesIsRejected(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "impl.hermes", "export struct S { a: int; }\n")
	entry := write(t, dir, "main.hermes", `#include "impl.hermes"
export workflow f() -> string { return "x"; }
`)
	src, _ := os.ReadFile(entry)
	_, ds := Resolve(entry, src, Options{})
	var hit *diag.Diag
	for i := range ds {
		if ds[i].Code == diag.CodeIncludeHermes {
			hit = &ds[i]
		}
	}
	if hit == nil {
		t.Fatalf("期望 E3015，实际：%s", codes(ds))
	}
	// "修复建议里必须含可直接复制的那一行" 是 E3015 的文案契约。
	joined := strings.Join(hit.Fix, " | ")
	if !strings.Contains(joined, `#link "impl";`) {
		t.Errorf("E3015 的修复建议里没有可复制的 #link 行：%v", hit.Fix)
	}
}

// E1009：路径含 ..
func TestPathEscapeIsRejected(t *testing.T) {
	dir := t.TempDir()
	entry := write(t, dir, "main.hermes", `#include "../outside.h"
export workflow f() -> string { return "x"; }
`)
	src, _ := os.ReadFile(entry)
	_, ds := Resolve(entry, src, Options{})
	if !hasCode(ds, diag.CodePathEscape) {
		t.Fatalf("期望 E1009，实际：%s", codes(ds))
	}
}

// E3002：环，且链要完整、每跳都是模块名。
func TestIncludeCycleIsRejected(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.h", `#include "b.h"
export struct A { x: int; }
`)
	write(t, dir, "b.h", `#include "a.h"
export struct B { y: int; }
`)
	entry := write(t, dir, "main.hermes", `#include "a.h"
export workflow f() -> string { return "x"; }
`)
	src, _ := os.ReadFile(entry)
	_, ds := Resolve(entry, src, Options{})
	var chain string
	for _, d := range ds {
		if d.Code == diag.CodeIncludeCycle {
			chain = d.Message
		}
	}
	if chain == "" {
		t.Fatalf("期望 E3002，实际：%s", codes(ds))
	}
	// 链条必须**只含环上的节点**，且用 " -> " 连接。
	// 刻意不包含入口 main.hermes：它只是读文件的起点，不在环上 ——
	// 把它画进链条会让人以为入口也要改，而实际要改的是 a.h / b.h 之间的那条边。
	for _, want := range []string{"a.h", "b.h", "->"} {
		if !strings.Contains(chain, want) {
			t.Errorf("环的链条里缺 %q：%s", want, chain)
		}
	}
	if strings.Contains(chain, "main.hermes") {
		t.Errorf("入口不该被画进环的链条（它不在环上）：%s", chain)
	}
}

// include-once：同一个文件经两条路径 include，只展开一次。
func TestIncludeOnce(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "common.h", "export struct C { v: int; }\n")
	write(t, dir, "a.h", `#include "common.h"
export struct A { c: C; }
`)
	write(t, dir, "b.h", `#include "common.h"
export struct B { c: C; }
`)
	entry := write(t, dir, "main.hermes", `#include "a.h"
#include "b.h"
export workflow f() -> string { return "x"; }
`)
	src, _ := os.ReadFile(entry)
	res, ds := Resolve(entry, src, Options{})
	if len(ds) != 0 {
		t.Fatalf("应零诊断，实际：\n%s", diag.RenderAll(ds))
	}
	count := 0
	for _, m := range res.Modules {
		if m.Path == "common.h" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("common.h 被展开 %d 次，include-once 要求恰好 1 次", count)
	}
}

// 搜索顺序：当前目录优先于 -I，-I 优先于项目根。
func TestSearchOrder(t *testing.T) {
	dir := t.TempDir()
	incDir := filepath.Join(dir, "vendor")
	// 三点各放一份同名头，内容不同，靠 struct 名区分最终命中的是哪个。
	write(t, dir, "here/x.h", "export struct FromHere { a: int; }\n")
	write(t, incDir, "x.h", "export struct FromInclude { a: int; }\n")
	write(t, dir, "x.h", "export struct FromRoot { a: int; }\n")

	entry := write(t, dir, "here/main.hermes", `#include "x.h"
export workflow f() -> string { return "x"; }
`)
	src, _ := os.ReadFile(entry)
	res, ds := Resolve(entry, src, Options{IncludeDirs: []string{incDir}, ProjectRoot: dir})
	if len(ds) != 0 {
		t.Fatalf("应零诊断，实际：\n%s", diag.RenderAll(ds))
	}
	// 当前目录优先：命中 here/x.h
	found := ""
	for _, m := range res.Modules {
		if strings.HasSuffix(m.AbsPath, "x.h") {
			found = m.AbsPath
		}
	}
	if !strings.Contains(found, "here") {
		t.Errorf("搜索顺序错：期望命中当前目录的 here/x.h，实际 %q", found)
	}
}

// 找不到时错误里必须列出**试过的每个路径**。
func TestMissingIncludeListsCandidates(t *testing.T) {
	dir := t.TempDir()
	entry := write(t, dir, "main.hermes", `#include "nope.h"
export workflow f() -> string { return "x"; }
`)
	src, _ := os.ReadFile(entry)
	_, ds := Resolve(entry, src, Options{})
	if len(ds) == 0 {
		t.Fatal("找不到头文件时必须报错")
	}
	msg := ds[0].Message
	if !strings.Contains(msg, "nope.h") || !strings.Contains(msg, "tried:") {
		t.Errorf("错误里没有列出尝试过的路径：%s", msg)
	}
	// 还要提示 #link —— 这是从 v3.0 迁移过来的人 100% 会踩的一脚。
	if !strings.Contains(msg, "#link") {
		t.Errorf("错误里没有 #link 提示：%s", msg)
	}
}

// 内置头只查内存表，**不触文件系统**：即使磁盘上真有一个 net.h 也不该被读到。
func TestBuiltinIncludesDoNotTouchDisk(t *testing.T) {
	dir := t.TempDir()
	// 故意在项目根放一个 net.h：如果实现走磁盘，就会读到它而不是内置表。
	write(t, dir, "std/net.h", "export struct Decoy { a: int; }\n")
	entry := write(t, dir, "main.hermes", `#include <std/net.h>
export workflow f() -> string { return "x"; }
`)
	src, _ := os.ReadFile(entry)
	res, ds := Resolve(entry, src, Options{})
	if len(ds) != 0 {
		t.Fatalf("内置 include 应零诊断，实际：\n%s", diag.RenderAll(ds))
	}
	// 内置头现在**有字节**（T42 的 go:embed），所以它会出现在 modules[] 里 ——
	// 但必须满足两条：Builtin=true，且内容**不是**磁盘上那个诱饵。
	for _, m := range res.Modules {
		if m.Path != "std/net.h" {
			continue
		}
		if !m.Builtin {
			t.Errorf("内置模块的 Builtin 必须为 true，实际 false（%q）", m.Path)
		}
		if strings.Contains(string(m.Source), "Decoy") {
			t.Fatalf("读到了磁盘上的诱饵文件：内置解析必须只查内存表")
		}
		if len(m.Source) == 0 {
			t.Errorf("内置模块应有真实字节（T42），实际为空")
		}
	}
}

// E3001：<std/nope.h> 不存在，且错误里列出全部可用模块。
func TestUnknownBuiltinListsAvailable(t *testing.T) {
	dir := t.TempDir()
	entry := write(t, dir, "main.hermes", `#include <std/nope.h>
export workflow f() -> string { return "x"; }
`)
	src, _ := os.ReadFile(entry)
	_, ds := Resolve(entry, src, Options{})
	if !hasCode(ds, diag.CodeLinkNotStd) && !hasCode(ds, diag.CodeLoadFailed) {
		t.Fatalf("期望 E3001，实际：%s", codes(ds))
	}
	// 可用模块列表在 help 里（message 保持一句话，列表放 help 才不臃肿）。
	for _, d := range ds {
		if strings.Contains(d.Message, "<std/net>") || strings.Contains(strings.Join(d.Fix, " "), "<std/net>") {
			return
		}
	}
	t.Errorf("错误里没有列出可用内置模块：%s", diag.RenderAll(ds))
}

// E1010：`#link "models.hbc"` 带后缀。
func TestLinkWithSuffixIsRejected(t *testing.T) {
	dir := t.TempDir()
	entry := write(t, dir, "main.hermes", `#link "models.hbc";
export workflow f() -> string { return "x"; }
`)
	src, _ := os.ReadFile(entry)
	_, ds := Resolve(entry, src, Options{})
	if !hasCode(ds, diag.CodeLinkBadModule) {
		t.Fatalf("期望 E1010，实际：%s", codes(ds))
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("找不到 go.mod")
	return ""
}
