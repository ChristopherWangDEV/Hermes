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
#include <string>
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
  KW("extern", KW_EXTERN)
  KW("fallback", KW_FALLBACK)
  KW("case", KW_CASE) KW("default", KW_DEFAULT)
  // unlimited：上界位置专用的保留字。做成保留字（而不是上下文 keyword）是
  // 有意的 —— 它不许被变量名遮蔽，见 ast.h 里 KW_UNLIMITED 的注释。
  KW("unlimited", KW_UNLIMITED)
  KW("true", KW_TRUE) KW("false", KW_FALSE)
  KW("string", TY_STRING) KW("int", TY_INT)
  KW("bool", TY_BOOL) KW("cost", TY_COST)
  // `double` 是唯一的浮点类型名（IEEE-754 binary64）；`float` 已移除，不再是
  // 关键字，也不再是 did-you-mean 的候选。token 枚举名沿用 TY_FLOAT / Tok::FLOAT，
  // 语义现在是 double —— 词法层不重写，改动只落在类型名表上。
  KW("double", TY_FLOAT)
  // char / short / long：C 风格的整数宽度类型名。当前统一按 int（int64）承载
  // —— 名字对齐 C，但宽度收窄（溢出语义）留作后续；先让声明可用，不与 int 的
  // 语义分叉。
  KW("char", TY_INT) KW("short", TY_INT) KW("long", TY_INT)
  KW("model", NS_MODEL) KW("tool", NS_TOOL) KW("json", NS_JSON) KW("input", NS_INPUT)
  KW("memory", NS_MEMORY) KW("vector", NS_VECTOR)
  KW("NetworkError", ERR_NETWORKERROR) KW("HttpError", ERR_HTTPERROR)
  KW("ModelError", ERR_MODELERROR) KW("ContractError", ERR_CONTRACTERROR)
  KW("CircuitOpenError", ERR_CIRCUITOPENERROR) KW("Error", ERR_ERROR)
  #undef KW
  return Tok::IDENT;
}

}  // namespace

// ---- M1：did-you-mean 的「期望 token 表」----
//
// 与上面 classify_ident 的 KW(...) 列表保持一致（关键字 / 类型名 / 命名空间 /
// 错误类型名）。这是 suggest_token 的可遍历候选集：把原本只活在宏展开里的
// 关键字表抽成一份静态数组，专门给编辑距离最近匹配用。
// 注意：改 classify_ident 的 KW(...) 时也要同步这里，否则 did-you-mean 会漏候选。
static const char* const kExpectedTokens[] = {
  // 关键字
  "struct", "backend", "workflow", "if", "else", "match", "while", "security",
  "allow", "deny", "with", "for", "in", "const", "auto", "break", "continue",
  "retry", "delay", "try", "catch", "return", "parallel", "export", "extern",
  "fallback", "case", "default", "unlimited", "true", "false",
  // 类型名
  "string", "int", "double", "bool", "cost", "char", "short", "long",
  // 命名空间
  "model", "tool", "json", "input", "memory", "vector",
  // 错误类型名
  "NetworkError", "HttpError", "ModelError", "ContractError",
  "CircuitOpenError", "Error",
};

// suggest_token：在 kExpectedTokens 里返回 Edit-Distance ≤ max_dist 的最近候选。
// 同距离时取第一个命中（候选表的顺序即优先级）。找不到返回 nullptr。
// edit_distance 来自 ast.h（hm 命名空间，inline），这里直接可见。
const char* suggest_token(const char* s, size_t n, int max_dist) {
  if (!s || n == 0) return nullptr;
  const char* best = nullptr;
  int best_d = max_dist + 1;  // 严格小于 max_dist+1 ⇒ 只接受 ≤ max_dist
  const size_t k = sizeof(kExpectedTokens) / sizeof(kExpectedTokens[0]);
  for (size_t i = 0; i < k; ++i) {
    const char* cand = kExpectedTokens[i];
    size_t nc = strlen(cand);
    int d = edit_distance(s, n, cand, nc);
    if (d <= max_dist && d < best_d) { best_d = d; best = cand; }
  }
  return best;
}

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
      emit_err("E1001", "unknown preprocessor directive", l,
               "Hermes only recognizes #include and #link.",
               "Remove this line, or change it to #include / #link.");
      return;
    }

    // 跳过关键字后空白
    while (pos < len && (src[pos] == ' ' || src[pos] == '\t')) advance(1);

    // 读一个带引号的字符串（含引号本身），用于 `#link native "archive" from "header"`。
    // 失败时已报过诊断。返回 arena 里的 `""...` 文本。
    auto read_quoted_string = [this]() -> const char* {
      if (pos >= len || static_cast<char>(src[pos]) != '"') {
        Loc l = {line, col, line, col};
        emit_err("E1001", "#link native expects a quoted string here", l,
                 "The #link native syntax is: #link native \"archive\" from \"header\".", nullptr);
        return nullptr;
      }
      size_t q_start = pos;
      advance(1);  // 吃掉开引号
      int s_line = line, s_col = col;
      while (pos < len && static_cast<char>(src[pos]) != '"' &&
             static_cast<char>(src[pos]) != '\n') advance(1);
      if (pos >= len || static_cast<char>(src[pos]) != '"') {
        Loc l = {s_line, s_col, line, col};
        emit_err("E1001", "#link native string is not closed", l,
                 "A quoted string in #link native must close on the same line.", nullptr);
        return nullptr;
      }
      advance(1);  // 吃掉闭引号
      size_t q_end = pos;
      return arena.intern(reinterpret_cast<const char*>(src + q_start), q_end - q_start);
    };

    // ---- #link native 形式：#link native "archive" from "header" ----
    // 在普通 `<...>`/`"..."` payload 之前判定：link 关键字后若紧跟单词 `native`，
    // 走这条分支。text 原样记录为 `native "archive" from "header"`，Golang 侧用正则解析。
    {
      size_t wpos = pos; int wline = line, wcol = col;
      size_t wstart = pos;
      while (pos < len && is_ident_cont(static_cast<char>(src[pos]))) advance(1);
      size_t wlen = pos - wstart;
      bool is_native = (wlen == 6 && memcmp(src + wstart, "native", 6) == 0);
      if (!is_native) {
        pos = wpos; line = wline; col = wcol;  // 还原，走普通 payload 分支
    } else {
      while (pos < len && (src[pos] == ' ' || src[pos] == '\t')) advance(1);
      const char* archive = read_quoted_string();
        if (!archive) return;
        // `from` 关键字
        while (pos < len && (src[pos] == ' ' || src[pos] == '\t')) advance(1);
        size_t fstart = pos;
        while (pos < len && is_ident_cont(static_cast<char>(src[pos]))) advance(1);
        size_t flen = pos - fstart;
        if (!(flen == 4 && memcmp(src + fstart, "from", 4) == 0)) {
          Loc l = {line, col, line, col};
          emit_err("E1001", "expected 'from' after the archive in #link native", l,
                   "The #link native syntax is: #link native \"archive\" from \"header\".", nullptr);
          return;
        }
        while (pos < len && (src[pos] == ' ' || src[pos] == '\t')) advance(1);
        const char* header = read_quoted_string();
        if (!header) return;
        // 可选的分号 → W2003（与 #link 一致）
        while (pos < len && (src[pos] == ' ' || src[pos] == '\t')) advance(1);
        if (pos < len && static_cast<char>(src[pos]) == ';') {
          Loc l = {line, col, line, col};
          emit_warn("W2003", "#link does not need a trailing semicolon (like #include)", l,
                    "A directive is not a statement: `#include <x>` and `#link <std/net>` both end at the line end.",
                    "Remove the trailing ';'");
          advance(1);
        }
        // text = `native "archive" from "header"`（verbatim，Golang 侧正则解析）
        std::string payload = std::string("native ") + archive + " from " + header;
        const char* full = arena.intern(payload.c_str(), payload.size());
        Token t = make_tok(Tok::LINK);
        t.text = full;
        t.sub = 1;  // quote 形式
        t.loc = {wline, wcol, line, col};
        out.push_back(t);
        return;
      }
    }

    char delim = pos < len ? static_cast<char>(src[pos]) : '\0';
    if (delim != '<' && delim != '"') {
      Loc l = {line, col, line, col};
      emit_err("E1001", is_include ? "#include expects <...> or \"...\"" : "#link expects <...> or \"...\"",
               l, "The directive payload must close on the same line.", "Write #include <std/net.h> or #include \"models/order.h\".");
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
      emit_err("E1001", is_include ? "#include is not closed" : "#link is not closed", l,
               "The payload must close on the same line.", "Check whether the closing > or \" is missing.");
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
            emit_err("E1009", "include path forbids a \".\" component", l,
                     "\".\" is the current-directory component; it is meaningless in a reproducible build and easy to misuse.", "Use a relative directory or an absolute project path.");
            break;
          }
          if (comp_len == 2 && spec[i] == '.' && spec[i + 1] == '.') {
            Loc l = {spec_line, spec_col + static_cast<int>(i), line, col};
            emit_err("E1009", "include path forbids a \"..\" component (path traversal)", l,
                     "\"..\" can escape the project root, breaking reproducibility and auditing.", "Use a relative path inside the project.");
            break;
          }
          i = (j < spec_len) ? j + 1 : j;
        }
      } else {
        // link 模块名：Ident(/Ident)*，不得带 .hbc 后缀
        size_t n = spec_len;
        if (n >= 4 && memcmp(spec + n - 4, ".hbc", 4) == 0) {
          Loc l = {spec_line, spec_col, line, col};
          emit_err("E1010", "module name is not a filename", l,
                   "A module name looks like models; the linker resolves it to models.hbc.",
                   "Change it to #link \"models\";");
          spec_len -= 4;
        } else {
          // 校验 Ident(/Ident)*
          // 简化：不允许出现 '.' 或 '/.' 或 '..'
          for (size_t i = 0; i < n; ++i) {
            if (spec[i] == '.') {
              Loc l = {spec_line, spec_col + static_cast<int>(i), line, col};
              emit_err("E1010", "invalid #link module name", l,
                       "A module name must be Ident(/Ident)*, without '.' or '..'.",
                       "Change it to #link \"models\";");
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
          emit_err("E1009", "include payload contains a NUL byte", l, nullptr, nullptr);
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
        emit_warn("W2003", "#link does not need a trailing semicolon (like #include)", l,
                  "A directive is not a statement: `#include <x>` and `#link <std/net>` both end at the line end.",
                  "Remove the trailing ';'");
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
        emit_err("E1007", "unterminated string (must be single-line)", l,
                 "An unterminated or line-broken string swallows the following code.", "Use \\n for a newline, or split the content into multiple segments.");
        flush_lit();
        return;
      }
      if (c == '\\') {
        // 转义
        if (pos + 1 >= len) {
          Loc l = {line, col, line, col};
          emit_err("E1008", "unterminated string escape", l, nullptr, nullptr);
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
            emit_err("E1008", "invalid \\u escape (expected \\u{XXXX})", l,
                     "A Unicode escape must be written as \\u{1F600}.", "Check the escape format.");
            advance(2);
            continue;
          }
          size_t hex_start = pos + 3;
          size_t hex_end = hex_start;
          while (hex_end < len && is_xdigit(static_cast<char>(src[hex_end])) &&
                 (hex_end - hex_start) < 6) hex_end++;
          if (hex_end == hex_start || src[hex_end] != '}') {
            Loc l = {line, col, line, col};
            emit_err("E1008", "invalid \\u{...} escape", l,
                     "Must be 1-6 hex digits and a valid scalar value.", "Check the \\u{...} contents.");
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
        emit_err("E1008", "unknown escape sequence", l,
                 "Valid escapes: \\\\ \\\" \\n \\r \\t \\u{XXXX}.",
                 "Remove or fix the escape.");
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
          emit_err("E1008", "interpolation must be a path (ident(.ident)*)", l,
                   "Interpolation does not allow arbitrary expressions, calls, or nesting.", "Write a plain path such as {input.text}.");
          continue;
        }
        while (pos < len && is_ident_cont(static_cast<char>(src[pos]))) advance(1);
        while (pos < len && src[pos] == '.') {
          advance(1);
          if (pos >= len || !is_ident_start(static_cast<char>(src[pos]))) {
            Loc l = {pline, pcol, line, col};
            emit_err("E1008", "invalid interpolation path", l, nullptr, nullptr);
            break;
          }
          while (pos < len && is_ident_cont(static_cast<char>(src[pos]))) advance(1);
        }
        size_t plen = pos - pstart;
        if (pos >= len || static_cast<char>(src[pos]) != '}') {
          Loc l = {pline, pcol, line, col};
          emit_err("E1008", "unterminated interpolation (expected '}')", l,
                   "Interpolation must be closed with }.", "Add the closing }: {input.text}");
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
      emit_err("E1007", "unterminated string (must be single-line)", l,
               "An unterminated or line-broken string swallows the following code.", "Use \\n for a newline.");
      flush_lit();
      return;
    }

    // 落盘 parts 到 arena 数组
    StringPart* arr = nullptr;
    if (!parts.empty()) {
      arr = static_cast<StringPart*>(
          arena.alloc(sizeof(StringPart) * parts.size()));
      if (arr == nullptr) { emit_err("E1001", "out of memory", {str_line, str_col, line, col}, nullptr, nullptr); return; }
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

  // 字符字面量 'a' → 一个 INT token（值 = 码点）。与"char 是 int 的别名"一致：
  // 解析期它就是 int 字面量，C 后端就是 hm_int。支持 \n \t \r \0 \\ \' \"。
  void lex_char() {
    Loc start = {line, col, line, col};
    size_t s = pos;
    advance(1);  // 开引号
    if (pos >= len) {
      emit_err("E1001", "unterminated char literal", start,
               "A char literal is written between single quotes, e.g. 'a'.", "Close the quote.");
      return;
    }
    int64_t cp = 0;
    char c = static_cast<char>(src[pos]);
    if (c == '\\') {
      advance(1);
      if (pos >= len) {
        emit_err("E1001", "unterminated char escape", start,
                 "A backslash must be followed by an escape character.", "Close the quote.");
        return;
      }
      char e = static_cast<char>(src[pos]);
      switch (e) {
        case 'n': cp = 10; break;
        case 't': cp = 9; break;
        case 'r': cp = 13; break;
        case '0': cp = 0; break;
        case '\\': cp = 92; break;
        case '\'': cp = 39; break;
        case '"': cp = 34; break;
        default: cp = static_cast<unsigned char>(e); break;
      }
      advance(1);
    } else {
      cp = static_cast<unsigned char>(c);
      advance(1);
    }
    if (pos >= len || static_cast<char>(src[pos]) != '\'') {
      emit_err("E1001", "a char literal must hold exactly one character", start,
               "Write a single character between single quotes, e.g. 'a'.", "Fix the literal.");
      return;
    }
    advance(1);  // 闭引号
    Token t = make_tok(Tok::INT);
    t.loc = start;
    t.loc.end_line = line;
    t.loc.end_col = col;
    t.text = arena.intern(reinterpret_cast<const char*>(src + s), pos - s);
    t.i64 = cp;
    out.push_back(t);
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
          emit_err("E1006", "amount has more than 6 decimal places (micro-USD limit)", l,
                   "cost is in micro-USD and allows at most 6 decimal places.", "Round to 6 decimal places.");
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
      emit_err("E1001", "illegal character", l, "This character is not a valid token.", "Remove or replace it.");
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
      emit_err("E1002", "source file exceeds the 8 MiB limit", l,
               "The per-file limit is 8 MiB; include expansion is counted separately.", "Split the file or trim its contents.");
    }

    while (pos < len) {
      bool line_start = skip_trivia();
      if (pos >= len) break;
      char c = static_cast<char>(src[pos]);
      if (line_start && c == '#') { lex_directive(); continue; }
      if (c == '"') { lex_string(); continue; }
      if (c == '\'') { lex_char(); continue; }
      if (c == '$') { lex_cost(); continue; }
      if (is_digit(c)) { lex_number(); continue; }
      if (c == '.') {
        // .5 非法（E1005，浮点字面量必须写成 D.D）；否则 DOT
        if (pos + 1 < len && is_digit(static_cast<char>(src[pos + 1]))) {
          Loc l = {line, col, line, col};
          emit_err("E1005", "a double literal must be written as D.D ('.5' is invalid)", l,
                   "A leading dot is confused with the '.' of field access.", "Write 0.5.");
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
          emit_err("E1001", "invalid UTF-8 byte", l,
                   "The source file must be valid UTF-8.", "Re-save it as UTF-8.");
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
      emit_err("E1003", "token count exceeds the 2,000,000 limit", l,
               "The total token count after include expansion is limited to 2,000,000.", "Split the file or trim the includes.");
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
