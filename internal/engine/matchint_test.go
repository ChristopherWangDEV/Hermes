package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"hermes/internal/ir"
)

type intMatchRT struct{}

func (intMatchRT) Model(context.Context, ModelRequest) (any, error) { return nil, nil }
func (intMatchRT) Tool(context.Context, ToolRequest) (any, error)   { return nil, nil }

// TestMatchOnInteger 守"match 能对整数分派"。
//
// 这条以前不可能：case 只吃字符串，于是所有状态码都得先转字符串再比 ——
// 反直觉，而且转串的开销和错误都白付。
func TestMatchOnInteger(t *testing.T) {
	doc := intMatchDoc()
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{200, "ok"}, {404, "not found"}, {500, "error"},
	} {
		res, err := RunWith(doc, "", map[string]any{"priority": tc.in}, RunOptions{Runtime: intMatchRT{}})
		if err != nil {
			t.Fatalf("%d: %v", tc.in, err)
		}
		if res.Value != tc.want {
			t.Fatalf("priority=%d 应得 %q，实际 %v", tc.in, tc.want, res.Value)
		}
	}
}

func intMatchDoc() *ir.Doc {
	// **节点必须按 ID 顺序排列**：引擎用切片下标当节点 ID。
	inputVar := -1
	subj := 2
	return &ir.Doc{
		IRVersion: 1,
		Unit: ir.Unit{Name: "m", EntryPath: "m.hermes", LanguageVersion: 1,
			Modules: []ir.Module{{Path: "m.hermes", SHA256: strings.Repeat("0", 64)}}},
		Compiler: ir.Compiler{Name: "hermes", Version: "1", ParserABI: 1},
		Types:    []ir.TypeDef{}, Backends: []ir.Backend{}, Schemas: []ir.SchemaDoc{}, Tools: []ir.Tool{},
		Workflows: []ir.Workflow{{
			Name: "m", Exported: true, IsEntry: true, Returns: ir.TypeExpr{K: "string"}, Root: 0,
			Vars: []ir.Var{},
			Nodes: []ir.Node{
				{ID: 0, K: "seq", Kids: []int{3}, Loc: ir.Loc{M: 0, L: 1, C: 1, EL: 1, EC: 2}},
				{ID: 1, K: "var", Var: &inputVar, Loc: ir.Loc{M: 0, L: 2, C: 1, EL: 2, EC: 2}},
				{ID: 2, K: "field", Name: strp("priority"), Of: intp(1), Loc: ir.Loc{M: 0, L: 2, C: 1, EL: 2, EC: 2}},
				{ID: 3, K: "match", Subj: &subj, Default: intp(7),
					Arms: []ir.MatchArm{{Num: int64p(200), Body: 4}, {Num: int64p(404), Body: 5}},
					Loc:  ir.Loc{M: 0, L: 2, C: 1, EL: 2, EC: 2}},
				{ID: 4, K: "return", E: intp(6), Loc: ir.Loc{M: 0, L: 3, C: 1, EL: 3, EC: 2}},
				{ID: 5, K: "return", E: intp(8), Loc: ir.Loc{M: 0, L: 4, C: 1, EL: 4, EC: 2}},
				{ID: 6, K: "lit", LitT: strp("string"), LitV: json.RawMessage(`"ok"`), Loc: ir.Loc{M: 0, L: 3, C: 1, EL: 3, EC: 2}},
				{ID: 7, K: "return", E: intp(9), Loc: ir.Loc{M: 0, L: 5, C: 1, EL: 5, EC: 2}},
				{ID: 8, K: "lit", LitT: strp("string"), LitV: json.RawMessage(`"not found"`), Loc: ir.Loc{M: 0, L: 4, C: 1, EL: 4, EC: 2}},
				{ID: 9, K: "lit", LitT: strp("string"), LitV: json.RawMessage(`"error"`), Loc: ir.Loc{M: 0, L: 5, C: 1, EL: 5, EC: 2}},
			},
			Bounds:    ir.Bounds{MaxSteps: 4, MaxSpans: 2, MaxConcurrency: 1},
			Economics: []byte(`{}`), InterpDeps: []ir.InterpDep{}, Loc: ir.Loc{M: 0, L: 1, C: 1, EL: 1, EC: 2},
		}},
		Imports: []ir.ImportRef{}, Requires: []ir.RequireItem{},
		Exports: ir.Exports{Workflows: []string{"m"}},
	}
}

func int64p(v int64) *int64 { return &v }
