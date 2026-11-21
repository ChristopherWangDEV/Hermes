// linkpolicy.cpp —— 许可红线的**闭集**判据（与 internal/linkpolicy 同一份规则）。
//
// 为什么在 C++：`hermes cc` / `hermes compile` 要检查"链接面进了哪些库"，
// 而它们正在搬进 C++。红线判据不允许有两份会漂移的实现——所以这里逐条照抄
// Go 侧的表与匹配规则，并有一条判据（Go 与 C++ 全表比对）盯着。
//
// 判据是**闭集**，不是启发式：只看 filepath.Base(lib) 的小写是否**包含**某个
// 已知库名。绝不扫"看起来像 GPL"的子串——那会误伤合法的 C++ 翻译单元。
#include "hermes_parser.h"

#include "linkpolicy_table.h"

#include <cstring>
#include <string>

namespace {

// 规则表由 tools/genlinkpolicy 从 internal/linkpolicy 生成（build/linkpolicy_table.cpp）。
// 手抄一份进 C++ 必然漂移，而漂移的后果是 GPL 库被放进来。
using Rule = hm::LinkRule;
const Rule* kForbidden = hm::kForbiddenRules;
const size_t kForbiddenCount = hm::kForbiddenRuleCount;

std::string base_of(const std::string& path) {
  const size_t slash = path.find_last_of('/');
  return (slash == std::string::npos) ? path : path.substr(slash + 1);
}

std::string lower(std::string s) {
  for (char& c : s) {
    if (c >= 'A' && c <= 'Z') c = static_cast<char>(c - 'A' + 'a');
  }
  return s;
}

}  // namespace

// 命中返回 1 并写出规则名与许可；未命中返回 0。参数非法返回 -1。
extern "C" int hm_linkpolicy_match(const char* lib, char* out_name, size_t name_cap,
                                   char* out_licence, size_t lic_cap) {
  if (lib == nullptr) return -1;
  const std::string base = lower(base_of(lib));
  for (size_t i = 0; i < kForbiddenCount; ++i) {
    const Rule& r = kForbidden[i];
    if (base.find(r.name) != std::string::npos) {
      if (out_name != nullptr && name_cap > 0) {
        const size_t n = std::strlen(r.name);
        const size_t k = (n < name_cap - 1) ? n : name_cap - 1;
        std::memcpy(out_name, r.name, k);
        out_name[k] = 0;
      }
      if (out_licence != nullptr && lic_cap > 0) {
        const size_t n = std::strlen(r.licence);
        const size_t k = (n < lic_cap - 1) ? n : lic_cap - 1;
        std::memcpy(out_licence, r.licence, k);
        out_licence[k] = 0;
      }
      return 1;
    }
  }
  return 0;
}

// hm_linkpolicy_detail：命中时输出 JSON {name,licence,reason,source}，未命中输出 ""。
// 存在的理由：`compile` 的拒绝文案要引用 reason 与 source（"为什么禁"和"出处"），
// 而只返回 name/licence 的那版 API 拼不出完整句子。
extern "C" int hm_linkpolicy_detail(const char* lib, char* out, size_t cap, size_t* out_len) {
  if (lib == nullptr || out == nullptr || out_len == nullptr) return -1;
  const std::string base = lower(base_of(lib));
  for (size_t i = 0; i < kForbiddenCount; ++i) {
    const Rule& r = kForbidden[i];
    if (base.find(r.name) == std::string::npos) continue;
    std::string js = "{\"name\":\"";
    js += r.name;
    js += "\",\"licence\":\"";
    js += r.licence;
    js += "\",\"reason\":\"";
    js += r.reason;
    js += "\",\"source\":\"";
    js += r.source;
    js += "\"}";
    const size_t need = js.size() + 1;
    if (need > cap) {
      *out_len = need;
      return -4;
    }
    std::memcpy(out, js.data(), js.size());
    out[js.size()] = 0;
    *out_len = need;
    return 1;
  }
  out[0] = 0;
  *out_len = 1;
  return 0;
}
