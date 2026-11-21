package ir

import "testing"

// refsBaseIR is the minimal valid IR used by the ref-count pin. It is a copy of
// the fixture that used to live in validate_test.go, which is now gone.
const refsBaseIR = `{
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

// TestRefCountIsPinned 引用条数断言：新增 node kind 却没在 appendNodeRefs 里
// 登记时，条数会少。这个数字来自 refsBaseIR 的手工核对（backend 1 条 +
// root 1 条 + returns 是 string 无引用）。
//
// It lives in its own file because collectRefs stays in refs.go (the validator
// files were deleted, refs.go was not).
func TestRefCountIsPinned(t *testing.T) {
	d, ds := Load([]byte(refsBaseIR))
	if ds != nil {
		t.Fatalf("refsBaseIR 反序列化失败：%v", ds)
	}
	if d == nil {
		t.Fatal("refsBaseIR 解析出 nil Doc")
	}
	refs := collectRefs(d)
	// refsBaseIR: workflows[0].backend(1) + workflows[0].root(1) = 2
	if len(refs) != 2 {
		t.Errorf("refsBaseIR 的引用条数 = %d，期望 2（backend + root）。"+
			"若你新增了 node kind 却没在 appendNodeRefs 里登记，这里会变。", len(refs))
	}
}
