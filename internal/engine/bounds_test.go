package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"hermes/internal/ir"
	"hermes/internal/provider"
)

// countingRuntime 记录实际调用次数，用来与 IR 的上界对账。
type countingRuntime struct {
	models int
	tools  int
}

func (c *countingRuntime) Model(_ context.Context, req ModelRequest) (any, error) {
	c.models++
	// agent 走多轮：先要求一次工具调用，再给最终答案。
	//
	// **这里以前返回的是 map**，而 runAgent 断言的是 *ModelReply —— 断言失败就被
	// 当成错误，对账测试 continue 掉。于是"上界与真实执行对账"这条测试，对
	// agent 节点（整个项目存在的理由）**一次都没真正跑过**，绿灯是假的。
	if len(req.Messages) > 0 {
		if c.models%2 == 1 {
			return &ModelReply{ToolCalls: []provider.ToolCall{{
				ID: "c1", Name: "orders", Arguments: `{"id":"A1"}`,
			}}}, nil
		}
		return &ModelReply{Content: "done"}, nil
	}
	return map[string]any{
		"action": "ok",
		"order":  map[string]any{"order_id": "A1", "status": int64(200), "amount": int64(1)},
	}, nil
}

func (c *countingRuntime) Tool(_ context.Context, _ ToolRequest) (any, error) {
	c.tools++
	return map[string]any{"order_id": "A1", "status": int64(200), "amount": int64(1)}, nil
}

// TestBoundsHoldForEveryGolden 是 T33 的对账：
// **实际执行量必须落在 IR 声明的上界之内**。
//
// 上界不是装饰：剥离、trace 限额、预算预留都按它算。上界写小了，
// 运行期会被截断或拒绝；写大了，剥离不掉死代码、预算预留过多。
// 所以它必须与真实执行对账，而不是"看起来差不多"。
func TestBoundsHoldForEveryGolden(t *testing.T) {
	dir := "../../schemas/golden"
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	ran := 0
	for _, e := range ents {
		if filepath.Ext(e.Name()) != ".json" || len(e.Name()) < 4 || e.Name()[:3] != "ir-" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		doc, ds := ir.Load(data)
		if len(ds) != 0 {
			continue // 由 golden 测试负责报
		}
		for i := range doc.Workflows {
			w := &doc.Workflows[i]
			if !w.IsEntry {
				continue // 只跑入口：非入口需要显式参数
			}
			c := &countingRuntime{}
			// 给一个"每种 golden 都可能用到"的输入：空对象会让带 input 的 workflow
			// 在插值阶段就失败，而失败被当成"需要真实上游"跳过 —— 于是 agent 那份
			// （整个项目的核心）永远跑不到对账。
			input := map[string]any{"id": "A1", "text": "hi", "user_id": "u1", "title": "t"}
			res, err := RunWith(doc, w.Name, input, RunOptions{Runtime: c})
			if err != nil {
				// 需要真实上游的 golden（live / support_bot）会失败，跳过它们的数量对账，
				// 但**不跳过** bounds 自身的自洽检查（见下）。
				continue
			}
			ran++
			if w.Bounds.Unbounded {
				// unbounded 的数字**不是承诺**：递归函数的调用次数与未知循环的
				// 迭代次数都无法静态定界。真正的闸是运行期深度上限（512）与
				// 循环上限；这里只对"有界"workflow 做上界对账。
				continue
			}
			if res.Steps > w.Bounds.MaxSteps {
				t.Errorf("%s/%s: 实际 steps %d 超过上界 %d", e.Name(), w.Name, res.Steps, w.Bounds.MaxSteps)
			}
			if len(res.Spans) > w.Bounds.MaxSpans+1 {
				t.Errorf("%s/%s: 实际 spans %d 超过上界 %d(+1)", e.Name(), w.Name, len(res.Spans), w.Bounds.MaxSpans)
			}
			if c.models > w.Bounds.MaxModelCalls {
				t.Errorf("%s/%s: 实际 model 调用 %d 超过上界 %d", e.Name(), w.Name, c.models, w.Bounds.MaxModelCalls)
			}
			if c.tools > w.Bounds.MaxToolCalls {
				t.Errorf("%s/%s: 实际 tool 调用 %d 超过上界 %d", e.Name(), w.Name, c.tools, w.Bounds.MaxToolCalls)
			}
		}
	}
	if ran == 0 {
		t.Fatal("一份 golden 都没跑起来 —— 这条对账等于没做")
	}
	t.Logf("对账了 %d 个入口 workflow", ran)
}

// TestBoundsAreCoherent 检查上界自身的自洽：模型/工具调用不可能超过 steps。
func TestBoundsAreCoherent(t *testing.T) {
	dir := "../../schemas/golden"
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if filepath.Ext(e.Name()) != ".json" || e.Name()[:3] != "ir-" {
			continue
		}
		data, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		doc, ds := ir.Load(data)
		if len(ds) != 0 {
			continue
		}
		for i := range doc.Workflows {
			b := doc.Workflows[i].Bounds
			// agent 的每一步会多出一次模型调用 + 一次工具调用，而 agent 节点本身
			// 只占一个 statement 步。所以"调用数 ≤ 步数"这条自洽断言要按 agent
			// 的 max_steps 放宽 —— 放宽的是**这一项**，不是整体。
			allowance := 0
			for ni := range doc.Workflows[i].Nodes {
				if n := &doc.Workflows[i].Nodes[ni]; n.K == "agent" && n.Agent != nil {
					allowance += 2 * n.Agent.MaxSteps
				}
			}
			if b.MaxModelCalls > b.MaxSteps+allowance || b.MaxToolCalls > b.MaxSteps+allowance {
				t.Errorf("%s/%s: 调用上界(%d/%d) 超过 steps 上界 %d（含 agent 配额 %d）",
					e.Name(), doc.Workflows[i].Name, b.MaxModelCalls, b.MaxToolCalls, b.MaxSteps, allowance)
			}
			// **不检查 max_concurrency 与 max_spans 的大小关系**：
			// span 数是"模型/工具调用产生的 span"，并发数是"同时在跑的分支"，
			// 两者量纲不同。我一开始凭直觉加了这条断言，它对 parallel 立刻误报 ——
			// 而一个会误报的守卫很快就会被加 t.Skip。
		}
	}
	_ = json.Valid
}
