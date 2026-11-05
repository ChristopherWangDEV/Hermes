// 本文件守的是**红线本身**：不引入任何第三方代码，也不让构建过程去"获取"任何东西。
//
// 为什么需要一条专门的守卫：红线在文档里写了六道门，但它们守的都是"已经进来的东西"
// （Go 依赖表、Cargo.lock、归档里的 C++ 符号）。**没有任何一道门守"会不会有人顺手加一行
// curl / pip install"** —— 而那才是第三方代码进入这个仓库最现实的方式。
//
// 这条守卫的模式很轻：扫脚本里的**非注释行**，出现下载/安装类命令就红。
// 它不判断许可证，只判断"这个仓库会不会自己去拿别人的代码"。许可证判断在白名单那几条。
package guard_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// acquireCmd 是"会把第三方代码拉进本机/本仓库"的命令类。
//
// 刻意**不含** `git`：版本控制是本项目刻意接受的开发工具（GPL-2.0，但不分发、
// 不进产物，见 HANDOFF §1.4）。把它列进来会让守卫在第一次提交时就红，
// 而一个必然被关掉的守卫等于没有守卫。
var acquireCmd = regexp.MustCompile(`\b(curl|wget|pip|pip3|npm|npx|pnpm|yarn|brew|apt-get|apt|go\s+get|go\s+mod\s+download|cargo\s+install|cargo\s+add|git\s+clone|git\s+submodule)\b`)

// TestNoCodeAcquisitionAnywhere 扫所有构建/测试脚本。
func TestNoCodeAcquisitionAnywhere(t *testing.T) {
	root := repoRoot(t)

	var files []string
	// 只扫"会被执行的东西"：shell 脚本与构建入口。文档里出现 curl 是正常的
	// （README 可能教用户怎么装工具），脚本里出现才是问题。
	for _, pat := range []string{
		"scripts/*.sh",
		"tests/*/*.sh",
		"tests/*.sh",
		"tools/*/*.go",
		"tools/*.sh",
	} {
		matches, err := filepath.Glob(filepath.Join(root, pat))
		if err != nil {
			t.Fatalf("glob %s 失败: %v", pat, err)
		}
		files = append(files, matches...)
	}
	if len(files) == 0 {
		t.Fatal("一个脚本都没扫到，守卫等于没跑")
	}

	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读不到 %s: %v", f, err)
		}
		rel, _ := filepath.Rel(root, f)
		for i, line := range strings.Split(string(data), "\n") {
			code := strings.TrimSpace(line)
			// 注释行跳过：脚本里说明"不要用 curl"是应该的，那不是在用 curl。
			if code == "" || strings.HasPrefix(code, "#") || strings.HasPrefix(code, "//") {
				continue
			}
			if m := acquireCmd.FindString(code); m != "" {
				t.Errorf("%s:%d 出现获取类命令 %q：\n    %s\n"+
					"红线要求构建过程不引入任何第三方代码。若确实需要某个工具，"+
					"把它写进文档当作前置条件，而不是让构建自己去下载。",
					rel, i+1, m, code)
			}
		}
	}
}

// TestNoThirdPartyGoModules 证明 Go 侧依赖表是空的。
//
// go.mod 里**没有 require 块**是这份项目最硬的红线证据之一：它比任何注释都可靠，
// 因为 `go build` 不会因为注释说"我们只用标准库"就拒绝编译别人的包。
func TestNoThirdPartyGoModules(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("读不到 go.mod: %v", err)
	}
	text := string(data)
	if strings.Contains(text, "require") {
		t.Errorf("go.mod 出现 require：红线要求零第三方 Go 依赖\n%s", text)
	}
	if _, err := os.Stat(filepath.Join(root, "go.sum")); err == nil {
		t.Error("存在 go.sum：说明有第三方模块被引入过（即使 require 被删掉，校验和文件仍是证据）")
	}
	if _, err := os.Stat(filepath.Join(root, "vendor")); err == nil {
		t.Error("存在 vendor/ 目录：红线要求零第三方 Go 依赖，vendor 是它的常见绕过方式")
	}
}

// TestCompilersAreActuallyClang 守的是"clang-only"这条命令面红线。
//
// 只看 basename 是不够的：PATH 里放一个叫 clang 的包装脚本、里面 exec gcc，
// 就能一路通过。所以两个入口都必须**问编译器自己**（`--version` 里有 "clang"）。
//
// 为什么产物侧没法兜底：本项目的对象文件带 `-fno-ident`（可重现构建要求），
// **故意不写 .comment**，所以事后从 .o 里查不出是谁编的。既然产物不带证据，
// 就必须在驱动层断言 —— 这是"可重现"与"可追溯"之间一次有意识的取舍。
func TestCompilersAreActuallyClang(t *testing.T) {
	root := repoRoot(t)
	for _, f := range []string{
		"scripts/check_toolchain.sh",
		"tests/compiler/run.sh",
	} {
		data, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatalf("读不到 %s: %v", f, err)
		}
		text := string(data)
		if !strings.Contains(text, "--version") || !strings.Contains(text, "clang") {
			t.Errorf("%s 里没有对编译器身份的断言（只比 basename 挡不住包装脚本）", f)
		}
		// 判据必须是"输出里含 clang"，而不是"basename 等于 clang"。
		if strings.Contains(text, `case "$bin" in`) && strings.Contains(text, "*clang)") {
			// 允许存在，但必须同时有 --version 检查（上面已断言）。
			continue
		}
	}
}

// TestNoGnuToolchainInScripts 扫脚本里**执行** GNU 工具的位置。
//
// 允许出现在注释与拒绝列表里（build.sh 就是靠拒绝 gcc 来守门），
// 但不允许真的把 gcc/g++/cc/ld/as 当命令跑。
func TestNoGnuToolchainInScripts(t *testing.T) {
	root := repoRoot(t)
	// 只列**编译器/链接器**。ar / ranlib / nm 不在此列：它们是归档与检查工具，
	// 不参与代码生成，也从不随产物分发（§1.4）。它们的许可证由
	// check_toolchain.sh 实测记录（GNU 就写 GPL），而不是靠这里假装它们不存在。
	execGnu := regexp.MustCompile(`(^|[\s(])(gcc|g\+\+|cc|c\+\+|ld)(-[0-9.]+)?(\s|$)`)
	files, err := filepath.Glob(filepath.Join(root, "scripts/*.sh"))
	if err != nil || len(files) == 0 {
		t.Fatalf("扫不到 scripts/*.sh: %v", err)
	}
	for _, f := range files {
		data, _ := os.ReadFile(f)
		rel, _ := filepath.Rel(root, f)
		for i, line := range strings.Split(string(data), "\n") {
			code := strings.TrimSpace(line)
			if code == "" || strings.HasPrefix(code, "#") {
				continue
			}
			if execGnu.MatchString(code) {
				// 例外：这一行本身是在**拒绝** GNU 工具（门禁的自我描述）。
				if strings.Contains(code, "FATAL") || strings.Contains(code, "printf") ||
					strings.Contains(code, "case ") || strings.Contains(code, "grep") {
					continue
				}
				t.Errorf("%s:%d 直接执行了 GNU 工具：\n    %s\n"+
					"红线要求全程 clang；GNU 工具链的产物会带上 GPL 义务。", rel, i+1, code)
			}
		}
	}
}

// TestToolchainRecordsArLicenseHonestly 守的是"记录必须准确"。
//
// 背景：check_toolchain.sh 曾把 ar 的许可证**硬编码**成 Apache-2.0 WITH LLVM-exception。
// macOS 上那是真的（Apple cctools / llvm-ar），Linux 上 ar 来自 binutils 且是
// GPL-3.0-or-later —— 同一行字在两个平台上有一半是谎话。这个项目已经因为
// 硬编码的许可字段吃过一次亏（cpp_stdlib_license 就是这么被删掉的）。
//
// 这里不做"必须不是 GPL"的断言：使用 GNU ar **不构成许可义务**（它只拼接我们的 .o，
// 不链接代码、不随产物分发）。要求的是**记录与实际一致**。
func TestToolchainRecordsArLicenseHonestly(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "scripts/check_toolchain.sh"))
	if err != nil {
		t.Fatalf("读不到 check_toolchain.sh: %v", err)
	}
	text := string(data)

	if !strings.Contains(text, `"GNU ar"`) {
		t.Error("check_toolchain.sh 不再识别 GNU ar：ar 的许可证又变成硬编码了")
	}
	if !strings.Contains(text, "gnu_binutils_detected") {
		t.Error("toolchain.json 缺 gnu_binutils_detected 字段：GNU binutils 的事实没有被记录")
	}
	if !strings.Contains(text, `AR_LICENSE`) {
		t.Error("check_toolchain.sh 里 ar 的 license 不是由 AR_LICENSE 变量决定（又硬编码了？）")
	}
}
