package importhomo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hermes/internal/native"
)

// realPlan 是**真的** `homonoia compile --emit --json` 输出，原样存进 testdata。
//
// 刻意不手写 fixture：手写的计划会按"我以为上游长什么样"来写，于是导入器
// 与上游一起漂移而测试全绿。这份文件是用
//
//	homonoia compile <部署脚本> --emit --json
//
// 抓下来的，形状由上游决定而不是由我决定。
func realPlan(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "plan-two-nodes.json"))
	if err != nil {
		t.Fatalf("读不到 testdata 里的真实计划: %v", err)
	}
	return data
}

// parseGenerated 用**真实前端**解析生成的源码，返回错误级诊断的条数。
//
// 这是本文件最重要的一条断言：导入器的产物必须是一个合法的 `.hermes` 文件。
// 用前端而不是正则去判，是因为"看起来像 .hermes"与"能被编译"是两件事，
// 而只有后者对下游有用。
func parseGenerated(t *testing.T, src []byte) (string, string) {
	t.Helper()
	r, err := native.Parse(src, "imported.hermes", false)
	if err != nil {
		t.Fatalf("前端拒绝解析生成的源码: %v", err)
	}
	return r.Dump, r.Diag
}

// TestEmitProducesValidHermes 证明产物能被真实前端接受。
func TestEmitProducesValidHermes(t *testing.T) {
	steps, err := Parse(realPlan(t))
	if err != nil {
		t.Fatalf("解析真实计划失败: %v", err)
	}
	res := Emit(steps)

	dump, diags := parseGenerated(t, res.Source)
	if diags != "[]" && diags != "" {
		t.Fatalf("生成的 .hermes 有诊断：%s\n--- 源码 ---\n%s", diags, res.Source)
	}
	if !strings.Contains(dump, "WorkflowDecl") {
		t.Fatalf("生成的源码里没有 workflow 声明：\n%s", dump)
	}
}

// TestEmitIsDeterministic 是 T43 ⑨：两次导入同一 plan 字节相等。
//
// 不做这条的后果不是"文件丑一点"，而是 `source_sha256` 每次都变，
// 于是"同一份计划 ⇒ 同一份产物"这条可重现性质在导入这一步就断了。
func TestEmitIsDeterministic(t *testing.T) {
	steps, err := Parse(realPlan(t))
	if err != nil {
		t.Fatal(err)
	}
	first := Emit(steps)
	for i := 0; i < 20; i++ {
		again := Emit(steps)
		if string(again.Source) != string(first.Source) {
			t.Fatalf("两次导入字节不同（第 %d 次）", i)
		}
		if strings.Join(again.Warnings, "\n") != strings.Join(first.Warnings, "\n") {
			t.Fatalf("两次导入的 warning 顺序不同")
		}
	}
}

// TestEmitOnlyUnderstandsHttpAndModel 是 T43 ⑦：不解析 argv 语义。
//
// 真实计划里网关有 `--id`、`--model`、`--default-model`，节点里有
// `--config`、`--id`、`--bind-interface`。除了 `--http` 与 `--model`，
// 其余都必须变成一条 warning 而不是被"理解"。
func TestEmitOnlyUnderstandsHttpAndModel(t *testing.T) {
	steps, err := Parse(realPlan(t))
	if err != nil {
		t.Fatal(err)
	}
	res := Emit(steps)

	joined := strings.Join(res.Warnings, "\n")
	for _, flag := range []string{"--id", "--config", "--bind-interface", "--default-model"} {
		if !strings.Contains(joined, flag) {
			t.Errorf("未识别的 %s 没有产生 warning，说明有代码在偷偷解释它\n实际 warning:\n%s", flag, joined)
		}
	}
	for _, flag := range []string{"--http", "--model"} {
		if strings.Contains(joined, "丢弃未识别参数 "+flag+"（") {
			t.Errorf("被识别的 %s 进了 warning：\n%s", flag, joined)
		}
	}
}

// TestEmitModelsAreCommentsOnly 是 T43 ⑧：模型清单只作注释。
//
// 判据是"可执行部分里不含模型 id"：注释里写了什么无所谓，
// 但源码里一旦把模型写进去，运行期就会以它为准，而事实会与 /v1/models 分叉。
func TestEmitModelsAreCommentsOnly(t *testing.T) {
	steps, err := Parse(realPlan(t))
	if err != nil {
		t.Fatal(err)
	}
	res := Emit(steps)

	var code []string
	for _, line := range strings.Split(string(res.Source), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" && !strings.HasPrefix(trimmed, "//") {
			code = append(code, line)
		}
	}
	executable := strings.Join(code, "\n")
	if !strings.Contains(string(res.Source), "chat = ") {
		t.Fatalf("模型清单没有作为注释出现：\n%s", res.Source)
	}
	if strings.Contains(executable, "tiny.gguf") {
		t.Fatalf("模型路径进了可执行部分（运行期会以它为准，与 /v1/models 分叉）：\n%s", executable)
	}
}

// TestEmitWithoutHttpOmitsBackend 证明缺 `--http` 时不编一个地址出来。
//
// "没有"与"默认值"必须区分：编一个 127.0.0.1 进产物，是把导入器的猜测
// 变成运行期事实，而那个端口上多半什么都没有。
func TestEmitWithoutHttpOmitsBackend(t *testing.T) {
	steps := []Step{{Kind: "gateway", Argv: []string{"homonoia", "gateway", "--model", "chat=/x.gguf"}}}
	res := Emit(steps)
	if strings.Contains(string(res.Source), "backend homonoia") {
		t.Fatalf("计划里没有 --http，却生成了 backend 声明：\n%s", res.Source)
	}
	if strings.Contains(string(res.Source), "backend = homonoia") {
		t.Fatalf("没有 backend 声明却引用了它：\n%s", res.Source)
	}
	if _, diags := parseGenerated(t, res.Source); diags != "[]" && diags != "" {
		t.Fatalf("无 backend 的产物解析有诊断：%s", diags)
	}
}

// TestParseRejectsUnknownFields 证明上游加字段时我们会红，而不是静默少导。
func TestParseRejectsUnknownFields(t *testing.T) {
	_, err := Parse([]byte(`[{"kind":"gateway","argv":["h"],"surprise":1}]`))
	if err == nil {
		t.Fatal("未知字段应报错：静默忽略会让\"导入少了一半\"看起来像\"上游没写\"")
	}
}
