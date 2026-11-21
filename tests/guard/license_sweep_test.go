// 本文件是**许可证清点**（T30 的第二半）：不是抽查，是把每一处依赖来源枚举出来
// 并断言它在红线允许的集合里。
//
// 为什么值得一条测试：许可问题不会让测试变红，只会让产品在某天不能卖。
// 所以它必须由机器守着，而不是靠"我记得查过"。
package guard_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGoHasNoThirdPartyModules 是红线最硬的一条：Go 侧零第三方依赖。
//
// go.sum 只要存在且非空，就说明有外部模块参与过构建 —— 哪怕现在没用上，
// 它也是一条"下次改动会悄悄启用它"的路。
func TestGoHasNoThirdPartyModules(t *testing.T) {
	root := repoRoot(t)
	gomod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	// go.mod 里只允许出现 module / go / toolchain / require 指向自身的行。
	for _, line := range strings.Split(string(gomod), "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "require") && !strings.Contains(l, "hermes") {
			t.Errorf("go.mod 出现外部依赖：%s", l)
		}
	}
	gosum := filepath.Join(root, "go.sum")
	if st, err := os.Stat(gosum); err == nil && st.Size() > 0 {
		data, _ := os.ReadFile(gosum)
		t.Errorf("go.sum 非空（%d 字节），说明有第三方模块参与过构建：\n%s",
			st.Size(), firstLines(string(data), 3))
	}
}

// TestCargoDepsArePermissive 枚举 Cargo.toml 的依赖，只允许已知的宽松许可。
//
// 这里不解析 Cargo 的锁文件（那会引第三方库），而是**名字白名单**：
// 新增依赖必须改这张表，改动会出现在 code review 里 —— 这正是我们要的。
func TestCargoDepsArePermissive(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "native", "rust", "Cargo.toml"))
	if err != nil {
		t.Skip("没有 native/Cargo.toml")
	}
	// 白名单与 `RED_LINE_DEPENDENCIES` 表一一对应（另有一条守卫核对表的完整性）。
	// sha2: MIT OR Apache-2.0；zeroize: MIT OR Apache-2.0 —— 两者都允许闭源商用。
	allowed := map[string]bool{
		"sha2":    true,
		"zeroize": true,
	}
	inDeps := false
	for _, line := range strings.Split(string(data), "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "[dependencies]") || strings.HasPrefix(l, "[build-dependencies]") {
			inDeps = true
			continue
		}
		if strings.HasPrefix(l, "[") {
			inDeps = false
			continue
		}
		if !inDeps || l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		name := strings.TrimSpace(strings.SplitN(l, "=", 2)[0])
		if name == "" {
			continue
		}
		if !allowed[name] {
			t.Errorf("Rust 依赖 %q 不在许可白名单里：新增依赖必须同时更新本表与 DECISIONS", name)
		}
	}
}

// TestNoVendoredSources 确认仓库里没有被 vendor 进来的第三方源码目录。
func TestNoVendoredSources(t *testing.T) {
	root := repoRoot(t)
	for _, dir := range []string{"vendor", "third_party", "third-party", "deps"} {
		if st, err := os.Stat(filepath.Join(root, dir)); err == nil && st.IsDir() {
			t.Errorf("仓库里出现 %s/ —— 第三方代码不得进仓库（红线 §1.5）", dir)
		}
	}
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
