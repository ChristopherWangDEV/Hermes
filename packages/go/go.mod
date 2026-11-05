// 模块路径必须能被 Go 代理**直接解析**。
//
// 原来写的是 hermes.dev/sdk/go —— 那需要 hermes.dev 提供 go-import 元标签，
// 否则 `go get` 报 "unrecognized import path"。用仓库真实路径就不需要任何服务器：
// 代理从 GitHub 取，版本靠 tag（子目录模块的 tag 形如 packages/go/v1.0.1）。
module github.com/ChristopherWangDEV/Hermes/packages/go

go 1.21
