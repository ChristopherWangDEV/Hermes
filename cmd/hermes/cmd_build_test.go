package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hermes/internal/diag"
)

// TestBuildProducesStableArchive 是 R-HF-03 的端到端版本：
// 同一份源码两次 build，`.hbc` 字节相等。
//
// 这条比"IR 字节相等"更强：归档里还有索引、偏移、对齐填充 ——
// 任何一处不稳定都会让"同一份源码 ⇒ 同一份可分发产物"失效。
func TestBuildProducesStableArchive(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.hbc")
	b := filepath.Join(dir, "b.hbc")

	if code := run([]string{"build", "testdata/live.hermes", "-o", a}); code != 0 {
		t.Fatalf("第一次 build 退出码 %d", code)
	}
	if code := run([]string{"build", "testdata/live.hermes", "-o", b}); code != 0 {
		t.Fatalf("第二次 build 退出码 %d", code)
	}
	x, _ := os.ReadFile(a)
	y, _ := os.ReadFile(b)
	if !bytes.Equal(x, y) {
		t.Fatalf("两次 build 的 .hbc 字节不同（%d vs %d）", len(x), len(y))
	}
}

// TestArExtractMatchesGolden：拆出来的成员字节必须与 golden IR 一致。
//
// 这条把"归档只是容器"钉死：容器可以加索引、加填充、加签名，
// 但成员字节不许被它改动 —— 否则 ir_sha256 与签名会跟着变。
func TestArExtractMatchesGolden(t *testing.T) {
	dir := t.TempDir()
	hbc := filepath.Join(dir, "x.hbc")
	if code := run([]string{"build", "testdata/live.hermes", "-o", hbc}); code != 0 {
		t.Fatalf("build 退出码 %d", code)
	}
	out := filepath.Join(dir, "m.json")
	if code := run([]string{"ar", "x", hbc, "live", "-o", out}); code != 0 {
		t.Fatalf("ar x 退出码 %d", code)
	}
	got, _ := os.ReadFile(out)
	want, err := os.ReadFile("../../schemas/golden/ir-live.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("拆出的成员与 golden 不一致（%d vs %d 字节）", len(got), len(want))
	}
}

// TestArRejectsCorruptFile 守"损坏的归档不许被当成好的读".
func TestArRejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	hbc := filepath.Join(dir, "x.hbc")
	if code := run([]string{"build", "testdata/live.hermes", "-o", hbc}); code != 0 {
		t.Fatal("build 失败")
	}
	data, _ := os.ReadFile(hbc)
	data[0] = 'X' // 破坏 magic
	if err := os.WriteFile(hbc, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"ar", "t", hbc}); code != 4 {
		t.Fatalf("损坏的归档应退出 4，实际 %d", code)
	}
}

// TestArListShowsSymbols 证明 `ar t` 真的列了符号（而不只是成员名）。
func TestArListShowsSymbols(t *testing.T) {
	dir := t.TempDir()
	hbc := filepath.Join(dir, "x.hbc")
	if code := run([]string{"build", "testdata/live.hermes", "-o", hbc}); code != 0 {
		t.Fatal("build 失败")
	}
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	code := run([]string{"ar", "t", hbc})
	os.Stdout = old
	w.Close()

	buf := make([]byte, 1<<16)
	n, _ := r.Read(buf)
	r.Close()
	out := string(buf[:n])

	if code != 0 {
		t.Fatalf("ar t 退出码 %d", code)
	}
	for _, want := range []string{"live", "workflow", "type"} {
		if !strings.Contains(out, want) {
			t.Fatalf("ar t 输出缺少 %q：\n%s", want, out)
		}
	}
}

// TestBuildWarnsOnHTTPStatusCompare 是 W5004 的端到端形式，也是
// `--deny-warnings` 的**第二个**真实用例（此前只有 W2004 能测）。
//
// 三件事一起钉住：
//  1. 警告真的发出来了（`if (order.status == 404)` 默认契约下永不成立）；
//  2. 它**不阻塞**：默认退出 0、写出归档 —— 因为这是启发式，规范 §12.1 L3
//     明确承认判不了，不能假装判定；
//  3. `--deny-warnings` 能把它升级成失败（退出 4、不写归档）—— §12.1 L3
//     的「何时收紧」一栏承诺的就是这个。
func TestBuildWarnsOnHTTPStatusCompare(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "status.hermes")
	const body = `export struct Order { string id; int status; }
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

	outOK := filepath.Join(dir, "ok.hbc")
	code, out := captureStderr(t, func() int {
		return run([]string{"build", src, "-o", outOK})
	})
	if code != 0 {
		t.Fatalf("W5004 是警告，默认构建应退出 0，实际 %d\n%s", code, out)
	}
	if !strings.Contains(out, "warning[W5004]") {
		t.Fatalf("应发出 W5004：\n%s", out)
	}
	// 改法必须出现在输出里 —— 一条只说"错了"不说"怎么改"的警告是噪声。
	if !strings.Contains(out, "allow_status") {
		t.Fatalf("W5004 的 help 应给出 allow_status 的改法：\n%s", out)
	}
	if _, err := os.Stat(outOK); err != nil {
		t.Fatalf("默认构建应写出归档：%v", err)
	}

	outDeny := filepath.Join(dir, "deny.hbc")
	if code, out := captureStderr(t, func() int {
		return run([]string{"build", src, "-o", outDeny, "--deny-warnings"})
	}); code != 4 {
		t.Fatalf("--deny-warnings 应把 W5004 升级为退出 4，实际 %d\n%s", code, out)
	}
	if _, err := os.Stat(outDeny); err == nil {
		t.Fatal("--deny-warnings 失败时不应写出归档")
	}
}

// TestBuildDenyWarnings：`max_steps = unlimited` 会发 W2004。默认只打印警告、
// 退出 0 且写出归档；--deny-warnings 时退出 4 且**不写归档**（失败的构建不该留产物）。
func TestBuildDenyWarnings(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "unlim.hermes")
	raw, err := os.ReadFile("testdata/agent.hermes")
	if err != nil {
		t.Fatal(err)
	}
	warn := strings.Replace(string(raw), "max_steps = 4", "max_steps = unlimited", 1)
	if warn == string(raw) {
		t.Fatal("fixture 替换没生效：agent.hermes 里找不到 'max_steps = 4'")
	}
	if err := os.WriteFile(src, []byte(warn), 0o644); err != nil {
		t.Fatal(err)
	}

	// 默认：警告不阻塞，产出归档。
	outOK := filepath.Join(dir, "ok.hbc")
	if code := run([]string{"build", src, "-o", outOK}); code != 0 {
		t.Fatalf("默认 build 应退出 0（警告不阻塞），实际 %d", code)
	}
	if _, err := os.Stat(outOK); err != nil {
		t.Fatalf("默认 build 应写出归档：%v", err)
	}

	// --deny-warnings：退出 4，且不写归档。
	outDeny := filepath.Join(dir, "deny.hbc")
	if code := run([]string{"build", src, "-o", outDeny, "--deny-warnings"}); code != 4 {
		t.Fatalf("--deny-warnings 应退出 4，实际 %d", code)
	}
	if _, err := os.Stat(outDeny); err == nil {
		t.Fatal("--deny-warnings 失败时不应写出归档")
	}
}

// TestBuildKeepGoing 守 --keep-going 的两条口径（docs/LANGUAGE.md §11.3 第 4 条）：
//
//   - 默认：**只报第一条 error** 就停（严格按规范，不是"报完这一轮的全部"）；
//   - --keep-going：一轮把该阶段能恢复的诊断全部报出来（上限 50 条）。
//
// fixture 是三条各自独立报错的语句，前端一轮能恢复出 5 条诊断
// （tests/compiler/parser_test.cpp 的 test_keep_going 用同一种源锁定了
// "一次报出多条"这件事）。断言数的是 `error[` 出现次数，不数行数。
//
// 顺带钉住一件与上色有关的事：captureStderr 把 os.Stderr 换成管道，管道不是
// 字符设备，所以 ColorAuto 在这里一定退化成纯文本 —— 断言才敢按可见文本匹配。
func TestBuildKeepGoing(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "bad.hermes")
	// 引号包住 workflow 名（E2002）、缺函数体（E3005）、匿名 struct（E1001）、
	// backend 缺 '='（E1001）—— 彼此独立，前端不会因为一条报错就停摆。
	const body = "workflow \"a\" ();\nstruct { int x; };\nbackend foo\n"
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	countErr := func(s string) int { return strings.Count(s, "error[") }

	// 默认：退出 4，只渲染第一条 error，并留下"还有 N 条"的提示。
	code, out := captureStderr(t, func() int {
		return run([]string{"build", src, "-o", filepath.Join(dir, "a.hbc")})
	})
	if code != 4 {
		t.Fatalf("默认 build 应退出 4，实际 %d\n%s", code, out)
	}
	if n := countErr(out); n != 1 {
		t.Fatalf("默认口径只该渲染 1 条 error，实际 %d 条：\n%s", n, out)
	}
	if !strings.Contains(out, "suppressed") {
		t.Fatalf("默认截断必须提示还有未显示的诊断：\n%s", out)
	}

	// --keep-going：退出 4，把这一轮能恢复的诊断全部渲染出来，不再提示截断。
	code, out = captureStderr(t, func() int {
		return run([]string{"build", src, "-o", filepath.Join(dir, "b.hbc"), "--keep-going"})
	})
	if code != 4 {
		t.Fatalf("--keep-going build 应退出 4，实际 %d\n%s", code, out)
	}
	if n := countErr(out); n < 3 {
		t.Fatalf("--keep-going 应报出 >= 3 条 error，实际 %d 条：\n%s", n, out)
	}
	if strings.Contains(out, "suppressed") {
		t.Fatalf("--keep-going 下 5 条诊断远未到 50 条上限，不该提示截断：\n%s", out)
	}

	// 两种口径都不该留下归档 —— 编译失败不产产物。
	for _, name := range []string{"a.hbc", "b.hbc"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Fatalf("编译失败时不应写出 %s", name)
		}
	}
}

// TestTruncateDiags 是 --keep-going 口径的单元级判据，补 TestBuildKeepGoing
// 覆盖不到的两条边：warnings 排在 error 之前不能被吃掉、50 条上限真的生效。
//
// 用合成诊断而不是造 51 个语法错误：上限是**渲染策略**，与诊断怎么产生无关，
// 造 51 条真错误只会让这条测试变慢且脆弱。
func TestTruncateDiags(t *testing.T) {
	mk := func(sev diag.Severity, n int) []diag.Diag {
		ds := make([]diag.Diag, n)
		for i := range ds {
			ds[i] = diag.Diag{Severity: sev, Code: "E1001", Message: "x"}
		}
		return ds
	}

	savedMax, savedStop := diagMaxShown, diagStopAtFirstError
	defer func() { diagMaxShown, diagStopAtFirstError = savedMax, savedStop }()

	// 1) 默认口径：error 之前若有 warning，warning 要留住，然后停在第一条 error。
	diagMaxShown, diagStopAtFirstError = 0, true
	mixed := append(mk(diag.SeverityWarning, 2), mk(diag.SeverityError, 3)...)
	got, dropped := truncateDiags(mixed)
	if len(got) != 3 || dropped != 2 {
		t.Fatalf("默认口径应保留 2 条 warning + 1 条 error（共 3，丢 2），实际保留 %d 丢 %d", len(got), dropped)
	}
	if got[2].Severity != diag.SeverityError {
		t.Fatalf("保留的最后一条应是 error，实际 %v", got[2].Severity)
	}

	// 2) 默认口径 + 整轮只有 warning：一条都不许丢（警告不该被 error 规则误伤）。
	got, dropped = truncateDiags(mk(diag.SeverityWarning, 4))
	if len(got) != 4 || dropped != 0 {
		t.Fatalf("纯 warning 不该被截断，实际保留 %d 丢 %d", len(got), dropped)
	}

	// 3) --keep-going：50 条上限。
	diagMaxShown, diagStopAtFirstError = 50, false
	got, dropped = truncateDiags(mk(diag.SeverityError, 51))
	if len(got) != 50 || dropped != 1 {
		t.Fatalf("--keep-going 上限 50，实际保留 %d 丢 %d", len(got), dropped)
	}

	// 4) 默认值（0,false）= 不截断，与 ir / compile / demo 的旧行为逐字节一致。
	diagMaxShown, diagStopAtFirstError = 0, false
	got, dropped = truncateDiags(mk(diag.SeverityError, 7))
	if len(got) != 7 || dropped != 0 {
		t.Fatalf("未设置口径时不应截断，实际保留 %d 丢 %d", len(got), dropped)
	}
}
