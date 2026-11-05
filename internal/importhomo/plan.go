// Package importhomo 读 `homonoia compile --emit --json` 的产物，生成 `.hermes` 源码。
//
// 边界（D-023 路 A，**这是全项目唯一被允许接触 HomonoiaScript 的地方**）：
//
//   - 输入是 **JSON 计划**，不是 Homonoia 的部署源文件。那个语言的
//     词法/语法归 Homonoia，`tests/guard/homo_boundary_test.go` 盯着这条线。
//   - 输出是 **`.hermes` 源码**，不是旁路数据。因此导入产物天然是 `origin:"source"`，
//     人在 diff 里审过就是审过了，不需要第三套"导入来源"的元数据。
//   - **不解析 argv 语义**。只认 `--http` 与 `--model` 两节，其余一律丢弃并 warn。
//     认不透就少导几项，绝不"猜着导"：Homonoia 改一次 flag 名，猜的那版会静默导错，
//     而少导几项会表现为一条 warning。
//   - 两次导入同一 plan ⇒ 字节相等（无时间戳）。
//
// 这里刻意**不用** internal/diag：diag 是"编译/加载这份源码为什么被拒"，带源码位置；
// 导入失败是"命令行给的那个文件不对"，位置是文件名而不是行号。硬套 diag 会逼所有
// 诊断去编一个假的行号。
package importhomo

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Step 是 `homonoia compile --emit --json` 的一个元素。
//
// 字段形状来自 cli.py 的 `{"kind","argv","note","file"}`。可空字段用指针：
// `note` 与 `file` 都可能缺席，而"缺席"与"空串"是两件事。
type Step struct {
	Kind string   `json:"kind"`
	Argv []string `json:"argv"`
	Note *string  `json:"note"`
	File *string  `json:"file"`
}

// Parse 解析计划 JSON。
//
// 用 DisallowUnknownFields：这是同一台机器上两个工具之间的接口，不是要长期演进的
// 对外格式。上游加了字段而这边不认，静默忽略会让"导入少了一半"看起来像"上游没写"。
// 宁可报错让人来看一眼——这条判据与 LANGUAGE 的编译期从严是同一条。
func Parse(data []byte) ([]Step, error) {
	var steps []Step
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&steps); err != nil {
		return nil, fmt.Errorf("不是 `homonoia compile --emit --json` 的输出（应为 JSON 数组）: %w", err)
	}
	if len(steps) == 0 {
		return nil, fmt.Errorf("计划为空：数组里没有任何步骤，没有可导入的事实")
	}
	for i, s := range steps {
		if s.Kind == "" {
			return nil, fmt.Errorf("第 %d 个步骤没有 kind", i)
		}
		if len(s.Argv) == 0 || s.Argv[0] == "" {
			return nil, fmt.Errorf("第 %d 个步骤（%s）没有 argv[0]", i, s.Kind)
		}
	}
	return steps, nil
}
