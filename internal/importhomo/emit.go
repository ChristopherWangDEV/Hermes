package importhomo

import (
	"fmt"
	"sort"
	"strings"
)

// Result 是一次导入的产物。
type Result struct {
	// Source 是生成的 `.hermes` 源码。同一份 plan 两次导入必须逐字节相等。
	Source []byte
	// Warnings 是"看懂了但没导"的东西。**不是失败**：认不透就少导几项，
	// 让人在自己的 diff 里看到少了什么，比猜着导对得多。
	Warnings []string
}

// recognized 是导入器唯一理解的 argv 节（T43 ⑦）。
//
// 只有两个：
//   - `--http`  网关的监听地址 → 变成 backend 的字面地址
//   - `--model` 模型清单 → 只作注释（运行期以 /v1/models 为准）
//
// 其余一切 flag 都不碰。这条清单短到可以一眼看完，是刻意的：**任何需要解释
// "这个 flag 是什么意思"的导入，都是在 Hermes 里重建 Homonoia 的 CLI 语义**，
// 而 D-023 的三条理由里有两条正是这么死的。
var recognized = map[string]bool{
	"--http":  true,
	"--model": true,
}

// Emit 把计划转成 `.hermes` 源码。
//
// 输出里只有两类东西：**源码**（backend 声明 + 一个占位 workflow）与**注释**
// （步骤清单、模型清单、未识别的参数）。没有第三条：导入器不是编译器，
// 它不产出"半成品 IR"，也不产出旁路 JSON。
func Emit(steps []Step) Result {
	var warnings []string
	warned := map[string]bool{}

	var httpAddr string
	var httpRaw string
	var models []string
	var defaultModel string

	for _, s := range steps {
		for i := 0; i < len(s.Argv); i++ {
			arg := s.Argv[i]
			if !strings.HasPrefix(arg, "--") {
				continue
			}
			// 值取下一个参数；以 `--` 开头说明没有值（或下一个 flag），不猜。
			var val string
			hasVal := i+1 < len(s.Argv) && !strings.HasPrefix(s.Argv[i+1], "--")
			if hasVal {
				val = s.Argv[i+1]
			}
			if !recognized[arg] {
				key := arg + "|" + s.Kind
				if !warned[key] {
					warned[key] = true
					warnings = append(warnings, fmt.Sprintf(
						"丢弃未识别参数 %s（来自 %s）：导入器只认 --http 与 --model，"+
							"其余参数的含义归 Homonoia，这里不重建它", arg, s.Kind))
				}
				continue
			}
			switch arg {
			case "--http":
				if hasVal && httpAddr == "" {
					httpRaw = val
					// `--http` 给的是 host:port，而 backend 的地址是完整 URL。
					// 补 scheme 只在缺的时候做：已经带 http:// / https:// 的照抄，
					// 免得把 `https://x` 变成 `http://https://x`。
					if !strings.Contains(val, "://") {
						val = "http://" + val
					}
					httpAddr = val
				}
			case "--model":
				if hasVal {
					models = append(models, val)
					if id, _, ok := strings.Cut(val, "="); ok && defaultModel == "" {
						defaultModel = id
					}
				}
			}
			if hasVal {
				i++ // 跳过值，避免把 `--model` 的路径当成另一个 flag
			}
		}
	}

	sort.Strings(warnings) // 顺序稳定：两次导入的 warning 也要逐字节可比

	var b strings.Builder

	b.WriteString("// 由 `hermes import --from-plan` 生成。输入是一份 Homonoia 部署计划。\n")
	b.WriteString("//\n")
	b.WriteString("// 这份文件里只有**部署事实**，没有业务语义：一份部署脚本描述「起什么」\n")
	b.WriteString("// （部署与算力），一段 workflow 描述「做什么」（编排）。后者必须由人来写\n")
	b.WriteString("// —— 导入器不替你发明它（D-023 路 A），所以下面只有一个占位 workflow。\n")
	b.WriteString("//\n")
	b.WriteString("// 计划里的步骤（按顺序，仅供人对照）：\n")
	for i, s := range steps {
		note := ""
		if s.Note != nil {
			note = oneLine(*s.Note)
		}
		if note == "" {
			fmt.Fprintf(&b, "//   %d. %s\n", i+1, s.Kind)
		} else {
			fmt.Fprintf(&b, "//   %d. %s — %s\n", i+1, s.Kind, note)
		}
	}

	if len(models) > 0 {
		b.WriteString("//\n")
		b.WriteString("// 导入时看到的模型。**运行期一律以 /v1/models 为准**，这里只是当时的意图：\n")
		for _, m := range models {
			// `id=path` 拆开写，纯粹为了可读：`chat = /tmp/x.gguf` 比
			// `chat=/tmp/x.gguf` 在 diff 里好认。拆不动的原样抄。
			if id, path, ok := strings.Cut(m, "="); ok {
				fmt.Fprintf(&b, "//   %s = %s\n", oneLine(id), oneLine(path))
			} else {
				fmt.Fprintf(&b, "//   %s\n", oneLine(m))
			}
		}
		if defaultModel != "" {
			fmt.Fprintf(&b, "//   默认模型（导入时）：%s\n", oneLine(defaultModel))
		}
	}

	b.WriteString("\n")

	if httpAddr != "" {
		fmt.Fprintf(&b, "// 网关地址来自计划里的 `--http %s`。写死字面量是**导入的语义**，\n", oneLine(httpRaw))
		b.WriteString("// 不是推荐做法：发行版应该改用 env(...) 让地址由运行环境决定。\n")
		fmt.Fprintf(&b, "backend homonoia = \"%s\";\n\n", escapeStringLit(httpAddr))
	}

	b.WriteString("workflow imported")
	if httpAddr != "" {
		b.WriteString("(backend = homonoia)")
	} else {
		b.WriteString("()")
	}
	b.WriteString(" entry -> string {\n")
	b.WriteString("    // 占位体：让文件能被前端接受（零个 entry 会被 E3021 拒），不含业务含义。\n")
	b.WriteString("    // 写真正的编排时，用 model.generate / tool.call；记得 #link 对应的 std 模块。\n")
	b.WriteString("    return \"TODO: describe what this workflow should do\";\n")
	b.WriteString("}\n")

	return Result{Source: []byte(b.String()), Warnings: warnings}
}

// oneLine 把多行文本压成一行：注释里出现换行会把后面的内容踢出注释，
// 变成一行源码——那是能把导入产物变成非法文件的最简单方式。
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.TrimSpace(s)
}

// escapeStringLit 让地址安全地落进双引号字符串字面量。
func escapeStringLit(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}
