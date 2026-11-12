package main

// jsonboundary.go —— 结果**序列化边界**上的唯一一道非有限值检查。
//
// 裁决（LANGUAGE.md §4.4，2026-11-10）：`NaN` / `±Inf` **只在序列化边界拒绝**。
// 构造、传递、比较都不拒 —— 一个 float 在计算中途短暂变成 NaN（`0/0`、
// `Inf - Inf`）是完全正常的，拒在中间等于把这些表达式本身禁掉；而 JSON
// 根本没有 NaN/Inf 的表示，写到边界上不存在"合法的输出"，只能拒。
//
// 为什么不能只靠 encoding/json：它确实会拒，但吐的是
// `json: unsupported value: NaN` —— 既不说**哪一项**，也不说**怎么办**。
// 这里换成人话，并**把策略钉死在一个函数里**：
// `tests/guard/nonfinite_boundary_test.go` 扫全仓，断言 `math.IsNaN` /
// `math.IsInf` 只出现在本文件。别处再加一道，就说明有人把"只在边界拒"
// 悄悄改成了"到处拒"，那条守卫会立刻变红。
//
// v2.0.0 现状：浮点类型已落地为 `double`（IEEE-754 binary64），IR 常量用
// 位模式 `{"k":"double","bits":"<16 位小写 hex>"}`，运行期值就是 float64。
// 于是 `0.0/0.0`、`Inf - Inf` 这类表达式能真实产生 NaN / ±Inf —— 这条边界
// 检查不再是摆设，而是**唯一**拦截它们的地方。IR 内部 / 构造 / 传递 / 比较
// 仍然一律不拒（裁决是位置，不是值）。

import (
	"fmt"
	"math"
	"sort"
	"strconv"
)

// rejectNonFinite 递归检查一个待序列化的值，遇到 NaN / ±Inf 就返回具名错误。
//
// 遍历顺序是**确定的**（map 键排序）：同一份结果两次运行必须报同一个位置，
// 否则"报了哪一项"就成了随机信息，排查时会以为是两个不同的问题。
func rejectNonFinite(v any) error {
	return walkFinite(v, "value")
}

func walkFinite(v any, path string) error {
	switch x := v.(type) {
	case float64:
		switch {
		case math.IsNaN(x):
			return nonFiniteErr(path, "NaN")
		case math.IsInf(x, 1):
			return nonFiniteErr(path, "+Inf")
		case math.IsInf(x, -1):
			return nonFiniteErr(path, "-Inf")
		}
	case []any:
		for i, e := range x {
			if err := walkFinite(e, path+"["+strconv.Itoa(i)+"]"); err != nil {
				return err
			}
		}
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := walkFinite(x[k], path+"."+k); err != nil {
				return err
			}
		}
	}
	// 其余类型（string / bool / int64 / nil / json.Number / 结构体）不可能
	// 是非有限浮点，直接放行。刻意不递归进 struct：值域由 IR 的 json 子集
	// 限定，多写一层只会掩盖"某处塞了个不认识的类型"这件事。
	return nil
}

func nonFiniteErr(path, what string) error {
	return fmt.Errorf("result contains %s at %s: JSON has no representation for NaN or Inf, "+
		"so the value cannot be serialised; handle the non-finite case before returning it "+
		"(clamp it, or return an error instead of the value)", what, path)
}
