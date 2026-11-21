// archive_write.go —— `.hbc` 写入的 cgo 绑定（C++ 实现）。
package native

/*
#include <stdlib.h>
int hm_archive_write(const char* members_json, size_t len, unsigned char* out, size_t cap, size_t* out_len);
*/
import "C"

import (
	"errors"
	"unsafe"
)

// ArchiveWrite 用成员描述 JSON 生成 `.hbc` 字节（C++ 实现）。
func ArchiveWrite(membersJSON []byte) ([]byte, error) {
	if len(membersJSON) == 0 {
		return nil, errors.New("archive(native): empty members")
	}
	cBlob := C.CString(string(membersJSON))
	defer C.free(unsafe.Pointer(cBlob))
	cap := len(membersJSON)*4 + 4096
	for i := 0; i < 4; i++ {
		buf := make([]byte, cap)
		var n C.size_t
		rc := C.hm_archive_write(cBlob, C.size_t(len(membersJSON)),
			(*C.uchar)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)), &n)
		switch int(rc) {
		case 0:
			return buf[:int(n)], nil
		case -2:
			return nil, errors.New("archive(native): empty archive")
		case -3:
			return nil, errors.New("archive(native): hash failed")
		case -4:
			if int(n) > cap {
				cap = int(n)
				continue
			}
			return nil, errors.New("archive(native): buffer negotiation failed")
		default:
			return nil, errors.New("archive(native): malformed members JSON")
		}
	}
	return nil, errors.New("archive(native): grow attempts exhausted")
}
