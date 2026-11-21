package native

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
)

// TestObjfileDynamicLibsMatchesGoStdlib 是 Rust 实现的**等价判据**：
// 同一批真实二进制上，Rust 解析结果必须与 Go `debug/*` 的结果完全一致。
//
// 为什么用系统二进制当样本：它们覆盖了不同链接器、不同 padding、以及
// fat（多架构）Mach-O —— 手写合成样本只能覆盖"我以为的格式"。
func TestObjfileDynamicLibsMatchesGoStdlib(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	cands := []string{
		"/usr/bin/true", "/bin/ls", "/usr/bin/env", "/bin/sh",
	}
	// ELF 样本：macOS 上所有系统二进制都是 Mach-O，若只拿它们比对，**ELF 分支
	// 的等价性等于没有证据**（实测：把 ELF 分支关掉，本机测试仍全绿）。
	// 用 clang + ld.lld 交叉链一个带 DT_NEEDED 的 ELF .so 当样本；工具链缺失
	// 就明确跳过，而不是假装覆盖了。
	if elfSample, ok := buildELFSample(t); ok {
		cands = append(cands, elfSample)
	}
	// 仓里自己的产物：由 clang 链接，是最贴近我们要判的那类输入。
	if exe, err := os.Executable(); err == nil {
		cands = append(cands, exe)
	}
	if p, err := exec.LookPath("cc"); err == nil {
		cands = append(cands, p)
	}

	compared := 0
	for _, path := range cands {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		want, werr := goDynamicLibs(path)
		got, gerr := ObjfileDynamicLibs(path)
		if (werr == nil) != (gerr == nil) {
			t.Fatalf("%s: 一侧成功一侧失败（go=%v rust=%v）", path, werr, gerr)
		}
		if werr != nil {
			continue
		}
		sort.Strings(want)
		sort.Strings(got)
		compared++
		if len(want) != len(got) {
			t.Fatalf("%s: 条数不同 go=%v rust=%v", path, want, got)
		}
		for i := range want {
			if want[i] != got[i] {
				t.Fatalf("%s: 第 %d 条不同 go=%q rust=%q", path, i, want[i], got[i])
			}
		}
	}
	if compared < 2 {
		t.Fatalf("只比对到 %d 个样本 —— 这条测试会空转", compared)
	}
	// ELF 分支必须真的被验过：本机若连一个 ELF 样本都没有，这条测试对
	// `elf_dynamic_libs` 是空转，必须显式说明而不是让人以为覆盖了。
	sawELF := false
	for _, c := range cands {
		if b, err := os.ReadFile(c); err == nil && len(b) >= 4 && b[0] == 0x7f && b[1] == 'E' {
			sawELF = true
		}
	}
	if !sawELF {
		t.Logf("⚠️ 本轮样本里没有 ELF —— elf_dynamic_libs 分支未被验到（需要 Linux 或 clang+ld.lld 交叉链接）")
	}
	t.Logf("动态依赖等价：已比对 %d 个二进制", compared)
}

// buildELFSample 用 clang + ld.lld 交叉链一个小 ELF .so（带 DT_NEEDED），
// 返回它的路径。工具链不可用时返回 ok=false —— 宁可不覆盖，也不假装覆盖。
func buildELFSample(t *testing.T) (string, bool) {
	t.Helper()
	dir := t.TempDir()
	dep := filepath.Join(dir, "dep.c")
	src := filepath.Join(dir, "probe.c")
	if err := os.WriteFile(dep, []byte("int dep_fn(void){return 2;}\n"), 0o644); err != nil {
		return "", false
	}
	if err := os.WriteFile(src, []byte("int dep_fn(void);\nint hm_probe(void){return dep_fn();}\n"), 0o644); err != nil {
		return "", false
	}
	lib := filepath.Join(dir, "libdep.so")
	run := func(args ...string) bool {
		cmd := exec.Command("clang", args...)
		out, err := cmd.CombinedOutput()
		return err == nil && len(out) == 0 || err == nil
	}
	if !run("--target=x86_64-unknown-linux-gnu", "-fuse-ld=lld", "-shared", "-nostdlib",
		"-o", lib, dep) {
		t.Logf("跳过 ELF 样本：交叉链接 libdep.so 失败")
		return "", false
	}
	out := filepath.Join(dir, "probe.so")
	if !run("--target=x86_64-unknown-linux-gnu", "-fuse-ld=lld", "-shared", "-nostdlib",
		"-o", out, src, "-L"+dir, "-ldep") {
		t.Logf("跳过 ELF 样本：交叉链接 probe.so 失败")
		return "", false
	}
	return out, true
}

// TestObjfileRejectsTruncated 是**反向变异**：截断文件必须报错，不能崩溃、
// 不能返回"空依赖列表"（把坏文件当静态链接会静默放行许可检查）。
func TestObjfileRejectsTruncated(t *testing.T) {
	dir := t.TempDir()
	full, err := os.ReadFile("/bin/ls")
	if err != nil {
		t.Skipf("没有 /bin/ls: %v", err)
	}
	for _, n := range []int{4, 16, 64, 512} {
		if n >= len(full) {
			continue
		}
		p := filepath.Join(dir, "trunc")
		if err := os.WriteFile(p, full[:n], 0o644); err != nil {
			t.Fatal(err)
		}
		libs, err := ObjfileDynamicLibs(p)
		if err == nil {
			t.Fatalf("截断到 %d 字节必须报错，实际返回 %d 条依赖", n, len(libs))
		}
	}
	// 完全不是目标文件：必须是"格式不支持"，不能是"结构坏了"。
	p := filepath.Join(dir, "text")
	os.WriteFile(p, []byte("#!/bin/sh\necho hi\n"), 0o644)
	if _, err := ObjfileDynamicLibs(p); err != ErrObjUnsupportedFormat {
		t.Fatalf("非目标文件应报 ErrObjUnsupportedFormat，实际 %v", err)
	}
}
