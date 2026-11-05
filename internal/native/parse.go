// parse.go —— libhermes_parser.a 的 cgo 绑定（ARCH §4.5 / §2.3 的 internal/native/parse.go）。
//
// 这一层刻意**只做搬运**：把 Go 字节交给前端，把字节拿回来。
// 不解析 dump（那是 internal/include 的事）、不做 include 解析、不读文件 ——
// 文件 IO 与跨文件语义都在 Go 的 internal/include，前端永远是纯函数。
//
// 为什么缓冲区要"2 倍增长重试"而不是"先问长度"：
// hermes_parser.h 契约第 3 条写明 —— 缓冲不足时 *_len 回填的是**实际写入字节数**，
// 溢出时它正好等于 cap，调用方无法从中区分"刚好装满"与"被截断"。
// 所以判定条件是 `*len >= cap` 视为可能截断并扩容，直到 `*len < cap`。
package native

/*
#cgo CFLAGS: -I${SRCDIR}/../../native/include
#cgo LDFLAGS: -L${SRCDIR}/../../build -lhermes_parser -lc++
// Linux 上 libc++ 的实现依赖 libunwind，而 Ubuntu 的 libunwind.a 又依赖 lzma。
// macOS 的 libc++ 自带展开器，不需要这两条 —— 所以按平台分开写。
#cgo linux LDFLAGS: -lunwind -llzma
#include <stdlib.h>
#include "hermes_parser.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

// ErrParserOOM 对应 C 侧的 -2（Arena 分配失败）。
var ErrParserOOM = errors.New("native: parser arena allocation failed")

// ParseResult 是一次解析的全部产出。
//
// Dump 是 AST 的确定性文本（每行 `Kind n=N l:c-el:ec "text"`，缩进 = 深度×2），
// Diag 是诊断的 JSON 数组文本。两者都在**成功**时也可能非空（dump 总有内容，
// diag 可能有 warning）。
type ParseResult struct {
	Dump      string
	Diag      string
	NodeCount int
}

// ParseVersion 返回前端版本串，形如 "hermes-parser/0.1"。
func ParseVersion() string {
	return C.GoString(C.hm_parse_version())
}

// KindName 把 AST dump 里的 Kind 整数翻成名字。
// 用途：Go 侧解析 dump 时若遇到不认识的 Kind 名，可用它反查，
// 避免"前端加了新 Kind、Go 侧静默跳过"这种漂移。
func KindName(kind int) string {
	buf := make([]byte, 64)
	if rc := C.hm_parse_kind_name(C.int(kind), (*C.char)(unsafe.Pointer(&buf[0])), C.size_t(len(buf))); rc != 0 {
		return ""
	}
	n := 0
	for n < len(buf) && buf[n] != 0 {
		n++
	}
	return string(buf[:n])
}

// Parse 解析一段源码字节。
//
// isHeader = true 时按 `.h`（接口文件）的语法限制：带体 workflow → E3005、
// 语句 → E3006、`#link` → E3017、带地址的 backend → E3020。
//
// src 不需要 NUL 结尾；filename 只用于诊断文本，不参与解析语义。
func Parse(src []byte, filename string, isHeader bool) (*ParseResult, error) {
	if len(src) == 0 {
		return nil, errors.New("native: empty source")
	}

	csrc := (*C.uint8_t)(unsafe.Pointer(&src[0]))
	cfname := C.CString(filename)
	defer C.free(unsafe.Pointer(cfname))

	var ih C.int
	if isHeader {
		ih = 1
	}

	// 初始容量：AST dump 大约是源字节的数倍，给 4× + 4 KiB 起步。
	dumpCap := len(src)*4 + 4096
	diagCap := 4096

	const maxAttempts = 8 // 2^8 = 256× 扩容上限，足够覆盖任何合法输入
	for attempt := 0; attempt < maxAttempts; attempt++ {
		dumpBuf := make([]byte, dumpCap)
		diagBuf := make([]byte, diagCap)

		var dumpLen, diagLen C.size_t
		var nodeCount C.int
		rc := C.hm_parse(csrc, C.size_t(len(src)), cfname, ih,
			(*C.char)(unsafe.Pointer(&dumpBuf[0])), C.size_t(dumpCap), &dumpLen,
			(*C.char)(unsafe.Pointer(&diagBuf[0])), C.size_t(diagCap), &diagLen,
			&nodeCount)
		if rc != 0 {
			return nil, fmt.Errorf("%w (rc=%d)", ErrParserOOM, int(rc))
		}

		dl, gl := int(dumpLen), int(diagLen)
		if dl < dumpCap && gl < diagCap {
			// 两者都未顶到边界 ⇒ 完整。
			// *_len 含结尾 NUL，切掉它；dl==0 只可能出现在出参为 NULL 时（这里不会）。
			return &ParseResult{
				Dump:      trimNUL(dumpBuf, dl),
				Diag:      trimNUL(diagBuf, gl),
				NodeCount: int(nodeCount),
			}, nil
		}
		if dl >= dumpCap {
			dumpCap *= 2
		}
		if gl >= diagCap {
			diagCap *= 2
		}
	}
	return nil, fmt.Errorf("native: parser output exceeded %d grow attempts (dump=%d diag=%d)",
		maxAttempts, dumpCap, diagCap)
}

// ParseDump 只要 AST dump（省一次诊断序列化）。
func ParseDump(src []byte, filename string, isHeader bool) (string, error) {
	if len(src) == 0 {
		return "", errors.New("native: empty source")
	}
	csrc := (*C.uint8_t)(unsafe.Pointer(&src[0]))
	cfname := C.CString(filename)
	defer C.free(unsafe.Pointer(cfname))
	var ih C.int
	if isHeader {
		ih = 1
	}
	cap := len(src)*4 + 4096
	for attempt := 0; attempt < 8; attempt++ {
		buf := make([]byte, cap)
		var n C.size_t
		if rc := C.hm_parse_dump(csrc, C.size_t(len(src)), cfname, ih,
			(*C.char)(unsafe.Pointer(&buf[0])), C.size_t(cap), &n); rc != 0 {
			return "", fmt.Errorf("%w (rc=%d)", ErrParserOOM, int(rc))
		}
		if int(n) < cap {
			return trimNUL(buf, int(n)), nil
		}
		cap *= 2
	}
	return "", errors.New("native: parser dump exceeded grow attempts")
}

// ParseCount 只要节点数（golden 的"节点数 == K"断言用，比 dump 便宜得多）。
func ParseCount(src []byte, filename string, isHeader bool) (int, error) {
	if len(src) == 0 {
		return 0, errors.New("native: empty source")
	}
	csrc := (*C.uint8_t)(unsafe.Pointer(&src[0]))
	cfname := C.CString(filename)
	defer C.free(unsafe.Pointer(cfname))
	var ih C.int
	if isHeader {
		ih = 1
	}
	var count C.int
	if rc := C.hm_parse_count(csrc, C.size_t(len(src)), cfname, ih, &count); rc != 0 {
		return 0, fmt.Errorf("%w (rc=%d)", ErrParserOOM, int(rc))
	}
	return int(count), nil
}

// trimNUL 按 *_len（含结尾 NUL）切出内容，并防御性地剥掉可能的尾部 NUL。
func trimNUL(buf []byte, n int) string {
	if n <= 0 {
		return ""
	}
	if n > len(buf) {
		n = len(buf)
	}
	if n > 0 && buf[n-1] == 0 {
		n--
	}
	return string(buf[:n])
}

// ParseDiagRaw 用**指定**的缓冲大小调一次 hm_parse_diag，原样返回结果。
//
// 这是诊断探针：当 Parse 的扩容重试耗尽时，需要回答"到底要多大"。
// 正常路径请用 Parse —— 它会自己扩容。本函数的用处是：
//
//	· 测试里测量诊断规模，确认"诊断条数/体积"没有异常膨胀
//	· 排查"扩容 8 次仍不够"这类问题
//
// 返回的 n 是 C 侧回填的字节数（含 NUL），且当 n >= cap 时表示**可能被截断**
// （hermes_parser.h 契约第 3 条：C 侧不告诉我们真正需要多大）。
func ParseDiagRaw(src []byte, filename string, isHeader bool, cap int) ([]byte, int, error) {
	if len(src) == 0 {
		return nil, 0, errors.New("native: empty source")
	}
	if cap <= 0 {
		return nil, 0, errors.New("native: cap must be > 0")
	}
	csrc := (*C.uint8_t)(unsafe.Pointer(&src[0]))
	cfname := C.CString(filename)
	defer C.free(unsafe.Pointer(cfname))
	var ih C.int
	if isHeader {
		ih = 1
	}
	buf := make([]byte, cap)
	var n C.size_t
	if rc := C.hm_parse_diag(csrc, C.size_t(len(src)), cfname, ih,
		(*C.char)(unsafe.Pointer(&buf[0])), C.size_t(cap), &n); rc != 0 {
		return nil, 0, fmt.Errorf("%w (rc=%d)", ErrParserOOM, int(rc))
	}
	return buf, int(n), nil
}

// CodegenResult 是一次 codegen 的产出。
type CodegenResult struct {
	// JSON 是 workflow 对象（canonical JSON）。仅在 rc==0 时有值。
	JSON string
	// Diag 是诊断 JSON 数组。失败原因在这里（解析错误与 codegen 不支持同形）。
	Diag string
}

// Codegen 解析并把**第一个** workflow 编成 IR 的 workflow 对象。
//
// 返回 error 只表示"调用本身坏了"（OOM、扩容超限）；"这份源码编不出来"
// 走 rc，原因在 Diag 里 —— 与 Parse 的区别就在这里：Parse 的失败是把它自己
// 当作错误返回，而 codegen 的失败是**用户源码的问题**，必须能渲染成人话。
func Codegen(src []byte, filename string, isHeader bool) (*CodegenResult, error) {
	if len(src) == 0 {
		return nil, errors.New("native: empty source")
	}

	csrc := (*C.uint8_t)(unsafe.Pointer(&src[0]))
	cfname := C.CString(filename)
	defer C.free(unsafe.Pointer(cfname))

	var ih C.int
	if isHeader {
		ih = 1
	}

	jsonCap := len(src)*8 + 8192
	diagCap := 4096

	const maxAttempts = 8
	for attempt := 0; attempt < maxAttempts; attempt++ {
		jsonBuf := make([]byte, jsonCap)
		diagBuf := make([]byte, diagCap)

		var jsonLen, diagLen C.size_t
		rc := C.hm_codegen(csrc, C.size_t(len(src)), cfname, ih,
			(*C.char)(unsafe.Pointer(&jsonBuf[0])), C.size_t(jsonCap), &jsonLen,
			(*C.char)(unsafe.Pointer(&diagBuf[0])), C.size_t(diagCap), &diagLen)
		if rc != 0 && rc != 1 && rc != 2 {
			return nil, fmt.Errorf("%w (rc=%d)", ErrParserOOM, int(rc))
		}

		jl, gl := int(jsonLen), int(diagLen)
		if jl < jsonCap && gl < diagCap {
			return &CodegenResult{
				JSON: trimNUL(jsonBuf, jl),
				Diag: trimNUL(diagBuf, gl),
			}, nil
		}
		if jl >= jsonCap {
			jsonCap *= 2
		}
		if gl >= diagCap {
			diagCap *= 2
		}
	}
	return nil, fmt.Errorf("native: codegen output exceeded %d grow attempts (json=%d diag=%d)",
		maxAttempts, jsonCap, diagCap)
}

// CodegenTypes 解析一份模块并把顶层 struct 编成类型表（JSON 数组）。
// 类型引用只带名字，id 由装配阶段分配（见 codegen.h 的说明）。
func CodegenTypes(src []byte, filename string, isHeader bool) (string, string, error) {
	if len(src) == 0 {
		return "", "", errors.New("native: empty source")
	}
	csrc := (*C.uint8_t)(unsafe.Pointer(&src[0]))
	cfname := C.CString(filename)
	defer C.free(unsafe.Pointer(cfname))

	var ih C.int
	if isHeader {
		ih = 1
	}

	jsonCap := len(src)*4 + 4096
	diagCap := 4096
	const maxAttempts = 8
	for attempt := 0; attempt < maxAttempts; attempt++ {
		jsonBuf := make([]byte, jsonCap)
		diagBuf := make([]byte, diagCap)
		var jsonLen, diagLen C.size_t
		rc := C.hm_codegen_types(csrc, C.size_t(len(src)), cfname, ih,
			(*C.char)(unsafe.Pointer(&jsonBuf[0])), C.size_t(jsonCap), &jsonLen,
			(*C.char)(unsafe.Pointer(&diagBuf[0])), C.size_t(diagCap), &diagLen)
		if rc != 0 && rc != 1 && rc != 2 {
			return "", "", fmt.Errorf("%w (rc=%d)", ErrParserOOM, int(rc))
		}
		jl, gl := int(jsonLen), int(diagLen)
		if jl < jsonCap && gl < diagCap {
			return trimNUL(jsonBuf, jl), trimNUL(diagBuf, gl), nil
		}
		if jl >= jsonCap {
			jsonCap *= 2
		}
		if gl >= diagCap {
			diagCap *= 2
		}
	}
	return "", "", fmt.Errorf("native: codegen types output exceeded %d grow attempts", maxAttempts)
}

// CodegenWithExternals 同 Codegen，但额外接受一张"别的模块声明了哪些 struct"的表。
//
// 格式是最笨的 `Struct\tField\tType` 每行一条 —— 前端无 IO、无第三方库，
// 为一条声明表在 C++ 里手写 JSON 解析器得不偿失（见 D-027 推论）。
// 跨文件工程必须走这条口：单文件类型检查器看不到别的 .h。
func CodegenWithExternals(src []byte, filename string, isHeader bool, ext string) (*CodegenResult, error) {
	if len(src) == 0 {
		return nil, errors.New("native: empty source")
	}
	csrc := (*C.uint8_t)(unsafe.Pointer(&src[0]))
	cfname := C.CString(filename)
	defer C.free(unsafe.Pointer(cfname))

	cext := C.CString(ext)
	defer C.free(unsafe.Pointer(cext))

	var ih C.int
	if isHeader {
		ih = 1
	}

	jsonCap := len(src)*8 + 8192
	diagCap := 4096
	const maxAttempts = 8
	for attempt := 0; attempt < maxAttempts; attempt++ {
		jsonBuf := make([]byte, jsonCap)
		diagBuf := make([]byte, diagCap)
		var jsonLen, diagLen C.size_t
		rc := C.hm_codegen_ext(csrc, C.size_t(len(src)), cfname, ih,
			cext, C.size_t(len(ext)),
			(*C.char)(unsafe.Pointer(&jsonBuf[0])), C.size_t(jsonCap), &jsonLen,
			(*C.char)(unsafe.Pointer(&diagBuf[0])), C.size_t(diagCap), &diagLen)
		if rc != 0 && rc != 1 && rc != 2 {
			return nil, fmt.Errorf("%w (rc=%d)", ErrParserOOM, int(rc))
		}
		jl, gl := int(jsonLen), int(diagLen)
		if jl < jsonCap && gl < diagCap {
			return &CodegenResult{JSON: trimNUL(jsonBuf, jl), Diag: trimNUL(diagBuf, gl)}, nil
		}
		if jl >= jsonCap {
			jsonCap *= 2
		}
		if gl >= diagCap {
			diagCap *= 2
		}
	}
	return nil, fmt.Errorf("native: codegen output exceeded %d grow attempts", maxAttempts)
}

// CodegenWorkflowDecls 收集模块顶层 workflow 声明（名字 / exported / is_entry）。
func CodegenWorkflowDecls(src []byte, filename string, isHeader bool) (string, string, error) {
	if len(src) == 0 {
		return "", "", errors.New("native: empty source")
	}
	csrc := (*C.uint8_t)(unsafe.Pointer(&src[0]))
	cfname := C.CString(filename)
	defer C.free(unsafe.Pointer(cfname))

	var ih C.int
	if isHeader {
		ih = 1
	}
	jsonCap := len(src)*2 + 4096
	diagCap := 4096
	const maxAttempts = 8
	for attempt := 0; attempt < maxAttempts; attempt++ {
		jsonBuf := make([]byte, jsonCap)
		diagBuf := make([]byte, diagCap)
		var jsonLen, diagLen C.size_t
		rc := C.hm_codegen_wfdecls(csrc, C.size_t(len(src)), cfname, ih,
			(*C.char)(unsafe.Pointer(&jsonBuf[0])), C.size_t(jsonCap), &jsonLen,
			(*C.char)(unsafe.Pointer(&diagBuf[0])), C.size_t(diagCap), &diagLen)
		if rc != 0 && rc != 1 && rc != 2 {
			return "", "", fmt.Errorf("%w (rc=%d)", ErrParserOOM, int(rc))
		}
		jl, gl := int(jsonLen), int(diagLen)
		if jl < jsonCap && gl < diagCap {
			return trimNUL(jsonBuf, jl), trimNUL(diagBuf, gl), nil
		}
		if jl >= jsonCap {
			jsonCap *= 2
		}
		if gl >= diagCap {
			diagCap *= 2
		}
	}
	return "", "", fmt.Errorf("native: codegen wfdecls output exceeded %d grow attempts", maxAttempts)
}

// CodegenFunctions 收集模块顶层的函数定义（函数表，JSON 数组）。
//
// 与 CodegenWithExternals 分开：函数是 unit 级事实，跨 workflow 可见；
// 塞进某一份 workflow 会让同一份函数在多个 workflow 里各出现一次。
// ext 与 CodegenWithExternals 同义：函数签名里的 struct 可能来自别的 .h。
func CodegenFunctions(src []byte, filename string, isHeader bool, ext string) (string, string, error) {
	if len(src) == 0 {
		return "", "", errors.New("native: empty source")
	}
	csrc := (*C.uint8_t)(unsafe.Pointer(&src[0]))
	cfname := C.CString(filename)
	defer C.free(unsafe.Pointer(cfname))
	cext := C.CString(ext)
	defer C.free(unsafe.Pointer(cext))

	var ih C.int
	if isHeader {
		ih = 1
	}
	jsonCap := len(src)*4 + 4096
	diagCap := 4096
	const maxAttempts = 8
	for attempt := 0; attempt < maxAttempts; attempt++ {
		jsonBuf := make([]byte, jsonCap)
		diagBuf := make([]byte, diagCap)
		var jsonLen, diagLen C.size_t
		rc := C.hm_codegen_functions(csrc, C.size_t(len(src)), cfname, ih,
			cext, C.size_t(len(ext)),
			(*C.char)(unsafe.Pointer(&jsonBuf[0])), C.size_t(jsonCap), &jsonLen,
			(*C.char)(unsafe.Pointer(&diagBuf[0])), C.size_t(diagCap), &diagLen)
		if rc != 0 && rc != 1 && rc != 2 {
			return "", "", fmt.Errorf("%w (rc=%d)", ErrParserOOM, int(rc))
		}
		jl, gl := int(jsonLen), int(diagLen)
		if jl < jsonCap && gl < diagCap {
			return trimNUL(jsonBuf, jl), trimNUL(diagBuf, gl), nil
		}
		if jl >= jsonCap {
			jsonCap *= 2
		}
		if gl >= diagCap {
			diagCap *= 2
		}
	}
	return "", "", fmt.Errorf("native: codegen functions output exceeded %d grow attempts", maxAttempts)
}

// CodegenBackends 收集模块里的 backend 声明/定义。
func CodegenBackends(src []byte, filename string, isHeader bool) (string, string, error) {
	if len(src) == 0 {
		return "", "", errors.New("native: empty source")
	}
	csrc := (*C.uint8_t)(unsafe.Pointer(&src[0]))
	cfname := C.CString(filename)
	defer C.free(unsafe.Pointer(cfname))

	var ih C.int
	if isHeader {
		ih = 1
	}
	jsonCap := len(src)*2 + 4096
	diagCap := 4096
	const maxAttempts = 8
	for attempt := 0; attempt < maxAttempts; attempt++ {
		jsonBuf := make([]byte, jsonCap)
		diagBuf := make([]byte, diagCap)
		var jsonLen, diagLen C.size_t
		rc := C.hm_codegen_backends(csrc, C.size_t(len(src)), cfname, ih,
			(*C.char)(unsafe.Pointer(&jsonBuf[0])), C.size_t(jsonCap), &jsonLen,
			(*C.char)(unsafe.Pointer(&diagBuf[0])), C.size_t(diagCap), &diagLen)
		if rc != 0 && rc != 1 && rc != 2 {
			return "", "", fmt.Errorf("%w (rc=%d)", ErrParserOOM, int(rc))
		}
		jl, gl := int(jsonLen), int(diagLen)
		if jl < jsonCap && gl < diagCap {
			return trimNUL(jsonBuf, jl), trimNUL(diagBuf, gl), nil
		}
		if jl >= jsonCap {
			jsonCap *= 2
		}
		if gl >= diagCap {
			diagCap *= 2
		}
	}
	return "", "", fmt.Errorf("native: codegen backends output exceeded %d grow attempts", maxAttempts)
}

// CodegenTools 收集模块里的 `tool` 声明（D-034）。
//
// 与 CodegenBackends 同一套重试逻辑：输出的 JSON 可能比源码长（模板片段被展开），
// 所以缓冲区按需倍增而不是猜一个够大的值。

func CodegenTools(src []byte, filename string, isHeader bool) (string, string, error) {
	if len(src) == 0 {
		return "", "", errors.New("native: empty source")
	}
	csrc := (*C.uint8_t)(unsafe.Pointer(&src[0]))
	cfname := C.CString(filename)
	defer C.free(unsafe.Pointer(cfname))

	var ih C.int
	if isHeader {
		ih = 1
	}
	jsonCap := len(src)*2 + 4096
	diagCap := 4096
	const maxAttempts = 8
	for attempt := 0; attempt < maxAttempts; attempt++ {
		jsonBuf := make([]byte, jsonCap)
		diagBuf := make([]byte, diagCap)
		var jsonLen, diagLen C.size_t
		rc := C.hm_codegen_tools(csrc, C.size_t(len(src)), cfname, ih,
			(*C.char)(unsafe.Pointer(&jsonBuf[0])), C.size_t(jsonCap), &jsonLen,
			(*C.char)(unsafe.Pointer(&diagBuf[0])), C.size_t(diagCap), &diagLen)
		if rc != 0 && rc != 1 && rc != 2 {
			return "", "", fmt.Errorf("%w (rc=%d)", ErrParserOOM, int(rc))
		}
		jl, gl := int(jsonLen), int(diagLen)
		if jl < jsonCap && gl < diagCap {
			return trimNUL(jsonBuf, jl), trimNUL(diagBuf, gl), nil
		}
		if jl >= jsonCap {
			jsonCap *= 2
		}
		if gl >= diagCap {
			diagCap *= 2
		}
	}
	return "", "", fmt.Errorf("native: codegen backends output exceeded %d grow attempts", maxAttempts)
}
