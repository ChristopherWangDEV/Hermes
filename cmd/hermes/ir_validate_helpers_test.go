package main

import (
	"encoding/json"
	"strings"
	"testing"

	"hermes/internal/native"
)

// 这些助手原来住在 ir_equiv_test.go 里（那个文件随 Go 装配器一起删掉了），
// 但 ir_validate_evidence_test.go 仍需要它们：C++ 校验器的负向变异栅栏。
// 装配器没了不等于校验器没了 —— 校验器还在，它的证据也要留着。

type nativeDiag struct {
	Code     string
	Severity string
	Line     int
	Col      int
	Module   string
}

// parseNativeDiags 把 C++ 校验器的 JSON 诊断解成结构体（返回 ok=false 表示解不动）。
func parseNativeDiags(s string) ([]nativeDiag, bool) {
	var raw []map[string]any
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, false
	}
	out := make([]nativeDiag, 0, len(raw))
	for _, m := range raw {
		d := nativeDiag{}
		d.Code, _ = m["code"].(string)
		d.Severity, _ = m["severity"].(string)
		if f, ok := m["line"].(float64); ok {
			d.Line = int(f)
		}
		if f, ok := m["col"].(float64); ok {
			d.Col = int(f)
		}
		d.Module, _ = m["module"].(string)
		out = append(out, d)
	}
	return out, true
}



type irEdit struct {
	from, to string
}

const malformedBaseIR = `{
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

func deriveCleanIRBase(t *testing.T, edits ...irEdit) string {
	t.Helper()
	s := malformedBaseIR
	for _, e := range edits {
		if !strings.Contains(s, e.from) {
			t.Fatalf("搭台锚点不存在：%q", e.from)
		}
		s = strings.Replace(s, e.from, e.to, 1)
	}
	data := []byte(s)
	raw, err := native.IRValidate(data)
	if err != nil {
		t.Fatalf("专用基准 C++ 报错：%v", err)
	}
	ds, ok := parseNativeDiags(string(raw))
	if !ok {
		t.Fatalf("专用基准 C++ 诊断解不动：%s", raw)
	}
	if len(ds) != 0 {
		t.Fatalf("专用基准不干净（C++）：%v", ds)
	}
	return s
}

func hasCodeIn(codes []string, want string) bool {
	for _, c := range codes {
		if c == want {
			return true
		}
	}
	return false
}


func firstNIR(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		s = s[:n]
	}
	return s
}
