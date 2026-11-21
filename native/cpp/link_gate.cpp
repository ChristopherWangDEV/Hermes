// link_gate.cpp —— `compile` 的输入侧闸门（与 cmd_compile.go 的
// staticLibcGate / linkInputGate / isNativeLibPath 逐字对应）。
//
// 为什么输入侧闸门不可省：静态 `.a` **不产生 DT_NEEDED**，产物侧根本查不到它。
// 唯一的防线就是这里——库路径进入链接面之前。
#include "hermes_parser.h"

#include <sys/stat.h>

#include <cstdio>
#include "json_write.h"

#include <cstdio>
#include <cstring>
#include <string>

namespace {

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

bool ends_with(const std::string& s, const char* suffix) {
  const size_t n = std::strlen(suffix);
  return s.size() >= n && s.compare(s.size() - n, n, suffix) == 0;
}

// versionedSo：`libfoo.so.1`、`libfoo.so.1.2` 这类带版本号的共享库。
bool versioned_so(const std::string& b) {
  const size_t i = b.rfind(".so.");
  if (i == std::string::npos || i + 4 >= b.size()) return false;
  for (size_t k = i + 4; k < b.size(); ++k) {
    if (b[k] == '.') continue;
    if (b[k] < '0' || b[k] > '9') return false;
  }
  return true;
}

bool is_native_lib_path(const std::string& path) {
  const std::string b = lower(base_of(path));
  return ends_with(b, ".a") || ends_with(b, ".dylib") || ends_with(b, ".so") || versioned_so(b);
}

int emit_json(char* out, size_t cap, size_t* out_len, const char* code, const std::string& msg) {
  hm::JVal o = hm::JVal::object();
  o.set("code", hm::JVal::string(code));
  o.set("message", hm::JVal::string(msg));
  const std::string js = o.str();
  if (out != nullptr && cap > 0) {
    const size_t n = js.size() < cap - 1 ? js.size() : cap - 1;
    std::memcpy(out, js.c_str(), n);
    out[n] = 0;
  }
  if (out_len != nullptr) *out_len = js.size() + 1;
  return 1;
}

}  // namespace

// hm_static_libc_gate：Linux 上静态链 glibc（LGPL-2.1）会产生 LGPL 义务，默认拒绝。
// 与 Go 版一致：只对 linux + static 生效，且尊重 HERMES_ALLOW_GLIBC_STATIC=1。
extern "C" int hm_static_libc_gate(const char* goos, int is_static, char* out, size_t cap,
                                   size_t* out_len) {
  if (goos == nullptr || out == nullptr || out_len == nullptr) return -1;
  if (!is_static || std::strcmp(goos, "linux") != 0) return 0;
  const char* allow = std::getenv("HERMES_ALLOW_GLIBC_STATIC");
  if (allow != nullptr && std::strcmp(allow, "1") == 0) return 0;
  return emit_json(out, cap, out_len, "E3105",
              "hermes compile: static linking is refused on Linux.\n"
              "Reason: statically linking glibc (LGPL-2.1) creates LGPL obligations.\n"
              "Option 1 (recommended): use the musl compiler (libc is MIT) and point CC at it.\n"
              "Option 2: drop --static and use dynamic linking for glibc, and document that the "
              "target machine's glibc is required.\n"
              "Option 3 (controlled exception): HERMES_ALLOW_GLIBC_STATIC=1 - you assume the "
              "licensing judgement yourself");
}

// hm_link_input_gate：--link-* 的每一个路径。
// 契约是"本机已有的库文件路径"，不是让系统库名或链接器选项透传。
extern "C" int hm_link_input_gate(const char* flag, const char* lib, char* out, size_t cap,
                                  size_t* out_len) {
  if (flag == nullptr || lib == nullptr || out == nullptr || out_len == nullptr) return -1;
  const std::string f = flag;
  const std::string l = lib;
  if (l.empty()) {
    return emit_json(out, cap, out_len, "E3111", "hermes compile: " + f + " needs a non-empty library file path");
  }
  // 裸 -lxxx / -Lxxx / -Wl,... / 任何以 '-' 开头的选项都不接受。
  if (l[0] == '-' || l.find("-Wl,") != std::string::npos) {
    return emit_json(out, cap, out_len, "E3111",
                "hermes compile: " + f + " \"" + l +
                    "\" is not a library file path: bare -l<name> system libraries and -Wl, "
                    "linker options are not accepted; pass an existing .a / .so / .dylib file on "
                    "this machine");
  }
  // 纯 C ABI 主闸（ABI 原因，与许可原因分开）：放在许可之前，保证一个码一个原因。
  {
    char cmsg[1024] = {0};
    size_t cn = 0;
    const int rc = hm_check_pure_c_abi(f.c_str(), l.c_str(), cmsg, sizeof cmsg, &cn);
    if (rc == 1) {
      return emit_json(out, cap, out_len, "E3105", cmsg);
    }
  }
  // 许可闭集（纵深防御：纯 C ABI 查过之后仍要查一次——两件事的理由不同）
  {
    char detail[512] = {0};
    size_t dn = 0;
    if (hm_linkpolicy_detail(l.c_str(), detail, sizeof detail, &dn) == 1) {
      hm::JVal d;
      size_t dp = 0;
      if (hm::parse_json(detail, dn > 0 ? dn - 1 : 0, &d, &dp) && d.kind() == hm::JVal::kObj) {
        auto fld = [&](const char* k) -> std::string {
          const hm::JVal* v = d.find(k);
          return (v != nullptr && v->kind() == hm::JVal::kStr) ? v->as_str() : "";
        };
        char msg[1536];
        std::snprintf(msg, sizeof msg,
                      "hermes compile: %s \"%s\" is a %s runtime (%s): %s. The licence red line "
                      "forbids it from entering the link; the closed set is in "
                      "internal/linkpolicy (source: %s)",
                      f.c_str(), l.c_str(), fld("name").c_str(), fld("licence").c_str(),
                      fld("reason").c_str(), fld("source").c_str());
        return emit_json(out, cap, out_len, "E3105", msg);
      }
    }
  }
  if (!is_native_lib_path(l)) {
    return emit_json(out, cap, out_len, "E3111",
                "hermes compile: " + f + " \"" + l +
                    "\" is not a native library (expected .a, .so / .so.N, or .dylib)");
  }
  struct stat st{};
  if (::stat(l.c_str(), &st) != 0) {
    return emit_json(out, cap, out_len, "E3111",
                "hermes compile: " + f + " \"" + l +
                    "\" is not an existing library on this machine");
  }
  if (S_ISDIR(st.st_mode) || !S_ISREG(st.st_mode)) {
    return emit_json(out, cap, out_len, "E3111",
                "hermes compile: " + f + " \"" + l + "\" is not a regular library file");
  }
  return 0;
}
