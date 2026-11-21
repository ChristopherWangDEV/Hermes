//! 只读零拷贝字节视图（R-NA-04 的同族不变量：**这块内存不可写**）。
//!
//! 实现方式：`Arc<Vec<u8>>` 持有底层分配，视图只是 `(off, len)` 切片。
//! `hm_view_slice` 共享同一个 `Arc`，因此"切 1000 个子视图"不复制一个字节 ——
//! trace 分块哈希与响应体切片是热路径，复制会把它变成 O(n²)。
//!
//! 不可写的保证不是靠"我们没写"，而是靠**类型系统**：
//!   * `View::as_slice` 返回 `&[u8]`，不给 `&mut`；
//!   * 导出面 `hm_view_borrow` 的出参类型是 `*mut *const u8`，
//!     本 crate **不存在**任何返回 `*mut u8` 的函数（T03 验收⑤，由
//!     tests/guard 的源码扫描 + expected_symbols.txt 双向守卫）。
//!
//! 借用 vs 拥有（ABI 规则 R4）：`hm_view_borrow` 给的指针只在本次调用内有效，
//! 因为 `Arc` 的引用计数由 handle 表持有 —— 一旦 handle 被 free，指针就悬垂。
//! 要跨调用使用，必须 `hm_view_new`（拥有语义）。

use std::sync::Arc;
use std::sync::Mutex;

use crate::handle::HandleTable;
use crate::status;

pub const VIEW_MAX: usize = 16 << 20; // 16 MiB，与 HM_VIEW_MAX 一致

pub struct View {
    buf: Arc<Vec<u8>>,
    off: usize,
    len: usize,
}

impl View {
    pub fn new(bytes: &[u8]) -> Self {
        View {
            buf: Arc::new(bytes.to_vec()),
            off: 0,
            len: bytes.len(),
        }
    }

    /// 切片共享同一份底层分配（不复制）。越界返回 `HM_E_INVALID_ARG`。
    pub fn slice(&self, off: usize, len: usize) -> Result<View, i32> {
        if off > self.len {
            return Err(status::HM_E_INVALID_ARG);
        }
        if len > self.len - off {
            return Err(status::HM_E_INVALID_ARG);
        }
        Ok(View {
            buf: Arc::clone(&self.buf),
            off: self.off + off,
            len,
        })
    }

    pub fn as_slice(&self) -> &[u8] {
        &self.buf[self.off..self.off + self.len]
    }

    pub fn len(&self) -> usize {
        self.len
    }

    pub fn is_empty(&self) -> bool {
        self.len == 0
    }

    /// 借用指针：只在持有 handle 的期间有效（R4）。
    pub fn as_ptr(&self) -> *const u8 {
        self.buf.as_ptr().wrapping_add(self.off)
    }
}

/// 全局视图表。
pub static TABLE: Mutex<HandleTable<View>> = Mutex::new(HandleTable::new());

pub fn lock_table() -> std::sync::MutexGuard<'static, HandleTable<View>> {
    match TABLE.lock() {
        Ok(g) => g,
        Err(poisoned) => poisoned.into_inner(),
    }
}

pub fn shutdown() {
    let mut t = lock_table();
    t.clear();
}

/// 长度校验：超限返回 `HM_E_TOO_LARGE`，绝不截断（R3）。
pub fn check_len(len: usize) -> Result<(), i32> {
    if len > VIEW_MAX {
        return Err(status::HM_E_TOO_LARGE);
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn slice_is_zero_copy_and_bounded() {
        let v = View::new(b"0123456789");
        let s = v.slice(2, 3).unwrap();
        assert_eq!(s.as_slice(), b"234");
        // 零拷贝：切片与原视图指向同一块分配的同一个地址。
        assert_eq!(s.as_ptr() as usize, v.as_ptr() as usize + 2);

        // 边界
        assert!(v.slice(10, 0).is_ok());
        assert_eq!(v.slice(11, 0).unwrap_err(), status::HM_E_INVALID_ARG);
        assert_eq!(v.slice(8, 3).unwrap_err(), status::HM_E_INVALID_ARG);
        // 上溢：不能用 "off + len 溢出绕回小值" 通过检查
        assert_eq!(
            v.slice(usize::MAX, 2).unwrap_err(),
            status::HM_E_INVALID_ARG
        );
    }

    #[test]
    fn nested_slice_offsets_are_absolute() {
        let v = View::new(b"abcdefgh");
        let s = v.slice(4, 4).unwrap();
        let t = s.slice(1, 2).unwrap();
        assert_eq!(t.as_slice(), b"fg");
        assert_eq!(t.as_ptr() as usize, v.as_ptr() as usize + 5);
    }

    #[test]
    fn oversized_view_is_rejected() {
        assert!(check_len(VIEW_MAX).is_ok());
        assert_eq!(check_len(VIEW_MAX + 1).unwrap_err(), status::HM_E_TOO_LARGE);
    }
}
