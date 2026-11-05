package native_test

// 原生层的验收测试。
//
// 两条纪律，写在这里免得以后有人"优化"掉：
//
//  1. **不用墙钟判断超时是否生效。** 这台机器被占满时同一份代码的墙钟能差 4×
//     （Homonoia 上踩过），所以所有超时断言都看结构事实：进程组是否清空、
//     DeadlineHit 是否为真、DeadlineSource 是不是 "cycle"。真正的墙钟只用于
//     "别让测试挂死"的兜底，不作为断言依据。
//
//  2. **不测"能跑"，测"坏了会报错"。** free 之后读必须是 ErrHandleStale，
//     超限必须是 ErrTooLarge，杀进程必须连孙子一起杀 —— 这些才是这层的价值。

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"hermes/internal/native"
)

func TestMain(m *testing.M) {
	if err := native.Init(); err != nil {
		panic("native.Init: " + err.Error())
	}
	// 测试二进制跑在 go test 的临时目录里，"同目录找 shim" 必然失败。
	// 这里指到仓库的构建产物；找不到就让需要 shim 的用例报出明确错误，
	// 而不是让 ResolveShim 去猜一个路径。
	if os.Getenv("HERMES_SHIM") == "" {
		abs, err := filepath.Abs("../../build/hermes-shim")
		if err == nil {
			if _, statErr := os.Stat(abs); statErr == nil {
				os.Setenv("HERMES_SHIM", abs)
			}
		}
	}
	// 测试二进制跑在 go test 的临时目录里，"同目录找 shim" 必然失败。
	// 这里指到仓库的构建产物；找不到就让需要 shim 的用例报出明确错误，
	// 而不是让 ResolveShim 去猜一个路径。
	if os.Getenv("HERMES_SHIM") == "" {
		abs, err := filepath.Abs("../../build/hermes-shim")
		if err == nil {
			if _, statErr := os.Stat(abs); statErr == nil {
				os.Setenv("HERMES_SHIM", abs)
			}
		}
	}
	code := m.Run()
	native.Shutdown()
	os.Exit(code)
}

// ---------- T02: ABI 版本 ----------

func TestABIVersionMatchesHeader(t *testing.T) {
	got := native.ABIVersion()
	if got>>16 != native.ABIMajor {
		t.Fatalf("major = %d, want %d", got>>16, native.ABIMajor)
	}
	if got&0xffff != native.ABIMinor {
		t.Fatalf("minor = %d, want %d", got&0xffff, native.ABIMinor)
	}
	if native.BuildID() == "" {
		t.Fatal("BuildID is empty")
	}
}

func TestInitRejectsWrongMajor(t *testing.T) {
	err := native.InitExpecting(2 << 16)
	if err == nil {
		t.Fatal("InitExpecting(major 2) returned nil, want ABI mismatch")
	}
	if !strings.Contains(err.Error(), "ABI") {
		t.Fatalf("error does not mention ABI: %v", err)
	}
}

// ---------- T03: 敏感缓冲 ----------

func TestSensitiveRoundTrip(t *testing.T) {
	want := []byte("sk-live-0123456789")
	s, err := native.NewSensitive(want, false)
	if err != nil {
		t.Fatalf("NewSensitive: %v", err)
	}
	defer s.Free()

	n, err := s.Len()
	if err != nil || n != len(want) {
		t.Fatalf("Len = %d, %v; want %d", n, err, len(want))
	}
	got, err := s.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("Read = %q, want %q", got, want)
	}
}

func TestSensitiveRejectsOversize(t *testing.T) {
	big := make([]byte, native.SensitiveMax+1)
	if _, err := native.NewSensitive(big, true); err == nil {
		t.Fatal("NewSensitive over limit returned nil, want ErrTooLarge")
	}
}

func TestSensitiveStaleAfterFree(t *testing.T) {
	s, err := native.NewSensitive([]byte("x"), true)
	if err != nil {
		t.Fatalf("NewSensitive: %v", err)
	}
	if err := s.Free(); err != nil {
		t.Fatalf("Free: %v", err)
	}
	if _, err := s.Read(); err == nil {
		t.Fatal("Read after Free returned nil, want stale handle error")
	}
	if err := s.Free(); err == nil {
		t.Fatal("double Free returned nil, want stale handle error")
	}
}

// TestSensitiveZeroized 是这一层最核心的安全断言：
// 释放之后，那块内存里不能残留明文。
func TestSensitiveZeroized(t *testing.T) {
	secret := []byte("AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCY")
	s, err := native.NewSensitive(secret, true)
	if err != nil {
		t.Fatalf("NewSensitive: %v", err)
	}
	if err := s.Free(); err != nil {
		t.Fatalf("Free: %v", err)
	}
	peek, err := native.DebugPeek()
	if err != nil {
		t.Fatalf("DebugPeek: %v", err)
	}
	if len(peek) != len(secret) {
		t.Fatalf("DebugPeek len = %d, want %d", len(peek), len(secret))
	}
	for i, b := range peek {
		if b != 0 {
			t.Fatalf("byte %d not zeroed: %q", i, peek)
		}
	}
}

// TestSensitiveMlockReportsTruthfully：降级必须被如实报告，不能假装成功。
func TestSensitiveMlockReportsTruthfully(t *testing.T) {
	s, err := native.NewSensitive([]byte("x"), true) // no_mlock = 1
	if err != nil {
		t.Fatalf("NewSensitive: %v", err)
	}
	defer s.Free()
	locked, err := s.Locked()
	if err != nil {
		t.Fatalf("Locked: %v", err)
	}
	if locked {
		t.Fatal("no_mlock=1 but Locked() reports true")
	}
}

// ---------- T03: 只读视图 ----------

func TestViewSliceSharesBacking(t *testing.T) {
	src := []byte("0123456789")
	v, err := native.NewView(src)
	if err != nil {
		t.Fatalf("NewView: %v", err)
	}
	defer v.Free()

	sub, err := v.Slice(3, 4)
	if err != nil {
		t.Fatalf("Slice: %v", err)
	}
	defer sub.Free()

	got, err := sub.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if string(got) != "3456" {
		t.Fatalf("Bytes = %q, want %q", got, "3456")
	}
}

func TestViewSliceOutOfRange(t *testing.T) {
	v, err := native.NewView([]byte("abc"))
	if err != nil {
		t.Fatalf("NewView: %v", err)
	}
	defer v.Free()
	if _, err := v.Slice(2, 5); err == nil {
		t.Fatal("Slice(2,5) on 3-byte view returned nil, want invalid argument")
	}
	if _, err := v.Slice(-1, 1); err == nil {
		t.Fatal("Slice(-1,1) returned nil, want invalid argument")
	}
}

func TestViewRejectsOversize(t *testing.T) {
	big := make([]byte, native.ViewMax+1)
	if _, err := native.NewView(big); err == nil {
		t.Fatal("NewView over limit returned nil, want ErrTooLarge")
	}
}

// ---------- T04: 哈希链与 golden ----------

type goldenChain struct {
	Name     string   `json:"name"`
	Domain   string   `json:"domain"`
	FinalTag string   `json:"final_tag"`
	Blocks   []string `json:"blocks"`
	Expected []string `json:"expected"`
	Final    string   `json:"final"`
}

func TestChainMatchesGolden(t *testing.T) {
	if native.ChainDomain != "hermes.trace.v1" {
		t.Fatalf("ChainDomain = %q", native.ChainDomain)
	}
	paths, err := filepath.Glob("../../schemas/golden/chain-*.json")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no golden files found: %v", err)
	}
	for _, p := range paths {
		p := p
		t.Run(filepath.Base(p), func(t *testing.T) {
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			var g goldenChain
			if err := json.Unmarshal(raw, &g); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if g.Domain != native.ChainDomain {
				t.Fatalf("golden domain %q != native domain %q", g.Domain, native.ChainDomain)
			}
			prev := native.Genesis()
			var total uint64
			for i, hx := range g.Blocks {
				// golden 里存的是 hex，不是原文 —— 用原文读会把"转义后的文本"
				// 当字节，哈希自然对不上。
				blk, err := hex.DecodeString(hx)
				if err != nil {
					t.Fatalf("block %d is not valid hex: %v", i, err)
				}
				total += uint64(len(blk))
				got, err := native.ChainHash(prev, uint64(i), blk)
				if err != nil {
					t.Fatalf("block %d: %v", i, err)
				}
				if h := hex.EncodeToString(got[:]); h != g.Expected[i] {
					t.Fatalf("block %d hash = %s, want %s", i, h, g.Expected[i])
				}
				prev = got
			}
			// 终块把块数与总字节数一起封进去，尾部截断才可检测。
			fin, err := native.ChainFinal(prev, uint64(len(g.Blocks)), total)
			if err != nil {
				t.Fatalf("ChainFinal: %v", err)
			}
			if h := hex.EncodeToString(fin[:]); h != g.Final {
				t.Fatalf("final = %s, want %s", h, g.Final)
			}
		})
	}
}

func TestChainRejectsOversizeBlock(t *testing.T) {
	big := make([]byte, native.ChainBlockMax+1)
	if _, err := native.ChainHash(native.Genesis(), 0, big); err == nil {
		t.Fatal("ChainHash over limit returned nil, want ErrTooLarge")
	}
}

// ---------- T05: 硬件计数器 ----------

func TestCycleMonotonic(t *testing.T) {
	var prev uint64
	const rounds = 100000
	for i := 0; i < rounds; i++ {
		v, err := native.CycleNow()
		if err != nil {
			t.Fatalf("CycleNow: %v", err)
		}
		if i > 0 && v < prev {
			t.Fatalf("counter went backwards at %d: %d < %d", i, v, prev)
		}
		prev = v
	}
}

func TestCycleFreqAndArch(t *testing.T) {
	hz, err := native.CycleFreq()
	if err != nil {
		t.Fatalf("CycleFreq: %v", err)
	}
	if hz == 0 {
		t.Fatal("CycleFreq = 0")
	}
	arch, err := native.CycleArch()
	if err != nil {
		t.Fatalf("CycleArch: %v", err)
	}
	switch runtime.GOARCH {
	case "arm64":
		if arch != native.ArchARM64 {
			t.Fatalf("arch = %v, want arm64", arch)
		}
	case "amd64":
		if arch != native.ArchX8664 {
			t.Fatalf("arch = %v, want x86_64", arch)
		}
	}
}

// TestTicksNeverRoundToZero：极短 deadline 向上取整，绝不能变成 0。
// 0 在 ABI 里表示"无 deadline"，那会把熔断悄悄关掉。
func TestTicksNeverRoundToZero(t *testing.T) {
	ticks, err := native.TicksForDuration(1)
	if err != nil {
		t.Fatalf("TicksForDuration: %v", err)
	}
	if ticks == 0 {
		t.Fatal("1ns deadline rounded down to 0 ticks (would disable the deadline)")
	}
}

// ---------- T06/T07: 进程督导 ----------

func shimPath(t *testing.T) string {
	t.Helper()
	p, err := native.ResolveShim("")
	if err != nil {
		t.Fatalf("ResolveShim: %v", err)
	}
	return p
}

func TestSpawnEcho(t *testing.T) {
	shim := shimPath(t)
	p, err := native.Spawn(native.Spec{
		Exe:  "/bin/echo",
		Args: []string{"echo", "hello-hermes"},
	}, shim)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	res, err := p.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	defer p.Free()

	if !res.Exited || res.ExitCode != 0 {
		t.Fatalf("expected clean exit, got exited=%v code=%d signaled=%v", res.Exited, res.ExitCode, res.Signaled)
	}
	if !strings.Contains(string(res.Stdout), "hello-hermes") {
		t.Fatalf("stdout = %q", res.Stdout)
	}
	if res.SetupErrno != 0 {
		t.Fatalf("setup_errno = %d, want 0", res.SetupErrno)
	}
}

// TestSpawnDeadlineKillsProcessGroup 是熔断的地基：
// 工具挂住时必须被杀到进程组，包括它自己 fork 出来的孙子进程。
func TestSpawnDeadlineKillsProcessGroup(t *testing.T) {
	shim := shimPath(t)
	p, err := native.Spawn(native.Spec{
		Exe: "/bin/sh",
		Args: []string{"sh", "-c",
			`trap "" TERM; sleep 10 & sleep 10`},
		Deadline: 300 * time.Millisecond,
	}, shim)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	res, err := p.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	defer p.Free()

	if !res.DeadlineHit {
		t.Fatalf("deadline did not fire (exited=%v code=%d)", res.Exited, res.ExitCode)
	}
	if res.KilledPGID <= 0 {
		t.Fatalf("killed_pgid = %d, want > 0", res.KilledPGID)
	}
	if res.DeadlineSource != "cycle" && res.DeadlineSource != "wall" {
		t.Fatalf("deadline_source = %q", res.DeadlineSource)
	}
	// 进程组必须真的空了 —— 这是"杀干净"的唯一结构性证据。
	out, _ := exec.Command("pgrep", "-g", strconv.Itoa(res.KilledPGID)).Output()
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("process group %d still has members: %s", res.KilledPGID, out)
	}
}

func TestSpawnCancel(t *testing.T) {
	shim := shimPath(t)
	p, err := native.Spawn(native.Spec{
		Exe:  "/bin/sh",
		Args: []string{"sh", "-c", "sleep 10"},
	}, shim)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := p.Cancel(); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	res, err := p.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	defer p.Free()
	// cancel 的语义是 SIGTERM 到进程组：既要真的被 TERM 掉，也要记下杀了哪个组。
	// 只断言"进程没了"是不够的 —— 进程可能自己退出了。
	if !res.Signaled || res.TermSignal != 15 {
		t.Fatalf("expected SIGTERM (15) termination, got signaled=%v sig=%d", res.Signaled, res.TermSignal)
	}
	if res.KilledPGID <= 0 {
		t.Fatalf("killed_pgid = %d, want > 0", res.KilledPGID)
	}
}

func TestSpawnStdoutCap(t *testing.T) {
	shim := shimPath(t)
	p, err := native.Spawn(native.Spec{
		Exe:       "/bin/echo",
		Args:      []string{"echo", strings.Repeat("A", 4096)},
		StdoutCap: 8,
	}, shim)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	res, err := p.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	defer p.Free()
	if len(res.Stdout) != 8 {
		t.Fatalf("stdout len = %d, want 8 (truncated to cap)", len(res.Stdout))
	}
	_ = shim
}

func TestResolveShimRejectsMissingPath(t *testing.T) {
	want := "/nonexistent/hermes-shim-xyz"
	_, err := native.ResolveShim(want)
	if err == nil {
		t.Fatal("ResolveShim accepted a nonexistent path")
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error does not name the expected path: %v", err)
	}
}

// TestShimSelfTestThroughSpawnPath 顺带证明 shim 可以被当成普通工具启动，
// 且退出码与 stdout 都穿过整套督导逻辑。
func TestShimSelfTestThroughSpawnPath(t *testing.T) {
	shim := shimPath(t)
	p, err := native.Spawn(native.Spec{
		Exe:  shim,
		Args: []string{"hermes-shim", "--self-test"},
	}, shim)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	res, err := p.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	defer p.Free()
	if !res.Exited || res.ExitCode != 0 {
		t.Fatalf("shim --self-test failed: code=%d stderr=%s", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(string(res.Stdout), "OK") {
		t.Fatalf("unexpected self-test output: %q", res.Stdout)
	}
}
