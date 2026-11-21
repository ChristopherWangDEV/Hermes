// ir_net.go —— UsesNet 判定的 cgo 绑定（C++ 实现）。
package native

/*
#include <stdlib.h>
int hm_ir_uses_net(const char* ir_json, size_t len);
*/
import "C"

import (
	"errors"
	"unsafe"
)

// IRUsesNet 判定这份 IR 是否需要网络（call_tool / call_model 出现即为是）。
func IRUsesNet(irJSON []byte) (bool, error) {
	if len(irJSON) == 0 {
		return false, errors.New("ir(native): empty IR")
	}
	rc := int(C.hm_ir_uses_net((*C.char)(unsafe.Pointer(&irJSON[0])), C.size_t(len(irJSON))))
	switch rc {
	case 1:
		return true, nil
	case 0:
		return false, nil
	default:
		return false, errors.New("ir(native): malformed IR")
	}
}
