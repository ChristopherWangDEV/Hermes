package main

import (
	"strings"
	"testing"
)

// TestStaticLibcGate 是**红线规则的可执行形式**。
//
// 表驱动而不是"去 Linux 上试一次"：这条规则最容易漏验证，而漏的代价是
// 一个静态链了 LGPL glibc 的产物被发出去。
func TestStaticLibcGate(t *testing.T) {
	cases := []struct {
		name    string
		target  string
		static  bool
		allow   string
		wantErr bool
	}{
		{"macOS 静态无义务", "darwin", true, "", false},
		{"Linux 动态放行", "linux", false, "", false},
		{"Linux 静态必须拒绝", "linux", true, "", true},
		{"Linux 静态 + 受控例外", "linux", true, "1", false},
		{"非静态任何平台都放行", "windows", false, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.allow != "" {
				t.Setenv("HERMES_ALLOW_GLIBC_STATIC", c.allow)
			}
			err := staticLibcGate(c.target, c.static)
			if c.wantErr && err == nil {
				t.Fatal("应拒绝，实际放行")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("应放行，实际: %v", err)
			}
			// 拒绝文案必须给出**出路**，而不只是"不行"。
			if c.wantErr {
				msg := err.Error()
				for _, want := range []string{"musl", "动态链", "HERMES_ALLOW_GLIBC_STATIC"} {
					if !strings.Contains(msg, want) {
						t.Fatalf("拒绝文案缺少 %q：\n%s", want, msg)
					}
				}
			}
		})
	}
}
