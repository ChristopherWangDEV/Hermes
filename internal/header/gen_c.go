// Package header 从 IR 生成对外 C ABI 头（T47 / D-024）。
//
// 为什么是"从 IR 生成"而不是"给 HermesScript 的 .h 改名"：
// C 头的前提是"有符号地址"，而 HermesScript 的三样东西都没有 ——
// `string`/`cost` 不是 C 类型、`backend` 声明不是变量、**`workflow` 没有符号地址**
// （它是 IR 里的 DAG，由运行时解释执行）。所以 C 头不是"声明"，是**一层 shim 的声明**。
// 同一份 IR 可以生成多个头（C / C++），"共用同用"落在事实源，不落在文件。
package header

import (
	"fmt"
	"strings"

	"hermes/internal/ir"
)

// GenC 生成 `.h`（C89 + <stdint.h>）。
//
// 只 include `hermes/hermes_core.h` 一个自有头：核心类型手写冻结（见该文件注释），
// 生成的这层只放本 unit 特有的东西 —— 越往上越可以重新生成。
func GenC(doc *ir.Doc) ([]byte, error) {
	unit := Sanitize(doc.Unit.Name)
	var b strings.Builder

	fmt.Fprintf(&b, "/* 由 hermes header --c 生成，**不要手改**。源：%s */\n", sanitizePath(doc.Unit.EntryPath))
	b.WriteString("/* 方言：C89 + <stdint.h>（D-024）。多一个系统头都算违约。 */\n\n")

	guard := strings.ToUpper("HERMES_" + unit + "_H")
	fmt.Fprintf(&b, "#ifndef %s\n#define %s\n\n", guard, guard)
	b.WriteString("#include \"hermes/hermes_core.h\"\n\n")
	b.WriteString("#ifdef __cplusplus\nextern \"C\" {\n#endif\n\n")

	// 类型：一律**不透明指针**。展平只在 @frozen 时发生（D-024）——
	// HermesScript 的 struct 是用户随时会加字段的，展平 = 静默读错偏移。
	names := make([]string, 0, len(doc.Types))
	for i := range doc.Types {
		if doc.Types[i].Exported {
			names = append(names, doc.Types[i].Name)
		}
	}
	if len(names) > 0 {
		b.WriteString("/* 不透明类型：只有指针能过边界，字段通过 accessor 读。\n")
		b.WriteString(" * 展平会让\"用户加了一个字段\"变成\"客户读到错误偏移\"，而那是静默的。 */\n")
		for _, n := range names {
			fmt.Fprintf(&b, "typedef struct hermes_%s_%s hermes_%s_%s;\n",
				unit, Sanitize(n), unit, Sanitize(n))
		}
		b.WriteString("\n")
	}

	// workflow：每个导出 workflow 一个函数。返回值是状态码，结果走 out 参数。
	b.WriteString("/* 执行一个 workflow。input_json 是请求体（UTF-8），out_json 由本库分配，\n")
	b.WriteString(" * 调用方负责 hermes_buf_free。状态码非 0 时 out_json 不被赋值。 */\n")
	wfCount := 0
	for i := range doc.Workflows {
		w := &doc.Workflows[i]
		if !w.Exported {
			continue
		}
		wfCount++
		fmt.Fprintf(&b, "hermes_status hermes_%s_%s(const hermes_buf* input_json, hermes_buf* out_json);\n",
			unit, Sanitize(w.Name))
	}
	if wfCount == 0 {
		b.WriteString("/* 这个 unit 没有导出 workflow（纯类型库）。 */\n")
	}

	b.WriteString("\n#ifdef __cplusplus\n}\n#endif\n\n")
	fmt.Fprintf(&b, "#endif /* %s */\n", guard)
	return []byte(b.String()), nil
}

// Sanitize 把任意标识符收敛成 C 标识符（导出给 driver 用，避免第二个实现）。[a-z0-9_] 之外一律换成 '_'。
//
// 为什么不留原样：C 标识符与 HermesScript 的标识符**规则不同**（前者不允许
// 某些字符、且大小写敏感但实现可能不区分前 6 字符）。生成器若假设两者相同，
// 会在某个客户的名字上炸，而那时代码已经发出去了。
func Sanitize(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "_"
	}
	if out[0] >= '0' && out[0] <= '9' {
		return "_" + string(out)
	}
	return string(out)
}

// sanitizePath 只保留文件名，避免把构建机的绝对路径写进分发物
// —— 那会让"同一源码在两台机器上生成不同字节"。
func sanitizePath(p string) string {
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 {
		return p[i+1:]
	}
	return p
}
