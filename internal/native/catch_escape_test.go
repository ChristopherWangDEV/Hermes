package native

import (
	"strings"
	"testing"
)

// E4023：catch 绑定的错误对象不能整只存进另一个变量（LANGUAGE §7.1.1 约束 2）。
// 此前该码被误用于「数组下标必须是整数」；现在下标错误改用 E4031/E4032，
// E4023 回归文档语义：`auto x = e;` 把错误对象搬出 catch 块。
func TestErrorObjectEscapeIsRejected(t *testing.T) {
	src := []byte(`
workflow w(input: json) entry -> int {
  try {
    return 1;
  } catch (Error e) {
    auto x = e;
    return 0;
  }
}
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if !strings.Contains(res.Diag, "E4023") {
		t.Fatalf("期望 E4023（错误对象 e 被整只存进 x），实际诊断：%s", firstN(res.Diag, 800))
	}
}

// 对照组：读字段是合法的（e.status 是值，不是错误对象本体）。
func TestErrorFieldAccessIsAccepted(t *testing.T) {
	src := []byte(`
workflow w(input: json) entry -> int {
  try {
    return 1;
  } catch (Error e) {
    auto s = e.status;
    return 0;
  }
}
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if strings.Contains(res.Diag, "E4023") {
		t.Errorf("读字段 e.status 被误报 E4023：%s", firstN(res.Diag, 800))
	}
}

// 数组下标类型错已从 E4023 迁到 E4031（下标必须是整数）。
func TestArrayIndexMustBeIntegerIsE4031(t *testing.T) {
	src := []byte(`
workflow w(input: json) entry -> int {
  auto xs = [1, 2, 3];
  auto k = "a";
  return xs[k];
}
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if !strings.Contains(res.Diag, "E4031") {
		t.Fatalf("期望 E4031（字符串下标），实际诊断：%s", firstN(res.Diag, 800))
	}
	if strings.Contains(res.Diag, "E4023") {
		t.Errorf("下标错误仍报旧码 E4023：%s", firstN(res.Diag, 800))
	}
}

// 对非数组取下标已从 E4022 迁到 E4032（E4022 保留给「return 了 tuple」）。
func TestIndexNonArrayIsE4032(t *testing.T) {
	src := []byte(`
workflow w(input: json) entry -> int {
  auto n = 5;
  auto i = 0;
  return n[i];
}
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if !strings.Contains(res.Diag, "E4032") {
		t.Fatalf("期望 E4032（对 int 取下标），实际诊断：%s", firstN(res.Diag, 800))
	}
	if strings.Contains(res.Diag, "E4022") {
		t.Errorf("非数组下标仍报旧码 E4022：%s", firstN(res.Diag, 800))
	}
}
