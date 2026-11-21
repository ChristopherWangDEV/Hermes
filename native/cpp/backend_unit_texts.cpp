// backend_unit_texts.cpp —— unit AOT（`hermes cc` 的 .hbc 落地）用到的 C 片段。
//
// 这些文本原先散在 Go 的 `internal/header/core_impl.go` 与
// `internal/backend/unit.go` 里。搬到这里是**逐字节复制**（脚本抽取，非手抄），
// 因为它们是产物的一部分：改一个空格，unit.c 的字节就变，而那与语义无关。
//
// 只被 #include 或作为常量引用，不进 PARSER_SRCS。
namespace hm {
const char kUnitErrorSlotDecl[] = R"HMUNIT(/* Shared last-error slot: fixed layout, NO pointer members.
 * hermes_last_error() returns a view into msg[], which has static storage
 * duration. The caller NEVER frees it and must not keep it past the next call
 * (every entry point clears the slot). */
#define HERMES_LAST_ERROR_CAP 256
struct hermes_error_slot {
  int32_t code;
  int32_t status;
  size_t len;
  char msg[HERMES_LAST_ERROR_CAP];
};

/* code/status are raw producer-side diagnostic values. They are deliberately
 * NOT exposed by hermes_last_error() and are NEVER mapped onto hermes_status:
 * the AOT CatchType table and hermes_status are two different code spaces with
 * no forward mapping (docs/AOT-ERROR-OBJECT.md). The only public view is
 * hermes_str{ptr,len}. */
HERMES_THREAD_LOCAL extern struct hermes_error_slot hermes_last_error_slot;

/* Clear on EVERY entry point. "Valid until the next call" is an ABI promise,
 * not a courtesy: a successful call must leave len == 0. */
static void hermes_last_error_clear(void) {
  hermes_last_error_slot.code = 0;
  hermes_last_error_slot.status = 0;
  hermes_last_error_slot.len = 0;
  hermes_last_error_slot.msg[0] = 0;
}

/* Bounded raw-byte scan. Never calls strlen on an unbounded pointer and never
 * reads past CAP-1 bytes. The result is EXACTLY the number of bytes the
 * publisher will store, so len can never claim more text than the bounded
 * copy actually produced (no "strlen truncated but len still says N"). */
static size_t hermes_last_error_bounded_len(const char* s) {
  size_t n;
  n = 0;
  if (s == 0) {
    return 0;
  }
  while (n < (size_t)(HERMES_LAST_ERROR_CAP - 1) && s[n] != 0) {
    n++;
  }
  return n;
}

/* Publish by bounded COPY (memcpy, not strcpy/strlen): bytes are stored
 * verbatim, so an embedded NUL inside msg_len is preserved and hermes_str.len
 * -- NOT the terminating NUL -- is the authority on length. Always
 * NUL-terminates, so printf("%.*s") and C/Python/Go FFI readers stay safe.
 * msg_len must not exceed the readable source; callers pass the result of
 * hermes_last_error_bounded_len() or the fixed array's byte count. */
static void hermes_last_error_publish(int32_t code, int32_t status,
                                      const char* msg, size_t msg_len) {
  size_t n;
  n = msg_len;
  if (n > (size_t)(HERMES_LAST_ERROR_CAP - 1)) {
    n = (size_t)(HERMES_LAST_ERROR_CAP - 1);
  }
  hermes_last_error_slot.code = code;
  hermes_last_error_slot.status = status;
  if (n > 0 && msg != 0) {
    memcpy(hermes_last_error_slot.msg, msg, n);
  }
  hermes_last_error_slot.msg[n] = 0;
  hermes_last_error_slot.len = n;
}
)HMUNIT";
const char kUnitErrorSlotFallback[] = R"HMUNIT(
/* Standalone AOT unit: a weak definition so the shared slot resolves even when
 * this translation unit is linked WITHOUT hermes_core_impl.c. When core_impl.c
 * is present its strong definition wins and both TUs share one slot. */
#if defined(__clang__) || defined(__GNUC__)
HERMES_THREAD_LOCAL struct hermes_error_slot __attribute__((weak)) hermes_last_error_slot = { 0, 0, 0, { 0 } };
#endif
)HMUNIT";
const char kUnitToJSON[] = R"HMUNIT(
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
)HMUNIT";
}  // namespace hm
