package ir

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"

	"hermes/internal/diag"
)

// Load 是 IR 的**唯一入口**：字节 → Doc + 诊断。
//
// 它比 `json.Unmarshal` 多干三件事，每一件都是 Unmarshal 做不到的：
//
//	① V-7 浮点扫描（**必须在 Unmarshal 之前**）
//	② 反序列化失败时给出一条能读的诊断，而不是把 *json.SyntaxError 抛给上层
//	③ 反序列化后跑 Validate 的三阶段
//
// 返回的 Doc 为 nil 表示**这份 IR 不可用**（有任何 error 级诊断）。
// 警告不阻塞：能用的 IR 带 warning 是常态。
func Load(data []byte, opts ...Option) (*Doc, []diag.Diag) {
	v := newValidator(opts)

	// ---- 阶段 0：V-7 浮点扫描 ----
	//
	// 为什么必须在 Unmarshal 之前：Doc 里有若干 json.RawMessage 字段
	// （economics、schemas[].json、lit 的 v）。Unmarshal 对它们**完全不做
	// 数字类型检查** —— 一个 1.5 会原样躺进去，然后随 IR 一起进哈希。
	// 而 IR.md P-1 禁浮点的理由正是"最短往返表示在 Go/Rust/C++ 之间有真实
	// 分歧"，它一旦进了哈希，跨语言校验就会随机失败、且只在某些机器上失败。
	//
	// 有浮点就**不再往下走**：后续所有结论都建立在一份本不该存在的 IR 上，
	// 再报十条也只是噪声。
	if hits := scanFloats(data); len(hits) > 0 {
		for _, h := range hits {
			v.add(diag.CodeFloat, diag.SeverityError,
				diag.Loc{Path: h.Path},
				map[string]string{"path": h.Path, "preview": h.Preview})
		}
		return nil, v.finish()
	}

	var d Doc
	if err := json.Unmarshal(data, &d); err != nil {
		v.add(diag.CodeLoadFailed, diag.SeverityError, diag.Loc{},
			map[string]string{"detail": err.Error()})
		return nil, v.finish()
	}

	v.run(&d)

	// 返回 Doc 而不是 nil —— 即使有 error 级诊断，调用方也常常想看看解析出了什么
	// （比如 `hermes ir --json` 的调试路径）。是否拒绝使用由调用方按诊断级别决定，
	// 而不是由 Load 替它决定。
	return &d, v.finish()
}

// floatHit 是一处浮点数字。
type floatHit struct {
	Path    string // "workflows[0].nodes[7].args.temperature"
	Preview string // 数字原文的前若干字符，让人一眼认出是哪个值
}

// scanFloats 遍历整棵 JSON，找出所有**带小数点或指数**的数字。
//
// double 位模式的 bits 是 **string**（"3ff0000000000000"），走 case string，
// 不会被这里命中 —— 这正是「常量用位模式、绝不放 JSON 数字」的原因：V-7、
// canonical 的 ErrFloat、签名链三件事都依赖"IR 字节里没有浮点数字"。
//
// 用 `UseNumber()` 而不是默认的 float64：默认解码会把 `1.5` 变成一个
// float64，此时"它原本是不是浮点"这个信息已经丢了（`1` 和 `1.0` 解出来
// 都是同一个 float64）。UseNumber 把原文保留成字符串，才能判断写法。
func scanFloats(data []byte) []floatHit {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		// 连 JSON 都不是：交给后面的 Unmarshal 报 E_IR_LOAD_FAILED，
		// 这里不抢它的活（否则同一份坏输入会有两条诊断）。
		return nil
	}

	var hits []floatHit
	var walk func(v any, path string)
	walk = func(v any, path string) {
		switch t := v.(type) {
		case json.Number:
			s := t.String()
			if strings.ContainsAny(s, ".eE") {
				prev := s
				if len(prev) > 24 {
					prev = prev[:24] + "…"
				}
				hits = append(hits, floatHit{Path: path, Preview: prev})
			}
		case map[string]any:
			// **键必须排序**：map 迭代顺序随机，不排序会让同一份坏 IR
			// 两次加载产出顺序不同的诊断 —— 那 golden 就不可比，
			// 而"诊断顺序稳定"是这个包对下游的承诺。
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				child := k
				if path != "" {
					child = path + "." + k
				}
				walk(t[k], child)
			}
		case []any:
			for i, vv := range t {
				walk(vv, pathf("%s[%d]", path, i))
			}
		}
	}
	walk(root, "")
	return hits
}
