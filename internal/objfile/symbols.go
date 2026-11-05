// Package objfile 读目标文件的符号表。
//
// 为什么不用 `nm` / `dumpbin`：它们是**外部工具**，输出格式随平台与版本变化，
// 而我们要的只是一个布尔判断（"这个符号定义了吗"）。调用外部工具意味着
// 多一个部署前提、多一处解析别人的文本、多一条"版本不同就误判"的路径。
// Go 标准库自带 debug/elf 与 debug/macho —— 用它们，零依赖、零外部工具。
package objfile

import (
	"debug/elf"
	"debug/macho"
	"fmt"
	"os"
	"strings"
)

// Symbols 是某个目标文件的符号视图。
type Symbols struct {
	// Defined 是**已定义**的符号名。
	Defined map[string]bool
	// Undefined 是**被引用但未定义**的符号名。
	Undefined map[string]bool
}

// Read 读一个可执行文件或目标文件的符号表。格式按魔数判断，不靠扩展名。
func Read(path string) (*Symbols, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var magic [4]byte
	if _, err := f.ReadAt(magic[:], 0); err != nil {
		return nil, fmt.Errorf("objfile: 读不到 %s 的头部: %w", path, err)
	}

	switch {
	case magic[0] == 0x7f && magic[1] == 'E' && magic[2] == 'L' && magic[3] == 'F':
		return readELF(f, path)
	case isMachOMagic(magic):
		return readMachO(f, path)
	default:
		return nil, fmt.Errorf("objfile: %s 既不是 ELF 也不是 Mach-O（魔数 % x）", path, magic)
	}
}

func isMachOMagic(m [4]byte) bool {
	// 64/32 位、大/小端，以及 fat（多架构）头。
	switch {
	case m[0] == 0xcf && m[1] == 0xfa && m[2] == 0xed && m[3] == 0xfe: // 64 小端
		return true
	case m[0] == 0xfe && m[1] == 0xed && m[2] == 0xfa && m[3] == 0xcf: // 64 大端
		return true
	case m[0] == 0xce && m[1] == 0xfa && m[2] == 0xed && m[3] == 0xfe: // 32 小端
		return true
	}
	return false
}

func readELF(f *os.File, path string) (*Symbols, error) {
	ef, err := elf.NewFile(f)
	if err != nil {
		return nil, fmt.Errorf("objfile: %s: %w", path, err)
	}
	defer ef.Close()
	syms, err := ef.Symbols()
	if err != nil && err != elf.ErrNoSymbols {
		return nil, fmt.Errorf("objfile: %s: 读符号表失败: %w", path, err)
	}
	out := &Symbols{Defined: map[string]bool{}, Undefined: map[string]bool{}}
	for _, s := range syms {
		name := s.Name
		if name == "" {
			continue
		}
		if s.Section == elf.SHN_UNDEF {
			out.Undefined[name] = true
		} else {
			out.Defined[name] = true
		}
	}
	return out, nil
}

func readMachO(f *os.File, path string) (*Symbols, error) {
	mf, err := macho.NewFile(f)
	if err != nil {
		// fat 二进制（多架构）：本刀只读单一架构，明确报错而不是猜。
		return nil, fmt.Errorf("objfile: %s: 只支持单一架构的 Mach-O: %w", path, err)
	}
	defer mf.Close()
	out := &Symbols{Defined: map[string]bool{}, Undefined: map[string]bool{}}
	if mf.Symtab == nil {
		return out, nil
	}
	for _, s := range mf.Symtab.Syms {
		if s.Name == "" {
			continue
		}
		// Mach-O 里未定义符号的 type 是 N_UNDF（0x0），且 value == 0。
		if s.Type&0x0e == 0 && s.Value == 0 {
			out.Undefined[s.Name] = true
		} else {
			out.Defined[s.Name] = true
		}
	}
	return out, nil
}

// MachOName 把一个 C 符号名换成 Mach-O 里的写法（前导下划线）。
//
// 为什么需要：Darwin 上 `hermes_x` 在符号表里叫 `_hermes_x`。不做这层换算，
// "符号没定义"会在 macOS 上变成**永远报错**，而在 Linux 上永远通过 ——
// 那种只在某个平台上红的守卫，很快就会被加 `t.Skip`。
func MachOName(name string) string {
	if strings.HasPrefix(name, "_") {
		return name
	}
	return "_" + name
}

// Mangled 判断一个符号名是否被 C++ 名字修饰过。
//
// D-025 要求：mangled 符号即**硬失败**。理由不是洁癖 —— 修饰过的名字意味着
// 那个符号来自某个 C++ 翻译单元，而它通常也就意味着 libstdc++ 被链了进来，
// 那是许可问题，不只是兼容问题。
func Mangled(name string) bool {
	// 两种平台的写法不同：ELF 是 `_Z3foov`，Mach-O 会再加一个前导下划线 `__Z3foov`。
	// 早期实现先 TrimPrefix 一个下划线再找 `_Z`，于是 ELF 的 `_Z3foov` 被削成
	// `Z3foov` 再也匹配不上 —— 判据在 Linux 上**永远为假**，等于没有。
	if strings.HasPrefix(name, "__Z") {
		return true
	}
	return strings.HasPrefix(name, "_Z")
}
