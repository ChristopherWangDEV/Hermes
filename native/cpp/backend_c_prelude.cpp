// backend_c_prelude.inc —— 生成的 C 内嵌的最小运行期（值模型 + 运算 + JSON 序列化）。
//
// 这份文本原样来自 internal/backend/c.go 的 cPrelude，**含结尾换行**。迁移期间它是
// 唯一事实源：C++ 后端要产出与 Go 版逐字节相同的 C，prelude 就必须逐字节相同。
// 因此这里不做任何重排、不折行、不"顺手美化" —— 改一个空格，等价性测试就红。
//
// 迁移完成后（Go 版删除），本文件成为 prelude 的唯一来源。
R"HMRAW(#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <stddef.h>

typedef struct { int k; long i; int b; char* s; double d; } Val;  /* k: 0=int 1=bool 2=str 3=raw JSON 4=double */
/* Standard AOT error object. The struct has NO pointer members, so sizeof and
 * offsetof do not depend on pointer width (ILP32 and LP64 alike). The four
 * compile-time asserts below pin the layout on the CURRENT target only. A
 * 32-bit target is never actually compiled, linked or run in this repo
 * (docs/C-ABI.md: cross-compilation is not supported) -- this is "portable by
 * construction + asserted", NOT 32-bit hardware verification. */
#define HM_ERR_MSG_CAP 256
typedef struct HermesError {
  int32_t code;                  /* 0=none, 1..5 = CatchType, 8=Internal */
  int32_t status;                /* upstream HTTP status; 0 = sentinel (LANGUAGE 7.1.1) */
  char     msg[HM_ERR_MSG_CAP];  /* fixed-length, no pointer members */
} HermesError;
/* C89-safe compile-time layout asserts (no C11 _Static_assert). */
typedef char hm_assert_err_size[(sizeof(HermesError)==264)?1:-1];
typedef char hm_assert_err_code_off[(offsetof(HermesError,code)==0)?1:-1];
typedef char hm_assert_err_status_off[(offsetof(HermesError,status)==4)?1:-1];
typedef char hm_assert_err_msg_off[(offsetof(HermesError,msg)==8)?1:-1];

/* ====================================================================== *
 * PAL — Platform Abstraction Layer.
 *
 * Everything below is the ONLY place in generated code that knows which OS
 * it is on. Everything above (and every statement the backend emits) calls
 * hm_pal_* only, so adding a platform means adding one branch here instead
 * of hunting for #if blocks in twenty places.
 *
 * Targets: Windows, macOS, Linux, FreeBSD, OpenBSD, NetBSD, DragonFly.
 * Bare-metal targets never reach this file: the toolchain refuses them first
 * (E3109 for host-only nodes; a Go-runtime archive is stopped by the bare-metal
 * gate in internal/toolchain/link.go, step 3).
 *
 * That gate is not reported with a code the user can see: cmd_compile.go prints
 * Refusal.Message, never Refusal.Code, so the user reads the fixed line
 * "[ERROR] Go runtime libraries (g*) cannot be linked for bare-metal targets."
 * and nothing else.
 *
 * Do NOT write E3103 here. E3103 is the *naming* gate ("carries a Go runtime,
 * but is not called g*"), it runs only for NON-bare-metal targets, so it can
 * never be the reason a bare-metal build was refused. The bare-metal gate's
 * internal code is E3101, which collides with the E3101 the spec assigns to
 * "one workflow per compilation unit" (internal/diag/catalog.go, LANGUAGE.md
 * appendix A) -- a real defect, tracked separately, not fixed by this comment.
 * ====================================================================== */

#if defined(__GNUC__) || defined(__clang__)
#define HM_PAL_UNUSED __attribute__((unused))
#else
#define HM_PAL_UNUSED
#endif

#if defined(_WIN32)
#include <windows.h>
static void hm_pal_sleep_ms(long ms){ if (ms > 0) Sleep((DWORD)ms); }
static unsigned long long hm_pal_mono_ns(void){
  LARGE_INTEGER f, c;
  if (!QueryPerformanceFrequency(&f) || f.QuadPart == 0) return 0;
  QueryPerformanceCounter(&c);
  return (unsigned long long)((double)c.QuadPart * 1e9 / (double)f.QuadPart);
}
#elif defined(__APPLE__)
#include <time.h>
#include <pthread.h>
#include <mach/mach_time.h>
static void hm_pal_sleep_ms(long ms){
  if (ms <= 0) return;
  struct timespec ts;
  ts.tv_sec = (time_t)(ms / 1000);
  ts.tv_nsec = (long)((ms % 1000) * 1000000L);
  nanosleep(&ts, (struct timespec*)0);
}
static unsigned long long hm_pal_mono_ns(void){
  /* mach_absolute_time is the canonical monotonic clock on macOS and does not
   * depend on CLOCK_MONOTONIC availability; the timebase converts to ns. */
  static mach_timebase_info_data_t tb = {0, 0};
  if (tb.denom == 0) mach_timebase_info(&tb);
  return (unsigned long long)(mach_absolute_time() * tb.numer / tb.denom);
}
#elif defined(__linux__) || defined(__FreeBSD__) || defined(__OpenBSD__) || \
      defined(__NetBSD__) || defined(__DragonFly__) || defined(__unix__)
/* Linux / FreeBSD / OpenBSD / NetBSD / DragonFly: POSIX.1-2001 clocks.
 * All five provide CLOCK_MONOTONIC and nanosleep (<time.h>). The BSD macros are
 * listed explicitly rather than relying on __unix__ alone: __unix__ is also
 * defined on platforms this backend has never been tested on, and silently
 * compiling an untested path is worse than failing to compile. */
#include <time.h>
#include <pthread.h>
static void hm_pal_sleep_ms(long ms){
  if (ms <= 0) return;
  struct timespec ts;
  ts.tv_sec = (time_t)(ms / 1000);
  ts.tv_nsec = (long)((ms % 1000) * 1000000L);
  nanosleep(&ts, (struct timespec*)0);
}
static unsigned long long hm_pal_mono_ns(void){
  struct timespec ts;
  if (clock_gettime(CLOCK_MONOTONIC, &ts) != 0) return 0;
  return (unsigned long long)ts.tv_sec * 1000000000ULL + (unsigned long long)ts.tv_nsec;
}
#endif

/* PAL: plain mutex (the global slot table needs one; a condition variable is
 * not a substitute -- it has no "lock without waiting" operation). */
#if defined(_WIN32)
typedef CRITICAL_SECTION hm_pal_mutex_t;
HM_PAL_UNUSED static void hm_pal_mutex_init(hm_pal_mutex_t* m){ InitializeCriticalSection(m); }
HM_PAL_UNUSED static void hm_pal_mutex_lock(hm_pal_mutex_t* m){ EnterCriticalSection(m); }
HM_PAL_UNUSED static void hm_pal_mutex_unlock(hm_pal_mutex_t* m){ LeaveCriticalSection(m); }
HM_PAL_UNUSED static void hm_pal_mutex_destroy(hm_pal_mutex_t* m){ DeleteCriticalSection(m); }
#else
typedef pthread_mutex_t hm_pal_mutex_t;
HM_PAL_UNUSED static void hm_pal_mutex_init(hm_pal_mutex_t* m){ pthread_mutex_init(m, (const pthread_mutexattr_t*)0); }
HM_PAL_UNUSED static void hm_pal_mutex_lock(hm_pal_mutex_t* m){ pthread_mutex_lock(m); }
HM_PAL_UNUSED static void hm_pal_mutex_unlock(hm_pal_mutex_t* m){ pthread_mutex_unlock(m); }
HM_PAL_UNUSED static void hm_pal_mutex_destroy(hm_pal_mutex_t* m){ pthread_mutex_destroy(m); }
#endif

/* Compatibility wrapper: statement code emits hm_sleep_ms; the PAL section
 * above owns the OS detail, so the backend never changes per platform. */
static void hm_sleep_ms(long ms){ hm_pal_sleep_ms(ms); }

/* --------------------------------------------------------------------- *
 * PAL: threads.
 *
 * One signature on both sides: void* (*)(void*). Windows wants
 * DWORD WINAPI(*)(LPVOID), so the Windows branch uses a trampoline and
 * remembers fn/arg inside the handle -- the platform detail never leaks.
 * --------------------------------------------------------------------- */
typedef void* (*hm_pal_thread_fn)(void*);

#if defined(_WIN32)
typedef struct hm_pal_thread_t {
  HANDLE h;
  hm_pal_thread_fn fn;
  void* arg;
  void* ret;
} hm_pal_thread_t;

static DWORD WINAPI hm_pal_thread_tramp(LPVOID p){
  hm_pal_thread_t* t = (hm_pal_thread_t*)p;
  t->ret = t->fn(t->arg);
  return 0;
}
HM_PAL_UNUSED static int hm_pal_thread_create(hm_pal_thread_t* t, hm_pal_thread_fn fn, void* arg){
  t->fn = fn; t->arg = arg; t->ret = 0;
  t->h = CreateThread((LPSECURITY_ATTRIBUTES)0, 0, hm_pal_thread_tramp, t, 0, (LPDWORD)0);
  return t->h ? 0 : -1;
}
HM_PAL_UNUSED static int hm_pal_thread_join(hm_pal_thread_t* t, void** ret){
  if (!t->h) return -1;
  WaitForSingleObject(t->h, INFINITE);
  CloseHandle(t->h);
  t->h = 0;
  if (ret) *ret = t->ret;
  return 0;
}
#else
typedef struct hm_pal_thread_t {
  pthread_t h;
  int started;
} hm_pal_thread_t;

HM_PAL_UNUSED static int hm_pal_thread_create(hm_pal_thread_t* t, hm_pal_thread_fn fn, void* arg){
  t->started = (pthread_create(&t->h, (const pthread_attr_t*)0, fn, arg) == 0);
  return t->started ? 0 : -1;
}
HM_PAL_UNUSED static int hm_pal_thread_join(hm_pal_thread_t* t, void** ret){
  if (!t->started) return -1;
  t->started = 0;
  return pthread_join(t->h, ret) == 0 ? 0 : -1;
}
#endif

/* --------------------------------------------------------------------- *
 * PAL: dynamic libraries are intentionally NOT part of this prelude.
 *
 * An AOT artifact must resolve every symbol at link time; a project guard
 * asserts the emitted C contains no run-time symbol lookup. When a feature
 * actually needs a plug-in loader it will ship its own opt-in prelude, so
 * "no interpreter" stays a checkable property rather than a promise.
 * --------------------------------------------------------------------- */


/* --------------------------------------------------------------------- *
 * Global slot table (cross-unit dispatch).
 *
 * In-unit slots are paired at compile time (no table at all). This table is
 * for slots that live in ANOTHER compilation unit: their handler pointer is
 * unknown to the emitter, so it must be registered at module start.
 *
 * Design constraints:
 *   - statically linked list, no heap: registration order is deterministic.
 *   - guarded by ONE mutex, held only for the duration of a pointer walk
 *     (never across a handler call: calling user code under the registry lock
 *     would let a handler that emits recursively deadlock itself).
 *   - unregister by function pointer, so a module can drop its slots before
 *     its code is unmapped.
 *
 * Cross-unit handlers carry no captured state: a context built from another
 * module's locals could not outlive its frame. Captured slots stay in-unit.
 * --------------------------------------------------------------------- */
typedef struct hm_global_slot {
  unsigned long long sig;
  unsigned long long chan;     /* 0 = global route */
  int (*fn)(void*);
  void* ctx;
  struct hm_global_slot* next;
} hm_global_slot;

static hm_pal_mutex_t hm_global_lock;
static hm_global_slot* hm_global_head = 0;
static int hm_global_lock_ready = 0;

HM_PAL_UNUSED static void hm_global_init(void){
  if (!hm_global_lock_ready) { hm_pal_mutex_init(&hm_global_lock); hm_global_lock_ready = 1; }
}

/* Registration is append-only and idempotent per (sig, chan, fn): building the
 * same unit twice must not double-invoke a handler. */
HM_PAL_UNUSED static void hm_slot_register(unsigned long long sig, unsigned long long chan,
                                           int (*fn)(void*), void* ctx, hm_global_slot* storage){
  hm_global_slot* it;
  hm_global_init();
  hm_pal_mutex_lock(&hm_global_lock);
  for (it = hm_global_head; it; it = it->next) {
    if (it->sig == sig && it->chan == chan && it->fn == fn) { hm_pal_mutex_unlock(&hm_global_lock); return; }
  }
  storage->sig = sig;
  storage->chan = chan;
  storage->fn = fn;
  storage->ctx = ctx;
  storage->next = hm_global_head;
  hm_global_head = storage;
  hm_pal_mutex_unlock(&hm_global_lock);
}

HM_PAL_UNUSED static void hm_slot_unregister(int (*fn)(void*)){
  hm_global_slot** pp;
  hm_global_init();
  hm_pal_mutex_lock(&hm_global_lock);
  pp = &hm_global_head;
  while (*pp) {
    if ((*pp)->fn == fn) { *pp = (*pp)->next; continue; }
    pp = &(*pp)->next;
  }
  hm_pal_mutex_unlock(&hm_global_lock);
}

/* Dispatch to registered slots. Returns non-zero if any handler claimed the
 * signal (short-circuit). The handler call happens AFTER unlocking; see the
 * deadlock note above. Because the list can change under us, we take a
 * snapshot of matching entries first (bounded by HM_GLOBAL_SLOT_MAX). */
#define HM_GLOBAL_SLOT_MAX 64
HM_PAL_UNUSED static int hm_slot_dispatch(unsigned long long sig, unsigned long long chan){
  int (*fns[HM_GLOBAL_SLOT_MAX])(void*);
  void* ctxs[HM_GLOBAL_SLOT_MAX];
  int n = 0, i, claimed = 0;
  hm_global_slot* it;
  hm_global_init();
  hm_pal_mutex_lock(&hm_global_lock);
  for (it = hm_global_head; it && n < HM_GLOBAL_SLOT_MAX; it = it->next) {
    if (it->sig == sig && it->chan == chan) { fns[n] = it->fn; ctxs[n] = it->ctx; n++; }
  }
  hm_pal_mutex_unlock(&hm_global_lock);
  for (i = 0; i < n; i++) {
    if (fns[i] && fns[i](ctxs[i])) { claimed = 1; break; }
  }
  return claimed;
}

/* --------------------------------------------------------------------- *
 * PAL: condition variable (used by until).
 *
 * One opaque type hides both worlds:
 *   POSIX   pthread_mutex_t + pthread_cond_t   (+ CLOCK_REALTIME deadline)
 *   Windows CRITICAL_SECTION + CONDITION_VARIABLE (+ relative milliseconds)
 *
 * timeout_ms < 0 means "wait forever". The unit is milliseconds on both
 * sides; POSIX converts to an ABSOLUTE timespec, Windows takes a RELATIVE
 * millisecond count -- getting this backwards is the classic 1000x bug, so
 * the conversion lives in exactly one place per platform.
 * --------------------------------------------------------------------- */

typedef struct hm_pal_cond_t hm_pal_cond_t;

#if defined(_WIN32)
struct hm_pal_cond_t {
  CRITICAL_SECTION lock;
  CONDITION_VARIABLE cv;
};
HM_PAL_UNUSED static void hm_pal_cond_init(hm_pal_cond_t* c){
  InitializeCriticalSection(&c->lock);
  InitializeConditionVariable(&c->cv);
}
HM_PAL_UNUSED static void hm_pal_cond_destroy(hm_pal_cond_t* c){
  DeleteCriticalSection(&c->lock);
}
HM_PAL_UNUSED static void hm_pal_cond_wait(hm_pal_cond_t* c, long timeout_ms){
  DWORD ms = (timeout_ms < 0) ? INFINITE : (DWORD)timeout_ms;
  EnterCriticalSection(&c->lock);
  SleepConditionVariableCS(&c->cv, &c->lock, ms);
  LeaveCriticalSection(&c->lock);
}
HM_PAL_UNUSED static void hm_pal_cond_signal(hm_pal_cond_t* c){
  WakeConditionVariable(&c->cv);
}
HM_PAL_UNUSED static void hm_pal_cond_broadcast(hm_pal_cond_t* c){
  WakeAllConditionVariable(&c->cv);
}
/* Edge-triggered wait: clearing the flag and entering the wait must happen
 * under the SAME lock, otherwise an emit arriving in the window between them
 * is lost (the classic lost-wakeup bug). */
HM_PAL_UNUSED static void hm_pal_cond_wait_flag(hm_pal_cond_t* c, int* flag, long timeout_ms){
  DWORD ms = (timeout_ms < 0) ? INFINITE : (DWORD)timeout_ms;
  EnterCriticalSection(&c->lock);
  *flag = 0;
  while (!*flag) {
    if (!SleepConditionVariableCS(&c->cv, &c->lock, ms)) break; /* timeout */
  }
  LeaveCriticalSection(&c->lock);
}
HM_PAL_UNUSED static void hm_pal_cond_set_flag(hm_pal_cond_t* c, int* flag){
  EnterCriticalSection(&c->lock);
  *flag = 1;
  WakeAllConditionVariable(&c->cv);
  LeaveCriticalSection(&c->lock);
}
#else
#include <pthread.h>
struct hm_pal_cond_t {
  pthread_mutex_t lock;
  pthread_cond_t cv;
};
HM_PAL_UNUSED static void hm_pal_cond_init(hm_pal_cond_t* c){
  pthread_mutex_init(&c->lock, (const pthread_mutexattr_t*)0);
  pthread_cond_init(&c->cv, (const pthread_condattr_t*)0);
}
HM_PAL_UNUSED static void hm_pal_cond_destroy(hm_pal_cond_t* c){
  pthread_cond_destroy(&c->cv);
  pthread_mutex_destroy(&c->lock);
}
HM_PAL_UNUSED static void hm_pal_cond_wait(hm_pal_cond_t* c, long timeout_ms){
  pthread_mutex_lock(&c->lock);
  if (timeout_ms < 0) {
    pthread_cond_wait(&c->cv, &c->lock);
  } else {
    /* pthread_cond_timedwait takes an ABSOLUTE deadline on CLOCK_REALTIME. */
    struct timespec ts;
    long long add_ns;
    if (clock_gettime(CLOCK_REALTIME, &ts) != 0) {
      ts.tv_sec = 0;
      ts.tv_nsec = 0;
    }
    add_ns = (long long)ts.tv_nsec + (long long)(timeout_ms % 1000) * 1000000LL;
    ts.tv_sec += (time_t)(timeout_ms / 1000) + (time_t)(add_ns / 1000000000LL);
    ts.tv_nsec = (long)(add_ns % 1000000000LL);
    pthread_cond_timedwait(&c->cv, &c->lock, &ts);
  }
  pthread_mutex_unlock(&c->lock);
}
HM_PAL_UNUSED static void hm_pal_cond_signal(hm_pal_cond_t* c){
  pthread_mutex_lock(&c->lock);
  pthread_cond_signal(&c->cv);
  pthread_mutex_unlock(&c->lock);
}
HM_PAL_UNUSED static void hm_pal_cond_broadcast(hm_pal_cond_t* c){
  pthread_mutex_lock(&c->lock);
  pthread_cond_broadcast(&c->cv);
  pthread_mutex_unlock(&c->lock);
}
/* Edge-triggered: clear the flag and wait under one lock (no lost wakeup). */
HM_PAL_UNUSED static void hm_pal_cond_wait_flag(hm_pal_cond_t* c, int* flag, long timeout_ms){
  pthread_mutex_lock(&c->lock);
  *flag = 0;
  if (timeout_ms < 0) {
    while (!*flag) pthread_cond_wait(&c->cv, &c->lock);
  } else {
    struct timespec ts;
    long long add_ns;
    if (clock_gettime(CLOCK_REALTIME, &ts) != 0) { ts.tv_sec = 0; ts.tv_nsec = 0; }
    add_ns = (long long)ts.tv_nsec + (long long)(timeout_ms % 1000) * 1000000LL;
    ts.tv_sec += (time_t)(timeout_ms / 1000) + (time_t)(add_ns / 1000000000LL);
    ts.tv_nsec = (long)(add_ns % 1000000000L);
    while (!*flag) {
      if (pthread_cond_timedwait(&c->cv, &c->lock, &ts) != 0) break; /* timeout */
    }
  }
  pthread_mutex_unlock(&c->lock);
}
HM_PAL_UNUSED static void hm_pal_cond_set_flag(hm_pal_cond_t* c, int* flag){
  pthread_mutex_lock(&c->lock);
  *flag = 1;
  pthread_cond_broadcast(&c->cv);
  pthread_mutex_unlock(&c->lock);
}
#endif

#if !defined(_WIN32) && !defined(__APPLE__) && !defined(__linux__) && \
    !defined(__FreeBSD__) && !defined(__OpenBSD__) && !defined(__NetBSD__) && \
    !defined(__DragonFly__) && !defined(__unix__)
#error "Hermes PAL: unsupported platform. Add a branch in the PAL section (sleep + monotonic clock)."
#endif

/* Single error slot, zero-initialized. All helpers are static: they live only
 * in the generated main.c / unit.c, never exported into the static library. */
static HermesError hm_error = {0, 0, {0}};
/* Clear the WHOLE object: code, status and the message text. Clearing only
 * code would leave a stale status/msg for the next fallback / catch / retry. */
static void hm_clear_error(void){
  hm_error.code=0; hm_error.status=0; hm_error.msg[0]=0;
}
/* Extract the "(status N)" marker from the plain-text error emitted by the Go
 * side (engine.RunError.Error()). Returns 0 -- the LANGUAGE 7.1.1 sentinel --
 * when the marker is absent or not followed by ')'. The full original text is
 * kept in hm_error.msg; only the number is extracted here. */
static int hm_parse_status(const char* text){
  const char* p; char* end; long v;
  if(text==0) return 0;
  p=strstr(text,"(status ");
  if(!p) return 0;
  p+=8;
  v=strtol(p,&end,10);
  if(end==p || *end!=')') return 0;
  return (int)v;
}
/* Publish an error: code is given, status is parsed from the text, the text is
 * bounded-copied into msg (truncated to CAP-1 and NUL-terminated, NULL-safe).
 * No strcpy into the fixed-size array. */
static void hm_set_error(int code, const char* text){
  size_t n;
  hm_error.code=code;
  hm_error.status=hm_parse_status(text);
  if(text==0){ hm_error.msg[0]=0; return; }
  n=strlen(text);
  if(n>(size_t)(HM_ERR_MSG_CAP-1)) n=(size_t)(HM_ERR_MSG_CAP-1);
  memcpy(hm_error.msg,text,n);
  hm_error.msg[n]=0;
}
/* By-value snapshot: taken BEFORE hm_clear_error() in each catch arm so that
 * e.status can read the status of the caught error. */
static HermesError hm_error_snapshot(void){ return hm_error; }

static Val hm_int(long x){ Val v; v.k=0; v.i=x; v.b=0; v.s=0; v.d=0.0; return v; }
static Val hm_bool(int x){ Val v; v.k=1; v.i=0; v.b=x?1:0; v.s=0; v.d=0.0; return v; }
static Val hm_str(const char* x){ Val v; v.k=2; v.i=0; v.b=0; v.s=(char*)x; v.d=0.0; return v; }
/* ---- double (k=4): IEEE-754 binary64, carried as a bit pattern in IR ---- */
static Val hm_double(double x){ Val v; v.k=4; v.i=0; v.b=0; v.s=0; v.d=x; return v; }
/* Construct directly from the bit pattern: the 16 hex digits of bits are a
   big-endian numeric hexadecimal value, so memcpy reconstructs the same double
   bytes on little- and big-endian hosts alike (numeric value == big-endian
   memory order). No floating-point parsing is involved. */
static Val hm_double_from_bits(unsigned long long u){ double x; memcpy(&x,&u,sizeof(x)); return hm_double(x); }
/* JSON boundary guard: an all-ones exponent means NaN or +/-Inf, and JSON has
   no representation for either. Bit-pattern test only: no <math.h>, no libm. */
static int hm_double_finite(double x){ unsigned long long u; memcpy(&u,&x,sizeof(u)); return ((u>>52)&0x7ffULL)!=0x7ffULL; }
static double hm_as_double(Val v){
  if(v.k==4) return v.d;
  if(v.k==0) return (double)v.i;
  if(v.k==1) return (double)v.b;
  if(v.k==2||v.k==3) return strtod(v.s?v.s:"",0);
  return 0.0;
}
/* Match Go encoding/json's float64 encoding: shortest round-trip AND the same
   choice between fixed and exponent notation. %.17g first fixes that choice,
   then the %.*g loop finds the fewest significant digits that still round-trip;
   without it 1000 would be written as 1e+03 (Go writes 1000), so hermes run
   and hermes compile would diverge. No %f (loses precision), no libm. */
static void hm_double_text(double x, char* out, size_t cap){
  char full[64], tmp[64];
  int use_e, p;
  snprintf(full,sizeof full,"%.17g",x);
  use_e=(strchr(full,'e')!=0)?1:0;
  for(p=1;p<=17;p++){
    snprintf(tmp,sizeof tmp,"%.*g",p,x);
    if(strtod(tmp,0)!=x) continue;
    if(((strchr(tmp,'e')!=0)?1:0)!=use_e) continue;
    snprintf(out,cap,"%s",tmp);
    break;
  }
  if(p>17) snprintf(out,cap,"%s",full);
  { size_t n=strlen(out);
    if(n>=4 && out[n-4]=='e' && out[n-3]=='-' && out[n-2]=='0'){
      out[n-2]=out[n-1]; out[n-1]=0; } }
}
/* Same semantics as the engine's math.Mod (a - trunc(a/b)*b); does not depend
   on libm, so no target needs -lm. For |q| >= 2^52 the quotient is already an
   integer and need not go through long long. */
static double hm_fmod(double a,double b){
  double q,t;
  if(b==0.0){ double z=0.0; return z/z; }
  q=a/b;
  if(q>=4503599627370496.0||q<=-4503599627370496.0) t=q;
  else t=(double)(long long)q;
  return a-t*b;
}
static char* hm_dup(const char* s){ size_t n=strlen(s)+1; char* r=(char*)malloc(n); memcpy(r,s,n); return r; }
static int hm_truthy(Val v){ if(v.k==1) return v.b; if(v.k==0) return v.i!=0; if(v.k==4) return v.d!=0.0; return (v.s&&v.s[0])?1:0; }
static char* hm_str_of(Val v){
  if(v.k==2) return v.s? v.s : "";
  if(v.k==1) return hm_dup(v.b?"true":"false");
  if(v.k==4){ char buf[64]; hm_double_text(v.d,buf,sizeof buf); return hm_dup(buf); }
  { char buf[32]; snprintf(buf,sizeof buf,"%ld",v.i); return hm_dup(buf); }
}
static Val hm_cat(Val a, Val b){
  char* sa=hm_str_of(a); char* sb=hm_str_of(b);
  size_t n=strlen(sa)+strlen(sb)+1; char* r=(char*)malloc(n);
  strcpy(r,sa); strcat(r,sb);
  return hm_str(r);
}
/* ---- JSON value (S2): kind 3 = raw JSON text. Enough for tool/model results and input. ---- */
static Val hm_jsonv(const char* x){ Val v; v.k=3; v.i=0; v.b=0; v.s=(char*)x; v.d=0.0; return v; }
static const char* hm_ws(const char* p){ while(*p==' '||*p=='\t'||*p=='\n'||*p=='\r') ++p; return p; }
/* Read one JSON value from the start of c: scalar -> int/bool/str, object/array -> raw JSON (k=3). */
static Val hm_value_at(const char* c){
  c = hm_ws(c);
  if(*c=='"'){
    const char* s=c+1; const char* q=s;
    while(*q && *q!='"'){ if(*q=='\\' && q[1]) q+=2; else ++q; }
    { size_t n=(size_t)(q-s); char* b=(char*)malloc(n+1); memcpy(b,s,n); b[n]=0; return hm_str(b); }
  }
  if(*c=='{'||*c=='['){
    char op=*c, cl=(op=='{')?'}':']'; int depth=0; const char* q=c;
    while(*q){
      if(*q=='"'){ ++q; while(*q){ if(*q=='\\'&&q[1]) q+=2; else if(*q=='"'){ ++q; break; } else ++q; } continue; }
      if(*q==op) ++depth; else if(*q==cl){ --depth; if(depth==0){ ++q; break; } }
      ++q;
    }
    { size_t n=(size_t)(q-c); char* b=(char*)malloc(n+1); memcpy(b,c,n); b[n]=0; return hm_jsonv(b); }
  }
  { const char* q=c; while(*q && *q!=',' && *q!='}' && *q!=']' && *q!=' ' && *q!='\n' && *q!='\r' && *q!='\t') ++q;
    { size_t n=(size_t)(q-c); char* b=(char*)malloc(n+1); Val r;
      memcpy(b,c,n); b[n]=0;
      if(n==4&&memcmp(b,"true",4)==0){ r=hm_bool(1); } else if(n==5&&memcmp(b,"false",5)==0){ r=hm_bool(0); }
      else if(n==4&&memcmp(b,"null",4)==0){ r=hm_str(""); } else { r=hm_int(strtol(b,0,10)); }
      free(b); return r; } }
}
static Val hm_field(Val obj, const char* name){
  if(obj.k!=3 || !obj.s) return hm_str("");
  { const char* p=obj.s; size_t nl=strlen(name);
    while((p=strchr(p,'"'))!=0){
      const char* key=p+1; const char* q=strchr(key,'"'); if(!q) break;
      if((size_t)(q-key)==nl && strncmp(key,name,nl)==0){
        const char* c=hm_ws(q+1);
        if(*c==':'){ c=hm_ws(c+1); return hm_value_at(c); }
      }
      p=q+1;
    } }
  return hm_str("");
}
static long hm_as_int(Val v){
  if(v.k==0) return v.i;
  if(v.k==1) return v.b;
  if(v.k==4) return (long)v.d;
  if(v.k==2||v.k==3) return strtol(v.s?v.s:"",0,10);
  return 0;
}
static const char* hm_as_str(Val v){
  if(v.k==2||v.k==3) return v.s?v.s:"";
  if(v.k==1) return v.b?"true":"false";
  if(v.k==4){ static char hm_dblbuf[64]; hm_double_text(v.d,hm_dblbuf,sizeof hm_dblbuf); return hm_dblbuf; }
  { static char hm_numbuf[32]; snprintf(hm_numbuf,sizeof hm_numbuf,"%ld",v.i); return hm_numbuf; }
}
static Val op_add(Val a,Val b){ if(a.k==2||b.k==2) return hm_cat(a,b); if(a.k==4&&b.k==4) return hm_double(a.d+b.d); return hm_int(a.i+b.i); }
static Val op_sub(Val a,Val b){ if(a.k==4&&b.k==4) return hm_double(a.d-b.d); return hm_int(a.i-b.i); }
static Val op_mul(Val a,Val b){ if(a.k==4&&b.k==4) return hm_double(a.d*b.d); return hm_int(a.i*b.i); }
static Val op_div(Val a,Val b){ if(a.k==4&&b.k==4) return hm_double(a.d/b.d); if(b.i==0){ hm_set_error(1,"division by zero"); return hm_int(0); } return hm_int(a.i/b.i); }
static Val op_mod(Val a,Val b){ if(a.k==4&&b.k==4) return hm_double(hm_fmod(a.d,b.d)); if(b.i==0){ hm_set_error(1,"division by zero"); return hm_int(0); } return hm_int(a.i%b.i); }
static Val op_eq(Val a,Val b){
  if(a.k==2||b.k==2) return hm_bool((a.k==2&&b.k==2&&strcmp(a.s?a.s:"",b.s?b.s:"")==0)?1:0);
  if(a.k==1||b.k==1) return hm_bool(hm_truthy(a)==hm_truthy(b));
  if(a.k==4&&b.k==4) return hm_bool(a.d==b.d);
  return hm_bool(a.i==b.i);
}
static Val op_ne(Val a,Val b){ Val e=op_eq(a,b); return hm_bool(!e.b); }
static Val op_lt(Val a,Val b){ if(a.k==4&&b.k==4) return hm_bool(a.d<b.d); return hm_bool(a.i<b.i); }
static Val op_le(Val a,Val b){ if(a.k==4&&b.k==4) return hm_bool(a.d<=b.d); return hm_bool(a.i<=b.i); }
static Val op_gt(Val a,Val b){ if(a.k==4&&b.k==4) return hm_bool(a.d>b.d); return hm_bool(a.i>b.i); }
static Val op_ge(Val a,Val b){ if(a.k==4&&b.k==4) return hm_bool(a.d>=b.d); return hm_bool(a.i>=b.i); }
static Val op_and(Val a,Val b){ return hm_bool(hm_truthy(a)&&hm_truthy(b)); }
static Val op_or(Val a,Val b){ return hm_bool(hm_truthy(a)||hm_truthy(b)); }
static Val op_not(Val a){ return hm_bool(!hm_truthy(a)); }
static Val op_neg(Val a){ if(a.k==4) return hm_double(-a.d); return hm_int(-a.i); }
static Val hm_parse_input(const char* s){
  if(s==0) return hm_int(0);
  { const char* p=hm_ws(s); if(*p=='{'||*p=='[') return hm_jsonv(s); }
  if(strcmp(s,"true")==0) return hm_bool(1);
  if(strcmp(s,"false")==0) return hm_bool(0);
  if(s[0]=='"'){ size_t n=strlen(s); char* b=(char*)malloc(n); memcpy(b,s+1,n-2); b[n-2]=0; return hm_str(b); }
  return hm_int(strtol(s,0,10));
}
static void hm_print_json(Val v){
  if(v.k==0){ printf("%ld",v.i); return; }
  if(v.k==1){ printf("%s", v.b?"true":"false"); return; }
  if(v.k==4){ char buf[64]; hm_double_text(v.d,buf,sizeof buf); printf("%s",buf); return; }
  { const char* s=v.s?v.s:""; printf("\""); for(;*s;s++){ unsigned char c=(unsigned char)*s;
    if(c=='"') printf("\\\"");
    else if(c=='\\') printf("\\\\");
    else if(c=='\n') printf("\\n");
    else if(c=='\r') printf("\\r");
    else if(c=='\t') printf("\\t");
    else if(c<0x20) printf("\\u%04x",c);
    else putchar(c); } printf("\""); }
}
)HMRAW"
