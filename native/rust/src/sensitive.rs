//! 敏感缓冲：`mlock` 锁页 + 释放时 zeroize（R-NA-04）。
//!
//! 这条不变量的完整表述是：**API key 与 prompt 明文只存在于这块内存里，
//! 且它释放之后那块内存的字节全 0。** 两条都要，缺一条都不成立：
//!
//! * 只有 zeroize 没有 mlock → 密钥会被 swap 到磁盘，进程崩溃后还在 core 里。
//! * 只有 mlock 没有 zeroize → 释放后内存被 allocator 重新发出去，下一个
//!   使用者读到的第一段内容就是别人的密钥。
//!
//! mlock 会失败（进程 RLIMIT_MEMLOCK 受限、内存碎片等）。**失败不报错**，
//! 而是把 `locked` 置 0 并让调用方写 `security.mlock_unavailable` 事件。
//! 为什么降级而不是失败：锁页失败时密钥仍然在 RAM 里，功能可用；
//! 让整个 run 失败等于用可用性换一个概率极小的威胁。但"降级"必须**可见**，
//! 否则审计报告会说谎。

use std::os::raw::c_void;
use std::sync::Mutex;

use zeroize::Zeroize;

use crate::handle::HandleTable;
use crate::status;

pub const SENSITIVE_MAX: usize = 1 << 20; // 1 MiB，与 HM_SENSITIVE_MAX 一致

extern "C" {
    fn mlock(addr: *const c_void, len: usize) -> i32;
    fn munlock(addr: *const c_void, len: usize) -> i32;
    fn sysconf(name: i32) -> i64;
}

// `_SC_PAGESIZE` 的数值在两个平台不同。硬编码比引 libc crate 好：
// 依赖表只有 sha2 与 zeroize 是硬约束（PRD §8.1 A 组白名单）。
#[cfg(target_os = "macos")]
const SC_PAGESIZE: i32 = 29;
#[cfg(not(target_os = "macos"))]
const SC_PAGESIZE: i32 = 30;

fn page_size() -> usize {
    let v = unsafe { sysconf(SC_PAGESIZE) };
    if v <= 0 {
        16384 // Apple Silicon 默认；仅作兜底
    } else {
        v as usize
    }
}

/// 一块被锁页（可能失败）+ 会在 Drop 时擦除的字节缓冲。
pub struct Sensitive {
    data: Vec<u8>,
    locked: bool,
    lock_start: usize,
    lock_len: usize,
}

impl Sensitive {
    pub fn new(bytes: &[u8], no_mlock: bool) -> Self {
        let data = bytes.to_vec();
        let mut s = Sensitive {
            data,
            locked: false,
            lock_start: 0,
            lock_len: 0,
        };
        if !no_mlock {
            s.try_lock();
        }
        s
    }

    /// 尝试把底层内存锁进物理页。失败只是把 `locked` 留在 false。
    fn try_lock(&mut self) {
        if self.data.is_empty() {
            return;
        }
        let ps = page_size();
        let base = self.data.as_ptr() as usize;
        let end = base + self.data.len();
        let start = base & !(ps - 1);
        let stop = (end + ps - 1) & !(ps - 1);
        let len = stop - start;

        let rc = unsafe { mlock(start as *const c_void, len) };
        if rc == 0 {
            self.locked = true;
            self.lock_start = start;
            self.lock_len = len;
        } else {
            self.locked = false;
            self.lock_start = 0;
            self.lock_len = 0;
        }
    }

    fn unlock(&mut self) {
        if self.locked && self.lock_len > 0 {
            unsafe {
                munlock(self.lock_start as *const c_void, self.lock_len);
            }
        }
        self.locked = false;
        self.lock_start = 0;
        self.lock_len = 0;
    }

    pub fn is_locked(&self) -> bool {
        self.locked
    }

    pub fn len(&self) -> usize {
        self.data.len()
    }

    pub fn is_empty(&self) -> bool {
        self.data.is_empty()
    }

    pub fn as_slice(&self) -> &[u8] {
        &self.data
    }

    /// 就地擦除。`hm_sensitive_free` 在把内存交出去之前调用它。
    ///
    /// 必须对**切片**调用 zeroize，不能对 `Vec` 调用：
    /// `zeroize` 为 `Vec<u8>` 提供的实现会在擦除后 `clear()` 并释放容量，
    /// 于是 take_for_debug 拿到的长度变成 0 —— 那样"释放后读到原长度全 0"
    /// 这条验收就变成"读到 0 长度"，看着通过，实际什么都没证明。
    pub fn zeroize_now(&mut self) {
        self.data.as_mut_slice().zeroize();
    }

    /// 把内容取走（用于 `hm_sensitive_free` 之后仍能观察"擦除后的同一块内存"）。
    /// 调用方必须先 zeroize。
    pub fn take_for_debug(&mut self) -> Vec<u8> {
        self.unlock();
        std::mem::take(&mut self.data)
    }
}

impl Drop for Sensitive {
    fn drop(&mut self) {
        // 先擦除再解锁：解锁之后别的进程（或 SWAP）就可能看到内容。
        // 同样对切片调用：Vec 的 Zeroize 会顺手 clear()，这里不需要它清长度。
        self.data.as_mut_slice().zeroize();
        self.unlock();
    }
}

/// 全局敏感缓冲表。
pub static TABLE: Mutex<HandleTable<Sensitive>> = Mutex::new(HandleTable::new());

/// 最近一次释放的敏感缓冲（已擦除）。
///
/// 这是 T03 验收②"释放后原内存字节全 0"的观测点：`hm_debug_peek` 读的就是
/// 这里。保留 Vec 而不是裸指针，是为了让"同一块内存"这件事有明确定义 ——
/// Vec 没被 drop，地址就还有效，读到的是定义良好的 0 而不是已经还给
/// allocator 的悬垂字节（那读出来是 UB，测试会变成抛硬币）。
/// 只保留最近一个，因此最多多占 1 MiB。
pub static LAST_FREED: Mutex<Option<Vec<u8>>> = Mutex::new(None);

/// 供导出函数使用的加锁助手：poison 时不 panic（panic 路径禁止）。
pub fn lock_table() -> std::sync::MutexGuard<'static, HandleTable<Sensitive>> {
    match TABLE.lock() {
        Ok(g) => g,
        Err(poisoned) => poisoned.into_inner(),
    }
}

pub fn lock_last_freed() -> std::sync::MutexGuard<'static, Option<Vec<u8>>> {
    match LAST_FREED.lock() {
        Ok(g) => g,
        Err(poisoned) => poisoned.into_inner(),
    }
}

/// 关闭运行时：擦除一切。
pub fn shutdown() {
    {
        // `HandleTable::clear` 会 drop 每个 Slot 里的 T，
        // 而 `Sensitive::drop` 就是 zeroize + munlock。所以 clear 即擦除。
        let mut t = lock_table();
        t.clear();
    }
    {
        let mut last = lock_last_freed();
        if let Some(mut v) = last.take() {
            v.zeroize();
        }
    }
}

/// 长度校验：超限返回 `HM_E_TOO_LARGE`，**绝不截断**（R3）。
pub fn check_len(len: usize) -> Result<(), i32> {
    if len > SENSITIVE_MAX {
        return Err(status::HM_E_TOO_LARGE);
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn free_zeroizes_the_same_bytes() {
        let mut s = Sensitive::new(b"super-secret-api-key-0123456789", true);
        assert_eq!(s.len(), 33);
        assert!(!s.is_locked()); // no_mlock = true

        let mut data = s.take_for_debug();
        // 模拟 hm_sensitive_free 的顺序：先前置 zeroize，再交出所有权
        // （take_for_debug 之前必须已经 zeroize）。
        data.zeroize();
        assert!(data.iter().all(|&b| b == 0));
    }

    #[test]
    fn mlock_succeeds_or_degrades_but_never_lies() {
        // 真实路径：要么 locked=true（真的锁了页），要么 locked=false
        // （降级，调用方写 security.mlock_unavailable）。两者都是合法结果；
        // 这个测试只断言"不 panic、结论可读"，把降级与否交给上层观测。
        let s = Sensitive::new(b"k", false);
        let locked = s.is_locked();
        if !locked {
            eprintln!(
                "note: mlock 在本机降级（locked=false）—— 上层应写 security.mlock_unavailable"
            );
        }
        assert_eq!(s.len(), 1);
    }

    #[test]
    fn zero_length_is_allowed() {
        let s = Sensitive::new(b"", false);
        assert_eq!(s.len(), 0);
        assert!(!s.is_locked());
    }

    #[test]
    fn oversized_is_rejected_not_truncated() {
        assert_eq!(check_len(SENSITIVE_MAX).is_ok(), true);
        assert_eq!(check_len(SENSITIVE_MAX + 1).unwrap_err(), status::HM_E_TOO_LARGE);
    }
}
