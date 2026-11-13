package schema

import (
	"encoding/json"
	"strings"
	"testing"
)

func decode(t *testing.T, s string) any {
	t.Helper()
	var v any
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

const intentSchema = `{
  "type": "object",
  "properties": {
    "action": {"type": "string"},
    "order": {"$ref": "#/$defs/Order"}
  },
  "required": ["action", "order"],
  "additionalProperties": false,
  "$defs": {
    "Order": {
      "type": "object",
      "properties": {
        "order_id": {"type": "string"},
        "status": {"type": "integer"},
        "amount": {"type": "integer"}
      },
      "required": ["order_id", "status", "amount"],
      "additionalProperties": false
    }
  }
}`

// TestValidateAcceptsGood 是正常路径。
func TestValidateAcceptsGood(t *testing.T) {
	good := decode(t, `{"action":"refund","order":{"order_id":"A1","status":200,"amount":4999}}`)
	if err := Validate([]byte(intentSchema), good); err != nil {
		t.Fatalf("应通过，实际 %v", err)
	}
}

// TestValidateReportsPath 守"错误要指到字段"。
//
// 只说"schema 不匹配"等于没说：模型的输出是 JSON，而人要一眼看出是哪个字段。
func TestValidateReportsPath(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"缺必填", `{"action":"refund","order":{"order_id":"A1","amount":1}}`, "/order: missing required field status"},
		{"类型不对", `{"action":"refund","order":{"order_id":"A1","status":"200","amount":1}}`, "/order/status: expected integer, got string"},
		{"多余字段", `{"action":"refund","order":{"order_id":"A1","status":200,"amount":1},"x":1}`, "/: undeclared fields: x"},
		{"根类型不对", `"nope"`, "/: expected object, got string"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate([]byte(intentSchema), decode(t, c.body))
			if err == nil {
				t.Fatal("应失败")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误应包含 %q，实际 %q", c.want, err.Error())
			}
		})
	}
}

// TestValidateRefusesUnknownKeyword 守"不认识就报错，不跳过"。
//
// 跳过未知关键字 = 静默放行 —— 那比没有校验更危险：调用方以为校验过了。
func TestValidateRefusesUnknownKeyword(t *testing.T) {
	s := `{"type":"string","oneOf":[{"type":"string"}]}`
	err := Validate([]byte(s), decode(t, `"x"`))
	if err == nil || !strings.Contains(err.Error(), "oneOf") {
		t.Fatalf("不认识的关键字必须报错并点名，实际 %v", err)
	}
}

// TestValidateArray 覆盖 array/items（T42 之后 struct 字段可以是数组）。
func TestValidateArray(t *testing.T) {
	s := `{"type":"object","properties":{"tags":{"type":"array","items":{"type":"string"}}},"required":["tags"]}`
	if err := Validate([]byte(s), decode(t, `{"tags":["a","b"]}`)); err != nil {
		t.Fatalf("应通过: %v", err)
	}
	err := Validate([]byte(s), decode(t, `{"tags":["a",2]}`))
	if err == nil || !strings.Contains(err.Error(), "/tags/1") {
		t.Fatalf("应报出数组下标，实际 %v", err)
	}
}

// TestValidateEnumAndPattern 守 T12–T14 的新关键字。
//
// 这些关键字我们**生成器目前不产出**，但手写 schema 与未来扩展会用；
// 没有它们时校验器会报"不认识"——方向正确（不静默放行），但会挡住合法输入。
func TestValidateEnumAndPattern(t *testing.T) {
	cases := []struct {
		name   string
		schema string
		value  string
		ok     bool
	}{
		{"enum 命中", `{"type":"string","enum":["a","b"]}`, `"a"`, true},
		{"enum 未命中", `{"type":"string","enum":["a","b"]}`, `"c"`, false},
		{"enum 数字按值比", `{"type":"integer","enum":[1,2]}`, `1`, true},
		{"enum 空数组必须报错", `{"type":"string","enum":[]}`, `"a"`, false},
		{"pattern 命中", `{"type":"string","pattern":"^A[0-9]+$"}`, `"A102"`, true},
		{"pattern 未命中", `{"type":"string","pattern":"^A[0-9]+$"}`, `"B1"`, false},
		{"minimum 满足", `{"type":"integer","minimum":10}`, `10`, true},
		{"minimum 违反", `{"type":"integer","minimum":10}`, `9`, false},
		{"maximum 违反", `{"type":"integer","maximum":10}`, `11`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate([]byte(c.schema), decode(t, c.value))
			if c.ok && err != nil {
				t.Fatalf("应通过，实际 %v", err)
			}
			if !c.ok && err == nil {
				t.Fatal("应失败")
			}
		})
	}
}
