package diag

import (
	"os"
	"strings"
	"testing"
)

// 颜色关闭时 RenderAllColored 必须逐字节等于 RenderAll，否则会污染下游测试
// （cmd 侧不少断言直接比对 RenderAll 的纯文本）。
func TestRenderAllColoredOffIsIdentical(t *testing.T) {
	// 直接构造（阶段 2c 之后 Go 侧没有 catalog 了）：这些测试验的是**上色
	// 不改可见字节**，与文案来源无关，所以字段写死比查表更贴题。
	ds := []Diag{
		{
			Severity: SeverityError, Code: CodeDanglingRef,
			Loc:     Loc{Module: "m.hbc", Line: 7, Col: 9},
			Message: "x points at 3, but nodes has 5 entries (0..4)",
			Why:     "every reference must index into the table it names.",
			Fix:     []string{"drop the reference", "or add the missing entry"},
		},
		{
			Severity: SeverityWarning, Code: CodeDupID,
			Loc:     Loc{Module: "m.hbc", Line: 1, Col: 1},
			Message: "duplicate node id 3 in workflow w",
			Why:     "ids are array indices.",
			Fix:     []string{"renumber"},
		},
	}
	plain := RenderAll(ds)
	off := RenderAllColored(ds, ColorNever, os.Stderr)
	if off != plain {
		t.Fatalf("ColorNever 必须 == RenderAll：\n got %q\nwant %q", off, plain)
	}
	// 即便在不会上色的 writer 上用 ColorAlways，也只验证"不被破坏"的契约：
	// 这里用一个普通 bytes.Buffer 当 writer，ShouldColor 对非零 *os.File 返回 false，
	// 所以实际还是走 RenderAll 分支。真正的 ANSI 路径在下面 ColorAlways 用例里用
	// 强制模式覆盖。
}

// ColorAlways 必须：上色（含 ANSI 转义），且所有既有可见文本子串仍保留
// （下游 grep / 日志解析依赖这些子串，且 TestRenderIsStable 锁的就是它们）。
func TestRenderAllColoredAlwaysKeepsSubstrings(t *testing.T) {
	ds := []Diag{{
		Severity: SeverityError, Code: CodeDanglingRef,
		Loc:     Loc{Module: "m.hbc", Line: 7, Col: 9},
		Message: "x points at 3, but nodes has 5 entries (0..4)",
		Why:     "every reference must index into the table it names.",
		Fix:     []string{"drop the reference", "or add the missing entry"},
	}}
	got := RenderAllColored(ds, ColorAlways, os.Stdout)

	if !strings.Contains(got, "\x1b[") {
		t.Fatalf("ColorAlways 应含 ANSI 转义：%q", got)
	}
	for _, want := range []string{
		"error[E_IR_DANGLING_REF]: ",
		"  --> m.hbc",
		"      line 7, col 9",
		"   why: ",
		"  help: ",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ColorAlways 不应破坏子串 %q：\n%s", want, got)
		}
	}
	// 不能残留未替换占位符（与 Render 的契约一致）。
	if strings.IndexByte(got, '{') >= 0 {
		t.Errorf("渲染结果残留未替换占位符：%s", got)
	}
}

// ShouldColor 的边界：NO_COLOR 与 TERM=dumb 必须压制上色，哪怕 mode=auto。
func TestShouldColorEnvSuppression(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if ShouldColor(ColorAuto, os.Stdout) {
		t.Fatal("NO_COLOR 设了仍返回上色，违反 no-color.org 约定")
	}
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "dumb")
	if ShouldColor(ColorAuto, os.Stdout) {
		t.Fatal("TERM=dumb 仍返回上色")
	}
}

// RenderExplain 的头行严重程度必须来自码本身：`W` 开头的警告不能印成 `error[…]`。
//
// 这条踩过：explain 一律打 `error[W2003]:`，用户看到的是一条"错误"，
// 而构建里同一条码只是警告、不阻塞 —— 同一件事两种说法。
func TestRenderExplainSeverityFollowsCode(t *testing.T) {
	gotW := RenderExplain("W2003", "extra semicolon", "why-x", []string{"f1", "f2"}, ColorNever, os.Stdout)
	if !strings.HasPrefix(gotW, "warning[W2003]: ") {
		t.Fatalf("W 码头行应为 warning[W2003]: ，实际 %q", gotW)
	}
	gotE := RenderExplain("E4016", "no such field", "why-y", []string{"f1", "f2"}, ColorNever, os.Stdout)
	if !strings.HasPrefix(gotE, "error[E4016]: ") {
		t.Fatalf("E 码头行应为 error[E4016]: ，实际 %q", gotE)
	}
	// explain 没有具体源码位置，不该有定位行（否则打出 `  --> ` 空箭头）。
	for _, s := range []string{gotW, gotE} {
		if strings.Contains(s, "-->") {
			t.Errorf("explain 输出不该含定位行：%q", s)
		}
		if strings.IndexByte(s, '{') >= 0 {
			t.Errorf("渲染结果残留未替换占位符：%s", s)
		}
	}
	// 上色时警告走黄色（1;33）、错误走红色（1;31）。
	if w := RenderExplain("W2003", "m", "w", nil, ColorAlways, os.Stdout); !strings.Contains(w, "\x1b[1;33m") {
		t.Errorf("警告应上黄色：%q", w)
	}
	if e := RenderExplain("E4016", "m", "w", nil, ColorAlways, os.Stdout); !strings.Contains(e, "\x1b[1;31m") {
		t.Errorf("错误应上红色：%q", e)
	}
}
