package ir_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"hermes/internal/diag"
	"hermes/internal/ir"
	"hermes/internal/native"
)

// baseIR is a minimal, valid IR used by the emit tests. It lives here because
// the package-internal copy went away with validate_test.go; external tests
// must build their own fixtures.
const baseIR = `{
  "ir_version": 2,
  "unit": {"name":"t","entry_path":"t.hermes","language_version":2,
           "modules":[{"path":"t.hermes","sha256":"0000000000000000000000000000000000000000000000000000000000000000","builtin":false}]},
  "compiler": {"name":"hermes-parse","version":"0.1","parser_abi":1},
  "types": [],
  "backends": [{"name":"b","kind":"openai","addr":{"src":"literal","value":"http://127.0.0.1:1"}}],
  "tools": [],
  "schemas": [],
  "workflows": [{
    "name":"f","exported":true,"is_entry":true,"backend":"b",
    "input":null,
    "returns":{"k":"string"},
    "params":[],
    "root":0,
    "nodes":[{"id":0,"k":"seq","loc":{"m":0,"l":1,"c":1,"el":1,"ec":10},"kids":[]}],
    "vars":[],
    "bounds":{"max_steps":1,"max_spans":0,"max_model_calls":0,
              "max_tool_calls":0,"max_concurrency":1,"max_retry_expansion":0},
    "economics":{},
    "interp_deps":[],
    "loc":{"m":0,"l":1,"c":1,"el":1,"ec":10}
  }],
  "exports":{"workflows":["f"],"types":[],"backends":["b"],"tools":[]},
  "imports":[],
  "requires":[],
  "diag_counts":{"error":0,"warning":0}
}`

func loadBase(t *testing.T) *ir.Doc {
	t.Helper()
	d, ds := ir.Load([]byte(baseIR))
	if len(ds) != 0 {
		t.Fatalf("baseIR 反序列化失败（types.go 的 json tag 可能有错）：%s", diag.RenderAll(ds))
	}
	if d == nil {
		t.Fatal("baseIR 解析出 nil Doc")
	}
	return d
}

func strptr(s string) *string { return &s }

// unmarshalForTest 用 UseNumber 解回来：默认解码会把整数变成 float64，
// 而 canon 会（正确地）拒绝浮点 —— 那样这条"幂等"测试会红在一个假问题上。
func unmarshalForTest(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(v)
}

// TestEmitIsCanonical 证明写出的是**字节序键**的 canonical JSON。
//
// 这不是审美：`ir_sha256` 就是这几个字节的哈希，键序随插入顺序变化
// 会让同一份 IR 在不同编译器/不同版本下算出不同哈希，签名链当场失效。
func TestEmitIsCanonical(t *testing.T) {
	d := loadBase(t)
	out, err := ir.Emit(d)
	if err != nil {
		t.Fatalf("Emit 失败: %v", err)
	}

	if !bytes.HasPrefix(out, []byte(`{"backends"`)) {
		t.Fatalf("键序不是字节序（第一个键应是 backends）：\n%.80s", out)
	}
	if bytes.ContainsAny(out, "\n\t") {
		t.Fatalf("canonical JSON 不该有换行或制表符：\n%s", out)
	}
	// 幂等：再解一次再写一次，字节必须相同。
	var back ir.Doc
	if err := unmarshalForTest(out, &back); err != nil {
		t.Fatalf("Emit 的产出解不回来: %v", err)
	}
	again, err := ir.Emit(&back)
	if err != nil {
		t.Fatalf("二次 Emit 失败: %v", err)
	}
	if !bytes.Equal(out, again) {
		t.Fatalf("Emit 不是幂等的：\n%s\n%s", out, again)
	}
}

// TestEmitRoundTripValidates 是"写出去的东西自己读得回来"。
//
// 自举信任从零开始（IR.md §12 结尾）：我们刚产出的 IR 也要过全部 V 检查，
// 否则"编译器产出了坏 IR"这件事只有等第三方加载时才会暴露。
//
// After Load became pure parsing, the old `len(ir.Load(out).ds) == 0` assertion
// was vacuous. The check now runs the C++ validator on the emitted bytes, which
// is the same validator every production path uses.
func TestEmitRoundTripValidates(t *testing.T) {
	out, err := ir.Emit(loadBase(t))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := native.IRValidate(out)
	if err != nil {
		t.Fatalf("C++ 校验自己写出的 IR 报错: %v", err)
	}
	ds, ok := native.ParseValidateDiags(string(raw))
	if !ok {
		t.Fatalf("C++ 诊断解不动: %s", raw)
	}
	if len(ds) != 0 {
		t.Fatalf("自己写出的 IR 自己读不合格：%v", nativeDiagCodes(ds))
	}
	d, parseDs := ir.Load(out)
	if parseDs != nil {
		t.Fatalf("Emit 的字节应能绑定回 Doc：%s", diag.RenderAll(parseDs))
	}
	if d == nil {
		t.Fatal("Load 返回 nil Doc")
	}
	if got := d.Workflows[0].Name; got != "f" {
		t.Fatalf("往返后 workflow 名变了: %q", got)
	}
}

// nativeDiagCodes renders codes for failure messages.
func nativeDiagCodes(ds []native.NativeDiag) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Code)
	}
	return out
}

// TestEmitSortsSchemaJSON 守住 V-8 的写侧。
//
// V-8 要求 `schemas[].json` 重新 canonical 化后与原文逐字节相等。写出的那一刻
// 就是唯一能保证这件事的时机：一旦写出时键序不规范化，V-8 会在**下游**报错，
// 而那时看到的是一份别人产出的 IR，根本不知道是谁写坏的。
func TestEmitSortsSchemaJSON(t *testing.T) {
	d := loadBase(t)
	d.Schemas = []ir.SchemaDoc{{
		ID:   "Intent",
		Root: "Intent",
		JSON: []byte(`{"type":"object","properties":{"z":{"type":"string"},"a":{"type":"integer"}}}`),
	}}
	out, err := ir.Emit(d)
	if err != nil {
		t.Fatal(err)
	}
	// canonical 之后 properties 的键序必须是 a, z。
	if !strings.Contains(string(out), `"properties":{"a"`) {
		t.Fatalf("schema 内层键没有被规范化：\n%s", out)
	}
}

// TestEmitRejectsFloat 证明写侧仍挡 **JSON 浮点数字**，而 v2.0.0 的 double
// 位模式可以正常 Emit（读侧 V-7 同理：hex 字符串不是 json.Number）。
func TestEmitRejectsFloat(t *testing.T) {
	d := loadBase(t)
	// 位模式：{"k":"double","bits":"<16 位小写 hex>"}，两个字段都是字符串，
	// 不是 JSON 数字 —— V-7 扫不到 .eE，canon.FromAny 也不会报 ErrFloat。
	d.Workflows[0].Nodes = append(d.Workflows[0].Nodes, ir.Node{
		ID: 1, K: "lit", Loc: ir.Loc{M: 0, L: 2, C: 1, EL: 2, EC: 3},
		LitT: strptr("double"),
		LitV: []byte(`{"k":"double","bits":"3ff8000000000000"}`),
	})
	out, err := ir.Emit(d)
	if err != nil {
		t.Fatalf("double 位模式应该能 Emit（P-1 禁的是 JSON 浮点数字，不是浮点本身）：%v", err)
	}
	if !strings.Contains(string(out), `"bits":"3ff8000000000000"`) {
		t.Fatalf("产物里没有位模式 bits：\n%s", out)
	}
	if strings.Contains(string(out), `"v":1.5`) {
		t.Fatalf("位模式不该退化成 JSON 数字：\n%s", out)
	}

	// 真正的 JSON 浮点数字仍必须在写侧被 canon.FromAny 挡住（P-1 的另一半）。
	d.Workflows[0].Nodes[len(d.Workflows[0].Nodes)-1].LitV = []byte(`1.5`)
	_, err = ir.Emit(d)
	if err == nil {
		t.Fatal("JSON 浮点数字应该被写出侧拒绝（P-1），而不是原样写进 IR")
	}
	if !strings.Contains(err.Error(), "float") && !strings.Contains(err.Error(), "浮点") {
		t.Fatalf("错误信息没说清是浮点问题: %v", err)
	}
}
