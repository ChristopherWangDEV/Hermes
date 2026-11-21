// layout.go —— 资源定位的 cgo 绑定（C++ 实现，顺序与 internal/layout 一致）。
package native

/*
#include <stdlib.h>
int hm_layout_header_for(const char* lib_path, char* out, size_t cap);
int hm_layout_resolve(const char* exec_path, const char* cwd, char* out_root, size_t root_cap,
                      char* out_lib, size_t lib_cap, char* out_include, size_t inc_cap,
                      char* out_shim, size_t shim_cap);
*/
import "C"

import (
	"unsafe"
)

// LayoutAssets 是一次解析的结果（空串表示没找到）。
type LayoutAssets struct {
	Root       string
	RuntimeLib string
	IncludeDir string
	Shim       string
}

// LayoutResolve 按固定顺序找资源：HERMES_HOME → 可执行文件上一级 → 当前目录。
func LayoutResolve(execPath, cwd string) LayoutAssets {
	cExec := C.CString(execPath)
	cCwd := C.CString(cwd)
	defer C.free(unsafe.Pointer(cExec))
	defer C.free(unsafe.Pointer(cCwd))

	root := make([]byte, 4096)
	lib := make([]byte, 4096)
	inc := make([]byte, 4096)
	shim := make([]byte, 4096)
	C.hm_layout_resolve(cExec, cCwd,
		(*C.char)(unsafe.Pointer(&root[0])), C.size_t(len(root)),
		(*C.char)(unsafe.Pointer(&lib[0])), C.size_t(len(lib)),
		(*C.char)(unsafe.Pointer(&inc[0])), C.size_t(len(inc)),
		(*C.char)(unsafe.Pointer(&shim[0])), C.size_t(len(shim)))
	return LayoutAssets{
		Root:       cString(root),
		RuntimeLib: cString(lib),
		IncludeDir: cString(inc),
		Shim:       cString(shim),
	}
}

// LayoutHeaderFor 推出运行时库的 C 头路径（未命中返回空串）。
func LayoutHeaderFor(libPath string) string {
	cLib := C.CString(libPath)
	defer C.free(unsafe.Pointer(cLib))
	buf := make([]byte, 4096)
	if int(C.hm_layout_header_for(cLib, (*C.char)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)))) != 1 {
		return ""
	}
	return cString(buf)
}
