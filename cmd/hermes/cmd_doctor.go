package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
		return check{name: "native ABI", note: "周期计数器不可用: " + err.Error(), required: true}
	}
	return check{name: "native ABI", ok: true,
		note: fmt.Sprintf("abi=%d arch=%s", v, arch.String()), required: true}
}

func checkStdlib() check {
	names := std.Names()
	if len(names) == 0 {
		return check{name: "stdlib", note: "没有内嵌任何 std 模块", required: true}
	}
	for _, n := range names {
		if b, ok := std.Source(n); !ok || len(b) == 0 {
			return check{name: "stdlib", note: "模块 " + n + " 读不出来", required: true}
		}
	}
	return check{name: "stdlib", ok: true,
		note: strings.Join(names, ",") + "（" + itoa(len(names)) + " 个）", required: true}
}

func checkGoldens() check {
	dir := "schemas/golden"
	ents, err := os.ReadDir(dir)
	if err != nil {
		// golden 是**开发期**的自检（仓库完整性），发布包里没有它们。
		// 把它判成 FAIL 会让每个用户一装完就看到红色的 doctor —— 而那不是
		// 他们的问题。这里明确标成"不适用"。
		return check{name: "goldens(dev)", ok: true,
			note: "未随发布包提供（开发仓库里的自检项）"}
	}
	n := 0
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), "ir-") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return check{name: "goldens", note: "读不到 " + e.Name(), required: true}
		}
		if _, ds := ir.Load(data); len(ds) != 0 {
			// golden 与当前校验器不配套 —— 这通常意味着**校验器变严了**，
			// 而那必须重新生成 golden，不能默默通过。
			return check{name: "goldens", required: true,
				note: fmt.Sprintf("%s 没过当前校验器（%d 条）——重新生成 golden", e.Name(), len(ds))}
		}
		n++
	}
	if n == 0 {
		return check{name: "goldens", note: "一份 golden 都没有", required: true}
	}
	return check{name: "goldens", ok: true, note: itoa(n) + " 份全部可加载", required: true}
}

func checkClang() check {
	p, err := exec.LookPath("clang")
	if err != nil {
		return check{name: "clang", note: "PATH 里没有 clang（AOT / hermes cc 不可用）", required: true}
	}
	out, _ := exec.Command(p, "--version").Output()
	if !strings.Contains(string(out), "clang") {
		// 这是红线相关：名为 clang 的壳背后可能是 gcc。
		return check{name: "clang", note: p + " 的 --version 不是 clang（红线要求只用 clang）", required: true}
	}
	first := strings.SplitN(string(out), "\n", 2)[0]
	return check{name: "clang", ok: true, note: first, required: true}
}

func checkRuntimeLib() check {
	// 用 layout 解析，而不是写死 `build/`：发布包里它在 `lib/`，
	// 而写死路径会让每个下载安装的用户看到"缺 build/libhermesrt.a"。
	exe, _ := os.Executable()
	cwd, _ := os.Getwd()
	lay := layout.Resolve(exe, cwd)
	if lay.RuntimeLib == "" || lay.RuntimeHeader == "" || lay.IncludeDir == "" {
		return check{name: "runtime lib", required: true,
			note: "找不到运行时库 / 头（发布包应在 lib/ 与 include/；开发时先跑 scripts/build.sh）"}
	}
	return check{name: "runtime lib", ok: true,
		note: lay.RuntimeLib + " + " + filepath.Base(lay.RuntimeHeader), required: true}
}

func checkShim() check {
	p, err := native.ResolveShim("")
	if err != nil {
		// 不是必需：只有需要子进程隔离的执行路径才用它。
		return check{name: "sandbox shim", note: "没找到 hermes-shim（需要沙箱时不可用）"}
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
