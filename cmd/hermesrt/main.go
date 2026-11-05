// Command hermesrt 把运行时导出成 **C 可链接的静态库**（`-buildmode=c-archive`）。
//
// 这是 AOT 的另一半：`hermes compile` 生成的 C 程序内嵌 IR，链接这个库，
// 就得到一个**不需要安装 hermes** 的可执行文件 —— README 里"单文件二进制"的字面意思。
//
// 导出的只有一个函数：给它 IR 字节和输入 JSON，它跑完返回输出或错误。
// 刻意**不**导出更多：每多一个 C 函数，就多一处需要永远保持兼容的 ABI 面，
// 而第三方消费者不会与我们同步升级（D-024：对外走 major 相等 + abi_size）。
package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"encoding/json"
	"os"
	"unsafe"

	"hermes/internal/archive"
	"hermes/internal/engine"
	"hermes/internal/ir"
	"hermes/internal/runtime"
)

// hermes_rt_run 执行一份内嵌的 IR。
//
// 返回 0 表示成功，此时 *out 指向 malloc 出来的 NUL 结尾字符串（调用方 free）；
// 非 0 表示失败，*err 指向错误文本（同样由调用方 free）。
//
// 为什么结果用 malloc 出来的 C 字符串而不是 hermes_buf：这是**运行时内部**的
// 一次性入口，不是对外契约面；AOT 产物由生成器生成，它知道怎么 free。
// 真正对外的 C ABI（hermes_<unit>_*）走 hermes_core.h 的 hermes_buf。
//
//export hermes_rt_run
func hermes_rt_run(irData *C.char, irLen C.size_t, inputData *C.char, inputLen C.size_t,
	outPtr **C.char, errPtr **C.char) C.int {

	irBytes := C.GoBytes(unsafe.Pointer(irData), C.int(irLen))
	doc, ds := ir.Load(irBytes)
	if len(ds) != 0 {
		*errPtr = C.CString("IR 不合格（" + itoa(len(ds)) + " 条诊断）")
		return 2
	}

	var input any
	if inputData != nil && inputLen > 0 {
		raw := C.GoBytes(unsafe.Pointer(inputData), C.int(inputLen))
		dec := json.NewDecoder(bytesReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&input); err != nil {
			*errPtr = C.CString("input 不是合法 JSON: " + err.Error())
			return 2
		}
	}

	var rt engine.Runtime
	if len(doc.Backends) > 0 {
		byName := make(map[string]ir.Backend, len(doc.Backends))
		for _, b := range doc.Backends {
			byName[b.Name] = b
		}
		rt = &runtime.Runtime{Backends: byName}
	}

	res, err := engine.RunWith(doc, "", input, engine.RunOptions{Runtime: rt})
	if err != nil {
		*errPtr = C.CString(err.Error())
		return 3
	}

	if s, ok := res.Value.(string); ok {
		*outPtr = C.CString(s)
		return 0
	}
	b, mErr := json.Marshal(res.Value)
	if mErr != nil {
		*errPtr = C.CString("结果无法序列化: " + mErr.Error())
		return 3
	}
	*outPtr = C.CString(string(b))
	return 0
}

func main() {}

//export hermes_rt_free
func hermes_rt_free(p *C.char) {
	// 让 Python 侧（ctypes）能正确归还内存。不导出这个，调用方只能猜
	// 该用 libc 的 free 还是别的分配器 —— 猜错就是堆损坏，而症状离原因很远。
	C.free(unsafe.Pointer(p))
}

// hermes_rt_run_file 直接吃一个 `.hbc` 归档或 IR 文件。
//
// 为什么不让 Python 侧自己读 IR：那要它实现 `.hbc` 的解析（头、索引、成员）。
// 绑定层的价值就是"把这类事留在原生侧"——PyQt 也是这么做的。
//
//export hermes_rt_run_file
func hermes_rt_run_file(unitPath *C.char, inputData *C.char, inputLen C.size_t,
	outPtr **C.char, errPtr **C.char) C.int {

	path := C.GoString(unitPath)
	raw, err := os.ReadFile(path)
	if err != nil {
		*errPtr = C.CString("读不到 " + path + ": " + err.Error())
		return 2
	}
	irBytes := raw
	if len(raw) >= 4 && string(raw[0:4]) == archive.Magic {
		a, aerr := archive.Read(raw)
		if aerr != nil {
			*errPtr = C.CString(path + " 不是可用归档: " + aerr.Error())
			return 2
		}
		if len(a.Members) == 0 {
			*errPtr = C.CString(path + " 里没有成员")
			return 2
		}
		irBytes = a.Members[0].IR
	}
	return hermes_rt_run(
		(*C.char)(unsafe.Pointer(&irBytes[0])), C.size_t(len(irBytes)),
		inputData, inputLen, outPtr, errPtr)
}
