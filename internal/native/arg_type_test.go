package native

import (
	"strings"
	"testing"
)

// E4030：用户函数调用的实参类型必须匹配形参声明类型（语言无隐式参数转换）。
// 此前 check_type.cpp 对 CallExpr 一律返回 unknown()，add("x", 1) 能静默编过。
func TestArgTypeMismatchIsRejected(t *testing.T) {
	src := []byte(`
int add(int a, int b) {
  return a + b;
}
workflow w(input: json) entry -> int {
  return add("x", 1);
}
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if !strings.Contains(res.Diag, "E4030") {
		t.Fatalf("期望 E4030（实参 \"x\" 是 string，形参 a 是 int），实际诊断：%s", firstN(res.Diag, 800))
	}
}

// 对照组：类型正确的调用不能报 E4030。
func TestArgTypeMatchIsAccepted(t *testing.T) {
	src := []byte(`
int add(int a, int b) {
  return a + b;
}
workflow w(input: json) entry -> int {
  return add(1, 2);
}
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if strings.Contains(res.Diag, "E4030") {
		t.Errorf("类型正确的调用被误报 E4030：%s", firstN(res.Diag, 800))
	}
}

// E4030 必须覆盖 struct 形参：int 实参传给 Point 形参应报错（struct 用 struct_idx 比对）。
func TestArgTypeStructMismatchIsRejected(t *testing.T) {
	src := []byte(`
export struct Point { int x; int y; }
int dist(Point p) {
  return p.x;
}
workflow w(input: json) entry -> int {
  return dist(1);
}
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if !strings.Contains(res.Diag, "E4030") {
		t.Fatalf("期望 E4030（int 1 传给 Point 形参），实际诊断：%s", firstN(res.Diag, 800))
	}
}
