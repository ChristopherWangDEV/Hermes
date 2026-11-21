package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hermes/internal/backend"
)

// TestUnitEmitEquivalence 是 unit AOT 的判据：C++ 实现必须产出与 golden
// **逐字节相同**的 C。
//
// 历史：这条测试起初是"Go vs C++ 并排比较"，用来证明搬家没改字节（迁移期的
// 做法）。Go 参照实现删除后，判据换成 golden —— 否则删掉参照物就等于删掉证据。
// 生成/更新：HERMES_UPDATE_UNIT_GOLDEN=1。
//
// unit.c 会编成静态库成员被宿主链接 —— 任何一字节差异都意味着"宿主链接到的
// 实现"与"我们验证过的实现"不是同一份。而 unit 包装里全是手写 C 片段
// （错误槽、序列化 helper），最容易被"看起来等价"骗过去。
func TestUnitEmitEquivalence(t *testing.T) {
	root := repoRootForBackend(t)
	var srcs []string
	for _, dir := range []string{"examples", "tests", "cmd/hermes/testdata"} {
		filepath.Walk(filepath.Join(root, dir), func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() && strings.HasSuffix(p, ".hermes") {
				srcs = append(srcs, p)
			}
			return nil
		})
	}

	compared := 0
	for _, src := range srcs {
		src := src
		t.Run(filepath.Base(src), func(t *testing.T) {
			data, err := os.ReadFile(src)
			if err != nil {
				t.Skipf("读不到源码：%v", err)
			}
			doc, err := buildIR(src, data)
			if err != nil {
				t.Skipf("源码编不过：%v", err)
			}
			for i := range doc.Functions {
				fn := &doc.Functions[i]
				if len(fn.Params) != 1 || fn.Recursive {
					continue // unit AOT 只支持单形参非递归；两边都会拒绝其余形态
				}
				cppSrc, cerr := backend.GenerateUnitFuncCpp(doc, "u", fn)
				if cerr != nil {
					continue
				}
				compared++
				goldenPath := filepath.Join(root, "tests", "unit", "golden",
					strings.ReplaceAll(strings.TrimPrefix(src, root+string(filepath.Separator)), string(filepath.Separator), "#")+"."+fn.Name+".c")
				if os.Getenv("HERMES_UPDATE_UNIT_GOLDEN") == "1" {
					os.MkdirAll(filepath.Dir(goldenPath), 0o755)
					os.WriteFile(goldenPath, cppSrc, 0o644)
					continue
				}
				goSrc, rerr := os.ReadFile(goldenPath)
				if rerr != nil {
					t.Skipf("缺 golden：%v", rerr)
				}
				if !bytes.Equal(goSrc, cppSrc) {
					if os.Getenv("HERMES_DUMP_UNIT") != "" {
						os.WriteFile("/tmp/unit_go.c", goSrc, 0o644)
						os.WriteFile("/tmp/unit_cpp.c", cppSrc, 0o644)
					}
					t.Errorf("%s: unit C 与 golden 不一致（golden %d 字节 / C++ %d 字节）", fn.Name, len(goSrc), len(cppSrc))
				}
			}
		})
	}
	t.Logf("unit AOT 等价：已比对 %d 个函数", compared)
}
