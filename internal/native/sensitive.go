package native

/*
#include "hermes_abi.h"
*/
import "C"

import (
	"runtime"
	"unsafe"
)

// Sensitive 是一块"内容不外泄"的缓冲（R-NA-04）。
//
// 语义保证（由 Rust 侧实现，本层只转述）：
//   - 内容被 mlock 锁页（可能失败 → 降级，见 Locked）；
//   - 释放时先 zeroize 再归还槽位，generation++；
//   - free 之后任何访问都返回 ErrHandleStale，而不是读到一个"看起来还行"的旧值。
//
// 它保管的是 API key、prompt 明文这类东西。**不要**把它当普通 []byte 用：
// 它的全部价值在于"崩溃转储里看不到明文"。
type Sensitive struct {
	h    Handle
	done bool
}

// NewSensitive 从 b 复制一份内容建立敏感缓冲（不接管 b）。
//
// noMlock 只该在测试降级路径时传 true；生产路径应传 false 并检查 Locked()。
func NewSensitive(b []byte, noMlock bool) (*Sensitive, error) {
	if len(b) > SensitiveMax {
		return nil, ErrTooLargeErr
	}
	p, n := byteSlice(b)
	var h C.hm_handle
	var nm C.int32_t
	if noMlock {
		nm = 1
	}
	if err := toError(C.hm_sensitive_new(p, n, nm, &h)); err != nil {
		return nil, err
	}
	return &Sensitive{h: Handle(h)}, nil
}

// Len 返回字节长度。
func (s *Sensitive) Len() (int, error) {
	var n C.size_t
	if err := toError(C.hm_sensitive_len(C.hm_handle(s.h), &n)); err != nil {
		return 0, err
	}
	return int(n), nil
}

// Read 复制出内容。
//
// 这**不是**零拷贝视图 —— 刻意如此。零拷贝意味着指针会离开本函数的栈帧，
// 而那块内存在 free 之后就是垃圾；复制一份让调用方决定它的寿命。
func (s *Sensitive) Read() ([]byte, error) {
	n, err := s.Len()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return []byte{}, nil
	}
	buf := make([]byte, n)
	var written C.size_t
	if err := toError(C.hm_sensitive_read(C.hm_handle(s.h),
		(*C.uint8_t)(&buf[0]), C.size_t(n), &written)); err != nil {
		return nil, err
	}
	runtime.KeepAlive(s)
	return buf[:int(written)], nil
}

// Locked 报告内容是否真的被 mlock 锁页。
//
// 返回 false 不是错误，是降级：调用方**必须**写 trace 事件
// security.mlock_unavailable，否则审计报告会谎称这块内存受保护。
func (s *Sensitive) Locked() (bool, error) {
	var out C.int32_t
	if err := toError(C.hm_sensitive_locked(C.hm_handle(s.h), &out)); err != nil {
		return false, err
	}
	return out != 0, nil
}

// Free 零化并归还槽位。重复调用返回 ErrHandleStale（不是静默成功）——
// 双释放是必须被看见的 bug，不是可以忽略的噪音。
func (s *Sensitive) Free() error {
	if s.done {
		return ErrHandleStaleErr
	}
	err := toError(C.hm_sensitive_free(C.hm_handle(s.h)))
	if err == nil {
		s.done = true
	}
	return err
}

// View 是只读零拷贝视图。多个视图可以共享同一份底层分配（Slice）。
//
// 生命周期规则（R4）：底层分配在最后一个视图 Free 之前不能释放。
// 本类型不替你引用计数 —— 谁 Slice 出来的，谁负责 Free。
type View struct {
	h Handle
}

// NewView 从 b 复制建立只读视图。
func NewView(b []byte) (*View, error) {
	if len(b) > ViewMax {
		return nil, ErrTooLargeErr
	}
	p, n := byteSlice(b)
	var h C.hm_handle
	if err := toError(C.hm_view_new(p, n, &h)); err != nil {
		return nil, err
	}
	return &View{h: Handle(h)}, nil
}

// Slice 切出一个共享底层分配的视图（不复制）。越界返回 ErrInvalidArgument。
func (v *View) Slice(off, n int) (*View, error) {
	if off < 0 || n < 0 {
		return nil, ErrInvalidArgument
	}
	var h C.hm_handle
	if err := toError(C.hm_view_slice(C.hm_handle(v.h),
		C.size_t(off), C.size_t(n), &h)); err != nil {
		return nil, err
	}
	return &View{h: Handle(h)}, nil
}

// Borrow 返回一个只在 Free 之前有效的只读切片。
//
// 它不做复制，所以**快**；但调用方一旦把它存到别处、并在 Free 之后读，
// 就是 use-after-free。只要有一丝不确定，用 Bytes()。
func (v *View) Borrow() ([]byte, error) {
	var p *C.uint8_t
	var n C.size_t
	if err := toError(C.hm_view_borrow(C.hm_handle(v.h), &p, &n)); err != nil {
		return nil, err
	}
	if n == 0 || p == nil {
		return []byte{}, nil
	}
	return unsafeSlice(p, int(n)), nil
}

// Bytes 复制出内容。安全但多一次拷贝。
func (v *View) Bytes() ([]byte, error) {
	b, err := v.Borrow()
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out, nil
}

// Free 归还视图。共享同一底层分配的其他视图仍可继续使用。
func (v *View) Free() error {
	return toError(C.hm_view_free(C.hm_handle(v.h)))
}

// unsafeSlice 把 C 指针 + 长度包成 Go 切片（不复制）。
//
// 唯一调用点是 View.Borrow —— 那里的契约规定指针只在视图存活期内有效。
// 不要把它当成"任意 C 指针都能变切片"的通用工具推广出去。
func unsafeSlice(p *C.uint8_t, n int) []byte {
	if n == 0 || p == nil {
		return []byte{}
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(p)), n)
}
