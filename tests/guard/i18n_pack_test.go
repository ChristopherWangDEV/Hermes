package guard

// `i18n/hermes.rp` 必须**覆盖**源码里的每一个 `tr(en, zh)` 键。
//
// 语言包是**权威**：`tr()` 先查它，查不到才回退到调用点里的第二参数。
// 所以这里不再要求"译文和源码一致"（Lexii 校准后本来就会不一致），
// 只要求两件事：每个键都在包里；包里没有源码已经不用的键。
//
// 更新方式：`go run ./tools/geni18n cmd/hermes i18n/hermes.rp`（见 tools/geni18n）。

import (
	"encoding/json"
	"hermes/internal/i18npack"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

type rpDoc struct {
	Format  string `json:"format"`
	Entries []struct {
		Key          string            `json:"key"`
		Translations map[string]string `json:"translations"`
	} `json:"entries"`
}

// TestLanguagePackMatchesSource 守"语言包不过是期"。
func TestLanguagePackMatchesSource(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "i18n/hermes.rp"))
	if err != nil {
		t.Fatalf("读不到语言包：%v", err)
	}
	var doc rpDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("语言包不是合法 JSON：%v", err)
	}
	if doc.Format != "lexii.rp" {
		t.Fatalf("语言包格式应为 lexii.rp，实际 %q", doc.Format)
	}

	pack := map[string]string{}
	for _, e := range doc.Entries {
		pack[e.Key] = e.Translations["zh-Hans"]
	}

	src, err := i18npack.Extract(filepath.Join(root, "cmd/hermes"))
	if err != nil {
		t.Fatal(err)
	}

	var missing, stale []string
	for en, zh := range src {
		_ = zh
		if _, ok := pack[en]; !ok {
			missing = append(missing, en)
		}
	}
	for en := range pack {
		if _, ok := src[en]; !ok {
			stale = append(stale, en)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 {
		t.Fatalf("语言包缺少 %d 条（重新生成即可）：%s", len(missing), strings.Join(firstN(missing, 3), " | "))
	}
	if len(stale) > 0 {
		t.Fatalf("语言包里有 %d 条源码已不存在的键（重新生成即可）：%s", len(stale), strings.Join(firstN(stale, 3), " | "))
	}
}

func firstN(xs []string, n int) []string {
	if len(xs) <= n {
		return xs
	}
	return xs[:n]
}
