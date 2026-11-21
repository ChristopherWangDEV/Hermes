package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"hermes/internal/ir"
	"hermes/internal/native"
)

// 这组测试原在 internal/ir（Go 装配器时代），守着类型表的四条性质：
// id 按声明顺序、内容摘要不含位置、未知 struct 被拒、类型表往返稳定。
//
// 装配搬到 C++ 之后，被测对象不再存在于 Go，所以断言改走**生产路径**
// （native.IRAssembleFinal）—— 否则删掉实现就等于删掉证据。

func assembleTypesOnly(t *testing.T, src string) *ir.Doc {
	t.Helper()
	sum := sha256.Sum256([]byte(src))
	facts := []byte(`{"modules":[{"builtin":false,"kind":"hermes","path":"t.hermes","sha256":"` +
		hex.EncodeToString(sum[:]) + `","source":` + mustJSONString(src) + `}],"requires":[]}`)
	irJSON, err := native.IRAssembleFinal([]byte(src), "t.hermes", facts)
	if err != nil {
		return nil
	}
	doc, _ := ir.Load(irJSON)
	return doc
}

func mustJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// id 按声明顺序、struct 引用解析成 id。
func TestTypeTableIDsFollowDeclarationOrder(t *testing.T) {
	doc := assembleTypesOnly(t, `export struct OrderInfo {
    string id;
}
export struct Intent {
    OrderInfo order;
}
export workflow w() entry -> string { return "ok"; }
`)
	if doc == nil || len(doc.Types) != 2 {
		t.Fatalf("应解析出 2 个类型，实际 %+v", doc)
	}
	if doc.Types[0].Name != "OrderInfo" || doc.Types[0].ID != 1 ||
		doc.Types[1].Name != "Intent" || doc.Types[1].ID != 2 {
		t.Fatalf("id 应按声明顺序 1,2：%+v", doc.Types)
	}
	f := doc.Types[1].Fields[0]
	if f.T.ID == nil || *f.T.ID != 1 {
		t.Fatalf("struct 引用应解析成 id 1，实际 %+v", f.T)
	}
}

// 内容摘要**不含位置**：同一份 struct 定义写在不同行，摘要必须相同。
// 带位置的话链接期比较两个库里的同名 struct 会永远"不一致"，ODR 直接失效。
func TestTypeHashIgnoresLocation(t *testing.T) {
	a := assembleTypesOnly(t, `export struct S {
    string id;
}
export workflow w() entry -> string { return "ok"; }
`)
	b := assembleTypesOnly(t, `
// 前面多几行注释，让同一个 struct 落在不同的行上

export struct S {
    string id;
}
export workflow w() entry -> string { return "ok"; }
`)
	if a == nil || b == nil {
		t.Fatal("两份源码都应能装配")
	}
	if a.Types[0].SHA256 != b.Types[0].SHA256 {
		t.Fatalf("类型摘要必须与位置无关：%q vs %q", a.Types[0].SHA256, b.Types[0].SHA256)
	}
}

// 未知 struct 引用必须被拒（前端报错，装配不产出 IR）。
func TestUnknownStructReferenceIsRejected(t *testing.T) {
	doc := assembleTypesOnly(t, `export struct S {
    Missing m;
}
export workflow w() entry -> string { return "ok"; }
`)
	if doc != nil {
		t.Fatalf("引用不存在的类型必须失败，实际装配成功：%+v", doc.Types)
	}
	_ = strings.TrimSpace
}
