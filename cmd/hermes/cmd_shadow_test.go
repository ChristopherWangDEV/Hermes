package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hermes/internal/diag"
	"hermes/internal/ir"
)

// 变量名**遮蔽**（内层同名声明）是 §5.2「独立词法作用域、出块即关闭」的直接
// 推论，类型检查器（native/cpp/check_type.cpp 的 scope_/lookup_）一直按遮蔽
// 实现。但 codegen 把**名字**当变量表主键（`var_index` 只认名字，声明与引用
// 共用它），于是两个绑定共用一个槽位，装配期回填 vars[].decl 只能指向其中
// 一个，校验器（V-5 反向）当场判它「坏 IR」。
//
// 修复前实测（六个形状，逐个跑过）：
//   - workflow 层遮蔽（if/for/catch 里再声明一次同名变量）→ 编译**硬失败**，
//     报 `E_IR_VAR_MISMATCH ... (compiler bug, not an input problem)`；
//   - 函数体内联路径（路线 B）的同名遮蔽 → **静默编出错值**：`keep(1)` 返回
//     20 而不是 10，`keep(7)` 返回 3 而不是 8。它既不合规也不报错，是更坏的
//     那一类；
//   - `arr_len_` 按名字记定长数组长度 → 内层短数组会把外层的长度改掉，
//     于是 `xs[2]` 在长度 3 的外层数组上被误判越界。
//
// 所以这些用例同时守三件事：**不再假报 IR 坏**、**槽位真的分开**、**值是对的**。

// shadowCompile 把一份源码编成 IR，返回 IR 原文；失败时把 stderr 一起带出来。
func shadowCompile(t *testing.T, dir, name, src string) string {
	t.Helper()
	srcPath := filepath.Join(dir, name+".hermes")
	if err := os.WriteFile(srcPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, name+".ir.json")
	errPath := filepath.Join(dir, name+".stderr")
	code := captureRun(t, errPath, "", "ir", srcPath, "-o", outPath)
	if code != 0 {
		msg, _ := os.ReadFile(errPath)
		t.Fatalf("%s 应该编译通过，实际退出 %d：\n%s", name, code, msg)
	}
	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("%s 编译退出 0 但没有产物：%v", name, err)
	}
	return string(body)
}

// shadowRun 执行已编译的 IR，返回 workflow 的返回值。
func shadowRun(t *testing.T, dir, name, irText string) any {
	t.Helper()
	irPath := filepath.Join(dir, name+".ir.json")
	if err := os.WriteFile(irPath, []byte(irText), 0o644); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, name+".stdout")
	errPath := filepath.Join(dir, name+".run.stderr")
	code := captureRun(t, errPath, outPath, "run", irPath)
	if code != 0 {
		msg, _ := os.ReadFile(errPath)
		t.Fatalf("%s 执行失败，退出 %d：\n%s", name, code, msg)
	}
	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Value any `json:"value"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("%s 的输出不是 JSON：%v\n%s", name, err, raw)
	}
	return got.Value
}

// captureRun 把 run() 的 stdout / stderr 重定向到文件，返回退出码。
func captureRun(t *testing.T, errPath, outPath string, args ...string) int {
	t.Helper()
	ef, err := os.Create(errPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ef.Close()
	oldErr := os.Stderr
	os.Stderr = ef
	defer func() { os.Stderr = oldErr }()

	if outPath == "" {
		return run(args)
	}
	of, err := os.Create(outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer of.Close()
	oldOut := os.Stdout
	os.Stdout = of
	defer func() { os.Stdout = oldOut }()
	return run(args)
}

// shadowLoad 载入 IR 并断言**零诊断**：假报"编译器 bug"是这条用例的原始症状。
func shadowLoad(t *testing.T, irText string) *ir.Workflow {
	t.Helper()
	d, ds := ir.Load([]byte(irText))
	if len(ds) != 0 {
		t.Fatalf("IR 应无诊断，实际 %d 条：%s", len(ds), diag.RenderAll(ds))
	}
	if d == nil || len(d.Workflows) == 0 {
		t.Fatal("IR 里没有 workflow")
	}
	return &d.Workflows[0]
}

// slotsOf 返回某个名字占用的槽位 id（按变量表顺序）。
func slotsOf(w *ir.Workflow, name string) []int {
	var out []int
	for i := range w.Vars {
		if w.Vars[i].Name == name {
			out = append(out, w.Vars[i].ID)
		}
	}
	return out
}

// TestWorkflowBlockShadowing 是原始症状：`if` 块里再声明一次 `x`。
//
// 修复前：`E_IR_VAR_MISMATCH: var x (id 0) and the decl node binding it disagree`。
func TestWorkflowBlockShadowing(t *testing.T) {
	dir := t.TempDir()
	src := `export workflow s1() entry -> int {
    auto x = 1;
    if (x == 1) {
        auto x = 2;
        return x;
    }
    return x;
}
`
	text := shadowCompile(t, dir, "s1", src)
	w := shadowLoad(t, text)

	slots := slotsOf(w, "x")
	if len(slots) != 2 {
		t.Fatalf("遮蔽应得到两个槽位，实际 %d 个：%v（vars=%v）", len(slots), slots, w.Vars)
	}
	if slots[0] == slots[1] {
		t.Fatalf("两个绑定不能共用同一个槽：%v", slots)
	}
	if w.Vars[0].Decl == w.Vars[1].Decl {
		t.Fatalf("两个绑定的 decl 必须是不同节点，实际都是 %d", w.Vars[0].Decl)
	}
	// 语义：条件成立走内层（2），块后的 `return x` 读外层（1）。
	if got := shadowRun(t, dir, "s1", text); got != float64(2) {
		t.Fatalf("s1 应返回 2（内层绑定），实际 %v", got)
	}
}

// TestNestedCatchSameBindingName 是 ⑤ 报的那条：catch 里嵌 fallback、fallback
// 里再嵌 try，两层 `catch (ModelError e)` 用同一个名字。
//
// 修复前：`var e (id 3) ... var 3.decl points at node 1, but this binding node is 15`。
// 两层 catch 各写一次 `e` 是最普通的写法，不该被判成编译器 bug。
func TestNestedCatchSameBindingName(t *testing.T) {
	dir := t.TempDir()
	src := `#link <std/model>

export struct Intent { string action; }

export workflow nested() entry -> string {
    try {
        auto a = model.generate<Intent>(prompt = "a", precision = "exact");
        return a.action;
    } catch (ModelError e) {
        auto b = model.generate<Intent>(prompt = "b", precision = "exact") fallback {
            try {
                auto c = model.generate<Intent>(prompt = "c", precision = "exact");
                return c.action;
            } catch (ModelError e) {
                return "inner";
            }
        };
        return b.action;
    }
}
`
	text := shadowCompile(t, dir, "nested", src)
	w := shadowLoad(t, text)

	slots := slotsOf(w, "e")
	if len(slots) != 2 {
		t.Fatalf("两层 catch 应得到两个 e 槽位，实际 %d 个：%v（vars=%v）", len(slots), slots, w.Vars)
	}
	// 每个 catch 臂的 v 必须指向**它自己**那个槽，不能都指向第一个。
	var arms []int
	for i := range w.Nodes {
		if w.Nodes[i].K != "try" {
			continue
		}
		for _, c := range w.Nodes[i].Catches {
			arms = append(arms, c.V)
		}
	}
	if len(arms) != 2 {
		t.Fatalf("应有两个 catch 臂，实际 %d 个：%v", len(arms), arms)
	}
	if arms[0] == arms[1] {
		t.Fatalf("两个 catch 臂必须绑定不同槽位，实际都是 %d", arms[0])
	}
}

// TestForLoopVarShadowing：循环变量遮蔽外层同名变量。
//
// 循环变量只在循环体内存在（§3.1，E4028 守出循环后的引用）；出循环后 `i`
// 必须解析回外层那一个，而不是循环变量。
func TestForLoopVarShadowing(t *testing.T) {
	dir := t.TempDir()
	src := `export workflow f() entry -> int {
    auto i = 5;
    auto s = 0;
    for (auto i = 0; i < 3; i++) {
        s = s + i;
    }
    return i;
}
`
	text := shadowCompile(t, dir, "f", src)
	w := shadowLoad(t, text)

	if slots := slotsOf(w, "i"); len(slots) != 2 {
		t.Fatalf("循环变量与外层 i 应各占一个槽，实际 %v（vars=%v）", slots, w.Vars)
	}
	// 循环体里加的是 0+1+2=3，返回值是外层的 5。
	if got := shadowRun(t, dir, "f", text); got != float64(5) {
		t.Fatalf("f 应返回 5（外层 i），实际 %v", got)
	}
}

// TestInlineFunctionBodyShadowing 守**最坏的那一类**：函数体内联路径上的遮蔽
// 曾经不报错、只是把值算错。
//
// 修复前实测：`keep(1) * 10` 得 20（应 10），`acc(7)` 得 3（应 8）——
// 内层声明登记进 inline_locals_ 之后没有出块还原，于是块后的引用读的是内层槽。
// 两条用例刻意分开：一个 unit 只允许一个 entry workflow。
func TestInlineFunctionBodyShadowing(t *testing.T) {
	dir := t.TempDir()
	keepSrc := `int keep(int x) {
    auto a = x;
    if (a == 1) {
        auto a = 2;
    }
    return a;
}

export workflow m() entry -> int {
    return keep(1) * 10;
}
`
	text := shadowCompile(t, dir, "keep", keepSrc)
	shadowLoad(t, text)
	if got := shadowRun(t, dir, "keep", text); got != float64(10) {
		t.Fatalf("keep(1)*10 应为 10（块内赋值不改变外层的 a），实际 %v", got)
	}

	// acc(7)：循环里 a 遮蔽外层，b=0+1=1，返回 7+1=8。修复前是 3。
	accSrc := `int acc(int x) {
    auto a = x;
    auto b = 0;
    for (auto a = 0; a < 2; a++) {
        b = b + a;
    }
    return a + b;
}

export workflow m() entry -> int {
    return acc(7);
}
`
	text2 := shadowCompile(t, dir, "acc", accSrc)
	shadowLoad(t, text2)
	if got := shadowRun(t, dir, "acc", text2); got != float64(8) {
		t.Fatalf("acc(7) 应为 8（外层 a=7 加 b=1），实际 %v", got)
	}
}

// TestShadowedArrayLengthPerSlot 守 `arr_len_` 的键：定长数组长度必须按**槽位**
// 记，不能按名字。修复前内层短数组会把外层的长度顶掉，于是外层长度 3 的
// `xs[2]` 被误判越界（假失败）。
func TestShadowedArrayLengthPerSlot(t *testing.T) {
	dir := t.TempDir()
	src := `export workflow a() entry -> int {
    auto n = 0;
    auto xs = [1,2,3];
    if (n == 0) {
        auto xs = [1,2];
    }
    return xs[2];
}
`
	text := shadowCompile(t, dir, "arr", src)
	shadowLoad(t, text)
	if got := shadowRun(t, dir, "arr", text); got != float64(3) {
		t.Fatalf("xs[2] 应为 3，实际 %v", got)
	}

	// 反向：内层数组真的越界时仍然要拒（这条防"按槽位取值"把检查弄丢）。
	bad := strings.Replace(src, "        auto xs = [1,2];", "        auto xs = [1,2,3];\n        return xs[3];", 1)
	srcPath := filepath.Join(dir, "bad.hermes")
	if err := os.WriteFile(srcPath, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	errPath := filepath.Join(dir, "bad.stderr")
	if code := captureRun(t, errPath, "", "ir", srcPath, "-o", filepath.Join(dir, "bad.ir.json")); code == 0 {
		t.Fatal("内层 xs 长度 3 上取 xs[3] 应被拒，实际编译通过")
	}
	msg, _ := os.ReadFile(errPath)
	if !strings.Contains(string(msg), "out of range") {
		t.Fatalf("越界诊断应说明 out of range，实际：\n%s", msg)
	}
}
