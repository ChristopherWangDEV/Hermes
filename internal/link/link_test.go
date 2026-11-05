package link

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"hermes/internal/archive"
	"hermes/internal/ir"
	"hermes/internal/std"
)

// makeMember 造一个最小可加载的成员 IR（含一个导出 struct）。
func makeMember(t *testing.T, unit, structName, fieldType string) []byte {
	t.Helper()
	doc := &ir.Doc{
		IRVersion: 1,
		Unit: ir.Unit{Name: unit, EntryPath: unit + ".hermes", LanguageVersion: 1,
			Modules: []ir.Module{{Path: unit + ".hermes", Builtin: false,
				SHA256: "0000000000000000000000000000000000000000000000000000000000000000"}}},
		Compiler: ir.Compiler{Name: "hermes-parse", Version: "0.1.0", ParserABI: 1},
		Types: []ir.TypeDef{{
			ID: 1, Name: structName, Exported: true,
			Loc:    ir.Loc{M: 0, L: 1, C: 1, EL: 1, EC: 2},
			Fields: []ir.Field{{N: "x", T: ir.TypeExpr{K: fieldType}, Ann: []ir.Annotation{}}},
			SHA256: typeHash(structName, fieldType),
		}},
		Backends: []ir.Backend{}, Tools: []ir.Tool{}, Schemas: []ir.SchemaDoc{},
		Workflows: []ir.Workflow{{
			Name: unit, Exported: true, IsEntry: true, Backend: "",
			Returns:   ir.TypeExpr{K: "string"},
			Root:      0,
			Nodes:     []ir.Node{{ID: 0, K: "seq", Loc: ir.Loc{M: 0, L: 1, C: 1, EL: 1, EC: 2}}},
			Vars:      []ir.Var{},
			Bounds:    ir.Bounds{MaxSteps: 1, MaxSpans: 0, MaxConcurrency: 1},
			Economics: []byte(`{}`), InterpDeps: []ir.InterpDep{},
			Loc: ir.Loc{M: 0, L: 1, C: 1, EL: 1, EC: 2},
		}}, Imports: []ir.ImportRef{}, Requires: []ir.RequireItem{},
		Exports: ir.Exports{Workflows: []string{unit}, Types: []string{structName},
			Backends: []string{}, Tools: []string{}},
	}
	b, err := ir.Emit(doc)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if _, ds := ir.Load(b); len(ds) != 0 {
		for _, d := range ds {
			t.Logf("fixture 诊断: %s %s", d.Code, d.Message)
		}
		t.Fatalf("fixture 自己不合格: %d 条", len(ds))
	}
	return b
}

func typeHash(name, fieldType string) string {
	// 与 ir.typeSHA256 同构的简化版：测试只需要"同定义同哈希、不同定义不同哈希"。
	return name + "/" + fieldType
}

func archiveOf(t *testing.T, name string, irBytes []byte) *archive.Archive {
	t.Helper()
	data, err := archive.Write([]archive.Member{{
		Name: name, IR: irBytes, LanguageVersion: 1, ABIVersion: 1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	a, err := archive.Read(data)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestODRSameDefinitionMerges 证明"同名同构"是合并而不是冲突。
func TestODRSameDefinitionMerges(t *testing.T) {
	a := archiveOf(t, "a", makeMember(t, "a", "OrderInfo", "string"))
	res, problems := Link([]Input{{Path: "a.hbc", Archive: a}})
	if len(problems) != 0 {
		t.Fatalf("不该有问题: %v", problems)
	}
	if _, ok := res.Symbols[KindType]["OrderInfo"]; !ok {
		t.Fatal("OrderInfo 应进符号表")
	}
}

// TestODRMismatchReportsFieldDiff 是链接错误质量的核心：
// 必须**字段级**说出差在哪，否则人只能两个文件对读。
func TestODRMismatchReportsFieldDiff(t *testing.T) {
	a := archiveOf(t, "a", makeMember(t, "a", "OrderInfo", "string"))
	b := archiveOf(t, "b", makeMember(t, "b", "OrderInfo", "int"))
	_, problems := Link([]Input{
		{Path: "a.hbc", Archive: a},
		{Path: "b.hbc", Archive: b},
	})
	if len(problems) == 0 {
		t.Fatal("两个库的同名 struct 定义不同，必须报错")
	}
	msg := strings.Join(problems, "\n")
	for _, want := range []string{"两处定义不同", "字段 x", "string", "int", "怎么改"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("ODR 报错缺少 %q：\n%s", want, msg)
		}
	}
}

// TestODRNotTriggeredForUnexported 证明私有类型不参与 ODR。
//
// 私有 struct 是每个库的内部实现；把它也拉进 ODR，会让两个毫无关系的库
// 因为都有个叫 `State` 的私有结构而互相拒绝链接。
func TestODRNotTriggeredForUnexported(t *testing.T) {
	mk := func(unit, ty string) []byte {
		d := &ir.Doc{
			IRVersion: 1,
			Unit: ir.Unit{Name: unit, EntryPath: unit + ".hermes", LanguageVersion: 1,
				Modules: []ir.Module{{Path: unit + ".hermes", Builtin: false,
					SHA256: "0000000000000000000000000000000000000000000000000000000000000000"}}},
			Compiler: ir.Compiler{Name: "p", Version: "1", ParserABI: 1},
			Types: []ir.TypeDef{{ID: 1, Name: "State", Exported: false,
				Loc:    ir.Loc{M: 0, L: 1, C: 1, EL: 1, EC: 2},
				Fields: []ir.Field{{N: "x", T: ir.TypeExpr{K: ty}, Ann: []ir.Annotation{}}},
				SHA256: "private"}},
			Backends: []ir.Backend{}, Tools: []ir.Tool{}, Schemas: []ir.SchemaDoc{},
			Workflows: []ir.Workflow{{
				Name: unit, Exported: true, IsEntry: true, Backend: "",
				Returns:   ir.TypeExpr{K: "string"},
				Root:      0,
				Nodes:     []ir.Node{{ID: 0, K: "seq", Loc: ir.Loc{M: 0, L: 1, C: 1, EL: 1, EC: 2}}},
				Vars:      []ir.Var{},
				Bounds:    ir.Bounds{MaxSteps: 1, MaxSpans: 0, MaxConcurrency: 1},
				Economics: []byte(`{}`), InterpDeps: []ir.InterpDep{},
				Loc: ir.Loc{M: 0, L: 1, C: 1, EL: 1, EC: 2},
			}}, Imports: []ir.ImportRef{}, Requires: []ir.RequireItem{},
			Exports: ir.Exports{Workflows: []string{unit}, Types: []string{}, Backends: []string{}, Tools: []string{}},
		}
		b, _ := ir.Emit(d)
		return b
	}
	a := archiveOf(t, "a", mk("a", "string"))
	b := archiveOf(t, "b", mk("b", "int"))
	_, problems := Link([]Input{{Path: "a.hbc", Archive: a}, {Path: "b.hbc", Archive: b}})
	if len(problems) != 0 {
		t.Fatalf("私有 struct 不该触发 ODR：%v", problems)
	}
}

// TestDuplicateWorkflowIsError 守"同名 workflow 不覆盖"。
//
// 静默覆盖会让"我链接的到底是谁"变成不可知 —— 而这类问题在运行期
// 表现为"调用了另一个库的实现"，最难查。
func TestDuplicateWorkflowIsError(t *testing.T) {
	mk := func(unit string) []byte {
		d := &ir.Doc{
			IRVersion: 1,
			Unit: ir.Unit{Name: unit, EntryPath: unit + ".hermes", LanguageVersion: 1,
				Modules: []ir.Module{{Path: unit + ".hermes", Builtin: false,
					SHA256: "0000000000000000000000000000000000000000000000000000000000000000"}}},
			Compiler: ir.Compiler{Name: "p", Version: "1", ParserABI: 1},
			Types:    []ir.TypeDef{}, Backends: []ir.Backend{}, Tools: []ir.Tool{}, Schemas: []ir.SchemaDoc{},
			Workflows: []ir.Workflow{{
				Name: "dup", Exported: true, IsEntry: true, Backend: "",
				Returns: ir.TypeExpr{K: "string"}, Root: 0,
				Nodes:     []ir.Node{{ID: 0, K: "seq", Loc: ir.Loc{M: 0, L: 1, C: 1, EL: 1, EC: 2}}},
				Bounds:    ir.Bounds{MaxSteps: 1, MaxSpans: 0, MaxConcurrency: 1},
				Economics: []byte(`{}`), InterpDeps: []ir.InterpDep{},
				Loc: ir.Loc{M: 0, L: 1, C: 1, EL: 1, EC: 2},
			}},
			Imports: []ir.ImportRef{}, Requires: []ir.RequireItem{},
			Exports: ir.Exports{Workflows: []string{"dup"}, Types: []string{}, Backends: []string{}, Tools: []string{}},
		}
		b, _ := ir.Emit(d)
		return b
	}
	a := archiveOf(t, "a", mk("a"))
	b := archiveOf(t, "b", mk("b"))
	_, problems := Link([]Input{{Path: "a.hbc", Archive: a}, {Path: "b.hbc", Archive: b}})
	if len(problems) == 0 {
		t.Fatal("同名 workflow 必须报错")
	}
	if !strings.Contains(strings.Join(problems, "\n"), "同名 workflow") {
		t.Fatalf("错误文案应点名同名 workflow：%v", problems)
	}
}

// TestVerifyRequiresDetectsTampering 守"声明了依赖谁，就得说清是哪份字节"。
//
// 这是三层判据里**唯一防投毒**的一条：版本号可以伪造，字节哈希不行。
func TestVerifyRequiresDetectsTampering(t *testing.T) {
	mk := func(unit, sha string) []byte {
		d := &ir.Doc{
			IRVersion: 1,
			Unit: ir.Unit{Name: unit, EntryPath: unit + ".hermes", LanguageVersion: 1,
				Modules: []ir.Module{{Path: unit + ".hermes", Builtin: false,
					SHA256: "0000000000000000000000000000000000000000000000000000000000000000"}}},
			Compiler: ir.Compiler{Name: "p", Version: "1", ParserABI: 1},
			Types:    []ir.TypeDef{}, Backends: []ir.Backend{}, Tools: []ir.Tool{}, Schemas: []ir.SchemaDoc{},
			Workflows: []ir.Workflow{{
				Name: unit, Exported: true, IsEntry: true,
				Returns: ir.TypeExpr{K: "string"}, Root: 0,
				Nodes:     []ir.Node{{ID: 0, K: "seq", Loc: ir.Loc{M: 0, L: 1, C: 1, EL: 1, EC: 2}}},
				Bounds:    ir.Bounds{MaxSteps: 1, MaxSpans: 0, MaxConcurrency: 1},
				Economics: []byte(`{}`), InterpDeps: []ir.InterpDep{},
				Loc: ir.Loc{M: 0, L: 1, C: 1, EL: 1, EC: 2},
			}},
			Imports: []ir.ImportRef{},
			Requires: []ir.RequireItem{{
				Spec: "models", Kind: "local", ABIVersion: 1, IRVersion: 1,
				Origin: "source", MemberSHA256: sha,
			}},
			Exports: ir.Exports{Workflows: []string{unit}, Types: []string{}, Backends: []string{}, Tools: []string{}},
		}
		b, _ := ir.Emit(d)
		return b
	}

	t.Run("伪造的哈希必须被发现", func(t *testing.T) {
		a := archiveOf(t, "a", mk("a", "deadbeef"))
		b := archiveOf(t, "b", mk("b", "cafebabe"))
		problems := VerifyRequires([]Input{{Path: "a.hbc", Archive: a}, {Path: "b.hbc", Archive: b}})
		if len(problems) == 0 {
			t.Fatal("requires 的 member_sha256 对不上任何输入，必须报 E_LINK_TAMPERED")
		}
		if !strings.Contains(strings.Join(problems, "\n"), "member_sha256") {
			t.Fatalf("报错应点名 member_sha256：%v", problems)
		}
	})

	t.Run("缺 member_sha256 也是错误", func(t *testing.T) {
		a := archiveOf(t, "a", mk("a", ""))
		problems := VerifyRequires([]Input{{Path: "a.hbc", Archive: a}})
		if len(problems) == 0 {
			t.Fatal("非 implicit 的 requires 缺 member_sha256 必须报 E_LINK_REQUIRES_INCOMPLETE")
		}
	})
}

// TestStdRequireVerifiedAgainstEmbeddedStdlib 是供应链那一环的闭合点。
//
// std 模块永远不会出现在任何 `.hbc` 里（它没有 workflow，构不出合法 IR 成员），
// 所以"查链接输入"这条路走不通。它的字节就在二进制里 —— 对着那里校验，
// 才能回答"我依赖的到底是哪份 stdlib"。
func TestStdRequireVerifiedAgainstEmbeddedStdlib(t *testing.T) {
	src, ok := std.Source("net")
	if !ok {
		t.Fatal("内嵌 stdlib 里没有 net")
	}
	sum := sha256.Sum256(src)
	goodSHA := hex.EncodeToString(sum[:])

	mk := func(sha string) []byte {
		d := &ir.Doc{
			IRVersion: 1,
			Unit: ir.Unit{Name: "u", EntryPath: "u.hermes", LanguageVersion: 1,
				Modules: []ir.Module{{Path: "u.hermes", Builtin: false,
					SHA256: "0000000000000000000000000000000000000000000000000000000000000000"}}},
			Compiler: ir.Compiler{Name: "p", Version: "1", ParserABI: 1},
			Types:    []ir.TypeDef{}, Backends: []ir.Backend{}, Tools: []ir.Tool{}, Schemas: []ir.SchemaDoc{},
			Workflows: []ir.Workflow{{
				Name: "u", Exported: true, IsEntry: true,
				Returns: ir.TypeExpr{K: "string"}, Root: 0,
				Nodes:     []ir.Node{{ID: 0, K: "seq", Loc: ir.Loc{M: 0, L: 1, C: 1, EL: 1, EC: 2}}},
				Bounds:    ir.Bounds{MaxSteps: 1, MaxSpans: 0, MaxConcurrency: 1},
				Economics: []byte(`{}`), InterpDeps: []ir.InterpDep{},
				Loc: ir.Loc{M: 0, L: 1, C: 1, EL: 1, EC: 2},
			}},
			Imports: []ir.ImportRef{},
			Requires: []ir.RequireItem{{
				Spec: "<std/net>", Kind: "std", ABIVersion: 1, IRVersion: 1,
				Origin: "source", MemberSHA256: sha,
			}},
			Exports: ir.Exports{Workflows: []string{"u"}, Types: []string{}, Backends: []string{}, Tools: []string{}},
		}
		b, _ := ir.Emit(d)
		return b
	}

	t.Run("正确的 std 哈希通过", func(t *testing.T) {
		a := archiveOf(t, "u", mk(goodSHA))
		if problems := VerifyRequires([]Input{{Path: "u.hbc", Archive: a}}); len(problems) != 0 {
			t.Fatalf("不该有问题: %v", problems)
		}
	})

	t.Run("伪造的 std 哈希被抓住", func(t *testing.T) {
		a := archiveOf(t, "u", mk("deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"))
		problems := VerifyRequires([]Input{{Path: "u.hbc", Archive: a}})
		if len(problems) == 0 {
			t.Fatal("std 依赖哈希对不上，必须报 E_LINK_TAMPERED")
		}
	})

	t.Run("依赖了不存在的 std 模块", func(t *testing.T) {
		a := archiveOf(t, "u", mk(goodSHA))
		d, _ := ir.Load(a.Members[0].IR)
		_ = d
		// 直接构造一条指向不存在模块的 spec
		if err := verifyStdRequire("<std/nope>", goodSHA); err == nil {
			t.Fatal("不存在的 std 模块必须报错")
		}
	})
}

// TestReachDropsUnusedMembers 是"我只付我用到的那部分"（IR.md §9.4）。
//
// 两个库：base 导出 OrderInfo，app 导出 entry 且**不引用**它。
// 从 entry 出发，base 不可达 → 必须被剥掉。
func TestReachDropsUnusedMembers(t *testing.T) {
	mk := func(unit string, entry bool, usesType bool) []byte {
		types := []ir.TypeDef{}
		exportsTypes := []string{}
		if unit == "base" {
			types = []ir.TypeDef{{ID: 1, Name: "OrderInfo", Exported: true,
				Loc:    ir.Loc{M: 0, L: 1, C: 1, EL: 1, EC: 2},
				Fields: []ir.Field{{N: "id", T: ir.TypeExpr{K: "string"}, Ann: []ir.Annotation{}}},
				SHA256: "base/OrderInfo"}}
			exportsTypes = []string{"OrderInfo"}
		}
		var imps []ir.ImportRef
		if usesType {
			imps = []ir.ImportRef{{Sym: "OrderInfo", Kind: "type",
				Loc: ir.Loc{M: 0, L: 2, C: 1, EL: 2, EC: 2}}}
		}
		d := &ir.Doc{
			IRVersion: 1,
			Unit: ir.Unit{Name: unit, EntryPath: unit + ".hermes", LanguageVersion: 1,
				Modules: []ir.Module{{Path: unit + ".hermes", Builtin: false,
					SHA256: "0000000000000000000000000000000000000000000000000000000000000000"}}},
			Compiler: ir.Compiler{Name: "p", Version: "1", ParserABI: 1},
			Types:    types, Backends: []ir.Backend{}, Tools: []ir.Tool{}, Schemas: []ir.SchemaDoc{},
			Workflows: []ir.Workflow{{
				Name: unit, Exported: true, IsEntry: entry,
				Returns: ir.TypeExpr{K: "string"}, Root: 0,
				Nodes:     []ir.Node{{ID: 0, K: "seq", Loc: ir.Loc{M: 0, L: 1, C: 1, EL: 1, EC: 2}}},
				Bounds:    ir.Bounds{MaxSteps: 1, MaxSpans: 0, MaxConcurrency: 1},
				Economics: []byte(`{}`), InterpDeps: []ir.InterpDep{},
				Loc: ir.Loc{M: 0, L: 1, C: 1, EL: 1, EC: 2},
			}},
			Imports: imps, Requires: []ir.RequireItem{},
			Exports: ir.Exports{Workflows: []string{unit}, Types: exportsTypes, Backends: []string{}, Tools: []string{}},
		}
		b, _ := ir.Emit(d)
		return b
	}

	t.Run("未被引用就剥掉", func(t *testing.T) {
		a := archiveOf(t, "base", mk("base", false, false))
		b := archiveOf(t, "app", mk("app", true, false))
		kept, dropped, _ := Reach([]Input{{Path: "base.hbc", Archive: a}, {Path: "app.hbc", Archive: b}})
		if !kept["app"] {
			t.Fatal("entry 所在的成员必须保留")
		}
		if kept["base"] {
			t.Fatal("base 从 entry 不可达，应被剥掉")
		}
		if len(dropped) != 1 || dropped[0] != "base" {
			t.Fatalf("应剥掉 base，实际 %v", dropped)
		}
	})

	t.Run("被引用就保留", func(t *testing.T) {
		a := archiveOf(t, "base", mk("base", false, false))
		b := archiveOf(t, "app", mk("app", true, true)) // app 引用 OrderInfo
		kept, _, _ := Reach([]Input{{Path: "base.hbc", Archive: a}, {Path: "app.hbc", Archive: b}})
		if !kept["base"] {
			t.Fatal("app 引用了 base 导出的类型，base 必须保留")
		}
	})
}

// TestLocalRequireVerifiedByIndexHash 守本地依赖的校验形式：
// **链接输入里必须有一份索引哈希等于 member_sha256 的归档**。
//
// 这就是"我要那份确切字节"的可校验形式：比成员 IR 更对，因为一个库可以有多个
// 成员，而依赖声明的是"这个库"。
func TestLocalRequireVerifiedByIndexHash(t *testing.T) {
	// 造一个库归档，算它的索引哈希。
	libData, err := archive.Write([]archive.Member{{
		Name: "models", IR: []byte(`{"ir_version":1,"unit":{"name":"models"}}`),
		Exports: archive.Exports{Workflows: []string{"m"}}, Entry: false,
		LanguageVersion: 1, ABIVersion: 1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	libA, err := archive.Read(libData)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(libA.IndexBytes(libData))
	goodSHA := hex.EncodeToString(sum[:])

	mk := func(sha string) []byte {
		d := &ir.Doc{
			IRVersion: 1,
			Unit: ir.Unit{Name: "app", EntryPath: "app.hermes", LanguageVersion: 1,
				Modules: []ir.Module{{Path: "app.hermes", SHA256: strings.Repeat("0", 64)}}},
			Compiler: ir.Compiler{Name: "p", Version: "1", ParserABI: 1},
			Types:    []ir.TypeDef{}, Backends: []ir.Backend{}, Tools: []ir.Tool{}, Schemas: []ir.SchemaDoc{},
			Workflows: []ir.Workflow{{
				Name: "app", Exported: true, IsEntry: true,
				Returns: ir.TypeExpr{K: "string"}, Root: 0,
				Nodes:     []ir.Node{{ID: 0, K: "seq", Loc: ir.Loc{M: 0, L: 1, C: 1, EL: 1, EC: 2}}},
				Bounds:    ir.Bounds{MaxSteps: 1, MaxSpans: 0, MaxConcurrency: 1},
				Economics: []byte(`{}`), InterpDeps: []ir.InterpDep{},
				Loc: ir.Loc{M: 0, L: 1, C: 1, EL: 1, EC: 2},
			}},
			Imports: []ir.ImportRef{},
			Requires: []ir.RequireItem{{
				Spec: `"models"`, Kind: "local", ABIVersion: 1, IRVersion: 1,
				Origin: "source", MemberSHA256: sha,
			}},
			Exports: ir.Exports{Workflows: []string{"app"}},
		}
		b, _ := ir.Emit(d)
		return b
	}

	appData, err := archive.Write([]archive.Member{{
		Name: "app", IR: mk(goodSHA), Entry: true, LanguageVersion: 1, ABIVersion: 1,
		Exports: archive.Exports{Workflows: []string{"app"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	appA, err := archive.Read(appData)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("哈希匹配通过", func(t *testing.T) {
		ins := []Input{
			{Path: "app.hbc", Archive: appA, Raw: appData},
			{Path: "models.hbc", Archive: libA, Raw: libData},
		}
		if p := VerifyRequires(ins); len(p) != 0 {
			t.Fatalf("不该有问题: %v", p)
		}
	})

	t.Run("缺了被依赖的库", func(t *testing.T) {
		ins := []Input{{Path: "app.hbc", Archive: appA, Raw: appData}}
		if p := VerifyRequires(ins); len(p) == 0 {
			t.Fatal("依赖的库不在输入里，必须报错")
		}
	})

	t.Run("哈希不符", func(t *testing.T) {
		otherData, _ := archive.Write([]archive.Member{{
			Name: "models", IR: []byte(`{"ir_version":1}`),
			LanguageVersion: 1, ABIVersion: 1,
			Exports: archive.Exports{Workflows: []string{}},
		}})
		otherA, _ := archive.Read(otherData)
		ins := []Input{
			{Path: "app.hbc", Archive: appA, Raw: appData},
			{Path: "models.hbc", Archive: otherA, Raw: otherData},
		}
		if p := VerifyRequires(ins); len(p) == 0 {
			t.Fatal("索引哈希不符，必须报错")
		}
	})
}
