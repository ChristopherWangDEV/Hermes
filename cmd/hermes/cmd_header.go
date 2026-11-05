package main

import (
	"fmt"
	"os"

	"hermes/internal/archive"
	"hermes/internal/header"
	"hermes/internal/ir"
)

// header 子命令：从 IR 生成对外 C ABI 头（T47）。
//
// 产物是**给 C/C++ 编译器读的**，与 HermesScript 的 `.h` 同名不同目录、语法不同
// （D-024）。同一份 IR 出两个头："共用同用"落在事实源，不落在文件。
func cmdHeader(args []string) int {
	var irPath, outPath, lang string
	lang = "c"
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--c":
			lang = "c"
		case "--cpp":
			lang = "cpp"
		case "-o", "--out":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, tr("hermes header: -o needs a file\n\n")+headerUsage)
				return 2
			}
			outPath = args[i+1]
			i++
		case "-h", "--help", "help":
			fmt.Print(headerUsage)
			return 0
		default:
			if irPath == "" && a != "" && a[0] != '-' {
				irPath = a
				continue
			}
			fmt.Fprintf(os.Stderr, "hermes header: %s %q\n\n%s", tr("unknown argument"), a, headerUsage)
			return 2
		}
	}
	if irPath == "" {
		fmt.Fprint(os.Stderr, tr("hermes header: missing IR / .hbc\n\n")+headerUsage)
		return 2
	}

	data, err := os.ReadFile(irPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes header: cannot read %s: %v\n"), irPath, err)
		return 4
	}
	if len(data) >= 4 && string(data[0:4]) == archive.Magic {
		a, err := archive.Read(data)
		if err != nil {
			fmt.Fprintf(os.Stderr, tr("hermes header: %s is not a usable archive: %v\n"), irPath, err)
			return 4
		}
		data = a.Members[0].IR
	}
	doc, ds := ir.Load(data)
	if len(ds) != 0 {
		fmt.Fprintf(os.Stderr, tr("hermes header: IR is invalid (%d diagnostics)\n"), len(ds))
		return 4
	}

	var out []byte
	if lang == "cpp" {
		out, err = header.GenCPP(doc)
	} else {
		out, err = header.GenC(doc)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes header: %v\n"), err)
		return 4
	}
	if outPath == "" {
		os.Stdout.Write(out)
		return 0
	}
	if err := os.WriteFile(outPath, out, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes header: cannot write %s: %v\n"), outPath, err)
		return 4
	}
	fmt.Fprintf(os.Stderr, tr("hermes header: wrote %s (%d bytes)\n"), outPath, len(out))
	return 0
}

var headerUsage = pickUsage(`hermes header - generate the public C ABI header from IR (T47 / D-024)

Usage:
  hermes header <unit.ir.json | unit.hbc> --c [-o out.h]

Notes:
  The output is for a C/C++ compiler: C89 + <stdint.h>, including only the handwritten,
  frozen hermes/hermes_core.h. It shares a name with the HermesScript .h but not a syntax.

  Types are opaque pointers (flattened only for @frozen); a workflow becomes a function
  returning a status code, with the result through an out parameter.

Exit codes:
  0  success
  2  usage error
  4  unreadable or invalid IR
`,
	`hermes header —— 从 IR 生成对外 C ABI 头（T47 / D-024）

用法:
  hermes header <unit.ir.json | unit.hbc> --c [-o out.h]

说明:
  产物是**给 C/C++ 编译器读的**：C89 + <stdint.h>，只 include 手写冻结的
  hermes/hermes_core.h。它与 HermesScript 的 .h 同名不同目录、语法不同。

  类型一律不透明指针（展平只在 @frozen）；workflow 是函数，返回状态码，
  结果走 out 参数。

退出码:
  0  成功
  2  用法错误
  4  IR 读不到/不合格
`)
