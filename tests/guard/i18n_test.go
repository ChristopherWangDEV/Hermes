package guard

// 用户可见的输出必须走 `tr(en, zh)`，且两句都非空。
//
// 为什么需要守卫：绕过 tr() 直接写一句中文，在英文环境里就是一段看不懂的文字；
// 这种事不会让任何别的测试变红，只会让界面慢慢分裂成"半个中文、半个英文"。
//
// 这条守卫是在把 49 处写死的中文清干净之后才装上的 —— 在此之前它是红的，
// 而一个长期红的守卫等于没有守卫。

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestNoBareChineseOutput 守"打印语句里不许直接写中文"。
func TestNoBareChineseOutput(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "cmd/hermes")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	han := regexp.MustCompile(`\p{Han}`)
	printer := regexp.MustCompile(`Fprint|Printf|Println`)
	var bad []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			// 注释不算输出。
			if strings.HasPrefix(trimmed, "//") || !printer.MatchString(line) {
				continue
			}
			if han.MatchString(line) && !strings.Contains(line, "tr(") {
				bad = append(bad, name+":"+strconv.Itoa(i+1))
			}
		}
	}
	if len(bad) > 0 {
		t.Fatalf("这些输出直接写了中文，没有走 tr(en, zh)：%s", strings.Join(bad, ", "))
	}
}

// TestTRCallsUseOneArgument 守 tr() 只收一个参数。
//
// 迁移完成后，中文只在语言包里。若代码里又冒出 tr("en", "zh")，说明
// 有人在往回走 —— 那一份中文永远不会被 Lexii 看到。
func TestTRCallsUseOneArgument(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "cmd/hermes")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	call := regexp.MustCompile(`tr\(\s*"[^"]*"\s*,`)
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if m := call.FindStringSubmatch(string(raw)); m != nil {
			t.Fatalf("%s: tr() 又收了两段参数（%q）——中文只应存在于 i18n/hermes.rp", name, m[0])
		}
	}
}
