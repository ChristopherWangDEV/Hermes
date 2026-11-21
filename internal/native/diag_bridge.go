// diag_bridge.go —— 双语诊断渲染的 C++ 入口（阶段 2b）。
//
// 分工：C++ 是**文案权威**（native/cpp/diag_catalog.cpp + diag_render.cpp），
// Go 只负责"宿主事实"——语言选择（HERMES_LANG）、着色、源码片段、IO。
//
// 为什么这一层必须住在 internal/native：只有这个包链着 `-lhermes_parser`
// （见 parse.go 的 LDFLAGS）。**不能让 internal/ir 或 internal/diag 传递依赖
// 到 cgo** —— `cmd/hermesrt` 会把 ir 编译进 libhermesrt.a，而 `hermes cc`
// 只链那一个归档，C++ 符号到不了，AOT 静态链接当场失败。
package native

/*
#include <stdlib.h>
#include "hermes_diag.h"
*/
import "C"

import (
	"encoding/json"
	"errors"
	"fmt"
	"unsafe"

	"hermes/internal/diag"
)

// DiagNote 是一条关联位置的说明（note 指向派生上界的来源）。
type DiagNote struct {
	Module  string
	Path    string
	Line    int
	Col     int
	Message string
}

// DiagRenderInput 是渲染一条诊断所需的全部输入。
//
// 字段划分照抄 C 侧 hm_diag：前九个是"这条诊断是什么"，Args 是模板实参，
// Notes 是关联位置。**不含**任何 IO 或着色信息——那些留在 Go。
type DiagRenderInput struct {
	Code     string
	Severity string
	Module   string
	Path     string
	Snippet  string
	Line     int
	Col      int
	EndLine  int
	EndCol   int
	Args     map[string]string
	Notes    []DiagNote
	Lang     string
}

// maxDiagGrowAttempts 与 parse.go 的其它入口同一套缓冲协商策略：
// 契约规定缓冲区不足时 out_len 回填实际写入量，溢出时恰好等于 cap，
// 所以判据是 `outLen >= cap` 视为可能截断并扩容。
const maxDiagGrowAttempts = 8

// renderWith 调一个 `f(out, cap, &len)` 形状的 C 函数并按需扩容。
func renderWith(what string, call func(out *C.char, cap C.size_t, outLen *C.size_t) C.int) (string, error) {
	capHint := 4096
	for attempt := 0; attempt < maxDiagGrowAttempts; attempt++ {
		buf := make([]byte, capHint)
		var outLen C.size_t
		rc := call((*C.char)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)), &outLen)
		if rc != C.HM_OK {
			return "", fmt.Errorf("native: %s failed with status %d", what, int(rc))
		}
		if int(outLen) < len(buf) {
			// 契约：out_len 是**含结尾 NUL 的字节数**（diag_write_buffer 里
			// n+1）。直接切片会把 NUL 带进 Go 字符串，比对与输出都会多一个
			// 不可见字节 —— 这类缺陷在终端上看不出来，只有逐字节比对会红。
			n := int(outLen)
			if n > 0 && buf[n-1] == 0 {
				n--
			}
			return string(buf[:n]), nil
		}
		capHint *= 2
	}
	return "", fmt.Errorf("native: %s exceeded %d grow attempts", what, maxDiagGrowAttempts)
}

// withCDiag 构造 C 侧 hm_diag（含 args / notes 两个数组），交给 call 执行。
//
// args/notes 数组必须**分配在 C 内存里**：cgo 禁止"Go 内存里嵌套指向 Go
// 内存的指针"这一次调用（hm_diag 结构里装着指向数组的指针）。用 Go slice
// 会在运行时 panic —— cgo argument has Go pointer to unpinned Go pointer。
func withCDiag(in DiagRenderInput,
	call func(d *C.hm_diag, lang *C.char, out *C.char, cap C.size_t, outLen *C.size_t) C.int,
	what string) (string, error) {
	cCode := C.CString(in.Code)
	defer C.free(unsafe.Pointer(cCode))
	cSev := C.CString(in.Severity)
	defer C.free(unsafe.Pointer(cSev))
	cMod := C.CString(in.Module)
	defer C.free(unsafe.Pointer(cMod))
	cPath := C.CString(in.Path)
	defer C.free(unsafe.Pointer(cPath))
	cSnip := C.CString(in.Snippet)
	defer C.free(unsafe.Pointer(cSnip))
	cLang := C.CString(in.Lang)
	defer C.free(unsafe.Pointer(cLang))

	nArgs := len(in.Args)
	var argsPtr *C.hm_diag_arg
	if nArgs > 0 {
		argsPtr = (*C.hm_diag_arg)(C.malloc(C.size_t(nArgs) * C.size_t(unsafe.Sizeof(C.hm_diag_arg{}))))
		if argsPtr == nil {
			return "", errors.New("native: out of memory building diag args")
		}
		defer C.free(unsafe.Pointer(argsPtr))
	}
	cArgs := unsafe.Slice(argsPtr, nArgs)
	argNames := make([]*C.char, 0, nArgs)
	argVals := make([]*C.char, 0, nArgs)
	for k, v := range in.Args {
		argNames = append(argNames, C.CString(k))
		argVals = append(argVals, C.CString(v))
	}
	defer func() {
		for i := range argNames {
			C.free(unsafe.Pointer(argNames[i]))
			C.free(unsafe.Pointer(argVals[i]))
		}
	}()
	for i := 0; i < nArgs; i++ {
		cArgs[i].name = argNames[i]
		cArgs[i].value = argVals[i]
	}

	nNotes := len(in.Notes)
	var notesPtr *C.hm_diag_note
	if nNotes > 0 {
		notesPtr = (*C.hm_diag_note)(C.malloc(C.size_t(nNotes) * C.size_t(unsafe.Sizeof(C.hm_diag_note{}))))
		if notesPtr == nil {
			return "", errors.New("native: out of memory building diag notes")
		}
		defer C.free(unsafe.Pointer(notesPtr))
	}
	cNotes := unsafe.Slice(notesPtr, nNotes)
	noteMods := make([]*C.char, 0, nNotes)
	notePaths := make([]*C.char, 0, nNotes)
	noteMsgs := make([]*C.char, 0, nNotes)
	for _, n := range in.Notes {
		noteMods = append(noteMods, C.CString(n.Module))
		notePaths = append(notePaths, C.CString(n.Path))
		noteMsgs = append(noteMsgs, C.CString(n.Message))
	}
	defer func() {
		for i := range noteMods {
			C.free(unsafe.Pointer(noteMods[i]))
			C.free(unsafe.Pointer(notePaths[i]))
			C.free(unsafe.Pointer(noteMsgs[i]))
		}
	}()
	for i := 0; i < nNotes; i++ {
		cNotes[i].module = noteMods[i]
		cNotes[i].path = notePaths[i]
		cNotes[i].line = C.int(in.Notes[i].Line)
		cNotes[i].col = C.int(in.Notes[i].Col)
		cNotes[i].message = noteMsgs[i]
	}

	d := C.hm_diag{
		code:     cCode,
		severity: cSev,
		module:   cMod,
		path:     cPath,
		line:     C.int(in.Line),
		col:      C.int(in.Col),
		end_line: C.int(in.EndLine),
		end_col:  C.int(in.EndCol),
		snippet:  cSnip,
		args:     argsPtr,
		n_args:   C.size_t(nArgs),
		notes:    notesPtr,
		n_notes:  C.size_t(nNotes),
	}
	return renderWith(what, func(out *C.char, cap C.size_t, outLen *C.size_t) C.int {
		return call(&d, cLang, out, cap, outLen)
	})
}

// RenderDiagText 用 C++ catalog 渲染一条诊断的完整人读文本。
//
// 与 Go 侧 `diag.New(...).Render()` 的输出**逐字节相同**由
// TestDiagRenderMatchesGoCatalog 全量钉住（112 码 × 2 语种 × 2 变体）。
func RenderDiagText(in DiagRenderInput) (string, error) {
	return withCDiag(in, func(d *C.hm_diag, lang *C.char, out *C.char, cap C.size_t, outLen *C.size_t) C.int {
		return C.hm_diag_render(d, lang, out, cap, outLen)
	}, "hm_diag_render")
}

// DiagFields 是 C++ 渲染出的一条诊断的字段（与 diag.Diag 一一对应）。
type DiagFields struct {
	Code     string   `json:"code"`
	Severity string   `json:"severity"`
	Message  string   `json:"message"`
	Why      string   `json:"why"`
	Fix      []string `json:"fixes"`
	Text     string   `json:"text"`
}

// RenderDiagFields 用 C++ catalog 渲染一条诊断的**全部字段**。
//
// 与 RenderDiagText 的区别：那个只给整段文本，这个还给结构化字段。
// 为什么要两个：Go 的 Diag 结构仍要承载 message/why/fix（LSP 与 explain 读），
// 而它们必须来自同一份模板 —— 让 Go 的 catalog 继续填字段就等于让两套文案
// 并存，那正是 2c 要删掉的东西。
func RenderDiagFields(in DiagRenderInput) (DiagFields, error) {
	blob, err := withCDiag(in, func(d *C.hm_diag, lang *C.char, out *C.char, cap C.size_t, outLen *C.size_t) C.int {
		return C.hm_diag_render_json(d, lang, out, cap, outLen)
	}, "hm_diag_render_json")
	if err != nil {
		return DiagFields{}, err
	}
	var f DiagFields
	if err := json.Unmarshal([]byte(blob), &f); err != nil {
		return DiagFields{}, fmt.Errorf("native: diag fields JSON: %w", err)
	}
	return f, nil
}

// ExplainCode 返回 `hermes explain <code>` 的人读文本（已含 docs: 行）。
// ok=false 表示该码未在 C++ catalog 登记（C 侧对未登记码返回非 0）。
func ExplainCode(code, lang string) (string, bool) {
	cCode := C.CString(code)
	defer C.free(unsafe.Pointer(cCode))
	cLang := C.CString(lang)
	defer C.free(unsafe.Pointer(cLang))
	s, err := renderWith("hm_diag_explain", func(out *C.char, cap C.size_t, outLen *C.size_t) C.int {
		return C.hm_diag_explain(cCode, cLang, out, cap, outLen)
	})
	if err != nil {
		return "", false
	}
	return s, true
}

// ExplainCodeJSON 返回 explain 的 JSON 形态（code/message/why/fixes/docs）。
//
// 注意：调用方（cmd/hermes 的 --json）**必须**再 Unmarshal 成 map 后用
// json.MarshalIndent 重排——那是今天的字节契约（2 空格缩进 + HTML 转义）。
// 直接透传这份紧凑 JSON 会改变输出字节。
func ExplainCodeJSON(code, lang string) ([]byte, bool) {
	cCode := C.CString(code)
	defer C.free(unsafe.Pointer(cCode))
	cLang := C.CString(lang)
	defer C.free(unsafe.Pointer(cLang))
	s, err := renderWith("hm_diag_explain_json", func(out *C.char, cap C.size_t, outLen *C.size_t) C.int {
		return C.hm_diag_explain_json(cCode, cLang, out, cap, outLen)
	})
	if err != nil {
		return nil, false
	}
	return []byte(s), true
}

// DiagFromCpp 用 C++ catalog 构造一条完整诊断（文案字段 + 整段文本）。
//
// 这是 2b 之后 Go 侧**唯一**的 diag 构造入口：Go 不再查自己的 catalog，
// 因此不存在"两套文案并存"。第二个返回值为 false 表示渲染失败（调用方
// 应保留一个无文案的 Diag，至少不丢位置与代码）。
func DiagFromCpp(code, severity string, loc diag.Loc, snippet string, args map[string]string,
	notes []DiagNote, lang string) (diag.Diag, bool) {
	sev := diag.SeverityError
	switch severity {
	case "warning":
		sev = diag.SeverityWarning
	case "note":
		sev = diag.SeverityNote
	}
	fields, err := RenderDiagFields(DiagRenderInput{
		Code: code, Severity: severity,
		Module: loc.Module, Path: loc.Path, Snippet: snippet,
		Line: loc.Line, Col: loc.Col, EndLine: loc.EndLine, EndCol: loc.EndCol,
		Args: args, Notes: notes, Lang: lang,
	})
	if err != nil {
		return diag.Diag{Severity: sev, Code: code, Loc: loc}, false
	}
	return diag.Diag{
		Severity:     sev,
		Code:         code,
		Loc:          loc,
		Message:      fields.Message,
		Why:          fields.Why,
		Fix:          fields.Fix,
		Snippet:      snippet,
		RenderedText: fields.Text,
	}, true
}
