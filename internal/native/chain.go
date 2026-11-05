package native

/*
#include "hermes_abi.h"
*/
import "C"

import "fmt"

// 哈希链（R-TR-04）。
//
// 分工是刻意的：**Go 负责 canonical 字节序化，Rust 负责算哈希且无状态**。
// 如果两边都算 SHA-256，两份实现必然漂移；如果 Rust 持有链状态，它就得
// 管理生命周期（又回到 FFI 内存地狱）。无状态纯函数是这条边界上最省心的形态。
//
// 字节规则（唯一真相是 native/rust/src/chain.rs 的模块文档 + docs/CANONICAL.md）：
//
//	block_hash(prev, index, block) =
//	    SHA-256(DOMAIN ‖ 0x00 ‖ prev[32] ‖ u64le(index) ‖ u64le(len) ‖ block)
//	final_hash(prev, count, total_len) =
//	    SHA-256(DOMAIN ‖ 0x00 ‖ prev[32] ‖ u64le(count) ‖ u64le(total_len) ‖ FINAL_TAG)
//
// DOMAIN 是域分隔串，参与哈希 —— 改它，schemas/golden/chain-*.json 立刻对不上。
// 这不是形式主义：没有域分隔，一条 trace 链的中间块就能被另一条链的块替换。

// ChainDomain 是参与哈希的域分隔串。
const ChainDomain = "hermes.trace.v1"

// ChainFinalTag 是终块哈希的尾标记。
const ChainFinalTag = "hermes.trace.final"

// Digest 是 32 字节哈希。
type Digest [32]byte

// Genesis 返回创世块的 prev（32 个零字节）。
func Genesis() Digest { return Digest{} }

// ChainHash 计算第 index 块的哈希。
//
// block 超过 ChainBlockMax 返回 ErrTooLargeErr —— **不截断**。
// 截断会让"链对不上"这个信号消失，而那是 trace 防篡改的全部意义。
func ChainHash(prev Digest, index uint64, block []byte) (Digest, error) {
	var out Digest
	if len(block) > ChainBlockMax {
		return out, fmt.Errorf("%w: block %d bytes > %d",
			ErrTooLargeErr, len(block), ChainBlockMax)
	}
	p, n := byteSlice(block)
	if err := toError(C.hm_chain_hash((*C.uint8_t)(&prev[0]), C.uint64_t(index),
		p, n, (*C.uint8_t)(&out[0]))); err != nil {
		return Digest{}, err
	}
	return out, nil
}

// ChainFinal 计算终块哈希，把块数与总字节数一起封进去。
//
// 封"块数"和"总量"是为了让尾部截断可检测：只封最后一个块的话，删掉末尾几块
// 仍然能通过校验。
func ChainFinal(prev Digest, count, totalLen uint64) (Digest, error) {
	var out Digest
	if err := toError(C.hm_chain_final((*C.uint8_t)(&prev[0]),
		C.uint64_t(count), C.uint64_t(totalLen), (*C.uint8_t)(&out[0]))); err != nil {
		return Digest{}, err
	}
	return out, nil
}
