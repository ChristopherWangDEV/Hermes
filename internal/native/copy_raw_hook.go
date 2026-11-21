package native

/*
#include <stdlib.h>
#include "hermes_abi.h"
*/
import "C"

import "unsafe"

// 这个文件存在的理由：给"Go 封装**必然会挡住**的 C 分支"留一个测试直通口。
//
// Go 不允许在 _test.go 里 import "C"（"use of cgo in test not supported"），
// 所以任何"被封装层的 if 短路掉的生产分支"都没法在 Go 测试里触及。目前两处：
//
//  1. `hm_proc_copy_*` 的 `cap < need` 分支：copyStream / copyDeadlineSource
//     刻意**永远**先问长度再精确分配。QA 的注入实验证实了后果 —— 把它改成
//     "截断写 + 返回 HM_OK"（违反 R3），一条测试都不红。
//  2. `hm_proc_free` 的幂等 / `HM_HANDLE_INVALID` 分支：Go 的 `Proc.Free()`
//     在 `done` 之后直接返回 nil，永远走不到 C。要断言"清理可以重复、
//     读取必须报错"这条**C 契约**，必须绕过 `done`。
//
// 这些函数都是**一字不改的透传**：不查参数、不分配、不给默认值，调用方给的
// 原样交给 C，返回值原样转成 hm_status。出参用 unsafe.Pointer / *uint64 而
// 不是 C 类型，是因为调用方是 _test.go，那里不能出现 C 类型；指针只是转手，
// 解引用仍在 C 侧发生。
//
// 为什么不导出、也不进任何文档的符号面：导出就把"可以拿原始指针调"变成对外
// 承诺，而契约是"用封装、按错误码分支取用"。**本文件的全部函数只为测试存在，
// 不属于导出面、不属于生产路径** —— 生产代码不调用它们中的任何一个。

func rawCopyStdout(p *Proc, buf unsafe.Pointer, cap uint64, outLen *uint64) Status {
	return Status(C.hm_proc_copy_stdout(
		C.hm_handle(p.h),
		(*C.uint8_t)(buf),
		C.size_t(cap),
		(*C.size_t)(unsafe.Pointer(outLen)),
	))
}

func rawCopyDeadlineSource(p *Proc, buf unsafe.Pointer, cap uint64, outLen *uint64) Status {
	return Status(C.hm_proc_copy_deadline_source(
		C.hm_handle(p.h),
		(*C.char)(buf),
		C.size_t(cap),
		(*C.size_t)(unsafe.Pointer(outLen)),
	))
}

// rawProcFree 直接调到 C 的 hm_proc_free，**绕过 Proc.done 短路**。
//
// 为什么需要它：Go 的 Free() 在 done 之后直接返回 nil，所以"对已释放句柄
// 再 free 是 HM_OK"这条**C 契约**永远走不到 —— 不绕过就只能测到 Go 的 if，
// 测不到 C 的 if。契约要两侧都对（hermes_abi.h 的 hm_proc_free）。
func rawProcFree(p *Proc) Status {
	return Status(C.hm_proc_free(C.hm_handle(p.h)))
}

// rawProcFreeHandle / rawGetStateHandle / rawProcWaitHandle 收**裸句柄**而不是
// *Proc：要验的是 `HM_HANDLE_INVALID`（0）与伪造句柄，那些值没有对应的 Go 对象，
// 而 `Proc.Free()` / `p.getState()` 也都不允许被这样的值驱动。
//
// 为什么读取面也要直通：契约是**两个方向**——free 宽容（HM_OK）、读取不放宽
// （必须报错）。只测宽容那一半，等于允许下一个人"顺手把 getter 也放宽"。
func rawProcFreeHandle(h uint64) Status {
	return Status(C.hm_proc_free(C.hm_handle(h)))
}

func rawGetStateHandle(h uint64, out *int32) Status {
	return Status(C.hm_proc_get_state(C.hm_handle(h), (*C.int32_t)(unsafe.Pointer(out))))
}

func rawProcWaitHandle(h uint64) Status {
	return Status(C.hm_proc_wait(C.hm_handle(h)))
}
