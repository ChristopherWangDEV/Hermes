// objfile.go —— 目标文件动态依赖的 Rust 实现绑定（internal/objfile 的替代者）。
//
// 为什么这件事在 Rust：它解析**不可信二进制**（可能被截断/伪造）。越界读是
// 这类代码的典型故障，而 Rust 侧的单一路径只做边界检查后的切片访问。
package native

/*
#include <stdlib.h>
int hm_objfile_dynamic_libs(const char* path, unsigned char* out, size_t cap, size_t* out_len);
*/
import "C"

import (
	"errors"
	"strings"
	"unsafe"
)

// ErrObjUnsupportedFormat 表示"这个文件不是 ELF/Mach-O"（裸机产物等）。
// 与"读不出来"必须分开：前者可以放行，后者必须 fail-closed。
var ErrObjUnsupportedFormat = errors.New("objfile: unsupported object format")

// ErrObjMalformed 表示是 ELF/Mach-O，但结构坏了（截断、越界偏移）。
var ErrObjMalformed = errors.New("objfile: malformed object file")

// ObjfileDynamicLibs 返回动态依赖名（ELF 的 DT_NEEDED / Mach-O 的 LC_LOAD_DYLIB）。
// 空切片 = 静态链接（不是错误）。
func ObjfileDynamicLibs(path string) ([]string, error) {
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))

	cap := 4096
	for attempt := 0; attempt < 8; attempt++ {
		buf := make([]byte, cap)
		var outLen C.size_t
		rc := C.hm_objfile_dynamic_libs(
			cPath, (*C.uchar)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)), &outLen)
		switch int(rc) {
		case 0:
			n := int(outLen)
			if n > 0 && buf[n-1] == 0 {
				n--
			}
			if n == 0 {
				return nil, nil
			}
			return strings.Split(string(buf[:n]), "\n"), nil
		case -4: // HM_E_TOO_LARGE：扩容重试
			if int(outLen) > cap {
				cap = int(outLen)
				continue
			}
			return nil, errors.New("objfile: buffer negotiation failed")
		case -20:
			return nil, ErrObjUnsupportedFormat
		case -21:
			return nil, ErrObjMalformed
		case -6:
			return nil, errors.New("objfile: cannot read file")
		default:
			return nil, errors.New("objfile: unexpected status")
		}
	}
	return nil, errors.New("objfile: output exceeded grow attempts")
}
