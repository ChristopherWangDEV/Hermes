package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"hermes/internal/layout"
)

// licenses 子命令：把分发时**必须随附**的许可文本打出来。
//
// 为什么值得一条命令：合规手册上写着"分发要带许可证"，而真正会出错的地方是
// "带哪一个、从哪儿拿"。让它变成一条命令，就没人需要凭记忆拼一份清单。
func cmdLicenses(args []string) int {
	// 先定位发布根：许可文件在 <root>/LICENSE、<root>/NOTICE、
	// <root>/THIRD_PARTY_NOTICES.md、<root>/licenses/。
	exe, _ := os.Executable()
	cwd, _ := os.Getwd()
	// 候选根：可执行文件的布局根、当前目录、以及当前目录往上的几层。
	// 向上找是必要的：`go run` 把二进制放在临时目录，而许可文件在仓库里。
	var candidates []string
	if r := layout.Resolve(exe, cwd).Root; r != "" {
		candidates = append(candidates, r)
	}
	dir := cwd
	for i := 0; i < 4 && dir != "" && dir != string(os.PathSeparator); i++ {
		candidates = append(candidates, dir)
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	root := ""
	var tried []string
	for _, c := range candidates {
		tried = append(tried, c)
		if _, err := os.Stat(filepath.Join(c, "LICENSE")); err == nil {
			root = c
			break
		}
	}
	if root == "" {
		fmt.Fprintln(os.Stderr, tr("hermes licenses: no LICENSE found. Tried:"))
		for _, t := range tried {
			fmt.Fprintf(os.Stderr, "  %s\n", t)
		}
		fmt.Fprintln(os.Stderr, tr("  a release package should ship LICENSE / NOTICE / THIRD_PARTY_NOTICES.md / licenses/."))
		return 4
	}

	files := []string{
		filepath.Join(root, "LICENSE"),
		filepath.Join(root, "NOTICE"),
		filepath.Join(root, "THIRD_PARTY_NOTICES.md"),
	}
	licDir := filepath.Join(root, "licenses")
	if ents, err := os.ReadDir(licDir); err == nil {
		names := make([]string, 0, len(ents))
		for _, e := range ents {
			if !e.IsDir() {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, n := range names {
			files = append(files, filepath.Join(licDir, n))
		}
	}

	printed := 0
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		fmt.Printf("===== %s =====\n\n%s\n\n", filepath.Base(f), data)
		printed++
	}
	if printed == 0 {
		fmt.Fprintf(os.Stderr, "hermes licenses: %s\n", fmt.Sprintf(tr("no license file found under %s"), root))
		return 4
	}
	return 0
}

const licensesUsage = `hermes licenses - print the license texts that must ship with Hermes

Usage:
  hermes licenses

Notes:
  Prints LICENSE, NOTICE, THIRD_PARTY_NOTICES.md and every text under licenses/.
  Binary distribution requires these to travel with the binary; this command is the
  shortest path to collecting them.
`
