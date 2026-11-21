// product_gate.cpp —— 产物侧链接面闸门（与 cmd_compile.go 的 productLinkGate 一致）。
//
// 与 `cc` 的 fail-open 警告**不同**：只要本次用了 --link-dynamic，读不出依赖就
// 判死——绝不把"读不懂"当成"干净"。
//
// 单独成文件的原因：它需要能被测试直接调用（裸机 raw firmware 既不误杀、
// 又能抓住计划里混入的 Go runtime）。
#include "hermes_parser.h"

// 来自 Rust 的 objfile 模块（动态依赖）。
extern "C" int hm_objfile_dynamic_libs(const char* path, char* out, size_t cap, size_t* out_len);
#include "json_write.h"

#include <cstdio>
#include <cstring>
#include <string>
#include <vector>

namespace {

int emit(char* out, size_t cap, size_t* out_len, const std::string& msg) {
  if (out != nullptr && cap > 0) {
    const size_t n = msg.size() < cap - 1 ? msg.size() : cap - 1;
    std::memcpy(out, msg.c_str(), n);
    out[n] = 0;
  }
  if (out_len != nullptr) *out_len = msg.size() + 1;
  return 1;
}

std::vector<std::string> split_csv(const char* csv) {
  std::vector<std::string> out;
  if (csv == nullptr || *csv == 0) return out;
  std::string cur;
  for (const char* p = csv; *p; ++p) {
    if (*p == ',' || *p == ' ') {
      if (!cur.empty()) out.push_back(cur);
      cur.clear();
    } else {
      cur.push_back(*p);
    }
  }
  if (!cur.empty()) out.push_back(cur);
  return out;
}

std::string detail_field(const std::string& lib, const char* key) {
  char detail[512] = {0};
  size_t dn = 0;
  if (hm_linkpolicy_detail(lib.c_str(), detail, sizeof detail, &dn) != 1) return "";
  hm::JVal d;
  size_t dp = 0;
  if (!hm::parse_json(detail, dn > 0 ? dn - 1 : 0, &d, &dp)) return "";
  const hm::JVal* v = d.find(key);
  return (v != nullptr && v->kind() == hm::JVal::kStr) ? v->as_str() : "";
}

}  // namespace

// 返回 0 通过 / 1 拒绝（out_msg）/ -1 参数非法。
extern "C" int hm_product_link_gate(const char* out_path, int used_dynamic,
                                    const char* bare_metal_json, const char* plan_json, char* out_msg,
                                    size_t cap, size_t* out_len) {
  if (out_path == nullptr || plan_json == nullptr || out_msg == nullptr || out_len == nullptr) {
    return -1;
  }
  bool is_bare = false;
  if (bare_metal_json != nullptr) {
    is_bare = std::strcmp(bare_metal_json, "1") == 0;
  }
  std::string go_runtime, dynamic;
  if (plan_json[0] != 0) {
    hm::JVal p;
    size_t pp = 0;
    if (hm::parse_json(plan_json, std::strlen(plan_json), &p, &pp) && p.kind() == hm::JVal::kObj) {
      const hm::JVal* g = p.find("go_runtime");
      if (g != nullptr && g->kind() == hm::JVal::kStr) go_runtime = g->as_str();
      const hm::JVal* d = p.find("dynamic");
      if (d != nullptr && d->kind() == hm::JVal::kStr) dynamic = d->as_str();
    }
  }

  const std::string path = out_path;
  // 独立于 BuildLinkPlan 的第二道断言：plan 是别处产出的值，不是这里的局部变量，
  // 所以这条检查能失败。日后谁绕过 BuildLinkPlan 直接塞库，会在这里红。
  if (is_bare) {
    if (!split_csv(go_runtime.c_str()).empty()) {
      return emit(out_msg, cap, out_len,
                  "hermes compile: bare-metal artifact " + path +
                      " has Go runtime libraries in its plan: " + go_runtime +
                      "; hardware targets must contain no Go runtime at all");
    }
    if (!split_csv(dynamic.c_str()).empty()) {
      return emit(out_msg, cap, out_len,
                  "hermes compile: bare-metal artifact " + path +
                      " has dynamic libraries in its plan: " + dynamic +
                      "; hardware targets allow static .a only");
    }
  }

  constexpr size_t kCap = 1 << 16;
  std::vector<char> libs(kCap);
  size_t ln = 0;
  const int lrc = hm_objfile_dynamic_libs(path.c_str(), libs.data(), libs.size(), &ln);
  if (lrc != 0) {
    // 裸机的 raw firmware 没有动态段：那是合法的，不是"读不懂"。
    if (is_bare && lrc == -20) return 0;
    if (used_dynamic) {
      return emit(out_msg, cap, out_len,
                  "hermes compile: cannot read the dynamic dependencies of " + path +
                      " after --link-dynamic; refusing a product whose link inputs cannot be "
                      "verified");
    }
    return emit(out_msg, cap, out_len,
                "hermes compile: cannot read the dynamic dependencies of " + path +
                    "; the artifact's link surface is unverifiable");
  }

  std::vector<std::string> list;
  {
    std::string joined(libs.data(), ln > 0 ? ln - 1 : 0);
    size_t pos = 0;
    while (pos <= joined.size() && !joined.empty()) {
      const size_t nl = joined.find('\n', pos);
      const std::string one =
          joined.substr(pos, (nl == std::string::npos) ? std::string::npos : nl - pos);
      if (!one.empty()) list.push_back(one);
      if (nl == std::string::npos) break;
      pos = nl + 1;
    }
  }
  for (const std::string& one : list) {
    const std::string name = detail_field(one, "name");
    if (name.empty()) continue;
    return emit(out_msg, cap, out_len,
                "hermes compile: " + path + " is linked against " + one + ", a " + name +
                    " runtime (" + detail_field(one, "licence") + "): " +
                    detail_field(one, "reason") +
                    ". The GPL/copyleft runtime must not enter the link (source: " +
                    detail_field(one, "source") + ")");
  }
  if (is_bare && !list.empty()) {
    std::string shown;
    for (const std::string& l : list) {
      if (!shown.empty()) shown += " ";
      shown += l;
    }
    return emit(out_msg, cap, out_len, "hermes compile: bare-metal artifact " + path +
                                          " carries dynamic dependencies " + shown +
                                          "; hardware targets have no dynamic loader");
  }
  return 0;
}
