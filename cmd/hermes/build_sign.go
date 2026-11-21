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

// buildWithSigning 是 `hermes build -k <key>` 的整个实现。
//
// build 本体已经在 C++ 里（`hermes-native build`），这里只做**签名那一半**：
// 先让 native 产出 .hbc，再读出它、签名、写 sealed.hermes。
//
// 为什么签名留在 Go：Ed25519 只有 Go 标准库免费给（红线：零第三方依赖）。
// 这不是"还没搬"，是**永久分工**——所以这里刻意不复制一遍编译流程。
func buildWithSigning(args []string) int {
	var keyPath string
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-k" || a == "--key" {
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, tr("hermes build: -k needs a key file\n\n")+buildUsageForSigning())
				return 2
			}
			keyPath = args[i+1]
			i++
			continue
		}
		rest = append(rest, a)
	}

	// 先解析出源与产物路径：签名与封签都要用它们，而 native build 自己会算默认值。
	var srcPath, outPath string
	for i := 0; i < len(rest); i++ {
		switch a := rest[i]; a {
		case "-o", "--out":
			if i+1 < len(rest) {
				outPath = rest[i+1]
				i++
			}
		default:
			if srcPath == "" && a != "" && a[0] != '-' {
				srcPath = a
			}
		}
	}
	if srcPath == "" {
		fmt.Fprint(os.Stderr, tr("hermes build: missing source file\n\n")+buildUsageForSigning())
		return 2
	}
	if outPath == "" {
		base := filepath.Base(srcPath)
		outPath = base[:len(base)-len(filepath.Ext(base))] + ".hbc"
	}

	if code := execNative(append([]string{"build"}, rest...)...); code != 0 {
		return code
	}

	// 从产物读回事实：签名与封签需要的 IR / 导出 / bounds 全在 .hbc 里，
	// 不再回前端重算一遍（重算就又多了一份可能与产物不一致的真相）。
	data, err := os.ReadFile(outPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes build: cannot read %s: %v\n"), outPath, err)
		return 4
	}
	ar, err := archive.Read(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes build: cannot read %s: %v\n"), outPath, err)
		return 4
	}
	irBytes := ar.Members[0].IR
	doc, ds := ir.Load(irBytes)
	if doc == nil || len(ds) != 0 {
		fmt.Fprintf(os.Stderr, tr("hermes build: the archive's IR is not readable (%d diagnostics)\n"), len(ds))
		return 4
	}
	src, err := os.ReadFile(srcPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes build: cannot read %s: %v\n"), srcPath, err)
		return 4
	}

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
	// 顺序不可交换：先 .hbc 后 sealed（IR.md §8.6）。崩溃在中间的结果是
	// "新 .hbc + 旧/无 sealed" ⇒ verify 必然失败，而不会是"验证通过但不一致"。
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
		// 必须 UseNumber：默认解码会把 1048576 变成 1.048576e+06，而 canon 会
		// 正确地拒绝浮点 —— 于是封签失败，报错却是"含浮点"。
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

// buildUsageForSigning 只列 -k 相关用法：帮助文本的主体在 native 侧，
// 这里不复制一份（复制就会漂移）。
func buildUsageForSigning() string {
	return pickUsage(
		"Usage: hermes build <file.hermes> -k <key.pem> [-o out.hbc]\n\nWith -k the archive is signed and a sealed.hermes is written next to it (IR.md §8).\n",
		"用法: hermes build <file.hermes> -k <key.pem> [-o out.hbc]\n\n带 -k 时归档会被签名，并在旁边写出 sealed.hermes（IR.md §8）。\n")
}
