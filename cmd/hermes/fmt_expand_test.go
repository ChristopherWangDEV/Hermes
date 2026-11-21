package main

import (
	"os"
	"regexp"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"hermes/internal/native"
)

// TestFmtExpandLowersEveryBindForm 是 `fmt --expand` 的判据：
// 三种形态都必须展开成**规范 #link**，且其余字节一个不动。
func TestFmtExpandLowersEveryBindForm(t *testing.T) {
	src := "#bind <std/net>;\n" +
		"// #bind in a comment must stay\n" +
		"#bind models\n" +
		"#bind native c \"libcrypto.a\" via \"crypto.h\"\n" +
		"export workflow w() entry -> string { return \"ok\"; }\n"
	want := "#link <std/net>\n" +
		"// #bind in a comment must stay\n" +
		"#link \"models\"\n" +
		"#link native \"libcrypto.a\" from \"crypto.h\"\n" +
		"export workflow w() entry -> string { return \"ok\"; }\n"
	got, err := native.FmtExpand([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("展开结果不符\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	// 注释里的 #bind 必须原样保留：展开是词法层判定，不是正则替换。
	if !strings.Contains(string(got), "// #bind in a comment must stay") {
		t.Fatal("注释里的 #bind 被误展开")
	}
}

// TestFmtExpandOutputCompilesIdentically 是"糖可逆"的端到端证据：
// 把展开结果写进文件，与原始糖写法编出的 IR **逐字节相同**。
func TestFmtExpandOutputCompilesIdentically(t *testing.T) {
	root := repoRootForBackend(t)
	bin := filepath.Join(root, "build", "hermes")
	dir := t.TempDir()

	sugar := "#bind <std/net>\nexport workflow w() entry -> string { return \"ok\"; }\n"
	plain := "#link <std/net>\nexport workflow w() entry -> string { return \"ok\"; }\n"

	sugarPath := filepath.Join(dir, "sugar.hermes")
	if err := os.WriteFile(sugarPath, []byte(sugar), 0o644); err != nil {
		t.Fatal(err)
	}
	expanded, err := native.FmtExpand([]byte(sugar))
	if err != nil {
		t.Fatal(err)
	}
	if string(expanded) != plain {
		t.Fatalf("展开结果应与手写等价形态一致：%q", expanded)
	}

	// 两次都用**同一个文件名**、放在不同子目录：IR 里的 entry_path / name /
	// sha256 都由文件名与内容决定，文件名不同会让比较失去意义（这是这条测试
	// 第一次跑就踩到的坑）。
	irOf := func(src, sub string) []byte {
		d := filepath.Join(dir, sub)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		name := "w.hermes"
		p := filepath.Join(d, name)
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(d, "out.json")
		cmd := exec.Command(bin, "ir", p, "-o", out)
		cmd.Dir = dir
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("hermes ir %s: %v\n%s", name, err, b)
		}
		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	a := irOf(sugar, "sugar")
	b := irOf(string(expanded), "expanded")
	// 唯一允许不同的是模块源码的 sha256：糖写法与展开写法的**源文件字节**
	// 本来就不一样，哈希覆盖源码。除此之外必须逐字节相同——这才是"糖可逆"。
	strip := func(ir []byte) string {
		return regexp.MustCompile(`"sha256":"[0-9a-f]{64}"`).ReplaceAllString(string(ir), `"sha256":"<SRC>"`)
	}
	if strip(a) != strip(b) {
		t.Fatalf("糖写法与展开后的 IR 不同\n--- 糖 ---\n%s\n--- 展开 ---\n%s", a, b)
	}
}
