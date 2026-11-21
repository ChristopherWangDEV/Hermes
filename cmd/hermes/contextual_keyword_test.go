package main

import "testing"

// 上下文关键字的"声明优先"规则：用户定义过同名函数时，emit/stop/sleep/until
// 必须编成普通调用，而不是内置语句。
//
// 这条规则不成立时的症状是**静默的**：2.0.0 里任何叫这些名字的辅助函数，
// 一旦被当裸语句调用，就会被当成信号发射或休眠，行为悄悄变了，编译不报错。
// 所以两种方向都要钉：有同名函数 → 不能出内置节点；没有 → 必须是内置节点。
func TestContextualKeywordYieldsToUserFunction(t *testing.T) {
	cases := []struct {
		name        string
		src         string
		kind        string
		shouldExist bool
	}{
		{
			name: "user_emit_wins",
			src: `string emit(string x) { return x; }
export workflow main() entry -> string {
  auto v = "hi";
  emit(v);
  return "ok";
}
`,
			kind: "emit", shouldExist: false,
		},
		{
			name: "user_sleep_wins",
			src: `string sleep(int ms) { return "slept"; }
export workflow main() entry -> string {
  sleep(10);
  return "ok";
}
`,
			kind: "sleep", shouldExist: false,
		},
		{
			name: "user_until_wins",
			src: `string until(string s) { return s; }
export workflow main() entry -> string {
  until("x");
  return "ok";
}
`,
			kind: "until", shouldExist: false,
		},
		{
			name: "user_stop_wins",
			src: `string stop(string r) { return r; }
export workflow main() entry -> string {
  stop("x");
  return "ok";
}
`,
			kind: "stop", shouldExist: false,
		},
		{
			name: "bare_emit_is_signal",
			src: `export signal S;
export workflow main() entry -> string {
  emit(S);
  return "ok";
}
`,
			kind: "emit", shouldExist: true,
		},
		{
			name: "bare_sleep_is_builtin",
			src: `export workflow main() entry -> string {
  sleep(10);
  return "ok";
}
`,
			kind: "sleep", shouldExist: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc, err := buildIR("t.hermes", []byte(c.src))
			if err != nil {
				t.Fatalf("编译失败: %v", err)
			}
			found := false
			for i := range doc.Workflows[0].Nodes {
				if doc.Workflows[0].Nodes[i].K == c.kind {
					found = true
					break
				}
			}
			if found != c.shouldExist {
				t.Fatalf("节点 %q 存在=%v，期望=%v（声明优先规则失效）", c.kind, found, c.shouldExist)
			}
		})
	}
}
