// 本文件把"八个 SDK 零第三方依赖"变成**机器检查**。
//
// 为什么需要它：SDK 是别人装进他们项目里的东西。一个 SDK 引入的依赖会进使用者的
// lock 文件、进他们的审计、进他们的许可证清单。所以"零依赖"不是我们的偏好，
// 是对使用者的承诺 —— 而承诺必须被检查，不能靠评审时记得。
package guard_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestSDKsHaveNoRuntimeDeps 逐个检查运行期依赖为空。
func TestSDKsHaveNoRuntimeDeps(t *testing.T) {
	root := repoRoot(t)

	t.Run("Python", func(t *testing.T) {
		data := readFile(t, filepath.Join(root, "packages/python/pyproject.toml"))
		if !strings.Contains(data, "dependencies = []") {
			t.Fatalf("pyproject.toml 的 dependencies 必须为空数组（现在是：%s）", grepLine(data, "dependencies"))
		}
		// 版本号两处必须一致：用户报 bug 时先问"你装的是哪个版本"，
		// 两个地方说法不同会让这句话失去意义。
		ver := regexp.MustCompile(`(?m)^version\s*=\s*"([^"]+)"`).FindStringSubmatch(data)
		initSrc := readFile(t, filepath.Join(root, "packages/python/hermes/__init__.py"))
		initVer := regexp.MustCompile(`(?m)^__version__\s*=\s*"([^"]+)"`).FindStringSubmatch(initSrc)
		if ver == nil || initVer == nil {
			t.Fatalf("版本号没写全：pyproject=%v __init__=%v", ver, initVer)
		}
		if ver[1] != initVer[1] {
			t.Fatalf("版本号不一致：pyproject %s vs __init__ %s", ver[1], initVer[1])
		}
	})

	for _, pkg := range []string{"javascript", "typescript"} {
		t.Run(pkg, func(t *testing.T) {
			var p struct {
				Deps    map[string]string `json:"dependencies"`
				DevDeps map[string]string `json:"devDependencies"`
			}
			raw := readFile(t, filepath.Join(root, "packages", pkg, "package.json"))
			if err := json.Unmarshal([]byte(raw), &p); err != nil {
				t.Fatal(err)
			}
			if len(p.Deps) != 0 {
				t.Fatalf("%s 有运行期依赖：%v —— SDK 的依赖就是使用者的依赖", pkg, p.Deps)
			}
			// devDependencies 允许有（构建工具不随包分发），但要逐个白名单。
			for name := range p.DevDeps {
				if name != "typescript" {
					t.Fatalf("%s 的 devDependency %q 不在白名单里：新增就必须更新本守卫与 NOTICE", pkg, name)
				}
			}
		})
	}

	t.Run("Go", func(t *testing.T) {
		mod := readFile(t, filepath.Join(root, "packages/go/go.mod"))
		if regexp.MustCompile(`(?m)^\s*require\b`).MatchString(mod) {
			t.Fatalf("go.mod 出现 require：\n%s", mod)
		}
	})

	t.Run("Rust", func(t *testing.T) {
		toml := readFile(t, filepath.Join(root, "packages/rust/Cargo.toml"))
		idx := strings.Index(toml, "[dependencies]")
		if idx < 0 {
			t.Fatal("Cargo.toml 缺 [dependencies]（显式写空段，比省略更容易审）")
		}
		rest := strings.TrimSpace(toml[idx+len("[dependencies]"):])
		// 只看 [dependencies] 这一段，遇到下一个 [section] 就停。
		if next := strings.Index(rest, "\n["); next >= 0 {
			rest = strings.TrimSpace(rest[:next])
		}
		if rest != "" {
			t.Fatalf("[dependencies] 必须为空，实际有：%q", rest)
		}
	})

	t.Run("C#", func(t *testing.T) {
		csproj := readFile(t, filepath.Join(root, "packages/csharp/HermesSdk.csproj"))
		if strings.Contains(csproj, "PackageReference") {
			t.Fatal("csproj 出现 PackageReference：SDK 只用 BCL")
		}
	})

	t.Run("Java/Kotlin", func(t *testing.T) {
		// 两个 JVM SDK 只用 JDK 自带的 java.net.http 与 java.lang.foreign。
		// 出现任何 build 文件（maven/gradle）就说明有人开始引依赖了。
		for _, dir := range []string{"packages/java", "packages/kotlin"} {
			for _, name := range []string{"pom.xml", "build.gradle", "build.gradle.kts"} {
				if _, err := os.Stat(filepath.Join(root, dir, name)); err == nil {
					t.Fatalf("%s/%s 存在：JVM SDK 必须只用 JDK，不引构建系统依赖", dir, name)
				}
			}
			// 源码目录里不许有 jar。**不能只靠 git ls-files**：从 GitHub 下载 ZIP
			// （而不是 git clone）是最常见的获取方式，那种目录里根本没有 .git，
			// `git ls-files` 会以退出码 128 失败 —— 于是守卫在"合法获取方式"下变成假红。
			// 所以：有 git 就用索引（能排除未跟踪的构建产物），没有就退化成文件遍历。
			for _, f := range trackedFiles(t, root, dir) {
				if strings.HasSuffix(f, ".jar") {
					t.Fatalf("%s 里有 jar：%s（SDK 不能自带二进制依赖）", dir, f)
				}
			}
		}
	})
}

// TestSDKsDoNotCallTheNetworkDuringBuild 守"SDK 的构建不下载东西"。
//
// Node 的原生插件、Kotlin 的编译都不该顺手 `npm install` / 拉 Maven 包 ——
// 那会让"构建一次"变成"联网一次"，而红线的实质是"构建过程不引入第三方代码"。
func TestSDKsDoNotCallTheNetworkDuringBuild(t *testing.T) {
	root := repoRoot(t)
	// 只看**行首**的安装命令：文档里说"没有 Gradle 文件"是合规说明，
	// 而 `gradle build` 是一条让读者去联网的指令。按子串匹配会把前者判死。
	//
	// 例外：装**我们自己发布的包**是正当的（`pip install hermes_sdk`）——
	// 这条规则要防的是"SDK 的构建需要联网拉第三方依赖"，不是"不能安装 SDK 本身"。
	// Go 的 regexp 不支持负向前瞻，所以先粗匹配再逐个排除自己发布的包。
	forbidden := regexp.MustCompile(`(?mi)^\s*(npm\s+(install|i)\b[^\n]*|pip\s+install\b[^\n]*|yarn\s+add\b[^\n]*|mvn\b|gradle\b|dotnet\s+add\s+package\b[^\n]*)`)
	// 我们自己的分发名：安装它是正当的，不属于"构建要联网拉第三方"。
	ownPackage := regexp.MustCompile(`(?i)(hermes_sdk|@hermes/)`)
	dirs := []string{
		"packages/python/README.md", "packages/javascript/README.md",
		"packages/typescript/README.md", "packages/go/README.md",
		"packages/rust/README.md", "packages/java/README.md",
		"packages/csharp/README.md", "packages/kotlin/README.md",
	}
	for _, d := range dirs {
		p := filepath.Join(root, d)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		for _, m := range forbidden.FindAllString(readFile(t, p), -1) {
			if ownPackage.MatchString(m) {
				continue // 安装我们的包是文档该做的事
			}
			t.Fatalf("%s 里出现安装指令 %q —— SDK 的构建不该要求联网拉第三方依赖", d, m)
		}
	}
}

// trackedFiles 返回一个目录下的文件（相对 root）。
//
// 有 .git 且 git 可用时用索引：能自动排除未跟踪的构建产物（out.jar 等）。
// 否则（比如从 zipball 解出来的源码）退化为遍历，并跳过 .git / build / node_modules。
func trackedFiles(t *testing.T, root, dir string) []string {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, ".git")); err == nil {
		if out, err := exec.Command("git", "-C", root, "ls-files", dir).Output(); err == nil {
			var files []string
			for _, f := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if f != "" {
					files = append(files, f)
				}
			}
			return files
		}
	}
	base := filepath.Join(root, dir)
	var files []string
	_ = filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "build", "node_modules", "out", "bin", "obj", "target", "__pycache__":
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	return files
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读不到 %s: %v", p, err)
	}
	return string(b)
}

func grepLine(text, needle string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return "(未找到)"
}
