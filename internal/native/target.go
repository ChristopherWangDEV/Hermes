// target.go —— --target 分类的 cgo 绑定（C++ 实现）。
package native

/*
#include <stdlib.h>
int hm_target_parse(const char* raw, const char* goos, const char* goarch, char* out_json,
                    size_t cap, size_t* out_len, char* out_err, size_t err_cap, size_t* err_len);
*/
import "C"

import (
	"encoding/json"
	"errors"
	"unsafe"
)

// TargetInfo 是 hm_target_parse 的解析结果（字段与 Go 的 toolchain.Target 对齐）。
type TargetInfo struct {
	Raw      string `json:"raw"`
	Triple   string `json:"triple"`
	Arch     string `json:"arch"`
	Vendor   string `json:"vendor"`
	OS       string `json:"os"`
	Env      string `json:"env"`
	GOOS     string `json:"goos"`
	Class    string `json:"class"`
	Native   bool   `json:"native"`
	Explicit bool   `json:"explicit"`
}

// ParseTarget 调 C++ 侧的 --target 分类器。
func ParseTarget(raw, goos, goarch string) (TargetInfo, error) {
	cRaw := C.CString(raw)
	cGoos := C.CString(goos)
	cGoarch := C.CString(goarch)
	defer C.free(unsafe.Pointer(cRaw))
	defer C.free(unsafe.Pointer(cGoos))
	defer C.free(unsafe.Pointer(cGoarch))

	buf := make([]byte, 4096)
	errBuf := make([]byte, 1024)
	var n, en C.size_t
	rc := C.hm_target_parse(cRaw, cGoos, cGoarch,
		(*C.char)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)), &n,
		(*C.char)(unsafe.Pointer(&errBuf[0])), C.size_t(len(errBuf)), &en)
	switch int(rc) {
	case 0:
		var t TargetInfo
		if err := json.Unmarshal(buf[:int(n)-1], &t); err != nil {
			return TargetInfo{}, err
		}
		return t, nil
	case 1:
		msg := errBuf
		if int(en) > 0 && int(en) <= len(errBuf) {
			msg = errBuf[:int(en)-1]
		}
		return TargetInfo{}, errors.New(string(msg))
	default:
		return TargetInfo{}, errors.New("target(native): invalid argument")
	}
}
