package canon

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// P-2：键序按字节序，与插入顺序无关。
func TestKeyOrderIsBytewise(t *testing.T) {
	a := Obj("b", Int(2), "a", Int(1), "C", Int(3))
	want := `{"C":3,"a":1,"b":2}`
	if got := string(a.Bytes()); got != want {
		t.Fatalf("键序错：\nwant %s\ngot  %s", want, got)
	}
}

// P-1：浮点是禁区。两条入口都必须挡住，不能截断成整数。
func TestFloatRejected(t *testing.T) {
	if _, err := Float(1.5); !errors.Is(err, ErrFloat) {
		t.Fatalf("Float 构造应报 ErrFloat，got %v", err)
	}
	var v any
	if err := json.Unmarshal([]byte(`{"x":1.5}`), &v); err != nil {
		t.Fatal(err)
	}
	if _, err := FromAny(v); !errors.Is(err, ErrFloat) {
		t.Fatalf("FromAny 遇浮点应报 ErrFloat，got %v", err)
	}
	// float64 直接传进来也要挡住（防止调用方忘了 UseNumber）
	if _, err := FromAny(map[string]any{"x": 1.5}); !errors.Is(err, ErrFloat) {
		t.Fatalf("float64 应报 ErrFloat，got %v", err)
	}
}

// 同数据 ⇒ 同字节（这是哈希链成立的前提）。
func TestDeterministic(t *testing.T) {
	build := func() Value {
		return Obj(
			"z", Arr(Int(1), Str("a"), Null(), Bool(true)),
			"a", Obj("k2", Int(-7), "k1", Str(`引号"与\反斜杠`)),
		)
	}
	first := string(build().Bytes())
	for i := 0; i < 100; i++ {
		if got := string(build().Bytes()); got != first {
			t.Fatalf("不稳定：\n%s\n%s", first, got)
		}
	}
}

// 字符串转义走 encoding/json，两个实现不许有第二套规则。
func TestStringEscaping(t *testing.T) {
	v := Str("换行\n制表\t引号\"反斜杠\\")
	out := string(v.Bytes())
	var back string
	if err := json.Unmarshal([]byte(out), &back); err != nil {
		t.Fatalf("转义后不是合法 JSON：%s (%v)", out, err)
	}
	if back != "换行\n制表\t引号\"反斜杠\\" {
		t.Fatalf("往返失真：%q", back)
	}
}

// 合法 JSON 往返：FromAny → Bytes → FromAny 应完全一致。
func TestRoundTrip(t *testing.T) {
	src := `{"b":[1,2,{"c":null}],"a":true}`
	var v any
	dec := json.NewDecoder(strings.NewReader(src))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	first, err := FromAny(v)
	if err != nil {
		t.Fatal(err)
	}
	b1 := first.Bytes()

	var v2 any
	dec2 := json.NewDecoder(strings.NewReader(string(b1)))
	dec2.UseNumber()
	if err := dec2.Decode(&v2); err != nil {
		t.Fatal(err)
	}
	second, err := FromAny(v2)
	if err != nil {
		t.Fatal(err)
	}
	if string(second.Bytes()) != string(b1) {
		t.Fatalf("往返不稳定：\n%s\n%s", b1, second.Bytes())
	}
}
