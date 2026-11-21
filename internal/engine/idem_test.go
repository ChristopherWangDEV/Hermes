package engine

import (
	"context"
	"encoding/json"
	"testing"

	"hermes/internal/ir"
)

// captureToolRuntime 只记一次工具请求的头，用来验证 §5.7 条件 ② 的
// 运行期一半真的发生了。
type captureToolRuntime struct {
	got ToolRequest
	n   int
}

func (c *captureToolRuntime) Model(_ context.Context, _ ModelRequest) (any, error) {
	return map[string]any{}, nil
}

func (c *captureToolRuntime) Tool(_ context.Context, req ToolRequest) (any, error) {
	c.got = req
	c.n++
	return map[string]any{"ok": true}, nil
}

// TestIdemKeyInjectsHeader 守「幂等证明不是标签」。
//
// 编译期证明 idem_key 只由 input 决定之后，引擎必须在每次工具调用里注入
// 同名 Idempotency-Key 头。此前 codegen 把 args.idem_key 恒写成 null、
// callTool 也从不读它 —— 于是 idem.mode="idem_key" 是一句漂亮但无效的话，
// 服务端收不到键，重试照样重复扣款。
//
// 用真实的 golden IR（由 `hermes ir` 产出）而不是手写节点，保证
// 源 → IR → 执行器整条链都在测。
func TestIdemKeyInjectsHeader(t *testing.T) {
	d := loadGolden(t, "ir-idemkey.json")
	c := &captureToolRuntime{}
	_, err := RunWith(d, "", map[string]any{"user_id": "u42"}, RunOptions{Runtime: c})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if c.n != 1 {
		t.Fatalf("应恰好调用一次工具，实际 %d 次", c.n)
	}
	if got := c.got.Header["Idempotency-Key"]; got != "order-u42" {
		t.Fatalf("Idempotency-Key 应为 %q，实际 %q（全部头：%v）",
			"order-u42", got, c.got.Header)
	}
}

// TestIdemKeyRejectsNonString 守「不悄悄转型」：幂等键若算出数字，
// fmt.Sprint 一下就能让它「看起来正常」，但服务端读到的是另一个键。
func TestIdemKeyRejectsNonString(t *testing.T) {
	d := loadGolden(t, "ir-idemkey.json")
	// 把 idem_key 指向的 interp 节点换成一个 int 字面量。
	w := &d.Workflows[0]
	for i := range w.Nodes {
		n := &w.Nodes[i]
		if n.K != "call_tool" {
			continue
		}
		var a ir.ToolArgs
		if err := json.Unmarshal(n.Args, &a); err != nil {
			t.Fatal(err)
		}
		if a.IdemKey == nil {
			t.Fatal("golden 的 call_tool 没有 idem_key")
		}
		id := *a.IdemKey
		if id < 0 || id >= len(w.Nodes) {
			t.Fatalf("idem_key 指向越界节点 %d", id)
		}
		t := "int"
		w.Nodes[id].K = "lit"
		w.Nodes[id].LitT = &t
		w.Nodes[id].LitV = json.RawMessage("42")
		w.Nodes[id].Parts = nil
	}
	c := &captureToolRuntime{}
	_, err := RunWith(d, "", map[string]any{"user_id": "u42"}, RunOptions{Runtime: c})
	if err == nil {
		t.Fatal("非字符串幂等键必须报错，不能悄悄转成 \"42\"")
	}
	if c.n != 0 {
		t.Fatal("幂等键不合法时根本不该发出请求")
	}
}
