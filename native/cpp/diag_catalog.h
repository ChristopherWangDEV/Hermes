// diag_catalog.h —— C++ 内部使用的双语 catalog 声明。
//
// 这个文件放在 native/cpp 而不是 native/include：它不是 ABI 的一部分，
// 只有前端实现与 tests/compiler/parser_test.cpp 的守卫会 include。
#pragma once

#include <cstddef>

namespace hm {

struct DiagCatalogEntry {
  const char* code;
  const char* text;
  const char* why;
  const char* fix[3];   // 末尾 nullptr 填齐；本项目当前最多 2 条
  const char* docs;     // docsAnchorEN，en/zh 共用
  const char* args[8];  // 末尾 nullptr 填齐；本项目当前最多 7 个
};

extern const DiagCatalogEntry kDiagCatalogEN[];
extern const size_t kDiagCatalogENCount;
extern const DiagCatalogEntry kDiagCatalogZH[];
extern const size_t kDiagCatalogZHCount;
extern const char* const kDiagCodes[];
extern const size_t kDiagCodesCount;

// 未知码返回 nullptr（调用方必须自曝，不许静默空文案）。
const DiagCatalogEntry* diag_catalog_find(const char* code, bool zh);

}  // namespace hm
