package native

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// f11bProcessAlive 用 /bin/kill -0 探活，**不走 Go 的 syscall**。
//
// 为什么不能直接 syscall.Kill：`TestGoDoesNotTouchSyscalls` 是仓里的红线守卫
// ——Go 侧一个 syscall 都不许碰，执行细节归 C 层。测试也遵守同一条规矩
// （这不是洁癖：跨语言故障面只要扩到两处，"谁来负责"就再也说不清）。
func f11bProcessAlive(t *testing.T, pid int) bool {
	t.Helper()
	err := exec.Command("/bin/kill", "-0", strconv.Itoa(pid)).Run()
	return err == nil
}

// TestF11bFreeBeforeWaitCancelsAndReaps 钉住 hm_proc_free 的**统一语义**：
// 在 wait 之前调用 = "取消 + 收尸"，返回 nil，且不留孤儿。
//
// 为什么必须钉：QA 实测三个地方三套说法 ——
//   - POSIX 实现一直是 kill(-pgid, SIGKILL) + waitpid 并返回 HM_OK；
//   - hermes_abi.h 却写"**必须**在 hm_proc_wait 之后调用"；
//   - internal/native 里声明过 ErrProcNotWaited 并注释"Wait 之前 Free 时返回"，
//     而这个哨兵**全仓零引用、永不返回**（已删除）；
//   - Windows 的 hm_proc_free 没有 `!exited && proc` 分支 ⇒ free 直接成功、
//     子进程变孤儿。
//
// 现在的契约只有一句话：wait 之前 free 等价于取消并收尸。这条测试是它的
// POSIX 侧证据：free 之后**进程必须不存在**，而不仅仅是"free 返回了 nil"。
func TestF11bFreeBeforeWaitCancelsAndReaps(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX 侧证据；Windows 分支是本机无法编译/运行的镜像实现")
	}
	shim, err := ResolveShim("")
	if err != nil {
		t.Fatalf("ResolveShim: %v (build/hermes-shim missing? run scripts/build.sh first)", err)
	}

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")

	// 先把 pid 落到文件里再 exec：exec 不换 pid，所以文件里的 pid 就是最终
	// 那个 sleep 的 pid。这样测试才有一个**可以 fact-check 的对象**，而不是
	// 只能相信"Free 大概杀了吧"。
	p, err := Spawn(Spec{
		Exe:  "/bin/sh",
		Args: []string{"sh", "-c", "echo $$ > '" + pidFile + "'; exec sleep 10"},
	}, shim)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	var pid int
	deadline := time.Now().Add(3 * time.Second)
	for pid == 0 {
		if b, readErr := os.ReadFile(pidFile); readErr == nil {
			if n, convErr := strconv.Atoi(strings.TrimSpace(string(b))); convErr == nil {
				pid = n
				break
			}
		}
		if time.Now().After(deadline) {
			p.Free()
			t.Fatal("the child never wrote its pid: this test would have nothing to verify")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// 前提确认：此刻子进程真的活着。否则下面那条"已消失"断言可能是"它早就
	// 自己退了"，测试就变成空的。
	if !f11bProcessAlive(t, pid) {
		p.Free()
		t.Fatalf("pid %d is not alive before Free: the test premise is broken", pid)
	}

	// ① 不 wait 直接 free：按契约返回 nil（不是错误）。
	if err := p.Free(); err != nil {
		t.Fatalf("Free before Wait returned %v, want nil: the contract is \"cancel + reap\", "+
			"not \"you called it in the wrong order\"", err)
	}

	// ② 子进程必须不存在了 —— 这是"收尸"的唯一结构性证据。
	if f11bProcessAlive(t, pid) {
		t.Fatalf("pid %d is still alive after Free: free-before-wait leaked orphans instead of "+
			"cancelling and reaping", pid)
	}

	// ③ 句柄已归还：所有 getter 必须报 STALE，不能拿零值当答案。
	if _, err := p.getState(); !errors.Is(err, ErrHandleStale) {
		t.Fatalf("hm_proc_get_state after Free = %v, want ErrHandleStale", err)
	}
	if _, err := p.getTerm(); !errors.Is(err, ErrHandleStale) {
		t.Fatalf("hm_proc_get_term after Free = %v, want ErrHandleStale", err)
	}
	if _, err := p.Wait(); !errors.Is(err, ErrHandleStale) {
		t.Fatalf("Wait after Free = %v, want ErrHandleStale", err)
	}

	// ④ 重复 free 是幂等的（Go 侧 done 短路），不能返回错误 —— finalizer 与
	// 显式 defer 会各调一次。
	if err := p.Free(); err != nil {
		t.Fatalf("second Free returned %v, want nil (Free must be idempotent)", err)
	}

	// ⑤ 幂等不只是 Go 的短路：**C 侧的契约**同样是 HM_OK。这里绕过 done
	// 直接调 C —— 否则 ④ 只证明了 Go 的 if，证明不了 C 的 if。
	// 与 ③ 合起来才是完整契约：**清理可以重复（OK），读取必须报错（STALE）**。
	if st := rawProcFree(p); st != Status(0) {
		t.Fatalf("hm_proc_free on an already-released handle = %d, want %d (HM_OK): "+"releasing twice is a no-op by contract, while wait/cancel/getters must say "+"HM_E_HANDLE_STALE — the asymmetry is deliberate and documented in hermes_abi.h",
			int32(st), 0)
	}
	// ⑥ 反方向也要钉住：同一个失效句柄上，getter 仍然必须是 STALE，
	// 不能因为 free 变宽容了就跟着变宽容。
	if _, err := p.getState(); !errors.Is(err, ErrHandleStale) {
		t.Fatalf("hm_proc_get_state after raw hm_proc_free = %v, want ErrHandleStale", err)
	}
}

// TestF11bFreeInvalidHandleIsNoOpButReadsAreNot 钉住**两个方向**，用裸句柄直打 C。
//
// 为什么必须有这条：QA 实测 `hm_proc_free(0)` 在 POSIX 上返 -1
// （HM_E_INVALID_ARG）、在 Windows 上返 0（HM_OK）—— 两侧不一致，而
// `hermes_abi.h` 与 `docs/C-ABI.md` 写的是"已释放**或压根不是本表签发的**句柄
// ⇒ HM_OK"。`0` 就是"不是本表签发的"，所以是 POSIX 代码错了，不是文档错了。
// 修完之后两侧都对，但那是**两条不同的代码路径得到同一个结果**（POSIX：显式
// 前置分支；Windows：无前置检查 + `slot_of(0) == NULL`），一致性只能靠这条
// 测试守，不能靠巧合。
//
// 另一半同样重要：**读取面不享受这个宽容**。free 变宽容之后，如果 getter 也
// 跟着"反正没有就返回 0"，那"用 0 冒充答案"就回来了 —— 所以这里同时断言
// getState / wait 对同一批句柄**必须是错误**。
func TestF11bFreeInvalidHandleIsNoOpButReadsAreNot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX 侧；Windows 走 slot_of(0)==NULL 分支，语义相同但本机无法执行")
	}

	handles := []struct {
		name string
		h    uint64
	}{
		{"HM_HANDLE_INVALID (0)", 0},
		{"slot 1 / gen 0（从未签发）", 1},
		{"垃圾高位（0xdeadbeef00000001）", 0xdeadbeef00000001},
	}
	for _, c := range handles {
		// ① 清理可以重复：free 一个"没有东西可释放"的句柄 ⇒ HM_OK。
		if st := rawProcFreeHandle(c.h); st != Status(0) {
			t.Fatalf("hm_proc_free(%s) = %d, want 0 (HM_OK): freeing something that does not "+
				"exist is a no-op — the contract says 'already released OR not issued by this "+
				"table' ⇒ HM_OK, and POSIX used to return HM_E_INVALID_ARG here while Windows "+
				"returned HM_OK", c.name, int32(st))
		}
		// ② 读取不可重复：同一批句柄上 getter / wait 必须是错误，不许返回 0 当答案。
		var st int32
		if s := rawGetStateHandle(c.h, &st); s == Status(0) {
			t.Fatalf("hm_proc_get_state(%s) = HM_OK: reads must NOT inherit the free-side "+
				"tolerance — a stale/invalid handle has no state to report, and 0 is not a "+
				"valid HM_PROC_* value", c.name)
		}
		if s := rawProcWaitHandle(c.h); s == Status(0) {
			t.Fatalf("hm_proc_wait(%s) = HM_OK: wait on a handle that was never issued must "+
				"fail, not 'succeed with nothing'", c.name)
		}
	}
}
