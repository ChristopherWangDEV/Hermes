// Package include 是 Hermes 的 **include 图解析**（T10 的 Go 侧）。
//
// 为什么在 Go 而不是 C++：`native/cpp` 的契约是**纯函数、不读文件**
// （hermes_parser.h 契约 1，IR.md §9.6）。include 要读文件系统，所以它只能在
// Go 里做。由此形成一条清晰分工：
//
//	C++ 前端：单文件的词法/语法/单文件语法限制（E1xxx / E2xxx / 部分 E3xxx）
//	Go 侧   ：跨文件的 include 图（E3002 环 / E3015 角色 / E1009 路径 / 上限）
//
// 分工依据是"谁看得见"：C++ 拿不到别的文件，所以它根本无从判断环。
package include

import (
	"regexp"
	"strconv"
)

// Directive 是 `#include` / `#link` 的一条指令。
type Directive struct {
	Kind  string // "Include" | "Link"（与 AST 的 Kind 同名）
	Spec  string // 原文，**含**外层引号或尖括号："support.h" / <std/net>
	Path  string // 去掉外层的目标："support.h" / "std/net"
	Angle bool   // true = 尖括号形式（只查内置表）
	Line  int
	Col   int
}

// dumpLine 匹配 ast_dump.cpp 的输出行：
//
//	Kind n=N l:c-el:ec "text"
//
// 缩进 = 深度 × 2 空格。text 用 `"(.*)"` 贪婪匹配 —— 因为 spec 原文自带
// 引号（`""support.h""`），非贪婪会在第一个内层引号处停住，切出半个路径。
var dumpLine = regexp.MustCompile(`^(\s*)([A-Za-z_][A-Za-z0-9_]*) n=\d+ (\d+):(\d+)-\d+:\d+ "(.*)"$`)

// ScanDirectives 从一份 AST dump 里取出**顶层**的 include / link 指令。
//
// 只取顶层（depth == 1）：`Module` 的直接子节点就是全部指令。深处的同名节点
// 不存在，但把范围钉死在顶层能让"将来 AST 里加了别的 Include 节点"不改变语义。
func ScanDirectives(dump string) []Directive {
	var out []Directive
	for _, line := range splitLines(dump) {
		m := dumpLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		depth := len(m[1]) / 2
		if depth != 1 {
			continue
		}
		kind := m[2]
		if kind != "Include" && kind != "Link" {
			continue
		}
		lineNo, _ := strconv.Atoi(m[3])
		col, _ := strconv.Atoi(m[4])
		raw := m[5]

		d := Directive{Kind: kind, Spec: raw, Line: lineNo, Col: col}
		switch {
		case len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"':
			d.Path = raw[1 : len(raw)-1]
			d.Angle = false
		case len(raw) >= 2 && raw[0] == '<' && raw[len(raw)-1] == '>':
			d.Path = raw[1 : len(raw)-1]
			d.Angle = true
		default:
			// 既不是引号也不是尖括号：词法器已经拦过，这里原样收下，
			// 交给上层按"不可解析"处理，而不是静默丢掉。
			d.Path = raw
		}
		out = append(out, d)
	}
	return out
}

// splitLines 按 '\n' 切且不产生末尾空串。
func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
