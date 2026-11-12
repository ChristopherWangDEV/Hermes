// Command hermes 是 Hermes 的唯一入口。
//
// 目前只有 version 子命令 —— 这是刻意的。T01 的职责是"证明构建管线与 ABI 是通的"，
// 不是"把 CLI 铺满"。一个只会打印版本但能稳定构建的二进制，比一个子命令齐全
// 但 ABI 没验证过的二进制有用得多。
package main

import (
	"fmt"
	"os"
	"runtime"

	"hermes/internal/native"
)

// semver 是产品版本。构建产物里还会带一个 buildID（头文件集合内容哈希），
// 两者不同：semver 说"这是什么版本的产品"，buildID 说"这是哪一次 ABI 快照"。
//
// 2.0.0 是**长期支持版本（LTS）**：2.x 只保证与 ≥2.0.0 的 IR 互读，
// 1.x 的产物一律硬拒绝（E_IR_VERSION），不提供迁移路径。
// 破坏性变更：`fallback` 成为保留字，`auto fallback = 1;` 不再是合法源码。
const semver = "2.0.0"

// buildID 由 scripts/build.sh 通过 -ldflags -X 注入。
// 默认值 "unset" 是刻意的：如果有人用 go build 而不是 build.sh 构建，
// 版本信息里会明明白白写着"这是非正规构建"，而不是伪装成一个正常产物。
var buildID = "unset"

var usageText = pickUsage(`hermes - a compiled agent orchestration runtime

Usage:
  hermes <command> [flags]

Commands:
  version     print version, build ID and native ABI info
  trace       verify or summarise a trace (verify / summary)
  import      turn a Homonoia deployment plan into .hermes source (D-023)
  ir          compile .hermes into IR (canonical JSON)
  run         execute an IR document or a .hbc archive
  build       build a .hbc archive (add -k to sign and seal it)
  ar          inspect or extract .hbc members (t / x)
  link        link several .hbc archives into one
  sign        sign and verify .hbc (keygen / hbc / verify)
  verify      verify both products together (.hbc + sealed)
  serve       expose workflows over HTTP (T28)
  header      generate the public C ABI header from IR (T47)
  compile     compile .hermes into a native executable (AOT)
  cc          link your own C program against a Hermes unit (T48)
  doctor      report whether this machine can run Hermes
  explain     print a workflow contract and its cost bounds, or explain a diagnostic code
  fmt         normalise whitespace in sources (--check for CI)
  lsp         language server over stdio (for editors)
  demo        see the whole chain in one command
  demo-runaway  a bounded agent stops; a runtime-counting one does not
  new         scaffold a project (contract + implementation + sample input)
  i18n        check / export / import the language pack
  upgrade     check for / fetch the latest release
  licenses    print license texts that must ship with Hermes

Exit codes:
  0    success
  2    usage error
  4    operation failed
  70   internal error (native layer unavailable, ABI mismatch)

`,
	`hermes —— 编译型 Agent 编排运行时

用法:
  hermes <command> [flags]

命令:
  version     打印版本、构建 ID 与原生层 ABI 信息
  trace       验证与汇总一份 trace（verify / summary）
  import      把 Homonoia 的部署计划转成 .hermes 源码（D-023 的唯一接触面）
  ir          把 .hermes 编成 IR（canonical JSON）
  run         执行一份 IR
  build       把一个 .hermes 编成 .hbc 归档
  ar          查看与拆开 .hbc（t / x）
  link        把多个 .hbc 合成一个可执行归档
  sign        .hbc 的签名与验签（keygen / hbc / verify）
  verify      两个产物一起验（.hbc + sealed）
  serve       把 workflow 暴露成 HTTP 面（T28）
  header      从 IR 生成对外 C ABI 头（T47）
  compile     把 .hermes 编成原生可执行文件（AOT）

退出码:
  0    成功
  2    用法错误
  4    运行失败（trace 被篡改、无法解析）
  70   内部错误（原生层不可用、ABI 不匹配）
`)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usageText)
		return 2
	}

	// 原生层必须先初始化：它是引擎的地基，且版本不匹配要在"做任何事之前"失败。
	// 让一个 ABI 不匹配的进程跑起来再在半路崩，是最难查的一类故障。
	if err := native.Init(); err != nil {
		fmt.Fprintf(os.Stderr, "hermes: %s: %v\n", tr("native layer failed to initialise"), err)
		return 70
	}
	defer native.Shutdown()

	// clang 风格：第一个参数是源文件就走 driver（`hermes a.hermes -o a`）。
	if isDriverInvocation(args) {
		return cmdDriver(args)
	}

	switch args[0] {
	case "build":
		return cmdBuild(args[1:])
	case "licenses":
		return cmdLicenses(args[1:])
	case "i18n":
		return cmdI18n(args[1:])
	case "upgrade":
		return cmdUpgrade(args[1:])
	case "new":
		return cmdNew(args[1:])
	case "demo-runaway":
		return cmdRunaway(args[1:])
	case "demo":
		return cmdDemo(args[1:])
	case "lsp":
		return cmdLSP(args[1:])
	case "fmt":
		return cmdFmt(args[1:])
	case "explain":
		return cmdExplain(args[1:])
	case "doctor":
		return cmdDoctor(args[1:])
	case "cc":
		return cmdCC(args[1:])
	case "compile":
		return cmdCompile(args[1:])
	case "header":
		return cmdHeader(args[1:])
	case "serve":
		return cmdServe(args[1:])
	case "verify":
		return cmdVerify(args[1:])
	case "sign":
		return cmdSign(args[1:])
	case "link":
		return cmdLink(args[1:])
	case "ar":
		return cmdAr(args[1:])
	case "run":
		return cmdRun(args[1:])
	case "ir":
		return cmdIR(args[1:])
	case "import":
		return cmdImport(args[1:])
	case "version":
		// `version --check` 问发布源是否最新；不带参数时打印构建信息。
		for _, a := range args[1:] {
			if a == "--check" {
				return cmdVersionCheck()
			}
		}
		return cmdVersion()
	case "--version", "-version":
		return cmdVersion()
	case "trace":
		return cmdTrace(args[1:])
	case "-h", "--help", "help":
		fmt.Print(usageText)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "hermes: %s %q\n\n%s", tr("unknown command"), args[0], usageText)
		return 2
	}
}

func cmdVersion() int {
	abi := native.ABIVersion()
	arch, err := native.CycleArch()
	archName := arch.String()
	if err != nil {
		// 计数器不可用不该让 version 失败 —— 它是诊断命令，
		// 报"不可用"比报错退出更有用。
		archName = "unavailable(" + err.Error() + ")"
	}

	fmt.Printf("hermes %s\n", semver)
	fmt.Printf("  build_id   %s\n", buildID)
	fmt.Printf("  abi        %d.%d\n", abi>>16, abi&0xffff)
	fmt.Printf("  native_id  %s\n", native.BuildID())
	fmt.Printf("  go         %s\n", runtime.Version())
	fmt.Printf("  platform   %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Printf("  cycle_arch %s\n", archName)
	return 0
}
