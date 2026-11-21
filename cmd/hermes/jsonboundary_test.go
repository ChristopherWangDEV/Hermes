package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// 非有限值必须是**运行期**算出来的：`0/0` 作为常量表达式在 Go 里是编译错误，
// 写不出来。这几个小函数只是让测试读起来像它想表达的东西。
func f64NaN() float64    { z := 0.0; return z / z }
func f64PosInf() float64 { z := 0.0; return 1.0 / z }
func f64NegInf() float64 { z := 0.0; return -1.0 / z }

// TestRejectNonFinite 守「NaN / ±Inf 只在序列化边界拒」这条裁决的**执行侧**。
//
// 为什么要有这条测试：v2.0.0 起语言的浮点类型是 IEEE-754 binary64 double，
// 运行期确实可能算出 NaN / ±Inf；rejectNonFinite 负责在序列化边界把这些
// 非有限值拒掉。直接喂值是为了稳定覆盖每个分支，不依赖某条具体的运行期
// 算式 —— 边界一旦失守，这个测试就是"有人守着"的证据。
func TestRejectNonFinite(t *testing.T) {
	cases := []struct {
		name string
		v    any
		// wantPath 是错误里必须出现的位置串；空串表示"应当放行"。
		wantPath string
		wantWhat string
	}{
		{name: "顶层 NaN", v: f64NaN(), wantPath: "value", wantWhat: "NaN"},
		{name: "顶层 +Inf", v: f64PosInf(), wantPath: "value", wantWhat: "+Inf"},
		{name: "顶层 -Inf", v: f64NegInf(), wantPath: "value", wantWhat: "-Inf"},
		{
			name:     "数组里的 NaN 要报出下标",
			v:        []any{int64(1), f64NaN()},
			wantPath: "value[1]",
			wantWhat: "NaN",
		},
		{
			name: "嵌套对象里的 Inf 要报出路径",
			v: map[string]any{
				"a": map[string]any{"b": []any{f64PosInf()}},
			},
			wantPath: "value.a.b[0]",
			wantWhat: "+Inf",
		},
		// 放行侧：有限值与所有非浮点类型都必须过。
		{name: "有限浮点", v: 1.5},
		{name: "有限浮点嵌在结构里", v: map[string]any{"x": []any{1.5, int64(2), "s", true, nil}}},
		{name: "整棵树没有浮点", v: map[string]any{"a": int64(1), "b": []any{"x", false}}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := rejectNonFinite(c.v)
			if c.wantPath == "" {
				if err != nil {
					t.Fatalf("不该被拒，实际：%v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("应当被拒（期望位置 %s）", c.wantPath)
			}
			msg := err.Error()
			if !strings.Contains(msg, c.wantPath) {
				t.Errorf("错误里没报出位置 %q：%s", c.wantPath, msg)
			}
			if !strings.Contains(msg, c.wantWhat) {
				t.Errorf("错误里没报出 %q：%s", c.wantWhat, msg)
			}
			// 必须说清"怎么办"，否则用户只知道不能序列化，不知道该怎么改。
			if !strings.Contains(msg, "handle the non-finite case") {
				t.Errorf("错误里缺改法：%s", msg)
			}
		})
	}
}

// TestRejectNonFiniteIsDeterministic 守遍历顺序。
//
// map 的遍历顺序在 Go 里是随机的：同一份结果两次运行若报出不同的字段，
// 排查的人会以为是两个不同的问题。所以键必须排序。
func TestRejectNonFiniteIsDeterministic(t *testing.T) {
	v := map[string]any{"zz": f64NaN(), "aa": f64NaN(), "mm": f64NaN()}

	first := ""
	for i := 0; i < 50; i++ {
		err := rejectNonFinite(v)
		if err == nil {
			t.Fatal("应当被拒")
		}
		if first == "" {
			first = err.Error()
			continue
		}
		if err.Error() != first {
			t.Fatalf("同一份结果报出了不同的位置：\n  第一次: %s\n  第 %d 次: %s", first, i+1, err.Error())
		}
	}
	if !strings.Contains(first, "value.aa") {
		t.Fatalf("排序后应稳定报最小的键 aa，实际：%s", first)
	}
}

// TestEncodingJSONWordingIsWhyWeGuard 说明这道守卫**为什么存在**：
// encoding/json 自己也会拒 NaN，但吐的是标准库内部措辞 —— 它既说不出
// 是哪一项（下面用一个不可能撞词的对象键来证明），也给不出改法。
// 这条测试把"我们换掉的是哪句话"钉住，否则下一个人会觉得
// rejectNonFinite 是多余的一层，顺手删掉。
func TestEncodingJSONWordingIsWhyWeGuard(t *testing.T) {
	v := map[string]any{"order_status_field": f64NaN()}
	_, err := json.Marshal(v)
	if err == nil {
		t.Fatal("encoding/json 应当拒绝 NaN")
	}
	if !strings.Contains(err.Error(), "unsupported value") {
		t.Fatalf("标准库措辞变了（%v）—— 这条测试的前提需要复核", err)
	}
	if strings.Contains(err.Error(), "order_status_field") {
		t.Fatalf("标准库居然报出了字段名（%v）—— 那 rejectNonFinite 的价值需要重新论证", err)
	}
}
