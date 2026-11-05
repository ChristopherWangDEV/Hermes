package native

/*
#include <stdlib.h>
#include "hermes_abi.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"time"
	"unsafe"
)

// SandboxFlags 是沙箱开关位。零值 = 不启用沙箱（不是"全开"）。
type SandboxFlags uint32

const (
	SandboxNoNet       SandboxFlags = 1 << 0
	SandboxReadonlyTmp SandboxFlags = 1 << 1
	SandboxNoNewPrivs  SandboxFlags = 1 << 2
)

// Limits 映射到子进程的 rlimit。零值表示"不设置"。
//
// 这些限制只作用于工具子进程，永远不影响 Go 引擎进程 —— 因为设置发生在
// hermes-shim 里（它是被 exec 的目标），而不是在引擎里调 setrlimit。
// 这不是风格问题：setrlimit 是进程级的，在引擎里调一次，整个引擎的
// 地址空间与打开文件数就被永久改了。
type Limits struct {
	AddressSpaceBytes uint64
	CPUSeconds        uint64
	OpenFiles         uint64
	Processes         uint64
	FileSizeBytes     uint64
}

// Spec 描述一次工具调用。Exe 与 Args 必填。
type Spec struct {
	Exe  string
	Args []string
	Env  []string // nil = **空环境**（不继承）；这是 ABI 契约，不是默认值巧合
	Dir  string

	Deadline     time.Duration // 0 = 无 deadline
	WallBackstop time.Duration // 计数器不可用时的兜底
	KillGrace    time.Duration // SIGTERM → SIGKILL 的宽限；0 = 原生默认 500ms

	Limits  Limits
	Sandbox SandboxFlags

	Stdin     []byte
	StdoutCap int // 0 = 原生默认 1 MiB
	StderrCap int
}

// Result 是一次工具调用的完整结局。
//
// 保留这么多字段不是冗余：归因必须能区分"工具自己 exit(1)"（Exited=true,
// ExitCode=1）、"被我们的 deadline 杀了"（DeadlineHit=true）、"被信号干掉"
// （Signaled=true）与"我们连笼子都没给它关好"（SetupErrno != 0）。
// 少一个字段，这四种情况就会在 trace 里糊成同一句"调用失败"。
type Result struct {
	Exited         bool
	ExitCode       int
	Signaled       bool
	TermSignal     int
	Escalated      bool
	DeadlineHit    bool
	KilledPGID     int
	DeadlineSource string // "cycle" | "wall" | ""
	FiredCycles    uint64
	SetupErrno     int
	Stdout         []byte
	Stderr         []byte
}

// Proc 是一个已启动的工具进程。必须 Wait 之后 Free。
type Proc struct {
	h    Handle
	done bool
}

// shimName 是督导程序的文件名。它是交付物之一（D-005），不是可选组件。
const shimName = "hermes-shim"

// ResolveShim 定位 hermes-shim。
//
// explicit 非空时只认它（这正是 T08 ④ 要的行为：`HERMES_SHIM` 指向不存在的
// 路径必须报错并说出期望路径，而不是"悄悄换一个能用的"——那样部署时的
// 配置错误会被吞掉，直到某天超时全部失灵才发现跑的是别的 shim）。
func ResolveShim(explicit string) (string, error) {
	// 环境变量是部署期的覆盖点（容器里二进制与 shim 常不在同一目录）。
	// 显式参数优先级高于它，但那也是"覆盖"，不是"忽略"。
	if explicit == "" {
		explicit = os.Getenv("HERMES_SHIM")
	}
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("%w: HERMES_SHIM=%q is not usable: %v",
				ErrSpawnFailed, explicit, err)
		}
		return explicit, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("%w: cannot locate own executable: %v", ErrSpawnFailed, err)
	}
	cand := filepath.Join(filepath.Dir(exe), shimName)
	if _, err := os.Stat(cand); err != nil {
		return "", fmt.Errorf("%w: expected %q next to the engine (set HERMES_SHIM to override)",
			ErrSpawnFailed, cand)
	}
	return cand, nil
}

// Spawn 启动一个工具进程。
func Spawn(spec Spec, shimPath string) (*Proc, error) {
	if spec.Exe == "" {
		return nil, fmt.Errorf("%w: Spec.Exe is empty", ErrInvalidArgument)
	}
	shim, err := ResolveShim(shimPath)
	if err != nil {
		return nil, err
	}

	cShim := C.CString(shim)
	cExe := C.CString(spec.Exe)
	defer C.free(unsafe.Pointer(cShim))
	defer C.free(unsafe.Pointer(cExe))

	argv, freeArgv := cStringArray(spec.Args)
	defer freeArgv()
	var envp **C.char
	var freeEnv func()
	if spec.Env != nil {
		envp, freeEnv = cStringArray(spec.Env)
		defer freeEnv()
	}

	var cwd *C.char
	if spec.Dir != "" {
		cwd = C.CString(spec.Dir)
		defer C.free(unsafe.Pointer(cwd))
	}

	var stdinPtr *C.uint8_t
	if len(spec.Stdin) > 0 {
		stdinPtr = (*C.uint8_t)(C.CBytes(spec.Stdin))
		defer C.free(unsafe.Pointer(stdinPtr))
	}

	var deadlineCycles C.uint64_t
	if spec.Deadline > 0 {
		ticks, err := TicksForDuration(uint64(spec.Deadline.Nanoseconds()))
		if err != nil {
			return nil, err
		}
		deadlineCycles = C.uint64_t(ticks)
	}

	var req C.hm_spawn_req
	req.abi_size = C.uint32_t(unsafe.Sizeof(req))
	req.shim = cShim
	req.exe = cExe
	req.argv = argv
	req.envp = envp
	req.cwd = cwd
	req.deadline_cycles = deadlineCycles
	req.wall_backstop_ms = C.uint64_t(spec.WallBackstop.Milliseconds())
	req.rl_as_bytes = C.uint64_t(spec.Limits.AddressSpaceBytes)
	req.rl_cpu_sec = C.uint64_t(spec.Limits.CPUSeconds)
	req.rl_nofile = C.uint64_t(spec.Limits.OpenFiles)
	req.rl_nproc = C.uint64_t(spec.Limits.Processes)
	req.rl_fsize = C.uint64_t(spec.Limits.FileSizeBytes)
	req.kill_grace_ms = C.uint32_t(spec.KillGrace.Milliseconds())
	req.sandbox_flags = C.uint32_t(spec.Sandbox)
	req.stdin_bytes = stdinPtr
	req.stdin_len = C.size_t(len(spec.Stdin))
	req.stdout_cap = C.size_t(spec.StdoutCap)
	req.stderr_cap = C.size_t(spec.StderrCap)

	var h C.hm_handle
	if err := toError(C.hm_spawn(&req, &h)); err != nil {
		return nil, err
	}
	p := &Proc{h: Handle(h)}
	runtime.SetFinalizer(p, func(x *Proc) {
		// 漏掉 Wait/Free 是模型错误，但不能因此泄漏 fd 与僵尸进程。
		// Finalizer 只做兜底，不做归因 —— 真正的归因在 trace 里。
		if !x.done {
			_ = x.Free()
		}
	})
	return p, nil
}

// Cancel 立即向工具进程组发 SIGTERM。可从任意 goroutine 调用。
//
// 发的是 -pgid 而不是单个 pid：工具自己 fork 出来的孙子进程必须一起死，
// 否则"杀掉卡死的调用"只是把孤儿留在了机器上。
func (p *Proc) Cancel() error { return toError(C.hm_spawn_cancel(C.hm_handle(p.h))) }

// Wait 阻塞到子进程被收割。
//
// 阻塞语义是 ABI 层的规定（R6）：只允许在专用 goroutine 上调用，并发度受
// bulkhead 上限约束。调用前请确保 SetThreadBudget 已按同一上界设置过，
// 否则线程数会在高并发下被 cgo 的阻塞调用撑爆。
func (p *Proc) Wait() (*Result, error) {
	var res C.hm_spawn_res
	res.abi_size = C.uint32_t(unsafe.Sizeof(res))
	if err := toError(C.hm_spawn_wait(C.hm_handle(p.h), &res)); err != nil {
		return nil, err
	}
	// res 里的缓冲区是 C malloc 的，必须用 C 的释放函数；先拷走再释放。
	out := &Result{
		Exited:      res.exited != 0,
		ExitCode:    int(res.exit_code),
		Signaled:    res.signaled != 0,
		TermSignal:  int(res.term_signal),
		Escalated:   res.escalated != 0,
		DeadlineHit: res.deadline_hit != 0,
		KilledPGID:  int(res.killed_pgid),
		FiredCycles: uint64(res.fired_cycles),
		SetupErrno:  int(res.setup_errno),
	}
	if res.deadline_source != nil {
		out.DeadlineSource = C.GoString(res.deadline_source)
	}
	if res.stdout_bytes != nil && res.stdout_len > 0 {
		out.Stdout = C.GoBytes(unsafe.Pointer(res.stdout_bytes), C.int(res.stdout_len))
	}
	if res.stderr_bytes != nil && res.stderr_len > 0 {
		out.Stderr = C.GoBytes(unsafe.Pointer(res.stderr_bytes), C.int(res.stderr_len))
	}
	C.hm_spawn_res_free(&res)
	return out, nil
}

// Free 归还进程槽位。必须在 Wait 之后调用，否则 err 会指出顺序错了。
func (p *Proc) Free() error {
	if p.done {
		return nil
	}
	err := toError(C.hm_spawn_free(C.hm_handle(p.h)))
	if err == nil {
		p.done = true
		runtime.SetFinalizer(p, nil)
	}
	return err
}

// SetThreadBudget 按 bulkhead 上限设置 Go 线程数上限。
//
// 为什么要手动设：Wait 是阻塞的 cgo 调用，每个阻塞调用可能占一个 OS 线程。
// 不设上限时，一次误配的并发（比如 bulkhead 配成 4096）会把线程数推到
// 系统极限，然后进程以"无法创建线程"这种与业务毫无关系的理由死掉。
//
// 预留 GOMAXPROCS + 16 给运行时自己用；这个数字不是魔法，是"调度器需要
// 跑 GC 与 sysmon"的最低余量。
func SetThreadBudget(bulkhead int) {
	if bulkhead <= 0 {
		return
	}
	debug.SetMaxThreads(bulkhead + runtime.GOMAXPROCS(0) + 16)
}

// cStringArray 把 Go 字符串切片转成以 NULL 结尾的 C 字符串数组。
//
// 指针数组本身也用 C.malloc 分配，而不是 Go 切片 —— 因为 cgo 规定
// "C 不得保存 Go 指针"，而 posix_spawn 会读这块内存在调用返回之前。
// 用 C 内存可以让这条规则从"我知道它不会保存"变成"它根本没得保存"。
func cStringArray(items []string) (**C.char, func()) {
	n := len(items)
	arr := (**C.char)(C.malloc(C.size_t(n+1) * C.size_t(unsafe.Sizeof(uintptr(0)))))
	if arr == nil {
		return nil, func() {}
	}
	slice := unsafe.Slice(arr, n+1)
	strs := make([]*C.char, 0, n)
	for i, s := range items {
		cs := C.CString(s)
		strs = append(strs, cs)
		slice[i] = cs
	}
	slice[n] = nil
	return arr, func() {
		for _, cs := range strs {
			C.free(unsafe.Pointer(cs))
		}
		C.free(unsafe.Pointer(arr))
	}
}

// ErrProcNotWaited 在 Wait 之前调用 Free 时返回，用于把顺序错误变成可诊断的错误。
var ErrProcNotWaited = errors.New("native: Free called before Wait")
