package ir

import (
	"encoding/json"
	"testing"
)

// intp returns a pointer to v; shared by package ir tests.
func intp(v int) *int { return &v }

// TestAgentMaxStepsRoundTripsAsAbsent 守"缺键在 IR 往返后仍然缺键"。
//
// 这是 `AgentSpec.MaxSteps` 从 int 改成 *int 的**唯一动机**：非指针 int 会把
// 缺键解成 0，再编码回 `"max_steps":0` —— 一个凭空出现的假数字写进 .hbc。
// 断言用字节层面的 `max_steps` 出现次数，而不是断言字段等于 0：
// 后者在两版实现下都能通过。
//
// 序列化的是**节点**而不是整个 workflow：workflow.bounds 里也有一个必填的
// max_steps，拿整个文档去数字符串会把两者混在一起，测试就算错了也看不出来。
//
// This test was moved verbatim out of bounds_test.go: it exercises types.go
// serialization, not the deleted Go bounds implementation.
func TestAgentMaxStepsRoundTripsAsAbsent(t *testing.T) {
	goal := 1
	cases := []struct {
		name  string
		steps *int
		want  int // 期望在编码后的字节里出现 `"max_steps":` 的次数
	}{
		{"absent", nil, 0},
		{"present", intp(4), 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := Node{ID: 0, K: "agent", Agent: &AgentSpec{
				Goal: &goal, Tools: []string{}, MaxSteps: c.steps,
			}}
			blob, err := json.Marshal(n)
			if err != nil {
				t.Fatal(err)
			}
			if got := countSubstring(string(blob), `"max_steps":`); got != c.want {
				t.Fatalf("编码后 `\"max_steps\":` 出现 %d 次，应为 %d：%s", got, c.want, blob)
			}
			// 回读一遍：缺失必须还是缺失（而不是变成 0）。
			var back Node
			if err := json.Unmarshal(blob, &back); err != nil {
				t.Fatal(err)
			}
			if back.Agent == nil {
				t.Fatal("往返后 agent 丢了")
			}
			if (back.Agent.MaxSteps == nil) != (c.steps == nil) {
				t.Fatalf("往返后指针的 nil 性变了：%v → %v",
					c.steps == nil, back.Agent.MaxSteps == nil)
			}
		})
	}
}

// countSubstring counts non-overlapping occurrences of sub in s.
func countSubstring(s, sub string) int {
	if sub == "" {
		return 0
	}
	n := 0
	for i := 0; i+len(sub) <= len(s); {
		if s[i:i+len(sub)] == sub {
			n++
			i += len(sub)
			continue
		}
		i++
	}
	return n
}
