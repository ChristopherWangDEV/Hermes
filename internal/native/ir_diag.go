package native

import (
	"encoding/json"

	"hermes/internal/diag"
)

// NativeDiag is the flat shape of a native (C++) diagnostic: line/col live on
// the outer object rather than in a nested Loc. Decoding into diag.Diag
// directly would silently lose those fields.
//
// Notes is `[{"module","line","col","message"}]`: a note points back to the
// source of a derived limit, and dropping it loses half of the diagnostic.
type NativeDiag struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	// Args carries the template instance values produced by the C++ validator.
	// diag.New re-renders them from the Go catalog; the C++ message text is not
	// consumed on this path.
	Args    map[string]string `json:"args"`
	Line    int               `json:"line"`
	Col     int               `json:"col"`
	EndLine int               `json:"end_line"`
	EndCol  int               `json:"end_col"`
	Module  string            `json:"module"`
	Why     string            `json:"why"`
	Fix     []string          `json:"fix"`
	Notes   []NativeNot       `json:"notes"`
	// Edits is the optional machine-applicable fix shape emitted by C++:
	// [{"span":{"line","col","end_line","end_col"},"replacement":"..."}].
	Edits []NativeEdit `json:"edits"`
}

// NativeNot is one associated note of a native diagnostic.
type NativeNot struct {
	Module  string `json:"module"`
	Line    int    `json:"line"`
	Col     int    `json:"col"`
	Message string `json:"message"`
}

// NativeEdit and NativeSpan mirror the edits[] shape written by C++. They are
// only used for decoding; the public representation is diag.Edit.
type NativeEdit struct {
	Span        NativeSpan `json:"span"`
	Replacement string     `json:"replacement"`
}

// NativeSpan is a source span inside a native diagnostic edit.
type NativeSpan struct {
	Line    int `json:"line"`
	Col     int `json:"col"`
	EndLine int `json:"end_line"`
	EndCol  int `json:"end_col"`
}

// ParseValidateDiags decodes the diagnostic JSON array returned by the C++
// validator. ok=false means the payload could not be decoded; callers must not
// treat that as "no diagnostics" but must surface the raw output instead.
func ParseValidateDiags(diagJSON string) (ds []NativeDiag, ok bool) {
	if diagJSON == "" || diagJSON == "[]" {
		return nil, true
	}
	if err := json.Unmarshal([]byte(diagJSON), &ds); err != nil {
		return nil, false
	}
	return ds, true
}

// NativeSeverity maps a native severity string onto diag.Severity.
//
// The mapping is deliberately conservative: unknown and empty values become
// SeverityError. Treating an unrecognized level as a warning could let a
// blocking diagnostic slip through silently.
func NativeSeverity(s string) diag.Severity {
	switch s {
	case "warning":
		return diag.SeverityWarning
	case "note":
		return diag.SeverityNote
	case "error", "":
		return diag.SeverityError
	}
	return diag.SeverityError
}

// ValidateDiagsToDiag converts validator diagnostics into diag.Diag.
//
// 文案由 **C++ catalog** 渲染（阶段 2b）：NativeDiag.Message 仍然不被消费 ——
// 它是发射点硬编码的英文，不是 catalog 模板；消费它会让双语彻底失效。
// 这里只把 (code, args, lang, 位置) 交给 C++，拿回整段文本塞进 RenderedText。
func ValidateDiagsToDiag(ds []NativeDiag) []diag.Diag {
	lang := string(diag.LangFromEnv())
	out := make([]diag.Diag, 0, len(ds))
	for _, d := range ds {
		loc := diag.Loc{
			Module:  d.Module,
			Line:    d.Line,
			Col:     d.Col,
			EndLine: d.EndLine,
			EndCol:  d.EndCol,
		}
		// 文案与结构化字段都来自 C++ catalog（阶段 2b）；NativeDiag.Message
		// 仍不消费 —— 它是发射点硬编码的英文，消费它会让双语失效。
		dg, _ := DiagFromCpp(d.Code, d.Severity, loc, "", d.Args, toDiagNotes(d.Notes), lang)
		// 机器可读的 Notes / Edits 由 JSON 搬运，不由渲染器产生。
		for _, n := range d.Notes {
			dg = dg.WithNote(diag.Loc{Module: n.Module, Line: n.Line, Col: n.Col}, n.Message)
		}
		// M2：把原生层的结构化编辑搬进 diag.Diag.Edits（机器可应用修正提示）。
		for _, e := range d.Edits {
			dg.Edits = append(dg.Edits, diag.Edit{
				Loc: diag.Loc{
					Module:  d.Module,
					Line:    e.Span.Line,
					Col:     e.Span.Col,
					EndLine: e.Span.EndLine,
					EndCol:  e.Span.EndCol,
				},
				Replacement: e.Replacement,
			})
		}
		out = append(out, dg)
	}
	return out
}

// toDiagNotes 把原生 note 转成渲染输入。
func toDiagNotes(ns []NativeNot) []DiagNote {
	out := make([]DiagNote, 0, len(ns))
	for _, n := range ns {
		out = append(out, DiagNote{Module: n.Module, Line: n.Line, Col: n.Col, Message: n.Message})
	}
	return out
}
