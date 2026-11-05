// Package native 是 Hermes 原生层（C / C++ / Rust）在 Go 侧的唯一门面。
//
// 边界纪律（native/include/hermes_abi.h 的 R1–R8，逐条都是硬约束）：
//
//	R1  Go 绝不让 C/Rust 保存 Go 指针。跨调用存活的数据一律在不透明 handle 后面。
//	R2  handle = slot(低32) | generation(高32)，0 非法；generation 不匹配返回
//	    ErrHandleStale（是命名错误，不是未定义行为）。
//	R5  分配与释放同侧：C malloc 的只能 hm_spawn_res_free，Rust 分配的只能 hm_*_free。
//	    本包**从不** free 他人的内存，也不把 Go 内存交给 C 长期持有。
//
// 这里刻意只做"薄封装"：不引入超时、重试、预算等任何策略。
// 策略属于 Go 引擎层（internal/resilience / internal/budget），原生层只负责
// "把这件事做到内核级正确"，越界就会让两层职责互相污染。
package native

/*
#cgo CFLAGS: -I${SRCDIR}/../../native/include
#cgo LDFLAGS: -L${SRCDIR}/../../build -lhermes_native
#include <stdlib.h>
#include <string.h>
#include "hermes_abi.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"
)

// ABI 版本。必须与 native/include/hermes_abi.h 的 HM_ABI_MAJOR/MINOR 一致；
// tests/guard 有一条测试逐字段比对，防止两边手改漂移。
const (
	ABIMajor = 1
	ABIMinor = 0
)

// 长度硬上限（R3）。超限一律报错，**绝不截断** —— 截断会让哈希静默对不上。
const (
	SensitiveMax  = 1 << 20  // 1 MiB
	ViewMax       = 16 << 20 // 16 MiB
	ChainBlockMax = 1 << 20  // 1 MiB
)

// Status 是原生层返回码。零值即成功，负数为错误。
type Status int32

const (
	OK               Status = 0
	ErrInvalidArg    Status = -1
	ErrABIVersion    Status = -2
	ErrHandleStale   Status = -3
	ErrTooLarge      Status = -4
	ErrNoMem         Status = -5
	ErrIO            Status = -6
	ErrSpawn         Status = -10
	ErrDeadline      Status = -11
	ErrSandboxDenied Status = -12
	ErrKilled        Status = -13
	ErrRlimitSetup   Status = -14
	ErrPanic         Status = -15
)

// 错误值。全部是哨兵，便于 errors.Is 判定与跨层翻译。
var (
	ErrInvalidArgument  = errors.New("native: invalid argument")
	ErrABIMismatch      = errors.New("native: ABI version mismatch")
	ErrHandleStaleErr   = errors.New("native: stale handle (use after free)")
	ErrTooLargeErr      = errors.New("native: input exceeds ABI hard limit")
	ErrOutOfMemory      = errors.New("native: out of memory")
	ErrIOFail           = errors.New("native: io failure")
	ErrSpawnFailed      = errors.New("native: spawn failed")
	ErrDeadlineHit      = errors.New("native: deadline exceeded")
	ErrSandboxDeniedErr = errors.New("native: sandbox denied")
	ErrProcessKilled    = errors.New("native: process killed")
	ErrRlimitSetupErr   = errors.New("native: rlimit setup failed")
	ErrNativePanic      = errors.New("native: native layer panicked")
)

// Status 实现 error，同时保留原始数值（trace 里要记，不能只记字符串）。
func (s Status) Error() string {
	switch s {
	case OK:
		return "native: ok"
	case ErrInvalidArg:
		return ErrInvalidArgument.Error()
	case ErrABIVersion:
		return ErrABIMismatch.Error()
	case ErrHandleStale:
		return ErrHandleStaleErr.Error()
	case ErrTooLarge:
		return ErrTooLargeErr.Error()
	case ErrNoMem:
		return ErrOutOfMemory.Error()
	case ErrIO:
		return ErrIOFail.Error()
	case ErrSpawn:
		return ErrSpawnFailed.Error()
	case ErrDeadline:
		return ErrDeadlineHit.Error()
	case ErrSandboxDenied:
		return ErrSandboxDeniedErr.Error()
	case ErrKilled:
		return ErrProcessKilled.Error()
	case ErrRlimitSetup:
		return ErrRlimitSetupErr.Error()
	case ErrPanic:
		return ErrNativePanic.Error()
	default:
		return fmt.Sprintf("native: unknown status %d", int32(s))
	}
}

// toError 把 C 返回码翻译成 error。成功返回 nil。
func toError(rc C.hm_status) error {
	if rc == 0 {
		return nil
	}
	return Status(int32(rc))
}

// ABIVersion 返回原生层编译期看到的版本（major<<16 | minor）。
func ABIVersion() uint32 { return uint32(C.hm_abi_version()) }

// BuildID 是头文件集合的内容哈希，参与构建可重现性校验。
func BuildID() string { return C.GoString(C.hm_abi_build_id()) }

var initMu sync.Mutex

// InitExpecting 校验 major 相等并初始化原生运行时。
//
// expect 必须是调用方编译期看到的版本。major 不等就是 ErrABIMismatch ——
// 这里**不提供**"降级试试"这条路：ABI 主版本不同意味着结构体布局可能已经变了，
// 降级使用会让 C 侧按旧偏移写新字段，那是内存损坏，不是兼容性问题。
func InitExpecting(expect uint32) error {
	if expect>>16 != uint32(ABIMajor) {
		return fmt.Errorf("%w: caller expects major %d, library is %d",
			ErrABIMismatch, expect>>16, ABIMajor)
	}
	initMu.Lock()
	defer initMu.Unlock()
	return toError(C.hm_runtime_init(C.uint32_t(expect), 0))
}

// Init 用当前链接到的原生层版本初始化，是常规入口。
func Init() error { return InitExpecting(ABIVersion()) }

// Shutdown 释放原生运行时持有的全局资源。重复调用安全。
func Shutdown() { C.hm_runtime_shutdown() }

// Handle 是跨边界不透明句柄。零值非法（R2）。
type Handle uint64

// Valid 报告句柄是否非零。注意：非零**不等于**仍然有效 ——
// 有效性只能由使用它的调用回答（失效会返回 ErrHandleStale）。
func (h Handle) Valid() bool { return uint64(h) != 0 }

// DebugPeek 复制"最近一次 hm_sensitive_free 释放的那块内存"的内容。
//
// 只给测试用（T03 的零化验证）。生产代码路径不得调用；tests/guard 有一条
// 测试断言除 tests/ 与 _test.go 外没有文件引用它。
func DebugPeek() ([]byte, error) {
	n := C.size_t(SensitiveMax)
	buf := C.malloc(n)
	if buf == nil {
		return nil, ErrOutOfMemory
	}
	defer C.free(buf)
	var written C.size_t
	if err := toError(C.hm_debug_peek((*C.uint8_t)(buf), n, &written)); err != nil {
		return nil, err
	}
	return C.GoBytes(buf, C.int(written)), nil
}

// byteSlice 把 Go 切片的首地址交给 C 用一次。
//
// 空切片返回 nil —— C 侧对 NULL/0 的处理与"零长度非 NULL"不同，
// 传一个悬空的 non-nil 指针是自找麻烦。
func byteSlice(b []byte) (*C.uint8_t, C.size_t) {
	if len(b) == 0 {
		return nil, 0
	}
	return (*C.uint8_t)(unsafe.Pointer(&b[0])), C.size_t(len(b))
}
