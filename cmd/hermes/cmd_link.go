package main

import (
	"fmt"
	"os"

	"path/filepath"
	"strings"

	"hermes/internal/archive"
	"hermes/internal/link"
)

// link 子命令：把多个 `.hbc` 合成一个可执行归档（IR.md §9）。
//
// 它比"把文件拼起来"多做的三件事：符号解析（重名是错误）、ODR、依赖字节校验。
// 这三件都不做的话，产物能跑，但"跑的是谁"没人说得清。
func cmdLink(args []string) int {
	var outPath string
	var inputs []string
	var searchDirs []string
	var libNames []string
	noStrip := false

	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--no-strip":
			noStrip = true
		case "-o", "--out":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, tr("hermes link: -o needs a file\n\n")+linkUsage)
				return 2
			}
			outPath = args[i+1]
			i++
		case "-l":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, tr("hermes link: -l needs a library name\n\n")+linkUsage)
				return 2
			}
			// **只记名字，不在这里拼路径**：`-L` 可能出现在 `-l` 之后，
			// 而参数顺序不该影响结果 —— 那是用户最容易踩的一脚。
			libNames = append(libNames, args[i+1])
			i++
		case "-L":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, tr("hermes link: -L needs a directory\n\n")+linkUsage)
				return 2
			}
			searchDirs = append(searchDirs, args[i+1])
			i++
		case "-h", "--help", "help":
			fmt.Print(linkUsage)
			return 0
		default:
			if a != "" && a[0] != '-' {
				inputs = append(inputs, a)
				continue
			}
			fmt.Fprintf(os.Stderr, "hermes link: %s %q\n\n%s", tr("unknown argument"), a, linkUsage)
			return 2
		}
	}
	// 解析 -l：在 -L 目录里找 `<dir>/lib<name>.hbc`，再找 `<dir>/<name>.hbc`。
	// 找不到时把**试过的每个路径**列出来 —— 只说"找不到 libfoo"会让人反复猜
	// `-L` 该指哪，而这类猜测每次都要跑一遍命令。
	for _, name := range libNames {
		found, tried := findLib(name, searchDirs)
		if found == "" {
			fmt.Fprintf(os.Stderr, tr("hermes link: cannot find library %q; tried: %s\n"), name, strings.Join(tried, ", "))
			return 4
		}
		inputs = append(inputs, found)
	}

	if len(inputs) == 0 {
		fmt.Fprint(os.Stderr, tr("hermes link: missing input .hbc\n\n")+linkUsage)
		return 2
	}
	if outPath == "" {
		outPath = "a.out.hbc"
	}

	var ins []link.Input
	for _, p := range inputs {
		data, err := os.ReadFile(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, tr("hermes link: cannot read %s: %v\n"), p, err)
			return 4
		}
		a, err := archive.Read(data)
		if err != nil {
			fmt.Fprintf(os.Stderr, tr("hermes link: %s is not a usable archive: %v\n"), p, err)
			return 4
		}
		ins = append(ins, link.Input{Path: p, Archive: a, Raw: data})
	}

	// 先做依赖字节校验，再做符号解析：前者是"你拿的是不是你以为的那份"，
	// 后者是"这些份之间有没有冲突"。顺序反了会把投毒报成重名。
	if problems := link.VerifyRequires(ins); len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintf(os.Stderr, tr("hermes link: %s\n"), p)
		}
		return 4
	}
	res, problems := link.Link(ins)
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintf(os.Stderr, tr("hermes link: %s\n"), p)
		}
		return 4
	}

	members := link.Merge(ins)
	if noStrip {
		fmt.Fprintf(os.Stderr, tr("hermes link: --no-strip: keeping all %d members\n"), len(members))
	} else {
		kept, dropped, probs := link.Reach(ins)
		if len(probs) > 0 {
			for _, p := range probs {
				fmt.Fprintf(os.Stderr, tr("hermes link: %s\n"), p)
			}
			return 4
		}
		// 一个都不剩 = 没有 entry，或可达性算法坏了。**必须报错**：
		// 交出一个空归档比交出大归档危险得多，而它验签、验结构全都过。
		if len(kept) == 0 {
			fmt.Fprint(os.Stderr, tr("hermes link: nothing reachable after stripping (missing is_entry?)\n"))
			return 4
		}
		filtered := members[:0]
		for _, m := range members {
			if kept[m.Name] {
				filtered = append(filtered, m)
			}
		}
		members = filtered
		for _, d := range dropped {
			fmt.Fprintf(os.Stderr, tr("hermes link: stripped %s (unreachable from entry)\n"), d)
		}
	}

	data, err := archive.Write(members)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes link: cannot write archive: %v\n"), err)
		return 4
	}
	if err := os.WriteFile(outPath, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes link: cannot write %s: %v\n"), outPath, err)
		return 4
	}
	fmt.Fprintf(os.Stderr, tr("hermes link: wrote %s (%d bytes, %d members, %d entries)\n"),
		outPath, len(data), len(res.Members), len(res.Entries))
	return 0
}

var linkUsage = pickUsage(`hermes link - link several .hbc archives into one

Usage:
  hermes link [<a.hbc> ...] [-L <dir>] [-l <name> ...] [-o <out.hbc>] [--no-strip]

  -L <dir>    add a search directory (repeatable)
  -l <name>   find a library by name: <dir>/lib<name>.hbc, then <dir>/<name>.hbc
              without -L the search starts at the current directory

Notes:
  Three things plain concatenation does not do:
    - symbol resolution: duplicate workflow/backend/tool is an ERROR, never overridden
    - ODR: duplicate structs are compared by sha256, mismatch reports a field-level diff
    - dependency bytes: requires[].member_sha256 that matches nothing is E_LINK_TAMPERED

  Dead code is stripped by reachability from the entry workflows; use --no-strip to keep all.

Exit codes:
  0  success
  2  usage error
  4  unreadable archive or link failed
`,
	`hermes link —— 把多个 .hbc 合成一个可执行归档

用法:
  hermes link <a.hbc> <b.hbc> ... [-o <out.hbc>]

说明:
  做三件"拼接"不会做的事：
    · 符号解析：同名 workflow / backend / tool 是**错误**，不覆盖
    · ODR：同名 struct 比 sha256，不同则报字段级 diff
    · 依赖字节校验：requires[].member_sha256 对不上就是 E_LINK_TAMPERED

  这一刀**不做死代码剥离** —— 剥错了比不剥危险（后者只是产物大）。

退出码:
  0  成功
  2  用法错误
  4  读不到 / 不是合法归档 / 链接失败
`)

// findLib 按 `-L` 顺序查找库文件，返回命中路径与**全部候选**。
//
// 两种命名都试（`libfoo.hbc` 与 `foo.hbc`）：前者是 unix 习惯，后者是
// 我们自己的 `hermes build` 默认产出的名字。只支持一种就会让"我刚 build 出来的
// 那个文件找不到"，而那种错误看起来像路径问题。
//
// 没有 `-L` 时退化为当前目录：让 `hermes link -l foo` 在同目录下也能工作，
// 而不是必须写 `-L .`。
func findLib(name string, dirs []string) (string, []string) {
	if len(dirs) == 0 {
		dirs = []string{"."}
	}
	var tried []string
	for _, d := range dirs {
		for _, candidate := range []string{
			filepath.Join(d, "lib"+name+".hbc"),
			filepath.Join(d, name+".hbc"),
		} {
			tried = append(tried, candidate)
			if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
				return candidate, tried
			}
		}
	}
	return "", tried
}
