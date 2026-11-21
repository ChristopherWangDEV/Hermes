// backend_c_net_prelude.cpp —— 生成的 C 里的网络运行期段（只在用到 call_model /
// call_tool 时追加）。
//
// 这份文本原样来自 internal/backend/c.go 的 cNetPrelude，**含结尾换行**。
// 迁移期间它是唯一事实源：C++ 后端要产出与 Go 版逐字节相同的 C，这一段就必须
// 逐字节相同。它 #include "hermes_net.h" 并引用 hermes_net_*，因此产物在链接期
// 需要 glibhermesnet.a —— 那是唯一允许带 Go runtime 的 g* 归档（软件层网络库）。
R"HMRAW(
#include "hermes_net.h"

/* Serialize Val to JSON text (malloc'd): int -> number, bool -> true/false,
   str -> escaped quoted string, JSON(k=3) -> verbatim, double(k=4) -> shortest
   round-trip number. Non-finite doubles are rejected at this JSON boundary and
   never emitted as inf/nan text. */
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

/* Publish an out-parameter error object into the global hm_error slot BY VALUE.
   This is the ONLY bridge from the net boundary's HermesError* out parameter to
   the single global error slot that try/catch/fallback/retry dispatch on. The
   net boundary itself never writes the global; dropping this call (or the out
   parameter) makes every network failure invisible to the dispatch logic. */
static void hm_publish_error(const HermesError* e){
  if(e) hm_error=*e;
}

/* Internal boundary: status-code return + HermesError* out parameter.
   Calls the frozen C ABI (hermes_net_tool / hermes_net_model). On failure it
   builds a LOCAL HermesError (code=st, status parsed from the plain-text err by
   hm_parse_status, msg=bounded copy of the full err text) and hands it back
   ONLY through *out_err -- it does NOT touch the global hm_error. The caller
   MUST publish it with hm_publish_error(&out_err) to feed the dispatch logic.
   On success it hands the result JSON to *out and returns 0. */
static int hm_net_invoke(int is_model, const char* req, size_t len, char** out, HermesError* out_err){
  char* res=0; char* err=0; int st;
  *out=0;
  st = is_model ? hermes_net_model(req,len,&res,&err)
                : hermes_net_tool(req,len,&res,&err);
  if(st!=0){
    HermesError e; size_t n;
    e.code=st;
    e.status=hm_parse_status(err);
    e.msg[0]=0;
    if(err){ n=strlen(err); if(n>(size_t)(HM_ERR_MSG_CAP-1)) n=(size_t)(HM_ERR_MSG_CAP-1); memcpy(e.msg,err,n); e.msg[n]=0; }
    if(out_err) *out_err=e;
    if(err) hermes_net_free(err);
    if(res) hermes_net_free(res);
    return st;
  }
  *out=res;
  return 0;
}

/* One tool call: build request JSON -> hm_net_invoke -> on success return the
   result JSON value; on a non-zero status the caller publishes the out-parameter
   error object with hm_publish_error, so try/catch/fallback/retry can dispatch
   on it. hm_net_invoke itself never writes the global error slot. */
static Val hm_net_tool_call(Val url, Val header, Val body, const char* method, const char* allow_json, long timeout_ms, const char* workflow, const char* tool){
  char* u=hm_to_json(url); char* h=hm_to_json(header); char* b=hm_to_json(body);
  size_t cap=strlen(u)+strlen(h)+strlen(b)+strlen(method)+strlen(allow_json)+strlen(workflow)+strlen(tool)+256;
  char* req=(char*)malloc(cap);
  snprintf(req,cap,"{\"url\":%s,\"method\":\"%s\",\"header\":%s,\"body\":%s,\"allow_status\":%s,\"timeout_ms\":%ld,\"workflow\":\"%s\",\"tool\":\"%s\"}",u,method,h,b,allow_json,timeout_ms,workflow,tool);
  { char* result=0; HermesError out_err={0,0,{0}}; int st; Val r=hm_str("");
    st=hm_net_invoke(0,req,strlen(req),&result,&out_err);
    if(st!=0){ hm_publish_error(&out_err); r=hm_str(""); }
    else { r=hm_jsonv(result?hm_dup(result):hm_dup("null")); if(result) hermes_net_free(result); }
    free(u); free(h); free(b); free(req);
    return r; }
}

/* One model call: same boundary as the tool call, through hm_net_invoke. */
static Val hm_net_model_call(Val prompt, const char* precision, long max_tokens, const char* temperature, const char* schema_json, const char* backend, const char* workflow){
  char* p=hm_to_json(prompt);
  size_t cap=strlen(p)+strlen(precision)+strlen(temperature)+strlen(schema_json)+strlen(backend)+strlen(workflow)+256;
  char* req=(char*)malloc(cap);
  snprintf(req,cap,"{\"prompt\":%s,\"precision\":\"%s\",\"max_tokens\":%ld,\"temperature\":\"%s\",\"schema\":%s,\"backend\":\"%s\",\"workflow\":\"%s\"}",p,precision,max_tokens,temperature,schema_json,backend,workflow);
  { char* result=0; HermesError out_err={0,0,{0}}; int st; Val r=hm_str("");
    st=hm_net_invoke(1,req,strlen(req),&result,&out_err);
    if(st!=0){ hm_publish_error(&out_err); r=hm_str(""); }
    else { r=hm_jsonv(result?hm_dup(result):hm_dup("null")); if(result) hermes_net_free(result); }
    free(p); free(req);
    return r; }
}
)HMRAW"
