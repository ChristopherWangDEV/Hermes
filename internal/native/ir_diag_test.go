package native

import (
	"regexp"
	"strings"
	"testing"

	"hermes/internal/diag"
)

// placeholderRELocal 只匹配合法占位符名（与 cmd/hermes 的同名判据一致）。
var placeholderRELocal = regexp.MustCompile(`\{[a-z_][a-z0-9_]*\}`)

// TestDiagTextComesFromCppCatalog 是阶段 2b 的**渲染权证明**：C++ catalog 是
// 文案的唯一来源，Go 侧不再参与拼装。
//
// 判据写法：把 C++ 渲染结果钉成**字面量前缀**（不是引用常量，也不是
// "包含 code"这种恒真断言）。反向变异（改 C++ catalog 注入哨兵）必须让它变红，
// 命令记录在 commit message 里。
func TestDiagTextComesFromCppCatalog(t *testing.T) {
	ds := []NativeDiag{{
		Code:     "E_IR_DANGLING_REF",
		Severity: "error",
		Module:   "w.hermes",
		Line:     7,
		Col:      3,
		EndLine:  7,
		EndCol:   9,
		Args: map[string]string{
			"what": "node", "id": "99", "table": "nodes",
			"range": "0..4", "path": "workflows[0].nodes[1]",
		},
	}}
	got := ValidateDiagsToDiag(ds)
	if len(got) != 1 {
		t.Fatalf("应产出 1 条诊断，实际 %d", len(got))
	}
	d := got[0]

	// 1) 整段文本由 C++ 渲染（RenderedText 非空，且与字段一致）。
	if d.RenderedText == "" {
		t.Fatal("RenderedText 必须由 C++ 填充（阶段 2b 默认路径）")
	}
	if !strings.HasPrefix(d.RenderedText, "error[E_IR_DANGLING_REF]: ") {
		t.Fatalf("RenderedText 头行不对：%q", firstLine(d.RenderedText))
	}
	// 2) 头行文案是 catalog 的英文模板（钉字面量）。
	wantHead := "error[E_IR_DANGLING_REF]: node points at 99, but nodes has 0..4"
	if firstLine(d.RenderedText) != wantHead {
		t.Fatalf("头行必须逐字来自 C++ catalog:\n got: %q\nwant: %q", firstLine(d.RenderedText), wantHead)
	}
	// 3) Go 的结构化字段也来自 C++（不是 Go catalog）。
	if !strings.HasPrefix(d.Message, "node points at 99") {
		t.Fatalf("Message 应来自 C++ 模板，实际 %q", d.Message)
	}
	if strings.Contains(d.RenderedText, "{") && strings.Contains(d.RenderedText, "}") {
		if m := placeholderRELocal.FindString(d.RenderedText); m != "" {
			t.Fatalf("RenderedText 残留占位符 %s", m)
		}
	}
	// 4) 语言切换走同一份 C++ catalog。
	zh, err := RenderDiagText(DiagRenderInput{
		Code: "E_IR_DANGLING_REF", Severity: "error", Module: "w.hermes",
		Line: 7, Col: 3, EndLine: 7, EndCol: 9,
		Args: ds[0].Args, Lang: string(diag.LangZH),
	})
	if err != nil {
		t.Fatalf("中文渲染失败: %v", err)
	}
	if !strings.HasPrefix(zh, "error[E_IR_DANGLING_REF]: ") {
		t.Fatalf("中文头行骨架应不变：%q", firstLine(zh))
	}
	if !strings.Contains(zh, "节点") && !strings.Contains(zh, "引用") {
		t.Fatalf("中文渲染应含中文文案，实际 %q", firstLine(zh))
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
