package trace

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"hermes/internal/canon"
	"hermes/internal/native"
)

// ErrTooLarge：单个 span 超过哈希块上限。**不截断**——
// 截断会让"链对不上"这个信号消失，而那是防篡改的全部意义。
var ErrTooLarge = errors.New("trace: span exceeds chain block limit")

// ErrTruncated：文件已达 max_bytes，后续 span 被丢弃。
var ErrTruncated = errors.New("trace: file size limit reached")

// ErrClosed：Writer 已关闭。
var ErrClosed = errors.New("trace: writer closed")

// Options 控制落盘行为。
type Options struct {
	RunID    string
	MaxBytes int64 // <=0 取默认 8 MiB
}

const defaultMaxBytes = 8 << 20

// footerReserve 是给 chain 尾巴预留的空间。不留的话，最后一个 span 会把
// 尾巴挤掉，产出一个**非法 JSON** —— 而非法 JSON 的 trace 等于没有 trace。
const footerReserve = 8192

// ChainInfo 是 Close 的结果，也是写入文件尾巴的内容。
type ChainInfo struct {
	Blocks      int      `json:"blocks"`
	TotalLen    int64    `json:"total_len"`
	Root        string   `json:"root"`
	BlockHashes []string `json:"block_hashes"`
	Truncated   bool     `json:"truncated"`
}

// Writer 流式写 trace：每写一个 span 文件即非空，崩溃时至少留下已发生的部分。
type Writer struct {
	w       io.Writer
	run     string
	max     int64
	written int64

	started bool
	closed  bool

	n      int
	total  int64
	prev   native.Digest
	hashes []string

	truncated bool
}

// NewWriter 构造。不写任何字节，直到第一个 span 到来——
// 这样"写完第一个 span 后文件已非空"是自然成立的，不靠额外 flush。
func NewWriter(w io.Writer, opt Options) *Writer {
	if opt.MaxBytes <= 0 {
		opt.MaxBytes = defaultMaxBytes
	}
	return &Writer{w: w, run: opt.RunID, max: opt.MaxBytes, prev: native.Genesis()}
}

func (w *Writer) write(s string) error {
	b := []byte(s)
	n, err := w.w.Write(b)
	w.written += int64(n)
	return err
}

// Emit 追加一个 span。返回 ErrTruncated 后，调用方应停止 emit 并 Close。
func (w *Writer) Emit(s Span) error {
	if w.closed {
		return ErrClosed
	}
	body := s.toCanon().Bytes()
	if len(body) > native.ChainBlockMax {
		return fmt.Errorf("%w: %d > %d", ErrTooLarge, len(body), native.ChainBlockMax)
	}

	// 预留尾巴空间；不够就先落一个 truncated 事件收口。
	// 预留量在极小 max 下要收缩，否则"永远装不下第一个 span"。
	reserve := int64(footerReserve)
	if reserve > w.max/4 {
		reserve = w.max / 4
	}
	if w.written+int64(len(body))+2+reserve > w.max {
		w.truncated = true
		if err := w.emitTruncated(); err != nil {
			return err
		}
		return ErrTruncated
	}

	if !w.started {
		if err := w.write(`{"run_id":"` + jsonEscape(w.runID()) + `","events":[`); err != nil {
			return err
		}
		w.started = true
	} else if err := w.write(","); err != nil {
		return err
	}

	if err := w.write(string(body)); err != nil {
		return err
	}
	return w.chain(body)
}

func (w *Writer) runID() string { return w.run }

func (w *Writer) chain(body []byte) error {
	h, err := native.ChainHash(w.prev, uint64(w.n), body)
	if err != nil {
		return err
	}
	w.prev = h
	w.hashes = append(w.hashes, hex.EncodeToString(h[:]))
	w.n++
	w.total += int64(len(body))
	return nil
}

func (w *Writer) emitTruncated() error {
	ev := canon.Obj(
		"kind", canon.Str("event"),
		"name", canon.Str("trace.truncated"),
		"status", canon.Str("truncated"),
		"detail", canon.Obj("at_bytes", canon.Int(w.written)),
	)
	body := ev.Bytes()
	if !w.started {
		if err := w.write(`{"run_id":"` + jsonEscape(w.run) + `","events":[`); err != nil {
			return err
		}
		w.started = true
	} else if err := w.write(","); err != nil {
		return err
	}
	if err := w.write(string(body)); err != nil {
		return err
	}
	return w.chain(body)
}

// Close 收口：算终块哈希，写出 chain 尾巴。可重复调用。
func (w *Writer) Close() (*ChainInfo, error) {
	if w.closed {
		return nil, ErrClosed
	}
	w.closed = true
	if !w.started {
		if err := w.write(`{"run_id":"` + jsonEscape(w.run) + `","events":[`); err != nil {
			return nil, err
		}
		w.started = true
	}
	root, err := native.ChainFinal(w.prev, uint64(w.n), uint64(w.total))
	if err != nil {
		return nil, err
	}
	info := &ChainInfo{
		Blocks:      w.n,
		TotalLen:    w.total,
		Root:        hex.EncodeToString(root[:]),
		BlockHashes: w.hashes,
		Truncated:   w.truncated,
	}
	tail, err := json.Marshal(info)
	if err != nil {
		return nil, err
	}
	if err := w.write(`],"chain":` + string(tail) + `}`); err != nil {
		return nil, err
	}
	return info, nil
}

// 内部字段（放这里以保持 NewWriter 的签名简单）。
func (w *Writer) ensureRun(id string) {
	if w.run == "" {
		w.run = id
	}
}

func jsonEscape(s string) string {
	q, _ := json.Marshal(s)
	return strings.Trim(string(q), `"`)
}

// ---------------- 校验 ----------------

// Mismatch 指出**第 k 块**被改了（R-TR-04 的"指出第 k 块"）。
type Mismatch struct {
	Block  int    // 从 0 开始；-1 表示无法定位（例如块数对不上）
	Want   string // 文件里记录的哈希
	Got    string // 重算出来的哈希
	Detail string
}

func (m *Mismatch) Error() string {
	if m.Block < 0 {
		return fmt.Sprintf("trace tampered: %s", m.Detail)
	}
	return fmt.Sprintf("trace tampered at block %d (want %s, got %s)", m.Block, m.Want, m.Got)
}

// Verify 重算整条链并与文件里记录的根比对。返回 (info, nil) 表示未篡改。
func Verify(data []byte) (*ChainInfo, error) {
	var top map[string]any
	if err := decodeNumbers(data, &top); err != nil {
		return nil, fmt.Errorf("trace: 解析失败: %w", err)
	}
	rawEvents, ok := top["events"].([]any)
	if !ok {
		return nil, errors.New("trace: 缺 events")
	}
	rawChain, ok := top["chain"].(map[string]any)
	if !ok {
		return nil, errors.New("trace: 缺 chain")
	}

	prev := native.Genesis()
	var total int64
	hashes := make([]string, 0, len(rawEvents))
	for i, e := range rawEvents {
		v, err := canon.FromAny(e)
		if err != nil {
			return nil, fmt.Errorf("trace: 第 %d 块不可 canonical 化: %w", i, err)
		}
		body := v.Bytes()
		h, err := native.ChainHash(prev, uint64(i), body)
		if err != nil {
			return nil, err
		}
		prev = h
		hashes = append(hashes, hex.EncodeToString(h[:]))
		total += int64(len(body))
	}
	root, err := native.ChainFinal(prev, uint64(len(rawEvents)), uint64(total))
	if err != nil {
		return nil, err
	}
	info := &ChainInfo{
		Blocks:      len(rawEvents),
		TotalLen:    total,
		Root:        hex.EncodeToString(root[:]),
		BlockHashes: hashes,
	}

	gotRoot := asString(rawChain["root"])
	if gotRoot == info.Root {
		return info, nil
	}

	// 根对不上 → 逐块定位。文件里存了 block_hashes，才能回答"哪一块"。
	want, _ := rawChain["block_hashes"].([]any)
	if len(want) == len(hashes) {
		for i := range hashes {
			if asString(want[i]) != hashes[i] {
				return info, &Mismatch{Block: i, Want: asString(want[i]), Got: hashes[i]}
			}
		}
	}
	return info, &Mismatch{
		Block:  -1,
		Detail: fmt.Sprintf("root 不符（want %s, got %s），块数 %d", gotRoot, info.Root, len(rawEvents)),
	}
}

// Summary 用**单文件、零网络**回答三件事：哪步失败、花了多少、调了几次（R-TR-05）。
type Summary struct {
	RunID       string `json:"run_id"`
	Spans       int    `json:"spans"`
	FailedStep  string `json:"failed_step"`
	FailedSpan  string `json:"failed_span"`
	Attribution string `json:"attribution"`
	MicroUSD    int64  `json:"micro_usd"`
	TokensIn    int64  `json:"tokens_in"`
	TokensOut   int64  `json:"tokens_out"`
	Calls       int    `json:"provider_calls"`
	Tools       int    `json:"tool_calls"`
	Truncated   bool   `json:"truncated"`
}

// Summarize 扫一遍已落盘的 trace。不触网、不依赖源文件。
func Summarize(data []byte) (*Summary, error) {
	var top map[string]any
	if err := decodeNumbers(data, &top); err != nil {
		return nil, fmt.Errorf("trace: 解析失败: %w", err)
	}
	s := &Summary{RunID: asString(top["run_id"])}
	evs, _ := top["events"].([]any)
	s.Spans = len(evs)
	for _, e := range evs {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		s.MicroUSD += asInt(m["micro_usd"])
		s.TokensIn += asInt(m["tokens_in"])
		s.TokensOut += asInt(m["tokens_out"])
		switch asString(m["kind"]) {
		case "provider":
			s.Calls++
		case "tool":
			s.Tools++
		}
		if asString(m["status"]) == "truncated" {
			s.Truncated = true
		}
		if s.FailedStep == "" && asString(m["status"]) == "failed" {
			s.FailedStep = asString(m["step_id"])
			s.FailedSpan = asString(m["span_id"])
			s.Attribution = asString(m["attr"])
		}
	}
	return s, nil
}

func decodeNumbers(data []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber() // 关键：默认会解成 float64，而浮点在 canonical 里是禁区
	return dec.Decode(out)
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func asInt(v any) int64 {
	switch t := v.(type) {
	case json.Number:
		i, _ := t.Int64()
		return i
	case int64:
		return t
	}
	return 0
}
