// diag_render.h —— C++ 内部渲染 helper 声明（不是 ABI；仅供实现与 C++ 守卫）。
#pragma once

#include "diag_catalog.h"
#include "hermes_diag.h"

#include <string>
#include <vector>

namespace hm {

// 与 Go subst 逐字等价：{{ → {、}} → }，{name} 有参则替换、无参原样保留。
std::string diag_subst(const std::string& tmpl, const hm_diag_arg* args, size_t n_args);

// 模板里出现的合法占位符名（跳过 {{ 转义），用于 catalog 双向守卫。
std::vector<std::string> diag_placeholder_names(const std::string& tmpl);

// 未转义也不构成合法占位符的花括号片段；正常 catalog 应为空。
std::vector<std::string> diag_unescaped_braces(const std::string& tmpl);

// 渲染一条诊断的完整人读文本（骨架与 Go Diag.Render 一致）。
std::string diag_render_one(const hm_diag& d, bool zh);

// 未登记码的**自曝**文本：绝不静默空。
std::string diag_unknown_message(const char* code);

// explain 的完整人读文本；未登记码返回空串（由调用方报错）。
std::string diag_explain_text(const char* code, bool zh);

// explain 的 JSON（code/message/why/fixes/docs）；未登记码返回空串。
std::string diag_explain_json(const char* code, bool zh);

// 一条诊断的字段 JSON（code/severity/message/why/fixes/text）。
std::string diag_render_fields_json(const hm_diag& d, bool zh);

// 把字符串写进调用方 buffer；返回 HM_OK / HM_E_INVALID_ARG。
hm_status diag_write_buffer(const std::string& s, char* out, size_t cap, size_t* out_len);

}  // namespace hm
