// Package schema 校验一个 JSON 值是否满足 `schemas[]` 里那份 JSON Schema。
//
// 为什么必须自己写而不是引第三方：整个项目零第三方 Go 依赖是红线（§1.5），
// 而且我们**只**需要校验自己生成的那一小撮关键字 —— 引一个完整实现等于为了
// 3 个关键字拉进几千行别人维护的代码。
//
// 覆盖范围就是 `internal/ir/schema.go` 会生成的那些：object（properties /
// required / additionalProperties）、array（items）、string、integer、boolean。
// 遇到不认识的关键字**明确报错**，不跳过 —— 跳过等于"校验通过"，
// 而"通过了但没真校验"比没校验更危险。
package schema

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Violation 是一条校验失败：位置 + 原因。
//
// 位置用 JSON Pointer 形式（`/order/amount`），因为模型的输出本来就是 JSON，
// 而"哪个字段不对"必须一眼可见 —— 只说"schema 不匹配"等于没说。
type Violation struct {
	Path   string
	Reason string
}

func (v Violation) Error() string { return v.Path + ": " + v.Reason }

// Validate 用 schemaJSON 校验 value。
func Validate(schemaJSON []byte, value any) error {
	var s map[string]any
	dec := json.NewDecoder(strings.NewReader(string(schemaJSON)))
	dec.UseNumber()
	if err := dec.Decode(&s); err != nil {
		return fmt.Errorf("schema is not valid JSON: %w", err)
	}
	defs, _ := s["$defs"].(map[string]any)
	return validate(s, value, "", defs)
}

// validate 递归校验。defs 是根部的 `$defs`，`$ref` 只允许指向它。
func validate(s map[string]any, v any, path string, defs map[string]any) error {
	// `$ref` 优先：有它时其余关键字按 JSON Schema 语义应被忽略，
	// 而我们的生成器正是用 `{"$ref": "#/$defs/X"}` 表达"嵌套的具名结构体"。
	if ref, ok := s["$ref"].(string); ok {
		target, err := resolveRef(ref, defs)
		if err != nil {
			return Violation{pathOrRoot(path), err.Error()}
		}
		return validate(target, v, path, defs)
	}
	// 先认 type：它是唯一必须存在的关键字（我们生成的 schema 都有）。
	t, _ := s["type"].(string)
	if t == "" {
		return Violation{pathOrRoot(path), "schema is missing type (the validator refuses to guess)"}
	}
	if err := checkType(t, v, path); err != nil {
		return err
	}
	// 逐关键字处理，遇到不认识的**报错**。
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	sort.Strings(keys) // 顺序固定：同输入同报错，便于测试与 diff
	for _, k := range keys {
		switch k {
		case "enum":
			// 枚举：值必须**恰好等于**其中之一。按 canonical 字节比较，
			// 不用 DeepEqual —— 后者对 1 与 1.0 会给出不同结论，而 JSON 里它们是同一个数。
			vals, ok := s[k].([]any)
			if !ok {
				return Violation{pathOrRoot(path), "enum is not an array"}
			}
			if len(vals) == 0 {
				return Violation{pathOrRoot(path), "enum cannot be empty (an empty enum rejects everything and is almost always a mistake)"}
			}
			if !inEnum(vals, v) {
				return Violation{pathOrRoot(path), "not in enum: " + renderValue(v)}
			}
		case "pattern":
			pat, ok := s[k].(string)
			if !ok {
				return Violation{pathOrRoot(path), "pattern is not a string"}
			}
			str, ok := v.(string)
			if !ok {
				return Violation{pathOrRoot(path), "pattern only applies to string, got " + typeName(v)}
			}
			re, err := regexp.Compile(pat)
			if err != nil {
				return Violation{pathOrRoot(path), "pattern is not a valid regex: " + err.Error()}
			}
			if !re.MatchString(str) {
				return Violation{pathOrRoot(path), "does not match pattern " + pat}
			}
		case "minimum", "maximum":
			n, err := asInt64(v)
			if err != nil {
				return Violation{pathOrRoot(path), k + " only supports integers (IR forbids floating point, §3.2 P-1)"}
			}
			lim, ok := numToInt64(s[k])
			if !ok {
				return Violation{pathOrRoot(path), k + " is not an integer"}
			}
			if k == "minimum" && n < lim {
				return Violation{pathOrRoot(path), fmt.Sprintf("below minimum %d (got %d)", lim, n)}
			}
			if k == "maximum" && n > lim {
				return Violation{pathOrRoot(path), fmt.Sprintf("above maximum %d (got %d)", lim, n)}
			}
		case "type", "$defs", "$schema", "title", "description":
			// $defs 是定义容器，不约束当前值；其余是注释性关键字（不影响校验）。
		case "properties":
			props, ok := s[k].(map[string]any)
			if !ok {
				return Violation{pathOrRoot(path), "properties is not an object"}
			}
			obj, ok := v.(map[string]any)
			if !ok {
				return Violation{pathOrRoot(path), "expected object, got " + typeName(v)}
			}
			for _, name := range sortedNames(props) {
				sub := props[name]
				sm, ok := sub.(map[string]any)
				if !ok {
					return Violation{pathOrRoot(path), "field " + name + " has a non-object schema"}
				}
				if fv, present := obj[name]; present {
					if err := validate(sm, fv, path+"/"+name, defs); err != nil {
						return err
					}
				}
			}
		case "required":
			req, ok := s[k].([]any)
			if !ok {
				return Violation{pathOrRoot(path), "required is not an array"}
			}
			obj, ok := v.(map[string]any)
			if !ok {
				return Violation{pathOrRoot(path), "expected object, got " + typeName(v)}
			}
			for _, r := range req {
				name, _ := r.(string)
				if _, present := obj[name]; !present {
					return Violation{pathOrRoot(path), "missing required field " + name}
				}
			}
		case "additionalProperties":
			allowed, ok := s[k].(bool)
			if !ok {
				return Violation{pathOrRoot(path), "additionalProperties only supports booleans"}
			}
			if allowed {
				continue
			}
			obj, ok := v.(map[string]any)
			if !ok {
				return Violation{pathOrRoot(path), "expected object, got " + typeName(v)}
			}
			props, _ := s["properties"].(map[string]any)
			extra := make([]string, 0)
			for name := range obj {
				if _, known := props[name]; !known {
					extra = append(extra, name)
				}
			}
			if len(extra) > 0 {
				sort.Strings(extra)
				return Violation{pathOrRoot(path), "undeclared fields: " + strings.Join(extra, ", ")}
			}
		case "items":
			sub, ok := s[k].(map[string]any)
			if !ok {
				return Violation{pathOrRoot(path), "items is not an object"}
			}
			arr, ok := v.([]any)
			if !ok {
				return Violation{pathOrRoot(path), "expected array, got " + typeName(v)}
			}
			for i, e := range arr {
				if err := validate(sub, e, fmt.Sprintf("%s/%d", path, i), defs); err != nil {
					return err
				}
			}
		default:
			return Violation{pathOrRoot(path), "validator does not recognize keyword " + k + " (refusing to skip: skipping means allowing)"}
		}
	}
	return nil
}

func checkType(t string, v any, path string) error {
	ok := false
	switch t {
	case "object":
		_, ok = v.(map[string]any)
	case "array":
		_, ok = v.([]any)
	case "string":
		_, ok = v.(string)
	case "boolean":
		_, ok = v.(bool)
	case "integer":
		switch n := v.(type) {
		case json.Number:
			_, err := n.Int64()
			ok = err == nil
		case float64:
			// 只按整数值接受：1.0 是整数语义，1.5 不是。
			ok = n == float64(int64(n))
		case int:
			ok = true
		case int64:
			ok = true
		}
	default:
		return Violation{pathOrRoot(path), "validator does not support type " + t}
	}
	if !ok {
		return Violation{pathOrRoot(path), "expected " + t + ", got " + typeName(v)}
	}
	return nil
}

func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case json.Number, float64, int, int64:
		return "number"
	default:
		return fmt.Sprintf("%T", v)
	}
}

func pathOrRoot(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

// resolveRef 解析 `#/$defs/Name`。只支持这一种形式。
//
// 为什么不去支持外部文件引用：生成的 schema 是自包含的（`$defs` 全在根里），
// 支持更多形式等于为不存在的需求写代码，还会让"从哪儿加载"变成安全问题。
func resolveRef(ref string, defs map[string]any) (map[string]any, error) {
	const prefix = "#/$defs/"
	if !strings.HasPrefix(ref, prefix) {
		return nil, fmt.Errorf("only #/$defs/... $refs are supported, got %q", ref)
	}
	name := strings.TrimPrefix(ref, prefix)
	d, ok := defs[name].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("$ref points to a nonexistent definition %q", name)
	}
	return d, nil
}

// sortedNames 让遍历顺序固定：同输入同报错，便于测试与 diff。
func sortedNames(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sameJSON 判断两个 JSON 值是否相等，数字按**数值**比（1 与 1.0 是同一个数）。
func sameJSON(a, b any) bool {
	an, aIsNum := numToInt64OK(a)
	bn, bIsNum := numToInt64OK(b)
	if aIsNum && bIsNum {
		return an == bn
	}
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ab) == string(bb)
}

func numToInt64OK(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	case float64:
		return int64(n), n == float64(int64(n))
	case int:
		return int64(n), true
	case int64:
		return n, true
	default:
		return 0, false
	}
}

func numToInt64(v any) (int64, bool) { return numToInt64OK(v) }

func asInt64(v any) (int64, error) {
	n, ok := numToInt64OK(v)
	if !ok {
		return 0, fmt.Errorf("not an integer")
	}
	return n, nil
}

// renderValue 给错误信息用：截断长值，避免把整个响应塞进一行诊断。
func renderValue(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	s := string(b)
	if len(s) > 60 {
		return s[:57] + "..."
	}
	return s
}

func inEnum(vals []any, v any) bool {
	for _, want := range vals {
		if sameJSON(want, v) {
			return true
		}
	}
	return false
}
