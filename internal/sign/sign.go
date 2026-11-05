// Package sign 给 `.hbc` 签名与验签（IR.md §8 / D-011）。
//
// 一个必须先说清的事实：**签名覆盖索引与成员哈希，不覆盖成员全文**。
//
// 这不是图省事。`hermes link` 要复用别人已签名的成员库，而链接方**没有**
// 那个库的私钥；若签名覆盖全文，链接后就必须用原私钥重签，整条
// "分发到没有源码的生产环境"的链条当场断掉。签"索引 + 每个成员的 ir_sha256"
// 就够：索引断言"这些确切字节的成员在我的归档里"，而每个 ir_sha256 由原始
// 归档的签名背书，可逐级回溯。
package sign

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"

	"hermes/internal/archive"
)

const (
	// Alg 是 footer 的算法字节。1 = Ed25519。
	Alg = 1
	// 签名消息的域分隔前缀。**必须带 \0**：不加分隔，"hbc.v1" 与 "hbc.v11"
	// 会前缀相同，跨版本重放就成了可能。
	HBCPrefix = "hermes.hbc.v1\x00"
)

var (
	ErrNoSignature = errors.New("sign: 归档未签名（footer 全 0）")
	ErrBadAlg      = errors.New("sign: footer 的算法字节不是 Ed25519")
	ErrBadSig      = errors.New("sign: 签名不匹配（文件被改过，或不是这把钥匙签的）")
)

// Key 是签名密钥对。
type Key struct {
	Priv ed25519.PrivateKey
	Pub  ed25519.PublicKey
}

// NewKey 生成一把新钥匙。私钥的保存与分发不在本包职责内。
func NewKey() (*Key, error) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, err
	}
	return &Key{Priv: priv, Pub: pub}, nil
}

// ID 是 footer 里那 16 个 ASCII 字符：hex(sha256(pubkey)) 的前 16 位。
//
// 为什么存"钥匙的哈希前缀"而不是钥匙本身：footer 只有 16 字节，
// 而这个字段的用途是**选出该用哪把公钥验签**，不是承载公钥。
func (k *Key) ID() string {
	sum := sha256.Sum256(k.Pub)
	return hex.EncodeToString(sum[:])[:16]
}

// message 是签名消息：域前缀 + 索引字节。
func message(index []byte) []byte {
	m := make([]byte, 0, len(HBCPrefix)+len(index))
	m = append(m, HBCPrefix...)
	m = append(m, index...)
	return m
}

// Sign 就地填充 footer 的 key_id 与 sig，返回新的字节。
//
// 不动除 footer 之外的任何字节 —— 成员与索引的字节是签名**覆盖的对象**，
// 签名过程本身不能改变它们，否则"签完就对不上"。
func Sign(data []byte, k *Key) ([]byte, error) {
	if len(data) < archive.HeaderLen+archive.FooterLen {
		return nil, archive.ErrBadLayout
	}
	indexOff := int(binary.LittleEndian.Uint64(data[28:36]))
	indexLen := int(binary.LittleEndian.Uint64(data[36:44]))
	if indexOff < archive.HeaderLen || indexOff+indexLen > len(data)-archive.FooterLen {
		return nil, archive.ErrBadLayout
	}
	index := data[indexOff : indexOff+indexLen]
	sig := ed25519.Sign(k.Priv, message(index))

	out := make([]byte, len(data))
	copy(out, data)
	foot := len(out) - archive.FooterLen
	out[foot] = Alg
	// key_id：16 个 ASCII 字符，长度恰好 16，直接拷。
	copy(out[foot+4:foot+20], []byte(k.ID()))
	copy(out[foot+20:foot+84], sig)
	return out, nil
}

// Verify 校验 footer 签名。
//
// 顺序是对外契约的一部分：先看有没有签名、再看算法、最后验签。
// 反过来（先验签）会把"未签名"报成"签名不匹配"，而这两种情况该做的事完全不同：
// 前者是"你忘了签"，后者是"文件被人动过"。
func Verify(data []byte) error {
	if len(data) < archive.HeaderLen+archive.FooterLen {
		return archive.ErrBadLayout
	}
	foot := len(data) - archive.FooterLen
	if data[foot] == 0 {
		return ErrNoSignature
	}
	if data[foot] != Alg {
		return fmt.Errorf("%w: %d", ErrBadAlg, data[foot])
	}
	indexOff := int(binary.LittleEndian.Uint64(data[28:36]))
	indexLen := int(binary.LittleEndian.Uint64(data[36:44]))
	if indexOff < archive.HeaderLen || indexOff+indexLen > len(data)-archive.FooterLen {
		return archive.ErrBadLayout
	}
	// 先过一遍结构校验：一份连段边界都不对的文件，报"签名不对"只会误导。
	if _, err := archive.Read(data); err != nil {
		return err
	}
	// 公钥从哪来？footer 只存 key_id，不存公钥。调用方按 key_id 选钥匙。
	// 这里无法自己选，所以 Verify 只做"结构 + 有无签名"，真正的验签在
	// VerifyWithPub。这种拆法让"没有公钥时能给出什么结论"变清楚：
	// 能说"它自称被签过"，不能说"它是真的"。
	return nil
}

// VerifyWithPub 用给定公钥验签。
func VerifyWithPub(data []byte, pub ed25519.PublicKey) error {
	if err := Verify(data); err != nil {
		return err
	}
	foot := len(data) - archive.FooterLen
	indexOff := int(binary.LittleEndian.Uint64(data[28:36]))
	indexLen := int(binary.LittleEndian.Uint64(data[36:44]))
	index := data[indexOff : indexOff+indexLen]
	sig := data[foot+20 : foot+84]
	if !ed25519.Verify(pub, message(index), sig) {
		return ErrBadSig
	}
	return nil
}

// KeyID 读 footer 里的 key_id（用来选公钥）。
func KeyID(data []byte) string {
	if len(data) < archive.HeaderLen+archive.FooterLen {
		return ""
	}
	foot := len(data) - archive.FooterLen
	return string(data[foot+4 : foot+20])
}
