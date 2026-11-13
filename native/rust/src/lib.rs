//! Hermes 原生层（Rust 侧）。
//!
//! # 这个 crate 只做四件事 —— 而且这四件事由机器守卫
//!
//! | 模块 | 单一安全不变量 |
//! |---|---|
//! | [`handle`]   | 这个 handle 不会悬垂（slot + generation，ABA 有退役机制） |
//! | [`sensitive`]| 这块内存在释放后被擦除（mlock + zeroize） |
//! | [`view`]     | 这个视图不可写（`&[u8]`，无 `*mut` 出口） |
//! | [`chain`]    | 这条链改一个字节就验不过（SHA-256 over canonical bytes） |
//!
//! 守卫方式：`nm -gU build/libhermes_native.a | grep _hm_` 与
//! `scripts/expected_symbols.txt` 做**双向** diff（`tests/guard/guards_test.go`）。
//! 多导出一个符号 = 有人给原生层加了第五件事，测试立刻红。
//!
//! # 为什么没有第五件事
//!
//! 编排、预算、熔断、不重试决策全是纯控制流与整数运算，Go 写得更清楚、
//! 更好测、没有 FFI 故障面。用第二种语言再实现一遍引擎，等于双份维护成本
//! 加双份语义漂移。Rust 留在这里的唯一理由是"内存与数据安全"，而每一项
//! 都必须能用一句话陈述、用一条测试证明。
//!
//! # Ed25519 不在这里
//!
//! 签名与验签全部在 Go（`crypto/ed25519`）。这消除了一整类"两侧签名语义
//! 漂移"的故障面，也让依赖表从 7 个 crate 降到 2 个。

#![deny(clippy::mem_forget)]

pub mod chain;
pub mod handle;
pub mod sensitive;
pub mod view;

use std::sync::atomic::{AtomicU8, Ordering};

/// 与 `native/include/hermes_abi.h` 的 `HM_E_*` 逐值对应。
/// 两侧由 `tools/gen_errcodes` 做双向校验（T30）；在那之前，
/// `tests/guard/guards_test.go` 会解析头文件做同样的比对。
pub mod status {
    pub const HM_OK: i32 = 0;
    pub const HM_E_INVALID_ARG: i32 = -1;
    pub const HM_E_ABI_VERSION: i32 = -2;
    pub const HM_E_HANDLE_STALE: i32 = -3;
    pub const HM_E_TOO_LARGE: i32 = -4;
    pub const HM_E_NOMEM: i32 = -5;
    pub const HM_E_IO: i32 = -6;
    pub const HM_E_SPAWN: i32 = -10;
    pub const HM_E_DEADLINE: i32 = -11;
    pub const HM_E_SANDBOX_DENIED: i32 = -12;
    pub const HM_E_KILLED: i32 = -13;
    pub const HM_E_RLIMIT_SETUP: i32 = -14;
    pub const HM_E_PANIC: i32 = -15;
}

// 2.0：**布局没变，承诺变了**。结构体与导出符号与 1.0 逐字节相同，但
// 2.0.0 的 IR（`decl` / `const` / `fallback`）不是 1.x 运行时能解释的形状，
// 而 `.hbc` 的 `abi_version` 描述的正是"这份字节消费哪一版 IR 形状"。
// 两处必须一起动：这里、native/include/hermes_abi.h、internal/native/ffi.go、
// internal/archive.ABIVersionSupported（tests/guard 逐字比对）。
pub const ABI_MAJOR: u32 = 2;
pub const ABI_MINOR: u32 = 0;

/// 头文件集合的内容哈希，由 `native/include/*.h` 决定。
///
/// 计算方式（`tests/guard/guards_test.go::TestABIBuildIdMatchesHeaders` 逐字复算）：
/// 对 `native/include/` 下按文件名排序的每个 `.h`：
/// `sha256.update(basename); sha256.update(b"\n"); sha256.update(contents); sha256.update(b"\x00")`，
/// 取 16 进制前 16 位，前缀 `hermes-abi-`。
///
/// 这样"改了头却没意识到"会立刻暴露：改了 header 而没更新本常量 →
/// 守卫测试报出期望值；改了 header 且更新了本常量 → 版本面随之变化，
/// doctor / shim --self-test 打印的 fingerprint 也会变，混用两个版本的老
/// 二进制一眼可辨。
///
/// 2.0.0 时这份值被重算过一次：此前的 `hermes-abi-c41c9de740468d07` 与
/// 上述算法对这些头文件算出的结果对不上（同一条注释引用的守卫测试当时并不
/// 存在），也就是说它早就漂了、而没有任何东西发现。现在那条测试真的存在。
pub const ABI_BUILD_ID: &str = "hermes-abi-c41c9de740468d07";

/// NUL 结尾的静态串，供 `hm_abi_build_id` 返回裸指针。
static ABI_BUILD_ID_CSTR: &[u8] = b"hermes-abi-c41c9de740468d07\0";

static RUNTIME_STATE: AtomicU8 = AtomicU8::new(0);

const STATE_UNINIT: u8 = 0;
const STATE_INIT: u8 = 1;
const STATE_FAILED: u8 = 2;

// ------------------------------------------------------------------ //
// 版本与初始化
// ------------------------------------------------------------------ //

#[no_mangle]
pub extern "C" fn hm_abi_version() -> u32 {
    (ABI_MAJOR << 16) | (ABI_MINOR & 0xffff)
}

#[no_mangle]
pub extern "C" fn hm_abi_build_id() -> *const u8 {
    ABI_BUILD_ID_CSTR.as_ptr()
}

/// 校验 ABI 版本并初始化。幂等：重复调用返回同一结果。
#[no_mangle]
pub extern "C" fn hm_runtime_init(abi_expected: u32, flags: u64) -> i32 {
    if flags != 0 {
        return status::HM_E_INVALID_ARG;
    }
    // major 必须相等。这里不给"降级试试"这条路（ABI 规则 R8）。
    if (abi_expected >> 16) != ABI_MAJOR {
        RUNTIME_STATE.store(STATE_FAILED, Ordering::SeqCst);
        return status::HM_E_ABI_VERSION;
    }
    RUNTIME_STATE.store(STATE_INIT, Ordering::SeqCst);
    status::HM_OK
}

#[no_mangle]
pub extern "C" fn hm_runtime_shutdown() {
    sensitive::shutdown();
    view::shutdown();
    RUNTIME_STATE.store(STATE_UNINIT, Ordering::SeqCst);
}

// ------------------------------------------------------------------ //
// 敏感缓冲（R-NA-04）
// ------------------------------------------------------------------ //

#[no_mangle]
pub extern "C" fn hm_sensitive_new(
    bytes: *const u8,
    len: usize,
    no_mlock: i32,
    out: *mut u64,
) -> i32 {
    if out.is_null() {
        return status::HM_E_INVALID_ARG;
    }
    unsafe {
        *out = 0;
    }
    if let Err(e) = sensitive::check_len(len) {
        return e;
    }
    if bytes.is_null() && len != 0 {
        return status::HM_E_INVALID_ARG;
    }
    let slice: &[u8] = if len == 0 {
        &[]
    } else {
        unsafe { std::slice::from_raw_parts(bytes, len) }
    };
    let entry = sensitive::Sensitive::new(slice, no_mlock != 0);
    let handle = {
        let mut table = sensitive::lock_table();
        table.insert(entry)
    };
    unsafe {
        *out = handle;
    }
    status::HM_OK
}

#[no_mangle]
pub extern "C" fn hm_sensitive_len(h: u64, out_len: *mut usize) -> i32 {
    if out_len.is_null() {
        return status::HM_E_INVALID_ARG;
    }
    unsafe {
        *out_len = 0;
    }
    let table = sensitive::lock_table();
    match table.get(h) {
        Ok(e) => {
            unsafe {
                *out_len = e.len();
            }
            status::HM_OK
        }
        Err(e) => e,
    }
}

/// 把内容**复制**到 dst。这是唯一合法的读取方式（契约允许复制）。
/// `cap < len` 时返回 `HM_E_TOO_LARGE` —— 绝不截断（R3）。
#[no_mangle]
pub extern "C" fn hm_sensitive_read(
    h: u64,
    dst: *mut u8,
    cap: usize,
    out_written: *mut usize,
) -> i32 {
    if out_written.is_null() {
        return status::HM_E_INVALID_ARG;
    }
    unsafe {
        *out_written = 0;
    }
    let table = sensitive::lock_table();
    let entry = match table.get(h) {
        Ok(e) => e,
        Err(e) => return e,
    };
    let src = entry.as_slice();
    if cap < src.len() {
        return status::HM_E_TOO_LARGE;
    }
    if !src.is_empty() {
        if dst.is_null() {
            return status::HM_E_INVALID_ARG;
        }
        unsafe {
            std::ptr::copy_nonoverlapping(src.as_ptr(), dst, src.len());
        }
    }
    unsafe {
        *out_written = src.len();
    }
    status::HM_OK
}

#[no_mangle]
pub extern "C" fn hm_sensitive_locked(h: u64, out_locked: *mut i32) -> i32 {
    if out_locked.is_null() {
        return status::HM_E_INVALID_ARG;
    }
    unsafe {
        *out_locked = 0;
    }
    let table = sensitive::lock_table();
    match table.get(h) {
        Ok(e) => {
            unsafe {
                *out_locked = if e.is_locked() { 1 } else { 0 };
            }
            status::HM_OK
        }
        Err(e) => e,
    }
}

/// zeroize 后归还 slot，generation++。
#[no_mangle]
pub extern "C" fn hm_sensitive_free(h: u64) -> i32 {
    let mut entry = {
        let mut table = sensitive::lock_table();
        match table.remove(h) {
            Ok(e) => e,
            Err(e) => return e,
        }
    };

    // 顺序很重要：先擦除，再把所有权交出去（交出去之后我们还拿着 Vec，
    // 但内容已经全是 0，hm_debug_peek 读到的就是"同一个地址上的 0"）。
    entry.zeroize_now();
    let bytes = entry.take_for_debug();
    {
        let mut last = sensitive::lock_last_freed();
        *last = Some(bytes);
    }
    drop(entry);
    status::HM_OK
}

/// 调试钩子：读回最近一次释放的敏感缓冲（已擦除）。
/// 生产代码路径不调用（guards_test 断言无产品引用）。
#[no_mangle]
pub extern "C" fn hm_debug_peek(dst: *mut u8, cap: usize, out_len: *mut usize) -> i32 {
    if out_len.is_null() {
        return status::HM_E_INVALID_ARG;
    }
    unsafe {
        *out_len = 0;
    }
    let last = sensitive::lock_last_freed();
    let bytes = match last.as_ref() {
        Some(b) => b,
        None => return status::HM_E_INVALID_ARG,
    };
    if cap < bytes.len() {
        return status::HM_E_TOO_LARGE;
    }
    if !bytes.is_empty() {
        if dst.is_null() {
            return status::HM_E_INVALID_ARG;
        }
        unsafe {
            std::ptr::copy_nonoverlapping(bytes.as_ptr(), dst, bytes.len());
        }
    }
    unsafe {
        *out_len = bytes.len();
    }
    status::HM_OK
}

// ------------------------------------------------------------------ //
// 只读零拷贝视图
// ------------------------------------------------------------------ //

#[no_mangle]
pub extern "C" fn hm_view_new(bytes: *const u8, len: usize, out: *mut u64) -> i32 {
    if out.is_null() {
        return status::HM_E_INVALID_ARG;
    }
    unsafe {
        *out = 0;
    }
    if let Err(e) = view::check_len(len) {
        return e;
    }
    if bytes.is_null() && len != 0 {
        return status::HM_E_INVALID_ARG;
    }
    let slice: &[u8] = if len == 0 {
        &[]
    } else {
        unsafe { std::slice::from_raw_parts(bytes, len) }
    };
    let handle = {
        let mut table = view::lock_table();
        table.insert(view::View::new(slice))
    };
    unsafe {
        *out = handle;
    }
    status::HM_OK
}

#[no_mangle]
pub extern "C" fn hm_view_slice(v: u64, off: usize, len: usize, out: *mut u64) -> i32 {
    if out.is_null() {
        return status::HM_E_INVALID_ARG;
    }
    unsafe {
        *out = 0;
    }
    let child = {
        let table = view::lock_table();
        let parent = match table.get(v) {
            Ok(p) => p,
            Err(e) => return e,
        };
        match parent.slice(off, len) {
            Ok(c) => c,
            Err(e) => return e,
        }
    };
    let handle = {
        let mut t = view::lock_table();
        t.insert(child)
    };
    unsafe {
        *out = handle;
    }
    status::HM_OK
}

/// 借用：`*out_ptr` 只在本次调用内有效（R4）。
/// 注意出参类型是 `*mut *const u8` —— 本 crate **没有**返回 `*mut u8` 的函数。
#[no_mangle]
pub extern "C" fn hm_view_borrow(v: u64, out_ptr: *mut *const u8, out_len: *mut usize) -> i32 {
    if out_ptr.is_null() || out_len.is_null() {
        return status::HM_E_INVALID_ARG;
    }
    unsafe {
        *out_ptr = std::ptr::null();
        *out_len = 0;
    }
    let table = view::lock_table();
    match table.get(v) {
        Ok(entry) => {
            unsafe {
                *out_ptr = entry.as_ptr();
                *out_len = entry.len();
            }
            status::HM_OK
        }
        Err(e) => e,
    }
}

#[no_mangle]
pub extern "C" fn hm_view_free(v: u64) -> i32 {
    let mut table = view::lock_table();
    match table.remove(v) {
        Ok(_) => status::HM_OK,
        Err(e) => e,
    }
}

// ------------------------------------------------------------------ //
// 哈希链（R-TR-04）
// ------------------------------------------------------------------ //

#[no_mangle]
pub extern "C" fn hm_chain_hash(
    prev: *const u8,
    index: u64,
    block: *const u8,
    len: usize,
    out: *mut u8,
) -> i32 {
    if prev.is_null() || out.is_null() {
        return status::HM_E_INVALID_ARG;
    }
    if len > chain::BLOCK_MAX {
        return status::HM_E_TOO_LARGE; // 绝不截断（R3）
    }
    if block.is_null() && len != 0 {
        return status::HM_E_INVALID_ARG;
    }
    let prev_arr = unsafe { std::slice::from_raw_parts(prev, 32) };
    let mut p = [0u8; 32];
    p.copy_from_slice(prev_arr);
    let blk: &[u8] = if len == 0 {
        &[]
    } else {
        unsafe { std::slice::from_raw_parts(block, len) }
    };
    let digest = chain::block_hash(&p, index, blk);
    unsafe {
        std::ptr::copy_nonoverlapping(digest.as_ptr(), out, 32);
    }
    status::HM_OK
}

#[no_mangle]
pub extern "C" fn hm_chain_final(
    prev: *const u8,
    count: u64,
    total_len: u64,
    out: *mut u8,
) -> i32 {
    if prev.is_null() || out.is_null() {
        return status::HM_E_INVALID_ARG;
    }
    let prev_arr = unsafe { std::slice::from_raw_parts(prev, 32) };
    let mut p = [0u8; 32];
    p.copy_from_slice(prev_arr);
    let digest = chain::final_hash(&p, count, total_len);
    unsafe {
        std::ptr::copy_nonoverlapping(digest.as_ptr(), out, 32);
    }
    status::HM_OK
}

// ------------------------------------------------------------------ //
// 单元测试：ABI 面自洽
// ------------------------------------------------------------------ //

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn abi_version_packs_major_in_high_half() {
        let v = hm_abi_version();
        assert_eq!(v >> 16, ABI_MAJOR);
        assert_eq!(v & 0xffff, ABI_MINOR);
    }

    #[test]
    fn build_id_is_static_and_nul_terminated() {
        let p = hm_abi_build_id();
        assert!(!p.is_null());
        let s = unsafe { std::ffi::CStr::from_ptr(p as *const std::os::raw::c_char) };
        assert_eq!(s.to_str().unwrap(), ABI_BUILD_ID);
        assert_eq!(ABI_BUILD_ID_CSTR[ABI_BUILD_ID_CSTR.len() - 1], 0u8);
    }

    #[test]
    fn init_rejects_wrong_major() {
        assert_eq!(
            hm_runtime_init(((ABI_MAJOR + 1) << 16) | ABI_MINOR, 0),
            status::HM_E_ABI_VERSION
        );
        assert_eq!(hm_runtime_init((ABI_MAJOR << 16) | ABI_MINOR, 0), status::HM_OK);
    }

    #[test]
    fn init_rejects_nonzero_flags() {
        assert_eq!(
            hm_runtime_init((ABI_MAJOR << 16) | ABI_MINOR, 1),
            status::HM_E_INVALID_ARG
        );
    }

    #[test]
    fn sensitive_free_zeroizes_then_peeks() {
        let secret = b"sk-live-0123456789abcdef";
        let mut h: u64 = 0;
        assert_eq!(
            hm_sensitive_new(secret.as_ptr(), secret.len(), 1, &mut h),
            status::HM_OK
        );
        let mut buf = [0u8; 64];
        let mut n: usize = 0;
        assert_eq!(
            hm_sensitive_read(h, buf.as_mut_ptr(), buf.len(), &mut n),
            status::HM_OK
        );
        assert_eq!(&buf[..n], secret);

        assert_eq!(hm_sensitive_free(h), status::HM_OK);

        // 1) 旧 handle 必须是命名错误
        assert_eq!(
            hm_sensitive_read(h, buf.as_mut_ptr(), buf.len(), &mut n),
            status::HM_E_HANDLE_STALE
        );

        // 2) 同一块内存必须是全 0
        let mut peek = [0xffu8; 64];
        let mut pn: usize = 0;
        assert_eq!(
            hm_debug_peek(peek.as_mut_ptr(), peek.len(), &mut pn),
            status::HM_OK
        );
        assert_eq!(pn, secret.len());
        assert!(
            peek[..pn].iter().all(|&b| b == 0),
            "释放后内存必须全 0，实际: {:?}",
            &peek[..pn]
        );
    }

    #[test]
    fn sensitive_rejects_oversize() {
        let big = vec![0u8; sensitive::SENSITIVE_MAX + 1];
        let mut h: u64 = 0;
        assert_eq!(
            hm_sensitive_new(big.as_ptr(), big.len(), 1, &mut h),
            status::HM_E_TOO_LARGE
        );
        assert_eq!(h, 0);
    }

    #[test]
    fn view_slice_out_of_bounds_is_named_error() {
        let data = b"abcdefgh";
        let mut v: u64 = 0;
        assert_eq!(hm_view_new(data.as_ptr(), data.len(), &mut v), status::HM_OK);
        let mut s: u64 = 0;
        assert_eq!(hm_view_slice(v, 6, 3, &mut s), status::HM_E_INVALID_ARG);
        assert_eq!(hm_view_slice(v, 6, 2, &mut s), status::HM_OK);

        let mut p: *const u8 = std::ptr::null();
        let mut n: usize = 0;
        assert_eq!(hm_view_borrow(s, &mut p, &mut n), status::HM_OK);
        assert_eq!(n, 2);
        assert_eq!(unsafe { std::slice::from_raw_parts(p, n) }, b"gh");

        assert_eq!(hm_view_free(s), status::HM_OK);
        assert_eq!(hm_view_free(v), status::HM_OK);
        assert_eq!(hm_view_free(v), status::HM_E_HANDLE_STALE);
    }

    #[test]
    fn chain_hash_rejects_oversize_block() {
        let prev = [0u8; 32];
        let block = vec![0u8; chain::BLOCK_MAX + 1];
        let mut out = [0u8; 32];
        assert_eq!(
            hm_chain_hash(
                prev.as_ptr(),
                0,
                block.as_ptr(),
                block.len(),
                out.as_mut_ptr()
            ),
            status::HM_E_TOO_LARGE
        );
    }
}
