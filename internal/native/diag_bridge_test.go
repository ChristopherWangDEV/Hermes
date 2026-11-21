package native

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// 这组测试是阶段 2b 的**切换前置条件**：在把渲染权从 Go catalog 交给 C++ 之前，
// 先证明两者对**全部已登记码 × 两种语言**产出逐字节相同的文本。
//
// 为什么必须全量而不是抽几条：诊断骨架里有一堆条件分支（有无 path、有无 snippet、
// why 为空、fix 条数 0/1/多、有无 notes、docs 锚点有无），抽样很容易刚好绕开
// 某个分支，而那条分支恰恰是用户会遇到的那一种。

// loadGoldens 读出全部 golden（每个 (code, lang) 一份）。
//
// 2c 之后 Go catalog 已删除，**golden 是唯一判据**：它是 2026-11-17 用当时的
// Go 渲染结果冻结的，含 code/lang/loc/args 与期望的 message/why/fix/text。
func loadGoldens(t *testing.T) []goldenRecord {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "tests", "diag", "golden", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("找不到 golden（tests/diag/golden）：%v", err)
	}
	out := make([]goldenRecord, 0, len(files))
	for _, f := range files {
		blob, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		var rec goldenRecord
		if err := json.Unmarshal(blob, &rec); err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		out = append(out, rec)
	}
	return out
}

// TestDiagRenderMatchesGolden 是渲染判据：C++ 渲染必须与 golden 逐字节相同。
func TestDiagRenderMatchesGolden(t *testing.T) {
	goldens := loadGoldens(t)
	compared := 0
	for _, g := range goldens {
		got, err := RenderDiagText(DiagRenderInput{
			Code: g.Code, Severity: "error",
			Module: g.Module, Path: g.Path,
			Line: g.Line, Col: g.Col, EndLine: g.EndLine, EndCol: g.EndCol,
			Args: g.Args, Lang: g.Lang,
		})
		if err != nil {
			t.Fatalf("%s/%s: 渲染失败: %v", g.Code, g.Lang, err)
		}
		compared++
		if got != g.Text {
			t.Errorf("%s/%s: 与 golden 不一致\n--- golden ---\n%s\n--- C++ ---\n%s", g.Code, g.Lang, g.Text, got)
		}
	}
	t.Logf("渲染等价：已比对 %d 份 golden", compared)
}

// TestDiagFieldsMatchGolden 是字段判据：message/why/fix 必须与 golden 相同。
func TestDiagFieldsMatchGolden(t *testing.T) {
	goldens := loadGoldens(t)
	compared := 0
	for _, g := range goldens {
		got, err := RenderDiagFields(DiagRenderInput{
			Code: g.Code, Severity: "error",
			Module: g.Module, Path: g.Path,
			Line: g.Line, Col: g.Col, EndLine: g.EndLine, EndCol: g.EndCol,
			Args: g.Args, Lang: g.Lang,
		})
		if err != nil {
			t.Fatalf("%s/%s: 字段渲染失败: %v", g.Code, g.Lang, err)
		}
		compared++
		if got.Message != g.Message {
			t.Errorf("%s/%s: message 不一致 golden=%q cpp=%q", g.Code, g.Lang, g.Message, got.Message)
		}
		if got.Why != g.Why {
			t.Errorf("%s/%s: why 不一致 golden=%q cpp=%q", g.Code, g.Lang, g.Why, got.Why)
		}
		if len(got.Fix) != len(g.Fix) {
			t.Errorf("%s/%s: fix 条数 %d vs %d", g.Code, g.Lang, len(got.Fix), len(g.Fix))
			continue
		}
		for i := range g.Fix {
			if got.Fix[i] != g.Fix[i] {
				t.Errorf("%s/%s: fix[%d] 不一致 golden=%q cpp=%q", g.Code, g.Lang, i, g.Fix[i], got.Fix[i])
			}
		}
	}
	t.Logf("字段等价：已比对 %d 份 golden", compared)
}

// goldenRecord 是每个 (code, lang) 的**判据存档**。
//
// 为什么必须现在存：2c 会删掉 Go catalog，而上面两条等价测试原先拿 Go 侧
// 当 oracle。参照物被删掉之后，如果判据也跟着没了，就等于"删实现 = 删证据"。
// 所以先把 Go 的渲染结果冻结成 golden，之后 C++ 一直与 golden 比对。
type goldenRecord struct {
	Code     string            `json:"code"`
	Lang     string            `json:"lang"`
	Module   string            `json:"module"`
	Path     string            `json:"path"`
	Line     int               `json:"line"`
	Col      int               `json:"col"`
	EndLine  int               `json:"end_line"`
	EndCol   int               `json:"end_col"`
	Args     map[string]string `json:"args"`
	Message  string            `json:"message"`
	Why      string            `json:"why"`
	Fix      []string          `json:"fix"`
	Text     string            `json:"text"`
}

// goldenPath 是一条 (code, lang) 的存档路径。
func goldenPath(dir, code, lang string) string {
	return filepath.Join(dir, code+"."+lang+".json")
}
