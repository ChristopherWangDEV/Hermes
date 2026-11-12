package objfile

// 动态依赖的名字（DT_NEEDED / LC_LOAD_DYLIB）—— 许可红线的真正判据。
//
// 为什么红线判在"进了哪些库"而不是"符号长什么样"：
//
//	· 名字修饰（Mangled）假阳性：一个**合法**的 C++ 翻译单元用 <string>，
//	  它的未定义符号就是 __ZNSt3__112basic_string…、__ZdlPvm、___cxa_throw。
//	  macOS 只有 libc++（Apache-2.0 WITH LLVM-exception，红线允许），
//	  所以"mangled 即失败"等于"C++ 用户必然被拒"。
//	· 名字修饰假阴性：静态 GPL 归档（.a）里的符号**根本不 mangled**，
//	  那道闸抓不到它本来想抓的东西。
//
// 两条链接路径共用同一份闭集判据（internal/linkpolicy），但**输入面不同**：
//   · `hermes cc` 自己构造整条 link line（一个源文件，没有 -l / -c / 额外 .o），
//     所以"进来了哪些库"对它是**闭合可知**的；产物侧读动态依赖只是兜底。
//   · `hermes compile --link-static/--link-dynamic <本机库路径>` 是另一条输入
//     路径，用户显式给库，输入侧由 cmd/hermes 的 linkInputGate 用同一份闭集
//     判死，产物侧在链接成功后再查一次动态依赖兜底。
// 所以不能无条件写"这条链没有 -l"：cc 的确没有，但 compile 的 --link-* 有。
// 查「进了哪些库」比猜符号名严格得多；静态 .a 不产生 DT_NEEDED，产物侧天然
// 查不到，那份唯一依靠输入侧闸门。

import (
	"debug/elf"
	"debug/macho"
	"errors"
	"fmt"
	"os"
)

// ErrUnsupportedFormat 标记"产物不是 ELF/Mach-O"。裸机目标常产出 raw
// firmware / 自定义格式：那不是"读不懂动态依赖"，而是"这个格式本来就没有
// 动态依赖概念"。调用方（productLinkGate）据此区分"合法的裸机产物"与
// "宿主产物读不出来" —— 后者必须 fail-closed，前者可以放行。
var ErrUnsupportedFormat = errors.New("objfile: unsupported object format")

// DynamicLibs 返回一个可执行文件/共享库在链接期承诺的动态依赖名。
//
// Mach-O 取 LC_LOAD_DYLIB / LC_LOAD_WEAK_DYLIB / LC_REEXPORT_DYLIB（0x0c 与
// 0x80000000 位那一族），ELF 取 DT_NEEDED。名字**原样**返回（`@rpath/libc++.1.dylib`
// 之类的前缀不做解析）：判据只需要子串匹配，解析 rpath 会引入第三个事实源。
//
// 静态链接（没有动态段）返回空切片 + nil error —— "没有动态依赖"不是错误，
// 用 error 表达它会让调用方把"静态"和"读不懂"混成一种情况。
//
// 实现只用标准库：`debug/elf.File.ImportedLibraries` 内部已经是
// `.dynamic` → `DT_NEEDED` → `.dynstr` 那段最小走法，`debug/macho` 同理。
// 手写一遍只会多出第二份同样的解析而没有多出任何保证。
func DynamicLibs(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var magic [4]byte
	if _, err := f.ReadAt(magic[:], 0); err != nil {
		return nil, fmt.Errorf("objfile: cannot read header of %s: %w", path, err)
	}

	switch {
	case magic[0] == 0x7f && magic[1] == 'E' && magic[2] == 'L' && magic[3] == 'F':
		return elfDynamicLibs(f, path)
	case isMachOMagic(magic), isFatMachOMagic(magic):
		return machoDynamicLibs(f, path)
	default:
		return nil, fmt.Errorf("%w: %s is neither ELF nor Mach-O (magic % x)", ErrUnsupportedFormat, path, magic)
	}
}

func elfDynamicLibs(f *os.File, path string) ([]string, error) {
	ef, err := elf.NewFile(f)
	if err != nil {
		return nil, fmt.Errorf("objfile: %s: %w", path, err)
	}
	defer ef.Close()
	libs, err := ef.ImportedLibraries()
	if err != nil {
		// 没有 .dynamic/.dynstr 的静态产物：debug/elf 会报 ErrNoSymbols。
		if errors.Is(err, elf.ErrNoSymbols) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("objfile: %s: cannot read DT_NEEDED: %w", path, err)
	}
	return dedupe(libs), nil
}

func machoDynamicLibs(f *os.File, path string) ([]string, error) {
	if mf, err := macho.NewFile(f); err == nil {
		defer mf.Close()
		libs, lerr := mf.ImportedLibraries()
		if lerr != nil {
			return nil, fmt.Errorf("objfile: %s: cannot read LC_LOAD_DYLIB: %w", path, lerr)
		}
		return dedupe(libs), nil
	}
	// fat（多架构）产物：只看第一片。多架构在发布包里才会出现，而 cc 的产物
	// 永远是单架构；取第一片是为了让 `DynamicLibs("/usr/bin/true")` 这类
	// 诊断调用不至于在半数 macOS 系统二进制上失败。
	ff, err := macho.NewFatFile(f)
	if err != nil {
		return nil, fmt.Errorf("objfile: %s: only single-architecture Mach-O is supported: %w", path, err)
	}
	defer ff.Close()
	if len(ff.Arches) == 0 {
		return []string{}, nil
	}
	libs, err := ff.Arches[0].ImportedLibraries()
	if err != nil {
		return nil, fmt.Errorf("objfile: %s: cannot read LC_LOAD_DYLIB: %w", path, err)
	}
	return dedupe(libs), nil
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
