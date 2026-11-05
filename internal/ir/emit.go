package ir

import (
	"bytes"
	"encoding/json"
	"fmt"

	"hermes/internal/canon"
)

// Emit 把 Doc 写成 **canonical JSON** 字节（键按字节序、无多余空白、无浮点）。
//
// 为什么写出器在 Go 而不在 C++ 前端：canonical 序列化在本项目只有一个实现点
// （`internal/canon`），trace 的哈希链与 `.hbc` 的签名共用它。前端产出的是**节点图**
// （AST → Doc），把它落成字节是另一件事，放在 Go 里才不会出现第二份键序实现。
// 这也正是 Homonoia 互操作账本 `docs/hermes-interop.json` 里记的
// "ir_writer_in_hermes"。
//
// 实现方式刻意绕了一圈：先 `json.Marshal` 成普通 JSON，再用 `UseNumber` 解回
// `any`，交给 canon.FromAny 重排。绕这一圈换来两件事：
//
//  1. **不必给每个结构体写一遍编码逻辑** —— 那正是本项目反复消灭的"两份实现"；
//  2. **浮点在写出侧也会被挡住** —— canon.FromAny 见到 `1.5` 返回 ErrFloat，
//     而不是把它截断成整数。读侧（V-7）已经挡住了，写侧再挡一次不是冗余：
//     编译器自己产出的 IR 也要走同一条门（"自举信任从零开始"，IR.md §12 结尾）。
//
// 注意它**不做**校验：合法与否是 Load/Validate 的事。写出一份坏 IR 是调用方的
// bug，Emit 只保证"同样的 Doc ⇒ 同样的字节"。
// normalizeNodeNulls 把"缺省即 null"的节点字段显式写出来。
//
// `schema` 与 `attempt_ctx` 在 IR.md 里是 **required**（可以为 null，但必须出现）：
//   - schema: null 表示"无约束（返回 json）"，那是一个决定，不是省略；
//   - attempt_ctx: null 表示"不在 retry 里"，运行期靠它算 attempt=2/3。
//
// Go 侧用指针 + omitempty 表示可空，序列化时 nil 会被丢掉。在**写出这最后一层**
// 补回来，比给每个字段造一个"显式 nil"类型要省事，而且只有一个地方需要维护。
func normalizeNodeNulls(v any) {
	root, ok := v.(map[string]any)
	if !ok {
		return
	}
	flows, _ := root["workflows"].([]any)
	for _, f := range flows {
		wf, ok := f.(map[string]any)
		if !ok {
			continue
		}
		nodes, _ := wf["nodes"].([]any)
		for _, n := range nodes {
			nd, ok := n.(map[string]any)
			if !ok {
				continue
			}
			switch nd["k"] {
			case "call_model":
				if _, has := nd["schema"]; !has {
					nd["schema"] = nil
				}
				if _, has := nd["attempt_ctx"]; !has {
					nd["attempt_ctx"] = nil
				}
			case "call_tool":
				if _, has := nd["attempt_ctx"]; !has {
					nd["attempt_ctx"] = nil
				}
			}
		}
	}
}

func Emit(d *Doc) ([]byte, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("ir.Emit: 结构体无法序列化: %w", err)
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // 关键：默认解码会把 1 变成 float64，canon 会（正确地）拒绝它
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, fmt.Errorf("ir.Emit: 序列化结果不是合法 JSON: %w", err)
	}

	normalizeNodeNulls(generic)

	v, err := canon.FromAny(generic)
	if err != nil {
		return nil, fmt.Errorf("ir.Emit: %w", err)
	}
	return v.Bytes(), nil
}
