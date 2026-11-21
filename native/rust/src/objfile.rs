// objfile.rs —— 读目标文件的动态依赖名（ELF 的 DT_NEEDED / Mach-O 的 LC_LOAD_DYLIB）。
//
// # 为什么这件事在 Rust
//
// 它解析的是**不可信输入**：编译产物可能来自任何地方，也可能是被截断/伪造的
// 文件。这类代码出错就是**越界读**，而越界读正是 Rust 在这里的唯一理由
// （见 lib.rs 顶部的教条表）。单一安全不变量：
//
//     **读到的每一个偏移都先与文件长度比较过，越界一律返回错误、不 panic。**
//
// # 为什么不用 crate
//
// 零第三方依赖是红线。这里只认两条最小走法：ELF 的 `PT_DYNAMIC → DT_NEEDED →
// DT_STRTAB`，与 Mach-O 的 `LC_LOAD_DYLIB`。两条走法各四十行，比引入一个
// 解析库再去审它的许可证便宜。

use crate::status;

/// 一条动态依赖的名字（原样返回，不解析 `@rpath` 之类的占位前缀）。
pub struct Libs {
    pub names: Vec<String>,
}

/// 解析错误：与 Go 侧 `ErrUnsupportedFormat` / IO 错误一一对应。
pub enum ObjErr {
    /// 不是 ELF / Mach-O（裸机 firmware 等）。调用方据此区分"格式本来就没有
    /// 动态依赖概念"与"宿主产物读不出来"。
    Unsupported,
    /// 文件读不了（不存在 / 权限 / 太短）。
    Io,
    /// 是 ELF / Mach-O，但结构坏了（截断、越界偏移）。
    Malformed,
}

impl ObjErr {
    pub fn status(&self) -> i32 {
        match self {
            ObjErr::Unsupported => status::HM_E_FORMAT_UNSUPPORTED,
            ObjErr::Io => status::HM_E_IO,
            ObjErr::Malformed => status::HM_E_MALFORMED,
        }
    }
}

fn rd_u16(b: &[u8], off: usize, be: bool) -> Option<u16> {
    let s = b.get(off..off + 2)?;
    let a = [s[0], s[1]];
    Some(if be { u16::from_be_bytes(a) } else { u16::from_le_bytes(a) })
}

fn rd_u32(b: &[u8], off: usize, be: bool) -> Option<u32> {
    let s = b.get(off..off + 4)?;
    let a = [s[0], s[1], s[2], s[3]];
    Some(if be { u32::from_be_bytes(a) } else { u32::from_le_bytes(a) })
}

fn rd_u64(b: &[u8], off: usize, be: bool) -> Option<u64> {
    let s = b.get(off..off + 8)?;
    let mut a = [0u8; 8];
    a.copy_from_slice(s);
    Some(if be { u64::from_be_bytes(a) } else { u64::from_le_bytes(a) })
}

/// 读一个 NUL 结尾的字符串（不含 NUL）。没有 NUL 就返回 None —— 截断文件
/// 不能被当成"名字就是这么长"。
fn cstr(b: &[u8], off: usize) -> Option<String> {
    if off >= b.len() {
        return None;
    }
    let rest = &b[off..];
    let end = rest.iter().position(|c| *c == 0)?;
    std::str::from_utf8(&rest[..end]).ok().map(|s| s.to_string())
}

fn dedupe(mut v: Vec<String>) -> Vec<String> {
    let mut seen = std::collections::BTreeSet::new();
    v.retain(|s| !s.is_empty() && seen.insert(s.clone()));
    v
}

/// 读一个文件的动态依赖。空结果 = 静态链接（不是错误）。
pub fn dynamic_libs(path: &str) -> Result<Vec<String>, ObjErr> {
    let bytes = std::fs::read(path).map_err(|_| ObjErr::Io)?;
    if bytes.len() < 4 {
        return Err(ObjErr::Io);
    }
    let m = [bytes[0], bytes[1], bytes[2], bytes[3]];
    if m == [0x7f, b'E', b'L', b'F'] {
        return elf_dynamic_libs(&bytes);
    }
    let le = u32::from_le_bytes(m);
    let be = u32::from_be_bytes(m);
    if is_macho(le) || is_macho(be) {
        return macho_dynamic_libs(&bytes);
    }
    if is_fat(le) || is_fat(be) {
        return fat_dynamic_libs(&bytes);
    }
    Err(ObjErr::Unsupported)
}

fn is_macho(m: u32) -> bool {
    // 0xfeedfacf / 0xfeedface 与它们的字节交换形态。
    m == 0xfeed_facf || m == 0xfeed_face || m == 0xcffa_edfe || m == 0xcefa_edfe
}

fn is_fat(m: u32) -> bool {
    m == 0xcafe_babe || m == 0xbeba_feca || m == 0xcafe_babf || m == 0xbfba_feca
}

// ---- ELF -------------------------------------------------------------------

fn elf_dynamic_libs(b: &[u8]) -> Result<Vec<String>, ObjErr> {
    if b.len() < 0x40 {
        return Err(ObjErr::Malformed);
    }
    let class = b[4]; // 1 = 32 位, 2 = 64 位
    let data = b[5]; // 1 = 小端, 2 = 大端
    if (class != 1 && class != 2) || (data != 1 && data != 2) {
        return Err(ObjErr::Malformed);
    }
    let be = data == 2;
    let is64 = class == 2;

    let (phoff, phentsize, phnum) = if is64 {
        (
            rd_u64(b, 0x20, be).ok_or(ObjErr::Malformed)?,
            rd_u16(b, 0x36, be).ok_or(ObjErr::Malformed)? as usize,
            rd_u16(b, 0x38, be).ok_or(ObjErr::Malformed)? as usize,
        )
    } else {
        (
            rd_u32(b, 0x1c, be).ok_or(ObjErr::Malformed)? as u64,
            rd_u16(b, 0x2a, be).ok_or(ObjErr::Malformed)? as usize,
            rd_u16(b, 0x2c, be).ok_or(ObjErr::Malformed)? as usize,
        )
    };
    if phentsize == 0 || phnum == 0 {
        return Ok(Vec::new()); // 没有程序头 = 没有动态段（.o 文件就是这样）
    }

    // PT_DYNAMIC 的位置，以及 vaddr → 文件偏移 的映射（解析 DT_STRTAB 要用）。
    let mut dyn_off = None;
    let mut dyn_size = 0u64;
    let mut loads: Vec<(u64, u64, u64)> = Vec::new(); // (vaddr, offset, filesz)
    for i in 0..phnum {
        let base = phoff as usize + i * phentsize;
        let p_type = rd_u32(b, base, be).ok_or(ObjErr::Malformed)?;
        let (p_offset, p_vaddr, p_filesz) = if is64 {
            (
                rd_u64(b, base + 0x08, be).ok_or(ObjErr::Malformed)?,
                rd_u64(b, base + 0x10, be).ok_or(ObjErr::Malformed)?,
                rd_u64(b, base + 0x20, be).ok_or(ObjErr::Malformed)?,
            )
        } else {
            (
                rd_u32(b, base + 0x04, be).ok_or(ObjErr::Malformed)? as u64,
                rd_u32(b, base + 0x08, be).ok_or(ObjErr::Malformed)? as u64,
                rd_u32(b, base + 0x10, be).ok_or(ObjErr::Malformed)? as u64,
            )
        };
        match p_type {
            2 => {
                dyn_off = Some(p_offset);
                dyn_size = p_filesz;
            }
            1 => loads.push((p_vaddr, p_offset, p_filesz)),
            _ => {}
        }
    }
    let dyn_off = match dyn_off {
        Some(o) => o,
        None => return Ok(Vec::new()), // 静态链接
    };

    // 扫动态项：先收 DT_NEEDED 的字符串偏移，再拿 DT_STRTAB 的地址。
    let ent = if is64 { 16usize } else { 8usize };
    let mut needed: Vec<u64> = Vec::new();
    let mut strtab_addr: Option<u64> = None;
    let mut n = 0usize;
    loop {
        let off = dyn_off as usize + n * ent;
        if (n as u64) * (ent as u64) >= dyn_size {
            break;
        }
        let tag = if is64 {
            rd_u64(b, off, be).ok_or(ObjErr::Malformed)?
        } else {
            rd_u32(b, off, be).ok_or(ObjErr::Malformed)? as u64
        };
        let val = if is64 {
            rd_u64(b, off + 8, be).ok_or(ObjErr::Malformed)?
        } else {
            rd_u32(b, off + 4, be).ok_or(ObjErr::Malformed)? as u64
        };
        match tag {
            0 => break,        // DT_NULL
            1 => needed.push(val), // DT_NEEDED
            5 => strtab_addr = Some(val), // DT_STRTAB
            _ => {}
        }
        n += 1;
    }
    let strtab_addr = match strtab_addr {
        Some(a) => a,
        None => return Ok(Vec::new()),
    };
    // vaddr → 文件偏移
    let mut strtab_off = None;
    for (vaddr, offset, filesz) in loads {
        if strtab_addr >= vaddr && strtab_addr < vaddr.saturating_add(filesz) {
            strtab_off = Some((offset + (strtab_addr - vaddr)) as usize);
            break;
        }
    }
    let strtab_off = match strtab_off {
        Some(o) => o,
        None => return Err(ObjErr::Malformed),
    };

    let mut out = Vec::new();
    for rel in needed {
        match cstr(b, strtab_off + rel as usize) {
            Some(s) => out.push(s),
            None => return Err(ObjErr::Malformed),
        }
    }
    Ok(dedupe(out))
}

// ---- Mach-O ----------------------------------------------------------------

fn macho_dynamic_libs(b: &[u8]) -> Result<Vec<String>, ObjErr> {
    let be = u32::from_be_bytes([b[0], b[1], b[2], b[3]]) == 0xfeed_facf
        || u32::from_be_bytes([b[0], b[1], b[2], b[3]]) == 0xfeed_face;
    let magic = rd_u32(b, 0, be).ok_or(ObjErr::Malformed)?;
    let is64 = magic == 0xfeed_facf;
    let (ncmds, hdr) = if is64 {
        (rd_u32(b, 0x10, be).ok_or(ObjErr::Malformed)?, 32usize)
    } else {
        (rd_u32(b, 0x0c, be).ok_or(ObjErr::Malformed)?, 28usize)
    };
    let mut off = hdr;
    let mut out = Vec::new();
    for _ in 0..ncmds {
        let cmd = rd_u32(b, off, be).ok_or(ObjErr::Malformed)?;
        let size = rd_u32(b, off + 4, be).ok_or(ObjErr::Malformed)? as usize;
        if size < 8 {
            return Err(ObjErr::Malformed);
        }
        // LC_LOAD_DYLIB / LC_LOAD_WEAK_DYLIB / LC_REEXPORT_DYLIB
        if cmd == 0xc || cmd == 0x8000_0018 || cmd == 0x8000_001f {
            let name_off = rd_u32(b, off + 8, be).ok_or(ObjErr::Malformed)? as usize;
            match cstr(b, off + name_off) {
                Some(s) => out.push(s),
                None => return Err(ObjErr::Malformed),
            }
        }
        off = off.checked_add(size).ok_or(ObjErr::Malformed)?;
        if off > b.len() {
            return Err(ObjErr::Malformed);
        }
    }
    Ok(dedupe(out))
}

fn fat_dynamic_libs(b: &[u8]) -> Result<Vec<String>, ObjErr> {
    // fat 头是大端（0xcafebabe 的字节序即 BE）。
    let be = u32::from_be_bytes([b[0], b[1], b[2], b[3]]) == 0xcafe_babe
        || u32::from_be_bytes([b[0], b[1], b[2], b[3]]) == 0xcafe_babf;
    let n = rd_u32(b, 4, be).ok_or(ObjErr::Malformed)? as usize;
    if n == 0 {
        return Ok(Vec::new());
    }
    let magic = rd_u32(b, 0, be).ok_or(ObjErr::Malformed)?;
    let fat64 = magic == 0xcafe_babf;
    let (offset, _size) = if fat64 {
        (
            rd_u64(b, 8 + 8, be).ok_or(ObjErr::Malformed)?,
            rd_u64(b, 8 + 16, be).ok_or(ObjErr::Malformed)?,
        )
    } else {
        (
            rd_u32(b, 8 + 8, be).ok_or(ObjErr::Malformed)? as u64,
            rd_u32(b, 8 + 12, be).ok_or(ObjErr::Malformed)? as u64,
        )
    };
    // 只看第一片：cc 的产物永远单架构，取第一片是为了让 `DynamicLibs(/usr/bin/true)`
    // 这类诊断调用不至于在半数 macOS 系统二进制上失败。
    let off = offset as usize;
    let slice = b.get(off..).ok_or(ObjErr::Malformed)?;
    macho_dynamic_libs(slice)
}

// ---- 符号状态（defined / undefined / 不存在） --------------------------------
//
// 为什么需要：`hermes cc` 要验证产物里**确实定义**了 per-unit 符号
// （`hermes_<unit>_<wf>`），否则用户拿到的是一个"能编过、运行期才炸"的程序。
// 同样地，红线要能证明某些符号**只被引用未定义**。
//
// 返回三态而不是布尔：`Unknown` 与 `Undefined` 的处置不同——前者是"这个文件里
// 没有符号表"（可能合法，如 stripped 产物），后者是"引用了但没定义"（拦不拦是
// 调用方的策略）。

/// 符号状态。与 Go 侧 `objfile.Symbols` 的语义对齐。
#[derive(Clone, Copy, PartialEq, Eq)]
pub enum SymState {
    Missing = 0,
    Defined = 1,
    Undefined = 2,
}

pub fn symbol_state(path: &str, name: &str) -> Result<SymState, ObjErr> {
    let bytes = std::fs::read(path).map_err(|_| ObjErr::Io)?;
    if bytes.len() < 4 {
        return Err(ObjErr::Io);
    }
    let m = [bytes[0], bytes[1], bytes[2], bytes[3]];
    let le = u32::from_le_bytes(m);
    let be = u32::from_be_bytes(m);
    if m == [0x7f, b'E', b'L', b'F'] {
        return elf_symbol_state(&bytes, name);
    }
    if is_macho(le) || is_macho(be) {
        return macho_symbol_state(&bytes, name);
    }
    if is_fat(le) || is_fat(be) {
        return fat_symbol_state(&bytes, name);
    }
    Err(ObjErr::Unsupported)
}

fn elf_symbol_state(b: &[u8], name: &str) -> Result<SymState, ObjErr> {
    if b.len() < 0x40 {
        return Err(ObjErr::Malformed);
    }
    let class = b[4];
    let data = b[5];
    if (class != 1 && class != 2) || (data != 1 && data != 2) {
        return Err(ObjErr::Malformed);
    }
    let is64 = class == 2;
    let be = data == 2;

    let (shoff, shentsize, shnum) = if is64 {
        (
            rd_u64(b, 0x28, be).ok_or(ObjErr::Malformed)?,
            rd_u16(b, 0x3a, be).ok_or(ObjErr::Malformed)? as usize,
            rd_u16(b, 0x3c, be).ok_or(ObjErr::Malformed)? as usize,
        )
    } else {
        (
            rd_u32(b, 0x20, be).ok_or(ObjErr::Malformed)? as u64,
            rd_u16(b, 0x2e, be).ok_or(ObjErr::Malformed)? as usize,
            rd_u16(b, 0x30, be).ok_or(ObjErr::Malformed)? as usize,
        )
    };
    if shentsize == 0 || shnum == 0 {
        return Ok(SymState::Missing);
    }

    let mut state = SymState::Missing;
    for i in 0..shnum {
        let base = shoff as usize + i * shentsize;
        let sh_type = rd_u32(b, base + 4, be).ok_or(ObjErr::Malformed)?;
        // SHT_SYMTAB = 2（不是 DYNSYM：我们要的是完整符号表，含 undefined）
        if sh_type != 2 {
            continue;
        }
        let (off, size, link, entsize) = if is64 {
            (
                rd_u64(b, base + 0x18, be).ok_or(ObjErr::Malformed)?,
                rd_u64(b, base + 0x20, be).ok_or(ObjErr::Malformed)?,
                rd_u32(b, base + 0x28, be).ok_or(ObjErr::Malformed)? as usize,
                rd_u64(b, base + 0x38, be).ok_or(ObjErr::Malformed)? as usize,
            )
        } else {
            (
                rd_u32(b, base + 0x10, be).ok_or(ObjErr::Malformed)? as u64,
                rd_u32(b, base + 0x14, be).ok_or(ObjErr::Malformed)? as u64,
                rd_u32(b, base + 0x18, be).ok_or(ObjErr::Malformed)? as usize,
                rd_u32(b, base + 0x24, be).ok_or(ObjErr::Malformed)? as usize,
            )
        };
        if entsize == 0 {
            continue;
        }
        // 关联的字符串表
        let str_base = shoff as usize + link * shentsize;
        let (str_off, str_size) = if is64 {
            (
                rd_u64(b, str_base + 0x18, be).ok_or(ObjErr::Malformed)?,
                rd_u64(b, str_base + 0x20, be).ok_or(ObjErr::Malformed)?,
            )
        } else {
            (
                rd_u32(b, str_base + 0x10, be).ok_or(ObjErr::Malformed)? as u64,
                rd_u32(b, str_base + 0x14, be).ok_or(ObjErr::Malformed)? as u64,
            )
        };
        let strtab = b
            .get(str_off as usize..(str_off + str_size) as usize)
            .ok_or(ObjErr::Malformed)?;

        let count = (size as usize) / entsize;
        for k in 0..count {
            let e = off as usize + k * entsize;
            let (name_off, shndx) = if is64 {
                (
                    rd_u32(b, e, be).ok_or(ObjErr::Malformed)? as usize,
                    rd_u16(b, e + 6, be).ok_or(ObjErr::Malformed)?,
                )
            } else {
                (
                    rd_u32(b, e, be).ok_or(ObjErr::Malformed)? as usize,
                    rd_u16(b, e + 14, be).ok_or(ObjErr::Malformed)?,
                )
            };
            if name_off >= strtab.len() {
                continue;
            }
            let s = cstr(strtab, name_off).unwrap_or_default();
            if s != name {
                continue;
            }
            state = if shndx == 0 { SymState::Undefined } else { SymState::Defined };
            if state == SymState::Defined {
                return Ok(state);
            }
        }
    }
    Ok(state)
}

fn macho_symbol_state(b: &[u8], name: &str) -> Result<SymState, ObjErr> {
    let be = u32::from_be_bytes([b[0], b[1], b[2], b[3]]) == 0xfeed_facf
        || u32::from_be_bytes([b[0], b[1], b[2], b[3]]) == 0xfeed_face;
    let magic = rd_u32(b, 0, be).ok_or(ObjErr::Malformed)?;
    let is64 = magic == 0xfeed_facf;
    let (ncmds, hdr) = if is64 {
        (rd_u32(b, 0x10, be).ok_or(ObjErr::Malformed)?, 32usize)
    } else {
        (rd_u32(b, 0x0c, be).ok_or(ObjErr::Malformed)?, 28usize)
    };
    let mut off = hdr;
    let mut state = SymState::Missing;
    for _ in 0..ncmds {
        let cmd = rd_u32(b, off, be).ok_or(ObjErr::Malformed)?;
        let size = rd_u32(b, off + 4, be).ok_or(ObjErr::Malformed)? as usize;
        if size < 8 {
            return Err(ObjErr::Malformed);
        }
        if cmd == 0x2 {
            // LC_SYMTAB
            let symoff = rd_u32(b, off + 8, be).ok_or(ObjErr::Malformed)? as usize;
            let nsyms = rd_u32(b, off + 12, be).ok_or(ObjErr::Malformed)? as usize;
            let stroff = rd_u32(b, off + 16, be).ok_or(ObjErr::Malformed)? as usize;
            let strsize = rd_u32(b, off + 20, be).ok_or(ObjErr::Malformed)? as usize;
            let strtab = b
                .get(stroff..stroff + strsize)
                .ok_or(ObjErr::Malformed)?;
            let entsize = if is64 { 16 } else { 12 };
            for k in 0..nsyms {
                let e = symoff + k * entsize;
                let strx = rd_u32(b, e, be).ok_or(ObjErr::Malformed)? as usize;
                let ntype = b.get(e + 4).copied().ok_or(ObjErr::Malformed)?;
                let value = if is64 {
                    rd_u64(b, e + 8, be).ok_or(ObjErr::Malformed)?
                } else {
                    rd_u32(b, e + 8, be).ok_or(ObjErr::Malformed)? as u64
                };
                let s = cstr(strtab, strx).unwrap_or_default();
                // **精确匹配**，不做前导下划线容错：调用方要用带下划线的名字就自己
                // 传（Go 侧有 MachOName 干这事）。容错会让 "main" 在 Mach-O 上
                // 命中 "_main"，与 Go 的 objfile.Read 结论不一致——实测踩过。
                let hit = s == name;
                if !hit {
                    continue;
                }
                let undef = ntype & 0x0e == 0 && value == 0;
                state = if undef { SymState::Undefined } else { SymState::Defined };
                if state == SymState::Defined {
                    return Ok(state);
                }
            }
        }
        off = off.checked_add(size).ok_or(ObjErr::Malformed)?;
        if off > b.len() {
            return Err(ObjErr::Malformed);
        }
    }
    Ok(state)
}

fn fat_symbol_state(b: &[u8], name: &str) -> Result<SymState, ObjErr> {
    let be = u32::from_be_bytes([b[0], b[1], b[2], b[3]]) == 0xcafe_babe
        || u32::from_be_bytes([b[0], b[1], b[2], b[3]]) == 0xcafe_babf;
    let n = rd_u32(b, 4, be).ok_or(ObjErr::Malformed)? as usize;
    if n == 0 {
        return Ok(SymState::Missing);
    }
    let magic = rd_u32(b, 0, be).ok_or(ObjErr::Malformed)?;
    let fat64 = magic == 0xcafe_babf;
    let offset = if fat64 {
        rd_u64(b, 8 + 8, be).ok_or(ObjErr::Malformed)?
    } else {
        rd_u32(b, 8 + 8, be).ok_or(ObjErr::Malformed)? as u64
    };
    let slice = b.get(offset as usize..).ok_or(ObjErr::Malformed)?;
    macho_symbol_state(slice, name)
}
