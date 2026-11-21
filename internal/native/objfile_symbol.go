// objfile_symbol.go —— 符号三态的 cgo 绑定（Rust 实现）。
package native

/*
#include <stdlib.h>
int hm_objfile_symbol_state(const char* path, const char* name);
*/
import "C"

import (
	"errors"
	"unsafe"
)

// SymbolState 返回符号状态：0 缺失 / 1 已定义 / 2 未定义（被引用未定义）。
func SymbolState(path, name string) (int, error) {
	cPath := C.CString(path)
	cName := C.CString(name)
	defer C.free(unsafe.Pointer(cPath))
	defer C.free(unsafe.Pointer(cName))
	rc := int(C.hm_objfile_symbol_state(cPath, cName))
	switch rc {
	case 0, 1, 2:
		return rc, nil
	case -20:
		return 0, ErrObjUnsupportedFormat
	case -21:
		return 0, ErrObjMalformed
	case -6:
		return 0, errors.New("objfile(native): cannot read file")
	default:
		return 0, errors.New("objfile(native): invalid argument")
	}
}
