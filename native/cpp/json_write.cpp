#include "json_write.h"

#include <algorithm>
#include <cstdio>
#include <cstring>

namespace hm {

JVal JVal::boolean(bool b) {
  JVal v;
  v.k_ = kBool;
  v.b_ = b;
  return v;
}

JVal JVal::integer(std::int64_t i) {
  JVal v;
  v.k_ = kInt;
  v.i_ = i;
  return v;
}

JVal JVal::string(const std::string& s) {
  JVal v;
  v.k_ = kStr;
  v.s_ = s;
  return v;
}

JVal JVal::string(const char* s) { return string(std::string(s ? s : "")); }

JVal JVal::array() {
  JVal v;
  v.k_ = kArr;
  return v;
}

JVal JVal::object() {
  JVal v;
  v.k_ = kObj;
  return v;
}

JVal& JVal::push(JVal v) {
  a_.push_back(std::move(v));
  return *this;
}

JVal& JVal::set(const std::string& key, JVal v) {
  for (auto& kv : o_) {
    if (kv.first == key) {  // 后写覆盖先写，与 Go 的 map 语义一致
      kv.second = std::move(v);
      return *this;
    }
  }
  o_.emplace_back(key, std::move(v));
  return *this;
}

// write_escaped 必须与 Go 的 `encoding/json.Marshal(string)` **逐字节一致**。
//
// 这不是"差不多就行"：IR 的哈希是这几个字节的哈希，而 Go 侧的 canonical
// 实现（internal/canon）直接调用 json.Marshal。两边差一个转义写法，
// 同一份 IR 就有两个 ir_sha256，签名链当场失效——而且只在含特殊字符的
// 输入上出现，正常样例永远测不到。
//
// Go 的具体规则（encodeState.string，HTML 转义默认开启）：
//   "  → \"      \\ → \\      \n → \n   \r → \r   \t → \t
//   <0x20 → \u00XX
//   < → \u003c   > → \u003e   & → \u0026
//   U+2028 → \u2028   U+2029 → \u2029
//   其余（含全部 UTF-8 多字节）原样输出
void write_escaped(std::string& out, const std::string& s) {
  static const char* kHex = "0123456789abcdef";
  out.push_back('"');
  for (std::size_t i = 0; i < s.size(); ++i) {
    unsigned char c = static_cast<unsigned char>(s[i]);
    switch (c) {
      case '"':  out += "\\\""; break;
      case '\\': out += "\\\\"; break;
      case '\n': out += "\\n"; break;
      case '\r': out += "\\r"; break;
      case '\t': out += "\\t"; break;
      case '<':  out += "\\u003c"; break;
      case '>':  out += "\\u003e"; break;
      case '&':  out += "\\u0026"; break;
      default:
        if (c < 0x20) {
          out += "\\u00";
          out.push_back(kHex[(c >> 4) & 0xF]);
          out.push_back(kHex[c & 0xF]);
        } else if (c == 0xE2 && i + 2 < s.size() &&
                   static_cast<unsigned char>(s[i + 1]) == 0x80 &&
                   (static_cast<unsigned char>(s[i + 2]) == 0xA8 ||
                    static_cast<unsigned char>(s[i + 2]) == 0xA9)) {
          // U+2028 / U+2029：JavaScript 里是行终止符，Go 会转义它们。
          out += (static_cast<unsigned char>(s[i + 2]) == 0xA8) ? "\\u2028" : "\\u2029";
          i += 2;
        } else {
          out.push_back(static_cast<char>(c));
        }
    }
  }
  out.push_back('"');
}

void JVal::append(std::string& b) const {
  switch (k_) {
    case kNull: b += "null"; break;
    case kBool: b += b_ ? "true" : "false"; break;
    case kInt: {
      char tmp[32];
      std::snprintf(tmp, sizeof(tmp), "%lld", static_cast<long long>(i_));
      b += tmp;
      break;
    }
    case kStr: write_escaped(b, s_); break;
    case kArr:
      b.push_back('[');
      for (std::size_t i = 0; i < a_.size(); ++i) {
        if (i) b.push_back(',');
        a_[i].append(b);
      }
      b.push_back(']');
      break;
    case kObj: {
      // 键序 = 字节序。std::string 的 operator< 是字典序（按 unsigned char
      // 的字典序，libc++ 用 memcmp 语义），与 Go 的 sort.Strings 等价。
      std::vector<std::size_t> idx(o_.size());
      for (std::size_t i = 0; i < idx.size(); ++i) idx[i] = i;
      std::sort(idx.begin(), idx.end(),
                [this](std::size_t x, std::size_t y) { return o_[x].first < o_[y].first; });
      b.push_back('{');
      for (std::size_t i = 0; i < idx.size(); ++i) {
        if (i) b.push_back(',');
        write_escaped(b, o_[idx[i]].first);
        b.push_back(':');
        o_[idx[i]].second.append(b);
      }
      b.push_back('}');
      break;
    }
  }
}

std::size_t JVal::write(char* out, std::size_t cap) const {
  std::string b;
  append(b);
  if (out && cap > 0 && b.size() <= cap) {
    std::memcpy(out, b.data(), b.size());
  }
  return b.size();  // > cap 表示被截断，调用方据此扩容重试
}

std::string JVal::str() const {
  std::string b;
  append(b);
  return b;
}

}  // namespace hm
