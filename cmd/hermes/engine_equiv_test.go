package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestEngineBehaviourIsPinned 把 **IR 执行器**（internal/engine，Go）今天的实际行为
// 固化下来，形成一份可回归的契约。
//
// 为什么要先有它："用 C++ 重写执行器"这件事的唯一可信验收标准是"行为逐字相同"。
// 没有这份基线，重写完成后没有任何东西能证明它没变 —— 而执行器的行为差异
// （步数、错误码、trace 形状）恰恰是最难靠肉眼发现的那一类。
//
// 它今天锁的是 Go 实现；将来 C++ 实现必须跑出同样的字节。
// 生成基线：HERMES_UPDATE_ENGINE_GOLDEN=1 go test ./cmd/hermes -run TestEngineBehaviourIsPinned
func TestEngineBehaviourIsPinned(t *testing.T) {
	root := repoRootForBackend(t)
	bin := filepath.Join(root, "build", "hermes")
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("没有 %s（先跑 scripts/build.sh）", bin)
	}
	update := os.Getenv("HERMES_UPDATE_ENGINE_GOLDEN") == "1"
	goldenDir := filepath.Join(root, "tests", "engine", "golden")
	if update {
		if err := os.MkdirAll(goldenDir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	var srcs []string
	for _, dir := range []string{"examples", "tests", "cmd/hermes/testdata"} {
		filepath.Walk(filepath.Join(root, dir), func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() && strings.HasSuffix(p, ".hermes") {
				srcs = append(srcs, p)
			}
			return nil
		})
	}
	sort.Strings(srcs)

	dir := t.TempDir()
	pinned, skipped := 0, 0
	for _, src := range srcs {
		rel, _ := filepath.Rel(root, src)
		irPath := filepath.Join(dir, strings.ReplaceAll(rel, string(filepath.Separator), "_")+".json")
		if out, err := exec.Command(bin, "ir", src, "-o", irPath).CombinedOutput(); err != nil {
			_ = out
			skipped++
			continue
		}
		// 以**可执行入口**为准：需要 input 的 workflow 不带输入跑，会以契约错误
		// 收场 —— 那种"错误形状"同样是有价值的行为基线，所以不跳过，
		// 只要求输出可解析。
		out, _ := exec.Command(bin, "run", irPath).Output()
		trimmed := strings.TrimSpace(string(out))
		if trimmed == "" {
			skipped++
			continue
		}
		var probe map[string]any
		if json.Unmarshal([]byte(trimmed), &probe) != nil {
			skipped++ // 非 JSON 输出（错误文本）不进基线
			continue
		}
		name := strings.ReplaceAll(rel, string(filepath.Separator), "_") + ".json"
		path := filepath.Join(goldenDir, name)
		if update {
			if err := os.WriteFile(path, []byte(trimmed+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			pinned++
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s：缺基线 %s（用 HERMES_UPDATE_ENGINE_GOLDEN=1 生成）", rel, name)
			continue
		}
		pinned++
		if trimmed != strings.TrimSpace(string(want)) {
			t.Errorf("%s：执行器输出与基线不同\n  基线: %s\n  现在: %s", rel, strings.TrimSpace(string(want)), trimmed)
		}
	}
	t.Logf("执行器基线：已钉 %d 份，跳过 %d 份（共 %d 份源码）", pinned, skipped, len(srcs))
}
