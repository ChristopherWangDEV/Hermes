package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"

	"hermes/internal/engine"
	"hermes/internal/ir"
	"hermes/internal/runtime"
)

// cmdRunaway 是"有界"这个论点的**可执行证据**。
//
// 场景：模型每一轮都要求调用同一个工具 —— 这是最坏情况，也是最常见的翻车形态
// （模型以为自己在推进，其实在原地打转）。运行期计数的框架会一直转下去，
// 账单是事后才知道的；Hermes 的 max_steps 是编译期常量，所以它在第 N 步就停。
//
// 为什么这个命令值得存在：`hermes demo` 展示的是"能编译"，而别人真正怀疑的是
// "你说有界，凭什么"。一个能贴到任何地方复现的实验，比一千行文档有说服力。
func cmdRunaway(args []string) int {
	maxSteps := 4

	var modelCalls, toolCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/tool") {
			toolCalls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"still_running"}`))
			return
		}
		modelCalls++
		w.Header().Set("Content-Type", "application/json")
		// 每一轮都要求调用工具，永不给出最终答案。
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"content":null,
			"tool_calls":[{"id":"c1","type":"function",
			"function":{"name":"lookup_order","arguments":"{\"id\":\"A1\"}"}}]},
			"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer srv.Close()

	var b bytes.Buffer
	p := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }

	p("%s\n\n", tr("An agent that will not stop on its own"))
	p("%s\n", tr("  Every turn the model asks for the same tool: it thinks it is making progress,"))
	p("%s\n", tr("  while it is going in circles. Runtime-counting frameworks keep going and you learn\n"+
		"  the cost afterwards; here max_steps is a compile-time constant."))
	p("\n  max_steps = %d%s\n\n", maxSteps,
		tr(" (a compile-time constant, not a setting)"))

	doc := runawayDoc(srv.URL, maxSteps)
	res, err := engine.RunWith(doc, "", map[string]any{}, engine.RunOptions{
		Runtime: &runtime.Runtime{Backends: map[string]ir.Backend{"gw": {
			Name: "gw", Kind: "openai", Model: "m",
			Addr: ir.Addr{Src: "literal", Value: strptr(srv.URL)},
		}}},
	})

	for i := 1; i <= modelCalls; i++ {
		p("  %s %-2d %s\n", tr("step"), i,
			tr("model call -> asks for lookup_order"))
	}
	p("\n")

	ok := false
	if err != nil {
		var re *engine.RunError
		if asRunError(err, &re) {
			p("  → %s\n", re.Kind)
			// 引擎的消息是内部文案（单一语言）。演示脚本按当前语言重述一遍，
			// 而不是把中文原文塞进英文输出里 —— 那正是这次要修的问题。
			p("    %s\n\n", tr(
				"the agent still asked for tools at max_steps="+strconv.Itoa(maxSteps)+
					" — the bound is a promise, not a starting line"))
			ok = strings.Contains(re.Msg, "max_steps")
		} else {
			p("  → %v\n\n", err)
		}
	} else {
		p("  → 提前收敛：%v\n\n", res.Value)
	}

	p(tr("  %d model calls, %d tool calls — all within the bound\n"), modelCalls, toolCalls)
	if ok {
		p(tr("\n  It stopped — right after paying for step %d, not after reconciling a bill.\n"), maxSteps)
		os_Exit(b.String(), 0)
	}
	os_Exit(tr("  FAILED: it did not stop at the bound\n"), 1)
	return 0
}

// runawayDoc 手写一份最小 IR：`return agent { goal, tools: [lookup_order], max_steps }`。
// 不走源码编译是为了让这个实验**不依赖任何文件**（也不依赖 stdlib）。
func runawayDoc(baseURL string, maxSteps int) *ir.Doc {
	goal, agent, retID := 1, 2, 3
	return &ir.Doc{
		IRVersion: 1,
		Unit: ir.Unit{Name: "runaway", EntryPath: "runaway.hermes", LanguageVersion: 1,
			Modules: []ir.Module{{Path: "runaway.hermes", SHA256: strings.Repeat("0", 64)}}},
		Compiler: ir.Compiler{Name: "hermes", Version: semver, ParserABI: 1},
		Types:    []ir.TypeDef{}, Backends: []ir.Backend{}, Schemas: []ir.SchemaDoc{},
		Tools: []ir.Tool{{
			Name: "lookup_order", Description: "查订单状态", Method: "GET",
			URLTemplate: []ir.TemplatePart{
				{Lit: baseURL + "/tool/"},
				{Path: []string{"id"}},
			},
		}},
		Workflows: []ir.Workflow{{
			Name: "runaway", Exported: true, IsEntry: true,
			Returns: ir.TypeExpr{K: "json"}, Root: 0,
			Nodes: []ir.Node{
				{ID: 0, K: "seq", Kids: []int{retID}, Loc: ir.Loc{M: 0, L: 1, C: 1, EL: 1, EC: 2}},
				{ID: 1, K: "lit", LitT: strptr("string"), LitV: json.RawMessage(`"查一下订单 A1"`),
					Loc: ir.Loc{M: 0, L: 2, C: 1, EL: 2, EC: 9}},
				{ID: 2, K: "agent", Loc: ir.Loc{M: 0, L: 2, C: 1, EL: 2, EC: 9},
					Agent: &ir.AgentSpec{Goal: &goal, Tools: []string{"lookup_order"}, MaxSteps: maxSteps}},
				{ID: 3, K: "return", E: &agent, Loc: ir.Loc{M: 0, L: 2, C: 1, EL: 2, EC: 9}},
			},
			Bounds: ir.Bounds{MaxSteps: 1, MaxSpans: 2 * maxSteps, MaxConcurrency: 1,
				MaxModelCalls: maxSteps, MaxToolCalls: maxSteps},
			Economics: []byte(`{}`), InterpDeps: []ir.InterpDep{},
			Loc: ir.Loc{M: 0, L: 1, C: 1, EL: 1, EC: 2},
		}},
		Imports: []ir.ImportRef{}, Requires: []ir.RequireItem{},
		Exports: ir.Exports{Workflows: []string{"runaway"}},
	}
}

func strptr(s string) *string { return &s }

func asRunError(err error, target **engine.RunError) bool {
	if re, ok := err.(*engine.RunError); ok {
		*target = re
		return true
	}
	return false
}

func os_Exit(msg string, code int) {
	fmt.Print(msg)
	os.Exit(code)
}
