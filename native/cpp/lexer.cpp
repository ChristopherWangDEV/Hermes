// lexer.cpp —— Hermes 词法器（NORMAL / STRING 双模式）。
//
// 硬约束：零 IO、零全局可变状态、无异常。输入是 (bytes, len, filename)，输出是一串 Token
// （写入调用方提供的 vector）加诊断。不读取文件系统、不读环境变量、不打印。
//
// 双模式要点（LANGUAGE.md §2.6 I-1..I-5）：`{` 的两种角色由词法状态决定，不存在回溯。
//   NORMAL 模式：块分隔 `{` 在此层。
//   STRING 模式：进入 `"` 后切到；`{` 只可能是插值开头；`}}` 是转义字面 `{`；
//                `}` 在插值之外是普通字符（刻意非对称）。

#include "ast.h"
#include "arena.h"

#include <cstdint>
#include <cstdlib>
#include <cstring>
#include <vector>

namespace hm {

namespace {

// 源字节分类辅助。
inline bool is_digit(char c) { return c >= '0' && c <= '9'; }
inline bool is_xdigit(char c) {
  return is_digit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F');
}
inline bool is_alpha(char c) {
  return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z');
}
inline bool is_ident_start(char c) { return is_alpha(c) || c == '_'; }
inline bool is_ident_cont(char c) { return is_alpha(c) || is_digit(c) || c == '_'; }

// UTF-8 前导字节 → 期望的后续字节数；0 表示非法前导。
int utf8_len(unsigned char b) {
  if (b < 0x80) return 1;
  if ((b & 0xE0) == 0xC0) return 2;
  if ((b & 0xF0) == 0xE0) return 3;
  if ((b & 0xF8) == 0xF0) return 4;
  return 0;
}

// 关键字 / 类型 / 命名空间 / 错误类型分类。
Tok classify_ident(const char* s, size_t n) {
  // 关键字
  #define KW(x, k) if (n == sizeof(x)-1 && memcmp(s, x, n) == 0) return Tok::k;
  KW("struct", KW_STRUCT) KW("backend", KW_BACKEND) KW("workflow", KW_WORKFLOW)
  KW("if", KW_IF) KW("else", KW_ELSE) KW("match", KW_MATCH)
  KW("while", KW_WHILE) KW("security", KW_SECURITY) KW("allow", KW_ALLOW) KW("deny", KW_DENY) KW("with", KW_WITH) KW("for", KW_FOR) KW("in", KW_IN) KW("const", KW_CONST) KW("auto", KW_AUTO)
  KW("break", KW_BREAK) KW("continue", KW_CONTINUE)
  KW("retry", KW_RETRY) KW("delay", KW_DELAY) KW("try", KW_TRY) KW("catch", KW_CATCH)
  KW("return", KW_RETURN) KW("parallel", KW_PARALLEL) KW("export", KW_EXPORT)
  KW("case", KW_CASE) KW("default", KW_DEFAULT)
  KW("true", KW_TRUE) KW("false", KW_FALSE)
  KW("string", TY_STRING) KW("int", TY_INT) KW("float", TY_FLOAT)
  KW("bool", TY_BOOL) KW("cost", TY_COST)
  KW("model", NS_MODEL) KW("tool", NS_TOOL) KW("json", NS_JSON) KW("input", NS_INPUT)
  KW("memory", NS_MEMORY) KW("vector", NS_VECTOR)
  KW("NetworkError", ERR_NETWORKERROR) KW("HttpError", ERR_HTTPERROR)
  KW("ModelError", ERR_MODELERROR) KW("ContractError", ERR_CONTRACTERROR)
  KW("CircuitOpenError", ERR_CIRCUITOPENERROR) KW("Error", ERR_ERROR)
  #undef KW
  return Tok::IDENT;
}

}  // namespace

struct Lexer {
  const uint8_t* src;
  size_t len;
  size_t pos = 0;
  int line = 1;
  int col = 1;
  const char* filename;
  Arena& arena;
  DiagBag& diags;
  std::vector<Token>& out;
  bool failed = false;  // 词法失败（已报过，语法器应停）

  Lexer(const uint8_t* s, size_t l, const char* fn, Arena& a, DiagBag& d,
        std::vector<Token>& o)
      : src(s), len(l), filename(fn), arena(a), diags(d), out(o) {}

  // emit_warn：能编，但建议改。用于"语法还接受、但已不推荐"的写法。
  void emit_warn(const char* code, const char* msg, Loc loc, const char* why,
                 const char* fix) {
    Diag dg;
    memset(&dg, 0, sizeof(dg));
    dg.severity = Severity::Warning;
    dg.code = code;
    dg.message = msg;
    dg.loc = loc;
    dg.module = filename;
    dg.why = why;
    dg.fix = fix;
    diag_add(diags, arena, dg);
  }

  void emit_err(const char* code, const char* msg, Loc loc, const char* why,
                const char* fix) {
    Diag dg;
    memset(&dg, 0, sizeof(dg));
    dg.severity = Severity::Error;
    dg.code = code;
    dg.message = msg;
    dg.loc = loc;
    dg.module = filename;
    dg.why = why;
    dg.fix = fix;
    diag_add(diags, arena, dg);
    failed = true;
  }

  void advance(int n) {
    for (int i = 0; i < n; ++i) {
      if (pos >= len) return;
      if (src[pos] == '\n') { line++; col = 1; }
      else col++;
      pos++;
    }
  }

  // 跳过空白与注释；返回当前是否在行首（用于 # 行指令判定）。
  bool skip_trivia() {
    bool line_start = (col == 1);
    for (;;) {
      if (pos >= len) return line_start;
      char c = static_cast<char>(src[pos]);
      if (c == '\n') { advance(1); line_start = true; continue; }
      if (c == ' ' || c == '\t' || c == '\r') { advance(1); continue; }
      if (c == '/' && pos + 1 < len && src[pos + 1] == '/') {
        // 行注释到行尾
        while (pos < len && src[pos] != '\n') advance(1);
        line_start = false;
        continue;
      }
      if (c == '/' && pos + 1 < len && src[pos + 1] == '*') {
        advance(2);
        while (pos < len && !(src[pos] == '*' && pos + 1 < len && src[pos + 1] == '/')) {
          if (src[pos] == '\n') line_start = true;
          advance(1);
        }
        if (pos < len) advance(2);
        line_start = false;
        continue;
      }
      return line_start;
    }
  }

  Token make_tok(Tok kind) {
    Token t;
    memset(&t, 0, sizeof(t));
    t.kind = kind;
    t.text = nullptr;
    t.loc.line = line;
    t.loc.col = col;
    return t;
  }

  // 行指令：#include / #link
  void lex_directive() {
    // pos 指向 '#'
    advance(1);  // 吃掉 '#'
    // 读关键字
    size_t kw_start = pos;
    int kw_line = line, kw_col = col;
    while (pos < len && is_alpha(static_cast<char>(src[pos]))) advance(1);
    size_t kw_len = pos - kw_start;
    bool is_include = (kw_len == 7 && memcmp(src + kw_start, "include", 7) == 0);
    bool is_link = (kw_len == 4 && memcmp(src + kw_start, "link", 4) == 0);

    if (!is_include && !is_link) {
      Loc l = {kw_line, kw_col, line, col};
      emit_err("E1001", "未知预处理指令", l,
               "Hermes 只认识 #include 与 #link。",
               "删除该行，或改为 #include / #link。");
      return;
    }

    // 跳过关键字后空白
    while (pos < len && (src[pos] == ' ' || src[pos] == '\t')) advance(1);

    char delim = pos < len ? static_cast<char>(src[pos]) : '\0';
    if (delim != '<' && delim != '"') {
      Loc l = {line, col, line, col};
      emit_err("E1001", is_include ? "#include 期望 <...> 或 \"...\"" : "#link 期望 <...> 或 \"...\"",
               l, "行指令的载荷必须同行闭合。", "写成 #include <std/net.h> 或 #include \"models/order.h\"。");
      return;
    }
    size_t delim_pos = pos;  // 含分隔符的完整载荷区间起点（用于 text）
    int mode = (delim == '<') ? 0 : 1;  // 0=angle, 1=quote
    char close = (delim == '<') ? '>' : '"';
    advance(1);  // 吃掉 delim

    size_t spec_start = pos;
    int spec_line = line, spec_col = col;
    while (pos < len && static_cast<char>(src[pos]) != close && src[pos] != '\n') advance(1);
    size_t spec_len = pos - spec_start;
    if (pos >= len || static_cast<char>(src[pos]) == '\n') {
      Loc l = {spec_line, spec_col, line, col};
      emit_err("E1001", is_include ? "#include 未闭合" : "#link 未闭合", l,
               "载荷必须在同一行内闭合。", "检查是否漏了 > 或 \"。");
      return;
    }
    advance(1);  // 吃掉 close

    // text 带分隔符（"<std/net>" / "\"models\""），便于下游与模式无关地重建载荷。
    const char* full = arena.intern(reinterpret_cast<const char*>(src + delim_pos), pos - delim_pos);

    const char* spec = arena.intern(reinterpret_cast<const char*>(src + spec_start), spec_len);

    // 路径/模块名校验
    if (mode == 1) {
      // quote 形式：include 是相对路径；link 是模块名 Ident(/Ident)*
      if (is_include) {
        // 禁止路径分量 . 或 ..
        size_t i = 0;
        while (i < spec_len) {
          size_t j = i;
          while (j < spec_len && spec[i] != '/') j++;
          size_t comp_len = j - i;
          if (comp_len == 1 && spec[i] == '.') {
            Loc l = {spec_line, spec_col + static_cast<int>(i), line, col};
            emit_err("E1009", "include 路径禁止 \".\" 分量", l,
                     "\".\" 当前目录分量在可重现构建里无意义且易被误用。", "用相对目录或绝对项目路径。");
            break;
          }
          if (comp_len == 2 && spec[i] == '.' && spec[i + 1] == '.') {
            Loc l = {spec_line, spec_col + static_cast<int>(i), line, col};
            emit_err("E1009", "include 路径禁止 \"..\" 分量（防穿越）", l,
                     "\"..\" 可逃出项目根，破坏可重现与审计。", "改为项目内的相对路径。");
            break;
          }
          i = (j < spec_len) ? j + 1 : j;
        }
      } else {
        // link 模块名：Ident(/Ident)*，不得带 .hbc 后缀
        size_t n = spec_len;
        if (n >= 4 && memcmp(spec + n - 4, ".hbc", 4) == 0) {
          Loc l = {spec_line, spec_col, line, col};
          emit_err("E1010", "模块名不是文件名", l,
                   "模块名形如 models，链接器会自行解析到 models.hbc。",
                   "改成 #link \"models\";");
          spec_len -= 4;
        } else {
          // 校验 Ident(/Ident)*
          // 简化：不允许出现 '.' 或 '/.' 或 '..'
          for (size_t i = 0; i < n; ++i) {
            if (spec[i] == '.') {
              Loc l = {spec_line, spec_col + static_cast<int>(i), line, col};
              emit_err("E1010", "link 模块名非法", l,
                       "模块名必须是 Ident(/Ident)*，不含 '.' 或 '..'。",
                       "改成 #link \"models\";");
              break;
            }
          }
        }
      }
    } else {
      // angle 形式：禁止 >、换行、NUL（已在循环里排除 > 与换行）
      for (size_t i = 0; i < spec_len; ++i) {
        if (spec[i] == '\0') {
          Loc l = {spec_line, spec_col + static_cast<int>(i), line, col};
          emit_err("E1009", "include 载荷含空字节", l, nullptr, nullptr);
          break;
        }
      }
    }

    if (is_link) {
      // `#link` 的分号**可选**：新写法是 `#link <std/net>`（不带分号），
      // 旧写法 `#link <std/net>;` 继续接受 —— 仓里有大量旧源码与文档，
      // 一次性改字符串比让它们立刻编不过要稳。
      while (pos < len && (src[pos] == ' ' || src[pos] == '\t')) advance(1);
      if (pos < len && static_cast<char>(src[pos]) == ';') {
        // 像 C++ 的 `#include <stdio.h>` 一样：指令后面不带分号。
        // 仍然接受（旧源码要能编），但发一条警告 —— 静默接受会让写法长期分裂。
        Loc l = {line, col, line, col};
        emit_warn("W2003", "#link 后面不需要分号（像 #include 一样）", l,
                  "指令不是语句：`#include <x>` 与 `#link <std/net>` 都以行尾结束。",
                  "删掉行尾的 ';'");
        advance(1);
      }
    }

    Token t = make_tok(is_include ? Tok::INCLUDE : Tok::LINK);
    t.text = full;
    t.sub = mode;
    t.loc = {spec_line, spec_col, line, col};
    t.loc.end_line = line;
    t.loc.end_col = col;
    out.push_back(t);
  }

  // 字符串扫描（含插值）。返回 true 表示成功闭合，false 表示未闭合（已报错）。
  void lex_string() {
    // pos 指向开引号 '"'
    int str_line = line, str_col = col;
    advance(1);  // 吃掉开引号

    std::vector<StringPart> parts;
    // 当前字面文本累积缓冲（遇到 { 或 " 或错误时刷出）
    char lit_buf[1024];
    size_t lit_len = 0;
    auto flush_lit = [&]() {
      if (lit_len > 0) {
        StringPart p;
        p.is_path = false;
        p.text = arena.intern(lit_buf, lit_len);
        parts.push_back(p);
        lit_len = 0;
      }
    };

    bool closed = false;
    while (pos < len) {
      char c = static_cast<char>(src[pos]);
      if (c == '"') {
        flush_lit();
        advance(1);
        closed = true;
        break;
      }
      if (c == '\n') {
        // 字符串必须单行
        Loc l = {str_line, str_col, line, col};
        emit_err("E1007", "字符串未闭合（必须单行）", l,
                 "未闭合或字面换行的字符串会吞掉后续代码。", "用 \\n 表示换行，或把内容拆到多段。");
        flush_lit();
        return;
      }
      if (c == '\\') {
        // 转义
        if (pos + 1 >= len) {
          Loc l = {line, col, line, col};
          emit_err("E1008", "字符串转义未闭合", l, nullptr, nullptr);
          flush_lit();
          return;
        }
        char e = static_cast<char>(src[pos + 1]);
        if (e == 'n') { lit_buf[lit_len++] = '\n'; advance(2); continue; }
        if (e == 'r') { lit_buf[lit_len++] = '\r'; advance(2); continue; }
        if (e == 't') { lit_buf[lit_len++] = '\t'; advance(2); continue; }
        if (e == '\\' || e == '"') { lit_buf[lit_len++] = e; advance(2); continue; }
        if (e == 'u') {
          // \u{XXXX}
          if (pos + 2 >= len || src[pos + 2] != '{') {
            Loc l = {line, col, line, col};
            emit_err("E1008", "非法转义 \\u（期望 \\u{XXXX}）", l,
                     "Unicode 转义必须写成 \\u{1F600} 形式。", "检查转义格式。");
            advance(2);
            continue;
          }
          size_t hex_start = pos + 3;
          size_t hex_end = hex_start;
          while (hex_end < len && is_xdigit(static_cast<char>(src[hex_end])) &&
                 (hex_end - hex_start) < 6) hex_end++;
          if (hex_end == hex_start || src[hex_end] != '}') {
            Loc l = {line, col, line, col};
            emit_err("E1008", "非法 \\u{...} 转义", l,
                     "必须是 1–6 位十六进制且合法标量值。", "检查 \\u{...} 内容。");
            advance(2);
            continue;
          }
          unsigned long cp = 0;
          for (size_t k = hex_start; k < hex_end; ++k) {
            char h = static_cast<char>(src[k]);
            cp = cp * 16 + (is_digit(h) ? (h - '0')
                                        : (h >= 'a' && h <= 'f') ? (h - 'a' + 10)
                                                                : (h - 'A' + 10));
          }
          // UTF-8 编码（不校验代理对合法性，仅编码）
          if (cp < 0x80) {
            lit_buf[lit_len++] = static_cast<char>(cp);
          } else if (cp < 0x800) {
            lit_buf[lit_len++] = static_cast<char>(0xC0 | (cp >> 6));
            lit_buf[lit_len++] = static_cast<char>(0x80 | (cp & 0x3F));
          } else if (cp < 0x10000) {
            lit_buf[lit_len++] = static_cast<char>(0xE0 | (cp >> 12));
            lit_buf[lit_len++] = static_cast<char>(0x80 | ((cp >> 6) & 0x3F));
            lit_buf[lit_len++] = static_cast<char>(0x80 | (cp & 0x3F));
          } else {
            lit_buf[lit_len++] = static_cast<char>(0xF0 | (cp >> 18));
            lit_buf[lit_len++] = static_cast<char>(0x80 | ((cp >> 12) & 0x3F));
            lit_buf[lit_len++] = static_cast<char>(0x80 | ((cp >> 6) & 0x3F));
            lit_buf[lit_len++] = static_cast<char>(0x80 | (cp & 0x3F));
          }
          advance(static_cast<int>(hex_end - pos + 1));
          continue;
        }
        // 未知转义
        Loc l = {line, col, line, col};
        emit_err("E1008", "未知转义序列", l,
                 "合法转义：\\\\ \\\" \\n \\r \\t \\u{XXXX}。",
                 "删除或改正转义。");
        advance(2);
        continue;
      }
      if (c == '{') {
        // 可能是插值的 { 或转义 {{
        if (pos + 1 < len && src[pos + 1] == '{') {
          lit_buf[lit_len++] = '{';  // 转义字面 {
          advance(2);
          continue;
        }
        // 插值开始：先刷出当前字面
        flush_lit();
        advance(1);  // 吃掉 {
        // 读路径 ident(.ident)*
        size_t pstart = pos;
        int pline = line, pcol = col;
        if (pos >= len || !is_ident_start(static_cast<char>(src[pos]))) {
          Loc l = {pline, pcol, line, col};
          emit_err("E1008", "插值必须是路径 (ident(.ident)*)", l,
                   "插值内不允许任意表达式、调用或嵌套。", "写成 {input.text} 这类纯路径。");
          continue;
        }
        while (pos < len && is_ident_cont(static_cast<char>(src[pos]))) advance(1);
        while (pos < len && src[pos] == '.') {
          advance(1);
          if (pos >= len || !is_ident_start(static_cast<char>(src[pos]))) {
            Loc l = {pline, pcol, line, col};
            emit_err("E1008", "插值路径非法", l, nullptr, nullptr);
            break;
          }
          while (pos < len && is_ident_cont(static_cast<char>(src[pos]))) advance(1);
        }
        size_t plen = pos - pstart;
        if (pos >= len || static_cast<char>(src[pos]) != '}') {
          Loc l = {pline, pcol, line, col};
          emit_err("E1008", "插值未闭合（期望 '}'）", l,
                   "插值必须用 } 闭合。", "补上 }：{input.text}");
          continue;
        }
        advance(1);  // 吃掉 }
        StringPart p;
        p.is_path = true;
        p.text = arena.intern(reinterpret_cast<const char*>(src + pstart), plen);
        parts.push_back(p);
        continue;
      }
      // 普通字符（含字面 }）—— 累积进字面
      lit_buf[lit_len++] = c;
      if (lit_len >= sizeof(lit_buf)) lit_len = sizeof(lit_buf) - 1;  // 防御性截断
      advance(1);
    }

    if (!closed) {
      Loc l = {str_line, str_col, line, col};
      emit_err("E1007", "字符串未闭合（必须单行）", l,
               "未闭合或字面换行的字符串会吞掉后续代码。", "用 \\n 表示换行。");
      flush_lit();
      return;
    }

    // 落盘 parts 到 arena 数组
    StringPart* arr = nullptr;
    if (!parts.empty()) {
      arr = static_cast<StringPart*>(
          arena.alloc(sizeof(StringPart) * parts.size()));
      if (arr == nullptr) { emit_err("E1001", "内存不足", {str_line, str_col, line, col}, nullptr, nullptr); return; }
      for (size_t i = 0; i < parts.size(); ++i) arr[i] = parts[i];
    }

    Token t = make_tok(Tok::STRING);
    t.loc = {str_line, str_col, line, col};
    t.parts = arr;
    t.n_parts = static_cast<int>(parts.size());
    out.push_back(t);
  }

  void lex_number() {
    Loc start = {line, col, line, col};
    size_t s = pos;
    bool is_float = false;
    // 整数/十六进制部分
    if (src[pos] == '0' && pos + 1 < len &&
        (src[pos + 1] == 'x' || src[pos + 1] == 'X')) {
      advance(2);
      while (pos < len && (is_xdigit(static_cast<char>(src[pos])) || src[pos] == '_')) advance(1);
      // 校验至少有一个十六进制数字
      Token t = make_tok(Tok::INT);
      t.loc = start;
      t.loc.end_line = line; t.loc.end_col = col;
      t.text = arena.intern(reinterpret_cast<const char*>(src + s), pos - s);
      // 解析值
      unsigned long long v = 0;
      const char* p = reinterpret_cast<const char*>(src + s + 2);
      for (; *p && *p != ' '; ++p) {
        if (*p == '_') continue;
        if (!is_xdigit(*p)) break;
        v = v * 16 + (is_digit(*p) ? (*p - '0') : (*p >= 'a') ? (*p - 'a' + 10) : (*p - 'A' + 10));
      }
      t.i64 = static_cast<int64_t>(v);
      out.push_back(t);
      return;
    }
    while (pos < len && (is_digit(static_cast<char>(src[pos])) || src[pos] == '_')) advance(1);
    // 小数部分
    if (pos < len && src[pos] == '.' &&
        pos + 1 < len && is_digit(static_cast<char>(src[pos + 1]))) {
      is_float = true;
      advance(1);  // '.'
      while (pos < len && (is_digit(static_cast<char>(src[pos])) || src[pos] == '_')) advance(1);
    }
    // 指数部分
    if (pos < len && (src[pos] == 'e' || src[pos] == 'E')) {
      size_t save = pos;
      advance(1);
      if (pos < len && (src[pos] == '+' || src[pos] == '-')) advance(1);
      if (pos < len && is_digit(static_cast<char>(src[pos]))) {
        is_float = true;
        while (pos < len && (is_digit(static_cast<char>(src[pos])) || src[pos] == '_')) advance(1);
      } else {
        pos = save;  // 不是指数，'e' 留给后续当 ident
      }
    }
    if (is_float) {
      Token t = make_tok(Tok::FLOAT);
      t.loc = start; t.loc.end_line = line; t.loc.end_col = col;
      t.text = arena.intern(reinterpret_cast<const char*>(src + s), pos - s);
      t.f64 = strtod(t.text, nullptr);
      out.push_back(t);
    } else {
      Token t = make_tok(Tok::INT);
      t.loc = start; t.loc.end_line = line; t.loc.end_col = col;
      t.text = arena.intern(reinterpret_cast<const char*>(src + s), pos - s);
      unsigned long long v = 0;
      for (size_t k = s; k < pos; ++k) {
        char c = static_cast<char>(src[k]);
        if (c == '_') continue;
        v = v * 10 + static_cast<unsigned long long>(c - '0');
      }
      t.i64 = static_cast<int64_t>(v);
      out.push_back(t);
    }
  }

  void lex_cost() {
    // src[pos] == '$'
    Loc start = {line, col, line, col};
    size_t s = pos;
    advance(1);  // '$'
    // 整数部分
    while (pos < len && (is_digit(static_cast<char>(src[pos])) || src[pos] == '_')) advance(1);
    if (pos < len && src[pos] == '.') {
      advance(1);
      int frac = 0;
      while (pos < len && is_digit(static_cast<char>(src[pos]))) {
        advance(1); frac++;
        if (frac > 6) {
          Loc l = {line, col, line, col};
          emit_err("E1006", "金额小数位超过 6 位（micro-USD 上限）", l,
                   "cost 以 micro-USD 计，最多 6 位小数。", "四舍五入到 6 位小数。");
          break;
        }
      }
    }
    Token t = make_tok(Tok::COST_LIT);
    t.loc = start; t.loc.end_line = line; t.loc.end_col = col;
    t.text = arena.intern(reinterpret_cast<const char*>(src + s), pos - s);
    // 计算 micro-USD
    const char* p = reinterpret_cast<const char*>(src + s + 1);
    long long dollars = 0, cents = 0, div = 1000000;
    bool in_frac = false;
    for (; *p && *p != ' ' && *p != '\n' && *p != '\r' && *p != '\t' && *p != ';'; ++p) {
      if (*p == '_') continue;
      if (*p == '.') { in_frac = true; continue; }
      if (!is_digit(*p)) break;
      if (!in_frac) dollars = dollars * 10 + (*p - '0');
      else { cents = cents * 10 + (*p - '0'); div /= 10; }
    }
    t.i64 = dollars * 1000000 + cents * div;
    out.push_back(t);
  }

  void lex_ident() {
    Loc start = {line, col, line, col};
    size_t s = pos;
    while (pos < len && is_ident_cont(static_cast<char>(src[pos]))) advance(1);
    size_t n = pos - s;
    Tok k = classify_ident(reinterpret_cast<const char*>(src + s), n);
    Token t = make_tok(k);
    t.loc = start; t.loc.end_line = line; t.loc.end_col = col;
    t.text = arena.intern(reinterpret_cast<const char*>(src + s), n);
    out.push_back(t);
  }

  void lex_operator() {
    Loc start = {line, col, line, col};
    char c = static_cast<char>(src[pos]);
    char c2 = (pos + 1 < len) ? static_cast<char>(src[pos + 1]) : '\0';
    Tok kind = Tok::ERROR;
    int consume = 1;
    switch (c) {
      case '{': kind = Tok::LBRACE; break;
      case '}': kind = Tok::RBRACE; break;
      case '(': kind = Tok::LPAREN; break;
      case ')': kind = Tok::RPAREN; break;
      case '[': kind = Tok::LBRACKET; break;
      case ']': kind = Tok::RBRACKET; break;
      case ';': kind = Tok::SEMI; break;
      case ',': kind = Tok::COMMA; break;
      case '.': kind = Tok::DOT; break;
      case ':': kind = Tok::COLON; break;
      case '@': kind = Tok::AT; break;
      case '?': kind = Tok::QUESTION; break;
      case '+':
        if (c2 == '+') { kind = Tok::INC; consume = 2; }
        else if (c2 == '=') { kind = Tok::PLUS_EQ; consume = 2; }
        else kind = Tok::PLUS;
        break;
      case '-':
        if (c2 == '>') { kind = Tok::ARROW; consume = 2; }
        else if (c2 == '-') { kind = Tok::DEC; consume = 2; }
        else if (c2 == '=') { kind = Tok::MINUS_EQ; consume = 2; }
        else kind = Tok::MINUS;
        break;
      case '*': kind = Tok::STAR; break;
      case '/': kind = Tok::SLASH; break;
      case '%': kind = Tok::PERCENT; break;
      case '!':
        if (c2 == '=') { kind = Tok::NE; consume = 2; } else kind = Tok::BANG;
        break;
      case '=':
        if (c2 == '=') { kind = Tok::EQEQ; consume = 2; } else kind = Tok::EQ;
        break;
      case '<':
        if (c2 == '=') { kind = Tok::LE; consume = 2; } else kind = Tok::LT;
        break;
      case '>':
        if (c2 == '=') { kind = Tok::GE; consume = 2; } else kind = Tok::GT;
        break;
      case '&':
        if (c2 == '&') { kind = Tok::ANDAND; consume = 2; } else kind = Tok::ERROR;
        break;
      case '|':
        if (c2 == '|') { kind = Tok::OROR; consume = 2; } else kind = Tok::ERROR;
        break;
      default: kind = Tok::ERROR; break;
    }
    if (kind == Tok::ERROR) {
      Loc l = {line, col, line, col};
      emit_err("E1001", "非法字符", l, "该字符不是合法 token。", "删除或替换它。");
      advance(1);
      return;
    }
    Token t = make_tok(kind);
    t.loc = start; t.loc.end_line = line; t.loc.end_col = col;
    advance(consume);
    out.push_back(t);
  }

  void run() {
    // BOM 剥除
    if (len >= 3 && src[0] == 0xEF && src[1] == 0xBB && src[2] == 0xBF) advance(3);

    // E1002：单文件 8 MiB 上限
    if (len > 8u * 1024u * 1024u) {
      Loc l = {1, 1, 1, 1};
      emit_err("E1002", "源文件超过 8 MiB 上限", l,
               "单文件上限 8 MiB，include 展开另计。", "拆分文件或精简内容。");
    }

    while (pos < len) {
      bool line_start = skip_trivia();
      if (pos >= len) break;
      char c = static_cast<char>(src[pos]);
      if (line_start && c == '#') { lex_directive(); continue; }
      if (c == '"') { lex_string(); continue; }
      if (c == '$') { lex_cost(); continue; }
      if (is_digit(c)) { lex_number(); continue; }
      if (c == '.') {
        // .5 非法（E1005）；否则 DOT
        if (pos + 1 < len && is_digit(static_cast<char>(src[pos + 1]))) {
          Loc l = {line, col, line, col};
          emit_err("E1005", "浮点必须写成 D.D（'.5' 非法）", l,
                   "前导点会与字段访问的 '.' 混淆。", "写成 0.5。");
          advance(1);
          continue;
        }
        lex_operator();
        continue;
      }
      if (is_ident_start(c)) { lex_ident(); continue; }
      if (static_cast<unsigned char>(c) >= 0x80) {
        int ul = utf8_len(static_cast<unsigned char>(c));
        if (ul == 0) {
          Loc l = {line, col, line, col};
          emit_err("E1001", "非法 UTF-8 字节", l,
                   "源文件必须是合法 UTF-8。", "用 UTF-8 重新保存。");
          advance(1);
          continue;
        }
        advance(ul);
        continue;
      }
      lex_operator();
    }

    // E1003：include 展开后总 token 数上限 2,000,000
    if (out.size() > 2000000u) {
      Loc l = {1, 1, 1, 1};
      emit_err("E1003", "token 数超过 2,000,000 上限", l,
               "include 展开后总 token 数上限 2,000,000。", "拆分文件或精简 include。");
    }

    // EOF
    Token eof = make_tok(Tok::EOF_);
    eof.loc = {line, col, line, col};
    out.push_back(eof);
  }
};

void lex(const uint8_t* src, size_t len, const char* filename, Arena& arena,
         DiagBag& diags, std::vector<Token>& out) {
  Lexer lexer(src, len, filename, arena, diags, out);
  lexer.run();
}

}  // namespace hm
