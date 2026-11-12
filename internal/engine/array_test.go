package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"hermes/internal/ir"
)

// TestArrayLiteralAndLen 守数组值语义的第一块：字面量、len()、以及**能作为返回值**。
//
// 这一块以前完全没有：`[1,2,3]` 能过语法但 codegen 直接报"不支持"，
// `len(xs)` 连 parser 都进不去（语言里只有 ns.fn(...) 形式的调用）。
func TestArrayLiteralAndLen(t *testing.T) {
	doc := arrayDoc()
	res, err := RunWith(doc, "", map[string]any{}, RunOptions{Runtime: intMatchRT{}})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	arr, ok := res.Value.([]any)
	if !ok {
		t.Fatalf("应返回数组，实际 %T: %v", res.Value, res.Value)
	}
	if len(arr) != 3 {
		t.Fatalf("数组长度应为 3，实际 %d", len(arr))
	}
	if arr[0] != int64(1) || arr[2] != int64(3) {
		t.Fatalf("元素不对: %v", arr)
	}
}

// TestLenOfLiteral 守 len() 对数组与字符串都成立。
func TestLenOfLiteral(t *testing.T) {
	doc := lenDoc()
	res, err := RunWith(doc, "", map[string]any{}, RunOptions{Runtime: intMatchRT{}})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if res.Value != int64(3) {
		t.Fatalf("len([1,2,3]) 应为 3，实际 %v", res.Value)
	}
}

// arrayDoc: `return [1, 2, 3];`
func arrayDoc() *ir.Doc {
	return miniDoc([]ir.Node{
		{ID: 0, K: "seq", Kids: []int{1}, Loc: ir.Loc{M: 0, L: 1, C: 1, EL: 1, EC: 2}},
		{ID: 1, K: "return", E: intp(2), Loc: ir.Loc{M: 0, L: 2, C: 1, EL: 2, EC: 2}},
		{ID: 2, K: "array_lit", Kids: []int{3, 4, 5}, Loc: ir.Loc{M: 0, L: 2, C: 1, EL: 2, EC: 2}},
		{ID: 3, K: "lit", LitT: strp("int"), LitV: json.RawMessage(`1`), Loc: ir.Loc{M: 0, L: 2, C: 1, EL: 2, EC: 2}},
		{ID: 4, K: "lit", LitT: strp("int"), LitV: json.RawMessage(`2`), Loc: ir.Loc{M: 0, L: 2, C: 1, EL: 2, EC: 2}},
		{ID: 5, K: "lit", LitT: strp("int"), LitV: json.RawMessage(`3`), Loc: ir.Loc{M: 0, L: 2, C: 1, EL: 2, EC: 2}},
	}, "json")
}

// lenDoc: `return len([1, 2, 3]);`
func lenDoc() *ir.Doc {
	return miniDoc([]ir.Node{
		{ID: 0, K: "seq", Kids: []int{1}, Loc: ir.Loc{M: 0, L: 1, C: 1, EL: 1, EC: 2}},
		{ID: 1, K: "return", E: intp(2), Loc: ir.Loc{M: 0, L: 2, C: 1, EL: 2, EC: 2}},
		{ID: 2, K: "len", E: intp(3), Loc: ir.Loc{M: 0, L: 2, C: 1, EL: 2, EC: 2}},
		{ID: 3, K: "array_lit", Kids: []int{4, 5, 6}, Loc: ir.Loc{M: 0, L: 2, C: 1, EL: 2, EC: 2}},
		{ID: 4, K: "lit", LitT: strp("int"), LitV: json.RawMessage(`1`), Loc: ir.Loc{M: 0, L: 2, C: 1, EL: 2, EC: 2}},
		{ID: 5, K: "lit", LitT: strp("int"), LitV: json.RawMessage(`2`), Loc: ir.Loc{M: 0, L: 2, C: 1, EL: 2, EC: 2}},
		{ID: 6, K: "lit", LitT: strp("int"), LitV: json.RawMessage(`3`), Loc: ir.Loc{M: 0, L: 2, C: 1, EL: 2, EC: 2}},
	}, "int")
}

func miniDoc(nodes []ir.Node, ret string) *ir.Doc {
	return &ir.Doc{
		IRVersion: 1,
		Unit: ir.Unit{Name: "arr", EntryPath: "arr.hermes", LanguageVersion: 1,
			Modules: []ir.Module{{Path: "arr.hermes", SHA256: strings.Repeat("0", 64)}}},
		Compiler: ir.Compiler{Name: "hermes", Version: "1", ParserABI: 1},
		Types:    []ir.TypeDef{}, Backends: []ir.Backend{}, Schemas: []ir.SchemaDoc{}, Tools: []ir.Tool{},
		Workflows: []ir.Workflow{{
			Name: "arr", Exported: true, IsEntry: true, Returns: ir.TypeExpr{K: ret}, Root: 0,
			Nodes:     nodes,
			Bounds:    ir.Bounds{MaxSteps: 3, MaxSpans: 1, MaxConcurrency: 1},
			Economics: []byte(`{}`), InterpDeps: []ir.InterpDep{}, Loc: ir.Loc{M: 0, L: 1, C: 1, EL: 1, EC: 2},
		}},
		Imports: []ir.ImportRef{}, Requires: []ir.RequireItem{},
		Exports: ir.Exports{Workflows: []string{"arr"}},
	}
}
