// host_diag.go —— AOT 运行期路径上的诊断文案（宿主常量）。
//
// 为什么这里要**手工**放一份文案，而不是调 C++ 渲染：
//
//	`cmd/hermesrt` 把 internal/ir 编进 libhermesrt.a，而 `hermes cc` 只链
//	那一个归档，C++ 符号到不了。所以 internal/ir **必须永久 cgo-free**
//	（见 cmd/hermesrt/main.go 的同段注释）。ir.Load 又是运行期的热路径 ——
//	它不能 import internal/native。
//
// 因此这一条诊断（E_IR_LOAD_FAILED，仅在"IR 字节根本不是合法 JSON"时出现）
// 用 Go 常量 + 原骨架渲染。这不是"豁免 C++ 渲染"，是**显式的手工构造**：
// 只有这一条，且有漂移守卫盯着它与 C++ catalog 的字节是否一致。
//
// 漂移守卫：cmd/hermes/host_diag_test.go（比对 native/cpp/diag_catalog.cpp
// 里该码的 en/zh 文案）。注意它防的是"副本漂移"，不是渲染权证明。
package ir

import (
	"strings"

	"hermes/internal/diag"
)

// hostDiagText 是 E_IR_LOAD_FAILED 的宿主常量形状（en/zh 各一份）。
type hostDiagText struct {
	Text string
	Why  string
	Fix  []string
}

var hostLoadFailedEN = hostDiagText{
	Text: "the IR bytes are not well-formed JSON for this schema: {detail}",
	Why: "The loader decodes into a fixed shape. When a field has the wrong JSON type " +
		"(an object where an integer was expected, a string where a list was expected), decoding stops at the first mismatch " +
		"and nothing after it is checked — so this is the only diagnostic you get until it is fixed.",
	Fix: []string{
		"fix the reported field; the surrounding IR is probably fine",
		"or regenerate the artifact if it was hand-edited",
	},
}

var hostLoadFailedZH = hostDiagText{
	Text: "IR 字节不是本 schema 期望的合法 JSON：{detail}",
	Why: "加载器按固定形状解码。某个字段的 JSON 类型不对时（该是整数的地方给了对象、该是列表的地方给了字符串），" +
		"解码会在第一处不匹配处停下，之后的内容一概不检查 —— 所以在修好它之前你只会看到这一条。",
	Fix: []string{
		"修报出来的那个字段；周围的内容大概率是好的",
		"如果这份产物是手改的，重新生成一份",
	},
}

// loadFailedDiag 构造 E_IR_LOAD_FAILED（不碰 catalog，也不碰 cgo）。
//
// RenderedText 刻意留空：Render() 会走 Go 骨架，字段填齐即完整 why/help，
// 与 C++ 渲染逐字节相同（由 cmd/hermes/host_diag_test.go 钉住）。
func loadFailedDiag(detail string) diag.Diag {
	lang := diag.LangFromEnv()
	tmpl := hostLoadFailedEN
	if lang == diag.LangZH {
		tmpl = hostLoadFailedZH
	}
	return diag.Diag{
		Severity: diag.SeverityError,
		Code:     diag.CodeLoadFailed,
		Message:  strings.Replace(tmpl.Text, "{detail}", detail, 1),
		Why:      tmpl.Why,
		Fix:      append([]string(nil), tmpl.Fix...),
	}
}
