package native

/*
#include <stdlib.h>
#include "hermes_parser.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

// ErrBackendNotCovered 表示 C++ 的 C 后端尚未覆盖这份 IR（迁移中）。
//
// 这是可判别的**未覆盖状态**，不是致命失败：调用方据此把未覆盖原因带进错误，
// 并由等价测试识别（见 ir_equiv_test.go）。Go 版 C 后端已删除，没有回落实现。
// 哨兵保留是为了让调用方不必解析错误文本。
var ErrBackendNotCovered = errors.New("backend(native): this IR is not covered by the C++ backend yet")

// BackendEmitC 调 C++ 的 IR → C 生成器。
//
// 返回码约定见 hermes_parser.h 的 hm_backend_c_emit：
//
//	0  成功
//	1  未覆盖 → ErrBackendNotCovered（调用方带原因报错，不再回落 Go）
//	2  致命错误 → 返回带原因的错误
//
// 缓冲区用与 parse.go 相同的"先试后扩"策略：缓冲不足时不截断，
// *_len 回填所需大小，于是照它再分配一次就能成功。
func BackendEmitC(irJSON []byte) ([]byte, error) {
	if len(irJSON) == 0 {
		return nil, errors.New("backend(native): empty IR")
	}
	out := make([]byte, 64*1024)
	errBuf := make([]byte, 4096)
	var outLen, errLen C.size_t
	var inPtr *C.char
	if len(irJSON) > 0 {
		inPtr = (*C.char)(unsafe.Pointer(&irJSON[0]))
	}
	for attempt := 0; ; attempt++ {
		rc := C.hm_backend_c_emit(
			inPtr, C.size_t(len(irJSON)),
			(*C.char)(unsafe.Pointer(&out[0])), C.size_t(len(out)), &outLen,
			(*C.char)(unsafe.Pointer(&errBuf[0])), C.size_t(len(errBuf)), &errLen,
		)
		switch rc {
		case 0:
			return out[:int(outLen)], nil
		case 1:
			// 带上原因：迁移期最常见的问题是"到底哪个节点没覆盖"，
			// 只给一个哨兵会让排查回到翻代码。errors.Is 仍然成立。
			return nil, fmt.Errorf("%w: %s", ErrBackendNotCovered, trimNUL(errBuf, int(errLen)))
		default:
			// 缓冲不足：按回填的大小扩容重试（最多两次，避免死循环）。
			if attempt >= 2 {
				return nil, errors.New("backend(native): " + trimNUL(errBuf, int(errLen)))
			}
			if int(outLen) > len(out) {
				out = make([]byte, int(outLen))
				continue
			}
			if int(errLen) > len(errBuf) {
				errBuf = make([]byte, int(errLen))
				continue
			}
			return nil, errors.New("backend(native): " + trimNUL(errBuf, int(errLen)))
		}
	}
}

// IsIRNotCovered 判断"这份源码还没被 C++ 装配覆盖"（对应 hm_ir_assemble_final 的 1）。
func IsIRNotCovered(err error) bool { return errors.Is(err, ErrIRNotCovered) }

// ErrIRNotCovered 是 C++ 装配尚未覆盖的哨兵。
var ErrIRNotCovered = errors.New("ir(native): this source is not covered by the C++ assembler yet")

// IRAssembleFinal 调 C++ 的"源码 → 最终规范 IR JSON"。
//
// 返回的字节必须与 `hermes ir` 的输出**逐字节相同** —— 这是 internal/ir
// C++ 化的唯一判据（见 cmd/hermes/ir_equiv_test.go 的 41 份基线）。
// 未覆盖时返回 ErrIRNotCovered，调用方据此把未覆盖原因带进错误；装配已无回落。
// 哨兵仍保留：ir_assemble.cpp 可能返回未覆盖，且 ir_equiv_test.go:82 在用。
func IRAssembleFinal(src []byte, filename string, factsJSON []byte) ([]byte, error) {
	if len(src) == 0 {
		return nil, errors.New("ir(native): empty source")
	}
	out := make([]byte, 64*1024)
	errBuf := make([]byte, 4096)
	var outLen, errLen C.size_t
	cName := C.CString(filename)
	defer C.free(unsafe.Pointer(cName))
	var factsPtr *C.char
	if len(factsJSON) > 0 {
		factsPtr = (*C.char)(unsafe.Pointer(&factsJSON[0]))
	}
	for attempt := 0; ; attempt++ {
		rc := C.hm_ir_assemble_final(
			(*C.uint8_t)(unsafe.Pointer(&src[0])), C.size_t(len(src)), cName,
			factsPtr, C.size_t(len(factsJSON)),
			(*C.char)(unsafe.Pointer(&out[0])), C.size_t(len(out)), &outLen,
			(*C.char)(unsafe.Pointer(&errBuf[0])), C.size_t(len(errBuf)), &errLen,
		)
		switch rc {
		case 0:
			return out[:int(outLen)], nil
		case 1:
			return nil, fmt.Errorf("%w: %s", ErrIRNotCovered, trimNUL(errBuf, int(errLen)))
		default:
			if attempt >= 2 {
				return nil, errors.New("ir(native): " + trimNUL(errBuf, int(errLen)))
			}
			if int(outLen) > len(out) {
				out = make([]byte, int(outLen))
				continue
			}
			if int(errLen) > len(errBuf) {
				errBuf = make([]byte, int(errLen))
				continue
			}
			return nil, errors.New("ir(native): " + trimNUL(errBuf, int(errLen)))
		}
	}
}

// IRValidate 调 C++ 的"已装配 IR → 诊断"。
//
// 返回的字节是诊断 JSON 数组（扁平形状，见 cmd/hermes/ir_build.go 的
// nativeDiag）。校验没有"未覆盖"概念（任何已装配 IR 都能被校验），所以只有
// rc=0（成功，诊断在 out）与 rc=2（致命错误，原因在 out_err）。调用方据此把
// C++ 诊断作为唯一结果；桥接失败即基础设施故障，如实报错，不再回落 Go。
func IRValidate(irJSON []byte) ([]byte, error) {
	if len(irJSON) == 0 {
		return nil, errors.New("ir(native): empty IR")
	}
	out := make([]byte, 64*1024)
	errBuf := make([]byte, 4096)
	var outLen, errLen C.size_t
	for attempt := 0; ; attempt++ {
		rc := C.hm_ir_validate(
			(*C.uint8_t)(unsafe.Pointer(&irJSON[0])), C.size_t(len(irJSON)),
			(*C.char)(unsafe.Pointer(&out[0])), C.size_t(len(out)), &outLen,
			(*C.char)(unsafe.Pointer(&errBuf[0])), C.size_t(len(errBuf)), &errLen,
		)
		switch rc {
		case 0:
			return out[:int(outLen)], nil
		default:
			if attempt >= 2 {
				return nil, errors.New("ir(native): " + trimNUL(errBuf, int(errLen)))
			}
			if int(outLen) > len(out) {
				out = make([]byte, int(outLen))
				continue
			}
			if int(errLen) > len(errBuf) {
				errBuf = make([]byte, int(errLen))
				continue
			}
			return nil, errors.New("ir(native): " + trimNUL(errBuf, int(errLen)))
		}
	}
}

// BackendUnitEmit 调 C++ 的 unit AOT 生成（`hermes cc` 的 .hbc 落地）。
// 返回码同 BackendEmitC：0 成功 / 1 未覆盖 / 2 致命。
func BackendUnitEmit(irJSON []byte, unit, fn string) ([]byte, error) {
	if len(irJSON) == 0 {
		return nil, errors.New("unit(native): empty IR")
	}
	out := make([]byte, 64*1024)
	errBuf := make([]byte, 4096)
	var outLen, errLen C.size_t
	cUnit := C.CString(unit)
	defer C.free(unsafe.Pointer(cUnit))
	cFn := C.CString(fn)
	defer C.free(unsafe.Pointer(cFn))
	for attempt := 0; attempt < 3; attempt++ {
		rc := C.hm_backend_unit_emit(
			(*C.char)(unsafe.Pointer(&irJSON[0])), C.size_t(len(irJSON)), cUnit, cFn,
			(*C.char)(unsafe.Pointer(&out[0])), C.size_t(len(out)), &outLen,
			(*C.char)(unsafe.Pointer(&errBuf[0])), C.size_t(len(errBuf)), &errLen,
		)
		switch rc {
		case 0:
			return out[:int(outLen)], nil
		case 1:
			return nil, fmt.Errorf("%w: %s", ErrBackendNotCovered, trimNUL(errBuf, int(errLen)))
		default:
			if int(outLen) > len(out) {
				out = make([]byte, int(outLen))
				continue
			}
			if int(errLen) > len(errBuf) {
				errBuf = make([]byte, int(errLen))
				continue
			}
			return nil, errors.New("unit(native): " + trimNUL(errBuf, int(errLen)))
		}
	}
	return nil, errors.New("unit(native): buffer negotiation failed")
}
