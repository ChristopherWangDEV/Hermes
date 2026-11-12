package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"hermes/internal/ir"
)

// TestRetryIdempotencyProof 是 LANGUAGE.md §5.7 / R-CB-06 的可执行形式。
//
// 这条红线的核心不是「报错」，而是「重复扣款不可能上线」：POST 落在 retry
// 子树里，若没有编译期可见的幂等证明，构建必须**失败**，而不是在运行期
// 猜一次。曾经 check_idem.cpp 根本不存在，E5007/E5009 零引用，
// `retry(2){ tool.call(POST) }` 能编出一份合法 IR —— 事故只差一次 5xx。
func TestRetryIdempotencyProof(t *testing.T) {
	const prelude = `#link <std/net>

export struct Q { string user_id; }
`
	const modelPrelude = prelude + `#link <std/model>

export struct I { string action; }
`

	cases := []struct {
		name string
		src  string
		// wantCode 非空 ⇒ 必须阻塞并出现该诊断；空 ⇒ 必须编过。
		wantCode string
		// wantMode 只在不阻塞时校验。
		wantMode string
	}{
		{
			name: "裸 POST 在 retry 内被拒",
			src: prelude + `export workflow w(input: Q) entry -> json {
    retry (2) {
        auto r = tool.call<json>(url = "https://x/pay", method = "POST");
        return r;
    }
}
`,
			wantCode: "E5007",
		},
		{
			name: "idempotent = false 不算证明",
			src: prelude + `export workflow w(input: Q) entry -> json {
    retry (2) {
        auto r = tool.call<json>(url = "https://x/pay", method = "POST", idempotent = false);
        return r;
    }
}
`,
			wantCode: "E5007",
		},
		{
			name: "method 不是字面量 ⇒ E2016，readonly 无从谈起",
			src: prelude + `export workflow w(input: Q) entry -> json {
    string m = "POST";
    retry (2) {
        auto r = tool.call<json>(url = "https://x/pay", method = m);
        return r;
    }
}
`,
			wantCode: "E2016",
		},
		{
			name: "method 带插值 ⇒ E2016",
			src: prelude + `export workflow w(input: Q) entry -> json {
    auto r = tool.call<json>(url = "https://x/pay", method = "{input.user_id}");
    return r;
}
`,
			wantCode: "E2016",
		},
		{
			name: "method 是字面量但不在 6 个方法内 ⇒ E2017",
			src: prelude + `export workflow w(input: Q) entry -> json {
    auto r = tool.call<json>(url = "https://x/pay", method = "TRACE");
    return r;
}
`,
			wantCode: "E2017",
		},
		{
			name: "嵌在 if 里也算 retry 子树",
			src: prelude + `export workflow w(input: Q) entry -> json {
    retry (2) {
        if (true) {
            auto r = tool.call<json>(url = "https://x/pay", method = "POST");
            return r;
        }
        return "no";
    }
}
`,
			wantCode: "E5007",
		},
		{
			name: "retry 内 parallel 的每个分支都要证明（§5.2）",
			src: prelude + `export workflow w(input: Q) entry -> string {
    retry (2) {
        auto [a, b] = parallel {
            auto x = tool.call<json>(url = "https://x/pay", method = "POST");
            auto y = "two";
            return [x, y];
        };
        return "done";
    }
}
`,
			wantCode: "E5007",
		},
		{
			name: "fallback 降级块里的调用也算 retry 子树",
			src: prelude + `export workflow w(input: Q) entry -> json {
    retry (2) {
        auto r = tool.call<json>(url = "https://x/get", method = "GET") fallback {
            auto d = tool.call<json>(url = "https://x/pay", method = "POST");
            return d;
        };
        return r;
    }
}
`,
			wantCode: "E5007",
		},
		{
			name: "幂等键来自模型输出 ⇒ 键会变",
			src: modelPrelude + `export workflow w(input: Q) entry -> json {
    auto m = model.generate<I>(prompt = "p", precision = "exact");
    auto r = tool.call<json>(url = "https://x/pay", method = "POST", idem_key = "{m.action}");
    return r;
}
`,
			wantCode: "E5009",
		},
		{
			name: "幂等键经由不纯别名 ⇒ 键会变",
			src: modelPrelude + `export workflow w(input: Q) entry -> json {
    auto m = model.generate<I>(prompt = "p", precision = "exact");
    string k = m.action;
    retry (2) {
        auto r = tool.call<json>(url = "https://x/pay", method = "POST", idem_key = "{k}");
        return r;
    }
}
`,
			wantCode: "E5009",
		},
		{
			name: "只读方法天然幂等",
			src: prelude + `export workflow w(input: Q) entry -> json {
    retry (2) {
        auto r = tool.call<json>(url = "https://x/get", method = "GET");
        return r;
    }
}
`,
			wantMode: "readonly",
		},
		{
			name: "作者声明上游幂等",
			src: prelude + `export workflow w(input: Q) entry -> json {
    retry (2) {
        auto r = tool.call<json>(url = "https://x/pay", method = "POST", idempotent = true);
        return r;
    }
}
`,
			wantMode: "idem_true",
		},
		{
			name: "纯幂等键 + input 别名",
			src: prelude + `export workflow w(input: Q) entry -> json {
    string k = input.user_id;
    retry (2) {
        auto r = tool.call<json>(url = "https://x/pay", method = "POST", idem_key = "o-{k}");
        return r;
    }
}
`,
			wantMode: "idem_key",
		},
		{
			name: "retry 外的未证明 POST 如实标 none",
			src: prelude + `export workflow w(input: Q) entry -> json {
    auto r = tool.call<json>(url = "https://x/pay", method = "POST");
    return r;
}
`,
			wantMode: "none",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, msg, outPath := compileInlineSource(t, c.src)
			if c.wantCode != "" {
				if code == 0 {
					t.Fatalf("必须阻塞构建并报 %s，实际编过了。stderr:\n%s", c.wantCode, msg)
				}
				if !strings.Contains(msg, c.wantCode) {
					t.Fatalf("诊断里没有 %s，实际:\n%s", c.wantCode, msg)
				}
				// 同一个调用点只报一条：树遍历把 next 混进 scan 会重复访问
				// 子树，于是同一行出现两条 E5007 —— 那不是「更严格」，是噪声。
				if n := strings.Count(msg, "error["+c.wantCode+"]"); n != 1 {
					t.Fatalf("%s 应恰好 1 条，实际 %d 条:\n%s", c.wantCode, n, msg)
				}
				return
			}
			if code != 0 {
				t.Fatalf("合法写法不该被拒（退出码 %d）：\n%s", code, msg)
			}
			data, err := os.ReadFile(outPath)
			if err != nil {
				t.Fatal(err)
			}
			doc, ds := ir.Load(data)
			if len(ds) != 0 {
				t.Fatalf("IR 不合法：%v", ds)
			}
			if len(doc.Tools) != 1 {
				t.Fatalf("应有 1 个 tool，实际 %d", len(doc.Tools))
			}
			if got := doc.Tools[0].Idem.Mode; got != c.wantMode {
				t.Fatalf("idem.mode 应为 %q，实际 %q", c.wantMode, got)
			}
			if c.wantMode == "idem_key" {
				if len(doc.Tools[0].Idem.Key) == 0 {
					t.Fatal("idem_key 模式必须带结构化的 key 片段")
				}
			} else if len(doc.Tools[0].Idem.Key) != 0 {
				t.Fatalf("%s 模式不该带 key", c.wantMode)
			}
		})
	}
}

// TestIdemKeyInjectionIsWired 守「证明是真的」而非只是一个标签：
// 声明了 idem_key 的调用必须同时产出 args.idem_key 的节点引用与
// tools[].idem.key 的结构化片段。只发其中一个，重试时服务端收不到
// Idempotency-Key，或审计者看不到键从哪来 —— 两种都等于假证明。
func TestIdemKeyInjectionIsWired(t *testing.T) {
	data, err := os.ReadFile("../../schemas/golden/ir-idemkey.json")
	if err != nil {
		t.Fatal(err)
	}
	doc, ds := ir.Load(data)
	if len(ds) != 0 {
		t.Fatalf("idemkey golden 不合格：%v", ds)
	}
	tool := doc.Tools[0]
	if tool.Idem.Mode != "idem_key" || len(tool.Idem.Key) == 0 {
		t.Fatalf("idem_key golden 的 idem 形状不对：%+v", tool.Idem)
	}
	var found bool
	for _, n := range doc.Workflows[0].Nodes {
		if n.K != "call_tool" {
			continue
		}
		var a ir.ToolArgs
		if err := json.Unmarshal(n.Args, &a); err != nil {
			t.Fatal(err)
		}
		if a.IdemKey == nil {
			t.Fatal("call_tool.args.idem_key 为 null：引擎无从注入幂等键")
		}
		if *a.IdemKey < 0 || *a.IdemKey >= len(doc.Workflows[0].Nodes) {
			t.Fatalf("idem_key 指向越界节点 %d", *a.IdemKey)
		}
		found = true
	}
	if !found {
		t.Fatal("golden 里没有 call_tool 节点")
	}
}
