package header

// CoreImpl 是 `hermes_core.h` 里那几个函数的**唯一实现**，生成成一个 C 文件。
//
// 为什么不做成静态库：这几个函数加起来不到三十行，而且它们是**每一份产物都必须
// 带的**（谁调 hermes_buf_free 谁就需要它）。做成库就多一次"库版本要和头版本
// 配套"的约束，而它们之间根本没有会变的东西。
//
// 为什么不做成 header-only（static inline）：那会让每个翻译单元各有一份副本，
// 而 `hermes_buf_free` 的行为里含 malloc/free —— 跨模块各带一份是自找麻烦。
//
// 共享错误槽（`hermes_last_error_slot`）也定义在这里，但它是**变量**而不是函数：
// core_impl_test.go 的 funcDefRe 会把 CoreImpl 里每个函数定义都当成"必须在
// hermes_core.h 里有声明"的候选，而新增导出函数属于 1.0→1.1 的 minor bump
// （docs/C-ABI.md），本次不改公开 ABI。槽是 external-linkage 变量，不新增符号面
// 上的函数，且三份生成 C（core_impl.c / unit_shim.c / unit.c）共享同一个槽。
const CoreImpl = coreImplHead + cErrorSlotStruct + coreImplTail

// cErrorSlotStruct 是共享错误槽的**唯一** C 布局定义。三份生成 C 各自重复这份
// struct 标签（struct 标签是翻译单元内部的，不产生链接符号），但内容逐字一致：
// 固定宽度整数 + 定长字符数组 + size_t，**没有任何指针成员**，静态存储期。
// 把它写成一份 Go 常量，是为了让 core_impl.c / shim / unit.c 不可能各写各的而漂移。
const cErrorSlotStruct = `/* Shared last-error slot: fixed layout, NO pointer members.
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
`

// CErrorSlotDecl 是给两处写方（`hermes cc` 的 per-unit shim 与 unit AOT 产物）
// 复用的 C 片段：extern 声明 + 三个 static helper。static helper 不增加导出符号；
// 槽本身是 external linkage，所以同一个链接单元里所有翻译单元共享一个槽。
//
// 全部按 C89 书写（hermes cc 的 C 路径是 -std=c89 一条 invoke）：不用 // 注释、
// 不在语句后声明变量、不用 long long。
const CErrorSlotDecl = cErrorSlotStruct + `
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
`

const coreImplHead = `/* Provided by hermes: implementation of hermes_core.h. Do not edit. */
#include <stdlib.h>

#include "hermes/hermes_core.h"

`

const coreImplTail = `
/* Shared last-error slot: EXTERNAL linkage, static storage duration, fixed
 * layout with NO pointer members (same discipline as the AOT HermesError).
 *
 * HERMES_THREAD_LOCAL comes from hermes_core.h: under C11+ / MSVC this slot is
 * thread-local; under -std=c89 / -std=c99 the macro expands to NOTHING and the
 * slot degrades to a PROCESS-GLOBAL slot -- concurrent calls may overwrite
 * each other's error (registered in docs/C-ABI.md / docs/KNOWN-GAPS.md).
 * Every translation unit in one link must therefore use the same dialect.
 *
 * Ownership: NO malloc, NO strdup, NO dangling-pointer copy. The view returned
 * by hermes_last_error() points into this static msg[] array and is owned by
 * the slot. The caller does NOT free it, does NOT retain the pointer, and the
 * view is valid only until the next Hermes entry-point call (which clears it).
 * That is what lets Python/Go/... read the error text through the C ABI without
 * a matching free() and without a use-after-free. */
HERMES_THREAD_LOCAL struct hermes_error_slot hermes_last_error_slot = { 0, 0, 0, { 0 } };

void hermes_buf_free(hermes_buf* b) {
  if (!b) {
    return;
  }
  free(b->ptr);
  b->ptr = NULL;
  b->len = 0;
}

hermes_str hermes_last_error(void) {
  /* ptr is NEVER NULL: it points into the static msg[] array. With no error
   * pending, len == 0 and msg[0] == 0, so printf("%.*s") stays safe. */
  hermes_str s;
  s.ptr = hermes_last_error_slot.msg;
  s.len = hermes_last_error_slot.len;
  return s;
}
`
