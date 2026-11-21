// archive_index.go —— `.hbc` 索引哈希的 C++ 绑定（M2）。
package native

/*
#include <stdlib.h>
int hm_archive_index_sha256(const unsigned char* data, size_t len, char* out_hex);
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

// ErrNotArchive / ErrArchiveVersion / ErrArchiveMalformed 让调用方能区分
// "这不是归档"（可以跳过）与"归档坏了"（必须 fail-closed）。
var (
	ErrNotArchive       = errors.New("archive(native): not a .hbc file")
	ErrArchiveVersion   = errors.New("archive(native): unsupported .hbc version")
	ErrArchiveMalformed = errors.New("archive(native): malformed .hbc index")
)

// ArchiveIndexSHA256 返回 `.hbc` **索引区**的 sha256（64 位十六进制）。
func ArchiveIndexSHA256(raw []byte) (string, error) {
	if len(raw) == 0 {
		return "", ErrNotArchive
	}
	out := make([]byte, 65)
	rc := C.hm_archive_index_sha256(
		(*C.uchar)(unsafe.Pointer(&raw[0])), C.size_t(len(raw)),
		(*C.char)(unsafe.Pointer(&out[0])))
	switch int(rc) {
	case 0:
		return string(out[:64]), nil
	case 2:
		return "", ErrNotArchive
	case 3:
		return "", ErrArchiveVersion
	case 4:
		return "", ErrArchiveMalformed
	case 5:
		return "", errors.New("archive(native): hash failed")
	default:
		return "", fmt.Errorf("archive(native): unexpected status %d", int(rc))
	}
}
