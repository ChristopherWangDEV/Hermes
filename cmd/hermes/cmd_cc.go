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
	"hermes/internal/objfile"
)

// cc 子命令：把**用户自己的 C/C++ 程序**链上 Hermes 产物（T48）。
//
// D-025 的 driver 模型：不内置编译器，只探测 + 调用 + **自解析符号表校验**。
// 不调 `nm`/`dumpbin` —— 那会多一个部署前提，而我们要的只是一个布尔判断。
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
		fmt.Fprint(os.Stderr, tr("hermes cc: need <src.c> --unit <unit.hbc>\n\n")+ccUsage)
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
	defer os.RemoveAll(dir)

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

	args2 := []string{"-std=c89", "-O2", "-I", dir, "-I", filepath.Dir(rtHeader)}
	if st, err := os.Stat(coreInc); err == nil && st.IsDir() {
		args2 = append(args2, "-I", coreInc)
	}
	args2 = append(args2, srcPath, shimPath, corePath, libPath, "-o", outPath)
	if runtime.GOOS == "darwin" {
		args2 = append(args2, "-framework", "CoreFoundation", "-framework", "Security")
	}
	if b, err := exec.Command(cc, args2...).CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes cc: compilation failed: %v\n%s\n"), err, b)
		return 4
	}

	// ---- 自解析符号表校验 ----
	if code := verifyLinkedSymbols(outPath, doc); code != 0 {
		return code
	}
	fmt.Fprintf(os.Stderr, tr("hermes cc: produced %s, symbols verified\n"), outPath)
	return 0
}

// verifyLinkedSymbols 检查最终可执行文件：期望的 ABI 符号**确实定义了**，
// 且没有 C++ 修饰名混进来（D-025：mangled 即硬失败）。
func verifyLinkedSymbols(path string, doc *ir.Doc) int {
	syms, err := objfile.Read(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes cc: cannot read symbols: %v\n"), err)
		return 4
	}
	unit := header.Sanitize(unitName(doc))
	for i := range doc.Workflows {
		w := &doc.Workflows[i]
		if !w.Exported {
			continue
		}
		want := "hermes_" + unit + "_" + header.Sanitize(w.Name)
		if runtime.GOOS == "darwin" {
			want = objfile.MachOName(want)
		}
		if !syms.Defined[want] {
			fmt.Fprintf(os.Stderr, tr("hermes cc: symbol %s is referenced but not defined -- the runtime library and the unit disagree\n"), want)
			return 4
		}
	}
	for name := range syms.Undefined {
		if objfile.Mangled(name) {
			fmt.Fprintf(os.Stderr, tr("hermes cc: mangled symbol %s leaked in; a C++ translation unit (and likely libstdc++) got linked\n"), name)
			return 4
		}
	}
	return 0
}

func unitName(doc *ir.Doc) string { return doc.Unit.Name }

const ccUsage = `hermes cc - link your own C program against a Hermes unit (T48 / D-025)

Usage:
  hermes cc <src.c> --unit <unit.hbc> [-o <prog>] [--runtime-lib build/libhermesrt.a]

Notes:
  Driver model: no bundled compiler; the local clang is detected and invoked.
  After linking, the symbol table is parsed **by us** (debug/elf, debug/macho) --
  nm / dumpbin are never called: they add a deployment prerequisite and a text
  format to parse, while we only need a yes/no answer.

  Mangled symbols are a hard failure: they mean a C++ translation unit, and
  usually libstdc++, got linked in -- a licence problem, not just an ABI one.

Exit codes:
  0  success
  2  usage error
  4  compilation failed / symbol mismatch / mangled symbol found
`
