// link_plan.cpp —— `compile` 的**唯一**链接面决策点（与 toolchain.BuildLinkPlan 逐字对应）。
//
// 为什么只能有一个地方决定"哪些库进产物"：目标相关的规则一旦散落在调用点，
// 硬件层隔离就会退回成注释。这里每条拒绝都带**独立的码**——一个码两种原因
// 等于没有原因（绿测试分不清走了哪条路）。
#include "hermes_parser.h"
#include "json_write.h"

#include <cstring>
#include <string>
#include <vector>

namespace {

const char* kGoRuntimeBareMetalMessage =
    "[ERROR] Go runtime libraries (g*) cannot be linked for bare-metal targets. Please use pure C/Rust "
    "static libraries.";

const char* kCppABIRuntimes[] = {"libstdc++", "libsupc++", "libc++", "libc++abi", "libcxxrt", "libcxx"};
const char* kLegacyGoRuntimeNames[] = {"libhermesnet", "hermesnet", "libhermesrt", "hermesrt"};

std::string lower(std::string s) {
  for (char& c : s) {
    if (c >= 'A' && c <= 'Z') c = static_cast<char>(c - 'A' + 'a');
  }
  return s;
}

std::string base_of(const std::string& p) {
  const size_t s = p.find_last_of('/');
  return (s == std::string::npos) ? p : p.substr(s + 1);
}

// libStem：去掉扩展名与 soname 版本号，保留 "lib" 前缀。
//   libstdc++.so.6 → libstdc++；libc++.1.dylib → libc++
std::string lib_stem(const std::string& path) {
  std::string base = lower(base_of(path));
  const size_t so = base.find(".so");
  if (so != std::string::npos) return base.substr(0, so);
  for (const char* ext : {".dylib", ".dll", ".a", ".lib"}) {
    const size_t n = std::strlen(ext);
    if (base.size() >= n && base.compare(base.size() - n, n, ext) == 0) {
      base = base.substr(0, base.size() - n);
      break;
    }
  }
  while (true) {
    const size_t dot = base.find_last_of('.');
    if (dot == std::string::npos || dot + 1 >= base.size()) break;
    bool digits = true;
    for (size_t i = dot + 1; i < base.size(); ++i) {
      if (base[i] < '0' || base[i] > '9') {
        digits = false;
        break;
      }
    }
    if (!digits) break;
    base = base.substr(0, dot);
  }
  return base;
}

bool has_go_runtime_prefix(const std::string& lib) {
  const std::string b = lower(base_of(lib));
  return !b.empty() && b[0] == 'g';
}

bool is_go_runtime_lib(const std::string& lib) {
  if (has_go_runtime_prefix(lib)) return true;
  const std::string stem = lib_stem(lib);
  for (const char* n : kLegacyGoRuntimeNames) {
    if (stem == n) return true;
  }
  return false;
}

// refusal 把码与文案写进 out_json（调用方据此断言 CodeOf）。
int refusal(const char* code, const std::string& msg, char* out, size_t cap, size_t* out_len) {
  hm::JVal o = hm::JVal::object();
  o.set("code", hm::JVal::string(code));
  o.set("message", hm::JVal::string(msg));
  const std::string js = o.str();
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

int check_pure_c_abi(const std::string& flag, const std::string& lib, char* out, size_t cap,
                     size_t* out_len) {
  const std::string stem = lib_stem(lib);
  for (const char* n : kCppABIRuntimes) {
    if (stem != n) continue;
    char msg[1024];
    std::snprintf(msg, sizeof msg,
                  "hermes compile: %s \"%s\" is a C++ ABI runtime (%s). Hermes links pure C ABI "
                  "only: name mangling and STL layout are not portable across compilers. Provide a "
                  "C bridge compiled with extern \"C\" and link that instead.",
                  flag.c_str(), lib.c_str(), stem.c_str());
    const size_t mlen = std::strlen(msg);
    const size_t k = (mlen < cap - 1) ? mlen : cap - 1;
    std::memcpy(out, msg, k);
    out[k] = 0;
    *out_len = k + 1;
    return 1;
  }
  return 0;
}

// 逗号分隔的列表 <-> vector（空串 = 空列表）。
std::vector<std::string> split_csv(const char* csv) {
  std::vector<std::string> out;
  if (csv == nullptr || *csv == 0) return out;
  std::string cur;
  for (const char* p = csv; *p; ++p) {
    if (*p == ',') {
      out.push_back(cur);
      cur.clear();
    } else {
      cur.push_back(*p);
    }
  }
  if (!cur.empty()) out.push_back(cur);
  return out;
}

std::string join_csv(const std::vector<std::string>& v) {
  std::string out;
  for (size_t i = 0; i < v.size(); ++i) {
    if (i) out += ",";
    out += v[i];
  }
  return out;
}

}  // namespace

// target_json 是 hm_target_parse 的产物。返回 0 = 有 plan（out_json），
// 1 = 拒绝（out_json 是 {code,message}），-1 参数非法，-4 缓冲不足。
extern "C" int hm_link_plan(const char* target_json, int need_net, const char* net_lib,
                            const char* static_csv, const char* dynamic_csv, char* out_json,
                            size_t cap, size_t* out_len) {
  if (target_json == nullptr || out_json == nullptr || out_len == nullptr) return -1;
  hm::JVal tgt;
  size_t pos = 0;
  if (!hm::parse_json(target_json, std::strlen(target_json), &tgt, &pos) ||
      tgt.kind() != hm::JVal::kObj) {
    return -1;
  }
  auto tstr = [&](const char* k) -> std::string {
    const hm::JVal* v = tgt.find(k);
    return (v != nullptr && v->kind() == hm::JVal::kStr) ? v->as_str() : "";
  };
  const std::string t_raw = tstr("raw");
  const std::string t_goos = tstr("goos");
  const bool t_bare = tstr("class") == "bare-metal";
  const bool t_native = [&] {
    const hm::JVal* v = tgt.find("native");
    return v != nullptr && v->kind() == hm::JVal::kBool && v->as_bool(false);
  }();

  const std::vector<std::string> statics = split_csv(static_csv);
  const std::vector<std::string> dynamics = split_csv(dynamic_csv);
  const std::string net = (net_lib != nullptr) ? net_lib : "";

  // 1. 纯 C ABI：与目标无关，先查。
  for (const auto& set : {std::make_pair("--link-static", statics),
                          std::make_pair("--link-dynamic", dynamics)}) {
    for (const std::string& lib : set.second) {
      char cmsg[1024] = {0};
      size_t cn = 0;
      if (check_pure_c_abi(set.first, lib, cmsg, sizeof cmsg, &cn) == 1) {
        return refusal("E3105", cmsg, out_json, cap, out_len);
      }
    }
  }

  // 2. 许可闭集，在**计划面**上再查一次（纵深防御：内置网络归档是另一条输入路径）。
  std::vector<std::string> all = statics;
  all.insert(all.end(), dynamics.begin(), dynamics.end());
  if (!net.empty()) all.push_back(net);
  for (const std::string& lib : all) {
    char detail[512] = {0};
    size_t dn = 0;
    if (hm_linkpolicy_detail(lib.c_str(), detail, sizeof detail, &dn) == 1) {
      hm::JVal d;
      size_t dp = 0;
      if (hm::parse_json(detail, dn > 0 ? dn - 1 : 0, &d, &dp) && d.kind() == hm::JVal::kObj) {
        auto f = [&](const char* k) -> std::string {
          const hm::JVal* v = d.find(k);
          return (v != nullptr && v->kind() == hm::JVal::kStr) ? v->as_str() : "";
        };
        char msg[1536];
        std::snprintf(msg, sizeof msg, "hermes compile: %s is a %s runtime (%s): %s (source: %s)",
                      lib.c_str(), f("name").c_str(), f("licence").c_str(), f("reason").c_str(),
                      f("source").c_str());
        return refusal("E3108", msg, out_json, cap, out_len);
      }
    }
  }

  // 3. 硬件层：结构性切断通往 Go runtime 的每一条路。
  if (t_bare) {
    if (need_net) return refusal("E3110", kGoRuntimeBareMetalMessage, out_json, cap, out_len);
    for (const std::string& lib : all) {
      if (is_go_runtime_lib(lib)) {
        return refusal("E3110", kGoRuntimeBareMetalMessage, out_json, cap, out_len);
      }
    }
    for (const std::string& lib : statics) {
      const std::string low = lower(lib);
      if (!(low.size() >= 2 && low.compare(low.size() - 2, 2, ".a") == 0)) {
        char msg[1024];
        std::snprintf(msg, sizeof msg,
                      "hermes compile: bare-metal target %s may link static .a archives only; "
                      "--link-static \"%s\" is not a static archive. Rebuild it as a pure C/Rust .a.",
                      t_raw.c_str(), lib.c_str());
        return refusal("E3106", msg, out_json, cap, out_len);
      }
    }
    if (!dynamics.empty()) {
      char msg[1024];
      std::snprintf(msg, sizeof msg,
                    "hermes compile: bare-metal target %s has no dynamic loader; --link-dynamic is "
                    "forbidden (got \"%s\"). Use --link-static with a pure C/Rust .a.",
                    t_raw.c_str(), dynamics[0].c_str());
      return refusal("E3102", msg, out_json, cap, out_len);
    }
    hm::JVal o = hm::JVal::object();
    o.set("static", hm::JVal::string(join_csv(statics)));
    o.set("dynamic", hm::JVal::string(""));
    o.set("frameworks", hm::JVal::string(""));
    o.set("go_runtime", hm::JVal::string(""));
    o.set("used_net", hm::JVal::boolean(false));
    const std::string js = o.str();
    const size_t need = js.size() + 1;
    if (need > cap) {
      *out_len = need;
      return -4;
    }
    std::memcpy(out_json, js.data(), js.size());
    out_json[js.size()] = 0;
    *out_len = need;
    return 0;
  }

  // 4. 软件层：g* 允许，但命名空间必须被遵守。
  std::vector<std::string> plan_static, plan_dynamic, go_libs;
  bool used_net = false;
  if (need_net) {
    if (net.empty()) {
      return refusal("E3107",
                     "hermes compile: this program calls tools/models and needs the Go network "
                     "archive; build it with: go build -buildmode=c-archive -o "
                     "build/glibhermesnet.a ./cmd/hermesnet",
                     out_json, cap, out_len);
    }
    if (!has_go_runtime_prefix(net)) {
      char msg[1024];
      std::snprintf(msg, sizeof msg,
                    "hermes compile: \"%s\" carries a Go runtime but is not in the g* namespace. "
                    "Rename it to glibhermesnet.a (or gnet.a) so the target gate can see it; all "
                    "Go-runtime libraries must start with 'g'.",
                    net.c_str());
      return refusal("E3103", msg, out_json, cap, out_len);
    }
    if (!t_native) {
      char msg[1024];
      std::snprintf(msg, sizeof msg,
                    "hermes compile: the built-in Go network archive is built for the host and "
                    "cannot be linked into an artifact cross-compiled to %s. Build a g* c-archive "
                    "for %s and pass it with --link-static, or compile this unit natively.",
                    t_raw.c_str(), t_raw.c_str());
      return refusal("E3104", msg, out_json, cap, out_len);
    }
    plan_static.push_back(net);  // 网络归档排最前：静态归档按命令行顺序解析
    go_libs.push_back(net);
    used_net = true;
  }
  plan_static.insert(plan_static.end(), statics.begin(), statics.end());
  plan_dynamic.insert(plan_dynamic.end(), dynamics.begin(), dynamics.end());
  for (const std::string& lib : plan_static) {
    if (is_go_runtime_lib(lib) && std::find(go_libs.begin(), go_libs.end(), lib) == go_libs.end()) {
      go_libs.push_back(lib);
    }
  }
  for (const std::string& lib : plan_dynamic) {
    if (is_go_runtime_lib(lib) && std::find(go_libs.begin(), go_libs.end(), lib) == go_libs.end()) {
      go_libs.push_back(lib);
    }
  }

  // 框架由**对账后**的 Go-runtime 面决定，不是由 NeedNet 单独决定：
  // 用户自带的 g* c-archive 在 NeedNet=false 时同样会把 CoreFoundation/Security 拉进来。
  std::string frameworks;
  if (t_goos == "darwin" && !go_libs.empty()) {
    frameworks = "-framework CoreFoundation -framework Security";
  }

  hm::JVal o = hm::JVal::object();
  o.set("static", hm::JVal::string(join_csv(plan_static)));
  o.set("dynamic", hm::JVal::string(join_csv(plan_dynamic)));
  o.set("frameworks", hm::JVal::string(frameworks));
  o.set("go_runtime", hm::JVal::string(join_csv(go_libs)));
  o.set("used_net", hm::JVal::boolean(used_net));
  const std::string js = o.str();
  const size_t need = js.size() + 1;
  if (need > cap) {
    *out_len = need;
    return -4;
  }
  std::memcpy(out_json, js.data(), js.size());
  out_json[js.size()] = 0;
  *out_len = need;
  return 0;
}

// hm_check_pure_c_abi：把纯 C ABI 闸门单独暴露出来（link_gate 复用同一条判定，
// 避免"两份 ABI 判定"漂移）。
extern "C" int hm_check_pure_c_abi(const char* flag, const char* lib, char* out, size_t cap,
                                   size_t* out_len) {
  if (flag == nullptr || lib == nullptr || out == nullptr || out_len == nullptr) return -1;
  return check_pure_c_abi(flag, lib, out, cap, out_len);
}
