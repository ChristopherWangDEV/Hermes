package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"hermes/internal/archive"
	"hermes/internal/header"
	"hermes/internal/ir"
	"hermes/internal/layout"
	"hermes/internal/linkpolicy"
	"hermes/internal/objfile"
)

// cc 子命令：把**用户自己的 C/C++ 程序**链上 Hermes 产物（T48）。
//
// D-025 的 driver 模型：不内置编译器，只探测 + 调用 + **自解析符号表校验**。
// 不调 `nm`/`dumpbin` —— 那会多一个部署前提，而我们要的只是一个布尔判断。
//
// 两种源语言，两条链：
//
//	C：一条 invoke、`-std=c89`。**这条路径一个字都不许动** —— 既有 golden 与
//	   T48 的验收都踩在它上面。
//	C++：两遍。clang 的 `-std` 不是 positional，一个 invoke 里混 C 与 C++ 不可能
//	   （实测 `error: invalid argument '-std=c89' not allowed with 'C++'`），
//	   所以先把 unit shim 与 core impl **当 C** 编成 .o，再用 C++ 驱动链接。
func cmdCC(args []string) int {
	var srcPath, outPath, unitPath, libPath string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "-o", "--out":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, tr("hermes cc: -o needs a file\n\n")+ccUsage)
				return 2
			}
			outPath = args[i+1]
			i++
		case "--unit":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, tr("hermes cc: --unit needs an .hbc\n\n")+ccUsage)
				return 2
			}
			unitPath = args[i+1]
			i++
		case "--runtime-lib":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, tr("hermes cc: --runtime-lib needs a path\n\n")+ccUsage)
				return 2
			}
			libPath = args[i+1]
			i++
		case "-h", "--help", "help":
			fmt.Print(ccUsage)
			return 0
		default:
			if srcPath == "" && a != "" && a[0] != '-' {
				srcPath = a
				continue
			}
			fmt.Fprintf(os.Stderr, tr("hermes cc: unknown argument %q\n\n%s"), a, ccUsage)
			return 2
		}
	}
	if srcPath == "" || unitPath == "" {
		fmt.Fprint(os.Stderr, tr("hermes cc: need <src.c|src.cpp> --unit <unit.hbc>\n\n")+ccUsage)
		return 2
	}
	// 语言由**扩展名**决定，不是由内容猜。猜错方向的代价不对称：把 C++ 当 C 编
	// 只会得到一堆语法错，把 C 当 C++ 编会静默改变链接面（名字修饰、异常表）。
	lang, err := langOf(srcPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermes cc: %v\n", err)
		return 2
	}
	if outPath == "" {
		outPath = "a.out"
	}
	if libPath == "" {
		exe, _ := os.Executable()
		cwd, _ := os.Getwd()
		lay := layout.Resolve(exe, cwd)
		libPath = lay.RuntimeLib
		if err := layout.Require(lay); err != nil {
			fmt.Fprintf(os.Stderr, "hermes cc: %v\n", err)
			return 4
		}
	}
	if _, err := os.Stat(libPath); err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes cc: runtime library %s not found; run scripts/build.sh first\n"), libPath)
		return 4
	}

	// 读 unit，生成配套头 —— 用户程序 include 的就是它。
	data, err := os.ReadFile(unitPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes cc: cannot read %s: %v\n"), unitPath, err)
		return 4
	}
	if len(data) >= 4 && string(data[0:4]) == archive.Magic {
		a, err := archive.Read(data)
		if err != nil {
			fmt.Fprintf(os.Stderr, tr("hermes cc: %s is not a usable archive: %v\n"), unitPath, err)
			return 4
		}
		data = a.Members[0].IR
	}
	doc, ds := ir.Load(data)
	if len(ds) != 0 {
		fmt.Fprintf(os.Stderr, tr("hermes cc: IR is invalid (%d diagnostics)\n"), len(ds))
		return 4
	}
	hdr, err := header.GenC(doc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermes cc: %v\n", err)
		return 4
	}
	// 生成 unit 的 C 实现层：没有它，用户程序能编过却链不上
	// （头承诺 per-unit 符号，运行时库只有通用入口）。
	shim, err := header.GenCShim(doc, data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermes cc: %v\n", err)
		return 4
	}

	cc, err := exec.LookPath("clang")
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes cc: clang not found; Hermes never bundles a compiler\n"))
		return 4
	}
	ver, _ := exec.Command(cc, "--version").Output()
	if !strings.Contains(string(ver), "clang") {
		fmt.Fprintf(os.Stderr, tr("hermes cc: %s does not report clang on --version; refusing\n"), cc)
		return 4
	}

	dir, err := os.MkdirTemp("", "hermes-cc-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermes cc: %v\n", err)
		return 4
	}
	// HERMES_CC_KEEP=1 时保留临时目录并打印路径：driver 的失败现场（生成的头、
	// 编出来的 .o、实际 link line）是排查"为什么链不上"的唯一证据，而默认删掉
	// 它意味着每个 C++ 链接问题都得靠猜。
	if os.Getenv("HERMES_CC_KEEP") != "" {
		fmt.Fprintf(os.Stderr, tr("hermes cc: keeping work dir %s (HERMES_CC_KEEP)\n"), dir)
	} else {
		defer os.RemoveAll(dir)
	}

	corePath := filepath.Join(dir, "hermes_core_impl.c")
	if err := os.WriteFile(corePath, []byte(header.CoreImpl), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "hermes cc: %v\n", err)
		return 4
	}
	shimPath := filepath.Join(dir, "unit_shim.c")
	if err := os.WriteFile(shimPath, shim, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "hermes cc: %v\n", err)
		return 4
	}
	unitHeader := filepath.Join(dir, strings.TrimSuffix(filepath.Base(unitPath), ".hbc")+".h")
	if err := os.WriteFile(unitHeader, hdr, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "hermes cc: %v\n", err)
		return 4
	}
	// .hpp：C++ 用户会写 `#include "<unit>.hpp"`。**两种语言都落盘** ——
	// 让"用户把 .hbc 起了什么名字"决定磁盘上有没有 .hpp，是一处纯粹靠猜的约定。
	// .h 本身带 extern "C" 守卫，C++ 里直接 include 也合法；.hpp 只是多给一个
	// 带 opt-in `namespace hermes::<unit>` 包装的入口。
	if code := writeUnitHPP(dir, doc, unitPath); code != 0 {
		return code
	}
	rtHeader := layout.HeaderFor(libPath)
	if _, err := os.Stat(rtHeader); err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes cc: %s not found (the C header next to the runtime archive)\n"), rtHeader)
		return 4
	}

	// hermes_core.h 在仓库的 include/ 下，而运行时库在 build/ 下 —— 生成的 unit 头
	// include 的是 "hermes/hermes_core.h"，所以必须把 include/ 也指给编译器。
	// 少了这一条，用户会看到 "hermes/hermes_core.h not found"，而我们刚生成的头
	// 就在眼前，那种错最容易被误判成"头生成坏了"。
	// include 根的来源：先看运行时库自己的位置（`--runtime-lib` 可能指到别处），
	// 再看可执行文件的布局。
	coreInc := layout.IncludeNear(libPath)
	if coreInc == "" {
		exe2, _ := os.Executable()
		cwd2, _ := os.Getwd()
		coreInc = layout.Resolve(exe2, cwd2).IncludeDir
	}
	// 运行时库的 C 头叫 <archive>.h，而 shim 里引用的是固定名 hermes_rt.h。
	// 复制一份，免得 shim 生成器要知道库叫什么。
	rtBytes, err := os.ReadFile(rtHeader)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermes cc: %v\n", err)
		return 4
	}
	if err := os.WriteFile(filepath.Join(dir, "hermes_rt.h"), rtBytes, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "hermes cc: %v\n", err)
		return 4
	}

	commonInc := []string{"-I", dir, "-I", filepath.Dir(rtHeader)}
	if st, err := os.Stat(coreInc); err == nil && st.IsDir() {
		commonInc = append(commonInc, "-I", coreInc)
	}

	if lang == langC {
		// ---- C：单条 invoke ----
		// 不许"顺手统一"成两遍编译：一次 invoke 里 clang 会自己做 C 的
		// 语义与链接，多出来的 .o 只会让产物多一份临时路径。
		// -ffp-contract=off：禁止 FMA 收缩，保证 double 的位级结果不随编译选项漂移。
		args2 := append([]string{"-std=c89", "-O2", "-ffp-contract=off"}, commonInc...)
		args2 = append(args2, srcPath, shimPath, corePath, libPath, "-o", outPath)
		if runtime.GOOS == "darwin" {
			args2 = append(args2, "-framework", "CoreFoundation", "-framework", "Security")
		}
		if b, err := exec.Command(cc, args2...).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, tr("hermes cc: compilation failed: %v\n%s\n"), err, b)
			return 4
		}
	} else {
		// ---- C++：第一遍把 shim 与 core impl **当 C** 编成 .o ----
		// 它们必须是 C：头里承诺的就是未修饰的 hermes_<unit>_<wf>，
		// 用 C++ 编会得到 _Z… 修饰名，头里声明的名字当场链不上。
		shimObj := filepath.Join(dir, "unit_shim.o")
		coreObj := filepath.Join(dir, "hermes_core_impl.o")
		steps := []struct{ src, obj, what string }{
			{shimPath, shimObj, "unit shim"},
			{corePath, coreObj, "runtime core impl"},
		}
		for _, st := range steps {
			// -ffp-contract=off 同样作用于按 C 前置编译的 shim / core impl。
			cargs := append([]string{"-std=c89", "-O2", "-ffp-contract=off", "-c"}, commonInc...)
			cargs = append(cargs, st.src, "-o", st.obj)
			if b, err := exec.Command(cc, cargs...).CombinedOutput(); err != nil {
				fmt.Fprintf(os.Stderr,
					tr("hermes cc: cannot compile the %s as C: %v\n%s\n"), st.what, err, b)
				return 4
			}
		}

		// ---- 第二遍：C++ 驱动编译并链接用户源 ----
		args2 := ccArgsCPP(srcPath, []string{shimObj, coreObj}, libPath, outPath, dir, coreInc, filepath.Dir(rtHeader))
		if b, err := exec.Command(cc, args2...).CombinedOutput(); err != nil {
			// 把实际命令行打出来：driver 的报错几乎都在讲"符号找不到"，
			// 而人真正需要看的是"我们到底传了什么开关"。只在失败时打，不污染成功路径。
			fmt.Fprintf(os.Stderr, tr("hermes cc: link line: %s %s\n"), cc, strings.Join(args2, " "))
			fmt.Fprintf(os.Stderr, tr("hermes cc: compilation failed: %v\n%s\n"), err, b)
			if hint := cxxStdlibHint(b); hint != "" {
				fmt.Fprintf(os.Stderr, "%s\n", hint)
			}
			return 4
		}
	}

	// ---- 自解析符号表 + 动态依赖校验 ----
	if code := verifyLinkedSymbols(outPath, doc); code != 0 {
		return code
	}
	fmt.Fprintf(os.Stderr, tr("hermes cc: produced %s, symbols verified\n"), outPath)
	return 0
}

// ccLang 是用户源文件的语言。
type ccLang int

const (
	langC ccLang = iota
	langCPP
)

// langOf 按扩展名判定语言。
//
// `.C` 是**大写的 C++**（几十年历史约定），所以这里不能先 ToLower 再比 ——
// 那会把 `.C` 认成 C，症状是 C++ 源码被 `-std=c89` 编成一堆语法错，而报错
// 里一个字都不会提"你其实该走 C++ 路径"。
func langOf(path string) (ccLang, error) {
	switch filepath.Ext(path) {
	case ".c":
		return langC, nil
	case ".cpp", ".cc", ".cxx", ".c++", ".C":
		return langCPP, nil
	default:
		return 0, fmt.Errorf(
			"%s: only C/C++ sources are accepted (.c / .cpp / .cc / .cxx / .c++ / .C)", path)
	}
}

// ccArgsCPP 是 C++ 路径的编译 + 链接命令行。
//
// 抽成**纯函数**的理由：许可红线（libstdc++/libsupc++ 不得进入链接）落在这里，
// 而"哪天有人顺手加个 -lstdc++ 让某个用户编过"是这类红线最典型的死法。
// 纯函数才能在测试里逐字断言"某些开关永不出现"。它唯一的非参数输入是
// `runtime.GOOS`，那是编译期常量，测试在任何平台上的期望都是确定的。
//
// **为什么第一个参数是 `--driver-mode=g++` 而不是换成 `clang++`**：
//
//	实测（Apple clang 21 / arm64 darwin）：`clang -std=c++17 -stdlib=libc++ -x c++ x.cpp`
//	会**不链 libc++**，症状是一串 `operator new` / `___cxa_throw` /
//	`___gxx_personality_v0` 未定义，而报错只字不提"没有 C++ 运行时"。
//	同一个二进制加上 `--driver-mode=g++` 就正常。这比调 `clang++` 更好：
//	可执行文件仍然是那个已经过 `--version` 探测的 clang，没有新增探测路径
//	（红线：只用 clang，且"名字叫 clang"必须由编译器自己承认）。
func ccArgsCPP(srcPath string, objs []string, libPath, outPath, dir, coreInc, rtDir string) []string {
	// -ffp-contract=off：C++ 主编译也禁止 FMA 收缩，与 C 路径口径一致。
	args := []string{"--driver-mode=g++", "-std=c++17", "-stdlib=libc++", "-O2", "-ffp-contract=off", "-I", dir, "-I", rtDir}
	if coreInc != "" {
		args = append(args, "-I", coreInc)
	}
	// `-x c++` 只作用于**紧随其后的输入**，所以 srcPath 之后必须立刻收回
	// （`-x none`）。少了这一句，后面的 .o 与 .a 会被当成 C++ 源码再编一遍，
	// 症状是"编译 .o 时报一个莫名其妙的语法错"。
	args = append(args, "-x", "c++", srcPath, "-x", "none")
	args = append(args, objs...)
	args = append(args, libPath, "-o", outPath)
	if runtime.GOOS == "darwin" {
		// 运行时库（Go c-archive）用到 CoreFoundation/Security。
		args = append(args, "-framework", "CoreFoundation", "-framework", "Security")
	}
	return args
}

// cxxStdlibHint 在 C++ 编译失败时补一句人话。
//
// Linux 上没装 libc++ 的诊断是"找不到某个 C++ 标准头"，那句报错完全看不出
// 真正的原因（我们**故意**不用系统默认的 libstdc++）。所以这里把它翻译成
// "libstdc++ 因 GPL 不可用，请装 libc++"，而不是让它静默回落到 libstdc++。
func cxxStdlibHint(out []byte) string {
	s := strings.ToLower(string(out))
	for _, needle := range []string{"libc++", "cstddef", "stdlib", "c++ standard library"} {
		if strings.Contains(s, needle) {
			return tr("hermes cc: this link uses -stdlib=libc++ (libstdc++ is GPL and not allowed by the red line).\n" +
				"  If this machine has no libc++: install the libc++ development package\n" +
				"  (Linux: libc++-dev / libc++-N-dev; macOS ships it), or compile your C++ source\n" +
				"  yourself with clang -stdlib=libc++ and link the objects.\n" +
				"  Hermes will **not** fall back to libstdc++ for you.")
		}
	}
	return ""
}

// gplRuntime 返回第一个把 copyleft 运行时带进链接的动态依赖名；空串表示干净。
//
// 判据本身住在 internal/linkpolicy 的**闭集**里，`hermes cc` 与
// `hermes compile --link-*` 共用同一份，避免两处各写一份启发式扫描而漂移。
// 红线是「它不许进入链接」，所以判据落在库名上，而不是符号长什么样
// （见 internal/objfile/dynamiclibs.go）。这里只做薄适配，保留原来的
// `[]string -> 第一个坏库名` 契约给 verifyLinkedSymbols 与单测使用。
func gplRuntime(libs []string) string {
	bad, _, ok := linkpolicy.FirstForbidden(libs)
	if !ok {
		return ""
	}
	return bad
}

// isUnwinderOnly 判断这条动态依赖是不是"只为了异常展开"进来的 libgcc_s。
//
// 为什么需要这个区分：Debian/Ubuntu 的 libc++abi 是**用 GCC 展开器**构建的，
// 所以任何 `-stdlib=libc++` 链出来的程序，DT_NEEDED 里都会出现 libgcc_s.so.1 ——
// 即使一个 libstdc++ 符号都没用到。这不是"用了 C++ 标准库的 GPL 实现"，
// 而是"标准库实现自己挑的展开器"。
//
// 判据是**两条同时成立**：
//  1. 名字是 libgcc_s（不是 libstdc++）
//  2. 同一次链接里确实有 libc++（证明 C++ 运行时来自 libc++，不是 libstdc++）
//
// 只满足第一条就放行会把"真的链了 libstdc++"也放过去 —— 那才是红线要挡的。
func isUnwinderOnly(lib string, all []string) bool {
	if !strings.Contains(lib, "libgcc_s") {
		return false
	}
	for _, l := range all {
		if strings.Contains(l, "libc++") || strings.Contains(l, "libc++abi") {
			return true
		}
	}
	return false
}

// writeUnitHPP 往临时目录写 unit 的 `.hpp`。
//
// 写两份名字（`.hbc` 的文件名主干 与 IR 里的 unit 名），因为用户会照着其中任意
// 一个写 `#include`：`hello.hbc` → `hello.h` + `hello.hpp`，而 C 符号永远是
// `hermes_<unit>_<wf>`。两份内容相同，代价是两个文件，收益是"不用猜"。
func writeUnitHPP(dir string, doc *ir.Doc, unitPath string) int {
	hpp, err := header.GenCPP(doc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermes cc: %v\n", err)
		return 4
	}
	stems := []string{strings.TrimSuffix(filepath.Base(unitPath), ".hbc")}
	if u := header.Sanitize(doc.Unit.Name); u != stems[0] {
		stems = append(stems, u)
	}
	for _, stem := range stems {
		if err := os.WriteFile(filepath.Join(dir, stem+".hpp"), hpp, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "hermes cc: %v\n", err)
			return 4
		}
	}
	return 0
}

// verifyLinkedSymbols 检查最终可执行文件，两件事：
//
//  1. 每个导出 workflow 的 `hermes_<unit>_<wf>` **确实定义了**，而且不是以修饰名
//     形式定义的 —— 那是"头里声明的名字链不上"的唯一可靠信号，也是 shim 被当
//     C++ 编译时的症状。
//  2. **进了链接的 C++ 运行时没有 GPL 那一份**（libstdc++ / libsupc++）。
//     判据落在动态依赖上，不落在符号形态上：libc++ 也产 mangled 符号（一个用
//     <string> 的合法 C++ TU 必然如此），而静态 GPL 归档里的符号根本不 mangled。
//     拿符号名当许可判据，两头都不对。
func verifyLinkedSymbols(path string, doc *ir.Doc) int {
	syms, err := objfile.Read(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes cc: cannot read symbols: %v\n"), err)
		return 4
	}
	unit := header.Sanitize(unitName(doc))

	// 一次收集，别在每个 workflow 里重扫整张表。
	mangledDefined := make([]string, 0, 8)
	for name := range syms.Defined {
		if objfile.Mangled(name) {
			mangledDefined = append(mangledDefined, name)
		}
	}

	for i := range doc.Workflows {
		w := &doc.Workflows[i]
		if !w.Exported {
			continue
		}
		cname := "hermes_" + unit + "_" + header.Sanitize(w.Name)
		want := cname
		if runtime.GOOS == "darwin" {
			want = objfile.MachOName(cname)
		}
		if !syms.Defined[want] {
			fmt.Fprintf(os.Stderr, tr("hermes cc: symbol %s is referenced but not defined -- the runtime library and the unit disagree\n"), want)
			return 4
		}
		// 修饰名里嵌着原文（Itanium：`_Z16hermes_hello_hellov`），所以用子串判定。
		// 只查**我们导出的那几个名字**：用户自己的 C++ 函数当然是 mangled 的，
		// 而那不是问题 —— 过去那段"任何 mangled 即失败"正是这么误杀 C++ 用户的。
		for _, m := range mangledDefined {
			if strings.Contains(m, cname) {
				fmt.Fprintf(os.Stderr,
					tr("hermes cc: %s is defined as the mangled name %s -- the unit shim was compiled as C++, "+
						"so the name declared in the header (hermes_%s_%s) is not what got linked\n"),
					cname, m, unit, header.Sanitize(w.Name))
				return 4
			}
		}
	}

	libs, err := objfile.DynamicLibs(path)
	if err != nil {
		// 读不出动态依赖不等于"链了 GPL 运行时"：静态产物没有动态段，而
		// cc 不传任何 -l，libstdc++ 只可能随某个库进来。所以这里报警不判死。
		fmt.Fprintf(os.Stderr, tr("hermes cc: warning: cannot read dynamic dependencies of %s: %v\n"), path, err)
		return 0
	}
	if bad := gplRuntime(libs); bad != "" {
		if isUnwinderOnly(bad, libs) {
			// 展开器不是 C++ 标准库：libc++abi 用它做栈展开，GCC Runtime Library
			// Exception 覆盖这一用途。这里如实说明，而不是假装没这回事。
			fmt.Fprintf(os.Stderr, tr("hermes cc: note: %s pulls %s as its unwinder (libc++abi on this platform is built against it); "+
				"the C++ standard library in this link is libc++, not libstdc++\n"), path, bad)
		} else {
			fmt.Fprintf(os.Stderr, tr("hermes cc: %s is linked against %s, which is GPL -- "+
				"the GPL C++ runtime must not enter the link; use libc++ (-stdlib=libc++) or compile your C++ source yourself\n"),
				path, bad)
			return 4
		}
	}
	return 0
}

func unitName(doc *ir.Doc) string { return doc.Unit.Name }

const ccUsage = `hermes cc - link your own C/C++ program against a Hermes unit (T48 / D-025)

Usage:
  hermes cc <src.c|src.cpp> --unit <unit.hbc> [-o <prog>] [--runtime-lib build/libhermesrt.a]

Notes:
  Driver model: no bundled compiler; the local clang is detected and invoked.
  Language comes from the extension: .c is C, .cpp/.cc/.cxx/.c++/.C is C++.
  (C++ needs two passes -- clang's -std is not positional, so a single invoke
  cannot mix a C89 shim with a C++17 translation unit.)

  C++ is linked with -stdlib=libc++ on purpose. The licence red line is a
  *closed set* of copyleft runtimes (libstdc++, libsupc++, libgcc_s, libgomp,
  libquadmath -- see internal/linkpolicy, shared with hermes compile --link-*),
  not a heuristic scan and not a guess from symbol names: mangled symbols are
  normal for any C++ unit using libc++, while a static GPL archive's symbols
  are not mangled at all. So hermes cc scans the product's dynamic dependencies
  (LC_LOAD_DYLIB / DT_NEEDED) after linking and fails if one of those runtimes
  is in the link.

  After linking, the symbol table is parsed **by us** (debug/elf, debug/macho) --
  nm / dumpbin are never called: they add a deployment prerequisite and a text
  format to parse, while we only need a yes/no answer.

Exit codes:
  0  success
  2  usage error (including an unsupported source extension)
  4  compilation failed / symbol mismatch / GPL C++ runtime linked in
`
