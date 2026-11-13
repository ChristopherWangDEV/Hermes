package ir

import (
	"strings"
	"testing"

	"hermes/internal/diag"
)

// 版本闸：**不做向前兼容**。
//
// 用户口径：2.X 只兼容 2.0.0 及之后的 IR，2.0.0 是长期支持版本。1.x 的产物
// 必须被**明确拒绝**，不写迁移路径 —— 按 2.0 的字段语义去解释一份 1.x 产物，
// 得到的是"看起来对的错答案"，比报错危险得多。
//
// 这两条同时是「1.x 不兼容」的可执行证据，不只是口头声明。
func TestLoadRejectsUnsupportedVersions(t *testing.T) {
	cases := []struct{ name, old, new string }{
		{"ir_version_1", `"ir_version": 2`, `"ir_version": 1`},
		{"language_version_1", `"language_version":2`, `"language_version":1`},
		{"ir_version_3", `"ir_version": 2`, `"ir_version": 3`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := strings.Replace(baseIR, tc.old, tc.new, 1)
			if bad == baseIR {
				t.Fatalf("替换没生效（baseIR 里找不到 %q）：这条测试会假绿", tc.old)
			}
			d, ds := Load([]byte(bad))
			if !hasCode(ds, diag.CodeIRVersion) {
				t.Fatalf("期望 E_IR_VERSION，实际：%s", codes(ds))
			}
			// 版本不对就**不再往下走**：后续阶段都在按 2.0 的字段语义解释
			// 这份产物，多报十条悬空诊断只会盖住唯一真正的原因。
			if len(ds) != 1 {
				t.Fatalf("版本闸应该只报 1 条（后面不再跑），实际 %d 条：%s", len(ds), codes(ds))
			}
			if d == nil {
				t.Fatal("Load 仍应返回解析出的 Doc（调用方决定怎么处置）")
			}
		})
	}
}

// 反向：合法版本必须一条 E_IR_VERSION 都没有（防"无论如何都拒绝"的假绿）。
func TestLoadAcceptsSupportedVersions(t *testing.T) {
	_, ds := Load([]byte(baseIR))
	if hasCode(ds, diag.CodeIRVersion) {
		t.Fatalf("ir_version=%d / language_version=%d 不该被拒：%s",
			IRVersionSupported, IRVersionSupported, codes(ds))
	}
}

func TestLoadAcceptsBaseIR(t *testing.T) {
	d, ds := Load([]byte(baseIR))
	if len(ds) != 0 {
		t.Fatalf("baseIR 应无诊断，实际 %d 条：%s", len(ds), diag.RenderAll(ds))
	}
	if d == nil {
		t.Fatal("合法的 IR 应该返回非 nil 的 Doc")
	}
	if len(d.Workflows) != 1 || d.Workflows[0].Name != "f" {
		t.Errorf("解析结果不对：%+v", d.Workflows)
	}
}

// V-7：浮点在**字段**里（不是 RawMessage）也能被 json.Unmarshal 抓到，
// 但抓到的错误信息是 Go 的语法细节（"cannot unmarshal number 1.5 into ..."），
// 不是我们要的那条。所以 scanFloats 必须抢在 Unmarshal 之前。
func TestLoadRejectsFloatInIntField(t *testing.T) {
	bad := strings.Replace(baseIR, `"max_steps":1`, `"max_steps":1.5`, 1)
	_, ds := Load([]byte(bad))
	if !hasCode(ds, diag.CodeFloat) {
		t.Fatalf("期望 E_IR_FLOAT，实际：%s", codes(ds))
	}
	if hasCode(ds, diag.CodeLoadFailed) {
		t.Error("有浮点时应只报 E_IR_FLOAT，不该同时报 E_IR_LOAD_FAILED（抢活会让同一份坏输入有两条诊断）")
	}
}

// V-7 的关键场景：浮点在 json.RawMessage 里，**Unmarshal 完全看不见**。
// economics 与 schemas[].json 就是这种字段，而它们会进哈希。
func TestLoadRejectsFloatInsideRawMessage(t *testing.T) {
	bad := strings.Replace(baseIR, `"economics":{}`, `"economics":{"cost_estimate":0.25}`, 1)
	_, ds := Load([]byte(bad))
	if !hasCode(ds, diag.CodeFloat) {
		t.Fatalf("RawMessage 里的浮点没被抓住（这正是 json.Unmarshal 的盲区）：%s", codes(ds))
	}
	// 路径要能定位到具体位置，不能只说"IR 里有浮点"。
	found := false
	for _, d := range ds {
		if d.Code == diag.CodeFloat && strings.Contains(d.Loc.Path, "economics.cost_estimate") {
			found = true
		}
	}
	if !found {
		t.Errorf("浮点诊断没指出精确路径，实际：%s", diag.RenderAll(ds))
	}
}

// 指数写法也算浮点（`1e3` 在 JSON 里是浮点数）。
func TestLoadRejectsExponentNotation(t *testing.T) {
	bad := strings.Replace(baseIR, `"max_spans":0`, `"max_spans":1e3`, 1)
	_, ds := Load([]byte(bad))
	if !hasCode(ds, diag.CodeFloat) {
		t.Errorf("1e3 应被判为浮点，实际：%s", codes(ds))
	}
}

// 整数不能误报：`1`、`-1`、`0` 都要放行。
func TestScanFloatsAllowsIntegers(t *testing.T) {
	cases := []string{`{"a":1}`, `{"a":-1}`, `{"a":0}`, `{"a":[1,2,3]}`}
	for _, c := range cases {
		if hits := scanFloats([]byte(c)); len(hits) != 0 {
			t.Errorf("%s 被误报为浮点：%v", c, hits)
		}
	}
}

// 诊断顺序必须**确定**：同一份坏 IR 加载两次，输出逐字节相同。
// 这条守的是 scanFloats 里"map 键要排序"那件事 —— 不排序就是随机顺序，
// golden 不可比，diff 也没有意义。
func TestScanFloatsOrderIsDeterministic(t *testing.T) {
	bad := `{"z":1.5,"a":2.5,"m":{"y":3.5,"b":4.5},"list":[5.5,6.5]}`
	first := scanFloats([]byte(bad))
	if len(first) != 6 {
		t.Fatalf("应有 6 处浮点，实际 %d：%v", len(first), first)
	}
	for i := 0; i < 20; i++ {
		again := scanFloats([]byte(bad))
		for j := range first {
			if again[j] != first[j] {
				t.Fatalf("第 %d 次扫描顺序与第一次不同（map 迭代没排序）：\n first=%v\n again=%v", i, first, again)
			}
		}
	}
	// 键序断言：a 在 m 在 z 之前
	if first[0].Path != "a" {
		t.Errorf("首个浮点路径 = %q，期望 a（字节序排最前）", first[0].Path)
	}
}

// 形状错误（该是整数的地方给了字符串）要报 E_IR_LOAD_FAILED，且不崩。
func TestLoadReportsShapeError(t *testing.T) {
	bad := strings.Replace(baseIR, `"ir_version": 2`, `"ir_version": "one"`, 1)
	_, ds := Load([]byte(bad))
	if !hasCode(ds, diag.CodeLoadFailed) {
		t.Fatalf("期望 E_IR_LOAD_FAILED，实际：%s", codes(ds))
	}
	for _, d := range ds {
		if d.Why == "" || len(d.Fix) == 0 {
			t.Errorf("E_IR_LOAD_FAILED 也必须有 why 与 fix：%+v", d)
		}
	}
}

// 非 JSON 输入：不能让 panic 漏出去。
func TestLoadHandlesGarbage(t *testing.T) {
	for _, bad := range []string{``, `{`, `not json`, `{"a":`, "\x00\x01"} {
		d, ds := Load([]byte(bad))
		if d != nil {
			t.Errorf("%q 不该解析出 Doc", bad)
		}
		if !hasCode(ds, diag.CodeLoadFailed) {
			t.Errorf("%q 应报 E_IR_LOAD_FAILED，实际：%s", bad, codes(ds))
		}
	}
}

// Load 与 Validate 必须用同一套 option（语言、上限）。
func TestLoadHonorsOptions(t *testing.T) {
	bad := strings.Replace(baseIR, `"max_steps":1`, `"max_steps":1.5`, 1)
	_, ds := Load([]byte(bad), WithLang(diag.LangZH))
	if len(ds) == 0 {
		t.Fatal("应至少一条诊断")
	}
	zh := false
	for _, d := range ds {
		if strings.Contains(d.Message, "浮点") {
			zh = true
		}
	}
	if !zh {
		t.Errorf("WithLang(zh) 没传到阶段 0：%s", diag.RenderAll(ds))
	}
}
