// Command arfix 把静态归档里所有成员的**头部可变字段**钉成固定值。
//
// 存在的唯一原因：Apple 的 `ranlib` 在生成 __.SYMDEF 符号索引时，会把
// **当前时间**写进该成员的 mtime 字段。于是同样输入、同样目标文件的两次
// 归档，只有这 12 个字节不同，整个 .a 的 sha256 随之变化 —— 可重现构建
// （R-OPS-05）当场失效，而 `ar rcs` / `libtool -static` / `ranlib` 三者
// 在 macOS 上没有一个能直接给出确定性的结果（都实测过）。
//
// 为什么改写头部是安全的：ar 格式的成员头**没有校验和**（只有末尾的
// 0x60 0x0A 魔数），mtime / uid / gid 是给工具看的元数据，ld64 不读取。
// 改写它们不改变归档语义，只改变字节。
//
// 为什么用 Go 而不是 sed/python：这是构建链的一部分，构建链只允许
// Go toolchain + clang。引入第四个解释器等于给"零费用、零传染性依赖"
// 的承诺开一个没人审计的口子。
//
// 用法：
//
//	go run ./tools/arfix path/to/lib.a
package main

import (
	"fmt"
	"os"
)

const (
	globalMagic = "!<arch>\n"
	headerSize  = 60
	// 成员头内的字段偏移（相对成员头起点）。
	offDate = 16
	offUID  = 28
	offGID  = 34
	offMode = 40
	offSize = 48
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: arfix <archive>")
		os.Exit(2)
	}
	path := os.Args[1]
	data, err := os.ReadFile(path)
	if err != nil {
		fail("read %s: %v", path, err)
	}
	if len(data) < len(globalMagic) || string(data[:len(globalMagic)]) != globalMagic {
		fail("%s: not an ar archive (bad global magic)", path)
	}

	patched := 0
	off := len(globalMagic)
	for off+headerSize <= len(data) {
		hdr := data[off : off+headerSize]
		if hdr[58] != 0x60 || hdr[59] != 0x0A {
			fail("%s: bad member magic at offset %d", path, off)
		}
		putField(hdr[offDate:offDate+12], 0)
		putField(hdr[offUID:offUID+6], 0)
		putField(hdr[offGID:offGID+6], 0)
		putField(hdr[offMode:offMode+8], 0o644)

		size, err := parseField(hdr[offSize : offSize+10])
		if err != nil {
			fail("%s: unparsable member size at offset %d: %v", path, off, err)
		}
		if size%2 == 1 {
			size++ // ar 成员按偶数字节对齐，填充字节不计入 size 字段
		}
		off += headerSize + int(size)
		patched++
	}
	if off != len(data) {
		fail("%s: trailing %d bytes after last member", path, len(data)-off)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		fail("write %s: %v", path, err)
	}
	fmt.Printf("arfix: %s members=%d\n", path, patched)
}

// putField 把一个整数以 ar 的十进制 ASCII 格式写进定长字段（空格左对齐填充）。
func putField(dst []byte, v int64) {
	s := fmt.Sprintf("%d", v)
	if len(s) > len(dst) {
		s = s[:len(dst)]
	}
	for i := range dst {
		if i < len(s) {
			dst[i] = s[i]
		} else {
			dst[i] = ' '
		}
	}
}

func parseField(b []byte) (int64, error) {
	var buf [10]byte
	n := 0
	for _, c := range b {
		if c == ' ' {
			break
		}
		if n < len(buf) {
			buf[n] = c
			n++
		}
	}
	if n == 0 {
		return 0, nil
	}
	return parseInt(string(buf[:n]))
}

func parseInt(s string) (int64, error) {
	var v int64
	if s == "" {
		return 0, nil
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a decimal number: %q", s)
		}
		v = v*10 + int64(c-'0')
	}
	return v, nil
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "arfix: "+format+"\n", args...)
	os.Exit(1)
}
