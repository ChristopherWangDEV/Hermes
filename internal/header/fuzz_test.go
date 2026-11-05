package header

import (
	"testing"

	"hermes/internal/ir"
)

// FuzzGenC 守 T46：任意 IR（哪怕是畸形的）都不能让头生成器崩或越界。
//
// 种子用真实的 golden；fuzz 会在此基础上变异出字段缺失、类型为空、
// 名字含奇怪字符等等。断言只看"不 panic"，因为生成器的输出对畸形输入
// 本来就是"要么能编，要么难看" —— 但**不能崩**：崩了就没法给用户任何诊断。
func FuzzGenC(f *testing.F) {
	for _, name := range []string{"ir-hello.json", "ir-support_bot.json", "ir-arrays.json"} {
		data, err := readGolden(name)
		if err != nil {
			continue
		}
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		doc, _ := ir.Load(data)
		if doc == nil {
			return
		}
		// 三种生成器都要跑：一个崩了，用户拿到的就是 panic 而不是诊断。
		_, _ = GenC(doc)
		_, _ = GenCPP(doc)
		_, _ = GenCShim(doc, data)
	})
}

func readGolden(name string) ([]byte, error) {
	return osReadFile("../../schemas/golden/" + name)
}
