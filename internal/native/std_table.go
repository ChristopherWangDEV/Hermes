// std_table.go —— C++ 侧内置 std 模块表的 cgo 绑定（M1）。
//
// 表的**唯一事实源**仍是 internal/std/stdlib/*.h；C++ 侧那份由
// tools/genstdcpp 在构建时生成（build/std_table.cpp），不入库。
package native

/*
#include <stdlib.h>
#include <stddef.h>
int hm_std_count(void);
const char* hm_std_name(int i);
int hm_std_source(const char* name, char* out, size_t cap, size_t* out_len);
*/
import "C"

import (
	"errors"
	"unsafe"
)

// StdCount 返回 C++ 侧内置模块的个数。
func StdCount() int { return int(C.hm_std_count()) }

// StdName 返回第 i 个内置模块名（越界返回空串）。
func StdName(i int) string {
	if i < 0 || i >= StdCount() {
		return ""
	}
	return C.GoString(C.hm_std_name(C.int(i)))
}

// StdSource 返回某个内置模块的源码字节。
func StdSource(name string) ([]byte, error) {
	cName := C.CString(name)
	defer C.free(unsafe.Pointer(cName))
	cap := 4096
	for attempt := 0; attempt < 4; attempt++ {
		buf := make([]byte, cap)
		var outLen C.size_t
		rc := C.hm_std_source(cName, (*C.char)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)), &outLen)
		switch int(rc) {
		case 0:
			n := int(outLen) - 1 // 含结尾 NUL
			return buf[:n], nil
		case -4:
			if int(outLen) > cap {
				cap = int(outLen)
				continue
			}
			return nil, errors.New("std(native): buffer negotiation failed")
		default:
			return nil, errors.New("std(native): module not found")
		}
	}
	return nil, errors.New("std(native): output exceeded grow attempts")
}
