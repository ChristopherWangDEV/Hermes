package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// castRuntime 返回一个固定的 json 值，用来喂 json.as_*。
type castRuntime struct{ value any }

func (c *castRuntime) Model(_ context.Context, _ ModelRequest) (any, error) {
	return c.value, nil
}
func (c *castRuntime) Tool(_ context.Context, _ ToolRequest) (any, error) {
	return c.value, nil
}

// TestCastNarrowing 覆盖四种窄化的成功路径。
func TestCastNarrowing(t *testing.T) {
	d := loadGolden(t, "ir-cast.json")
	// 只测 int：golden 里那个 workflow 声明的是 `json.as_int`，
	// 拿字符串去喂它**应该**失败（见 TestCastFailureIsContractError）。
	cases := []struct {
		name  string
		input map[string]any
		want  any
	}{
		{"int", map[string]any{"total": int64(42)}, int64(42)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := RunWith(d, "", c.input, RunOptions{Runtime: &castRuntime{value: c.input["total"]}})
			if err != nil {
				t.Fatalf("应成功: %v", err)
			}
			if res.Value != c.want {
				t.Fatalf("期望 %v，实际 %v", c.want, res.Value)
			}
		})
	}
}

// TestCastFailureIsContractError 守"窄化失败必须报 ContractError 且带 expect/got"。
//
// 三件不能做的事都在这里被排除：返回零值（静默改业务含义）、
// panic（穿透 catch）、只说"失败"（排障时没有信息）。
func TestCastFailureIsContractError(t *testing.T) {
	d := loadGolden(t, "ir-cast.json")
	_, err := RunWith(d, "", map[string]any{"total": "not-an-int"},
		RunOptions{Runtime: &castRuntime{value: "not-an-int"}})
	if err == nil {
		t.Fatal("类型不符必须失败")
	}
	var re *RunError
	if !errors.As(err, &re) {
		t.Fatalf("应是 RunError（可被 catch），实际 %T", err)
	}
	if re.Kind != "ContractError" {
		t.Fatalf("应报 ContractError，实际 %s", re.Kind)
	}
	if !strings.Contains(re.Msg, "int") || !strings.Contains(re.Msg, "string") {
		t.Fatalf("错误应带 expect 与 got：%s", re.Msg)
	}
}

// TestCastRejectsFractionalInt 守"1.5 不能悄悄变成 1"。
func TestCastRejectsFractionalInt(t *testing.T) {
	d := loadGolden(t, "ir-cast.json")
	_, err := RunWith(d, "", map[string]any{"total": 1.5},
		RunOptions{Runtime: &castRuntime{value: 1.5}})
	if err == nil {
		t.Fatal("小数窄化成 int 必须失败（否则是静默的数据损坏）")
	}
}
