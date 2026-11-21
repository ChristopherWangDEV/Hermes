package native

import (
	"testing"

	"hermes/internal/toolchain"
)

// TestParseTargetMatchesGo 是 --target 分类的等价判据。
//
// 为什么这条必须**连错误文案都比**：这个分类器决定"Go runtime 能不能进产物"，
// 而它的拒绝路径（不认识就报错、绝不猜）是红线的一部分。文案不同意味着两条
// 路径的拒绝理由不同——那正是要避免的漂移。
func TestParseTargetMatchesGo(t *testing.T) {
	cases := []string{
		// 原生
		"", "darwin", "linux",
		// 完整三元组：宿主
		"x86_64-unknown-linux-gnu",
		"aarch64-apple-darwin",
		"x86_64-unknown-freebsd",
		"arm-linux-gnueabihf",
		// 裸机
		"thumbv7em-none-eabi",
		"arm-none-eabihf",
		"riscv32imc-unknown-none-elf",
		// 拒绝：跨 GOOS 的裸名字
		"linux",
		// 拒绝：字段数不对
		"x86_64-linux", "a-b-c-d-e",
		// 拒绝：未知 OS（闭集之外）
		"x86_64-unknown-haiku",
	}
	const goos, goarch = "darwin", "arm64"
	for _, raw := range cases {
		want, werr := toolchain.ParseTarget(raw, goos, goarch)
		got, gerr := ParseTarget(raw, goos, goarch)

		if (werr == nil) != (gerr == nil) {
			t.Fatalf("%q: 成败不同 Go=%v C++=%v", raw, werr, gerr)
		}
		if werr != nil {
			if werr.Error() != gerr.Error() {
				t.Fatalf("%q: 错误文案不同\n  Go =%s\n  C++=%s", raw, werr, gerr)
			}
			continue
		}
		if got.Triple != want.Triple || got.Arch != want.Arch || got.Vendor != want.Vendor ||
			got.OS != want.OS || got.Env != want.Env || got.GOOS != want.GOOS ||
			got.Native != want.Native || got.Explicit != want.Explicit {
			t.Fatalf("%q: 字段不同\n  Go =%+v\n  C++=%+v", raw, want, got)
		}
		wantClass := "host"
		if want.IsBareMetal() {
			wantClass = "bare-metal"
		}
		if got.Class != wantClass {
			t.Fatalf("%q: 分类不同 Go=%s C++=%s", raw, wantClass, got.Class)
		}
	}
}
