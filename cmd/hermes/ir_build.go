package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

// buildWarned 记录本轮 buildIRWith 是否打印过 warning，供 cmdBuild 的
// --deny-warnings 判定：警告已渲染给用户，但"构建算不算失败"由调用方决定。
// CLI 单线程执行、测试无 t.Parallel，故无需同步；每次进入 buildIRWith 重置。
var buildWarned bool

func buildIRWith(entryPath string, src []byte, libs *libSearch) (*ir.Doc, error) {
	buildWarned = false
	// seenDiag 记这一趟构建里**已经渲染过**的诊断，用来合并跨趟重复。
	// 同一次构建会对同一个模块跑 5~6 个前端入口，每个入口都把整袋诊断交回来
	// （模块级的 W2003 / W5004 因此会各出好几份）—— 详见 dedupeDiags。
	// 作用域刻意是**每次构建一份**：跨构建共享会让第二次编译静默吞掉诊断。
	seenDiag := make(map[string]bool, 16)
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
		switch l.Kind {
		case "std":
			// `Link.Module` 是 **`std/net`**（带 `std/` 前缀），而 std.Source 要的是
			// 模块名 `net`。少剥这一层，requires 会静默为空 —— 而"空"看起来
			// 和"这个 unit 没有 #link"一模一样。
			mod := strings.TrimPrefix(l.Module, "std/")
			var sum []byte
			if src, ok := std.Source(mod); ok {
				h := sha256.Sum256(src)
				sum = h[:]
			}
			if sum == nil {
				fmt.Fprintf(os.Stderr, "hermes ir: warning: std dependency %s could not be resolved (builtin module missing)\n", l.Spec)
				continue
			}
			requires = append(requires, ir.RequireItem{
				Spec: l.Spec, Kind: "std",
				ABIVersion: archive.ABIVersionSupported, IRVersion: ir.IRVersionSupported,
				Origin: "source", MemberSHA256: hex.EncodeToString(sum),
			})
		case "local":
			sum, ok := libs.resolve(l.Module)
			if !ok {
				// 本地库：编译期拿不到实现字节，而 IR.md §9.5 要求非 implicit 的
				// requires 必须给 member_sha256 —— 所以这里**不能编一个假的哈希**，
				// 只能不写进 requires[]。
				//
				// 但"不写"必须是**可见**的：静默丢掉一条用户声明的依赖，会让
				// 供应链校验以为"这个 unit 什么都不依赖"。所以发一条 warning
				// 并说清补救方式（link 时用 -l 指定，链接器才有字节）。
				msg := fmt.Sprintf("local dependency %s could not be resolved: it will be omitted from requires[], "+
					"the artifact will be incomplete on the distribution chain (implementation bytes are unavailable at compile time). Use -L <dir> to point to the library directory.", l.Spec)
				if libs != nil && libs.strict {
					return nil, fmt.Errorf("%s\n"+
						"  to build without implementation bytes anyway, add --no-strict-link", msg)
				}
				fmt.Fprintf(os.Stderr, "hermes ir: warning: %s\n", msg)
				continue
			}
			hexSum, _ := hex.DecodeString(sum)
			requires = append(requires, ir.RequireItem{
				Spec: l.Spec, Kind: "local",
				ABIVersion: archive.ABIVersionSupported, IRVersion: ir.IRVersionSupported,
				Origin: "source", MemberSHA256: hex.EncodeToString(hexSum),
			})
		case "native":
			// 反向融合（extern / #link native / call_native）：声明"我依赖一份 C 归档"，
			// 由头文件（Header）给出契约，产物名（Archive，如 libmath.a）与名字
			// （Name，通常等于归档名）用于链接期解析。C 归档不是 .hbc，编译期
			// 拿不到 member_sha256 —— 它的字节完整性由 S2 的 cgo 桥在链接期校验
			// （link.VerifyRequires 对 kind=="native" 跳过 member_sha256 检查）。
			requires = append(requires, ir.RequireItem{
				// IRVersion / ABIVersion 必须是常量而不是裸数字：唯一事实源是
				// archive.ABIVersionSupported / ir.IRVersionSupported。
				Spec:       l.Spec,
				Kind:       "native",
				Name:       l.Archive,
				Archive:    l.Archive,
				Header:     l.Header,
				ABIVersion: archive.ABIVersionSupported,
				IRVersion:  ir.IRVersionSupported,
				Origin:     "source",
			})
		default:
			fmt.Fprintf(os.Stderr, "hermes ir: warning: unknown #link kind %q (%s), ignored\n", l.Kind, l.Spec)
		}
	}

	// 每个模块各出一张类型表，按 include 到达顺序合并。顺序即 id 顺序（IR.md §3.1）。
	merged := make([]map[string]any, 0, 16)
	_ = merged
	exportedDecls := map[string]map[string]any{}
	// 跨文件 extern 声明行，按符号名去重。见文件末尾 ext 拼接处的说明：
	// 这些行经 ext_types 文本通道喂给 C++ 前端，在编译期静态注入入口 AST。
	externLines := map[string]string{}
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
			return nil, fmt.Errorf("%s: failed to generate the type table:\n%s", m.Path, tdiag)
		}
		var ts []map[string]any
		if err := json.Unmarshal([]byte(js), &ts); err != nil {
			return nil, fmt.Errorf("%s: type table is not valid JSON: %w", m.Path, err)
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
			return nil, fmt.Errorf("%s: failed to generate workflow declarations:\n%s", m.Path, wdiag)
		}
		bjs, bdiag, err := native.CodegenBackends(m.Source, filepath.Base(m.Path), m.Kind == "h")
		if err != nil {
			return nil, err
		}
		if i != 0 && bdiag != "" && bdiag != "[]" {
			return nil, fmt.Errorf("%s: failed to generate the backend table:\n%s", m.Path, bdiag)
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
			return nil, fmt.Errorf("%s: failed to generate tool declarations:\n%s", m.Path, tdiag)
		}
		var tds []map[string]any
		if err := json.Unmarshal([]byte(tjs), &tds); err == nil {
			toolsDecl = append(toolsDecl, tds...)
		}

		var wds []map[string]any
		if err := json.Unmarshal([]byte(wjs), &wds); err == nil {
			for _, w := range wds {
				// codegen_wfdecls 追加的 extern 项**故意不带 name**：这里若先按
				// name 逻辑走，一个 extern 符号会被误收成一条 workflow 导出标注。
				// 按 sym 去重：入口模块自己也带 extern 声明，会与 .h 里的重复。
				if truthy(w["extern"]) {
					sym, _ := w["sym"].(string)
					line, _ := w["ext_line"].(string)
					if sym != "" && line != "" {
						externLines[sym] = line
					}
					continue
				}
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

	// 把收集到的跨文件 extern 签名也拼进同一张声明表，格式：
	//   extern\t<sym>\t<w|f>\t<ret>\t<t1>,<t2>,...
	// C++ 侧 check_type.cpp::load_externals_ 会跳过这些行（否则会被误建成
	// 名为 "extern" 的伪 struct），api.cpp::inject_extern_decls 则在 parse
	// 之后把它们静态还原成 AST 声明节点 —— 于是入口 AST 的 extern_fns_
	// 能看到写在 .h 里的符号，call_native 才编得出来。
	//
	// **顺序必须确定**：按 sym 字典序输出。map 的遍历顺序是随机的，而注入
	// 顺序会进入入口 AST 的 child 链、进而影响 extern_fns_ / extern_table_
	// 的建成顺序与诊断顺序；同一份源码两次编译必须产出逐字节相同的 IR
	// （golden 比对与 unit sha256 都依赖这一点）。CodegenFunctions 与
	// CodegenWithExternals 都会收到这份 ext：函数体里调跨文件 extern 同样要能编。
	syms := make([]string, 0, len(externLines))
	for sym := range externLines {
		syms = append(syms, sym)
	}
	sort.Strings(syms)
	for _, sym := range syms {
		ext.WriteString(externLines[sym])
		ext.WriteByte('\n')
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
			//
			// 标签必须准确：这一趟（CodegenFunctions）产出的诊断**不一定**是
			// "函数编译失败" —— 一个普通 workflow 体里的 E2015/E4024 也走这条路。
			// 把一条 body 错误标成 "functions failed to compile" 会让人去翻
			// 函数表，而错误根本不在那里。所以按诊断自己报的位置起标题。
			if nativeHasError(fdiag) {
				return nil, fmt.Errorf("%s: %s:\n%s",
					m.Path, nativeDiagLabel(fdiag), renderDiagsColored(fdiag, m.Source, seenDiag))
			}
			// 只有警告：渲染成人读的文本打到 stderr，继续构建（退出码 0）。
			// 记下"本轮出过警告"，--deny-warnings 时由 cmdBuild 据此判失败。
			buildWarned = true
			fmt.Fprint(os.Stderr, renderDiagsColored(fdiag, m.Source, seenDiag))
		}
		var fs []map[string]any
		if err := json.Unmarshal([]byte(fjs), &fs); err != nil {
			return nil, fmt.Errorf("%s: function table is not valid JSON: %w", m.Path, err)
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
	//
	// 警告也**必须渲染**，不能 `fmt.Fprintf(os.Stderr, "hermes: %s\n", wf.Diag)`：
	// 那样用户看到的是一整行原始 JSON（前端探针的输出就是这样），没有
	// 行列、没有源码片段、没有 why/help —— 而 W2004（max_steps = unlimited）
	// 恰恰是一条**必须被人读懂**的警告：它说明两个保证随 unlimited 一起消失。
	// 退出码仍然是 0：警告不阻塞。
	if wf.Diag != "" && wf.Diag != "[]" {
		if nativeHasError(wf.Diag) {
			return nil, fmt.Errorf("%s: workflow failed to compile:\n%s", entryPath, renderDiagsColored(wf.Diag, src, seenDiag))
		}
		buildWarned = true
		fmt.Fprint(os.Stderr, renderDiagsColored(wf.Diag, src, seenDiag))
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
		return nil, fmt.Errorf("workflows is not valid JSON: %w", err)
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
		// 2.0.0：语言版本与 ir_version 同轴推进（见 ir.IRVersionSupported）。
		LanguageVersion: ir.IRVersionSupported,
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
		return nil, fmt.Errorf("%s: no workflow found", entryPath)
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
			return nil, fmt.Errorf("assembling %s produced a workflow count other than 1", entryPath)
		}
		base.Workflows = append(base.Workflows, more.Workflows[0])
		base.Exports.Workflows = append(base.Exports.Workflows, more.Exports.Workflows...)
		// imports[] 来自各 workflow 的 call_native 节点，逐个装配后合并（按
		// kind:sym 去重，否则同一 extern 在多个 workflow 里被调用会重复）。
		base.Imports = mergeImports(base.Imports, more.Imports)
	}
	// entry 只能有一个：它是 `serve` 与 `run` 的默认入口，多个就没有默认可言。
	entries := 0
	for i := range base.Workflows {
		if base.Workflows[i].IsEntry {
			entries++
		}
	}
	if entries > 1 {
		return nil, fmt.Errorf("%s: a unit can only have one entry workflow (found %d)", entryPath, entries)
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

// mergeImports 合并多个 workflow 装配出的 imports[]，按 kind:sym 去重。
//
// 一个 unit 可以有多个 workflow（D-037），每个 workflow 各自扫自己的 call_native
// 节点生成 imports[]。同一个 native 符号在多个 workflow 里被调用时，合并后只应
// 出现一次——否则 imports[] 会随 workflow 数线性膨胀，且重复项会让链接期对账
// 的"已解析"判断变得不可信。
func mergeImports(a, b []ir.ImportRef) []ir.ImportRef {
	seen := map[string]bool{}
	out := make([]ir.ImportRef, 0, len(a)+len(b))
	for _, im := range append(a, b...) {
		key := im.Kind + ":" + im.Sym
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, im)
	}
	return out
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

// nativeDiag 是 C++ 前端诊断的**扁平**形状（line/col 在外层），和 diag.Diag 的
// 嵌套 Loc 不是同一个形状 —— 直接用 diag.Diag 解会静默失败并回退到 JSON。
//
// Notes 是 `[{"module","line","col","message"}]`：W2004（max_steps = unlimited）
// 就带一条 note 指回 "unlimited 写在哪"。丢掉它等于把"这条警告指到哪一行"
// 这件事扔掉一半。
type nativeDiag struct {
	Severity string      `json:"severity"`
	Code     string      `json:"code"`
	Message  string      `json:"message"`
	Line     int         `json:"line"`
	Col      int         `json:"col"`
	EndLine  int         `json:"end_line"`
	EndCol   int         `json:"end_col"`
	Module   string      `json:"module"`
	Why      string      `json:"why"`
	Fix      []string    `json:"fix"`
	Notes    []nativeNot `json:"notes"`
	// Edits 是 M2 的机器可应用修正编辑（可选；C++ 仅在 edit_replacement 非空时输出）。
	// JSON 形状：[{"span":{"line","col","end_line","end_col"},"replacement":"..."}]。
	Edits []nativeEdit `json:"edits"`
}

type nativeNot struct {
	Module  string `json:"module"`
	Line    int    `json:"line"`
	Col     int    `json:"col"`
	Message string `json:"message"`
}

// nativeEdit / nativeSpan 对齐 C++ diag_to_json 输出的 edits[] 形状，
// 仅用于反序列化；真正落到 diag.Diag 的是 internal/diag 的 Edit（见 renderDiags）。
type nativeEdit struct {
	Span        nativeSpan `json:"span"`
	Replacement string     `json:"replacement"`
}

type nativeSpan struct {
	Line    int `json:"line"`
	Col     int `json:"col"`
	EndLine int `json:"end_line"`
	EndCol  int `json:"end_col"`
}

// parseNativeDiags 解析原生诊断 JSON。ok=false 表示"解不动"：调用方那时只能把
// 原文吐出来（至少没丢信息），而不能当成"没有诊断"。
func parseNativeDiags(diagJSON string) (ds []nativeDiag, ok bool) {
	if diagJSON == "" || diagJSON == "[]" {
		return nil, true
	}
	if err := json.Unmarshal([]byte(diagJSON), &ds); err != nil {
		return nil, false
	}
	return ds, true
}

// nativeSeverity 把原生层的 severity 字符串**如实**翻成 diag.Severity。
//
// 为什么必须如实：这里的产物最终会渲染成 `error[...]` / `warning[...]`。
// 曾经这里对所有原生诊断硬写 SeverityError（注释断言"原生层的诊断是扁平的"——
// 那是在说**形状**，却被当成了"没有严重级别"），于是 W2004 一出现在 error
// 路径上就显示成 `error[W2004]`：一条警告被渲染成错误，而这门语言对
// "警告与错误的区别"是有承诺的（警告不阻塞、退出码 0）。
//
// 未知/空值 → error：**保守方向是"宁可当成错误"**，因为把它当成 warning 会让
// 一条本该阻塞的诊断悄悄放行。正常产物里不会走到这里（前端只产出这三种），
// 所以这条兜底是给"前端新增了一种级别"留的显式痕迹，而不是日常路径。
func nativeSeverity(s string) diag.Severity {
	switch s {
	case "warning":
		return diag.SeverityWarning
	case "note":
		return diag.SeverityNote
	case "error", "":
		return diag.SeverityError
	}
	return diag.SeverityError
}

// nativeHasError 报告这批原生诊断里有没有 error。
//
// **解析后按字段判**，不再在 JSON 文本里搜 `"severity":"error"` 子串：子串
// 搜索对空格（`"severity": "error"`）、字段顺序、以及"message 正文里恰好
// 提到这句话"都会误判 —— 而误判的方向是把一条警告当成错误、直接失败。
func nativeHasError(diagJSON string) bool {
	ds, ok := parseNativeDiags(diagJSON)
	if !ok {
		// 解不动时按"有错误"处理：宁可多报一次让用户贴出来，也不要拿一份
		// 读不懂的诊断继续构建。
		return true
	}
	for _, d := range ds {
		if nativeSeverity(d.Severity) == diag.SeverityError {
			return true
		}
	}
	return false
}

// nativeDiagLabel 给"这趟编译为什么失败"起一个**准确**的标题。
//
// 以前这里是硬编码的 `functions failed to compile:`，而 CodegenFunctions 这一趟
// 的诊断**不一定**来自函数：一个普通 workflow 体里的 `retry(unlimited)`（E2015）
// 或 `auto x = unlimited;`（E4024）也走这条路。于是用户看到
// "functions failed to compile"，去翻自己写的函数 —— 而错误在 body 里，
// 一句话就把人指错了方向。
//
// 这里只承诺一件可验证的事：**哪些错误码**出现在这个模块里。要指名"哪个函数"
// 需要把行号映射回源码结构，而 nativeDiag 只给 line/col —— 猜一个名字比不给
// 标题更糟（一个错的定位会被当成事实）。
func nativeDiagLabel(diagJSON string) string {
	ds, ok := parseNativeDiags(diagJSON)
	if !ok {
		return "diagnostics"
	}
	var codes []string
	seen := map[string]bool{}
	for _, d := range ds {
		if nativeSeverity(d.Severity) != diag.SeverityError || d.Code == "" {
			continue
		}
		if seen[d.Code] {
			continue
		}
		seen[d.Code] = true
		codes = append(codes, d.Code)
	}
	if len(codes) == 0 {
		return "source errors"
	}
	return "source errors (" + strings.Join(codes, ", ") + ")"
}

// diagMaxShown / diagStopAtFirstError 控制诊断渲染的截断，**只由 hermes build 设置**：
//   - 默认（build 未加 --keep-going）：diagStopAtFirstError=true，渲染到第一条 error 即停
//     （warnings 若排在 error 之前仍会显示，绝不把 error 藏起来）；
//   - --keep-going：diagMaxShown=50，最多渲染 50 条再停（防噪声洪水）；
//   - 其余命令（ir / compile / demo）与直接调 renderDiags 的测试保持 (0,false)=不截断，
//     与旧行为逐字节一致。
//
// cmdBuild 用 defer 恢复默认值，避免污染后续命令（CLI 单线程、测试无 t.Parallel）。
var (
	diagMaxShown         int
	diagStopAtFirstError bool
)

// truncateDiags 按上面的口径截断诊断，返回截断后的切片与被抑制的条数。
func truncateDiags(ds []diag.Diag) ([]diag.Diag, int) {
	if diagStopAtFirstError {
		for i, d := range ds {
			if d.Severity == diag.SeverityError {
				if i+1 < len(ds) {
					return ds[:i+1], len(ds) - (i + 1)
				}
				return ds, 0
			}
		}
		return ds, 0
	}
	if diagMaxShown > 0 && len(ds) > diagMaxShown {
		return ds[:diagMaxShown], len(ds) - diagMaxShown
	}
	return ds, 0
}

// suppressedNote 生成「还有 N 条未显示」的提示行（N<=0 时为空）。
//
// 提示语随当前口径变：默认截断时劝人加 --keep-going；已经在 --keep-going 里
// 还超了 50 条，就不能再劝同一句话（那是在教人重跑一个已经开着的旗标）。
func suppressedNote(n int) string {
	if n <= 0 {
		return ""
	}
	if diagStopAtFirstError {
		return fmt.Sprintf("\n... %d more diagnostic(s) suppressed (re-run with --keep-going to see up to 50)\n", n)
	}
	return fmt.Sprintf("\n... %d more diagnostic(s) suppressed (--keep-going shows at most 50)\n", n)
}

// nativeDiagsToDiag 把前端返回的诊断 JSON 转成 []diag.Diag，并补上源码片段。
//
// 纯文本与彩色两条渲染路径以前**各抄了一份**这段 40 行的转换，改一处忘一处的
// 风险是结构性的（比如 M2 的结构化编辑只加在一条上，另一条就静默丢字段）。
// 收成唯一一份之后，"哪些字段会被搬到终端"只有一个答案。
//
// ok=false 表示 JSON 解不动 —— 调用方原样吐出 JSON，至少不丢信息。
func nativeDiagsToDiag(diagJSON string, src []byte) ([]diag.Diag, bool) {
	raw, ok := parseNativeDiags(diagJSON)
	if !ok {
		return nil, false
	}
	ds := make([]diag.Diag, 0, len(raw))
	for _, d := range raw {
		out := diag.Diag{
			Severity: nativeSeverity(d.Severity),
			Code:     d.Code,
			Message:  d.Message,
			Loc:      diag.Loc{Module: d.Module, Line: d.Line, Col: d.Col, EndCol: d.EndCol},
			Why:      d.Why,
			Fix:      d.Fix,
			Notes:    make([]diag.Note, 0, len(d.Notes)),
		}
		for _, n := range d.Notes {
			out.Notes = append(out.Notes, diag.Note{
				Loc:     diag.Loc{Module: n.Module, Line: n.Line, Col: n.Col},
				Message: n.Message,
			})
		}
		// M2：把原生层的结构化编辑搬进 diag.Diag.Edits（机器可应用修正提示）。
		for _, e := range d.Edits {
			out.Edits = append(out.Edits, diag.Edit{
				Loc: diag.Loc{
					Module:  d.Module,
					Line:    e.Span.Line,
					Col:     e.Span.Col,
					EndLine: e.Span.EndLine,
					EndCol:  e.Span.EndCol,
				},
				Replacement: e.Replacement,
			})
		}
		ds = append(ds, out)
	}
	for i := range ds {
		if ds[i].Snippet == "" {
			ds[i].Snippet = diag.Snippet(src, raw[i].Line, raw[i].Col, raw[i].EndLine, raw[i].EndCol)
		}
	}
	return ds, true
}

// dedupeDiags 按"逐字相同"合并诊断，返回保留的切片与被合并掉的条数。
// seen 非 nil 时**跨调用累积**，因此同一次构建里的多个渲染调用共享一张表。
//
// 为什么需要它（这不是为了好看，是因为同一个模块会被解析好几遍）：
// 前端每个 C 入口（hm_codegen_types / _wfdecls / _backends / _tools /
// _functions / _ext）都完整跑一遍 lex + parse + check_types + check_idempotency
// （见 api.cpp::run_parse），而每个入口都把**整袋**诊断交回来 —— 不只是自己那一趟
// 产出的。于是**模块级**诊断（lexer 的 W2003、type_of_ 里的 W5004）会随每一趟
// 各出一份，用户在终端看到同一条警告印两遍。
//
// 为什么修在这里而不是前端：每个 C 入口各建一个 DiagBag，跨袋去重在前端
// **做不到**（没有共享状态）；Go 侧是各趟唯一汇合的地方。也不能只保留某一趟的
// 诊断 —— 各趟的 codegen 诊断是**互补**的（函数体的错只从 _functions 来，
// workflow 体的错只从 _ext 来），只能取并集再去重。
//
// 为什么可以按"逐字相同"合并：key 含模块名 + 行列区间 + 严重级 + 码 + 消息，
// 一条源码构造在 AST 里位置唯一，两条这样的诊断只可能是同一件事说了两遍。
// 合并**不丢信息**（字节完全相同），所以不计数、不打印"已合并 N 条"—— 那是噪声。
func dedupeDiags(ds []diag.Diag, seen map[string]bool) ([]diag.Diag, int) {
	if seen == nil {
		seen = make(map[string]bool, len(ds))
	}
	out := ds[:0]
	for _, d := range ds {
		// 刻意不把 Notes / Edits 放进 key：它们不是身份，是同一件事的附件。
		key := fmt.Sprintf("%s\x00%s\x00%s\x00%d:%d-%d:%d\x00%s",
			d.Severity, d.Code, d.Loc.Module,
			d.Loc.Line, d.Loc.Col, d.Loc.EndLine, d.Loc.EndCol, d.Message)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, d)
	}
	return out, len(ds) - len(out)
}

// renderDiagSlice 是两条渲染路径的公共尾部：去重 → 截断 → 渲染 → 补"还有 N 条"。
//
// 顺序不能换：去重必须在截断之前，否则 50 条的上限会被重复项吃掉。
func renderDiagSlice(ds []diag.Diag, seen map[string]bool, colored bool) string {
	ds, _ = dedupeDiags(ds, seen)
	ds, n := truncateDiags(ds)
	var body string
	if colored {
		body = diag.RenderAllColored(ds, diagColor, os.Stderr)
	} else {
		body = diag.RenderAll(ds)
	}
	return body + suppressedNote(n)
}

// renderDiags 把原生层返回的诊断 JSON 渲染成 Rust 风格的文本。
//
// 为什么不能直接打印 JSON：JSON 是**给机器**的（LSP、CI 解析），
// 而人在终端里要的是"哪一行、指到哪个字符、为什么、怎么改"。
// 同一份数据，两种呈现，不能混。
func renderDiags(diagJSON string, src []byte) string {
	ds, ok := nativeDiagsToDiag(diagJSON, src)
	if !ok {
		return diagJSON // 解不动就原样吐出来：至少没丢信息
	}
	return renderDiagSlice(ds, nil, false)
}

// diagColor 控制诊断是否上色，由 --color 标志设置（默认 auto：仅在交互终端且
// 未设 NO_COLOR 时上色）。包级变量，供 ir / compile / run 三个子命令共享。
var diagColor = diag.ColorAuto

// renderDiagsColored 与 renderDiags 同输出，但当标准错误是交互终端（且未设
// NO_COLOR）时按严重程度给诊断上色，层次与 rustc 一致。颜色关闭时逐字节等于
// renderDiags，故既有测试（直接调 renderDiags）不受影响。
//
// seen 由调用方持有并在**一次构建内**共享，用来合并跨趟重复的模块级诊断
// （见 dedupeDiags 的说明）。传 nil 就是"只看这一趟"。
func renderDiagsColored(diagJSON string, src []byte, seen map[string]bool) string {
	ds, ok := nativeDiagsToDiag(diagJSON, src)
	if !ok {
		return diagJSON
	}
	return renderDiagSlice(ds, seen, true)
}
