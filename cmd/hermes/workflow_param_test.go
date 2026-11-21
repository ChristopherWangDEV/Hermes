package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"hermes/internal/native"
)

// D-035 之后，有体 workflow 的形参白名单只有两种**形态**：
// `input: T`（宿主 payload）与 `backend = <裸 backend 名>`（编译期选后端）。
// 其余具名形参在 parser 就报 E3026。
//
// 为什么这件事值得三条用例：判据是"这个名字有没有取值来源"，而不是"这个语法
// 解析得动"——`n: int` 能解析（parse_param 为 extern 签名复用），放行它只会得到
// "编得过、永远跑不起来"，而运行期那句 `variable 0 (n) was read before being
// assigned` 的措辞像编译器出了故障：把语言设计的缺口伪装成编译器的锅。
const (
	typedParamSrc = `export workflow f(n: int) entry {
  if (n > 0) { return "pos"; }
  return "neg";
}
`
	defaultParamSrc = `export workflow f(n = 5) entry {
  if (n > 0) { return "pos"; }
  return "neg";
}
`

	// 两条**合法**形态，用来防误伤：一个都不能被 E3026 打到。
	//   · 无体 `.h` 里的 extern 签名 —— `url: string` 是 C 调用 ABI 的位置参数，
	//     由 call_native 的调用点按位置喂值（走 parse_extern，不进 parse_workflow）。
	//   · 有体 workflow 的 `input: T` + `backend = <裸名>`。
	externHeaderSrc = `export struct NetResp { string body; }
export extern workflow net_get(url: string) -> NetResp;
`
	legalParamSrc = `export struct S { string q; }
export backend gw : openai = env("HERMES_TEST_URL", "");
export workflow f(input: S, backend = gw) entry -> string {
  return input.q;
}
`

	// 两文件场景：`.h` 里写**非 extern** 的签名。
	//
	// 这一条不是"重复一遍 n: int"——它守的是另一个洞。`is_header` 与 `is_extern`
	// 是两件正交的事，而曾经的白名单用 `!is_header` 当豁免，于是 `.h` 里这条
	// 声明被整段跳过：声明侧 `n: int`、定义侧 `input: json`，两边形参完全对不上，
	// 没有任何码报出来，`hermes ir` 退出码 0，产物合法。那不是"漏报一条诊断"，
	// 是**一条可以任意声明形参、且定义不必匹配的后门**。
	headerDeclSrc = `export workflow f(n: int) entry -> string;
`
	bodyDefSrc = `#include "x.h"

export workflow f(input: json) entry { return "x"; }
`
)

// compileFilesFrom 把一组文件写进同一个临时目录，编 entry，返回
// (退出码, stderr 全文, IR 路径)。
//
// 为什么不复用 compileInlineSource：`#include "x.h"` 是**相对引号包含**，
// 依赖同目录解析；只写一个文件的辅助函数建不出两文件场景，而这恰好是要测的东西。
func compileFilesFrom(t *testing.T, files map[string]string, entry string) (int, string, string) {
	t.Helper()
	dir := t.TempDir()
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names) // 写入顺序确定，失败时可复现
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(files[n]), 0o644); err != nil {
			t.Fatalf("写 %s: %v", n, err)
		}
	}
	outPath := filepath.Join(dir, "out.ir.json")
	stderrPath := filepath.Join(dir, "stderr.txt")
	ef, err := os.Create(stderrPath)
	if err != nil {
		t.Fatal(err)
	}
	code := func() int {
		old := os.Stderr
		os.Stderr = ef
		defer func() { os.Stderr = old }()
		return run([]string{"ir", filepath.Join(dir, entry), "-o", outPath})
	}()
	if err := ef.Close(); err != nil {
		t.Fatal(err)
	}
	msg, err := os.ReadFile(stderrPath)
	if err != nil {
		t.Fatal(err)
	}
	return code, string(msg), outPath
}

// diagEntry 是 `hermes ir` 在 gen 阶段打印的诊断数组里的一项。
//
// 只声明断言用得上的字段：字段少一个不会因为产物加字段而误红，字段多一个也
// 不影响 json 解码。loc 必须是**数值 + 模块名**两个都拿到，才能证明"错在声明侧"
// 而不是"错在定义侧" —— 只看 `line 1` 分不出是 x.h 还是 main.hermes 的第 1 行。
type diagEntry struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	Line     int    `json:"line"`
	Col      int    `json:"col"`
	Module   string `json:"module"`
}

// parseDiagArray 从 stderr 里抠出那段诊断 JSON 数组。
//
// 为什么要从 stderr 里抠而不是让 run() 直接返回结构：既有的
// compileInlineSource 也只拿得到文本，改 run() 的返回类型会牵动一大票调用点，
// 而这一刀只该动 workflow 形参。抠法是"首 '[' 到末 ']'"，因为 gen 阶段
// stderr 的形态就是 `hermes ir: <file>: failed to ...:` + 一行紧凑 JSON。
// 形态一变这里会返回 false，测试会带着完整 stderr 报错，不会静默漏断言。
func parseDiagArray(stderr string) ([]diagEntry, bool) {
	i := strings.Index(stderr, "[")
	j := strings.LastIndex(stderr, "]")
	if i < 0 || j <= i {
		return nil, false
	}
	var out []diagEntry
	if err := json.Unmarshal([]byte(stderr[i:j+1]), &out); err != nil {
		return nil, false
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// paramsOf 取 `hermes ir` 产物里 workflows[0].params 的**原始**形状。
//
// 断言落在 map[string]any 上而不是 ir.Param 结构体上：结构体分不出"键不存在"
// 与"键是 null"，而 `$defs/param` 的 required 是 ["n","v"]，这两件事在
// 线格式上并不等价。
func paramsOf(t *testing.T, irPath string) []any {
	t.Helper()
	raw, err := os.ReadFile(irPath)
	if err != nil {
		t.Fatalf("读不到 IR 产物 %s: %v", irPath, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("IR 产物不是合法 JSON: %v\n--- 前 400 ---\n%s", err, firstNIR(raw, 400))
	}
	flows, _ := doc["workflows"].([]any)
	if len(flows) == 0 {
		t.Fatalf("IR 里没有 workflows[]")
	}
	wf, _ := flows[0].(map[string]any)
	params, _ := wf["params"].([]any)
	return params
}

// TestWorkflowNamedParamRejected 是 E3026 的主用例：两种"看起来像形参"的写法
// 都必须**编不过**且报**自己的码**。
//
// 为什么按 code 断言而不是文案：文案会改，码是 `hermes explain` 与 grep 的锚点。
// 为什么还要钉住"不是那三样"：这条走的是 parser，如果哪天又退回 codegen / 装配层
// 去报，错误码会变成 E_IR_CODEGEN 或 E_IR_VAR_MISMATCH，症状是"编译器 bug"——
// 那正是 D-035 要消灭的东西。只断言"有错"会让它悄悄退回去也绿。
func TestWorkflowNamedParamRejected(t *testing.T) {
	cases := []struct{ name, src string }{
		{"typed", typedParamSrc},
		{"default_value", defaultParamSrc},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, stderr, _ := compileInlineSource(t, c.src)
			if code == 0 {
				t.Fatalf("具名形参应编不过（E3026），实际退出码 0")
			}
			if !strings.Contains(stderr, "E3026") {
				t.Fatalf("应报 E3026，实际 stderr:\n%s", stderr)
			}
			for _, wrong := range []string{"E_IR_CODEGEN", "E_IR_VAR_MISMATCH", "compiler bug"} {
				if strings.Contains(stderr, wrong) {
					t.Fatalf("不该出现 %q —— 出现它说明判定退回到了下游（症状是"+
						"「编译器 bug」）。实际 stderr:\n%s", wrong, stderr)
				}
			}
		})
	}
}

// TestExternSignatureParamsNotRejected 防误伤 ①：无体 `.h` 的 extern 签名必须继续
// 接受 `url: string`。
//
// 它和 `n: int` 长得很像，判据却完全相反：extern 是 C 调用 ABI，参数是**位置**
// 参数，绑定者是 `call_native` 的调用点。豁免**不是**靠 `is_header` 写的 ——
// `parse_workflow` 里那段白名单没有 `is_extern` 判断，也**没有** `is_header` 判断，
// 它无条件跑；extern 之所以不被拒，是因为 `extern workflow g(...)` 走
// `parse_extern()`，根本到不了 `parse_workflow()`。这个等价由调用图保证。
//
// 别把它改成 `if (!is_header)`：那会把 `.h` 里的非 extern 签名一起放过，
// 而那种签名是一条能让声明与定义互不匹配的后门（见 TestHeaderNonExtern...
// 那一对）。
func TestExternSignatureParamsNotRejected(t *testing.T) {
	res, err := native.Parse([]byte(externHeaderSrc), "net.h", true)
	if err != nil {
		t.Fatalf("native.Parse: %v", err)
	}
	if strings.Contains(res.Diag, "E3026") {
		t.Fatalf("`.h` 的 extern 签名不该报 E3026，实际诊断:\n%s", res.Diag)
	}
}

// TestHeaderNonExternSignatureRejected 是 E3026 的**第三**条用例，也是唯一
// "曾经 exit 0"的那条。
//
// 场景是两个文件，不是单文件：
//
//	x.h          export workflow f(n: int) entry -> string;   ← 非 extern 的签名
//	main.hermes  #include "x.h"
//	             export workflow f(input: json) entry { return "x"; }
//
// 为什么必须两文件：单写一个 `.h` 只能证明"有没有报 E3026"，证明不了危害。
// 两文件才暴露真正的下场 —— 声明侧 `n: int` 被放过、定义侧 `input: json` 合法，
// 两边形参**完全对不上**，却没有任何码报出来（`E3014` 尚未实现），`hermes ir`
// 退出码 0、产物合法。这不是"漏一条诊断"，是一条可以任意声明形参、且定义不必
// 匹配的后门，比"编得过但跑不起来"更隐蔽：它连运行期那句话都不会喊。
//
// 断言要钉 **loc 落在 x.h**：错在声明侧。把人指到 `.hermes` 的定义行，等于让他
// 去改一个本来就没错的地方 —— 这类"指错位置"比不报更费时间。
func TestHeaderNonExternSignatureRejected(t *testing.T) {
	code, stderr, _ := compileFilesFrom(t, map[string]string{
		"x.h":         headerDeclSrc,
		"main.hermes": bodyDefSrc,
	}, "main.hermes")
	if code == 0 {
		t.Fatalf("`.h` 里的非 extern 签名 `f(n: int)` 应编不过（E3026），实际退出码 0 —— "+
			"这正是那条后门：声明与定义形参不符却能编过。\n--- stderr ---\n%s", stderr)
	}
	diags, ok := parseDiagArray(stderr)
	if !ok {
		// C++ 主程序把诊断**渲染成人读文本**（error[E3026]: … --> x.h  line 1），
		// 不再输出 JSON 数组。判据（码 / 文件 / 行）仍然要成立，只是从文本里读。
		if strings.Count(stderr, "E3026") != 1 {
			t.Fatalf("E3026 应恰报 1 条（错只在声明侧），实际 stderr:\n%s", stderr)
		}
		if !strings.Contains(stderr, "x.h") || !strings.Contains(stderr, "line 1") {
			t.Fatalf("E3026 的 loc 应指在声明侧 x.h 第 1 行，实际 stderr:\n%s", stderr)
		}
		return
	}
	var hits []diagEntry
	for _, d := range diags {
		if d.Code == "E3026" {
			hits = append(hits, d)
		}
	}
	if len(hits) == 0 {
		t.Fatalf("应报 E3026，实际诊断:\n%s", stderr)
	}
	if len(hits) != 1 {
		t.Fatalf("E3026 应恰报 1 条（错只在声明侧），实际 %d 条: %+v", len(hits), hits)
	}
	got := hits[0]
	if base := filepath.Base(got.Module); base != "x.h" {
		t.Fatalf("E3026 的 loc 应指在声明侧 x.h，实际 module=%q（第 %d 行）—— "+
			"指到定义侧等于让人去改一个没错的地方。完整诊断:\n%s", got.Module, got.Line, stderr)
	}
	if got.Line != 1 {
		t.Fatalf("E3026 应指在 x.h 第 1 行（`n` 所在行），实际第 %d 行 col %d", got.Line, got.Col)
	}
}

// TestLegalWorkflowParamsStillCompile 防误伤 ②：`input: T` + `backend = <裸名>`
// 必须照常编得过，且 `params[]` 里只有 `backend` 一项、带已折叠的 v。
//
// 这一条同时守住"input 不进 params[]"：params[] 装的是**其余**参数的默认值，
// input 的类型是请求契约（§4.8），两者不是一回事。
func TestLegalWorkflowParamsStillCompile(t *testing.T) {
	code, stderr, outPath := compileInlineSource(t, legalParamSrc)
	if code != 0 {
		t.Fatalf("`f(input: S, backend = gw)` 应编得过，实际退出码 %d\n--- stderr ---\n%s", code, stderr)
	}
	params := paramsOf(t, outPath)
	if len(params) != 1 {
		t.Fatalf("params[] 应恰有 1 条（backend），实际 %d 条: %v", len(params), params)
	}
	p0, _ := params[0].(map[string]any)
	if got, _ := p0["n"].(string); got != "backend" {
		t.Fatalf("params[0].n 应为 \"backend\"（input 不进 params[]），实际 %q（整条: %v）", got, p0)
	}
	v, has := p0["v"].(map[string]any)
	if !has {
		t.Fatalf("backend 默认值必须折叠成 v（$defs/param 要求它），实际整条: %v", p0)
	}
	if got, _ := v["k"].(string); got != "backend" {
		t.Fatalf("params[0].v.k 应为 \"backend\"，实际 %v（整条: %v）", v["k"], p0)
	}
	if got, _ := v["v"].(string); got != "gw" {
		t.Fatalf("params[0].v.v 应为 \"gw\"，实际 %v（整条: %v）", v["v"], p0)
	}
}
