package native

import (
	"bytes"
	"encoding/json"
	"testing"

	"hermes/internal/archive"
)

// TestArchiveWriteMatchesGo 是 `.hbc` 写入的等价判据：同一份成员描述，
// C++ 写出的字节必须与 Go 的 `archive.Write` **完全相同**。
//
// 为什么必须逐字节：归档里有偏移、索引哈希、以及 canonical 索引 JSON；
// 任何一处不同都会让"同一份源码两次构建产出不同产物"，可复现性直接失效，
// 而症状（签名对不上 / 分发链断开）出现在很远的地方。
func TestArchiveWriteMatchesGo(t *testing.T) {
	cases := []struct {
		name    string
		members []archive.Member
	}{
		{
			name: "single-entry",
			members: []archive.Member{{
				Name: "app", IR: []byte(`{"ir_version":2}`),
				Exports: archive.Exports{Workflows: []string{"w"}, Types: []string{"T"}},
				Entry:   true, LanguageVersion: 2, ABIVersion: 2,
			}},
		},
		{
			name: "two-members-with-requires",
			members: []archive.Member{
				{
					Name: "b", IR: []byte(`{"b":1}`),
					Exports:  archive.Exports{Workflows: []string{"wb"}},
					Requires: []archive.Require{{Spec: "<std/net>", Kind: "std", ABIVersion: 2, IRVersion: 2, Origin: "source", MemberSHA256: "aa"}},
				},
				{
					Name: "a", IR: []byte(`{"a":2}`),
					Exports: archive.Exports{Types: []string{"Ta"}}, LanguageVersion: 2, ABIVersion: 2,
				},
			},
		},
	}

	for _, c := range cases {
		want, err := archive.Write(c.members)
		if err != nil {
			t.Fatalf("%s: archive.Write: %v", c.name, err)
		}
		got := cppArchiveWrite(t, c.members)
		if !bytes.Equal(got, want) {
			t.Fatalf("%s: 字节不同（C++ %d 字节，Go %d 字节）", c.name, len(got), len(want))
		}
	}
}

// cppArchiveWrite 把 Go 的 Member 结构转成 C++ 侧要的 JSON 描述。
func cppArchiveWrite(t *testing.T, members []archive.Member) []byte {
	t.Helper()
	type req struct {
		Spec         string `json:"spec"`
		Kind         string `json:"kind"`
		ABIVersion   int    `json:"abi_version"`
		IRVersion    int    `json:"ir_version"`
		Origin       string `json:"origin"`
		MemberSHA256 string `json:"member_sha256"`
		Implicit     bool   `json:"implicit,omitempty"`
	}
	type exp struct {
		Workflows []string `json:"workflows"`
		Types     []string `json:"types"`
		Backends  []string `json:"backends"`
		Tools     []string `json:"tools"`
	}
	type mem struct {
		Name            string  `json:"name"`
		IR              string  `json:"ir"`
		ABIVersion      int     `json:"abi_version"`
		LanguageVersion int     `json:"language_version"`
		Entry           bool    `json:"entry"`
		Exports         exp     `json:"exports"`
		Requires        []req   `json:"requires"`
	}
	var ms []mem
	for _, m := range members {
		var rs []req
		for _, r := range m.Requires {
			rs = append(rs, req{r.Spec, r.Kind, r.ABIVersion, r.IRVersion, r.Origin, r.MemberSHA256, r.Implicit})
		}
		ms = append(ms, mem{
			Name: m.Name, IR: string(m.IR), ABIVersion: m.ABIVersion,
			LanguageVersion: m.LanguageVersion, Entry: m.Entry,
			Exports:  exp{m.Exports.Workflows, m.Exports.Types, m.Exports.Backends, m.Exports.Tools},
			Requires: rs,
		})
	}
	blob, err := json.Marshal(ms)
	if err != nil {
		t.Fatal(err)
	}
	out, err := ArchiveWrite(blob)
	if err != nil {
		t.Fatalf("ArchiveWrite: %v", err)
	}
	return out
}
