package native

/*
#include <stdlib.h>
#include "hermes_abi.h"
*/
import "C"

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sync/atomic"
	"time"
	"unsafe"

	"hermes/internal/errs"
)

// SandboxFlags 是沙箱开关位。零值 = 不启用沙箱（不是"全开"）。
type SandboxFlags uint32

const (
	SandboxNoNet       SandboxFlags = 1 << 0
	SandboxReadonlyTmp SandboxFlags = 1 << 1
	SandboxNoNewPrivs  SandboxFlags = 1 << 2
)

// criticalSandboxFlags 是"被忽略就必须在进程启动之前拒绝"的沙箱位。
// NO_NEW_PRIVS 刻意不在其中：Linux 在**未请求** NNP 时反而会把它记成
// ignored（native/c/sandbox.c 的记账反常），算进关键位会误拒所有正常调用。
const criticalSandboxFlags = SandboxNoNet | SandboxReadonlyTmp

// hmShimExitSetup 对齐 native/include/hm_internal.h 的 HM_SHIM_EXIT_SETUP。
// exec.go 的 cgo 前言刻意只包含公开头 hermes_abi.h，所以写成常量；
// internal/native 的结构守卫逐字比对头文件，避免两份真相漂移。
//
// F-11b 起这个常量**不再参与任何判据**：shim 在 setup 阶段失败时改走
// fd 3 的 [HMIF] 记录（→ Result.SetupFailed），退出码 40 退化成一个普通的
// 数字。保留常量只为那条跨语言一致性守卫 —— 头文件改了它就必须跟着改。
const hmShimExitSetup = 40

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

// hermes_abi.h 的位定义在 Go 侧的镜像。数值必须逐位一致 ——
// sandbox_decision_test.go 的 TestF11HeaderAndConstantsStayHonest 会比对
// 头文件里的字面量，改一边不改另一边会红。

// 句柄状态（hm_proc_get_state）。0 非法，getter 永不写出 0。
const (
	procRunning int32 = 1
	procReaped  int32 = 2
)

// 终止面（hm_proc_get_term 的 out_bits）。
const (
	termExited      uint32 = 1 << 0
	termSignaled    uint32 = 1 << 1
	termEscalated   uint32 = 1 << 2
	termDeadlineHit uint32 = 1 << 3
)

// setup 面（hm_proc_get_setup 的 out_bits）。
const (
	setupFailed          uint32 = 1 << 0
	setupExecFailed      uint32 = 1 << 1
	setupExecConfirmed   uint32 = 1 << 2
	setupChannelMeasured uint32 = 1 << 3
)

// Result 是一次工具调用的完整结局。
//
// F-11b/v4：这些值全部来自 hm_proc_* getter（槽位是唯一真相），没有任何
// 结构体承载它们跨边界 —— 也没有任何 free 函数需要配对。
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
	// Ignored 是平台"请求了但没能施加"的 HM_SB_* 位掩码（F-11）。
	// 来源：shim 经 fd 3 的带 tag 记录 → hm_proc_get_sb_ignored。
	Ignored SandboxFlags
	// SandboxRequested 是 C 侧回读的"本次 spawn 实际收到的请求位"
	// （hm_proc_get_sb_requested）。它与 Proc.requested 必须逐位相等：
	// 不等就说明 native 层把请求丢掉或改写了（判据②）。
	SandboxRequested SandboxFlags
	// Acked 是 shim 通过 fd 3 的 [HMIA] 记录回报的**实际施加成功**的
	// HM_SB_* 位（F-11b）。这是正面证据：Ignored 只能证明"没施加什么"，
	// Acked 才能证明"施加过什么"。缺任何一个被请求的关键位即拒绝。
	Acked SandboxFlags
	// SetupFailed 表示 shim 走的是 **setup 失败**路径（写过 [HMIF]）。
	// 它与"业务工具自己 exit(40)"彻底分开：40 这个数字不再参与任何判定，
	// 否则一个恰好返回 40 的正常工具会被误判成沙箱没关好（E2）。
	SetupFailed bool
	// ExecFailed 表示 shim 报了 execve 失败（裸 4 字节 errno）—— 目标
	// **从未启动**，因此不存在"未沙箱执行"的可能。归因由 SetupErrno 承担，
	// 上层按 spawn 失败报，不打 policy/denied。
	ExecFailed bool
	// ExecConfirmed 表示 fd 3 出现 EOF 的那一刻子进程仍然存活 —— 即
	// execve 已经发生（EOF 由 FD_CLOEXEC 触发，而不是"进程死了"）。
	// 它把 E3（execve 之前被 SIGKILL，必须拒）与 E4（execve 之后被杀死，
	// 必须放行）区分开：两者在通道上都表现为"有 ACK + 有信号"。
	ExecConfirmed bool
}

// Proc 是一个已启动的工具进程。必须 Wait 之后 Free（Free 早于 Wait 也安全：
// 它等价于"取消 + 收尸"，见 Free 的注释）。
type Proc struct {
	h    Handle
	done bool
	// requested 记住本次请求的沙箱位：fail-closed 判据必须做"按位交集"，
	// 只有"请求了 && 被忽略 && 是关键位"才拒绝。
	requested SandboxFlags
	// cancelRequested 记录"本方是否主动发过 Cancel"。Wait 用它区分 pre-exec
	// 死亡的两种真相：外部信号杀（可重试的 ErrToolNotStarted）与本方取消
	// （不可重试的 ErrToolCancelled）。用原子量是因为 Cancel 允许从任意
	// goroutine 调用，而 Wait 在另一个 goroutine 上读同一位。
	cancelRequested atomic.Bool
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

// sandboxFailClosed 是全仓唯一的沙箱拒绝错误构造点。
//
// POSIX 路径由 Wait() 兜底产出，Windows 路径由 Spawn() 拿到
// HM_E_SANDBOX_DENIED 后产出，两条路径共用它，保证对外错误类型完全一致。
// Lookup(AttrPolicy, SubDenied, 0) 命中 internal/errs/table.go 的 policy 行：
// ClassPolicy / RetryNone / CountsAsFailure=false。
// 刻意不复用 callTool 的 asRunError("NetworkError", err) —— 那会把策略拒绝
// 误分类成可重试的网络故障；也不用 diag.Diag —— 那是编译/加载期诊断且属冻结区。
//
// F-11b 给拒绝加了**机读的 reason**（走 Detail，不新增错误类：AttrAll/SubAll
// 是闭集，加一条会牵动映射表与 local_coverage 守卫）。reason 的取值集合是
// "state" / "requested" / "setup_failed" / "no_ack" / "ignored_critical" /
// "pre_create_denied"，与判据顺序一一对应，因此日志里一条 reason 就能定位
// 是哪一条先命中。
//
// 文本里带 acked= 是判据⑤的必要条件（"没有正面证据"必须能从日志里看出来），
// 且新格式仍包含旧子串 "ignored=0x..."，既有断言不受影响。
func sandboxFailClosed(op, reason string, requested, acked, ignored SandboxFlags) error {
	return errs.New(errs.AttrPolicy, errs.SubDenied,
		fmt.Sprintf("%s: sandbox denied (reason=%s) requested=0x%x acked=0x%x ignored=0x%x critical",
			op, reason, uint32(requested), uint32(acked), uint32(ignored))).
		WithDetail("reason", reason).
		WithDetail("requested", fmt.Sprintf("0x%x", uint32(requested))).
		WithDetail("acked", fmt.Sprintf("0x%x", uint32(acked))).
		WithDetail("ignored", fmt.Sprintf("0x%x", uint32(ignored)))
}

// spawnFailure 把 hm_spawn 的失败返回码翻译成对外错误。
// 只有 HM_E_SANDBOX_DENIED 需要改写：那是 Windows 在**进程创建之前**的硬拒，
// 必须与 POSIX 的 Wait() 拒绝走同一个 sandboxFailClosed()。
func spawnFailure(err error, requested SandboxFlags) error {
	st, ok := err.(Status)
	if ok && st == ErrSandboxDenied {
		return sandboxFailClosed("native.Spawn", "pre_create_denied", requested, 0, 0)
	}
	return err
}

// sandboxDecision 是 F-11b fail-closed 判据的纯函数形态，便于直接构造
// state=RUNNING、sbAcked=0、setupBits=0 等边界，不必逼测试去跑真进程。
//
// 判据顺序（顺序本身是判据的一部分，E5 依赖它）：
//
//	1. state != REAPED                        → state（程序错误，不归因成安全事件）
//	2. sbRequested != requested               → requested（native 层丢了/改了请求）
//	3. setupFailed                            → setup_failed（E2：语义信号取代 exit 40）
//	4. execFailed                             → allow（目标从未启动，无未沙箱执行）
//	5. critical &^ sbAcked != 0               → no_ack（E1 主干）
//	6. sbIgnored & critical != 0              → ignored_critical（F-11 既有）
//	7. allow
//
// **第一条不再是 abi_size。** 没有大小可协商，就没有"旧库按旧大小写"——
// 那条守卫守的失效模式已经被形态本身消灭，这是设计收益而非覆盖缺口。
// 旧库缺少 getter 符号 ⇒ 链接期 undefined reference ⇒ 构建期 fail-closed。
//
// 为什么 3 在 5 之前：真实 shim 里"setup 阶段失败"必然同时没有 ACK，两条
// 同时为真。把 setup_failed 放前面，能给出更精确的归因（哪一阶段失败）。
//
// 为什么 6 在最后：ignored 携带的信息（**哪一位**没施加）比 no_ack 更具体，
// 但"没有正面证据"比"自报了某位没施加"更严重 —— 一个既没 ACK 又自报
// ignored 的 shim 首先该被当成"没有握手"。
//
// setupErrno 明确不参与：它是纯数值描述（错在哪），不描述"是否施加成功"。
func sandboxDecision(
	state int32,
	sbRequested, sbAcked, sbIgnored SandboxFlags,
	requested SandboxFlags,
	setupBits uint32,
	setupErrno int, // 负例证据：判据中**显式忽略**
) error {
	_ = setupErrno

	if state != procReaped {
		// 读得太早是**程序错误**，不归因成安全事件。用 ErrInvalidArgument
		// 而不是 policy/denied：后者会让上层把它当成"策略拒绝、不要重试"。
		return fmt.Errorf("%w: result read before hm_proc_wait", ErrInvalidArgument)
	}
	if uint32(sbRequested) != uint32(requested) {
		// ② E1 变体：exec.c 没把 sandbox_flags 传下去（或传了个改写过的值）。
		// 这一条只在 native 层自己坏掉时触发，真实部署不可达。
		return sandboxFailClosed("native.Wait(request)", "requested",
			requested, sbAcked, sbIgnored)
	}
	if setupBits&setupFailed != 0 {
		// ③ E2：shim 显式写了 [HMIF][errno]。业务自己的 exit(40) 走不到这里。
		return sandboxFailClosed("native.Wait(setup)", "setup_failed",
			requested, sbAcked, sbIgnored)
	}
	if setupBits&setupExecFailed != 0 {
		// ④ execve 失败：目标**从未启动**，不存在未沙箱执行的可能。
		// 归因由 Result.SetupErrno 承担，上层按 spawn 失败报。
		return nil
	}
	critical := requested & criticalSandboxFlags
	if critical != 0 && (critical &^ sbAcked) != 0 {
		// ⑤ E1 主干：不合作 / 旧 shim 一字不写 fd 3 → 没有正面证据 → 拒绝。
		// 这是 F-11b 的核心：把"没有证据"从"放行"改成"拒绝"。
		return sandboxFailClosed("native.Wait(ack)", "no_ack",
			requested, sbAcked, sbIgnored)
	}
	if sbIgnored&critical != 0 {
		// ⑥ F-11 既有语义：平台自报了"请求了但没施加"的关键位。
		return sandboxFailClosed("native.Wait", "ignored_critical",
			requested, sbAcked, sbIgnored)
	}
	return nil
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
		return nil, spawnFailure(err, spec.Sandbox)
	}
	p := &Proc{h: Handle(h), requested: spec.Sandbox}
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
//
// F1：先在 Go 侧打一个"本方主动取消"的标记再进 C。标记必须**先于**信号落地
// —— 否则"信号已到、标记还没写"的那一瞬会让 Wait 把这次取消归错类。反过来
// （标记已写、信号未到）是安全方向：最坏把一次外部杀归到取消，而调用方本来
// 就打算结束它。
func (p *Proc) Cancel() error {
	p.cancelRequested.Store(true)
	return toError(C.hm_proc_cancel(C.hm_handle(p.h)))
}

// Wait 阻塞到子进程被收割。
//
// 阻塞语义是 ABI 层的规定（R6）：只允许在专用 goroutine 上调用，并发度受
// bulkhead 上限约束。调用前请确保 SetThreadBudget 已按同一上界设置过，
// 否则线程数会在高并发下被 cgo 的阻塞调用撑爆。
//
// F-11b/v4：hm_proc_wait 没有出参，结果留在槽位里；这里逐个用 getter 读出。
// 顺序是**判据的一部分**：先把 state 读出来（它决定"到底该不该读别的"），
// 再读 term / deadline / setup / sb_*，最后才做安全判定与结局判定。
func (p *Proc) Wait() (*Result, error) {
	if err := toError(C.hm_proc_wait(C.hm_handle(p.h))); err != nil {
		return nil, err
	}

	out := &Result{}

	// ① 状态：非 REAPED 时后面所有 getter 都会返回 ErrNotReady，所以先问它。
	state, err := p.getState()
	if err != nil {
		return nil, err
	}

	// ② 终止面：四个值来自同一次槽位读取（C 侧 D2 的原子性承诺）。
	t, err := p.getTerm()
	if err != nil {
		return nil, err
	}
	out.Exited = t.bits&termExited != 0
	out.Signaled = t.bits&termSignaled != 0
	out.Escalated = t.bits&termEscalated != 0
	out.DeadlineHit = t.bits&termDeadlineHit != 0
	out.ExitCode = t.exitCode
	out.TermSignal = t.termSignal
	out.KilledPGID = t.killedPGID

	// ③ deadline 面。
	if out.FiredCycles, err = p.getDeadline(); err != nil {
		return nil, err
	}
	if out.DeadlineSource, err = p.copyDeadlineSource(); err != nil {
		return nil, err
	}

	// ④ setup 面 + 沙箱位面。
	bits, setupErrno, err := p.getSetup()
	if err != nil {
		return nil, err
	}
	out.SetupErrno = setupErrno
	out.SetupFailed = bits&setupFailed != 0
	out.ExecFailed = bits&setupExecFailed != 0
	out.ExecConfirmed = bits&setupExecConfirmed != 0
	if out.SandboxRequested, err = p.getSbRequested(); err != nil {
		return nil, err
	}
	if out.Acked, err = p.getSbAcked(); err != nil {
		return nil, err
	}
	if out.Ignored, err = p.getSbIgnored(); err != nil {
		return nil, err
	}

	// ⑤ 输出缓冲：两阶段拷贝（先问长度，再按长度拷）。没有 free 要配对。
	if out.Stdout, err = p.copyStream(false); err != nil {
		return nil, err
	}
	if out.Stderr, err = p.copyStream(true); err != nil {
		return nil, err
	}

	// ⑥ 安全判据。setupErrno 作为负例证据传入，函数体内显式忽略。
	if err := sandboxDecision(
		state, out.SandboxRequested, out.Acked, out.Ignored,
		p.requested, bits, out.SetupErrno,
	); err != nil {
		return nil, err
	}

	// ⑦ 结局判据（D5）—— **在安全判定之后**：安全优先于正确性。
	//
	// 判据是 `MEASURED && !ExecConfirmed && Signaled`，三个合取项各有一个
	// 不可替代的理由（不是"保守起见先留着"）：
	//
	// 1. `!ExecConfirmed` —— `EXEC_CONFIRMED` 是"fd3 EOF 时子进程仍存活"的
	//    **采样**结果（supervisor.c:454/469/471/550，每 2ms 一轮）。它的缺失
	//    同时覆盖两件**性质完全不同**的事：
	//      (a) shim 在 execve 之前就死了 —— 工具确实没跑；
	//      (b) 工具跑了、而且很快退出 —— 我们没采样到。
	//    这两件事在父进程可观测的数据上**无法区分**，而 (b) 是**常态**
	//    （真实 shim 下 `/bin/echo hi` 跨轮 **0/20 ~ 18/20**、`/usr/bin/true`
	//    0~2/20、`/bin/sh -c 'exit 40'` 恒 20/20；替身 shim 下 echo 1..18/30）。
	//    这三个数放在一起正好证明机制是采样窗口而不是目标性质，而 echo 自己
	//    跨轮差一个数量级更说明**这个位承载不了"没执行"这种事实**）。
	//    所以单用 `!ExecConfirmed` 当判据，等价于把"采样不准"当成"工具没跑"
	//    —— 那不是保守，那是错的。完整跨轮表见 docs/KNOWN-GAPS.md 的证据等级表。
	// 2. `setupChannelMeasured` —— Windows 没有 fd3 通道，EXEC_CONFIRMED 恒 0、
	//    Signaled = cancelled；不写这一项会把每次主动取消都报成"工具未启动"
	//    （D5 的 Windows 豁免开关）。
	// 3. `Signaled` —— **判据的正当性就在这里**，不是补丁。因为"没确认到 exec"
	//    有两种解读（上面 (a)/(b)），要让它**只**指向 (a)，唯一的判别信号是
	//    死因：被信号杀死 ⇒ 工具没有机会跑完它想跑的东西/根本没起来；
	//    干净退出 ⇒ 它跑了（只是我们没采样到），或它在违约（见下）。
	//    加上这一项之后，判据宣称的命题收窄成一句**说得准**的话：
	//    「**被信号杀死**且没确认 exec」——这确实等于"工具没启动"。
	//    而"干净退出且没确认 exec"是**不可判定**（父进程侧无信息），不属于任何
	//    判据，归入信任边界。
	//
	// ⚠ **不要把 ACK-then-exit 那件事当成判据漏洞。** 一个诚实 shim 在写完
	// ACK 之后只有两条出路：(i) execve 成功；(ii) execve 失败 → 写 `[HMIF]`
	// → `_exit(41)`。要出现"ACK 之后干净 exit(0) 且没有 exec"，shim 必须
	// **违反自己的协议**（代码缺陷或撒谎）—— 那正好是信任边界已经涵盖的那
	// 一类（见 docs/C-ABI.md「信任边界：ACK 是 shim 自报的」）。判据不负责
	// 猜违约者的意图；它负责在自己能看见的信息上不说假话。
	// 该形态因此登记为「**不可判定 + 信任边界**」而不是「缺口」：
	// TestF11bAckThenExitWithoutExecIsUndecidable 的 body 断言的是**当前被接受**
	// 这个事实，不是"应该被接受"。
	//
	// 为什么必须给它一个独立结局（而不是混进成功的空结果）：ACK 之后"未确认
	// exec"的三种真相必须分开 —— 施加沙箱后被 SIGKILL（工具没跑）、施加前被杀
	// （不安全，已被判据⑤拦下）、exec 后被杀（安全的正常结果）。把第一种报成
	// "成功的空结果"，"工具被杀了"与"工具跑了但什么都没打印"在结果上完全同形。
	//
	// 而第一种还要再分两侧（F1）：
	//   - 本方主动 Cancel 掉了 → ErrToolCancelled，**不可重试**；
	//   - 外部信号 / shim 自己崩 → ErrToolNotStarted，可重试。
	// 不分开的话，"父 ctx 已取消 → spawn → cancel → 见 retryable → 重试"
	// 会自旋。
	if toolNotStarted(out.ExecConfirmed, bits, out.Signaled) {
		if p.cancelRequested.Load() {
			return nil, ErrToolCancelled
		}
		return nil, ErrToolNotStarted
	}
	if state != procReaped {
		// 兜底：sandboxDecision 之外再确认一次，防止将来有人把 ① 的检查挪走。
		return nil, fmt.Errorf("%w: result read before hm_proc_wait", ErrInvalidArgument)
	}
	return out, nil
}

// toolNotStarted 是 D5 结局判据的纯函数形态（与 sandboxDecision 分开：
// **安全判定先跑**，它是正确性判定）。抽成函数不是为了好看，是为了让
// "Windows 上永不触发"这件事可以被直接断言，而不必跑一个 Windows 进程。
//
// 它宣称的命题只有一个，而且说得准：
//
//	「**被信号杀死**，且我们**没有确认到 exec**」⇒ 工具没启动。
//
// 三个合取项各自不可替代（推导见 Wait() 里的 ⑦ 段）：
//   - !execConfirmed：没有 exec 的证据。**单独的它不构成命题** ——
//     "没确认到"既可能是"没跑"，也可能是"跑完得太快没采样到"，两者在父进程
//     侧不可区分，而后一种是常态（真实 shim 下 /bin/echo 跨轮 0/20 ~ 18/20）。
//   - channelMeasured：没有 fd3 通道就没有"确认"这个概念（Windows）。
//   - signaled：把不可区分的那一半排除掉 —— 干净退出属于**不可判定**，
//     不归任何判据；被信号杀死才是"它就是没跑完"的确定信号。
//
// 反过来说：**这个函数不该被扩展去判"干净退出且没确认 exec"**。那需要
// 协议级事实（shim 明示 exec），而不是更多采样或更多合取项 ——
// 见 docs/KNOWN-GAPS.md 的 F-11c 条目与 docs/C-ABI.md 的「信任边界」。
func toolNotStarted(execConfirmed bool, setupBits uint32, signaled bool) bool {
	return !execConfirmed &&
		setupBits&setupChannelMeasured != 0 &&
		signaled
}

// termSnapshot 是 hm_proc_get_term 的一次原子快照（D2 的 Go 侧视图）。
type termSnapshot struct {
	bits       uint32
	exitCode   int
	termSignal int
	killedPGID int
}

// getState 读 HM_PROC_RUNNING / HM_PROC_REAPED。这是唯一**不要求**已收割的
// getter —— RUNNING 本身就是一个合法答案。
func (p *Proc) getState() (int32, error) {
	var st C.int32_t
	if err := toError(C.hm_proc_get_state(C.hm_handle(p.h), &st)); err != nil {
		return 0, err
	}
	return int32(st), nil
}

// getTerm 一次读走 waitpid 状态字派生出的全部四个值。
func (p *Proc) getTerm() (termSnapshot, error) {
	var bits C.uint32_t
	var code, sig, pgid C.int32_t
	if err := toError(C.hm_proc_get_term(C.hm_handle(p.h), &bits, &code, &sig, &pgid)); err != nil {
		return termSnapshot{}, err
	}
	return termSnapshot{
		bits:       uint32(bits),
		exitCode:   int(code),
		termSignal: int(sig),
		killedPGID: int(pgid),
	}, nil
}

// getDeadline 读触发时的计数器读数（Windows 上恒 0：只有 wall 兜底）。
func (p *Proc) getDeadline() (uint64, error) {
	var v C.uint64_t
	if err := toError(C.hm_proc_get_deadline(C.hm_handle(p.h), &v)); err != nil {
		return 0, err
	}
	return uint64(v), nil
}

// copyDeadlineSource 读 "cycle" / "wall" / ""。
//
// 字符串容量语义（D6）：第一次调用 buf==NULL 取**字符数** n；分配 n+1
// （因为要拷那个 '\0'）。TOO_LARGE 时 C 侧把 *out_len 写成"所需 cap"，
// 必须按它重试 —— 两条路径下 *out_len 的含义不同，这里是唯一按错误码
// 分支取用的地方，不把这个歧义泄漏给上层。
func (p *Proc) copyDeadlineSource() (string, error) {
	var n C.size_t
	if err := toError(C.hm_proc_copy_deadline_source(C.hm_handle(p.h), nil, 0, &n)); err != nil {
		return "", err
	}
	if n == 0 {
		return "", nil
	}
	buf := make([]byte, int(n)+1)
	var got C.size_t
	err := toError(C.hm_proc_copy_deadline_source(
		C.hm_handle(p.h), (*C.char)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)), &got))
	if err != nil {
		if err != ErrTooLarge {
			return "", err
		}
		// got 现在是"所需 cap"（含 '\0'）。
		buf = make([]byte, int(got))
		if err2 := toError(C.hm_proc_copy_deadline_source(
			C.hm_handle(p.h), (*C.char)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)), &got)); err2 != nil {
			return "", err2
		}
	}
	return string(buf[:int(got)]), nil
}

// getSetup 读 setup 面：bits 与 setup_errno。
func (p *Proc) getSetup() (uint32, int, error) {
	var bits C.uint32_t
	var errno C.int32_t
	if err := toError(C.hm_proc_get_setup(C.hm_handle(p.h), &bits, &errno)); err != nil {
		return 0, 0, err
	}
	return uint32(bits), int(errno), nil
}

// getSbRequested 读 C 侧回读的"本次请求位"，用于判据②逐位比对。
func (p *Proc) getSbRequested() (SandboxFlags, error) {
	var v C.uint32_t
	if err := toError(C.hm_proc_get_sb_requested(C.hm_handle(p.h), &v)); err != nil {
		return 0, err
	}
	return SandboxFlags(v), nil
}

// getSbAcked 读 shim 的正面握手位（F-11b 的核心证据）。
func (p *Proc) getSbAcked() (SandboxFlags, error) {
	var v C.uint32_t
	if err := toError(C.hm_proc_get_sb_acked(C.hm_handle(p.h), &v)); err != nil {
		return 0, err
	}
	return SandboxFlags(v), nil
}

// getSbIgnored 读平台自报的"请求了但没施加"位。
func (p *Proc) getSbIgnored() (SandboxFlags, error) {
	var v C.uint32_t
	if err := toError(C.hm_proc_get_sb_ignored(C.hm_handle(p.h), &v)); err != nil {
		return 0, err
	}
	return SandboxFlags(v), nil
}

// copyStream 是 hm_proc_copy_stdout / hm_proc_copy_stderr 的两阶段封装。
//
// 字节流语义（D6，**与字符串那条不同**）：*out_len 在所有路径上都是
// "需要的/已写的字节数"，没有 +1，也不需要终止符。
// 内存始终在 C 侧槽位里：只 memcpy 出来，从不接手所有权。
func (p *Proc) copyStream(stderr bool) ([]byte, error) {
	call := func(buf *C.uint8_t, cap C.size_t, outLen *C.size_t) C.hm_status {
		if stderr {
			return C.hm_proc_copy_stderr(C.hm_handle(p.h), buf, cap, outLen)
		}
		return C.hm_proc_copy_stdout(C.hm_handle(p.h), buf, cap, outLen)
	}
	var n C.size_t
	if err := toError(call(nil, 0, &n)); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	buf := make([]byte, int(n))
	var got C.size_t
	err := toError(call((*C.uint8_t)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)), &got))
	if err != nil {
		if err != ErrTooLarge {
			return nil, err
		}
		buf = make([]byte, int(got))
		if err2 := toError(call((*C.uint8_t)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)), &got)); err2 != nil {
			return nil, err2
		}
	}
	return buf[:int(got)], nil
}

// 说明（v4）：v3 曾经有一个 Proc.SetupStatus() + SetupStatus 结构体，用来
// 装 hm_setup_status 的出参。随着那个结构体被删除，这条 API 也一起消失 ——
// setup 面现在是 Result 里的 SetupFailed / ExecFailed / ExecConfirmed 三个
// 布尔（加上 Acked / Ignored / SandboxRequested 三个位面），全部来自
// hm_proc_get_setup / hm_proc_get_sb_*，没有第二份真源。
// 注意这不是破坏性变更：SetupStatus 只存在于未提交的 v3 工作树里，HEAD 中
// 从来没有过这个类型。

// Free 归还进程槽位。**Wait 之前调用也是合法的**：等价于"取消 + 收尸"
// （向进程组发 SIGKILL 并 waitpid），返回 nil。
//
// 为什么不做成"顺序错误就报错"：Free 是清理动作，最需要它的场景恰恰是
// "调用方在 Wait 之前就决定不要这个结果了"（父 context 取消、提前 return、
// finalizer 兜底）。这时报错的唯一效果是——调用方把 err 忽略掉，孤儿进程
// 留在机器上。宁可让它顺手收拾干净。
//
// 真正的顺序**错误**只有一种：有 getter 在飞（refcnt > 0）或 hm_proc_wait
// 还在另一个 goroutine 里阻塞。这两种返回 HM_E_INVALID_ARG —— 因为此时
// 清槽位就是 use-after-free，不能猜。
func (p *Proc) Free() error {
	if p.done {
		return nil
	}
	err := toError(C.hm_proc_free(C.hm_handle(p.h)))
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
