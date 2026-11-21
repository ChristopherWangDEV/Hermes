// linkpolicy.go —— 许可红线闭集判据的 cgo 绑定（C++ 实现）。
package native

/*
#include <stdlib.h>
int hm_linkpolicy_match(const char* lib, char* out_name, size_t name_cap, char* out_licence, size_t lic_cap);
*/
import "C"

import (
	"unsafe"
)

// LinkPolicyMatch 返回命中的规则名与许可；未命中返回空串。
func LinkPolicyMatch(lib string) (name, licence string) {
	cLib := C.CString(lib)
	defer C.free(unsafe.Pointer(cLib))
	nameBuf := make([]byte, 128)
	licBuf := make([]byte, 128)
	rc := C.hm_linkpolicy_match(cLib,
		(*C.char)(unsafe.Pointer(&nameBuf[0])), C.size_t(len(nameBuf)),
		(*C.char)(unsafe.Pointer(&licBuf[0])), C.size_t(len(licBuf)))
	if int(rc) != 1 {
		return "", ""
	}
	return cString(nameBuf), cString(licBuf)
}

func cString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
