// json_read.cpp —— 最小 JSON 读入器（读已装配的 IR）。
//
// 为什么手写而不是引第三方：红线是零第三方 C++ 库。为什么现在才写：
// 在 2.0.1 之前 C++ 侧只**写** IR（json_write.cpp），从不读它 —— 读回来的是
// Go 的装配层。C++ 后端要接手"IR → C"，就必须能读 IR，这个读入器是那一步的地基。
//
// 刻意支持得很窄，与 canonical 的三条约束对齐：
//   · 对象、数组、字符串、整数、true/false/null
//   · **不接受浮点 JSON 数字**。IR 里浮点常量是位模式对象（见 json_write.h），
//     真出现浮点数说明有人手改了 IR 或换了发射器，那应当当场报错而不是猜。
//   · 键序不要求有序：读入不依赖顺序（写出才依赖）。
#include "json_write.h"

#include <cstring>

namespace hm {
namespace {

struct Cursor {
  const char* p;
  const char* end;
  bool failed;
};

void skip_ws(Cursor& c) {
  while (c.p < c.end) {
    char ch = *c.p;
    if (ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r') { ++c.p; continue; }
    break;
  }
}

bool parse_value(Cursor& c, JVal* out, int depth);

bool parse_string(Cursor& c, std::string* out) {
  if (c.p >= c.end || *c.p != '"') return false;
  ++c.p;
  std::string s;
  while (c.p < c.end) {
    char ch = *c.p++;
    if (ch == '"') { *out = std::move(s); return true; }
    if (ch != '\\') { s.push_back(ch); continue; }
    if (c.p >= c.end) return false;
    char esc = *c.p++;
    switch (esc) {
      case '"': s.push_back('"'); break;
      case '\\': s.push_back('\\'); break;
      case '/': s.push_back('/'); break;
      case 'b': s.push_back('\b'); break;
      case 'f': s.push_back('\f'); break;
      case 'n': s.push_back('\n'); break;
      case 'r': s.push_back('\r'); break;
      case 't': s.push_back('\t'); break;
      case 'u': {
        // canonical 的转义只对 < 0x20 与引号/反斜杠出手，且一律 \u00XX；
        // 这里只处理 BMP 内的四段 hex，够读回自己写出的东西。
        if (c.end - c.p < 4) return false;
        unsigned v = 0;
        for (int i = 0; i < 4; ++i) {
          char h = *c.p++;
          v <<= 4;
          if (h >= '0' && h <= '9') v |= static_cast<unsigned>(h - '0');
          else if (h >= 'a' && h <= 'f') v |= static_cast<unsigned>(h - 'a' + 10);
          else if (h >= 'A' && h <= 'F') v |= static_cast<unsigned>(h - 'A' + 10);
          else return false;
        }
        if (v < 0x80) {
          s.push_back(static_cast<char>(v));
        } else if (v < 0x800) {
          s.push_back(static_cast<char>(0xC0 | (v >> 6)));
          s.push_back(static_cast<char>(0x80 | (v & 0x3F)));
        } else {
          s.push_back(static_cast<char>(0xE0 | (v >> 12)));
          s.push_back(static_cast<char>(0x80 | ((v >> 6) & 0x3F)));
          s.push_back(static_cast<char>(0x80 | (v & 0x3F)));
        }
        break;
      }
      default: return false;
    }
  }
  return false;
}

bool parse_number(Cursor& c, JVal* out) {
  const char* start = c.p;
  if (c.p < c.end && *c.p == '-') ++c.p;
  while (c.p < c.end && *c.p >= '0' && *c.p <= '9') ++c.p;
  // 浮点/指数：IR 里不该出现。报错而不是四舍五入 —— 静默截断会让
  // 「canonical 无浮点」这条约束在读取侧失效。
  if (c.p < c.end && (*c.p == '.' || *c.p == 'e' || *c.p == 'E')) return false;
  if (c.p == start) return false;
  std::int64_t v = 0;
  bool neg = (*start == '-');
  const char* d = neg ? start + 1 : start;
  while (d < c.p) {
    int64_t next = v * 10 + (*d - '0');
    if (next < v) return false;  // 溢出
    v = next;
    ++d;
  }
  *out = JVal::integer(neg ? -v : v);
  return true;
}

bool parse_value(Cursor& c, JVal* out, int depth) {
  if (depth > 200) return false;  // 深嵌套只会是坏输入
  skip_ws(c);
  if (c.p >= c.end) return false;
  char ch = *c.p;
  if (ch == '{') {
    ++c.p;
    JVal obj = JVal::object();
    skip_ws(c);
    if (c.p < c.end && *c.p == '}') { ++c.p; *out = std::move(obj); return true; }
    for (;;) {
      skip_ws(c);
      std::string key;
      if (!parse_string(c, &key)) return false;
      skip_ws(c);
      if (c.p >= c.end || *c.p != ':') return false;
      ++c.p;
      JVal v;
      if (!parse_value(c, &v, depth + 1)) return false;
      obj.set(key, std::move(v));
      skip_ws(c);
      if (c.p < c.end && *c.p == ',') { ++c.p; continue; }
      if (c.p < c.end && *c.p == '}') { ++c.p; break; }
      return false;
    }
    *out = std::move(obj);
    return true;
  }
  if (ch == '[') {
    ++c.p;
    JVal arr = JVal::array();
    skip_ws(c);
    if (c.p < c.end && *c.p == ']') { ++c.p; *out = std::move(arr); return true; }
    for (;;) {
      JVal v;
      if (!parse_value(c, &v, depth + 1)) return false;
      arr.push(std::move(v));
      skip_ws(c);
      if (c.p < c.end && *c.p == ',') { ++c.p; continue; }
      if (c.p < c.end && *c.p == ']') { ++c.p; break; }
      return false;
    }
    *out = std::move(arr);
    return true;
  }
  if (ch == '"') {
    std::string s;
    if (!parse_string(c, &s)) return false;
    *out = JVal::string(s);
    return true;
  }
  if (c.end - c.p >= 4 && std::memcmp(c.p, "true", 4) == 0) { c.p += 4; *out = JVal::boolean(true); return true; }
  if (c.end - c.p >= 5 && std::memcmp(c.p, "false", 5) == 0) { c.p += 5; *out = JVal::boolean(false); return true; }
  if (c.end - c.p >= 4 && std::memcmp(c.p, "null", 4) == 0) { c.p += 4; *out = JVal::null(); return true; }
  return parse_number(c, out);
}

}  // namespace

// parse_json 把文本解析成 JVal。失败返回 false，err_pos（可空）指向出错位置。
bool parse_json(const char* text, std::size_t len, JVal* out, std::size_t* err_pos) {
  Cursor c{text, text + len, false};
  JVal v;
  if (!parse_value(c, &v, 0)) {
    if (err_pos) *err_pos = static_cast<std::size_t>(c.p - text);
    return false;
  }
  skip_ws(c);
  if (c.p != c.end) {
    if (err_pos) *err_pos = static_cast<std::size_t>(c.p - text);
    return false;
  }
  *out = std::move(v);
  return true;
}

}  // namespace hm
