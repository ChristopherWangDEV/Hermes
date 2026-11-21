package native

import (
	"encoding/json"
	"strings"
	"testing"

	"hermes/internal/diag"
	"hermes/internal/ir"
)

// irLoadBaseIR is a minimal, self-contained IR with no diagnostics. It is kept
// small so each quadrant below is a single deterministic mutation of a known
// clean baseline (the same baseline shape used by cmd/hermes equivalence tests).
const irLoadBaseIR = `{
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

// assertIRLoadBaseIsClean fails early if the fixture itself stops being valid:
// otherwise a mutation test could pass with both sides reporting the baseline's
// own diagnostics.
func assertIRLoadBaseIsClean(t *testing.T) {
	t.Helper()
	doc, ds, err := LoadIR([]byte(irLoadBaseIR))
	if err != nil {
		t.Fatalf("LoadIR(base): unexpected bridge error: %v", err)
	}
	if doc == nil {
		t.Fatal("LoadIR(base): doc must be non-nil for a clean IR")
	}
	if len(ds) != 0 {
		t.Fatalf("LoadIR(base): expected no diagnostics, got %v", irLoadCodes(ds))
	}
}

func irLoadCodes(ds []diag.Diag) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Code)
	}
	return out
}

func irLoadHasCode(ds []diag.Diag, code string) bool {
	for _, d := range ds {
		if d.Code == code {
			return true
		}
	}
	return false
}

// Quadrant 1: C++ bridge failure is infrastructure failure. No diagnostics
// are synthesized and no Doc is bound.
func TestLoadIRInfraFailure(t *testing.T) {
	doc, ds, err := LoadIR(nil)
	if err == nil {
		t.Fatal("LoadIR(nil): expected a bridge error")
	}
	if doc != nil {
		t.Errorf("LoadIR(nil): doc must be nil, got %+v", doc)
	}
	if ds != nil {
		t.Errorf("LoadIR(nil): ds must be nil, got %v", irLoadCodes(ds))
	}
}

// Quadrant 2: malformed JSON is a user input error. The C++ validator emits
// E_IR_LOAD_FAILED with rc=0, so this must not escape through the err channel.
func TestLoadIRJSONSyntaxError(t *testing.T) {
	doc, ds, err := LoadIR([]byte("{not json"))
	if err != nil {
		t.Fatalf("LoadIR(bad JSON): bridge error must stay nil, got %v", err)
	}
	if doc != nil {
		t.Errorf("LoadIR(bad JSON): doc must be nil, got %+v", doc)
	}
	if !irLoadHasCode(ds, diag.CodeLoadFailed) {
		t.Fatalf("LoadIR(bad JSON): expected %s, got %v", diag.CodeLoadFailed, irLoadCodes(ds))
	}
}

// Quadrant 3: a float in a typed int field. The original Go loader short-
// circuited on any float before json.Unmarshal; LoadIR must preserve that.
func TestLoadIRFloatInTypedIntField(t *testing.T) {
	assertIRLoadBaseIsClean(t)
	bad := strings.Replace(irLoadBaseIR, `"ir_version": 2`, `"ir_version": 1.5`, 1)
	if bad == irLoadBaseIR {
		t.Fatal("float mutation anchor not found")
	}

	// Prove the input really would fail data binding, otherwise the short-circuit
	// assertion below is vacuous.
	var probe ir.Doc
	if err := json.Unmarshal([]byte(bad), &probe); err == nil {
		t.Fatal("test input is not a typed-int float: json.Unmarshal unexpectedly succeeded")
	}

	doc, ds, err := LoadIR([]byte(bad))
	if err != nil {
		t.Fatalf("LoadIR(float in int field): expected nil err, got %v", err)
	}
	if doc != nil {
		t.Errorf("LoadIR(float in int field): doc must be nil, got %+v", doc)
	}
	if !irLoadHasCode(ds, diag.CodeFloat) {
		t.Fatalf("LoadIR(float in int field): expected %s, got %v", diag.CodeFloat, irLoadCodes(ds))
	}
}

// Quadrant 3 (v2.1 erratum): a float inside a json.RawMessage field binds
// successfully in Go, but must still short-circuit with a nil Doc.
func TestLoadIRFloatInRawMessageField(t *testing.T) {
	assertIRLoadBaseIsClean(t)
	bad := strings.Replace(irLoadBaseIR, `"economics":{}`, `"economics":{"cost_estimate":0.25}`, 1)
	if bad == irLoadBaseIR {
		t.Fatal("RawMessage float mutation anchor not found")
	}

	// Prove this is the RawMessage case: binding would have succeeded.
	var probe ir.Doc
	if err := json.Unmarshal([]byte(bad), &probe); err != nil {
		t.Fatalf("test input is not a RawMessage float: json.Unmarshal failed: %v", err)
	}

	doc, ds, err := LoadIR([]byte(bad))
	if err != nil {
		t.Fatalf("LoadIR(float in RawMessage): expected nil err, got %v", err)
	}
	if doc != nil {
		t.Errorf("LoadIR(float in RawMessage): doc must be nil per the v2.1 erratum, got %+v", doc)
	}
	if !irLoadHasCode(ds, diag.CodeFloat) {
		t.Fatalf("LoadIR(float in RawMessage): expected %s, got %v", diag.CodeFloat, irLoadCodes(ds))
	}
}

// Quadrant 4: an ordinary validator error is reported through ds while the
// Doc is still bound, so callers can inspect the parsed IR if they want to.
func TestLoadIRValidatorErrorKeepsDoc(t *testing.T) {
	assertIRLoadBaseIsClean(t)
	bad := strings.Replace(irLoadBaseIR, `"ir_version": 2`, `"ir_version": 1`, 1)
	if bad == irLoadBaseIR {
		t.Fatal("version mutation anchor not found")
	}

	doc, ds, err := LoadIR([]byte(bad))
	if err != nil {
		t.Fatalf("LoadIR(version mismatch): expected nil err, got %v", err)
	}
	if doc == nil {
		t.Fatal("LoadIR(version mismatch): doc must be non-nil")
	}
	if !irLoadHasCode(ds, diag.CodeIRVersion) {
		t.Fatalf("LoadIR(version mismatch): expected %s, got %v", diag.CodeIRVersion, irLoadCodes(ds))
	}
}
