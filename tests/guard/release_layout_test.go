// 本文件守"下载即用"这条**用户可见**的承诺：装好的包必须能编译 .hermes 代码。
//
// 为什么值得一条守卫：发布包的布局与开发布局不同（lib/ vs build/），
// 任何一处写死路径，症状都是"开发者一切正常、用户一装就报找不到文件"。
// 这类 bug 只在打包之后才出现，所以必须由打包后的产物来验。
package guard_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestReleasePackageCanCompile 解包 → doctor → 编译 → 运行。
//
// 没有发布包时跳过（需要先跑 scripts/release.sh）—— 在 CI 里它是必跑项。
func TestReleasePackageCanCompile(t *testing.T) {
	root := repoRoot(t)
	tars, _ := filepath.Glob(filepath.Join(root, "build", "release", "hermes-*.tar.gz"))
	if len(tars) == 0 {
		t.Skip("没有发布包（先跑 scripts/release.sh）")
	}
	tar := tars[len(tars)-1]

	dir := t.TempDir()
	if out, err := exec.Command("tar", "-xzf", tar, "-C", dir).CombinedOutput(); err != nil {
		t.Fatalf("解包失败: %v\n%s", err, out)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("解包后是空的: %v", err)
	}
	home := filepath.Join(dir, entries[0].Name())
	bin := filepath.Join(home, "bin", "hermes")
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("包里没有 bin/hermes: %v", err)
	}

	// doctor 必须全绿：用户装完第一件事就是跑它。
	doctor := exec.Command(bin, "doctor")
	doctor.Dir = dir
	doctor.Env = append(os.Environ(), "HERMES_HOME="+home)
	if out, err := doctor.CombinedOutput(); err != nil {
		t.Fatalf("装好的包 doctor 未通过: %v\n%s", err, out)
	}

	// 编译一个真实的 .hermes 并运行它 —— 这是"能用"的定义。
	src := filepath.Join(dir, "hello.hermes")
	if err := os.WriteFile(src, []byte(
		"export workflow hello() entry -> string { return \"from package\"; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prog := filepath.Join(dir, "hello")
	build := exec.Command(bin, src, "-o", prog)
	build.Dir = dir
	build.Env = append(os.Environ(), "HERMES_HOME="+home)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("装好的包编不出可执行文件: %v\n%s", err, out)
	}
	run, err := exec.Command(prog).Output()
	if err != nil {
		t.Fatalf("产物跑不起来: %v", err)
	}
	// 2.0.0 起 AOT 产物输出的是与 `hermes run` 相同的信封：
	// {"steps":N,"value":…,"workflow":…}。这里解析信封而不是比裸字符串，
	// 否则"行为改了但测试没改"会表现成一条莫名其妙的失败。
	{
		var env struct {
			Value *string `json:"value"`
		}
		out := strings.TrimSpace(string(run))
		if err := json.Unmarshal([]byte(out), &env); err == nil {
			if env.Value == nil || *env.Value != "from package" {
				t.Fatalf("产物输出的 value 不对: %q", out)
			}
		} else if out != "from package" {
			t.Fatalf("产物输出不对: %q", out)
		}
	}

	// -c 也要能出归档（等价 clang -c）。
	obj := exec.Command(bin, src, "-c")
	obj.Dir = dir
	obj.Env = append(os.Environ(), "HERMES_HOME="+home)
	if out, err := obj.CombinedOutput(); err != nil {
		t.Fatalf("-c 失败: %v\n%s", err, out)
	}
	if st, err := os.Stat(filepath.Join(dir, "hello.hbc")); err != nil || st.Size() == 0 {
		t.Fatalf("-c 没产出 hello.hbc: %v", err)
	}
}
