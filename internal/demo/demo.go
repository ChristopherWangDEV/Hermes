// Package demo 是"一条命令看懂 Hermes"的演示。
//
// 源码用 go:embed 打进二进制：用户装的是发布包，**没有仓库**，
// 所以示例不能是磁盘上的一个相对路径 —— 否则 `hermes demo` 在用户机器上
// 只会报"找不到文件"，而那正是最不该发生的第一次体验。
package demo

import _ "embed"

// Source 是演示用的 .hermes 源码。
//
//go:embed demo.hermes
var Source []byte

// Name 是示例文件名（写进 IR 的 unit.entry_path，影响可重现性，所以是固定的）。
const Name = "demo.hermes"

// Input 是示例的输入。与 demo.hermes 里的 Ticket 对应。
var Input = map[string]any{
	"title":    "打印机又坏了",
	"priority": "urgent",
}
