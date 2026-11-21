package ir

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustSchemas(t *testing.T, types []TypeDef) []SchemaDoc {
	t.Helper()
	ss, err := BuildSchemas(types)
	if err != nil {
		t.Fatalf("BuildSchemas: %v", err)
	}
	return ss
}

func decodeSchema(t *testing.T, s SchemaDoc) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(s.JSON, &m); err != nil {
		t.Fatalf("schema 不是合法 JSON: %v\n%s", err, s.JSON)
	}
	return m
}

// TestSchemaOptionalFieldIsRequiredAndNullable 守 LANGUAGE §8.3 规则 6。
//
// 可选字段**仍然进 required**，类型写 anyOf[T, null]。做法相反（把可选字段
// 从 required 里去掉）会让"字段缺失"与"字段为 null"在上游变成两件事，
// 而语言里它们是同一件事。
func TestSchemaOptionalFieldIsRequiredAndNullable(t *testing.T) {
	types := []TypeDef{{
		ID: 1, Name: "S",
		Fields: []Field{{N: "note", T: TypeExpr{K: "string"}, Opt: true}},
	}}
	ss := mustSchemas(t, types)
	m := decodeSchema(t, ss[0])

	req, _ := m["required"].([]any)
	if len(req) != 1 || req[0] != "note" {
		t.Fatalf("可选字段也必须在 required 里，实际 %v", req)
	}
	props := m["properties"].(map[string]any)
	note := props["note"].(map[string]any)
	if _, ok := note["anyOf"]; !ok {
		t.Fatalf("可选字段应写成 anyOf[T, null]，实际 %v", note)
	}
}

// TestSchemaStructFieldIsRef 守"不内联"：struct 字段写成 $ref。
//
// 内联会让同一类型在多处各有一份，改一处忘一处；而 $defs 是**扁平表**，
// 保证同一类型在各根 schema 里字节相同。
func TestSchemaStructFieldIsRef(t *testing.T) {
	types := []TypeDef{
		{ID: 1, Name: "Order", Fields: []Field{{N: "id", T: TypeExpr{K: "string"}}}},
		{ID: 2, Name: "Intent", Fields: []Field{{N: "order", T: TypeExpr{K: "struct", ID: intp(1)}}}},
	}
	ss := mustSchemas(t, types)
	root := ss[1]
	m := decodeSchema(t, root)
	props := m["properties"].(map[string]any)
	ref := props["order"].(map[string]any)["$ref"]
	if ref != "#/$defs/Order" {
		t.Fatalf("struct 字段应是 $ref，实际 %v", ref)
	}
	defs := m["$defs"].(map[string]any)
	if _, ok := defs["Order"]; !ok {
		t.Fatalf("$defs 里应有 Order，实际 %v", defs)
	}
}

// TestSchemaCostCarriesUnit 守 D-018：金额是整数 micro-USD，单位必须写出来。
//
// 不写单位，上游会把 1234 当成 1234 美元 —— 而它其实是 0.001234 美元。
func TestSchemaCostCarriesUnit(t *testing.T) {
	types := []TypeDef{{
		ID: 1, Name: "S",
		Fields: []Field{{N: "amount", T: TypeExpr{K: "cost"}}},
	}}
	m := decodeSchema(t, mustSchemas(t, types)[0])
	amount := m["properties"].(map[string]any)["amount"].(map[string]any)
	if amount["type"] != "integer" {
		t.Fatalf("cost 应是 integer，实际 %v", amount)
	}
	if s, _ := amount["description"].(string); !strings.Contains(s, "micro-USD") {
		t.Fatalf("cost 必须带单位 micro-USD，实际 %v", amount)
	}
}

// TestSchemaJSONIsExplicitUnion 守 §8.4：json 字段默认 7 类型 union，不用 {}。
func TestSchemaJSONIsExplicitUnion(t *testing.T) {
	types := []TypeDef{{
		ID: 1, Name: "S",
		Fields: []Field{{N: "blob", T: TypeExpr{K: "json"}}},
	}}
	m := decodeSchema(t, mustSchemas(t, types)[0])
	blob := m["properties"].(map[string]any)["blob"].(map[string]any)
	arr, ok := blob["type"].([]any)
	if !ok || len(arr) != 7 {
		t.Fatalf("json 字段应是 7 类型 union，实际 %v", blob)
	}
}

// TestSchemaIsCanonical 证明生成的字节已经是 canonical（V-8 的前提）。
func TestSchemaIsCanonical(t *testing.T) {
	types := []TypeDef{
		{ID: 1, Name: "B", Fields: []Field{{N: "z", T: TypeExpr{K: "int"}}, {N: "a", T: TypeExpr{K: "int"}}}},
		{ID: 2, Name: "A", Fields: []Field{{N: "b", T: TypeExpr{K: "struct", ID: intp(1)}}}},
	}
	for _, s := range mustSchemas(t, types) {
		if !strings.HasPrefix(string(s.JSON), `{"$defs"`) {
			t.Fatalf("canonical 的第一个键应是 $defs（字节序），实际: %.60s", s.JSON)
		}
	}
}

// TestSchemaArrayField 守 `string[] tags` 的类型不被静默降级。
//
// 数组维度记在 Type 节点的 i64 里；忽略它，`string[]` 会变成 `string`，
// 而"少了个数组"在 IR 里完全合法，到运行期才表现为"取到的不是列表"。
func TestSchemaArrayField(t *testing.T) {
	elem := TypeExpr{K: "string"}
	of, _ := json.Marshal(elem)
	types := []TypeDef{{
		ID: 1, Name: "S",
		Fields: []Field{{N: "tags", T: TypeExpr{K: "array", Of: of}}},
	}}
	m := decodeSchema(t, mustSchemas(t, types)[0])
	tags := m["properties"].(map[string]any)["tags"].(map[string]any)
	if tags["type"] != "array" {
		t.Fatalf("应是 array，实际 %v", tags)
	}
	items := tags["items"].(map[string]any)
	if items["type"] != "string" {
		t.Fatalf("items 应是 string，实际 %v", items)
	}
}

// TestSchemaHTTPResult 守 HttpResult<T> 的三个字段一个都不能少。
//
// 漏了 status，"订单不存在"的分支就无从判断 —— 而那正是 allow_status 存在的理由。
func TestSchemaHTTPResult(t *testing.T) {
	elem := TypeExpr{K: "struct", ID: intp(2)}
	of, _ := json.Marshal(elem)
	types := []TypeDef{
		{ID: 1, Name: "R", Fields: []Field{{N: "r", T: TypeExpr{K: "httpresult", Of: of}}}},
		{ID: 2, Name: "Order", Fields: []Field{{N: "id", T: TypeExpr{K: "string"}}}},
	}
	m := decodeSchema(t, mustSchemas(t, types)[0])
	r := m["properties"].(map[string]any)["r"].(map[string]any)
	props := r["properties"].(map[string]any)
	for _, want := range []string{"status", "body", "headers"} {
		if _, ok := props[want]; !ok {
			t.Fatalf("HttpResult 缺少 %q：%v", want, props)
		}
	}
	req := r["required"].([]any)
	if len(req) != 3 {
		t.Fatalf("status/body/headers 都应 required，实际 %v", req)
	}
}
