// Package i18n 把语言包编译进二进制。
//
// 为什么要 embed 而不是运行时读文件：`hermes` 是**单个可执行文件**，
// 分发时不带额外资源目录。语言包是构建产物的一部分，不是运行时素材。
package i18n

import (
	_ "embed"
	"encoding/json"
)

//go:embed hermes.rp
var raw []byte

// Entry 是语言包里的一条。
type Entry struct {
	Key          string            `json:"key"`
	Source       string            `json:"source"`
	Translations map[string]string `json:"translations"`
	State        string            `json:"state"`
}

type doc struct {
	Format          string   `json:"format"`
	FormatVersion   int      `json:"formatVersion"`
	SourceLanguage  string   `json:"sourceLanguage"`
	TargetLanguages []string `json:"targetLanguages"`
	Entries         []Entry  `json:"entries"`
}

var (
	loaded  doc
	targets map[string]map[string]string // 语言 → (key → 译文)
)

func init() {
	if err := json.Unmarshal(raw, &loaded); err != nil {
		// 语言包是编译进二进制的：解析失败意味着构建产物坏了，
		// 那不该在运行时悄悄退化 —— 直接 panic 比"半英文半中文"好查。
		panic("i18n: language pack is not valid JSON: " + err.Error())
	}
	targets = make(map[string]map[string]string, len(loaded.TargetLanguages))
	for _, lang := range loaded.TargetLanguages {
		m := make(map[string]string, len(loaded.Entries))
		for _, e := range loaded.Entries {
			if v, ok := e.Translations[lang]; ok && v != "" {
				m[e.Key] = v
			}
		}
		targets[lang] = m
	}
}

// Lookup 按英文原文查译文。没有就返回 ok=false（调用方回退到英文）。
func Lookup(lang, en string) (string, bool) {
	m, ok := targets[lang]
	if !ok {
		return "", false
	}
	v, ok := m[en]
	return v, ok
}

// Count 返回条目数（doctor / i18n check 用）。
func Count() int { return len(loaded.Entries) }

// Languages 返回语言包里声明的目标语言。
func Languages() []string { return loaded.TargetLanguages }
