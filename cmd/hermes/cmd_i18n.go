package main

// `hermes i18n`：语言包的导出、导入与检查。
//
// 语言包是**权威**（`tr()` 先查它），所以它必须能被验证：
//   check   —— 键覆盖与幽灵键，CI 用
//   export  —— 从源码重新生成（键的来源永远是源码）
//   import  —— 读一份 Lexii 校准过的包，校验后覆盖仓库里的那一份

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"hermes/internal/i18npack"
)

const defaultPackPath = "i18n/hermes.rp"

type packEntry struct {
	Key          string            `json:"key"`
	Source       string            `json:"source"`
	Translations map[string]string `json:"translations"`
	State        string            `json:"state"`
}

type packDoc struct {
	Format          string      `json:"format"`
	FormatVersion   int         `json:"formatVersion"`
	ID              string      `json:"id"`
	Name            string      `json:"name"`
	SourceLanguage  string      `json:"sourceLanguage"`
	TargetLanguages []string    `json:"targetLanguages"`
	Entries         []packEntry `json:"entries"`
}

func cmdI18n(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, i18nUsage)
		return 2
	}
	switch args[0] {
	case "check":
		return i18nCheck(args[1:])
	case "export":
		return i18nExport(args[1:])
	case "import":
		return i18nImport(args[1:])
	case "-h", "--help", "help":
		fmt.Print(i18nUsage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "hermes i18n: %s %q\n\n%s", tr("unknown subcommand"), args[0], i18nUsage)
		return 2
	}
}

// i18nCheck 验证语言包与源码一致：不缺口、不幽灵、不漏译。
func i18nCheck(args []string) int {
	path := defaultPackPath
	if len(args) > 0 {
		path = args[0]
	}
	src, err := i18npack.Extract("cmd/hermes")
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermes i18n check: %v\n", err)
		return 4
	}
	doc, err := readPack(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermes i18n check: %v\n", err)
		return 4
	}
	have := map[string]bool{}
	empty := 0
	for _, e := range doc.Entries {
		have[e.Key] = true
		for _, lang := range doc.TargetLanguages {
			if e.Translations[lang] == "" {
				fmt.Fprintf(os.Stderr, "hermes i18n check: "+tr("%s: %q is empty in %s\n"), path, e.Key, lang)
				empty++
			}
		}
	}
	missing, ghost := 0, 0
	for k := range src {
		if !have[k] {
			fmt.Fprintf(os.Stderr, "hermes i18n check: "+tr("%s: missing key %q\n"), path, k)
			missing++
		}
	}
	for _, e := range doc.Entries {
		if _, ok := src[e.Key]; !ok {
			fmt.Fprintf(os.Stderr, "hermes i18n check: "+tr("%s: ghost key %q (no longer in source)\n"), path, e.Key)
			ghost++
		}
	}
	if missing+ghost+empty > 0 {
		fmt.Fprintf(os.Stderr, "hermes i18n check: "+tr("%d missing, %d ghost, %d empty - run `hermes i18n export` to regenerate\n"), missing, ghost, empty)
		return 4
	}
	fmt.Printf("i18n: %d keys, %d languages, ok\n", len(doc.Entries), len(doc.TargetLanguages))
	return 0
}

// i18nExport 从源码重新生成语言包。
func i18nExport(args []string) int {
	path := defaultPackPath
	for i := 0; i < len(args); i++ {
		if args[i] == "-o" && i+1 < len(args) {
			path = args[i+1]
			i++
		}
	}
	pairs, err := i18npack.Extract("cmd/hermes")
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermes i18n export: %v\n", err)
		return 4
	}
	keys := make([]string, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	doc := packDoc{
		Format: "lexii.rp", FormatVersion: 1,
		ID: "com.hermes.cli", Name: "Hermes CLI",
		SourceLanguage: "en", TargetLanguages: []string{"zh-Hans"},
	}
	for _, k := range keys {
		doc.Entries = append(doc.Entries, packEntry{
			Key: k, Source: k,
			Translations: map[string]string{"zh-Hans": pairs[k]},
			State:        "draft",
		})
	}
	if err := writePack(path, doc); err != nil {
		fmt.Fprintf(os.Stderr, "hermes i18n export: %v\n", err)
		return 4
	}
	fmt.Printf("wrote %s (%d entries)\n", path, len(doc.Entries))
	return 0
}

// i18nImport 读一份（Lexii 校准过的）语言包，校验后覆盖仓库里的那一份。
func i18nImport(args []string) int {
	if len(args) != 1 {
		fmt.Fprint(os.Stderr, i18nUsage)
		return 2
	}
	doc, err := readPack(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermes i18n import: %v\n", err)
		return 4
	}
	// 导入前必须自洽：否则会把一份坏的包覆盖到仓库里，而构建照常通过。
	src, err := i18npack.Extract("cmd/hermes")
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermes i18n import: %v\n", err)
		return 4
	}
	for k := range src {
		found := false
		for _, e := range doc.Entries {
			if e.Key == k {
				found = true
				break
			}
		}
		if !found {
			fmt.Fprintf(os.Stderr, "hermes i18n import: "+tr("pack is missing key %q; refusing to import\n"), k)
			return 4
		}
	}
	if err := writePack(defaultPackPath, doc); err != nil {
		fmt.Fprintf(os.Stderr, "hermes i18n import: %v\n", err)
		return 4
	}
	fmt.Printf("imported %s → %s (%d entries)\n", args[0], defaultPackPath, len(doc.Entries))
	return 0
}

func readPack(path string) (packDoc, error) {
	var doc packDoc
	raw, err := os.ReadFile(path)
	if err != nil {
		return doc, err
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return doc, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	if doc.Format != "lexii.rp" {
		return doc, fmt.Errorf("%s: format is %q, expected lexii.rp", path, doc.Format)
	}
	if len(doc.TargetLanguages) == 0 {
		return doc, fmt.Errorf("%s does not declare targetLanguages", path)
	}
	return doc, nil
}

func writePack(path string, doc packDoc) error {
	blob, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(blob, '\n'), 0o644)
}

const i18nUsage = `hermes i18n - manage the language pack

Usage:
  hermes i18n check [file]     verify coverage, ghosts, and empty translations
  hermes i18n export [-o f]    regenerate the pack from tr() call sites
  hermes i18n import <file>    install a reviewed pack (validated first)

The pack ships inside the binary, so the installed hermes always matches it.
`
