// schema.go —— C++ JSON Schema 校验器的 cgo 绑定。
//
// 分工：`internal/schema` 仍是**运行期**（engine 校验模型响应）的实现；
// 这一层给**消费方**（CLI / 租户校验）用。两侧由 golden 判据逐字节对齐，
// 覆盖不到的关键字（pattern / minimum / maximum）如实返回 covered=false，
// 由调用方回落 Go 实现。
package native

/*
#include <stdlib.h>
#include "hermes_parser.h"
*/
import "C"

import (
	"errors"
	"unsafe"
)

// ErrSchemaUncovered 表示这份 schema 用了 C++ 版未覆盖的关键字。
var ErrSchemaUncovered = errors.New("schema(native): keyword is not covered by the C++ validator")

// SchemaValidateResult 是一次 C++ 校验的结果。
type SchemaValidateResult struct {
	// Violation 非空表示校验失败（"path: reason"，与 Go 版逐字节同形）。
	Violation string
	// Covered 为 false 表示该 schema 含未覆盖关键字，调用方应回落 Go 实现。
	Covered bool
}

// SchemaValidate 用 schemaJSON 校验 valueJSON（两者都是 JSON 文本）。
func SchemaValidate(schemaJSON, valueJSON []byte) (SchemaValidateResult, error) {
	if len(schemaJSON) == 0 {
		return SchemaValidateResult{}, errors.New("schema(native): empty schema")
	}
	errBuf := make([]byte, 4096)
	var errLen C.size_t
	rc := C.hm_schema_validate(
		(*C.char)(unsafe.Pointer(&schemaJSON[0])), C.size_t(len(schemaJSON)),
		(*C.char)(unsafe.Pointer(&valueJSON[0])), C.size_t(len(valueJSON)),
		(*C.char)(unsafe.Pointer(&errBuf[0])), C.size_t(len(errBuf)), &errLen,
	)
	msg := trimNUL(errBuf, int(errLen))
	switch int(rc) {
	case 0:
		return SchemaValidateResult{Covered: true}, nil
	case 1:
		return SchemaValidateResult{Covered: false}, ErrSchemaUncovered
	case 2:
		return SchemaValidateResult{Violation: msg, Covered: true}, nil
	case 3:
		return SchemaValidateResult{}, errors.New("schema(native): " + msg)
	default:
		return SchemaValidateResult{}, errors.New("schema(native): unexpected status")
	}
}
