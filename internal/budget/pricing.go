package budget

import "math/bits"

// Source 说明这个单价**是哪来的**。它不是装饰：
// 预算数字对不上账时，第一个要问的就是"这个价是谁给的"。
type Source uint8

const (
	// SourceUnknown：不知道单价。此时金额不参与预留（见 UpperFor），
	// 只按 token 计——宁可少拦，也不能用一个猜出来的价格把请求全挡在门外。
	SourceUnknown Source = iota
	// SourceDeclared：用户在 `.hermes` 里声明。
	SourceDeclared
	// SourceMeasured：从上游返回的实际 usage 反推（事后校准）。
	SourceMeasured
)

func (s Source) String() string {
	switch s {
	case SourceUnknown:
		return "unknown"
	case SourceDeclared:
		return "declared"
	case SourceMeasured:
		return "measured"
	}
	return "unknown_source"
}

// Price 是每百万 token 的单价，单位 micro-USD（1e-6 USD）。
// 用整数而不是 float：D-010.1 明确金额禁止浮点。
type Price struct {
	InPerMTok  int64
	OutPerMTok int64
	Source     Source
}

// Cost 计算 (in,out) 个 token 的费用（micro-USD），向零截断。
func (p Price) Cost(in, out int64) (int64, error) {
	ci, ok := mulDiv1e6(in, p.InPerMTok)
	if !ok {
		return 0, ErrPriceOverflow
	}
	co, ok := mulDiv1e6(out, p.OutPerMTok)
	if !ok {
		return 0, ErrPriceOverflow
	}
	sum, over := satAddOK(ci, co)
	if over {
		return 0, ErrPriceOverflow
	}
	return sum, nil
}

// mulDiv1e6 计算 a*b/1e6。先乘后除，乘的时候用 128 位检查溢出。
func mulDiv1e6(a, b int64) (int64, bool) {
	if a < 0 || b < 0 {
		return 0, false
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	if hi != 0 {
		return 0, false
	}
	return int64(lo / 1_000_000), true
}

// UpperFor 把 (in, out) token 与单价翻成一个可预留的 Upper。
// **`SourceUnknown` 时金额置 0**：D-013/§5.4 的规则是"不知道价格就别拿它当预算闸门"，
// 否则一个默认 0 元/默认天价的 price 会分别导致"永不拦截"或"永远拦截"。
func UpperFor(p Price, in, out int64) (Upper, error) {
	u := Upper{In: in, Out: out}
	if p.Source == SourceUnknown {
		return u, nil
	}
	c, err := p.Cost(in, out)
	if err != nil {
		return Upper{}, err
	}
	u.MicroUSD = c
	return u, nil
}
