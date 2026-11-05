package ir

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"hermes/internal/diag"
)

// unmarshalForTest 用 UseNumber 解回来：默认解码会把整数变成 float64，
// 而 canon 会（正确地）拒绝浮点 —— 那样这条"幂等"测试会红在一个假问题上。
func unmarshalForTest(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(v)
}

// TestEmitIsCanonical 证明写出的是**字节序键**的 canonical JSON。
//
// 这不是审美：`ir_sha256` 就是这几个字节的哈希，键序随插入顺序变化
// 会让同一份 IR 在不同编译器/不同版本下算出不同哈希，签名链当场失效。
func TestEmitIsCanonical(t *testing.T) {
	d := loadBase(t)
	out, err := Emit(d)
	if err != nil {
		t.Fatalf("Emit 失败: %v", err)
	}

	if !bytes.HasPrefix(out, []byte(`{"backends"`)) {
		t.Fatalf("键序不是字节序（第一个键应是 backends）：\n%.80s", out)
	}
	if bytes.ContainsAny(out, "\n\t") {
		t.Fatalf("canonical JSON 不该有换行或制表符：\n%s", out)
	}
	// 幂等：再解一次再写一次，字节必须相同。
	var back Doc
	if err := unmarshalForTest(out, &back); err != nil {
		t.Fatalf("Emit 的产出解不回来: %v", err)
	}
	again, err := Emit(&back)
	if err != nil {
		t.Fatalf("二次 Emit 失败: %v", err)
	}
	if !bytes.Equal(out, again) {
		t.Fatalf("Emit 不是幂等的：\n%s\n%s", out, again)
	}
}

// TestEmitRoundTripValidates 是"写出去的东西自己读得回来"。
//
// 自举信任从零开始（IR.md §12 结尾）：我们刚产出的 IR 也要过全部 V 检查，
// 否则"编译器产出了坏 IR"这件事只有等第三方加载时才会暴露。
func TestEmitRoundTripValidates(t *testing.T) {
	out, err := Emit(loadBase(t))
	if err != nil {
		t.Fatal(err)
	}
	d, ds := Load(out)
	if len(ds) != 0 {
		t.Fatalf("自己写出的 IR 自己读不合格：%s", diag.RenderAll(ds))
	}
	if d == nil {
		t.Fatal("Load 返回 nil Doc")
	}
	if got := d.Workflows[0].Name; got != "f" {
		t.Fatalf("往返后 workflow 名变了: %q", got)
	}
}

// TestEmitSortsSchemaJSON 守住 V-8 的写侧。
//
// V-8 要求 `schemas[].json` 重新 canonical 化后与原文逐字节相等。写出的那一刻
// 就是唯一能保证这件事的时机：一旦写出时键序不规范化，V-8 会在**下游**报错，
// 而那时看到的是一份别人产出的 IR，根本不知道是谁写坏的。
func TestEmitSortsSchemaJSON(t *testing.T) {
	d := loadBase(t)
	d.Schemas = []SchemaDoc{{
		ID:   "Intent",
		Root: "Intent",
		JSON: []byte(`{"type":"object","properties":{"z":{"type":"string"},"a":{"type":"integer"}}}`),
	}}
	out, err := Emit(d)
	if err != nil {
		t.Fatal(err)
	}
	// canonical 之后 properties 的键序必须是 a, z。
	if !strings.Contains(string(out), `"properties":{"a"`) {
		t.Fatalf("schema 内层键没有被规范化：\n%s", out)
	}
}

// TestEmitRejectsFloat 证明写侧也挡浮点（读侧 V-7 已挡一次）。
func TestEmitRejectsFloat(t *testing.T) {
	d := loadBase(t)
	// Lit 的 v 是 RawMessage，能塞进任意 JSON——包括浮点。
	d.Workflows[0].Nodes = append(d.Workflows[0].Nodes, Node{
		ID: 1, K: "lit", Loc: Loc{M: 0, L: 2, C: 1, EL: 2, EC: 3},
		LitT: strptr("float"), LitV: []byte(`1.5`),
	})
	_, err := Emit(d)
	if err == nil {
		t.Fatal("浮点字面量应该被写出侧拒绝（P-1），而不是原样写进 IR")
	}
	if !strings.Contains(err.Error(), "float") && !strings.Contains(err.Error(), "浮点") {
		t.Fatalf("错误信息没说清是浮点问题: %v", err)
	}
}

func strptr(s string) *string { return &s }
