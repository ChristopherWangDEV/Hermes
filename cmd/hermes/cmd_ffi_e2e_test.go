package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"hermes/internal/ir"
)

// 反向融合（extern / #link native / call_native）的**端到端**回归。
//
// 为什么要这一份：前端（tests/compiler/parser_test.cpp）只验到 codegen 吐出的
// 单 workflow 片段，Go IR 层（internal/ir）只验到"认得 call_native 这个 kind"。
// 中间那条链 —— lexer 的 #link native 载荷 → ScanDirectives → handleLink 的
// native 分支 → requires[] 装配 → importsForWorkflow → ir.Load 校验 →
// link 期 hasNativeRequire —— 之前没有任何一处测试把它整条串起来跑过。
// 结果就是：四段各自"看起来实现了"，但一个未调用的非法 extern 能整体编过
// （rc=0）。这里按真实 `hermes ir` 路径把链钉死。
const ffiHappySrc = `#link native "math" from "math_tool.h"
extern int imul(int a, int b);
export workflow probe() entry -> int {
    return imul(6, 7);
}
`

// TestFFIExternEndToEnd 验正向：源码里的一个 extern 调用必须同时体现为
// requires[].kind=="native"、imports[].kind=="native"、call_native 节点、
// 以及 workflow.bounds.unbounded —— 四者缺一都说明链在某处断了。
func TestFFIExternEndToEnd(t *testing.T) {
	code, msg, outPath := compileInlineSource(t, ffiHappySrc)
	if code != 0 {
		t.Fatalf("合法 extern 源码应编译通过，实际退出 %d：\n%s", code, msg)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	doc, ds := ir.Load(data)
	if len(ds) != 0 {
		t.Fatalf("产物 IR 应通过加载期校验，实际 %d 条诊断：%v", len(ds), ds)
	}

	// ① requires[]：一条且仅一条 native，带齐 Name/Archive/Header。
	var natives []ir.RequireItem
	for _, r := range doc.Requires {
		if r.Kind == "native" {
			natives = append(natives, r)
		}
	}
	if len(natives) != 1 {
		t.Fatalf("应恰好一条 native require，实际 %d：%+v", len(natives), doc.Requires)
	}
	r := natives[0]
	if r.Name != "math" || r.Archive != "math" || r.Header != "math_tool.h" {
		t.Errorf("native require 的 name/archive/header 应为 math/math/math_tool.h，实际 %+v", r)
	}
	if r.Origin != "source" {
		t.Errorf("native require.origin 应为 source，实际 %q", r.Origin)
	}
	if r.IRVersion != ir.IRVersionSupported {
		t.Errorf("native require.ir_version 应为 %d，实际 %d", ir.IRVersionSupported, r.IRVersion)
	}
	// C 归档不是 .hbc，编译期拿不到 member_sha256；编译一个假哈希比不写更坏。
	if r.MemberSHA256 != "" {
		t.Errorf("native require 不应伪造 member_sha256，实际 %q", r.MemberSHA256)
	}

	// ② imports[]：被实际调用的 native 符号一条，且 kind=="native"。
	//    kind 必须是 native 而不是 workflow —— 走 workflow 符号表会找不到、
	//    触发 E_LINK_UNRESOLVED（docs/EXTERN-C-ABI.md §2 的示例是笔误）。
	var nativeImports []ir.ImportRef
	for _, im := range doc.Imports {
		if im.Kind == "native" {
			nativeImports = append(nativeImports, im)
		}
	}
	if len(nativeImports) != 1 || nativeImports[0].Sym != "imul" {
		t.Fatalf("应恰好一条 native import 指向 imul，实际 %+v", doc.Imports)
	}

	// ③ call_native 节点：形状严格是 {id,k,loc,fn,args,ret,arg_types}。
	// arg_types 是 AOT 的编译期静态形参类型表（args 只有节点 id，没有类型）；
	// 没有它 C 后端就只能做运行期类型分发，那条路被 0 解释器红线禁止。
	if len(doc.Workflows) != 1 {
		t.Fatalf("应有 1 个 workflow，实际 %d", len(doc.Workflows))
	}
	wf := &doc.Workflows[0]
	var calls []*ir.Node
	for i := range wf.Nodes {
		if wf.Nodes[i].K == "call_native" {
			calls = append(calls, &wf.Nodes[i])
		}
	}
	if len(calls) != 1 {
		t.Fatalf("应恰好一个 call_native 节点，实际 %d", len(calls))
	}
	c := calls[0]
	if c.Fn == nil || *c.Fn != "imul" {
		t.Errorf("call_native.fn 应为 imul，实际 %v", c.Fn)
	}
	if c.Ret == nil || c.Ret.K != "int" {
		t.Errorf("call_native.ret 应为 {k:int}，实际 %+v", c.Ret)
	}
	var args []int
	if err := json.Unmarshal(c.Args, &args); err != nil {
		t.Fatalf("call_native.args 应是节点 id 数组：%v", err)
	}
	if len(args) != 2 {
		t.Errorf("call_native.args 应有 2 个实参节点，实际 %v", args)
	}
	for _, a := range args {
		if a < 0 || a >= len(wf.Nodes) {
			t.Errorf("call_native.args 指向越界节点 %d", a)
		}
	}

	// ④ extern 不进 functions[]（体在 native 归档，不是 Hermes 函数）。
	for _, f := range doc.Functions {
		if f.Name == "imul" {
			t.Error("extern 符号不应出现在 functions[] 里")
		}
	}

	// ⑤ 含 call_native 的 workflow 必须标 unbounded（终止性不可证，
	//    不是 span 数不可数 —— EXTERN-C-ABI.md §4.2）。
	if !wf.Bounds.Unbounded {
		t.Error("含 call_native 的 workflow 应标 bounds.unbounded:true")
	}
}

// TestFFIExternDiagnosticsEndToEnd 验负向：三条诊断走真实 CLI 都拦得住，
// 且**未被调用**的非法 extern 也不能漏（声明级检查，曾整体 rc=0 编过）。
func TestFFIExternDiagnosticsEndToEnd(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{
			name: "E3031 缺 #link native",
			src: `extern int imul(int a, int b);
export workflow probe() entry -> int { return imul(6, 7); }
`,
			want: []string{"E3031"},
		},
		{
			name: "E3032 .hpp 头",
			src: `#link native "math" from "math_tool.hpp"
extern int imul(int a, int b);
export workflow probe() entry -> int { return imul(6, 7); }
`,
			want: []string{"E3032"},
		},
		{
			name: "E3030 json 形参（被调用）",
			src: `#link native "math" from "math_tool.h"
extern int parse(json j);
export workflow probe() entry -> int { return parse(1); }
`,
			want: []string{"E3030"},
		},
		{
			// 这条是回归核心：extern 声明了 -> json 却**从不调用**。
			// 旧实现把 E3030 挂在调用点，于是整份源码 rc=0 编过。
			name: "E3030 未调用的 -> json extern 也必须拦",
			src: `#link native "math" from "math_tool.h"
extern workflow bad() -> json;
export workflow probe() entry -> int { return 1; }
`,
			want: []string{"E3030"},
		},
		{
			name: "E3032 未调用 extern 时仍拦 .hpp",
			src: `#link native "math" from "math_tool.hpp"
extern workflow bad() -> int;
export workflow probe() entry -> int { return 1; }
`,
			want: []string{"E3032"},
		},
		{
			name: "export extern 合法（§1.3 正例 1 同形）",
			src: `#link native "math" from "math_tool.h"
export extern int imul(int a, int b);
export workflow probe() entry -> int { return imul(6, 7); }
`,
			want: nil, // 不该有任何 error
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, msg, _ := compileInlineSource(t, tc.src)
			if len(tc.want) == 0 {
				if code != 0 {
					t.Fatalf("应编译通过，实际退出 %d：\n%s", code, msg)
				}
				return
			}
			if code != 4 {
				t.Fatalf("应被诊断拦下（退出 4），实际 %d：\n%s", code, msg)
			}
			for _, w := range tc.want {
				if !strings.Contains(msg, w) {
					t.Errorf("诊断应含 %s，实际：\n%s", w, msg)
				}
			}
		})
	}
}
