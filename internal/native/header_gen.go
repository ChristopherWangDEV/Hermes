// header_gen.go —— C 头生成器的 cgo 绑定（C++ 实现）。
package native

/*
#include <stdlib.h>
int hm_header_gen_c(const char* ir_json, size_t len, char* out, size_t cap, size_t* out_len);
int hm_header_gen_shim(const char* ir_json, size_t len, const unsigned char* ir_bytes, size_t ir_len, char* out, size_t cap, size_t* out_len);
int hm_header_core_impl(char* out, size_t cap, size_t* out_len);
int hm_header_gen_cpp(const char* ir_json, size_t len, char* out, size_t cap, size_t* out_len);
*/
import "C"

import (
	"errors"
	"unsafe"
)

// HeaderGenC 从 IR JSON 生成 C 头（与 header.GenC 逐字节一致）。
func HeaderGenC(irJSON []byte) ([]byte, error) {
	if len(irJSON) == 0 {
		return nil, errors.New("header(native): empty IR")
	}
	cIR := C.CString(string(irJSON))
	defer C.free(unsafe.Pointer(cIR))
	cap := len(irJSON) + 8192
	for i := 0; i < 4; i++ {
		buf := make([]byte, cap)
		var n C.size_t
		rc := C.hm_header_gen_c(cIR, C.size_t(len(irJSON)),
			(*C.char)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)), &n)
		switch int(rc) {
		case 0:
			return buf[:int(n)-1], nil
		case -4:
			if int(n) > cap {
				cap = int(n)
				continue
			}
			return nil, errors.New("header(native): buffer negotiation failed")
		default:
			return nil, errors.New("header(native): malformed IR")
		}
	}
	return nil, errors.New("header(native): grow attempts exhausted")
}

// HeaderGenShim 生成 unit 的 C 实现层（per-unit 符号 → hermes_rt_run）。
func HeaderGenShim(irJSON, irBytes []byte) ([]byte, error) {
	if len(irJSON) == 0 {
		return nil, errors.New("header(native): empty IR")
	}
	cIR := C.CString(string(irJSON))
	defer C.free(unsafe.Pointer(cIR))
	var irPtr *C.uchar
	if len(irBytes) > 0 {
		irPtr = (*C.uchar)(unsafe.Pointer(&irBytes[0]))
	}
	cap := len(irJSON) + len(irBytes)*6 + 8192
	for i := 0; i < 4; i++ {
		buf := make([]byte, cap)
		var n C.size_t
		rc := C.hm_header_gen_shim(cIR, C.size_t(len(irJSON)), irPtr, C.size_t(len(irBytes)),
			(*C.char)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)), &n)
		switch int(rc) {
		case 0:
			return buf[:int(n)-1], nil
		case -4:
			if int(n) > cap {
				cap = int(n)
				continue
			}
			return nil, errors.New("header(native): buffer negotiation failed")
		default:
			return nil, errors.New("header(native): malformed IR")
		}
	}
	return nil, errors.New("header(native): grow attempts exhausted")
}

// HeaderCoreImpl 返回运行时核心实现正文（hermes_core_impl.c）。
func HeaderCoreImpl() ([]byte, error) {
	cap := 64 << 10
	for i := 0; i < 4; i++ {
		buf := make([]byte, cap)
		var n C.size_t
		rc := C.hm_header_core_impl((*C.char)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)), &n)
		switch int(rc) {
		case 0:
			return buf[:int(n)-1], nil
		case -4:
			if int(n) > cap {
				cap = int(n)
				continue
			}
			return nil, errors.New("header(native): buffer negotiation failed")
		default:
			return nil, errors.New("header(native): invalid args")
		}
	}
	return nil, errors.New("header(native): grow attempts exhausted")
}

// HeaderGenCPP 生成 `<unit>.hpp`（C++ 入口头，便利层默认关闭）。
func HeaderGenCPP(irJSON []byte) ([]byte, error) {
	if len(irJSON) == 0 {
		return nil, errors.New("header(native): empty IR")
	}
	cIR := C.CString(string(irJSON))
	defer C.free(unsafe.Pointer(cIR))
	cap := len(irJSON) + 8192
	for i := 0; i < 4; i++ {
		buf := make([]byte, cap)
		var n C.size_t
		rc := C.hm_header_gen_cpp(cIR, C.size_t(len(irJSON)),
			(*C.char)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)), &n)
		switch int(rc) {
		case 0:
			return buf[:int(n)-1], nil
		case -4:
			if int(n) > cap {
				cap = int(n)
				continue
			}
			return nil, errors.New("header(native): buffer negotiation failed")
		default:
			return nil, errors.New("header(native): malformed IR")
		}
	}
	return nil, errors.New("header(native): grow attempts exhausted")
}
