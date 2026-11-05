// 本文件守**许可的范围**：Apache-2.0 覆盖 Hermes 本身，且说清楚不覆盖什么。
//
// 为什么值得一条守卫：许可声明最怕"看起来覆盖了一切"。读者（尤其是要把它放进
// 闭源产品的人）需要能一眼确认三件事：许可种类、覆盖范围、以及"我的产物归谁"。
// 这三件事写错不会让任何测试变红，只会让法律问题在很久以后出现。
package guard_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestLicenseIsApache2 确认仓库根有 Apache-2.0 全文。
func TestLicenseIsApache2(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "LICENSE"))
	if err != nil {
		t.Fatalf("缺 LICENSE: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"Apache License",
		"Version 2.0, January 2004",
		"TERMS AND CONDITIONS FOR USE, REPRODUCTION, AND DISTRIBUTION",
		"Grant of Patent License",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("LICENSE 不像 Apache-2.0 全文：缺 %q", want)
		}
	}
}

// TestNoticeScopesTheLicense 确认 NOTICE 划清了范围。
//
// 三件必须写清的事：只覆盖 Hermes、不覆盖兄弟项目、产物归使用者。
func TestNoticeScopesTheLicense(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "NOTICE"))
	if err != nil {
		t.Fatalf("缺 NOTICE: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"Apache License, Version 2.0",
		"this license covers",  // 范围声明（小写，容忍大小写差异下面的检查）
		"Homonoia",             // 明确不覆盖的兄弟项目
		"Your output is yours", // 产物归属
	} {
		if !strings.Contains(strings.ToLower(text), strings.ToLower(want)) {
			t.Fatalf("NOTICE 缺 %q —— 范围没写清，读者无法确认自己能不能商用", want)
		}
	}
}

// TestReadmeStatesLicense 确认 README 里有许可小节（开发者第一眼看的地方）。
func TestReadmeStatesLicense(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "Apache-2.0") {
		t.Fatal("README 没提 Apache-2.0")
	}
	// README 是**单文件双语**：英文版与中文版都在 README.md 里，
	// 所以两种语言的许可范围声明都要在这个文件里找到。
	if !strings.Contains(text, "仅限本仓库") {
		t.Fatal("中文版没写清许可范围仅限本仓库里的 Hermes")
	}
	if !strings.Contains(text, "this repository only") {
		t.Fatal("英文版没写清许可范围仅限本仓库")
	}
}

// TestReleaseShipsLicenseFiles 守"分发时该带的都带了"。
//
// 这是**分发责任**的核心：Apache-2.0 只覆盖本体，而二进制里还有 Go 运行时与
// 两个 Rust crate（MIT/BSD）。不把这些文本随包发出，开源声明就只履行了一半。
func TestReleaseShipsLicenseFiles(t *testing.T) {
	root := repoRoot(t)
	tars, _ := filepath.Glob(filepath.Join(root, "build", "release", "hermes-*.tar.gz"))
	if len(tars) == 0 {
		t.Skip("没有发布包（先跑 scripts/release.sh）")
	}
	tar := tars[len(tars)-1]

	dir := t.TempDir()
	if out, err := exec.Command("tar", "-tzf", tar).CombinedOutput(); err != nil {
		t.Fatalf("读不了 %s: %v\n%s", tar, err, out)
	} else {
		list := string(out)
		for _, want := range []string{
			"/LICENSE",
			"/NOTICE",
			"/THIRD_PARTY_NOTICES.md",
			"/licenses/go-LICENSE",
			"/licenses/sha2-LICENSE-MIT",
			"/licenses/zeroize-LICENSE-MIT",
		} {
			if !strings.Contains(list, want) {
				t.Fatalf("发布包缺 %s —— 分发时这是义务，不是可选项", want)
			}
		}
	}
	_ = dir
}

// TestThirdPartyNoticesCoverLinkedDeps 确认 NOTICES 里点名了实际链接的依赖。
//
// 新增依赖却忘了更新 NOTICES，是合规最常见的漏。
func TestThirdPartyNoticesCoverLinkedDeps(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "THIRD_PARTY_NOTICES.md"))
	if err != nil {
		t.Fatalf("缺 THIRD_PARTY_NOTICES.md: %v", err)
	}
	text := string(data)
	for _, dep := range []string{"sha2", "zeroize", "Go 运行时", "BSD-3-Clause"} {
		if !strings.Contains(text, dep) {
			t.Fatalf("THIRD_PARTY_NOTICES.md 没提到 %q", dep)
		}
	}
	// 每个链接的 crate 都要有对应许可原文。
	for _, f := range []string{
		"licenses/sha2-LICENSE-MIT",
		"licenses/zeroize-LICENSE-MIT",
		"licenses/go-LICENSE",
	} {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Fatalf("缺 %s（NOTICES 里点名了，就得有原文）", f)
		}
	}
}
