// cli.cpp —— C++ 主命令行（`hermes` 的接班者）。
//
// 迁移进行中：Go 的 `hermes` 仍在，已搬过来的子命令由它 **exec** 本程序。
// 每搬一个命令，Go 侧就删掉一份实现——不做"两版并存、Go 兜底"。
//
// 为什么先从这里开始：`version` 与 `lsp` 的能力全部来自 C++（ABI/构建 id 来自
// hm_abi_*，LSP 本来就已经是 C++ 实现），搬它们不需要把任何 Go 逻辑再写一遍。
#include "hermes_abi.h"
#include "hermes_diag.h"

#include "json_write.h"

// 链接面在 C ABI 上是逗号分隔字符串；这两个助手在全文件多处使用，放在最前。
std::vector<std::string> split_csv_c(const std::string& s);
std::string split_join(const std::string& csv);
#include "hermes_parser.h"

#include <cstdio>
#include <cstring>
#include <cstdlib>
#if defined(__APPLE__)
#include <mach-o/dyld.h>
#endif
#include <dirent.h>
#include <sys/stat.h>
#include <cerrno>
#include <sys/stat.h>
#include <sys/wait.h>
#include <unistd.h>
#include <string>
#include <algorithm>
#include <map>
#include <set>
#include <tuple>
#include <functional>

extern "C" int hm_sha256(const unsigned char* data, size_t len, unsigned char* out);
extern "C" int hm_std_source(const char* name, char* out, size_t cap, size_t* out_len);
extern "C" int hm_std_count(void);
extern "C" const char* hm_std_name(int i);
extern "C" const char* hm_tr(const char* key, const char* lang);
extern "C" int hm_archive_write(const char* members_json, size_t len, unsigned char* out, size_t cap, size_t* out_len);
extern "C" int hm_diag_render_json_all(const char* src, size_t src_len, const char* json, size_t json_len, char* out, size_t cap, size_t* out_len);
extern "C" int hm_diag_render_json_keep_going(const char* src, size_t src_len, const char* json, size_t json_len, char* out, size_t cap, size_t* out_len);
extern "C" int hm_diag_render_json_src(const char* src, size_t src_len, const char* json,
                                       size_t json_len, const char* lang, char* out, size_t cap,
                                       size_t* out_len);
extern "C" int hm_codegen_backends(const unsigned char* src, size_t len, const char* filename, int is_header, char* out, size_t cap, size_t* out_len, char* out_diag, size_t diag_cap, size_t* diag_len);
extern "C" int hm_codegen_tools(const unsigned char* src, size_t len, const char* filename, int is_header, char* out, size_t cap, size_t* out_len, char* out_diag, size_t diag_cap, size_t* diag_len);
extern "C" int hm_codegen_functions(const unsigned char* src, size_t len, const char* filename, int is_header, const char* ext, size_t ext_len, char* out, size_t cap, size_t* out_len, char* out_diag, size_t diag_cap, size_t* diag_len);
extern "C" int hm_codegen_types(const unsigned char* src, size_t len, const char* filename, int is_header, char* out, size_t cap, size_t* out_len, char* out_diag, size_t diag_cap, size_t* diag_len);
extern "C" int hm_codegen_wfdecls(const unsigned char* src, size_t len, const char* filename,
                                 int is_header, char* out, size_t cap, size_t* out_len,
                                 char* out_diag, size_t diag_cap, size_t* diag_len);
extern "C" int hm_codegen_ext(const unsigned char* src, size_t len, const char* filename,
                              int is_header, const char* ext_types, size_t ext_len, char* out,
                              size_t cap, size_t* out_len, char* out_diag, size_t diag_cap,
                              size_t* diag_len);
#include <vector>

// lsp.cpp 提供的入口（HM_LSP_MAIN 不在本 TU 定义，所以它不会生成 main）。
int hm_lsp_main();

// link_cmd.cpp 的命令入口（自己解析 argv，与其它子命令同形）。
int hm_link_main(int argc, char** argv);
int hm_trace_main(int argc, char** argv);


namespace {

#if defined(__APPLE__)
#define HM_HOST_GOOS "darwin"
#elif defined(__FreeBSD__)
#define HM_HOST_GOOS "freebsd"
#elif defined(__linux__)
#define HM_HOST_GOOS "linux"
#elif defined(_WIN32)
#define HM_HOST_GOOS "windows"
#else
#define HM_HOST_GOOS "unknown"
#endif

#if defined(__aarch64__) || defined(__arm64__)
#define ARCH_NAME "arm64"
#define HM_HOST_GOARCH "arm64"
#elif defined(__x86_64__)
#define ARCH_NAME "amd64"
#define HM_HOST_GOARCH "amd64"
#else
#define ARCH_NAME "unknown"
#define HM_HOST_GOARCH "unknown"
#endif

// current_lang 与 Go 侧同源：HERMES_LANG 为 "zh" 时用中文，其余一律英文。
const char* current_lang() {
  // 与 Go 的 diag.LangFromEnv 同口径：zh / zh-CN / zh-Hans 都算中文。
  const char* env = std::getenv("HERMES_LANG");
  return (env != nullptr && std::strncmp(env, "zh", 2) == 0) ? "zh" : "en";
}

// tr 走语言包：key 就是英文原文（与 Go 侧 `tr("...")` 同一约定）。
// 没有译文时 hm_tr 原样返回 key —— 绝不返回空串，宁可英文也不给空白。

// host_exe_path 取本进程的可执行文件路径（失败时返回空串）。
//
// 三个平台三条路，互不干扰：macOS 用 `_NSGetExecutablePath`，Linux 用
// `/proc/self/exe`，FreeBSD 用 `sysctl(KERN_PROC_PATHNAME)`。写成同一个函数
// 而不是在四处各写一遍 `#if`：那四处里任何一处漏掉分支，症状都是"某个命令在
// 某个平台上找不到自己的安装根"，而报错会指向完全不相关的东西。
//
// 为什么 FreeBSD 不用 /proc：FreeBSD 的 procfs 是可选的（默认不挂），
// 依赖它的代码在这台机器上能跑、在那台机器上找不到路径。
#if defined(__FreeBSD__)
#include <sys/types.h>
#include <sys/sysctl.h>
#endif

std::string host_exe_path() {
  char exe[4096] = {0};
#if defined(__APPLE__)
  {
    uint32_t sz = sizeof exe;
    if (_NSGetExecutablePath(exe, &sz) != 0) return std::string();
  }
#elif defined(__FreeBSD__)
  {
    int mib[4] = {CTL_KERN, KERN_PROC, KERN_PROC_PATHNAME, -1};
    size_t sz = sizeof exe;
    if (sysctl(mib, 4, exe, &sz, nullptr, 0) != 0) return std::string();
  }
#elif defined(__linux__)
  {
    const ssize_t n = ::readlink("/proc/self/exe", exe, sizeof exe - 1);
    if (n <= 0) return std::string();
  }
#endif
  return std::string(exe);
}

const char* tr(const char* key) { return hm_tr(key, current_lang()); }

// trf：先翻译再格式化。**键必须是单个字符串字面量**——跨行拼接会让提取器
// 只取到第一段，语言包里就会出现被截断的键（实测踩过）。
std::string trf(const char* key, ...) {
  const std::string fmt = tr(key);
  char buf[2048];
  va_list ap;
  va_start(ap, key);
  std::vsnprintf(buf, sizeof buf, fmt.c_str(), ap);
  va_end(ap);
  return std::string(buf);
}

int cmd_version() {
  const unsigned abi = hm_abi_version();
  const char* native_id = hm_abi_build_id();

  // 与 Go 版同形，但**没有 `go` 行**：这条命令现在由 C++ 主程序回答，
  // 而"本产物依赖哪个 Go 运行时"这个问题在去 Go 之后不再成立。
  std::printf("hermes %s\n", HERMES_SEMVER);
  std::printf("  build_id   %s\n", HERMES_BUILD_ID);
  std::printf("  abi        %u.%u\n", abi >> 16, abi & 0xffffu);
  std::printf("  native_id  %s\n", native_id ? native_id : "");
#if defined(__APPLE__)
  std::printf("  platform   darwin/%s\n", ARCH_NAME);
#elif defined(__FreeBSD__)
  std::printf("  platform   freebsd/%s\n", ARCH_NAME);
#elif defined(__linux__)
  std::printf("  platform   linux/%s\n", ARCH_NAME);
#else
  std::printf("  platform   %s/%s\n", "unknown", ARCH_NAME);
#endif
  int32_t arch = 0;
  const char* arch_name = "unavailable";
  if (hm_cycle_arch(&arch) == 0) {
    arch_name = (arch == 1) ? "arm64" : (arch == 2 ? "x86_64" : "unknown");
  }
  std::printf("  cycle_arch %s\n", arch_name);
  return 0;
}

int usage(FILE* out) {
  std::fprintf(out,
               "hermes (native) - commands already ported to C++\n"
               "\n"
               "Usage:\n"
               "  hermes-native version\n"
               "  hermes-native lsp\n"
               "  hermes-native explain <CODE> [--why] [--json]\n"
               "  hermes-native fmt [--check|--expand] <file...>\n"
               "  hermes-native deps --resolve [--modules] <file...>\n"
               "  hermes-native ir <file> [-o out.json]\n"
               "  hermes-native cc <src.c> --unit <unit.hbc> [-o prog]\n"
               "\n"
               "Notes:\n"
               "  The Go `hermes` binary dispatches these commands here; the rest\n"
               "  still live in Go. This binary is the successor to that CLI.\n");
  return 0;
}

}  // namespace


namespace {


// pretty_print 复刻 Go 的 `json.MarshalIndent(v, "", "  ")`：
// 两空格缩进、键序沿用输入（canonical 写出器已经排好序）。
//
// 为什么自己写而不是引 JSON 库：输出必须与 Go 版逐字节一致，而"一致的
// 缩进规则"只有十几行；引一个库反而要审它的排版细节与许可证。
std::string pretty_print(const std::string& compact) {
  std::string out;
  int depth = 0;
  bool in_str = false, esc = false;
  auto newline = [&]() {
    out += '\n';
    out.append(static_cast<size_t>(depth) * 2, ' ');
  };
  for (size_t i = 0; i < compact.size(); ++i) {
    const char c = compact[i];
    if (in_str) {
      out += c;
      if (esc) esc = false;
      else if (c == '\\') esc = true;
      else if (c == '"') in_str = false;
      continue;
    }
    switch (c) {
      case '"': in_str = true; out += c; break;
      case '{':
      case '[':
        out += c;
        if (i + 1 < compact.size() && (compact[i + 1] == '}' || compact[i + 1] == ']')) {
          out += compact[++i];
          break;
        }
        ++depth;
        newline();
        break;
      case '}':
      case ']':
        --depth;
        newline();
        out += c;
        break;
      case ',':
        out += c;
        newline();
        break;
      case ':':
        out += ": ";
        break;
      default:
        out += c;
        break;
    }
  }
  return out;
}

bool color_enabled(const std::string& mode) {
  if (mode == "always") return true;
  if (mode == "never") return false;
  // auto：只在交互终端上色，并尊重 NO_COLOR / TERM=dumb（与 rustc 一致）。
  const char* no = std::getenv("NO_COLOR");
  if (no != nullptr && *no != 0) return false;
  const char* term = std::getenv("TERM");
  if (term != nullptr && std::strcmp(term, "dumb") == 0) return false;
  return isatty(STDOUT_FILENO) != 0;
}

std::string colorize(const std::string& text, bool /*is_error*/, bool use_color) {
  if (!use_color) return text;
  std::string out;
  bool first = true;
  size_t pos = 0;
  while (pos <= text.size()) {
    size_t nl = text.find('\n', pos);
    std::string line = text.substr(pos, (nl == std::string::npos ? text.size() : nl) - pos);
    if (first && line.rfind("error[", 0) == 0) {
      out += "\x1b[1;31m" + line + "\x1b[0m";
    } else if (first && line.rfind("warning[", 0) == 0) {
      out += "\x1b[1;33m" + line + "\x1b[0m";
    } else {
      // 前缀（why: / help: / note:）用暗色，正文保持原样。
      const size_t colon = line.find(": ");
      const bool prefix = colon != std::string::npos && colon <= 6 &&
                          line.find_first_not_of(" ") == line.find_first_of("whyhelpnot");
      if (prefix) {
        out += line.substr(0, colon + 2);
        out += "\x1b[2m";
        out += line.substr(colon + 2);
        out += "\x1b[0m";
      } else {
        out += line;
      }
    }
    first = false;
    if (nl == std::string::npos) break;
    out += '\n';
    pos = nl + 1;
  }
  return out;
}

// 定义在文件后半段（同一匿名 namespace 内）：explain 的 IR 分支要用。
bool read_file(const char* path, std::string* out);
bool file_exists(const std::string& p);

int explain_code(const std::string& code, bool as_json, bool why_only,
                 const std::string& color_mode) {
  char buf[16384];
  size_t n = 0;
  const hm_status rc = as_json || why_only
                           ? hm_diag_explain_json(code.c_str(), current_lang(), buf, sizeof buf, &n)
                           : hm_diag_explain(code.c_str(), current_lang(), buf, sizeof buf, &n);
  if (rc != HM_OK) {
    std::fprintf(stderr, "hermes explain: no explanation registered for %s\n", code.c_str());
    return 4;
  }
  if (n > 0 && buf[n - 1] == 0) --n;
  const std::string payload(buf, n);

  if (as_json || why_only) {
    // 走 JVal：它的 canonical 写出器**键排序 + HTML 转义**（<>& → \u003c 等），
    // 与 Go 的 json.Marshal 逐字节一致。-–json 的字节契约就靠这条。
    hm::JVal jv;
    size_t pos = 0;
    if (!hm::parse_json(payload.data(), payload.size(), &jv, &pos)) {
      std::fprintf(stderr, "hermes explain: malformed explanation JSON for %s\n", code.c_str());
      return 4;
    }
    if (why_only) {
      const hm::JVal* why = jv.find("why");
      if (why != nullptr && why->kind() == hm::JVal::kStr && !why->as_str().empty()) {
        std::printf("why: %s\n", why->as_str().c_str());
      }
      const hm::JVal* fixes = jv.find("fixes");
      if (fixes != nullptr && fixes->kind() == hm::JVal::kArr) {
        for (size_t i = 0; i < fixes->size(); ++i) {
          const hm::JVal* f = fixes->at(i);
          if (f != nullptr && f->kind() == hm::JVal::kStr && !f->as_str().empty()) {
            std::printf("fix: %s\n", f->as_str().c_str());
          }
        }
      }
      return 0;
    }
    std::printf("%s\n", pretty_print(jv.str()).c_str());
    return 0;
  }
  const bool is_error = payload.rfind("error[", 0) == 0;
  std::printf("%s\n", colorize(payload, is_error, color_enabled(color_mode)).c_str());
  return 0;
}


// explain_ir_file：读 IR 或 .hbc 的第一个成员，渲染契约表 / --json。
//
// 为什么 .hbc 也能直接喂：用户手上的分发物就是 .hbc；要求他先 `hermes ar x`
// 解出 IR 才能看契约，等于让"看一眼上界"这件事多一道手工步骤。
int explain_ir_file(const std::string& path, const std::string& wf_name, bool as_json) {
  std::string data;
  if (!read_file(path.c_str(), &data)) {
    std::fprintf(stderr, "hermes explain: cannot read %s\n", path.c_str());
    return 4;
  }
  if (data.size() >= 4 && data.compare(0, 4, "HBC\x1a") == 0) {
    std::vector<char> members(1 << 16);
    size_t mn = 0;
    int rc = hm_archive_read(reinterpret_cast<const unsigned char*>(data.data()), data.size(),
                             members.data(), members.size(), &mn);
    if (rc == -4 && mn > members.size()) {
      members.resize(mn);
      rc = hm_archive_read(reinterpret_cast<const unsigned char*>(data.data()), data.size(),
                           members.data(), members.size(), &mn);
    }
    if (rc != 0) {
      std::fprintf(stderr, "hermes explain: %s is not a usable archive (status %d)\n",
                   path.c_str(), rc);
      return 4;
    }
    hm::JVal arr;
    size_t pos = 0;
    if (!hm::parse_json(members.data(), mn > 0 ? mn - 1 : 0, &arr, &pos) ||
        arr.kind() != hm::JVal::kArr || arr.size() == 0) {
      std::fprintf(stderr, "hermes explain: %s has no members\n", path.c_str());
      return 4;
    }
    const hm::JVal* m0 = arr.at(0);
    const hm::JVal* off = m0->find("ir_off");
    const hm::JVal* len = m0->find("ir_len");
    if (off == nullptr || len == nullptr) return 4;
    const size_t o = static_cast<size_t>(off->as_int(0));
    const size_t k = static_cast<size_t>(len->as_int(0));
    if (o + k > data.size()) return 4;
    data.assign(data.data() + o, k);
  }

  std::vector<char> out(1 << 16), err(512);
  size_t on = 0, en = 0;
  int rc = hm_explain_ir(data.data(), data.size(), wf_name.c_str(), as_json ? 1 : 0, tr,
                         out.data(), out.size(), &on, err.data(), err.size(), &en);
  if (rc == -4) {
    out.resize(on);
    rc = hm_explain_ir(data.data(), data.size(), wf_name.c_str(), as_json ? 1 : 0, tr,
                       out.data(), out.size(), &on, err.data(), err.size(), &en);
  }
  if (rc == 1) {
    std::fprintf(stderr, "hermes explain: %s\n", err.data());
    return 4;
  }
  if (rc != 0) {
    std::fprintf(stderr, "hermes explain: this IR is not readable\n");
    return 4;
  }
  const size_t n = on > 0 ? on - 1 : 0;
  std::fwrite(out.data(), 1, n, stdout);
  return 0;
}

int cmd_explain(int argc, char** argv) {
  bool as_json = false, why_only = false;
  std::string code, color_mode = "auto", wf_name;
  bool color_set = false;
  for (int i = 2; i < argc; ++i) {
    const std::string a = argv[i];
    if (a == "--json") as_json = true;
    else if (a == "--why") why_only = true;
    else if (a == "--workflow") {
      if (i + 1 >= argc) {
        std::fprintf(stderr, "hermes explain: --workflow needs a name\n");
        return 2;
      }
      wf_name = argv[++i];
    }
    else if (a.rfind("--color=", 0) == 0) { color_mode = a.substr(8); color_set = true; }
    else if (a == "--color") {
      if (i + 1 >= argc) {
        std::fprintf(stderr, "hermes explain: --color needs auto|always|never\n");
        return 2;
      }
      color_mode = argv[++i];
      color_set = true;
    }
    else if (a == "-h" || a == "--help" || a == "help") {
      std::printf("hermes explain <CODE> [--why] [--json]\n");
      return 0;
    } else if (!a.empty() && a[0] == '-') {
      std::fprintf(stderr, "hermes explain: unknown flag %s\n", a.c_str());
      return 2;
    } else {
      code = a;
    }
  }
  if (code.empty()) {
    std::fprintf(stderr, "hermes explain: missing DIAG_CODE\n");
    return 2;
  }
  // 文件存在 ⇒ 解释这份 IR 的**契约**；否则当诊断码解释。
  // 判据是"文件在不在"而不是字符串形状：一个叫 E3002 的文件确实存在时，
  // 用户要的是那份文件，不是那条诊断。
  if (file_exists(code)) {
    // --color 只对诊断码有意义：IR 契约是纯文本表格，没有上色实现。
    // **必须报错而不是静默忽略**——静默吞掉一个开关，会让用户以为它生效了，
    // 下次照着写进 CI 也不会有人发现它一直没作用。
    if (color_set) {
      std::fprintf(stderr,
                   "%s\n", tr("hermes explain: --color only applies to a DIAG_CODE; an IR contract is printed as plain text"));
      return 2;
    }
    return explain_ir_file(code, wf_name, as_json);
  }
  return explain_code(code, as_json, why_only, color_mode);
}

}  // namespace


namespace {

// normalize_whitespace 复刻 Go 版的三件局部变换：
//   · CRLF/CR → LF
//   · 去掉每行行尾空白
//   · 恰好一个结尾换行
// 缩进**不碰**：那需要理解块结构，而"看起来对了但语义没变"没法证明。
std::string normalize_whitespace(const std::string& src) {
  std::string s;
  for (size_t i = 0; i < src.size(); ++i) {
    if (src[i] == '\r') {
      if (i + 1 < src.size() && src[i + 1] == '\n') continue;  // CRLF → LF（吃掉 CR）
      s += '\n';                                                // 单独 CR → LF
    } else {
      s += src[i];
    }
  }
  std::string out;
  size_t pos = 0;
  while (pos <= s.size()) {
    size_t nl = s.find('\n', pos);
    std::string line = s.substr(pos, (nl == std::string::npos ? s.size() : nl) - pos);
    while (!line.empty() && (line.back() == ' ' || line.back() == '\t')) line.pop_back();
    out += line;
    if (nl == std::string::npos) break;
    out += '\n';
    pos = nl + 1;
  }
  while (!out.empty() && out.back() == '\n') out.pop_back();
  out += '\n';
  return out;
}

bool read_file(const char* path, std::string* out) {
  FILE* f = std::fopen(path, "rb");
  if (f == nullptr) return false;
  char buf[16384];
  size_t n;
  out->clear();
  while ((n = std::fread(buf, 1, sizeof buf, f)) > 0) out->append(buf, n);
  std::fclose(f);
  return true;
}

int cmd_fmt(int argc, char** argv) {
  bool check = false, expand = false;
  std::vector<std::string> files;
  for (int i = 2; i < argc; ++i) {
    const std::string a = argv[i];
    if (a == "--check") check = true;
    else if (a == "--expand") expand = true;
    else if (a == "-h" || a == "--help" || a == "help") {
      std::printf("hermes fmt <file...>\n"
                  "hermes fmt --check <file...>\n"
                  "hermes fmt --expand <file...>   (writes to stdout, never modifies)\n");
      return 0;
    } else if (!a.empty() && a[0] == '-') {
      std::fprintf(stderr, "hermes fmt: unknown flag %s\n", a.c_str());
      return 2;
    } else {
      files.push_back(a);
    }
  }
  if (files.empty()) {
    std::fprintf(stderr, "hermes fmt: missing source file\n");
    return 2;
  }

  int changed = 0;
  for (const std::string& p : files) {
    std::string src;
    if (!read_file(p.c_str(), &src)) {
      std::fprintf(stderr, "hermes fmt: cannot read %s\n", p.c_str());
      return 4;
    }
    if (expand) {
      char* out = nullptr;
      size_t cap = src.size() * 4 + 1024, n = 0;
      out = static_cast<char*>(std::malloc(cap));
      if (out == nullptr) return 4;
      const int rc = hm_fmt_expand(reinterpret_cast<const uint8_t*>(src.data()), src.size(),
                                   out, cap, &n);
      if (rc != 0) {
        std::fprintf(stderr, "hermes fmt: cannot expand %s\n", p.c_str());
        std::free(out);
        return 4;
      }
      if (n > 0) std::fwrite(out, 1, n - 1, stdout);  // n 含结尾 NUL
      std::free(out);
      continue;
    }
    const std::string formatted = normalize_whitespace(src);
    if (formatted == src) continue;
    ++changed;
    if (check) {
      std::fprintf(stderr, "hermes fmt: %s needs formatting\n", p.c_str());
      continue;
    }
    FILE* f = std::fopen(p.c_str(), "wb");
    if (f == nullptr) {
      std::fprintf(stderr, "hermes fmt: cannot write %s\n", p.c_str());
      return 4;
    }
    std::fwrite(formatted.data(), 1, formatted.size(), f);
    std::fclose(f);
    std::fprintf(stderr, "hermes fmt: formatted %s\n", p.c_str());
  }
  if (check && changed > 0) return 4;
  return 0;
}

}  // namespace


namespace {

// ---- deps --resolve --------------------------------------------------------
// 依赖图解析：读入口文件，递归跟随 #include，把每个 #link（含 #bind 展开的）
// 归类成 std / local / native 并打印。
//
// 只做**声明层**的解析：不读 .hbc、不建 IR。`hermes deps` 回答的是"我声明了
// 什么、解析到了哪"，构建是另一件事。

struct Dirent {
  std::string kind, spec;
  bool angle = false;
  int line = 0, col = 0;
};

// filter_directive_diags 只保留**指令级**诊断：E3017（#link 在 .h）/ E3027
// （#bind 在 .h）/ E3028（#bind 的目标不合法）。其余诊断一律丢弃——见调用点注释。
std::string filter_directive_diags(const std::string& json) {
  hm::JVal arr;
  size_t pos = 0;
  if (!hm::parse_json(json.data(), json.size(), &arr, &pos) || arr.kind() != hm::JVal::kArr) {
    return std::string();
  }
  hm::JVal out = hm::JVal::array();
  for (size_t i = 0; i < arr.size(); ++i) {
    const hm::JVal* d = arr.at(i);
    const hm::JVal* code = (d != nullptr) ? d->find("code") : nullptr;
    if (code == nullptr || code->kind() != hm::JVal::kStr) continue;
    const std::string c = code->as_str();
    if (c == "E3017" || c == "E3027" || c == "E3028") out.push(*d);
  }
  return out.size() == 0 ? std::string() : out.str();
}

bool scan_directives(const std::string& src, const std::string& path, bool is_header,
                     std::vector<Dirent>* out, std::string* diags) {
  size_t cap = src.size() * 8 + 4096;
  for (int attempt = 0; attempt < 4; ++attempt) {
    std::vector<char> buf(cap);
    size_t n = 0;
    std::vector<char> dbuf(1 << 16);
    size_t dn = 0;
    const int rc = hm_deps_scan(reinterpret_cast<const uint8_t*>(src.data()), src.size(),
                                path.c_str(), is_header ? 1 : 0, buf.data(), buf.size(), &n,
                                dbuf.data(), dbuf.size(), &dn);
    if (rc == -4) { cap = n + 1024; continue; }
    if (rc != 0) return false;
    if (diags != nullptr && dn > 0) diags->assign(dbuf.data(), dn > 0 ? dn - 1 : 0);
    if (n > 0 && buf[n - 1] == 0) --n;
    hm::JVal arr;
    size_t pos = 0;
    if (!hm::parse_json(buf.data(), n, &arr, &pos) || arr.kind() != hm::JVal::kArr) return false;
    for (size_t i = 0; i < arr.size(); ++i) {
      const hm::JVal* it = arr.at(i);
      if (it == nullptr) continue;
      Dirent d;
      const hm::JVal* k = it->find("kind");
      const hm::JVal* sp = it->find("spec");
      const hm::JVal* an = it->find("angle");
      const hm::JVal* ln = it->find("line");
      const hm::JVal* cl = it->find("col");
      d.kind = (k && k->kind() == hm::JVal::kStr) ? k->as_str() : "";
      d.spec = (sp && sp->kind() == hm::JVal::kStr) ? sp->as_str() : "";
      d.angle = (an && an->kind() == hm::JVal::kBool) ? an->as_bool(false) : false;
      d.line = ln ? static_cast<int>(ln->as_int(0)) : 0;
      d.col = cl ? static_cast<int>(cl->as_int(0)) : 0;
      out->push_back(std::move(d));
    }
    return true;
  }
  return false;
}

std::string dir_of(const std::string& path) {
  const size_t slash = path.find_last_of('/');
  return (slash == std::string::npos) ? std::string(".") : path.substr(0, slash);
}

bool file_exists(const std::string& p) {
  struct stat st{};
  return ::stat(p.c_str(), &st) == 0 && !S_ISDIR(st.st_mode);
}

// rel_path 与 Go 的 relName 同义：相对项目根、统一用 '/'。
std::string rel_path(const std::string& root, const std::string& path) {
  std::string r = root;
  while (!r.empty() && r.back() == '/') r.pop_back();
  if (!r.empty() && path.rfind(r + "/", 0) == 0) return path.substr(r.size() + 1);
  return path;
}

bool ends_with(const std::string& s, const char* suf) {
  const size_t n = std::strlen(suf);
  return s.size() >= n && s.compare(s.size() - n, n, suf) == 0;
}

std::string join_path(const std::string& dir, const std::string& name) {
  if (dir.empty() || dir == ".") return name;
  return dir + "/" + name;
}

bool is_std_spec(const std::string& path) { return path.rfind("std/", 0) == 0; }

// classify_link 把一条 #link 的 spec 归成 std / local / native，并给出可用状态。
void classify(const std::string& spec, std::string* kind, std::string* module,
              std::string* archive, std::string* header) {
  if (spec.rfind("native ", 0) == 0) {
    *kind = "native";
    const size_t a1 = spec.find('"');
    const size_t a2 = (a1 == std::string::npos) ? std::string::npos : spec.find('"', a1 + 1);
    const size_t h1 = (a2 == std::string::npos) ? std::string::npos : spec.find('"', a2 + 1);
    const size_t h2 = (h1 == std::string::npos) ? std::string::npos : spec.find('"', h1 + 1);
    if (a1 != std::string::npos && a2 != std::string::npos) *archive = spec.substr(a1 + 1, a2 - a1 - 1);
    if (h1 != std::string::npos && h2 != std::string::npos) *header = spec.substr(h1 + 1, h2 - h1 - 1);
    *module = *archive;
    return;
  }
  std::string path = spec;
  if (path.size() >= 2 && ((path.front() == '"' && path.back() == '"') ||
                           (path.front() == '<' && path.back() == '>'))) {
    path = path.substr(1, path.size() - 2);
  }
  *module = path;
  *kind = is_std_spec(path) ? "std" : "local";
}

struct Dep {
  std::string kind, spec, module, archive, header, from;
  int line = 0;
};

void walk(const std::string& path, bool is_header, int depth, std::vector<Dep>* deps,
          std::vector<std::string>* seen, bool* failed, std::vector<std::string>* mods) {
  if (depth > 64) return;
  std::string src;
  if (!read_file(path.c_str(), &src)) {
    std::fprintf(stderr, "hermes deps: cannot read %s\n", path.c_str());
    *failed = true;
    return;
  }
  if (mods != nullptr) mods->push_back(path);
  std::vector<Dirent> dirs;
  if (!scan_directives(src, path, is_header, &dirs, nullptr)) {
    std::fprintf(stderr, "hermes deps: cannot parse %s\n", path.c_str());
    *failed = true;
    return;
  }
  const std::string base = dir_of(path);
  for (const Dirent& d : dirs) {
    if (d.kind == "include") {
      std::string target = d.spec;
      if (target.size() >= 2 && ((target.front() == '"' && target.back() == '"') ||
                                 (target.front() == '<' && target.back() == '>'))) {
        target = target.substr(1, target.size() - 2);
      }
      if (d.angle) continue;  // <...> 是内建模块，不递归文件系统
      const std::string full = join_path(base, target);
      if (std::find(seen->begin(), seen->end(), full) != seen->end()) continue;
      seen->push_back(full);
      walk(full, ends_with(full, ".h"), depth + 1, deps, seen, failed, mods);
      continue;
    }
    Dep dep;
    dep.spec = d.spec;
    dep.from = path;
    dep.line = d.line;
    classify(d.spec, &dep.kind, &dep.module, &dep.archive, &dep.header);
    deps->push_back(std::move(dep));
  }
}

int cmd_deps(int argc, char** argv) {
  bool resolve = false, show_modules = false;
  std::vector<std::string> files;
  for (int i = 2; i < argc; ++i) {
    const std::string a = argv[i];
    if (a == "--resolve") resolve = true;
    else if (a == "--modules") show_modules = true;
    else if (a == "-h" || a == "--help" || a == "help") {
      std::printf("hermes deps --resolve <file...>\n");
      return 0;
    } else if (!a.empty() && a[0] == '-') {
      std::fprintf(stderr, "hermes deps: unknown flag %s\n", a.c_str());
      return 2;
    } else {
      files.push_back(a);
    }
  }
  if (!resolve) {
    std::fprintf(stderr,
                 "hermes deps: --resolve is required (this command only reports, it does not build)\n");
    return 2;
  }
  if (files.empty()) {
    std::fprintf(stderr, "hermes deps: missing source file\n");
    return 2;
  }

  int exit_code = 0;
  for (const std::string& p : files) {
    std::vector<Dep> deps;
    std::vector<std::string> seen{p};
    std::vector<std::string> mods;
    bool failed = false;
    walk(p, false, 0, &deps, &seen, &failed, &mods);
    if (failed) return 4;
    if (show_modules) {
      // 模块顺序 = DFS 先序（与 Go 的 include.Resolve 同一顺序：它决定类型 id）。
      for (const std::string& m : mods) std::printf("module %s\n", m.c_str());
      continue;
    }
    if (deps.empty()) {
      std::fprintf(stderr, "hermes deps: %s declares no dependencies\n", p.c_str());
      continue;
    }
    std::sort(deps.begin(), deps.end(), [](const Dep& a, const Dep& b) {
      if (a.kind != b.kind) return a.kind < b.kind;
      return a.spec < b.spec;
    });
    std::printf("%s (%zu dependencies)\n", p.c_str(), deps.size());
    for (const Dep& d : deps) {
      const char* status = "declared";
      if (d.kind == "std") status = "builtin";
      else if (d.kind == "local") status = "needs -L (no implementation bytes at compile time)";
      else if (d.kind == "native") status = "native archive";
      std::printf("  %-7s %-40s %s\n", d.kind.c_str(), d.spec.c_str(), status);
      if (d.kind == "native") {
        std::printf("          archive=%s header=%s\n", d.archive.c_str(), d.header.c_str());
      }
      std::printf("          declared in %s:%d\n", d.from.c_str(), d.line);
    }
  }
  return exit_code;
}

}  // namespace


bool sha256_hex(const std::string& data, std::string* out) {
  unsigned char sum[32] = {0};
  if (::hm_sha256(reinterpret_cast<const unsigned char*>(data.data()), data.size(), sum) != 0) {
    return false;
  }
  static const char* kHex = "0123456789abcdef";
  out->clear();
  for (int i = 0; i < 32; ++i) {
    out->push_back(kHex[sum[i] >> 4]);
    out->push_back(kHex[sum[i] & 0xf]);
  }
  return true;
}

std::string json_escape(const std::string& s) {
  std::string out;
  for (unsigned char c : s) {
    switch (c) {
      case '"': out += "\\\""; break;
      case '\\': out += "\\\\"; break;
      case '\n': out += "\\n"; break;
      case '\r': out += "\\r"; break;
      case '\t': out += "\\t"; break;
      default:
        if (c < 0x20) {
          char buf[8];
          std::snprintf(buf, sizeof buf, "\\u%04x", c);
          out += buf;
        } else {
          out += static_cast<char>(c);
        }
    }
  }
  return out;
}

namespace {

// ---- ir（M4：单文件 + <std/...> 的 include/link + native link） -------------
// 覆盖不到的形态一律明确拒绝：静默产出错的 IR 比报错危险得多。


// render_diag_json_mode：按口径渲染。默认只给第一条 + "被抑制"提示；
// keep_going 时最多 50 条、不提"首错即停"。
// RenderMode：三个命令的口径**不一样**（实测确认）：
//   · `ir`    —— Go 不设截断，**全部**打印；
//   · `build` —— 默认"渲染到第一条 error 为止"+ 抑制提示；
//   · `build --keep-going` —— 最多 50 条，不提抑制。
enum class RenderMode { kAll, kFirstError, kKeepGoing };

void render_diag_json_mode(const std::string& src, const char* data, size_t len, RenderMode mode,
                           FILE* out = stderr) {
  std::vector<char> buf(1 << 18);
  size_t n = 0;
  const int rc =
      (mode == RenderMode::kKeepGoing)
          ? hm_diag_render_json_keep_going(src.data(), src.size(), data, len, buf.data(), buf.size(),
                                           &n)
          : (mode == RenderMode::kAll)
                ? hm_diag_render_json_all(src.data(), src.size(), data, len, buf.data(), buf.size(),
                                          &n)
                : hm_diag_render_json_src(src.data(), src.size(), data, len, current_lang(),
                                          buf.data(), buf.size(), &n);
  if (rc == 0 && n > 0) {
    std::fwrite(buf.data(), 1, n - 1, out);
    std::fputc('\n', out);
    return;
  }
  std::fwrite(data, 1, len, out);
  std::fputc('\n', out);
}

// render_diag_json 把前端给的诊断 JSON 数组渲染成人读文本（片段 + 文案）。
//
// 为什么要渲染而不是打 JSON：用户看到的是 `error[CODE]: … --> 文件 : 行` 加
// 源码片段；把 JSON 甩到 stderr 等于没做诊断。守卫
// `TestCompilerDiagnosticsAreEnglishByDefault` 就是照这个判的。
void render_diag_json(const std::string& src, const char* data, size_t len, FILE* out) {
  std::vector<char> buf(1 << 18);
  size_t n = 0;
  const int rc = hm_diag_render_json_src(src.data(), src.size(), data, len, current_lang(),
                                         buf.data(), buf.size(), &n);
  if (rc == 0 && n > 0) {
    std::fwrite(buf.data(), 1, n - 1, out);
    std::fputc('\n', out);
    return;
  }
  std::fwrite(data, 1, len, out);  // 渲染失败也不丢信息
  std::fputc('\n', out);
}

// collect_warnings 把诊断数组里的 warning 按 (code,line,col) 去重收集。
void collect_warnings(const char* data, size_t len, std::map<std::string, std::string>* seen) {
  hm::JVal arr;
  size_t pos = 0;
  if (len == 0 || !hm::parse_json(data, len, &arr, &pos) || arr.kind() != hm::JVal::kArr) return;
  for (size_t i = 0; i < arr.size(); ++i) {
    const hm::JVal* d = arr.at(i);
    if (d == nullptr) continue;
    const hm::JVal* sv = d->find("severity");
    if (sv == nullptr || sv->kind() != hm::JVal::kStr || sv->as_str() != "warning") continue;
    hm::JVal one = hm::JVal::array();
    one.push(*d);
    // 去重键：码 + 位置 + 文案 + **模块基名**。
    //
    // 为什么不直接用整条 JSON：同一个模块会被装配两趟（入口一趟、作为被包含方
    // 一趟），两趟记下的路径拼写不同（绝对 vs 相对），于是整条 JSON 不等、
    // 同一条警告就会印两遍。基名相同即同一文件，这才是"同一件事"的判据。
    // （Go 侧同样按位置+文案去重，只是它的 module 拼写恰好一致。）
    auto pick = [&](const char* k) -> std::string {
      const hm::JVal* v = d->find(k);
      if (v == nullptr) return "";
      if (v->kind() == hm::JVal::kStr) return v->as_str();
      if (v->kind() == hm::JVal::kInt) return std::to_string(v->as_int(0));
      return "";
    };
    std::string module = pick("module");
    const size_t slash = module.find_last_of('/');
    if (slash != std::string::npos) module = module.substr(slash + 1);
    const std::string key = pick("severity") + "\x00" + pick("code") + "\x00" + module + "\x00" +
                            pick("line") + ":" + pick("col") + "-" + pick("end_line") + ":" +
                            pick("end_col") + "\x00" + pick("message");
    (*seen)[key] = one.str();
  }
}

// render_warning_entries 渲染一组（单条）警告。
void render_warning_entries(const std::string& one_json, const std::string& src);

// render_warning_entries 渲染一组（单条）警告。
void render_warning_entries(const std::string& one_json, const std::string& src) {
  render_diag_json_mode(src, one_json.data(), one_json.size(), RenderMode::kAll);
}

// assemble_unit：源码 → 最终 IR JSON。`build` 与 `ir` 共用这一段。
// 返回 0 成功（*out_json 是 IR）；非 0 是退出码（诊断已打印）。
int assemble_unit(const std::string& in_path, const std::vector<std::string>& inc_dirs,
                  const std::vector<std::string>& lib_dirs, bool strict_link, RenderMode mode,
                  std::string* out_json, bool* warned, std::string* preprocess_dump = nullptr) {
  std::string src;
  if (!read_file(in_path.c_str(), &src)) {
    std::fprintf(stderr, "hermes ir: cannot read %s\n", in_path.c_str());
    return 4;
  }

  const std::string project_root = dir_of(in_path);
  const size_t slash = in_path.find_last_of('/');
  const std::string entry_mod =
      (slash == std::string::npos) ? in_path : in_path.substr(slash + 1);

  // 警告按 (code,line,col) 去重：每个阶段返回的诊断数组是**同一份**，
  // 不去重会把同一条警告印四遍。
  std::map<std::string, std::string> warn_seen;
  std::string mods_json, requires_json;
  std::vector<std::string> seen_mod;
  // 每个模块的 (path, source, is_header)：构造 ext_types 与跑语义检查都要用。
  std::vector<std::tuple<std::string, std::string, bool>> mod_sources;
  auto add_module = [&](const std::string& path, bool builtin, const std::string& kind,
                        const std::string& body) -> bool {
    if (std::find(seen_mod.begin(), seen_mod.end(), path) != seen_mod.end()) return true;
    std::string sum;
    if (!sha256_hex(body, &sum)) return false;
    seen_mod.push_back(path);
    mod_sources.emplace_back(path, body, kind == "h");
    if (!mods_json.empty()) mods_json += ",";
    mods_json += "{\"builtin\":" + std::string(builtin ? "true" : "false") + ",\"kind\":\"" + kind +
                 "\",\"path\":\"" + json_escape(path) + "\",\"sha256\":\"" + sum +
                 "\",\"source\":\"" + json_escape(body) + "\"}";
    return true;
  };
  if (!add_module(entry_mod, false, ends_with(in_path, ".h") ? "h" : "hermes", src)) {
    std::fprintf(stderr, "hermes ir: hashing failed\n");
    return 4;
  }
  auto add_require = [&](const std::string& obj) {
    if (!requires_json.empty()) requires_json += ",";
    requires_json += obj;
  };

  // 递归处理每个文件的指令。**必须递归**：被 include 的头文件自己也可能
  // include/link（starter 例子里 app.h 就 #include 了两个 std 头）——
  // 只扫入口会漏掉它们，表现为 modules[] 少几项、requires[] 却正常。
  std::set<std::string> processed{entry_mod};
  std::vector<std::string> stack{entry_mod};
  int status = 0;
  std::function<bool(const std::string&, bool, const std::string&)> process;
  process = [&](const std::string& path, bool is_header, const std::string& text) -> bool {
    std::vector<Dirent> ds;
    std::string mod_diags;
    if (!scan_directives(text, path, is_header, &ds, &mod_diags)) return false;
    // 解析期诊断必须并入（K-1）：`#bind` / `#link` 写在 .h 里是硬错。
    // 入口模块除外——它的诊断由后面的装配阶段统一报。
    if (!mod_diags.empty() && mod_diags != "[]" && path != entry_mod) {
      // **只取指令级诊断**（E3017 / E3027 / E3028）。
      //
      // 为什么不能整段并入：`.h` 是**单独**解析的，它看不到别的模块声明的类型，
      // 于是会产出 "cannot access field on unknown" 这类**假错误**——那是
      // "缺 externals" 的后果，不是源码的问题。整段并入会让每个正常的多文件
      // 工程都红（support_bot 就是第一个）。
      //
      // 而指令级诊断与类型无关：`#bind` / `#link` 写在 `.h` 里就是错，
      // 这句话在单文件里也成立。
      std::string filtered = filter_directive_diags(mod_diags);
      if (!filtered.empty() && filtered != "[]") {
        collect_warnings(filtered.data(), filtered.size(), &warn_seen);
        if (filtered.find("\"severity\":\"error\"") != std::string::npos) {
          render_diag_json_mode(text, filtered.data(), filtered.size(), mode);
          status = 4;
          return false;
        }
      }
    }
    for (const Dirent& d : ds) {
      if (d.kind == "include") {
        std::string spec = d.spec;
        if (spec.size() >= 2 && ((spec.front() == '"' && spec.back() == '"') ||
                                 (spec.front() == '<' && spec.back() == '>'))) {
          spec = spec.substr(1, spec.size() - 2);
        }
        if (!d.angle) {
          if (!ends_with(spec, ".h")) {
            std::fprintf(stderr, "hermes ir: %s: #include only takes .h files (got \"%s\")\n",
                         path.c_str(), spec.c_str());
            status = 4;
            return false;
          }
          std::vector<std::string> candidates;
          std::vector<std::string> dirs2{dir_of(path)};
          for (const std::string& d2 : inc_dirs) dirs2.push_back(d2);
          dirs2.push_back(project_root);
          std::string found;
          for (const std::string& d2 : dirs2) {
            const std::string cand = join_path(d2, spec);
            candidates.push_back(cand);
            if (file_exists(cand)) {
              found = cand;
              break;
            }
          }
          if (found.empty()) {
            std::string tried;
            for (size_t k = 0; k < candidates.size(); ++k) {
              if (k) tried += ", ";
              tried += candidates[k];
            }
            std::fprintf(stderr, "hermes ir: %s: cannot find %s; tried: %s\n", path.c_str(),
                         spec.c_str(), tried.c_str());
            status = 4;
            return false;
          }
          const std::string rel = rel_path(project_root, found);
          if (std::find(stack.begin(), stack.end(), rel) != stack.end()) {
            std::fprintf(stderr, "hermes ir: %s: include cycle through %s\n", path.c_str(),
                         rel.c_str());
            status = 4;
            return false;
          }
          if (processed.count(rel) != 0) continue;  // include-once
          std::string body2;
          if (!read_file(found.c_str(), &body2)) {
            std::fprintf(stderr, "hermes ir: cannot read %s\n", found.c_str());
            status = 4;
            return false;
          }
          if (!add_module(rel, false, "h", body2)) {
            status = 4;
            return false;
          }
          processed.insert(rel);
          stack.push_back(rel);
          if (!process(found, true, body2)) return false;
          stack.pop_back();
          continue;
        }
        if (spec.rfind("std/", 0) != 0 || !ends_with(spec, ".h")) {
          std::fprintf(stderr, "hermes ir: %s: <std/...> is the only builtin include form\n",
                       path.c_str());
          status = 4;
          return false;
        }
        const std::string mod = spec.substr(4, spec.size() - 4 - 2);
        std::vector<char> buf(1 << 16);
        size_t n = 0;
        if (hm_std_source(mod.c_str(), buf.data(), buf.size(), &n) != 0) {
          std::fprintf(stderr, "hermes ir: %s: unknown builtin module std/%s\n", path.c_str(),
                       mod.c_str());
          status = 4;
          return false;
        }
        if (!add_module("std/" + mod + ".h", true, "h", std::string(buf.data(), n - 1))) {
          status = 4;
          return false;
        }
        continue;
      }
      std::string kind, module, archive, header;
      classify(d.spec, &kind, &module, &archive, &header);
      if (kind == "std") {
        const std::string mod = module.rfind("std/", 0) == 0 ? module.substr(4) : module;
        std::vector<char> buf(1 << 16);
        size_t n = 0;
        if (hm_std_source(mod.c_str(), buf.data(), buf.size(), &n) != 0) {
          std::fprintf(stderr, "hermes ir: %s: unknown builtin module std/%s\n", path.c_str(),
                       mod.c_str());
          status = 4;
          return false;
        }
        std::string sum;
        if (!sha256_hex(std::string(buf.data(), n - 1), &sum)) {
          status = 4;
          return false;
        }
        add_require("{\"abi_version\":2,\"ir_version\":2,\"kind\":\"std\",\"member_sha256\":\"" +
                    sum + "\",\"origin\":\"source\",\"spec\":\"" + json_escape(d.spec) + "\"}");
        continue;
      }
      if (kind == "native") {
        add_require("{\"abi_version\":2,\"archive\":\"" + json_escape(archive) + "\",\"header\":\"" +
                    json_escape(header) + "\",\"ir_version\":2,\"kind\":\"native\",\"name\":\"" +
                    json_escape(archive) + "\",\"origin\":\"source\",\"spec\":\"" +
                    json_escape(d.spec) + "\"}");
        continue;
      }
      std::string found;
      for (const std::string& d2 : lib_dirs) {
        for (const std::string& cand :
             {join_path(d2, "lib" + module + ".hbc"), join_path(d2, module + ".hbc")}) {
          if (file_exists(cand)) {
            found = cand;
            break;
          }
        }
        if (!found.empty()) break;
      }
      if (found.empty()) {
        std::fprintf(stderr,
                     "hermes ir: warning: local dependency \"%s\" could not be resolved "
                     "(E_LINK_NOT_FOUND): omitted from requires[], the artifact will be incomplete "
                     "on the distribution chain; use -L <dir>\n",
                     module.c_str());
        if (strict_link) {
          std::fprintf(stderr,
                       "  to build without implementation bytes anyway, add --no-strict-link\n");
          status = 4;
          return false;
        }
        continue;
      }
      std::string archive_bytes;
      if (!read_file(found.c_str(), &archive_bytes)) {
        status = 4;
        return false;
      }
      char hex[65] = {0};
      const int hrc = hm_archive_index_sha256(
          reinterpret_cast<const unsigned char*>(archive_bytes.data()), archive_bytes.size(), hex);
      if (hrc != 0) {
        std::fprintf(stderr, "hermes ir: %s is not a readable .hbc archive (status %d)\n",
                     found.c_str(), hrc);
        status = 4;
        return false;
      }
      add_require("{\"abi_version\":2,\"ir_version\":2,\"kind\":\"local\",\"member_sha256\":\"" +
                  std::string(hex) + "\",\"origin\":\"source\",\"spec\":\"" + json_escape(d.spec) +
                  "\"}");
    }
    return true;
  };
  if (!process(in_path, ends_with(in_path, ".h"), src)) return status == 0 ? 4 : status;

  // `-E`：只要展开结果（模块按被访问顺序，即拓扑序），不装配、不校验。
  // 放在这一步而不是另做一遍遍历：行标记的顺序必须与编译器看到的顺序一致，
  // 而"编译器看到的顺序"就是这里的 walk 顺序。
  if (preprocess_dump != nullptr) {
    std::string out;
    for (const auto& m : mod_sources) {
      out += "# " + std::get<0>(m) + "\n";
      out += std::get<1>(m);
      if (out.empty() || out.back() != '\n') out += "\n";
    }
    *preprocess_dump = out;
    return 0;
  }

  {
    std::string ext;
    for (const auto& m : mod_sources) {
      std::vector<char> tb(1 << 18), td(1 << 12);
      size_t tn = 0, tdn = 0;
      if (hm_codegen_types(reinterpret_cast<const unsigned char*>(std::get<1>(m).data()),
                           std::get<1>(m).size(), std::get<0>(m).c_str(),
                           std::get<2>(m) ? 1 : 0, tb.data(), tb.size(), &tn, td.data(),
                           td.size(), &tdn) != 0) continue;
      // 每个模块的类型表诊断**不能丢**：Go 对非入口模块的 tdiag 是硬失败的
      // （E3026 之类就出在这里——.h 里的非 extern 签名必须被拒）。
      if (tdn > 0 && td[tdn - 1] == 0) --tdn;
      collect_warnings(td.data(), tdn, &warn_seen);
      if (std::get<0>(m) != entry_mod &&
          std::string(td.data(), tdn).find("\"severity\":\"error\"") != std::string::npos) {
        render_diag_json_mode(std::get<1>(m), td.data(), tdn, mode);
        return 4;
      }
      if (tn > 0 && tb[tn - 1] == 0) --tn;
      hm::JVal types;
      size_t pos = 0;
      if (!hm::parse_json(tb.data(), tn, &types, &pos) || types.kind() != hm::JVal::kArr) continue;
      for (size_t i = 0; i < types.size(); ++i) {
        const hm::JVal* t = types.at(i);
        if (t == nullptr) continue;
        const hm::JVal* nm = t->find("name");
        const hm::JVal* flds = t->find("fields");
        if (nm == nullptr || nm->kind() != hm::JVal::kStr || flds == nullptr ||
            flds->kind() != hm::JVal::kArr) continue;
        for (size_t j = 0; j < flds->size(); ++j) {
          const hm::JVal* f = flds->at(j);
          if (f == nullptr) continue;
          const hm::JVal* fn = f->find("n");
          const hm::JVal* ft = f->find("t");
          if (fn == nullptr || ft == nullptr || fn->kind() != hm::JVal::kStr ||
              ft->kind() != hm::JVal::kObj) continue;
          const hm::JVal* fk = ft->find("k");
          std::string tname;
          if (fk != nullptr && fk->kind() == hm::JVal::kStr) {
            tname = fk->as_str();
            if (tname == "struct") {
              const hm::JVal* ref = ft->find("ref");
              if (ref != nullptr && ref->kind() == hm::JVal::kStr) tname = ref->as_str();
            }
          }
          ext += nm->as_str() + "\t" + fn->as_str() + "\t" + tname + "\n";
        }
      }
    }
    std::map<std::string, std::string> by_sym;
    for (const auto& m : mod_sources) {
      std::vector<char> b(1 << 18), d(1 << 12);
      size_t bn = 0, dn = 0;
      if (hm_codegen_wfdecls(reinterpret_cast<const unsigned char*>(std::get<1>(m).data()),
                             std::get<1>(m).size(), std::get<0>(m).c_str(),
                             std::get<2>(m) ? 1 : 0, b.data(), b.size(), &bn, d.data(), d.size(),
                             &dn) != 0) continue;
      if (dn > 0 && d[dn - 1] == 0) --dn;
      collect_warnings(d.data(), dn, &warn_seen);
      if (std::get<0>(m) != entry_mod &&
          std::string(d.data(), dn).find("\"severity\":\"error\"") != std::string::npos) {
        render_diag_json_mode(std::get<1>(m), d.data(), dn, mode);
        return 4;
      }
      // backends / tools / functions 三趟：诊断顺序与 Go 一致（types → wfdecls →
      // backends → tools → functions），否则"第一条 error"会落在不同的诊断上。
      using StageFn = int (*)(const unsigned char*, size_t, const char*, int, char*, size_t,
                              size_t*, char*, size_t, size_t*);
      auto stage = [&](StageFn fn) -> bool {
        std::vector<char> ob(1 << 18), od(1 << 12);
        size_t on = 0, odn = 0;
        if (fn(reinterpret_cast<const unsigned char*>(std::get<1>(m).data()),
               std::get<1>(m).size(), std::get<0>(m).c_str(), std::get<2>(m) ? 1 : 0, ob.data(),
               ob.size(), &on, od.data(), od.size(), &odn) != 0) {
          return true;
        }
        if (odn > 0 && od[odn - 1] == 0) --odn;
        if (std::get<0>(m) != entry_mod &&
            std::string(od.data(), odn).find("\"severity\":\"error\"") != std::string::npos) {
          render_diag_json_mode(std::get<1>(m), od.data(), odn, mode);
          return false;
        }
        return true;
      };
      if (!stage(hm_codegen_backends)) return 4;
      if (!stage(hm_codegen_tools)) return 4;
      // functions 这一趟 Go 对**所有**模块（含入口）都检查诊断：实测 E4021
      // 就出在这里，而入口 codegen_ext 只给 E5007。两趟各报一条 → 默认口径下
      // 用户看到两条（每趟各自"首错即停"，不是全局只留一条）。
      {
        std::vector<char> ob(1 << 18), od(1 << 12);
        size_t on = 0, odn = 0;
        if (hm_codegen_functions(reinterpret_cast<const unsigned char*>(std::get<1>(m).data()),
                                 std::get<1>(m).size(), std::get<0>(m).c_str(),
                                 std::get<2>(m) ? 1 : 0, nullptr, 0, ob.data(), ob.size(), &on,
                                 od.data(), od.size(), &odn) == 0) {
          if (odn > 0 && od[odn - 1] == 0) --odn;
          if (std::get<0>(m) != entry_mod &&
              std::string(od.data(), odn).find("\"severity\":\"error\"") != std::string::npos) {
            render_diag_json_mode(std::get<1>(m), od.data(), odn, mode);
            return 4;
          }
        }
      }
      if (bn > 0 && b[bn - 1] == 0) --bn;
      hm::JVal arr;
      size_t pos = 0;
      if (!hm::parse_json(b.data(), bn, &arr, &pos) || arr.kind() != hm::JVal::kArr) continue;
      for (size_t i = 0; i < arr.size(); ++i) {
        const hm::JVal* it = arr.at(i);
        if (it == nullptr) continue;
        const hm::JVal* ex = it->find("extern");
        if (ex == nullptr || ex->kind() != hm::JVal::kBool || !ex->as_bool(false)) continue;
        const hm::JVal* sym = it->find("sym");
        const hm::JVal* ln = it->find("ext_line");
        if (sym == nullptr || ln == nullptr || sym->kind() != hm::JVal::kStr ||
            ln->kind() != hm::JVal::kStr) continue;
        by_sym[sym->as_str()] = ln->as_str();
      }
    }
    for (const auto& kv : by_sym) {
      ext += kv.second;
      ext += '\n';
    }
    std::vector<char> cg(1 << 20), cgd(1 << 16);
    size_t cglen = 0, cgdlen = 0;
    (void)hm_codegen_ext(reinterpret_cast<const unsigned char*>(src.data()), src.size(),
                         in_path.c_str(), ends_with(in_path, ".h") ? 1 : 0,
                         ext.empty() ? nullptr : ext.data(), ext.size(), cg.data(), cg.size(),
                         &cglen, cgd.data(), cgd.size(), &cgdlen);
    if (cgdlen > 0 && cgd[cgdlen - 1] == 0) --cgdlen;
    collect_warnings(cgd.data(), cgdlen, &warn_seen);
    if (std::string(cgd.data(), cgdlen).find("\"severity\":\"error\"") != std::string::npos) {
      render_diag_json_mode(src, cgd.data(), cgdlen, mode);
      return 4;
    }
  }

  // 警告：渲染到 stderr，不阻塞（--deny-warnings 由调用方决定成败）。
  for (const auto& kv : warn_seen) {
    render_warning_entries(kv.second, src);
    if (warned != nullptr) *warned = true;
  }

  const std::string facts = "{\"modules\":[" + mods_json + "],\"requires\":[" + requires_json + "]}";
  std::vector<char> out(1 << 20), err(1 << 16);
  size_t olen = 0, elen = 0;
  const int rc = hm_ir_assemble_final(reinterpret_cast<const uint8_t*>(src.data()), src.size(),
                                      in_path.c_str(), facts.data(), facts.size(), out.data(),
                                      out.size(), &olen, err.data(), err.size(), &elen);
  if (rc != 0) {
    // 装配层的错误既可能是文本也可能是诊断 JSON：后者必须渲染成人读形态，
    // 否则用户看到的就是一坨 JSON。
    if (elen > 0 && err[0] == '[') {
      render_diag_json(src, err.data(), elen, stderr);
    } else {
      std::fprintf(stderr, "hermes ir: %.*s\n", static_cast<int>(elen), err.data());
    }
    return 4;
  }
  *out_json = std::string(out.data(), olen);
  return 0;
}

int cmd_ir(int argc, char** argv) {
  std::string in_path, out_path;
  std::vector<std::string> inc_dirs, lib_dirs;
  bool strict_link = true;
  for (int i = 2; i < argc; ++i) {
    const std::string a = argv[i];
    if (a == "-I" || a == "-L") {
      if (i + 1 >= argc) {
        std::fprintf(stderr, "hermes ir: %s needs a directory\n", a.c_str());
        return 2;
      }
      (a == "-I" ? inc_dirs : lib_dirs).push_back(argv[++i]);
    } else if (a == "--no-strict-link") {
      strict_link = false;
    } else if (a == "-o") {
      if (i + 1 >= argc) {
        std::fprintf(stderr, "hermes ir: -o needs a path\n");
        return 2;
      }
      out_path = argv[++i];
    } else if (a == "-h" || a == "--help" || a == "help") {
      std::printf("%s\n", tr("hermes-native ir <file> [-o out.json] [-I dir] [-L dir] [--no-strict-link]"));
      return 0;
    } else if (!a.empty() && a[0] == '-') {
      std::fprintf(stderr, "hermes ir: unknown flag %s\n", a.c_str());
      return 2;
    } else {
      in_path = a;
    }
  }
  if (in_path.empty()) {
    std::fprintf(stderr, "hermes ir: missing source file\n");
    return 2;
  }
  std::string ir_json;
  bool warned = false;
  const int rc =
      assemble_unit(in_path, inc_dirs, lib_dirs, strict_link, RenderMode::kAll, &ir_json, &warned);
  if (rc != 0) return rc;
  if (out_path.empty()) {
    std::fwrite(ir_json.data(), 1, ir_json.size(), stdout);
    return 0;
  }
  FILE* f = std::fopen(out_path.c_str(), "wb");
  if (f == nullptr) {
    std::fprintf(stderr, "hermes ir: cannot write %s\n", out_path.c_str());
    return 4;
  }
  std::fwrite(ir_json.data(), 1, ir_json.size(), f);
  std::fclose(f);
  return 0;
}

}  // namespace


// ---- build：源码 → .hbc 归档 -----------------------------------------------
// 与 `ir` 共用 assemble_unit（那是已验证逐字节等价的那一段），这里只多做
// "IR → 归档字节 → 落盘"。签名（`-k`）需要 Ed25519，仍在 Go：明确拒绝而不是
// 悄悄产出未签名的产物。
int cmd_build(int argc, char** argv) {
  std::string in_path, out_path;
  std::vector<std::string> inc_dirs, lib_dirs;
  bool strict_link = true, denied = false, keep_going = false;
  for (int i = 2; i < argc; ++i) {
    const std::string a = argv[i];
    if (a == "-o" || a == "--out") {
      if (i + 1 >= argc) {
        std::fprintf(stderr, "hermes build: -o needs a file\n");
        return 2;
      }
      out_path = argv[++i];
    } else if (a == "-L" || a == "-I") {
      if (i + 1 >= argc) {
        std::fprintf(stderr, "hermes build: %s needs a directory\n", a.c_str());
        return 2;
      }
      (a == "-L" ? lib_dirs : inc_dirs).push_back(argv[++i]);
    } else if (a == "--no-strict-link") {
      strict_link = false;
    } else if (a == "--strict-link") {
      strict_link = true;
    } else if (a == "--deny-warnings") {
      denied = true;
    } else if (a == "--keep-going") {
      keep_going = true;
    } else if (a == "-k" || a == "--key") {
      std::fprintf(stderr,
                   "hermes build: %s needs signing, which is not ported to the native path yet "
                   "(Ed25519 lives in Go)\n",
                   a.c_str());
      return 2;
    } else if (a == "-h" || a == "--help" || a == "help") {
      std::printf("%s\n", tr("hermes-native build <file> [-o out.hbc] [-I dir] [-L dir] [--no-strict-link] [--deny-warnings] [--keep-going]"));
      return 0;
    } else if (!a.empty() && a[0] == '-') {
      std::fprintf(stderr, "hermes build: unknown flag %s\n", a.c_str());
      return 2;
    } else {
      in_path = a;
    }
  }
  if (in_path.empty()) {
    std::fprintf(stderr, "hermes build: missing source file\n");
    return 2;
  }
  if (out_path.empty()) {
    const size_t dot = in_path.find_last_of('.');
    const size_t slash = in_path.find_last_of('/');
    std::string base = (slash == std::string::npos) ? in_path : in_path.substr(slash + 1);
    (void)dot;
    const size_t d2 = base.find_last_of('.');
    if (d2 != std::string::npos) base = base.substr(0, d2);
    out_path = base + ".hbc";
  }

  std::string ir_json;
  bool warned = false;
  const int rc = assemble_unit(in_path, inc_dirs, lib_dirs, strict_link,
                               keep_going ? RenderMode::kKeepGoing : RenderMode::kFirstError,
                               &ir_json, &warned);
  if (rc != 0) return rc;
  // --deny-warnings：警告已在上面渲染过，这里只决定成败。
  // 放在写归档**之前** —— 失败的构建不该留下产物。
  if (denied && warned) {
    std::fprintf(stderr, "hermes build: warnings treated as errors (--deny-warnings); no archive written\n");
    return 4;
  }

  // 从 IR 里取归档需要的成员事实（与 Go 的 archive.Write 输入同源）。
  hm::JVal doc;
  size_t pos = 0;
  if (!hm::parse_json(ir_json.data(), ir_json.size(), &doc, &pos) || doc.kind() != hm::JVal::kObj) {
    std::fprintf(stderr, "hermes build: assembled IR is not an object\n");
    return 4;
  }
  const hm::JVal* unit = doc.find("unit");
  const hm::JVal* exports = doc.find("exports");
  const hm::JVal* requires = doc.find("requires");
  const hm::JVal* workflows = doc.find("workflows");
  std::string name;
  long long language_version = 0;
  if (unit != nullptr) {
    const hm::JVal* n = unit->find("name");
    if (n != nullptr && n->kind() == hm::JVal::kStr) name = n->as_str();
    const hm::JVal* lv = unit->find("language_version");
    if (lv != nullptr) language_version = lv->as_int(0);
  }
  bool is_entry = false;
  if (workflows != nullptr && workflows->kind() == hm::JVal::kArr) {
    for (size_t i = 0; i < workflows->size(); ++i) {
      const hm::JVal* w = workflows->at(i);
      if (w == nullptr) continue;
      const hm::JVal* e = w->find("is_entry");
      if (e != nullptr && e->kind() == hm::JVal::kBool && e->as_bool(false)) is_entry = true;
    }
  }

  hm::JVal m = hm::JVal::object();
  m.set("name", hm::JVal::string(name));
  m.set("ir", hm::JVal::string(ir_json));
  m.set("abi_version", hm::JVal::integer(2));
  m.set("language_version", hm::JVal::integer(language_version));
  m.set("entry", hm::JVal::boolean(is_entry));
  hm::JVal ex = hm::JVal::object();
  auto copy_arr = [&](const char* key) {
    hm::JVal a = hm::JVal::array();
    const hm::JVal* src2 = (exports != nullptr) ? exports->find(key) : nullptr;
    if (src2 != nullptr && src2->kind() == hm::JVal::kArr) {
      for (size_t i = 0; i < src2->size(); ++i) {
        const hm::JVal* v = src2->at(i);
        if (v != nullptr && v->kind() == hm::JVal::kStr) a.push(hm::JVal::string(v->as_str()));
      }
    }
    return a;
  };
  ex.set("workflows", copy_arr("workflows"));
  ex.set("types", copy_arr("types"));
  ex.set("backends", copy_arr("backends"));
  ex.set("tools", copy_arr("tools"));
  m.set("exports", std::move(ex));
  m.set("requires", requires != nullptr ? *requires : hm::JVal::array());

  hm::JVal members = hm::JVal::array();
  members.push(std::move(m));
  const std::string members_json = members.str();

  std::vector<unsigned char> out(1 << 20);
  size_t n = 0;
  int wrc = hm_archive_write(members_json.data(), members_json.size(), out.data(), out.size(), &n);
  if (wrc == -4 && n > out.size()) {
    out.resize(n);
    wrc = hm_archive_write(members_json.data(), members_json.size(), out.data(), out.size(), &n);
  }
  if (wrc != 0) {
    std::fprintf(stderr, "hermes build: cannot write archive (status %d)\n", wrc);
    return 4;
  }
  FILE* f = std::fopen(out_path.c_str(), "wb");
  if (f == nullptr) {
    std::fprintf(stderr, "hermes build: cannot write %s\n", out_path.c_str());
    return 4;
  }
  std::fwrite(out.data(), 1, n, f);
  std::fclose(f);
  return 0;
}

extern "C" int hm_archive_read(const unsigned char* data, size_t len, char* out, size_t cap,
                               size_t* out_len);
extern "C" int hm_layout_resolve(const char* exec_path, const char* cwd, char* out_root,
                                 size_t root_cap, char* out_lib, size_t lib_cap, char* out_include,
                                 size_t inc_cap, char* out_shim, size_t shim_cap);
extern "C" int hm_layout_header_for(const char* lib_path, char* out, size_t cap);
extern "C" int hm_header_gen_c(const char* ir_json, size_t len, char* out, size_t cap,
                               size_t* out_len);
extern "C" int hm_header_gen_shim(const char* ir_json, size_t len, const unsigned char* ir_bytes,
                                  size_t ir_len, char* out, size_t cap, size_t* out_len);
extern "C" int hm_header_core_impl(char* out, size_t cap, size_t* out_len);
extern "C" int hm_objfile_dynamic_libs(const char* path, char* out, size_t cap, size_t* out_len);
extern "C" int hm_linkpolicy_match(const char* lib, char* out_name, size_t name_cap,
                                   char* out_licence, size_t lic_cap);

// ---- cc：把用户程序链到某个 unit 上（C 路径） -------------------------------
// 只做 `.c`。C++ 需要两遍编译 + .hpp 生成，另行处理——**明确拒绝**而不是
// 悄悄走错路径（把 C++ 源码用 -std=c89 编会得到一堆语法错，且报错里一个字
// 都不会提"你其实该走 C++ 路径"）。
std::string split_join(const std::string& csv) {
  std::string out;
  for (const std::string& one : split_csv_c(csv)) {
    if (!out.empty()) out += " ";
    out += one;
  }
  return out;
}

std::vector<std::string> split_csv_c(const std::string& s) {
  std::vector<std::string> out;
  std::string cur;
  for (char c : s) {
    if (c == ',' || c == ' ') {
      if (!cur.empty()) out.push_back(cur);
      cur.clear();
    } else {
      cur.push_back(c);
    }
  }
  if (!cur.empty()) out.push_back(cur);
  return out;
}

namespace {

bool ends_with_ci(const std::string& s, const char* suffix) {
  const size_t n = std::strlen(suffix);
  if (s.size() < n) return false;
  for (size_t i = 0; i < n; ++i) {
    char a = s[s.size() - n + i];
    char b = suffix[i];
    if (a >= 'A' && a <= 'Z') a = static_cast<char>(a - 'A' + 'a');
    if (b >= 'A' && b <= 'Z') b = static_cast<char>(b - 'A' + 'a');
    if (a != b) return false;
  }
  return true;
}

// run_argv 直接 exec、不经过 shell：路径里的空格与引号不该成为命令注入面。
int run_argv(const std::vector<std::string>& argv) {
  std::vector<char*> cargv;
  cargv.reserve(argv.size() + 1);
  for (const std::string& a : argv) cargv.push_back(const_cast<char*>(a.c_str()));
  cargv.push_back(nullptr);
  pid_t pid = ::fork();
  if (pid < 0) return -1;
  if (pid == 0) {
    ::execvp(cargv[0], cargv.data());
    ::_exit(127);
  }
  int status = 0;
  if (::waitpid(pid, &status, 0) < 0) return -1;
  if (WIFEXITED(status)) return WEXITSTATUS(status);
  return -1;
}

bool write_text(const std::string& path, const std::string& body) {
  FILE* f = std::fopen(path.c_str(), "wb");
  if (f == nullptr) return false;
  std::fwrite(body.data(), 1, body.size(), f);
  std::fclose(f);
  return true;
}

int cmd_cc(int argc, char** argv) {
  std::string src_path, unit_path, lib_path, out_path = "a.out";
  bool keep = false;
  for (int i = 2; i < argc; ++i) {
    const std::string a = argv[i];
    if (a == "-o" || a == "--out") {
      if (i + 1 >= argc) {
        std::fprintf(stderr, "hermes cc: -o needs a file\n");
        return 2;
      }
      out_path = argv[++i];
    } else if (a == "--unit") {
      if (i + 1 >= argc) {
        std::fprintf(stderr, "hermes cc: --unit needs an .hbc\n");
        return 2;
      }
      unit_path = argv[++i];
    } else if (a == "--runtime-lib") {
      if (i + 1 >= argc) {
        std::fprintf(stderr, "hermes cc: --runtime-lib needs a path\n");
        return 2;
      }
      lib_path = argv[++i];
    } else if (a == "--keep") {
      keep = true;
    } else if (a == "-h" || a == "--help" || a == "help") {
      std::printf("%s\n", tr("hermes-native cc <src.c> --unit <unit.hbc> [-o prog] [--runtime-lib path]"));
      return 0;
    } else if (!a.empty() && a[0] == '-') {
      std::fprintf(stderr, "hermes cc: unknown flag %s\n", a.c_str());
      return 2;
    } else {
      src_path = a;
    }
  }
  if (src_path.empty() || unit_path.empty()) {
    std::fprintf(stderr, "%s\n",
                 tr("hermes cc: need <src.c> --unit <unit.hbc>"));
    return 2;
  }
  // 语言由**扩展名**决定，不猜内容：把 C++ 当 C 编只会得到语法错，
  // 把 C 当 C++ 编会静默改变链接面（名字修饰、异常表）。
  bool is_cpp = false;
  bool is_c = ends_with_ci(src_path, ".c");
  if (!is_c) {
    for (const char* ext : {".cpp", ".cc", ".cxx", ".c++"}) {
      if (ends_with_ci(src_path, ext)) is_cpp = true;
    }
    // `.C` 是大写 C++（几十年历史约定）。ends_with_ci 已经覆盖大小写，
    // 所以这里只需单独判断"扩展名恰为 .C"——它不可能是 C。
    if (!is_cpp && src_path.size() >= 2 && src_path.compare(src_path.size() - 2, 2, ".C") == 0) {
      is_cpp = true;
    }
  }
  if (!is_c && !is_cpp) {
    std::fprintf(stderr,
                 "hermes cc: %s: only C/C++ sources are accepted (.c / .cpp / .cc / .cxx / .c++ / "
                 ".C)\n",
                 src_path.c_str());
    return 2;
  }

  // 运行时库与 include 根：--runtime-lib 优先，否则按布局找。
  char rootbuf[4096], libbuf[4096], incbuf[4096], shimbuf[4096];
  // 可执行文件的位置是**关键线索**：发布包布局里 include/ 在 bin/ 的上一级，
  // 开发布局里在仓库根——而用户从任意目录调用 cc，只有 exe 路径能定位到根。
  // （只做最小实现：取 exe 的上一级交给 layout，软链解析也在 layout 里。）
  const std::string exe_s = host_exe_path();
  char exe[4096] = {0};
  std::snprintf(exe, sizeof exe, "%s", exe_s.c_str());
  char cwd[4096];
  if (::getcwd(cwd, sizeof cwd) == nullptr) cwd[0] = 0;
  hm_layout_resolve(exe, cwd, rootbuf, sizeof rootbuf, libbuf, sizeof libbuf, incbuf, sizeof incbuf,
                    shimbuf, sizeof shimbuf);
  if (lib_path.empty()) lib_path = libbuf;
  if (lib_path.empty()) {
    std::fprintf(stderr, "%s\n",
                 tr("hermes cc: runtime library libhermesrt.a not found; run scripts/build.sh first"));
    return 4;
  }

  std::string unit_bytes;
  if (!read_file(unit_path.c_str(), &unit_bytes)) {
    std::fprintf(stderr, "hermes cc: cannot read %s\n", unit_path.c_str());
    return 4;
  }
  // 归档：取第 0 个成员的 IR 正文；裸 IR：直接用。
  std::string ir_json;
  if (unit_bytes.size() >= 4 && unit_bytes.compare(0, 4, "HBC\x1a") == 0) {
    std::vector<char> members(1 << 16);
    size_t mn = 0;
    int rc = hm_archive_read(reinterpret_cast<const unsigned char*>(unit_bytes.data()),
                             unit_bytes.size(), members.data(), members.size(), &mn);
    if (rc == -4 && mn > members.size()) {
      members.resize(mn);
      rc = hm_archive_read(reinterpret_cast<const unsigned char*>(unit_bytes.data()),
                           unit_bytes.size(), members.data(), members.size(), &mn);
    }
    if (rc != 0) {
      std::fprintf(stderr, "hermes cc: %s is not a usable archive (status %d)\n", unit_path.c_str(),
                   rc);
      return 4;
    }
    hm::JVal arr;
    size_t pos = 0;
    if (!hm::parse_json(members.data(), mn > 0 ? mn - 1 : 0, &arr, &pos) ||
        arr.kind() != hm::JVal::kArr || arr.size() == 0) {
      std::fprintf(stderr, "hermes cc: %s has no members\n", unit_path.c_str());
      return 4;
    }
    const hm::JVal* m0 = arr.at(0);
    const hm::JVal* off = m0->find("ir_off");
    const hm::JVal* len = m0->find("ir_len");
    if (off == nullptr || len == nullptr) {
      std::fprintf(stderr, "hermes cc: %s member 0 is malformed\n", unit_path.c_str());
      return 4;
    }
    const size_t o = static_cast<size_t>(off->as_int(0));
    const size_t k = static_cast<size_t>(len->as_int(0));
    if (o + k > unit_bytes.size()) {
      std::fprintf(stderr, "hermes cc: %s member 0 is out of range\n", unit_path.c_str());
      return 4;
    }
    ir_json.assign(unit_bytes.data() + o, k);
  } else {
    ir_json = unit_bytes;
  }

  // 生成三份 C 源：unit 头、unit shim、运行时核心实现。
  std::vector<char> hdr(1 << 18), shim(1 << 20), core(1 << 20);
  size_t hdr_n = 0, shim_n = 0, core_n = 0;
  if (hm_header_gen_c(ir_json.data(), ir_json.size(), hdr.data(), hdr.size(), &hdr_n) != 0) {
    std::fprintf(stderr, "hermes cc: cannot generate the unit header\n");
    return 4;
  }
  std::vector<char> hpp(1 << 18);
  size_t hpp_n = 0;
  if (is_cpp) {
    if (hm_header_gen_cpp(ir_json.data(), ir_json.size(), hpp.data(), hpp.size(), &hpp_n) != 0) {
      std::fprintf(stderr, "hermes cc: cannot generate the C++ header\n");
      return 4;
    }
  }
  if (hm_header_gen_shim(ir_json.data(), ir_json.size(),
                         reinterpret_cast<const unsigned char*>(ir_json.data()), ir_json.size(),
                         shim.data(), shim.size(), &shim_n) != 0) {
    std::fprintf(stderr, "hermes cc: cannot generate the unit shim\n");
    return 4;
  }
  if (hm_header_core_impl(core.data(), core.size(), &core_n) != 0) {
    std::fprintf(stderr, "hermes cc: cannot read the runtime core implementation\n");
    return 4;
  }

  // 运行时库的 C 头叫 <archive>.h；shim 里引用固定名 hermes_rt.h，复制一份。
  char rt_hdr[4096];
  if (hm_layout_header_for(lib_path.c_str(), rt_hdr, sizeof rt_hdr) != 1) {
    std::fprintf(stderr, "hermes cc: no C header next to %s\n", lib_path.c_str());
    return 4;
  }
  std::string rt_body;
  if (!read_file(rt_hdr, &rt_body)) {
    std::fprintf(stderr, "hermes cc: cannot read %s\n", rt_hdr);
    return 4;
  }

  char tmpl[] = "/tmp/hermes-cc-XXXXXX";
  const char* dir = ::mkdtemp(tmpl);
  if (dir == nullptr) {
    std::fprintf(stderr, "hermes cc: cannot create a work dir\n");
    return 4;
  }
  const std::string d(dir);
  // 头文件名跟 unit 走（用户会 `#include "length.h"`）：取 .hbc 的基名去掉扩展名。
  std::string unit_base = unit_path;
  const size_t us = unit_base.find_last_of('/');
  if (us != std::string::npos) unit_base = unit_base.substr(us + 1);
  const size_t udot = unit_base.find_last_of('.');
  if (udot != std::string::npos) unit_base = unit_base.substr(0, udot);
  const std::string hdr_path = d + "/" + unit_base + ".h";
  const std::string hpp_path = d + "/" + unit_base + ".hpp";
  const std::string shim_path = d + "/unit_shim.c";
  const std::string core_path = d + "/hermes_core_impl.c";
  const std::string rt_path = d + "/hermes_rt.h";
  const std::string rt_dir = dir_of(rt_hdr);
  if (is_cpp && !write_text(hpp_path, std::string(hpp.data(), hpp_n - 1))) {
    std::fprintf(stderr, "hermes cc: cannot write the work files\n");
    return 4;
  }
  if (!write_text(hdr_path, std::string(hdr.data(), hdr_n - 1)) ||
      !write_text(shim_path, std::string(shim.data(), shim_n - 1)) ||
      !write_text(core_path, std::string(core.data(), core_n - 1)) ||
      !write_text(rt_path, rt_body)) {
    std::fprintf(stderr, "hermes cc: cannot write the work files\n");
    return 4;
  }
  if (keep) std::fprintf(stderr, "hermes cc: keeping work dir %s\n", d.c_str());

  const char* clang = ::getenv("CC");
  if (clang == nullptr || *clang == 0) clang = "clang";
  std::vector<std::string> common{"-I", d, "-I", rt_dir};
  if (incbuf[0] != 0) {
    common.push_back("-I");
    common.push_back(incbuf);
  }

  int rc = 0;
  if (!is_cpp) {
    // C：单条 invoke（clang 自己做完语义与链接）。
    std::vector<std::string> cmd{clang, "-std=c89", "-O2", "-ffp-contract=off"};
    cmd.insert(cmd.end(), common.begin(), common.end());
    for (const std::string& extra :
         std::vector<std::string>{src_path, shim_path, core_path, lib_path, "-o", out_path}) {
      cmd.push_back(extra);
    }
#if defined(__APPLE__)
    cmd.push_back("-framework");
    cmd.push_back("CoreFoundation");
    cmd.push_back("-framework");
    cmd.push_back("Security");
#endif
    rc = run_argv(cmd);
  } else {
    // C++：第一遍把 shim 与 core impl **当 C** 编成 .o。
    // 它们必须是 C：头里承诺的是未修饰的 hermes_<unit>_<wf>，用 C++ 编会得到
    // _Z… 修饰名，头里声明的名字当场链不上。
    const std::string shim_obj = d + "/unit_shim.o";
    const std::string core_obj = d + "/hermes_core_impl.o";
    for (const auto& pair : std::vector<std::pair<std::string, std::string>>{
             {shim_path, shim_obj}, {core_path, core_obj}}) {
      std::vector<std::string> cmd{clang, "-std=c89", "-O2", "-ffp-contract=off", "-c"};
      cmd.insert(cmd.end(), common.begin(), common.end());
      cmd.push_back(pair.first);
      cmd.push_back("-o");
      cmd.push_back(pair.second);
      rc = run_argv(cmd);
      if (rc != 0) break;
    }
    if (rc == 0) {
      // 第二遍：C++ 驱动。`-x c++` 只作用于**紧随其后的输入**，所以 src 之后
      // 立刻 `-x none` 收回——否则 .o 与 .a 会被当成 C++ 源码再编一遍。
      std::vector<std::string> cmd{clang, "--driver-mode=g++", "-std=c++17", "-stdlib=libc++",
                                   "-O2", "-ffp-contract=off"};
      cmd.insert(cmd.end(), common.begin(), common.end());
      cmd.push_back("-x");
      cmd.push_back("c++");
      cmd.push_back(src_path);
      cmd.push_back("-x");
      cmd.push_back("none");
      for (const std::string& extra :
           std::vector<std::string>{shim_obj, core_obj, lib_path, "-o", out_path}) {
        cmd.push_back(extra);
      }
#if defined(__APPLE__)
      cmd.push_back("-framework");
      cmd.push_back("CoreFoundation");
      cmd.push_back("-framework");
      cmd.push_back("Security");
#endif
      rc = run_argv(cmd);
    }
  }
  if (rc != 0) {
    std::fprintf(stderr, "hermes cc: compilation failed (clang exit %d)\n", rc);
    if (!keep) std::system((std::string("rm -rf ") + d).c_str());
    return 4;
  }

  if (!keep) std::system((std::string("rm -rf ") + d).c_str());
  std::fprintf(stderr, "hermes cc: produced %s\n", out_path.c_str());
  return 0;
}

}  // namespace

// ---- header：从 IR 生成公开的 C/C++ ABI 头 ---------------------------------
namespace {

int cmd_header(int argc, char** argv) {
  std::string in_path, out_path, lang = "c";
  for (int i = 2; i < argc; ++i) {
    const std::string a = argv[i];
    if (a == "--c") {
      lang = "c";
    } else if (a == "--cpp") {
      lang = "cpp";
    } else if (a == "-o" || a == "--out") {
      if (i + 1 >= argc) {
        std::fprintf(stderr, "%s\n", tr("hermes header: -o needs a file"));
        return 2;
      }
      out_path = argv[++i];
    } else if (a == "-h" || a == "--help" || a == "help") {
      std::printf("%s\n",
                  tr("hermes-native header <unit.ir.json|unit.hbc> [--c|--cpp] [-o out]"));
      return 0;
    } else if (!a.empty() && a[0] == '-') {
      std::fprintf(stderr, "hermes header: unknown flag %s\n", a.c_str());
      return 2;
    } else {
      in_path = a;
    }
  }
  if (in_path.empty()) {
    std::fprintf(stderr, "%s\n", tr("hermes header: missing IR / .hbc"));
    return 2;
  }

  std::string data;
  if (!read_file(in_path.c_str(), &data)) {
    std::fprintf(stderr, "hermes header: cannot read %s\n", in_path.c_str());
    return 4;
  }
  // 归档：取第 0 个成员；裸 IR：直接用。
  if (data.size() >= 4 && data.compare(0, 4, "HBC\x1a") == 0) {
    std::vector<char> members(1 << 16);
    size_t mn = 0;
    int rc = hm_archive_read(reinterpret_cast<const unsigned char*>(data.data()), data.size(),
                             members.data(), members.size(), &mn);
    if (rc == -4 && mn > members.size()) {
      members.resize(mn);
      rc = hm_archive_read(reinterpret_cast<const unsigned char*>(data.data()), data.size(),
                           members.data(), members.size(), &mn);
    }
    if (rc != 0) {
      std::fprintf(stderr, "hermes header: %s is not a usable archive (status %d)\n",
                   in_path.c_str(), rc);
      return 4;
    }
    hm::JVal arr;
    size_t pos = 0;
    if (!hm::parse_json(members.data(), mn > 0 ? mn - 1 : 0, &arr, &pos) ||
        arr.kind() != hm::JVal::kArr || arr.size() == 0) {
      std::fprintf(stderr, "hermes header: %s has no members\n", in_path.c_str());
      return 4;
    }
    const hm::JVal* m0 = arr.at(0);
    const hm::JVal* off = m0->find("ir_off");
    const hm::JVal* len = m0->find("ir_len");
    if (off == nullptr || len == nullptr) {
      std::fprintf(stderr, "hermes header: %s member 0 is malformed\n", in_path.c_str());
      return 4;
    }
    const size_t o = static_cast<size_t>(off->as_int(0));
    const size_t k = static_cast<size_t>(len->as_int(0));
    if (o + k > data.size()) {
      std::fprintf(stderr, "hermes header: %s member 0 is out of range\n", in_path.c_str());
      return 4;
    }
    data.assign(data.data() + o, k);
  }

  std::vector<char> out(1 << 18);
  size_t n = 0;
  const int rc = (lang == "cpp")
                     ? hm_header_gen_cpp(data.data(), data.size(), out.data(), out.size(), &n)
                     : hm_header_gen_c(data.data(), data.size(), out.data(), out.size(), &n);
  if (rc != 0) {
    std::fprintf(stderr, "%s\n", tr("hermes header: cannot generate the header from this IR"));
    return 4;
  }
  const size_t body_len = n > 0 ? n - 1 : 0;
  if (out_path.empty()) {
    std::fwrite(out.data(), 1, body_len, stdout);
    return 0;
  }
  FILE* f = std::fopen(out_path.c_str(), "wb");
  if (f == nullptr) {
    std::fprintf(stderr, "hermes header: cannot write %s\n", out_path.c_str());
    return 4;
  }
  std::fwrite(out.data(), 1, body_len, f);
  std::fclose(f);
  std::fprintf(stderr, "%s %s (%zu bytes)\n", tr("hermes header: wrote"), out_path.c_str(),
               body_len);
  return 0;
}

}  // namespace

// ---- ar：查看与拆开 `.hbc` --------------------------------------------------
// 名字沿用 `ar`：这两件事同构——「归档里有什么」「把其中一个拿出来」。
namespace {

int ar_read(const std::string& path, std::string* raw, std::string* members) {
  if (!read_file(path.c_str(), raw)) {
    std::fprintf(stderr, "%s %s\n", tr("hermes ar: cannot read"), path.c_str());
    return 4;
  }
  std::vector<char> buf(1 << 16);
  size_t n = 0;
  int rc = hm_archive_read(reinterpret_cast<const unsigned char*>(raw->data()), raw->size(),
                           buf.data(), buf.size(), &n);
  if (rc == -4 && n > buf.size()) {
    buf.resize(n);
    rc = hm_archive_read(reinterpret_cast<const unsigned char*>(raw->data()), raw->size(),
                         buf.data(), buf.size(), &n);
  }
  if (rc != 0) {
    std::fprintf(stderr, "%s %s (%d)\n", tr("hermes ar: not a usable archive:"), path.c_str(), rc);
    return 4;
  }
  members->assign(buf.data(), n > 0 ? n - 1 : 0);
  return 0;
}

int cmd_ar_list(const std::vector<std::string>& args) {
  if (args.empty()) {
    std::fprintf(stderr, "%s\n", tr("hermes ar t: missing .hbc file"));
    return 2;
  }
  std::string raw, members;
  if (int rc = ar_read(args[0], &raw, &members)) return rc;
  hm::JVal arr;
  size_t pos = 0;
  if (!hm::parse_json(members.data(), members.size(), &arr, &pos) || arr.kind() != hm::JVal::kArr) {
    std::fprintf(stderr, "%s\n", tr("hermes ar: index is not readable"));
    return 4;
  }
  for (size_t i = 0; i < arr.size(); ++i) {
    const hm::JVal* m = arr.at(i);
    if (m == nullptr) continue;
    const hm::JVal* name = m->find("name");
    if (name == nullptr || name->kind() != hm::JVal::kStr) continue;
    std::printf("%s\n", name->as_str().c_str());
    // 四个命名空间分别列出：它们彼此独立，混在一起会让人以为同名冲突。
    const hm::JVal* exp = m->find("exports");
    const char* ns[4] = {"workflows", "types", "backends", "tools"};
    const char* label[4] = {"workflow", "type", "backend", "tool"};
    for (int k = 0; k < 4; ++k) {
      const hm::JVal* list = (exp != nullptr) ? exp->find(ns[k]) : nullptr;
      if (list == nullptr || list->kind() != hm::JVal::kArr) continue;
      for (size_t j = 0; j < list->size(); ++j) {
        const hm::JVal* v = list->at(j);
        if (v != nullptr && v->kind() == hm::JVal::kStr) {
          std::printf("  %-10s %s\n", label[k], v->as_str().c_str());
        }
      }
    }
  }
  return 0;
}

int cmd_ar_extract(const std::vector<std::string>& args) {
  std::string hbc, member, out;
  for (size_t i = 0; i < args.size(); ++i) {
    const std::string a = args[i];
    if (a == "-o" || a == "--out") {
      if (i + 1 >= args.size()) {
        std::fprintf(stderr, "%s\n", tr("hermes ar x: -o needs a file"));
        return 2;
      }
      out = args[++i];
    } else if (hbc.empty()) {
      hbc = a;
    } else if (member.empty()) {
      member = a;
    } else {
      std::fprintf(stderr, "%s %s\n", tr("hermes ar x: unexpected argument"), a.c_str());
      return 2;
    }
  }
  if (hbc.empty() || member.empty()) {
    std::fprintf(stderr, "%s\n", tr("hermes ar x: need <file.hbc> <member>"));
    return 2;
  }
  std::string raw, members;
  if (int rc = ar_read(hbc, &raw, &members)) return rc;
  hm::JVal arr;
  size_t pos = 0;
  if (!hm::parse_json(members.data(), members.size(), &arr, &pos) || arr.kind() != hm::JVal::kArr) {
    std::fprintf(stderr, "%s\n", tr("hermes ar: index is not readable"));
    return 4;
  }
  for (size_t i = 0; i < arr.size(); ++i) {
    const hm::JVal* m = arr.at(i);
    if (m == nullptr) continue;
    const hm::JVal* name = m->find("name");
    if (name == nullptr || name->kind() != hm::JVal::kStr || name->as_str() != member) continue;
    const hm::JVal* off = m->find("ir_off");
    const hm::JVal* len = m->find("ir_len");
    if (off == nullptr || len == nullptr) return 4;
    const size_t o = static_cast<size_t>(off->as_int(0));
    const size_t k = static_cast<size_t>(len->as_int(0));
    if (o + k > raw.size()) return 4;
    if (out.empty()) {
      std::string base = hbc;
      const size_t s = base.find_last_of('/');
      if (s != std::string::npos) base = base.substr(s + 1);
      out = base + "." + member + ".ir.json";
    }
    FILE* f = std::fopen(out.c_str(), "wb");
    if (f == nullptr) {
      std::fprintf(stderr, "%s %s\n", tr("hermes ar x: cannot write"), out.c_str());
      return 4;
    }
    std::fwrite(raw.data() + o, 1, k, f);
    std::fclose(f);
    std::fprintf(stderr, "%s %s (%zu bytes)\n", tr("hermes ar x: extracted"), out.c_str(), k);
    return 0;
  }
  std::fprintf(stderr, "%s %s\n", tr("hermes ar x: no member named"), member.c_str());
  return 4;
}

int cmd_ar(int argc, char** argv) {
  if (argc < 3) {
    std::printf("%s\n", tr("hermes ar: t <file.hbc> | x <file.hbc> <member> [-o out]"));
    return 2;
  }
  const std::string sub = argv[2];
  std::vector<std::string> rest;
  for (int i = 3; i < argc; ++i) rest.push_back(argv[i]);
  if (sub == "t") return cmd_ar_list(rest);
  if (sub == "x") return cmd_ar_extract(rest);
  if (sub == "-h" || sub == "--help" || sub == "help") {
    std::printf("%s\n", tr("hermes ar: t <file.hbc> | x <file.hbc> <member> [-o out]"));
    return 0;
  }
  std::fprintf(stderr, "%s %s\n", tr("hermes ar: unknown subcommand"), sub.c_str());
  return 2;
}

}  // namespace

// ---- doctor：这台机器能不能跑 Hermes ---------------------------------------
// 只读：它**从不修复**任何东西——静默修复会把"环境不对"变成"环境被改过"，
// 后者更难诊断。
namespace {

struct Check {
  std::string name;
  bool ok = false;
  bool required = true;
  std::string note;
};

std::string first_line(const std::string& s) {
  const size_t nl = s.find('\n');
  return (nl == std::string::npos) ? s : s.substr(0, nl);
}

// run_capture 跑一个命令并抓 stdout（用于 `clang --version` 探测）。
bool run_capture(const std::vector<std::string>& argv, std::string* out) {
  int fds[2];
  if (::pipe(fds) != 0) return false;
  pid_t pid = ::fork();
  if (pid < 0) {
    ::close(fds[0]);
    ::close(fds[1]);
    return false;
  }
  if (pid == 0) {
    ::dup2(fds[1], 1);
    ::close(fds[0]);
    ::close(fds[1]);
    std::vector<char*> cargv;
    for (const std::string& a : argv) cargv.push_back(const_cast<char*>(a.c_str()));
    cargv.push_back(nullptr);
    ::execvp(cargv[0], cargv.data());
    ::_exit(127);
  }
  ::close(fds[1]);
  char buf[4096];
  ssize_t n;
  out->clear();
  while ((n = ::read(fds[0], buf, sizeof buf)) > 0) out->append(buf, static_cast<size_t>(n));
  ::close(fds[0]);
  int status = 0;
  ::waitpid(pid, &status, 0);
  return WIFEXITED(status) && WEXITSTATUS(status) == 0;
}

Check check_native() {
  const unsigned abi = hm_abi_version();
  int32_t arch = 0;
  const char* arch_name = "unavailable";
  if (hm_cycle_arch(&arch) == 0) {
    arch_name = (arch == 1) ? "arm64" : (arch == 2 ? "x86_64" : "unknown");
  }
  Check c;
  c.name = "native ABI";
  c.ok = true;
  c.note = "abi=" + std::to_string(abi) + " arch=" + arch_name;
  return c;
}

Check check_stdlib() {
  Check c;
  c.name = "stdlib";
  const int n = hm_std_count();
  if (n == 0) {
    c.note = "no std modules embedded";
    return c;
  }
  std::string names;
  for (int i = 0; i < n; ++i) {
    const char* nm = hm_std_name(i);
    if (nm == nullptr) continue;
    std::vector<char> buf(1 << 16);
    size_t len = 0;
    if (hm_std_source(nm, buf.data(), buf.size(), &len) != 0 || len <= 1) {
      c.note = std::string("module ") + nm + " cannot be read";
      return c;
    }
    if (!names.empty()) names += ",";
    names += nm;
  }
  c.ok = true;
  c.note = names + " (" + std::to_string(n) + ")";
  return c;
}

// 目录列举：只为 doctor 的 golden 自检服务（POSIX opendir）。
std::vector<std::string> list_dir(const std::string& dir) {
  std::vector<std::string> out;
  DIR* d = ::opendir(dir.c_str());
  if (d == nullptr) return out;
  struct dirent* e;
  while ((e = ::readdir(d)) != nullptr) {
    const std::string name = e->d_name;
    if (name == "." || name == "..") continue;
    out.push_back(name);
  }
  ::closedir(d);
  std::sort(out.begin(), out.end());
  return out;
}

Check check_goldens() {
  Check c;
  c.name = "goldens(dev)";
  const std::string dir = "schemas/golden";
  const std::vector<std::string> ents = list_dir(dir);
  if (ents.empty()) {
    // golden 是**开发期**自检；发布包里没有它们。判成 FAIL 会让每个用户一装完
    // 就看到红色 doctor —— 而那不是他们的问题。
    c.ok = true;
    c.note = "not shipped with the release (dev-repo self-check only)";
    return c;
  }
  int n = 0;
  for (const std::string& name : ents) {
    if (name.rfind("ir-", 0) != 0 || name.size() < 5 ||
        name.compare(name.size() - 5, 5, ".json") != 0) {
      continue;
    }
    std::string body;
    if (!read_file((dir + "/" + name).c_str(), &body)) {
      c.name = "goldens";
      c.ok = false;
      c.note = "cannot read " + name;
      return c;
    }
    std::vector<char> dg(1 << 16);
    size_t dn = 0;
    char errbuf[256] = {0};
    size_t errn = 0;
    const int rc = hm_ir_validate(reinterpret_cast<const unsigned char*>(body.data()), body.size(),
                                  dg.data(), dg.size(), &dn, errbuf, sizeof errbuf, &errn);
    if (rc != 0) {
      c.name = "goldens";
      c.ok = true;  // 读不出来不等于 golden 过期（可能是校验器的输入要求变了）
      c.note = "cannot validate " + name;
      return c;
    }
    const std::string diagtxt(dg.data(), dn > 0 ? dn - 1 : 0);
    if (diagtxt != "[]" && diagtxt.find("\"severity\":\"error\"") != std::string::npos) {
      c.name = "goldens";
      c.ok = false;
      c.note = name + " failed the current validator - regenerate the golden";
      return c;
    }
    ++n;
  }
  if (n == 0) {
    c.name = "goldens";
    c.ok = false;
    c.note = "no goldens found";
    return c;
  }
  c.name = "goldens";
  c.ok = true;
  c.note = std::to_string(n) + " goldens all loadable";
  return c;
}

Check check_clang() {
  Check c;
  c.name = "clang";
  std::string out;
  if (!run_capture({"clang", "--version"}, &out)) {
    c.note = "clang not found on PATH (AOT / hermes cc unavailable)";
    return c;
  }
  if (out.find("clang") == std::string::npos) {
    // 红线相关：名为 clang 的壳背后可能是 gcc。
    c.note = "clang does not report clang on --version (policy requires clang only)";
    return c;
  }
  c.ok = true;
  c.note = first_line(out);
  return c;
}

Check check_runtime_lib(const char* lib, const char* inc) {
  Check c;
  c.name = "runtime lib";
  if (lib == nullptr || lib[0] == 0 || inc == nullptr || inc[0] == 0) {
    c.note = "runtime library/header not found (release should ship lib/ and include/; in dev run scripts/build.sh first)";
    return c;
  }
  // 显示的是**头文件名**（与本行语义一致：库 + 它的 C 头），不是库自己的 basename。
  char hdr[4096] = {0};
  if (hm_layout_header_for(lib, hdr, sizeof hdr) != 1 || hdr[0] == 0) {
    c.note = std::string(lib) + " (no C header next to it)";
    return c;
  }
  std::string base = hdr;
  const size_t s = base.find_last_of('/');
  if (s != std::string::npos) base = base.substr(s + 1);
  c.ok = true;
  c.note = std::string(lib) + " + " + base;
  return c;
}

int cmd_doctor(int argc, char** argv) {
  for (int i = 2; i < argc; ++i) {
    const std::string a = argv[i];
    if (a == "-h" || a == "--help" || a == "help") {
      std::printf("%s\n", tr("hermes-native doctor"));
      return 0;
    }
  }
  const std::string exe_s = host_exe_path();
  char exe[4096] = {0};
  std::snprintf(exe, sizeof exe, "%s", exe_s.c_str());
  char cwd[4096] = {0};
  if (::getcwd(cwd, sizeof cwd) == nullptr) cwd[0] = 0;
  char rootbuf[4096], libbuf[4096], incbuf[4096], shimbuf[4096];
  hm_layout_resolve(exe, cwd, rootbuf, sizeof rootbuf, libbuf, sizeof libbuf, incbuf, sizeof incbuf,
                    shimbuf, sizeof shimbuf);

  std::vector<Check> checks;
  checks.push_back(check_native());
  checks.push_back(check_stdlib());
  checks.push_back(check_goldens());
  checks.push_back(check_clang());
  checks.push_back(check_runtime_lib(libbuf, incbuf));
  {
    Check c;
    c.name = "sandbox shim";
    c.required = false;
    if (shimbuf[0] == 0) {
      c.note = "hermes-shim not found (sandbox unavailable)";
    } else {
      c.ok = true;
      c.note = shimbuf;
    }
    checks.push_back(c);
  }

  int failed = 0;
  for (const Check& c : checks) {
    const char* mark = "ok  ";
    if (!c.ok) {
      if (c.required) {
        mark = "FAIL";
        ++failed;
      } else {
        mark = "warn";
      }
    }
    std::printf("%s  %-22s %s\n", mark, c.name.c_str(), c.note.c_str());
  }
  if (failed > 0) {
    std::fprintf(stderr, "%s\n", tr("doctor: required checks failed"));
    return 4;
  }
  std::printf("%s\n", tr("doctor: all required checks passed"));
  return 0;
}

}  // namespace

extern "C" unsigned hm_abi_version(void);
extern "C" int hm_cycle_arch(int32_t* out);
extern "C" int hm_std_count(void);
extern "C" const char* hm_std_name(int i);
extern "C" int hm_ir_validate(const unsigned char* ir_json, size_t len, char* out,
                             size_t cap, size_t* out_len, char* out_err, size_t err_cap,
                             size_t* err_len);
// split_csv_c 按逗号/空格切（链接面在 C ABI 上是逗号分隔字符串）。
// ---- compile：.hermes → 原生可执行（AOT）-----------------------------------
// 默认路径先行：无 --link-* / 无 --target / 无 --static / 无 --sysroot。
// 其余旗标**明确拒绝**并说明——半支持的旗标比不支持更危险（用户以为按了，
// 实际没生效）。
namespace {

int cmd_compile(int argc, char** argv) {
  std::string src_path, out_path, backend = "c", target, sysroot;
  bool is_static = false;
  std::vector<std::string> link_static, link_dynamic;
  for (int i = 2; i < argc; ++i) {
    const std::string a = argv[i];
    if (a == "-o" || a == "--out") {
      if (i + 1 >= argc) {
        std::fprintf(stderr, "%s\n", tr("hermes compile: -o needs a file"));
        return 2;
      }
      out_path = argv[++i];
    } else if (a.rfind("--backend=", 0) == 0) {
      backend = a.substr(10);
    } else if (a == "--backend") {
      if (i + 1 >= argc) return 2;
      backend = argv[++i];
    } else if (a == "-h" || a == "--help" || a == "help") {
      std::printf("%s\n", tr("hermes-native compile <file.hermes> [-o prog] [--backend=c]"));
      return 0;
    } else if (a == "--target") {
      if (i + 1 >= argc) return 2;
      target = argv[++i];
    } else if (a.rfind("--target=", 0) == 0) {
      target = a.substr(9);
    } else if (a == "--sysroot") {
      if (i + 1 >= argc) return 2;
      sysroot = argv[++i];
    } else if (a.rfind("--sysroot=", 0) == 0) {
      sysroot = a.substr(10);
    } else if (a == "--static") {
      is_static = true;
    } else if (a == "--link-static") {
      if (i + 1 >= argc) return 2;
      link_static.push_back(argv[++i]);
    } else if (a.rfind("--link-static=", 0) == 0) {
      link_static.push_back(a.substr(14));
    } else if (a == "--link-dynamic") {
      if (i + 1 >= argc) return 2;
      link_dynamic.push_back(argv[++i]);
    } else if (a.rfind("--link-dynamic=", 0) == 0) {
      link_dynamic.push_back(a.substr(15));
    } else if (!a.empty() && a[0] == '-') {
      std::fprintf(stderr, "hermes compile: unknown flag %s\n", a.c_str());
      return 2;
    } else {
      src_path = a;
    }
  }
  if (src_path.empty()) {
    std::fprintf(stderr, "%s\n", tr("hermes compile: missing source file"));
    return 2;
  }
  if (backend != "c") {
    std::fprintf(stderr, "hermes compile: unknown backend \"%s\" (available: c)\n", backend.c_str());
    return 2;
  }
  if (out_path.empty()) {
    std::string base = src_path;
    const size_t s = base.find_last_of('/');
    if (s != std::string::npos) base = base.substr(s + 1);
    const size_t dot = base.find_last_of('.');
    if (dot != std::string::npos) base = base.substr(0, dot);
    out_path = base;
  }

  // 1. 装配（与 ir / build 共用同一段，41/41 逐字节）
  std::string ir_json;
  {
    std::string src;
    if (!read_file(src_path.c_str(), &src)) {
      std::fprintf(stderr, "%s %s\n", tr("hermes compile: cannot read"), src_path.c_str());
      return 4;
    }
    bool warned = false;
    const int rc = assemble_unit(src_path, {}, {}, true, RenderMode::kAll, &ir_json, &warned);
    if (rc != 0) return rc;
  }

  // 1.5 目标先解析出来：裸机闸门与 clang 参数都依赖它。
  char tgt_json_early[2048] = {0};
  bool is_bare = false;
  std::string triple;
  {
    char err[1024] = {0};
    size_t tn = 0, en = 0;
    if (sysroot.empty()) {
      const char* env_sysroot = ::getenv("HERMES_SYSROOT");
      if (env_sysroot != nullptr && *env_sysroot != 0) sysroot = env_sysroot;
    }
    const int rc = hm_target_parse(target.c_str(), HM_HOST_GOOS, HM_HOST_GOARCH, tgt_json_early,
                                   sizeof tgt_json_early, &tn, err, sizeof err, &en);
    if (rc != 0) {
      std::fprintf(stderr, "%s\n", err);
      return 4;
    }
    hm::JVal tj;
    size_t tp = 0;
    if (hm::parse_json(tgt_json_early, std::strlen(tgt_json_early), &tj, &tp)) {
      const hm::JVal* cls = tj.find("class");
      if (cls != nullptr && cls->kind() == hm::JVal::kStr) is_bare = cls->as_str() == "bare-metal";
      const hm::JVal* tr_ = tj.find("triple");
      if (tr_ != nullptr && tr_->kind() == hm::JVal::kStr) triple = tr_->as_str();
    }
  }

  // 1.6 输入侧闸门：静态链 glibc 的许可 + 每一个 --link-* 路径。
  //      放在编译动作之前——**静态 .a 不产生 DT_NEEDED**，产物侧查不到它。
  {
    std::vector<char> msg(2048);
    size_t mn = 0;
    if (hm_static_libc_gate(HM_HOST_GOOS, is_static ? 1 : 0, msg.data(), msg.size(), &mn) == 1) {
      std::fprintf(stderr, "%s\n", msg.data());
      return 4;
    }
    auto gate_one = [&](const char* flag, const std::string& lib) -> bool {
      if (hm_link_input_gate(flag, lib.c_str(), msg.data(), msg.size(), &mn) != 1) return false;
      // 只打 message：码是给测试与程序化调用用的，用户不该看到 JSON 壳。
      hm::JVal rej;
      size_t rp = 0;
      std::string body = msg.data();
      if (hm::parse_json(msg.data(), std::strlen(msg.data()), &rej, &rp) &&
          rej.kind() == hm::JVal::kObj) {
        const hm::JVal* m = rej.find("message");
        if (m != nullptr && m->kind() == hm::JVal::kStr) body = m->as_str();
      }
      std::fprintf(stderr, "%s\n", body.c_str());
      return true;
    };
    for (const std::string& lib : link_static) {
      if (gate_one("--link-static", lib)) return 4;
    }
    for (const std::string& lib : link_dynamic) {
      if (gate_one("--link-dynamic", lib)) return 4;
    }
  }

  // 2. 需要网络吗（决定要不要链 g* 归档）
  const int net_flag = hm_ir_uses_net(ir_json.data(), ir_json.size());
  if (net_flag < 0) {
    std::fprintf(stderr, "hermes compile: assembled IR is not readable\n");
    return 4;
  }
  const bool need_net = net_flag == 1;

  // 2.5 编译型产物的最后一道闸：非递归函数必须已展开（否则运行期变成动态派发）。
  {
    std::vector<char> msg(1024);
    size_t mn = 0;
    const int rc =
        hm_check_compiled_static(ir_json.data(), ir_json.size(), msg.data(), msg.size(), &mn);
    if (rc == 1) {
      std::fprintf(stderr, "hermes compile: %s\n", msg.data());
      return 4;
    }
    if (rc < 0) {
      std::fprintf(stderr, "hermes compile: assembled IR is not readable\n");
      return 4;
    }
  }

  // 3. 裸机闸门：宿主 OS 专属语义在编译期拒绝（文案是定稿，逐字不改）。
  if (is_bare) {
    std::vector<char> msg(512);
    size_t mn = 0;
    const int rc = hm_check_hostonly(ir_json.data(), ir_json.size(), 1, msg.data(), msg.size(), &mn);
    if (rc == 1) {
      std::fprintf(stderr, "%s\n", msg.data());
      return 4;
    }
    if (rc < 0) {
      std::fprintf(stderr, "hermes compile: assembled IR is not readable\n");
      return 4;
    }
  }

  // 4. 链接面（唯一决策点）
  // 网络归档的路径只给**候选**：要不要它、缺了算哪种拒绝，全部由 link plan 决定。
  // 在这里提前判"文件在不在"会把裸机目标的 E3110（硬件层禁止 Go runtime）抢走，
  // 变成一句"缺文件"——一个错误码两种原因等于没有原因。
  std::string net_lib;
  if (need_net) {
    // 与 Go 一致：HERMES_NET_LIB 可覆盖默认位置（测试与打包都要能指过去）。
    const char* env_net = ::getenv("HERMES_NET_LIB");
    net_lib = (env_net != nullptr && *env_net != 0) ? env_net : "build/glibhermesnet.a";
  }
  char plan_json[4096] = {0};
  {
    size_t pn = 0;
    std::string sjoin, djoin;
    for (const std::string& l : link_static) {
      if (!sjoin.empty()) sjoin += ",";
      sjoin += l;
    }
    for (const std::string& l : link_dynamic) {
      if (!djoin.empty()) djoin += ",";
      djoin += l;
    }
    const int rc = hm_link_plan(tgt_json_early, need_net ? 1 : 0, net_lib.c_str(), sjoin.c_str(),
                                djoin.c_str(), plan_json, sizeof plan_json, &pn);
    if (rc == 1) {
      // 拒绝时只打 **message**：码是给测试与程序化调用用的，用户的 stderr
      // 不该看到 JSON 壳（定稿文案要求逐字出现）。
      hm::JVal rej;
      size_t rp = 0;
      std::string msg = plan_json;
      if (hm::parse_json(plan_json, std::strlen(plan_json), &rej, &rp) &&
          rej.kind() == hm::JVal::kObj) {
        const hm::JVal* m = rej.find("message");
        if (m != nullptr && m->kind() == hm::JVal::kStr) msg = m->as_str();
      }
      std::fprintf(stderr, "%s\n", msg.c_str());
      return 4;
    }
    if (rc != 0) {
      std::fprintf(stderr, "hermes compile: link plan failed\n");
      return 4;
    }
  }
  hm::JVal plan;
  size_t ppos = 0;
  if (!hm::parse_json(plan_json, std::strlen(plan_json), &plan, &ppos)) return 4;
  auto plan_str = [&](const char* k) -> std::string {
    const hm::JVal* v = plan.find(k);
    return (v != nullptr && v->kind() == hm::JVal::kStr) ? v->as_str() : "";
  };

  // 5. IR → C
  std::vector<char> csrc(1 << 20);
  size_t cn = 0;
  {
    char cerr[512] = {0};
    size_t cen = 0;
    int rc = hm_backend_c_emit(ir_json.data(), ir_json.size(), csrc.data(), csrc.size(), &cn, cerr,
                               sizeof cerr, &cen);
    if (rc == -4 && cn > csrc.size()) {
      csrc.resize(cn);
      rc = hm_backend_c_emit(ir_json.data(), ir_json.size(), csrc.data(), csrc.size(), &cn, cerr,
                             sizeof cerr, &cen);
    }
    if (rc != 0) {
      std::fprintf(stderr, "hermes compile: %s\n", cerr);
      return 4;
    }
  }

  char tmpl[] = "/tmp/hermes-compile-XXXXXX";
  const char* dir = ::mkdtemp(tmpl);
  if (dir == nullptr) {
    std::fprintf(stderr, "hermes compile: cannot create a work dir\n");
    return 4;
  }
  const std::string d(dir);
  // HERMES_COMPILE_KEEP=1 保留工作目录：失败现场（生成的 C、实际命令行）是
  // 排查"为什么产物行为不对"的唯一证据，而默认删掉意味着每个问题都得靠猜。
  const bool keep_work = ::getenv("HERMES_COMPILE_KEEP") != nullptr;
  if (keep_work) std::fprintf(stderr, "hermes compile: keeping work dir %s\n", d.c_str());
  const std::string cfile = d + "/unit.c";
  if (!write_text(cfile, std::string(csrc.data(), cn > 0 ? cn - 1 : 0))) {
    std::fprintf(stderr, "hermes compile: cannot write the generated C\n");
    return 4;
  }
  // 网络头只在真需要时落盘：裸机产物目录里连一个 hermes_net.h 都不该出现。
  if (need_net) {
    std::string hdr;
    const char* env_hdr = ::getenv("HERMES_NET_HEADER");
    const std::string hdr_path =
        (env_hdr != nullptr && *env_hdr != 0) ? env_hdr : "native/include/hermes_net.h";
    if (!read_file(hdr_path.c_str(), &hdr)) {
      std::fprintf(stderr, "hermes compile: this program calls tools/models but "
                           "native/include/hermes_net.h is missing\n");
      return 4;
    }
    if (!write_text(d + "/hermes_net.h", hdr)) return 4;
  }

  // 6. clang
  const char* cc = ::getenv("CC");
  if (cc == nullptr || *cc == 0) cc = "clang";
  std::vector<std::string> cmd{cc, "-std=c99", "-O2", "-Wall", "-ffp-contract=off"};
  // 原生目标刻意**不加** -target：clang 自己的默认三元组就是对的，显式传会改变
  // 驱动对 sysroot/链接器的选择（Apple 平台上尤其）。
  if (!triple.empty()) {
    cmd.push_back("-target");
    cmd.push_back(triple);
  }
  if (is_bare) {
    // 裸机没有 hosted 环境：不许 clang 假设 libc 语义，也不许它补 hosted-only 内建。
    cmd.push_back("-ffreestanding");
  }
  if (!sysroot.empty()) {
    cmd.push_back("--sysroot");
    cmd.push_back(sysroot);
  }
  if (need_net) {
    cmd.push_back("-I");
    cmd.push_back(d);
  }
  cmd.push_back(cfile);
  for (const std::string& lib : split_csv_c(plan_str("static"))) cmd.push_back(lib);
  for (const std::string& lib : split_csv_c(plan_str("dynamic"))) cmd.push_back(lib);
  for (const std::string& fw : split_csv_c(plan_str("frameworks"))) cmd.push_back(fw);
  cmd.push_back("-o");
  cmd.push_back(out_path);
  const int rc = run_argv(cmd);
  if (rc != 0) {
    std::fprintf(stderr, "hermes compile: compilation failed (clang exit %d)\n", rc);
    if (!keep_work) std::system((std::string("rm -rf ") + d).c_str());
    return 4;
  }

  // 7. 产物侧兜底（fail-closed），实现与判据都在 product_gate.cpp 里。
  {
    std::vector<char> msg(2048);
    size_t mn = 0;
    const int rc = hm_product_link_gate(out_path.c_str(), link_dynamic.empty() ? 0 : 1,
                                        is_bare ? "1" : "0", plan_json, msg.data(), msg.size(), &mn);
    if (rc == 1) {
      std::fprintf(stderr, "%s\n", msg.data());
      if (!keep_work) std::system((std::string("rm -rf ") + d).c_str());
      return 4;
    }
  }

  if (!keep_work) std::system((std::string("rm -rf ") + d).c_str());
  if (need_net) {
    std::fprintf(stderr, "%s\n",
                 trf("hermes compile: produced native executable %s (AOT, no IR, no interpreter; tool/model calls link glibhermesnet.a)",
                     out_path.c_str())
                     .c_str());
  } else if (!split_join(plan_str("go_runtime")).empty()) {
    const std::string gr = split_join(plan_str("go_runtime"));
    std::fprintf(stderr, "%s\n",
                 trf("hermes compile: produced native executable %s (AOT, no IR, no interpreter; links Go-runtime libraries %s via --link-static/--link-dynamic)",
                     out_path.c_str(), gr.c_str())
                     .c_str());
  } else if (is_bare) {
    std::fprintf(stderr, "%s\n",
                 trf("hermes compile: produced bare-metal AOT artifact %s (no IR, no Go runtime, no interpreter, static C/Rust only)",
                     out_path.c_str())
                     .c_str());
  } else {
    std::fprintf(stderr, "%s\n",
                 trf("hermes compile: produced native executable %s (AOT, no IR, no Go runtime, no interpreter)",
                     out_path.c_str())
                     .c_str());
  }
  return 0;
}

}  // namespace

extern "C" int hm_ir_uses_net(const char* ir_json, size_t len);
extern "C" int hm_link_plan(const char* target_json, int need_net, const char* net_lib, const char* static_csv, const char* dynamic_csv, char* out_json, size_t cap, size_t* out_len);
extern "C" int hm_target_parse(const char* raw, const char* goos, const char* goarch, char* out_json, size_t cap, size_t* out_len, char* out_err, size_t err_cap, size_t* err_len);
extern "C" int hm_backend_c_emit(const char* ir_json, size_t len, char* out, size_t cap, size_t* out_len, char* out_err, size_t err_cap, size_t* err_len);
// ---- 驱动：clang 风格的直接用法 -------------------------------------------
//
//   hermes app.hermes -o app      一条命令出可执行文件
//   hermes app.hermes -c          只产 .hbc
//   hermes app.hermes -E          只做预处理（展开 #include）
//
// 为什么不另做一套编译流程：driver 只**翻译参数**，每条路径都走同一套已经过
// 测判据的实现。两套实现意味着两份 bug。
namespace {

bool is_driver_invocation(int argc, char** argv) {
  if (argc < 2) return false;
  const std::string first = argv[1];
  if (!first.empty() && first[0] == '-') {
    return first == "-o" || first == "-c" || first == "-E";
  }
  static const char* kCommands[] = {"version", "trace", "import", "ir", "run", "build",
                                    "ar",      "link",  "sign",  "verify", "serve", "header",
                                    "compile", "cc",    "doctor", "fmt", "explain", "lsp"};
  for (const char* c : kCommands) {
    if (first == c) return false;
  }
  return ends_with_ci(first, ".hermes") || ends_with_ci(first, ".h");
}

int cmd_driver(int argc, char** argv) {
  std::string out, src;
  bool obj_only = false, preproc = false, verbose = false;
  std::vector<std::string> lib_dirs;
  int src_count = 0;
  for (int i = 1; i < argc; ++i) {
    const std::string a = argv[i];
    if (a == "-o") {
      if (i + 1 >= argc) {
        std::fprintf(stderr, "hermes: -o needs a file name\n");
        return 2;
      }
      out = argv[++i];
    } else if (a == "-c") {
      obj_only = true;
    } else if (a == "-E") {
      preproc = true;
    } else if (a == "-v" || a == "--verbose") {
      verbose = true;
    } else if (a == "--version") {
      return cmd_version();
    } else if (a == "-h" || a == "--help") {
      std::printf("%s\n", tr("hermes-native <file.hermes> [-o out] [-c|-E] [-L dir]"));
      return 0;
    } else if (a == "-I" || a == "-l") {
      if (i + 1 >= argc) {
        std::fprintf(stderr, "hermes: %s needs a value\n", a.c_str());
        return 2;
      }
      ++i;  // include 搜索路径由源文件目录推导；库由 requires 驱动
    } else if (a == "-L") {
      if (i + 1 >= argc) {
        std::fprintf(stderr, "hermes: -L needs a directory\n");
        return 2;
      }
      lib_dirs.push_back(argv[++i]);
    } else if (!a.empty() && a[0] == '-') {
      std::fprintf(stderr, "hermes: unknown flag \"%s\"\n", a.c_str());
      return 2;
    } else {
      ++src_count;
      if (src.empty()) src = a;
    }
  }
  if (src_count == 0) {
    std::fprintf(stderr, "%s\n", tr("hermes: no input files"));
    return 2;
  }
  if (src_count > 1) {
    // 多源文件需要"分别编译再链接"，而现在一次构建产出一个 unit。
    // 明确拒绝，不假装支持 —— 假装支持会产出只含第一个文件的二进制。
    std::fprintf(stderr,
                 "hermes: only one source file is accepted (got %d).\n"
                 "compile multiple files into separate .hbc archives, then merge them with "
                 "`hermes link`\n",
                 src_count);
    return 2;
  }

  std::string base = src;
  const size_t slash = base.find_last_of('/');
  if (slash != std::string::npos) base = base.substr(slash + 1);
  const size_t dot = base.find_last_of('.');
  if (dot != std::string::npos) base = base.substr(0, dot);

  if (preproc) {
    std::string dump, ir;
    bool warned = false;
    const int rc = assemble_unit(src, {}, {}, true, RenderMode::kAll, &ir, &warned, &dump);
    if (rc != 0) return rc;
    if (out.empty()) {
      std::fwrite(dump.data(), 1, dump.size(), stdout);
      return 0;
    }
    return write_text(out, dump) ? 0 : 4;
  }

  std::vector<std::string> argv2;
  if (obj_only) {
    const std::string o = out.empty() ? base + ".hbc" : out;
    argv2 = {src, "-o", o};
    for (const std::string& d : lib_dirs) {
      argv2.push_back("-L");
      argv2.push_back(d);
    }
    if (verbose) {
      std::string line;
      for (const std::string& x : argv2) line += x + " ";
      std::fprintf(stderr, "hermes: %s\n", line.c_str());
    }
    // 复用 build：构造一个 argv 交给 cmd_build。
    std::vector<std::string> full = {"hermes-native", "build"};
    full.insert(full.end(), argv2.begin(), argv2.end());
    std::vector<char*> cargv;
    for (auto& x : full) cargv.push_back(const_cast<char*>(x.c_str()));
    return cmd_build(static_cast<int>(cargv.size()), cargv.data());
  }

  argv2 = {src, "-o", out.empty() ? std::string("a.out") : out};
  if (verbose) {
    std::string line;
    for (const std::string& x : argv2) line += x + " ";
    std::fprintf(stderr, "hermes: %s\n", line.c_str());
  }
  std::vector<std::string> full = {"hermes-native", "compile"};
  full.insert(full.end(), argv2.begin(), argv2.end());
  std::vector<char*> cargv;
  for (auto& x : full) cargv.push_back(const_cast<char*>(x.c_str()));
  return cmd_compile(static_cast<int>(cargv.size()), cargv.data());
}

}  // namespace

// ---- licenses：把分发时必须随附的许可文本打出来 -----------------------------
//
// 为什么值得一条命令：合规手册写着"分发要带许可证"，而真正会出错的地方是
// "带哪一个、从哪儿拿"。让它变成一条命令，就没人需要凭记忆拼一份清单。
namespace {

int cmd_licenses(int argc, char** argv) {
  for (int i = 2; i < argc; ++i) {
    const std::string a = argv[i];
    if (a == "-h" || a == "--help" || a == "help") {
      std::printf("%s\n", tr("hermes licenses: print LICENSE / NOTICE / THIRD_PARTY_NOTICES.md / licenses/*"));
      return 0;
    }
  }

  const std::string exe_s = host_exe_path();
  char exe[4096] = {0};
  std::snprintf(exe, sizeof exe, "%s", exe_s.c_str());
  char cwd[4096] = {0};
  if (::getcwd(cwd, sizeof cwd) == nullptr) cwd[0] = 0;
  char rootbuf[4096], libbuf[4096], incbuf[4096], shimbuf[4096];
  hm_layout_resolve(exe, cwd, rootbuf, sizeof rootbuf, libbuf, sizeof libbuf, incbuf, sizeof incbuf,
                    shimbuf, sizeof shimbuf);

  // 候选根：布局根、当前目录、以及当前目录往上的四层。
  // 向上找是必要的：`go run` 把二进制放在临时目录，而许可文件在仓库里。
  std::vector<std::string> candidates;
  if (rootbuf[0] != 0) candidates.push_back(rootbuf);
  std::string dir = cwd;
  for (int i = 0; i < 4 && !dir.empty() && dir != "/"; ++i) {
    candidates.push_back(dir);
    const size_t s = dir.find_last_of('/');
    if (s == std::string::npos) break;
    dir = (s == 0) ? "/" : dir.substr(0, s);
  }

  std::string root;
  std::vector<std::string> tried;
  for (const std::string& c : candidates) {
    tried.push_back(c);
    if (file_exists(c + "/LICENSE")) {
      root = c;
      break;
    }
  }
  if (root.empty()) {
    std::fprintf(stderr, "%s\n", tr("hermes licenses: no LICENSE found."));
    for (const std::string& t : tried) std::fprintf(stderr, "  %s\n", t.c_str());
    std::fprintf(stderr, "%s\n",
                 tr("hermes licenses: a release package should ship LICENSE / NOTICE / THIRD_PARTY_NOTICES.md / licenses/."));
    return 4;
  }

  std::vector<std::string> files = {root + "/LICENSE", root + "/NOTICE",
                                    root + "/THIRD_PARTY_NOTICES.md"};
  const std::string lic_dir = root + "/licenses";
  DIR* d = ::opendir(lic_dir.c_str());
  if (d != nullptr) {
    std::vector<std::string> names;
    struct dirent* e;
    while ((e = ::readdir(d)) != nullptr) {
      const std::string n = e->d_name;
      if (n == "." || n == "..") continue;
      struct stat st{};
      if (::stat((lic_dir + "/" + n).c_str(), &st) == 0 && !S_ISDIR(st.st_mode)) names.push_back(n);
    }
    ::closedir(d);
    std::sort(names.begin(), names.end());
    for (const std::string& n : names) files.push_back(lic_dir + "/" + n);
  }

  int printed = 0;
  for (const std::string& f : files) {
    std::string body;
    if (!read_file(f.c_str(), &body)) continue;
    const size_t s = f.find_last_of('/');
    const std::string base = (s == std::string::npos) ? f : f.substr(s + 1);
    std::printf("===== %s =====\n\n%s\n\n", base.c_str(), body.c_str());
    ++printed;
  }
  if (printed == 0) {
    std::fprintf(stderr, "%s %s\n", tr("hermes licenses: no license file found under"), root.c_str());
    return 4;
  }
  return 0;
}

}  // namespace

// ---- new：脚手架 -----------------------------------------------------------
// 为什么需要它：语言允许一个 unit 有多个 workflow（D-037），但"多 workflow 怎么
// 组织文件、`#link` 放哪、契约与实现怎么分"这些**结构性决定**仍然费脑子。
// 脚手架把决定固化成能跑的例子，用户改内容而不是从头想结构。
namespace {

std::string sanitize_unit(const std::string& name) {
  std::string out;
  for (size_t i = 0; i < name.size(); ++i) {
    const char c = name[i];
    const bool alpha = (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_';
    const bool digit = c >= '0' && c <= '9';
    if (alpha) {
      out.push_back(c);
    } else if (digit && i > 0) {
      out.push_back(c);
    } else if (c == '-' || c == ' ') {
      out.push_back('_');
    }
  }
  if (out.empty()) return "unit";
  return out;
}

bool make_dir(const std::string& path) {
  std::string cur;
  for (size_t i = 0; i <= path.size(); ++i) {
    if (i == path.size() || path[i] == '/') {
      if (!cur.empty() && ::mkdir(cur.c_str(), 0755) != 0 && errno != EEXIST) return false;
    }
    if (i < path.size()) cur.push_back(path[i]);
  }
  return true;
}

int cmd_new(int argc, char** argv) {
  std::string name;
  for (int i = 2; i < argc; ++i) {
    const std::string a = argv[i];
    if (a == "-h" || a == "--help" || a == "help") {
      std::printf("%s\n", tr("hermes new <dir> - scaffold a project"));
      return 0;
    }
    if (name.empty()) {
      name = a;
    } else {
      std::fprintf(stderr, "%s %s\n", tr("hermes new: unexpected argument"), a.c_str());
      return 2;
    }
  }
  if (name.empty()) {
    std::fprintf(stderr, "%s\n", tr("hermes new <dir> - scaffold a project"));
    return 2;
  }
  if (file_exists(name)) {
    std::fprintf(stderr, "%s %s\n", tr("hermes new: already exists:"), name.c_str());
    return 4;
  }
  const std::string unit = sanitize_unit(name);

  const std::string h =
      "// Contract: types, backend declarations, workflow signatures.\n"
      "// When distributing a .hbc, the other side only needs this file.\n"
      "#include <std/model.h>\n"
      "#include <std/net.h>\n\n"
      "export struct Input {\n    string text;\n}\n\n"
      "export backend gateway : openai;\n\n"
      "export workflow triage(input: Input, backend = gateway) entry -> string;\n"
      "export workflow ping(input: Input) -> string;\n";

  const std::string impl =
      "// Implementation: #link, addresses, workflow bodies.\n"
      "#include \"" + unit + ".h\"\n\n" +
      "#link <std/model>;\n#link <std/net>;\n\n"
      "// Addresses come from environment injection: a hard-coded address cannot be distributed.\n"
      "backend gateway = env(\"GATEWAY_URL\", \"http://127.0.0.1:8080\");\n\n"
      "workflow triage(input: Input, backend = gateway) entry {\n"
      "    return \"triage: {input.text}\";\n"
      "}\n\n"
      "// A second workflow: a unit may hold several (D-037).\n"
      "// Each unit has exactly **one** entry -- the default for run / serve.\n"
      "workflow ping(input: Input) {\n"
      "    return \"pong\";\n"
      "}\n";

  const std::string readme =
      "# " + unit + "\n\n"
      "    hermes build " + unit + ".hermes -o " + unit + ".hbc\n"
      "    hermes run " + unit + ".hbc --input in.json\n"
      "    hermes run " + unit + ".hbc --workflow ping --input in.json\n"
      "    hermes explain " + unit + ".hbc\n\n" +
      unit + ".h holds the contract (types, backend declarations, workflow signatures).\n" +
      unit + ".hermes holds the implementation (#link, addresses, bodies).\n"
      "They are separate because the contract must be distributable on its own,\n"
      "while addresses and link inputs are choices of this file.\n";

  const std::vector<std::pair<std::string, std::string>> files = {
      {name + "/" + unit + ".h", h},
      {name + "/" + unit + ".hermes", impl},
      {name + "/README.md", readme},
      {name + "/in.json", "{\"text\": \"hello\"}\n"},
  };
  for (const auto& f : files) {
    const size_t slash = f.first.find_last_of('/');
    const std::string dir = (slash == std::string::npos) ? "" : f.first.substr(0, slash);
    if (!dir.empty() && !make_dir(dir)) {
      std::fprintf(stderr, "hermes new: cannot create %s\n", dir.c_str());
      return 4;
    }
    if (!write_text(f.first, f.second)) {
      std::fprintf(stderr, "hermes new: cannot write %s\n", f.first.c_str());
      return 4;
    }
  }
  std::printf("%s %s/\n", tr("created"), name.c_str());
  std::printf("  cd %s && hermes build %s.hermes -o %s.hbc\n", name.c_str(), unit.c_str(),
              unit.c_str());
  return 0;
}

}  // namespace

// ---- import：把 homonoia 计划转成 .hermes 源码 ------------------------------
namespace {

int cmd_import(int argc, char** argv) {
  std::string plan_path, out_path;
  for (int i = 2; i < argc; ++i) {
    const std::string a = argv[i];
    if (a == "--from-plan") {
      if (i + 1 >= argc) {
        std::fprintf(stderr, "%s\n", tr("hermes import: --from-plan needs a file argument"));
        return 2;
      }
      plan_path = argv[++i];
    } else if (a == "-o" || a == "--out") {
      if (i + 1 >= argc) {
        std::fprintf(stderr, "%s\n", tr("hermes import: -o needs a file argument"));
        return 2;
      }
      out_path = argv[++i];
    } else if (a == "-h" || a == "--help" || a == "help") {
      std::printf("%s\n", tr("hermes import --from-plan <plan.json> [-o <out.hermes>]"));
      return 0;
    } else if (!a.empty() && a[0] == '-') {
      std::fprintf(stderr, "%s %s\n", tr("hermes import: unknown argument"), a.c_str());
      return 2;
    } else if (plan_path.empty()) {
      plan_path = a;
    } else {
      std::fprintf(stderr, "%s %s\n", tr("hermes import: unknown argument"), a.c_str());
      return 2;
    }
  }
  if (plan_path.empty()) {
    std::fprintf(stderr, "%s\n", tr("hermes import: missing --from-plan"));
    return 2;
  }

  std::string data;
  if (!read_file(plan_path.c_str(), &data)) {
    std::fprintf(stderr, "%s %s\n", tr("hermes import: cannot read plan"), plan_path.c_str());
    return 4;
  }

  std::vector<char> src(1 << 16), warn(1 << 16), err(1024);
  size_t sn = 0, wn = 0, en = 0;
  int rc = hm_import_plan(data.data(), data.size(), src.data(), src.size(), &sn, warn.data(),
                          warn.size(), &wn, err.data(), err.size(), &en);
  if (rc == -4) {
    src.resize(sn);
    warn.resize(wn);
    rc = hm_import_plan(data.data(), data.size(), src.data(), src.size(), &sn, warn.data(),
                        warn.size(), &wn, err.data(), err.size(), &en);
  }
  if (rc == 1) {
    std::fprintf(stderr, "%s %s %s\n", "hermes import:", plan_path.c_str(),
                 (std::string("is not a usable plan: ") + err.data()).c_str());
    return 4;
  }
  if (rc != 0) {
    std::fprintf(stderr, "hermes import: internal error\n");
    return 4;
  }

  // warning 走 stderr：产物 stdout 必须只有源码，否则 `> x.hermes` 会把警告
  // 一起写进文件，而那是一个会被提交上去的坏文件。
  hm::JVal warr;
  size_t wp = 0;
  int warn_count = 0;
  if (wn > 0 && hm::parse_json(warn.data(), wn > 0 ? wn - 1 : 0, &warr, &wp) &&
      warr.kind() == hm::JVal::kArr) {
    for (size_t i = 0; i < warr.size(); ++i) {
      const hm::JVal* w = warr.at(i);
      if (w != nullptr && w->kind() == hm::JVal::kStr) {
        std::fprintf(stderr, "%s %s\n", tr("hermes import: note:"), w->as_str().c_str());
        ++warn_count;
      }
    }
  }

  const size_t body_len = sn > 0 ? sn - 1 : 0;
  if (out_path.empty()) {
    std::fwrite(src.data(), 1, body_len, stdout);
    return 0;
  }
  const size_t slash = out_path.find_last_of('/');
  if (slash != std::string::npos) {
    const std::string dir = out_path.substr(0, slash);
    if (!dir.empty() && !make_dir(dir)) {
      std::fprintf(stderr, "hermes import: cannot create directory %s\n", dir.c_str());
      return 4;
    }
  }
  if (!write_text(out_path, std::string(src.data(), body_len))) {
    std::fprintf(stderr, "hermes import: cannot write %s\n", out_path.c_str());
    return 4;
  }
  std::fprintf(stderr, "%s %s (%zu bytes, dropped %d kinds of unrecognised flags)\n",
               tr("hermes import: wrote"), out_path.c_str(), body_len, warn_count);
  return 0;
}

}  // namespace

int main(int argc, char** argv) {
  if (argc < 2) return usage(stderr) + 2;
  // clang 风格（`hermes app.hermes -o app`）先判：它没有子命令名。
  if (is_driver_invocation(argc, argv)) return cmd_driver(argc, argv);
  const char* cmd = argv[1];
  if (std::strcmp(cmd, "version") == 0) return cmd_version();
  if (std::strcmp(cmd, "lsp") == 0) return hm_lsp_main();
  if (std::strcmp(cmd, "explain") == 0) return cmd_explain(argc, argv);
  if (std::strcmp(cmd, "fmt") == 0) return cmd_fmt(argc, argv);
  if (std::strcmp(cmd, "deps") == 0) return cmd_deps(argc, argv);
  if (std::strcmp(cmd, "ir") == 0) return cmd_ir(argc, argv);
  if (std::strcmp(cmd, "build") == 0) return cmd_build(argc, argv);
  if (std::strcmp(cmd, "cc") == 0) return cmd_cc(argc, argv);
  if (std::strcmp(cmd, "header") == 0) return cmd_header(argc, argv);
  if (std::strcmp(cmd, "ar") == 0) return cmd_ar(argc, argv);
  if (std::strcmp(cmd, "doctor") == 0) return cmd_doctor(argc, argv);
  if (std::strcmp(cmd, "licenses") == 0) return cmd_licenses(argc, argv);
  if (std::strcmp(cmd, "new") == 0) return cmd_new(argc, argv);
  if (std::strcmp(cmd, "import") == 0) return cmd_import(argc, argv);
  if (std::strcmp(cmd, "link") == 0) return hm_link_main(argc, argv);
  if (std::strcmp(cmd, "trace") == 0) return hm_trace_main(argc, argv);
  if (std::strcmp(cmd, "compile") == 0) return cmd_compile(argc, argv);
  if (std::strcmp(cmd, "-h") == 0 || std::strcmp(cmd, "--help") == 0 ||
      std::strcmp(cmd, "help") == 0) {
    return usage(stdout);
  }
  std::fprintf(stderr, "hermes-native: command %s is not ported yet\n", cmd);
  return 2;
}
