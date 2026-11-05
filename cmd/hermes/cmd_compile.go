package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"hermes/internal/header"
	"hermes/internal/ir"
	"hermes/internal/layout"
)

// compile 子命令：`.hermes` → **原生可执行文件**（AOT）。
//
// 它是 D-025 的 driver 模型：**不内置编译器**，探测用户已有的 clang 并调用它。
// 内置编译器会把 GPL 写进分发链（GCC 是 GPL-3.0），而"调用用户机器上已有的
// 编译器"在任何许可下都不构成分发。
func cmdCompile(args []string) int {
	var srcPath, outPath, libPath, target string
	static := false
	target = runtime.GOOS
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "-o", "--out":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, tr("hermes compile: -o needs a file\n\n")+compileUsage)
				return 2
			}
			outPath = args[i+1]
			i++
		case "--static":
			static = true
		case "--target":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, tr("hermes compile: --target needs a GOOS\n\n")+compileUsage)
				return 2
			}
			target = args[i+1]
			i++
		case "--runtime-lib":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, tr("hermes compile: --runtime-lib needs a path\n\n")+compileUsage)
				return 2
			}
			libPath = args[i+1]
			i++
		case "-h", "--help", "help":
			fmt.Print(compileUsage)
			return 0
		default:
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
	if libPath == "" {
		// 发布包布局（bin/ 与 lib/ 平级）与开发布局（build/）都认；
		// 顺序固定在 internal/layout 里，避免"同一二进制在不同目录行为不同"。
		exe, _ := os.Executable()
		cwd, _ := os.Getwd()
		lay := layout.Resolve(exe, cwd)
		libPath = lay.RuntimeLib
		if err := layout.Require(lay); err != nil {
			fmt.Fprintf(os.Stderr, "hermes compile: %v\n", err)
			return 4
		}
	}
	if _, err := os.Stat(libPath); err != nil {
		fmt.Fprintf(os.Stderr,
			tr("hermes compile: runtime library %s not found. Run scripts/build.sh first (it produces build/libhermesrt.a)\n"), libPath)
		return 4
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
	// 编译型产物的最后一道闸：非递归函数必须已经在 codegen 展开。
	// 产物里若还留下指向非递归函数的 call_fn，运行期就会变成动态派发 ——
	// 而 "Hermes 的边界在编译期" 是这门语言的定义，不是优化选项。
	if err := checkCompiledStatic(doc); err != nil {
		fmt.Fprintf(os.Stderr, "hermes compile: %v\n", err)
		return 4
	}
	irBytes, err := ir.Emit(doc)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes compile: %v\n"), err)
		return 4
	}
	csrc, err := header.GenCExe(doc, irBytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes compile: %v\n"), err)
		return 4
	}

	// 红线闸门（在任何编译动作之前）：静态链 C 运行库的许可 + 跨平台。
	if err := staticLibcGate(target, static); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 4
	}
	if target != runtime.GOOS {
		fmt.Fprintf(os.Stderr,
			tr("hermes compile: cross-compilation is not supported yet (--target %s, host %s).\n")+
				"不假装支持：产物要能在目标平台跑，必须用目标平台的编译器\n", target, runtime.GOOS)
		return 4
	}

	// 编译器探测：**不假设 PATH 里有 cc**，也不内置。
	cc, err := exec.LookPath("clang")
	if err != nil {
		fmt.Fprintf(os.Stderr,
			tr("hermes compile: clang not found. The D-025 driver model requires the machine own compiler; Hermes never bundles one\n"))
		return 4
	}
	// 与 check_toolchain.sh 的 1c 段同一条判据：**名字叫 clang 不等于实际是 clang**。
	// PATH 里放一个名为 clang 的包装脚本、里面 exec gcc，就能一路通过，
	// 而编译日志完全看不出来（驱动名就是 clang）。
	ver, _ := exec.Command(cc, "--version").Output()
	if !strings.Contains(string(ver), "clang") {
		fmt.Fprintf(os.Stderr,
			tr("hermes compile: %s does not report clang on --version; refusing to use it.\n")+
				"红线要求只用 clang 编译：GNU 工具链的产物会带上 GPL 义务\n", cc)
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
	// 运行时库导出的 C 头就在它旁边：`go build -buildmode=c-archive -o X.a`
	// 会生成 **X.h**（跟着 -o 的名字走），不是固定的 hermes_rt.h。
	// 这里把它复制成生成代码里引用的固定名字，免得生成器要知道库叫什么。
	rtHeader := layout.HeaderFor(libPath)
	if _, err := os.Stat(rtHeader); err != nil {
		fmt.Fprintf(os.Stderr,
			"hermes compile: 找不到 %s（运行时库的 C 头）。go build -buildmode=c-archive 会一并生成它\n", rtHeader)
		return 4
	}
	hdrBytes, err := os.ReadFile(rtHeader)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes compile: %v\n"), err)
		return 4
	}
	if err := os.WriteFile(filepath.Join(dir, "hermes_rt.h"), hdrBytes, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes compile: %v\n"), err)
		return 4
	}
	cp := exec.Command(cc, "-std=c89", "-O2", "-I", dir, cfile, libPath, "-o", outPath)
	// macOS 的 Go c-archive 需要 CoreFoundation（运行时）与 Security（TLS 证书校验），
	// 这是 Go 标准库在 darwin 上的链接需求，不是我们的代码引进来的依赖。
	// Linux 上不需要这两行（那边由 glibc/openssl 的静态依赖处理）。
	if runtime.GOOS == "darwin" {
		cp.Args = append(cp.Args, "-framework", "CoreFoundation", "-framework", "Security")
	}
	if out, err := cp.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes compile: compilation failed: %v\n%s\n"), err, out)
		return 4
	}
	fmt.Fprintf(os.Stderr, tr("hermes compile: produced executable %s (does not require hermes to be installed)\n"), outPath)
	return 0
}

var compileUsage = pickUsage(`hermes compile - compile .hermes into a native executable (AOT)

Usage:
  hermes compile <file.hermes> [-o <prog>] [--runtime-lib build/libhermesrt.a] [--static]

Notes:
  The executable embeds the IR and links the runtime archive, so the target machine does
  not need hermes installed. Driver model (D-025): no bundled compiler, the local clang
  is detected and invoked.

  Static linking of the C runtime is refused on Linux by default: glibc is LGPL-2.1 and
  static linking creates obligations (see D-033).

Exit codes:
  0  success
  2  usage error
  4  compilation failed / no clang / no runtime library / licence gate refused
`,
	`hermes compile —— 把 .hermes 编成原生可执行文件（AOT）

用法:
  hermes compile <file.hermes> [-o <prog>] [--runtime-lib build/libhermesrt.a]

说明:
  产物内嵌 IR 并链接运行时静态库，**不需要目标机器安装 hermes**。
  driver 模型（D-025）：不内置编译器，调用本机已有的 clang。

  运行时库由 scripts/build.sh 生成（build/libhermesrt.a + hermes_rt.h）。

退出码:
  0  成功
  2  用法错误
  4  编译失败 / 找不到 clang / 找不到运行时库
`)

// staticLibcGate 是"静态链 C 运行库"的红线闸门。
//
// 为什么必须是一个独立函数而不是内联在命令里：这条规则**要在没有 Linux 的
// 机器上也能被测到**。内联就只能靠"人工在 Linux 上试一次"，而那正是
// 最容易漏的验证方式。现在它是一条纯函数，表驱动测试直接覆盖四种组合。
//
// 规则：
//   - macOS：libSystem 是 BSD/Apple，静态链无义务 → 放行
//   - Linux：默认 glibc 是 LGPL-2.1，**静态链会产生义务** → 拒绝，
//     除非显式 HERMES_ALLOW_GLIBC_STATIC=1（受控例外，必须留痕）
func staticLibcGate(target string, static bool) error {
	if !static || target != "linux" {
		return nil
	}
	if os.Getenv("HERMES_ALLOW_GLIBC_STATIC") == "1" {
		return nil
	}
	return fmt.Errorf(`hermes compile: 拒绝在 Linux 上做静态链接。
原因：Go 的 c-archive 默认静态链 glibc（LGPL-2.1），静态链接会产生 LGPL 义务。
做法一（推荐）：用 musl 编译器（libc 是 MIT），并把 CC 指向它。
做法二：去掉 --static，动态链 glibc，并在交付文档里写明「依赖目标机器的 glibc」。
做法三（受控例外）：HERMES_ALLOW_GLIBC_STATIC=1 —— 开了必须自己承担许可判断`)
}

// checkCompiledStatic 检查产物里没有"指向非递归函数的 call_fn"。
//
// 为什么这条检查放在 AOT 而不是运行期：运行期看到 call_fn 只会照章执行，
// 它无从判断"这个调用本来该在编译期展开"。一旦漏展开，程序仍然能跑，
// 只是悄悄多了动态派发 —— 那种失败没有任何症状，所以必须在编译期断言。
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
