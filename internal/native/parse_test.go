package native

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"hermes/internal/ir"
)

// 这些测试验证 cgo 绑定的**真实行为**，不是"函数存在"。
// 关键断言用的是 golden 已经钉死的数字（support.hermes 的 AST 节点数 = 90），
// 所以它能同时抓住"绑定传错参数"与"前端行为变了"两类问题。

func TestParseVersionIsReported(t *testing.T) {
	v := ParseVersion()
	if v == "" {
		t.Fatal("ParseVersion 返回空串：hermes_parser.h 的 hm_parse_version 没接上")
	}
	if !strings.HasPrefix(v, "hermes-parser/") {
		t.Errorf("版本串格式不对：%q（期望前缀 hermes-parser/）", v)
	}
}

func TestParseSupportBotMatchesGoldenNodeCount(t *testing.T) {
	root := repoRootForTest(t)
	src, err := os.ReadFile(filepath.Join(root, "examples", "support_bot", "support.hermes"))
	if err != nil {
		t.Fatalf("读不到 support.hermes：%v", err)
	}

	res, err := Parse(src, "support.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}

	// golden 的 [#1] 断言：support.hermes AST 节点数 = 90。
	// 这个数字来自 tests/compiler/ast-support_bot.golden，两边必须一致。
	if res.NodeCount != 90 {
		t.Errorf("节点数 = %d，golden 是 90：要么绑定传错了 is_header，要么前端行为变了", res.NodeCount)
	}
	if res.Dump == "" {
		t.Fatal("Dump 为空：AST dump 没拿到")
	}
	if !strings.Contains(res.Dump, "Module") {
		t.Errorf("Dump 里没有 Module 节点，前 200 字节：%q", firstN(res.Dump, 200))
	}
	if !strings.HasSuffix(res.Dump, "\n") {
		t.Error("Dump 不以换行结尾：dump_node 每行都带 \\n，说明尾部被截断了")
	}

	// 诊断必须是合法 JSON（成功时也可能是空数组，但必须是 JSON）。
	var diags []map[string]any
	if err := json.Unmarshal([]byte(res.Diag), &diags); err != nil {
		t.Errorf("Diag 不是合法 JSON 数组：%v；原文前 200 字节：%q", err, firstN(res.Diag, 200))
	}
}

func TestParseHeaderRejectsBodyWorkflow(t *testing.T) {
	// `.h` 里出现带体 workflow → E3005（LANGUAGE §9.4）。
	src := []byte("export workflow f() -> string { return \"x\"; }\n")
	res, err := Parse(src, "x.h", true)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if !strings.Contains(res.Diag, "E3005") {
		t.Errorf("is_header=true 时带体 workflow 应报 E3005，实际诊断：%q", firstN(res.Diag, 300))
	}

	// 同一份源码按 .hermes 解析则不该报 E3005（对照组：证明不是恒真）。
	res2, err := Parse(src, "x.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if strings.Contains(res2.Diag, "E3005") {
		t.Errorf("is_header=false 时不该报 E3005，实际诊断：%q", firstN(res2.Diag, 300))
	}
}

func TestParseCountAgreesWithParse(t *testing.T) {
	src := []byte("export struct S { a: int; }\nexport workflow f(input: S) -> string { return \"x\"; }\n")
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	n, err := ParseCount(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("ParseCount 失败：%v", err)
	}
	if n != res.NodeCount {
		t.Errorf("ParseCount=%d 与 Parse.NodeCount=%d 不一致：两条路径走了不同的字段", n, res.NodeCount)
	}
	if n == 0 {
		t.Error("节点数为 0：解析器没产出任何节点")
	}
}

func TestKindNameRoundTrip(t *testing.T) {
	// Kind 0 是 Module（ast.h 的 enum 顺序）。这里不硬编码全部枚举，
	// 只验证"能翻出名且名不为空"，以及非法值的兜底。
	if got := KindName(0); got == "" || got == "?" {
		t.Errorf("KindName(0) = %q，期望非空且不是兜底值（0 应是 Module）", got)
	}
	// 越界值应给兜底 "?" 而不是空串或崩溃。
	if got := KindName(99999); got != "?" {
		t.Errorf("KindName(99999) = %q，期望兜底 \"?\"", got)
	}
}

func TestParseRejectsEmptySource(t *testing.T) {
	if _, err := Parse(nil, "x.hermes", false); err == nil {
		t.Error("空源码应该报错，不应静默返回空 AST")
	}
}

// repoRootForTest 向上找 go.mod。本包不依赖 tests/guard 的 repoRoot（那是另一个包）。
func repoRootForTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd 失败：%v", err)
	}
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("找不到 go.mod：测试必须从仓库内运行")
	return ""
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// T11 第一层的端到端验证：字段访问检查（E4016）。
//
// 这条走的是"对非 struct 取字段"这条路：`n` 被推导成 int，`.foo` 就不合法。
// 它证明类型检查真的接进了 hm_parse —— 不是"文件存在"，而是"推导出的类型
// 参与了判定"。
func TestTypeCheckRejectsFieldOnNonStruct(t *testing.T) {
	src := []byte(`
workflow f(input: json) -> string {
  auto n = 1;
  return n.foo;
}
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if !strings.Contains(res.Diag, "E4016") {
		t.Fatalf("期望 E4016（int 上取 .foo），实际诊断：%s", firstN(res.Diag, 600))
	}
	if !strings.Contains(res.Diag, "foo") || !strings.Contains(res.Diag, "int") {
		t.Errorf("E4016 没说清\"在什么类型上取什么字段\"：%s", firstN(res.Diag, 600))
	}
}

// 对照组：合法代码不能被误报 —— 证明上一条不是恒真。
func TestTypeCheckAcceptsValidProgram(t *testing.T) {
	src := []byte(`
export struct Intent { string action; string order_id; }
workflow f(input: json) -> string {
  auto n = 1;
  return "ok";
}
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if strings.Contains(res.Diag, "E4016") {
		t.Errorf("合法程序被报了 E4016：%s", firstN(res.Diag, 600))
	}
}

// T11 的核心路径：`model.generate<Intent>(...)` 的 struct 类型推导 + 字段检查。
//
// 这一条同时验证两件事：
//
//	① `CallExpr.type_args` 带得出 Intent（dump 里它是 CallExpr 的第二个子节点）
//	② 拿到 struct 后字段查找能给出**可用字段清单**
//
// 注意 args 是 `Param` 节点而不是表达式（命名参数 `prompt = "x"`），
// 检查器对 Param 走 default 分支返回 unknown，这是有意的：命名参数的值
// 已经在 parser 里作为 Param.child 挂着了。
func TestTypeCheckStructFieldViaGeneric(t *testing.T) {
	src := []byte(`
export struct Intent { string action; string order_id; }
workflow f(input: json) -> string {
  auto i = model.generate<Intent>(prompt = "x");
  return i.oder_id;
}
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if !strings.Contains(res.Diag, "E4016") {
		t.Fatalf("期望 E4016（Intent 没有 oder_id），实际诊断：%s", firstN(res.Diag, 700))
	}
	// 修复建议里必须列出 Intent 的真实字段 —— 这是"比 Python 好"的具体兑现：
	// AttributeError 只说没有 oder_id，不说有 action / order_id。
	if !strings.Contains(res.Diag, "action") || !strings.Contains(res.Diag, "order_id") {
		t.Errorf("E4016 没列出 Intent 的可用字段：%s", firstN(res.Diag, 800))
	}
}

// 对照组：同一个 struct，写对字段名时不能报错 —— 证明上一条不是恒真。
func TestTypeCheckStructFieldViaGenericAcceptsCorrect(t *testing.T) {
	src := []byte(`
export struct Intent { string action; string order_id; }
workflow f(input: json) -> string {
  auto i = model.generate<Intent>(prompt = "x");
  return i.order_id;
}
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if strings.Contains(res.Diag, "E4016") {
		t.Errorf("字段名正确却报了 E4016：%s", firstN(res.Diag, 700))
	}
}

func repoRootCabi(t *testing.T) string { return repoRootForTest(t) }

// E4002：`catch (Error e)` 之后还有 catch —— 后者永远不可达。
//
// 语言只有一层继承（6 个错误类型并列，"兄弟互不匹配"），所以 Error 是唯一
// 可能被写反的父类型。写反的后果不是报错，而是专用分支永远进不去、
// 错误静默降级到通用分支。
func TestTypeCheckRejectsCatchAfterError(t *testing.T) {
	src := []byte(`
workflow f(input: json) -> string {
  try {
    return "ok";
  } catch (Error e) {
    return "wide";
  } catch (HttpError h) {
    return "narrow";
  }
}
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if !strings.Contains(res.Diag, "E4002") {
		t.Fatalf("期望 E4002（Error 之后的 catch 不可达），实际诊断：%s", firstN(res.Diag, 700))
	}
	if !strings.Contains(res.Diag, "swap") {
		t.Errorf("E4002 没给出「换顺序」的修复建议：%s", firstN(res.Diag, 800))
	}
}

// 对照组：具体类型在前、Error 在最后 —— 这是正确写法，不能报。
func TestTypeCheckAcceptsSpecificCatchFirst(t *testing.T) {
	src := []byte(`
workflow f(input: json) -> string {
  try {
    return "ok";
  } catch (HttpError h) {
    return "narrow";
  } catch (Error e) {
    return "wide";
  }
}
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if strings.Contains(res.Diag, "E4002") {
		t.Errorf("正确顺序（具体在前）被误报 E4002：%s", firstN(res.Diag, 700))
	}
}

// catch 体**必须被检查**：原来 TryStmt 走的是 block_(s->args)，
// 把 CatchClause 当语句遍历，catch 块里的字段错误全被漏掉。
func TestTypeCheckChecksInsideCatchBody(t *testing.T) {
	src := []byte(`
workflow f(input: json) -> string {
  try {
    return "ok";
  } catch (Error e) {
    auto n = 1;
    return n.foo;
  }
}
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if !strings.Contains(res.Diag, "E4016") {
		t.Errorf("catch 体内的 E4016 被漏掉了（说明 catch body 没被遍历）：%s", firstN(res.Diag, 700))
	}
}

// E4018：match 缺 default 分支。
//
// 被匹配的是字符串，"所有情况都覆盖了"无法静态证明。没有 default 时，
// 一个没人预料到的取值就没有分支可走 —— 这类缺口只在"那个没人试过的输入"
// 上暴露。
func TestTypeCheckRejectsMatchWithoutDefault(t *testing.T) {
	src := []byte(`
workflow f(input: json) -> string {
  auto x = "a";
  match (x) {
    case "a": { return "got a"; }
  }
  return "none";
}
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if !strings.Contains(res.Diag, "E4018") {
		t.Fatalf("期望 E4018（match 没有 default），实际诊断：%s", firstN(res.Diag, 700))
	}
	if !strings.Contains(res.Diag, "default") {
		t.Errorf("E4018 的修复建议里没提 default：%s", firstN(res.Diag, 800))
	}
}

// 对照组：有 default 时不报（否则整门语言没法用）。
func TestTypeCheckAcceptsMatchWithDefault(t *testing.T) {
	src := []byte(`
workflow f(input: json) -> string {
  auto x = "a";
  match (x) {
    case "a": { return "got a"; }
    default: { return "other"; }
  }
}
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if strings.Contains(res.Diag, "E4018") {
		t.Errorf("有 default 却报 E4018：%s", firstN(res.Diag, 700))
	}
}

// 真实样例不能被误报 —— examples/support_bot 的 match 有 default。
func TestTypeCheckAcceptsSupportBotMatch(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(repoRootForTest(t), "examples", "support_bot", "support.hermes"))
	if err != nil {
		t.Fatal(err)
	}
	res, perr := Parse(src, "support.hermes", false)
	if perr != nil {
		t.Fatalf("Parse 失败：%v", perr)
	}
	if strings.Contains(res.Diag, "E4018") {
		t.Errorf("examples/support_bot 的 match 被误报 E4018：%s", firstN(res.Diag, 700))
	}
}

// E5002：跨 parallel 块引用块内的声明。
//
// 每个分支并发执行，块内产生的值在块外不存在 —— 结果只经块自己的
// `return [...]` 带出来。引用它是"读一个没有任何分支拥有的槽位"。
func TestTypeCheckRejectsOutOfScopeParallelVar(t *testing.T) {
	src := []byte(`
workflow f(input: json) -> string {
  auto r = parallel {
    auto a = 1;
    return [a];
  };
  return a;
}
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if !strings.Contains(res.Diag, "E5002") {
		t.Fatalf("期望 E5002（a 在 parallel 块内声明），实际诊断：%s", firstN(res.Diag, 700))
	}
	// 必须说清"怎么把结果拿出来"。
	if !strings.Contains(res.Diag, "auto [") {
		t.Errorf("E5002 没给出解构的修复建议：%s", firstN(res.Diag, 800))
	}
}

// 对照组：块**外**声明的变量在块内使用是合法的。
// 没有这条，E5002 可能被实现成"parallel 里什么都不许引用"而自以为通过。
func TestTypeCheckAcceptsVarDeclaredBeforeParallel(t *testing.T) {
	src := []byte(`
workflow f(input: json) -> string {
  auto a = 1;
  auto r = parallel {
    return [a];
  };
  return "ok";
}
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if strings.Contains(res.Diag, "E5002") {
		t.Errorf("块外声明的变量被误报 E5002：%s", firstN(res.Diag, 700))
	}
}

// E5010：parallel 体内写体外绑定。两种写语句（`x = expr;` / `x++;`）都必须被拒。
//
// 运行期每个分支拿到的是父作用域的副本（internal/engine/run.go 的 cloneFrames /
// runBranches），这类写入落在副本上、出分支即丢弃，恒等于零效果。「不报错 +
// 不生效」是最糟的组合，所以硬错：无迁移期警告、无 opt-in 开关。
func TestTypeCheckRejectsWritesToOuterVarInParallel(t *testing.T) {
	src := []byte(`
workflow f(input: json) -> int {
  auto x = 0;
  auto r = parallel {
    x = 1;
    x++;
    return [x];
  };
  return x;
}
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	// 两条写语句各报一条：少一条就说明漏掉了 `=` 或 `++` 的一种。
	if got := strings.Count(res.Diag, `"code":"E5010"`); got != 2 {
		t.Fatalf("期望 2 条 E5010（`x = 1` 与 `x++`），实际 %d 条：%s",
			got, firstN(res.Diag, 900))
	}
	// 修复建议要指向"用 join 值带出去"，而不是让用户加锁或另开开关。
	if !strings.Contains(res.Diag, "auto [") || !strings.Contains(res.Diag, "return [") {
		t.Errorf("E5010 没给出解构 / join 的修复建议：%s", firstN(res.Diag, 900))
	}
}

// 对照组 ①：读体外变量必须继续合法 —— 那是 §5.2 里真正成立的部分。
// 读走的是 IdentExpr，不经过写检查；这条同时防"顺手把 parallel 里所有引用禁掉"。
func TestTypeCheckAcceptsReadOfOuterVarInParallel(t *testing.T) {
	src := []byte(`
workflow f(input: json) -> int {
  auto x = 7;
  auto r = parallel {
    auto y = x + 1;
    return [y];
  };
  return x;
}
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if strings.Contains(res.Diag, "E5010") {
		t.Errorf("读体外变量被误报 E5010：%s", firstN(res.Diag, 700))
	}
	if strings.Contains(res.Diag, "E5002") {
		t.Errorf("读体外变量被误报 E5002：%s", firstN(res.Diag, 700))
	}
}

// 对照组 ②：分支自己声明的绑定可以写。判据是「绑定下标 >= 最内层 parallel
// 水位」，不是「parallel 里什么都不许写」—— 没有这条，把检查写成恒真也会自以为通过。
func TestTypeCheckAcceptsWriteToVarDeclaredInParallel(t *testing.T) {
	src := []byte(`
workflow f(input: json) -> int {
  auto r = parallel {
    auto local = 0;
    local = 3;
    local++;
    return [local];
  };
  return 0;
}
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if strings.Contains(res.Diag, "E5010") {
		t.Errorf("分支内声明的绑定被误报 E5010：%s", firstN(res.Diag, 700))
	}
}

// 嵌套：只看最内层水位。内层分支同样拿到外层分支作用域的副本，所以「外层
// parallel 里声明的 y」对内层也是体外绑定，写它同样是死写；而内层自己声明的
// z 与外层自己声明的 y 在外层写都合法。
func TestTypeCheckRejectsWritesInNestedParallel(t *testing.T) {
	src := []byte(`
workflow f(input: json) -> int {
  auto x = 0;
  auto r = parallel {
    auto y = 0;
    auto s = parallel {
      x = 1;
      y = 2;
      auto z = 0;
      z = 3;
      return [z];
    };
    y = 4;
    return [y];
  };
  return x;
}
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	// x = 1 与 y = 2 各一条；z = 3（内层自己声明）与 y = 4（外层自己声明）
	// 不能报 —— 若被报，条数就会是 4 而不是 2。
	if got := strings.Count(res.Diag, `"code":"E5010"`); got != 2 {
		t.Fatalf("期望 2 条 E5010（内层分支写体外 x / y），实际 %d 条：%s",
			got, firstN(res.Diag, 900))
	}
	if !strings.Contains(res.Diag, "`x`") || !strings.Contains(res.Diag, "`y`") {
		t.Errorf("E5010 没点名被写的体外绑定：%s", firstN(res.Diag, 900))
	}
}

// 回归：parallel 分支里调用可归约的非递归函数（if/return 决策树），必须照样
// 编过、结果正确。
//
// 这条测试是「E5010 必须落在源码 AST、不能落 codegen」这个设计选择存在的理由：
// 路线 A 内联会把 pick 的函数体归约成指向 __hm_inline_r_N 的 assign 节点，
// 在 IR 上判「目标必须声明在本分支内」会把 `auto t = pick(3);` —— 用户一行
// 赋值都没写 —— 判成非法。源码 AST 里 `pick(3)` 只是 CallExpr，assign 还不存在。
func TestTypeCheckAcceptsInlinedFunctionInParallel(t *testing.T) {
	src := []byte(`
int pick(int n) {
  if (n > 2) { return 7; }
  return 3;
}
export workflow main() entry -> string {
  auto [v, w] = parallel {
    auto t = pick(3);
    auto u = 5;
    return [t, u];
  };
  if (v == 7) { return "ok"; }
  return "bad";
}
`)

	// ① 源码 AST 层：一条 error 都不该有。
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if strings.Contains(res.Diag, "E5010") {
		t.Fatalf("内联调用被误报 E5010：%s", firstN(res.Diag, 900))
	}
	if strings.Contains(res.Diag, `"severity":"error"`) {
		t.Fatalf("编译期不该有 error：%s", firstN(res.Diag, 900))
	}

	// ② 全链路：hermes ir 能编过，且 IR 里确实有内联合成的 assign / 结果变量。
	//    若检查误落在 codegen，这份源码根本编不出 IR。
	root := repoRootForTest(t)
	bin := filepath.Join(root, "build", "hermes")
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "case.hermes")
	irPath := filepath.Join(dir, "out.ir.json")
	if err := os.WriteFile(srcPath, src, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(bin, "ir", srcPath, "-o", irPath).CombinedOutput(); err != nil {
		t.Fatalf("hermes ir 失败（内联被误伤）：%v\n%s", err, out)
	}
	data, err := os.ReadFile(irPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "__hm_inline_r_") {
		t.Errorf("IR 里没有内联结果变量，说明没走到内联归约路径：%s", firstN(string(data), 900))
	}
	if !strings.Contains(string(data), `"k":"assign"`) {
		t.Errorf("IR 里没有内联合成的 assign 节点：%s", firstN(string(data), 900))
	}

	// ③ 结果正确：pick(3) 走 n > 2 分支 ⇒ 7 ⇒ "ok"。内联若被误伤（ir 失败）
	//    或归约结果不对，这里要么编不过、要么返回 "bad"。
	out, err := exec.Command(bin, "run", irPath).CombinedOutput()
	if err != nil {
		t.Fatalf("hermes run 失败：%v\n%s", err, out)
	}
	var got struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("解析 run 输出失败：%v\n%s", err, out)
	}
	if got.Value != "ok" {
		t.Fatalf("期望 \"ok\"（pick(3)=7），实际 %q；完整输出：%s", got.Value, firstN(string(out), 900))
	}
}

// E5010 回归（QA Round 1）：写语句藏进 `for` / `while` / `match` 臂 / `retry`
// **体内**时也必须被拒。
//
// 阶段 B 之前，这四种体在 check_type.cpp::stmt_() 里根本没被遍历（RetryStmt
// 读错槽走 args、MatchStmt 走 arm->args、For/While 没有 case），但 codegen 会
// 处理它们，于是写确实进了 IR —— 运行期落在分支作用域的副本上、静默丢弃，
// 正是「不报错 + 不生效」。Round 1 的修法是在 parallel_() 内做一次**独立的**
// 写语句子树扫描（覆盖 AST 全部子节点槽位），刻意不补齐 stmt_() 的遍历，
// 以免语义扩张。阶段 B 已补齐 stmt_() 对这四类体的下潜，但 E5010 的**唯一
// 命中点仍然是那次独立扫描**：stmt_() 的 AssignStmt / UpdateStmt case 保持
// `break;`，否则 block_() 路径与扫描器会对同一条写各报一条（重复诊断）。
// 这里的用例因此在阶段 B 前后都成立，且顺带守住了"两条遍历路径不打架"。
//
// 每个子用例都断言**恰好 1 条** E5010：多一条说明重复诊断，少一条说明又漏了。
// 同时守 E1001（语法错会让 AST 残缺，1 条 E5010 也可能是假绿）。
func TestTypeCheckRejectsWritesInControlFlowBodiesInParallel(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "for_body",
			src: `
workflow f(input: json) -> int {
  auto x = 7;
  auto r = parallel {
    for (int i = 0; i < 1; i++) {
      x = 1;
    }
    return [x];
  };
  return x;
}
`,
		},
		{
			name: "while_body",
			src: `
workflow f(input: json) -> int {
  auto x = 7;
  auto r = parallel {
    while (x > 0) {
      x = 1;
    }
    return [x];
  };
  return x;
}
`,
		},
		{
			name: "match_arm",
			src: `
workflow f(input: json) -> int {
  auto x = 7;
  auto r = parallel {
    match (x) {
      case 7: { x = 1; }
      default: { }
    }
    return [x];
  };
  return x;
}
`,
		},
		{
			name: "retry_body",
			src: `
workflow f(input: json) -> int {
  auto x = 7;
  auto r = parallel {
    retry(2) {
      x = 1;
    }
    return [x];
  };
  return x;
}
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Parse([]byte(tc.src), "t.hermes", false)
			if err != nil {
				t.Fatalf("Parse 失败：%v", err)
			}
			if strings.Contains(res.Diag, "E1001") {
				t.Fatalf("用例本身有语法错误（AST 可能残缺，1 条 E5010 会是假绿）：%s", firstN(res.Diag, 900))
			}
			if got := strings.Count(res.Diag, `"code":"E5010"`); got != 1 {
				t.Fatalf("期望恰好 1 条 E5010，实际 %d 条：%s", got, firstN(res.Diag, 900))
			}
		})
	}
}

// 嵌套去重：扫描器遇到嵌套 ParallelExpr 会压入新水位再递归，内层 parallel_()
// 因「不是最外层」不再发起扫描，因此同一条写只判一次。
//
// 两个子用例分别覆盖：嵌套 parallel 在 parallel 体顶层（QA 的
// RejectsWritesInNestedParallel 形态，2 条写 → 恰好 2 条，不能变 4 条），
// 以及嵌套 parallel 藏在 for 体里（stmt_() 完全走不到、只能靠扫描器递归，
// 1 条写 → 恰好 1 条，不能变 2 条）。
func TestTypeCheckNestedParallelDiagnosticsAreNotDoubled(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want int
	}{
		{
			name: "nested_at_top_level",
			src: `
workflow f(input: json) -> int {
  auto x = 0;
  auto r = parallel {
    auto y = 0;
    auto s = parallel {
      x = 1;
      y = 2;
      auto z = 0;
      z = 3;
      return [z];
    };
    y = 4;
    return [y];
  };
  return x;
}
`,
			want: 2,
		},
		{
			name: "nested_in_for_body",
			src: `
workflow f(input: json) -> int {
  auto x = 7;
  auto r = parallel {
    for (int i = 0; i < 1; i++) {
      auto s = parallel {
        x = 1;
        return [x];
      };
    }
    return [x];
  };
  return x;
}
`,
			want: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Parse([]byte(tc.src), "t.hermes", false)
			if err != nil {
				t.Fatalf("Parse 失败：%v", err)
			}
			if strings.Contains(res.Diag, "E1001") {
				t.Fatalf("用例本身有语法错误：%s", firstN(res.Diag, 900))
			}
			if got := strings.Count(res.Diag, `"code":"E5010"`); got != tc.want {
				t.Fatalf("期望恰好 %d 条 E5010（嵌套去重，不能翻倍），实际 %d 条：%s",
					tc.want, got, firstN(res.Diag, 900))
			}
		})
	}
}

// E3016：用了 tool.call / model.generate 却没链对应的 std 模块。
//
// 这条是**单文件**能判定的事（#link 就写在同一个文件里），所以它在 C++ 侧；
// 跨文件的链接解析（符号到底存不存在）才是 Go 侧 internal/link 的活。
func TestTypeCheckRequiresStdLinkForToolCall(t *testing.T) {
	src := []byte(`
workflow f(input: json) -> string {
  auto o = tool.call(url = "http://example.com");
  return "ok";
}
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if !strings.Contains(res.Diag, "E3016") {
		t.Fatalf("期望 E3016（没有 #link <std/net>），实际诊断：%s", firstN(res.Diag, 700))
	}
	// 修复建议必须给出可直接复制的那一行。
	if !strings.Contains(res.Diag, "#link <std/net>") {
		t.Errorf("E3016 没给出可复制的 #link 行：%s", firstN(res.Diag, 800))
	}
}

// 对照组：写了 #link <std/net>; 就不能再报 E3016 —— 证明上一条不是恒真。
func TestTypeCheckAcceptsToolCallWithStdLink(t *testing.T) {
	src := []byte(`
#link <std/net>;
workflow f(input: json) -> string {
  auto o = tool.call(url = "http://example.com");
  return "ok";
}
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if strings.Contains(res.Diag, "E3016") {
		t.Errorf("已写 #link <std/net>; 却仍报 E3016：%s", firstN(res.Diag, 700))
	}
}

// E4021：声明返回 string、实际 return int。
func TestTypeCheckReturnTypeMismatch(t *testing.T) {
	src := []byte(`
workflow f(input: json) -> string {
  return 42;
}
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if !strings.Contains(res.Diag, "E4021") {
		t.Fatalf("期望 E4021（声明 string、返回 int），实际诊断：%s", firstN(res.Diag, 700))
	}
	if !strings.Contains(res.Diag, "string") || !strings.Contains(res.Diag, "int") {
		t.Errorf("E4021 没说清期望与实际：%s", firstN(res.Diag, 800))
	}
}

// 对照组：类型一致时不报。
func TestTypeCheckReturnTypeMatch(t *testing.T) {
	src := []byte(`
workflow f(input: json) -> string {
  return "ok";
}
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if strings.Contains(res.Diag, "E4021") {
		t.Errorf("类型一致却报了 E4021：%s", firstN(res.Diag, 700))
	}
}

// E3019：backend 地址字面量里带 user:pass@。
//
// 为什么值得一个错误码：地址会被写进 IR、.hbc、签名产物、explain 输出。
// 一个带凭据的字面量会沿所有这些路径扩散，而修复动作（换成 env）与
// "地址写错了"完全不同。
func TestTypeCheckRejectsCredentialsInAddress(t *testing.T) {
	src := []byte(`
backend b = "http://user:pass@example.com";
workflow f(input: json) -> string { return "ok"; }
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if !strings.Contains(res.Diag, "E3019") {
		t.Fatalf("期望 E3019（地址带凭据），实际诊断：%s", firstN(res.Diag, 700))
	}
	// 修复建议要有可复制的那一行。
	if !strings.Contains(res.Diag, "env(") {
		t.Errorf("E3019 没给出 env( 形式的修复建议：%s", firstN(res.Diag, 800))
	}
}

// env 的**默认值**也是字面量，同样要查 —— 漏这条等于留了个后门。
func TestTypeCheckRejectsCredentialsInEnvDefault(t *testing.T) {
	src := []byte(`
backend b = env("URL", "http://user:pass@example.com");
workflow f(input: json) -> string { return "ok"; }
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if !strings.Contains(res.Diag, "E3019") {
		t.Fatalf("env 的默认值里带凭据也应报 E3019，实际：%s", firstN(res.Diag, 700))
	}
}

// 对照组：不带凭据的地址、以及 env 单实参形式，都不能报。
func TestTypeCheckAcceptsCleanAddress(t *testing.T) {
	for _, src := range []string{
		"backend b = \"http://example.com\";\nworkflow f(input: json) -> string { return \"ok\"; }\n",
		"backend b = env(\"URL\");\nworkflow f(input: json) -> string { return \"ok\"; }\n",
	} {
		res, err := Parse([]byte(src), "t.hermes", false)
		if err != nil {
			t.Fatalf("Parse 失败：%v", err)
		}
		if strings.Contains(res.Diag, "E3019") {
			t.Errorf("干净地址被误报 E3019：%s\n源：%s", firstN(res.Diag, 500), src)
		}
	}
}

// E4022：不能 return 一个 tuple。
//
// tuple 只由 parallel 产生，且不可命名不可存储（IR.md §3.1）。return 它
// 等于把一个"类型系统描述不了的东西"交给调用方 —— 返回契约根本写不下来。
func TestTypeCheckRejectsReturningTuple(t *testing.T) {
	src := []byte(`
#link <std/net>;
workflow f(input: json) -> string {
  return parallel {
    auto a = 1;
    auto b = 2;
    return [a, b];
  };
}
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if !strings.Contains(res.Diag, "E4022") {
		t.Fatalf("期望 E4022（return 了 tuple），实际诊断：%s", firstN(res.Diag, 700))
	}
	// 修复建议要给出"先解构"这个动作。
	if !strings.Contains(res.Diag, "parallel") {
		t.Errorf("E4022 的修复建议没说清该怎么做：%s", firstN(res.Diag, 800))
	}
}

// TestCodegenFunctionsEmitsRecursiveTable 守 hm_codegen_functions 的**真实产物**。
//
// 它同时证明三件事：函数表是独立 JSON 数组（不是 workflow 的字段）、
// 递归由调用图判定（fib → fib）、调用点生成 call_fn。
func TestCodegenFunctionsEmitsRecursiveTable(t *testing.T) {
	src := []byte(`
int fib(int n) {
    if (n < 2) { return n; }
    return fib(n - 1) + fib(n - 2);
}
`)
	js, diag, err := CodegenFunctions(src, "t.hermes", false, "")
	if err != nil {
		t.Fatalf("CodegenFunctions 失败：%v", err)
	}
	if diag != "" && diag != "[]" {
		t.Fatalf("不该有诊断：%s", firstN(diag, 600))
	}

	var fns []struct {
		Name      string `json:"name"`
		Recursive bool   `json:"recursive"`
		Params    []struct {
			N string `json:"n"`
		} `json:"params"`
		Nodes []struct {
			K  string `json:"k"`
			Fn string `json:"fn"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(js), &fns); err != nil {
		t.Fatalf("函数表不是合法 JSON：%v\n%s", err, firstN(js, 600))
	}
	if len(fns) != 1 {
		t.Fatalf("函数表应有 1 项，实际 %d：%s", len(fns), firstN(js, 600))
	}
	if fns[0].Name != "fib" || !fns[0].Recursive {
		t.Errorf("期望 fib/recursive=true，实际 %q/%v", fns[0].Name, fns[0].Recursive)
	}
	if len(fns[0].Params) != 1 || fns[0].Params[0].N != "n" {
		t.Errorf("参数表不对：%+v", fns[0].Params)
	}
	calls := 0
	for _, n := range fns[0].Nodes {
		if n.K == "call_fn" && n.Fn == "fib" {
			calls++
		}
	}
	if calls != 2 {
		t.Errorf("函数体里应有 2 个 fib 调用点，实际 %d", calls)
	}
}

// TestCodegenFunctionsEmptyIsArray 守"没有函数"的形状：必须是空数组而不是 null。
// null 在 Go 里也能解成 nil slice，所以它不会当场炸 —— 但下游 map[string]any
// 的消费者会看到 null，而 schema 写的是 array。契约要在边界上说清楚。
// ---------------------------------------------------------------------------
// fallback（2.0.0）：错误边界，不是异常
//
// 语义是「main 返回非零 C ABI 状态码时改跑块并取它的返回值」。
// 前端本来就 -fno-exceptions 编译，**零栈展开是既成事实** —— 这些用例里
// 没有任何一处依赖 throw / catch。
// ---------------------------------------------------------------------------

// 规格给的三条示例必须都编得过：表达式位置、块内读外部变量、纯副作用位置。
func TestFallbackCompilesInSpecShapes(t *testing.T) {
	cases := []struct{ name, src string }{
		{
			name: "expression_value",
			src: `#link <std/model>
export workflow main() entry -> string {
  auto result = model.generate(prompt = "hi") fallback { return "degraded reply"; };
  return result;
}
`,
		},
		{
			name: "block_reads_outer_var",
			src: `#link <std/net>
export workflow main() entry -> json {
  auto cached = "cached";
  auto data = tool.call(url = "https://api.example.com/x") fallback { return cached; };
  return data;
}
`,
		},
		{
			name: "side_effect_only",
			src: `#link <std/net>
export workflow main() entry -> string {
  tool.call(url = "https://api.example.com/x") fallback { auto t = 1; };
  return "ok";
}
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Codegen([]byte(tc.src), "t.hermes", false)
			if err != nil {
				t.Fatalf("Codegen 调用失败：%v", err)
			}
			if strings.Contains(res.Diag, "E1001") {
				t.Fatalf("用例本身有语法错误，后面的断言会是假绿：%s", firstN(res.Diag, 900))
			}
			if strings.TrimSpace(res.Diag) != "" && strings.TrimSpace(res.Diag) != "[]" {
				t.Fatalf("这条示例必须零诊断，实际：%s", firstN(res.Diag, 900))
			}
			if !strings.Contains(res.JSON, `"k":"fallback"`) {
				t.Fatalf("IR 里没有 fallback 节点：%s", firstN(res.JSON, 900))
			}
			// main / body 两个子节点都必须真的写进 IR：只写一个的话，
			// 运行期不是"降级路径不存在"就是"主路径不存在"，而 IR 仍然合法。
			if !strings.Contains(res.JSON, `"main":`) || !strings.Contains(res.JSON, `"body":`) {
				t.Fatalf("fallback 节点缺 main/body 槽位：%s", firstN(res.JSON, 900))
			}
		})
	}
}

// E4025：降级块 return 的类型必须等于 main_expr 的类型。
//
// 注意这里刻意用**静态可推**的 main（字面量 1 → int），不用
// `model.generate(...)`：后者的类型在单文件检查器里是 Unknown（它不读
// std 模块的返回类型），Unknown 会让 E4025 主动跳过 —— 于是「拿
// model.generate 测 E4025」既测不到，又与「示例 1 必须编得过」直接矛盾。
// 这条矛盾已如实报给 team-lead，不靠放宽检查器绕过。
// TestTypeCheckRejectsFallbackReturnTypeMismatch 是 E4025 的**钉子**。
//
// 它专门防一类退化：fallback_() 在任一侧是 `Unknown` 时会跳过比对（否则
// `model.generate(prompt = "hi")` 这种没写 `<T>`、类型为 Unknown 的写法会被
// 误报 E4025，而规格里的示例 1 必须编得过）。但「跳过 Unknown」很容易被顺手
// 扩大成「干脆不检查」，那时所有 fallback 类型错误都会静默通过。
//
// 这里两侧都是**静态可推**的确定类型（`1` 是 int、`"s"` 是 string），所以
// 只要 E4025 的比对还在，这条就必须红。故意不用 model.generate：它的类型是
// Unknown，用它当反例会让测试在「比对被删掉」时照样通过 —— 那是假绿。
func TestTypeCheckRejectsFallbackReturnTypeMismatch(t *testing.T) {
	src := []byte(`export workflow main() entry -> int {
  auto y = 1 fallback { return "s"; };
  return y;
}
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if got := strings.Count(res.Diag, `"code":"E4025"`); got != 1 {
		t.Fatalf("期望恰好 1 条 E4025，实际 %d 条：%s", got, firstN(res.Diag, 900))
	}
}

// E4026：降级块内不许给 const 绑定重新赋值。
func TestTypeCheckRejectsConstWriteInFallback(t *testing.T) {
	src := []byte(`#link <std/net>
export workflow main() entry -> string {
  const auto n = 1;
  tool.call(url = "https://api.example.com/x") fallback { n = 2; };
  return "ok";
}
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if got := strings.Count(res.Diag, `"code":"E4026"`); got != 1 {
		t.Fatalf("期望恰好 1 条 E4026，实际 %d 条：%s", got, firstN(res.Diag, 900))
	}
}

// 非 const 绑定在降级块里可写（E4026 不能误伤普通变量）。
func TestTypeCheckAcceptsWriteToNonConstInFallback(t *testing.T) {
	src := []byte(`#link <std/net>
export workflow main() entry -> string {
  auto n = 1;
  tool.call(url = "https://api.example.com/x") fallback { n = 2; };
  return "ok";
}
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if strings.Contains(res.Diag, "E4026") {
		t.Fatalf("普通变量不该报 E4026：%s", firstN(res.Diag, 900))
	}
}

// `fallback` 在 2.0.0 是**保留字**，这是本版的破坏性变更。
// 写成测试是为了让"1.x 的源码不再编得过"这件事有据可查，而不是口头声明。
func TestFallbackIsReservedWord(t *testing.T) {
	src := []byte(`export workflow main() entry -> string {
  auto fallback = 1;
  return "ok";
}
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if !strings.Contains(res.Diag, "E1001") {
		t.Fatalf("`auto fallback = 1;` 必须报语法错（2.0.0 破坏性变更），实际：%s", firstN(res.Diag, 900))
	}
}

// 降级块的**体**必须被类型检查下潜。
//
// 这条与 11 形态守卫等价，只是对象是 FallbackExpr（表达式 Kind，不在
// stmt_() 的 switch 里）。把 fallback_() 里的 block_(body) 删掉，这条就红。
func TestTypeCheckDiagnosesInsideFallbackBody(t *testing.T) {
	src := []byte(`#link <std/net>
export workflow main() entry -> string {
  tool.call(url = "https://api.example.com/x") fallback {
    auto n = 1;
    auto bad = n.foo;
  };
  return "ok";
}
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if strings.Contains(res.Diag, "E1001") {
		t.Fatalf("用例本身有语法错误，1 条 E4016 会是假绿：%s", firstN(res.Diag, 900))
	}
	if got := strings.Count(res.Diag, `"code":"E4016"`); got != 1 {
		t.Fatalf("fallback 块内的字段访问错必须报恰好 1 条 E4016，实际 %d 条：%s", got, firstN(res.Diag, 900))
	}
}

func TestCodegenFunctionsEmptyIsArray(t *testing.T) {
	js, _, err := CodegenFunctions([]byte("workflow f() -> int { return 1; }\n"), "t.hermes", false, "")
	if err != nil {
		t.Fatalf("CodegenFunctions 失败：%v", err)
	}
	if strings.TrimSpace(js) != "[]" {
		t.Fatalf("没有函数时应返回 []，实际 %q", firstN(js, 200))
	}
}

// ---------------------------------------------------------------------------
// 阶段 C：11 形态表驱动防复发守卫
//
// 用户原话：「这条测试是本轮唯一必须留下的东西。E5010 修的是症状，它守的是病因。」
// 病因是 check_type.cpp::stmt_() 曾整类跳过 match 臂 / while 体 / for 体 /
// retry 体 —— 这四类体里的字段错 / 返回类型错 / 实参错全部零诊断编成 IR。
//
// statementShapeCases() 是**唯一数据源**：功能测试与两条元守卫都读它，避免
// 「表」与「守卫」各写一份、日后悄悄失配：
//   · TestTypeCheckDiagnosesInsideEveryStatementShape —— 每个形态必报 E4016，
//     不含 E1001（防 AST 残缺假绿），并跑合法对照（防「恒真报错」假绿）。
//   · TestStatementShapeGuardCoversAllStatementKinds —— 遍历 ast.h::Kind 全枚举，
//     新语句 Kind / 漏形态立刻红；len(cases)==11 硬断言。
//   · TestStmtSwitchCoversEveryStatementShape —— 文本扫描 stmt_() 的 case 集合，
//     与形态表对齐；少下潜一个体或读错槽位就红。
// ---------------------------------------------------------------------------

const wantStatementShapeCases = 11

type statementShapeCase struct {
	name string // 形态名（t.Run 子测试名）
	kind string // 拥有该体的 ast.h::Kind（元守卫按它对齐 switch / 全枚举）
	src  string // 包在 workflow 里、体内放探针 `auto n = 1; auto bad = n.foo;` 的源码
}

// statementShapeCases 返回 11 种「体内藏一处坏语句」的最小源码。修复前 6 个
// 报 E4016（顶层 / if / else / try / catch / parallel 分支），5 个零诊断
// （match 臂 / match default / while 体 / for 体 / retry 体）—— 用户原话
// 「缺任何一个形态，测试就红」。两条臂是两个独立形态，缺一个守卫就复发。
func statementShapeCases() []statementShapeCase {
	return []statementShapeCase{
		{
			name: "top_level",
			kind: "Block",
			src: `export workflow main() entry -> string {
  auto n = 1;
  auto bad = n.foo;
  return "ok";
}
`,
		},
		{
			name: "if_then",
			kind: "IfStmt",
			src: `export workflow main() entry -> string {
  if (1 > 0) {
    auto n = 1;
    auto bad = n.foo;
  }
  return "ok";
}
`,
		},
		{
			name: "if_else",
			kind: "IfStmt",
			src: `export workflow main() entry -> string {
  if (1 > 0) { } else {
    auto n = 1;
    auto bad = n.foo;
  }
  return "ok";
}
`,
		},
		{
			name: "try_body",
			kind: "TryStmt",
			src: `export workflow main() entry -> string {
  try {
    auto n = 1;
    auto bad = n.foo;
  } catch (Error e) { }
  return "ok";
}
`,
		},
		{
			name: "catch_body",
			kind: "TryStmt",
			src: `export workflow main() entry -> string {
  try { } catch (Error e) {
    auto n = 1;
    auto bad = n.foo;
  }
  return "ok";
}
`,
		},
		{
			name: "parallel_branch",
			kind: "ParallelExpr",
			src: `export workflow main() entry -> string {
  auto [a] = parallel {
    auto n = 1;
    auto bad = n.foo;
    return [1];
  };
  return "ok";
}
`,
		},
		{
			name: "match_arm",
			kind: "MatchStmt",
			src: `export workflow main() entry -> string {
  auto subj = 7;
  match (subj) {
    case 7: {
      auto n = 1;
      auto bad = n.foo;
    }
    default: { }
  }
  return "ok";
}
`,
		},
		{
			name: "match_default",
			kind: "MatchStmt",
			src: `export workflow main() entry -> string {
  auto subj = 7;
  match (subj) {
    case 7: { }
    default: {
      auto n = 1;
      auto bad = n.foo;
    }
  }
  return "ok";
}
`,
		},
		{
			name: "while_body",
			kind: "WhileStmt",
			src: `export workflow main() entry -> string {
  while (1 > 2) {
    auto n = 1;
    auto bad = n.foo;
  }
  return "ok";
}
`,
		},
		{
			name: "for_body",
			kind: "ForStmt",
			src: `export workflow main() entry -> string {
  for (int i = 0; i < 1; i++) {
    auto n = 1;
    auto bad = n.foo;
  }
  return "ok";
}
`,
		},
		{
			name: "retry_body",
			kind: "RetryStmt",
			src: `export workflow main() entry -> string {
  retry(2) {
    auto n = 1;
    auto bad = n.foo;
  }
  return "ok";
}
`,
		},
	}
}

// TestTypeCheckDiagnosesInsideEveryStatementShape 是阶段 C 的功能守卫：对每一种
// 语句形态各放一处故意的字段错误（`n` 是 int，`.foo` 触发 E4016），断言「必须报
// 诊断」。修复前 match 臂 / while / for / retry 四形态零诊断，这条测试就红。
//
// 两个假绿陷阱都堵死：
//
//	· 断言不含 E1001 —— 语法错会让 AST 残缺，「恰好 1 条诊断」可能只是残缺产物；
//	· 合法对照 —— 把 n.foo 换成 n，同一形态必须 0 条 E4016，防「检查器恒真报错」。
func TestTypeCheckDiagnosesInsideEveryStatementShape(t *testing.T) {
	cases := statementShapeCases()
	if len(cases) != wantStatementShapeCases {
		t.Fatalf("11 形态表必须是 %d 条，实际 %d 条：增删形态必须同步更新两条元守卫", wantStatementShapeCases, len(cases))
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.src, "n.foo") {
				t.Fatalf("形态 %q 的源码里没有探针 n.foo", tc.name)
			}
			res, err := Parse([]byte(tc.src), "t.hermes", false)
			if err != nil {
				t.Fatalf("Parse 失败：%v", err)
			}
			if strings.Contains(res.Diag, "E1001") {
				t.Fatalf("用例本身有语法错误（AST 会残缺，E4016 可能是假绿）：%s", firstN(res.Diag, 900))
			}
			if !strings.Contains(res.Diag, "E4016") {
				t.Fatalf("体内探针没被诊断：期望至少 1 条 E4016，实际 0 条：%s", firstN(res.Diag, 900))
			}

			// 合法对照：把 .foo 换成合法表达式，其余一字不动。它证明 E4016 是
			// 「这个体里的字段访问」触发的，而不是检查器对该形态恒真报错。
			valid := strings.ReplaceAll(tc.src, "n.foo", "n")
			if valid == tc.src {
				t.Fatal("合法对照没构造出来（n.foo 未被替换）")
			}
			vres, err := Parse([]byte(valid), "t.hermes", false)
			if err != nil {
				t.Fatalf("合法对照 Parse 失败：%v", err)
			}
			if strings.Contains(vres.Diag, "E1001") {
				t.Fatalf("合法对照有语法错误：%s", firstN(vres.Diag, 900))
			}
			if strings.Contains(vres.Diag, "E4016") {
				t.Fatalf("合法对照不该报 E4016（检查器恒真报错 = 功能用例假绿）：%s", firstN(vres.Diag, 900))
			}
		})
	}
}

// kindClass 给 ast.h::Kind 全枚举逐项分类。这是「新增语句形态若用了新 Kind，
// 测试立刻红」的落点：新 Kind 不在这里登记就红；删 / 改名的 Kind 也会红。
//
//	body   —— 拥有嵌套语句体，必须由 11 形态表覆盖（8 个）
//	leaf   —— 语句，但没有嵌套语句体，藏不进一处坏语句（白名单）
//	helper —— 由父 case 代扫的子节点，不是独立语句（白名单）
//	decl   —— 声明 / 结构节点，不是语句
//	expr   —— 表达式节点，不是语句
var kindClass = map[string]string{
	// body（8）：11 形态表依赖的 8 个 Kind
	"Block": "body", "IfStmt": "body", "WhileStmt": "body", "ForStmt": "body",
	"RetryStmt": "body", "TryStmt": "body", "MatchStmt": "body", "ParallelExpr": "body",
	// leaf（7）：无体语句，白名单并注理由
	"LetStmt": "leaf", "AssignStmt": "leaf", "UpdateStmt": "leaf", "ReturnStmt": "leaf",
	"ExprStmt": "leaf", "BreakStmt": "leaf", "ContinueStmt": "leaf",
	// helper（4）：由父 case 代扫，无独立 case、无独立体
	"Pattern": "helper", "MatchArm": "helper", "CatchClause": "helper", "ParallelReturn": "helper",
	// decl（17）
	"Module": "decl", "Include": "decl", "Link": "decl", "StructDecl": "decl",
	"FunctionDecl": "decl", "SecurityDecl": "decl", "SecurityRule": "decl", "Field": "decl",
	"Type": "decl", "BackendSig": "decl", "BackendDef": "decl", "ToolDecl": "decl",
	"WorkflowSig": "decl", "WorkflowDecl": "decl", "Param": "decl", "Annotation": "decl",
	"StringPartNode": "decl",
	// expr（11）
	"CallExpr": "expr", "PathExpr": "expr", "LitExpr": "expr", "IdentExpr": "expr",
	"BinaryExpr": "expr", "UnaryExpr": "expr", "IndexExpr": "expr", "UnlimitedExpr": "expr",
	"ArrayLit": "expr", "EnvExpr": "expr",
	// FallbackExpr 是**表达式**（2.0.0）：在 type_of_ 分派、由 fallback_() 下潜，
	// 不在 stmt_() 的 switch 里，所以不属于 11 形态（那 11 个是**语句**形态）。
	// 它的体仍需被守住 —— 由 TestTypeCheckDiagnosesInsideFallbackBody 单独覆盖，
	// 那条测的就是「fallback 块里漏下潜就红」，与形态守卫等价。
	"FallbackExpr": "expr",
}

// kindWhitelistReason 给每个无体 / 由父 case 代扫的 Kind 写清「为什么不需要
// 独立形态」。元守卫报错时直接打出来，省得再翻代码。
var kindWhitelistReason = map[string]string{
	"LetStmt":        "语句，但 init 是表达式不是语句体；它本身就是 11 形态表里每个 case 的第一行",
	"AssignStmt":     "叶子写语句，没有嵌套语句体",
	"UpdateStmt":     "叶子自增语句，没有嵌套语句体",
	"ReturnStmt":     "叶子返回语句，值是表达式不是语句体",
	"ExprStmt":       "叶子表达式语句，没有嵌套语句体",
	"BreakStmt":      "循环控制，无体；check_type 不需要为它下潜",
	"ContinueStmt":   "循环控制，无体；check_type 不需要为它下潜",
	"Pattern":        "LetStmt 绑定的子结构，由 LetStmt case 收全 next 链，不是独立语句",
	"MatchArm":       "MatchStmt case 在臂链上代扫（block_(arm->child)），不是独立语句",
	"CatchClause":    "TryStmt case 在 catch 链上代扫（block_(c->else_branch)），不是独立语句",
	"ParallelReturn": "parallel 体内的 return [...]，由 parallel_() 处理，不是独立语句",
}

// allKindNames 遍历 cgo 暴露的 KindName 全枚举（底层是 parser.cpp::kind_name，
// 越界返回 "?" 兜底），拿到当前二进制里真实存在的全部 Kind 名。
func allKindNames() []string {
	var out []string
	for i := 0; i < 512; i++ {
		n := KindName(i)
		if n == "" || n == "?" {
			break
		}
		out = append(out, n)
	}
	return out
}

// TestStatementShapeGuardCoversAllStatementKinds 是第一条元守卫：兑现
// check_type.cpp:1113-1114 注释里写下的名字。它不检查功能，只检查「守卫本身
// 还完整」—— 新增语句 Kind 未登记、body 类 Kind 漏形态、Kind 被删 / 改名、
// len(cases)!=11，任一情况立刻红。这是防复发机制里「新形态逼出红」的那一环。
func TestStatementShapeGuardCoversAllStatementKinds(t *testing.T) {
	cases := statementShapeCases()
	if len(cases) != wantStatementShapeCases {
		t.Fatalf("11 形态表必须是 %d 条，实际 %d 条：新增语句形态必须同步更新 wantStatementShapeCases 与两条元守卫", wantStatementShapeCases, len(cases))
	}

	covered := map[string]bool{}
	for _, c := range cases {
		if c.kind == "" {
			t.Errorf("形态 %q 缺 kind 字段", c.name)
		}
		covered[c.kind] = true
	}

	enumerated := map[string]bool{}
	for _, n := range allKindNames() {
		enumerated[n] = true
		cls, ok := kindClass[n]
		if !ok {
			t.Errorf("KindName 出现未登记的 Kind %q：新增语句形态？请分类并补 11 形态守卫", n)
			continue
		}
		if cls == "body" && !covered[n] {
			t.Errorf("语句 Kind %q 拥有语句体，却没有对应的 11 形态守卫", n)
		}
	}
	for n := range kindClass {
		if !enumerated[n] {
			t.Errorf("kindClass 登记了 %q，但 ast.h::Kind 全枚举里已无此 Kind（删/改名？）", n)
		}
	}
	for n := range covered {
		if kindClass[n] != "body" {
			t.Errorf("形态表覆盖了 %q，但它被分类为 %q：只有 body 类需要形态守卫", n, kindClass[n])
		}
	}
	// 白名单逐项都要能说清理由，防止有人把 Kind 随手塞进白名单绕过守卫。
	for n, cls := range kindClass {
		if (cls == "leaf" || cls == "helper") && kindWhitelistReason[n] == "" {
			t.Errorf("白名单 Kind %q 没写理由", n)
		}
	}
}

// TestStmtSwitchCoversEveryStatementShape 是第二条元守卫：兑现
// check_type.cpp:1115-1116 注释里写下的名字。它文本扫描 check_type.cpp 里
// stmt_() 的 case Kind:: 集合，与 11 形态表对齐 —— 少下潜一个体（case 落回
// default: break）或把槽位读错（改回 arm->args）就红。default 保留是阶段 C
// 第 3 条的评估结论，一并钉住。
func TestStmtSwitchCoversEveryStatementShape(t *testing.T) {
	root := repoRootForTest(t)
	data, err := os.ReadFile(filepath.Join(root, "native", "cpp", "check_type.cpp"))
	if err != nil {
		t.Fatalf("读不到 check_type.cpp：%v", err)
	}
	src := string(data)
	start := strings.Index(src, "void stmt_(Node* s) {")
	if start < 0 {
		t.Fatal("找不到 `void stmt_(Node* s) {`：函数改名了？守卫要同步")
	}
	rest := src[start:]
	end := strings.Index(rest, "\n  bool same_type_(")
	if end < 0 {
		t.Fatal("找不到 stmt_() 之后的 same_type_()：切片边界失效，守卫要同步")
	}
	// 先剥注释再扫，不能直接 strings.Contains 原文：`check_type.cpp:1082-1085`
	// 的注释里写着「原来 block_(arm->args) 遍历的是一个恒为空的槽位」，那是一条
	// **正确的警告**，但会被 Contains 当成「代码里又出现了」——本守卫初版就这么
	// 假红过一次。剥注释后断言只反映真实代码。
	stmtBody := stripCPPComments(rest[:end])

	// 保留 default 是阶段 C 第 3 条的评估结论（去掉会被 -Wswitch -Werror 逼出
	// 约 25 行纯噪声空 case）。它同时也是「切片确实截到了 stmt_() 尾部」的凭据。
	if !strings.Contains(stmtBody, "default: break;") {
		t.Fatalf("stmt_() 里找不到 `default: break;`：要么兜底被去掉（要同步改评估注释），要么切片边界错了。切片前 200 字节：%q", firstN(stmtBody, 200))
	}

	switchKinds := map[string]bool{}
	for _, m := range regexp.MustCompile(`case\s+Kind::(\w+)\s*:`).FindAllStringSubmatch(stmtBody, -1) {
		switchKinds[m[1]] = true
	}
	if len(switchKinds) == 0 {
		t.Fatal("stmt_() 里一条 case Kind:: 都没扫到：正则或函数体切片失效")
	}

	// 与 11 形态表对齐：每个形态依赖的 Kind 都必须有 case，少下潜一个体就红。
	for _, c := range statementShapeCases() {
		if !switchKinds[c.kind] {
			t.Errorf("形态 %q 依赖 `case Kind::%s:`，但 stmt_() 里没有：会落 default: break，体被整类跳过", c.name, c.kind)
		}
	}

	// 阶段 B 修的四类体：case 存在还不够，必须真的下潜进正确槽位。这两条同时
	// 把「改回 arm->args / 删掉 s->body」的回归钉死（功能用例也守，但这里给出
	// 更早、更定位的诊断）。
	if !strings.Contains(stmtBody, "block_(s->body)") {
		t.Error("stmt_() 里没有 `block_(s->body)`：while / for 的体没被下潜")
	}
	if !strings.Contains(stmtBody, "block_(arm->child)") {
		t.Error("stmt_() 里没有 `block_(arm->child)`：match 臂体没被下潜")
	}
	if strings.Contains(stmtBody, "block_(arm->args)") {
		t.Error("stmt_() 又出现 `block_(arm->args)`：MatchArm 的体在 child 不在 args（阶段 B 已修，别改回去）")
	}
}

// stripCPPComments 去掉 C++ 源码里的 `//` 行注释与 `/* */` 块注释，字符串与
// 字符字面量原样保留（字面量里的 `//` 不是注释）。
//
// 为什么需要它：元守卫要断言「stmt_() 的代码里不再有 block_(arm->args)」，
// 而修复处的注释恰恰写着「原来 block_(arm->args) 遍历的是一个恒为空的槽位」。
// 直接对原文 strings.Contains 会把这条**正确的警告**判成回归 —— 守卫自己骗自己。
// 另一种修法是改注释措辞绕开字面量，但那只是把同一个陷阱留给下一个写注释的人。
func stripCPPComments(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '"' || c == '\'':
			q := c
			b.WriteByte(c)
			i++
			for i < len(src) {
				if src[i] == '\\' && i+1 < len(src) {
					b.WriteByte(src[i])
					b.WriteByte(src[i+1])
					i += 2
					continue
				}
				b.WriteByte(src[i])
				if src[i] == q {
					i++
					break
				}
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			i += 2
			for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
				i++
			}
			if i+1 < len(src) {
				i += 2 // 吃掉闭合的 */
			} else {
				i = len(src) // 未闭合到 EOF：吞掉剩余全部，别把最后一个字符漏回正文
			}
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// assign 的 span 传播：codegen 与 bounds.go 必须对同一个节点算出同一个数
//
// 缺陷（本条测试的来源）：`assign` 的右值在 engine/run.go 里是**真的会被执行**
// 的（`case "assign": r.expr(*n.E)`），所以 `x = parallel { ... }` 里的
// parallel 会实打实产生 span。codegen 的 AssignStmt 分支却把
// span_/model_/tool_[assign] 一律写死 0 —— 于是 bounds.max_spans 偏小，
// 装配期 E_IR_RECOMPUTE_DRIFT 把一份合法源码判成"编译器 bug"。
//
// 为什么断言一个**具体数字**而不是 "> 0"："> 0" 对"给 assign 自己加 1"也成立，
// 而那是另一种错（bounds.go 的 assign 只加 span(e)，不加 1 —— 对照 try /
// fallback 才加 1）。只有钉死数值才能同时排掉"不传播"（0）与"多加 1"（2）。
// ---------------------------------------------------------------------------

// assembleFirstWorkflow 把 Codegen 产出的第一个 workflow 装配成完整 IR，
// 走的是 `hermes ir` 的同一条路径（含 Validate 的派生字段复算）。
// 返回的 error 就是装配/校验失败时的原文，测试可以断言里面没有 drift。
func assembleFirstWorkflow(t *testing.T, src, name string) (*ir.Doc, error) {
	t.Helper()
	res, err := Codegen([]byte(src), name, false)
	if err != nil {
		t.Fatalf("Codegen 调用失败：%v", err)
	}
	if d := strings.TrimSpace(res.Diag); d != "" && d != "[]" {
		t.Fatalf("源码必须零诊断，实际：%s", firstN(res.Diag, 900))
	}
	if strings.TrimSpace(res.JSON) == "" {
		t.Fatal("Codegen 产出为空 JSON")
	}
	// codegen 的产出可能是单对象（当前绑定形态）或数组（D-037 的多 workflow
	// 形态）。两种都认：这条测试要守的是上界代数，不是线上形状 —— 形状由
	// cmd/hermes/ir_build.go 与 golden 钉着。装配层按"一份 workflow 一份 IR"
	// 工作，所以数组时取第一份。
	wfJSON := []byte(res.JSON)
	var items []json.RawMessage
	if err := json.Unmarshal(wfJSON, &items); err != nil {
		if json.Valid(wfJSON) && strings.HasPrefix(strings.TrimSpace(res.JSON), "{") {
			items = []json.RawMessage{json.RawMessage(wfJSON)}
		} else {
			t.Fatalf("codegen 产出既不是 workflow 对象也不是数组：%v；原文前 300 字节：%q",
				err, firstN(res.JSON, 300))
		}
	}
	if len(items) == 0 {
		t.Fatal("codegen 产出里没有 workflow")
	}
	assembly := ir.Assembly{
		Name:      name,
		EntryPath: name,
		// 单模块：loc.m 恒为 0，所以一张模块表就够（跨文件装配在 cmd 侧）。
		Modules:         []ir.Module{{Path: name}},
		CompilerName:    "hermes-parse",
		CompilerVersion: "test",
		ParserABI:       1,
		LanguageVersion: ir.IRVersionSupported,
		TypeModule:      0,
		BackendsJSON:    []byte("[]"),
	}
	return ir.AssembleUnit(assembly, items[0])
}

// TestAssignSpanPropagatesToBounds 是这条缺陷的**钉子**。
//
// 右值是 parallel（1 个 span）→ 传播后 max_spans = 1。
//   · 不传播（修复前）：0  ← 装配期报 E_IR_RECOMPUTE_DRIFT
//   · 传播 + 给 assign 自己多加 1：2  ← 另一种错，同样被这个数字排掉
func TestAssignSpanPropagatesToBounds(t *testing.T) {
	src := `workflow w() -> int {
  auto x = 1;
  x = parallel { return [2]; };
  return x;
}
`
	doc, err := assembleFirstWorkflow(t, src, "assign_span.hermes")
	if err != nil {
		t.Fatalf("合法源码必须装配成功，实际报错：%v", err)
	}
	if got := doc.Workflows[0].Bounds.MaxSpans; got != 1 {
		t.Errorf("bounds.max_spans = %d，期望 1（assign 传播 parallel 的 1 个 span，"+
			"且 assign 自己不加 1）：codegen 的 AssignStmt 与 bounds.go 的 assign 又分岔了",
			got)
	}
}

// TestAssignPureRValueAddsNoSpan 是反向用例：纯右值赋值不产生 span。
//
// 它守的是"修反了"这一类错：把 assign 的 span 写成 `1 + span(e)`（照抄 try /
// fallback 的形状）能让上面那条测试也……不，会让它变成 2 —— 但更要紧的是，
// 它会让每一条普通赋值都凭空多出一个 span，而运行时那条 right-hand side 一次
// 调用都没打。所以纯右值必须**保持原来的上界 0**。
func TestAssignPureRValueAddsNoSpan(t *testing.T) {
	src := `workflow w() -> int {
  auto x = 1;
  x = 2;
  return x;
}
`
	doc, err := assembleFirstWorkflow(t, src, "assign_pure.hermes")
	if err != nil {
		t.Fatalf("合法源码必须装配成功，实际报错：%v", err)
	}
	if got := doc.Workflows[0].Bounds.MaxSpans; got != 0 {
		t.Errorf("纯右值赋值的 bounds.max_spans = %d，期望 0："+
			"`x = 2` 不调模型也不调工具，给它算 span 就是把上界凭空放大", got)
	}
}
