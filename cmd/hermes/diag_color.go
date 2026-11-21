package main

import "hermes/internal/diag"

// diagColor 是 `run` 与 `explain`（两条仍留在 Go 的命令）共用的着色口径。
//
// 它原来是 ir_build.go 的一部分（那个文件随装配器一起删掉了）。留着这个
// 独立文件而不是塞进某条命令里，是因为**两个消费方**都要它：塞进任何一个，
// 另一个就要跨文件依赖，读起来会像"共用状态"而不是"共用口径"。
var diagColor = diag.ColorAuto
