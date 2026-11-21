// layout.cpp —— "我需要的那些文件在哪"（与 internal/layout 同一套顺序）。
//
// 为什么这值得单独实现：开发时资源在仓库布局里（build/、include/），而用户
// 下载到的是**发布包**（bin/ + lib/ + include/）。两套都要能找到，且**顺序固定**
// ——否则同一个二进制在不同目录下行为不同，而那种 bug 只在用户的机器上出现。
//
// 无 IO 原则在这里不适用：layout 的**全部工作**就是探文件系统。
#include "hermes_parser.h"

#include <sys/stat.h>
#include <unistd.h>

#include <cstdlib>
#include <cstring>
#include <string>
#include <vector>

namespace {

bool is_file(const std::string& p) {
  struct stat st{};
  return ::stat(p.c_str(), &st) == 0 && !S_ISDIR(st.st_mode);
}

bool is_dir(const std::string& p) {
  struct stat st{};
  return ::stat(p.c_str(), &st) == 0 && S_ISDIR(st.st_mode);
}

std::string dir_of(const std::string& p) {
  const size_t s = p.find_last_of('/');
  if (s == std::string::npos) return ".";
  if (s == 0) return "/";
  return p.substr(0, s);
}

std::string base_of(const std::string& p) {
  const size_t s = p.find_last_of('/');
  return (s == std::string::npos) ? p : p.substr(s + 1);
}

std::string stem_of(const std::string& p) {
  const std::string b = base_of(p);
  const size_t dot = b.find_last_of('.');
  return (dot == std::string::npos || dot == 0) ? b : b.substr(0, dot);
}

std::string abs_path(const std::string& p) {
  char buf[4096];
  if (::realpath(p.c_str(), buf) != nullptr) return std::string(buf);
  std::string out = p;
  if (!out.empty() && out[0] == '/') return out;
  char cwd[4096];
  if (::getcwd(cwd, sizeof cwd) != nullptr) {
    std::string base(cwd);
    if (!base.empty() && base.back() != '/') base += '/';
    return base + p;
  }
  return p;
}

void set_str(char* out, size_t cap, const std::string& v) {
  if (out == nullptr || cap == 0) return;
  const size_t n = v.size() < cap - 1 ? v.size() : cap - 1;
  std::memcpy(out, v.c_str(), n);
  out[n] = 0;
}

}  // namespace

// hm_layout_header_for：推出运行时库的 C 头（先 hermes_rt.h，再 <stem>.h）。
extern "C" int hm_layout_header_for(const char* lib_path, char* out, size_t cap) {
  if (lib_path == nullptr || out == nullptr || cap == 0) return -1;
  const std::string lib = lib_path;
  const std::string dir = dir_of(lib);
  for (const std::string& cand :
       {dir + "/hermes_rt.h", dir + "/" + stem_of(lib) + ".h"}) {
    if (is_file(cand)) {
      set_str(out, cap, cand);
      return 1;
    }
  }
  out[0] = 0;
  return 0;
}

// hm_layout_resolve：按固定顺序找资源。
//   1. HERMES_HOME
//   2. 可执行文件所在目录的上一层（发布包布局）
//   3. 当前目录（开发布局）
// 顺序不能反：开发时当前目录也常有 build/，但那不代表用户的安装位置。
extern "C" int hm_layout_resolve(const char* exec_path, const char* cwd, char* out_root,
                                 size_t root_cap, char* out_lib, size_t lib_cap, char* out_include,
                                 size_t inc_cap, char* out_shim, size_t shim_cap) {
  if (out_root == nullptr || out_lib == nullptr || out_include == nullptr || out_shim == nullptr) {
    return -1;
  }
  std::string bin_dir, root;
  if (exec_path != nullptr && exec_path[0] != 0) {
    const std::string real = abs_path(exec_path);  // 解符号链接（安装后常有人软链）
    bin_dir = dir_of(real);
    root = dir_of(bin_dir);
  }

  std::vector<std::string> candidates;
  const char* home = std::getenv("HERMES_HOME");
  if (home != nullptr && home[0] != 0) candidates.push_back(home);
  if (!root.empty()) candidates.push_back(root);
  if (cwd != nullptr && cwd[0] != 0) candidates.push_back(cwd);

  std::string lib_path, include_dir, shim;
  for (const std::string& r : candidates) {
    if (lib_path.empty()) {
      for (const std::string& p : {r + "/lib/libhermesrt.a", r + "/build/libhermesrt.a"}) {
        if (is_file(p)) {
          lib_path = p;
          break;
        }
      }
    }
    if (include_dir.empty()) {
      for (const std::string& p : {r + "/include", r + "/../include"}) {
        if (is_dir(p + "/hermes")) {
          include_dir = p;
          break;
        }
      }
    }
    if (shim.empty()) {
      for (const std::string& p : {r + "/bin/hermes-shim", r + "/build/hermes-shim"}) {
        if (is_file(p)) {
          shim = p;
          break;
        }
      }
    }
    if (!lib_path.empty() && !include_dir.empty()) {
      if (root.empty()) root = r;
      break;
    }
  }
  if (root.empty() && !candidates.empty()) root = candidates[0];

  set_str(out_root, root_cap, root);
  set_str(out_lib, lib_cap, lib_path);
  set_str(out_include, inc_cap, include_dir);
  set_str(out_shim, shim_cap, shim);
  return 0;
}
