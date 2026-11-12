// geni18n 从 cmd/hermes 的 tr() 调用点生成语言包。
//
// 用法：go run ./tools/geni18n cmd/hermes i18n/hermes.rp
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"hermes/internal/i18npack"
)

type entry struct {
	Key          string            `json:"key"`
	Source       string            `json:"source"`
	Translations map[string]string `json:"translations"`
	State        string            `json:"state"`
}

type doc struct {
	Format          string   `json:"format"`
	FormatVersion   int      `json:"formatVersion"`
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	SourceLanguage  string   `json:"sourceLanguage"`
	TargetLanguages []string `json:"targetLanguages"`
	Entries         []entry  `json:"entries"`
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: geni18n <source dir> <output .rp>")
		os.Exit(2)
	}
	pairs, err := i18npack.Extract(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	keys := make([]string, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// **先读旧包**：源码里已经没有中文了，重新生成只能得到英文键。
	// 不合并的话，一次 export 就会把所有译文抹成空 —— 那是灾难性的。
	old, oldState := loadExisting(os.Args[2])
	d := doc{
		Format: "lexii.rp", FormatVersion: 1,
		ID: "com.hermes.cli", Name: "Hermes CLI",
		SourceLanguage: "en", TargetLanguages: []string{"zh-Hans"},
	}
	for _, k := range keys {
		zh := pairs[k]
		state := "draft"
		// 旧包里已有译文就沿用（并保留它的校准状态）：
		if e, ok := old[k]; ok {
			if e != "" {
				zh = e
			}
			if st, ok := oldState[k]; ok && st != "" {
				state = st
			}
		}
		d.Entries = append(d.Entries, entry{
			Key: k, Source: k,
			Translations: map[string]string{"zh-Hans": zh},
			State:        state,
		})
	}
	blob, _ := json.MarshalIndent(d, "", "  ")
	if err := os.MkdirAll(filepath.Dir(os.Args[2]), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(os.Args[2], append(blob, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d entries)\n", os.Args[2], len(d.Entries))
}

// loadExisting 读旧包里的译文与状态：合并而不是覆盖。
func loadExisting(path string) (map[string]string, map[string]string) {
	tr := map[string]string{}
	st := map[string]string{}
	raw, err := os.ReadFile(path)
	if err != nil {
		return tr, st
	}
	var d doc
	if json.Unmarshal(raw, &d) != nil {
		return tr, st
	}
	for _, e := range d.Entries {
		tr[e.Key] = e.Translations["zh-Hans"]
		st[e.Key] = e.State
	}
	return tr, st
}
