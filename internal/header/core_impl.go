package header

// CoreImpl 是 `hermes_core.h` 里那几个函数的**唯一实现**，生成成一个 C 文件。
//
// 为什么不做成静态库：这几个函数加起来不到三十行，而且它们是**每一份产物都必须
// 带的**（谁调 hermes_buf_free 谁就需要它）。做成库就多一次"库版本要和头版本
// 配套"的约束，而它们之间根本没有会变的东西。
//
// 为什么不做成 header-only（static inline）：那会让每个翻译单元各有一份副本，
// 而 `hermes_buf_free` 的行为里含 malloc/free —— 跨模块各带一份是自找麻烦。
const CoreImpl = `/* 由 hermes 提供：hermes_core.h 的实现。不要手改。 */
#include <stdlib.h>

#include "hermes/hermes_core.h"

void hermes_buf_free(hermes_buf* b) {
  if (!b) {
    return;
  }
  free(b->ptr);
  b->ptr = NULL;
  b->len = 0;
}

hermes_str hermes_last_error(void) {
  /* 空串而不是 NULL：调用方不必为"有没有错误"再写一条分支，
   * 而 NULL 的 ptr 会让 printf("%.*s") 这类用法直接崩。 */
  hermes_str s;
  static const char empty[1] = { 0 };
  s.ptr = empty;
  s.len = 0;
  return s;
}

uint32_t hermes_abi_major(void) {
  /* 对外 ABI 的 major：只要求 major 相等（D-024），不是严格相等。 */
  return 1u;
}
`
