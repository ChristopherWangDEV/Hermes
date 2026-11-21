// Package native 是 Hermes 原生层（C / C++ / Rust）在 Go 侧的唯一门面。
//
// 边界纪律（native/include/hermes_abi.h 的 R1–R8，逐条都是硬约束）：
//
//	R1  Go 绝不让 C/Rust 保存 Go 指针。跨调用存活的数据一律在不透明 handle 后面。
//	R2  handle = slot(低32) | generation(高32)，0 非法；generation 不匹配返回
//	    ErrHandleStale（是命名错误，不是未定义行为）。
//	R5  分配与释放同侧：缓冲区始终由 C 侧槽位持有，hm_proc_copy_* 只做
//	    memcpy，调用方从不接手所有权，因此**没有**任何 free 函数需要配对
//	    （F-11b/v4 删掉了 hm_spawn_res_free：结果不再跨边界搬运）。
//	    Rust 分配的只能 hm_*_free 释放；Go 侧永不 free 他人内存。
//
// 这里刻意只做"薄封装"：不引入超时、重试、预算等任何策略。
// 策略属于 Go 引擎层（internal/resilience / internal/budget），原生层只负责
// "把这件事做到内核级正确"，越界就会让两层职责互相污染。
package native

/*
#cgo CFLAGS: -I${SRCDIR}/../../native/include
#cgo !windows LDFLAGS: -L${SRCDIR}/../../build -lhermes_native
// Windows 上由构建脚本用绝对路径把三个 .lib 传给链接器：lld-link 的库搜索
// 与 GNU ld 的 -L 语义不同，走 -l 容易在中文路径或空格路径下失败。
#cgo windows LDFLAGS: -L${SRCDIR}/../../build -lhermes_native
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
//
// 2.0：结构体布局与导出符号一个都没动，升的是**语义** —— 2.0.0 的 IR
// （`decl` / `const` / `fallback`）不是 1.x 运行时能解释的形状，而 `.hbc`
// 成员的 `abi_version` / `requires[].abi_version` 描述的就是这件事。
// 布局不变却必须升号的理由写在 hermes_abi.h 与 docs/C-ABI.md。
const (
	ABIMajor = 2
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
	// ErrNotReady = "结果还没算出来"。**不是** ErrInvalidArg：那不是调用方的
	// 错。加它不新增符号、不动任何布局（HM_E_NOT_READY = -16）。
	ErrNotReady Status = -16
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

	// ErrResultNotReady 是 getter 层的哨兵：hm_proc_wait 尚未返回，槽位里
	// 还没有结果。它与 ErrHandleStaleErr 严格区分（"还没算出来" vs
	// "这个名字已经失效"），并且**绝不**退化成"返回零值当答案"。
	ErrResultNotReady = errors.New("native: result not ready (call hm_proc_wait first)")

	// ErrToolNotStarted 是 Wait() 层的哨兵（D5）：有 fd3 setup 通道、
	// 且从未确认 execve、且**是死于信号** —— 即"工具根本没启动"。
	//
	// 第三个合取项（Signaled）不是保守兜底，是**判据的正当性来源**：
	// `!ExecConfirmed` 本身混淆了两件事 ——「shim 在 execve 前就死了」与
	// 「工具跑了、很快退出、我们没采样到」，而父进程侧**无法区分**这两者，
	// 后者还是常态（真实 build/hermes-shim + /bin/echo hi 跨轮 0/20 ~ 18/20，
	// 同一目标不同轮次差一个数量级，Wait 却从不报错）。只有"被信号杀死"这一种
	// 死因能把后者排除掉，于是判据宣称的命题收窄成一句说得准的话。推导与数据见
	// exec.go 的 ⑦ 段与 docs/KNOWN-GAPS.md 的证据等级表（R1 的量化形态，不是 flake）。
	//
	// "ACK 全了然后干净 exit(0) 而不 execve"**不是**判据漏洞：诚实 shim 在
	// ACK 之后只有 execve 成功 / 写 [HMIF] 后 _exit(41) 两条出路，第三条要求
	// shim 违反自己的协议 ⇒ 归信任边界（docs/C-ABI.md）。那件事的用例断言的
	// 是"当前被接受"这个事实，不是"应该被接受"。
	//
	// 它**不是** policy/denied：后者是安全策略拒绝（Class=policy、
	// Retry=None、不该重试），而它是"链路没走完"的瞬时失败（可重试）。
	// 两者的 Class 与重试策略相反，绝不能合并。
	// 它也不是 internal/errs 的错误类（AttrAll/SubAll 是闭集）：
	// 这里只是本包的普通 error，以普通形式向上浮。
	ErrToolNotStarted = errors.New("native: tool never started (no exec confirmation)")

	// ErrToolCancelled 是 pre-exec 死亡的**另一侧**：这次 spawn 是被**本方
	// 主动 Cancel** 掉的，不是环境把它弄死的。
	//
	// 为什么要单独一个哨兵：ErrToolNotStarted 是可重试的（"链路没走完，
	// 再试一次也许就好了"）。如果把"我们自己取消了"也报成它，一个朴素循环
	// （父 ctx 取消 → spawn → Cancel → 见到可重试 → 重试）会**自旋**。
	// 所以两者必须是两个结局，且**互斥**：可重试类里只有 ErrToolNotStarted，
	// 断言 err 不是它就等于断言"不可重试"，不需要第二套 Retry 分类。
	//
	// 判据不变，只看一个附加输入：本次 Proc 上有没有发出过 Cancel 请求。
	// C 侧语义一字未改（取消仍然是"杀进程组 + 收尸"）。
	ErrToolCancelled = errors.New("native: tool cancelled before it started")
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
	case ErrNotReady:
		return ErrResultNotReady.Error()
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
