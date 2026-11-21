// Package guard_test —— diag_shape_test.go（M0 守卫）
//
// 守编译器诊断的「形状」红线，不锁具体文本、不锁逐字快照（那些都不在本期范围内，
// 且会随文案演进频繁误报）：
//
//	(i)   每条诊断都有非空 why（"为什么错"必须存在）；
//	(ii)  每条诊断都至少有一条非空 fix（"怎么改"必须存在，且不能是空串）；
//	(iii) 每条诊断的 code 匹配 ^(E|W)[0-9]{4}$（错误/警告码的格式契约）。
//
// 获取诊断的方式：直接用原生解析器（internal/native）跑几个会出错的 .hermes 小源，
// 把产出的诊断 JSON 数组反序列化后逐条校验。覆盖多个错误码（E1001/E4016/E4029 等），
// 但不对具体码值做白名单——新增码不会让本测试变脆。
package guard_test

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"hermes/internal/native"
)

// nativeDiagLite 只取本测试关心的字段；与原生 JSON 对齐，多出的字段会被 JSON 忽略。
type nativeDiagLite struct {
	Severity string   `json:"severity"`
	Code     string   `json:"code"`
	Why      string   `json:"why"`
	Fix      []string `json:"fix"`
}

// codeRE 是诊断码的格式契约：E/W 前缀 + 4 位数字。
var codeRE = regexp.MustCompile(`^(E|W)[0-9]{4}$`)

// diagsFrom 用原生解析器解析一个会出错的源，返回反序列化后的诊断列表。
func diagsFrom(t *testing.T, src string, isHeader bool) []nativeDiagLite {
	t.Helper()
	res, err := native.Parse([]byte(src), "guard.hermes", isHeader)
	if err != nil {
		t.Fatalf("native.Parse 不应失败（诊断走 Diag 通道）: %v", err)
	}
	if strings.TrimSpace(res.Diag) == "" || res.Diag == "[]" {
		return nil
	}
	var ds []nativeDiagLite
	if e := json.Unmarshal([]byte(res.Diag), &ds); e != nil {
		t.Fatalf("诊断 JSON 解析失败: %v\nraw=%s", e, res.Diag)
	}
	return ds
}

func TestDiagShapeInvariants(t *testing.T) {
	// 多个会出错的源，覆盖不同错误码；互不依赖，某条源没出诊断才需要报警。
	sources := []struct {
		name     string
		src      string
		isHeader bool
	}{
		// 字段名拼错 → E4016（且应带 did-you-mean 的 fix2）。
		{
			"field-miss",
			"export struct OrderInfo { int order_id; string status; }\n" +
				"export workflow w(input: OrderInfo) -> string {\n" +
				"  auto x = input.statuss;\n  return \"x\";\n}\n",
			false,
		},
		// 顶层关键字拼错 → E1001（带 did-you-mean 的 fix2）。
		{"keyword-miss", "wihle x = 1;\n", false},
		// v2.0.0：double 字面量已放开，改用 double 相等（E4033）验证 why/fix 形状。
		{"double-equality", "export struct S { double x; }\nexport workflow w(input: S) -> bool { return input.x == input.x; }\n", false},
		// workflow 名用引号 → E2002。
		{"quoted-name", "workflow \"x\" () -> string;\n", true},
		// 一个源累积多条诊断：两个字段名都拼错，各产出 E4016（都带 why/fix）。
		// 注意：这里刻意避开会触发「匿名结构体」等历史_parser 路径（如 `struct { int x; };`），
		// 那些路径的某些 E1001 缺少 why/fix，属于本期 M1/M2 范围之外的既有缺口，
		// 由专门的 parser 守卫另行覆盖，本测试只校验「有 why/fix」这一形状契约。
		{
			"multi",
			"export struct OrderInfo { int order_id; string status; }\n" +
				"export workflow w(input: OrderInfo) -> string {\n" +
				"  auto a = input.statuss;\n" +
				"  auto b = input.amunt;\n" +
				"  return \"x\";\n}\n",
			false,
		},
	}

	total := 0
	for _, s := range sources {
		t.Run(s.name, func(t *testing.T) {
			ds := diagsFrom(t, s.src, s.isHeader)
			if len(ds) == 0 {
				t.Fatalf("源 %q 应至少产出一条诊断用于形状校验", s.name)
			}
			for i, d := range ds {
				if d.Why == "" {
					t.Errorf("[%s] 第 %d 条诊断 why 为空（code=%s）", s.name, i, d.Code)
				}
				if len(d.Fix) == 0 {
					t.Errorf("[%s] 第 %d 条诊断缺少 fix（code=%s）", s.name, i, d.Code)
				}
				for _, f := range d.Fix {
					if strings.TrimSpace(f) == "" {
						t.Errorf("[%s] 第 %d 条诊断含空 fix（code=%s）", s.name, i, d.Code)
					}
				}
				if !codeRE.MatchString(d.Code) {
					t.Errorf("[%s] 第 %d 条诊断 code=%q 不匹配 ^(E|W)[0-9]{4}$", s.name, i, d.Code)
				}
			}
		})
		total += len(diagsFrom(t, s.src, s.isHeader))
	}
	if total == 0 {
		t.Fatalf("守卫测试至少应累计到一条诊断；当前为 0（解析器可能没产出诊断）")
	}
}
