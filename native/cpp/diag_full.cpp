// diag_full.cpp —— 把"一条诊断 JSON + 源码"渲染成完整人读文本。
//
// 两步合一：先截片段（hm_diag_snippet），再渲染文案（hm_diag_render）。
// 分开做会让每个调用点各自拼一遍，迟早出现两种排版 —— 用户不该因为命令不同
// 而看到不同的错误样式。
#include "hermes_diag.h"
#include "hermes_parser.h"
#include "json_write.h"

#include <algorithm>
#include <cstring>
#include <string>
#include <vector>

extern "C" int hm_diag_snippet(const char* src, size_t len, int line, int col, int end_col,
                               char* out, size_t cap, size_t* out_len);

namespace {

const char* str_of(const hm::JVal* d, const char* k) {
  const hm::JVal* v = d->find(k);
  return (v != nullptr && v->kind() == hm::JVal::kStr) ? v->as_str().c_str() : "";
}

int int_of(const hm::JVal* d, const char* k) {
  const hm::JVal* v = d->find(k);
  return (v != nullptr) ? static_cast<int>(v->as_int(0)) : 0;
}

}  // namespace

// 渲染一个诊断 JSON 数组。返回 0 成功；-1 参数非法；-4 缓冲不足。
static int render_impl(const char* src, size_t src_len, const char* json, size_t json_len,
                       char* out, size_t cap, size_t* out_len, int max_show, bool stop_first) {
  if (json == nullptr || out == nullptr || out_len == nullptr) return -1;
  hm::JVal arr;
  size_t pos = 0;
  if (!hm::parse_json(json, json_len, &arr, &pos) || arr.kind() != hm::JVal::kArr) return -1;

  // 默认口径不是"只渲染第一条诊断"，而是"渲染到**第一条 error** 为止"：
  // 前面挂着的 warning 必须显示（它可能是那条 error 的成因），
  // 而 error 之后的诊断全部抑制。按"只渲染第一条"会把 warning 当成唯一输出，
  // 真正该看的 error 被挤掉——实测踩过：tool.call 用例只看到 W2003。
  int limit = max_show;
  if (stop_first) {
    limit = static_cast<int>(arr.size());
    for (size_t i = 0; i < arr.size(); ++i) {
      const hm::JVal* d0 = arr.at(i);
      if (d0 == nullptr) continue;
      const hm::JVal* sv = d0->find("severity");
      if (sv != nullptr && sv->kind() == hm::JVal::kStr && sv->as_str() == "error") {
        limit = static_cast<int>(i) + 1;
        break;
      }
    }
  }

  std::string all;
  int shown = 0;
  for (size_t i = 0; i < arr.size(); ++i) {
    if (limit > 0 && shown >= limit) break;
    ++shown;
    const hm::JVal* d = arr.at(i);
    if (d == nullptr || d->kind() != hm::JVal::kObj) continue;

    const int line = int_of(d, "line");
    const int col = int_of(d, "col");
    const int end_col = int_of(d, "end_col");

    std::vector<char> snip(1 << 12);
    size_t snip_len = 0;
    std::string snippet;
    if (src != nullptr && hm_diag_snippet(src, src_len, line, col, end_col, snip.data(),
                                          snip.size(), &snip_len) == 0 &&
        snip_len > 1) {
      snippet.assign(snip.data(), snip_len - 1);  // 去掉结尾 NUL
    }

    // args 必须传下去：不传的话 hm_diag_render 只能吐 catalog 的**通用模板**
    // （"the type has no such field"），而前端给的正文是具体的
    // （"struct Intent has no field action"）。两者看起来都像诊断，但后者才能定位。
    std::vector<hm_diag_arg> args;
    std::vector<std::string> arg_names, arg_values;
    const hm::JVal* ja = d->find("args");
    if (ja != nullptr && ja->kind() == hm::JVal::kObj) {
      std::vector<std::string> keys;
      ja->keys(&keys);
      std::sort(keys.begin(), keys.end());
      for (const std::string& k : keys) {
        const hm::JVal* v = ja->find(k);
        if (v == nullptr) continue;
        arg_names.push_back(k);
        arg_values.push_back(v->kind() == hm::JVal::kStr ? v->as_str() : v->str());
      }
      for (size_t k = 0; k < arg_names.size(); ++k) {
        hm_diag_arg a{};
        a.name = arg_names[k].c_str();
        a.value = arg_values[k].c_str();
        args.push_back(a);
      }
    }

    // **用前端的 message，不用 catalog 模板**。
    // 编译器诊断正文按设计恒英文，且前端给的是发射点的具体文案
    // （"struct H has no field nope"），catalog 只给泛化模板
    // （"the type has no such field"）。换模板会牺牲精度——而这正是用户最常
    // 看到的错误。why/help 优先取诊断自带的，没有才回落 catalog。
    const char* sev = str_of(d, "severity");
    const char* msg = str_of(d, "message");
    const char* mod = str_of(d, "module");
    const std::string sev_word = (sev != nullptr && sev[0] != '\0') ? sev : "error";

    if (!all.empty()) all += '\n';
    all += sev_word;
    all += "[";
    all += str_of(d, "code");
    all += "]: ";
    all += (msg != nullptr && msg[0] != '\0') ? msg : "";
    all += "\n  --> ";
    all += mod;
    if (line > 0) {
      char loc[64];
      std::snprintf(loc, sizeof loc, "\n      line %d, col %d", line, col);
      all += loc;
    }
    if (!snippet.empty()) {
      all += "\n\n";
      all += snippet;
    }
    const char* why = str_of(d, "why");
    if (why != nullptr && why[0] != '\0') {
      all += "\n   why: ";
      all += why;
    }
    const hm::JVal* fixes = d->find("fix");
    if (fixes != nullptr && fixes->kind() == hm::JVal::kArr) {
      for (size_t k = 0; k < fixes->size(); ++k) {
        const hm::JVal* fx = fixes->at(k);
        if (fx == nullptr || fx->kind() != hm::JVal::kStr || fx->as_str().empty()) continue;
        all += "\n  help: ";
        all += fx->as_str();
      }
    }
  }

  const int total = static_cast<int>(arr.size());
  if (total > shown) {
    char note[160];
    if (stop_first) {
      std::snprintf(note, sizeof note,
                    "\n... %d more diagnostic(s) suppressed (re-run with --keep-going to see up to 50)\n",
                    total - shown);
    } else {
      std::snprintf(note, sizeof note,
                    "\n... %d more diagnostic(s) suppressed (--keep-going shows at most 50)\n",
                    total - shown);
    }
    all += note;
  }

  const size_t need = all.size() + 1;
  if (need > cap) {
    *out_len = need;
    return -4;
  }
  std::memcpy(out, all.data(), all.size());
  out[all.size()] = 0;
  *out_len = need;
  return 0;
}

// 默认口径：只渲染第一条，其余给"被抑制"提示。
extern "C" int hm_diag_render_json_src(const char* src, size_t src_len, const char* json,
                                       size_t json_len, const char* /*lang*/, char* out, size_t cap,
                                       size_t* out_len) {
  return render_impl(src, src_len, json, json_len, out, cap, out_len, 1, true);
}

// --keep-going：最多渲染 50 条，且不提示"首错即停"。
extern "C" int hm_diag_render_json_keep_going(const char* src, size_t src_len, const char* json,
                                              size_t json_len, char* out, size_t cap,
                                              size_t* out_len) {
  return render_impl(src, src_len, json, json_len, out, cap, out_len, 50, false);
}

// ir 口径：全部渲染，无抑制提示（Go 的 ir 不设截断）。
extern "C" int hm_diag_render_json_all(const char* src, size_t src_len, const char* json,
                                       size_t json_len, char* out, size_t cap, size_t* out_len) {
  return render_impl(src, src_len, json, json_len, out, cap, out_len, 0, false);
}
