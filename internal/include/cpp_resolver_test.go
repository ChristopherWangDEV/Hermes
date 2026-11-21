package include

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCppResolverMatchesGo 是 M3 的判据：C++ 侧的 include 图解析必须与 Go 的
// `include.Resolve` 给出**同一份模块序列**（顺序也相同——它决定类型 id 与产物字节）。
//
// 用真二进制驱动（`hermes-native deps --resolve --modules`），因为解析要读文件；
// 参照实现仍是 Go 的 Resolve，所以两侧真的互相独立。
func TestCppResolverMatchesGo(t *testing.T) {
	root := filepath.Join("..", "..")
	bin := filepath.Join(root, "build", "hermes-native")
	if _, err := os.Stat(bin); err != nil {
		t.Skip("没有 build/hermes-native（先跑 scripts/build.sh）")
	}

	// 样本：仓库里真实带 include 的工程 + 临时构造的边界情形。
	type sample struct {
		name    string
		files   map[string]string
		entry   string
		wantErr bool // 期望两侧都失败
	}
	samples := []sample{
		{
			name: "chain",
			files: map[string]string{
				"a.h": "#include \"b.h\"\nexport struct A { B b; }\n",
				"b.h": "export struct B { int x; }\n",
				"m.hermes": "#include \"a.h\"\nexport workflow w() entry -> string { return \"ok\"; }\n",
			},
			entry: "m.hermes",
		},
		{
			name: "diamond",
			files: map[string]string{
				"common.h": "export struct C { int v; }\n",
				"a.h":      "#include \"common.h\"\nexport struct A { C c; }\n",
				"b.h":      "#include \"common.h\"\nexport struct B { C c; }\n",
				"m.hermes": "#include \"a.h\"\n#include \"b.h\"\nexport workflow w() entry -> string { return \"ok\"; }\n",
			},
			entry: "m.hermes",
		},
		{
			name: "missing",
			files: map[string]string{
				"m.hermes": "#include \"nope.h\"\nexport workflow w() entry -> string { return \"ok\"; }\n",
			},
			entry:   "m.hermes",
			wantErr: true,
		},
	}

	for _, s := range samples {
		s := s
		t.Run(s.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range s.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			entryPath := filepath.Join(dir, s.entry)
			src, err := os.ReadFile(entryPath)
			if err != nil {
				t.Fatal(err)
			}
			res, ds := Resolve(entryPath, src, Options{})
			var goMods []string
			for _, m := range res.Modules {
				goMods = append(goMods, filepath.Base(m.Path))
			}
			goFailed := len(ds) > 0

			out, cmdErr := exec.Command(bin, "deps", "--resolve", "--modules", entryPath).CombinedOutput()
			var cppMods []string
			for _, line := range strings.Split(string(out), "\n") {
				if strings.HasPrefix(line, "module ") {
					cppMods = append(cppMods, filepath.Base(strings.TrimPrefix(line, "module ")))
				}
			}
			cppFailed := cmdErr != nil

			if goFailed != s.wantErr || cppFailed != s.wantErr {
				t.Fatalf("失败语义不一致：Go failed=%v C++ failed=%v 期望 %v\nC++ 输出:\n%s",
					goFailed, cppFailed, s.wantErr, out)
			}
			if s.wantErr {
				return // 两侧都失败即可；诊断文案的等价留给 M4
			}
			if strings.Join(goMods, ",") != strings.Join(cppMods, ",") {
				t.Fatalf("模块序列不同\n  Go =%v\n  C++=%v", goMods, cppMods)
			}
		})
	}
}
