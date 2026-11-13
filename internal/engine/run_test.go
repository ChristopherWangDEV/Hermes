package engine

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hermes/internal/ir"
)

// loadGolden 读一份真实的 IR（由 `hermes ir` 生成，不是手写 fixture）。
func loadGolden(t *testing.T, name string) *ir.Doc {
	t.Helper()
	data, err := os.ReadFile("../../schemas/golden/" + name)
	if err != nil {
		t.Fatalf("读不到 golden %s: %v", name, err)
	}
	d, ds := ir.Load(data)
	if len(ds) != 0 {
		t.Fatalf("golden 自己不合格: %v", ds)
	}
	return d
}

// TestRunGoldenHello 是"能编译 → 能执行"的分界线：
// 一份真实的 IR 文件跑出源码里写的那个值。
func TestRunGoldenHello(t *testing.T) {
	d := loadGolden(t, "ir-hello.json")
	res, err := Run(d, "", nil)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if res.Value != "hi" {
		t.Fatalf("期望 \"hi\"，实际 %#v", res.Value)
	}
	if res.Workflow != "hello" {
		t.Fatalf("应自动选中 is_entry 的 workflow，实际 %q", res.Workflow)
	}
	if res.Steps == 0 {
		t.Fatal("Steps 为 0：说明没真的走节点")
	}
}

// TestRunGoldenTyped 覆盖 field 与 interp 之外的基本值：字符串字段读取。
func TestRunGoldenControl(t *testing.T) {
	d := loadGolden(t, "ir-control.json")
	res, err := Run(d, "", nil)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if res.Value != "r" {
		t.Fatalf("match 的 \"refund\" 臂应返回 \"r\"，实际 %#v", res.Value)
	}
}

// TestRunRejectsUnknownNode 证明不支持的节点**报错**而不是跳过。
//
// 跳过会让"少执行一个分支"看起来像成功——而产物是错的，且没有任何信号。
func TestRunRejectsUnknownNode(t *testing.T) {
	d := loadGolden(t, "ir-hello.json")
	d.Workflows[0].Nodes[0].Kids = []int{99}
	if _, err := Run(d, "", nil); err == nil {
		t.Fatal("越界节点应报错")
	}
}

// TestRunInputSentinel 证明 `var: -1` 读的是 input（IR.md §4.3 的哨兵）。
func TestRunInputSentinel(t *testing.T) {
	d := loadGolden(t, "ir-hello.json")
	// 把 return 的 e 换成一个 var -1 节点，验证它读 input。
	w := &d.Workflows[0]
	minus := -1
	w.Nodes = append(w.Nodes, ir.Node{
		ID: len(w.Nodes), K: "var", Var: &minus,
		Loc: ir.Loc{M: 0, L: 1, C: 1, EL: 1, EC: 2},
	})
	for i := range w.Nodes {
		if w.Nodes[i].K == "return" {
			v := len(w.Nodes) - 1
			w.Nodes[i].E = &v
		}
	}
	res, err := Run(d, "", map[string]any{"text": "hello"})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if m, ok := res.Value.(map[string]any); !ok || m["text"] != "hello" {
		t.Fatalf("var -1 应读到 input，实际 %#v", res.Value)
	}
}

// TestSpansStayWithinBound 是 IR.md §5.1 的运行期断言：
// span 数不得超过 bounds.max_spans（根 span 不计入 SP 代数，见 run.go 的说明）。
func TestSpansStayWithinBound(t *testing.T) {
	for _, name := range []string{"ir-hello.json", "ir-control.json", "ir-typed.json", "ir-support_bot.json"} {
		d := loadGolden(t, name)
		w := &d.Workflows[0]
		res, err := Run(d, "", nil)
		if err != nil {
			// support_bot 还跑不了（parallel/外部调用），那不是本测试的失败。
			continue
		}
		if len(res.Spans) > w.Bounds.MaxSpans+1 {
			t.Errorf("%s: %d 个 span 超过上界 %d(+1)", name, len(res.Spans), w.Bounds.MaxSpans)
		}
		if len(res.Spans) == 0 {
			t.Errorf("%s: 一条 span 都没有 —— 根 span 必须存在", name)
		}
		if res.Spans[0].Kind != "workflow" || res.Spans[0].Parent != "" {
			t.Errorf("%s: 根 span 形状不对: %+v", name, res.Spans[0])
		}
		for _, s := range res.Spans[1:] {
			if s.Parent != res.Spans[0].ID {
				t.Errorf("%s: 子 span %s 的 parent 应为根 span，实际 %q", name, s.ID, s.Parent)
			}
		}
	}
}

// TestSpanIDsAreGreppable 守 IR.md §11 的 span_id 规则：`<run>/L<line>`。
//
// 行号是能跳回源码的锚；换成自增编号，trace 就只剩"第几个"而没有"哪一行"。
func TestSpanIDsAreGreppable(t *testing.T) {
	d := loadGolden(t, "ir-control.json")
	res, err := Run(d, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Spans) < 2 {
		t.Fatalf("control 应至少产生根 span + match span，实际 %d", len(res.Spans))
	}
	if !strings.Contains(res.Spans[1].ID, "/L") {
		t.Fatalf("子 span 的 id 应含行号锚（/L<line>），实际 %q", res.Spans[1].ID)
	}
}

// TestRunGoldenParallel 证明 fork/join 与按位置解构真的能执行（D-031）。
//
// 这条测试存在的原因：`parallel` 曾经只有 join_k 而没有 join 表达式表，
// 那份 IR **完全合法**却执行不了 —— 执行器知道"两元"却不知道该装哪两个值。
func TestRunGoldenParallel(t *testing.T) {
	d := loadGolden(t, "ir-parallel.json")
	res, err := Run(d, "", nil)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if res.Value != "done" {
		t.Fatalf("期望 \"done\"，实际 %#v", res.Value)
	}
}

// TestDestructureBindsByPosition 直接钉住"按位置配对"（LANGUAGE §5.2 / E-7）。
//
// 做法：把 golden 的 return 改成读第一个被解构的名字 `a`。若位置配错了，
// 读到的会是 "two"（y 的值）而不是 "one"（x 的值）—— 而两种结果都是合法字符串，
// 只有对着具体值断言才分得出来。
func TestDestructureBindsByPosition(t *testing.T) {
	d := loadGolden(t, "ir-parallel.json")
	w := &d.Workflows[0]

	first := 0 // vars[0] = a
	w.Nodes = append(w.Nodes, ir.Node{
		ID: len(w.Nodes), K: "var", Var: &first,
		Loc: ir.Loc{M: 0, L: 1, C: 1, EL: 1, EC: 2},
	})
	varID := len(w.Nodes) - 1
	for i := range w.Nodes {
		if w.Nodes[i].K == "return" {
			w.Nodes[i].E = &varID
		}
	}

	res, err := Run(d, "", nil)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if res.Value != "one" {
		t.Fatalf("a 应绑定元组第 0 个值（\"one\"），实际 %#v —— 位置配对错了", res.Value)
	}
}

// TestParallelActuallyConcurrent 证明分支**真的**同时跑。
//
// 做法：两个工具调用各到达一次屏障，**必须两个都到齐才放行**。顺序执行的话
// 第一个永远等不到第二个 —— 用超时判失败，而不是让测试挂死。
// 不用"测耗时"：快机器上顺序执行也可能很快，阈值一松就测不出东西。
func TestParallelActuallyConcurrent(t *testing.T) {
	d := loadGolden(t, "ir-support_bot.json")
	g := &gateRuntime{ch: make(chan struct{})}

	_, err := RunWith(d, "", map[string]any{"text": "x", "user_id": "u"}, RunOptions{Runtime: g})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !g.both.Load() {
		t.Fatal("两个分支从未同时到达：parallel 是顺序执行的")
	}
}

// gateRuntime 让两个工具调用互相等待：都到齐才放行，否则超时。
type gateRuntime struct {
	mu      sync.Mutex
	arrived int
	ch      chan struct{}
	once    sync.Once
	both    atomic.Bool
}

func (g *gateRuntime) Model(_ context.Context, _ ModelRequest) (any, error) {
	return map[string]any{
		"action": "refund",
		"order":  map[string]any{"order_id": "A1", "status": int64(200), "amount": int64(1)},
	}, nil
}

func (g *gateRuntime) Tool(_ context.Context, _ ToolRequest) (any, error) {
	g.mu.Lock()
	g.arrived++
	if g.arrived == 2 {
		g.both.Store(true)
		g.once.Do(func() { close(g.ch) })
	}
	g.mu.Unlock()

	select {
	case <-g.ch:
		return map[string]any{"order_id": "A1", "status": int64(200), "amount": int64(1)}, nil
	case <-time.After(2 * time.Second):
		// 顺序执行时这里必然超时：第一个分支等不到第二个。
		return nil, &RunError{Kind: "NetworkError", Msg: "屏障超时：分支没有并发执行"}
	}
}

// TestCancelStopsTheRun 守"取消要真的停止执行"，而不是等它跑完再返回。
//
// 做法：一个工具调用阻塞在 context 上，取消后它应立刻返回；
// 整个 Run 也必须以 ErrCancelled 结束，而不是继续走后面的节点。
func TestCancelStopsTheRun(t *testing.T) {
	d := loadGolden(t, "ir-support_bot.json")
	ctx, cancel := context.WithCancel(context.Background())

	rt := &blockingRuntime{ctx: ctx}
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := RunWith(d, "", map[string]any{"text": "x", "user_id": "u"},
		RunOptions{Runtime: rt, Context: ctx})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("取消后 Run 必须返回错误")
	}
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("应是 ErrCancelled，实际 %v", err)
	}
	// 取消后不该还等到阻塞调用自然结束（那要 5 秒）。
	if elapsed > 2*time.Second {
		t.Fatalf("取消没有生效：Run 花了 %v", elapsed)
	}
}

// blockingRuntime 的模型调用会一直阻塞到 ctx 取消 —— 模拟一次"在飞的请求"。
type blockingRuntime struct{ ctx context.Context }

func (b *blockingRuntime) Model(ctx context.Context, _ ModelRequest) (any, error) {
	<-ctx.Done()
	return nil, &RunError{Kind: "NetworkError", Msg: "cancelled"}
}

func (b *blockingRuntime) Tool(ctx context.Context, _ ToolRequest) (any, error) {
	<-ctx.Done()
	return nil, &RunError{Kind: "NetworkError", Msg: "cancelled"}
}

// TestConcurrencyOverrideLimitsParallel 守"自适应上限真的生效"。
//
// 注入一个恒返回 1 的 Concurrency：parallel 必须**串行**执行。
// 用一个"同时只能有一个"的计数器断言 —— 若并发超过 1，峰值会 >1。
func TestConcurrencyOverrideLimitsParallel(t *testing.T) {
	d := loadGolden(t, "ir-support_bot.json")
	rt := &peakRuntime{}
	_, err := RunWith(d, "", map[string]any{"text": "x", "user_id": "u"}, RunOptions{
		Runtime:     rt,
		Concurrency: func(int) int { return 1 },
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if rt.peak.Load() > 1 {
		t.Fatalf("上限为 1 时不该有并发，峰值 %d", rt.peak.Load())
	}
}

type peakRuntime struct {
	cur  atomic.Int32
	peak atomic.Int32
}

func (p *peakRuntime) enter() {
	n := p.cur.Add(1)
	for {
		old := p.peak.Load()
		if n <= old || p.peak.CompareAndSwap(old, n) {
			break
		}
	}
}

func (p *peakRuntime) exit() { p.cur.Add(-1) }

func (p *peakRuntime) Model(_ context.Context, _ ModelRequest) (any, error) {
	return map[string]any{"action": "refund",
		"order": map[string]any{"order_id": "A1", "status": int64(200), "amount": int64(1)}}, nil
}

func (p *peakRuntime) Tool(_ context.Context, _ ToolRequest) (any, error) {
	p.enter()
	defer p.exit()
	time.Sleep(10 * time.Millisecond)
	return map[string]any{"order_id": "A1", "status": int64(200), "amount": int64(1)}, nil
}

// TestSchemaViolationFailsBeforeOrchestration 守"校验在边界上"。
//
// 模型输出不符合 schema 时，必须在**进入编排之前**失败：那时还没花钱走下一步、
// 也没在错误的路径上走远。断言错误里带字段路径 —— 只说"不匹配"等于没说。
func TestSchemaViolationFailsBeforeOrchestration(t *testing.T) {
	d := loadGolden(t, "ir-support_bot.json")
	rt := &badModelRuntime{}
	_, err := RunWith(d, "", map[string]any{"text": "x", "user_id": "u"}, RunOptions{Runtime: rt})
	if err == nil {
		t.Fatal("模型输出不合 schema 必须失败")
	}
	msg := err.Error()
	if !strings.Contains(msg, "schema") {
		t.Fatalf("错误应说明是 schema 问题：%s", msg)
	}
	if !strings.Contains(msg, "status") {
		t.Fatalf("错误应指出缺哪个字段：%s", msg)
	}
	// 关键：不该走到工具调用。走到了就说明校验发生在编排之后。
	if rt.toolCalled {
		t.Fatal("schema 不合时不该继续调用工具 —— 校验必须发生在边界上")
	}
}

type badModelRuntime struct {
	toolCalled bool
}

func (b *badModelRuntime) Model(_ context.Context, _ ModelRequest) (any, error) {
	// Intent 的 schema 要求 order.{order_id,status,amount}，这里少 status。
	return map[string]any{"action": "refund",
		"order": map[string]any{"order_id": "A1", "amount": int64(1)}}, nil
}

func (b *badModelRuntime) Tool(_ context.Context, _ ToolRequest) (any, error) {
	b.toolCalled = true
	return map[string]any{}, nil
}

// TestToolHeaderRejectsNonString 守"头值必须是字符串"。
//
// 自动把数字/布尔转成 "1"/"true"，服务端读到的就是另一个值，
// 而客户端一切看起来都正常 —— 那种错只能在服务端发现。
func TestToolHeaderRejectsNonString(t *testing.T) {
	_, err := toStringMap(map[string]any{"n": int64(1)})
	if err == nil {
		t.Fatal("非字符串头值必须报错")
	}
	if !strings.Contains(err.Error(), "n") {
		t.Fatalf("错误应点名字段：%v", err)
	}
	ok, err := toStringMap(map[string]any{"X-A": "b"})
	if err != nil || ok["X-A"] != "b" {
		t.Fatalf("字符串头应通过，实际 %v %v", ok, err)
	}
}
