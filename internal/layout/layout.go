// Package layout 回答"我需要的那些文件在哪"。
//
// 为什么这值得一个包：开发时资源在仓库布局里（`build/`、`include/`），
// 而用户下载到的是一个**发布包**：bin/hermes + lib/ + include/。
// 两套布局都要能找到，且**顺序固定** —— 否则同一个二进制在不同目录下
// 行为不同，而那种 bug 只在用户的机器上出现。
//
// 这就是"下载即用"的技术前提：二进制不能依赖"从仓库根目录运行"。
package layout

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Assets 是一次解析的结果。空字符串表示没找到。
type Assets struct {
	// BinDir 是 hermes 可执行文件所在目录。
	BinDir string
	// Root 是安装根（发布包里是 bin/ 的上一级；开发布局里是仓库根）。
	Root string
	// RuntimeLib 是 AOT / `hermes cc` 需要的静态库。
	RuntimeLib string
	// RuntimeHeader 是它的 C 头（`go build -buildmode=c-archive` 生成）。
	RuntimeHeader string
	// IncludeDir 是 hermes_core.h 所在的 include 根。
	IncludeDir string
	// Shim 是沙箱用的 hermes-shim（可缺）。
	Shim string
}

// Resolve 按固定顺序找资源：
//
//  1. `HERMES_HOME`（显式指定，最高优先：打包者与 CI 需要可覆盖）
//  2. 可执行文件所在目录的上一层（**发布包布局**：bin/ 与 lib/ 平级）
//  3. 当前目录（**开发布局**：仓库根，资源在 build/ 与 include/）
//
// 顺序不能反：开发时当前目录也常有 build/，但那不代表用户的安装位置。
func Resolve(execPath, cwd string) Assets {
	var a Assets
	if execPath != "" {
		// **先解符号链接**：安装后常有人把 `~/.local/bin/hermes` 软链到真正的
		// 安装位置。不解链接的话，根目录会算成 `~/.local`，而 lib/ 在那里不存在 ——
		// 症状是"从更目录跑 demo 正常、从别处跑就找不到运行时库"。
		if real, err := filepath.EvalSymlinks(execPath); err == nil {
			execPath = real
		}
		if abs, err := filepath.Abs(execPath); err == nil {
			a.BinDir = filepath.Dir(abs)
			a.Root = filepath.Dir(a.BinDir)
		}
	}

	candidates := make([]string, 0, 3)
	if home := os.Getenv("HERMES_HOME"); home != "" {
		candidates = append(candidates, home)
	}
	if a.Root != "" {
		candidates = append(candidates, a.Root)
	}
	if cwd != "" {
		candidates = append(candidates, cwd)
	}

	for _, root := range candidates {
		if a.RuntimeLib == "" {
			for _, p := range []string{
				filepath.Join(root, "lib", "libhermesrt.a"),
				filepath.Join(root, "build", "libhermesrt.a"),
			} {
				if isFile(p) {
					a.RuntimeLib = p
					a.RuntimeHeader = runtimeHeaderFor(p)
					break
				}
			}
		}
		if a.IncludeDir == "" {
			for _, p := range []string{
				filepath.Join(root, "include"),
				filepath.Join(root, "..", "include"),
			} {
				if isDir(filepath.Join(p, "hermes")) {
					a.IncludeDir = p
					break
				}
			}
		}
		if a.Shim == "" {
			for _, p := range []string{
				filepath.Join(root, "bin", "hermes-shim"),
				filepath.Join(root, "build", "hermes-shim"),
			} {
				if isFile(p) {
					a.Shim = p
					break
				}
			}
		}
		if a.RuntimeLib != "" && a.IncludeDir != "" {
			if a.Root == "" {
				a.Root = root
			}
			break
		}
	}
	if a.Root == "" && len(candidates) > 0 {
		a.Root = candidates[0]
	}
	return a
}

// runtimeHeaderFor 推出运行时库的 C 头名。
//
// `go build -buildmode=c-archive -o X.a` 生成的头叫 **X.h**（跟着 -o 走），
// 不是固定名。发布包里我们把它改成 hermes_rt.h，两种都要认。
func runtimeHeaderFor(libPath string) string { return HeaderFor(libPath) }

// Require 在缺关键资源时给出**可操作**的错误，而不是"找不到文件"。
func Require(a Assets) error {
	if a.RuntimeLib == "" {
		return fmt.Errorf("找不到运行时库 libhermesrt.a。" +
			"发布包应当自带（lib/ 或 build/ 下）；开发时先跑 scripts/build.sh。" +
			"也可以用 HERMES_HOME 指定安装根")
	}
	if a.IncludeDir == "" {
		return fmt.Errorf("找不到 include/hermes/hermes_core.h。" +
			"发布包应当自带 include/；开发时它就是仓库里的 include/")
	}
	return nil
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// HeaderFor 找到某个运行时库对应的 C 头。
//
// 两种命名都要认：`go build -buildmode=c-archive -o X.a` 生成 **X.h**，
// 而发布包把它统一成 `hermes_rt.h`。只认一种，就会在"开发时装得好好的、
// 装到用户机器上就找不到头"——而错误信息看起来像打包漏了文件。
func HeaderFor(libPath string) string {
	dir := filepath.Dir(libPath)
	base := filepath.Base(libPath)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	for _, p := range []string{
		filepath.Join(dir, "hermes_rt.h"),
		filepath.Join(dir, stem+".h"),
	} {
		if isFile(p) {
			return p
		}
	}
	return ""
}

// IncludeNear 从运行时库的位置**往上**找 include/hermes/hermes_core.h。
//
// 为什么需要它：`--runtime-lib` 显式指定时，资源根就不再是"可执行文件上一级"——
// 用户可能是从别处指过来的。此时唯一可靠的线索是那个库自己所在的目录树。
func IncludeNear(libPath string) string {
	dir := filepath.Dir(libPath)
	for i := 0; i < 4; i++ {
		if isDir(filepath.Join(dir, "hermes")) {
			return dir
		}
		if isDir(filepath.Join(dir, "include", "hermes")) {
			return filepath.Join(dir, "include")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}
