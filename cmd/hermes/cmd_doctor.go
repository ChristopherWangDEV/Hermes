package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"hermes/internal/ir"
	"hermes/internal/layout"
	"hermes/internal/native"
	"hermes/internal/std"
)

// doctor 子命令：回答"这台机器现在能不能用 Hermes"。
//
// 为什么值得一条命令：出问题时人先怀疑自己的用法，而真正的差异往往在环境
// （没跑 build.sh、clang 是 GNU 的壳、golden 与编译器版本不配套）。
// 这些**一条条说出来**，比让人去翻文档快得多。
//
// 它只**读**不写，也绝不自动修复：自动修复会把"环境不对"变成"环境被悄悄改过"，
// 而后者更难排查。
type check struct {
	name string
	ok   bool
	note string
	// required 为 false 表示"缺了也能跑，但会少能力"。
	required bool
}

func cmdDoctor(args []string) int {
	for _, a := range args {
		switch a {
		case "-h", "--help", "help":
			fmt.Print(doctorUsage)
			return 0
		}
	}

	checks := []check{}
	checks = append(checks, checkNative())
	checks = append(checks, checkStdlib())
	checks = append(checks, checkGoldens())
	checks = append(checks, checkClang())
	checks = append(checks, checkRuntimeLib())
	checks = append(checks, checkShim())

	failed := 0
	for _, c := range checks {
		mark := "ok  "
		switch {
		case c.ok:
		case c.required:
			mark = "FAIL"
			failed++
		default:
			mark = "warn"
		}
		fmt.Printf("%s  %-22s %s\n", mark, c.name, c.note)
	}
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "doctor: %s\n", fmt.Sprintf(tr("%d required checks failed"), failed))
		return 4
	}
	fmt.Println(tr("doctor: all required checks passed"))
	return 0
}

func checkNative() check {
	v := native.ABIVersion()
	arch, err := native.CycleArch()
	if err != nil {
		return check{name: "native ABI", note: "cycle counter unavailable: " + err.Error(), required: true}
	}
	return check{name: "native ABI", ok: true,
		note: fmt.Sprintf("abi=%d arch=%s", v, arch.String()), required: true}
}

func checkStdlib() check {
	names := std.Names()
	if len(names) == 0 {
		return check{name: "stdlib", note: "no std modules embedded", required: true}
	}
	for _, n := range names {
		if b, ok := std.Source(n); !ok || len(b) == 0 {
			return check{name: "stdlib", note: "module " + n + " cannot be read", required: true}
		}
	}
	return check{name: "stdlib", ok: true,
		note: strings.Join(names, ",") + " (" + itoa(len(names)) + ")", required: true}
}

func checkGoldens() check {
	dir := "schemas/golden"
	ents, err := os.ReadDir(dir)
	if err != nil {
		// golden 是**开发期**的自检（仓库完整性），发布包里没有它们。
		// 把它判成 FAIL 会让每个用户一装完就看到红色的 doctor —— 而那不是
		// 他们的问题。这里明确标成"不适用"。
		return check{name: "goldens(dev)", ok: true,
			note: "not shipped with the release (dev-repo self-check only)"}
	}
	n := 0
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), "ir-") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return check{name: "goldens", note: "cannot read " + e.Name(), required: true}
		}
		if _, ds := ir.Load(data); len(ds) != 0 {
			// golden 与当前校验器不配套 —— 这通常意味着**校验器变严了**，
			// 而那必须重新生成 golden，不能默默通过。
			return check{name: "goldens", required: true,
				note: fmt.Sprintf("%s failed the current validator (%d diagnostics) - regenerate the golden", e.Name(), len(ds))}
		}
		n++
	}
	if n == 0 {
		return check{name: "goldens", note: "no goldens found", required: true}
	}
	return check{name: "goldens", ok: true, note: itoa(n) + " goldens all loadable", required: true}
}

func checkClang() check {
	p, err := exec.LookPath("clang")
	if err != nil {
		return check{name: "clang", note: "clang not found on PATH (AOT / hermes cc unavailable)", required: true}
	}
	out, _ := exec.Command(p, "--version").Output()
	if !strings.Contains(string(out), "clang") {
		// 这是红线相关：名为 clang 的壳背后可能是 gcc。
		return check{name: "clang", note: p + " does not report clang on --version (policy requires clang only)", required: true}
	}
	first := strings.SplitN(string(out), "\n", 2)[0]
	return check{name: "clang", ok: true, note: first, required: true}
}

// runtimeLibRequired 说明这台机器上"运行时库"是不是必需项。
//
// Windows 上它取决于构建时用的是哪条工具链，而不是平台本身好恶：
//
//	LLVM-MinGW（目标 x86_64-w64-windows-gnu）
//	  clang 走 GNU 味的 ld.lld，**支持可重定位链接（-r）**，所以 Go 的
//	  c-archive 能产出来 —— 此时 C 嵌入库是可用的，缺了就是真问题。
//
//	纯 LLVM（目标 x86_64-pc-windows-msvc）
//	  lld-link 不支持 -r，cgo 探针过不去，于是只能出 CLI。此时降级为 warn，
//	  并**明确告诉用户怎么开启**，而不是假装通过。
//
// 为什么不是 GNU MinGW：它带 GCC 与 libstdc++（GPL）。LLVM-MinGW 是
// Clang/LLVM + MinGW-w64 运行时 + libc++，全程无 GCC —— 两者不能混为一谈。
func runtimeLibRequired() bool {
	return runtime.GOOS != "windows"
}

func checkRuntimeLib() check {
	// 用 layout 解析，而不是写死 `build/`：发布包里它在 `lib/`，
	// 而写死路径会让每个下载安装的用户看到"缺 build/libhermesrt.a"。
	exe, _ := os.Executable()
	cwd, _ := os.Getwd()
	lay := layout.Resolve(exe, cwd)
	if lay.RuntimeLib == "" || lay.RuntimeHeader == "" || lay.IncludeDir == "" {
		if runtimeLibRequired() {
			return check{name: "runtime lib", required: true,
				note: "runtime library/header not found (release should ship lib/ and include/; in dev run scripts/build.sh first)"}
		}
		return check{name: "runtime lib",
			note: "not built on Windows with the msvc target (lld-link has no -r). " +
				"CLI is fully functional; to enable C embedding, build with LLVM-MinGW: " +
				"set LLVM_MINGW_PATH, run `rustup target add x86_64-pc-windows-gnu`, " +
				"then `pwsh -File scripts\\build.ps1 -Toolchain gnu`"}
	}
	return check{name: "runtime lib", ok: true,
		note: lay.RuntimeLib + " + " + filepath.Base(lay.RuntimeHeader), required: true}
}

func checkShim() check {
	p, err := native.ResolveShim("")
	if err != nil {
		// 不是必需：只有需要子进程隔离的执行路径才用它。
		return check{name: "sandbox shim", note: "hermes-shim not found (sandbox unavailable)"}
	}
	return check{name: "sandbox shim", ok: true, note: p}
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

const doctorUsage = `hermes doctor - report whether this machine can run Hermes

Usage:
  hermes doctor

Notes:
  Read-only: it never repairs anything, because silent repair turns "the environment
  is wrong" into "the environment was changed", which is harder to diagnose.

  FAIL means a required capability is missing; warn means optional (for example the
  sandbox shim is only needed for isolated child processes).

Exit codes:
  0  all required checks passed
  4  at least one required check failed
`
