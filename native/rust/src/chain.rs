//! trace 哈希链的**唯一**实现点（R-TR-04 / 不变量 INV-3）。
//!
//! 职责划分（ARCH §5.3，别自己另发明）：
//!   * canonical 字节序化 → 只在 Go（`internal/trace/canon.go`）。Rust 看不到
//!     `Trace` 结构体，也不该看到。
//!   * SHA-256 与链推进 → 只在这里，且**无状态**：`(prev, index, block) → out`。
//!     无状态让第 k 块的验证可以脱离"从头跑"单独进行。
//!
//! 字节级定义（docs/CANONICAL.md 与 Go 侧 `internal/native/chain.go` 必须逐字一致）：
//!
//! ```text
//! block_hash(prev, index, block) =
//!     SHA-256( "hermes.trace.v1"            // 域分隔串，15 字节，无长度前缀
//!            ‖ 0x00                          // 分隔符，防止 "domain+prev" 的边界混淆
//!            ‖ prev[32]                      // 上一块哈希；创世块为 32 个 0
//!            ‖ u64_le(index)                 // 块序号
//!            ‖ u64_le(len(block))            // 块长度
//!            ‖ block )
//!
//! final_hash(prev, count, total_len) =
//!     SHA-256( "hermes.trace.v1"
//!            ‖ 0x00
//!            ‖ prev[32]
//!            ‖ u64_le(count)                 // 块数
//!            ‖ u64_le(total_len)             // 总字节数
//!            ‖ "hermes.trace.final" )        // 尾标记，防止把 final 当成第 count+1 块
//! ```
//!
//! 为什么域分隔串必须参与哈希：没有它，一个 trace 的块可以搬到另一个格式/
//! 另一个产品里当合法块用（跨版本重放），而且 "domain ‖ prev" 的边界会
//! 因为两个字段都可变长而产生长度混淆攻击。golden vector 的第 4 条验收
//! （把域分隔串改掉 → chain-002 立刻红）就是这条的机器证明。

use sha2::{Digest, Sha256};

pub const DOMAIN: &[u8] = b"hermes.trace.v1";
pub const FINAL_TAG: &[u8] = b"hermes.trace.final";
pub const BLOCK_MAX: usize = 1 << 20; // 1 MiB，与 HM_CHAIN_BLOCK_MAX 一致

/// 创世块的前驱哈希。
pub const GENESIS_PREV: [u8; 32] = [0u8; 32];

fn dispatch(domain: &[u8], prev: &[u8; 32], index: u64, len: u64, tail: &[u8]) -> [u8; 32] {
    let mut h = Sha256::new();
    h.update(domain);
    h.update([0u8]);
    h.update(prev);
    h.update(index.to_le_bytes());
    h.update(len.to_le_bytes());
    h.update(tail);
    let d = h.finalize();
    let mut out = [0u8; 32];
    out.copy_from_slice(&d);
    out
}

/// 计算一块的哈希。`block` 长度必须 ≤ `BLOCK_MAX`（调用方已校验）。
pub fn block_hash(prev: &[u8; 32], index: u64, block: &[u8]) -> [u8; 32] {
    dispatch(DOMAIN, prev, index, block.len() as u64, block)
}

/// 计算链尾（root_hash）。
pub fn final_hash(prev: &[u8; 32], count: u64, total_len: u64) -> [u8; 32] {
    dispatch(DOMAIN, prev, count, total_len, FINAL_TAG)
}

/// 测试与文档用：用任意域分隔串计算 block hash。
/// **只在测试里用**，生产路径只走 `block_hash`。
#[cfg(test)]
pub fn block_hash_with_domain(domain: &[u8], prev: &[u8; 32], index: u64, block: &[u8]) -> [u8; 32] {
    dispatch(domain, prev, index, block.len() as u64, block)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::BTreeMap;

    // ---------------------------------------------------------------- //
    // 极小的 JSON 解析器：golden 文件是本仓自己生成的固定形状，
    // 为一个测试 fixture 引 serde_json 会把依赖表从 2 个 crate 变成 7 个
    // （serde / serde_json / itoa / ryu / memchr / ...），违反 A 组白名单。
    // ---------------------------------------------------------------- //
    mod json {
        use std::collections::BTreeMap;

        pub enum Val {
            Str(String),
            ArrStr(Vec<String>),
        }

        struct P<'a> {
            b: &'a [u8],
            i: usize,
        }

        impl<'a> P<'a> {
            fn ws(&mut self) {
                while self.i < self.b.len() && (self.b[self.i] as char).is_ascii_whitespace() {
                    self.i += 1;
                }
            }

            fn peek(&mut self) -> u8 {
                self.ws();
                if self.i < self.b.len() {
                    self.b[self.i]
                } else {
                    0
                }
            }

            fn eat(&mut self, c: u8) -> bool {
                if self.peek() == c {
                    self.i += 1;
                    true
                } else {
                    false
                }
            }

            fn string(&mut self) -> Option<String> {
                if !self.eat(b'"') {
                    return None;
                }
                let mut out = String::new();
                while self.i < self.b.len() {
                    let c = self.b[self.i];
                    self.i += 1;
                    match c {
                        b'"' => return Some(out),
                        b'\\' => {
                            if self.i >= self.b.len() {
                                return None;
                            }
                            let e = self.b[self.i];
                            self.i += 1;
                            match e {
                                b'"' => out.push('"'),
                                b'\\' => out.push('\\'),
                                b'/' => out.push('/'),
                                b'n' => out.push('\n'),
                                b't' => out.push('\t'),
                                b'r' => out.push('\r'),
                                b'b' => out.push('\u{8}'),
                                b'f' => out.push('\u{c}'),
                                b'u' => {
                                    if self.i + 4 > self.b.len() {
                                        return None;
                                    }
                                    let hex = std::str::from_utf8(
                                        &self.b[self.i..self.i + 4],
                                    )
                                    .ok()?;
                                    let v = u32::from_str_radix(hex, 16).ok()?;
                                    self.i += 4;
                                    out.push(char::from_u32(v)?);
                                }
                                _ => return None,
                            }
                        }
                        _ => out.push(c as char),
                    }
                }
                None
            }

            fn skip_value(&mut self) {
                let c = self.peek();
                match c {
                    b'"' => {
                        let _ = self.string();
                    }
                    b'[' | b'{' => {
                        let open = c;
                        let close = if open == b'[' { b']' } else { b'}' };
                        self.i += 1;
                        let mut depth = 1;
                        while self.i < self.b.len() && depth > 0 {
                            let ch = self.b[self.i];
                            if ch == b'"' {
                                let _ = self.string();
                                continue;
                            }
                            if ch == open {
                                depth += 1;
                            } else if ch == close {
                                depth -= 1;
                            }
                            self.i += 1;
                        }
                    }
                    _ => {
                        while self.i < self.b.len() {
                            let ch = self.b[self.i];
                            if ch == b',' || ch == b'}' || ch == b']' {
                                break;
                            }
                            self.i += 1;
                        }
                    }
                }
            }
        }

        pub fn parse(src: &str) -> BTreeMap<String, Val> {
            let mut p = P {
                b: src.as_bytes(),
                i: 0,
            };
            let mut out = BTreeMap::new();
            if !p.eat(b'{') {
                return out;
            }
            loop {
                if p.peek() == b'}' {
                    p.i += 1;
                    break;
                }
                let key = match p.string() {
                    Some(k) => k,
                    None => break,
                };
                if !p.eat(b':') {
                    break;
                }
                match p.peek() {
                    b'[' => {
                        p.i += 1;
                        let mut arr: Vec<String> = Vec::new();
                        loop {
                            if p.peek() == b']' {
                                p.i += 1;
                                break;
                            }
                            match p.string() {
                                Some(s) => arr.push(s),
                                None => {
                                    p.skip_value();
                                }
                            }
                            if p.eat(b',') {
                                continue;
                            }
                            if p.eat(b']') {
                                break;
                            }
                            break;
                        }
                        out.insert(key, Val::ArrStr(arr));
                    }
                    b'"' => {
                        if let Some(s) = p.string() {
                            out.insert(key, Val::Str(s));
                        }
                    }
                    _ => {
                        p.skip_value();
                    }
                }
                if p.eat(b',') {
                    continue;
                }
                if p.eat(b'}') {
                    break;
                }
                break;
            }
            out
        }
    }

    fn unhex(s: &str) -> Vec<u8> {
        assert_eq!(s.len() % 2, 0, "hex 长度必须是偶数");
        let mut out = Vec::with_capacity(s.len() / 2);
        let b = s.as_bytes();
        let mut i = 0;
        while i < b.len() {
            let hi = (b[i] as char).to_digit(16).expect("hex");
            let lo = (b[i + 1] as char).to_digit(16).expect("hex");
            out.push((hi * 16 + lo) as u8);
            i += 2;
        }
        out
    }

    fn hex(bytes: &[u8]) -> String {
        let mut s = String::with_capacity(bytes.len() * 2);
        for b in bytes {
            s.push_str(&format!("{:02x}", b));
        }
        s
    }

    fn as32(v: &[u8]) -> [u8; 32] {
        assert_eq!(v.len(), 32, "哈希必须是 32 字节");
        let mut o = [0u8; 32];
        o.copy_from_slice(v);
        o
    }

    fn run_vector(name: &str, src: &str) {
        let obj = json::parse(src);

        let domain = match obj.get("domain") {
            Some(json::Val::Str(s)) => s.clone(),
            _ => panic!("{}: missing domain", name),
        };
        assert_eq!(
            domain.as_bytes(),
            DOMAIN,
            "{}: golden 的域分隔串与实现不一致 —— 这正是不变量 INV-3 该报警的时刻",
            name
        );

        let blocks: Vec<Vec<u8>> = match obj.get("blocks") {
            Some(json::Val::ArrStr(v)) => v.iter().map(|h| unhex(h)).collect(),
            _ => panic!("{}: missing blocks", name),
        };
        let expected: Vec<Vec<u8>> = match obj.get("expected") {
            Some(json::Val::ArrStr(v)) => v.iter().map(|h| unhex(h)).collect(),
            _ => panic!("{}: missing expected", name),
        };
        let final_hex = match obj.get("final") {
            Some(json::Val::Str(s)) => s.clone(),
            _ => panic!("{}: missing final", name),
        };

        assert_eq!(blocks.len(), expected.len(), "{}: blocks/expected 长度不等", name);

        let mut prev = GENESIS_PREV;
        let mut total: u64 = 0;
        for (i, block) in blocks.iter().enumerate() {
            let got = block_hash(&prev, i as u64, block);
            assert_eq!(
                hex(&got),
                hex(&expected[i]),
                "{}: 第 {} 块的哈希与 golden 不符",
                name,
                i
            );
            prev = got;
            total += block.len() as u64;
        }
        let fin = final_hash(&prev, blocks.len() as u64, total);
        assert_eq!(hex(&fin), final_hex, "{}: final 与 golden 不符", name);
    }

    #[test]
    fn golden_chain_001_single_empty_block() {
        run_vector(
            "chain-001",
            include_str!("../../../schemas/golden/chain-001.json"),
        );
    }

    #[test]
    fn golden_chain_002_multiblock_with_escapes() {
        run_vector(
            "chain-002",
            include_str!("../../../schemas/golden/chain-002.json"),
        );
    }

    #[test]
    fn golden_chain_003_across_1mib_boundary() {
        run_vector(
            "chain-003",
            include_str!("../../../schemas/golden/chain-003.json"),
        );
    }

    #[test]
    fn changing_one_byte_changes_the_hash() {
        let prev = GENESIS_PREV;
        let a = block_hash(&prev, 0, b"hello");
        let b = block_hash(&prev, 0, b"hellp");
        assert_ne!(a, b);
    }

    #[test]
    fn changing_index_changes_the_hash() {
        let prev = GENESIS_PREV;
        assert_ne!(
            block_hash(&prev, 0, b"x"),
            block_hash(&prev, 1, b"x"),
            "索引必须参与哈希，否则块可以被换序"
        );
    }

    #[test]
    fn domain_separation_is_real() {
        // T04 验收④ 的本地证明：改域分隔串 → 同一个 (prev, index, block)
        // 得到完全不同的哈希。golden 文件用固定 domain 生成，因此实现里
        // 一旦改了 DOMAIN，三组 golden 全红。
        let prev = GENESIS_PREV;
        let a = block_hash_with_domain(b"hermes.trace.v1", &prev, 0, b"block");
        let b = block_hash_with_domain(b"hermes.trace.v2", &prev, 0, b"block");
        assert_ne!(a, b);
    }

    #[test]
    fn final_differs_from_any_block_hash() {
        let prev = GENESIS_PREV;
        let f = final_hash(&prev, 0, 0);
        let b = block_hash(&prev, 0, b"");
        assert_ne!(f, b, "final 必须与空块哈希不同（尾标记生效）");
    }

    #[test]
    fn block_at_size_limit_is_fine() {
        let prev = GENESIS_PREV;
        let block = vec![0xab; BLOCK_MAX];
        let h = block_hash(&prev, 0, &block);
        assert_ne!(h, [0u8; 32]);
    }

    #[test]
    fn genesis_prev_is_32_zero_bytes() {
        assert_eq!(GENESIS_PREV, [0u8; 32]);
        assert_eq!(as32(&GENESIS_PREV).len(), 32);
    }
}
