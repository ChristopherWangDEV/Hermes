// Package trace 是执行追踪：span 树 + canonical 落盘 + 哈希链。
//
// 它存在的全部理由是回答一个问题：**业务挂了，是模型胡说还是外部 API 变了？**
// 因此三件事缺一不可：
//   - span 与 IR 同构（每个 step 一个 span），才能从 trace 反推"哪一步"；
//   - 归因来自 errs 的闭集（不是自由文本），才能机器统计；
//   - 哈希链，才能事后证明"这份 trace 没被改过"。
package trace

import (
	"encoding/json"
	"errors"
	"fmt"

	"hermes/internal/canon"
)

// Span 是一次执行的最小单元。全部字段都是**整数或字符串**——
// 时间用纳秒整数而不是 time.Time：canonical JSON 里出现浮点或时区，
// 哈希就会在不同机器上漂。
type Span struct {
	ID        string            `json:"span_id"`
	Parent    string            `json:"parent_id"`
	StepID    string            `json:"step_id"` // 必须能在源文件里 grep 到（IR 的变量名规则）
	Kind      string            `json:"kind"`    // workflow|step|provider|tool|admission|budget|event
	Name      string            `json:"name"`
	Status    string            `json:"status"` // ok|failed|refused|truncated
	Attr      string            `json:"attribution"`
	StartNS   int64             `json:"start_ns"`
	DurNS     int64             `json:"dur_ns"`
	TokensIn  int64             `json:"tokens_in"`
	TokensOut int64             `json:"tokens_out"`
	MicroUSD  int64             `json:"micro_usd"`
	Detail    map[string]string `json:"detail,omitempty"`
}

// toCanon 把 span 编成 canonical 值。字段顺序由 canon 的键序规则决定，
// 不由结构体定义顺序决定 —— 后者一改就会让所有历史 trace 的哈希失效。
func (s Span) toCanon() canon.Value {
	detail := canon.Null()
	if len(s.Detail) > 0 {
		kv := make([]any, 0, len(s.Detail)*2)
		for k, v := range s.Detail {
			kv = append(kv, k, canon.Str(v))
		}
		detail = canon.Obj(kv...)
	}
	return canon.Obj(
		"attr", canon.Str(s.Attr),
		"detail", detail,
		"dur_ns", canon.Int(s.DurNS),
		"kind", canon.Str(s.Kind),
		"micro_usd", canon.Int(s.MicroUSD),
		"name", canon.Str(s.Name),
		"parent_id", canon.Str(s.Parent),
		"span_id", canon.Str(s.ID),
		"start_ns", canon.Int(s.StartNS),
		"status", canon.Str(s.Status),
		"step_id", canon.Str(s.StepID),
		"tokens_in", canon.Int(s.TokensIn),
		"tokens_out", canon.Int(s.TokensOut),
	)
}

// Trace 是一整份执行记录。
type Trace struct {
	RunID string
	Spans []Span
}

// Validate 检查树的三条硬性质：id 唯一、父存在、无环。
// 孤儿与环都不是"数据不好看"，而是**归因会指错地方**——那比没有 trace 更糟。
func (t *Trace) Validate() error {
	byID := make(map[string]int, len(t.Spans))
	for i, s := range t.Spans {
		if s.ID == "" {
			return fmt.Errorf("trace: span[%d] is missing span_id", i)
		}
		if _, dup := byID[s.ID]; dup {
			return fmt.Errorf("trace: duplicate span_id %q", s.ID)
		}
		byID[s.ID] = i
	}
	roots := 0
	for _, s := range t.Spans {
		if s.Parent == "" {
			roots++
			continue
		}
		if _, ok := byID[s.Parent]; !ok {
			return fmt.Errorf("trace: span %q has parent %q that does not exist (orphan)", s.ID, s.Parent)
		}
	}
	if roots == 0 && len(t.Spans) > 0 {
		return fmt.Errorf("trace: no root span (every span has a parent, so there must be a cycle)")
	}
	// 环检测：沿父链上溯，步数不超过 span 数，否则必环。
	for _, s := range t.Spans {
		cur, steps := s, 0
		for cur.Parent != "" {
			steps++
			if steps > len(t.Spans) {
				return fmt.Errorf("trace: parent chain of span %q contains a cycle", s.ID)
			}
			i, ok := byID[cur.Parent]
			if !ok {
				return fmt.Errorf("trace: parent chain of span %q is broken", s.ID)
			}
			cur = t.Spans[i]
		}
	}
	return nil
}

// Diff 比较两份 trace：按 step_id 对齐，报出新增、消失、状态变化的 span。
//
// 为什么按 step_id 而不是按 span_id：span_id 里带 run_id 与行号，两次运行的
// span_id 天然不同，按它对齐等于每次都"全变了"。而 step_id 是源文件里能 grep 到的
// 位置标识（"这一步"），它跨运行稳定 —— 那才是人想比较的东西。
type SpanChange struct {
	StepID string
	Want   string // 左侧状态
	Got    string // 右侧状态
}

// DiffResult 是两次运行的结构差异。
type DiffResult struct {
	OnlyLeft  []string // 只在左侧出现的 step_id
	OnlyRight []string // 只在右侧出现的 step_id
	Changed   []SpanChange
	// 汇总差（右侧 − 左侧）：负数表示右侧更省。
	TokenDelta  int64
	CostDelta   int64
	FailedLeft  int
	FailedRight int
}

// Diff 按 step_id 对齐两份 trace。
func Diff(left, right []Span) DiffResult {
	var out DiffResult
	byStep := func(spans []Span) map[string]Span {
		m := make(map[string]Span, len(spans))
		for _, s := range spans {
			// 同一个 step 可能有多条 span（重试）；保留**第一条失败的**，
			// 否则重试成功后的 ok 会盖掉失败信息，而失败才是要看的。
			if prev, ok := m[s.StepID]; ok {
				if prev.Status == "failed" || s.Status != "failed" {
					continue
				}
			}
			m[s.StepID] = s
		}
		return m
	}
	l, r := byStep(left), byStep(right)

	for id, ls := range l {
		if rs, ok := r[id]; ok {
			if ls.Status != rs.Status {
				out.Changed = append(out.Changed, SpanChange{StepID: id, Want: ls.Status, Got: rs.Status})
			}
		} else {
			out.OnlyLeft = append(out.OnlyLeft, id)
		}
		if ls.Status == "failed" {
			out.FailedLeft++
		}
	}
	for id, rs := range r {
		if _, ok := l[id]; !ok {
			out.OnlyRight = append(out.OnlyRight, id)
		}
		if rs.Status == "failed" {
			out.FailedRight++
		}
	}

	for _, s := range left {
		out.TokenDelta -= s.TokensIn + s.TokensOut
		out.CostDelta -= s.MicroUSD
	}
	for _, s := range right {
		out.TokenDelta += s.TokensIn + s.TokensOut
		out.CostDelta += s.MicroUSD
	}

	// 排序：map 迭代顺序随机，而 diff 输出要被看、被 diff。
	sortStrings(out.OnlyLeft)
	sortStrings(out.OnlyRight)
	sortChanges(out.Changed)
	return out
}

func sortStrings(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}

func sortChanges(xs []SpanChange) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j].StepID < xs[j-1].StepID; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}

// Parse 读出 trace 里的 span 列表（**不验哈希链**，那是 Verify 的事）。
//
// 分开的理由：diff / summary 只需要内容；把"内容可读"和"链完整"绑在一起，
// 会让一份被篡改但结构完好的 trace 连 summary 都跑不出来 —— 而那时人最需要的
// 恰恰是先看看里面写了什么。
func Parse(data []byte) (*Trace, error) {
	var top map[string]any
	if err := decodeNumbers(data, &top); err != nil {
		return nil, fmt.Errorf("trace: parse failed: %w", err)
	}
	raw, ok := top["events"].([]any)
	if !ok {
		return nil, errors.New("trace: missing events")
	}
	t := &Trace{Spans: make([]Span, 0, len(raw))}
	for i, e := range raw {
		b, err := json.Marshal(e)
		if err != nil {
			return nil, fmt.Errorf("trace: block %d cannot be re-encoded: %w", i, err)
		}
		var s Span
		if err := json.Unmarshal(b, &s); err != nil {
			return nil, fmt.Errorf("trace: block %d is not a span: %w", i, err)
		}
		t.Spans = append(t.Spans, s)
	}
	return t, nil
}
