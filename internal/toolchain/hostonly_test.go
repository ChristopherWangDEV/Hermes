package toolchain

import (
	"strings"
	"testing"
)

// 裸机守卫的三条边界：sleep、until、emit ... to 必须拒；emit 的全局/频道
// 路由必须放行（它们只是整数比较，裸机做得来）；宿主目标一律放行。
func TestCheckHostOnlyNodes(t *testing.T) {
	cases := []struct {
		name  string
		bare  bool
		nodes []HostOnlyKind
		want  bool // true = 拒绝
	}{
		{"bare_sleep", true, []HostOnlyKind{{Kind: "sleep"}}, true},
		{"bare_until", true, []HostOnlyKind{{Kind: "until", Signal: "S"}}, true},
		{"bare_emit_to", true, []HostOnlyKind{{Kind: "emit", Route: 2}}, true},
		{"bare_emit_channel_ok", true, []HostOnlyKind{{Kind: "emit", Route: 1}}, false},
		{"bare_emit_global_ok", true, []HostOnlyKind{{Kind: "emit", Route: 0}}, false},
		{"bare_stop_ok", true, []HostOnlyKind{{Kind: "stop"}}, false},
		{"host_sleep_ok", false, []HostOnlyKind{{Kind: "sleep"}}, false},
		{"host_emit_to_ok", false, []HostOnlyKind{{Kind: "emit", Route: 2}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := CheckHostOnlyNodes(c.bare, c.nodes)
			if c.want {
				if err == nil {
					t.Fatal("裸机上应当拒绝，但放行了")
				}
				if CodeOf(err) != CodeHostOnlyNode {
					t.Fatalf("错误码应为 %s，实际 %s", CodeHostOnlyNode, CodeOf(err))
				}
				if !strings.Contains(err.Error(), HostOnlyMessage) {
					t.Fatalf("文案不是定稿那句：%q", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("不该拒绝：%v", err)
			}
		})
	}
}
