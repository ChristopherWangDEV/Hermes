package header

import (
	"fmt"
	"strings"

	"hermes/internal/ir"
)

// GenCShim 生成 **unit 的 C 实现层**：把 `hermes_<unit>_<workflow>` 这几个符号
// 实现成对运行时入口 `hermes_rt_run` 的调用。
//
// 为什么必须有这一层：头里承诺的是 per-unit 的符号（`hermes_support_support_bot`），
// 而运行时库只有一个通用入口（`hermes_rt_run`）。没有 shim，用户程序能编过、
// 却**链不上** —— 报错是 "undefined symbol"，离"缺一层生成"这个真实原因很远。
//
// 这一层由编译器生成、而不是让用户手写，理由和 C 头一样：它是 IR 的函数，
// 换了 unit 就要变，而手写的那份一定会和 IR 漂移。
func GenCShim(doc *ir.Doc, irBytes []byte) ([]byte, error) {
	unit := Sanitize(doc.Unit.Name)
	var b strings.Builder

	b.WriteString("/* 由 hermes cc 生成，**不要手改**：unit 的 C ABI 实现层。 */\n")
	b.WriteString("/* 它把 per-unit 符号接到运行时入口 hermes_rt_run 上。 */\n\n")
	b.WriteString("#include <stdlib.h>\n#include <string.h>\n\n")
	// 核心类型（hermes_status / hermes_buf）在 hermes_core.h；只 include 运行时头
	// 会得到 "unknown type name hermes_status" —— 而那个错误看起来像头生成坏了。
	b.WriteString("#include \"hermes/hermes_core.h\"\n")
	b.WriteString("#include \"hermes_rt.h\"\n\n")

	b.WriteString("static const unsigned char hermes_unit_ir[] = {\n")
	for i := 0; i < len(irBytes); i += 16 {
		end := i + 16
		if end > len(irBytes) {
			end = len(irBytes)
		}
		b.WriteString("  ")
		for _, c := range irBytes[i:end] {
			fmt.Fprintf(&b, "0x%02x,", c)
		}
		b.WriteString("\n")
	}
	b.WriteString("  0x00\n};\n\n")
	fmt.Fprintf(&b, "#define HERMES_UNIT_IR_LEN ((size_t)%d)\n\n", len(irBytes))

	wfCount := 0
	for i := range doc.Workflows {
		w := &doc.Workflows[i]
		if !w.Exported {
			continue
		}
		wfCount++
		fmt.Fprintf(&b, "hermes_status hermes_%s_%s(const hermes_buf* input_json, hermes_buf* out_json) {\n",
			unit, Sanitize(w.Name))
		b.WriteString("  char* out = NULL;\n  char* err = NULL;\n  int rc;\n")
		b.WriteString("  if (!out_json) { return HERMES_ERR_INVALID_ARG; }\n")
		b.WriteString("  out_json->ptr = NULL;\n  out_json->len = 0;\n")
		b.WriteString("  rc = hermes_rt_run((char*)hermes_unit_ir, HERMES_UNIT_IR_LEN,\n")
		b.WriteString("                     input_json ? input_json->ptr : NULL,\n")
		b.WriteString("                     input_json ? input_json->len : 0,\n")
		b.WriteString("                     &out, &err);\n")
		b.WriteString("  if (rc != 0) {\n")
		b.WriteString("    /* 错误文本交给 hermes_last_error 那条路径；这里只释放并返回码。 */\n")
		b.WriteString("    free(err);\n")
		b.WriteString("    return HERMES_ERR_INTERNAL;\n")
		b.WriteString("  }\n")
		// 用 strlen 取长度：hermes_rt_run 产出的是 C 字符串。
		// 含 NUL 的返回值会被截断 —— 这条路留给以后的 hermes_buf 直出。
		b.WriteString("  out_json->ptr = out;\n")
		b.WriteString("  out_json->len = out ? strlen(out) : 0;\n")
		b.WriteString("  return HERMES_OK;\n")
		b.WriteString("}\n\n")
	}
	if wfCount == 0 {
		b.WriteString("/* 这个 unit 没有导出 workflow。 */\n")
	}
	return []byte(b.String()), nil
}
