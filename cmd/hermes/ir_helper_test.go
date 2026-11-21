package main

import (
	"os"
	"path/filepath"

	"hermes/internal/ir"
)

// buildIR 是**测试专用**的装配入口：把源码写成临时文件，交给 C++ 装配器
// （`hermes-native ir`），读回 *ir.Doc。
//
// 为什么保留这个名字与签名：装配器搬进 C++ 之后，仍有十几处测试需要一份 doc
// 来做后端/校验比较。让它们继续写 `buildIR(...)` 比逐个改成"写文件 + 起进程 +
// 解析"更不容易出错——而这条 shim 本身就说明了"装配只有一份实现，在 C++ 里"。
//
// 参数 entryPath 只用来决定临时文件的**基名**（诊断里会引用它）；真实路径是临时的。
func buildIR(entryPath string, src []byte) (*ir.Doc, error) {
	dir, err := os.MkdirTemp("", "hermes-ir-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, filepath.Base(entryPath))
	if err := os.WriteFile(path, src, 0o644); err != nil {
		return nil, err
	}
	out, err := nativeIR(path)
	if err != nil {
		return nil, err
	}
	doc, ds := ir.Load(out)
	if doc == nil || len(ds) != 0 {
		return nil, &irLoadError{n: len(ds)}
	}
	return doc, nil
}

type irLoadError struct{ n int }

func (e *irLoadError) Error() string { return "assembled IR is not readable" }
