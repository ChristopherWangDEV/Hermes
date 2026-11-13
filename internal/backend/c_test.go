package backend

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"hermes/internal/engine"
	"hermes/internal/ir"
	"hermes/internal/netcall"
	"hermes/internal/runtime"
)

func iptr(v int) *int       { return &v }
func sptr(v string) *string { return &v }

// helloDoc 构造与 testdata/hello.hermes 等价的 IR：seq(decl msg="hi"; return msg)。
func helloDoc() *ir.Doc {
	litV := json.RawMessage(`"hi"`)
	nodes := []ir.Node{
		{ID: 0, K: "seq", Kids: []int{1, 3}},
		{ID: 1, K: "decl", Var: iptr(0), E: iptr(2)},
		{ID: 2, K: "lit", LitT: sptr("string"), LitV: litV},
		{ID: 3, K: "return", E: iptr(4)},
		{ID: 4, K: "var", Var: iptr(0)},
	}
	return &ir.Doc{
		Workflows: []ir.Workflow{{
			Name: "hello", IsEntry: true, Root: 0,
			Nodes: nodes,
			Vars:  []ir.Var{{ID: 0, Name: "msg"}},
		}},
	}
}

// TestGenerateHelloShape 断言生成的 C 结构：有 steps 信封、无 IR/运行时引用。
func TestGenerateHelloShape(t *testing.T) {
	src, err := (cBackend{}).Generate(helloDoc())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	s := string(src)
	for _, want := range []string{
		`"{\"steps\":%ld,\"value\":"`, // 与 hermes run 的 stdout 信封一致
		`goto hm_done`,
		`hm_steps++`,
		`hm_str("hi")`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("generated C missing %q", want)
		}
	}
	// 零解释器的硬证据：产物源码里**不许**出现 IR 或 Go 运行时入口。
	for _, bad := range []string{"hermes_rt_run", "hermes_ir_bytes", "libhermesrt"} {
		if strings.Contains(s, bad) {
			t.Errorf("generated C references %q; the C backend must not embed IR or link the Go runtime", bad)
		}
	}
}

// TestGenerateHelloRuns 端到端：编译并用 clang 跑，stdout 必须与 `hermes run` 逐字一致。
// 没有 clang 就跳过（红线：只用 clang，不内置编译器）。
func TestGenerateHelloRuns(t *testing.T) {
	cc, err := exec.LookPath("clang")
	if err != nil {
		t.Skip("clang not found; skipping end-to-end C-backend test")
	}
	src, err := (cBackend{}).Generate(helloDoc())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	dir := t.TempDir()
	cfile := filepath.Join(dir, "main.c")
	if err := os.WriteFile(cfile, src, 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "prog")
	if out, err := exec.Command(cc, "-std=c99", "-O2", cfile, "-o", bin).CombinedOutput(); err != nil {
		t.Fatalf("clang failed: %v\n%s", err, out)
	}
	got, err := exec.Command(bin).Output()
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	want := `{"steps":3,"value":"hi","workflow":"hello"}` + "\n"
	if string(got) != want {
		t.Fatalf("stdout mismatch:\n got %q\nwant %q", got, want)
	}
}

// ---- AOT 标准错误对象 HermesError 的守卫集合 ----------------------------
//
// 这些守卫服务于「AOT 产物里有一个固定布局、无指针成员的标准错误对象」
// 这一契约：形态 / 编译期布局断言 / 双 target 编译探针 / 三处清错点 /
// catch 臂 status 快照 / 真实 404·503 端到端 / Go↔C 文本格式一致性。

// clangPath 返回 clang 路径；不存在就按红线（只用 clang）跳过编译类守卫。
func clangPath(t *testing.T) string {
	t.Helper()
	cc, err := exec.LookPath("clang")
	if err != nil {
		t.Skip("clang not found; skipping C-compilation guard")
	}
	return cc
}

// backendRepoRoot 从 internal/backend 往上两级到仓库根。
func backendRepoRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	return abs
}

func writeTempC(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// caughtErrorFieldDoc 构造 try { a = 1 } catch (HttpError e) { return e.<field>; }
// 的 IR；field 由参数决定，便于分别验证 status 被放行、其它字段被拒绝。
func caughtErrorFieldDoc(field string) *ir.Doc {
	nodes := []ir.Node{
		{ID: 0, K: "seq", Kids: []int{1, 6}},
		{ID: 1, K: "try", Body: iptr(2), Catches: []ir.CatchArm{{T: "HttpError", V: 1, Body: 3}}},
		{ID: 2, K: "seq", Kids: []int{}},
		{ID: 3, K: "seq", Kids: []int{4}},
		{ID: 4, K: "return", E: iptr(5)},
		{ID: 5, K: "field", Name: sptr(field), Of: iptr(7)},
		{ID: 6, K: "return", E: iptr(8)},
		{ID: 7, K: "var", Var: iptr(1)},
		{ID: 8, K: "lit", LitT: sptr("string"), LitV: json.RawMessage(`"ok"`)},
	}
	return &ir.Doc{Workflows: []ir.Workflow{{
		Name: "w", IsEntry: true, Root: 0,
		Nodes: nodes,
		Vars:  []ir.Var{{ID: 0, Name: "a"}, {ID: 1, Name: "e"}},
	}}}
}

// fallbackDoc 构造 x = fallback { 1/0 } { return 7 }; return x;
// 除零会置起错误槽，从而走 fallback 的「清错 + 降级块」路径。
func fallbackDoc() *ir.Doc {
	nodes := []ir.Node{
		{ID: 0, K: "seq", Kids: []int{1, 9}},
		{ID: 1, K: "decl", Var: iptr(0), E: iptr(2)},
		{ID: 2, K: "fallback", Main: iptr(3), Body: iptr(6)},
		{ID: 3, K: "binop", Op: sptr("div"), L: iptr(4), R: iptr(5)},
		{ID: 4, K: "lit", LitT: sptr("int"), LitV: json.RawMessage(`1`)},
		{ID: 5, K: "lit", LitT: sptr("int"), LitV: json.RawMessage(`0`)},
		{ID: 6, K: "seq", Kids: []int{7}},
		{ID: 7, K: "return", E: iptr(8)},
		{ID: 8, K: "lit", LitT: sptr("int"), LitV: json.RawMessage(`7`)},
		{ID: 9, K: "return", E: iptr(10)},
		{ID: 10, K: "var", Var: iptr(0)},
	}
	return &ir.Doc{Workflows: []ir.Workflow{{
		Name: "w", IsEntry: true, Root: 0,
		Nodes: nodes,
		Vars:  []ir.Var{{ID: 0, Name: "x"}},
	}}}
}

// retryDoc 构造 retry(3) { return 7 }; return 0; —— 覆盖第三处清错点。
func retryDoc() *ir.Doc {
	nodes := []ir.Node{
		{ID: 0, K: "seq", Kids: []int{1, 5}},
		{ID: 1, K: "retry", N: iptr(2), Body: iptr(2)},
		{ID: 2, K: "seq", Kids: []int{3}},
		{ID: 3, K: "return", E: iptr(4)},
		{ID: 4, K: "lit", LitT: sptr("int"), LitV: json.RawMessage(`7`)},
		{ID: 5, K: "return", E: iptr(6)},
		{ID: 6, K: "lit", LitT: sptr("int"), LitV: json.RawMessage(`0`)},
	}
	return &ir.Doc{Workflows: []ir.Workflow{{
		Name: "w", IsEntry: true, Root: 0,
		Nodes: nodes,
		Vars:  []ir.Var{},
	}}}
}

// caughtStatusToolDoc 构造：
//
//	try { auto x = tool.call(url="http://stub"); }
//	catch (HttpError e) { if (e.status == 404) { return "not_found"; } return "other"; }
func caughtStatusToolDoc() *ir.Doc {
	nodes := []ir.Node{
		{ID: 0, K: "seq", Kids: []int{1}},
		{ID: 1, K: "try", Body: iptr(2), Catches: []ir.CatchArm{{T: "HttpError", V: 1, Body: 4}}},
		{ID: 2, K: "seq", Kids: []int{3}},
		{ID: 3, K: "decl", Var: iptr(0), E: iptr(30)},
		{ID: 4, K: "seq", Kids: []int{5, 9}},
		{ID: 5, K: "if", C: iptr(6), Then: iptr(7)},
		{ID: 6, K: "binop", Op: sptr("eq"), L: iptr(10), R: iptr(12)},
		{ID: 7, K: "seq", Kids: []int{8}},
		{ID: 8, K: "return", E: iptr(13)},
		{ID: 9, K: "return", E: iptr(14)},
		{ID: 10, K: "field", Name: sptr("status"), Of: iptr(11)},
		{ID: 11, K: "var", Var: iptr(1)},
		{ID: 12, K: "lit", LitT: sptr("int"), LitV: json.RawMessage(`404`)},
		{ID: 13, K: "lit", LitT: sptr("string"), LitV: json.RawMessage(`"not_found"`)},
		{ID: 14, K: "lit", LitT: sptr("string"), LitV: json.RawMessage(`"other"`)},
		{ID: 30, K: "call_tool", Args: json.RawMessage(`{"url":31,"method":"GET"}`), Tool: sptr("stub")},
		{ID: 31, K: "lit", LitT: sptr("string"), LitV: json.RawMessage(`"http://stub"`)},
	}
	return &ir.Doc{Workflows: []ir.Workflow{{
		Name: "w", IsEntry: true, Root: 0,
		Nodes: nodes,
		Vars:  []ir.Var{{ID: 0, Name: "x"}, {ID: 1, Name: "e"}},
	}}}
}

// TestGenerateErrorObjectShape 断言生成的 C 含标准错误对象的完整形态，且
// 不含指针成员 msg / 旧的独立错误槽 / 已被规格移除的 hm_error_get。
func TestGenerateErrorObjectShape(t *testing.T) {
	src, err := (cBackend{}).Generate(helloDoc())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	s := string(src)
	for _, want := range []string{
		"typedef struct HermesError",
		"int32_t code;",
		"int32_t status;",
		"char     msg[HM_ERR_MSG_CAP];",
		"static HermesError hm_error = {0, 0, {0}};",
		"hm_clear_error",
		"hm_parse_status",
		"hm_set_error",
		"hm_error_snapshot",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("generated C missing %q", want)
		}
	}
	for _, bad := range []string{"const char* msg", "static int hm_err", "hm_error_get"} {
		if strings.Contains(s, bad) {
			t.Errorf("generated C must not contain %q", bad)
		}
	}
	// \bhm_err\b 精确匹配独立记号：hm_error / hm_errsnap_<id> 都以 hm_err
	// 开头但后面紧跟字母，不构成词尾边界，不会被误判为残留。
	if regexp.MustCompile(`\bhm_err\b`).MatchString(s) {
		t.Errorf("generated C still contains the legacy standalone `hm_err` token")
	}
}

// TestGenerateErrorObjectLayoutAsserts 断言 4 条 typedef char 断言齐全、分别
// 钉死 264 / 0 / 4 / 8，并让完整生成的 main.c 真过 clang -std=c99 -O2 -Wall。
func TestGenerateErrorObjectLayoutAsserts(t *testing.T) {
	cc := clangPath(t)
	dir := t.TempDir()
	src, err := (cBackend{}).Generate(helloDoc())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	s := string(src)
	for _, want := range []string{
		"typedef char hm_assert_err_size[(sizeof(HermesError)==264)?1:-1];",
		"typedef char hm_assert_err_code_off[(offsetof(HermesError,code)==0)?1:-1];",
		"typedef char hm_assert_err_status_off[(offsetof(HermesError,status)==4)?1:-1];",
		"typedef char hm_assert_err_msg_off[(offsetof(HermesError,msg)==8)?1:-1];",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("generated C missing layout assert %q", want)
		}
	}
	mainC := writeTempC(t, dir, "main.c", s)
	if out, err := exec.Command(cc, "-std=c99", "-O2", "-Wall", "-c", mainC,
		"-o", filepath.Join(dir, "main.o")).CombinedOutput(); err != nil {
		t.Fatalf("clang -std=c99 -O2 -Wall -c 失败: %v\n%s", err, out)
	}
}

// TestErrorObjectLayoutCrossTarget 用默认 target + 一个 32 位 target 各做一次
// -fsyntax-only，验证编译期布局断言在 ILP32 数据模型下同样成立。
//
// 探针直接取自**真实生成产物**里的 HermesError 定义 + 4 条断言，不是手抄副本
// —— 手抄副本永远不会随产物漂移，是假绿。若 i386 target 不被本机 clang 支持
// 则 Skip 并明确注明「32 位 target 未实测」。
//
// 注意：这是**编译期断言**（布局在 ILP32 下也成立），不是 32 位 target 真
// 编译 / 链接 / 运行验证；本仓从未在 32 位 target 实机验证过（docs/C-ABI.md）。
func TestErrorObjectLayoutCrossTarget(t *testing.T) {
	cc := clangPath(t)
	dir := t.TempDir()
	src, err := (cBackend{}).Generate(helloDoc())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	s := string(src)
	start := strings.Index(s, "#define HM_ERR_MSG_CAP")
	endMark := "typedef char hm_assert_err_msg_off"
	mi := strings.Index(s, endMark)
	if start < 0 || mi < 0 {
		t.Fatal("生成产物里找不到 HermesError 定义或第 4 条布局断言")
	}
	lineEnd := strings.IndexByte(s[mi:], '\n')
	if lineEnd < 0 {
		t.Fatal("第 4 条布局断言没有换行结尾")
	}
	block := s[start : mi+lineEnd+1]
	if n := strings.Count(block, "typedef char hm_assert_err_"); n != 4 {
		t.Fatalf("抽取的布局块应含 4 条断言，实际 %d:\n%s", n, block)
	}
	probe := "#include <stdint.h>\n#include <stddef.h>\n" + block +
		"int hm_layout_probe(void){ return (int)sizeof(HermesError); }\n"
	p := writeTempC(t, dir, "layout_probe.c", probe)

	i386Args := func(extra ...string) []string {
		return append([]string{"-target", "i386-unknown-linux-gnu", "-std=c99", "-ffreestanding", "-fsyntax-only"}, extra...)
	}
	// 先用平凡探针确认 target 本身可用：这一步就失败说明 target 不被支持，
	// 按要求 Skip（而不是把断言失败伪装成 target 不支持）。
	trivial := writeTempC(t, dir, "trivial.c", "int hm_trivial(void){ return 0; }\n")
	if out, err := exec.Command(cc, i386Args(trivial)...).CombinedOutput(); err != nil {
		t.Skipf("32 位 target 未实测：i386-unknown-linux-gnu 不被本机 clang 支持（%v）\n%s", err, out)
	}
	if out, err := exec.Command(cc, "-std=c99", "-ffreestanding", "-fsyntax-only", p).CombinedOutput(); err != nil {
		t.Fatalf("host target 布局探针 -fsyntax-only 失败: %v\n%s", err, out)
	}
	if out, err := exec.Command(cc, i386Args(p)...).CombinedOutput(); err != nil {
		t.Fatalf("i386 target 布局探针 -fsyntax-only 失败（断言在 32 位下不成立即为真回归）: %v\n%s", err, out)
	}
}

// TestFallbackClearsWholeErrorObject 守「三处清错点统一清整个错误对象」：
//  1. 源码扫描：fallback / try 的 catch 臂 / retry 每轮都用 hm_clear_error();；
//  2. 运行期：#include 生成产物（重命名 main），先 hm_set_error 置满三个字段，
//     再 hm_clear_error()，断言 code/status/msg 真的全部归零。
//
// 为什么用直连 helper 的探针：所有错误源都走 hm_set_error，它会把三个字段
// 一起覆盖，所以「再来一个新错误」无论如何都会冲掉旧值，测不出「只清 code」
// 的残留。真正能暴露残留的窗口在 hm_clear_error() 之后、下一次 hm_set_error
// 之前。把清错变异回「只清 code」时，status/msg 会残留，下面的断言即转红。
func TestFallbackClearsWholeErrorObject(t *testing.T) {
	// 每个 doc 只含一个清错点，加上 main() 入口那一处，理应恰好 2 次
	// hm_clear_error();。用**计数**而不是"整文件 Contains"：后者会被 main()
	// 入口那一处喂饱，即使清错点被改回「只清 code」也照样绿。
	for _, tc := range []struct {
		name string
		doc  *ir.Doc
		want int
	}{
		{"fallback", fallbackDoc(), 2},
		{"try", caughtStatusToolDoc(), 2},
		{"retry", retryDoc(), 2},
	} {
		src, err := (cBackend{}).Generate(tc.doc)
		if err != nil {
			t.Fatalf("%s Generate: %v", tc.name, err)
		}
		s := string(src)
		if n := strings.Count(s, "hm_clear_error();"); n != tc.want {
			t.Errorf("%s 应有 %d 处 hm_clear_error();（main 入口 + 该清错点），实际 %d", tc.name, tc.want, n)
		}
		if regexp.MustCompile(`\bhm_err\b`).MatchString(s) {
			t.Errorf("%s 产物仍含旧的独立 hm_err 记号", tc.name)
		}
	}

	cc := clangPath(t)
	dir := t.TempDir()
	src, err := (cBackend{}).Generate(fallbackDoc())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	writeTempC(t, dir, "gen.c", string(src))
	harness := `#define main hm_generated_main
#include "gen.c"
#undef main
int main(void){
  hm_set_error(3, "HttpError: not found (status 404)");
  if (hm_error.code != 3 || hm_error.status != 404 || hm_error.msg[0] == 0) return 10;
  hm_clear_error();
  printf("%d|%d|%d", (int)hm_error.code, (int)hm_error.status, hm_error.msg[0] == 0 ? 0 : 1);
  return (hm_error.code == 0 && hm_error.status == 0 && hm_error.msg[0] == 0) ? 0 : 1;
}
`
	hC := writeTempC(t, dir, "harness.c", harness)
	hBin := filepath.Join(dir, "h")
	if out, err := exec.Command(cc, "-std=c99", hC, "-o", hBin).CombinedOutput(); err != nil {
		t.Fatalf("清错探针编译失败: %v\n%s", err, out)
	}
	got, err := exec.Command(hBin).CombinedOutput()
	if err != nil {
		t.Fatalf("清错探针运行失败（说明清错不彻底）: %v\n%s", err, got)
	}
	if strings.TrimSpace(string(got)) != "0|0|0" {
		t.Fatalf("hm_clear_error() 后 code/status/msg 必须全部归零，实际 %q", got)
	}
}

// TestCaughtErrorStatusReadsSnapshot 断言 e.status 生成的是快照读取形态
// （hm_int((long)hm_errsnap_<id>.status)），快照声明集中在块首且在清错之前；
// 其它仍未 ready 的字段继续显式拒绝，且文案点名字段 / E4034 / hermes run。
func TestCaughtErrorStatusReadsSnapshot(t *testing.T) {
	src, err := (cBackend{}).Generate(caughtErrorFieldDoc("status"))
	if err != nil {
		t.Fatalf("e.status 现在应被 AOT 后端支持，实际报错: %v", err)
	}
	s := string(src)
	if !regexp.MustCompile(`hm_int\(\(long\)hm_errsnap_\d+\.status\)`).MatchString(s) {
		t.Errorf("e.status 应生成 hm_int((long)hm_errsnap_<tryID>.status)，实际:\n%s", s)
	}
	if strings.Contains(s, `hm_field(hm_str("")`) {
		t.Errorf("e.status 不应再走 hm_field(hm_str(\"\"), ...) 静默 null 路径")
	}
	order := regexp.MustCompile(`(?s)HermesError hm_errsnap_\d+ = hm_error_snapshot\(\);\n\s*Val var_\d+ = hm_str\(""\);\n\s*\(void\)var_\d+;\n\s*hm_clear_error\(\);`)
	if !order.MatchString(s) {
		t.Errorf("catch 臂必须是「快照声明 → 绑定变量声明 → hm_clear_error();」，且快照在清错之前:\n%s", s)
	}

	_, err = (cBackend{}).Generate(caughtErrorFieldDoc("url"))
	if err == nil {
		t.Fatal("e.url 必须仍被显式拒绝（AOT 后端尚未带出该字段，前端 E4034 亦未放行）")
	}
	for _, want := range []string{"url", "E4034", "hermes run"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("e.url 的拒绝信息应含 %q，实际：%v", want, err)
		}
	}
}

// netStubC 是手写的 C 桩，替代 glibhermesnet.a（Go c-archive，构建重）：
// 实现冻结头里的三个符号，失败时返回 HttpError(3)，err 纯文本由环境变量
// HM_STUB_ERR 注入 —— 这样 Go 边界产出的 errMsg 能不经修改地进到 C 侧
// hm_parse_status，验证 status 真的穿过 ABI。
const netStubC = `#include <stdlib.h>
#include <string.h>
#include "hermes_net.h"

static char* hm_stub_dup(const char* s){
  size_t n = strlen(s) + 1;
  char* r = (char*)malloc(n);
  memcpy(r, s, n);
  return r;
}
int hermes_net_tool(const char* req, size_t req_len, char** out, char** err){
  const char* text;
  (void)req; (void)req_len;
  *out = 0;
  text = getenv("HM_STUB_ERR");
  if (!text || !*text) text = "HttpError: upstream had no status";
  *err = hm_stub_dup(text);
  return 3; /* HttpError == CatchType 3 */
}
int hermes_net_model(const char* req, size_t req_len, char** out, char** err){
  (void)req; (void)req_len;
  *out = 0;
  *err = hm_stub_dup("ModelError: stub (status 500)");
  return 4;
}
void hermes_net_free(char* p){ free(p); }
`

// TestAOTCaughtErrorStatusIsReal 是最关键的端到端守卫：真实 404 / 503。
//  1. Go 边界：httptest 返回 404/503 → netcall.Tool 的 errMsg 必须带
//     "(status 404)" / "(status 503)"（钉死用 re.Error() 而非 re.Msg）；
//  2. C 端到端：把上面产出的 errMsg 交给手写 C 桩，编译生成的 main.c + 桩
//     并运行，验证 hm_parse_status → e.status 真生效：
//     404  → "not_found"；503 / 无 status → "other"。
//
// 503 与「无 status」两组对照，能同时排除「写死 404 / 恒真」和「什么都不
// 命中」两种假实现；不依赖 glibhermesnet.a。
func TestAOTCaughtErrorStatusIsReal(t *testing.T) {
	cc := clangPath(t)
	root := backendRepoRoot(t)
	inc := filepath.Join(root, "native", "include")
	dir := t.TempDir()

	texts := map[string]string{}
	for _, code := range []int{404, 503} {
		code := code
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte("boom"))
		}))
		rt := &runtime.Runtime{HTTPClient: srv.Client()}
		_, errMsg, st := netcall.Tool(rt, netcall.ToolReq{URL: srv.URL, Method: "GET"})
		srv.Close()
		if st != netcall.HTTP {
			t.Fatalf("HTTP %d: status=%d msg=%q", code, st, errMsg)
		}
		want := fmt.Sprintf("(status %d)", code)
		if !strings.Contains(errMsg, want) {
			t.Fatalf("HTTP %d: netcall.Tool 的 errMsg 必须含 %q（re.Error() 被改回 re.Msg 会让 AOT 的 e.status 静默恒 0），实际 %q", code, want, errMsg)
		}
		texts[fmt.Sprintf("%d", code)] = errMsg
	}
	texts["none"] = "HttpError: upstream had no status"

	src, err := (cBackend{}).Generate(caughtStatusToolDoc())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	mainC := writeTempC(t, dir, "main.c", string(src))
	stubC := writeTempC(t, dir, "net_stub.c", netStubC)
	bin := filepath.Join(dir, "prog")
	if out, err := exec.Command(cc, "-std=c99", "-O2", "-I", inc, mainC, stubC,
		"-o", bin).CombinedOutput(); err != nil {
		t.Fatalf("生成产物 + C 桩链接失败: %v\n%s", err, out)
	}

	cases := []struct {
		env, errText, want, refuse string
	}{
		{"404", texts["404"], "not_found", "other"},
		{"503", texts["503"], "other", "not_found"},
		{"none", texts["none"], "other", "not_found"}, // 无 (status N) → 0 哨兵
	}
	for _, tc := range cases {
		cmd := exec.Command(bin)
		cmd.Env = append(os.Environ(), "HM_STUB_ERR="+tc.errText)
		got, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("run (%s) failed: %v\n%s", tc.env, err, got)
		}
		if !strings.Contains(string(got), tc.want) {
			t.Errorf("%s: stdout 应含 %q，实际 %q", tc.env, tc.want, got)
		}
		if strings.Contains(string(got), tc.refuse) {
			t.Errorf("%s: stdout 不应含 %q（排除写死/恒真），实际 %q", tc.env, tc.refuse, got)
		}
	}
}

// TestRunErrorStatusTextMatchesCParser 钉住「engine.RunError.Error() 格式 ↔
// 产物 hm_parse_status」的一致性：Go 侧若改了 "(status %d)" 片段的格式，
// C 侧解析会静默退化成 0 哨兵，这条会转红。文本直接取自 RunError.Error()，
// 交给生成产物里的静态 hm_parse_status 解析，逐一比对 404 / 503 / 0。
func TestRunErrorStatusTextMatchesCParser(t *testing.T) {
	cc := clangPath(t)
	dir := t.TempDir()
	src, err := (cBackend{}).Generate(helloDoc())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	writeTempC(t, dir, "gen.c", string(src))
	harness := `#define main hm_generated_main
#include "gen.c"
#undef main
int main(int argc, char** argv){
  int i;
  for(i=1;i<argc;i++) printf("%d\n", hm_parse_status(argv[i]));
  return 0;
}
`
	hC := writeTempC(t, dir, "harness.c", harness)
	hBin := filepath.Join(dir, "h")
	if out, err := exec.Command(cc, "-std=c99", hC, "-o", hBin).CombinedOutput(); err != nil {
		t.Fatalf("解析探针编译失败: %v\n%s", err, out)
	}

	cases := []struct {
		status int
		e      *engine.RunError
	}{
		{404, &engine.RunError{Kind: "HttpError", Msg: "not found", Status: 404}},
		{503, &engine.RunError{Kind: "HttpError", Msg: "unavailable", Status: 503}},
		{0, &engine.RunError{Kind: "HttpError", Msg: "no status"}},
	}
	args := make([]string, 0, len(cases))
	for _, tc := range cases {
		text := tc.e.Error()
		args = append(args, text)
		if tc.status != 0 && !strings.Contains(text, fmt.Sprintf("(status %d)", tc.status)) {
			t.Fatalf("RunError.Error() 格式漂移：Status=%d 的文本必须含 (status %d)，实际 %q", tc.status, tc.status, text)
		}
	}
	out, err := exec.Command(hBin, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("解析探针运行失败: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != len(cases) {
		t.Fatalf("解析探针应输出 %d 行，实际 %q", len(cases), out)
	}
	for i, tc := range cases {
		want := fmt.Sprintf("%d", tc.status)
		if strings.TrimSpace(lines[i]) != want {
			t.Errorf("RunError.Error() %q → hm_parse_status 应回 %s，实际 %q", tc.e.Error(), want, lines[i])
		}
	}
}

// TestNetBoundaryOutParamIsConsumed 钉住「状态码 + HermesError* 出参」边界在
// 生成产物里的形态：hm_net_invoke 只把局部错误对象写回 *out_err、绝不调用
// hm_set_error 写全局；两个 net 调用方在 st!=0 时必须用
// hm_publish_error(&out_err) 把出参发布到全局，供 try/catch/fallback/retry
// 分派。删掉消费点会让全局错误槽永不被置起 —— TestAOTCaughtErrorStatusIsReal
// 的真实 404/503 会静默退化成 value:0；这里把形态也钉成常驻守卫，防止
// 「装饰性出参 / 死参数」回归（本轮 team-lead 指出的核心问题）。
func TestNetBoundaryOutParamIsConsumed(t *testing.T) {
	src, err := (cBackend{}).Generate(caughtStatusToolDoc())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	s := string(src)
	if n := strings.Count(s, "hm_publish_error(&out_err);"); n != 2 {
		t.Errorf("两个 net 调用方都必须消费出参（hm_publish_error(&out_err);），实际 %d 处", n)
	}
	if strings.Contains(s, "(void)out_err;") {
		t.Error("出参不得被 (void)out_err; 丢弃：那会让 out param 退化成装饰")
	}
	if strings.Contains(s, "hm_set_error(st,err)") {
		t.Error("hm_net_invoke 不得再调用 hm_set_error 写全局；网络错误须经 *out_err 回传")
	}
	if !strings.Contains(s, "static void hm_publish_error(const HermesError* e)") {
		t.Error("缺少把出参按值发布到全局 hm_error 的 hm_publish_error helper")
	}
	if !strings.Contains(s, "if(out_err) *out_err=e;") {
		t.Error("hm_net_invoke 必须把局部错误对象写回 *out_err")
	}
}
