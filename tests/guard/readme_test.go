// 本文件守 README 的**可执行性**：示例代码块必须真的能编译。
//
// 为什么值得一条守卫：README 里的示例是最多人复制的东西，而它坏掉时没人会报
// —— 读者只会默默关掉页面。文档里的代码和文档里的数字一样，都得被机器验过。
package guard_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
)

// TestReadmeExamplesCompile 抽出 README 里所有 ```hermes 代码块并逐个编译。
func TestReadmeExamplesCompile(t *testing.T) {
	root := repoRoot(t)
	bin := filepath.Join(root, "build", "hermes")
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("没有 %s（先跑 scripts/build.sh）", bin)
	}
	// 两份 README 都要验：中文读者拿到的示例不能是没编译过的。
	// 中英两版合并进同一个 README.md，所以只验一个文件 —— 但里面的代码块两份都要能编。
	checkREADME(t, root, bin, "README.md")
}

func checkREADME(t *testing.T, root, bin, name string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatalf("读不到 %s: %v", name, err)
	}
	blocks := regexp.MustCompile("(?s)```hermes\\n(.*?)```").FindAllStringSubmatch(string(data), -1)
	if len(blocks) == 0 {
		t.Fatalf("%s 里一个 hermes 代码块都没有", name)
	}
	dir := t.TempDir()
	for i, m := range blocks {
		src := filepath.Join(dir, "example"+itoa(i)+".hermes")
		if err := os.WriteFile(src, []byte(m[1]), 0o644); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(dir, "example"+itoa(i)+".json")
		if b, err := exec.Command(bin, "ir", src, "-o", out).CombinedOutput(); err != nil {
			t.Fatalf("%s 第 %d 个示例编译失败: %v\n%s\n--- 源码 ---\n%s", name, i+1, err, b, m[1])
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var s []byte
	for n > 0 {
		s = append([]byte{byte('0' + n%10)}, s...)
		n /= 10
	}
	return string(s)
}
