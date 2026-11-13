// Command hermesnet 是 S2 的 Go c-archive：把 internal/netcall（复用
// internal/runtime 的 Tool/Model）导出成 C ABI，供编译产物调用。
//
// 构建：go build -buildmode=c-archive -o build/glibhermesnet.a ./cmd/hermesnet
// （会同时生成 build/glibhermesnet.h）。文件名必须以 g 开头：这是
// internal/toolchain 的 Go runtime 命名空间约定，裸机目标据此把任何含 Go
// runtime 的库挡在链接面之外。语义与 `hermes run` 共用同一份 Runtime
// 实现，不复制粘贴。
package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"encoding/json"
	"net/http"
	"unsafe"

	"hermes/internal/netcall"
	"hermes/internal/runtime"
)

// 单进程一个 Runtime：c-archive 的导出函数无状态，运行期配置（backend/环境）
// 在内部惰性读取。
var rt = &runtime.Runtime{HTTPClient: http.DefaultClient}

//export hermes_net_tool
func hermes_net_tool(req *C.char, reqLen C.size_t, out **C.char, errOut **C.char) C.int {
	var tr netcall.ToolReq
	if e := json.Unmarshal([]byte(C.GoStringN(req, C.int(reqLen))), &tr); e != nil {
		*errOut = C.CString("bad tool request: " + e.Error())
		return C.int(netcall.Contract)
	}
	b, msg, st := netcall.Tool(rt, tr)
	if st != netcall.OK {
		*errOut = C.CString(msg)
		return C.int(st)
	}
	*out = C.CString(string(b))
	return 0
}

//export hermes_net_model
func hermes_net_model(req *C.char, reqLen C.size_t, out **C.char, errOut **C.char) C.int {
	var mr netcall.ModelReq
	if e := json.Unmarshal([]byte(C.GoStringN(req, C.int(reqLen))), &mr); e != nil {
		*errOut = C.CString("bad model request: " + e.Error())
		return C.int(netcall.Contract)
	}
	b, msg, st := netcall.Model(rt, mr)
	if st != netcall.OK {
		*errOut = C.CString(msg)
		return C.int(st)
	}
	*out = C.CString(string(b))
	return 0
}

//export hermes_net_free
func hermes_net_free(p *C.char) {
	if p != nil {
		C.free(unsafe.Pointer(p))
	}
}

func main() {}
