package native

import (
	"strings"
	"testing"
)

// E4020：match 的重复 case 字面量必须在编译期被拒。此前 check_type.cpp 只查
// default 缺失（E4018）与臂/主语类型（E4019），完全不查重复字面量，于是
// `case "a":` 写两遍能静默编过 —— 第二个分支永不可达，是纯死代码。
// 字符串臂与整型臂类型不同（严格相等），故以 "i:<v>" / "s:<v>" 为键分别计数。
func TestMatchDuplicateStringCaseIsRejected(t *testing.T) {
	src := []byte(`
workflow w(input: json) entry -> int {
  auto s = "a";
  match (s) {
    case "a": { return 1; }
    case "a": { return 2; }
    default:  { return 0; }
  }
}
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if !strings.Contains(res.Diag, "E4020") {
		t.Fatalf("期望 E4020（重复字符串 case \"a\"），实际诊断：%s", firstN(res.Diag, 800))
	}
}

func TestMatchDuplicateIntCaseIsRejected(t *testing.T) {
	src := []byte(`
workflow w(input: json) entry -> int {
  auto n = 1;
  match (n) {
    case 1: { return 1; }
    case 1: { return 2; }
    default: { return 0; }
  }
}
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if !strings.Contains(res.Diag, "E4020") {
		t.Fatalf("期望 E4020（重复整型 case 1），实际诊断：%s", firstN(res.Diag, 800))
	}
}

// 对照组：字面量互不相同的 match 不得报 E4020。
func TestMatchDistinctCasesAreAccepted(t *testing.T) {
	src := []byte(`
workflow w(input: json) entry -> int {
  auto s = "a";
  match (s) {
    case "a": { return 1; }
    case "b": { return 2; }
    default:  { return 0; }
  }
}
`)
	res, err := Parse(src, "t.hermes", false)
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if strings.Contains(res.Diag, "E4020") {
		t.Errorf("字面量互不相同的 match 被误报 E4020：%s", firstN(res.Diag, 800))
	}
}
