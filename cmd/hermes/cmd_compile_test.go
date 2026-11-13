package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"hermes/internal/backend"
	"hermes/internal/header"
	"hermes/internal/ir"
)

// TestCompileProducesRunnableBinary 是 AOT 的验收：
// `.hermes` → 原生可执行文件 → 跑出与 `hermes run` 逐字一致的 stdout。
//
// 产物不再链接 Go 运行时（零解释器），所以这里不需要 libhermesrt.a；
// 只要求本机有 clang。
func TestCompileProducesRunnableBinary(t *testing.T) {
	if _, err := exec.LookPath("clang"); err != nil {
		t.Skip("没有 clang")
	}
	out := filepath.Join(t.TempDir(), "prog")
	if code := run([]string{"compile", "testdata/hello.hermes", "-o", out}); code != 0 {
		t.Fatalf("compile 退出码 %d", code)
	}
	got, err := exec.Command(out).Output()
	if err != nil {
		t.Fatalf("运行产物失败: %v", err)
	}
	// `hermes run` 的 stdout 信封：{"steps":N,"value":…,"workflow":…}
	want := `{"steps":3,"value":"hi","workflow":"hello"}` + "\n"
	if string(got) != want {
		t.Fatalf("产物 stdout = %q，期望 %q", got, want)
	}
}

// TestCompileFixturesMatchInterpreter 逐 fixture 证明：C 后端产物的 stdout
// 与 `hermes run`（解释器）逐字一致。这是"换后端不丢特性"的回归资产。
func TestCompileFixturesMatchInterpreter(t *testing.T) {
	if _, err := exec.LookPath("clang"); err != nil {
		t.Skip("没有 clang")
	}
	cases := []struct{ file, input, want string }{
		{"testdata/hello.hermes", "", `{"steps":3,"value":"hi","workflow":"hello"}` + "\n"},
		{"testdata/arrays.hermes", "", `{"steps":2,"value":"ok","workflow":"w"}` + "\n"},
		{"testdata/typed.hermes", "", `{"steps":2,"value":"ok","workflow":"typed"}` + "\n"},
		{"testdata/control.hermes", "", `{"steps":6,"value":"r","workflow":"ctl"}` + "\n"},
		{"testdata/loop.hermes", "", `{"steps":32,"value":"4","workflow":"loopy"}` + "\n"},
		{"testdata/trydiv.hermes", "", `{"steps":7,"value":"caught","workflow":"tc"}` + "\n"},
		{"testdata/retrydiv.hermes", "", `{"steps":15,"value":"retried","workflow":"rt"}` + "\n"},
		{"testdata/charlit.hermes", "", `{"steps":6,"value":"72:100000","workflow":"ch"}` + "\n"},
		{"testdata/width.hermes", "", `{"steps":5,"value":"44:-25536:100000","workflow":"ov"}` + "\n"},
		{"testdata/fallback.hermes", "", `{"steps":6,"value":"42","workflow":"fb"}` + "\n"},
		{"testdata/fallbackok.hermes", "", `{"steps":3,"value":"3","workflow":"fb2"}` + "\n"},
		{"testdata/binop.hermes", "2", `{"steps":12,"value":"same:1","workflow":"calc"}` + "\n"},
		{"testdata/cast.hermes", `{"total":7}`, `{"steps":2,"value":7,"workflow":"f"}` + "\n"},
	}
	for _, c := range cases {
		t.Run(filepath.Base(c.file), func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "prog")
			if code := run([]string{"compile", c.file, "-o", out}); code != 0 {
				t.Fatalf("compile 退出码 %d", code)
			}
			var got []byte
			var err error
			if c.input == "" {
				got, err = exec.Command(out).Output()
			} else {
				got, err = exec.Command(out, c.input).Output()
			}
			if err != nil {
				t.Fatalf("运行产物失败: %v", err)
			}
			if string(got) != c.want {
				t.Fatalf("stdout = %q，期望 %q", got, c.want)
			}
		})
	}
}

// TestCompileIsDeterministic 守"同 IR 两次代码生成字节相等"。
//
// 代码生成必须是一个纯函数：同一份 IR 两次 Generate 得到逐字相同的 C。
// 产物是分发物，不确定性意味着同一份源码在两次构建里得到两个哈希，而签名
// 与供应链校验都建立在那份字节上。
func TestCompileIsDeterministic(t *testing.T) {
	src, err := os.ReadFile("testdata/hello.hermes")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := buildIR("testdata/hello.hermes", src)
	if err != nil {
		t.Fatalf("buildIR: %v", err)
	}
	be, ok := backend.For("c")
	if !ok {
		t.Fatal("找不到 c 后端")
	}
	a, err := be.Generate(doc)
	if err != nil {
		t.Fatalf("Generate #1: %v", err)
	}
	b, err := be.Generate(doc)
	if err != nil {
		t.Fatalf("Generate #2: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("两次代码生成不一致：同一份 IR 必须产出逐字相同的 C")
	}
}

// TestCheckCompiledStaticRejectsNonRecursiveCall 守 AOT 的静态性：
// 产物里指向非递归函数的 call_fn 一律拒绝；递归调用允许（那是唯一动态路径）。
func TestCheckCompiledStaticRejectsNonRecursiveCall(t *testing.T) {
	name := "f"
	doc := &ir.Doc{
		Functions: []ir.Function{{Name: name, Recursive: false}},
		Workflows: []ir.Workflow{{
			Name:  "w",
			Nodes: []ir.Node{{ID: 0, K: "call_fn", Fn: &name}},
		}},
	}
	if err := checkCompiledStatic(doc); err == nil {
		t.Fatal("非递归 call_fn 必须被拒绝：那是运行期动态派发")
	}
	doc.Functions[0].Recursive = true
	if err := checkCompiledStatic(doc); err != nil {
		t.Fatalf("递归 call_fn 应被允许（深度上限兜底），实际：%v", err)
	}
}

// TestCompileNativeToolCall 证 S2 的整条链在**原生产物**里跑通：
// C 后端发 hermes_net_tool → glibhermesnet（Go, g* 命名空间）→ 真 HTTP → 结果/错误槽。
// 200 → "ok"；500 → try/catch(HttpError) 捕获 → "caught"。
func TestCompileNativeToolCall(t *testing.T) {
	if _, err := exec.LookPath("clang"); err != nil {
		t.Skip("没有 clang")
	}
	root, _ := filepath.Abs("../..")
	lib := filepath.Join(root, "build", "glibhermesnet.a")
	if _, err := os.Stat(lib); err != nil {
		t.Skipf("没有 %s（go build -buildmode=c-archive -o build/glibhermesnet.a ./cmd/hermesnet）", lib)
	}
	t.Setenv("HERMES_NET_LIB", lib)
	t.Setenv("HERMES_NET_HEADER", filepath.Join(root, "native", "include", "hermes_net.h"))

	src := `#link <std/net>

export struct OrderInfo {
    string id;
}

export workflow te(input: json) entry -> string {
    auto u = json.as_string(input.url);
    try {
        auto r = tool.call<OrderInfo>(url = "{u}", method = "GET");
        return "ok";
    } catch (HttpError e) {
        return "caught";
    }
}
`
	dir := t.TempDir()
	file := filepath.Join(dir, "t.hermes")
	if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "t")
	if code := run([]string{"compile", file, "-o", bin}); code != 0 {
		t.Fatalf("compile 退出码 %d", code)
	}

	cases := []struct {
		status int
		want   string
	}{{200, `"value":"ok"`}, {500, `"value":"caught"`}}
	for _, c := range cases {
		code := c.status
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"id":"x"}`))
		}))
		inp := fmt.Sprintf(`{"url":%q}`, ts.URL+"/")
		out, err := exec.Command(bin, inp).Output()
		ts.Close()
		if err != nil {
			t.Fatalf("HTTP %d 运行失败: %v", code, err)
		}
		if !strings.Contains(string(out), c.want) {
			t.Fatalf("HTTP %d: stdout=%q，期望包含 %q", code, out, c.want)
		}
	}
}

// TestUnitFuncAOT 证 `.hbc ≙ .a` 的第一步：一个导出的 Hermes 函数被编成原生
// C 符号 hermes_<unit>_<fn>，由普通 C 驱动**直接调用**（不经 hermes_rt_run）。
func TestUnitFuncAOT(t *testing.T) {
	if _, err := exec.LookPath("clang"); err != nil {
		t.Skip("没有 clang")
	}
	src := `export int f(int x) {
    return x + 1;
}

export workflow w() entry -> string {
    return "ok";
}
`
	dir := t.TempDir()
	file := filepath.Join(dir, "u.hermes")
	if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	doc, err := buildIR(file, []byte(src))
	if err != nil {
		t.Fatalf("buildIR: %v", err)
	}
	var f *ir.Function
	for i := range doc.Functions {
		if doc.Functions[i].Name == "f" {
			f = &doc.Functions[i]
			break
		}
	}
	if f == nil {
		t.Fatal("函数 f 不在 functions[] 里")
	}
	csrc, err := backend.GenerateUnitFunc(doc, "u", f)
	if err != nil {
		t.Fatalf("GenerateUnitFunc: %v", err)
	}
	if strings.Contains(string(csrc), "hermes_rt_run") {
		t.Fatal("单元 AOT 产物不许引用 hermes_rt_run（那就是解释器）")
	}
	unitc := filepath.Join(dir, "unit.c")
	if err := os.WriteFile(unitc, csrc, 0o644); err != nil {
		t.Fatal(err)
	}
	driver := filepath.Join(dir, "drv.c")
	if err := os.WriteFile(driver, []byte(`#include <stdio.h>
#include <string.h>
#include "hermes/hermes_core.h"
extern hermes_status hermes_u_f(const hermes_buf* in, hermes_buf* out);
int main(void){
  char* in = "41";
  hermes_buf ib; ib.ptr = in; ib.len = strlen(in);
  hermes_buf ob; ob.ptr = 0; ob.len = 0;
  hermes_status st = hermes_u_f(&ib, &ob);
  if(st != 0){ printf("err %d\n", (int)st); return 1; }
  fwrite(ob.ptr, 1, ob.len, stdout); printf("\n"); return 0;
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	root, _ := filepath.Abs("../..")
	// core_impl.c 由 header.CoreImpl 写出，和 `hermes cc` 用的是同一份字符串常量。
	// unit.c 自带一份 weak 槽定义，单独链接也能过；这里再带上强定义，是为了让
	// driver 能直接调 hermes_last_error()，并证明 weak/strong 合并后共享同一个槽。
	corec := filepath.Join(dir, "hermes_core_impl.c")
	if err := os.WriteFile(corec, []byte(header.CoreImpl), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "prog")
	if out, err := exec.Command("clang", "-std=c99", "-I", filepath.Join(root, "include"),
		unitc, corec, driver, "-o", bin).CombinedOutput(); err != nil {
		t.Fatalf("clang: %v\n%s", err, out)
	}
	got, err := exec.Command(bin).Output()
	if err != nil {
		t.Fatalf("运行失败: %v", err)
	}
	if strings.TrimSpace(string(got)) != "42" {
		t.Fatalf("产物输出 %q，期望 42", strings.TrimSpace(string(got)))
	}
}

// TestUnitFuncAOTLastErrorSlot 是 unit AOT 路径的端到端验收：产物失败后，同进程
// 调 hermes_last_error() 必须读到写进共享槽的真实错误文本；成功调用后入口清槽
// 生效（len == 0）；槽用 len 表达长度，内嵌 NUL 不会被静默截断。
//
// 不是「生成的 C 里含某字符串」：这里真编、真链、真跑，输出由 C 程序打印。
func TestUnitFuncAOTLastErrorSlot(t *testing.T) {
	if _, err := exec.LookPath("clang"); err != nil {
		t.Skip("没有 clang")
	}
	// f(41) 走 x / 0（op_div 置 hm_error → goto hm_fail）；f(-1) 走 x + 1。
	// 一个函数同时给出失败与成功两条路径，才能在同一次链接里验入口清槽。
	src := `export int f(int x) {
    auto z = 0;
    if (x > 0) {
        return x / z;
    }
    return x + 1;
}

export workflow w() entry -> string {
    return "ok";
}
`
	dir := t.TempDir()
	file := filepath.Join(dir, "u.hermes")
	if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	doc, err := buildIR(file, []byte(src))
	if err != nil {
		t.Fatalf("buildIR: %v", err)
	}
	var f *ir.Function
	for i := range doc.Functions {
		if doc.Functions[i].Name == "f" {
			f = &doc.Functions[i]
			break
		}
	}
	if f == nil {
		t.Fatal("函数 f 不在 functions[] 里")
	}
	csrc, err := backend.GenerateUnitFunc(doc, "u", f)
	if err != nil {
		t.Fatalf("GenerateUnitFunc: %v", err)
	}
	generated := string(csrc)
	for _, want := range []string{
		"hermes_last_error_clear();",
		"hermes_last_error_publish((int32_t)hm_e.code, (int32_t)hm_e.status",
		"hermes_last_error_bounded_len(hm_e.msg)",
		"HERMES_THREAD_LOCAL extern struct hermes_error_slot hermes_last_error_slot;",
		"__attribute__((weak)) hermes_last_error_slot",
		"return HERMES_ERR_INTERNAL; }",
	} {
		if !strings.Contains(generated, want) {
			t.Errorf("unit AOT 产物缺少 %q", want)
		}
	}
	if strings.Contains(generated, "(void)hm_e;") {
		t.Error("unit AOT 产物仍在丢弃 hm_e 快照，错误没有发布到共享槽")
	}

	unitc := filepath.Join(dir, "unit.c")
	if err := os.WriteFile(unitc, csrc, 0o644); err != nil {
		t.Fatal(err)
	}
	corec := filepath.Join(dir, "hermes_core_impl.c")
	if err := os.WriteFile(corec, []byte(header.CoreImpl), 0o644); err != nil {
		t.Fatal(err)
	}
	driver := filepath.Join(dir, "drv.c")
	if err := os.WriteFile(driver, []byte(`#include <stdio.h>
#include <string.h>
#include "hermes/hermes_core.h"

/* 与生成产物逐字一致的槽布局；driver 直接写它来验内嵌 NUL 的 len 语义。 */
#define HERMES_LAST_ERROR_CAP 256
struct hermes_error_slot {
  int32_t code;
  int32_t status;
  size_t len;
  char msg[HERMES_LAST_ERROR_CAP];
};
HERMES_THREAD_LOCAL extern struct hermes_error_slot hermes_last_error_slot;

extern hermes_status hermes_u_f(const hermes_buf* in, hermes_buf* out);

int main(void){
  hermes_buf ib;
  hermes_buf ob;
  hermes_str e;
  hermes_status st;
  const char* bad = "41";
  const char* good = "-1";

  ib.ptr = (char*)bad; ib.len = strlen(bad);
  ob.ptr = 0; ob.len = 0;
  st = hermes_u_f(&ib, &ob);
  if (st == 0) { printf("BAD_FAIL_STATUS\n"); return 1; }
  e = hermes_last_error();
  if (e.ptr == 0 || e.len == 0 || e.ptr[0] == 0) { printf("BAD_EMPTY\n"); return 1; }
  printf("FAIL_REASON:%.*s\n", (int)e.len, e.ptr);
  printf("FAIL_LEN_OK:%d\n", (int)(e.len > 0));

  /* 入口清槽：成功调用后必须 len == 0 且 ptr 指向非 NULL 的空串。 */
  ib.ptr = (char*)good; ib.len = strlen(good);
  ob.ptr = 0; ob.len = 0;
  st = hermes_u_f(&ib, &ob);
  if (st != 0) { printf("BAD_OK_STATUS\n"); return 1; }
  e = hermes_last_error();
  printf("OK_LEN_ZERO:%d\n", (int)(e.len == 0 && e.ptr != 0 && e.ptr[0] == 0));
  hermes_buf_free(&ob);

  /* hermes_str 用 len 表达长度，不靠 C 字符串的 NUL 结尾。 */
  hermes_last_error_slot.code = 0;
  hermes_last_error_slot.status = 0;
  memcpy(hermes_last_error_slot.msg, "a\0bcd", 5);
  hermes_last_error_slot.msg[5] = 0;
  hermes_last_error_slot.len = 5;
  e = hermes_last_error();
  printf("NUL_LEN:%d NUL_BYTES:%d,%d,%d,%d,%d\n", (int)e.len,
    (int)(unsigned char)e.ptr[0], (int)(unsigned char)e.ptr[1],
    (int)(unsigned char)e.ptr[2], (int)(unsigned char)e.ptr[3],
    (int)(unsigned char)e.ptr[4]);
  return 0;
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	root, _ := filepath.Abs("../..")
	bin := filepath.Join(dir, "prog")
	if out, err := exec.Command("clang", "-std=c99", "-I", filepath.Join(root, "include"),
		unitc, corec, driver, "-o", bin).CombinedOutput(); err != nil {
		t.Fatalf("clang（unit.c + core_impl.c + drv.c）: %v\n%s", err, out)
	}
	got, err := exec.Command(bin).CombinedOutput()
	if err != nil {
		t.Fatalf("运行失败: %v\n%s", err, got)
	}
	s := string(got)
	if strings.Contains(s, "BAD_") {
		t.Fatalf("C 驱动自检失败:\n%s", s)
	}
	for _, want := range []string{
		"FAIL_REASON:division by zero",
		"FAIL_LEN_OK:1",
		"OK_LEN_ZERO:1",
		"NUL_LEN:5 NUL_BYTES:97,0,98,99,100",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("输出缺少 %q，实际:\n%s", want, s)
		}
	}

	// 单独编译链接（不链 core_impl.c）：unit.c 自带的 weak 槽定义必须让这个
	// 链接单元自洽，否则 .hbc ≙ .a 的宿主会在链接期冒出 undefined symbol。
	standalone := filepath.Join(dir, "standalone.c")
	if err := os.WriteFile(standalone, []byte(`#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include "hermes/hermes_core.h"
extern hermes_status hermes_u_f(const hermes_buf* in, hermes_buf* out);
int main(void){
  hermes_buf ib;
  hermes_buf ob;
  const char* s = "-1";
  ib.ptr = (char*)s; ib.len = strlen(s);
  ob.ptr = 0; ob.len = 0;
  if (hermes_u_f(&ib, &ob) != 0) { return 1; }
  printf("STANDALONE:%.*s\n", (int)ob.len, ob.ptr);
  free(ob.ptr);
  return 0;
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	bin2 := filepath.Join(dir, "prog_standalone")
	if out, err := exec.Command("clang", "-std=c99", "-I", filepath.Join(root, "include"),
		unitc, standalone, "-o", bin2).CombinedOutput(); err != nil {
		t.Fatalf("unit.c 单独链接（无 core_impl.c）失败: %v\n%s", err, out)
	}
	got2, err := exec.Command(bin2).Output()
	if err != nil || !strings.Contains(string(got2), "STANDALONE:0") {
		t.Fatalf("单独链接产物运行异常: %v\n%s", err, got2)
	}
}
