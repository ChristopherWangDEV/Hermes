// compiled_static.go —— 编译型产物静态性闸门的 cgo 绑定（C++ 实现）。
package native

/*
#include <stdlib.h>
int hm_check_compiled_static(const char* ir_json, size_t len, char* out_msg, size_t cap,
                             size_t* out_len);
*/
import "C"

import (
	"errors"
	"unsafe"
)

// CheckCompiledStatic 校验产物里没有指向非递归函数的 call_fn。
func CheckCompiledStatic(irJSON []byte) error {
	if len(irJSON) == 0 {
		return errors.New("compiledstatic(native): empty IR")
	}
	buf := make([]byte, 1024)
	var n C.size_t
	rc := int(C.hm_check_compiled_static((*C.char)(unsafe.Pointer(&irJSON[0])), C.size_t(len(irJSON)),
		(*C.char)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)), &n))
	switch rc {
	case 0:
		return nil
	case 1:
		return errors.New(string(buf[:int(n)-1]))
	default:
		return errors.New("compiledstatic(native): malformed IR")
	}
}
