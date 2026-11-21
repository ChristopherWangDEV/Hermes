package sign

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"hermes/internal/archive"
	"hermes/internal/canon"
)

// `sealed.hermes` 是**第二个产物**（IR.md §8.3）。
//
// 为什么必须有两个产物：`.hbc` 签的是索引与成员哈希，它不含源码，
// 所以无法证明"这份 IR 来自那份源码"；`sealed` 里嵌了源码与 IR 原文，
// 能证明同源，但**不含 `.hbc` 全文**，所以无法证明"分发出去的那个文件没被换过"。
// 单靠任一个都有一半的链断了 —— 这就是 D-011 说"缺一不可"的形式化理由。

const (
	// IR.md §8.3 写的是 `magic "SHRM\x1a" (4)` —— **自相矛盾**：
	// "SHRM\x1a" 是 5 字节，而布局只给 magic 4 字节（版本从偏移 4 开始）。
	// 按布局取 4 字节 "SHRM"。代价：少了 HBC 那个 \x1a 的"文本模式会暴露"性质。
	// 记在 B-6，等规格统一（要么改成 "SHR\x1a"，要么把 magic 扩到 5 字节）。
	SealMagic   = "SHRM"
	SealVersion = 1
	SealHeader  = 64
	// SealPrefix 是 sealed 的签名消息前缀（同样带 \0）。
	SealPrefix = "hermes.sealed.v1\x00"
)

var (
	ErrSealMagic   = errors.New("seal: bad magic (not sealed.hermes)")
	ErrSealVersion = errors.New("seal: unsupported seal_version")
	ErrSealLayout  = errors.New("seal: invalid segment boundaries")
	ErrSealCross   = errors.New("seal: artifacts come from different sources (hash mismatch)")
)

// SealInput 是封签需要的全部事实。
type SealInput struct {
	SourcePath string
	Source     []byte
	IR         []byte
	// HBC 是**同一份**归档的完整字节。它的 sha256 进 manifest，
	// 从而让"sealed 指向的是哪个 .hbc"变成不可伪造的事实。
	HBC []byte
	// IndexSHA 是 .hbc 头部那 32 字节的十六进制。
	IndexSHA string
	// EntryWorkflows / Bounds / Economics 进 manifest 供审计。
	EntryWorkflows []string
	Bounds         map[string]int64
	Economics      map[string]any
}

// Seal 生成 `.sealed.hermes` 字节（未签名）。
func Seal(in SealInput, k *Key) ([]byte, error) {
	if len(in.HBC) < archive.HeaderLen+archive.FooterLen {
		return nil, archive.ErrBadLayout
	}
	srcSum := sha256.Sum256(in.Source)
	irSum := sha256.Sum256(in.IR)
	hbcSum := sha256.Sum256(in.HBC)

	bounds := make([]any, 0, len(in.Bounds)*2)
	for _, key := range sortedKeys(in.Bounds) {
		bounds = append(bounds, key, canon.Int(in.Bounds[key]))
	}
	eco := canon.Null()
	if len(in.Economics) > 0 {
		v, err := canon.FromAny(in.Economics)
		if err != nil {
			return nil, fmt.Errorf("seal: economics contains floating point or invalid values: %w", err)
		}
		eco = v
	}

	manifest := canon.Obj(
		"seal_version", canon.Int(SealVersion),
		"source_path", canon.Str(in.SourcePath),
		"source_sha256", canon.Str(hex.EncodeToString(srcSum[:])),
		"source_bytes", canon.Int(int64(len(in.Source))),
		"ir_sha256", canon.Str(hex.EncodeToString(irSum[:])),
		"ir_bytes", canon.Int(int64(len(in.IR))),
		"hbc_sha256", canon.Str(hex.EncodeToString(hbcSum[:])),
		"hbc_bytes", canon.Int(int64(len(in.HBC))),
		"index_sha256", canon.Str(in.IndexSHA),
		"entry_workflows", strArr(in.EntryWorkflows),
		"bounds", canon.Obj(bounds...),
		"economics", eco,
		"key_id", canon.Str(k.ID()),
	).Bytes()

	srcOff := SealHeader
	irOff := srcOff + len(in.Source)
	manOff := irOff + len(in.IR)
	fileLen := manOff + len(manifest) + archive.FooterLen

	out := make([]byte, fileLen)
	copy(out[0:4], SealMagic)
	binary.LittleEndian.PutUint16(out[4:6], SealVersion)
	binary.LittleEndian.PutUint16(out[6:8], 0)
	binary.LittleEndian.PutUint64(out[8:16], uint64(srcOff))
	binary.LittleEndian.PutUint64(out[16:24], uint64(len(in.Source)))
	binary.LittleEndian.PutUint64(out[24:32], uint64(irOff))
	binary.LittleEndian.PutUint64(out[32:40], uint64(len(in.IR)))
	binary.LittleEndian.PutUint64(out[40:48], uint64(manOff))
	binary.LittleEndian.PutUint64(out[48:56], uint64(len(manifest)))
	// 56..64 reserved 保持 0

	copy(out[srcOff:], in.Source)
	copy(out[irOff:], in.IR)
	copy(out[manOff:], manifest)
	copy(out[manOff:], manifest)

	// 签名：前缀 + manifest + source。
	// **为什么把 source 也签进去**：manifest 只含源码的哈希；不签源码本身，
	// 攻击者可以改源码并同步改 manifest —— 而 manifest 的哈希是自洽的。
	msg := append([]byte(SealPrefix), manifest...)
	msg = append(msg, in.Source...)
	sig := ed25519.Sign(k.Priv, msg)

	foot := fileLen - archive.FooterLen
	out[foot] = Alg
	copy(out[foot+4:foot+20], []byte(k.ID()))
	copy(out[foot+20:foot+84], sig)
	return out, nil
}

// Sealed 是读出来的封签产物。
type Sealed struct {
	Source   []byte
	IR       []byte
	Manifest map[string]any
	Raw      map[string]any
}

// ReadSealed 解析并做**结构**校验（不含密码学）。
func ReadSealed(data []byte) (*Sealed, map[string]any, error) {
	if len(data) < SealHeader+archive.FooterLen {
		return nil, nil, ErrSealLayout
	}
	if string(data[0:4]) != SealMagic {
		return nil, nil, ErrSealMagic
	}
	if v := binary.LittleEndian.Uint16(data[4:6]); v != SealVersion {
		return nil, nil, fmt.Errorf("%w: %d", ErrSealVersion, v)
	}
	srcOff := int(binary.LittleEndian.Uint64(data[8:16]))
	srcLen := int(binary.LittleEndian.Uint64(data[16:24]))
	irOff := int(binary.LittleEndian.Uint64(data[24:32]))
	irLen := int(binary.LittleEndian.Uint64(data[32:40]))
	manOff := int(binary.LittleEndian.Uint64(data[40:48]))
	manLen := int(binary.LittleEndian.Uint64(data[48:56]))

	if srcOff < SealHeader || irOff < srcOff+srcLen || manOff < irOff+irLen ||
		manOff+manLen > len(data)-archive.FooterLen {
		return nil, nil, ErrSealLayout
	}

	man, err := decodeCanon(data[manOff : manOff+manLen])
	if err != nil {
		return nil, nil, fmt.Errorf("%w: manifest is not valid JSON: %v", ErrSealLayout, err)
	}
	s := &Sealed{
		Source:   data[srcOff : srcOff+srcLen],
		IR:       data[irOff : irOff+irLen],
		Manifest: man,
		Raw:      map[string]any{"manifest_bytes": data[manOff : manOff+manLen]},
	}
	return s, man, nil
}

// VerifySealed 是 §8.5 的五步交叉校验。任一步失败即整体失败。
func VerifySealed(sealed, hbc []byte) error {
	s, man, err := ReadSealed(sealed)
	if err != nil {
		return err
	}

	// 2. 签名存在性。真正的验签在 VerifySealedWithPub —— footer 只存 key_id，
	// 没有公钥时**不能假装验过**：那会让"未签名"和"签名有效"看起来一样。
	foot := len(sealed) - archive.FooterLen
	if sealed[foot] == 0 {
		return ErrNoSignature
	}
	if sealed[foot] != Alg {
		return fmt.Errorf("%w: %d", ErrBadAlg, sealed[foot])
	}

	// 3. 交叉：manifest.index_sha256 == .hbc 头部 44..76
	hbcIndex := hex.EncodeToString(hbc[44:76])
	if got, _ := man["index_sha256"].(string); got != hbcIndex {
		return fmt.Errorf("%w: manifest.index_sha256=%s, but the .hbc header is %s", ErrSealCross, shortSHA(got), shortSHA(hbcIndex))
	}

	// 4. 交叉：manifest.ir_sha256 == sha256(sealed 内 ir) == sha256(.hbc 成员 0)
	irSum := sha256.Sum256(s.IR)
	wantIR, _ := man["ir_sha256"].(string)
	if hex.EncodeToString(irSum[:]) != wantIR {
		return fmt.Errorf("%w: IR inside sealed does not match manifest ir_sha256", ErrSealCross)
	}
	a, err := archive.Read(hbc)
	if err != nil {
		return err
	}
	if len(a.Members) == 0 {
		return fmt.Errorf("%w: .hbc has no members", ErrSealCross)
	}
	m0 := sha256.Sum256(a.Members[0].IR)
	if hex.EncodeToString(m0[:]) != wantIR {
		return fmt.Errorf("%w: IR of .hbc member 0 does not match manifest ir_sha256", ErrSealCross)
	}

	// 5. 交叉：manifest.hbc_sha256 == sha256(.hbc 全文件)；source_sha256 同理。
	hbcSum := sha256.Sum256(hbc)
	if got, _ := man["hbc_sha256"].(string); got != hex.EncodeToString(hbcSum[:]) {
		return fmt.Errorf("%w: whole-file hash of .hbc does not match manifest", ErrSealCross)
	}
	srcSum := sha256.Sum256(s.Source)
	if got, _ := man["source_sha256"].(string); got != hex.EncodeToString(srcSum[:]) {
		return fmt.Errorf("%w: source inside sealed does not match manifest source_sha256", ErrSealCross)
	}
	return nil
}

// VerifySealedWithPub 验签 + 五步交叉校验。
func VerifySealedWithPub(sealed, hbc []byte, pub ed25519.PublicKey) error {
	if err := VerifySealed(sealed, hbc); err != nil {
		return err
	}
	s, _, err := ReadSealed(sealed)
	if err != nil {
		return err
	}
	foot := len(sealed) - archive.FooterLen
	manBytes, _ := s.Raw["manifest_bytes"].([]byte)
	msg := append(append([]byte(SealPrefix), manBytes...), s.Source...)
	sig := sealed[foot+20 : foot+84]
	if !ed25519.Verify(pub, msg, sig) {
		return ErrBadSig
	}
	return nil
}

// shortSHA 截断显示；**不能直接切片**，短输入会 panic，
// 而报错路径上的 panic 会盖掉它本来要报的那条诊断。
func shortSHA(s string) string {
	if len(s) <= 16 {
		return s
	}
	return s[:16]
}

func strArr(xs []string) canon.Value {
	vs := make([]canon.Value, 0, len(xs))
	for _, s := range xs {
		vs = append(vs, canon.Str(s))
	}
	return canon.Arr(vs...)
}

func sortedKeys(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

func sortStrings(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}

// decodeCanon 用 UseNumber 解 JSON：整数保持整数，不被悄悄变成 float64。
func decodeCanon(b []byte) (map[string]any, error) {
	var v map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// MemberIR 取归档里第 i 个成员的 IR 字节（供 verify 打印与比对）。
func MemberIR(a *archive.Archive, i int) ([]byte, error) {
	if i < 0 || i >= len(a.Members) {
		return nil, fmt.Errorf("seal: member index %d out of range (%d total)", i, len(a.Members))
	}
	return a.Members[i].IR, nil
}
