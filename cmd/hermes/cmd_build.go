package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"hermes/internal/archive"
	"hermes/internal/ir"
	"hermes/internal/sign"
)

// build 子命令：`.hermes` → `.hbc`。
//
// 这一刀只产**一个成员**（一个 unit）。多库链接是 T40 的事：
// `hermes build` 的产物是"库或可执行"，`hermes link` 才把多个库并起来。
func cmdBuild(args []string) int {
	var srcPath, outPath, keyPath string
	strictLink := true
	var libDirs []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "-o", "--out":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, tr("hermes build: -o needs a file\n\n")+buildUsage)
				return 2
			}
			outPath = args[i+1]
			i++
		case "-L":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, tr("hermes build: -L needs a directory\n\n")+buildUsage)
				return 2
			}
			libDirs = append(libDirs, args[i+1])
			i++
		case "--no-strict-link":
			// 明确放宽：允许本地 #link 解析不到而继续构建。
			// 产物在分发链上不完整，所以这是一个**显式**的选择，不是默认。
			strictLink = false
		case "--strict-link":
			strictLink = true
		case "-k", "--key":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, tr("hermes build: -k needs a key file\n\n")+buildUsage)
				return 2
			}
			keyPath = args[i+1]
			i++
		case "-h", "--help", "help":
			fmt.Print(buildUsage)
			return 0
		default:
			if srcPath == "" && a != "" && a[0] != '-' {
				srcPath = a
				continue
			}
			fmt.Fprintf(os.Stderr, "hermes build: %s %q\n\n%s", tr("unknown argument"), a, buildUsage)
			return 2
		}
	}
	if srcPath == "" {
		fmt.Fprint(os.Stderr, tr("hermes build: missing source file\n\n")+buildUsage)
		return 2
	}
	if outPath == "" {
		base := filepath.Base(srcPath)
		outPath = base[:len(base)-len(filepath.Ext(base))] + ".hbc"
	}

	src, err := os.ReadFile(srcPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes build: cannot read %s: %v\n"), srcPath, err)
		return 4
	}
	doc, err := buildIRWith(srcPath, src, &libSearch{dirs: libDirs, strict: strictLink})
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes build: %v\n"), err)
		return 4
	}
	irBytes, err := ir.Emit(doc)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes build: %v\n"), err)
		return 4
	}

	entry := false
	for i := range doc.Workflows {
		if doc.Workflows[i].IsEntry {
			entry = true
		}
	}
	reqs := make([]archive.Require, 0, len(doc.Requires))
	for _, r := range doc.Requires {
		reqs = append(reqs, archive.Require{
			Spec: r.Spec, Kind: r.Kind, ABIVersion: r.ABIVersion, IRVersion: r.IRVersion,
			Origin: r.Origin, MemberSHA256: r.MemberSHA256, Implicit: r.Implicit,
		})
	}

	data, err := archive.Write([]archive.Member{{
		Name: doc.Unit.Name,
		IR:   irBytes,
		Exports: archive.Exports{
			Workflows: doc.Exports.Workflows,
			Types:     doc.Exports.Types,
			Backends:  doc.Exports.Backends,
			Tools:     doc.Exports.Tools,
		},
		Requires:        reqs,
		Entry:           entry,
		LanguageVersion: doc.Unit.LanguageVersion,
		ABIVersion:      1,
	}})
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes build: cannot write archive: %v\n"), err)
		return 4
	}
	if err := os.WriteFile(outPath, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes build: cannot write %s: %v\n"), outPath, err)
		return 4
	}
	fmt.Fprintf(os.Stderr, tr("hermes build: wrote %s (%d bytes, %d members)\n"), outPath, len(data), 1)

	if keyPath == "" {
		return 0
	}

	// 带 -k 时同时产出 sealed.hermes（IR.md §8：两个产物缺一不可）。
	// 顺序**不可交换**：先 .hbc 后 sealed（§8.6）。崩溃在中间的结果是
	// "新 .hbc + 旧/无 sealed" ⇒ verify 必然失败，而不会是"验证通过但不一致"。
	k, err := loadKey(keyPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes build: cannot read key: %v\n"), err)
		return 4
	}
	signed, err := sign.Sign(data, k)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes build: signing failed: %v\n"), err)
		return 4
	}
	if err := os.WriteFile(outPath, signed, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes build: cannot write %s: %v\n"), outPath, err)
		return 4
	}

	bounds := map[string]int64{}
	eco := map[string]any{}
	if len(doc.Workflows) > 0 {
		b := doc.Workflows[0].Bounds
		bounds = map[string]int64{
			"max_spans": int64(b.MaxSpans), "max_model_calls": int64(b.MaxModelCalls),
			"max_tool_calls": int64(b.MaxToolCalls), "max_concurrency": int64(b.MaxConcurrency),
		}
		// **必须 UseNumber**：默认解码会把 1048576 变成 1.048576e+06（float64），
		// 而 canon 会正确地拒绝浮点 —— 于是封签失败，报错却是"含浮点"。
		var ev map[string]any
		dec := json.NewDecoder(bytes.NewReader(doc.Workflows[0].Economics))
		dec.UseNumber()
		if err := dec.Decode(&ev); err == nil {
			eco = ev
		}
	}

	sealedPath := strings.TrimSuffix(outPath, ".hbc") + ".sealed.hermes"
	sealed, err := sign.Seal(sign.SealInput{
		SourcePath:     filepath.Base(srcPath),
		Source:         src,
		IR:             irBytes,
		HBC:            signed,
		IndexSHA:       hex.EncodeToString(signed[44:76]),
		EntryWorkflows: doc.Exports.Workflows,
		Bounds:         bounds,
		Economics:      eco,
	}, k)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes build: sealing failed: %v\n"), err)
		return 4
	}
	if err := os.WriteFile(sealedPath, sealed, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes build: cannot write %s: %v\n"), sealedPath, err)
		return 4
	}
	fmt.Fprintf(os.Stderr, tr("hermes build: wrote %s (%d bytes, key_id=%s)\n"), sealedPath, len(sealed), k.ID())
	return 0
}

var buildUsage = pickUsage(`hermes build - build a .hbc archive

Usage:
  hermes build <file.hermes> [-o <out.hbc>] [-k <key.pem>] [-L <dir>]

Notes:
  The archive holds IR bytes plus an index, no source. With -k it is signed and a
  sealed.hermes is produced next to it (both products are required by IR.md §8).

Exit codes:
  0  success
  2  usage error
  4  compilation or write failed
`,
	`hermes build —— 把一个 .hermes 编成 .hbc 归档

用法:
  hermes build <file.hermes> [-o <out.hbc>]

说明:
  产物是**可分发、可签名**的归档：装的是 IR 字节与索引，不含源码。
  这一刀一个 unit 一个成员；多库合并是 hermes link（T40）。

退出码:
  0  成功
  2  用法错误
  4  编译失败或写不出
`)
