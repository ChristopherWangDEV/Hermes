package main

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"hermes/internal/backend"
)

// TestCBackendEquivalenceOnRealSources 是"去 Go 化"切换默认前的**扩样**验证。
//
// 为什么 golden 不够：schemas/golden 只有 12 份可比对的 IR，覆盖不到的节点
// （while / parallel / assign / emit / slot / store_op…）一旦切换默认，就会以
// "产物行为悄悄变了"的形态进正式版 —— 而那种缺陷没有任何测试会报。
//
// 所以这里拿**仓库里所有真实的 .hermes 源码**跑一遍：同一份源码 → 同一份 IR →
// 两条后端各生成一次 C → 逐字节比对。覆盖面由真实代码决定，不由我挑样本决定。
func TestCBackendEquivalenceOnRealSources(t *testing.T) {
	root := repoRootForBackend(t)
	goBE, ok := backend.For("c")
	if !ok {
		t.Fatal("找不到 c 后端")
	}

	var srcs []string
	for _, dir := range []string{"examples", "tests", "cmd/hermes/testdata"} {
		base := filepath.Join(root, dir)
		err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if info.IsDir() {
				return nil
			}
			if strings.HasSuffix(path, ".hermes") {
				srcs = append(srcs, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("遍历 %s: %v", dir, err)
		}
	}
	sort.Strings(srcs)
	if len(srcs) == 0 {
		t.Fatal("一个 .hermes 源码都没找到 —— 路径变了？这条扩样测试就没意义了")
	}

	var compared, goUnsupported, loadFailed int
	var mismatches []string
	for _, path := range srcs {
		rel, _ := filepath.Rel(root, path)
		src, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		doc, err := buildIR(rel, src)
		if err != nil {
			// 源码本身编不过（负例 testdata）：不是后端的事。
			loadFailed++
			continue
		}
		want, err := goBE.Generate(doc)
		if err != nil {
			// Go 版不支持这份 IR（S0 子集外）—— 等价性无从谈起。
			goUnsupported++
			continue
		}
		got, err := backend.NativeC().Generate(doc)
		if err != nil {
			if backend.ErrNotCovered(err) {
				mismatches = append(mismatches, rel+"  [C++ 未覆盖] "+err.Error())
				continue
			}
			mismatches = append(mismatches, rel+"  [C++ 报错] "+err.Error())
			continue
		}
		compared++
		if !bytes.Equal(got, want) {
			mismatches = append(mismatches, rel+"  [产物不一致] "+firstDiffLine(want, got))
		}
	}

	t.Logf("真实源码 %d 份：已比对 %d、Go 版不支持 %d、源码编不过 %d、问题 %d",
		len(srcs), compared, goUnsupported, loadFailed, len(mismatches))
	if len(mismatches) > 0 {
		for _, m := range mismatches {
			t.Errorf("%s", m)
		}
	}
}

// firstDiffLine 找出第一处不同，给出两侧的行内容 —— 只报"长度不同"没用，
// 要能一眼看出差在哪一行。
func firstDiffLine(want, got []byte) string {
	w := strings.Split(string(want), "\n")
	g := strings.Split(string(got), "\n")
	n := len(w)
	if len(g) < n {
		n = len(g)
	}
	for i := 0; i < n; i++ {
		if w[i] != g[i] {
			return "第 " + itoaBackend(i+1) + " 行\n    Go:  " + w[i] + "\n    C++: " + g[i]
		}
	}
	return "前 " + itoaBackend(n) + " 行相同，长度不同：Go " +
		itoaBackend(len(w)) + " 行 / C++ " + itoaBackend(len(g)) + " 行"
}

func itoaBackend(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func repoRootForBackend(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("找不到仓库根（go.mod）")
		}
		dir = parent
	}
}
