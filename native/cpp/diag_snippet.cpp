// diag_snippet.cpp —— 从源码里截出诊断位置的片段（含行号与 ^ 指示）。
//
// 为什么在 C++：诊断渲染分两层 —— 文案（catalog，已在 C++）与片段（由宿主从
// 源码里截）。native 主程序要自己打诊断，就得自己截片段；让 Go 截好再传进来，
// 等于把宿主职责又推回 Go。
//
// 形状刻意与 Go 侧一致（用户不该因为命令实现不同而看到两种排版）：
//
//       |
//   3 |     return input.nope;
//       |                  ^^^^
#include "hermes_parser.h"

#include <cstdio>
#include <cstring>
#include <string>

namespace {

// 取第 line 行（1-based）。找不到返回空串。
std::string line_at(const std::string& src, int line) {
  if (line <= 0) return "";
  int cur = 1;
  size_t pos = 0;
  while (pos <= src.size()) {
    size_t nl = src.find('\n', pos);
    const size_t end = (nl == std::string::npos) ? src.size() : nl;
    if (cur == line) return src.substr(pos, end - pos);
    if (nl == std::string::npos) break;
    pos = nl + 1;
    ++cur;
  }
  return "";
}

}  // namespace

extern "C" int hm_diag_snippet(const char* src, size_t len, int line, int col, int end_col,
                               char* out, size_t cap, size_t* out_len) {
  if (src == nullptr || out == nullptr || out_len == nullptr) return -1;
  if (line <= 0) {
    *out_len = 1;
    if (cap > 0) out[0] = 0;
    return 0;
  }
  const std::string text(src, len);
  const std::string body = line_at(text, line);
  // 行号栏宽度固定 2 位足够（我们的源码不会出现 4 位行号的片段需求；
  // 超出时仍然正确，只是栏宽变宽）。
  const std::string gutter = "  ";

  std::string o;
  o += gutter + " |\n";
  char ln[32];
  std::snprintf(ln, sizeof ln, "%d", line);
  std::string num(ln);
  while (num.size() < 2) num = " " + num;
  o += num + " | " + body + "\n";
  o += gutter + " | ";
  const int start = (col > 0) ? col - 1 : 0;
  const int end = (end_col > col) ? end_col - 1 : start + 1;
  for (int i = 0; i < start; ++i) o += (i < static_cast<int>(body.size()) && body[i] == '\t') ? '\t' : ' ';
  const int width = (end > start) ? (end - start) : 1;
  for (int i = 0; i < width; ++i) o += '^';

  const size_t need = o.size() + 1;
  if (need > cap) {
    *out_len = need;
    return -4;
  }
  std::memcpy(out, o.data(), o.size());
  out[o.size()] = 0;
  *out_len = need;
  return 0;
}
