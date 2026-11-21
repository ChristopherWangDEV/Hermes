package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hermes/internal/engine"
	"hermes/internal/runtime"
)

// TestErrFieldStatusIsReadable 是 §7.1.1 / D-014 的**端到端**证明。
//
// 要证的是一条曾经写不出来的规范承诺：`catch (HttpError e) { if (e.status == 404) }`。
// 此前它报 `E4016: cannot access field status on unknown` —— 而 §7.1 与
// `examples/support_bot/README.md` 都在推荐这个写法。
//
// 为什么必须是端到端而不是只测前端：这条链有三段，任何一段断掉都会退化成
// "编得过但读不到"：
//
//	① 前端按 §7.1.1 给 `e.status` 一个 int 类型（否则 E4016）；
//	② codegen 把字段访问编成 `field` 节点（否则 IR 里没有这次读取）；
//	③ 引擎把真实状态码放进 catch 绑定的错误值（否则读到 0 / 缺键）。
//
// 只测 ① 的测试在 ③ 断掉时仍然全绿，而症状是"分支永远不进"——最难查的一类。
//
// 对照组的做法：**换服务端的真实状态码**再跑一遍。第一版写的是
// `e.status == 0` 必须不命中 —— 那条对"把 status 写成常量 404"是无效的，
// 常量 404 同样不命中 0。换成 (503, 比 404) 之后，任何把状态码写死成 404 的
// 实现都会让对照组误命中，才真的钉住"读到的就是服务端那个值"。
func TestErrFieldStatusIsReadable(t *testing.T) {
	build := func(t *testing.T, serverStatus int, want string) *engine.Result {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(serverStatus) // 不带 body：状态码是唯一的信息
		}))
		defer srv.Close()
		dir := t.TempDir()
		src := filepath.Join(dir, "e.hermes")
		body := `#link <std/net>

export struct Order { string id; int status; }

export workflow w(input: string) entry -> string {
    try {
        auto r = tool.call<Order>(url = "` + srv.URL + `/o", method = "GET");
    } catch (HttpError e) {
        if (e.status == ` + want + `) { return "hit"; }
    }
    return "ok";
}
`
		if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		doc, err := buildIR(src, []byte(body))
		if err != nil {
			t.Fatalf("源码应能编过（e.status 可读）：%v", err)
		}
		res, err := engine.RunWith(doc, "", nil, engine.RunOptions{
			Runtime: &runtime.Runtime{},
		})
		if err != nil {
			t.Fatalf("执行失败: %v", err)
		}
		return res
	}

	if got := build(t, 404, "404").Value; got != "hit" {
		t.Fatalf("e.status 应是服务端返回的 404，实际分支没命中（拿到 %#v）", got)
	}
	// 对照 ①：服务端给 503，同一个 `== 404` 必须不命中（排除"写死成 404"）。
	if got := build(t, 503, "404").Value; got != "ok" {
		t.Fatalf("服务端返回 503 时 `e.status == 404` 不该命中，实际 %#v", got)
	}
	// 对照 ②：同一个 503 服务端，比 503 必须命中 —— 与上一条合起来才排除
	// "什么都不命中"这种同样错误的实现。
	if got := build(t, 503, "503").Value; got != "hit" {
		t.Fatalf("服务端返回 503 时 `e.status == 503` 应命中，实际 %#v", got)
	}
}

// TestErrFieldNotYetIsRefused 守 E4034：规范里有、运行期还没带的字段**必须拒**。
//
// 这条比"能读 status"更重要：它是这条改动不变成"编得过、运行期炸"的唯一保证。
// `field "url" does not exist` 是普通 error，不是 *RunError，`catch` 接不住 ——
// 也就是说放行的话，用户会遇到一个连 try 都兜不住的运行期失败。
func TestErrFieldNotYetIsRefused(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "u.hermes")
	const body = `export workflow w(input: string) entry -> string {
    try { auto a = 1; } catch (HttpError e) { return e.url; }
    return "ok";
}
`
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := captureStderr(t, func() int {
		return run([]string{"build", src, "-o", filepath.Join(dir, "u.hbc")})
	})
	if code == 0 {
		t.Fatalf("E4034 是硬错，构建应失败：\n%s", out)
	}
	if !strings.Contains(out, "error[E4034]") {
		t.Fatalf("应报 E4034（而不是 E4016 —— 那是在说「没有这个字段」）：\n%s", out)
	}
	if strings.Contains(out, "error[E4016]") {
		t.Fatalf("同一处不该同时报 E4016：\n%s", out)
	}
	// 改法必须真的能写：`allow_status` 那条是这条诊断的出口。
	if !strings.Contains(out, "allow_status") {
		t.Fatalf("E4034 的 help 应给出可写的替代路径：\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "u.hbc")); err == nil {
		t.Fatal("失败的构建不该写出归档")
	}
}
