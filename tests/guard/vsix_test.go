// 本文件守 .vsix 的结构：它是**手拼的** zip，所以结构必须由守卫确认。
//
// 为什么值得测：`vsce` 会替我们保证这些不变量，而我们刻意不用它（要 npm）。
// 自己拼就必须自己验 —— 否则用户拿到一个"装不上"的包，而错误只会显示
// "扩展损坏"，看不出是哪一条不满足。
package guard_test

import (
	"archive/zip"
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVsixStructure 检查 vsix 的三样必备内容：清单、内容类型、扩展清单。
func TestVsixStructure(t *testing.T) {
	vsix := latestVsix(t)
	zr, err := zip.OpenReader(vsix)
	if err != nil {
		t.Fatalf("打不开 %s: %v", vsix, err)
	}
	defer zr.Close()

	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	for _, want := range []string{
		"extension.vsixmanifest",
		"[Content_Types].xml",
		"extension/package.json",
		"extension/src/extension.js",
		"extension/syntaxes/hermes.tmLanguage.json",
		"extension/language-configuration.json",
	} {
		if !names[want] {
			t.Fatalf("vsix 缺 %s", want)
		}
	}

	// 清单必须是合法 XML，且 Identity 的 Id/Version 与 package.json 一致 ——
	// 不一致的后果是"装上去显示旧版本"，而那种问题极难自查。
	man := readZipFile(t, zr, "extension.vsixmanifest")
	var m struct {
		Metadata struct {
			Identity struct {
				Id        string `xml:"Id,attr"`
				Version   string `xml:"Version,attr"`
				Publisher string `xml:"Publisher,attr"`
			} `xml:"Identity"`
		} `xml:"Metadata"`
	}
	if err := xml.Unmarshal(man, &m); err != nil {
		t.Fatalf("vsixmanifest 不是合法 XML: %v", err)
	}
	if m.Metadata.Identity.Id == "" || m.Metadata.Identity.Version == "" || m.Metadata.Identity.Publisher == "" {
		t.Fatalf("vsixmanifest 的 Identity 不完整: %+v", m.Metadata.Identity)
	}

	pkg := readZipFile(t, zr, "extension/package.json")
	var p struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Main    string `json:"main"`
	}
	if err := json.Unmarshal(pkg, &p); err != nil {
		t.Fatalf("package.json 不合法: %v", err)
	}
	if p.Name != m.Metadata.Identity.Id {
		t.Fatalf("package.name=%q 与 manifest Id=%q 不一致", p.Name, m.Metadata.Identity.Id)
	}
	if p.Version != m.Metadata.Identity.Version {
		t.Fatalf("package.version=%q 与 manifest Version=%q 不一致", p.Version, m.Metadata.Identity.Version)
	}
	if p.Main == "" || !names["extension/"+strings.TrimPrefix(p.Main, "./")] {
		t.Fatalf("package.main=%q 在包里不存在", p.Main)
	}

	// 语法文件必须是合法 JSON 且声明了 scopeName —— 高亮全靠它。
	var grammar struct {
		ScopeName string `json:"scopeName"`
	}
	if err := json.Unmarshal(readZipFile(t, zr, "extension/syntaxes/hermes.tmLanguage.json"), &grammar); err != nil {
		t.Fatalf("语法文件不是合法 JSON: %v", err)
	}
	if grammar.ScopeName != "source.hermes" {
		t.Fatalf("语法 scopeName 应为 source.hermes，实际 %q", grammar.ScopeName)
	}
}

func latestVsix(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	files, _ := filepath.Glob(filepath.Join(root, "build", "release", "hermes-vscode-*.vsix"))
	if len(files) == 0 {
		t.Skip("没有 .vsix（先跑 scripts/package_extension.sh）")
	}
	return files[len(files)-1]
}

func readZipFile(t *testing.T, zr *zip.ReadCloser, name string) []byte {
	t.Helper()
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		buf := make([]byte, f.UncompressedSize64)
		if _, err := rc.Read(buf); err != nil && err.Error() != "EOF" {
			t.Fatal(err)
		}
		return buf
	}
	t.Fatalf("包里没有 %s", name)
	return nil
}

var _ = os.Getenv
