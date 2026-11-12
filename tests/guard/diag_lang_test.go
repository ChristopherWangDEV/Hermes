package guard_test

// 守「编译器诊断与所有用户可见报错默认英文」这条用户可见承诺。
//
// 为什么现有的 i18n_test.go 不够：它只扫 cmd/hermes 里 Fprint*/Printf/Println
// 的行，看不到两处更大的漏网：
//   ① native/cpp/*.cpp —— C++ 前端把 message / why / fix 直接写进 out_diag
//      JSON；Go 侧 renderDiags 是**直接构造 diag.Diag 字面量**，从没走
//      diag.New / catalog / HERMES_LANG，所以语言包对它们完全不生效；
//   ② internal/**、tools/**、cmd/hermesrt/**、packages/** 里的
//      fmt.Errorf / errors.New / 生成的 C 头注释 / LSP hover 文档。
// 两者都会原样出现在默认（英文）界面里。
//
// 这里从两头堵：静态扫描覆盖所有没被测试走到的分支，端到端跑真二进制证明
// 「扫的确实是会输出的那份字节」。只有静态会漏拼接与生成产物，只有动态会漏
// 没走到的路径，所以两条都要。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var hanInDiag = regexp.MustCompile(`\p{Han}`)

// ---------------------------------------------------------------------------
// C++ 前端：字符串字面量级精确扫描
// ---------------------------------------------------------------------------

// cppStringLiterals 返回 src 中全部字符串字面量（含两端引号），跳过 // 注释、
// /* */ 块注释、'c' 字符字面量与 C++ 原始字符串 R"delim(...)delim"。
// 为什么不用正则：lexer.cpp 里 '\"' / '{' 这类字符字面量含引号，正则会把
// 从字符字面量到下一个引号之间的整段代码误认成字符串，产生假红。
func cppStringLiterals(src string) []string {
	var out []string
	i, n := 0, len(src)
	for i < n {
		c := src[i]
		if c == '/' && i+1 < n && src[i+1] == '/' {
			i += 2
			for i < n && src[i] != '\n' {
				i++
			}
			continue
		}
		if c == '/' && i+1 < n && src[i+1] == '*' {
			i += 2
			for i+1 < n && !(src[i] == '*' && src[i+1] == '/') {
				i++
			}
			if i+1 < n {
				i += 2
			}
			continue
		}
		// R"tag( ... )tag" / u8"..." 前缀：把 R 吃掉，让下面的 '"' 分支接手。
		if c == 'R' && i+1 < n && src[i+1] == '"' {
			start := i
			i += 2
			for i < n && src[i] != '(' {
				i++
			}
			if i < n {
				i++ // 吃掉 '('
			}
			for i < n && !(src[i] == ')' && i+1 < n && src[i+1] == '"') {
				i++
			}
			if i < n {
				i += 2
			}
			// 原始字符串是生成产物的模板，不是诊断文案；不纳入检查，
			// 但仍作为一个字面量返回，保持「扫到过」的可观测性。
			out = append(out, src[start:min(i, n)])
			continue
		}
		if c == '\'' { // 字符字面量
			i++
			for i < n {
				if src[i] == '\\' {
					i += 2
					continue
				}
				if src[i] == '\'' {
					i++
					break
				}
				i++
			}
			continue
		}
		if c == '"' {
			start := i
			i++
			for i < n {
				if src[i] == '\\' {
					i += 2
					continue
				}
				if src[i] == '"' {
					i++
					break
				}
				i++
			}
			out = append(out, src[start:min(i, n)])
			continue
		}
		i++
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestCppStringLiteralScannerCatchesChinese 是扫描器自己的元守卫。
// 项目里吃过「不做变异验证的守卫等于没有守卫」的亏：一条恒绿的守卫
// 和被注释掉的守卫没有区别。这里喂一段已知含中文字符串、中文注释、
// 中文字符字面量的假源码，证明扫描器确实抓得到、且不误伤注释。
func TestCppStringLiteralScannerCatchesChinese(t *testing.T) {
	const fixture = `// 这一行是中文注释，不该被算作输出
const char* a = "expected ']' after array type";
const char* b = "数组类型缺少 ']'";
const char* c = "plain english";
char q = '"';  // 字符字面量里带引号，不该让下一行被吞进来
/* 中文块注释 "里面还有假引号" */
const char* d = R"(raw 中文模板)";
`
	lits := cppStringLiterals(fixture)
	var withHan []string
	for _, l := range lits {
		if hanInDiag.MatchString(l) {
			withHan = append(withHan, l)
		}
	}
	// 只有 b 与原始字符串模板会被计入；注释与字符字面量不算。
	if len(withHan) == 0 {
		t.Fatal("扫描器没抓到任何中文字符串字面量 —— 这条守卫已经失效，所有 TestNoChineseInCppDiagnostics 都会恒绿")
	}
	joined := strings.Join(withHan, "\n")
	if !strings.Contains(joined, "数组类型缺少") {
		t.Fatalf("扫描器应抓到 b 的中文，实际：%s", joined)
	}
	// 注释里的假引号不能被当成字符串内容。
	for _, l := range lits {
		if strings.Contains(l, "中文块注释") {
			t.Fatalf("块注释被误判成字符串：%q", l)
		}
	}
}

// TestNoChineseInCppDiagnostics 守 C++ 前端：所有字符串字面量恒英文。
// 中文源码注释不在此列（它们不是输出）；catalog / 语言包在 Go 侧，另见下。
func TestNoChineseInCppDiagnostics(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "native", "cpp")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var bad []string
	for _, e := range entries {
		if e.IsDir() || !(strings.HasSuffix(e.Name(), ".cpp") || strings.HasSuffix(e.Name(), ".h")) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, lit := range cppStringLiterals(string(raw)) {
			if !hanInDiag.MatchString(lit) {
				continue
			}
			// C++ 原始字符串是生成产物模板，不进 out_diag；单独放行，
			// 但仍在报告里点名，避免它变成无声的中文后门。
			if strings.HasPrefix(lit, `R"`) {
				t.Logf("native/cpp/%s 的原始字符串含中文（生成产物模板，非诊断）：%.80s", e.Name(), lit)
				continue
			}
			bad = append(bad, e.Name()+": "+lit)
		}
	}
	if len(bad) > 0 {
		t.Fatalf("C++ 前端诊断文案必须恒英文（它们绕过 HERMES_LANG 直接进 out_diag）：\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ---------------------------------------------------------------------------
// Go 侧：用 go/parser 精确扫字符串字面量（注释天然不在 BasicLit 里）
// ---------------------------------------------------------------------------

// goChineseWhitelist 是「刻意保留中文」的精确豁免，每一条都要写清理由。
// 白名单按文件粒度，不允许出现通配：加宽一条就等于放一个人走后门。
var goChineseWhitelist = map[string]string{
	"internal/diag/catalog.go":          "catalogZH：显式的中文语言包，只在 HERMES_LANG=zh 时生效",
	"internal/diag/catalog_frontend.go": "catalogFrontendZH：C++ 前端诊断码的中文语言包，同样只在 HERMES_LANG=zh 时生效",
	"i18n/pack.go":                      "语言包加载器，中文是译文数据而非输出文案",
}

// TestNoChineseInGoUserVisibleStrings 守 Go 侧：除白名单外，源码里所有
// 字符串字面量恒英文。用 go/parser 而不是逐行正则，是因为中文注释、行尾
// 注释、raw string、跨行拼接都会让正则产生假红或漏报。
//
// pickUsage 的中文帮助块是**显式可切换**的（默认走英文那一版），按
// AST 精确定位到「pickUsage 的第二个参数」放行，不放宽到整个文件。
func TestNoChineseInGoUserVisibleStrings(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()
	var bad []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case "build", ".git", "scripts", "vendor", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if _, ok := goChineseWhitelist[filepath.ToSlash(rel)]; ok {
			return nil
		}
		if strings.HasPrefix(filepath.ToSlash(rel), "i18n/") {
			return nil // 语言包目录整体是译文数据
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		// pickUsage(en, zh) 的 zh 参数：唯一允许「默认不显示」的中文。
		pickArgs := map[token.Pos]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "pickUsage" {
				if lit, ok := call.Args[1].(*ast.BasicLit); ok {
					pickArgs[lit.Pos()] = true
				}
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || pickArgs[lit.Pos()] {
				return true
			}
			if !hanInDiag.MatchString(lit.Value) {
				return true
			}
			pos := fset.Position(lit.Pos())
			bad = append(bad, fmtPos(rel, pos.Line, lit.Value))
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) > 0 {
		t.Fatalf("Go 侧用户可见字符串必须默认英文（中文只能来自 catalog / 语言包 / pickUsage）：\n  %s",
			strings.Join(bad, "\n  "))
	}
}

func fmtPos(file string, line int, lit string) string {
	if len(lit) > 100 {
		lit = lit[:100] + "..."
	}
	return file + ":" + strconv.Itoa(line) + ": " + lit
}

// TestGoStringScannerCatchesChinese 是 Go 侧扫描器的元守卫：证明
// 「解析 + 白名单」这条路径真能抓到一条中文字符串，而不是恒绿。
func TestGoStringScannerCatchesChinese(t *testing.T) {
	const fixture = `package p
// 中文注释不该被算进来
func f() string { return "数组类型缺少 ']'" }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "fixture.go", fixture, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING && hanInDiag.MatchString(lit.Value) {
			found = true
		}
		return true
	})
	if !found {
		t.Fatal("go/parser 没抓到 BasicLit 里的中文字符串 —— Go 侧守卫已失效")
	}
}

// ---------------------------------------------------------------------------
// 端到端：跑真二进制，证明扫的确实是会输出的那份字节
// ---------------------------------------------------------------------------

const diagFixture = "workflow \"x\" () -> string;\n"

// TestCompilerDiagnosticsAreEnglishByDefault 守用户真正看到的那一行。
// 静态扫描会漏「字符串是运行时拼出来的」；这里真的编一份坏源码，
// 断言默认（无 HERMES_LANG）输出一个汉字都没有。
func TestCompilerDiagnosticsAreEnglishByDefault(t *testing.T) {
	bin := cliBinary(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "bad.hermes")
	if err := os.WriteFile(src, []byte(diagFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "ir", src)
	// 显式清掉再设空：开发机上的 HERMES_LANG 会让「默认」这一侧失真。
	cmd.Env = append(os.Environ(), "HERMES_LANG=")
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "E2002") {
		t.Fatalf("fixture 没触发 E2002，这条测试会空跑：\n%s", out)
	}
	if hanInDiag.Match(out) {
		t.Fatalf("默认英文模式下 `hermes ir` 的编译诊断输出含中文：\n%s", out)
	}
}

// TestCompilerDiagnosticsStayEnglishInChineseMode 守「中文模式不把编译诊断
// 也染成中文」：CLI 散文（hermes ir: ...）允许中文，但诊断正文恒英文。
// 只检查含诊断码的行，避免把 CLI 前缀的正常本地化误判成回归。
func TestCompilerDiagnosticsStayEnglishInChineseMode(t *testing.T) {
	bin := cliBinary(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "bad.hermes")
	if err := os.WriteFile(src, []byte(diagFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "ir", src)
	cmd.Env = append(os.Environ(), "HERMES_LANG=zh-CN")
	out, _ := cmd.CombinedOutput()
	codeLine := regexp.MustCompile(`E\d{4}`)
	sawDiag := false
	for _, line := range strings.Split(string(out), "\n") {
		if !codeLine.MatchString(line) {
			continue
		}
		sawDiag = true
		if hanInDiag.MatchString(line) {
			t.Errorf("HERMES_LANG=zh 时编译诊断行仍应恒英文：%s", line)
		}
	}
	if !sawDiag {
		t.Fatalf("fixture 没触发任何诊断行，这条测试会空跑：\n%s", out)
	}
}
