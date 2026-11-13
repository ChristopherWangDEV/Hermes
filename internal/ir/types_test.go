package ir

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestResolveTypesAssignsIDsInDeclarationOrder 证明 id 按声明顺序从 1 递增，
// 且 struct 引用被解析成 id（IR.md §3.1）。
//
// id 顺序不是审美：它决定 `$defs` 的键序，进而决定 JSON Schema 的字节，
// 也就决定 `schemas[].json` 的哈希。
func TestResolveTypesAssignsIDsInDeclarationOrder(t *testing.T) {
	raw := `[
	  {"name":"OrderInfo","exported":true,"loc":{"m":0,"l":1,"c":1,"el":1,"ec":2},
	   "fields":[{"n":"id","t":{"k":"string"},"opt":false,"ann":[]}]},
	  {"name":"Intent","exported":true,"loc":{"m":0,"l":5,"c":1,"el":5,"ec":2},
	   "fields":[{"n":"order","t":{"k":"struct","ref":"OrderInfo"},"opt":false,"ann":[]}]}
	]`
	ts, err := resolveTypes([]byte(raw), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 2 {
		t.Fatalf("应解析出 2 个类型，实际 %d", len(ts))
	}
	if ts[0].ID != 1 || ts[1].ID != 2 {
		t.Fatalf("id 应 1,2，实际 %d,%d", ts[0].ID, ts[1].ID)
	}
	if ts[1].Fields[0].T.ID == nil || *ts[1].Fields[0].T.ID != 1 {
		t.Fatalf("struct 引用应解析成 id 1，实际 %+v", ts[1].Fields[0].T)
	}
	if got := exportedTypeNames(ts); len(got) != 2 {
		t.Fatalf("exports.types 应有 2 个，实际 %v", got)
	}
}

// TestTypeSHA256IgnoresLocation 证明类型哈希与位置无关。
//
// 带上 loc，两个库里同名同构的 struct 会因行号不同而"不一致"，
// 链接期 ODR 判定直接失效 —— 而失效的表现是"明明同一个结构却说定义冲突"。
func TestTypeSHA256IgnoresLocation(t *testing.T) {
	fields := []Field{{N: "a", T: TypeExpr{K: "string"}}}
	a, err := typeSHA256("T", fields)
	if err != nil {
		t.Fatal(err)
	}
	b, err := typeSHA256("T", fields)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("同一份字段定义两次哈希应相等")
	}
	// 改字段类型必须改变哈希。
	c, err := typeSHA256("T", []Field{{N: "a", T: TypeExpr{K: "int"}}})
	if err != nil {
		t.Fatal(err)
	}
	if a == c {
		t.Fatal("字段类型变了，哈希必须变")
	}
}

// TestResolveTypesRejectsUnknownStruct 证明拼错类型名会报错而不是静默变成无效引用。
func TestResolveTypesRejectsUnknownStruct(t *testing.T) {
	raw := `[{"name":"Intent","exported":true,"loc":{"m":0,"l":1,"c":1,"el":1,"ec":2},
	  "fields":[{"n":"x","t":{"k":"struct","ref":"Nope"},"opt":false,"ann":[]}]}]`
	_, err := resolveTypes([]byte(raw), 0)
	if err == nil {
		t.Fatal("引用未声明的 struct 应报错")
	}
	if !strings.Contains(err.Error(), "Nope") {
		t.Fatalf("错误信息应点名那个类型，实际: %v", err)
	}
}

// TestTypeJSONRoundTripIsStable 证明类型表序列化后再解析结果相同（无隐藏状态）。
func TestTypeJSONRoundTripIsStable(t *testing.T) {
	raw := `[{"name":"A","exported":false,"loc":{"m":0,"l":1,"c":1,"el":1,"ec":2},
	  "fields":[{"n":"x","t":{"k":"int"},"opt":true,"ann":[]}]}]`
	ts, err := resolveTypes([]byte(raw), 0)
	if err != nil {
		t.Fatal(err)
	}
	buf, err := json.Marshal(ts)
	if err != nil {
		t.Fatal(err)
	}
	var back []TypeDef
	if err := json.Unmarshal(buf, &back); err != nil {
		t.Fatal(err)
	}
	if back[0].SHA256 != ts[0].SHA256 || back[0].ID != ts[0].ID {
		t.Fatalf("往返后类型定义变了: %+v vs %+v", back[0], ts[0])
	}
}
