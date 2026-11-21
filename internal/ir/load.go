package ir

import (
	"encoding/json"

	"hermes/internal/diag"
)

// Load binds IR bytes into a Doc.
//
// Load does **no validation**. The only validator is the C++ implementation
// behind native.LoadIR (internal/native/ir_load.go); every production load path
// calls it, and it is the single source of validation truth. Keeping a second
// implementation here is exactly what the migration removed.
//
// The two things this function still owns are "result representation", not
// validation:
//
//	(1) json.Unmarshal -> *Doc, because Doc is a Go struct consumed by
//	    engine / link / runtime / header.
//	(2) A diagnostic for malformed JSON, so callers get a renderable
//	    E_IR_LOAD_FAILED instead of a raw *json.SyntaxError.
//
// A nil Doc means the bytes could not be bound to the IR shape at all.
func Load(data []byte) (*Doc, []diag.Diag) {
	var d Doc
	if err := json.Unmarshal(data, &d); err != nil {
		// 不调 diag.New：本包必须 cgo-free，且 diag.New 的文案表即将被
		// C++ catalog 取代。这里显式构造（见 host_diag.go 的完整理由）。
		return nil, []diag.Diag{loadFailedDiag(err.Error())}
	}
	return &d, nil
}
