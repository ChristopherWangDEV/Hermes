package ir

import (
	"bytes"
	"encoding/json"
	"fmt"

	"hermes/internal/canon"
)

// BuildSchemas 为类型表里的每个 struct 生成一份 JSON Schema（IR.md §3.5）。
//
// 为什么 schema 原文要进 IR 而不是运行期现算：
//  1. 审计者要能看到"到底发给上游的是什么"。现算则审计不可复现。
//  2. schema 字节进请求体 ⇒ 进 request_sha256。IR 里存一份，trace 就能引用哈希。
//  3. 运行期不依赖 codegen 逻辑（前端是 C++、运行器是 Go）。
//
// ⚠️ 与 LANGUAGE §8.3 的冲突（已记 D-028）：
// 那一节要求键序 type → properties → required → additionalProperties → $defs、
// 且 properties 内按**字段声明顺序**。但 V-8 要求 schemas[].json 重新 canonical
// 化后**字节相等**，而 canonical 的键序是**字节序**（$defs 在最前）。
// 两者不可同时成立。这里服从 canonical —— 哈希稳定性只能有一个规则，而 canonical
// 已经被 trace 哈希链与 .hbc 签名共用（单一实现点，ARCH §5.3）。
// 数组（required）的顺序不受影响，仍按字段声明顺序。
func BuildSchemas(types []TypeDef) ([]SchemaDoc, error) {
	if len(types) == 0 {
		return []SchemaDoc{}, nil
	}
	byID := make(map[int]string, len(types))
	for i := range types {
		byID[types[i].ID] = types[i].Name
	}

	out := make([]SchemaDoc, 0, len(types))
	for i := range types {
		v, err := objectSchema(&types[i], types, byID)
		if err != nil {
			return nil, err
		}
		out = append(out, SchemaDoc{
			ID:   types[i].Name,
			Root: types[i].Name,
			JSON: v.Bytes(),
		})
	}
	return out, nil
}

// objectSchema 生成一个 struct 的根 schema。
//
// $defs 放**本 unit 的全部其它类型**，不是只放被引用到的：schema 是给人审计的
// （"这个 unit 里还有一个 OrderInfo"本身就是信息），而按引用裁剪会让同一份类型表
// 产出两份不同的 $defs，它们的哈希就无法互相校验。
func objectSchema(t *TypeDef, all []TypeDef, byID map[int]string) (canon.Value, error) {
	props := make([]any, 0, len(t.Fields)*2)
	required := make([]canon.Value, 0, len(t.Fields))

	for i := range t.Fields {
		f := &t.Fields[i]
		ts, err := fieldSchema(f, byID)
		if err != nil {
			return canon.Value{}, fmt.Errorf("ir.BuildSchemas: type %q field %q: %w", t.Name, f.N, err)
		}
		props = append(props, f.N, ts)
		// **可选字段也进 required**（LANGUAGE §8.3 规则 6）：类型写成 anyOf[T, null]。
		// 这样"字段缺失"与"字段为 null"在协议层是同一件事。
		required = append(required, canon.Str(f.N))
	}

	kv := []any{
		"type", canon.Str("object"),
		"properties", canon.Obj(props...),
		"required", canon.Arr(required...),
		"additionalProperties", canon.Bool(false),
	}

	if len(all) > 1 {
		defs := make([]any, 0, (len(all)-1)*2)
		for i := range all {
			if all[i].Name == t.Name {
				continue
			}
			// $defs 内部的类型不再各自带 $defs（扁平表）：嵌套 $defs 会让同一类型
			// 在不同根 schema 里出现不同字节，V-8 的比对就失去意义。
			dv, err := objectSchemaShallow(&all[i], byID)
			if err != nil {
				return canon.Value{}, err
			}
			defs = append(defs, all[i].Name, dv)
		}
		kv = append(kv, "$defs", canon.Obj(defs...))
	}
	return canon.Obj(kv...), nil
}

// objectSchemaShallow 与 objectSchema 相同，但不生成 $defs（用于 $defs 内部）。
func objectSchemaShallow(t *TypeDef, byID map[int]string) (canon.Value, error) {
	props := make([]any, 0, len(t.Fields)*2)
	required := make([]canon.Value, 0, len(t.Fields))
	for i := range t.Fields {
		f := &t.Fields[i]
		ts, err := fieldSchema(f, byID)
		if err != nil {
			return canon.Value{}, fmt.Errorf("ir.BuildSchemas: type %q field %q: %w", t.Name, f.N, err)
		}
		props = append(props, f.N, ts)
		required = append(required, canon.Str(f.N))
	}
	return canon.Obj(
		"type", canon.Str("object"),
		"properties", canon.Obj(props...),
		"required", canon.Arr(required...),
		"additionalProperties", canon.Bool(false),
	), nil
}

// fieldSchema 是 IR 类型表达式 → JSON Schema 的映射（LANGUAGE §4.6）。
func fieldSchema(f *Field, byID map[int]string) (canon.Value, error) {
	var inner canon.Value

	switch f.T.K {
	case "string":
		inner = canon.Obj("type", canon.Str("string"))
	case "int":
		inner = canon.Obj("type", canon.Str("integer"))
	case "cost":
		// cost 是整数 micro-USD（D-018：金额禁 float）。schema 层只表达"整数"，
		// 单位写进 description —— 否则上游会以为这是美元。
		inner = canon.Obj("type", canon.Str("integer"), "description", canon.Str("micro-USD"))
	case "bool":
		inner = canon.Obj("type", canon.Str("boolean"))
	case "double":
		// 语言的 double 在 schema 层（对数量的描述）是 number；IR 自身的
		// "禁浮点"针对的是**常量编码**——位模式字符串，不是 JSON 数字。
		inner = canon.Obj("type", canon.Str("number"))
	case "json":
		// §8.4：默认显式 7 类型 union，不用 `{}`（空 schema 有些后端不收）。
		inner = canon.Obj("type", canon.Arr(
			canon.Str("object"), canon.Str("array"), canon.Str("string"),
			canon.Str("integer"), canon.Str("number"), canon.Str("boolean"), canon.Str("null"),
		))
	case "struct":
		if f.T.ID == nil {
			return canon.Value{}, fmt.Errorf("struct field is missing id")
		}
		name, ok := byID[*f.T.ID]
		if !ok {
			return canon.Value{}, fmt.Errorf("struct field references nonexistent type id %d", *f.T.ID)
		}
		inner = canon.Obj("$ref", canon.Str("#/$defs/"+name))
	case "array":
		// 元素类型在 Of 里（IR.md §3.1）。LANGUAGE §4.1 限制元素只能是 BaseType，
		// 但结构体字段的 `OrderInfo[]` 也合法 —— 所以这里递归生成，不假设是基础类型。
		// Of 里是**类型表达式**，不是 Field —— 解成 Field 会得到空类型，
		// 而报错是"未知类型 ''"，看起来像数据缺失。
		var sub TypeExpr
		if len(f.T.Of) == 0 {
			return canon.Value{}, fmt.Errorf("array is missing its element type")
		}
		if err := jsonDecode(f.T.Of, &sub); err != nil {
			return canon.Value{}, fmt.Errorf("array has a malformed of: %w", err)
		}
		// 注意用 `=` 而不是 `:=`：`:=` 会**遮蔽**外层的 inner，
		// 于是返回的还是零值 —— schema 里那个字段变成 null，而 IR 依然合法。
		elem, err := fieldSchema(&Field{T: sub}, byID)
		if err != nil {
			return canon.Value{}, err
		}
		inner = canon.Obj("type", canon.Str("array"), "items", elem)
	case "httpresult":
		// HttpResult<T> = { int status; T body; json headers; }（LANGUAGE §5.6）。
		// 三个字段一个都不能少：漏了 status，"订单不存在"的分支就无从判断。
		var sub TypeExpr
		if len(f.T.Of) == 0 {
			return canon.Value{}, fmt.Errorf("httpresult is missing its body type")
		}
		if err := jsonDecode(f.T.Of, &sub); err != nil {
			return canon.Value{}, fmt.Errorf("httpresult has a malformed of: %w", err)
		}
		body, err := fieldSchema(&Field{T: sub}, byID)
		if err != nil {
			return canon.Value{}, err
		}
		inner = canon.Obj(
			"type", canon.Str("object"),
			"properties", canon.Obj(
				"status", canon.Obj("type", canon.Str("integer")),
				"body", body,
				"headers", canon.Obj("type", canon.Str("object")),
			),
			"required", canon.Arr(canon.Str("status"), canon.Str("body"), canon.Str("headers")),
			"additionalProperties", canon.Bool(false),
		)
	case "tuple":
		return canon.Value{}, fmt.Errorf("tuple cannot be stored or named (IR.md §3.1); it must not appear in a field")
	default:
		return canon.Value{}, fmt.Errorf("unknown type %q", f.T.K)
	}

	if f.Opt {
		// §8.3 规则 6：可选字段写成 anyOf[T, null]。
		return canon.Obj("anyOf", canon.Arr(inner, canon.Obj("type", canon.Str("null")))), nil
	}
	return inner, nil
}

// jsonDecode 用 UseNumber 解：整数保持整数，不被悄悄变成 float64。
func jsonDecode(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	return dec.Decode(v)
}
