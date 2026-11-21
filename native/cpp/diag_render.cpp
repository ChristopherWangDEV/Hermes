// diag_render.cpp —— 纯渲染层：从 C++ 双语 catalog 产出完整人读文本。
//
// 零 IO 铁律：本文件不读 env、不碰终端、不开文件。lang 由调用方参数传入；
// 不认识的语言值回落英文（与 Go Lookup 的回落语义一致）。所有输出写调用方 buffer。
//
// 文案唯一来源是 diag_catalog.cpp；这里只做占位符替换与固定骨架拼装。
// 骨架与 Go 迁移前的 diag.Diag.Render / RenderExplain 逐字一致 ——
// 改骨架要在 tests/compiler/parser_test.cpp 的 test_diag_render_is_stable 里同步改判据。

#include "diag_render.h"

#include <cstring>

namespace hm {
namespace {

bool is_placeholder_name(const std::string& s) {
  if (s.empty()) return false;
  for (size_t i = 0; i < s.size(); ++i) {
    const unsigned char c = static_cast<unsigned char>(s[i]);
    const bool ok = c == '_' || (c >= 'a' && c <= 'z') ||
                    (i > 0 && c >= '0' && c <= '9');
    if (!ok) return false;
  }
  return true;
}

std::string itoa(int n) {
  if (n == 0) return "0";
  const bool neg = n < 0;
  unsigned int v = neg ? static_cast<unsigned int>(-(static_cast<long long>(n)))
                       : static_cast<unsigned int>(n);
  char buf[24];
  int i = 24;
  while (v > 0) {
    buf[--i] = static_cast<char>('0' + (v % 10));
    v /= 10;
  }
  if (neg) buf[--i] = '-';
  return std::string(buf + i, buf + 24);
}

const char* arg_value(const std::string& name, const hm_diag_arg* args, size_t n) {
  for (size_t i = 0; i < n; ++i) {
    if (args[i].name != nullptr && name == args[i].name) {
      return args[i].value != nullptr ? args[i].value : "";
    }
  }
  return nullptr;
}

std::string json_escape(const std::string& s) {
  std::string out;
  out.reserve(s.size() + 2);
  for (size_t i = 0; i < s.size(); ++i) {
    const unsigned char c = static_cast<unsigned char>(s[i]);
    switch (c) {
      case '"': out += "\\\""; break;
      case '\\': out += "\\\\"; break;
      case '\n': out += "\\n"; break;
      case '\r': out += "\\r"; break;
      case '\t': out += "\\t"; break;
      default:
        if (c < 0x20) {
          char tmp[8];
          int n = 0;
          tmp[n++] = '\\'; tmp[n++] = 'u'; tmp[n++] = '0'; tmp[n++] = '0';
          const char* hex = "0123456789abcdef";
          tmp[n++] = hex[(c >> 4) & 0xf]; tmp[n++] = hex[c & 0xf];
          out.append(tmp, static_cast<size_t>(n));
        } else {
          out += static_cast<char>(c);
        }
    }
  }
  return out;
}

std::string jstr(const std::string& s) { return "\"" + json_escape(s) + "\""; }

std::string severity_of_code(const char* code) {
  if (code == nullptr) return "error";
  const std::string c(code);
  if (!c.empty() && (c[0] == 'W')) return "warning";
  if (c.size() > 2 && c[0] == 'W' && c[1] == '_') return "warning";
  return "error";
}

}  // namespace

std::string diag_subst(const std::string& tmpl, const hm_diag_arg* args, size_t n_args) {
  if (tmpl.empty()) return tmpl;
  std::string b;
  b.reserve(tmpl.size());
  for (size_t i = 0; i < tmpl.size();) {
    const char c = tmpl[i];
    if (c == '{' && i + 1 < tmpl.size() && tmpl[i + 1] == '{') {
      b += '{';
      i += 2;
      continue;
    }
    if (c == '}' && i + 1 < tmpl.size() && tmpl[i + 1] == '}') {
      b += '}';
      i += 2;
      continue;
    }
    if (c != '{') {
      b += c;
      ++i;
      continue;
    }
    const size_t close = tmpl.find('}', i);
    if (close == std::string::npos) {
      b.append(tmpl, i, std::string::npos);
      break;
    }
    const std::string name = tmpl.substr(i + 1, close - i - 1);
    const char* v = arg_value(name, args, n_args);
    if (v != nullptr) {
      b += v;
    } else {
      // 缺参原样保留占位（Go subst 的同一条纪律）：吞掉会把「漏传」伪装成
      // 「文案读起来怪」，而原样保留一眼可见。
      b.append(tmpl, i, close - i + 1);
    }
    i = close + 1;
  }
  return b;
}

std::vector<std::string> diag_placeholder_names(const std::string& tmpl) {
  std::vector<std::string> out;
  for (size_t i = 0; i < tmpl.size();) {
    if (tmpl[i] != '{') { ++i; continue; }
    if (i + 1 < tmpl.size() && tmpl[i + 1] == '{') { i += 2; continue; }
    const size_t close = tmpl.find('}', i);
    if (close == std::string::npos) break;
    const std::string name = tmpl.substr(i + 1, close - i - 1);
    if (is_placeholder_name(name)) out.push_back(name);
    i = close + 1;
  }
  return out;
}

std::vector<std::string> diag_unescaped_braces(const std::string& tmpl) {
  std::vector<std::string> out;
  for (size_t i = 0; i < tmpl.size();) {
    if (tmpl[i] != '{') { ++i; continue; }
    if (i + 1 < tmpl.size() && tmpl[i + 1] == '{') { i += 2; continue; }
    const size_t close = tmpl.find('}', i);
    if (close == std::string::npos) {
      out.push_back(tmpl.substr(i));
      break;
    }
    const std::string name = tmpl.substr(i + 1, close - i - 1);
    if (!is_placeholder_name(name)) out.push_back(tmpl.substr(i, close - i + 1));
    i = close + 1;
  }
  return out;
}

std::string diag_unknown_message(const char* code) {
  return std::string("internal: no message registered for code ") + (code ? code : "(null)");
}

std::string diag_render_one(const hm_diag& d, bool zh) {
  const DiagCatalogEntry* e = diag_catalog_find(d.code, zh);
  std::string message;
  std::string why;
  std::vector<std::string> fixes;
  if (e == nullptr) {
    // 未知码必须自曝，不能静默空 —— 拼错码比文案难看更早被发现。
    message = diag_unknown_message(d.code);
    why = "The diagnostic catalog has no entry for this code. This is a bug in Hermes, not in your input.";
    fixes.push_back("report this code at the issue tracker");
  } else {
    message = diag_subst(e->text, d.args, d.n_args);
    why = diag_subst(e->why, d.args, d.n_args);
    for (int i = 0; i < 3 && e->fix[i] != nullptr; ++i) {
      fixes.push_back(diag_subst(e->fix[i], d.args, d.n_args));
    }
  }

  const std::string sevFallback = severity_of_code(d.code);
  const char* sev = (d.severity != nullptr && d.severity[0] != '\0')
                        ? d.severity : sevFallback.c_str();
  std::string b = std::string(sev) + "[" + (d.code ? d.code : "?") + "]: " + message + "\n";
  b += "  --> ";
  b += (d.module != nullptr ? d.module : "?");
  if (d.path != nullptr && d.path[0] != '\0') b += std::string("\n      at ") + d.path;
  if (d.line > 0) {
    b += "\n      line " + itoa(d.line) + ", col " + itoa(d.col);
  }
  if (d.snippet != nullptr && d.snippet[0] != '\0') b += std::string("\n\n") + d.snippet;
  b += "\n";
  if (!why.empty()) b += "   why: " + why + "\n";
  for (size_t i = 0; i < fixes.size(); ++i) b += "  help: " + fixes[i] + "\n";
  for (size_t i = 0; i < d.n_notes; ++i) {
    const hm_diag_note& n = d.notes[i];
    b += "  note: ";
    b += (n.message != nullptr ? n.message : "");
    if (n.module != nullptr && n.module[0] != '\0') {
      b += " (";
      b += n.module;
      if (n.path != nullptr && n.path[0] != '\0') b += std::string(" ") + n.path;
      if (n.line > 0) b += std::string(" line ") + itoa(n.line);
      b += ")";
    }
    b += "\n";
  }
  return b;
}

std::string diag_explain_text(const char* code, bool zh) {
  const DiagCatalogEntry* e = diag_catalog_find(code, zh);
  if (e == nullptr) return std::string();
  // Explain 的元变量逐个替换：模板写 {id}，元变量写 <id>，两者不同名，
  // 所以先建一张 name→持久 buffer 的表再交给 diag_subst。
  std::vector<std::string> names;
  std::vector<hm_diag_arg> mv;
  for (int i = 0; i < 8 && e->args[i] != nullptr; ++i) {
    names.push_back(std::string("<") + e->args[i] + ">");
  }
  mv.reserve(names.size());
  for (size_t i = 0; i < names.size(); ++i) {
    hm_diag_arg a{};
    a.name = e->args[i];
    a.value = names[i].c_str();  // names 在本函数返回前一直有效
    mv.push_back(a);
  }
  std::string b = severity_of_code(code) + "[" + (code ? code : "?") + "]: " +
                  diag_subst(e->text, mv.data(), mv.size()) + "\n";
  b += "   why: " + diag_subst(e->why, mv.data(), mv.size()) + "\n";
  for (int i = 0; i < 3 && e->fix[i] != nullptr; ++i) {
    b += "  help: " + diag_subst(e->fix[i], mv.data(), mv.size()) + "\n";
  }
  if (e->docs != nullptr && e->docs[0] != '\0') b += std::string("  docs: ") + e->docs + "\n";
  return b;
}

std::string diag_explain_json(const char* code, bool zh) {
  const DiagCatalogEntry* e = diag_catalog_find(code, zh);
  if (e == nullptr) return std::string();
  std::vector<hm_diag_arg> mv;
  std::vector<std::string> names;
  for (int i = 0; i < 8 && e->args[i] != nullptr; ++i) {
    names.push_back(std::string("<") + e->args[i] + ">");
  }
  mv.reserve(names.size());
  for (size_t i = 0; i < names.size(); ++i) {
    hm_diag_arg a{};
    a.name = e->args[i];
    a.value = names[i].c_str();  // 持久串：临时 string 的 c_str 会悬空
    mv.push_back(a);
  }
  std::string b = "{\"code\":" + jstr(code ? code : "") +
                  ",\"message\":" + jstr(diag_subst(e->text, mv.data(), mv.size())) +
                  ",\"why\":" + jstr(diag_subst(e->why, mv.data(), mv.size())) +
                  ",\"fixes\":[";
  bool first = true;
  for (int i = 0; i < 3 && e->fix[i] != nullptr; ++i) {
    if (!first) b += ",";
    first = false;
    b += jstr(diag_subst(e->fix[i], mv.data(), mv.size()));
  }
  b += "],\"docs\":" + jstr(e->docs ? e->docs : "") + "}";
  return b;
}

std::string diag_render_fields_json(const hm_diag& d, bool zh) {
  const DiagCatalogEntry* e = diag_catalog_find(d.code, zh);
  std::string message;
  std::string why;
  std::vector<std::string> fixes;
  if (e == nullptr) {
    message = diag_unknown_message(d.code);
    why = "The diagnostic catalog has no entry for this code. This is a bug in Hermes, not in your input.";
    fixes.push_back("report this code at the issue tracker");
  } else {
    message = diag_subst(e->text, d.args, d.n_args);
    why = diag_subst(e->why, d.args, d.n_args);
    for (int i = 0; i < 3 && e->fix[i] != nullptr; ++i) {
      fixes.push_back(diag_subst(e->fix[i], d.args, d.n_args));
    }
  }
  std::string b = "{\"code\":" + jstr(d.code ? d.code : "") +
                  ",\"severity\":" + jstr(d.severity ? d.severity : severity_of_code(d.code)) +
                  ",\"message\":" + jstr(message) +
                  ",\"why\":" + jstr(why) + ",\"fixes\":[";
  for (size_t i = 0; i < fixes.size(); ++i) {
    if (i) b += ",";
    b += jstr(fixes[i]);
  }
  b += "],\"text\":" + jstr(diag_render_one(d, zh)) + "}";
  return b;
}

hm_status diag_write_buffer(const std::string& s, char* out, size_t cap, size_t* out_len) {
  if (out == nullptr || cap == 0) return HM_E_INVALID_ARG;
  const size_t n = s.size() < cap - 1 ? s.size() : cap - 1;
  if (n > 0) memcpy(out, s.data(), n);
  out[n] = '\0';
  if (out_len != nullptr) *out_len = n + 1;
  return HM_OK;
}

}  // namespace hm

extern "C" {

hm_status hm_diag_render(const hm_diag* d, const char* lang,
                         char* out, size_t cap, size_t* out_len) {
  if (d == nullptr || d->code == nullptr) return HM_E_INVALID_ARG;
  const bool zh = (lang != nullptr && strcmp(lang, "zh") == 0);
  return hm::diag_write_buffer(hm::diag_render_one(*d, zh), out, cap, out_len);
}

hm_status hm_diag_render_all(const hm_diag* ds, size_t n, const char* lang,
                             char* out, size_t cap, size_t* out_len) {
  if (ds == nullptr && n > 0) return HM_E_INVALID_ARG;
  const bool zh = (lang != nullptr && strcmp(lang, "zh") == 0);
  std::string b;
  for (size_t i = 0; i < n; ++i) {
    if (i) b += "\n";
    b += hm::diag_render_one(ds[i], zh);
  }
  return hm::diag_write_buffer(b, out, cap, out_len);
}

hm_status hm_diag_explain(const char* code, const char* lang,
                          char* out, size_t cap, size_t* out_len) {
  if (code == nullptr) return HM_E_INVALID_ARG;
  const bool zh = (lang != nullptr && strcmp(lang, "zh") == 0);
  const std::string text = hm::diag_explain_text(code, zh);
  if (text.empty()) return HM_E_INVALID_ARG;
  return hm::diag_write_buffer(text, out, cap, out_len);
}

hm_status hm_diag_explain_json(const char* code, const char* lang,
                               char* out, size_t cap, size_t* out_len) {
  if (code == nullptr) return HM_E_INVALID_ARG;
  const bool zh = (lang != nullptr && strcmp(lang, "zh") == 0);
  const std::string text = hm::diag_explain_json(code, zh);
  if (text.empty()) return HM_E_INVALID_ARG;
  return hm::diag_write_buffer(text, out, cap, out_len);
}

hm_status hm_diag_render_json(const hm_diag* d, const char* lang,
                              char* out, size_t cap, size_t* out_len) {
  if (d == nullptr || d->code == nullptr) return HM_E_INVALID_ARG;
  const bool zh = (lang != nullptr && strcmp(lang, "zh") == 0);
  return hm::diag_write_buffer(hm::diag_render_fields_json(*d, zh), out, cap, out_len);
}

}  // extern "C"
