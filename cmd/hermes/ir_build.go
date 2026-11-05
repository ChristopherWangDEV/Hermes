package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"hermes/internal/archive"
	"hermes/internal/diag"
	"hermes/internal/include"
	"hermes/internal/ir"
	"hermes/internal/native"
	"hermes/internal/std"
)

// buildIR 从一份 `.hermes` 入口展开整棵 include 图，装配出一份完整 IR。
//
// 为什么装配在 Go：`unit.modules[].sha256` 要读文件字节、include 图要读文件系统，
// 而 C++ 前端被定义为无 IO（LANGUAGE §1.2）。分工是"谁看得见谁负责"（D-027 推论）：
// 前端出每个模块的类型表与入口的 workflow，Go 出模块表、类型表合并与最终字节。
// libSearch 是本地 `#link` 的解析器：把模块名解析成"那份实现的字节指纹"。
//
// 为什么在**编译期**就要解析：IR.md §9.5 要求非 implicit 的 requires 必须给
// `member_sha256` —— 声明"我要某个名字的东西"不算声明，"我要那份字节"才算。
// 而链接期已经太晚：那时 IR 已经生成、已经签过名了。
//
// 找不到时返回空串：调用方发 warning（不编假哈希，也不静默丢弃）。
type libSearch struct {
	dirs []string
	// strict 为 true 时，本地 #link 解析不到就直接失败。
	//
	// 为什么默认严格：不给 -L 时产物里**没有** requirement，而"看起来完全正常"。
	// 分发出去以后，供应链校验会以为这个 unit 什么都不依赖 —— 那是最难查的一类问题。
	// 想放宽必须显式说 `--no-strict-link`。
	strict bool
}

func (ls *libSearch) resolve(module string) (string, bool) {
	if ls == nil || len(ls.dirs) == 0 {
		return "", false
	}
	for _, d := range ls.dirs {
		for _, cand := range []string{
			filepath.Join(d, "lib"+module+".hbc"),
			filepath.Join(d, module+".hbc"),
		} {
			data, err := os.ReadFile(cand)
			if err != nil {
				continue
			}
			a, err := archive.Read(data)
			if err != nil {
				continue
			}
			// 用**索引字节**的哈希：那是 `.hbc` 签名覆盖的东西（D-011），
			// 也是"这份实现的指纹"，而不是某个成员 IR 的哈希 ——
			// 一个库可以有多个成员，而依赖是针对"这个库"的。
			index := a.IndexBytes(data)
			sum := sha256.Sum256(index)
			return hex.EncodeToString(sum[:]), true
		}
	}
	return "", false
}

func buildIR(entryPath string, src []byte) (*ir.Doc, error) {
	return buildIRWith(entryPath, src, nil)
}

func buildIRWith(entryPath string, src []byte, libs *libSearch) (*ir.Doc, error) {
	res, ds := include.Resolve(entryPath, src, include.Options{})
	if err := firstError(ds); err != nil {
		return nil, err
	}

	modules := make([]ir.Module, 0, len(res.Modules))
	// 内置模块的字节也进 sha256：T42 之后它们是真源码，不是"没有的东西"。
	// 于是 `unit.source_sha256` 与 `requires[].member_sha256` 都有据可依。
	byPath := map[string][]byte{}
	for _, m := range res.Modules {
		sum := sha256.Sum256(m.Source)
		modules = append(modules, ir.Module{
			Path: m.Path, Builtin: m.Builtin, SHA256: hex.EncodeToString(sum[:]),
		})
		byPath[m.Path] = m.Source
	}

	// requires[]：#link 的落地。每条都要有 member_sha256 —— 否则就是
	// "声明了要链接一个库却不说要它的哪份字节"（IR.md §9.5 的 E_LINK_REQUIRES_INCOMPLETE）。
	requires := make([]ir.RequireItem, 0, len(res.Links)+1)
	for _, l := range res.Links {
		var sum []byte
		if l.Kind == "std" {
			// `Link.Module` 是 **`std/net`**（带 `std/` 前缀），而 std.Source 要的是
			// 模块名 `net`。少剥这一层，requires 会静默为空 —— 而"空"看起来
			// 和"这个 unit 没有 #link"一模一样。
			mod := strings.TrimPrefix(l.Module, "std/")
			if src, ok := std.Source(mod); ok {
				h := sha256.Sum256(src)
				sum = h[:]
			}
		}
		if sum == nil && l.Kind == "local" {
			if h, ok := libs.resolve(l.Module); ok {
				hexSum, _ := hex.DecodeString(h)
				sum = hexSum
			}
		}
		if sum == nil {
			// 本地库：编译期拿不到实现字节，而 IR.md §9.5 要求非 implicit 的
			// requires 必须给 member_sha256 —— 所以这里**不能编一个假的哈希**，
			// 只能不写进 requires[]。
			//
			// 但"不写"必须是**可见**的：静默丢掉一条用户声明的依赖，会让
			// 供应链校验以为"这个 unit 什么都不依赖"。所以发一条 warning
			// 并说清补救方式（link 时用 -l 指定，链接器才有字节）。
			msg := fmt.Sprintf("本地依赖 %s 解析不到：requires[] 里不会写入它，"+
				"产物在分发链上不完整（编译期拿不到实现字节）。用 -L <dir> 指向库所在目录。", l.Spec)
			if libs != nil && libs.strict {
				return nil, fmt.Errorf("%s\n"+
					"  确实要在没有实现字节的情况下构建，加 --no-strict-link", msg)
			}
			fmt.Fprintf(os.Stderr, "hermes ir: warning: %s\n", msg)
			continue
		}
		requires = append(requires, ir.RequireItem{
			Spec: l.Spec, Kind: l.Kind, ABIVersion: 1, IRVersion: 1,
			Origin: "source", MemberSHA256: hex.EncodeToString(sum),
		})
	}

	// 每个模块各出一张类型表，按 include 到达顺序合并。顺序即 id 顺序（IR.md §3.1）。
	merged := make([]map[string]any, 0, 16)
	_ = merged
	exportedDecls := map[string]map[string]any{}
	var backends []map[string]any
	var toolsDecl []map[string]any
	for i, m := range res.Modules {
		if len(m.Source) == 0 {
			continue // 内置 std 模块没有源文件（T42 才把它们的字节装进来）
		}
		js, tdiag, err := native.CodegenTypes(m.Source, filepath.Base(m.Path), m.Kind == "h")
		if err != nil {
			return nil, err
		}
		// 入口模块的**类型检查**信息在这一趟还不完整（它的 struct 可能来自
		// 后面才合并进来的 .h），所以这一趟只为"收集声明"，诊断留给第二趟
		// （带 externals 的重跑）。.h 模块没有函数体，诊断照常严查。
		if i != 0 && tdiag != "" && tdiag != "[]" {
			return nil, fmt.Errorf("%s 的类型表编不出来:\n%s", m.Path, tdiag)
		}
		var ts []map[string]any
		if err := json.Unmarshal([]byte(js), &ts); err != nil {
			return nil, fmt.Errorf("%s 的类型表不是合法 JSON: %w", m.Path, err)
		}
		for _, t := range ts {
			t["module"] = i
			merged = append(merged, t)
		}

		// 顺带收集 workflow 声明：`export` / `entry` 写在 .h 的声明上，
		// `.hermes` 里的定义不带（LANGUAGE §3.5）。只看定义会把每个正常工程判错。
		wjs, wdiag, err := native.CodegenWorkflowDecls(m.Source, filepath.Base(m.Path), m.Kind == "h")
		if err != nil {
			return nil, err
		}
		if i != 0 && wdiag != "" && wdiag != "[]" {
			return nil, fmt.Errorf("%s 的 workflow 声明编不出来:\n%s", m.Path, wdiag)
		}
		bjs, bdiag, err := native.CodegenBackends(m.Source, filepath.Base(m.Path), m.Kind == "h")
		if err != nil {
			return nil, err
		}
		if i != 0 && bdiag != "" && bdiag != "[]" {
			return nil, fmt.Errorf("%s 的 backend 表编不出来:\n%s", m.Path, bdiag)
		}
		var bds []map[string]any
		if err := json.Unmarshal([]byte(bjs), &bds); err == nil {
			backends = append(backends, bds...)
		}

		// 工具声明与 backend 同一条路径收集：它们都是**模块级**事实，
		// 而 agent 按名字引用它们（名字只该有一份定义）。
		tjs, tdiag, err := native.CodegenTools(m.Source, filepath.Base(m.Path), m.Kind == "h")
		if err != nil {
			return nil, err
		}
		if i != 0 && tdiag != "" && tdiag != "[]" {
			return nil, fmt.Errorf("%s 的 tool 声明编不出来:\n%s", m.Path, tdiag)
		}
		var tds []map[string]any
		if err := json.Unmarshal([]byte(tjs), &tds); err == nil {
			toolsDecl = append(toolsDecl, tds...)
		}

		var wds []map[string]any
		if err := json.Unmarshal([]byte(wjs), &wds); err == nil {
			for _, w := range wds {
				name, _ := w["name"].(string)
				if name == "" {
					continue
				}
				// **按 OR 合并**，不是"先到先得"：同一个 workflow 在 .hermes 里定义
				// （不带 export）、在 .h 里声明（带 export）。先读到定义那一份就
				// 存下 false，声明里的 true 永远进不来 —— 而 entry 必须 exported。
				prev := exportedDecls[name]
				cur := map[string]any{}
				if prev != nil {
					cur["exported"] = truthy(prev["exported"])
					cur["is_entry"] = truthy(prev["is_entry"])
				}
				cur["exported"] = truthy(cur["exported"]) || truthy(w["exported"])
				cur["is_entry"] = truthy(cur["is_entry"]) || truthy(w["is_entry"])
				exportedDecls[name] = cur
			}
		}
	}
	typesJSON, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}

	// 先把所有声明拼成声明表：`Struct\tField\tType`，每行一条。
	// 这是**跨文件类型检查**的输入 —— 单文件检查器看不到别的 .h。
	var ext strings.Builder
	for _, t := range merged {
		name, _ := t["name"].(string)
		fields, _ := t["fields"].([]any)
		for _, f := range fields {
			fm, _ := f.(map[string]any)
			fname, _ := fm["n"].(string)
			tm, _ := fm["t"].(map[string]any)
			fk, _ := tm["k"].(string)
			if fk == "struct" {
				fk, _ = tm["ref"].(string)
			}
			fmt.Fprintf(&ext, "%s\t%s\t%s\n", name, fname, fk)
		}
	}

	// 函数表是 unit 级事实：每个模块各出一张，按 include 到达顺序合并。
	// 与 workflow 分开调用（新 C ABI 入口），不碰 hm_codegen_ext 的签名。
	var funcsDecl []map[string]any
	for _, m := range res.Modules {
		if len(m.Source) == 0 {
			continue // 内置 std 模块没有源文件
		}
		fjs, fdiag, err := native.CodegenFunctions(m.Source, filepath.Base(m.Path), m.Kind == "h", ext.String())
		if err != nil {
			return nil, err
		}
		if fdiag != "" && fdiag != "[]" {
			// 函数编不出来是**用户源码的问题**，必须带源码片段渲染，不能只吐 JSON。
			if strings.Contains(fdiag, `"severity":"error"`) {
				return nil, fmt.Errorf("%s: functions failed to compile:\n%s", m.Path, renderDiags(fdiag, m.Source))
			}
			fmt.Fprintf(os.Stderr, "hermes: %s\n", fdiag)
		}
		var fs []map[string]any
		if err := json.Unmarshal([]byte(fjs), &fs); err != nil {
			return nil, fmt.Errorf("%s 的函数表不是合法 JSON: %w", m.Path, err)
		}
		funcsDecl = append(funcsDecl, fs...)
	}
	var functionsJSON []byte
	if len(funcsDecl) > 0 {
		functionsJSON, err = json.Marshal(funcsDecl)
		if err != nil {
			return nil, err
		}
	}

	wf, err := native.CodegenWithExternals(src, filepath.Base(entryPath), false, ext.String())
	if err != nil {
		return nil, err
	}
	// 诊断分两级：**error 拦**，**warning 只打印**。
	//
	// 以前这里"有任何诊断就失败"，于是加一条警告就等于加一条阻塞 ——
	// 而警告存在的意义是"能编，但建议改"。
	if wf.Diag != "" && wf.Diag != "[]" {
		if strings.Contains(wf.Diag, `"severity":"error"`) {
			return nil, fmt.Errorf("%s: workflow failed to compile:\n%s", entryPath, renderDiags(wf.Diag, src))
		}
		// 只有警告：打印出来，继续构建。
		fmt.Fprintf(os.Stderr, "hermes: %s\n", wf.Diag)
	}

	unit := filepath.Base(entryPath)
	if ext := filepath.Ext(unit); ext != "" {
		unit = unit[:len(unit)-len(ext)]
	}

	// 用声明里的 export / entry 覆盖定义里的：语言规定这两件事写在 .h 上。
	// codegen 出的是**数组**（一个 unit 可以有多个 workflow，D-037）。
	// export / entry 的标注写在 .h 的声明上，所以按名字把这份标注贴回每个 workflow。
	wfJSON := []byte(wf.JSON)
	var wfObjs []map[string]any
	if err := json.Unmarshal(wfJSON, &wfObjs); err != nil {
		return nil, fmt.Errorf("workflows 不是合法 JSON: %w", err)
	}
	for _, wfObj := range wfObjs {
		name, _ := wfObj["name"].(string)
		d := exportedDecls[name]
		if name == "" || d == nil {
			continue
		}
		wfObj["exported"] = d["exported"]
		wfObj["is_entry"] = d["is_entry"]
	}
	if patched, err := json.Marshal(wfObjs); err == nil {
		wfJSON = patched
	}

	backendsJSON, err := json.Marshal(backends)
	if err != nil {
		return nil, err
	}

	assembly := ir.Assembly{
		Name:            unit,
		EntryPath:       filepath.Base(entryPath),
		Modules:         modules,
		CompilerName:    "hermes-parse",
		CompilerVersion: semver,
		ParserABI:       1,
		LanguageVersion: 1,
		TypeModule:      0,
		BackendsJSON:    backendsJSON,
		FunctionsJSON:   functionsJSON,
		Requires:        requires,
		ToolsDecl:       marshalToolsDecl(toolsDecl),
	}

	// 一个 unit 可以有多个 workflow（D-037）。装配层仍按"一个 workflow 一份 IR"
	// 工作，所以这里**逐个装配再合并**：比把装配层改成收数组风险小得多，
	// 而且每个 workflow 的校验路径与以前完全一致。
	var wfItems []json.RawMessage
	if err := json.Unmarshal(wfJSON, &wfItems); err != nil {
		// 单对象（旧产物 / golden）仍按一份处理。
		return ir.AssembleUnitWithTypes(assembly, wfJSON, typesJSON)
	}
	if len(wfItems) == 0 {
		return nil, fmt.Errorf("%s 里没有 workflow", entryPath)
	}

	base, err := ir.AssembleUnitWithTypes(assembly, wfItems[0], typesJSON)
	if err != nil {
		return nil, err
	}
	for _, item := range wfItems[1:] {
		more, err := ir.AssembleUnitWithTypes(assembly, item, typesJSON)
		if err != nil {
			return nil, err
		}
		if len(more.Workflows) != 1 {
			return nil, fmt.Errorf("装配 %s 时得到的 workflow 数不是 1", entryPath)
		}
		base.Workflows = append(base.Workflows, more.Workflows[0])
		base.Exports.Workflows = append(base.Exports.Workflows, more.Exports.Workflows...)
	}
	// entry 只能有一个：它是 `serve` 与 `run` 的默认入口，多个就没有默认可言。
	entries := 0
	for i := range base.Workflows {
		if base.Workflows[i].IsEntry {
			entries++
		}
	}
	if entries > 1 {
		return nil, fmt.Errorf("%s: 一个 unit 只能有一个 entry workflow（发现 %d 个）", entryPath, entries)
	}
	return base, nil
}

// firstError 把诊断里第一条 error 变成 error。include 解析的失败是**用户源码的问题**，
// 必须能渲染成人话，而不是"内部错误"。
func firstError(ds []diag.Diag) error {
	for _, d := range ds {
		if d.Severity == diag.SeverityError {
			return fmt.Errorf("%s: %s", d.Code, d.Message)
		}
	}
	return nil
}

// truthy 把"可能是 nil / false / true"的 JSON 值收敛成 bool。
func truthy(v any) bool {
	b, ok := v.(bool)
	return ok && b
}

// marshalToolsDecl 把收集到的 `tool` 声明重新编码成 JSON 交给装配。
//
// 这里不做解析：装配层才知道 tool 的完整结构（含 url_template 的 Path 形状），
// 在这边解一遍再编一遍只会多一处可能与结构体漂移的代码。
func marshalToolsDecl(decls []map[string]any) []byte {
	if len(decls) == 0 {
		return nil
	}
	b, err := json.Marshal(decls)
	if err != nil {
		return nil
	}
	return b
}

// renderDiags 把原生层返回的诊断 JSON 渲染成 Rust 风格的文本。
//
// 为什么不能直接打印 JSON：JSON 是**给机器**的（LSP、CI 解析），
// 而人在终端里要的是"哪一行、指到哪个字符、为什么、怎么改"。
// 同一份数据，两种呈现，不能混。
func renderDiags(diagJSON string, src []byte) string {
	// 原生层的诊断是**扁平**的（line/col 在外层），和 diag.Diag 的嵌套 Loc
	// 不是同一个形状 —— 直接用 diag.Diag 解会静默失败并回退到 JSON。
	type nativeDiag struct {
		Severity string   `json:"severity"`
		Code     string   `json:"code"`
		Message  string   `json:"message"`
		Line     int      `json:"line"`
		Col      int      `json:"col"`
		EndLine  int      `json:"end_line"`
		EndCol   int      `json:"end_col"`
		Module   string   `json:"module"`
		Why      string   `json:"why"`
		Fix      []string `json:"fix"`
	}
	var raw []nativeDiag
	if err := json.Unmarshal([]byte(diagJSON), &raw); err != nil {
		return diagJSON // 解不动就原样吐出来：至少没丢信息
	}
	ds := make([]diag.Diag, 0, len(raw))
	for _, d := range raw {
		ds = append(ds, diag.Diag{
			Severity: diag.SeverityError,
			Code:     d.Code,
			Message:  d.Message,
			Loc:      diag.Loc{Module: d.Module, Line: d.Line, Col: d.Col, EndCol: d.EndCol},
			Why:      d.Why,
			Fix:      d.Fix,
		})
	}
	for i := range ds {
		if ds[i].Snippet == "" {
			ds[i].Snippet = diag.Snippet(src, raw[i].Line, raw[i].Col, raw[i].EndLine, raw[i].EndCol)
		}
	}
	return diag.RenderAll(ds)
}
