package native_test

// F-11 / F-11b(v4) 的端到端回归。
//
// 被测的是**真实**链路：exec.c 的 fd 3 通道 → supervisor.c 的 pump_setup
// 累加解析 → 槽位字段 → hm_proc_* getter → Go Wait() 的判据 → errs 错误。
// 注意 v4 **没有**任何结果结构体：所有值都经 getter 从槽位读
// （hm_proc_get_term / get_setup / get_sb_* / copy_*）。
//
// f11FakeShim 是**黑盒 fd 3 协议替身**：它是一个直接往 fd 3 写原始
// 协议字节的 shell 脚本，**并不调用** hermes_sandbox_apply，替换的是整个
// shim 子进程而非「平台返回值」。本机是 macOS，NO_NET 在 profile 里被
// 真实施加（或外层沙箱让 sandbox_init 直接失败），没法稳定地造出
// "ignored 含 NO_NET"这一分支，故用协议字节确定性构造。这不是假接线：
// exec.c 的 fd 3 通道、pump_setup、槽位、全部 getter、Wait() 判据、
// errs 构造全部是生产代码，替身只提供输入。
//
// 平台诚实性：Windows 的 win_spawn.c 在 darwin 上被 #if defined(_WIN32)
// 排除，编不到、跑不了；其 P0 拒绝分支由 sandbox_decision_test.go 的
// 源码结构守卫 + spawnFailure 纯函数等价验证覆盖，不冒充真机实测。
//
// F-11b(v4) 的验收范围：判据不再有 `exit_code == 40` 这条启发式，改为 fd 3 上的
// [HMIA]（施加成功）/ [HMIF]（setup 失败）/ 裸 errno（execve 失败）三条记录
// + fd 3 EOF 的**时序**。因此本文件同时证明两件事：
//   · 不误报：业务自己的 exit(40)、execve 失败、exec 之后被信号杀 → 放行；
//   · 不漏报：不合作 shim（无 ACK）、setup 失败 → 拒绝；
//   · 单独一类：施加后被 SIGKILL → **不是** policy/denied，而是
//     ErrToolNotStarted（链路没走完，可重试）；若这次 spawn 是**本方主动
//     Cancel** 的则是 ErrToolCancelled（同样"没启动"，但不可重试）。
// 残差 F-11b-R1（**已实测**）是 `EXEC_CONFIRMED` 的**采样**性质（2ms 轮询）：
//   · 可判定的一侧：被信号杀死且没确认 exec ⇒ 工具没启动（判据命中，正确）；
//   · **不可判定**的一侧：干净退出且没确认 exec —— 父进程侧没有信息能区分
//     "跑得太快没采样到"（常态）与"shim 违约没 exec"，故不归任何判据，
//     归信任边界。用例 TestF11bAckThenExitWithoutExecIsUndecidable（t.Skip，
//     文本即性质），断言的是"当前被接受"这个事实，不是"应该被接受"。

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"hermes/internal/errs"
	"hermes/internal/native"
)

// fd 3 上三种带 tag 记录的 tag 值，与 native/include/hm_internal.h 逐字对应。
// 小端落盘：0x484D4947 → "GIMH"、0x484D4941 → "AIMH"、0x484D4946 → "FIMH"。
const (
	f11bTagIgnored uint32 = 0x484D4947
	f11bTagAck     uint32 = 0x484D4941
	f11bTagFailed  uint32 = 0x484D4946
)

// f11bRecord 把一条 8 字节带 tag 记录编码成 shell printf 的八进制转义串。
//
// 手写 `'\107\111\115\110\001\000\000\000'` 极易写反字节序（这正是蓝图里
// 专门提醒的坑），所以统一从这里生成：tag 小端 + payload 小端，各 4 字节。
func f11bRecord(tag, payload uint32) string {
	b := [8]byte{
		byte(tag), byte(tag >> 8), byte(tag >> 16), byte(tag >> 24),
		byte(payload), byte(payload >> 8), byte(payload >> 16), byte(payload >> 24),
	}
	var sb strings.Builder
	for _, c := range b {
		fmt.Fprintf(&sb, "\\%03o", c)
	}
	return sb.String()
}

// f11bRecordWrite 生成"把记录写到 fd 3"的一行脚本。
func f11bRecordWrite(tag, payload uint32) string {
	return "printf '" + f11bRecord(tag, payload) + "' >&3\n"
}

func f11RealShim(t *testing.T) string {
	t.Helper()
	p, err := native.ResolveShim("")
	if err != nil {
		t.Fatalf("ResolveShim: %v (build/hermes-shim missing? run scripts/build.sh first)", err)
	}
	return p
}

// f11FakeShim 写一个只负责"往 fd 3 写带 tag 记录 + errno 然后退出"的平台层
// 测试替身。它绝不 execve 目标程序 —— 这正是预执行拒绝要证明的事。
func f11FakeShim(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake-shim")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write fake shim: %v", err)
	}
	return p
}

func f11WriteTarget(t *testing.T, marker string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "target")
	body := "#!/bin/sh\nprintf ran > \"" + marker + "\"\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatalf("write target: %v", err)
	}
	return p
}

func f11Rejection(t *testing.T, err error) *errs.Error {
	t.Helper()
	if err == nil {
		t.Fatal("expected a fail-closed *errs.Error, got nil")
	}
	var e *errs.Error
	if !errors.As(err, &e) {
		t.Fatalf("error is %T, want *errs.Error: %v", err, err)
	}
	if e.Attr != errs.AttrPolicy || e.Sub != errs.SubDenied {
		t.Fatalf("attr/sub = %s/%s, want policy/denied", e.Attr, e.Sub)
	}
	if e.Retry() != errs.RetryNone || e.CountsAsBreakerFailure() {
		t.Fatalf("retry=%s counts=%v, want none/false", e.Retry(), e.CountsAsBreakerFailure())
	}
	if e.Key() != "policy/denied" || strings.Contains(e.Error(), "NetworkError") {
		t.Fatalf("misclassified rejection: %v", err)
	}
	return e
}

// TestF11SandboxIgnoredRecordPropagates 是"诚实传播"正例（macOS 实测）：
// 请求 NO_NEW_PRIVS → sandbox.c 明确记为 ignored → shim 写 8 字节带 tag
// 记录 → supervisor 累加 → Result.Ignored 必须真的带着这一位，且不得误拒。
// 该用例同时证明 fd 3 的加性记录没有破坏"EOF = execve 成功"这条既有语义。
func TestF11SandboxIgnoredRecordPropagates(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("this integration case is measured on macOS; Linux NNP noise bit is covered by the pure decision table")
	}
	p, err := native.Spawn(native.Spec{
		Exe:     "/bin/echo",
		Args:    []string{"echo", "f11-nnp"},
		Sandbox: native.SandboxNoNewPrivs,
	}, f11RealShim(t))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Free()

	res, err := p.Wait()
	if err != nil {
		t.Fatalf("Wait: noncritical ignored bit must NOT be rejected: %v", err)
	}
	if !res.Exited || res.ExitCode != 0 {
		t.Fatalf("expected clean exit, got exited=%v code=%d", res.Exited, res.ExitCode)
	}
	if res.SetupErrno != 0 {
		t.Fatalf("setup_errno = %d, want 0 (EOF = exec success)", res.SetupErrno)
	}
	if !strings.Contains(string(res.Stdout), "f11-nnp") {
		t.Fatalf("stdout = %q: target did not run through the shim", res.Stdout)
	}
	if res.Ignored&native.SandboxNoNewPrivs == 0 {
		t.Fatalf("Result.Ignored = 0x%x, want NO_NEW_PRIVS set: tagged fd-3 record did not reach Go",
			uint32(res.Ignored))
	}
	if res.Ignored&(native.SandboxNoNet|native.SandboxReadonlyTmp) != 0 {
		t.Fatalf("Result.Ignored = 0x%x carries critical bits that were never requested",
			uint32(res.Ignored))
	}
}

// TestF11SandboxZeroCleanNoOp 是 clean no-op 回归：Sandbox=0 不得触发任何
// 拒绝，Result.Ignored 必须为 0；hermes-shim --self-test 退出码必须为 0。
func TestF11SandboxZeroCleanNoOp(t *testing.T) {
	shim := f11RealShim(t)
	p, err := native.Spawn(native.Spec{
		Exe:  "/bin/echo",
		Args: []string{"echo", "f11-zero"},
	}, shim)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Free()

	res, err := p.Wait()
	if err != nil {
		t.Fatalf("Wait: Sandbox=0 must be a clean no-op: %v", err)
	}
	if !res.Exited || res.ExitCode != 0 || res.SetupErrno != 0 {
		t.Fatalf("unclean result: exited=%v code=%d setup_errno=%d",
			res.Exited, res.ExitCode, res.SetupErrno)
	}
	if res.Ignored != 0 {
		t.Fatalf("Result.Ignored = 0x%x, want 0", uint32(res.Ignored))
	}
	out, err := exec.Command(shim, "--self-test").CombinedOutput()
	if err != nil {
		t.Fatalf("hermes-shim --self-test failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("unexpected self-test output: %q", out)
	}
}

// TestF11CriticalIgnoredRejectsBeforeExec 是 POSIX 预执行拒绝的确定性回归（E5）：
// 平台层回报 ignored=NO_NET（关键位）**并且**写了 [HMIF] 与 exit(40)，绝不 execve。
// 断言 ① Wait 产出 policy/denied ② 目标标记文件不存在（进程从未启动）
// ③ 错误文本同时携带 requested=0x1 与 ignored=0x1（证明 [HMIG] 记录没有被
// setup 失败那条分支吞掉）。
//
// ⚠ v4 的 reason 是 setup_failed，不是 v3 的 ignored_critical：§5.1 把
// "setup 阶段自己失败"提到 no_ack / ignored_critical 之前（它能给出更精确的
// 阶段归因）。两者同时为真时先命中前者**不等于**后者丢了信息 —— ignored 位
// 仍从 Detail 里可读，这条断言证明的就是这一点。
func TestF11CriticalIgnoredRejectsBeforeExec(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "target-ran")
	target := f11WriteTarget(t, marker)

	// [HMIG][ignored=NO_NET(1)] + [HMIF][EPERM(1)]，然后 _exit(40)。
	fake := f11FakeShim(t,
		f11bRecordWrite(f11bTagIgnored, 1)+
			f11bRecordWrite(f11bTagFailed, 1)+
			"exit 40\n")

	p, err := native.Spawn(native.Spec{
		Exe:     target,
		Args:    []string{"target"},
		Sandbox: native.SandboxNoNet,
	}, fake)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Free()

	res, err := p.Wait()
	if res != nil {
		t.Fatalf("Wait must return no Result on a fail-closed rejection, got %+v", res)
	}
	e := f11Rejection(t, err)
	if e.Detail["reason"] != "setup_failed" {
		t.Fatalf("reason = %q, want setup_failed (§5.1 判据③先于⑤/⑥)", e.Detail["reason"])
	}
	if !strings.Contains(e.Error(), "sandbox") ||
		!strings.Contains(e.Error(), "requested=0x1") ||
		!strings.Contains(e.Error(), "ignored=0x1") {
		t.Fatalf("rejection lost the structured facts: %v", e)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("target program DID execute (marker exists): pre-exec rejection failed")
	}
}

// TestF11IgnoredRecordPlusExecveFailureKeepsErrno 覆盖"成功握手 + 非关键位
// 忽略 + execve 失败二次写"（E6）：
//   · pump_setup 必须同时保住 ACK、ignored 与 errno；
//   · execve 失败**不得**被算成 setup 失败（否则这条会被 setup_failed 拒掉）；
//   · 请求了关键位（NO_NET）时，ACK 是放行的必要条件 —— 没有 ACK 就是 no_ack。
//
// 这条用例故意请求关键位：只有 critical != 0，判据③/④/⑤ 才会被真正执行，
// 于是"把裸 errno 当 setup 失败"的实现会在这里变红（它会让 setup_failed
// 为真 → 拒绝），而不是在请求非关键位的用例里蒙混过关。
func TestF11IgnoredRecordPlusExecveFailureKeepsErrno(t *testing.T) {
	// [HMIA][applied=NO_NET(1)] + [HMIG][ignored=NO_NEW_PRIVS(4)]
	// + [ENOENT(2)]（裸 4 字节），然后 _exit(41) = EXEC 失败。
	fake := f11FakeShim(t,
		f11bRecordWrite(f11bTagAck, 1)+
			f11bRecordWrite(f11bTagIgnored, 4)+
			"printf '\\002\\000\\000\\000' >&3\n"+
			"exit 41\n")

	p, err := native.Spawn(native.Spec{
		Exe:     "/bin/echo",
		Args:    []string{"echo", "should-never-run"},
		Sandbox: native.SandboxNoNet | native.SandboxNoNewPrivs,
	}, fake)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Free()

	res, err := p.Wait()
	if err != nil {
		t.Fatalf("Wait: execve failure after a complete handshake must NOT be rejected: %v", err)
	}
	if res.Acked&native.SandboxNoNet == 0 {
		t.Fatalf("Result.Acked = 0x%x, want NO_NET: the [HMIA] record was lost",
			uint32(res.Acked))
	}
	if res.SetupFailed {
		t.Fatal("Result.SetupFailed = true, but the shim only reported an execve failure " +
			"(bare errno), not a setup failure")
	}
	// v4 新增：裸 errno 必须被识别成 exec 失败（判据④的依据），而不是丢弃。
	if !res.ExecFailed {
		t.Fatal("Result.ExecFailed = false: the bare 4-byte errno was not classified as an " +
			"execve failure")
	}
	if res.Ignored&native.SandboxNoNewPrivs == 0 {
		t.Fatalf("Result.Ignored = 0x%x, want NO_NEW_PRIVS: tagged record was lost",
			uint32(res.Ignored))
	}
	if res.SetupErrno != 2 {
		t.Fatalf("setup_errno = %d, want 2 (ENOENT): pump_setup swallowed the second write",
			res.SetupErrno)
	}
	if res.ExitCode != 41 {
		t.Fatalf("exit_code = %d, want 41 (HM_SHIM_EXIT_EXEC)", res.ExitCode)
	}
	if strings.Contains(string(res.Stdout), "should-never-run") {
		t.Fatal("target program ran even though execve was reported as failed")
	}
}

// TestF11SetupRecordBackfillsNoNetBit 直接断言 C 侧把 NO_NET 位累加进了
// hm_proc_get_sb_ignored（§4 用例 1 的 "Ignored & HM_SB_NO_NET != 0"）。
// 用"只请求非关键位 NNP、平台却回报 NO_NET|NNP 都被忽略"的组合让 Go 判据
// 不拒绝，从而能观察到 Result.Ignored；这同时再次证明按位交集：
// ignored 里含关键位、但关键位未被请求 → 不误拒。
func TestF11SetupRecordBackfillsNoNetBit(t *testing.T) {
	// [TAG][ignored = NO_NET(1) | NO_NEW_PRIVS(4) = 5]，写完后退出 → 父读到
	// 记录再读到 EOF（setup 成功），setup_errno 保持 0。
	fake := f11FakeShim(t, `printf '\107\111\115\110\005\000\000\000' >&3
exit 0
`)

	p, err := native.Spawn(native.Spec{
		Exe:     "/bin/echo",
		Args:    []string{"echo", "c-backfill"},
		Sandbox: native.SandboxNoNewPrivs,
	}, fake)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Free()

	res, err := p.Wait()
	if err != nil {
		t.Fatalf("Wait: ignored contains a critical bit that was NOT requested; must not reject: %v", err)
	}
	if res.Ignored&native.SandboxNoNet == 0 {
		t.Fatalf("C-side res.ignored = 0x%x does not carry NO_NET: tagged record was not accumulated",
			uint32(res.Ignored))
	}
	if res.Ignored&native.SandboxNoNewPrivs == 0 {
		t.Fatalf("C-side res.ignored = 0x%x does not carry NO_NEW_PRIVS", uint32(res.Ignored))
	}
	if res.SetupErrno != 0 {
		t.Fatalf("setup_errno = %d, want 0 (EOF = setup success)", res.SetupErrno)
	}
}

// TestF11RealShimNoNetNeverFailsOpen 用**真实** hermes-shim + SandboxNoNet
// 跑一次（E7）。F-11b 之后这条用例的口径变了：
//
//   · 拒绝分支：真实 shim 自己写了 [HMIF] 或 [HMIG]，Wait() 依判据产出
//     policy/denied（reason 可能是 ignored_critical 或 setup_failed）。
//   · 成功分支：**必须**看到 Result.Acked 带着 NO_NET。这是正面握手真的
//     从 shim 一路走到 Go 的证据 —— 旧版这里只能断言"没有报告被忽略"，
//     那不足以排除"shim 压根没写任何东西"（E1 的形态）。
//
// 因此本用例现在**不再**只证明"不误报"：成功分支要求 ACK 落地，一旦 shim
// 忘了写 ACK（或 pump_setup 丢了解析），这里就红。
//
// ⚠ **但成功分支在本机不可达（实测）**：本机 `sandbox_init` 恒失败，真实 shim
// 对 flags=1..3、5..7 一律写 `[HMIG][N] + [HMIF][1]` 然后拒绝；唯一被执行到的
// ACK 是 flags=4（仅 NNP）且 payload = `4 & ~4 = 0`，也就是"非零 payload 的
// 真实 ACK"在这台机器上根本不存在。所以：
//   - 下面 `if res.Acked&NoNet == 0` 那段断言在本机永远走不到；
//   - 想验证"shim 忘了写 ACK 会红"，必须在一台 `sandbox_init` 能成功的宿主上跑，
//     或者用 f11bAckShim 那样的替身（本文件里已有别的用例覆盖）。
// 保留断言而不是删掉：它在能施加沙箱的宿主上立刻恢复意义，而删掉就再也没人
// 记得这里本该证明什么。
func TestF11RealShimNoNetNeverFailsOpen(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("measured on macOS only; Linux NO_NET handling is covered by the pure decision table")
	}
	marker := filepath.Join(t.TempDir(), "nonet-ran")
	target := f11WriteTarget(t, marker)

	p, err := native.Spawn(native.Spec{
		Exe:     target,
		Args:    []string{"target"},
		Sandbox: native.SandboxNoNet,
	}, f11RealShim(t))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Free()

	res, err := p.Wait()
	_, markerErr := os.Stat(marker)
	if err != nil {
		// 拒绝路径：真实 shim 的 POSIX 证据。
		e := f11Rejection(t, err)
		if e.Detail["reason"] == "" {
			t.Fatalf("rejection lost its machine-readable reason: %v", e)
		}
		t.Logf("measured: real shim produced a policy/denied rejection (reason=%s); target did not run",
			e.Detail["reason"])
		if markerErr == nil {
			t.Fatal("rejected, yet the target program still executed")
		}
		return
	}
	// 成功分支：只允许「真实 shim 报告了 NO_NET 已施加」。
	if res.Ignored&native.SandboxNoNet != 0 {
		t.Fatalf("NO_NET was reported ignored yet the process was allowed to run (ignored=0x%x)",
			uint32(res.Ignored))
	}
	if res.Acked&native.SandboxNoNet == 0 {
		t.Fatalf("Result.Acked = 0x%x does not carry NO_NET: the shim's [HMIA] handshake "+
			"never reached Go (an uncooperative shim would look exactly like this)",
			uint32(res.Acked))
	}
	if markerErr != nil {
		t.Fatalf("Wait succeeded but target did not run: %v", markerErr)
	}
	t.Logf("measured: real shim applied NO_NET and ACKed it (Ignored=0x%x Acked=0x%x)",
		uint32(res.Ignored), uint32(res.Acked))
}

// f11bShimPrologue 是"跳过所有 flag 直到 --"的那段。执行完之后 $@ 就是
// exec.c 传下来的 [exe, args...]，与 real shim 的收尾形状一致。
const f11bShimPrologue = `while [ "$#" -gt 0 ]; do
  case "$1" in
    --) shift; break ;;
    *)  shift ;;
  esac
done
`

// f11UncooperativeShim 写一个「不合作 shim」：吞掉 --sandbox=N，fd 3
// 一字不写就关掉（= EOF），直接 execve 目标程序。它模拟"新 Go + 新库 +
// 旧/不合作 shim"的混合部署，也就是 F-11 时代唯一能静默放行的那种形态。
func f11UncooperativeShim(t *testing.T) string {
	t.Helper()
	return f11FakeShim(t, f11bShimPrologue+"exec 3>&-\nexec \"$@\"\n")
}

// f11bAckShim 写一个「已握手 shim」：跳过 flag → 写 ACK(关键位) → 关 fd 3 →
// execve 目标。它是 real shim 的正确行为替身，用来证明判据**不会**误拒
// 一个合作的 shim（E2 / E4）。
func f11bAckShim(t *testing.T, acked uint32) string {
	t.Helper()
	return f11FakeShim(t, f11bShimPrologue+
		f11bRecordWrite(f11bTagAck, acked)+
		"exec 3>&-\nexec \"$@\"\n")
}

// TestF11bUncooperativeShimRejected 是 E1：F-11 的静默放行被关闭。
//
// 旧行为（已被本用例反转）：不合作 shim 让 Wait() 静默成功 —— 三条旧判据
// （abi_size / ignored 交集 / exit_code==40）全都不触发，因为通道里根本
// 没有东西可看。新判据把"没有 ACK"当成"没有证据"，于是拒绝。
//
// 关于 marker：它**会**存在，因为不合作 shim 确实 execve 了目标。
// F-11b 能在协议层做到的是"拒绝把这次调用报告成成功"，做不到"阻止一个
// 恶意 shim 执行它想执行的东西" —— 后者是信任边界问题，不是协议问题。
// 这里如实断言 marker 存在，正是为了让"事后判定 ≠ 事前阻止"这件事留在
// 测试里，而不是靠注释里的愿望。
func TestF11bUncooperativeShimRejected(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("POSIX fd-3 channel measured on macOS; Windows rejects any nonzero sandbox_flags in hm_spawn")
	}
	marker := filepath.Join(t.TempDir(), "silent-ran")
	target := f11WriteTarget(t, marker)

	p, err := native.Spawn(native.Spec{
		Exe:     target,
		Args:    []string{"target"},
		Sandbox: native.SandboxNoNet,
	}, f11UncooperativeShim(t))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Free()

	res, err := p.Wait()
	if res != nil {
		t.Fatalf("Wait returned a Result for an uncooperative shim: %+v "+
			"(F-11's silent fail-open is back)", res)
	}
	e := f11Rejection(t, err)
	if e.Detail["reason"] != "no_ack" {
		t.Fatalf("reason = %q, want no_ack: absence of the [HMIA] handshake must be evidence "+
			"of nothing having been applied", e.Detail["reason"])
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		// 只能是"目标跑了"这一种结果：不合作 shim 的语义就是绕开我们。
		t.Fatalf("target did not run (marker missing: %v): a shim that execve's the target "+
			"cannot be prevented from running it, only from being reported as success", statErr)
	}
	t.Logf("measured: uncooperative shim rejected post-hoc with reason=no_ack; " +
		"the target did run (the shim execve'd it), but the call is NOT reported as success")
}

// TestF11bBusinessExit40Allowed 是 E2：业务工具自己 exit(40) 不再被误拒。
//
// 旧判据③把 exit_code == 40 当证据，于是任何恰好返回 40 的正常工具都会被
// 判成"沙箱没关好"。新协议下 setup 失败由 [HMIF] 表达，40 只是一个数字。
func TestF11bBusinessExit40Allowed(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("POSIX fd-3 channel measured on macOS; Windows rejects any nonzero sandbox_flags in hm_spawn")
	}
	marker := filepath.Join(t.TempDir(), "exit40-ran")

	p, err := native.Spawn(native.Spec{
		Exe:     "/bin/sh",
		Args:    []string{"-c", "printf ran > '" + marker + "'; exit 40"},
		Sandbox: native.SandboxNoNet,
	}, f11bAckShim(t, 1))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Free()

	res, err := p.Wait()
	if err != nil {
		t.Fatalf("Wait: a business exit(40) with a complete handshake must be ALLOWED: %v", err)
	}
	if !res.Exited || res.ExitCode != 40 {
		t.Fatalf("exited=%v code=%d, want exited=true code=40", res.Exited, res.ExitCode)
	}
	if res.SetupFailed {
		t.Fatal("Result.SetupFailed = true for a business exit(40): 40 is being used as evidence again")
	}
	if res.Acked&native.SandboxNoNet == 0 {
		t.Fatalf("Result.Acked = 0x%x, want NO_NET", uint32(res.Acked))
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("target marker missing (%v): the tool never ran", statErr)
	}
}

// TestF11bKilledBeforeExecIsToolNotStarted 是 E3 在 v4 的结局判定。
//
// fd 3 通道上"execve 成功"与"execve 之前被 SIGKILL"长得一模一样（都是 EOF），
// 区别只有时序：shim 死的那一刻 fd 3 才 EOF，所以 EOF 时子进程已经不活着了。
//
// v3 把这条判成 policy/denied（"setup_signaled"），代价是 E4（exec 之后被杀）
// 被一起误拒。v4 的切分：
//   · **安全侧**由判据⑤覆盖 —— 施加前被杀 = 没有 ACK = no_ack（见 E3a 那张表行）；
//   · **结局侧**由 ErrToolNotStarted 覆盖 —— 施加后被杀 = 工具没启动。
// 这条用例断言的是结局侧：ErrToolNotStarted，**不是** policy/denied，
// 也不是"成功的空结果"。
func TestF11bKilledBeforeExecIsToolNotStarted(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("POSIX fd-3 channel measured on macOS; Windows rejects any nonzero sandbox_flags in hm_spawn")
	}
	marker := filepath.Join(t.TempDir(), "killed-before-exec")
	target := f11WriteTarget(t, marker)

	// ACK 之后**不关 fd 3**、直接把自己打死：目标永远不会被 execve。
	shim := f11FakeShim(t, f11bShimPrologue+
		f11bRecordWrite(f11bTagAck, 1)+
		"kill -9 $$\n")

	p, err := native.Spawn(native.Spec{
		Exe:     target,
		Args:    []string{"target"},
		Sandbox: native.SandboxNoNet,
	}, shim)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Free()

	res, err := p.Wait()
	if res != nil {
		t.Fatalf("Wait returned a Result although the shim died before execve: %+v "+
			"(that is the \"successful empty result\" F-11 exists to kill)", res)
	}
	if !errors.Is(err, native.ErrToolNotStarted) {
		t.Fatalf("err = %v, want ErrToolNotStarted (D5: 链路没走完的瞬时失败)", err)
	}
	// 严格区分：它**不能**是 policy/denied —— 后者 Retry=None、不该重试，
	// 而"工具没启动"是可重试的。
	var e *errs.Error
	if errors.As(err, &e) {
		t.Fatalf("ErrToolNotStarted was reported as an errs.Error (%s/%s): its Class/Retry "+
			"policy is the opposite of policy/denied and must not be merged", e.Attr, e.Sub)
	}
	if errors.Is(err, native.ErrInvalidArgument) {
		t.Fatalf("ErrToolNotStarted was reported as ErrInvalidArgument: it is not the caller's fault")
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatal("target marker exists: the target ran even though execve never happened")
	}
}

// TestF11bExecThenKilledAllowed 是 E4：execve **之后**才被信号杀死 —— 必须放行。
//
// 这条与 E3 成对：两者都有 ACK、都有 signaled，唯一的差别是 EOF 的时序
// （exec_confirmed）。"无条件 signaled → 拒"的方案会在 E2/E4 变红。
func TestF11bExecThenKilledAllowed(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("POSIX fd-3 channel measured on macOS; Windows rejects any nonzero sandbox_flags in hm_spawn")
	}
	marker := filepath.Join(t.TempDir(), "exec-then-killed")

	p, err := native.Spawn(native.Spec{
		Exe: "/bin/sh",
		Args: []string{"-c",
			"printf ran > '" + marker + "'; sleep 0.2; kill -9 $$"},
		Sandbox: native.SandboxNoNet,
	}, f11bAckShim(t, 1))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Free()

	res, err := p.Wait()
	if err != nil {
		t.Fatalf("Wait: a signal AFTER a confirmed execve must be ALLOWED: %v", err)
	}
	if !res.Signaled {
		t.Fatalf("Signaled = false, want true (the tool killed itself with SIGKILL)")
	}
	if !res.ExecConfirmed {
		t.Fatal("Result.ExecConfirmed = false: EOF timing did not record that the child was " +
			"alive when fd 3 closed")
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("target marker missing (%v): the target never ran, so E4 proves nothing", statErr)
	}
}

// TestF11bAckedShimThatExitsWithoutExecIsAccepted 钉住一个**当前行为**：
// ACK 写满、然后 `exit(0)` 而不 `execve` 的 shim，会被当成"成功的空结果"放行。
//
// 这个用例**不是在说"应该这样"**，它钉的是「违约 shim 在父进程侧被当作成功」
// 这个事实 —— 一条绊线：谁哪天改了这个结局，它会红。
//
// 形态：shim 把关键位全 ACK 了（acked ⊇ requested），然后 **exit(0) 而不
// execve**。此时 Signaled=false、ExecConfirmed=false ⇒ 判据不触发 ⇒
// Wait 返回 nil error + `Stdout=""`。
//
// **为什么这不能、也不该由判据解决。** 一个**诚实**的 shim 在写完 ACK 之后
// 只有两条出路：
//   (i)  execve 成功 —— 目标接管进程；
//   (ii) execve 失败 —— 写裸 errno / [HMIF] 然后 `_exit(41)`（setup 阶段则
//        写 [HMIF] 走 setup_failed）。
// 要出现"ACK 之后干净 exit(0) 且没有 exec"，shim 必须**违反自己的协议**
// （代码缺陷，或直接撒谎）。而那正是信任边界已经涵盖的那一类 ——
// 见 docs/C-ABI.md 的「信任边界：ACK 是 shim 自报的」。判据不负责猜违约者
// 的意图；它负责在自己能看见的信息上不说假话。
//
// **为什么不是"因为我们误报太多所以只好留着 Signaled"。** 判据
// `MEASURED && !ExecConfirmed && Signaled` 宣称的命题是
// 「**被信号杀死**且没确认 exec」⇒ 工具没启动 —— 这句话是**对的**。
// `!ExecConfirmed` 单独存在时混淆了两件事：「shim 在 execve 前就死了」与
// 「工具跑了、很快退出、我们没采样到」，两者父进程侧不可区分，后者还是常态
// （真实 shim 下 /bin/echo hi 跨轮 0/20 ~ 18/20，波动极大，而 Wait 不报错）。
// 加上 Signaled 才把命题收窄到能说准的那一半；剩下的一半是**不可判定**，
// 不该由判据猜。
//
// 根因是**采样**：supervisor.c 每 2ms 跑一次
// pump_setup → 读 setup_eof → waitpid(WNOHANG)。快工具的 execve→退出 落在
// 同一个 tick 里，采样点晚于死亡就确认不了；sh 因为启动慢才 100% 跨得过
// （这条反过来证明机制是采样窗口，不是目标性质）。
//
// 将来若需要把"exec 到底发生没有"当作**功能性事实**（计费/审计），手段是让
// exec 确认从"采样推断"变成"协议事实"（shim 在 execve 前写一条 dedicated
// 记录），已挂在 docs/KNOWN-GAPS.md 的 F-11c 条目下，**不立项** —— 因为对
// 安全判定没有收益（违约 shim 本来就归信任边界）。poll() 边沿唤醒只是缩窗口，
// 不构成确定性，不做。
//
// ⚠ 曾经这个用例是 `t.Skip`，而 skip 是函数第一条语句 ⇒ body 永远不执行 ⇒
// 注释里写的"谁改判据它会红"是**假的**（QA 注入"删掉 Signaled 合取项"验证过：
// 带 Skip 时 SKIP PASS，去掉 Skip 才 FAIL）。现在 body 真跑。
func TestF11bAckedShimThatExitsWithoutExecIsAccepted(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "acked-then-exit")
	target := f11WriteTarget(t, marker)

	// 满 ACK（requested = NO_NET = 1）+ 正常退出，**不 exec**。
	shim := f11FakeShim(t, f11bShimPrologue+
		f11bRecordWrite(f11bTagAck, 1)+
		"exit 0\n")

	p, err := native.Spawn(native.Spec{
		Exe:     target,
		Args:    []string{"target"},
		Sandbox: native.SandboxNoNet,
	}, shim)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Free()

	res, err := p.Wait()
	// 下面这些断言是**绊线**：钉的是"违约 shim 今天被接受"这个事实。
	// 红了不代表"实现坏了"，代表**结局变了** —— 先分诊，别急着改断言。
	if res == nil || err != nil {
		t.Fatalf("this test pins a trust-boundary behaviour, not a desired one: a shim that "+
			"ACKs then exits 0 without execve is accepted as a successful empty result.\n"+
			"If this now FAILS, something changed the outcome for acked-but-never-exec'd "+
			"shims. Two cases, decide which:\n"+
			"  (a) F-11c / the pre-exec record closed it -> update docs/C-ABI.md and "+
			"KNOWN-GAPS F-11c, and change this test to assert REJECTION;\n"+
			"  (b) someone dropped a conjunct from toolNotStarted -> check the false-positive "+
			"rate first (EXEC_CONFIRMED for fast tools swings 0/20..18/20 across rounds for "+
			"the same target, so dropping Signaled makes fast tools report ErrToolNotStarted "+
			"nearly always).\n"+
			"Do not weaken the assertion to make it green.\n"+
			"Got Wait() = (%+v, %v)", res, err)
	}
	for _, g := range []struct {
		name string
		got  bool
		want bool
	}{
		{"Signaled", res.Signaled, false},
		{"ExecConfirmed", res.ExecConfirmed, false},
		{"ExitCode==0", res.ExitCode == 0, true},
	} {
		if g.got != g.want {
			t.Fatalf("%s = %v, want %v: the accepted shape is 'clean exit(0), no exec "+
				"confirmation'. Same triage as above: did the outcome change on purpose, or "+
				"did a conjunct vanish? Do not weaken the assertion to make it green",
				g.name, g.got, g.want)
		}
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatal("target marker exists: the target ran even though execve never happened — " +
			"the fixture is broken, this case proves nothing")
	}
}

// TestF11bCancelBeforeExecIsNotRetryable 钉住 pre-exec 死亡的**两个成因必须
// 分成两个结局**。
//
// 判据命中（MEASURED && !ExecConfirmed && Signaled）说明"工具没启动"，但没说
// **是谁**让它没启动：
//   - 环境杀的（外部信号、shim 自己崩）→ 可重试，上层重跑一次也许就好了；
//   - **本方主动 Cancel** 掉的 → 不可重试。
//
// 后者如果沿用可重试的 ErrToolNotStarted，一个朴素循环
// （父 ctx 取消 → spawn → Cancel → 见 retryable → 重试）会**自旋**。
// 两侧互斥，且可重试类里只有 ErrToolNotStarted —— 所以
// `!errors.Is(err, ErrToolNotStarted)` 就是"不可重试"的可执行表述，
// 不需要第二套 Retry 分类（那会是第二份真源）。
func TestF11bCancelBeforeExecIsNotRetryable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("POSIX fd-3 channel measured on macOS; Windows rejects any nonzero sandbox_flags in hm_spawn")
	}
	marker := filepath.Join(t.TempDir(), "cancel-before-exec")
	target := f11WriteTarget(t, marker)
	// 握手文件：shim 写完 ACK 之后才建它，测试等它出现再 Cancel。
	// 少了这个握手，"ACK 已到达"与"Cancel 抢在 ACK 之前"就成了竞态 ——
	// 抢赢的那种会命中判据⑤ no_ack（=policy/denied），测的就不是这里要测的
	// 那条分支了。等文件出现等于等到"ACK 已经写进管道"，而管道里的字节在
	// 写端关闭后依然可读，所以之后无论 shim 怎么死，ACK 都不会丢。
	ready := filepath.Join(t.TempDir(), "acked")

	// ACK 之后先睡住，把 execve 推后，保证 Cancel 落在 exec 之前。
	shim := f11FakeShim(t, f11bShimPrologue+
		f11bRecordWrite(f11bTagAck, 1)+
		": > '"+ready+"'\n"+
		"sleep 5\nexec \"$@\"\n")

	p, err := native.Spawn(native.Spec{
		Exe:     target,
		Args:    []string{"target"},
		Sandbox: native.SandboxNoNet,
	}, shim)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Free()

	// 等到 shim 确认 ACK 已写出；之后 Cancel 一定落在 exec 之前（还有 sleep 5）。
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, statErr := os.Stat(ready); statErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the shim never reached the post-ACK handshake: this test would silently " +
				"be measuring the no_ack branch instead of the cancelled-before-exec branch")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := p.Cancel(); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	res, err := p.Wait()
	if res != nil {
		t.Fatalf("Wait returned a Result although the tool never started: %+v", res)
	}
	if !errors.Is(err, native.ErrToolCancelled) {
		t.Fatalf("err = %v, want ErrToolCancelled (this spawn was cancelled by us)", err)
	}
	// ← 这一条就是"不可重试"。可重试类里只有 ErrToolNotStarted。
	if errors.Is(err, native.ErrToolNotStarted) {
		t.Fatal("a caller-initiated cancel was reported as ErrToolNotStarted (retryable): " +
			"ctx-cancelled → spawn → cancel → see retryable → retry is a spin loop")
	}
	var e *errs.Error
	if errors.As(err, &e) {
		t.Fatalf("ErrToolCancelled was reported as an errs.Error (%s/%s): a cancellation is not "+
			"a fault attribution", e.Attr, e.Sub)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatal("target marker exists: the target ran even though it was cancelled before execve")
	}
}
