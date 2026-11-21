package backend

import (
	"errors"
	"fmt"

	"hermes/internal/ir"
	"hermes/internal/native"
)

// cBackendNative 是 C 后端的 **C++ 实现**（迁移目标）。
//
// 它必须与此前 Go 版的 golden 输出逐字节一致 —— 这条由 equiv_test.go 把关。
// Go 版实现已删除，不再回落：C++ 侧返回 ErrBackendNotCovered 时如实报错，
// 哨兵只用于把未覆盖原因带进错误与等价测试。
type cBackendNative struct{}

func (cBackendNative) Name() string { return "c" }

func (cBackendNative) Generate(doc *ir.Doc) ([]byte, error) {
	raw, err := ir.Emit(doc)
	if err != nil {
		return nil, fmt.Errorf("backend(native): cannot serialise IR: %w", err)
	}
	out, err := native.BackendEmitC(raw)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// NativeC 返回 C 后端的 C++ 实现。
func NativeC() Backend { return cBackendNative{} }

// cBackendSwitched 返回当前实现。Go 版退路已删除（等价性有 12 份 golden +
// 23 份真实源码逐字节证据，且 41 份 IR 装配基线锁住上游），这里只留一个指针，
// 免得调用方到处写 cBackendNative{}。
func cBackendSwitched() Backend {
	return cBackendNative{}
}

// ErrNotCovered 让调用方判断"这份 IR 还没被 C++ 版覆盖"而不必比较错误文本。
func ErrNotCovered(err error) bool { return errors.Is(err, native.ErrBackendNotCovered) }
