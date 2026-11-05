package std

import "testing"

func TestEmbed(t *testing.T) {
	for _, n := range Names() {
		b, ok := Source(n)
		if !ok || len(b) == 0 {
			t.Fatalf("%s: ok=%v len=%d", n, ok, len(b))
		}
	}
	if len(Names()) == 0 {
		t.Fatal("没有内嵌任何 std 模块")
	}
}
