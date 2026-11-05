package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// goldenIR 是 T34 的第一份产物：`schemas/golden/ir-hello.json`。
//
// 它由这条命令生成，而不是手抄：
//
//	hermes ir cmd/hermes/testdata/hello.hermes -o schemas/golden/ir-hello.json
//
// 手抄的 golden 会把"我随手选的编号与顺序"固化成规格；由 codegen 产出的 golden
// 才记录"这条流水线现在给出什么"，于是它一变就说明流水线变了。
var goldenCases = []struct {
	name    string
	fixture string
	golden  string
}{
	{"hello", "testdata/hello.hermes", "../../schemas/golden/ir-hello.json"},
	{"parallel", "testdata/parallel.hermes", "../../schemas/golden/ir-parallel.json"},
	{"control", "testdata/control.hermes", "../../schemas/golden/ir-control.json"},
	{"typed", "testdata/typed.hermes", "../../schemas/golden/ir-typed.json"},
	{"model", "testdata/model.hermes", "../../schemas/golden/ir-model.json"},
	{"tool", "testdata/tool.hermes", "../../schemas/golden/ir-tool.json"},
	{"multi", "testdata/multi/app.hermes", "../../schemas/golden/ir-multi.json"},
	{"support_bot", "../../examples/support_bot/support.hermes", "../../schemas/golden/ir-support_bot.json"},
	{"try", "testdata/trycatch.hermes", "../../schemas/golden/ir-try.json"},
	{"retry", "testdata/retry.hermes", "../../schemas/golden/ir-retry.json"},
	{"live", "testdata/live.hermes", "../../schemas/golden/ir-live.json"},
	{"arrays", "testdata/arrays.hermes", "../../schemas/golden/ir-arrays.json"},
	{"allowstatus", "testdata/allowstatus.hermes", "../../schemas/golden/ir-allowstatus.json"},
	{"toolwire", "testdata/toolwire.hermes", "../../schemas/golden/ir-toolwire.json"},
	{"cast", "testdata/cast.hermes", "../../schemas/golden/ir-cast.json"},
	{"binop", "testdata/binop.hermes", "../../schemas/golden/ir-binop.json"},
	{"agent", "testdata/agent.hermes", "../../schemas/golden/ir-agent.json"},
	{"functions", "testdata/functions.hermes", "../../schemas/golden/ir-functions.json"},
}

// TestIROutputMatchesGolden 是 T34 验收 ⑨ 的最小版本：
// 同一份源码两次编译 ⇒ 与 golden **逐字节**相等。
func TestIROutputMatchesGolden(t *testing.T) {
	for _, c := range goldenCases {
		t.Run(c.name, func(t *testing.T) {
			want, err := os.ReadFile(c.golden)
			if err != nil {
				t.Fatalf("读不到 golden %s: %v（先跑 hermes ir 生成它）", c.golden, err)
			}

			out := filepath.Join(t.TempDir(), "out.ir.json")
			if code := run([]string{"ir", c.fixture, "-o", out}); code != 0 {
				t.Fatalf("hermes ir 退出码 %d", code)
			}

			got, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("IR 与 golden 不一致（%d vs %d 字节）\n--- got ---\n%s\n--- want ---\n%s",
					len(got), len(want), got, want)
			}
		})
	}
}

// TestIRIsDeterministic 连跑三次，证明"同源码 ⇒ 同字节"不是一次巧合。
//
// 单次比对只能证明"这次等于 golden"；三次才排掉"某处有 map 迭代序 / 时间戳"
// 这类概率性问题（Go 的 map 顺序随机化跑一次抓不到，本项目已经因此吃过一次）。
func TestIRIsDeterministic(t *testing.T) {
	var first []byte
	for i := 0; i < 3; i++ {
		out := filepath.Join(t.TempDir(), "a.json")
		if code := run([]string{"ir", goldenCases[1].fixture, "-o", out}); code != 0 {
			t.Fatalf("第 %d 次退出码非 0", i)
		}
		got, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = got
			continue
		}
		if !bytes.Equal(first, got) {
			t.Fatalf("第 %d 次的字节与第一次不同", i)
		}
	}
}

// TestIRRejectsUnsupportedConstruct 证明不支持的构造**报错**而不是产出半份 IR。
//
// 这条比"能编"更重要：一份缺了 return 的 IR 依然能通过形状校验，
// 而它到运行期才会表现为"workflow 什么都不做"。
func TestIRRejectsUnsupportedConstruct(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "bad.hermes")
	// 从数组里取字段：field 节点这一刀还不支持（需要类型表），必须报错而不是跳过。
	body := "export workflow f() entry -> string {\n" +
		"    let a = json.get(\"k\");\n" +
		"    return \"b\";\n}\n"
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"ir", src}); code != 4 {
		t.Fatalf("不支持的构造应退出 4，实际 %d", code)
	}
}
