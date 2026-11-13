package main

import (
	"fmt"
	packi18n "hermes/i18n"
	"os"

	"hermes/internal/diag"
)

// tr 是 CLI 自己那段散文的翻译入口。
//
// 为什么 CLI 需要它，而 diag 不需要：`internal/diag` 的文案是**目录**
// （查表、可缺、回落到英文），而 CLI 的话是**写死在调用点**的。
// 两者混在一起会让"诊断说英文、命令行说中文"成为常态 —— 而这种不一致
// 比单一语言更让人困惑。
//
// 默认英文（DefaultLang），HERMES_LANG=zh* 时才用中文。空中文串表示
// "这句不译"——宁可英文也不留空。
// tr 返回当前语言的文案。
//
// 查表顺序：语言包 → 调用点的第二参数（兼容旧写法）→ 英文原文。
// 语言包排在前面，是为了让 **Lexii 成为中文的唯一事实源**：改包即生效，
// 不用动代码。第二参数留着是为了让旧调用点仍然能编，迁移完可以删。
func tr(en string) string {
	if diag.LangFromEnv() != diag.LangZH {
		return en
	}
	if v, ok := packi18n.Lookup("zh-Hans", en); ok {
		return v
	}
	// 缺译回退英文。**不静默**：HERMES_I18N_VERBOSE=1 时打一行，
	// 否则漏译永远发现不了（而它能一直"看起来正常"）。
	if os.Getenv("HERMES_I18N_VERBOSE") != "" {
		fmt.Fprintf(os.Stderr, "missing translation: %s\n", en)
	}
	return en
}

// pickUsage 选一段帮助文本。
//
// 帮助文本不能走 tr 的单句替换：它是一整块排版，逐句替换会拼出中英混杂的用法说明
// （示例行是英文、说明行是中文），那种东西比纯中文更难读。所以按**整块**选。
func pickUsage(en, zh string) string {
	if diag.LangFromEnv() == diag.LangZH && zh != "" {
		return zh
	}
	return en
}
