package native

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// E5002：跨 parallel 块引用块内的 let。
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
func TestCodegenFunctionsEmptyIsArray(t *testing.T) {
	js, _, err := CodegenFunctions([]byte("workflow f() -> int { return 1; }\n"), "t.hermes", false, "")
	if err != nil {
		t.Fatalf("CodegenFunctions 失败：%v", err)
	}
	if strings.TrimSpace(js) != "[]" {
		t.Fatalf("没有函数时应返回 []，实际 %q", firstN(js, 200))
	}
}
