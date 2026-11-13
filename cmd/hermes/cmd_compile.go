package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"hermes/internal/backend"
	"hermes/internal/diag"
	"hermes/internal/ir"
	"hermes/internal/linkpolicy"
	"hermes/internal/objfile"
	"hermes/internal/toolchain"
)

// compile 子命令：`.hermes` → **原生可执行文件**（AOT）。
//
// 只走真正的代码生成后端（默认 `--backend=c`）：IR 直接降为 C，再用本机
// clang 编成机器码。**产物不含 IR、不含解释器** —— 这是「零解释器」红线。
// 旧的「内嵌 IR + libhermesrt.a（Go c-archive 解释器）」路径已删除
// （libhermesrt 仍保留给 `hermes cc` 的 C↔Hermes 互操作与 `hermes run` 的
// 进程内执行，但**不再进入任何编译产物**）。
//
// 链接决策已收敛到 internal/toolchain 单一判定点：
//   - 纯 C ABI：clang -std=c99 编 main.c，永不 clang++ / -x c++；C++ 库必须
//     由用户提供 `extern "C"` 桥接层，直接链 C++ ABI 运行时在输入侧拒绝。
//   - g* 命名空间：含 Go runtime 的库一律以 `g` 开头（glibhermesnet.a）。
//   - 按 Target Triple 隔离：宿主机目标允许 g* 与 Go runtime；裸机目标
//     结构化地斩断一切 g* / 动态库 / 内建网络归档。
//
// D-025 driver 模型不变：不内置编译器，只用本机 clang（GCC 是 GPL-3.0，
// 内置即分发）。静态链 C 运行库同样受红线约束（staticLibcGate，吃 t.GOOS
// 而不是原始 --target 字符串，否则 Linux 三元组会绕过 LGPL 闸门）。
func cmdCompile(args []string) int {
	var srcPath, outPath, target, sysroot string
	// linkStatic / linkDynamic hold user-supplied native library **paths** for
	// the extern / #link native / call_native FFI. They are append-only and are
	// validated by linkInputGate before any compile action. They deliberately do
	// NOT set `static`: `static` is `staticLibcGate`'s whole-program C-runtime
	// flag (glibc LGPL), a completely different red line.
	var linkStatic, linkDynamic []string
	backendName := "c"
	static := false
	// target 为空 = 本机原生目标（交给 clang 自己的默认 triple），不拼 -target。
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "-o", "--out":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, tr("hermes compile: -o needs a file\n\n")+compileUsage)
				return 2
			}
			outPath = args[i+1]
			i++
		case "--backend":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, tr("hermes compile: --backend needs a name\n\n")+compileUsage)
				return 2
			}
			backendName = args[i+1]
			i++
		case "--static":
			static = true
		case "--target":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, tr("hermes compile: --target needs a target triple or GOOS\n\n")+compileUsage)
				return 2
			}
			target = args[i+1]
			i++
		case "--sysroot":
			// 交叉编译 / 裸机目标的 C 运行库与头文件由用户自备；Hermes 不内置
			// 编译器也不内置 libc。没有它时 clang 会给出具名的缺 sysroot 错误。
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, tr("hermes compile: --sysroot needs a directory\n\n")+compileUsage)
				return 2
			}
			sysroot = args[i+1]
			i++
		case "--runtime-lib":
			// 已废弃：AOT 不再链接 Go 运行时。保留解析只为不打断旧脚本/旧用法。
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, tr("hermes compile: --runtime-lib needs a path\n\n")+compileUsage)
				return 2
			}
			i++
		case "--link-static", "--link-dynamic":
			// 只接受本机已有的库文件路径；裸 -lxxx 与 -Wl, 夹带由 linkInputGate 拒。
			// 可重复出现，顺序原样保留（链接器按参数顺序解析静态归档）。
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, tr("hermes compile: %s needs a library path\n\n"), a)
				fmt.Fprint(os.Stderr, compileUsage)
				return 2
			}
			if a == "--link-static" {
				linkStatic = append(linkStatic, args[i+1])
			} else {
				linkDynamic = append(linkDynamic, args[i+1])
			}
			i++
		case "--color":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, tr("hermes compile: --color needs auto|always|never\n\n")+compileUsage)
				return 2
			}
			switch args[i+1] {
			case "auto":
				diagColor = diag.ColorAuto
			case "always":
				diagColor = diag.ColorAlways
			case "never":
				diagColor = diag.ColorNever
			default:
				fmt.Fprintf(os.Stderr, tr("hermes compile: unknown --color %q (want auto|always|never)\n\n%s"), args[i+1], compileUsage)
				return 2
			}
			i++
		case "--color=auto":
			diagColor = diag.ColorAuto
		case "--color=always":
			diagColor = diag.ColorAlways
		case "--color=never":
			diagColor = diag.ColorNever
		case "-h", "--help", "help":
			fmt.Print(compileUsage)
			return 0
		default:
			if strings.HasPrefix(a, "--backend=") {
				backendName = strings.TrimPrefix(a, "--backend=")
				continue
			}
			if srcPath == "" && a != "" && a[0] != '-' {
				srcPath = a
				continue
			}
			fmt.Fprintf(os.Stderr, "hermes compile: %s %q\n\n%s", tr("unknown argument"), a, compileUsage)
			return 2
		}
	}
	if srcPath == "" {
		fmt.Fprint(os.Stderr, tr("hermes compile: missing source file\n\n")+compileUsage)
		return 2
	}
	if outPath == "" {
		base := filepath.Base(srcPath)
		outPath = base[:len(base)-len(filepath.Ext(base))]
	}

	// 目标分类在任何编译动作之前完成：Class 决定 Go runtime 能不能进产物，
	// GOOS 决定 LGPL 静态链闸门吃什么。用 t.GOOS 而不是原始 target 字符串是
	// 关键 —— 否则 `x86_64-unknown-linux-gnu` 会让 staticLibcGate 的
	// `target != "linux"` 判据空转，红线闸门全程发绿灯却什么都没看。
	tgt, err := toolchain.ParseTarget(target, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 4
	}
	if sysroot == "" {
		sysroot = os.Getenv("HERMES_SYSROOT")
	}

	src, err := os.ReadFile(srcPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes compile: cannot read %s: %v\n"), srcPath, err)
		return 4
	}
	doc, err := buildIR(srcPath, src)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes compile: %v\n"), err)
		return 4
	}

	// 裸机守卫：sleep / until / emit ... to 需要宿主 OS（调度器、条件变量、
	// 上下文句柄）。在**链接面**判而不是前端判 —— 前端不认识 target。
	if err := toolchain.CheckHostOnlyNodes(tgt.IsBareMetal(), hostOnlyNodes(doc)); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 4
	}
	// 编译型产物的最后一道闸：非递归函数必须已经在 codegen 展开。
	// 产物里若还留下指向非递归函数的 call_fn，运行期就会变成动态派发 ——
	// 而 "Hermes 的边界在编译期" 是这门语言的定义，不是优化选项。
	if err := checkCompiledStatic(doc); err != nil {
		fmt.Fprintf(os.Stderr, "hermes compile: %v\n", err)
		return 4
	}

	be, ok := backend.For(backendName)
	if !ok {
		fmt.Fprintf(os.Stderr, "hermes compile: unknown backend %q (available: %v)\n", backendName, backend.Names())
		return 2
	}
	csrc, err := be.Generate(doc)
	if err != nil {
		// 这里也是"语言特性还没进原生后端"的出口（如 for/while/try 尚未纳入
		// S0 子集）。如实报错，**绝不**回退到内嵌解释器 —— 那会违背零解释器红线。
		fmt.Fprintf(os.Stderr, tr("hermes compile: %v\n"), err)
		return 4
	}

	// 红线闸门（在任何编译动作之前）：静态链 C 运行库的许可。tgt.GOOS 是
	// 归一化后的 GOOS，三元组 `x86_64-unknown-linux-gnu` 同样会被拦下。
	if err := staticLibcGate(tgt.GOOS, static); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 4
	}
	// 输入侧许可主闸：--link-* 的每一个库路径都过 linkInputGate。放在编译动作
	// 之前、与 staticLibcGate 并列（见 linkInputGate 文档：静态 .a 不产生
	// DT_NEEDED，产物侧查不到，唯一依靠这一道闸）。exit code 4 与产物侧一致。
	if err := gateLinkInputs(linkStatic, linkDynamic); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 4
	}
	// 交叉编译不再被一句 "not supported yet" 挡掉：tgt.ClangTargetArgs() 把
	// -target <triple> 交给 clang，C 运行库由 --sysroot / HERMES_SYSROOT 自备。
	// 宿主目标与裸机目标的链接面差异全部由 toolchain.BuildLinkPlan 决定。

	// 唯一的链接决策点，**前移到工具链探测之前**：一条策略拒绝（例如裸机目标
	// 试图链 g* 库）不该取决于这台机器装没装 clang，也不该先落盘任何临时文件。
	// 裸机目标连 Go 网络库都不去探测、连头文件都不读 —— 这就是"斩断依赖"的结构。
	needNet := backend.UsesNet(doc)
	netLib := ""
	var netHeader []byte
	if needNet && !tgt.IsBareMetal() {
		hdrPath := os.Getenv("HERMES_NET_HEADER")
		if hdrPath == "" {
			hdrPath = filepath.Join("native", "include", "hermes_net.h")
		}
		h, err := os.ReadFile(hdrPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hermes compile: this program calls tools/models but %s is missing: %v\n", hdrPath, err)
			return 4
		}
		netHeader = h
		netLib = os.Getenv("HERMES_NET_LIB")
		if netLib == "" {
			netLib = filepath.Join("build", "glibhermesnet.a")
		}
		if _, err := os.Stat(netLib); err != nil {
			fmt.Fprintf(os.Stderr,
				"hermes compile: this program calls tools/models, so it needs %s.\n"+
					"Build it with: go build -buildmode=c-archive -o build/glibhermesnet.a ./cmd/hermesnet\n", netLib)
			return 4
		}
	}
	plan, err := toolchain.BuildLinkPlan(toolchain.Input{
		Target:      tgt,
		NeedNet:     needNet,
		NetLib:      netLib,
		StaticLibs:  linkStatic,
		DynamicLibs: linkDynamic,
	})
	if err != nil {
		// Refusal.Message 自带完整文案（含用户指定的 [ERROR] 原文），不套
		// "hermes compile:" 前缀，保证逐字输出。
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 4
	}
	// 此后 cmd_compile 不得再自行往 ccArgs 追加任何库：链接面已经封闭。

	// 编译器探测：**不假设 PATH 里有 cc**，也不内置。
	cc, err := exec.LookPath("clang")
	if err != nil {
		fmt.Fprintf(os.Stderr,
			tr("hermes compile: clang not found. The D-025 driver model requires the machine own compiler; Hermes never bundles one\n"))
		return 4
	}
	// 与 check_toolchain.sh 的 1c 段同一条判据：**名字叫 clang 不等于实际是 clang**。
	ver, _ := exec.Command(cc, "--version").Output()
	if !strings.Contains(string(ver), "clang") {
		fmt.Fprintf(os.Stderr,
			tr("hermes compile: %s does not report clang on --version; refusing to use it.\n")+
				"Policy requires clang only: GNU toolchain artifacts carry GPL obligations\n", cc)
		return 4
	}

	dir, err := os.MkdirTemp("", "hermes-aot-")
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes compile: %v\n"), err)
		return 4
	}
	defer os.RemoveAll(dir)

	cfile := filepath.Join(dir, "main.c")
	if err := os.WriteFile(cfile, csrc, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes compile: %v\n"), err)
		return 4
	}
	// 纯 C 后端：生成的 main.c 由 clang -std=c99 编成机器码，**永不**走
	// clang++ / -x c++，所以每个 extern 原型天然是 C linkage（无 name mangling）。
	// 纯 C ABI 是不是真的成立，由「driver」+「输入侧 C++ ABI 闭集」共同保证。
	// 只有宿主机目标且真的需要网络能力时，才把 C ABI 头落盘；裸机产物目录里
	// 连一个 hermes_net.h 都不会出现。
	if netHeader != nil {
		if err := os.WriteFile(filepath.Join(dir, "hermes_net.h"), netHeader, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, tr("hermes compile: %v\n"), err)
			return 4
		}
	}

	// -ffp-contract=off 对**所有** target 生效（放在 -O2 之后，不塞进下面的
	// freestanding 分支）：默认允许 FMA 收缩会让 a*b+c 的结果依赖编译选项，
	// 与 -O2 一起把 double 的位级结果变成"看编译器心情"。永不新增 -ffast-math。
	ccArgs := []string{"-std=c99", "-O2", "-Wall", "-ffp-contract=off"}
	ccArgs = append(ccArgs, tgt.ClangTargetArgs()...)
	if tgt.IsBareMetal() {
		// 裸机没有 hosted 环境：不许 clang 假设 libc 语义，也不许它补
		// hosted-only 内建。目标 C 库由 --sysroot 用户提供。
		ccArgs = append(ccArgs, "-ffreestanding")
	}
	if sysroot != "" {
		ccArgs = append(ccArgs, "--sysroot", sysroot)
	}
	if netHeader != nil {
		ccArgs = append(ccArgs, "-I", dir)
	}
	ccArgs = append(ccArgs, cfile)
	ccArgs = append(ccArgs, plan.StaticLibs...)
	ccArgs = append(ccArgs, plan.DynamicLibs...)
	ccArgs = append(ccArgs, plan.Frameworks...)
	ccArgs = append(ccArgs, "-o", outPath)
	usedDynamicLink := len(plan.DynamicLibs) > 0
	cp := exec.Command(cc, ccArgs...)
	if out, err := cp.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes compile: compilation failed: %v\n%s\n"), err, out)
		return 4
	}
	// 产物侧兜底：链接成功后、return 0 之前再读一次动态依赖。与 cmd_cc.go 的
	// fail-open warning **不同** —— 只要本次用了 --link-dynamic，读不出依赖就
	// fail-closed（绝不把"读不懂"当成"干净"）。裸机产物允许是 raw firmware
	// （objfile.ErrUnsupportedFormat），但若是 ELF/Mach-O 就必须零动态依赖。
	if err := productLinkGate(outPath, usedDynamicLink, tgt, plan); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 4
	}
	// 文案如实区分四种产物：① 链了内建 g* 网络归档的宿主机产物，② 裸机 AOT
	// 产物，③ 通过 --link-static/--link-dynamic 链了用户 g* c-archive 的宿主机
	// 产物，④ 纯计算宿主机产物。四者都零 IR、零解释器；① 与 ③ 含 Go runtime，
	// ② 与 ④ 不含。
	switch {
	case plan.UsedNet:
		fmt.Fprintf(os.Stderr, tr("hermes compile: produced native executable %s (AOT, no IR, no interpreter; tool/model calls link glibhermesnet.a)\n"), outPath)
	case tgt.IsBareMetal():
		fmt.Fprintf(os.Stderr, tr("hermes compile: produced bare-metal AOT artifact %s (no IR, no Go runtime, no interpreter, static C/Rust only)\n"), outPath)
	case len(plan.GoRuntimeLibs) > 0:
		fmt.Fprintf(os.Stderr, tr("hermes compile: produced native executable %s (AOT, no IR, no interpreter; links Go-runtime libraries %v via --link-static/--link-dynamic)\n"), outPath, plan.GoRuntimeLibs)
	default:
		fmt.Fprintf(os.Stderr, tr("hermes compile: produced native executable %s (AOT, no IR, no Go runtime, no interpreter)\n"), outPath)
	}
	return 0
}

var compileUsage = pickUsage(`hermes compile - compile .hermes into a native executable (AOT)

Usage:
  hermes compile <file.hermes> [-o <prog>] [--backend=c]
                 [--target <arch-vendor-os[-env]>] [--sysroot <dir>]
                 [--link-static <path/to/lib.a>] [--link-dynamic <path/to/lib.so|dylib>]

Notes:
  The executable is generated by lowering the IR straight to C and compiling it
  with the machine's own clang. Its AOT artifact embeds no IR and contains no
  interpreter. Driver model (D-025): no bundled compiler.

  --link-static / --link-dynamic link a native library for extern / #link native
  declarations (call_native). Both are repeatable and accept ONLY an existing
  library FILE PATH on this machine (.a / .so / .dylib). Bare -l<name> system
  libraries and -Wl, linker options are refused. The copyleft runtime closed set
  (libstdc++, libsupc++, libgcc_s, libgomp, libquadmath) is refused on input and
  on the produced artifact; there is no HERMES_ALLOW_LINK escape hatch.
  --link-static packs the .a into the executable; --link-dynamic makes the
  artifact carry DT_NEEDED / LC_LOAD_DYLIB. The artifact is always a native
  executable, never a shared library. --link-static does NOT enable --static.

  Pure C ABI only: main.c is compiled by clang -std=c99, never clang++/-x c++,
  so every extern prototype has unmangled C linkage. C++ ABI runtimes
  (libstdc++, libsupc++, libc++, libc++abi, ...) are refused on input. To call a
  C++ library, supply a shim compiled with extern "C" and link that plain C .a.

  --target takes a full LLVM target triple (e.g. x86_64-apple-darwin,
  x86_64-unknown-linux-gnu, thumbv7em-none-eabi) or the current GOOS for a
  native build. Cross-compilation passes -target to clang; --sysroot (or
  HERMES_SYSROOT) supplies the target C runtime. No compiler or libc is bundled.
  A bare GOOS for a *different* OS is refused: it carries no arch/ABI info.

  Go runtime namespace: every library containing a Go runtime MUST start with
  'g' (glibhermesnet.a, gnet.a). Hosted targets may link g* libraries and their
  artifacts may contain a Go runtime. Bare-metal targets (OS field "none")
  forbid ANY g* library, any dynamic library, and the built-in network archive;
  only pure C/Rust static .a is allowed. Violations are a compile-time error:
    [ERROR] Go runtime libraries (g*) cannot be linked for bare-metal targets. Please use pure C/Rust static libraries.

  --backend defaults to "c" (the native code generator). The old
  "embed IR + link libhermesrt.a" path was removed: it shipped an interpreter
  inside the artifact.

  Static linking of the C runtime is refused on Linux by default: glibc is
  LGPL-2.1 and static linking creates obligations (see D-033).

Exit codes:
  0  success
  2  usage error
  4  compilation failed / no clang / unsupported construct / licence gate refused
`,
	`hermes compile —— 把 .hermes 编成原生可执行文件（AOT）

用法:
  hermes compile <file.hermes> [-o <prog>] [--backend=c]
                 [--target <arch-vendor-os[-env]>] [--sysroot <dir>]
                 [--link-static <path/to/lib.a>] [--link-dynamic <path/to/lib.so|dylib>]

说明:
  产物由 IR 直接降为 C、再用本机 clang 编译，**不内嵌 IR、AOT 产物零解释器**。
  driver 模型（D-025）：不内置编译器。

  --link-static / --link-dynamic 为 extern / #link native 声明链原生库，可重复，
  只接受本机已有的库文件**路径**（.a / .so / .dylib）；裸 -l<name> 系统库名与
  -Wl, 选项一律拒绝。copyleft 运行时闭集（libstdc++、libsupc++、libgcc_s、
  libgomp、libquadmath）在输入侧与产物侧都拒绝，没有 HERMES_ALLOW_LINK 逃生舱。
  --link-static 把 .a 打进可执行文件，--link-dynamic 让产物带 DT_NEEDED /
  LC_LOAD_DYLIB；产物永远是原生可执行文件，不是共享库。--link-static 不等于
  --static。

  只认纯 C ABI：main.c 由 clang -std=c99 编译，永不 clang++ / -x c++，所以每个
  extern 原型都是无 name mangling 的 C linkage。C++ ABI 运行时（libstdc++、
  libsupc++、libc++、libc++abi 等）在输入侧拒绝；要调 C++ 库，请自己写
  extern "C" 桥接层，再链那个纯 C 的 .a。

  --target 接受完整 LLVM Target Triple（如 x86_64-apple-darwin、
  x86_64-unknown-linux-gnu、thumbv7em-none-eabi），或本机 GOOS 表示原生编译。
  交叉编译会把 -target 交给 clang；目标 C 运行库用 --sysroot / HERMES_SYSROOT
  自备，Hermes 不内置编译器也不内置 libc。给一个**不同 OS** 的裸 GOOS 会被拒：
  它不含架构与 ABI 信息。

  Go runtime 命名空间：所有含 Go runtime 的库名必须以 g 开头
  （glibhermesnet.a、gnet.a）。宿主机目标允许链 g* 库、产物允许含 Go runtime；
  裸机目标（OS 字段为 none）绝对禁止任何 g* 库、任何动态库以及内建网络归档，
  只准非 g 开头的纯 C/Rust 极简静态 .a。违反时编译期逐字报错：
    [ERROR] Go runtime libraries (g*) cannot be linked for bare-metal targets. Please use pure C/Rust static libraries.

  --backend 默认 "c"（原生代码生成器）。旧的「内嵌 IR + 链 libhermesrt.a」
  路径已删除——那条路会把解释器塞进产物。

  在 Linux 上默认拒绝静态链 C 运行库：glibc 是 LGPL-2.1，静态链会产生义务
  （见 D-033）。

退出码:
  0  成功
  2  用法错误
  4  编译失败 / 找不到 clang / 遇到尚未纳入原生后端的构造 / 红线闸门拒绝
`)

// staticLibcGate 是"静态链 C 运行库"的红线闸门。
//
// goos 必须是**归一化后的 GOOS**（toolchain.Target.GOOS），不是原始
// --target 字符串：传 `x86_64-unknown-linux-gnu` 进来会让 `!= "linux"`
// 恒真、闸门空转发绿灯。TestStaticLibcGateSeesLinuxTriple 钉住这一点。
//
// 规则：
//   - macOS：libSystem 是 BSD/Apple，静态链无义务 → 放行
//   - Linux：默认 glibc 是 LGPL-2.1，**静态链会产生义务** → 拒绝，
//     除非显式 HERMES_ALLOW_GLIBC_STATIC=1（受控例外，必须留痕）
func staticLibcGate(goos string, static bool) error {
	if !static || goos != "linux" {
		return nil
	}
	if os.Getenv("HERMES_ALLOW_GLIBC_STATIC") == "1" {
		return nil
	}
	return fmt.Errorf(`hermes compile: static linking is refused on Linux.
Reason: statically linking glibc (LGPL-2.1) creates LGPL obligations.
Option 1 (recommended): use the musl compiler (libc is MIT) and point CC at it.
Option 2: drop --static and use dynamic linking for glibc, and document that the target machine's glibc is required.
Option 3 (controlled exception): HERMES_ALLOW_GLIBC_STATIC=1 - you assume the licensing judgement yourself`)
}

// linkInputGate 是 `hermes compile --link-*` 的**输入侧**许可主闸，纯函数
// （不读环境变量、不写文件、不做 IO 之外的副作用）：给定 flag 与库路径，
// 判定它能不能进入链接。返回 nil 表示放行。
//
// 规则（与规格修正 2/7 一致）：
//   - 只接受用户本机**已有的库文件路径**（.a / .so / .dylib），用 os.Stat 核实；
//   - 拒绝裸 `-lxxx` 系统库名、`-Wl,` 夹带形式、以及任何以 `-` 开头的链接器选项；
//   - 拒绝闭集里的 copyleft runtime：libstdc++ / libsupc++ / libgcc_s /
//     libgomp / libquadmath（用 filepath.Base + strings.Contains，判据住在
//     internal/linkpolicy，与 `hermes cc` 共用同一份，不写启发式扫描）；
//   - **没有** HERMES_ALLOW_LINK 之类的逃生舱：红线不接受环境变量覆盖。
//
// 为什么输入侧是主闸：--link-static 的 .a 打进可执行文件后不产生 DT_NEEDED，
// 产物侧的 objfile.DynamicLibs 天然查不到它，静态库那份只能靠这一道闸。
func linkInputGate(flag, lib string) error {
	if lib == "" {
		return fmt.Errorf("hermes compile: %s needs a non-empty library file path", flag)
	}
	// 裸 -lxxx / -Lxxx / -Wl,... / 任何以 '-' 开头的选项都不接受：--link-* 的
	// 契约是"本机已有的库文件路径"，不是让系统库名或链接器选项透传。
	if strings.HasPrefix(lib, "-") || strings.Contains(lib, "-Wl,") {
		return fmt.Errorf("hermes compile: %s %q is not a library file path: "+
			"bare -l<name> system libraries and -Wl, linker options are not accepted; "+
			"pass an existing .a / .so / .dylib file on this machine", flag, lib)
	}
	// 纯 C ABI 主闸（ABI 原因，与下面的许可原因分开）：C++ ABI 运行时一律
	// 拒，提示用户写 extern "C" 桥接层。放在许可闭集之前，保证一个错误码只有
	// 一个原因 —— libstdc++ 既踩许可又踩 ABI，这里按 ABI 给出可照抄的修法。
	if err := toolchain.CheckPureCABI(flag, lib); err != nil {
		return err
	}
	if _, rule, ok := linkpolicy.FirstForbidden([]string{lib}); ok {
		return fmt.Errorf("hermes compile: %s %q is a %s runtime (%s): %s. "+
			"The licence red line forbids it from entering the link; "+
			"the closed set is in internal/linkpolicy (source: %s)",
			flag, lib, rule.Name, rule.Licence, rule.Reason, rule.Source)
	}
	if !isNativeLibPath(lib) {
		return fmt.Errorf("hermes compile: %s %q is not a native library "+
			"(expected .a, .so / .so.N, or .dylib)", flag, lib)
	}
	st, err := os.Stat(lib)
	if err != nil {
		return fmt.Errorf("hermes compile: %s %q is not an existing library on this machine: %v", flag, lib, err)
	}
	if st.IsDir() || !st.Mode().IsRegular() {
		return fmt.Errorf("hermes compile: %s %q is not a regular library file", flag, lib)
	}
	return nil
}

// versionedSo 匹配 Linux 上带版本后缀的共享库：libfoo.so.1、libfoo.so.1.2.3。
// 裸 filepath.Ext("libfoo.so.1") 返回 ".1"，会被误拒；这是真实发行版里最常见的
// 动态库命名，不能因为后缀就把它挡在 FFI 门外。
var versionedSo = regexp.MustCompile(`\.so(\.[0-9]+)+$`)

// isNativeLibPath 判定一个路径名是不是可链接的原生库。只认 .a / .dylib /
// .so / .so.N，大小写不敏感（filepath.Base 先小写化）。
func isNativeLibPath(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	if strings.HasSuffix(base, ".a") || strings.HasSuffix(base, ".dylib") ||
		strings.HasSuffix(base, ".so") || versionedSo.MatchString(base) {
		return true
	}
	return false
}

// gateLinkInputs 对两组 --link-* 输入逐个跑 linkInputGate，第一个错误即返回。
// 抽成纯函数便于表驱动单测覆盖"删掉闭集一行就该变红"这类变异。
func gateLinkInputs(linkStatic, linkDynamic []string) error {
	for _, lib := range linkStatic {
		if err := linkInputGate("--link-static", lib); err != nil {
			return err
		}
	}
	for _, lib := range linkDynamic {
		if err := linkInputGate("--link-dynamic", lib); err != nil {
			return err
		}
	}
	return nil
}

// productLinkGate 是**产物侧**兜底：clang 链接成功之后、return 0 之前读产物
// 的动态依赖，用与输入侧同一份闭集判死，并复核目标类别的不变量。
//
// 与 cmd_cc.go:435-438 的 fail-open warning **刻意不同**：cmd_cc 的 link line
// 是闭合的（没有用户提供的库），读不出动态依赖只报警；而这里 --link-* 是用户
// 显式给库的新输入路径，只要本次用了 --link-dynamic，读不出依赖就 fail-closed，
// 绝不把"读不懂"当作"干净"。静态产物（无动态段）返回 []string{}+nil，正常放行。
//
// 裸机例外：raw firmware 不是 ELF/Mach-O，objfile 报 ErrUnsupportedFormat 是
// **正常**（那种产物根本没有动态依赖概念），可以放行；但只要它恰好是
// ELF/Mach-O，就必须零动态依赖、零 g* 名字，否则说明 Go runtime 漏进来了。
func productLinkGate(outPath string, usedDynamicLink bool, tgt toolchain.Target, plan toolchain.Plan) error {
	if tgt.IsBareMetal() {
		// 独立于 BuildLinkPlan 的第二道断言：plan 是别处产出的值，不是这里的
		// 局部变量，所以这条检查能失败。日后谁绕过 BuildLinkPlan 直接往
		// ccArgs 塞库，会在这里红。
		if len(plan.GoRuntimeLibs) != 0 {
			return fmt.Errorf("hermes compile: bare-metal artifact %s has Go runtime libraries in its plan: %v; "+
				"hardware targets must contain no Go runtime at all", outPath, plan.GoRuntimeLibs)
		}
		if len(plan.DynamicLibs) != 0 {
			return fmt.Errorf("hermes compile: bare-metal artifact %s has dynamic libraries in its plan: %v; "+
				"hardware targets allow static .a only", outPath, plan.DynamicLibs)
		}
	}
	libs, err := objfile.DynamicLibs(outPath)
	if err != nil {
		if tgt.IsBareMetal() && errors.Is(err, objfile.ErrUnsupportedFormat) {
			return nil // raw firmware image: no dynamic-loader segment exists
		}
		if usedDynamicLink {
			return fmt.Errorf("hermes compile: cannot read the dynamic dependencies of %s "+
				"after --link-dynamic (%v); refusing a product whose link inputs cannot be verified", outPath, err)
		}
		// 没用 --link-dynamic 时产物只链 libc：读不出依赖仍不放行（不 fail-open），
		// 但给出与"用户显式链库"不同的措辞，便于定位。
		return fmt.Errorf("hermes compile: cannot read the dynamic dependencies of %s (%v); "+
			"the artifact's link surface is unverifiable", outPath, err)
	}
	if bad, rule, ok := linkpolicy.FirstForbidden(libs); ok {
		return fmt.Errorf("hermes compile: %s is linked against %s, a %s runtime (%s): %s. "+
			"The GPL/copyleft runtime must not enter the link (source: %s)",
			outPath, bad, rule.Name, rule.Licence, rule.Reason, rule.Source)
	}
	if tgt.IsBareMetal() {
		if len(libs) > 0 {
			return fmt.Errorf("hermes compile: bare-metal artifact %s carries dynamic dependencies %v; "+
				"hardware targets have no dynamic loader", outPath, libs)
		}
		for _, l := range libs {
			if toolchain.IsGoRuntimeLib(l) {
				return fmt.Errorf("hermes compile: bare-metal artifact %s is linked against %s, "+
					"a g* Go runtime library", outPath, l)
			}
		}
	}
	return nil
}

// checkCompiledStatic 检查产物里没有"指向非递归函数的 call_fn"。
//
// 为什么这条检查放在 AOT 而不是运行期：运行期看到 call_fn 只会照章执行，
// 它无从判断"这个调用本来该在编译期展开"。一旦漏展开，程序仍然能跑，
// 只是悄悄多了动态派发 —— 那种失败没有任何症状，所以必须在编译期断言。
// hostOnlyNodes 把 IR 里所有与宿主 OS 相关的节点摘出来，交给 toolchain 判定。
// 只摘 kind 与 route：判据要的是"用了什么"，不是整棵子树。
func hostOnlyNodes(doc *ir.Doc) []toolchain.HostOnlyKind {
	var out []toolchain.HostOnlyKind
	for wi := range doc.Workflows {
		for ni := range doc.Workflows[wi].Nodes {
			n := &doc.Workflows[wi].Nodes[ni]
			switch n.K {
			case "sleep", "until", "emit":
				st := toolchain.HostOnlyKind{Kind: n.K}
				if n.Route != nil {
					st.Route = *n.Route
				}
				if n.Signal != nil {
					st.Signal = *n.Signal
				}
				out = append(out, st)
			}
		}
	}
	return out
}

func checkCompiledStatic(doc *ir.Doc) error {
	recursive := make(map[string]bool, len(doc.Functions))
	for i := range doc.Functions {
		recursive[doc.Functions[i].Name] = doc.Functions[i].Recursive
	}
	check := func(where string, nodes []ir.Node) error {
		for i := range nodes {
			n := &nodes[i]
			if n.K != "call_fn" || n.Fn == nil {
				continue
			}
			if !recursive[*n.Fn] {
				return fmt.Errorf(
					"%s: call to non-recursive function %q survived codegen "+
						"(compiled mode must inline it); this is a compiler bug", where, *n.Fn)
			}
		}
		return nil
	}
	for wi := range doc.Workflows {
		if err := check("workflow "+doc.Workflows[wi].Name, doc.Workflows[wi].Nodes); err != nil {
			return err
		}
	}
	for fi := range doc.Functions {
		if err := check("function "+doc.Functions[fi].Name, doc.Functions[fi].Nodes); err != nil {
			return err
		}
	}
	return nil
}
