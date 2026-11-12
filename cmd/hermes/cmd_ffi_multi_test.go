package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hermes/internal/ir"
	"hermes/internal/native"
)

// 多文件 FFI（EXTERN-C-ABI.md §1.3 正例 1）的端到端回归。
//
// 单文件 FFI 的 7 个用例在 cmd_ffi_e2e_test.go。这里补的是**跨文件**那一刀：
// extern 声明住在 .h，入口 .hermes 只有 #include + #link native + 调用。
// 修复前 hm_codegen_ext 只拿到入口字节，codegen 的 extern_fns_ 只看入口
// AST 的顶层，于是 `imul(...)` / `net_get(...)` 被判成 "unknown call"
// （E_IR_CODEGEN）。修复走 ext_types 文本通道做编译期静态符号注入。
//
// fixture 在 testdata/ffi_multi/<case>/ 下，目录即 include 根（include.Resolve
// 默认以入口文件所在目录为根），所以不必复制到临时目录；只有 -o 产物写到
// t.TempDir()，测试不会在仓库里留下字节。

// compileFFIFixture 把一份多文件 fixture 编成 IR，返回
// (退出码, stderr 全文, IR 路径)。替换 os.Stderr 是为了逐字断言诊断码：
// 只看退出码会把「语法写错」和「正确拒绝」混成一件事。
func compileFFIFixture(t *testing.T, fixture string, outName string) (int, string, string) {
	t.Helper()
	dir := t.TempDir()
	srcPath := filepath.Join("testdata", "ffi_multi", fixture, "main.hermes")
	if _, err := os.Stat(srcPath); err != nil {
		t.Fatalf("fixture %s 不存在: %v", srcPath, err)
	}
	outPath := filepath.Join(dir, outName)
	stderrPath := filepath.Join(dir, "stderr.txt")
	ef, err := os.Create(stderrPath)
	if err != nil {
		t.Fatal(err)
	}
	code := func() int {
		old := os.Stderr
		os.Stderr = ef
		defer func() { os.Stderr = old }()
		return run([]string{"ir", srcPath, "-o", outPath})
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

// findCallNative 返回该 workflow 里唯一的 call_native 节点。
func findCallNative(t *testing.T, wf *ir.Workflow) *ir.Node {
	t.Helper()
	var found []*ir.Node
	for i := range wf.Nodes {
		if wf.Nodes[i].K == "call_native" {
			found = append(found, &wf.Nodes[i])
		}
	}
	if len(found) != 1 {
		t.Fatalf("应恰好一个 call_native 节点，实际 %d", len(found))
	}
	return found[0]
}

// typeIDByName 从产物的 types[] 按名字查 id。类型 id 是 1 起、按声明顺序分配
// （IR.md §3.1），不要在测试里硬编码成 1 —— 加一个 struct 就该让用例假红。
func typeIDByName(doc *ir.Doc, name string) (int, bool) {
	for _, td := range doc.Types {
		if td.Name == name {
			return td.ID, true
		}
	}
	return 0, false
}

// assertNativeContract 验反向融合的四件必备事实：
// requires[].kind=native、imports[].kind=native、bounds.unbounded、
// 以及 extern 符号不进 functions[]。
func assertNativeContract(t *testing.T, doc *ir.Doc, sym string) *ir.Workflow {
	t.Helper()
	var natives []ir.RequireItem
	for _, r := range doc.Requires {
		if r.Kind == "native" {
			natives = append(natives, r)
		}
	}
	if len(natives) != 1 {
		t.Fatalf("应恰好一条 native require，实际 %d：%+v", len(natives), doc.Requires)
	}
	if natives[0].Name == "" || natives[0].Archive == "" || natives[0].Header == "" {
		t.Errorf("native require 的 name/archive/header 不该为空：%+v", natives[0])
	}
	if natives[0].IRVersion != ir.IRVersionSupported {
		t.Errorf("native require.ir_version 应为 %d，实际 %d", ir.IRVersionSupported, natives[0].IRVersion)
	}

	var nativeImports []ir.ImportRef
	for _, im := range doc.Imports {
		if im.Kind == "native" {
			nativeImports = append(nativeImports, im)
		}
	}
	if len(nativeImports) != 1 || nativeImports[0].Sym != sym {
		t.Fatalf("应恰好一条 native import 指向 %s，实际 %+v", sym, doc.Imports)
	}

	if len(doc.Workflows) != 1 {
		t.Fatalf("应有 1 个 workflow，实际 %d", len(doc.Workflows))
	}
	wf := &doc.Workflows[0]
	if !wf.Bounds.Unbounded {
		t.Error("含 call_native 的 workflow 应标 bounds.unbounded:true")
	}
	for _, f := range doc.Functions {
		if f.Name == sym {
			t.Errorf("extern 符号 %s 不应出现在 functions[] 里", sym)
		}
	}
	return wf
}

// TestFFIMultiFileFormB 形态 B（C++ 风格 `extern int imul(int a, int b);`）。
// 这是机制的最小正例：先证明跨文件符号表真的接通了。
func TestFFIMultiFileFormB(t *testing.T) {
	code, msg, outPath := compileFFIFixture(t, "form_b", "form_b.ir.json")
	if code != 0 {
		t.Fatalf("多文件 extern 正例应编译通过，实际退出 %d：\n%s", code, msg)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	doc, ds := ir.Load(data)
	if len(ds) != 0 {
		t.Fatalf("产物 IR 应通过加载期校验，实际 %d 条诊断：%v", len(ds), ds)
	}

	wf := assertNativeContract(t, doc, "imul")
	c := findCallNative(t, wf)
	if c.Fn == nil || *c.Fn != "imul" {
		t.Errorf("call_native.fn 应为 imul，实际 %v", c.Fn)
	}
	if c.Ret == nil || c.Ret.K != "int" {
		t.Errorf("call_native.ret 应为 {k:int}，实际 %+v", c.Ret)
	}
	var args []int
	if err := json.Unmarshal(c.Args, &args); err != nil {
		t.Fatalf("call_native.args 应是节点 id 数组：%v", err)
	}
	if len(args) != 2 {
		t.Errorf("call_native.args 应有 2 个实参节点，实际 %v", args)
	}
	for _, a := range args {
		if a < 0 || a >= len(wf.Nodes) {
			t.Errorf("call_native.args 指向越界节点 %d", a)
		}
	}
}

// TestFFIMultiFileFormA 形态 A（`export extern workflow net_get(url: string) -> NetResp;`），
// 即 docs/EXTERN-C-ABI.md §1.3 正例 1 的形状：.h 声明 + 入口 include/link/调用。
//
// 注意：fixture 里 struct 字段写成 `int status; string body;`（parse_field 的
// 真实文法），而 §1.3 文档示例写的是 `status: int; body: string;` —— 文档与
// 解析器不一致，按解析器来（见报告「已知限制」）。
func TestFFIMultiFileFormA(t *testing.T) {
	code, msg, outPath := compileFFIFixture(t, "form_a", "form_a.ir.json")
	if code != 0 {
		t.Fatalf("§1.3 正例 1 应编译通过，实际退出 %d：\n%s", code, msg)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	doc, ds := ir.Load(data)
	if len(ds) != 0 {
		t.Fatalf("产物 IR 应通过加载期校验，实际 %d 条诊断：%v", len(ds), ds)
	}

	wf := assertNativeContract(t, doc, "net_get")
	c := findCallNative(t, wf)
	if c.Fn == nil || *c.Fn != "net_get" {
		t.Errorf("call_native.fn 应为 net_get，实际 %v", c.Fn)
	}
	// @opaque struct 返回：codegen 发 `{"k":"struct","ref":"NetResp"}`，
	// 装配层把它解析成 types[] 的 id（与 call_tool.ret 同形）。
	// **不硬编码 id**：按 name 从产物 types[] 查，类型表顺序变动不该让用例假红。
	netRespID, ok := typeIDByName(doc, "NetResp")
	if !ok {
		t.Fatalf("types[] 里应有 NetResp，实际 %+v", doc.Types)
	}
	if c.Ret == nil || c.Ret.K != "struct" {
		t.Fatalf(`call_native.ret 应为 {"k":"struct","id":N}，实际 %+v`, c.Ret)
	}
	if c.Ret.Ref != "" {
		t.Errorf("call_native.ret 不应再保留 ref 名字形态，实际 %+v", c.Ret)
	}
	if c.Ret.ID == nil || *c.Ret.ID != netRespID {
		t.Errorf("call_native.ret.id 应为 NetResp 的 id %d，实际 %+v", netRespID, c.Ret)
	}
	var args []int
	if err := json.Unmarshal(c.Args, &args); err != nil {
		t.Fatalf("call_native.args 应是节点 id 数组：%v", err)
	}
	if len(args) != 1 {
		t.Errorf("net_get 应有 1 个实参节点，实际 %v", args)
	}
}

// TestFFIMultiFileCallNativeRetIsStructID 是本轮的**聚焦回归**：
// 直接在**原始 IR 字节**上钉死 call_native.ret 的形状，而不是只信
// ir.Doc 的结构体解码（后者会把不认识的键静默丢掉，看不出 ref 残留）。
//
// 修之前 codegen 发 `{"k":"struct","ref":"NetResp"}`，装配层没解析，产物里
// 留着 ref；schemas/ir.schema.json 的 typeStruct 只认 id 且
// additionalProperties:false。修之后必须是恰好 `{k,id}` 两个键、id 指向
// types[] 里的 NetResp，且 ir.Load 0 诊断 —— 这也证明
// internal/ir/refs.go::appendTypeRefs 现在能收到一条 TableType 引用并正确
// 解析（id 无效会报 dangling ref，而不是安静放过）。
func TestFFIMultiFileCallNativeRetIsStructID(t *testing.T) {
	code, msg, outPath := compileFFIFixture(t, "form_a", "ret_shape.ir.json")
	if code != 0 {
		t.Fatalf("Form A 应编译通过，实际退出 %d：\n%s", code, msg)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	doc, ds := ir.Load(data)
	if len(ds) != 0 {
		t.Fatalf("产物 IR 应通过加载期校验（ref→id 后的 TableType 引用必须可解析），实际 %d 条诊断：%v", len(ds), ds)
	}

	wantID, ok := typeIDByName(doc, "NetResp")
	if !ok {
		t.Fatalf("types[] 里应有 NetResp，实际 %+v", doc.Types)
	}

	// 从原始 JSON 里挖出 call_native.ret，逐键比对。
	var raw struct {
		Workflows []struct {
			Nodes []map[string]json.RawMessage `json:"nodes"`
		} `json:"workflows"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("产物不是合法 JSON：%v", err)
	}
	var retRaw json.RawMessage
	count := 0
	for _, wf := range raw.Workflows {
		for _, n := range wf.Nodes {
			var k string
			if err := json.Unmarshal(n["k"], &k); err != nil || k != "call_native" {
				continue
			}
			count++
			retRaw = n["ret"]
		}
	}
	if count != 1 {
		t.Fatalf("应恰好一个 call_native 节点，实际 %d", count)
	}
	var ret map[string]json.RawMessage
	if err := json.Unmarshal(retRaw, &ret); err != nil {
		t.Fatalf("call_native.ret 不是对象：%s", retRaw)
	}
	// 恰好 k + id 两个键：多出 ref / 少 id 都是本轮要挡的形状。
	if len(ret) != 2 {
		t.Errorf("call_native.ret 应恰好 {k,id} 两个键，实际 %d 个：%s", len(ret), retRaw)
	}
	if _, hasRef := ret["ref"]; hasRef {
		t.Errorf("call_native.ret 不应残留 ref 键：%s", retRaw)
	}
	var k string
	if err := json.Unmarshal(ret["k"], &k); err != nil || k != "struct" {
		t.Errorf(`call_native.ret.k 应为 "struct"，实际 %q（%s）`, k, retRaw)
	}
	var gotID int
	if err := json.Unmarshal(ret["id"], &gotID); err != nil {
		t.Fatalf("call_native.ret.id 应是整数：%s（%v）", retRaw, err)
	}
	if gotID != wantID {
		t.Errorf("call_native.ret.id 应指向 NetResp 的 id %d，实际 %d：%s", wantID, gotID, retRaw)
	}
	t.Logf("call_native.ret = %s (NetResp id=%d, ir.Load diagnostics=%d)", retRaw, wantID, len(ds))
}

// TestFFIMultiFileDiagnostics 证明「原模块单独检查」没有被跨文件注入绕过：
//   - .h 里的 extern 返回 json → 必须报 E3030（证明声明级检查仍在 .h 自己那一趟跑）；
//   - 入口缺 #link native → 必须报 E3031（证明注入符号仍走 E3031 调用点检查）。
func TestFFIMultiFileDiagnostics(t *testing.T) {
	cases := []struct {
		name    string
		fixture string
		want    string
	}{
		{"E3030 .h 里的 extern 返回 json", "bad_ret", "E3030"},
		{"E3031 入口缺 #link native", "no_link", "E3031"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, msg, _ := compileFFIFixture(t, tc.fixture, "out.ir.json")
			if code != 4 {
				t.Fatalf("应被诊断拦下（退出 4），实际 %d：\n%s", code, msg)
			}
			if !strings.Contains(msg, tc.want) {
				t.Errorf("诊断应含 %s，实际：\n%s", tc.want, msg)
			}
		})
	}
}

// TestFFIMultiFileByteStable 守 ext 行的确定性：注入顺序按 sym 字典序固定，
// 同一份源码两次编译必须逐字节相同（golden 比对与 unit sha256 都依赖这点）。
func TestFFIMultiFileByteStable(t *testing.T) {
	c1, m1, p1 := compileFFIFixture(t, "form_b", "first.ir.json")
	if c1 != 0 {
		t.Fatalf("第一次编译失败（%d）：\n%s", c1, m1)
	}
	c2, m2, p2 := compileFFIFixture(t, "form_b", "second.ir.json")
	if c2 != 0 {
		t.Fatalf("第二次编译失败（%d）：\n%s", c2, m2)
	}
	b1, err := os.ReadFile(p1)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := os.ReadFile(p2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b1, b2) {
		t.Fatalf("同一份多文件源码两次编译产物不一致（%d vs %d 字节）", len(b1), len(b2))
	}
}

// TestFFIMultiFileExternDeclLine 直接钉死 ext_types 文本通道的**格式契约**：
// codegen_wfdecls 追加的 extern 项不带 name（避免被 exportedDecls 误收），
// 且 ext_line 是 `extern\t<sym>\t<w|f>\t<ret>\t<t1>,<t2>`（真 tab）。
func TestFFIMultiFileExternDeclLine(t *testing.T) {
	check := func(name, src, wantLine string) {
		t.Helper()
		js, diag, err := native.CodegenWorkflowDecls([]byte(src), name, true)
		if err != nil {
			t.Fatalf("%s: CodegenWorkflowDecls 失败: %v", name, err)
		}
		if diag != "" && diag != "[]" {
			t.Fatalf("%s: 不该有诊断: %s", name, diag)
		}
		var items []map[string]any
		if err := json.Unmarshal([]byte(js), &items); err != nil {
			t.Fatalf("%s: 不是合法 JSON: %v\n%s", name, err, js)
		}
		for _, it := range items {
			if !truthy(it["extern"]) {
				continue
			}
			if _, hasName := it["name"]; hasName {
				t.Errorf("%s: extern 项不该带 name 字段：%+v", name, it)
			}
			line, _ := it["ext_line"].(string)
			if line != wantLine {
				t.Errorf("%s: ext_line 不符\n want %q\n got  %q", name, wantLine, line)
			}
			return
		}
		t.Errorf("%s: 没有产出 extern 项：%s", name, js)
	}
	check("types.h", "extern int imul(int a, int b);\n", "extern\timul\tf\tint\tint,int")
	check("net.h",
		"export struct NetResp { int status; string body; }\n"+
			"export extern workflow net_get(url: string) -> NetResp;\n",
		"extern\tnet_get\tw\tNetResp\tstring")
}
