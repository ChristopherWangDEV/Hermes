package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hermes/internal/diag"
	"hermes/internal/ir"
	"hermes/internal/native"
)

// This file holds the validation evidence that used to live in
// internal/ir/validate_test.go, load_test.go and bounds_test.go. Those files
// tested the Go validator, which no longer exists: hm_ir_validate is the only
// implementation. Deleting a test is only allowed after its evidence is ported,
// so every assertion below runs the real C++ validator over the same shapes the
// Go tests used.
//
// The expectations are frozen literals, not a comparison against a second
// implementation. That is deliberate, not circular: commit cb3f913 ran each of
// these cases against the Go oracle while it still existed and required both
// sides to agree (truncation 101 = 101, every hidden edge 1 = 1, the decision
// matrix, the idem_key golden). This file keeps the numbers proved then, so a
// future C++ change that widens or narrows the contract still turns red.

// cppDiags runs the real C++ validator (hm_ir_validate) over data.
func cppDiags(t *testing.T, data []byte) []native.NativeDiag {
	t.Helper()
	raw, err := native.IRValidate(data)
	if err != nil {
		t.Fatalf("C++ validator bridge error: %v", err)
	}
	ds, ok := native.ParseValidateDiags(string(raw))
	if !ok {
		t.Fatalf("cannot parse C++ diagnostics: %s", raw)
	}
	return ds
}

func cppCount(ds []native.NativeDiag, code string) int {
	n := 0
	for _, d := range ds {
		if d.Code == code {
			n++
		}
	}
	return n
}

func cppCodes(ds []native.NativeDiag) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Code)
	}
	return out
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return b
}

// ---- the diagnostic panel is bounded, and says so ---------------------------

// TestCValidateDiagCap ports TestDiagCap. A hostile IR must not make the
// diagnostic panel grow without bound: max_diags is 100, and the dropped
// remainder is summarized by exactly one W_IR_PANEL_TRUNCATED on top of the
// cap (Go's finish() appended it outside the cap, and so must we).
func TestCValidateDiagCap(t *testing.T) {
	kids := make([]int, 150)
	for i := range kids {
		kids[i] = 900 + i // every one dangling
	}
	d := &ir.Doc{
		IRVersion: 2,
		Unit:      ir.Unit{LanguageVersion: 2},
		Workflows: []ir.Workflow{{
			Name: "f",
			Root: 0,
			Nodes: []ir.Node{{
				ID: 0, K: "seq",
				Kids: kids,
			}},
		}},
	}
	ds := cppDiags(t, mustJSON(t, d))

	if len(ds) != 101 {
		t.Errorf("expected 100 capped errors + 1 truncation warning = 101, got %d: %v",
			len(ds), cppCodes(ds))
	}
	if got := cppCount(ds, diag.CodePanelTruncated); got != 1 {
		t.Errorf("dropped diagnostics must be summarized by exactly one %s, got %d: %v",
			diag.CodePanelTruncated, got, cppCodes(ds))
	}
	if got := cppCount(ds, diag.CodeDanglingRef); got != 98 {
		t.Errorf("expected 98 %s (2 of the 100 capped slots go to E_IR_BAD_LOC), got %d",
			diag.CodeDanglingRef, got)
	}

	// The summary carries the numbers as structured facts: the Go catalog
	// re-renders from args, so an empty map would print raw {placeholders}.
	var panel *native.NativeDiag
	for i := range ds {
		if ds[i].Code == diag.CodePanelTruncated {
			panel = &ds[i]
			break
		}
	}
	if panel == nil {
		t.Fatalf("no %s in output: %v", diag.CodePanelTruncated, cppCodes(ds))
	}
	if panel.Severity != "warning" {
		t.Errorf("panel summary must be a warning, got %q", panel.Severity)
	}
	for _, key := range []string{"shown", "dropped"} {
		if v, ok := panel.Args[key]; !ok || v == "" {
			t.Errorf("panel summary must carry args[%q], got %v", key, panel.Args)
		}
	}
	if strings.Contains(panel.Message, "{shown}") || strings.Contains(panel.Message, "{dropped}") {
		t.Errorf("panel message kept its placeholders: %q", panel.Message)
	}
}

// ---- individual V checks whose Go tests are gone ---------------------------

// TestCValidateInputSentinelIsAllowed ports TestVInputSentinelIsAllowed.
// `var: -1` is the implicit input binding and must never be reported as a
// dangling variable reference. If this regresses, every IR that reads its
// input parameter becomes unusable.
func TestCValidateInputSentinelIsAllowed(t *testing.T) {
	loc := ir.Loc{M: 0, L: 1, C: 1, EL: 1, EC: 10}
	minusOne, one := -1, 1
	d := &ir.Doc{
		IRVersion: 2,
		Unit:      ir.Unit{LanguageVersion: 2},
		Workflows: []ir.Workflow{{
			Name: "f",
			Root: 0,
			Nodes: []ir.Node{
				{ID: 0, K: "seq", Kids: []int{1, 2}, Loc: loc},
				{ID: 1, K: "var", Var: &minusOne, Loc: loc},
				{ID: 2, K: "return", E: &one, Loc: loc},
			},
		}},
	}
	ds := cppDiags(t, mustJSON(t, d))
	if got := cppCount(ds, diag.CodeDanglingRef); got != 0 {
		t.Fatalf("var=-1 (input sentinel) was reported as dangling: %v", cppCodes(ds))
	}
}

// TestCValidateFloatIgnoresDoubleBitPattern ports TestScanFloatsIgnoresDoubleBitPattern.
// A double constant is encoded as a bit-pattern object whose fields are strings,
// so V-7 must not fire; a real JSON float next to it must fire exactly once.
func TestCValidateFloatIgnoresDoubleBitPattern(t *testing.T) {
	clean := []byte(`{"k":"double","bits":"3ff8000000000000","text":"1.5"}`)
	if got := cppCount(cppDiags(t, clean), diag.CodeFloat); got != 0 {
		t.Fatalf("bit-pattern hex strings must not trigger V-7, got %d: %v", got, cppCodes(cppDiags(t, clean)))
	}
	dirty := []byte(`{"k":"double","bits":"3ff8000000000000","bad":1.5}`)
	if got := cppCount(cppDiags(t, dirty), diag.CodeFloat); got != 1 {
		t.Fatalf("one JSON float must produce exactly one %s, got %d: %v",
			diag.CodeFloat, got, cppCodes(cppDiags(t, dirty)))
	}
}

// TestCValidateGoldenBoundsNoDrift ports the live half of
// TestGoldenBoundsMatchRecompute: every golden must validate with no
// E_IR_RECOMPUTE_DRIFT, i.e. the declared bounds still match the C++ recompute.
//
// The other bounds_test.go cases tested the deleted Go recompute implementation
// directly. Their behavioral coverage now comes from this drift check plus
// TestIRAssemblyEquivalence (byte-for-byte assembly against the 41 goldens).
func TestCValidateGoldenBoundsNoDrift(t *testing.T) {
	root := repoRootForBackend(t)
	goldens, err := filepath.Glob(filepath.Join(root, "tests", "ir", "golden", "*.json"))
	if err != nil || len(goldens) == 0 {
		t.Fatalf("no IR goldens found: %v", err)
	}
	for _, path := range goldens {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			ds := cppDiags(t, data)
			if got := cppCount(ds, diag.CodeRecomputeDrift); got != 0 {
				t.Errorf("golden reports %d %s: %v", got, diag.CodeRecomputeDrift, cppCodes(ds))
			}
		})
	}
}

// TestCValidateEntryNotExported ports TestVEntryNotExported. A workflow marked
// entry must also be exported.
func TestCValidateEntryNotExported(t *testing.T) {
	// Keep the base clean first (not an entry, not exported, no exports entry),
	// then flip only is_entry so the mutation is the single cause.
	base := deriveCleanIRBase(t,
		irEdit{`"exported":true,"is_entry":true`, `"exported":false,"is_entry":false`},
		irEdit{`"exports":{"workflows":["f"],`, `"exports":{"workflows":[],`},
	)
	broken := strings.Replace(base, `"exported":false,"is_entry":false`, `"exported":false,"is_entry":true`, 1)
	if broken == base {
		t.Fatal("mutation anchor not found")
	}
	ds := cppDiags(t, []byte(broken))
	if !hasCodeIn(cppCodes(ds), diag.CodeEntry) {
		t.Fatalf("entry that is not exported must report %s, got %v", diag.CodeEntry, cppCodes(ds))
	}
}

// ---- E_IR_RETRY_UNPROVEN: V-12 walks node-reference edges -------------------

func retryInt(x int) *int       { return &x }
func retryStr(x string) *string { return &x }

// emitIntPtr is a tiny helper so the emit fixtures below read as data.
func emitIntPtr(x int) *int { return &x }

// emitDoc is a three-node workflow shaped so that an emit node's Target is the
// only reference that can dangle: seq(0) -> emit(1, route=2) -> nop(2). Root is
// the seq, ids equal their indices, and args is empty, so a dangling count of 1
// can only come from target.
func emitDoc(t *testing.T, target int) []byte {
	t.Helper()
	d := &ir.Doc{
		IRVersion: 2,
		Unit:      ir.Unit{LanguageVersion: 2},
		Workflows: []ir.Workflow{{
			Name: "f",
			Root: 0,
			Nodes: []ir.Node{
				{ID: 0, K: "seq", Kids: []int{1}},
				{ID: 1, K: "emit", Route: emitIntPtr(2), Target: emitIntPtr(target),
					Signal: retryStr("Tick"), Args: json.RawMessage(`[]`)},
				{ID: 2, K: "nop"},
			},
		}},
	}
	return mustJSON(t, d)
}

// TestCValidateEmitTargetIsResolved is the regression anchor for the key fix in
// cb3f913: V-2 used to read `emit.to`, but an emit node's target is
// Node.Target, whose JSON key is "target" ("to" is the call_tool/cast *type*
// field). The read therefore never matched, and a dangling emit target was
// silently accepted — a blind slot in the reference checker.
//
// No golden ships a routed emit, so the 41-golden baselines cannot catch a
// regression here: this negative case is the only thing that can. Reverting the
// key makes the first subtest go from 1 diagnostic to 0, which is exactly the
// "positive-only change proves no false positives, not no missed reports"
// failure mode.
func TestCValidateEmitTargetIsResolved(t *testing.T) {
	t.Run("dangling target must be reported", func(t *testing.T) {
		ds := cppDiags(t, emitDoc(t, 999))
		if got := cppCount(ds, diag.CodeDanglingRef); got != 1 {
			t.Fatalf("emit.target pointing at a missing node must produce exactly one %s, got %d: %v",
				diag.CodeDanglingRef, got, cppCodes(ds))
		}
		// Pin the slot as well as the code: the diagnostic must name the target
		// reference, not some other position that happens to be broken too.
		var found bool
		for _, d := range ds {
			if d.Code != diag.CodeDanglingRef {
				continue
			}
			if p := d.Args["path"]; strings.HasSuffix(p, ".to") {
				found = true
			}
		}
		if !found {
			t.Fatalf("dangling diagnostic must name the target slot, got %v", cppCodes(ds))
		}
	})

	t.Run("valid target is not reported", func(t *testing.T) {
		ds := cppDiags(t, emitDoc(t, 2))
		if got := cppCount(ds, diag.CodeDanglingRef); got != 0 {
			t.Fatalf("a resolvable emit.target must produce no %s, got %d: %v",
				diag.CodeDanglingRef, got, cppCodes(ds))
		}
	})
}

// retryDoc builds a two-tool world: pay's idem.mode is supplied per case, safe
// is always readonly. Other validity problems (no modules, no bounds) are
// irrelevant here: we count only E_IR_RETRY_UNPROVEN, and the validator keeps
// going after reporting the unrelated shape errors.
func retryDoc(t *testing.T, mode string, nodes []ir.Node) []byte {
	t.Helper()
	d := &ir.Doc{
		IRVersion: 2,
		Unit:      ir.Unit{LanguageVersion: 2},
		Tools: []ir.Tool{
			{Name: "pay", Method: "POST", Idem: ir.Idem{Mode: mode}},
			{Name: "safe", Method: "GET", Idem: ir.Idem{Mode: "readonly"}},
		},
		Workflows: []ir.Workflow{{Name: "f", Nodes: nodes}},
	}
	return mustJSON(t, d)
}

func retrySeq(id int, kids ...int) ir.Node { return ir.Node{ID: id, K: "seq", Kids: kids} }
func retryCall(id int, tool string) ir.Node {
	return ir.Node{ID: id, K: "call_tool", Tool: retryStr(tool)}
}
func retryNode(id, body int) ir.Node {
	return ir.Node{ID: id, K: "retry", N: retryInt(2), DelayMS: retryInt(0), Body: retryInt(body)}
}

// TestCValidateRetryHiddenEdges is the V-12 edge-set contract. Each row hides a
// `call_tool pay` (POST, idem.mode=none) behind ONE expression slot inside a
// retry subtree. docs/LANGUAGE.md:1738 defines V-12 as "reachable from
// retry.body along node reference edges", so every one of them must report.
// Before commit cb3f913 the C++ walk used control-flow edges only and reported
// 0 for all of them: a tampered IR could smuggle a non-idempotent POST past the
// artifact-side E5007 defense.
//
// The last row is the control case: try.catches is a control-flow edge, so it
// reported correctly before the fix too. It must keep reporting, which proves
// the walk still works and was not merely made noisy.
func TestCValidateRetryHiddenEdges(t *testing.T) {
	cases := []struct {
		name  string
		nodes []ir.Node
		want  int
	}{
		{
			name: "fallback.main (expression slot)",
			nodes: []ir.Node{
				retryNode(0, 1), retrySeq(1, 2),
				{ID: 2, K: "fallback", Main: retryInt(3), Body: retryInt(4)},
				retryCall(3, "pay"), retrySeq(4),
			},
			want: 1,
		},
		{
			name: "call_tool.args.body (nested expression argument)",
			nodes: []ir.Node{
				retryNode(0, 1), retrySeq(1, 2),
				{ID: 2, K: "call_tool", Tool: retryStr("safe"), Args: json.RawMessage(`{"body":3}`)},
				retryCall(3, "pay"),
			},
			want: 1,
		},
		{
			name: "parallel.join (tuple element)",
			nodes: []ir.Node{
				retryNode(0, 1), retrySeq(1, 2),
				{ID: 2, K: "parallel", Branches: []int{4}, Join: []int{3}},
				retryCall(3, "pay"), retrySeq(4),
			},
			want: 1,
		},
		{
			name: "sleep.ms (duration expression)",
			nodes: []ir.Node{
				retryNode(0, 1), retrySeq(1, 2),
				{ID: 2, K: "sleep", MS: retryInt(3)}, retryCall(3, "pay"),
			},
			want: 1,
		},
		{
			name: "stop.data (payload expression)",
			nodes: []ir.Node{
				retryNode(0, 1), retrySeq(1, 2),
				{ID: 2, K: "stop", Data: retryInt(3)}, retryCall(3, "pay"),
			},
			want: 1,
		},
		{
			name: "emit.args (positional arguments)",
			nodes: []ir.Node{
				retryNode(0, 1), retrySeq(1, 2),
				{ID: 2, K: "emit", Args: json.RawMessage(`[3]`)}, retryCall(3, "pay"),
			},
			want: 1,
		},
		{
			// Derived from node_ref_successors reusing append_node_refs: the
			// same key fix that lets V-2 see emit.target also lets V-12 walk it.
			name: "emit.target (routed payload expression)",
			nodes: []ir.Node{
				retryNode(0, 1), retrySeq(1, 2),
				{ID: 2, K: "emit", Route: emitIntPtr(2), Target: emitIntPtr(3),
					Signal: retryStr("Tick"), Args: json.RawMessage(`[]`)},
				retryCall(3, "pay"),
			},
			want: 1,
		},
		{
			name: "control: try.catches.body (control-flow edge)",
			nodes: []ir.Node{
				retryNode(0, 1), retrySeq(1, 2),
				{ID: 2, K: "try", Body: retryInt(3), Catches: []ir.CatchArm{{Body: 4}}},
				retryCall(3, "pay"), retryCall(4, "pay"),
			},
			want: 2,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ds := cppDiags(t, retryDoc(t, "none", c.nodes))
			if got := cppCount(ds, diag.CodeRetryUnproven); got != c.want {
				t.Fatalf("%s must be exactly %d, got %d: %v",
					diag.CodeRetryUnproven, c.want, got, cppCodes(ds))
			}
		})
	}
}

// TestCValidateRetryIdempotencyOnNodeGraph ports TestRetryIdempotencyOnNodeGraph:
// seven proof/unproven modes, the not-reachable case, nested retry collapsing to
// one report, two distinct bad calls, an unknown tool (V-2 owns it), a body-less
// retry, and the attempt_ctx back-reference that must NOT be followed.
//
// The precision matters: "outer call proven but nested call_tool not proven"
// must report exactly one diagnostic, and nested retries must not multiply the
// report. Collapsing these into one coarse case would lose the reachability
// distinctions the validator exists to make.
func TestCValidateRetryIdempotencyOnNodeGraph(t *testing.T) {
	base := func(tool string) []ir.Node {
		return []ir.Node{
			retryNode(0, 1), retrySeq(1, 2),
			{ID: 2, K: "decl", E: retryInt(3)},
			retryCall(3, tool),
		}
	}

	cases := []struct {
		name  string
		mode  string
		nodes []ir.Node
		want  int
	}{
		{"none must report", "none", base("pay"), 1},
		{"missing mode (hand-edited) must report", "", base("pay"), 1},
		{"unknown mode is treated as unproven", "garbage", base("pay"), 1},
		{"readonly is proof", "readonly", base("pay"), 0},
		{"idem_true is proof", "idem_true", base("pay"), 0},
		{"idem_key is proof", "idem_key", base("pay"), 0},
		{"proven tool is unaffected by pay's mode", "none", base("safe"), 0},
		{
			name: "call_tool outside the retry subtree is not reported",
			mode: "none",
			nodes: []ir.Node{
				retrySeq(0, 1, 3), retryNode(1, 2), retrySeq(2), retryCall(3, "pay"),
			},
			want: 0,
		},
		{
			name: "hidden in parallel.branches",
			mode: "none",
			nodes: []ir.Node{
				retryNode(0, 1), retrySeq(1, 2),
				{ID: 2, K: "parallel", Branches: []int{3}},
				retryCall(3, "pay"),
			},
			want: 1,
		},
		{
			name: "hidden in if.then",
			mode: "none",
			nodes: []ir.Node{
				retryNode(0, 1), retrySeq(1, 2),
				{ID: 2, K: "if", Then: retryInt(3), Else: retryInt(4)},
				retryCall(3, "pay"), retrySeq(4),
			},
			want: 1,
		},
		{
			name:  "nested retry reports once, not twice",
			mode:  "none",
			nodes: []ir.Node{retryNode(0, 1), retryNode(1, 2), retrySeq(2, 3), retryCall(3, "pay")},
			want:  1,
		},
		{
			name:  "two distinct bad calls report twice",
			mode:  "none",
			nodes: []ir.Node{retryNode(0, 1), retrySeq(1, 2, 3), retryCall(2, "pay"), retryCall(3, "pay")},
			want:  2,
		},
		{
			name:  "unknown tool name: V-2 owns it, do not double-report",
			mode:  "none",
			nodes: base("ghost"),
			want:  0,
		},
		{
			name:  "retry without body must not crash",
			mode:  "none",
			nodes: []ir.Node{{ID: 0, K: "retry"}},
			want:  0,
		},
		{
			name: "attempt_ctx.retry_node is a back-reference, not an execution edge",
			mode: "none",
			nodes: []ir.Node{
				retryNode(0, 1), retrySeq(1),
				{ID: 2, K: "call_tool", Tool: retryStr("pay"),
					Att: &ir.AttemptCtx{RetryNode: 0}},
			},
			want: 0,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ds := cppDiags(t, retryDoc(t, c.mode, c.nodes))
			if got := cppCount(ds, diag.CodeRetryUnproven); got != c.want {
				t.Fatalf("%s must be exactly %d, got %d: %v",
					diag.CodeRetryUnproven, c.want, got, cppCodes(ds))
			}
		})
	}
}

// TestCValidateLoadRejectsTamperedRetryIR ports TestLoadRejectsTamperedRetryIR.
// It uses a real golden and a single string replacement, so the only difference
// from a valid artifact is the idem mode — and it goes through LoadIR, the
// production entry point, not the bare validator.
func TestCValidateLoadRejectsTamperedRetryIR(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRootForBackend(t), "schemas", "golden", "ir-idemkey.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ds, err := native.LoadIR(raw); err != nil || len(ds) != 0 {
		t.Fatalf("original golden must be clean, got err=%v codes=%v", err, diagCodesOf(ds))
	}

	const from = `"mode":"idem_key"`
	if n := strings.Count(string(raw), from); n != 1 {
		t.Fatalf("golden must contain %s exactly once, got %d", from, n)
	}

	check := func(label string, data []byte, want int) {
		t.Helper()
		_, ds, err := native.LoadIR(data)
		if err != nil {
			t.Fatalf("%s: LoadIR error: %v", label, err)
		}
		if got := countDiagCode(ds, diag.CodeRetryUnproven); got != want {
			t.Fatalf("%s: %s must be %d, got %d: %v",
				label, diag.CodeRetryUnproven, want, got, diagCodesOf(ds))
		}
		if want == 0 && len(ds) != 0 {
			t.Fatalf("%s: expected no diagnostics, got %v", label, diagCodesOf(ds))
		}
	}

	s := string(raw)
	check("switch to another proof value (idem_true)", []byte(strings.Replace(s, from, `"mode":"idem_true"`, 1)), 0)
	check("switch to none (unproven)", []byte(strings.Replace(s, from, `"mode":"none"`, 1)), 1)
	check("delete the whole mode key (unproven)", []byte(strings.Replace(s, `,"mode":"idem_key"`, ``, 1)), 1)
}

func diagCodesOf(ds []diag.Diag) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Code)
	}
	return out
}

func countDiagCode(ds []diag.Diag, code string) int {
	n := 0
	for _, d := range ds {
		if d.Code == code {
			n++
		}
	}
	return n
}
