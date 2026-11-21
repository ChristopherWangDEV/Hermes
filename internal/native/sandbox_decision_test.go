package native

// F-11 / F-11b(v4) 的判据单元测试、getter 生命周期测试，以及三条形态守卫
// （G1/G2/G3）。Windows 的 C 路径在本机（darwin）编不到，用「源码结构守卫 +
// 纯函数等价验证」提供证据，如实标注，不冒充真机实测。
//
// v4 的判据变化（v3 → v4）：
//   · **第一条 abi_size 守卫整条消失**。没有结构体承载返回值，就没有
//     "旧库按旧大小写"这件事；"新 Go + 旧库"变成**链接期** undefined
//     reference。删掉它（连同 TestF11ABISizeGuardIsFailClosed）是**设计
//     收益**，不是覆盖缺口 —— 见下面 TestF11ABISizeGuardIsGoneByDesign。
//   · 新增判据②：sbRequested != requested（native 层把请求丢掉/改写）。
//   · 新增判据④：setup exec_failed ⇒ **放行**（目标从未启动，没有"未沙箱
//     执行"的可能），归因走 Result.SetupErrno。
//   · v3 的判据⑤ "signaled && !execConfirmed ⇒ 拒绝"**删除**。它会把 E4
//     （exec 之后被杀）误拒。E3 的安全侧由判据⑤（缺 ACK）覆盖，结局侧由
//     ErrToolNotStarted 覆盖 —— 两者职责不同，不能合并。
//   · setup_failed 提到 no_ack / ignored_critical 之前：它能给出更精确的
//     阶段归因（v3 的顺序是 historical，v4 以 §5.1 为准）。

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"hermes/internal/errs"
)

// f11RepoRoot 从 internal/native 往上两级得到仓库根。
func f11RepoRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	return abs
}

func f11Read(t *testing.T, parts ...string) string {
	t.Helper()
	p := filepath.Join(append([]string{f11RepoRoot(t)}, parts...)...)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

// f11AssertPolicyDenied 是"这条拒绝被正确分类"的唯一断言入口。
// 它同时排除两种误分类：diag.Diag（冻结区的编译期诊断）与
// asRunError("NetworkError")（可重试的网络故障）。
func f11AssertPolicyDenied(t *testing.T, err error) *errs.Error {
	t.Helper()
	if err == nil {
		t.Fatal("expected a fail-closed error, got nil")
	}
	var e *errs.Error
	if !errors.As(err, &e) {
		t.Fatalf("error is %T, want *errs.Error (not diag.Diag, not a NetworkError wrapper): %v", err, err)
	}
	if e.Attr != errs.AttrPolicy || e.Sub != errs.SubDenied {
		t.Fatalf("attr/sub = %s/%s, want policy/denied", e.Attr, e.Sub)
	}
	if e.Class() != errs.ClassPolicy {
		t.Fatalf("Class() = %s, want %s", e.Class(), errs.ClassPolicy)
	}
	if e.Retry() != errs.RetryNone {
		t.Fatalf("Retry() = %s, want %s", e.Retry(), errs.RetryNone)
	}
	if e.CountsAsBreakerFailure() {
		t.Fatal("policy denial must not count as a breaker failure")
	}
	if e.Key() != "policy/denied" {
		t.Fatalf("Key() = %q, want policy/denied", e.Key())
	}
	if strings.Contains(e.Error(), "NetworkError") {
		t.Fatalf("policy denial was misclassified as a network error: %v", err)
	}
	return e
}

// TestF11SandboxDecisionTable 覆盖 v4 判据的全部边界。
//
// 每一行的 wantReason 就是 §5.1 里那几条规则的名字 —— 断言 reason 而不是
// 只断言"是否拒绝"，是因为**顺序**本身是判据的一部分：同一组输入在两种
// 顺序下会给出不同 reason，而 reason 是 trace 里唯一的定位手段。
func TestF11SandboxDecisionTable(t *testing.T) {
	crit := SandboxNoNet
	const measured = setupChannelMeasured

	cases := []struct {
		name       string
		state      int32
		sbReq      SandboxFlags
		sbAck      SandboxFlags
		sbIgn      SandboxFlags
		requested  SandboxFlags
		bits       uint32
		errno      int
		wantReason string // "" = 放行
		wantArg    bool   // true = 期望 ErrInvalidArgument（程序错误，不是策略拒绝）
	}{
		{
			name: "1 non-sandbox clean",
			// 没请求任何位：没有可被违反的承诺。
			state: procReaped, requested: 0,
		},
		{
			name:  "2 critical with full ACK",
			state: procReaped, sbReq: crit, sbAck: crit, requested: crit,
			bits: measured,
		},
		{
			name: "3 NNP noise ignored, no critical requested",
			// Linux 在未请求 NNP 时反而记 ignored；算进关键位会误拒全部正常调用。
			state: procReaped, sbReq: SandboxNoNewPrivs, sbIgn: SandboxNoNewPrivs,
			requested: SandboxNoNewPrivs,
		},
		{
			name: "4 E2 business exit 40 is ALLOWED (exit code is no longer an input)",
			// v4 的判据签名里根本没有 exitCode —— 这就是 E2 的修复本身：
			// 业务的合法退出码不再有任何机会被当成"沙箱没关好"。
			state: procReaped, sbReq: crit, sbAck: crit, requested: crit,
			bits: measured,
		},
		{
			name: "5 setup failed is rejected as setup_failed",
			state: procReaped, sbReq: crit, requested: crit,
			bits: measured | setupFailed, errno: 1,
			wantReason: "setup_failed",
		},
		{
			name: "6 E1 no ACK at all is rejected as no_ack",
			state: procReaped, sbReq: crit, requested: crit,
			bits: measured,
			wantReason: "no_ack",
		},
		{
			name: "7 E3 no ACK before being killed is rejected as no_ack (安全侧)",
			// shim 在施加沙箱之前就被 SIGKILL：没有 ACK = 没有证据 = 拒绝。
			state: procReaped, sbReq: crit, requested: crit,
			bits: measured,
			wantReason: "no_ack",
		},
		{
			name: "8 E4 ACK present, killed after exec is ALLOWED here",
			// 结局侧（是否 exec 成功）由 ErrToolNotStarted 判，不在安全判据里。
			state: procReaped, sbReq: crit, sbAck: crit, requested: crit,
			bits: measured, // 注意：**不带** EXEC_CONFIRMED 也放行
		},
		{
			name: "9 execve failed is ALLOWED (target never started)",
			state: procReaped, sbReq: crit, requested: crit,
			bits: measured | setupExecFailed, errno: 2,
		},
		{
			name: "10 setup failed wins over exec failed (order matters)",
			state: procReaped, sbReq: crit, requested: crit,
			bits: measured | setupFailed | setupExecFailed, errno: 1,
			wantReason: "setup_failed",
		},
		{
			name: "11 setup failed wins over no_ack (order matters)",
			state: procReaped, sbReq: crit, requested: crit,
			bits: measured | setupFailed, errno: 1,
			wantReason: "setup_failed",
		},
		{
			name: "12 setup failed wins over ignored_critical (order matters)",
			state: procReaped, sbReq: crit, sbIgn: crit, requested: crit,
			bits: measured | setupFailed, errno: 1,
			wantReason: "setup_failed",
		},
		{
			name: "13 ignored critical is rejected when there is no other signal",
			state: procReaped, sbReq: crit, sbAck: crit, sbIgn: crit, requested: crit,
			bits: measured,
			wantReason: "ignored_critical",
		},
		{
			name: "14 native layer dropped the request (E1 variant)",
			// exec.c 没把 sandbox_flags 传下去：requested=NO_NET 但 C 侧回读 0。
			state: procReaped, sbReq: 0, sbAck: crit, requested: crit,
			bits: measured,
			wantReason: "requested",
		},
		{
			name: "15 state RUNNING is an argument error, not a policy denial",
			state: procRunning, sbReq: crit, sbAck: crit, requested: crit,
			bits:     measured,
			wantArg:  true,
		},
		{
			name: "16 arbitrary setupErrno never decides",
			state: procReaped, sbReq: 0, requested: 0,
			bits: 0, errno: 999,
		},
		{
			name: "17 arbitrary setupErrno with full ACK still allowed",
			state: procReaped, sbReq: crit, sbAck: crit, requested: crit,
			bits: measured, errno: 999,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := sandboxDecision(tc.state, tc.sbReq, tc.sbAck, tc.sbIgn,
				tc.requested, tc.bits, tc.errno)
			if tc.wantArg {
				if !errors.Is(err, ErrInvalidArgument) {
					t.Fatalf("err = %v, want ErrInvalidArgument (程序错误不该被归因成安全事件)", err)
				}
				var e *errs.Error
				if errors.As(err, &e) {
					t.Fatalf("argument error was wrapped as an errs.Error (%s/%s); it must not be policy/denied", e.Attr, e.Sub)
				}
				return
			}
			if tc.wantReason == "" {
				if err != nil {
					t.Fatalf("unexpected fail-closed rejection: %v", err)
				}
				return
			}
			e := f11AssertPolicyDenied(t, err)
			if e.Detail["reason"] != tc.wantReason {
				t.Fatalf("reason = %q, want %q (full error: %v)",
					e.Detail["reason"], tc.wantReason, e)
			}
			if !strings.Contains(e.Error(), "reason="+tc.wantReason) {
				t.Fatalf("error text does not name the reason %q: %v", tc.wantReason, e)
			}
		})
	}
}

// TestF11ABISizeGuardIsGoneByDesign 记录一个**被删除的**守卫及其理由。
//
// 旧测试 TestF11ABISizeGuardIsFailClosed 断言"库侧 abi_size < Go 侧
// sizeof(hm_spawn_res) ⇒ 硬拒"，它守的是"新消费者按旧库的大小读，尾部字段
// 保持零值被当成真值"这个失效模式。v4 之后：
//   - 没有包大小可协商，没有 min-size memcpy；
//   - 结果只经 getter 从槽位读，getter 是**符号**；
//   - 旧库没有这些符号 ⇒ `go build` 直接 undefined reference ⇒
//     **链接期**硬失败，比运行期守卫更早、更响。
//
// 所以这条判据的消失是**设计收益**，不是覆盖缺口。这里保留一条"它真的不在
// 了"的断言，防止有人以为漏写而把它加回来（加了就说明返回值又被结构体承载）。
func TestF11ABISizeGuardIsGoneByDesign(t *testing.T) {
	src := f11Read(t, "internal", "native", "exec.go")
	body := f11FuncBody(t, src, "func sandboxDecision(")
	for _, banned := range []string{"abiSize", "abi_size", "hmSpawnResSize"} {
		if strings.Contains(body, banned) {
			t.Fatalf("sandboxDecision references %q again: the result面 must not carry a size "+
				"(v4 deleted that whole class of judgement)", banned)
		}
	}
	// 头文件里"hm_spawn_res"只允许出现在 R8 2b 的**注释**里（逐字列名是那条
	// 例外的要求），代码里一个都不许有。所以先剥注释再查。
	code := f11StripCComments(f11Read(t, "native", "include", "hermes_abi.h"))
	for _, banned := range []string{"hm_spawn_res", "hm_setup_status", "hm_spawn_wait", "hm_spawn_res_free"} {
		if strings.Contains(code, banned) {
			t.Fatalf("hermes_abi.h still declares %q: the fixed-size result struct is back", banned)
		}
	}
}

// TestF11SandboxFailClosedClassification 验证映射表那一行确实生效，
// 且错误文本携带结构化事实（reason / requested / acked / ignored）。
func TestF11SandboxFailClosedClassification(t *testing.T) {
	r, ok := errs.Lookup(errs.AttrPolicy, errs.SubDenied, 0)
	if !ok {
		t.Fatal("Lookup(policy, denied, 0) missed; table row is gone")
	}
	if r.Class != errs.ClassPolicy || r.Retry != errs.RetryNone || r.CountsAsFailure {
		t.Fatalf("policy/denied row = class=%s retry=%s counts=%v, want policy/none/false",
			r.Class, r.Retry, r.CountsAsFailure)
	}
	e := f11AssertPolicyDenied(t,
		sandboxFailClosed("native.Wait", "no_ack", SandboxNoNet, 0, SandboxNoNet))
	for _, want := range []string{"reason=no_ack", "requested=0x1", "acked=0x0", "ignored=0x1"} {
		if !strings.Contains(e.Error(), want) {
			t.Fatalf("error text lost %q: %v", want, e)
		}
	}
	// Detail 是机读的那一份，必须与文本一致（trace 与指标从这里取）。
	if e.Detail["reason"] != "no_ack" ||
		e.Detail["requested"] != "0x1" || e.Detail["acked"] != "0x0" ||
		e.Detail["ignored"] != "0x1" {
		t.Fatalf("structured details diverged from the text: %v", e.Detail)
	}
}

// TestF11SpawnFailureMapsSandboxDenied 断言 Windows(Spawn 产出) 与
// POSIX(Wait 产出) 两条拒绝路径对外是同一个 *errs.Error 形状。
func TestF11SpawnFailureMapsSandboxDenied(t *testing.T) {
	// Windows：hm_spawn 在 CreateProcessA 之前返回 HM_E_SANDBOX_DENIED(-12)。
	winErr := spawnFailure(Status(ErrSandboxDenied), SandboxNoNet)
	win := f11AssertPolicyDenied(t, winErr)

	// 其它原生错误必须原样透传，不能被改写成 policy。
	other := Status(ErrSpawn)
	if got := spawnFailure(other, SandboxNoNet); got != other {
		t.Fatalf("non-sandbox status was rewritten: got %v, want %v", got, other)
	}
	if got := spawnFailure(nil, SandboxNoNet); got != nil {
		t.Fatalf("nil error was rewritten: %v", got)
	}

	// POSIX：Wait() 依据 sbIgnored **或**握手证据兜底产出。两条 POSIX reason
	// 必须与 Windows 那条映射到同一个 Attr/Sub，否则调用方会看到两种"沙箱拒绝"。
	for _, posixErr := range []error{
		sandboxDecision(procReaped, SandboxNoNet, 0, SandboxNoNet, SandboxNoNet,
			setupChannelMeasured, 0),
		sandboxDecision(procReaped, SandboxNoNet, 0, 0, SandboxNoNet,
			setupChannelMeasured, 0),
	} {
		posix := f11AssertPolicyDenied(t, posixErr)
		if win.Attr != posix.Attr || win.Sub != posix.Sub {
			t.Fatalf("two rejection paths diverged: %s/%s vs %s/%s",
				win.Attr, win.Sub, posix.Attr, posix.Sub)
		}
		if !errors.Is(winErr, posix) {
			t.Fatal("errors.Is(winErr, posixErr) must hold: callers must see one error identity")
		}
	}
}

// f11WinGuardExpr 从 win_spawn.c 的**真实源码**里抽出 guard 的条件表达式
// 及其在 hm_spawn 内的偏移。
func f11WinGuardExpr(t *testing.T, src string) (expr string, guardOffset, fnOffset int) {
	t.Helper()
	fn := strings.Index(src, "hm_status hm_spawn(")
	if fn < 0 {
		t.Fatal("cannot locate hm_spawn in win_spawn.c")
	}
	// 只取 hm_spawn 函数体，避免把后面的函数（hm_proc_wait 等）误认成 guard。
	body := src[fn:]
	if cut := strings.Index(body, "\nhm_proc_cancel("); cut >= 0 {
		body = body[:cut]
	}
	// guard 是带花括号的 `if (...) { return HM_E_SANDBOX_DENIED; }`。两种
	// 排版都要能识别，但捕获组始终是**条件表达式本身**。
	re := regexp.MustCompile(`if\s*\(([^)]*)\)\s*(?:\{\s*)?return\s+HM_E_SANDBOX_DENIED\s*;`)
	m := re.FindStringSubmatchIndex(body)
	if m == nil {
		t.Fatal("win_spawn.c has no `if (...) return HM_E_SANDBOX_DENIED;` guard in hm_spawn (braced or single-line)")
	}
	expr = strings.TrimSpace(body[m[2]:m[3]])
	if expr == "" {
		t.Fatal("sandbox guard has an empty condition")
	}
	return expr, fn + m[0], fn
}

// f11GuardNotDisabled 逐行扫描 guard 之前的预处理指令，维护一个
// "#if 0 嵌套深度"：只有 guard 处于未闭合的 #if 0 块内才判死代码。
func f11GuardNotDisabled(t *testing.T, src string, guardOffset int) {
	t.Helper()
	dead := 0
	for _, line := range strings.Split(src[:guardOffset], "\n") {
		s := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(s, "#if 0"), strings.HasPrefix(s, "#if 0 "):
			dead++
		case strings.HasPrefix(s, "#endif"):
			if dead > 0 {
				dead--
			}
		}
	}
	if dead > 0 {
		t.Fatal("sandbox guard is inside an unclosed #if 0 block (dead code)")
	}
}

// TestF11WindowsSilentPassThroughCannotReachWait 钉死 P0 漏洞：
// Windows 仍然必须在 CreateProcessA 之前硬拒任何非零请求位 —— 即使 v4 的
// Wait 级判据也会拒（no_ack），"拒得太晚"依然不可接受：那时目标进程已经
// 开始跑，而 Windows 侧 Job Object 的沙箱位根本没有实现。
func TestF11WindowsSilentPassThroughCannotReachWait(t *testing.T) {
	if err := sandboxDecision(procReaped, SandboxNoNet, 0, 0, SandboxNoNet,
		setupChannelMeasured, 0); err == nil {
		t.Fatal("Wait-level judgement must reject a critical request with no handshake " +
			"(v4 no_ack); a clean verdict here means the handshake was silently dropped")
	} else if e, ok := err.(*errs.Error); !ok || e.Detail["reason"] != "no_ack" {
		t.Fatalf("Wait-level rejection reason = %v, want no_ack (E1 shape)", err)
	}
	src := f11Read(t, "native", "c", "win_spawn.c")
	expr, guard, fn := f11WinGuardExpr(t, src)

	// ① 条件表达式必须真的读 req->sandbox_flags、判非零，且非恒假。
	if !strings.Contains(expr, "req->sandbox_flags") {
		t.Fatalf("guard condition %q does not read req->sandbox_flags", expr)
	}
	if !regexp.MustCompile(`sandbox_flags\s*!=\s*0`).MatchString(expr) {
		t.Fatalf("guard condition %q does not reject ANY nonzero bit", expr)
	}
	if regexp.MustCompile(`^\s*(?:0|false|FALSE|!1|NULL)\s*$`).MatchString(expr) {
		t.Fatalf("guard condition %q is constant-false", expr)
	}
	// 条件里出现按位掩码就是「只拒关键位」的弱方案。
	if strings.Contains(expr, "&") || strings.Contains(expr, "CRITICAL") {
		t.Fatalf("guard condition %q must reject ANY nonzero bit, not a masked subset", expr)
	}
	// guard **不得**被「字段存在性」条件化（abi_size 不覆盖该字段时 guard
	// 被跳过 = 结构性 fail-open）。字段存在性必须是 guard 之前独立 return。
	for _, banned := range []string{"offsetof", "abi_size", "req_has", "&&", "||"} {
		if strings.Contains(expr, banned) {
			t.Fatalf("guard condition %q is conditioned on field presence (%q): "+
				"the abi_size check must be a separate return BEFORE the guard", expr, banned)
		}
	}

	// ② guard 不得被 #if 0 包住。
	f11GuardNotDisabled(t, src, guard)

	// ③ guard 必须早于 hm_spawn 内任何进程创建调用。
	for _, tok := range []string{
		"build_cmdline(req->argv)", "CreatePipe(&out_rd", "CreateProcessA(",
		"CreateJobObjectA(", "ResumeThread(",
	} {
		if i := strings.Index(src[fn:], tok); i >= 0 && guard > fn+i {
			t.Fatalf("sandbox guard comes AFTER %q", tok)
		}
	}
	f11AssertPolicyDenied(t, spawnFailure(Status(ErrSandboxDenied), SandboxNoNet))
}

// TestF11WindowsSpawnRejectsBeforeProcessCreation 是 P0 的静态守卫：
// 断言拒绝分支存在于 hm_spawn 内、早于所有进程创建调用，三道闸顺序正确，
// 且 getter 侧没有任何"事后回填"的弱方案。
//
// v4 删除了原先对 hm_spawn_wait 函数体的文本扫描（那个函数不存在了）：
// 它的三条断言（tmp + min-size memcpy / nbytes 夹紧 / 不得直写 out->）
// 守的都是"结果结构体跨边界搬运"的正确性。v4 没有 out 参数、没有 memcpy、
// 没有 nbytes —— 那三条断言守的对象已被形态消灭，连同失效模式一起消失。
func TestF11WindowsSpawnRejectsBeforeProcessCreation(t *testing.T) {
	src := f11Read(t, "native", "c", "win_spawn.c")
	_, gi, fn := f11WinGuardExpr(t, src)
	for _, tok := range []string{
		"build_cmdline(req->argv)", "CreatePipe(&out_rd", "CreateProcessA(",
		"CreateJobObjectA(", "ResumeThread(",
	} {
		i := strings.Index(src[fn:], tok)
		if i < 0 {
			t.Fatalf("cannot locate %q in hm_spawn", tok)
		}
		if gi > fn+i {
			t.Fatalf("sandbox guard comes AFTER %q: process creation would already be reachable", tok)
		}
	}

	// 三步形状：既有 80 阈值版本闸 → 独立的字段存在性检查（offsetof+sizeof）
	// → 无条件 guard。`win_spawn.c:139` 的版本闸**本轮不动**（§附 N1）。
	abiCheck := strings.Index(src, "req->abi_size < sizeof(hm_spawn_req) - 64")
	fieldCheck := strings.Index(src, "offsetof(hm_spawn_req, sandbox_flags)")
	if abiCheck < 0 || fieldCheck < 0 {
		t.Fatal("win_spawn.c lost the abi_size gate or the offsetof field check")
	}
	if abiCheck > fieldCheck || fieldCheck > gi {
		t.Fatalf("required order: 80-threshold gate -> offsetof field check -> guard (got %d/%d/%d)",
			abiCheck, fieldCheck, gi)
	}
	if !regexp.MustCompile(`(?s)req->abi_size\s*<\s*\n?\s*offsetof\(hm_spawn_req,\s*sandbox_flags\)\s*\+\s*sizeof\(req->sandbox_flags\)\s*\)\s*\{\s*\n?\s*return\s+HM_E_ABI_VERSION;`).MatchString(src) {
		t.Fatal("the offsetof field check must be a standalone `return HM_E_ABI_VERSION` block, " +
			"not a fallthrough or part of the guard condition")
	}

	// getter 侧：hb_ignored 必须**恒 0**（不是"事后回填 requested & CRITICAL"）。
	getter := f11FuncBody(t, src, "hm_status hm_proc_get_sb_ignored(hm_handle proc, uint32_t *out_flags) {")
	if !regexp.MustCompile(`(?m)^\s*\*out_flags = 0;`).MatchString(getter) {
		t.Fatal("hm_proc_get_sb_ignored must write a constant 0: the Windows path never " +
			"applies a sandbox bit, so any computed value would be a fabricated claim")
	}
	// 也排除"用 requested 位反推"的弱方案。
	for _, banned := range []string{"sb_requested", "CRITICAL", "& my"} {
		if strings.Contains(getter, banned) {
			t.Fatalf("hm_proc_get_sb_ignored references %q: the ignored bitmask must be a "+
				"constant 0 on Windows, not derived from the request", banned)
		}
	}
}

// TestF11CriticalMaskMatchesCHeader 钉死跨语言一致性：Go 的
// criticalSandboxFlags 与 C 的 HM_SB_CRITICAL_MASK 是两份手写常量，
// 没有编译期关联，任一处漂移都会让 shim 与 Wait() 对「关键位」的判断分叉。
func TestF11CriticalMaskMatchesCHeader(t *testing.T) {
	abi := f11Read(t, "native", "include", "hermes_abi.h")
	internal := f11Read(t, "native", "include", "hm_internal.h")

	bitDef := regexp.MustCompile(`(?m)^#define\s+HM_SB_(\w+)\s+\(1u\s*<<\s*(\d+)\)`)
	shift := map[string]uint{}
	for _, m := range bitDef.FindAllStringSubmatch(abi, -1) {
		n, err := strconv.ParseUint(m[2], 10, 32)
		if err != nil {
			t.Fatalf("bad shift in HM_SB_%s: %v", m[1], err)
		}
		shift[m[1]] = uint(n)
	}
	for _, name := range []string{"NO_NET", "READONLY_TMP", "NO_NEW_PRIVS"} {
		if _, ok := shift[name]; !ok {
			t.Fatalf("hermes_abi.h is missing HM_SB_%s bit definition", name)
		}
	}

	goBits := []struct {
		name string
		val  SandboxFlags
	}{
		{"NO_NET", SandboxNoNet},
		{"READONLY_TMP", SandboxReadonlyTmp},
		{"NO_NEW_PRIVS", SandboxNoNewPrivs},
	}
	for _, g := range goBits {
		if want := SandboxFlags(1) << shift[g.name]; g.val != want {
			t.Fatalf("Go %s = 0x%x, C HM_SB_%s = 0x%x", g.name, uint32(g.val), g.name, uint32(want))
		}
	}

	maskDef := regexp.MustCompile(`(?m)^#define\s+HM_SB_CRITICAL_MASK\s+\(([^)]*)\)`)
	mm := maskDef.FindStringSubmatch(internal)
	if mm == nil {
		t.Fatal("hm_internal.h is missing HM_SB_CRITICAL_MASK")
	}
	var cMask uint32
	ids := regexp.MustCompile(`HM_SB_(\w+)`).FindAllStringSubmatch(mm[1], -1)
	if len(ids) == 0 {
		t.Fatalf("HM_SB_CRITICAL_MASK %q references no HM_SB_* bits", mm[1])
	}
	for _, id := range ids {
		s, ok := shift[id[1]]
		if !ok {
			t.Fatalf("HM_SB_CRITICAL_MASK references undefined bit HM_SB_%s", id[1])
		}
		cMask |= uint32(1) << s
	}
	if cMask != uint32(criticalSandboxFlags) {
		t.Fatalf("C HM_SB_CRITICAL_MASK = 0x%x but Go criticalSandboxFlags = 0x%x: the two hand-written constants diverged",
			cMask, uint32(criticalSandboxFlags))
	}
	if criticalSandboxFlags&SandboxNoNewPrivs != 0 {
		t.Fatal("SandboxNoNewPrivs must NOT be in criticalSandboxFlags (Linux NNP noise bit)")
	}
	if cMask&uint32(SandboxNoNewPrivs) != 0 {
		t.Fatal("HM_SB_NO_NEW_PRIVS must NOT be in HM_SB_CRITICAL_MASK")
	}
}

// TestF11ShimRejectsBeforeExecve 是 POSIX 预执行拒绝的静态守卫：
// 断言带 tag 记录、按位交集、以及"绝不 execve"的拒绝分支都在 execve 之前，
// 且整个逻辑包在 if (sandbox_flags != 0) 里（flags==0 不得触发）。
func TestF11ShimRejectsBeforeExecve(t *testing.T) {
	src := f11Read(t, "native", "c", "shim.c")
	block := strings.Index(src, "if (sandbox_flags != 0) {")
	// 锚定真正的调用点：文件头注释里也有 "execve(...)"，别拿它当位置。
	execIdx := strings.Index(src, "execve(argv[exe_index], child_argv, environ);")
	if block < 0 || execIdx < 0 {
		t.Fatalf("shim.c shape changed (block=%d execve=%d)", block, execIdx)
	}
	rej := strings.Index(src[block:], "report_and_exit(EPERM, HM_SHIM_EXIT_SETUP);")
	if rej < 0 {
		t.Fatal("shim.c lost its pre-exec rejection")
	}
	rej += block
	if rej > execIdx {
		t.Fatal("pre-exec rejection comes after execve: target would already run")
	}
	for _, tok := range []string{
		"HM_SHIM_SETUP_TAG_IGNORED", "shim_report_ignored(ignored_flags)",
		"HM_SB_CRITICAL_MASK",
	} {
		if !strings.Contains(src, tok) {
			t.Fatalf("shim.c is missing %q", tok)
		}
	}
	if !regexp.MustCompile(`ignored_flags\s*&\s*\(uint32_t\)sandbox_flags\s*&\s*HM_SB_CRITICAL_MASK`).MatchString(src) {
		t.Fatal("shim.c must use the bitwise intersection, not ignored_flags != 0")
	}
	if !strings.Contains(src, "hermes_sandbox_apply(0, &ignored)") {
		t.Fatal("shim --self-test clean no-op assertion was removed")
	}

	// ---- F-11b：ACK 必须写在"失败分支之后、execve 之前" -------------------
	ackIdx := strings.Index(src, "shim_report_acked((uint32_t)sandbox_flags & ~ignored_flags);")
	if ackIdx < 0 {
		t.Fatal("shim.c never reports the [HMIA] ACK: the handshake would always be absent")
	}
	if ackIdx < rej {
		t.Fatal("shim.c writes ACK BEFORE the fail-closed rejection: a rejected setup would " +
			"still claim to have applied the sandbox")
	}
	if ackIdx > execIdx {
		t.Fatal("shim.c writes ACK after execve: unreachable, the handshake would never happen")
	}
	if !strings.Contains(src, "& ~ignored_flags)") {
		t.Fatal("ACK payload must be sandbox_flags & ~ignored_flags (actually applied bits)")
	}

	// setup 失败必须写带 tag 的 [HMIF]，execve 失败必须写**裸** errno。
	execReport := f11FuncBody(t, src, "static void report_exec_and_exit(int err, int code) {")
	if strings.Contains(execReport, "HM_SHIM_SETUP_TAG_FAILED") {
		t.Fatal("report_exec_and_exit writes a [HMIF] tag: execve failure would be misreported " +
			"as a setup failure")
	}
	if !strings.Contains(execReport, "shim_write_setup_record(payload, sizeof(payload));") {
		t.Fatal("report_exec_and_exit must write the bare 4-byte errno record")
	}
	setupReport := f11FuncBody(t, src, "static void report_and_exit(int err, int code) {")
	if !strings.Contains(setupReport, "HM_SHIM_SETUP_TAG_FAILED") {
		t.Fatal("report_and_exit must write the tagged [HMIF] record")
	}
	if n := strings.Count(src, "report_and_exit(EINVAL, HM_SHIM_EXIT_SETUP);"); n < 8 {
		t.Fatalf("shim.c has %d report_and_exit(EINVAL, ...) sites, want >= 8 "+
			"(7 parse_u64 + unknown option + missing --); plain `return HM_SHIM_EXIT_SETUP` "+
			"would make a setup failure indistinguishable from a business exit(40)", n)
	}
	if strings.Contains(src, "return HM_SHIM_EXIT_SETUP;") {
		t.Fatal("shim.c still returns HM_SHIM_EXIT_SETUP without writing a [HMIF] record")
	}
}

// TestF11bMacNoCriticalBitsIsNoop 钉住 macOS 上的一条反误拒守卫：
// 只请求非关键位（NO_NEW_PRIVS）时，sandbox profile 只是 "(allow default)"，
// 安全上等价于什么都没做。如果这时还去调 sandbox_init，平台自身的失败
// （本机实测 EPERM）就会被 shim 写成 [HMIF] → setup_failed → 在 execve 之前
// 拒绝一个本该"记为 ignored 后照常执行"的调用（TestF11SandboxIgnoredRecordPropagates
// 因此确定性变红）。sandbox.c 必须在 sandbox_init 之前短路返回 HM_OK，
// out_ignored 仍诚实带回被忽略的位；关键位存在时短路不得生效。
//
// 这是源码结构守卫（Windows/Linux 分支本机跑不到），行为侧由上面那条
// 真实 shim 的 macOS 集成用例覆盖——两边一起才构成完整证据。
func TestF11bMacNoCriticalBitsIsNoop(t *testing.T) {
	src := f11Read(t, "native", "c", "sandbox.c")
	apple := strings.Index(src, "#elif defined(__APPLE__)")
	if apple < 0 {
		t.Fatal("sandbox.c lost its __APPLE__ branch")
	}
	init := strings.Index(src[apple:], "rc = sandbox_init(profile, 0, &errbuf);")
	if init < 0 {
		t.Fatal("sandbox.c __APPLE__ branch no longer calls sandbox_init")
	}
	init += apple
	branch := src[apple:init]
	// 短路条件必须检查"没有任何可施加的关键位仍留在请求里"。
	re := regexp.MustCompile(`flags\s*&\s*\(\s*HM_SB_NO_NET\s*\|\s*HM_SB_READONLY_TMP\s*\)\s*\)?\s*==\s*0`)
	if !re.MatchString(branch) {
		t.Fatal("sandbox.c must short-circuit when no critical bit is requested: " +
			"requesting only NO_NEW_PRIVS must not call sandbox_init on an allow-default profile")
	}
	if !strings.Contains(branch, "return HM_OK;") {
		t.Fatal("the no-critical-bits short circuit must return HM_OK (out_ignored carries the truth)")
	}
	if !strings.Contains(branch, "*out_ignored = ignored;") {
		t.Fatal("the short circuit must still report ignored bits honestly")
	}
	// 关键位的施加路径不能被短路吃掉：profile 里还得有 deny 规则。
	if !strings.Contains(branch, "(deny network*)") ||
		!strings.Contains(branch, "(deny file-write*") {
		t.Fatal("sandbox.c lost the critical-bit deny rules: a requested NO_NET/" +
			"READONLY_TMP must still reach sandbox_init and fail closed")
	}
}

// TestF11HeaderAndConstantsStayHonest 钉住"不许偷偷改"的事：
// MAJOR/MINOR 不动、无悬空文档引用、Go 侧 hmShimExitSetup 与头文件逐字一致、
// 以及 F-11b 的 tag / 槽位字段都在。
func TestF11HeaderAndConstantsStayHonest(t *testing.T) {
	hdr := f11Read(t, "native", "include", "hermes_abi.h")
	if !strings.Contains(hdr, "#define HM_ABI_MAJOR 2") ||
		!strings.Contains(hdr, "#define HM_ABI_MINOR 0") {
		t.Fatal("HM_ABI_MAJOR/MINOR must stay 2/0")
	}
	if strings.Contains(hdr, "docs/ABI.md") {
		t.Fatal("dangling docs/ABI.md reference still present")
	}
	// 输入面一字不动：hm_spawn_req 仍以 abi_size 开头（剥注释后看代码）。
	if !regexp.MustCompile(`(?s)typedef struct hm_spawn_req \{\s*uint32_t abi_size;`).MatchString(
		f11StripCComments(hdr)) {
		t.Fatal("hm_spawn_req must keep abi_size as its first field (input side untouched)")
	}

	internal := f11Read(t, "native", "include", "hm_internal.h")
	sm := regexp.MustCompile(`#define HM_SHIM_EXIT_SETUP\s+(\d+)`).FindStringSubmatch(internal)
	if sm == nil {
		t.Fatal("cannot locate HM_SHIM_EXIT_SETUP")
	}
	n, err := strconv.Atoi(sm[1])
	if err != nil {
		t.Fatalf("parse HM_SHIM_EXIT_SETUP: %v", err)
	}
	if n != hmShimExitSetup {
		t.Fatalf("HM_SHIM_EXIT_SETUP=%d but Go const hmShimExitSetup=%d", n, hmShimExitSetup)
	}
	for _, tok := range []string{
		"#define HM_SHIM_SETUP_TAG_IGNORED 0x484D4947u",
		"#define HM_SHIM_IGNORED_RECORD 8",
		"#define HM_SB_CRITICAL_MASK (HM_SB_NO_NET | HM_SB_READONLY_TMP)",
		"uint32_t sandbox_ignored;",
		/* F-11b：ACK / FAILED 两种记录与槽位字段必须都在。 */
		"#define HM_SHIM_SETUP_TAG_ACKED 0x484D4941u",
		"#define HM_SHIM_SETUP_TAG_FAILED 0x484D4946u",
		"#define HM_SHIM_ACKED_RECORD 8",
		"#define HM_SHIM_FAILED_RECORD 8",
		"uint32_t sb_requested;",
		"uint32_t sb_acked;",
		"uint32_t setup_failed;",
		"uint32_t setup_exec_failed;",
		"uint32_t setup_eof;",
		"uint32_t setup_eof_while_alive;",
		"uint32_t setup_channel;",
	} {
		if !strings.Contains(internal, tok) {
			t.Fatalf("hm_internal.h is missing %q", tok)
		}
	}
	// 公开头必须能看见位定义（Go 与 C 两侧都要引用同一组数值）。
	for _, tok := range []string{
		"#define HM_PROC_RUNNING 1",
		"#define HM_PROC_REAPED 2",
		"#define HM_TERM_EXITED (1u << 0)",
		"#define HM_TERM_SIGNALED (1u << 1)",
		"#define HM_TERM_ESCALATED (1u << 2)",
		"#define HM_TERM_DEADLINE_HIT (1u << 3)",
		"#define HM_SETUP_FAILED (1u << 0)",
		"#define HM_SETUP_EXEC_FAILED (1u << 1)",
		"#define HM_SETUP_EXEC_CONFIRMED (1u << 2)",
		"#define HM_SETUP_CHANNEL_MEASURED (1u << 3)",
		"HM_E_NOT_READY = -16",
	} {
		if !strings.Contains(hdr, tok) {
			t.Fatalf("hermes_abi.h is missing %q", tok)
		}
	}
}

// ------------------------------------------------------------------ *
// G1 / G2 / G3：getter 形态的三条机器守卫（§2.6）
// ------------------------------------------------------------------ *

// f11StripCComments 去掉 C 源码里的块注释与行注释，保留字符串字面量之外的
// 代码。
//
// 为什么守卫必须先去注释：头文件里**必须**写明"禁止把它们写成 static inline
// 或宏"（那是给人看的约束），同时 G1 又把 static inline 当作违规 token。
// 两者只有在"扫代码、不扫注释"时才自洽。这不是放宽 —— 注释里的词不产生
// 任何代码，去掉注释之后每一个 token 都是真正会被编译的东西。
func f11StripCComments(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	inBlock := false
	inLine := false
	inStr := false
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case inBlock:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				inBlock = false
				i++
			}
		case inLine:
			if c == '\n' {
				inLine = false
				b.WriteByte(c)
			}
		case inStr:
			b.WriteByte(c)
			if c == '\\' && i+1 < len(src) {
				b.WriteByte(src[i+1])
				i++
			} else if c == '"' {
				inStr = false
			}
		default:
			if c == '/' && i+1 < len(src) && src[i+1] == '*' {
				inBlock = true
				i++
				continue
			}
			if c == '/' && i+1 < len(src) && src[i+1] == '/' {
				inLine = true
				i++
				continue
			}
			if c == '"' {
				inStr = true
			}
			b.WriteByte(c)
		}
	}
	return b.String()
}

// f11bGetterNames 是 §四 的 13 个符号，硬编码。
//
// G2 之所以必须是**硬编码表**而不是"读头文件再比对清单"：
// tests/guard 的 TestSymbolSurfaceMatchesManifest 是集合双向 diff，它只能
// 防漂移、**防不了缺失** —— "实现漏了一个 getter + 清单也漏了"⇒ 两边同缺
// ⇒ 绿。这是"守卫看起来很强、实际漏一类"的典型，必须用独立的硬编码表补。
var f11bGetterNames = []string{
	"hm_proc_wait",
	"hm_proc_cancel",
	"hm_proc_free",
	"hm_proc_get_state",
	"hm_proc_get_term",
	"hm_proc_get_deadline",
	"hm_proc_copy_deadline_source",
	"hm_proc_get_setup",
	"hm_proc_get_sb_requested",
	"hm_proc_get_sb_acked",
	"hm_proc_get_sb_ignored",
	"hm_proc_copy_stdout",
	"hm_proc_copy_stderr",
}

// TestF11bG1HeaderStaysClean 断言公开头里没有任何"用内联/宏假造导出面"或
// "把内部结构体搬进公开头"的 token。
//
// 为什么这是硬约束：getter 若写成 static inline 或宏，就不会产生导出符号，
// 于是"新 Go + 旧 libhermes_native.a"从**链接期硬失败**退化成**静默误读**
// —— F-11 的 fail-open 洞原地重开，而且没有任何测试会红。
func TestF11bG1HeaderStaysClean(t *testing.T) {
	code := f11StripCComments(f11Read(t, "native", "include", "hermes_abi.h"))
	for _, token := range []string{
		"static inline",
		"#define hm_proc_get_",
		"#define hm_proc_copy_",
		"struct hm_proc",
		"hm_proc_s",
		"} hm_proc",
	} {
		if strings.Contains(code, token) {
			t.Fatalf("hermes_abi.h contains %q outside comments: the getter surface must be "+
				"out-of-line exported functions, and hm_proc must stay in hm_internal.h", token)
		}
	}
	// hm_proc 的定义只能出现在内部头里（方向：internal → public）。
	if !strings.Contains(f11Read(t, "native", "include", "hm_internal.h"), "} hm_proc;") {
		t.Fatal("hm_internal.h lost the hm_proc definition")
	}
	// 每个 getter 在公开头里必须是 HM_EXPORT 声明。
	for _, name := range f11bGetterNames {
		decl := regexp.MustCompile(`HM_EXPORT\s+hm_status\s+` + name + `\s*\(`)
		if !decl.MatchString(code) {
			t.Fatalf("hermes_abi.h is missing an HM_EXPORT declaration for %s", name)
		}
	}
}

// TestF11bG2GetterNamesHardcoded 断言 13 个名字在三处同时存在：
// 头文件有 HM_EXPORT 声明、静态归档里有定义、清单里有一行。
//
// 任意一处缺失都意味着"实现漏了"或"清单漏了"，而两者同时漏正是双向 diff
// 看不见的那种缺口。
func TestF11bG2GetterNamesHardcoded(t *testing.T) {
	root := f11RepoRoot(t)
	code := f11StripCComments(f11Read(t, "native", "include", "hermes_abi.h"))

	manifestRaw := f11Read(t, "scripts", "expected_symbols.txt")
	manifest := map[string]bool{}
	for _, line := range strings.Split(manifestRaw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		manifest[line] = true
	}
	if len(manifest) == 0 {
		t.Fatal("expected_symbols.txt parsed to 0 symbols — the guard itself is broken")
	}

	archive := filepath.Join(root, "build", "libhermes_native.a")
	if _, err := os.Stat(archive); err != nil {
		t.Skipf("build/libhermes_native.a 不存在，先跑 scripts/build.sh: %v", err)
	}
	// 不能用 nm 的退出码判成败：Rust 1.98 产出 LLVM 22 的 DWARF，Apple 的 nm
	// 读端更旧，会对 compiler_builtins 的 .o 报 Unknown attribute kind 并非零
	// 退出，但符号表照常输出（tests/guard 里已有同样的注释）。
	out, _ := exec.Command("nm", "-gU", archive).Output()
	defined := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		sym := strings.TrimPrefix(fields[len(fields)-1], "_")
		if strings.HasPrefix(sym, "hm_") {
			defined[sym] = true
		}
	}
	if len(defined) == 0 {
		t.Fatal("nm 未解析出任何 hm_* 符号，无法验证 getter 是否真的被导出")
	}

	var missing []string
	for _, name := range f11bGetterNames {
		if !regexp.MustCompile(`HM_EXPORT\s+hm_status\s+` + name + `\s*\(`).MatchString(code) {
			missing = append(missing, name+" (头文件无声明)")
		}
		if !manifest[name] {
			missing = append(missing, name+" (清单无条目)")
		}
		if !defined[name] {
			missing = append(missing, name+" (归档无定义)")
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("getter 面不完整（双向 diff 看不见这类缺口）:\n  %s", strings.Join(missing, "\n  "))
	}
}

// TestF11bG3HeaderIncludeSet 断言公开头的 include 集合**恰好**是
// {<stddef.h>, <stdint.h>}，方向必须是 internal → public。
//
// 一旦 hermes_abi.h 反向 include hm_internal.h，内部结构体就成了跨语言
// 契约的一部分，"内部可随时改"这句话当场失效。
func TestF11bG3HeaderIncludeSet(t *testing.T) {
	code := f11StripCComments(f11Read(t, "native", "include", "hermes_abi.h"))
	re := regexp.MustCompile(`(?m)^\s*#\s*include\s+[<"]([^>"]+)[>"]`)
	var got []string
	for _, m := range re.FindAllStringSubmatch(code, -1) {
		got = append(got, m[1])
	}
	sort.Strings(got)
	want := []string{"stddef.h", "stdint.h"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("hermes_abi.h includes %v, want exactly %v", got, want)
	}
	if strings.Contains(code, "hm_internal.h") {
		t.Fatal("hermes_abi.h includes hm_internal.h: the dependency direction must be internal → public")
	}
	// 反方向必须成立。
	if !regexp.MustCompile(`(?m)^\s*#\s*include\s+"hermes_abi.h"`).MatchString(
		f11StripCComments(f11Read(t, "native", "include", "hm_internal.h"))) {
		t.Fatal("hm_internal.h must include \"hermes_abi.h\"")
	}
}

// f11FuncBody 从源码里截出以 sig 开头、以行首 "}" 结尾的函数体。
// 用字符串切而不是正则：Go/C 的函数体里带花括号的复合字面量太多。
func f11FuncBody(t *testing.T, src, sig string) string {
	t.Helper()
	i := strings.Index(src, sig)
	if i < 0 {
		t.Fatalf("cannot locate %q", sig)
	}
	rest := src[i:]
	if j := strings.Index(rest, "\n}\n"); j >= 0 {
		return rest[:j+3]
	}
	if j := strings.Index(rest, "\n}"); j >= 0 {
		return rest[:j+2]
	}
	t.Fatalf("cannot delimit the body of %q", sig)
	return ""
}

// TestF11bWaitReadsGettersBeforeDecision 是顺序守卫：Wait() 必须先逐个
// getter 取证据，再跑安全判据，最后才跑结局判据。
// 反过来（先判据后取证据）会让判据读到零值，于是每条请求都命中 no_ack ——
// 表面上"更安全"，实际是把所有正常调用拒掉。
func TestF11bWaitReadsGettersBeforeDecision(t *testing.T) {
	src := f11Read(t, "internal", "native", "exec.go")
	body := f11FuncBody(t, src, "func (p *Proc) Wait() (*Result, error) {")
	dec := strings.Index(body, "sandboxDecision(")
	exec2 := strings.Index(body, "ErrToolNotStarted")
	if dec < 0 || exec2 < 0 {
		t.Fatalf("Wait() lost the decision (%d) or the outcome judgement (%d)", dec, exec2)
	}
	// 每个 getter 调用都必须早于安全判据。
	for _, g := range []string{
		"p.getState()", "p.getTerm()", "p.getDeadline()",
		"p.copyDeadlineSource()", "p.getSetup()",
		"p.getSbRequested()", "p.getSbAcked()", "p.getSbIgnored()",
		"p.copyStream(false)", "p.copyStream(true)",
	} {
		i := strings.Index(body, g)
		if i < 0 {
			t.Fatalf("Wait() no longer calls %s", g)
		}
		if i > dec {
			t.Fatalf("Wait() calls %s AFTER the sandbox decision: the decision would read "+
				"zero-valued evidence", g)
		}
	}
	// 结局判据必须在安全判据**之后**：安全优先于正确性。
	if exec2 < dec {
		t.Fatal("Wait() applies ErrToolNotStarted BEFORE the sandbox decision: " +
			"a policy denial must win over a correctness verdict")
	}
	// v4 不再有任何需要配对的 free。
	if strings.Contains(body, "hm_spawn_res_free") || strings.Contains(body, "hm_proc_res_free") {
		t.Fatal("Wait() still frees a result buffer: v4 buffers stay in the C slot")
	}
}

// TestF11bOutcomeGateUsesChannelMeasured 是 ErrToolNotStarted 的**三个合取
// 项**的守卫。这三项不是"保守起见先堆着"，各自承担命题的一部分（推导见
// exec.go 的 ⑦ 段）：
//   - 少 channelMeasured ⇒ Windows 上每次主动取消都变成"工具未启动"；
//   - 少 Signaled        ⇒ 判据从"被信号杀死且没确认 exec"退化成
//     "没确认 exec"，而后者混淆"没跑"与"跑太快没采样到"（真实 shim 下
//     /bin/echo hi 跨轮 0/20 ~ 18/20 确认不到、Wait 却从不报错）⇒ 判据开始说假话；
//   - 少 !ExecConfirmed  ⇒ 判据本身消失。
func TestF11bOutcomeGateUsesChannelMeasured(t *testing.T) {
	src := f11Read(t, "internal", "native", "exec.go")
	// 判据本体抽在 toolNotStarted 里（纯函数，便于直接断言 Windows 豁免）。
	gate := f11FuncBody(t, src, "func toolNotStarted(execConfirmed bool, setupBits uint32, signaled bool) bool {")
	for _, tok := range []string{"!execConfirmed", "setupChannelMeasured", "signaled"} {
		if !strings.Contains(gate, tok) {
			t.Fatalf("the ErrToolNotStarted gate lost %q: that turns a specific false report "+
				"into a silent one", tok)
		}
	}
	// Wait 必须真的用它，而不是自己另写一份条件。
	wait := f11FuncBody(t, src, "func (p *Proc) Wait() (*Result, error) {")
	if !strings.Contains(wait, "toolNotStarted(out.ExecConfirmed, bits, out.Signaled)") {
		t.Fatal("Wait() does not delegate the outcome judgement to toolNotStarted")
	}

	// D5 的 Windows 豁免：没有 fd3 通道时永不触发。
	if toolNotStarted(false, 0, true) {
		t.Fatal("toolNotStarted fired without CHANNEL_MEASURED: on Windows every cancel would " +
			"be reported as \"tool never started\"")
	}
	// 正常快速退出不受影响 —— 这条是实测撑着的，不是推理。
	if toolNotStarted(false, setupChannelMeasured, false) {
		t.Fatal("toolNotStarted fired without Signaled: a sub-2ms normal exit would be misreported")
	}
	// POSIX 上未确认 exec + 被信号杀 = 工具没启动。
	if !toolNotStarted(false, setupChannelMeasured, true) {
		t.Fatal("toolNotStarted did not fire on the E3 shape (measured channel + signaled + no exec)")
	}
	// exec 已确认时永远放行（E4）。
	if toolNotStarted(true, setupChannelMeasured, true) {
		t.Fatal("toolNotStarted fired although execve was confirmed (E4 would be misreported)")
	}
	// F1：pre-exec 死亡分两侧，且两侧必须互斥 —— 主动取消的那个**不可重试**。
	w := f11FuncBody(t, src, "func (p *Proc) Wait() (*Result, error) {")
	if !strings.Contains(w, "p.cancelRequested.Load()") {
		t.Fatal("Wait() does not consult cancelRequested: a caller-initiated cancel would be " +
			"reported as retryable and a naive retry loop would spin")
	}
	cancelled := strings.Index(w, "return nil, ErrToolCancelled")
	notStarted := strings.Index(w, "return nil, ErrToolNotStarted")
	if cancelled < 0 || notStarted < 0 {
		t.Fatalf("Wait() lost one of the two pre-exec outcomes (cancelled=%d notStarted=%d)",
			cancelled, notStarted)
	}
	if cancelled > notStarted {
		t.Fatal("Wait() returns ErrToolNotStarted before ErrToolCancelled: the retryable class " +
			"would win and the spin loop is back")
	}

	// CHANNEL_MEASURED 必须来自**粘住**的槽位位，不能按 setup_fd 现算。
	sup := f11Read(t, "native", "c", "supervisor.c")
	getter := f11StripCComments(f11FuncBody(t, sup, "hm_status hm_proc_get_setup(hm_handle proc, uint32_t *out_bits,"))
	if !strings.Contains(getter, "p->setup_channel") {
		t.Fatal("hm_proc_get_setup must read the sticky p->setup_channel bit")
	}
	if strings.Contains(getter, "setup_fd") {
		t.Fatal("hm_proc_get_setup derives CHANNEL_MEASURED from setup_fd: pump_setup closes " +
			"the fd on EOF, so the bit would read 0 after wait and the whole D5 gate dies")
	}
	exec := f11Read(t, "native", "c", "exec.c")
	if !regexp.MustCompile(`p->setup_channel = \(p->setup_fd >= 0\) \? 1u : 0u;`).MatchString(exec) {
		t.Fatal("exec.c must set p->setup_channel once, when filling the slot")
	}
	// 且只能有一处写它（"粘住"意味着之后只读）。
	if n := strings.Count(exec, "p->setup_channel ="); n != 1 {
		t.Fatalf("exec.c assigns p->setup_channel %d times, want exactly 1 (set once, then read-only)", n)
	}
	if strings.Contains(sup, "p->setup_channel =") {
		t.Fatal("supervisor.c writes p->setup_channel: the bit must be set once in exec.c and never re-derived")
	}
}

// TestF11bDecisionIgnoresExitCode 是"元守卫"：判据函数体里不得出现
// hmShimExitSetup。常量本身留着（跨语言一致性守卫要用），但它一旦重新进入
// 判据，E2 的误拒就回来了。
func TestF11bDecisionIgnoresExitCode(t *testing.T) {
	src := f11Read(t, "internal", "native", "exec.go")
	body := f11FuncBody(t, src, "func sandboxDecision(")
	if strings.Contains(body, "hmShimExitSetup") {
		t.Fatal("sandboxDecision references hmShimExitSetup: exit 40 must never be evidence again (E2)")
	}
	if !strings.Contains(body, "_ = setupErrno") {
		t.Fatal("sandboxDecision must explicitly ignore setupErrno so the intent is checkable")
	}
}

// TestF11bNoZombieCSymbols 是 E-g 的另一半：Go 侧不得再引用被删除的符号。
// cgo 会在**编译期**就报错，但这条守卫把"有人手写 extern 绕过 cgo 检查"
// 这条缝也堵上，并且给出比 undefined reference 更清楚的原因。
func TestF11bNoZombieCSymbols(t *testing.T) {
	root := f11RepoRoot(t)
	dir := filepath.Join(root, "internal", "native")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	zombies := []string{"C.hm_spawn_res", "C.hm_spawn_wait", "C.hm_spawn_free",
		"C.hm_spawn_cancel", "C.hm_spawn_res_free", "C.hm_get_setup_status"}
	checked := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		// _test.go 里必须能写出这些名字（否则守卫自己没法描述它守什么）。
		if strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		checked++
		for _, z := range zombies {
			if strings.Contains(string(b), z) {
				t.Fatalf("%s references %s: that symbol was deleted in F-11b (R8 2b)", e.Name(), z)
			}
		}
	}
	if checked == 0 {
		t.Fatal("scanned 0 Go files — the guard itself is broken")
	}
}

// TestF11bPumpSetupBranches 断言 C 侧真的解析了 ACK/FAILED，且
// **裸 errno 分支不置 setup_failed**。后者是 E2 的结构性保证：
// execve 失败时 setup 是成功的，把它记成 setup 失败会让正常工具被误拒。
func TestF11bPumpSetupBranches(t *testing.T) {
	src := f11Read(t, "native", "c", "supervisor.c")
	body := f11FuncBody(t, src, "static void pump_setup(hm_proc *p) {")
	for _, tok := range []string{
		"HM_SHIM_SETUP_TAG_ACKED", "p->sb_acked |=",
		"HM_SHIM_SETUP_TAG_FAILED", "p->setup_failed = 1;",
		"p->setup_eof = 1;",
	} {
		if !strings.Contains(body, tok) {
			t.Fatalf("pump_setup is missing %q", tok)
		}
	}
	// 裸 errno 分支（注释锚点 → 该分支的 consume）不得**赋值** setup_failed，
	// 但**必须**置 setup_exec_failed（判据④的依据）。
	// 查赋值而不是查标识符：注释里写"不置 setup_failed"是文档，不是行为。
	i := strings.Index(body, "裸 4 字节 errno")
	if i < 0 {
		t.Fatal("pump_setup lost the bare-errno branch anchor comment")
	}
	tail := body[i:]
	if j := strings.Index(tail, "setup_consume(p, HM_SHIM_ERRFD_PAYLOAD);"); j >= 0 {
		tail = tail[:j]
	}
	if strings.Contains(tail, "setup_failed = 1") {
		t.Fatal("the bare 4-byte errno branch sets setup_failed: execve failure would be " +
			"misreported as a setup failure (E2)")
	}
	if !strings.Contains(tail, "setup_exec_failed = 1") {
		t.Fatal("the bare 4-byte errno branch must set setup_exec_failed (judgement ④)")
	}

	// read() 出错 ≠ EOF：错误分支不得置 setup_eof，否则 I/O 故障会被当成
	// "exec 已发生"。
	//
	// ⚠ 证据等级：**仅结构证据**。这条守卫没有行为对照 —— 管道 read() 的
	// 错误无法在测试里稳定构造（要拿到 EBADF 得先让 fd 在 pump_setup 期间
	// 失效，那与"非阻塞读"的时序竞争无法复现）。它变成可验证的条件是：
	// pump_setup 的 fd 参数化 + 一个 C 层单测传入 O_WRONLY 的 fd ⇒ 稳定
	// 得到 EBADF ⇒ 断言 setup_eof == 0。见 docs/KNOWN-GAPS.md 的证据等级表。
	errBranch := body[strings.Index(body, "if (n < 0) {"):]
	if j := strings.Index(errBranch, "    p->setup_acc_len += "); j >= 0 {
		errBranch = errBranch[:j]
	}
	if strings.Contains(errBranch, "setup_eof = 1") {
		t.Fatal("the read-error branch sets setup_eof: an I/O error would be read as a successful exec")
	}
}

// TestF11bGetterLifecycle 是 getter 的三态守卫（真实 shim，真跑）：
// Wait 之前 → HM_E_NOT_READY；Wait 之后 → 值；Free 之后 → HM_E_HANDLE_STALE。
// 三者都必须**明确报错**，绝不用零值冒充答案。
func TestF11bGetterLifecycle(t *testing.T) {
	shim, err := ResolveShim("")
	if err != nil {
		t.Fatalf("ResolveShim: %v (build/hermes-shim missing? run scripts/build.sh first)", err)
	}
	p, err := Spawn(Spec{Exe: "/bin/echo", Args: []string{"echo", "f11b-lifecycle"}}, shim)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// ① Wait 之前：状态是 RUNNING，其余 getter 必须报 HM_E_NOT_READY。
	if st, err := p.getState(); err != nil || st != procRunning {
		t.Fatalf("getState before Wait = (%d, %v), want (RUNNING, nil)", st, err)
	}
	if _, err := p.getTerm(); err != ErrNotReady {
		t.Fatalf("getTerm before Wait = %v, want ErrNotReady (HM_E_NOT_READY)", err)
	}
	if _, _, err := p.getSetup(); err != ErrNotReady {
		t.Fatalf("getSetup before Wait = %v, want ErrNotReady", err)
	}
	if _, err := p.copyDeadlineSource(); err != ErrNotReady {
		t.Fatalf("copyDeadlineSource before Wait = %v, want ErrNotReady", err)
	}
	if _, err := p.copyStream(false); err != ErrNotReady {
		t.Fatalf("copyStream before Wait = %v, want ErrNotReady", err)
	}

	res, err := p.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !strings.Contains(string(res.Stdout), "f11b-lifecycle") {
		t.Fatalf("stdout = %q: target did not run", res.Stdout)
	}

	// ② Wait 之后：完整答案。sandbox_flags=0 的请求既没有请求位、也没有
	// 施加位、更没有握手 —— 三个位面必须诚实为 0（不是"不知道"）。
	if res.SetupFailed || res.ExecFailed {
		t.Fatalf("zero-sandbox status reports failed/execFailed: nothing was requested, nothing can fail")
	}
	if res.Acked != 0 || res.SandboxRequested != 0 || res.Ignored != 0 || res.SetupErrno != 0 {
		t.Fatalf("zero-sandbox status = acked:0x%x requested:0x%x ignored:0x%x errno:%d, want all zero",
			uint32(res.Acked), uint32(res.SandboxRequested), uint32(res.Ignored), res.SetupErrno)
	}
	// POSIX 恒有 fd3 通道：CHANNEL_MEASURED 必须置位（D5 的豁免开关的反面）。
	// 这条是 E3 结局判据能生效的**前提**，不能只在 E3 里间接触及。
	bits, _, err := p.getSetup()
	if err != nil {
		t.Fatalf("getSetup after Wait: %v", err)
	}
	if bits&setupChannelMeasured == 0 {
		t.Fatal("CHANNEL_MEASURED is not set on POSIX: ErrToolNotStarted could never fire")
	}

	// ③ Free 之后：handle 已失效（generation++），必须报 stale。
	if err := p.Free(); err != nil {
		t.Fatalf("Free: %v", err)
	}
	if _, err := p.getTerm(); err != ErrHandleStale {
		t.Fatalf("getTerm after Free = %v, want ErrHandleStale (HM_E_HANDLE_STALE)", err)
	}
	if _, err := p.getState(); err != ErrHandleStale {
		t.Fatalf("getState after Free = %v, want ErrHandleStale", err)
	}
}

// f11bStripCComments 去掉 C 注释，只留代码文本。
//
// 为什么要它：源码结构守卫断言的是**代码形状**（"这一行调用的是哪个函数"），
// 而注释里恰恰要写"不要这么写 —— kill(-pgid) 在 pgid==0 时会自杀"。
// 不剥注释，守卫会禁止解释自己，结果是注释被删掉、知识丢失。
// 只处理 /* */ 与 //，不处理字符串字面量（本文件的断言目标里没有）。
func f11bStripCComments(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], "/*") {
			j := strings.Index(s[i+2:], "*/")
			if j < 0 {
				break // 未闭合：剩余全算注释
			}
			i += 2 + j + 2
			b.WriteByte(' ')
			continue
		}
		if strings.HasPrefix(s[i:], "//") {
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				break
			}
			i += j
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// TestF11bProcFreeKillsThroughKillGroup 是**脚枪守卫**（源码结构，不真跑）。
//
// 背景：native/c/supervisor.c 的 hm_proc_free 清理路径原来写的是
// `kill(-p->pgid, SIGKILL)`。pgid 正常由 spawn 填成子进程 pid，但这条是
// **清理路径**：一旦 pgid 因任何原因没被填（0 或负），`kill(-p->pgid, ...)`
// 就变成 `kill(0, SIGKILL)` —— **把 hermes 自己所在的整个进程组全杀掉**。
// 那不是"清理失败的边缘情况"，是把宿主进程连同同组的兄弟一起带走。
//
// 现在的写法是有守卫的 kill_group(p, SIGKILL)（内部 `if (p->pgid <= 0) return;`），
// 并在 pgid <= 0 时退化成只杀直接子进程 pid。
//
// 为什么用源码守卫而不是跑一遍：要真跑，得让 pgid 在 free 之前变 0/负，
// 那要求先从外部改坏 hm_proc 的内部状态 —— 测试能构造出来的路径也不会命中
// 裸 kill（pgid 恒 > 0）。所以这里断言形状：**清理路径只许经 kill_group**。
// 反向注入验证：把 kill_group(p, SIGKILL) 换回 kill(-p->pgid, SIGKILL) ⇒ 变红。
func TestF11bProcFreeKillsThroughKillGroup(t *testing.T) {
	src := f11Read(t, "native", "c", "supervisor.c")
	// 去注释再查：注释里**必须**允许写 `kill(-p->pgid, ...)` —— 那正是解释
	// "为什么不能这么写"的地方。断言对象是**代码**，不是散文。
	body := f11bStripCComments(f11FuncBody(t, src, "hm_status hm_proc_free(hm_handle proc) {"))

	if !strings.Contains(body, "kill_group(p, SIGKILL)") {
		t.Fatal("hm_proc_free no longer kills through kill_group(): the cleanup path must " +
			"not call raw kill(-pgid) — with pgid == 0 that is kill(0, SIGKILL), which kills " +
			"hermes' own process group")
	}
	// 裸 `kill(-p->pgid` / `kill(-p->pgid,` 都不许出现（含 p->pgid 变量名变体）。
	for _, bad := range []string{"kill(-p->pgid", "kill(- p->pgid"} {
		if strings.Contains(body, bad) {
			t.Fatalf("hm_proc_free contains raw %q: it must go through kill_group(), which "+
				"guards pgid <= 0", bad)
		}
	}
	// kill_group 自己必须保留 pgid <= 0 的短路 —— 否则守卫只是个名字。
	kg := f11FuncBody(t, src, "static void kill_group(hm_proc *p, int sig) {")
	if !strings.Contains(kg, "p->pgid <= 0") {
		t.Fatal("kill_group() lost its `p->pgid <= 0` guard: then kill(-pgid) can still " +
			"become kill(0, SIGKILL)")
	}
}
