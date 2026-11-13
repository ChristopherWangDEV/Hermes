package main

import (
	"testing"

	"hermes/internal/diag"
)

// TestParseNativeDiagsEdits 守 M2 的端到端链路：
// C++ diag_to_json 的 edits[] → parseNativeDiags → diag.Diag.Edits（机器可应用修正提示）。
//
// 它证明：(1) 原生 JSON 里的 edits[] 能被 parseNativeDiags 反序列化出来；
// (2) 那段"把原生 edit 搬进 diag.Diag.Edits"的逻辑（renderDiags 里同样的一段）
// 能产出可被 LSP/工具链直接消费的 diag.Edit：span 正确、replacement 正确。
func TestParseNativeDiagsEdits(t *testing.T) {
	// 形如 C++ diag_to_json 真实产出的片段：字段名 statuss 拼错，建议改 status。
	js := `[{"severity":"error","code":"E4016",` +
		`"message":"struct OrderInfo has no field statuss",` +
		`"line":3,"col":18,"end_line":3,"end_col":25,"module":"dym.hermes",` +
		`"why":"Field access is checked against the struct declaration.",` +
		`"fix":["OrderInfo has: order_id, status, amount"],` +
		`"edits":[{"span":{"line":3,"col":18,"end_line":3,"end_col":25},"replacement":"status"}]}]`

	ds, ok := parseNativeDiags(js)
	if !ok {
		t.Fatalf("parseNativeDiags 应成功解析带 edits 的诊断")
	}
	if len(ds) != 1 {
		t.Fatalf("应有 1 条诊断，实际 %d", len(ds))
	}
	if len(ds[0].Edits) != 1 {
		t.Fatalf("应有 1 条 edit，实际 %d", len(ds[0].Edits))
	}
	e := ds[0].Edits[0]
	if e.Replacement != "status" {
		t.Errorf("edit.replacement 应为 status，实际 %q", e.Replacement)
	}
	if e.Span.Line != 3 || e.Span.Col != 18 || e.Span.EndLine != 3 || e.Span.EndCol != 25 {
		t.Errorf("edit.span 应为 3:18-3:25，实际 %d:%d-%d:%d",
			e.Span.Line, e.Span.Col, e.Span.EndLine, e.Span.EndCol)
	}

	// 复刻 renderDiags 里把原生 edit 搬进 diag.Diag.Edits 的逻辑，验证端到端能落到可消费的 Edit。
	out := make([]diag.Diag, 0, len(ds))
	for _, d := range ds {
		gd := diag.Diag{
			Severity: nativeSeverity(d.Severity),
			Code:     d.Code,
			Message:  d.Message,
			Loc:      diag.Loc{Module: d.Module, Line: d.Line, Col: d.Col, EndCol: d.EndCol},
			Why:      d.Why,
			Fix:      d.Fix,
		}
		for _, ev := range d.Edits {
			gd.Edits = append(gd.Edits, diag.Edit{
				Loc: diag.Loc{
					Module:  d.Module,
					Line:    ev.Span.Line,
					Col:     ev.Span.Col,
					EndLine: ev.Span.EndLine,
					EndCol:  ev.Span.EndCol,
				},
				Replacement: ev.Replacement,
			})
		}
		out = append(out, gd)
	}
	if len(out[0].Edits) != 1 {
		t.Fatalf("diag.Diag.Edits 应含 1 条，实际 %d", len(out[0].Edits))
	}
	if out[0].Edits[0].Replacement != "status" {
		t.Errorf("diag.Diag.Edits[0].Replacement 应为 status，实际 %q", out[0].Edits[0].Replacement)
	}
	if out[0].Edits[0].Loc.Line != 3 || out[0].Edits[0].Loc.Col != 18 {
		t.Errorf("diag.Diag.Edits[0].Loc 应为 3:18，实际 %d:%d",
			out[0].Edits[0].Loc.Line, out[0].Edits[0].Loc.Col)
	}

	// 负向：没有 edits 的诊断不应产生任何 Edits（保持向后兼容，不污染既有路径）。
	plain := `[{"severity":"error","code":"E4016","message":"x","line":1,"col":1,"end_line":1,"end_col":2,"module":"m","why":"y","fix":["z"]}]`
	pds, ok := parseNativeDiags(plain)
	if !ok || len(pds) != 1 {
		t.Fatalf("解析无 edits 的诊断应成功且得到 1 条")
	}
	if len(pds[0].Edits) != 0 {
		t.Errorf("无 edits 时原生 Edits 应为空，实际 %d", len(pds[0].Edits))
	}
}
