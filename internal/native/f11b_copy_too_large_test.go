package native

import (
	"testing"
	"time"
	"unsafe"
)

// 为什么这两条测试要**绕过 Go 封装**：
//
// hm_proc_copy_stdout / hm_proc_copy_deadline_source 的 `cap < need` 分支在
// 正常调用路径上**永远到不了** —— copyStream / copyDeadlineSource 都是
// "先 buf=NULL 问长度，再按长度精确分配"。也就是说端到端测试**零覆盖**它。
// QA 的注入实验证实了后果：把这个分支从"返回 HM_E_TOO_LARGE、buf 一个字节
// 不写"改成"截断写 + 返回 HM_OK"（即违反 R3），**一条测试都不红**。
//
// 修法不是建一套 C 单测框架（那要另立构建目标）：copy_raw_hook.go 里有两个
// **一字不改的透传**（不查长度、不分配），测试直接持有句柄调它们。它绕过的
// 恰恰是"Go 封装帮我们避免了这个分支"的那层保护，正是我们要钉的东西。
// （_test.go 不能 import "C"，所以透传必须落在普通文件里 —— 那是语言限制，
// 不是设计选择。）
//
// 两条规则的容量语义**故意不同**（D6），所以两条测试各写一遍，不合并：
//   - 字节流：*out_len 成功时 = 字节数，需 cap = need；
//   - 字符串：*out_len 成功时 = 字符数（不含 '\0'），需 cap = len + 1；
//     TOO_LARGE 时 *out_len 写"所需 cap"= len + 1。
// 混用这两条就是差一个字节的缓冲区溢出。

// f11bEchoProc 起一个 stdout 非空的进程并等它跑完。
func f11bEchoProc(t *testing.T) *Proc {
	t.Helper()
	shim, err := ResolveShim("")
	if err != nil {
		t.Fatalf("ResolveShim: %v (build/hermes-shim missing? run scripts/build.sh first)", err)
	}
	p, err := Spawn(Spec{Exe: "/bin/echo", Args: []string{"echo", "0123456789"}}, shim)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if _, err := p.Wait(); err != nil {
		p.Free()
		t.Fatalf("Wait: %v", err)
	}
	return p
}

// TestF11bCopyStreamTooLargeIsNotTruncation 钉住字节流的 R3 语义：
// cap 差一个字节时**绝不**写 buf，也不返回成功。
func TestF11bCopyStreamTooLargeIsNotTruncation(t *testing.T) {
	p := f11bEchoProc(t)
	defer p.Free()

	// ① 先问长度（buf == NULL）：成功时 *out_len = 字节数。
	var need uint64
	if st := rawCopyStdout(p, nil, 0, &need); st != OK {
		t.Fatalf("length query returned %d, want HM_OK", int(st))
	}
	if need == 0 {
		t.Fatal("stdout is empty: /bin/echo produced nothing, so this test proves nothing")
	}

	// ② cap 比 need 少一个字节，且 buf 预先填满哨兵值。
	cap := need - 1
	buf := make([]byte, int(need))
	const sentinel = 0xAA
	for i := range buf {
		buf[i] = sentinel
	}
	var got uint64 = 0xDEAD

	rc := rawCopyStdout(p, unsafe.Pointer(&buf[0]), cap, &got)

	if rc != ErrTooLarge {
		t.Fatalf("cap %d < need %d returned %d, want HM_E_TOO_LARGE "+
			"(returning HM_OK here is a silent truncation, which R3 forbids)",
			int(cap), int(need), int(rc))
	}
	for i, b := range buf {
		if b != sentinel {
			t.Fatalf("byte %d of the caller's buffer was overwritten (%#x): a short buffer must "+
				"be left completely untouched, not partially filled", i, b)
		}
	}
	if got != need {
		t.Fatalf("*out_len = %d on TOO_LARGE, want the required byte count %d", got, need)
	}

	// ③ 反面确认：给足 cap 时**必须**成功 —— 否则上面那条可能只是"这个函数
	// 永远返回 TOO_LARGE"，那测试就是在给一个坏实现发合格证。
	exact := make([]byte, int(need))
	var n2 uint64
	if st := rawCopyStdout(p, unsafe.Pointer(&exact[0]), need, &n2); st != OK {
		t.Fatalf("exact-size copy returned %d, want HM_OK", int(st))
	}
	if n2 != need {
		t.Fatalf("*out_len = %d on success, want %d", n2, need)
	}
}

// f11bDeadlineProc 起一个被 deadline 杀掉的进程（deadline_source 非空）。
func f11bDeadlineProc(t *testing.T) *Proc {
	t.Helper()
	shim, err := ResolveShim("")
	if err != nil {
		t.Fatalf("ResolveShim: %v (build/hermes-shim missing? run scripts/build.sh first)", err)
	}
	p, err := Spawn(Spec{
		Exe:      "/bin/sh",
		Args:     []string{"sh", "-c", `trap "" TERM; sleep 10`},
		Deadline: 300 * time.Millisecond,
	}, shim)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if _, err := p.Wait(); err != nil {
		p.Free()
		t.Fatalf("Wait: %v", err)
	}
	return p
}

// TestF11bCopyDeadlineSourceTooLargeCountsTheNUL 钉住字符串的 R3 + D6 语义：
// 容量按 `len + 1` 算，TOO_LARGE 时 *out_len 报的是**所需 cap**（不是字符数）。
func TestF11bCopyDeadlineSourceTooLargeCountsTheNUL(t *testing.T) {
	p := f11bDeadlineProc(t)
	defer p.Free()

	// ① 问长度：成功时 *out_len = 字符数，**不含** '\0'。
	var chars uint64
	if st := rawCopyDeadlineSource(p, nil, 0, &chars); st != OK {
		t.Fatalf("length query returned %d, want HM_OK", int(st))
	}
	if chars == 0 {
		t.Fatal("deadline_source is empty: no deadline fired, so this test proves nothing")
	}

	// ② cap = chars（差那个 '\0'）必须被判 TooLarge —— 这就是 D6 的不对称之处：
	// 字节流在 cap == need 时成功，字符串在 cap == len 时必须失败。
	buf := make([]byte, int(chars)+4)
	const sentinel = byte(0x5A)
	for i := range buf {
		buf[i] = sentinel
	}
	var got uint64 = 0xDEAD

	rc := rawCopyDeadlineSource(p, unsafe.Pointer(&buf[0]), chars, &got)

	if rc != ErrTooLarge {
		t.Fatalf("cap %d == strlen %d returned %d, want HM_E_TOO_LARGE: a C string needs len+1 "+
			"and the implementation must not silently drop the NUL terminator",
			int(chars), int(chars), int(rc))
	}
	for i, b := range buf {
		if b != sentinel {
			t.Fatalf("byte %d of the caller's buffer was overwritten (%#x): TOO_LARGE must not "+
				"write a partial string", i, b)
		}
	}
	if got != chars+1 {
		t.Fatalf("*out_len = %d on TOO_LARGE for a string, want the required **cap** %d "+
			"(strlen+1, not strlen)", got, chars+1)
	}

	// ③ 给足 len+1 必须成功，且成功时 *out_len 回到字符数（不含 '\0'）。
	exact := make([]byte, int(chars)+1)
	var n2 uint64
	if st := rawCopyDeadlineSource(p, unsafe.Pointer(&exact[0]), chars+1, &n2); st != OK {
		t.Fatalf("exact-size copy returned %d, want HM_OK", int(st))
	}
	if n2 != chars {
		t.Fatalf("*out_len = %d on success, want the character count %d", n2, chars)
	}
	if exact[chars] != 0 {
		t.Fatalf("byte at index %d is %#x, want the NUL terminator", chars, exact[chars])
	}
}
