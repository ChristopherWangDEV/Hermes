package backend

import (
	"fmt"

	"hermes/internal/ir"
	"hermes/internal/native"
)

// unitErrorSlotFallback 让 unit AOT 产物在**不链** core_impl.c 时也能单独编译链接。
//
// cmd_compile_test.go 的 driver 原先只把 unit.c + drv.c 交给 clang，链接单元里
// 没有 core_impl.c；unit.c 里的 extern 声明于是会变成 undefined symbol。weak 定义
// 解决了这件事：单独链接时由它兜底；同一链接单元里若有 core_impl.c 的强定义，
// 链接器采用强定义（Mach-O weak_def / ELF weak 都会合并），所有翻译单元共享
// 同一个槽 —— 写进 unit.c 的错误与 hermes_last_error() 读到的就是同一份。
//
// 只在本仓支持的 clang/GCC 工具链上启用；其它编译器下 extern 声明仍然要求宿主
// 提供 core_impl.c（与 `hermes cc` 路径一致，那条路径总是链 core_impl.c）。
const unitErrorSlotFallback = `
/* Standalone AOT unit: a weak definition so the shared slot resolves even when
 * this translation unit is linked WITHOUT hermes_core_impl.c. When core_impl.c
 * is present its strong definition wins and both TUs share one slot. */
#if defined(__clang__) || defined(__GNUC__)
HERMES_THREAD_LOCAL struct hermes_error_slot __attribute__((weak)) hermes_last_error_slot = { 0, 0, 0, { 0 } };
#endif
`

// GenerateUnitFunc 把一个导出函数编成 C ABI 实现（`.hbc ≙ .a` 的落地）：
//
//	hermes_status hermes_<unit>_<fn>(const hermes_buf* in, hermes_buf* out);
//
// 输入/输出都是 JSON（in 是形参值，out 是返回值）。这是**静态库成员**，
// 链接期由 C 程序直接调用——不经 hermes_rt_run 解释。
//
// 当前只支持：单形参、非递归、C 后端子集内的函数体。其余如实报错。
// GenerateUnitFunc 是默认入口：C++ 实现优先，未覆盖时如实回落 Go 版。
// 两条实现必须逐字节相同（TestUnitEmitEquivalence 盯着）。
func GenerateUnitFunc(doc *ir.Doc, unit string, fn *ir.Function) ([]byte, error) {
	return GenerateUnitFuncCpp(doc, unit, fn)
}

// GenerateUnitFuncCpp 调用 C++ 实现（native/cpp/backend_c.cpp 的 emit_unit）。
func GenerateUnitFuncCpp(doc *ir.Doc, unit string, fn *ir.Function) ([]byte, error) {
	if doc == nil || fn == nil || fn.Name == "" {
		return nil, fmt.Errorf("backend(unit): missing doc or function")
	}
	irJSON, jerr := ir.Emit(doc)
	if jerr != nil {
		return nil, jerr
	}
	return native.BackendUnitEmit(irJSON, unit, fn.Name)
}

// cToJSON 是 unit AOT 需要的 Val→JSON 序列化（与 net 运行期里的同名 helper 一致）。
// **必须与 cNetPrelude 的 hm_to_json 同源**：net 运行期与 unit AOT 输出口径分叉
// 是纯静默损坏（两份逐字重复的实现，改一处漏一处就是两种 JSON）。k=4 的 double
// 分支在两边同时加：先用 cPrelude 里的 hm_double_finite 在序列化边界拦下非有限值，
// 再用 hm_double_text 做最短往返。
const cToJSON = `
static char* hm_to_json(Val v){
  if(v.k==0){ char* b=(char*)malloc(32); snprintf(b,32,"%ld",v.i); return b; }
  if(v.k==1){ return hm_dup(v.b?"true":"false"); }
  if(v.k==3){ return hm_dup(v.s?v.s:"null"); }
  if(v.k==4){ char buf[64]; if(!hm_double_finite(v.d)){ hm_set_error(1,"non-finite double at JSON boundary"); return hm_dup("null"); } hm_double_text(v.d,buf,sizeof buf); return hm_dup(buf); }
  { const char* s=v.s?v.s:""; size_t n=strlen(s); char* b=(char*)malloc(n*6+3); size_t j=0;
    b[j++]='"';
    for(;*s;s++){ unsigned char c=(unsigned char)*s;
      if(c=='"'){ b[j++]='\\'; b[j++]='"'; }
      else if(c=='\\'){ b[j++]='\\'; b[j++]='\\'; }
      else if(c=='\n'){ b[j++]='\\'; b[j++]='n'; }
      else if(c=='\r'){ b[j++]='\\'; b[j++]='r'; }
      else if(c=='\t'){ b[j++]='\\'; b[j++]='t'; }
      else if(c<0x20){ j+=(size_t)snprintf(b+j,n*6+3-j,"\\u%04x",c); }
      else b[j++]=c; }
    b[j++]='"'; b[j]=0; return b; }
}
`
