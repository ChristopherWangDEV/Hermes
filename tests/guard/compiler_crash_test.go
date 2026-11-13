// 本文件守一条底线：**用户写错代码，编译器要给诊断，不能崩。**
//
// 起因：拼错 struct 字段名时 `hermes ir` 直接 SIGSEGV。根因是
// `Checker::list_fields_` 解引用了空的 `structs_[si].decl`（从外部头登记进来的
// struct 没有本地 AST）。这条路径只在报错时走到，所以平时跑得通，
// 一旦用户写错字段就崩 —— 而"写错"是每个人每天都会做的事。
package guard_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompilerReportsBadFieldInsteadOfCrashing(t *testing.T) {
	root := repoRoot(t)
	bin := filepath.Join(root, "build", "hermes")
	if _, err := os.Stat(bin); err != nil {
		t.Skip("没有 build/hermes（先跑 scripts/build.sh）")
	}

	cases := []struct{ name, src string }{
		{"本地 struct 拼错字段", `export struct H { string s; }
export workflow a(input: H) entry -> string {
    return input.nope;
}
`},
		{"tool.call 结果上拼错字段", `#link <std/net>;
export struct H { string s; }
export workflow a() entry -> string {
    auto g = tool.call<H>(url = "http://x/h", method = "GET");
    return g.body.s;
}
`},
	}

	dir := t.TempDir()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := filepath.Join(dir, "case.hermes")
			if err := os.WriteFile(src, []byte(c.src), 0o644); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command(bin, "ir", src, "-o", filepath.Join(dir, "out.json")).CombinedOutput()
			text := string(out)
			// 崩溃的特征：Go 的 SIGSEGV 报告。
			if strings.Contains(text, "SIGSEGV") || strings.Contains(text, "signal arrived during cgo") {
				t.Fatalf("编译器崩溃了，应该给诊断：\n%s", text)
			}
			if err == nil {
				t.Fatal("拼错字段却编译成功")
			}
			if !strings.Contains(text, "E4016") {
				t.Fatalf("应是 E4016（字段不存在），实际：\n%s", text)
			}
			// 诊断里要有"这个 struct 有哪些字段"——只说错了不说有啥，是最难用的报错。
			if !strings.Contains(text, "has:") {
				t.Fatalf("诊断没列出可用字段：\n%s", text)
			}
		})
	}
}

// TestTwoWorkflowsBothSurvive 守"一个 unit 里的每个 workflow 都进产物"。
//
// 历史：第二个 workflow 曾经被悄悄丢弃（构建还是绿的），后来改成报 E3101，
// 现在真正支持。这条测试盯的是**最终语义**：两个都在，且各自可调用。
func TestTwoWorkflowsBothSurvive(t *testing.T) {
	root := repoRoot(t)
	bin := filepath.Join(root, "build", "hermes")
	if _, err := os.Stat(bin); err != nil {
		t.Skip("没有 build/hermes")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "two.hermes")
	if err := os.WriteFile(src, []byte(`export workflow a() entry -> string { return "a"; }
export workflow b() -> string { return "b"; }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(bin, "ir", src, "-o", filepath.Join(dir, "out.json")).CombinedOutput()
	if err != nil {
		t.Fatalf("两个 workflow 应该都能编：%v\n%s", err, out)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "out.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Workflows []struct {
			Name string `json:"name"`
		} `json:"workflows"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Workflows) != 2 {
		t.Fatalf("应有两个 workflow，实际 %d：%+v", len(doc.Workflows), doc.Workflows)
	}
	names := []string{doc.Workflows[0].Name, doc.Workflows[1].Name}
	if names[0] != "a" || names[1] != "b" {
		t.Fatalf("名字不对：%v", names)
	}
}
