// Package canon 是**唯一**的 canonical JSON 编码器。
//
// 为什么自己写而不直接用 encoding/json：map 迭代顺序随机、结构体字段序随定义变，
// 两者都会让"同一份数据"产出不同字节，而哈希链、归档索引、签名全都建立在
// "同数据⇒同字节"之上。这里把顺序**固化成规则**：
//
//	P-1  不允许浮点（值类型里根本没有 Float —— 想塞浮点必须显式调用 Float()，
//	     它返回错误）。金额一律整数 micro-USD，概率一律整数基点。
//	P-2  对象键按字节序升序。
//	P-3  字符串用 encoding/json 的转义（唯一实现，避免两套转义规则）。
//	P-4  无空白、无缩进。
package canon

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ErrFloat 是"canonical 遇浮点即失败"的实现（T20 ⑥）。
// 它不是洁癖：浮点的十进制表示与解析在不同实现间不保证一致，
// 一旦进哈希，跨语言校验就会莫名其妙地失败。
var ErrFloat = errors.New("canon: float is not allowed in canonical JSON")

// Kind 是值类型。刻意没有 Float。
type Kind uint8

const (
	KNull Kind = iota
	KBool
	KInt
	KStr
	KArr
	KObj
)

// Value 是一个 canonical JSON 值。构造即合法，无需校验。
type Value struct {
	k Kind
	b bool
	i int64
	s string
	a []Value
	o map[string]Value
}

func Null() Value           { return Value{k: KNull} }
func Bool(b bool) Value     { return Value{k: KBool, b: b} }
func Int(i int64) Value     { return Value{k: KInt, i: i} }
func Str(s string) Value    { return Value{k: KStr, s: s} }
func Arr(vs ...Value) Value { return Value{k: KArr, a: vs} }

// Obj 接受键值对。传奇数个参数会 panic —— 这是**编程错误**，
// 与其产出半个对象，不如在构造点就炸。
func Obj(kv ...any) Value {
	if len(kv)%2 != 0 {
		panic("canon.Obj: 需要偶数个参数（键,值交替）")
	}
	m := make(map[string]Value, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			panic("canon.Obj: 键必须是 string")
		}
		v, ok := kv[i+1].(Value)
		if !ok {
			panic("canon.Obj: 值必须是 canon.Value")
		}
		m[k] = v
	}
	return Value{k: KObj, o: m}
}

// Float 永远返回错误。它存在只是为了给"误传浮点"一个**显式**的失败点，
// 而不是让它悄悄被截成整数。
func Float(f float64) (Value, error) {
	return Value{}, fmt.Errorf("%w: %v", ErrFloat, f)
}

// Bytes 编码成 canonical 字节。
func (v Value) Bytes() []byte {
	buf := make([]byte, 0, 128)
	return v.appendTo(buf)
}

func (v Value) appendTo(b []byte) []byte {
	switch v.k {
	case KNull:
		return append(b, "null"...)
	case KBool:
		if v.b {
			return append(b, "true"...)
		}
		return append(b, "false"...)
	case KInt:
		return strconv.AppendInt(b, v.i, 10)
	case KStr:
		q, _ := json.Marshal(v.s) // 只有字符串会走到这里，Marshal 不会失败
		return append(b, q...)
	case KArr:
		b = append(b, '[')
		for i, e := range v.a {
			if i > 0 {
				b = append(b, ',')
			}
			b = e.appendTo(b)
		}
		return append(b, ']')
	case KObj:
		keys := make([]string, 0, len(v.o))
		for k := range v.o {
			keys = append(keys, k)
		}
		sort.Strings(keys) // P-2：键序即字节序
		b = append(b, '{')
		for i, k := range keys {
			if i > 0 {
				b = append(b, ',')
			}
			q, _ := json.Marshal(k)
			b = append(b, q...)
			b = append(b, ':')
			b = v.o[k].appendTo(b)
		}
		return append(b, '}')
	}
	return b
}

// FromAny 把 decoding/canonical 的输出（json.Number）翻成 Value。
// 只用于**校验**路径：读回来的字节要重新 canonical 化才能比对哈希。
func FromAny(x any) (Value, error) {
	switch t := x.(type) {
	case nil:
		return Null(), nil
	case bool:
		return Bool(t), nil
	case string:
		return Str(t), nil
	case json.Number:
		s := t.String()
		if strings.ContainsAny(s, ".eE") {
			return Value{}, fmt.Errorf("%w: %s", ErrFloat, s)
		}
		i, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return Value{}, fmt.Errorf("canon: 整数越界 %q: %w", s, err)
		}
		return Int(i), nil
	case float64:
		// 即使解码器没用 UseNumber，也要在这里挡住，而不是截断。
		return Value{}, fmt.Errorf("%w: %v", ErrFloat, t)
	case []any:
		out := make([]Value, 0, len(t))
		for _, e := range t {
			v, err := FromAny(e)
			if err != nil {
				return Value{}, err
			}
			out = append(out, v)
		}
		return Value{k: KArr, a: out}, nil
	case map[string]any:
		m := make(map[string]Value, len(t))
		for k, e := range t {
			v, err := FromAny(e)
			if err != nil {
				return Value{}, err
			}
			m[k] = v
		}
		return Value{k: KObj, o: m}, nil
	}
	return Value{}, fmt.Errorf("canon: 不支持的类型 %T", x)
}
