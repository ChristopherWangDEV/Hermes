package native

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"hermes/internal/schema"
)

// TestSchemaValidateMatchesGo 是 C++ 校验器的等价判据：
// 对 41 份 golden 里每一份 `schemas[].json`，喂同样的「合法值 / 缺失 / 类型错 /
// 多余键」样本，**错误文案必须与 Go 版逐字节相同**。
//
// 为什么连文案也比：调用方（CLI）会把 Violation.Error() 直接打给用户，
// 两侧文案不同就是用户可见的行为差异，而"都报错了"并不代表等价。
func TestSchemaValidateMatchesGo(t *testing.T) {
	root := filepath.Join("..", "..")
	goldens, err := filepath.Glob(filepath.Join(root, "tests", "ir", "golden", "*.json"))
	if err != nil || len(goldens) == 0 {
		t.Fatalf("找不到 IR golden: %v", err)
	}
	sort.Strings(goldens)

	compared := 0
	for _, g := range goldens {
		raw, err := os.ReadFile(g)
		if err != nil {
			continue
		}
		var doc struct {
			Schemas []struct {
				Root string          `json:"root"`
				JSON json.RawMessage `json:"json"`
			} `json:"schemas"`
		}
		if json.Unmarshal(raw, &doc) != nil || len(doc.Schemas) == 0 {
			continue
		}
		for _, s := range doc.Schemas {
			s := s
			t.Run(filepath.Base(g)+"/"+s.Root, func(t *testing.T) {
				valid := synthValid(t, s.JSON)
				cases := []struct {
					name  string
					value []byte
				}{
					{"valid", valid},
					{"missing", []byte(`{}`)},
					{"wrong_type", []byte(`[]`)},
					{"extra", append(append([]byte{}, valid[:len(valid)-1]...), []byte(`,"__x":1}`)...)},
				}
				for _, c := range cases {
					wantErr := schema.Validate(s.JSON, decodeAny(t, c.value))
					got, cerr := SchemaValidate(s.JSON, c.value)
					if cerr != nil {
						t.Fatalf("%s: C++ 报错（可能是未覆盖）: %v", c.name, cerr)
					}
					if !got.Covered {
						t.Skipf("%s: 该 schema 用了未覆盖关键字，回落 Go", c.name)
					}
					compared++
					wantText := ""
					if wantErr != nil {
						wantText = wantErr.Error()
					}
					if got.Violation != wantText {
						t.Errorf("%s: 文案不一致\nGo:  %q\nC++: %q", c.name, wantText, got.Violation)
					}
				}
			})
		}
	}
	if compared == 0 {
		t.Fatal("一组都没比到 —— 这条测试会空转")
	}
	t.Logf("schema 校验等价：已比对 %d 组", compared)
}

// synthValid 按 schema 造一个**通过校验**的最小值。
func synthValid(t *testing.T, schemaJSON []byte) []byte {
	t.Helper()
	var s map[string]any
	if err := json.Unmarshal(schemaJSON, &s); err != nil {
		return []byte(`{}`)
	}
	v := buildValue(s, s["$defs"])
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{}`)
	}
	return b
}

func buildValue(s map[string]any, defs any) any {
	if ref, ok := s["$ref"].(string); ok {
		if dm, ok := defs.(map[string]any); ok {
			name := ref[len("#/$defs/"):]
			if target, ok := dm[name].(map[string]any); ok {
				return buildValue(target, defs)
			}
		}
		return map[string]any{}
	}
	ty, _ := s["type"].(string)
	switch ty {
	case "object":
		out := map[string]any{}
		props, _ := s["properties"].(map[string]any)
		if req, ok := s["required"].([]any); ok {
			for _, r := range req {
				name, _ := r.(string)
				if pm, ok := props[name].(map[string]any); ok {
					out[name] = buildValue(pm, defs)
				}
			}
		}
		return out
	case "array":
		if items, ok := s["items"].(map[string]any); ok {
			return []any{buildValue(items, defs)}
		}
		return []any{}
	case "string":
		return "x"
	case "integer":
		return 1
	case "boolean":
		return true
	}
	return nil
}

func decodeAny(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("坏样本 %s: %v", b, err)
	}
	return v
}
