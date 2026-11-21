package native

import (
	"os"
	"testing"

	"hermes/internal/linkpolicy"
)

// TestLinkPolicyAgreesOnRealBinaries 把红线判据放到**真实二进制**的动态依赖上比对：
// 用 objfile（Rust 实现）读出 `/bin/ls`、本仓产物等的依赖库，再分别问 Go 与
// C++ 的闭集判据"有没有红线库"，结论必须一致。
//
// 为什么值得单独一条：前面的表比对用的是构造出来的字符串；这一条用的是
// 系统真实链接出来的依赖列表（`/usr/lib/libc++.1.dylib` 这类），能抓到
// "按路径取 basename" 那一步的实现差异——而这正是 cc 在产物侧要做的事。
func TestLinkPolicyAgreesOnRealBinaries(t *testing.T) {
	binaries := []string{"/bin/ls", "/usr/bin/env", "/usr/bin/true"}
	if exe, err := os.Executable(); err == nil {
		binaries = append(binaries, exe)
	}
	if _, err := os.Stat("../../build/hermes"); err == nil {
		binaries = append(binaries, "../../build/hermes")
	}

	checked := 0
	for _, bin := range binaries {
		if _, err := os.Stat(bin); err != nil {
			continue
		}
		libs, err := ObjfileDynamicLibs(bin)
		if err != nil || len(libs) == 0 {
			continue
		}
		checked++
		// Go 侧：第一个命中的库
		goLib, _, goHit := linkpolicy.FirstForbidden(libs)
		// C++ 侧：逐个问
		cppLib, cppHit := "", false
		for _, l := range libs {
			if name, _ := LinkPolicyMatch(l); name != "" {
				cppLib, cppHit = l, true
				break
			}
		}
		if goHit != cppHit {
			t.Fatalf("%s: 结论不一致 Go=%v(%s) C++=%v(%s)", bin, goHit, goLib, cppHit, cppLib)
		}
		if goHit && goLib != cppLib {
			t.Fatalf("%s: 命中的库不同 Go=%q C++=%q", bin, goLib, cppLib)
		}
	}
	if checked == 0 {
		t.Fatal("没有读到任何真实二进制的依赖 —— 这条测试会空转")
	}
	t.Logf("真实二进制红线判据一致：%d 个", checked)
}
