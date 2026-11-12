// gen_chain_golden.go —— 哈希链 golden vector 的**唯一**生成器。
//
// 三处消费、一处生成（ARCH §5.3）：
//   生成：本文件（Go，crypto/sha256）
//   消费 1：internal/native 的 Go 测试（走 Go→cgo→Rust）
//   消费 2：native/rust/src/chain.rs 的单元测试（直接调 Rust 函数）
//   消费 3：tests/guard 的交叉校验（Go 里独立的第二份 SHA-256 计算路径）
//
// 用法：
//   go run ./scripts/gen_chain_golden.go -write    # 重写 schemas/golden/chain-*.json
//   go run ./scripts/gen_chain_golden.go           # 校验磁盘上的文件与生成器一致
//
// build.sh 只跑校验模式（-verify，默认）。这样 golden 文件**不会**被构建
// 过程悄悄刷新 —— 一旦有人改了哈希的字节规则而没意识到，构建直接红，
// 而不是"自动更新到一个新的、同样自洽的错误答案"。
//
// 字节级定义见 docs/CANONICAL.md 与 native/rust/src/chain.rs 的模块文档。
// 本文件是那份定义的独立第二实现：两边任何一处改了分隔符 / 字段顺序 /
// 端序，三组 vector 立刻对不上。

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

const (
	domainName = "hermes.trace.v1"
	finalTag   = "hermes.trace.final"
	blockMax   = 1 << 20
	outDir     = "schemas/golden"
)

// dispatch 是哈希的**唯一**字节布局。Rust 侧的 `dispatch` 必须逐字节一致。
func dispatch(domain []byte, prev [32]byte, index, length uint64, tail []byte) [32]byte {
	h := sha256.New()
	h.Write(domain)
	h.Write([]byte{0x00}) // 域分隔符：防止 "domain‖prev" 的边界混淆
	h.Write(prev[:])
	var u [8]byte
	binary.LittleEndian.PutUint64(u[:], index)
	h.Write(u[:])
	binary.LittleEndian.PutUint64(u[:], length)
	h.Write(u[:])
	h.Write(tail)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func blockHash(prev [32]byte, index uint64, block []byte) [32]byte {
	return dispatch([]byte(domainName), prev, index, uint64(len(block)), block)
}

func finalHash(prev [32]byte, count, totalLen uint64) [32]byte {
	return dispatch([]byte(domainName), prev, count, totalLen, []byte(finalTag))
}

// Vector 是 golden 文件的结构。多出来的 name/note 字段是给人读的，
// Rust 侧的解析器容忍未知键。
type Vector struct {
	Name     string   `json:"name"`
	Note     string   `json:"note"`
	Domain   string   `json:"domain"`
	FinalTag string   `json:"final_tag"`
	Blocks   []string `json:"blocks"`
	Expected []string `json:"expected"`
	Final    string   `json:"final"`
}

func build(name, note string, blocks [][]byte) Vector {
	prev := [32]byte{}
	total := uint64(0)
	expected := make([]string, 0, len(blocks))
	for i, b := range blocks {
		h := blockHash(prev, uint64(i), b)
		expected = append(expected, hex.EncodeToString(h[:]))
		prev = h
		total += uint64(len(b))
	}
	fin := finalHash(prev, uint64(len(blocks)), total)
	return Vector{
		Name:     name,
		Note:     note,
		Domain:   domainName,
		FinalTag: finalTag,
		Blocks:   hexAll(blocks),
		Expected: expected,
		Final:    hex.EncodeToString(fin[:]),
	}
}

func hexAll(blocks [][]byte) []string {
	out := make([]string, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, hex.EncodeToString(b))
	}
	return out
}

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((i*31 + int(seed)) & 0xff)
	}
	return b
}

func vectors() []Vector {
	// chain-001：单个空块。测的是"len=0 也要有确定哈希"这个边界 ——
	// 很多实现会在这里对空切片取到 nil 指针然后特判错。
	v1 := build(
		"chain-001",
		"空块：单块、长度 0。覆盖 len=0 与创世 prev=全 0 的边界。",
		[][]byte{{}},
	)

	// chain-002：多块 + 转义路径。块内容刻意塞进
	//   \n、\u0000-\u001f、非 ASCII UTF-8、双引号、反斜杠。
	// 这些正是"canonical 字节化"最容易与其他语言产生分歧的字符。
	v2 := build(
		"chain-002",
		"多块 + 转义路径：换行、控制字符、非 ASCII、引号、反斜杠。",
		[][]byte{
			[]byte("{\"step\":\"extract\",\"ok\":true}\n"),
			{0x00, 0x01, 0x02, 0x1f, 0x7f, 0x0b, 0x0c},
			[]byte("héllo→世界\u2028\u2029"),
			[]byte("back\\slash and \"quote\" and /solidus/"),
		},
	)

	// chain-003：跨 1 MiB 边界。
	//   block[0] 长度**恰好**等于 HM_CHAIN_BLOCK_MAX（必须被接受，不许 TOO_LARGE）
	//   block[1] 让链的总长度越过 1 MiB
	// 这两件事一起把"边界判定用了 > 还是 >= "钉死。
	v3 := build(
		"chain-003",
		"跨 1 MiB 边界：首块恰好等于 HM_CHAIN_BLOCK_MAX，总长越过 1 MiB。",
		[][]byte{
			pattern(blockMax, 7),
			pattern(17, 200),
		},
	)

	return []Vector{v1, v2, v3}
}

func encode(v Vector) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func main() {
	write := flag.Bool("write", false, "重写 schemas/golden/chain-*.json")
	flag.Parse()

	failed := false
	for _, v := range vectors() {
		data, err := encode(v)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gen_chain_golden: encode %s: %v\n", v.Name, err)
			os.Exit(1)
		}
		path := filepath.Join(outDir, v.Name+".json")

		if *write {
			if err := os.MkdirAll(outDir, 0o755); err != nil {
				fmt.Fprintf(os.Stderr, "gen_chain_golden: mkdir: %v\n", err)
				os.Exit(1)
			}
			if err := os.WriteFile(path, data, 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "gen_chain_golden: write %s: %v\n", path, err)
				os.Exit(1)
			}
			fmt.Printf("wrote %s (%d bytes, %d blocks, final=%s)\n",
				path, len(data), len(v.Blocks), v.Final)
			continue
		}

		onDisk, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr,
				"gen_chain_golden: %s is missing or unreadable: %v\n(run 'go run ./scripts/gen_chain_golden.go -write' first)\n",
				path, err)
			failed = true
			continue
		}
		if !bytes.Equal(onDisk, data) {
			fmt.Fprintf(os.Stderr,
				"gen_chain_golden: %s disagrees with the generator output - the hash byte rule was changed.\n"+
					"this is the core guard for INV-3 (a trace must not be tampered without leaving a trace); do not cover it up with -write.\n", path)
			failed = true
			continue
		}
		fmt.Printf("ok %s (%d blocks, final=%s)\n", path, len(v.Blocks), v.Final)
	}

	if failed {
		os.Exit(1)
	}
}
