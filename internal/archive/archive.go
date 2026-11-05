// Package archive 读写 `.hbc` 归档（IR.md §7 / D-013）。
//
// 一句话：**签的是索引与成员哈希，不是成员全文**。这不是加密上的偏好，而是
// `hermes link` 的硬需求 —— 链接方没有成员库的私钥；若签名覆盖全文，
// 链接后就必须用原私钥重签，整条"分发到没有源码的生产环境"的链条会断。
package archive

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"hermes/internal/canon"
)

const (
	// Magic 带 \x1a 是刻意的：文本模式传输时它会暴露出来。
	Magic      = "HBC\x1a"
	Version    = 1
	HeaderLen  = 128
	FooterLen  = 84
	MaxFile    = 512 << 20
	MaxMembers = 4096
	align      = 8
)

// Member 是一个待写入的成员。
type Member struct {
	Name string
	IR   []byte
	// Exports 进索引的符号表：链接器只读它，不读成员内部（解耦）。
	Exports  Exports
	Requires []Require
	// Entry 为 true 表示它的 workflow 是 is_entry。
	Entry           bool
	LanguageVersion int
	ABIVersion      int
}

type Exports struct {
	Workflows []string `json:"workflows"`
	Types     []string `json:"types"`
	Backends  []string `json:"backends"`
	Tools     []string `json:"tools"`
}

type Require struct {
	Spec         string `json:"spec"`
	Kind         string `json:"kind"`
	ABIVersion   int    `json:"abi_version"`
	IRVersion    int    `json:"ir_version"`
	Origin       string `json:"origin"`
	MemberSHA256 string `json:"member_sha256"`
	Implicit     bool   `json:"implicit,omitempty"`
}

// Archive 是读出来的归档。
type Archive struct {
	Flags   uint16
	Members []ReadMember
	Index   []byte
}

type ReadMember struct {
	Name  string
	IR    []byte
	Index map[string]any
}

var (
	ErrBadMagic   = errors.New("hbc: magic 不对（不是 .hbc 文件）")
	ErrBadVersion = errors.New("hbc: archive_version 不受支持")
	ErrBadLayout  = errors.New("hbc: 段边界非法（越界/重叠/未对齐）")
	ErrReserved   = errors.New("hbc: reserved 字节非 0")
	ErrIndexSHA   = errors.New("hbc: index_sha256 与索引实际哈希不符（文件被改过）")
	ErrTooLarge   = errors.New("hbc: 超过大小上限")
	ErrTooMany    = errors.New("hbc: 成员数超上限")
	ErrMemberHash = errors.New("hbc: 成员 IR 的哈希与索引不符")
	ErrNoMembers  = errors.New("hbc: 归档至少要有一个成员")
)

type placement struct {
	off int
	n   int
	sum string
}

// Write 生成 `.hbc` 字节。成员按**名字典序**排列（§9.4）：
// 链接顺序不该取决于调用方给的顺序，否则"同一批库、不同参数顺序"会产出不同字节。
func Write(members []Member) ([]byte, error) {
	if len(members) > MaxMembers {
		return nil, ErrTooMany
	}
	// 空归档是**能构造出来的**（0 个成员），且它的结构、哈希、签名全都合法。
	// 正因为"什么都验得通"，它必须被显式拒绝：交出一个空产物比交出大产物危险，
	// 而症状要到运行期"找不到任何 workflow"才出现。
	if len(members) == 0 {
		return nil, ErrNoMembers
	}
	sorted := make([]Member, len(members))
	copy(sorted, members)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	places := make([]placement, len(sorted))
	off := HeaderLen
	for i := range sorted {
		h := sha256.Sum256(sorted[i].IR)
		places[i] = placement{off: off, n: len(sorted[i].IR), sum: hex.EncodeToString(h[:])}
		off = alignUp(off + len(sorted[i].IR))
	}

	indexOff := off
	index := buildIndex(sorted, places)
	fileLen := indexOff + len(index) + FooterLen
	if fileLen > MaxFile {
		return nil, ErrTooLarge
	}

	out := make([]byte, fileLen)
	copy(out[0:4], Magic)
	binary.LittleEndian.PutUint16(out[4:6], Version)
	binary.LittleEndian.PutUint16(out[6:8], 0)
	binary.LittleEndian.PutUint32(out[8:12], uint32(len(sorted)))
	binary.LittleEndian.PutUint64(out[12:20], HeaderLen)
	binary.LittleEndian.PutUint64(out[20:28], uint64(indexOff-HeaderLen))
	binary.LittleEndian.PutUint64(out[28:36], uint64(indexOff))
	binary.LittleEndian.PutUint64(out[36:44], uint64(len(index)))
	isum := sha256.Sum256(index)
	copy(out[44:76], isum[:])
	// 76..128 reserved 保持 0

	for i := range sorted {
		copy(out[places[i].off:], sorted[i].IR)
	}
	copy(out[indexOff:], index)
	// footer **整段留 0**（alg=0 表示未签名）。签名是 sign.Sign 的事。
	//
	// 这里刻意不写 alg=1：一份"自称签过、签名却是零"的文件，验签时会报
	// "签名不匹配"，而真正的问题是**它从来没被签过**。两种情况的修复动作不同
	// —— 一个去查密钥，一个去补签名步骤。
	return out, nil
}

func alignUp(n int) int {
	if rem := n % align; rem != 0 {
		return n + align - rem
	}
	return n
}

// buildIndex 用 canon 构造索引 —— 键序由 canon 统一决定，不在这里手写。
func buildIndex(members []Member, places []placement) []byte {
	ms := make([]canon.Value, 0, len(members))
	entries := []canon.Value{}
	symbols := []canon.Value{}

	for i := range members {
		m := members[i]
		rq := make([]canon.Value, 0, len(m.Requires))
		for _, r := range m.Requires {
			kv := []any{
				"spec", canon.Str(r.Spec),
				"kind", canon.Str(r.Kind),
				"abi_version", canon.Int(int64(r.ABIVersion)),
				"ir_version", canon.Int(int64(r.IRVersion)),
				"origin", canon.Str(r.Origin),
				"member_sha256", canon.Str(r.MemberSHA256),
			}
			if r.Implicit {
				kv = append(kv, "implicit", canon.Bool(true))
			}
			rq = append(rq, canon.Obj(kv...))
		}

		ms = append(ms, canon.Obj(
			"name", canon.Str(m.Name),
			"ir_off", canon.Int(int64(places[i].off)),
			"ir_len", canon.Int(int64(places[i].n)),
			"ir_sha256", canon.Str(places[i].sum),
			"abi_version", canon.Int(int64(m.ABIVersion)),
			"ir_version", canon.Int(1),
			"language_version", canon.Int(int64(m.LanguageVersion)),
			"exports", canon.Obj(
				"workflows", strArr(m.Exports.Workflows),
				"types", strArr(m.Exports.Types),
				"backends", strArr(m.Exports.Backends),
				"tools", strArr(m.Exports.Tools),
			),
			"requires", canon.Arr(rq...),
		))

		for _, w := range m.Exports.Workflows {
			symbols = append(symbols, canon.Obj(
				"member", canon.Str(m.Name), "kind", canon.Str("workflow"), "name", canon.Str(w)))
			if m.Entry {
				entries = append(entries, canon.Str(w))
			}
		}
		for _, t := range m.Exports.Types {
			symbols = append(symbols, canon.Obj(
				"member", canon.Str(m.Name), "kind", canon.Str("type"), "name", canon.Str(t)))
		}
	}

	root := canon.Obj(
		"archive_version", canon.Int(Version),
		"created_by", canon.Str("hermes/"+Version2()),
		"flags", canon.Int(0),
		"members", canon.Arr(ms...),
		"symbols", canon.Arr(symbols...),
		"entries", canon.Arr(entries...),
	)
	return root.Bytes()
}

// Version2 是"创建者版本"占位；真正接 CLI 版本时由调用方注入。
func Version2() string { return "1.0.0" }

func strArr(xs []string) canon.Value {
	vs := make([]canon.Value, 0, len(xs))
	for _, s := range xs {
		vs = append(vs, canon.Str(s))
	}
	return canon.Arr(vs...)
}

// Read 解析并校验一个 `.hbc`。
//
// 顺序刻意是**先结构、后密码学、最后语义**（§7.1）：结构错时报"第 k 个成员
// 长度越界"比"签名不对"有用得多。
func Read(data []byte) (*Archive, error) {
	if len(data) < HeaderLen+FooterLen {
		return nil, ErrBadLayout
	}
	if string(data[0:4]) != Magic {
		return nil, ErrBadMagic
	}
	if v := binary.LittleEndian.Uint16(data[4:6]); v != Version {
		return nil, fmt.Errorf("%w: %d", ErrBadVersion, v)
	}
	flags := binary.LittleEndian.Uint16(data[6:8])
	count := int(binary.LittleEndian.Uint32(data[8:12]))
	if count > MaxMembers {
		return nil, ErrTooMany
	}
	payloadOff := int(binary.LittleEndian.Uint64(data[12:20]))
	payloadLen := int(binary.LittleEndian.Uint64(data[20:28]))
	indexOff := int(binary.LittleEndian.Uint64(data[28:36]))
	indexLen := int(binary.LittleEndian.Uint64(data[36:44]))

	for _, v := range data[76:128] {
		if v != 0 {
			return nil, ErrReserved
		}
	}
	if payloadOff < HeaderLen || indexOff < payloadOff || indexLen < 0 ||
		indexOff+indexLen > len(data)-FooterLen || payloadOff+payloadLen > indexOff {
		return nil, ErrBadLayout
	}
	if payloadOff%align != 0 || indexOff%align != 0 {
		return nil, ErrBadLayout
	}
	index := data[indexOff : indexOff+indexLen]
	sum := sha256.Sum256(index)
	if !equalBytes(sum[:], data[44:76]) {
		return nil, ErrIndexSHA
	}

	var idx map[string]any
	dec := newDecoder(index)
	if err := dec.decode(&idx); err != nil {
		return nil, fmt.Errorf("%w: 索引不是合法 JSON: %v", ErrBadLayout, err)
	}
	rawMembers, _ := idx["members"].([]any)

	out := &Archive{Flags: flags, Index: index}
	for _, rm := range rawMembers {
		m, _ := rm.(map[string]any)
		name, _ := m["name"].(string)
		irOff := toInt(m["ir_off"])
		irLen := toInt(m["ir_len"])
		if irOff < payloadOff || irLen < 0 || irOff+irLen > indexOff {
			return nil, fmt.Errorf("%w: 成员 %q 的区间越界", ErrBadLayout, name)
		}
		ir := data[irOff : irOff+irLen]
		h := sha256.Sum256(ir)
		want, _ := m["ir_sha256"].(string)
		if hex.EncodeToString(h[:]) != want {
			return nil, fmt.Errorf("%w: 成员 %q", ErrMemberHash, name)
		}
		out.Members = append(out.Members, ReadMember{Name: name, IR: ir, Index: m})
	}
	return out, nil
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// IndexBytes 返回归档里的索引字节（签名覆盖的就是它，见 D-011）。
//
// 调用方拿到它才能回答"这份实现的指纹是什么" —— 而 requires[].member_sha256
// 要的正是这个问题的答案。
func (a *Archive) IndexBytes(raw []byte) []byte {
	if len(raw) < HeaderLen+FooterLen {
		return nil
	}
	indexOff := int(binary.LittleEndian.Uint64(raw[28:36]))
	indexLen := int(binary.LittleEndian.Uint64(raw[36:44]))
	if indexOff < HeaderLen || indexOff+indexLen > len(raw)-FooterLen {
		return nil
	}
	return raw[indexOff : indexOff+indexLen]
}
