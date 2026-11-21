// Command hermes 是 Hermes 的唯一入口。
//
// 目前只有 version 子命令 —— 这是刻意的。T01 的职责是"证明构建管线与 ABI 是通的"，
// 不是"把 CLI 铺满"。一个只会打印版本但能稳定构建的二进制，比一个子命令齐全
// 但 ABI 没验证过的二进制有用得多。
package main

import (
	"strings"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"hermes/internal/native"
)

// semver 是产品版本。构建产物里还会带一个 buildID（头文件集合内容哈希），
// 两者不同：semver 说"这是什么版本的产品"，buildID 说"这是哪一次 ABI 快照"。
//
// 2.0.0 是**长期支持版本（LTS）**：2.x 只保证与 ≥2.0.0 的 IR 互读，
// 1.x 的产物一律硬拒绝（E_IR_VERSION），不提供迁移路径。
// 破坏性变更：`fallback` 成为保留字，`auto fallback = 1;` 不再是合法源码。
const semver = "2.0.1"

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

	// clang 风格（`hermes a.hermes -o a` / `-c` / `-E`）由 native 驱动处理：
	// 它把 flag 翻译成 build/compile 调用，与子命令走同一套实现。
	if isDriverInvocation(args) {
		return execNative(args...)
	}

	switch args[0] {
	case "build":
		// 带 -k 时签名那一半在 Go（Ed25519 只有 Go 标准库免费给），编译那一半
		// 全在 C++。这不是"还没搬"，是**永久分工**。
		if hasSigningKey(args[1:]) {
			return buildWithSigning(args[1:])
		}
		return execNative(append([]string{"build"}, args[1:]...)...)
	case "licenses":
		return execNative(append([]string{"licenses"}, args[1:]...)...)
	case "i18n":
		return cmdI18n(args[1:])
	case "upgrade":
		return cmdUpgrade(args[1:])
	case "new":
		return execNative(append([]string{"new"}, args[1:]...)...)
	case "demo-runaway":
		return cmdRunaway(args[1:])
	case "demo":
		return cmdDemo(args[1:])
	case "lsp":
		return execNative(append([]string{"lsp"}, args[1:]...)...)
	case "deps":
		return execNative(append([]string{"deps"}, args[1:]...)...)
	case "fmt":
		return execNative(append([]string{"fmt"}, args[1:]...)...)
	case "explain":
		return execNative(append([]string{"explain"}, args[1:]...)...)
	case "doctor":
		return execNative(append([]string{"doctor"}, args[1:]...)...)
	case "cc":
		return execNative(append([]string{"cc"}, args[1:]...)...)
	case "compile":
		return execNative(append([]string{"compile"}, args[1:]...)...)
	case "header":
		return execNative(append([]string{"header"}, args[1:]...)...)
	case "serve":
		return cmdServe(args[1:])
	case "verify":
		return cmdVerify(args[1:])
	case "sign":
		return cmdSign(args[1:])
	case "link":
		return execNative(append([]string{"link"}, args[1:]...)...)
	case "ar":
		return execNative(append([]string{"ar"}, args[1:]...)...)
	case "run":
		return cmdRun(args[1:])
	case "ir":
		return execNative(append([]string{"ir"}, args[1:]...)...)
	case "import":
		return execNative(append([]string{"import"}, args[1:]...)...)
	case "version":
		// `version --check` 问发布源是否最新；不带参数时打印构建信息。
		for _, a := range args[1:] {
			if a == "--check" {
				return cmdVersionCheck()
			}
		}
		return execNative("version")
	case "--version", "-version":
		return execNative("version")
	case "trace":
		return execNative(append([]string{"trace"}, args[1:]...)...)
	case "-h", "--help", "help":
		fmt.Print(usageText)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "hermes: %s %q\n\n%s", tr("unknown command"), args[0], usageText)
		return 2
	}
}

// execNative 把命令交给 C++ 主程序（hermes-native）。
//
// 为什么是 exec 而不是"再实现一遍"：搬过来的命令只有**一份**实现，Go 侧只做
// 进程转发。找不到二进制就明确报错——静默回落到 Go 实现会让"去 Go"变成谎话。
func execNative(sub ...string) int {
	bin, err := findNativeBinary()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermes: %v\n", err)
		return 4
	}
	cmd := exec.Command(bin, sub...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "hermes: %v\n", err)
		return 4
	}
	return 0
}

// findNativeBinary 找 hermes-native：先看与 hermes 同目录，再看 PATH。
func findNativeBinary() (string, error) {
	// 显式覆盖（测试与打包脚本用）。
	if env := os.Getenv("HERMES_NATIVE_BIN"); env != "" {
		if st, err := os.Stat(env); err == nil && !st.IsDir() {
			return env, nil
		}
		return "", fmt.Errorf("HERMES_NATIVE_BIN=%s is not a file", env)
	}
	if exe, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(exe), "hermes-native")
		if st, err := os.Stat(cand); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return cand, nil
		}
	}
	if p, err := exec.LookPath("hermes-native"); err == nil {
		return p, nil
	}
	// 开发期兜底：在仓库根目录跑（含 go test）时，C++ 产物就在 build/ 下。
	// 发布包里不会命中这条——它要求 CWD 恰好是仓库根。
	if cand := filepath.Join("build", "hermes-native"); fileExists(cand) {
		return cand, nil
	}
	return "", errors.New("hermes-native not found (expected next to the hermes binary, or on PATH); build it with scripts/build.sh")
}

// fileExists 是最小存在性判断（不引入额外依赖）。
func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// hasSigningKey 判断 build 的参数里有没有 -k / --key。
func hasSigningKey(args []string) bool {
	for _, a := range args {
		if a == "-k" || a == "--key" {
			return true
		}
	}
	return false
}

// isDriverInvocation 判断 argv 是否该按 clang 风格解释（`hermes app.hermes -o app`）。
//
// 判据保持极简且与 native 侧一致：第一个参数不是子命令、且以 .hermes / .h 结尾；
// 或者第一个参数就是 -o / -c / -E（缺源文件时交给 driver 报用法错，
// 比"未知命令"清楚）。
func isDriverInvocation(args []string) bool {
	if len(args) == 0 {
		return false
	}
	first := args[0]
	if strings.HasPrefix(first, "-") {
		return first == "-o" || first == "-c" || first == "-E"
	}
	if knownCommands()[first] {
		return false
	}
	return strings.HasSuffix(first, ".hermes") || strings.HasSuffix(first, ".h")
}

// knownCommands 是子命令集合（与下面的 switch 一致）。
func knownCommands() map[string]bool {
	return map[string]bool{
		"version": true, "trace": true, "import": true, "ir": true, "run": true,
		"build": true, "ar": true, "link": true, "sign": true, "verify": true,
		"serve": true, "header": true, "compile": true, "cc": true, "doctor": true,
		"fmt": true, "explain": true, "lsp": true,
	}
}
