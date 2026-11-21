package diag

import (
	"strings"
	"testing"
)

// TestSnippetSingleLine 守最基本的形状：行号、`|`、`^` 对齐。
func TestSnippetSingleLine(t *testing.T) {
	src := []byte("alpha\n    return \"str\";\nomega\n")
	got := Snippet(src, 2, 5, 2, 11)
	want := []string{"2 |     return \"str\";", "  |     ^^^^^^"}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Fatalf("缺 %q：\n%s", w, got)
		}
	}
}

// TestSnippetMultiLine 守跨行 span：首行到行尾、末行从行首、中间整行。
func TestSnippetMultiLine(t *testing.T) {
	src := []byte("if (a &&\n    b &&\n    c) {\n}\n")
	got := Snippet(src, 1, 5, 3, 6)
	lines := strings.Split(got, "\n")
	if len(lines) < 6 {
		t.Fatalf("应画三行源码与三条标记，实际：\n%s", got)
	}
	// 布局：源码行与标记行交替（0 是表头 " |"）。
	// 首行只标记后缀（"a &&" 顶到行尾），中间整行，末行只标记前缀。
	if got := strings.Count(lines[2], "^"); got != len("a &&") {
		t.Fatalf("首行应标记 %d 个字符，实际 %d：%q", len("a &&"), got, lines[2])
	}
	if got := strings.Count(lines[4], "^"); got < len("    b &&") {
		t.Fatalf("中间行应整行标记（≥%d），实际 %d：%q", len("    b &&"), got, lines[4])
	}
	// endCol 是**排他**的：第 6 列之前共 5 个字符（"    c"）。
	if got := strings.Count(lines[6], "^"); got != 5 {
		t.Fatalf("末行应标记 5 个字符，实际 %d：%q", got, lines[6])
	}
}

// TestSnippetClampsOutOfRange 守"越界不 panic"：行号超出来源时返回空串。
func TestSnippetClampsOutOfRange(t *testing.T) {
	if got := Snippet([]byte("one line\n"), 9, 1, 9, 2); got != "" {
		t.Fatalf("越界应返回空串，实际：%q", got)
	}
	if got := Snippet(nil, 1, 1, 1, 2); got != "" {
		t.Fatalf("空来源应返回空串，实际：%q", got)
	}
}
