// archive_read.go —— `.hbc` 读取的 cgo 绑定（C++ 实现）。
package native

/*
#include <stdlib.h>
int hm_archive_read(const unsigned char* data, size_t len, char* out, size_t cap, size_t* out_len);
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

// ErrArchiveCorrupt 表示结构/哈希/JSON 有问题（与"不是归档"分开）。
var ErrArchiveCorrupt = errors.New("archive(native): corrupt archive")

// ArchiveReadMembers 校验归档并返回索引里的 members 数组（JSON）。
func ArchiveReadMembers(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return nil, ErrNotArchive
	}
	cap := 1 << 16
	for i := 0; i < 4; i++ {
		buf := make([]byte, cap)
		var n C.size_t
		rc := C.hm_archive_read((*C.uchar)(unsafe.Pointer(&raw[0])), C.size_t(len(raw)),
			(*C.char)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)), &n)
		switch int(rc) {
		case 0:
			return buf[:int(n)-1], nil
		case -4:
			if int(n) > cap {
				cap = int(n)
				continue
			}
			return nil, errors.New("archive(native): buffer negotiation failed")
		case 2:
			return nil, ErrNotArchive
		case 3:
			return nil, ErrArchiveVersion
		case 4:
			return nil, ErrArchiveCorrupt
		default:
			return nil, fmt.Errorf("archive(native): unexpected status %d", int(rc))
		}
	}
	return nil, errors.New("archive(native): grow attempts exhausted")
}
