package trace

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"hermes/internal/native"
)

func TestMain(m *testing.M) {
	// 哈希链在原生层（Rust），Go 侧只是字节搬运。测试必须先初始化 ABI。
	if err := native.Init(); err != nil {
		fmt.Fprintf(os.Stderr, "native.Init: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	native.Shutdown()
	os.Exit(code)
}

func span(id, parent, step, status, attr string) Span {
	return Span{
		ID: id, Parent: parent, StepID: step, Kind: "step",
		Name: "n_" + step, Status: status, Attr: attr,
		StartNS: 100, DurNS: 5, TokensIn: 10, TokensOut: 4, MicroUSD: 250,
	}
}

// ① 写完第一个 span 后文件已非空（流式），且整体是合法 JSON。
func TestStreamsAndProducesValidJSON(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, Options{RunID: "run-1"})
	if buf.Len() != 0 {
		t.Fatal("构造 Writer 不该立刻写字节")
	}
	if err := w.Emit(span("s1", "", "a", "ok", "")); err != nil {
		t.Fatal(err)
	}
	if buf.Len() == 0 {
		t.Fatal("第一个 span 之后文件必须非空（崩溃时才有东西可看）")
	}
	if err := w.Emit(span("s2", "s1", "b", "failed", "transport")); err != nil {
		t.Fatal(err)
	}
	info, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	if info.Blocks != 2 {
		t.Fatalf("应有 2 块，got %d", info.Blocks)
	}
	if !json.Valid(buf.Bytes()) {
		t.Fatalf("产物不是合法 JSON:\n%s", buf.String())
	}
}

// ② 未篡改 → Verify 通过，且根哈希与写入时一致。
func TestVerifyClean(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, Options{RunID: "run-2"})
	for i := 0; i < 5; i++ {
		if err := w.Emit(span(fmt.Sprintf("s%d", i), map[bool]string{true: "", false: fmt.Sprintf("s%d", i-1)}[i == 0], fmt.Sprintf("st%d", i), "ok", "")); err != nil {
			t.Fatal(err)
		}
	}
	info, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Verify(buf.Bytes())
	if err != nil {
		t.Fatalf("未篡改不该报错：%v", err)
	}
	if got.Root != info.Root {
		t.Fatalf("根哈希不一致：%s vs %s", got.Root, info.Root)
	}
	if got.Blocks != 5 {
		t.Fatalf("块数应为 5，got %d", got.Blocks)
	}
}

// ③ 改任一字节 → 非零错误，且指出**第 k 块**（R-TR-04）。
func TestVerifyLocatesTamperedBlock(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, Options{RunID: "run-3"})
	for i := 0; i < 4; i++ {
		parent := ""
		if i > 0 {
			parent = fmt.Sprintf("s%d", i-1)
		}
		if err := w.Emit(span(fmt.Sprintf("s%d", i), parent, fmt.Sprintf("st%d", i), "ok", "")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// 改**第 2 块的值**（不是改字节长度）：把 status 从 ok 改成 tampered。
	// 走解码→改值→重编码，避免手改字节时误伤 JSON 结构（那是测试自身的 bug，
	// 会让"检测不到篡改"与"解析失败"混在一起，掩盖真正要验的东西）。
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	evs := doc["events"].([]any)
	evs[2].(map[string]any)["status"] = "tampered"
	tampered, mErr := json.Marshal(doc)
	if mErr != nil {
		t.Fatal(mErr)
	}

	_, err := Verify(tampered)
	if err == nil {
		t.Fatal("改了字节必须报错")
	}
	var m *Mismatch
	if !errors.As(err, &m) {
		t.Fatalf("应是 *Mismatch，got %T: %v", err, err)
	}
	if m.Block != 2 {
		t.Fatalf("应指出第 2 块，got %d（%s）", m.Block, err)
	}
}

// ④ 超大内容触发截断：文件 ≤ max_bytes，含 trace.truncated，且仍是合法 JSON。
func TestTruncation(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, Options{RunID: "run-4", MaxBytes: 4096})
	hit := false
	for i := 0; i < 100; i++ {
		s := span(fmt.Sprintf("s%d", i), "", fmt.Sprintf("st%d", i), "ok", "")
		s.Detail = map[string]string{"payload": strings.Repeat("x", 200)}
		if err := w.Emit(s); err != nil {
			if !errors.Is(err, ErrTruncated) {
				t.Fatalf("应 ErrTruncated，got %v", err)
			}
			hit = true
			break
		}
	}
	if !hit {
		t.Fatal("4096 字节装不下 100×200 字节，应该触发截断")
	}
	info, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !info.Truncated {
		t.Fatal("ChainInfo.Truncated 应为 true")
	}
	if int64(buf.Len()) > 4096 {
		t.Fatalf("文件超 max_bytes：%d > 4096", buf.Len())
	}
	if !strings.Contains(buf.String(), "trace.truncated") {
		t.Fatal("必须留下 trace.truncated 事件，否则读者不知道有东西被丢了")
	}
	if !json.Valid(buf.Bytes()) {
		t.Fatal("截断后仍必须是合法 JSON")
	}
	// 截断的 trace 也必须能验签（证明"这份不完整的记录确实是我写的"）
	if _, err := Verify(buf.Bytes()); err != nil {
		t.Fatalf("截断 trace 应可验证：%v", err)
	}
}

// ⑤ Summarize 用单文件回答"哪步失败/花多少/调几次"，不触网。
func TestSummarize(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, Options{RunID: "run-5"})
	_ = w.Emit(span("s1", "", "fetch", "ok", ""))
	_ = w.Emit(Span{ID: "s2", Parent: "s1", StepID: "call_model", Kind: "provider",
		Name: "openai/qwen/chat", Status: "failed", Attr: "upstream_contract_drift",
		TokensIn: 100, TokensOut: 20, MicroUSD: 1234})
	_, _ = w.Close()

	s, err := Summarize(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if s.RunID != "run-5" || s.Spans != 2 {
		t.Fatalf("基本字段不对：%+v", s)
	}
	if s.FailedStep != "call_model" || s.Attribution != "upstream_contract_drift" {
		t.Fatalf("归因失败：%+v", s)
	}
	if s.MicroUSD != 1484 || s.TokensIn != 110 || s.Calls != 1 {
		t.Fatalf("聚合数字不对：%+v", s)
	}
}

// ⑥ 树的三条硬性质：重复 id / 孤儿 / 环。
func TestValidateTree(t *testing.T) {
	okTree := &Trace{RunID: "r", Spans: []Span{
		{ID: "a"}, {ID: "b", Parent: "a"}, {ID: "c", Parent: "a"},
	}}
	if err := okTree.Validate(); err != nil {
		t.Fatalf("合法树不该报错：%v", err)
	}

	dup := &Trace{Spans: []Span{{ID: "a"}, {ID: "a"}}}
	if err := dup.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("重复 id 必须报错，got %v", err)
	}

	orphan := &Trace{Spans: []Span{{ID: "a"}, {ID: "b", Parent: "nope"}}}
	if err := orphan.Validate(); err == nil || !strings.Contains(err.Error(), "orphan") {
		t.Fatalf("孤儿必须报错，got %v", err)
	}

	cycle := &Trace{Spans: []Span{{ID: "a", Parent: "b"}, {ID: "b", Parent: "a"}}}
	if err := cycle.Validate(); err == nil {
		t.Fatal("环必须报错")
	}
}

// ⑦ 产出的字段集必须与 schemas/trace.schema.json 声明的**逐字段**一致。
//
// 这里不引第三方 JSON Schema 校验器（许可红线：只允许 MIT/Apache/BSD/ISC/0BSD/Zlib，
// 且"零依赖"是 ARCH 的硬约束）。改为断言三件事：必填字段全在、没有 schema 之外的键、
// 数值全是整数。等自写的 schema 校验器（T12 族）到位后，这段应替换为真正的 schema 校验。
func TestShapeMatchesSchema(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, Options{RunID: "run-schema"})
	if err := w.Emit(span("s1", "", "fetch", "ok", "")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}

	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}

	// 顶层：恰好三个键
	if len(doc) != 3 {
		t.Fatalf("顶层键应恰好 3 个，got %v", keys(doc))
	}
	for _, k := range []string{"run_id", "events", "chain"} {
		if _, ok := doc[k]; !ok {
			t.Fatalf("缺顶层字段 %q", k)
		}
	}

	// span 事件：12 个必填 + 可选 detail
	ev := doc["events"].([]any)[0].(map[string]any)
	wantSpan := []string{
		"span_id", "parent_id", "step_id", "kind", "name", "status",
		"attr", "dur_ns", "micro_usd", "start_ns", "tokens_in", "tokens_out",
	}
	for _, k := range wantSpan {
		if _, ok := ev[k]; !ok {
			t.Fatalf("span 缺字段 %q（schema 声明的必填项）", k)
		}
	}
	allowed := map[string]bool{"detail": true}
	for _, k := range wantSpan {
		allowed[k] = true
	}
	for k := range ev {
		if !allowed[k] {
			t.Fatalf("span 出现 schema 之外的字段 %q", k)
		}
	}

	// chain
	ch := doc["chain"].(map[string]any)
	for _, k := range []string{"blocks", "total_len", "root", "block_hashes"} {
		if _, ok := ch[k]; !ok {
			t.Fatalf("chain 缺字段 %q", k)
		}
	}
	root, _ := ch["root"].(string)
	if len(root) != 64 {
		t.Fatalf("root 应是 64 位十六进制，got %q", root)
	}

	// 所有数值必须是整数：浮点会让哈希跨机器漂移。
	var walk func(any)
	walk = func(x any) {
		switch v := x.(type) {
		case json.Number:
			if strings.ContainsAny(v.String(), ".eE") {
				t.Fatalf("出现非整数数值 %q", v.String())
			}
		case []any:
			for _, e := range v {
				walk(e)
			}
		case map[string]any:
			for _, e := range v {
				walk(e)
			}
		}
	}
	walk(doc)
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ⑧ 单个 span 超过块上限 → 报错不截断。
func TestOversizeSpanRejected(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, Options{RunID: "r", MaxBytes: 1 << 30})
	s := span("s", "", "st", "ok", "")
	s.Detail = map[string]string{"big": strings.Repeat("y", native.ChainBlockMax+1)}
	if err := w.Emit(s); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("应 ErrTooLarge，got %v", err)
	}
}
