// 本文件守的是 docs/DECISIONS.md 的 **D-023**：
// HomonoiaScript（`.homo`）与 HermesScript（`.hermes`）是**正交两层**，
// 接触面是 HTTP 与错误词汇表，**不是文件格式**。
//
// D-023 的裁决是"路 A"：Hermes 不解析 `.homo`、不生成 `.homo`、
// 不 import `script_parser.py`。三条理由（层切分写不进文件 / 产物是 argv /
// `GATEWAY_KEYS` 里没有 upstream 概念）在 D-023 里，本文件不重复论证，
// 只把裁决变成可执行事实 —— 裁决一旦只写在文档里，下一个人读不到就照旧实现。
//
// 三块断言各自对应一种典型的越界方式：
//  1. 图省事，从 Go 里 import/调用对方的 Python 模块；
//  2. 图省事，在 Hermes 里长出 `.homo` 的解析或生成实现；
//  3. 图省事，在文档里提 `.homo` 却不指向裁决，于是下一个人不知道有裁决。
package guard_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// forbiddenPythonModules 是 D-023 禁止 Hermes 接线过去的 Python 侧模块片段。
//
// 判定"片段"而不是完整路径：`script_parser` 可能以任意包前缀出现，
// 完整路径匹配会在别人换一种 import 写法时静默失效。
var forbiddenPythonModules = []string{"script_parser", "homonoia"}

// skipWalkDirs 是遍历时整体跳过的目录：构建产物与依赖缓存里的 .go
// 不属于"我们自己写的代码"，扫它们只会拖慢守卫并制造噪声。
var skipWalkDirs = map[string]bool{
	"build":        true,
	".git":         true,
	"target":       true,
	"node_modules": true,
}

// walkGoFiles 遍历仓库所有 .go 文件，跳过构建产物目录与 tests/guard/testdata。
//
// testdata 是守卫自己的负样本区：将来若要在那里放"故意违规"的素材来
// 反向验证守卫本身有效，它不该让守卫变红。
func walkGoFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if skipWalkDirs[filepath.Base(path)] {
				return filepath.SkipDir
			}
			if filepath.Base(path) == "testdata" &&
				strings.Contains(filepath.ToSlash(path), "/tests/guard/") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(out) == 0 {
		t.Fatal("仓库里一个 .go 文件都没扫到 —— 巡检路径写错了，守卫在空集上通过等于没写")
	}
	return out
}

// TestHomoBoundaryNoPythonBridge 守住 D-023 路 A 的第一句：
// 「Hermes 不 import `script_parser.py`」。
//
// 为什么这条是硬边界而不是风格偏好：`script_parser.py` 是 Homonoia 的
// 部署文件编译器，它的"正确"由 `verify_argv` 把 argv 喂回真实 argparse 判定。
// 一旦 Hermes 从 Go 侧接线过去，就同时接过了它的 flag 命名、它的版本、
// 它的 Python 运行时 —— 而 D-023 事实②记着 `--batch` / `--batch-slots`
// 那次改名事故：参数名承载语义，改一次名字，这边静默读错一次且不报错。
//
// 检查方式（**只查 AST，不扫文件文本**）：注释里提到 `script_parser.py`
// 是允许的（本文件的注释就在提它），子串扫描会把我们自己的文档判成违规，
// 而一条会误报的守卫最终会被删掉 —— 那才是真正失去防护。
// 真正"可执行"的引用只有两种形态，两种都在 AST 里：
//   - `import` 一个含该片段的模块；
//   - 通过 `exec.Command` / `exec.CommandContext` 把它当参数交给子进程。
func TestHomoBoundaryNoPythonBridge(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()

	var importOffenders, execOffenders []string
	for _, path := range walkGoFiles(t, root) {
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)

		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("解析 %s 失败，守卫无法判定: %v", rel, err)
		}

		// ① import 路径。
		for _, spec := range f.Imports {
			p, uerr := strconv.Unquote(spec.Path.Value)
			if uerr != nil {
				continue
			}
			for _, bad := range forbiddenPythonModules {
				if strings.Contains(p, bad) {
					importOffenders = append(importOffenders, rel+" → "+p)
					break // 一个 import 只报一次：片段可能同时命中多项
				}
			}
		}

		// ② exec.Command 的实参字面量（真正的"可执行"引用）。
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "exec" {
				return true
			}
			if sel.Sel.Name != "Command" && sel.Sel.Name != "CommandContext" {
				return true
			}
			for _, arg := range call.Args {
				lit, ok := arg.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, uerr := strconv.Unquote(lit.Value)
				if uerr != nil {
					continue
				}
				for _, bad := range forbiddenPythonModules {
					if strings.Contains(v, bad) {
						execOffenders = append(execOffenders,
							rel+" → exec."+sel.Sel.Name+"("+strconv.Quote(v)+")")
						break
					}
				}
			}
			return true
		})
	}

	if len(importOffenders) > 0 {
		t.Fatalf("Hermes import 了 Homonoia 的 Python 模块（违反 D-023 路 A）：\n  %s\n"+
			"D-023 的接触面是 HTTP 与错误词汇表，不是 Python 模块。\n"+
			"需要的部署事实在运行期从 Homonoia 的 HTTP 面取（如 GET /v1/models），"+
			"不要跨语言拉一条实现依赖：那条依赖会把对方的 flag 命名与 Python 运行时都变成我们的。",
			strings.Join(importOffenders, "\n  "))
	}
	if len(execOffenders) > 0 {
		t.Fatalf("Hermes 通过子进程执行了 Homonoia 的 Python 脚本（违反 D-023 路 A）：\n  %s\n"+
			"shell 出去调 `script_parser.py` 与 import 它是同一件事，只是绕过了 import 检查。\n"+
			"部署事实该由 Homonoia 自己回答，Hermes 只认它的 HTTP 面（D-023 路 A）。",
			strings.Join(execOffenders, "\n  "))
	}
}

// TestHomoBoundaryNoImplementation 守住 D-023 路 A 的第二、三句：
// 「Hermes 不解析 `.homo`、不生成 `.homo`」。
//
// 判定方式刻意选得很笨：`internal/` 与 `cmd/` 下任何 .go 文件里出现字面量
// `.homo` 即 fail。理由是这个字面量**只有一种用途**——做扩展名识别、
// 词法、语法、或文件名拼装，也就是实现它；这两个目录里现在一次都不该出现。
// 笨判定的好处是不会漏：任何一处新增的文件格式实现，第一步都得写下这个字面量。
//
// 为什么在 Hermes 里实现它就是错的：`.homo` 描述"起什么进程、它们怎么连"，
// 而层切分这类信息 Homonoia 自己都拒绝写进文件（`_to_argv:1277-1280` 的
// `--split auto` 注释：切分取决于每个节点的内存与模型，编译器不能凭空发明）。
// 在 Hermes 里实现 `.homo` 等于养一个**第二事实源**，而它与真正的部署事实
// 必然分叉——`serve_models` 会过滤、`--model-dir` 会扫出更多、on-demand load
// 会加模型。分叉之后没有任何机制会报错，只会读到旧答案。
func TestHomoBoundaryNoImplementation(t *testing.T) {
	root := repoRoot(t)

	// `.homo` 作为**字面量**时可能出现的三种引号形态。
	// 不查裸的 `.homo`（无引号）：注释里提到这个概念是允许的，
	// 要提就该像本文件这样提，配上 D-023 与理由。
	literals := []string{
		`".homo"`,
		"`.homo`",
		`'.homo'`,
	}

	var offenders []string
	for _, dir := range []string{"internal", "cmd"} {
		base := filepath.Join(root, dir)
		if st, err := os.Stat(base); err != nil || !st.IsDir() {
			t.Fatalf("%s 不存在：守卫的巡检范围写错了，等于空转", dir)
		}
		err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if info.IsDir() {
				if skipWalkDirs[info.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			data, rerr := os.ReadFile(path)
			if rerr != nil {
				t.Fatalf("读 %s 失败: %v", path, rerr)
			}
			body := string(data)
			for _, lit := range literals {
				if strings.Contains(body, lit) {
					rel, _ := filepath.Rel(root, path)
					offenders = append(offenders,
						filepath.ToSlash(rel)+" 含字面量 "+lit)
					break
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}

	if len(offenders) > 0 {
		t.Fatalf("Hermes 的实现里出现了 `.homo`（违反 D-023 路 A）：\n  %s\n"+
			"`.homo` 是**部署文件**，不是编排语言：它回答「起什么进程」，"+
			"`.hermes` 回答「一次运行做什么」，两者正交。\n"+
			"部署事实该由 Homonoia 自己回答（D-023 路 A：运行期从 HTTP 面取）；"+
			"在 Hermes 里实现它等于养第二事实源——而意图与事实必然分叉，"+
			"分叉之后没有任何机制会报错，只会读到旧答案。",
			strings.Join(offenders, "\n  "))
	}
}

// TestHomoBoundaryDocsCiteDecision 守住"裁决可被下一个读者找到"。
//
// 前两条断言禁止越界，这一条防止越界以**文字**形态回来：有人在 docs/ 里
// 写下"以后要支持 `.homo`"这种待办，读者就以为这是允许的方向。
// 规则是文件级的：只要某份文档提到 `HomonoiaScript` 或 `.homo`，
// 它就必须同时出现 `D-023` —— 读者顺着编号能找到那三条硬理由，
// 而不是只看到一个孤零零的待办。
//
// 文件级而非行级判定，是为了抗改写：作者换一段话、换个位置写这条待办，
// 只要同文件里有引用就仍然合规；反之压成一行也不能绕过。
func TestHomoBoundaryDocsCiteDecision(t *testing.T) {
	root := repoRoot(t)
	docsDir := filepath.Join(root, "docs")
	entries, err := os.ReadDir(docsDir)
	if err != nil {
		t.Fatalf("读 docs/ 失败: %v", err)
	}

	var offenders []string
	mentioned := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, rerr := os.ReadFile(filepath.Join(docsDir, e.Name()))
		if rerr != nil {
			t.Fatalf("读 docs/%s 失败: %v", e.Name(), rerr)
		}
		body := string(data)
		if !strings.Contains(body, "HomonoiaScript") && !strings.Contains(body, ".homo") {
			continue
		}
		mentioned++
		if !strings.Contains(body, "D-023") {
			offenders = append(offenders, "docs/"+e.Name())
		}
	}

	// 空集守卫：如果 docs/ 里再没有任何一份文件提到 `.homo`，
	// 要么 D-023 被删了，要么巡检路径错了。两种情况都不该绿灯。
	if mentioned == 0 {
		t.Fatal("docs/ 下没有任何文件提到 HomonoiaScript 或 .homo —— " +
			"D-023 可能被删掉了，或者守卫的巡检路径失效。不许在空集上通过")
	}
	if len(offenders) > 0 {
		t.Fatalf("这些文档提到 `.homo` 却没有引用 D-023：\n  %s\n"+
			"提到一个被裁决禁止的方向，却不指向裁决，下一个人会以为它是待办而不是禁区。\n"+
			"请在文件里引用 D-023（三条硬理由都在那里），或者删掉这段描述。",
			strings.Join(offenders, "\n  "))
	}
}
