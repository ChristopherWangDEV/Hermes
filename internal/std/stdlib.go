// Package std 是内置 stdlib 的**唯一事实源**。
//
// 之前 `<std/*.h>` 只是 internal/include 里的一张字符串表（"net", "model",
// "runtime", "json"），没有字节，于是：
//
//	· `unit.modules[]` 里那几个内置模块没有 sha256；
//	· `requires[]` 里没有 member_sha256，**供应链那一环断着**（IR.md §9.5）。
//
// 现在它们是真正的源码文件，go:embed 进二进制：解析不触盘、可哈希、可审计。
package std

import (
	"embed"
	"sort"
	"strings"
)

//go:embed stdlib/*.h
var files embed.FS

// Source 返回某个内置模块（不含 `std/` 前缀与 `.h` 后缀）的源码字节。
func Source(mod string) ([]byte, bool) {
	b, err := files.ReadFile("stdlib/" + mod + ".h")
	if err != nil {
		return nil, false
	}
	return b, true
}

// Names 返回全部内置模块名，**按字典序**（顺序影响产物字节，不能依赖 map 迭代）。
func Names() []string {
	ents, err := files.ReadDir("stdlib")
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, strings.TrimSuffix(e.Name(), ".h"))
	}
	sort.Strings(out)
	return out
}
