package native

import (
	"debug/elf"
	"debug/macho"
	"errors"
	"os"
)

// goDynamicLibs 是迁移期的 Go 参照实现（debug/elf + debug/macho）。
// 它只在这条等价测试里用，迁移完成后随 internal/objfile 一起删除。
func goDynamicLibs(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var magic [4]byte
	if _, err := f.ReadAt(magic[:], 0); err != nil {
		return nil, err
	}
	switch {
	case magic[0] == 0x7f && magic[1] == 'E' && magic[2] == 'L' && magic[3] == 'F':
		ef, err := elf.NewFile(f)
		if err != nil {
			return nil, err
		}
		defer ef.Close()
		libs, err := ef.ImportedLibraries()
		if err != nil {
			if errors.Is(err, elf.ErrNoSymbols) {
				return nil, nil
			}
			return nil, err
		}
		return libs, nil
	default:
		// 与 internal/objfile 一致：先试 thin，再试 fat（macOS 系统二进制多数是 fat）。
		if mf, err := macho.NewFile(f); err == nil {
			defer mf.Close()
			return mf.ImportedLibraries()
		}
		ff, err := macho.NewFatFile(f)
		if err != nil {
			return nil, err
		}
		defer ff.Close()
		if len(ff.Arches) == 0 {
			return nil, nil
		}
		return ff.Arches[0].ImportedLibraries()
	}
}
