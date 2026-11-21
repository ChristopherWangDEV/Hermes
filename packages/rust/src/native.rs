//! 进程内绑定：运行时加载 `libhermesrt`，直接调 `hermes_rt_run_file`。
//!
//! **零依赖**：`dlopen`/`dlsym` 直接对 `extern "C"` 声明，不引 `libloading`。
//! 代价是要自己承担符号查找失败的错误处理，但那正是我们要显式说清楚的东西。
//!
//! 边界：只覆盖执行（run）。`serve` 的健康/模型端点是网络面。

use std::ffi::{CStr, CString};
use std::os::raw::{c_char, c_int, c_void};
use std::path::{Path, PathBuf};

#[cfg(target_os = "macos")]
#[link(name = "System")]
extern "C" {}

#[cfg(not(target_os = "macos"))]
#[link(name = "dl")]
extern "C" {}

extern "C" {
    fn dlopen(filename: *const c_char, flag: c_int) -> *mut c_void;
    fn dlsym(handle: *mut c_void, symbol: *const c_char) -> *mut c_void;
    fn dlerror() -> *mut c_char;
    fn dlclose(handle: *mut c_void) -> c_int;
}

const RTLD_NOW: c_int = 2;
const RTLD_LOCAL: c_int = 0;

type RunFileFn = unsafe extern "C" fn(
    unit: *const c_char,
    input: *const c_char,
    input_len: usize,
    out: *mut *mut c_char,
    err: *mut *mut c_char,
) -> c_int;
type FreeFn = unsafe extern "C" fn(p: *mut c_char);

/// 原生层错误。`code`：2 = 输入/文件问题，3 = 执行失败。
#[derive(Debug)]
pub struct NativeError {
    pub message: String,
    pub code: i32,
}

impl std::fmt::Display for NativeError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "hermes: {} (code={})", self.message, self.code)
    }
}

impl std::error::Error for NativeError {}

/// 一次加载、多次执行。
pub struct NativeRuntime {
    handle: *mut c_void,
    run_file: RunFileFn,
    free: FreeFn,
    unit: String,
}

impl NativeRuntime {
    /// 加载动态库并绑定符号。`library` 为 None 时按约定顺序查找。
    pub fn open(unit: &str, library: Option<&str>) -> Result<Self, NativeError> {
        let path = match library {
            Some(p) => PathBuf::from(p),
            None => find_library()?,
        };
        let cpath = CString::new(path.to_string_lossy().as_bytes()).map_err(|e| NativeError {
            message: format!("path contains NUL: {e}"),
            code: 0,
        })?;
        let handle = unsafe { dlopen(cpath.as_ptr(), RTLD_NOW | RTLD_LOCAL) };
        if handle.is_null() {
            return Err(NativeError {
                message: format!("failed to load {}: {}", path.display(), last_error()),
                code: 0,
            });
        }
        let run_sym = unsafe { dlsym(handle, b"hermes_rt_run_file\0".as_ptr() as *const c_char) };
        let free_sym = unsafe { dlsym(handle, b"hermes_rt_free\0".as_ptr() as *const c_char) };
        if run_sym.is_null() || free_sym.is_null() {
            unsafe { dlclose(handle) };
            return Err(NativeError {
                message: format!("{} is missing hermes_rt_run_file / hermes_rt_free", path.display()),
                code: 0,
            });
        }
        Ok(Self {
            handle,
            run_file: unsafe { std::mem::transmute::<*mut c_void, RunFileFn>(run_sym) },
            free: unsafe { std::mem::transmute::<*mut c_void, FreeFn>(free_sym) },
            unit: unit.to_string(),
        })
    }

    /// 执行构造时给的那个 unit。
    pub fn run(&self, input_json: Option<&str>) -> Result<String, NativeError> {
        self.run_on(&self.unit, input_json)
    }

    /// 执行指定 unit。
    pub fn run_on(&self, unit: &str, input_json: Option<&str>) -> Result<String, NativeError> {
        let cunit = CString::new(unit).map_err(|e| NativeError {
            message: format!("path contains NUL: {e}"),
            code: 0,
        })?;
        let payload = input_json.unwrap_or("");
        let cinput = CString::new(payload).map_err(|e| NativeError {
            message: format!("input contains NUL: {e}"),
            code: 0,
        })?;

        let mut out: *mut c_char = std::ptr::null_mut();
        let mut err: *mut c_char = std::ptr::null_mut();
        let code = unsafe {
            (self.run_file)(
                cunit.as_ptr(),
                if payload.is_empty() { std::ptr::null() } else { cinput.as_ptr() },
                payload.len(),
                &mut out,
                &mut err,
            )
        };

        // 两块内存都由原生层分配，必须用它的 free 归还。
        // SAFETY：两个指针要么是 null，要么由 hermes_rt_run_file 分配且只在这里释放一次。
        let out_str = unsafe { take_string(&self.free, out) };
        let err_str = unsafe { take_string(&self.free, err) };

        if code != 0 {
            return Err(NativeError {
                message: err_str.unwrap_or_else(|| format!("execution failed (code={code})")),
                code,
            });
        }
        Ok(out_str.unwrap_or_default())
    }
}

impl Drop for NativeRuntime {
    fn drop(&mut self) {
        if !self.handle.is_null() {
            unsafe { dlclose(self.handle) };
            self.handle = std::ptr::null_mut();
        }
    }
}

unsafe fn take_string(free: &FreeFn, p: *mut c_char) -> Option<String> {
    if p.is_null() {
        return None;
    }
    let s = CStr::from_ptr(p).to_string_lossy().into_owned();
    free(p);
    Some(s)
}

fn last_error() -> String {
    unsafe {
        let e = dlerror();
        if e.is_null() {
            "(no dlopen error message)".to_string()
        } else {
            CStr::from_ptr(e).to_string_lossy().into_owned()
        }
    }
}

/// 按 HERMES_HOME → 仓库 build/ → 包旁 lib/ 的顺序找动态库。
pub fn find_library() -> Result<PathBuf, NativeError> {
    let name = if cfg!(target_os = "macos") {
        "libhermesrt.dylib"
    } else if cfg!(target_os = "windows") {
        "hermesrt.dll"
    } else {
        "libhermesrt.so"
    };
    let mut tried: Vec<PathBuf> = Vec::new();
    if let Ok(home) = std::env::var("HERMES_HOME") {
        tried.push(Path::new(&home).join("lib").join(name));
    }
    for d in ["build", "../build", "../../build", "../../lib"] {
        tried.push(Path::new(d).join(name));
    }
    for p in &tried {
        if p.exists() {
            return Ok(p.clone());
        }
    }
    Err(NativeError {
        message: format!(
            "could not find {name}. tried: {}. release packages ship their own lib/; during development run scripts/build.sh first, or point HERMES_HOME at the install root.",
            tried.iter().map(|p| p.display().to_string()).collect::<Vec<_>>().join(", ")
        ),
        code: 0,
    })
}
