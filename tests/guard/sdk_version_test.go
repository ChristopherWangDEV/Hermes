package guard

// 语言 SDK 的版本必须和编译器版本**一致**。
//
// 为什么需要守卫：八个 SDK 分布在八种清单格式里（package.json / Cargo.toml /
// pyproject.toml / .csproj …），改版本号时漏掉一个，用户 `npm install` 装到的
// 是 0.1.0、而编译器是 1.2.1 —— 这类"版本对不上"的表现是运行期协议不匹配，
// 排查成本极高。宁可让 CI 在这里红。

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestSDKVersionsMatchCompiler(t *testing.T) {
	root := repoRoot(t)

	semver := compilerSemver(t, root)
	if semver == "" {
		t.Fatal("读不到编译器版本（cmd/hermes/main.go 里的 semver）")
	}

	cases := map[string]string{
		"packages/javascript/package.json": `"version"\s*:\s*"([^"]+)"`,
		"packages/typescript/package.json": `"version"\s*:\s*"([^"]+)"`,
		"packages/rust/Cargo.toml":         `(?m)^version\s*=\s*"([^"]+)"`,
		"packages/python/pyproject.toml":   `(?m)^version\s*=\s*"([^"]+)"`,
		"packages/csharp/HermesSdk.csproj": `<Version>([^<]+)</Version>`,
	}

	for rel, pattern := range cases {
		path := filepath.Join(root, rel)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		m := regexp.MustCompile(pattern).FindStringSubmatch(string(raw))
		if m == nil {
			t.Fatalf("%s: 找不到版本字段（匹配 %s）", rel, pattern)
		}
		if m[1] != semver {
			t.Fatalf("%s 的版本是 %s，编译器是 %s —— 两者必须一致", rel, m[1], semver)
		}
	}
}

// compilerSemver 从 cmd/hermes/main.go 读出版本号：**唯一事实源**。
func compilerSemver(t *testing.T, root string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "cmd/hermes/main.go"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`const semver = "([^"]+)"`).FindStringSubmatch(string(raw))
	if m == nil {
		return ""
	}
	return m[1]
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// tests/guard → 仓库根
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

var _ = strings.TrimSpace
