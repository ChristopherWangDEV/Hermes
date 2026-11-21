// target.cpp —— `--target` 的分类器（与 internal/toolchain.ParseTarget 逐字对应）。
//
// 为什么这个分类必须唯一且闭合：它决定**Go runtime 能不能进产物**。
// 把 freestanding 目标误判成 hosted，就是固件里被塞进一个 Go 运行时的路径。
// 所以这里的每一张表都是**闭集**，不认识的一律报错、绝不猜。
#include "hermes_parser.h"
#include "json_write.h"

#include <cstring>
#include <string>
#include <vector>

namespace {

struct Target {
  std::string raw, triple, arch, vendor, os, env, goos;
  std::string cls = "unknown";  // host | bare-metal | unknown
  bool is_native = false;
  bool explicit_ = false;
};

const char* kHostedOS[][2] = {
    {"darwin", "darwin"},   {"macos", "darwin"},     {"ios", "darwin"},
    {"linux", "linux"},     {"android", "linux"},    {"windows", "windows"},
    {"win32", "windows"},   {"freebsd", "freebsd"},  {"netbsd", "netbsd"},
    {"openbsd", "openbsd"}, {"dragonfly", "dragonfly"},
};

const char* kArchGOARCH[][2] = {
    {"x86_64", "amd64"}, {"amd64", "amd64"},   {"aarch64", "arm64"},
    {"arm64", "arm64"},  {"i386", "386"},      {"i486", "386"},
    {"i586", "386"},     {"i686", "386"},      {"x86", "386"},
    {"riscv64", "riscv64"}, {"loongarch64", "loong64"},
};

const char* kBareMetalOS[] = {"none"};
const char* kBareMetalEnvMarkers[] = {"eabi", "eabihf", "elf"};
const char* kHostedEnvMarkers[] = {"gnu", "gnueabi", "gnueabihf", "musl",
                                   "musleabi", "musleabihf", "msvc", "android"};

bool contains(const char* const* set, size_t n, const std::string& v) {
  for (size_t i = 0; i < n; ++i) {
    if (v == set[i]) return true;
  }
  return false;
}

template <size_t N>
bool contains(const char* const (&set)[N], const std::string& v) {
  return contains(set, N, v);
}

std::string hosted_norm(const std::string& os) {
  for (const auto& row : kHostedOS) {
    if (os == row[0]) return row[1];
  }
  return "";
}

std::string arch_goarch(const std::string& arch) {
  for (const auto& row : kArchGOARCH) {
    if (arch == row[0]) return row[1];
  }
  return "";
}

std::string lower(std::string s) {
  for (char& c : s) {
    if (c >= 'A' && c <= 'Z') c = static_cast<char>(c - 'A' + 'a');
  }
  return s;
}

std::vector<std::string> split(const std::string& s, char sep) {
  std::vector<std::string> out;
  std::string cur;
  for (char c : s) {
    if (c == sep) {
      out.push_back(cur);
      cur.clear();
    } else {
      cur.push_back(c);
    }
  }
  out.push_back(cur);
  return out;
}

// 已知 OS 的排序列表（用于报错文案：与 Go 一样按字典序）。
std::string known_os_list() {
  std::vector<std::string> all;
  for (const auto& row : kHostedOS) all.push_back(row[0]);
  all.push_back("none");
  std::sort(all.begin(), all.end());
  std::string out;
  for (size_t i = 0; i < all.size(); ++i) {
    if (i) out += ", ";
    out += all[i];
  }
  return out;
}

}  // namespace

// emit_err 把错误文案写进出参并返回 1（错误路径统一走这里，避免漏写 out_err）。
static int emit_err(char* out_err, size_t err_cap, size_t* err_len, const std::string& msg) {
  if (out_err != nullptr && err_cap > 0) {
    const size_t n = msg.size() < err_cap - 1 ? msg.size() : err_cap - 1;
    std::memcpy(out_err, msg.c_str(), n);
    out_err[n] = 0;
  }
  if (err_len != nullptr) *err_len = msg.size() + 1;
  return 1;
}

// 返回 0 成功（out_json 是 Target 的 JSON）、1 失败（out_err 是错误文案）、
// -1 参数非法、-4 缓冲不足（*out_len / *err_len 给出所需大小）。
extern "C" int hm_target_parse(const char* raw, const char* goos, const char* goarch, char* out_json,
                               size_t cap, size_t* out_len, char* out_err, size_t err_cap,
                               size_t* err_len) {
  if (raw == nullptr || goos == nullptr || goarch == nullptr || out_json == nullptr ||
      out_len == nullptr) {
    return -1;
  }
  const std::string r = raw;
  const std::string host_goos = goos;
  const std::string host_goarch = goarch;
  Target t;
  std::string err;

  if (r.empty()) {
    t.goos = host_goos;
    t.arch = host_goarch;
    t.cls = "host";
    t.is_native = true;
  } else {
    t.raw = r;
    t.explicit_ = true;
    t.triple = r;

    if (r.find('-') == std::string::npos) {
      if (hosted_norm(lower(r)).empty()) {
        err = "hermes compile: --target \"" + r +
              "\" is not a known GOOS and not a target triple; use a full LLVM triple such as "
              "x86_64-unknown-linux-gnu, or one of the supported GOOS values";
        return emit_err(out_err, err_cap, err_len, err);
      }
      if (lower(r) != host_goos) {
        err = "hermes compile: cross-compiling to GOOS \"" + r +
              "\" needs a full LLVM target triple (for example x86_64-unknown-linux-gnu or "
              "thumbv7em-none-eabi); a bare GOOS carries no arch or ABI information";
        return emit_err(out_err, err_cap, err_len, err);
      }
      t.os = lower(r);
      t.goos = host_goos;
      t.cls = "host";
      t.is_native = true;
      t.triple.clear();
    } else {
      const std::vector<std::string> parts = split(r, '-');
      if (parts.size() < 3 || parts.size() > 4) {
        err = "hermes compile: --target \"" + r +
              "\" is not an LLVM target triple; expected <arch>-<vendor>-<os>[-<env>] (3 or 4 "
              "fields)";
        return emit_err(out_err, err_cap, err_len, err);
      }
      for (const std::string& p : parts) {
        if (p.empty()) {
          err = "hermes compile: --target \"" + r + "\" has an empty triple field";
          return emit_err(out_err, err_cap, err_len, err);
        }
      }
      t.arch = lower(parts[0]);
      t.vendor = lower(parts[1]);
      t.os = lower(parts[2]);
      if (parts.size() == 4) {
        t.env = lower(parts[3]);
      } else if (hosted_norm(t.os).empty()) {
        // 3 字段简写有两种意思相反的形状，必须分别匹配：
        //   arch-none-eabi        → OS none，env eabi（裸机）
        //   arch-linux-gnueabihf  → OS linux，env gnu（宿主）
        if (t.vendor == "none" && contains(kBareMetalEnvMarkers, t.os)) {
          t.env = t.os;
          t.os = "none";
        } else if (!hosted_norm(t.vendor).empty() && contains(kHostedEnvMarkers, t.os)) {
          t.env = t.os;
          t.os = t.vendor;
          t.vendor = "unknown";
        }
      }

      const std::string norm = hosted_norm(t.os);
      const bool bare = contains(kBareMetalOS, t.os);
      if (bare) {
        t.cls = "bare-metal";
        t.goos = "none";
        t.is_native = false;
      } else if (!norm.empty()) {
        t.cls = "host";
        t.goos = norm;
        t.is_native = (norm == host_goos) && (arch_goarch(t.arch) == host_goarch);
      } else {
        err = "hermes compile: --target \"" + r + "\" has OS field \"" + t.os +
              "\" which is not in the closed set [" + known_os_list() +
              "]; refusing to guess whether it is hosted or bare-metal, because that guess "
              "decides whether a Go runtime may enter the artifact";
        return emit_err(out_err, err_cap, err_len, err);
      }
    }
  }

  hm::JVal o = hm::JVal::object();
  o.set("raw", hm::JVal::string(t.raw));
  o.set("triple", hm::JVal::string(t.triple));
  o.set("arch", hm::JVal::string(t.arch));
  o.set("vendor", hm::JVal::string(t.vendor));
  o.set("os", hm::JVal::string(t.os));
  o.set("env", hm::JVal::string(t.env));
  o.set("goos", hm::JVal::string(t.goos));
  o.set("class", hm::JVal::string(t.cls));
  o.set("native", hm::JVal::boolean(t.is_native));
  o.set("explicit", hm::JVal::boolean(t.explicit_));
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
