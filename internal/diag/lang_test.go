package diag

import "testing"

// TestFromTag 守"默认英文、zh* 归中文"这条规则。
//
// 只认前缀是刻意的：诊断只有两种语言，多解析一层只会多一类
// "标签写错所以悄悄变英文"的困惑。
func TestFromTag(t *testing.T) {
	cases := map[string]Lang{
		"":          LangEN,
		"en":        LangEN,
		"en-US":     LangEN,
		"zh":        LangZH,
		"zh-CN":     LangZH,
		"zh-Hans":   LangZH,
		"ZH":        LangZH, // 大小写不敏感
		"fr":        LangEN, // 不支持的语言回落英文
		"  zh-CN  ": LangZH, // 容忍空白
	}
	for tag, want := range cases {
		if got := FromTag(tag); got != want {
			t.Errorf("FromTag(%q) = %v，期望 %v", tag, got, want)
		}
	}
}

// TestDefaultIsEnglish 钉住默认值：**英文**。
//
// 默认语言影响所有诊断的文案；它变了，所有 golden 文案类断言都该跟着变，
// 所以值得一条测试，而不是让它在某次重构里被悄悄改掉。
func TestDefaultIsEnglish(t *testing.T) {
	if DefaultLang != LangEN {
		t.Fatalf("默认诊断语言必须是英文，实际 %v", DefaultLang)
	}
}

// TestLangFromEnv 证明 HERMES_LANG 真的生效（而不是只写在注释里）。
func TestLangFromEnv(t *testing.T) {
	t.Setenv("HERMES_LANG", "zh-CN")
	if got := LangFromEnv(); got != LangZH {
		t.Fatalf("HERMES_LANG=zh-CN 应得到中文，实际 %v", got)
	}

	t.Setenv("HERMES_LANG", "")
	if got := LangFromEnv(); got != DefaultLang {
		t.Fatalf("空值应回落到默认语言，实际 %v", got)
	}

	t.Setenv("HERMES_LANG", "klingon")
	if got := LangFromEnv(); got != LangEN {
		t.Fatalf("认不出的语言应回落英文，实际 %v", got)
	}
}
