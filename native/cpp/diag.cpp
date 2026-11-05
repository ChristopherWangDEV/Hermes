// diag.cpp —— 诊断的构造、JSON 序列化、文本渲染。
//
// 硬约束：前端零 IO。本文件**不**调用 printf/fopen；所有输出写入调用方提供的字节缓冲
// （hm_parse 的 *out_diag 走的就是这条路径）。测试驱动 parser_test.cpp 自己打印，
// 那不属于本库。

#include "ast.h"
#include "arena.h"

#include <cstddef>
#include <cstdio>
#include <cstring>

namespace hm {

// 把一条 Diag 追加进 bag。Diag 结构体本身在 arena 分配（指针稳定）。
// 失败（arena 满）返回 nullptr。传进来的字符串必须在本 Parse 生命周期内稳定
// （源字节、静态字面量、或 arena-intern 过的串）。
Diag* diag_add(DiagBag& bag, Arena& a, const Diag& d) {
  if (bag.count >= bag.cap) {
    int ncap = bag.cap == 0 ? 16 : bag.cap * 2;
    Diag* na = static_cast<Diag*>(a.alloc(sizeof(Diag) * static_cast<size_t>(ncap)));
    if (na == nullptr) return nullptr;
    if (bag.items != nullptr) {
      memcpy(na, bag.items, sizeof(Diag) * static_cast<size_t>(bag.count));
    }
    bag.items = na;
    bag.cap = ncap;
  }
  bag.items[bag.count] = d;
  Diag* p = &bag.items[bag.count];
  bag.count++;
  return p;
}

// 便捷构造：错误诊断（含可选 why / fix）。
Diag* diag_error(DiagBag& bag, Arena& a, const char* code, const char* msg,
                 Loc loc, const char* module, const char* why, const char* fix) {
  Diag d;
  memset(&d, 0, sizeof(d));
  d.severity = Severity::Error;
  d.code = code;
  d.message = msg;
  d.loc = loc;
  d.module = module;
  d.why = why;
  d.fix = fix;
  return diag_add(bag, a, d);
}

// ---- 小型 JSON 写出器（无第三方依赖，只写不读）-------------------------------

namespace {

struct Buf {
  char* o;
  char* end;
  bool overflow;
};

void put_raw(Buf& b, const char* s) {
  while (*s) {
    if (b.o + 1 >= b.end) { b.overflow = true; return; }
    *b.o++ = *s++;
  }
}

void put_escaped(Buf& b, const char* s) {
  if (b.o + 1 >= b.end) { b.overflow = true; return; }
  *b.o++ = '"';
  if (s == nullptr) s = "";
  for (const char* p = s; *p; ++p) {
    unsigned char c = static_cast<unsigned char>(*p);
    if (b.o + 8 >= b.end) { b.overflow = true; break; }
    switch (c) {
      case '"': *b.o++ = '\\'; *b.o++ = '"'; break;
      case '\\': *b.o++ = '\\'; *b.o++ = '\\'; break;
      case '\n': *b.o++ = '\\'; *b.o++ = 'n'; break;
      case '\r': *b.o++ = '\\'; *b.o++ = 'r'; break;
      case '\t': *b.o++ = '\\'; *b.o++ = 't'; break;
      case '\b': *b.o++ = '\\'; *b.o++ = 'b'; break;
      case '\f': *b.o++ = '\\'; *b.o++ = 'f'; break;
      default:
        if (c < 0x20) {
          static const char* hex = "0123456789abcdef";
          *b.o++ = '\\'; *b.o++ = 'u'; *b.o++ = '0'; *b.o++ = '0';
          *b.o++ = hex[(c >> 4) & 0xf]; *b.o++ = hex[c & 0xf];
        } else {
          *b.o++ = static_cast<char>(c);
        }
    }
  }
  if (b.o < b.end) *b.o++ = '"';
}

void put_int(Buf& b, int v) {
  char tmp[16];
  int n = snprintf(tmp, sizeof(tmp), "%d", v);
  if (n < 0) return;
  for (int i = 0; i < n && b.o + 1 < b.end; ++i) *b.o++ = tmp[i];
}

}  // namespace

// 把 DiagBag 序列化为 JSON 数组，写入 out（容量 cap）。*out_len 含结尾 '\0'。
void diag_to_json(const DiagBag& bag, char* out, size_t cap, size_t* out_len) {
  if (cap == 0) { if (out_len) *out_len = 0; return; }
  Buf b{out, out + cap, false};
  put_raw(b, "[");
  for (int i = 0; i < bag.count; ++i) {
    const Diag& d = bag.items[i];
    if (i) put_raw(b, ",");
    put_raw(b, "{\"severity\":\"");
    put_raw(b, d.severity == Severity::Error ? "error"
                : (d.severity == Severity::Warning ? "warning" : "note"));
    put_raw(b, "\",\"code\":");
    put_escaped(b, d.code ? d.code : "");
    put_raw(b, ",\"message\":");
    put_escaped(b, d.message ? d.message : "");
    put_raw(b, ",\"line\":"); put_int(b, d.loc.line);
    put_raw(b, ",\"col\":"); put_int(b, d.loc.col);
    put_raw(b, ",\"end_line\":"); put_int(b, d.loc.end_line);
    put_raw(b, ",\"end_col\":"); put_int(b, d.loc.end_col);
    put_raw(b, ",\"module\":"); put_escaped(b, d.module ? d.module : "");
    put_raw(b, ",\"why\":"); put_escaped(b, d.why ? d.why : "");
    put_raw(b, ",\"fix\":["); put_escaped(b, d.fix ? d.fix : "");
    if (d.fix2) { put_raw(b, ","); put_escaped(b, d.fix2); }
    if (d.fix3) { put_raw(b, ","); put_escaped(b, d.fix3); }
    put_raw(b, "]");
    if (d.n_notes > 0 && d.notes) {
      put_raw(b, ",\"notes\":[");
      for (int k = 0; k < d.n_notes; ++k) {
        if (k) put_raw(b, ",");
        put_raw(b, "{\"module\":"); put_escaped(b, d.notes[k].module);
        put_raw(b, ",\"line\":"); put_int(b, d.notes[k].line);
        put_raw(b, ",\"col\":"); put_int(b, d.notes[k].col);
        put_raw(b, ",\"message\":"); put_escaped(b, d.notes[k].message);
        put_raw(b, "}");
      }
      put_raw(b, "]");
    }
    put_raw(b, "}");
  }
  put_raw(b, "]");
  if (b.o < b.end) *b.o = '\0';
  else { b.o[-1] = '\0'; b.overflow = true; }
  if (out_len) *out_len = static_cast<size_t>(b.o - out) + 1;
}

// 文本渲染（供测试驱动打印；不属于零 IO 库路径，但实现简单复用同一份数据）。
void diag_to_text(const DiagBag& bag, char* out, size_t cap, size_t* out_len) {
  if (cap == 0) { if (out_len) *out_len = 0; return; }
  Buf b{out, out + cap, false};
  char tmp[128];
  for (int i = 0; i < bag.count; ++i) {
    const Diag& d = bag.items[i];
    snprintf(tmp, sizeof(tmp), "%s[%s]: %s\n  --> %s L%d:%d\n",
             d.severity == Severity::Error ? "error" : "warn",
             d.code ? d.code : "?", d.message ? d.message : "",
             d.module ? d.module : "?", d.loc.line, d.loc.col);
    put_raw(b, tmp);
  }
  if (b.o < b.end) *b.o = '\0'; else b.o[-1] = '\0';
  if (out_len) *out_len = static_cast<size_t>(b.o - out) + 1;
}

}  // namespace hm
