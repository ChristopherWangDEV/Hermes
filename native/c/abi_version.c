/*
 * abi_version.c —— C 侧的 ABI 布局自检。
 *
 * 这里**不**实现 hm_abi_version / hm_abi_build_id —— 那两个由 Rust 导出
 * （见 native/rust/src/lib.rs 的 ARCH §4.3 十五符号清单）。本文件负责的是
 * 另一半：把 hermes_abi.h 里的结构体布局在编译期钉死，并给 shim 自检与
 * hm_spawn 的 abi_size 校验提供一个可比较的指纹。
 *
 * 为什么值得单独一个文件：ABI 漂移最典型的形态不是"函数改了"，而是
 * "有人在 struct 中间插了一个字段"，链接期完全看不出来，运行期读到垃圾。
 * 静态断言让它变成编译错误。
 */

#include "hm_internal.h"

#include <stddef.h>

/* ------------------------------------------------------------------ *
 * 编译期布局断言
 *
 * 刻意写成 "除以零" 而不是 _Static_assert，是为了在 C99 之前也能用；
 * 本项目用 -std=c11，_Static_assert 可用，但下面这组消息更好读。
 * ------------------------------------------------------------------ */

#define HM_STATIC_ASSERT(cond, tag) \
  typedef char hm_static_assert_##tag[(cond) ? 1 : -1]

HM_STATIC_ASSERT(sizeof(hm_status) == 4, hm_status_is_4_bytes);
HM_STATIC_ASSERT(sizeof(hm_handle) == 8, hm_handle_is_8_bytes);
HM_STATIC_ASSERT(offsetof(hm_spawn_req, abi_size) == 0, spawn_req_abi_size_first);
HM_STATIC_ASSERT(offsetof(hm_spawn_res, abi_size) == 0, spawn_res_abi_size_first);
HM_STATIC_ASSERT(sizeof(((hm_spawn_req *)0)->abi_size) == 4, spawn_req_abi_size_32bit);
HM_STATIC_ASSERT(sizeof(((hm_spawn_res *)0)->abi_size) == 4, spawn_res_abi_size_32bit);

/* handle 的低 32 位是 slot、高 32 位是 generation；打包/拆包必须自反。 */
HM_STATIC_ASSERT(sizeof(uint32_t) * 8 == 32, uint32_is_32bit);
HM_STATIC_ASSERT(sizeof(uint64_t) * 8 == 64, uint64_is_64bit);

/* 长度上限必须是 2 的幂（R3 的实现依赖这一点做边界判断）。 */
HM_STATIC_ASSERT((HM_SENSITIVE_MAX & (HM_SENSITIVE_MAX - 1)) == 0, sensitive_max_pow2);
HM_STATIC_ASSERT((HM_VIEW_MAX & (HM_VIEW_MAX - 1)) == 0, view_max_pow2);
HM_STATIC_ASSERT((HM_CHAIN_BLOCK_MAX & (HM_CHAIN_BLOCK_MAX - 1)) == 0, chain_max_pow2);

/* ------------------------------------------------------------------ *
 * 布局指纹
 *
 * 混合所有"改了就该提升 ABI minor"的量。hermes-shim --self-test 会把它
 * 打出来，doctor 也会带上；两个版本的 shim 与 hermes 混用时一眼可辨。
 * ------------------------------------------------------------------ */

static uint64_t mix64(uint64_t h, uint64_t v) {
  h ^= v + 0x9e3779b97f4a7c15ull + (h << 6) + (h >> 2);
  return h;
}

uint64_t hermes_abi_header_hash(void) {
  uint64_t h = 0xcbf29ce484222325ull;
  h = mix64(h, (uint64_t)HM_ABI_MAJOR);
  h = mix64(h, (uint64_t)HM_ABI_MINOR);
  h = mix64(h, (uint64_t)sizeof(hm_spawn_req));
  h = mix64(h, (uint64_t)sizeof(hm_spawn_res));
  h = mix64(h, (uint64_t)offsetof(hm_spawn_req, shim));
  h = mix64(h, (uint64_t)offsetof(hm_spawn_req, exe));
  h = mix64(h, (uint64_t)offsetof(hm_spawn_req, deadline_cycles));
  h = mix64(h, (uint64_t)offsetof(hm_spawn_req, sandbox_flags));
  h = mix64(h, (uint64_t)offsetof(hm_spawn_req, stdout_cap));
  h = mix64(h, (uint64_t)offsetof(hm_spawn_res, deadline_source));
  h = mix64(h, (uint64_t)offsetof(hm_spawn_res, setup_errno));
  h = mix64(h, (uint64_t)offsetof(hm_spawn_res, stdout_bytes));
  h = mix64(h, (uint64_t)HM_SENSITIVE_MAX);
  h = mix64(h, (uint64_t)HM_VIEW_MAX);
  h = mix64(h, (uint64_t)HM_CHAIN_BLOCK_MAX);
  h = mix64(h, (uint64_t)HM_E_PANIC);
  return h;
}

uint32_t hermes_abi_layout_fingerprint(void) {
  uint64_t h = hermes_abi_header_hash();
  return (uint32_t)((h >> 32) ^ (h & 0xffffffffull));
}
