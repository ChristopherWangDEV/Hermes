package ir

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"hermes/internal/canon"
	"hermes/internal/diag"
)

// Assembly 是一次 unit 装配需要的全部**非 AST** 事实。
//
// 为什么装配在 Go 而不是 C++：`unit.modules[].sha256` 要读文件字节，而 C++ 前端
// 被定义为无 IO（LANGUAGE §1.2）。所以分工是"谁看得见谁负责"：C++ 出节点图，
// Go 出模块表、类型表、符号表与最终字节。这条分工写在 D-027 的推论里。
type Assembly struct {
	Name      string
	EntryPath string
	// Modules 按 include 到达顺序；下标即 loc.m。
	Modules []Module
	// 编译器信息：进 IR 是为了让上界/诊断可追溯到"谁编的"，但不含时间戳。
	CompilerName    string
	CompilerVersion string
	ParserABI       int
	LanguageVersion int
	// Requires 是模块级依赖（来自 #link），由调用方算好 member_sha256。
	Requires []RequireItem
	// ToolsDecl 是模块级 `tool` 声明（D-034）的 JSON 数组。
	ToolsDecl []byte
	// BackendsJSON 是各模块 backend 声明/定义合并后的 JSON（Go 侧合）。
	BackendsJSON []byte
	// FunctionsJSON 是各模块函数表合并后的 JSON（Go 侧合）。
	// 为空表示 unit 里没有函数 —— 此时 Doc.Functions 必须**不出现**，
	// 否则 1.x 的 golden 字节会变。
	FunctionsJSON []byte
	// TypeModule 是这些类型声明所在的模块下标（这一刀：单模块，恒为 0）。
	// 跨文件时每模块各调一次 codegen_types，各自带上自己的下标。
	TypeModule int
}

// rawType / rawField 是 C++ 侧类型表的**线上形状**。
// 类型引用带名字（ref）而不是 id：id 是"unit 内声明顺序"的产物，
// 只有装配阶段（看得见全部模块）才能分配。
type rawType struct {
	Name     string     `json:"name"`
	Exported bool       `json:"exported"`
	Loc      Loc        `json:"loc"`
	Fields   []rawField `json:"fields"`
	// Module 是该类型**声明所在的模块下标**。跨文件装配时由调用方写入；
	// 单模块时可省略（回落到 Assembly.TypeModule）。
	Module *int `json:"module,omitempty"`
}

type rawField struct {
	N   string       `json:"n"`
	T   rawTypeExpr  `json:"t"`
	Opt bool         `json:"opt"`
	Ann []Annotation `json:"ann"`
}

type rawTypeExpr struct {
	K   string          `json:"k"`
	ID  *int            `json:"id,omitempty"`
	Ref string          `json:"ref,omitempty"`
	Of  json.RawMessage `json:"of,omitempty"`
}

// AssembleUnit 把 C++ codegen 产出的 workflow 对象装配成一份完整 IR。
//
// 装配完**立刻跑一遍 Validate**：自举信任从零开始（IR.md §12 结尾）。我们刚产出的
// IR 必须过和读第三方一样的检查，否则"编译器产出了坏 IR"这件事要等到别人加载时
// 才暴露，而那时现场只有一份看起来正常、却在某处悬空的 JSON。
func AssembleUnit(a Assembly, workflowJSON []byte) (*Doc, error) {
	return AssembleUnitWithTypes(a, workflowJSON, nil)
}

// AssembleUnitWithTypes 额外接收一张类型表（来自 hm_codegen_types）。
//
// 为什么类型表是单独一口：类型来自 `.h` 模块，而 workflow 只来自 `.hermes`。
// 一次调用里既解析入口又解析全部头文件，等于把 include 图塞进 codegen —— 而
// 图解析在 Go（internal/include），前端被定义为无 IO。
func AssembleUnitWithTypes(a Assembly, workflowJSON, typesJSON []byte) (*Doc, error) {
	if len(a.Modules) == 0 {
		return nil, fmt.Errorf("ir.AssembleUnit: no module table (the entry must at least list itself)")
	}

	// 先装配类型与 schema：workflow 里的 schema 引用要按名字解析成下标，
	// 所以表必须先在手里。顺序反过来就是"引用一张还没建好的表"。
	backends, err := resolveBackends(a.BackendsJSON)
	if err != nil {
		return nil, err
	}
	types, err := resolveTypes(typesJSON, a.TypeModule)
	if err != nil {
		return nil, err
	}
	schemas, err := BuildSchemas(types)
	if err != nil {
		return nil, err
	}

	wfJSON, tools, err := extractTools(workflowJSON)
	if err != nil {
		return nil, err
	}
	// 模块级 `tool` 声明（D-034）与 call_tool 提升出来的工具合并进同一张表。
	// 名称冲突必须报错：同名两定义会让"模型调的是哪个"变成运气。
	declared, err := parseDeclaredTools(a.ToolsDecl)
	if err != nil {
		return nil, err
	}
	typeIDs := make(map[string]int, len(types))
	for i := range types {
		typeIDs[types[i].Name] = types[i].ID
	}
	// 函数签名里的 struct 名要在这里换成 id（与 types/call_tool.ret 同一条分工）。
	funcs, err := resolveFunctions(a.FunctionsJSON, typeIDs)
	if err != nil {
		return nil, err
	}
	wfJSON, err = resolveSchemaRefs(wfJSON, schemas, typeIDs)
	if err != nil {
		return nil, err
	}

	var wf Workflow
	dec := json.NewDecoder(bytes.NewReader(wfJSON))
	dec.UseNumber()
	if err := dec.Decode(&wf); err != nil {
		return nil, fmt.Errorf("ir.AssembleUnit: workflow from codegen is not valid JSON: %w", err)
	}

	// codegen 只给"有哪些变量"，不给 decl —— 因为 decl 是**节点 id**，而节点编号
	// 由 codegen 分配、由这里读回。让 codegen 再回填一次等于同一事实写两处，
	// 所以装配阶段按 decl 节点反查，查不到就是错的（不许留 -1 蒙混过关）。
	for i := range wf.Vars {
		if wf.Vars[i].Decl >= 0 {
			continue
		}
		// 用 boundVars 而不是"k == decl && node.Var"：解构（destructure）与
		// parallel.bind 也绑变量。只认 decl 会让 D-026 的两种读兼容形态
		// 在这里静默失败——而失败信息说的是"codegen 与装配对不上"，误导性极强。
		found := -1
		for ni := range wf.Nodes {
			ids, _ := boundVars(&wf.Nodes[ni])
			for _, id := range ids {
				if id == wf.Vars[i].ID {
					found = ni
					break
				}
			}
			if found >= 0 {
				break
			}
		}
		// workflow 的 input 参数由**调用方**绑定，没有 decl 节点。
		// 只认 decl 会让 `tool.call(header = input)` 这种合法写法在装配期报
		// "codegen 与装配对不上"——而那是个假的编译器 bug。
		// 按**名字**认这个变量，不按 id：input 的 id 由 codegen 分配，
		// 它不保证是 0（实测是 1），而这个判断一旦写错，报的还是"编译器 bug"。
		if found < 0 && wf.Vars[i].Name == "input" {
			// input 在**入口节点**处绑定（由调用方提供），没有 decl 节点。
			// decl 必须指向一个合法节点（校验器查这个），所以指向 root：
			// 它表达的就是"从根开始就可见"。
			wf.Vars[i].Decl = wf.Root
			continue
		}
		if found < 0 {
			return nil, fmt.Errorf(
				"ir.AssembleUnit: variable %q (id %d) has no decl node binding it: "+
					"codegen and assembly disagree (compiler bug)", wf.Vars[i].Name, wf.Vars[i].ID)
		}
		wf.Vars[i].Decl = found
	}

	d := &Doc{
		// 恒为 IRVersionSupported（2）。**不要**跟着 minor 升，见 types.go 的注释。
		IRVersion: IRVersionSupported,
		Unit: Unit{
			Name:            a.Name,
			EntryPath:       a.EntryPath,
			Modules:         a.Modules,
			LanguageVersion: a.LanguageVersion,
		},
		Compiler: Compiler{
			Name:      a.CompilerName,
			Version:   a.CompilerVersion,
			ParserABI: a.ParserABI,
		},
		Types:      types,
		Backends:   backends,
		Tools:      mergeTools(tools, declared),
		Schemas:    schemas,
		Functions:  funcs,
		Workflows:  []Workflow{wf},
		Imports:    importsForWorkflow(&wf),
		Requires:   a.Requires,
		DiagCounts: DiagCounts{},
	}

	// 符号表与各定义上的 exported 标量是同一事实的两份表示（V-9），
	// 所以这里必须**由同一个值**推出两份，不能各写一遍。
	if wf.Exported {
		d.Exports.Workflows = []string{wf.Name}
	} else {
		d.Exports.Workflows = []string{}
	}
	d.Exports.Types = exportedTypeNames(types)
	d.Exports.Backends = []string{}
	d.Exports.Tools = []string{}

	// workflow.backend 是**名字引用**，必须能在 backends[] 里找到。
	// 找不到就报错：空着跑会让运行期退化成"没有默认 backend"，
	// 而症状看起来像"配置没给"，不像"编译器丢了默认值"。
	if wf.Backend != "" {
		found := false
		for i := range backends {
			if backends[i].Name == wf.Backend {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf(
				"ir.AssembleUnit: workflow %q: default backend %q is not in backends[]"+
					" (declared in .h, defined in .hermes; missing either triggers this)", wf.Name, wf.Backend)
		}
	}

	if ds := Validate(d); len(ds) > 0 {
		// 有一条诊断是**用户能改的**（赋值给不可变变量），就不能报成"编译器 bug"：
		// 那会让用户以为是自己环境的问题，而实际上改一行 可变声明 就好了。
		for _, one := range ds {
			if one.Code == "E_IR_ASSIGN_IMMUTABLE" {
				return nil, fmt.Errorf("[ERROR] Cannot mutate const variable (line %d):\n  remove const, or declare a new variable",
					one.Loc.Line)
			}
		}
		return nil, fmt.Errorf("ir.AssembleUnit: IR just assembled failed validation (compiler bug, not an input problem):\n%s",
			renderDiags(ds))
	}
	return d, nil
}

// importsForWorkflow 从单个 workflow 的节点里抽出 `call_native` 调用，
// 为每个被调用的 native（C ABI）符号生成一条 imports[] 记录。
//
// 为什么从 call_native 节点抽而不是从 extern 声明抽：反向融合（extern / #link
// native / call_native）里，链接器只认"被实际调用的符号"（`internal/link/link.go`
// 的 native 解析同样扫 call_native），而未调用的 extern 声明不参与链接。从
// call_native 抽保证 imports[] 与链接期检查看的是同一份事实。符号去重：同一个
// 符号被多个节点调用只记一条。
//
// kind 恒为 "native"：extern 声明在 v1 只有 C ABI 一种后端（docs/EXTERN-C-ABI.md
// §1.2，extern 符号只从 `#link native` 的归档解析）。注意文档 §2 的 imports[] 示例
// 里写了 `"kind":"workflow"`，那是笔误——走 workflow 符号表会找不到 native 符号、
// 触发一个不该有的 E_LINK_UNRESOLVED，所以此处按语义用 "native"。
func importsForWorkflow(wf *Workflow) []ImportRef {
	seen := map[string]bool{}
	out := make([]ImportRef, 0)
	for i := range wf.Nodes {
		n := &wf.Nodes[i]
		if n.K == "call_native" && n.Fn != nil {
			if seen[*n.Fn] {
				continue
			}
			seen[*n.Fn] = true
			out = append(out, ImportRef{Sym: *n.Fn, Kind: "native", Loc: n.Loc})
		}
	}
	return out
}

// renderDiags 把诊断压成多行文本供 error 用。直接用 diag 的渲染器：
// 重新实现一遍只会得到"两套诊断长相"，而这两套迟早会说的不是一件事。
func renderDiags(ds []diag.Diag) string {
	var b bytes.Buffer
	for _, d := range ds {
		fmt.Fprintf(&b, "  %s: %s\n", d.Code, d.Message)
	}
	return b.String()
}

// resolveSchemaRefs 把 codegen 产出的 `schema_ref`（名字）换成 `schema`（下标）。
//
// 为什么不在 C++ 里直接算下标：codegen 只看得见一个文件，而 schemas[] 是
// **unit 级**的表（要跨 include 合并）。让前端猜下标，等于让它猜一张它看不见的表。
func resolveSchemaRefs(wfJSON []byte, schemas []SchemaDoc, typeIDs map[string]int) ([]byte, error) {
	if len(wfJSON) == 0 {
		return wfJSON, nil
	}
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(wfJSON))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("ir.AssembleUnit: workflow from codegen is not valid JSON: %w", err)
	}

	byName := make(map[string]int, len(schemas))
	for i, s := range schemas {
		byName[s.ID] = i
	}

	nodes, _ := doc["nodes"].([]any)
	for _, n := range nodes {
		m, ok := n.(map[string]any)
		if !ok {
			continue
		}
		ref, present := m["schema_ref"]
		if !present {
			continue
		}
		delete(m, "schema_ref")
		if ref == nil {
			m["schema"] = nil
			continue
		}
		name, _ := ref.(string)
		idx, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf(
				"ir.AssembleUnit: schema of node %v references undeclared type %q (typo?)",
				m["id"], name)
		}
		m["schema"] = idx
	}

	// call_tool.ret / call_native.ret 与类型表同理：codegen 只认识名字
	// （`<OrderInfo>` / `{"k":"struct","ref":"NetResp"}`），下标由装配阶段
	// 解析。builtin 类型原样保留。
	//
	// call_native 与 call_tool **同形**：两者的 struct 返回类型都由
	// codegen 的 fn_type_expr 写成 ref 形态，都要在这里换成 id —— 否则
	// 产物里会留一个 `ref` 键，而 schema 的 typeStruct 只认 `id`
	// （additionalProperties:false）。历史上这条分支只放行 call_tool，
	// @opaque struct 的 extern 返回（EXTERN-C-ABI.md §1.3 正例 1）就漏了过去。
	for _, n := range nodes {
		m, ok := n.(map[string]any)
		if !ok {
			continue
		}
		if m["k"] != "call_tool" && m["k"] != "call_native" {
			continue
		}
		ret, ok := m["ret"].(map[string]any)
		if !ok {
			continue
		}
		// 递归解析：`httpresult<OrderInfo>` 的 of 里还有一层类型名。
		// 只处理顶层会让 `of` 保持字符串形态，而校验器读的是 id ——
		// 报错会变成"找不到类型 httpresult"，把注意力引到错误的地方。
		//
		// v2.0.0：double 已回到公共白名单（唯一浮点类型；float 不再是类型名），
		// call_native 的 ret / arg_types 都能解析。但 `tool.call<double>` 仍必须
		// 拒：call_tool 的 T 必须是 JSON 结构类型，double 是标量 —— 这条守卫守的
		// 是「call_tool 的返回类型语义」，不是「double 不存在」。放行它会静默产出
		// call_tool.ret={"k":"double"}，emitExpr 不读 n.Ret，C 后端也照编不误，
		// 没有任何一层会拒。诊断文案保持与 C1 回归测试一致。
		if m["k"] == "call_tool" {
			if k, _ := ret["k"].(string); k == "double" {
				return nil, fmt.Errorf("ir.AssembleUnit: return type %q is undeclared (typo?)", k)
			}
		}
		resolved, err := resolveRetType(ret, typeIDs)
		if err != nil {
			return nil, err
		}
		m["ret"] = resolved

		// call_native.arg_types 与 ret **同形**：codegen 的 fn_type_expr 对
		// struct 形参同样写 `{"k":"struct","ref":"X"}`。C 后端要靠它生成
		// 编译期写死的 extern 原型，所以 struct 的 ref 也必须在装配层换成
		// id，否则产物里留一个 schema 的 typeStruct 不认的键。
		// 缺失 / 空数组都合法（无参 extern），类型断言失败必须报错而不是 panic。
		raw, present := m["arg_types"]
		if !present {
			continue
		}
		ats, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("ir.AssembleUnit: node %v arg_types is not an array", m["id"])
		}
		for i, item := range ats {
			tm, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("ir.AssembleUnit: node %v arg_types[%d] is not a type object", m["id"], i)
			}
			// arg_types 只属于 call_native，与 ret 同口径（公共白名单含 double）。
			rt, err := resolveRetType(tm, typeIDs)
			if err != nil {
				return nil, fmt.Errorf("ir.AssembleUnit: node %v arg_types[%d]: %w", m["id"], i, err)
			}
			ats[i] = rt
		}
		m["arg_types"] = ats
	}
	return json.Marshal(doc)
}

// resolveFunctions 把 C++ 函数表装配进 Doc.Functions。
//
// 做三件事：
//  1. 函数签名里的 `{"k":"struct","ref":"X"}` 换成 id —— 前端只看得见一个
//     文件，看不见 unit 级的类型表；
//  2. 局部变量的 decl 按绑定节点回填（参数例外：它们由调用方绑定，
//     decl 指向函数体根 seq，与 workflow.input 用 root 表示"从根可见"同义）；
//  3. 跨模块同名函数直接报错：同名两定义会让"调用的是哪个"变成运气。
func resolveFunctions(functionsJSON []byte, typeIDs map[string]int) ([]Function, error) {
	if len(functionsJSON) == 0 {
		return nil, nil
	}
	var fs []Function
	if err := json.Unmarshal(functionsJSON, &fs); err != nil {
		return nil, fmt.Errorf("ir.AssembleUnit: functions table is not valid JSON: %w", err)
	}
	if len(fs) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(fs))
	for i := range fs {
		f := &fs[i]
		if f.Name == "" {
			return nil, fmt.Errorf("ir.AssembleUnit: functions table contains an empty name")
		}
		if seen[f.Name] {
			return nil, fmt.Errorf("ir.AssembleUnit: function %q is defined twice (duplicate names across modules are not allowed)", f.Name)
		}
		seen[f.Name] = true

		paramNames := make(map[string]bool, len(f.Params))
		for pi := range f.Params {
			p := &f.Params[pi]
			if p.N == "" {
				return nil, fmt.Errorf("ir.AssembleUnit: function %q has an empty parameter name", f.Name)
			}
			if paramNames[p.N] {
				return nil, fmt.Errorf("ir.AssembleUnit: function %q has duplicate parameter %q", f.Name, p.N)
			}
			paramNames[p.N] = true
			if err := resolveFuncType(&p.T, typeIDs); err != nil {
				return nil, fmt.Errorf("ir.AssembleUnit: function %q parameter %q: %w", f.Name, p.N, err)
			}
		}
		if err := resolveFuncType(&f.Returns, typeIDs); err != nil {
			return nil, fmt.Errorf("ir.AssembleUnit: function %q return type: %w", f.Name, err)
		}

		// 局部变量：codegen 不知道 decl（节点 id 由自己分配，但"哪个节点绑定它"
		// 要扫一遍才知道）。参数保持 decl = root。
		for vi := range f.Vars {
			v := &f.Vars[vi]
			if err := resolveFuncType(&v.T, typeIDs); err != nil {
				return nil, fmt.Errorf("ir.AssembleUnit: function %q variable %q: %w", f.Name, v.Name, err)
			}
			if paramNames[v.Name] {
				continue
			}
			found := -1
			for ni := range f.Nodes {
				ids, _ := boundVars(&f.Nodes[ni])
				for _, id := range ids {
					if id == v.ID {
						found = ni
						break
					}
				}
				if found >= 0 {
					break
				}
			}
			if found < 0 {
				return nil, fmt.Errorf("ir.AssembleUnit: function %q variable %q (id %d) has no binding node: "+
					"codegen and assembly disagree (compiler bug)", f.Name, v.Name, v.ID)
			}
			v.Decl = found
		}
	}
	return fs, nil
}

// resolveFuncType 把类型表达式里的 struct 名字换成 id（递归，含 array/httpresult/tuple）。
func resolveFuncType(t *TypeExpr, typeIDs map[string]int) error {
	switch t.K {
	case "struct":
		if t.ID != nil {
			return nil
		}
		if t.Ref == "" {
			return fmt.Errorf("struct type is missing ref")
		}
		id, ok := typeIDs[t.Ref]
		if !ok {
			return fmt.Errorf("unknown type %q", t.Ref)
		}
		t.ID = &id
		t.Ref = "" // 必须清空：留着会让 emit 写出 schema 不认的字段
	case "array", "httpresult":
		if len(t.Of) == 0 {
			return fmt.Errorf("%s is missing of", t.K)
		}
		var sub TypeExpr
		if err := json.Unmarshal(t.Of, &sub); err != nil {
			return fmt.Errorf("%s has a malformed of: %w", t.K, err)
		}
		if err := resolveFuncType(&sub, typeIDs); err != nil {
			return err
		}
		b, err := json.Marshal(sub)
		if err != nil {
			return err
		}
		t.Of = b
	case "tuple":
		var subs []TypeExpr
		if len(t.Of) > 0 {
			if err := json.Unmarshal(t.Of, &subs); err != nil {
				return fmt.Errorf("tuple has a malformed of: %w", err)
			}
		}
		for i := range subs {
			if err := resolveFuncType(&subs[i], typeIDs); err != nil {
				return err
			}
		}
		b, err := json.Marshal(subs)
		if err != nil {
			return err
		}
		t.Of = b
	}
	return nil
}

// resolveTypes 把 C++ 的类型表装配成 IR 的 types[]：
//   - id 从 1 递增，按声明顺序（IR.md §3.1）
//   - `{"k":"struct","ref":"X"}` 解析成 `{"k":"struct","id":N}`
//   - 每个类型算 sha256（**不含 id/module/loc**，否则 ODR 比较会因位置不同而失败）
func resolveTypes(typesJSON []byte, module int) ([]TypeDef, error) {
	if len(typesJSON) == 0 {
		return []TypeDef{}, nil
	}
	var raws []rawType
	if err := json.Unmarshal(typesJSON, &raws); err != nil {
		return nil, fmt.Errorf("ir.AssembleUnit: type table is not valid JSON: %w", err)
	}
	if len(raws) == 0 {
		return []TypeDef{}, nil
	}

	// 先分配 id：声明顺序即 id 顺序。引用可能指向后面声明的类型（前向引用合法），
	// 所以必须先建完整张表再解析引用。
	byName := make(map[string]int, len(raws))
	for i := range raws {
		byName[raws[i].Name] = i + 1
	}

	out := make([]TypeDef, 0, len(raws))
	for i := range raws {
		rt := &raws[i]
		fields := make([]Field, 0, len(rt.Fields))
		for _, rf := range rt.Fields {
			// 递归解析：array 的 of 可能是 struct 引用（`OrderInfo[]`），
			// 也要把名字换成 id —— 只处理顶层会让元素类型指向一个不存在的 id。
			t, err := resolveTypeExpr(&rf.T, byName)
			if err != nil {
				return nil, fmt.Errorf("ir.AssembleUnit: type %q field %q: %w", rt.Name, rf.N, err)
			}
			fields = append(fields, Field{N: rf.N, T: t, Opt: rf.Opt, Ann: rf.Ann})
		}

		sum, err := typeSHA256(rt.Name, fields)
		if err != nil {
			return nil, err
		}
		mod := module
		if rt.Module != nil {
			mod = *rt.Module
		}
		out = append(out, TypeDef{
			ID:       i + 1,
			Name:     rt.Name,
			Module:   mod,
			Loc:      rt.Loc,
			Fields:   fields,
			SHA256:   sum,
			Exported: rt.Exported,
		})
	}
	return out, nil
}

// typeSHA256 是 IR.md §3.1 的"类型规范序列化"哈希：
// sha256(canonical_json({"name":…,"fields":[…]})，**不含 id/module/loc**。
//
// 不含位置是硬要求：链接期比较两个库里的同名 struct 时，它们在各自源码里
// 的行号必然不同；带上 loc 会让每个 struct 都"不一致"，ODR 直接失效。
func typeSHA256(name string, fields []Field) (string, error) {
	fieldVals := make([]canon.Value, 0, len(fields))
	for _, f := range fields {
		tv := canon.Obj("k", canon.Str(f.T.K))
		if f.T.ID != nil {
			tv = canon.Obj("k", canon.Str(f.T.K), "id", canon.Int(int64(*f.T.ID)))
		}
		anns := make([]canon.Value, 0, len(f.Ann))
		for _, a := range f.Ann {
			args := make([]canon.Value, 0, len(a.Args))
			for _, s := range a.Args {
				args = append(args, canon.Str(s))
			}
			anns = append(anns, canon.Obj("name", canon.Str(a.Name), "args", canon.Arr(args...)))
		}
		fieldVals = append(fieldVals, canon.Obj(
			"n", canon.Str(f.N),
			"t", tv,
			"opt", canon.Bool(f.Opt),
			"ann", canon.Arr(anns...),
		))
	}
	v := canon.Obj("name", canon.Str(name), "fields", canon.Arr(fieldVals...))
	sum := sha256.Sum256(v.Bytes())
	return hex.EncodeToString(sum[:]), nil
}

func exportedTypeNames(types []TypeDef) []string {
	out := make([]string, 0, len(types))
	for _, t := range types {
		if t.Exported {
			out = append(out, t.Name)
		}
	}
	return out
}

// extractTools 把每个 `call_tool` 节点上的 `tool_decl` 提升到 unit 级的 tools[]。
//
// 为什么工具表是 unit 级而不是节点级：链接期与 `hermes explain` 要能回答
// "这个 unit 一共调了哪些外部端点" —— 那需要一张**按端点去重**的表。
// 名字由 codegen 合成（D-029：语言里没有 tool 声明），去重按名字。
func extractTools(wfJSON []byte) ([]byte, []Tool, error) {
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(wfJSON))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, nil, fmt.Errorf("ir.AssembleUnit: workflow is not valid JSON: %w", err)
	}

	tools := []Tool{}
	seen := map[string]bool{}
	nodes, _ := doc["nodes"].([]any)
	for _, n := range nodes {
		m, ok := n.(map[string]any)
		if !ok || m["k"] != "call_tool" {
			continue
		}
		raw, ok := m["tool_decl"]
		if !ok {
			return nil, nil, fmt.Errorf("ir.AssembleUnit: call_tool node %v is missing tool_decl", m["id"])
		}
		delete(m, "tool_decl")

		buf, err := json.Marshal(raw)
		if err != nil {
			return nil, nil, err
		}
		var t Tool
		if err := json.Unmarshal(buf, &t); err != nil {
			return nil, nil, fmt.Errorf("ir.AssembleUnit: malformed tool_decl: %w", err)
		}
		if t.Method == "" {
			t.Method = "GET"
		}
		if !seen[t.Name] {
			seen[t.Name] = true
			tools = append(tools, t)
		}
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, nil, err
	}
	return out, tools, nil
}

// resolveBackends 把合并后的 backend 描述装配成 IR 的 backends[]。
//
// 声明（`.h`）给 kind 与 exported，定义（`.hermes`）给地址来源。两者按名字合并：
// 只有定义的 → 私有 backend（合法）；只有声明的 → 缺地址，是错误（E3007 的运行期形态）。
func resolveBackends(js []byte) ([]Backend, error) {
	if len(js) == 0 {
		return []Backend{}, nil
	}
	var raws []struct {
		Name     string          `json:"name"`
		Kind     string          `json:"kind"`
		Exported bool            `json:"exported"`
		DeclForm string          `json:"decl_form"`
		Loc      Loc             `json:"loc"`
		Addr     json.RawMessage `json:"addr"`
	}
	if err := json.Unmarshal(js, &raws); err != nil {
		return nil, fmt.Errorf("ir.AssembleUnit: backend table is not valid JSON: %w", err)
	}

	byName := map[string]*Backend{}
	order := []string{}
	for _, r := range raws {
		b, ok := byName[r.Name]
		if !ok {
			b = &Backend{Name: r.Name}
			byName[r.Name] = b
			order = append(order, r.Name)
		}
		if r.Kind != "" {
			b.Kind = r.Kind
		}
		if r.Exported {
			b.Exported = true
		}
		loc := r.Loc
		if r.DeclForm == "sig" {
			b.DeclLoc = &loc
		} else {
			b.DefLoc = &loc
			if len(r.Addr) > 0 {
				var addr Addr
				if err := json.Unmarshal(r.Addr, &addr); err != nil {
					return nil, fmt.Errorf("ir.AssembleUnit: backend %q has a malformed address: %w", r.Name, err)
				}
				b.Addr = addr
			}
		}
	}

	out := make([]Backend, 0, len(order))
	for _, n := range order {
		b := byName[n]
		if b.Kind == "" {
			return nil, fmt.Errorf(
				"ir.AssembleUnit: backend %q has no kind: write `backend %s : <openai|anthropic|ollama|homonoia|custom>` in source (D-032). "+
					"The compiler will not guess a protocol; a guess looks correct only when it 'happens to work on this machine'", n, n)
		}
		if b.Exported && b.DefLoc == nil {
			return nil, fmt.Errorf("ir.AssembleUnit: backend %q is declared export but has no definition (missing address)", n)
		}
		if b.DefLoc == nil && b.Addr.Src == "" {
			// 只有声明没有定义：允许（等 #link 提供），但这里没有链接信息，先放过。
			out = append(out, *b)
			continue
		}
		out = append(out, *b)
	}
	return out, nil
}

// resolveTypeExpr 递归解析类型表达式：把 `struct.ref` 换成 `struct.id`，
// 并对 array 的 of 递归处理。
func resolveTypeExpr(rt *rawTypeExpr, byName map[string]int) (TypeExpr, error) {
	t := TypeExpr{K: rt.K, ID: rt.ID}
	if rt.K == "struct" && rt.Ref != "" {
		id, ok := byName[rt.Ref]
		if !ok {
			return t, fmt.Errorf("reference to undeclared struct %q", rt.Ref)
		}
		t.ID = &id
	}
	if rt.K == "array" || rt.K == "httpresult" {
		if len(rt.Of) == 0 {
			return t, fmt.Errorf("%s is missing of", rt.K)
		}
		var sub rawTypeExpr
		if err := json.Unmarshal(rt.Of, &sub); err != nil {
			return t, err
		}
		inner, err := resolveTypeExpr(&sub, byName)
		if err != nil {
			return t, err
		}
		buf, err := json.Marshal(inner)
		if err != nil {
			return t, err
		}
		t.Of = buf
	}
	return t, nil
}

// resolveRetType 把 codegen 的返回类型（名字形态）递归解析成 id 形态。
//
// v2.0.0：double 已回到公共白名单（唯一浮点类型；float 不再是类型名）。
// 对 tool.call<double> 的拒绝**不在这个共享 helper 里**，而在调用点：那守的是
// 「call_tool 的 T 必须是 JSON 结构类型」，不是「double 不存在」。把拒绝塞回来
// 会让 call_native 的 double 一起被误拒。
func resolveRetType(t map[string]any, typeIDs map[string]int) (map[string]any, error) {
	k, _ := t["k"].(string)
	switch k {
	case "string", "int", "double", "bool", "json", "cost":
		return t, nil
	// 2.0.1：void 是合法的**函数返回类型**（不是值类型）。它只出现在
	// functions[].ret 里；声明一个 void 字段会被 schema 的 typeBase 拦下。
	case "void":
		return t, nil
	case "struct":
		if _, has := t["id"]; has {
			return t, nil
		}
		name, _ := t["ref"].(string)
		id, ok := typeIDs[name]
		if !ok {
			return nil, fmt.Errorf("ir.AssembleUnit: return type %q is undeclared (typo?)", name)
		}
		return map[string]any{"k": "struct", "id": id}, nil
	case "httpresult", "array":
		of, ok := t["of"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("ir.AssembleUnit: %s is missing of", k)
		}
		// of 递归沿用同一份白名单：array/httperesult 里包着一层 double 时，
		// 判据必须与外层类型属于同一条节点路径。
		inner, err := resolveRetType(of, typeIDs)
		if err != nil {
			return nil, err
		}
		return map[string]any{"k": k, "of": inner}, nil
	default:
		// 名字形态：codegen 直接写 `{"k":"OrderInfo"}`。
		if id, ok := typeIDs[k]; ok {
			return map[string]any{"k": "struct", "id": id}, nil
		}
		return nil, fmt.Errorf("ir.AssembleUnit: return type %q is undeclared (typo?)", k)
	}
}

// parseDeclaredTools 解析代码生成器给的 `tool` 声明数组。
func parseDeclaredTools(raw []byte) ([]Tool, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var arr []Tool
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("ir.AssembleUnit: malformed tool declaration: %w", err)
	}
	return arr, nil
}

// mergeTools 合并两张工具表。
//
// 同名必须报错，不能"后者覆盖前者"：模型按名字调工具，
// 而"调到了哪一个"取决于合并顺序的话，那就是运气不是设计。
func mergeTools(fromCalls, declared []Tool) []Tool {
	seen := make(map[string]bool, len(fromCalls)+len(declared))
	out := make([]Tool, 0, len(fromCalls)+len(declared))
	for _, t := range fromCalls {
		if seen[t.Name] {
			continue
		}
		seen[t.Name] = true
		out = append(out, t)
	}
	for _, t := range declared {
		if seen[t.Name] {
			continue
		}
		seen[t.Name] = true
		out = append(out, t)
	}
	return out
}
