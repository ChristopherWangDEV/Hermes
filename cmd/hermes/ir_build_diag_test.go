package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hermes/internal/diag"
)

// 这几个测试守的是"诊断从原生层搬到终端"这一段路 —— 它不在编译器的
// 正中心，却决定用户能不能看懂自己的错误。三个具体的旧缺陷：
//
//  1. renderDiags 对所有原生诊断硬写 SeverityError，于是 W2004（警告）一
//     出现在 error 路径上就显示成 `error[W2004]`；
//  2. 只有警告时调用处直接 `hermes: <raw JSON>`，用户看到一整行 JSON；
//  3. functions 那一趟的诊断被统一标成 "functions failed to compile" ——
//     而它也会带回普通 workflow 体里的 E2015/E4024。
//
// 三条都不影响编译结果，所以只有测试能拦住它们复发。

const warnOnlyJSON = `[{"severity":"warning","code":"W2004",` +
	`"message":"max_steps = unlimited：这个 agent 的步骤上界不再是编译期常量",` +
	`"line":2,"col":8,"end_line":2,"end_col":17,"module":"a.hermes",` +
	`"why":"两个保证随它一起消失","fix":["给一个常量"]}]`

// TestRenderDiagsKeepsWarningSeverity 守"警告显示成 warning，而不是 error"。
func TestRenderDiagsKeepsWarningSeverity(t *testing.T) {
	out := renderDiags(warnOnlyJSON, []byte("a\nb\n"))
	if !strings.Contains(out, "warning[W2004]") {
		t.Fatalf("W2004 必须以 warning 渲染，实际：\n%s", out)
	}
	if strings.Contains(out, "error[W2004]") {
		t.Fatalf("警告被渲染成了错误：\n%s", out)
	}
	// 渲染过的文本不该还是一行 JSON：那正是缺陷 2 的症状。
	if strings.Contains(out, `"severity"`) {
		t.Fatalf("输出里还有原始 JSON 字段，说明没走渲染器：\n%s", out)
	}
	// why / help 必须保留：这条警告的全部价值在解释，不在"有个警告"。
	if !strings.Contains(out, "why:") || !strings.Contains(out, "help:") {
		t.Fatalf("渲染后丢了 why/help：\n%s", out)
	}
}

// TestRenderDiagsSeverityMapping 把四个取值钉死。
//
// 未知级别 → error 是**刻意的保守方向**：把一条本该阻塞的诊断当成 warning
// 会静默放行，反过来只是多报一次。这条断言存在的意义是让那个选择显式可见
// （改它就得改这里，而改的人会看到这段理由）。
func TestRenderDiagsSeverityMapping(t *testing.T) {
	cases := []struct {
		sev  string
		want string
	}{
		{"error", "error[X]"},
		{"warning", "warning[X]"},
		{"note", "note[X]"},
		{"", "error[X]"},      // 缺字段：保守当错误
		{"fatal", "error[X]"}, // 未知级别：保守当错误
	}
	for _, c := range cases {
		t.Run(c.sev, func(t *testing.T) {
			blob := `[{"severity":"` + c.sev + `","code":"X","message":"m","line":1,"col":1}]`
			out := renderDiags(blob, []byte("m\n"))
			if !strings.Contains(out, c.want) {
				t.Fatalf("severity=%q 应渲染成 %q，实际：\n%s", c.sev, c.want, out)
			}
		})
	}
}

// TestNativeHasError 守"只有警告 ≠ 失败"。
//
// 这是 ir_build.go 里 is-error 判定的唯一入口，也是"warnings-only build 必须
// 继续构建"和"有 error 必须拦"这两件事的分界点。
func TestNativeHasError(t *testing.T) {
	if nativeHasError(warnOnlyJSON) {
		t.Fatal("只有 warning 时不该判成有错误（那会把警告变成阻塞）")
	}
	if nativeHasError(`[]`) || nativeHasError("") {
		t.Fatal("空诊断集不是错误")
	}
	withErr := `[{"severity":"warning","code":"W1","message":"w"},` +
		`{"severity":"error","code":"E2015","message":"e"}]`
	if !nativeHasError(withErr) {
		t.Fatal("混了一条 error 时必须判成有错误")
	}
	// 解不动按"有错误"处理：宁可让用户贴出来，也不要拿一份读不懂的诊断继续构建。
	if !nativeHasError(`not json`) {
		t.Fatal("解不动的诊断应按有错误处理")
	}
	// 旧实现靠子串搜索，`"severity": "error"`（带空格）会漏判。这里守住它。
	spaced := `[{"severity": "error", "code":"E1", "message":"e"}]`
	if !nativeHasError(spaced) {
		t.Fatal("带空格的 JSON 里的 error 必须能识别（别用子串搜索）")
	}
}

// TestNativeDiagLabelNamesCodes 守"标签说的是真的"。
//
// 以前它硬写成 "functions failed to compile"，而 E2015/E4024 可能来自普通
// workflow 体。现在标签列出实际错误码，读者至少知道自己面对哪一类诊断。
func TestNativeDiagLabelNamesCodes(t *testing.T) {
	blob := `[{"severity":"error","code":"E2015","message":"retry(unlimited) 不合法"},` +
		`{"severity":"warning","code":"W2004","message":"w"}]`
	got := nativeDiagLabel(blob)
	if !strings.Contains(got, "E2015") {
		t.Fatalf("标签应点名实际的错误码，实际 %q", got)
	}
	if strings.Contains(got, "W2004") {
		t.Fatalf("标签不该把警告码算进错误清单，实际 %q", got)
	}
	if strings.Contains(got, "functions failed to compile") {
		t.Fatalf("标签不该断言「函数编译失败」（诊断可能来自 workflow 体），实际 %q", got)
	}
}

// TestDedupeDiagsKeepsDistinct 守"去重只合并逐字相同的，不合并看起来像的"。
//
// 这条是 dedupeDiags 的**反向**守卫，比正向更重要：正向（"重复的合成一条"）
// 如果实现成"总是合成一条"也会绿，而那会把不同位置的警告吃掉。
func TestDedupeDiagsKeepsDistinct(t *testing.T) {
	mk := func(sev diag.Severity, code, module string, line, col int, msg string) diag.Diag {
		return diag.Diag{
			Severity: sev, Code: code, Message: msg,
			Loc: diag.Loc{Module: module, Line: line, Col: col, EndLine: line, EndCol: col + 4},
		}
	}
	cases := []struct {
		name string
		in   []diag.Diag
		want int
	}{
		{
			name: "逐字相同 → 合成一条",
			in: []diag.Diag{
				mk(diag.SeverityWarning, "W5004", "a.hermes", 3, 22, "m"),
				mk(diag.SeverityWarning, "W5004", "a.hermes", 3, 22, "m"),
			},
			want: 1,
		},
		{
			name: "同一行不同列 → 两条",
			in: []diag.Diag{
				mk(diag.SeverityWarning, "W5004", "a.hermes", 3, 22, "m"),
				mk(diag.SeverityWarning, "W5004", "a.hermes", 3, 40, "m"),
			},
			want: 2,
		},
		{
			// 跨模块不合并：两个文件里第 3 行第 22 列可以是完全无关的两处。
			name: "同一位置不同模块 → 两条",
			in: []diag.Diag{
				mk(diag.SeverityWarning, "W5004", "a.hermes", 3, 22, "m"),
				mk(diag.SeverityWarning, "W5004", "b.hermes", 3, 22, "m"),
			},
			want: 2,
		},
		{
			// 严重级进 key：同位置的 warning 与 error 是两件事，合并会藏掉阻塞项。
			name: "同位置不同严重级 → 两条",
			in: []diag.Diag{
				mk(diag.SeverityWarning, "W5004", "a.hermes", 3, 22, "m"),
				mk(diag.SeverityError, "W5004", "a.hermes", 3, 22, "m"),
			},
			want: 2,
		},
		{
			name: "同位置不同码 → 两条",
			in: []diag.Diag{
				mk(diag.SeverityWarning, "W5004", "a.hermes", 3, 22, "m"),
				mk(diag.SeverityWarning, "W2003", "a.hermes", 3, 22, "m"),
			},
			want: 2,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, dropped := dedupeDiags(c.in, nil)
			if len(got) != c.want {
				t.Fatalf("应保留 %d 条，实际 %d 条（丢 %d）", c.want, len(got), dropped)
			}
			if dropped != len(c.in)-c.want {
				t.Fatalf("dropped 计数错：应 %d，实际 %d", len(c.in)-c.want, dropped)
			}
		})
	}
}

// TestDedupeDiagsSeenIsSharedAcrossCalls 守"seen 是跨调用累积的"。
//
// 这是跨趟去重的**唯一机制**：前端每个 C 入口各建一个 DiagBag，跨袋去重只能
// 发生在 Go 侧两次渲染调用之间。如果 seen 忘了传下去（或者每次都新建一张），
// 这个测试是唯一会红的地方 —— 端到端测试只看到"印了两遍"，看不到原因。
func TestDedupeDiagsSeenIsSharedAcrossCalls(t *testing.T) {
	seen := make(map[string]bool)
	d := diag.Diag{
		Severity: diag.SeverityWarning, Code: "W2003", Message: "m",
		Loc: diag.Loc{Module: "a.hermes", Line: 3, Col: 18, EndLine: 3, EndCol: 18},
	}
	first, _ := dedupeDiags([]diag.Diag{d}, seen)
	if len(first) != 1 {
		t.Fatalf("第一趟应保留，实际丢了")
	}
	second, dropped := dedupeDiags([]diag.Diag{d}, seen)
	if len(second) != 0 || dropped != 1 {
		t.Fatalf("第二趟应被合并掉，实际保留 %d 条、丢 %d 条", len(second), dropped)
	}
	// 换一张表就等于"另一次构建"：必须重新报，否则第二次编译会静默吞掉诊断。
	fresh, _ := dedupeDiags([]diag.Diag{d}, make(map[string]bool))
	if len(fresh) != 1 {
		t.Fatal("新的 seen 表应重新保留（跨构建绝不能共享）")
	}
}

// TestBuildDedupesCrossPassDiagnostics 是这条缺陷的**端到端**守卫。
//
// 缺陷现象：`if (input.status == 404)` 的 W5004 与 `#link <std/model>;` 的
// W2003 都会在终端印两遍。原因是入口模块被前端解析了两次（CodegenFunctions
// 一趟、CodegenWithExternals 一趟），两趟都把整袋诊断交回来，而 Go 侧分别渲染。
//
// 为什么用 `build` 而不是 `ir`：`ir` 把 IR JSON 打到标准输出，而 captureStderr
// 只截标准错误 —— 整个 JSON 会漏进测试日志。`build -o <临时文件>` 把产物写进文件，
// 标准错误只剩诊断。这里没有 error，所以 build 的默认截断（到第一条 error 即停）
// 不会启动，"几条"数出来的就是去重本身的成绩。
func TestBuildDedupesCrossPassDiagnostics(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "dup.hermes")
	const body = `#link <std/model>;

export struct Order { string id; int status; }
export workflow w(input: Order) entry -> string {
    if (input.status == 404) {
        return "missing";
    }
    return "ok";
}
`
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := captureStderr(t, func() int {
		return run([]string{"build", src, "-o", filepath.Join(dir, "dup.hbc")})
	})
	if code != 0 {
		t.Fatalf("只有警告时构建应退出 0，实际 %d\n%s", code, out)
	}
	// 三条都要在：去重不能顺手吃掉内容不同的诊断。
	for _, want := range []string{"warning[W2003]", "warning[W5004]", "allow_status"} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出里应含 %q：\n%s", want, out)
		}
	}
	// 每一条都只能出现一次。用 Count 而不是 Contains —— "印两遍"正是缺陷本身，
	// 用 Contains 的断言对这条缺陷**完全无感**。
	for _, code := range []string{"warning[W2003]", "warning[W5004]"} {
		if n := strings.Count(out, code); n != 1 {
			t.Fatalf("%s 应恰好出现 1 次，实际 %d 次：\n%s", code, n, out)
		}
	}
}
