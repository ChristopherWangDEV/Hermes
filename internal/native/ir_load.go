package native

import (
	"encoding/json"
	"fmt"

	"hermes/internal/diag"
	"hermes/internal/ir"
)

// maxRawDiagLen bounds the raw C++ output embedded in a bridge error message.
// The full payload may be large (and may contain binary noise), so only a
// prefix is useful for diagnosing a serialization mismatch.
const maxRawDiagLen = 512

// LoadIR is the single load-time IR entry point: C++ validation plus Doc binding.
//
// Validation itself lives only in the C++ validator. Go keeps exactly two
// responsibilities here: decoding the structured diagnostics into diag.Diag
// (the Go catalog renders the user-visible text) and binding the same bytes
// into an *ir.Doc for the Go consumers. There is deliberately no Go validator
// call and no fallback path.
//
// The five result quadrants are:
//
//   - bridge failure -> (nil, nil, err)
//   - malformed validator output -> (nil, nil, err)
//   - any E_IR_FLOAT -> (nil, ds, nil), without running json.Unmarshal
//   - json.Unmarshal failure -> (nil, ds, nil), synthesizing E_IR_LOAD_FAILED
//   - otherwise -> (&doc, ds, nil), even when ds contains error diagnostics
func LoadIR(data []byte) (*ir.Doc, []diag.Diag, error) {
	raw, err := IRValidate(data)
	if err != nil {
		return nil, nil, fmt.Errorf("ir(native): C++ validator bridge: %w", err)
	}

	nativeDS, ok := ParseValidateDiags(string(raw))
	if !ok {
		report := string(raw)
		if len(report) > maxRawDiagLen {
			report = report[:maxRawDiagLen] + "..."
		}
		return nil, nil, fmt.Errorf("ir(native): cannot parse C++ validator diagnostics: %s", report)
	}
	ds := ValidateDiagsToDiag(nativeDS)

	// A float is a user input error, not an infrastructure failure. Short-circuit
	// before Unmarshal: a float in a typed int field would otherwise produce a
	// Go decoding error and be misclassified, and a float in a RawMessage field
	// would silently enter the IR hash. Both cases return the same shape.
	for _, d := range ds {
		if d.Code == diag.CodeFloat {
			return nil, ds, nil
		}
	}

	var doc ir.Doc
	if err := json.Unmarshal(data, &doc); err != nil {
		// Data binding failure is reported as a diagnostic, never as err: the
		// err channel is reserved for C++ bridge and diagnostic decode failures.
		if !hasDiagCode(ds, diag.CodeLoadFailed) {
			// detail 是 Go 解码错误原文，作为模板实参交给 C++（catalog 里有
			// {detail} 槽）—— 丢它等于让用户只看到"IR 加载失败"而不知道
			// 哪个字段类型不对。
			d, _ := DiagFromCpp(diag.CodeLoadFailed, "error", diag.Loc{}, "",
				map[string]string{"detail": err.Error()}, nil, string(diag.LangFromEnv()))
			ds = append(ds, d)
		}
		return nil, ds, nil
	}
	return &doc, ds, nil
}

// hasDiagCode reports whether ds already contains a diagnostic with code.
func hasDiagCode(ds []diag.Diag, code string) bool {
	for _, d := range ds {
		if d.Code == code {
			return true
		}
	}
	return false
}
