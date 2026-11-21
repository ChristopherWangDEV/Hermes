// lsp.cpp —— 语言服务器的 C++ 实现（阶段：协议层 + 诊断）。
//
// 为什么搬：`internal/lsp` 只依赖 `internal/native`（cgo），也就是说它**全部
// 能力都来自 C++ 前端**。搬到 C++ 后不再需要 cgo，也不再有第二份协议实现。
//
// 判据：cmd/hermes/lsp_golden_test.go 把 Go 版的脚本化会话输出钉成
// tests/lsp/golden/session.jsonl；本实现必须**逐帧相同**才算迁移成功。
//
// 本文件自带 main（-DHM_LSP_MAIN），构建产物是独立的 hermes-lsp；CLI 之后
// 只需转发给它，不需要链 Go。
#include "hermes_parser.h"
#include "json_write.h"

#include <cstdio>
#include <cstring>
#include <map>
#include <string>
#include <vector>

namespace hm {
namespace {

std::string read_header(FILE* in) {
  std::string line;
  int c;
  while ((c = fgetc(in)) != EOF) {
    if (c == '\n') {
      if (!line.empty() && line.back() == '\r') line.pop_back();
      return line;
    }
    line.push_back(static_cast<char>(c));
  }
  return line.empty() ? std::string() : line;
}

// read_frame 读一帧；返回 false 表示 EOF。
bool read_frame(FILE* in, std::string* body) {
  size_t len = 0;
  bool saw_header = false;
  while (true) {
    const std::string line = read_header(in);
    if (line.empty()) {
      if (!saw_header) return false;  // 空行且没有头 = 正常结束
      break;
    }
    saw_header = true;
    const std::string key = "Content-Length:";
    if (line.compare(0, key.size(), key) == 0) {
      len = std::strtoul(line.c_str() + key.size(), nullptr, 10);
    }
  }
  body->assign(len, '\0');
  if (len > 0 && fread(&(*body)[0], 1, len, in) != len) return false;
  return true;
}

void write_frame(FILE* out, const std::string& body) {
  std::fprintf(out, "Content-Length: %zu\r\n\r\n", body.size());
  std::fwrite(body.data(), 1, body.size(), out);
  std::fflush(out);
}

// ---- 能力声明 --------------------------------------------------------------
// 与 Go 版逐字对应（semanticLegend 的 tokenTypes 必须与 extension.js 一致）。
JVal capabilities() {
  JVal caps = JVal::object();
  caps.set("textDocumentSync", JVal::integer(1));

  JVal completion = JVal::object();
  JVal triggers = JVal::array();
  triggers.push(JVal::string("."));
  triggers.push(JVal::string("<"));
  triggers.push(JVal::string("\""));
  completion.set("triggerCharacters", std::move(triggers));
  caps.set("completionProvider", std::move(completion));

  caps.set("hoverProvider", JVal::boolean(true));
  caps.set("documentSymbolProvider", JVal::boolean(true));

  JVal semantic = JVal::object();
  semantic.set("full", JVal::boolean(true));
  JVal legend = JVal::object();
  JVal types = JVal::array();
  for (const char* t : {"variable", "function", "parameter", "type", "property",
                        "keyword", "number", "string"}) {
    types.push(JVal::string(t));
  }
  legend.set("tokenTypes", std::move(types));
  legend.set("tokenModifiers", JVal::array());
  semantic.set("legend", std::move(legend));
  caps.set("semanticTokensProvider", std::move(semantic));
  return caps;
}

int severity_of(const char* s) {
  if (s == nullptr) return 4;
  if (std::strcmp(s, "error") == 0) return 1;
  if (std::strcmp(s, "warning") == 0) return 2;
  if (std::strcmp(s, "info") == 0) return 3;
  return 4;
}

int64_t max0(int64_t v) { return v > 0 ? v - 1 : 0; }

// render_message 与 Go 的 renderDiag 同形：message + why + 每条 fix 前缀 "• "。
std::string render_message(const JVal* d) {
  std::string out;
  const JVal* m = d ? d->find("message") : nullptr;
  if (m != nullptr && m->kind() == JVal::kStr) out += m->as_str();
  const JVal* w = d->find("why");
  if (w != nullptr && w->kind() == JVal::kStr && !w->as_str().empty()) {
    out += "\n\n";
    out += w->as_str();
  }
  const JVal* fixes = d->find("fix");
  if (fixes != nullptr && fixes->kind() == JVal::kArr) {
    for (size_t i = 0; i < fixes->size(); ++i) {
      const JVal* f = fixes->at(i);
      if (f != nullptr && f->kind() == JVal::kStr && !f->as_str().empty()) {
        out += "\n\xe2\x80\xa2 ";  // "• "
        out += f->as_str();
      }
    }
  }
  return out;
}

}  // namespace
}  // namespace hm


namespace hm {
namespace {

struct Server {
  std::map<std::string, std::string> docs;  // uri -> text
  bool shutdown = false;
};

// reply 发一条响应：{"jsonrpc":"2.0","id":<id>,"result":<result>}
void reply(FILE* out, const std::string& id_json, const std::string& result_json) {
  std::string body = "{\"jsonrpc\":\"2.0\",\"id\":" + id_json + ",\"result\":" + result_json + "}";
  write_frame(out, body);
}

// notify 发一条通知：{"jsonrpc":"2.0","method":"...","params":{...}}
void notify(FILE* out, const char* method, const std::string& params_json) {
  std::string body = std::string("{\"jsonrpc\":\"2.0\",\"method\":\"") + method +
                     "\",\"params\":" + params_json + "}";
  write_frame(out, body);
}

// diagnostics_for 调前端拿诊断，产出 LSP 的 diagnostics 数组（canonical JSON）。
std::string diagnostics_for(const std::string& text, const std::string& uri) {
  const bool is_header = uri.size() >= 2 && uri.compare(uri.size() - 2, 2, ".h") == 0;
  std::vector<char> dump(64 * 1024), diag(64 * 1024);
  size_t dlen = 0, glen = 0;
  int nodes = 0;
  for (int attempt = 0; attempt < 3; ++attempt) {
    const int rc = hm_parse(reinterpret_cast<const uint8_t*>(text.data()), text.size(),
                            uri.c_str(), is_header ? 1 : 0, dump.data(), dump.size(), &dlen,
                            diag.data(), diag.size(), &glen, &nodes);
    if (rc == 0) break;
    if (dlen > dump.size()) { dump.resize(dlen); continue; }
    if (glen > diag.size()) { diag.resize(glen); continue; }
    return "[]";
  }
  JVal arr;
  size_t pos = 0;
  // diag_to_json 回填的长度**含结尾 NUL**；不剥掉会让 parse_json 失败，
  // 表现成"永远没有诊断"——一个只有对着 golden 才能发现的静默错误。
  if (glen > 0 && diag[glen - 1] == 0) --glen;
  if (glen == 0 || !parse_json(diag.data(), glen, &arr, &pos)) return "[]";
  if (arr.kind() != JVal::kArr) return "[]";

  JVal out = JVal::array();
  for (size_t i = 0; i < arr.size(); ++i) {
    const JVal* d = arr.at(i);
    if (d == nullptr || d->kind() != JVal::kObj) continue;
    JVal item = JVal::object();
    const JVal* code = d->find("code");
    const JVal* sev = d->find("severity");
    JVal range = JVal::object();
    JVal start = JVal::object();
    JVal end = JVal::object();
    const JVal* line = d->find("line");
    const JVal* col = d->find("col");
    const JVal* eline = d->find("end_line");
    const JVal* ecol = d->find("end_col");
    start.set("line", JVal::integer(max0(line ? line->as_int(0) : 0)));
    start.set("character", JVal::integer(max0(col ? col->as_int(0) : 0)));
    end.set("line", JVal::integer(max0(eline ? eline->as_int(0) : 0)));
    end.set("character", JVal::integer(max0(ecol ? ecol->as_int(0) : 0)));
    range.set("start", std::move(start));
    range.set("end", std::move(end));
    item.set("range", std::move(range));
    item.set("severity", JVal::integer(severity_of(sev && sev->kind() == JVal::kStr ? sev->as_str().c_str() : nullptr)));
    item.set("source", JVal::string("hermes"));
    if (code != nullptr) item.set("code", *code);
    item.set("message", JVal::string(render_message(d)));
    out.push(std::move(item));
  }
  return out.str();
}

void publish(FILE* out, Server* s, const std::string& uri) {
  auto it = s->docs.find(uri);
  if (it == s->docs.end()) return;
  std::string params = "{\"diagnostics\":" + diagnostics_for(it->second, uri) +
                       ",\"uri\":" + JVal::string(uri).str() + "}";
  notify(out, "textDocument/publishDiagnostics", params);
}

}  // namespace
}  // namespace hm


namespace hm {
// completion：关键字 + 内建命名空间（纯表驱动）。
// 表必须与 Go 版逐项一致——补全给错候选会让用户写出编不过的代码。
std::string completion_result() {
  static const char* kKeywords[] = {
      "export", "workflow", "struct", "backend", "entry", "const", "auto", "return",
      "if", "else", "match", "retry", "try", "catch", "parallel", "true", "false",
      "input", "string", "int", "double", "bool", "json", "cost",
  };
  struct Builtin { const char* name; const char* doc; };
  static const Builtin kBuiltins[] = {
      {"model.generate", "Call the model. Requires `#link <std/model>`. The return type is given by `<T>`."},
      {"tool.call", "Call an HTTP tool. Requires `#link <std/net>`. allow_status can make certain status codes a normal return."},
      {"json.as_int", "Narrowing: on failure it throws ContractError (with expect/got); it never returns a zero value."},
      {"json.as_string", "Narrowing: on failure it throws ContractError."},
      {"json.as_bool", "Narrowing: on failure it throws ContractError."},
      {"json.as_float", "Narrowing to `double` (IEEE-754 binary64); the API name `json.as_float` is kept. IR constants use a 16-hex-digit bit pattern, never a JSON number."},
  };
  JVal items = JVal::array();
  for (const char* k : kKeywords) {
    JVal it = JVal::object();
    it.set("label", JVal::string(k));
    it.set("kind", JVal::integer(14));  // 14 = Keyword
    items.push(std::move(it));
  }
  // builtins 按名字**字典序**（Go 侧 sort.Strings），表已按此顺序写死；
  // 但仍排序一次，避免有人插错位置后两侧悄悄不同。
  std::vector<std::pair<std::string, const char*>> bs;
  for (const Builtin& b : kBuiltins) bs.emplace_back(b.name, b.doc);
  std::sort(bs.begin(), bs.end(),
            [](const auto& a, const auto& b) { return a.first < b.first; });
  for (const auto& b : bs) {
    JVal it = JVal::object();
    it.set("label", JVal::string(b.first));
    it.set("kind", JVal::integer(3));  // 3 = Function
    it.set("detail", JVal::string("builtin"));
    it.set("documentation", JVal::string(b.second));
    items.push(std::move(it));
  }
  JVal result = JVal::object();
  result.set("isIncomplete", JVal::boolean(false));
  result.set("items", std::move(items));
  return result.str();
}
}  // namespace hm


namespace hm {
// ---- hover -----------------------------------------------------------------
// 三种来源按可靠性排序：内建（表里有）→ 关键字 → 用户 struct（AST dump）。
// 前两种已移植；struct 需要解析 AST dump，尚未移植 —— 那种情况回 null，
// 与 Go 版"找不到就回 null"同形，但**能力上是缺口**（见 KNOWN-GAPS）。

bool is_ident_char(char c) {
  return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
         (c >= '0' && c <= '9');
}

// word_at 与 Go 版逐字对应：光标在词尾时向左看一个字符，否则行末永远取不到词。
std::string word_at(const std::string& text, int line_no, int character) {
  std::vector<std::string> lines;
  std::string cur;
  for (char c : text) {
    if (c == '\n') { lines.push_back(cur); cur.clear(); } else { cur.push_back(c); }
  }
  lines.push_back(cur);
  if (line_no < 0 || line_no >= static_cast<int>(lines.size())) return "";
  const std::string& line = lines[line_no];
  if (line.empty()) return "";
  int c = character;
  if (c > static_cast<int>(line.size())) c = static_cast<int>(line.size());
  if (c == static_cast<int>(line.size()) || !is_ident_char(line[c])) {
    if (c > 0 && is_ident_char(line[c - 1])) --c;
  }
  int start = c, end = c;
  while (start > 0 && is_ident_char(line[start - 1])) --start;
  while (end < static_cast<int>(line.size()) && is_ident_char(line[end])) ++end;
  return line.substr(start, end - start);
}

// dotted_at 取光标处的 `ns.member`（与 Go 版同一套走法）。
//
// 两个方向都要处理：光标可能在成员上（点号在左，`json.as_int` 的 int），
// 也可能在命名空间上（点号在右）。只做左向会让"光标停在 json 上"退回关键字说明。
bool dotted_at(const std::string& text, int line_no, int character, std::string* out) {
  std::vector<std::string> lines;
  std::string cur;
  for (char c : text) {
    if (c == '\n') { lines.push_back(cur); cur.clear(); } else { cur.push_back(c); }
  }
  lines.push_back(cur);
  if (line_no < 0 || line_no >= static_cast<int>(lines.size())) return false;
  const std::string& line = lines[line_no];
  const int n = static_cast<int>(line.size());
  int c = character;
  if (c > n) c = n;
  // 把光标落在词上：光标在词尾时向左看一格。
  if (c == n || !is_ident_char(line[c])) {
    if (c > 0 && is_ident_char(line[c - 1])) --c;
  }
  if (c >= n || !is_ident_char(line[c])) return false;
  int start = c, end = c;
  while (start > 0 && is_ident_char(line[start - 1])) --start;
  while (end < n && is_ident_char(line[end])) ++end;

  // 右边是点号：当前词是命名空间。
  if (end < n && line[end] == '.') {
    int ms = end + 1, me = ms;
    while (me < n && is_ident_char(line[me])) ++me;
    if (me > ms) {
      *out = line.substr(start, end - start) + "." + line.substr(ms, me - ms);
      return true;
    }
  }
  // 左边是点号：当前词是成员，往左取命名空间。
  if (start > 1 && line[start - 1] == '.') {
    const int ne = start - 1;
    int ns = ne;
    while (ns > 0 && is_ident_char(line[ns - 1])) --ns;
    if (ne > ns) {
      *out = line.substr(ns, ne - ns) + "." + line.substr(start, end - start);
      return true;
    }
  }
  return false;
}

const char* keyword_doc(const std::string& w) {
  if (w == "export") return "`export` — makes the symbol appear in `exports` so other units can use it.";
  if (w == "entry") return "`entry` — this workflow is the unit's entry; `hermes run` / `serve` select it by default.";
  if (w == "parallel") return "`parallel` — runs branches concurrently, throttled by `max_par`; branches **do not share writable state**.";
  if (w == "retry") return "`retry` — retries. Only applies to **retryable** errors (retrying a contract error is usually still wrong).";
  if (w == "try") return "`try`/`catch` — catches the language's 6 error types; the body may run partly before throwing.";
  if (w == "json") return "`json` — an unconstrained JSON value. Use `json.as_*` narrowing to extract a concrete type.";
  if (w == "cost") return "`cost` — an amount, always `int64` micro_usd (no implicit promotion to `double`; IR constants use bit patterns, never JSON float numbers).";
  return nullptr;
}

const char* builtin_doc(const std::string& w) {
  static const struct { const char* name; const char* doc; } kBuiltins[] = {
      {"model.generate", "Call the model. Requires `#link <std/model>`. The return type is given by `<T>`."},
      {"tool.call", "Call an HTTP tool. Requires `#link <std/net>`. allow_status can make certain status codes a normal return."},
      {"json.as_int", "Narrowing: on failure it throws ContractError (with expect/got); it never returns a zero value."},
      {"json.as_string", "Narrowing: on failure it throws ContractError."},
      {"json.as_bool", "Narrowing: on failure it throws ContractError."},
      {"json.as_float", "Narrowing to `double` (IEEE-754 binary64); the API name `json.as_float` is kept. IR constants use a 16-hex-digit bit pattern, never a JSON number."},
  };
  for (const auto& b : kBuiltins) {
    if (w == b.name) return b.doc;
  }
  return nullptr;
}

bool is_keyword(const std::string& w) {
  static const char* kKeywords[] = {
      "export", "workflow", "struct", "backend", "entry", "const", "auto", "return",
      "if", "else", "match", "retry", "try", "catch", "parallel", "true", "false",
      "input", "string", "int", "double", "bool", "json", "cost",
  };
  for (const char* k : kKeywords) {
    if (w == k) return true;
  }
  return false;
}

std::string hover_text(const std::string& doc) {
  JVal contents = JVal::object();
  contents.set("kind", JVal::string("markdown"));
  contents.set("value", JVal::string(doc));
  JVal result = JVal::object();
  result.set("contents", std::move(contents));
  return result.str();
}

}  // namespace hm


namespace hm {
// hm_parse_dump 只取 AST dump（`hm_parse` 同时给 dump 与 diag，这里只要 dump）。
bool hm_parse_dump(const std::string& text, const std::string& uri, std::string* out) {
  const bool is_header = uri.size() >= 2 && uri.compare(uri.size() - 2, 2, ".h") == 0;
  std::vector<char> dump(64 * 1024), diag(8 * 1024);
  size_t dlen = 0, glen = 0;
  int nodes = 0;
  for (int attempt = 0; attempt < 3; ++attempt) {
    const int rc = hm_parse(reinterpret_cast<const uint8_t*>(text.data()), text.size(),
                            uri.c_str(), is_header ? 1 : 0, dump.data(), dump.size(), &dlen,
                            diag.data(), diag.size(), &glen, &nodes);
    if (rc == 0) break;
    if (dlen > dump.size()) { dump.resize(dlen); continue; }
    if (glen > diag.size()) { diag.resize(glen); continue; }
    return false;
  }
  if (dlen > 0 && dump[dlen - 1] == 0) --dlen;
  out->assign(dump.data(), dlen);
  return true;
}

// quoted 与 Go 版同义：dump 行里**最后一个引号对**的内容（名字）。
std::string quoted_in(const std::string& line) {
  const size_t end = line.rfind('"');
  if (end == std::string::npos || end == 0) return "";
  const size_t start = line.rfind('"', end - 1);
  if (start == std::string::npos) return "";
  return line.substr(start + 1, end - start - 1);
}

JVal zero_range() {
  JVal start = JVal::object();
  start.set("line", JVal::integer(0));
  start.set("character", JVal::integer(0));
  JVal end = JVal::object();
  end.set("line", JVal::integer(0));
  end.set("character", JVal::integer(0));
  JVal r = JVal::object();
  r.set("start", std::move(start));
  r.set("end", std::move(end));
  return r;
}

// dump_symbols：从 AST dump 里挑 workflow / struct / backend 三类顶层符号。
// 与 Go 版同一套判据（子串匹配 + 最后一对引号里的名字），因此两侧顺序一致。
std::string dump_symbols(const std::string& dump) {
  JVal syms = JVal::array();
  size_t pos = 0;
  while (pos <= dump.size()) {
    size_t nl = dump.find('\n', pos);
    const std::string line = dump.substr(pos, (nl == std::string::npos ? dump.size() : nl) - pos);
    const char* kind = nullptr;
    int lsp_kind = 0;
    if (line.find("WorkflowDecl ") != std::string::npos) { kind = "workflow"; lsp_kind = 12; }
    else if (line.find("StructDecl ") != std::string::npos) { kind = "struct"; lsp_kind = 23; }
    else if (line.find("BackendDef ") != std::string::npos ||
             line.find("BackendSig ") != std::string::npos) { kind = "backend"; lsp_kind = 13; }
    if (kind != nullptr) {
      const std::string name = quoted_in(line);
      if (!name.empty()) {
        JVal sym = JVal::object();
        sym.set("name", JVal::string(name));
        sym.set("detail", JVal::string(kind));
        sym.set("kind", JVal::integer(lsp_kind));
        sym.set("range", zero_range());
        sym.set("selectionRange", zero_range());
        syms.push(std::move(sym));
      }
    }
    if (nl == std::string::npos) break;
    pos = nl + 1;
  }
  return syms.str();
}

// struct_doc：hover 的第三来源（用户 struct）。与 Go 版同样扫 dump 的缩进块。
bool struct_doc(const std::string& dump, const std::string& name, std::string* out) {
  std::vector<std::string> lines;
  std::string cur;
  for (char c : dump) {
    if (c == '\n') { lines.push_back(cur); cur.clear(); } else { cur.push_back(c); }
  }
  if (!cur.empty()) lines.push_back(cur);
  auto indent_of = [](const std::string& l) {
    size_t i = 0;
    while (i < l.size() && l[i] == ' ') ++i;
    return static_cast<int>(i);
  };
  for (size_t i = 0; i < lines.size(); ++i) {
    const std::string& line = lines[i];
    if (line.find("StructDecl") == std::string::npos) continue;
    // 行尾必须是 "name"
    const std::string suffix = "\"" + name + "\"";
    std::string t = line;
    while (!t.empty() && (t.back() == ' ' || t.back() == '\r')) t.pop_back();
    if (t.size() < suffix.size() || t.compare(t.size() - suffix.size(), suffix.size(), suffix) != 0) continue;
    const int indent = indent_of(line);
    std::vector<std::string> fields;
    for (size_t j = i + 1; j < lines.size(); ++j) {
      const std::string& sub = lines[j];
      if (indent_of(sub) <= indent) break;
      if (sub.find("Field ") != std::string::npos) {
        const std::string nm = quoted_in(sub);
        if (!nm.empty()) fields.push_back(nm);
      }
    }
    std::string doc = "**struct " + name + "**";
    if (!fields.empty()) {
      doc += "\n\nFields: ";
      for (size_t k = 0; k < fields.size(); ++k) {
        if (k) doc += ", ";
        doc += fields[k];
      }
    }
    *out = doc;
    return true;
  }
  return false;
}
}  // namespace hm


namespace hm {
// ---- semantic tokens -------------------------------------------------------
// 行内词法 + legend 索引 + LSP 要求的**增量编码**（dl, ds, len, type, mods）。
// 判据是 golden 里的 data 数组：一个数字错，整条链就错位。
struct Word {
  std::string text;
  int start = 0;
};

bool sem_ident(char c) {
  return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
         (c >= '0' && c <= '9');
}

bool sem_type_keyword(const std::string& s) {
  return s == "string" || s == "int" || s == "double" || s == "bool" ||
         s == "json" || s == "cost";
}

bool sem_upper_start(const std::string& s) {
  return !s.empty() && s[0] >= 'A' && s[0] <= 'Z';
}

std::vector<Word> scan_words(const std::string& line) {
  std::vector<Word> out;
  for (size_t i = 0; i < line.size();) {
    if (!sem_ident(static_cast<unsigned char>(line[i]))) { ++i; continue; }
    const size_t start = i;
    while (i < line.size() && sem_ident(static_cast<unsigned char>(line[i]))) ++i;
    out.push_back({line.substr(start, i - start), static_cast<int>(start)});
  }
  return out;
}

std::vector<Word> scan_numbers(const std::string& line) {
  std::vector<Word> out;
  for (size_t i = 0; i < line.size();) {
    if (line[i] < '0' || line[i] > '9') { ++i; continue; }
    // 前一个字符是标识符字符时不是独立数字（foo2）。
    if (i > 0 && sem_ident(static_cast<unsigned char>(line[i - 1]))) { ++i; continue; }
    const size_t start = i;
    while (i < line.size() && line[i] >= '0' && line[i] <= '9') ++i;
    out.push_back({line.substr(start, i - start), static_cast<int>(start)});
  }
  return out;
}

// token_type_index 与 Go 的 tokenIndexOf 同序（legend 顺序即索引）。
int token_type_index(const std::string& t) {
  static const char* kTypes[] = {"variable", "function", "parameter", "type",
                                 "property", "keyword",  "number",   "string"};
  for (int i = 0; i < 8; ++i) {
    if (t == kTypes[i]) return i;
  }
  return -1;
}

std::string semantic_tokens(const std::string& text) {
  struct Tok { int line, start, length, type; };
  std::vector<Tok> toks;
  auto add = [&](int line, int start, int length, const std::string& typ) {
    const int idx = token_type_index(typ);
    if (idx < 0 || length <= 0) return;
    toks.push_back({line, start, length, idx});
  };

  std::vector<std::string> lines;
  {
    std::string cur;
    for (char c : text) {
      if (c == '\n') { lines.push_back(cur); cur.clear(); } else { cur.push_back(c); }
    }
    lines.push_back(cur);
  }

  for (size_t ln = 0; ln < lines.size(); ++ln) {
    std::string raw = lines[ln];
    // 注释整行跳过（注释里的词不该着色）。
    const size_t cmt = raw.find("//");
    if (cmt != std::string::npos) raw = raw.substr(0, cmt);
    const std::vector<Word> words = scan_words(raw);
    for (size_t i = 0; i < words.size(); ++i) {
      const std::string& w = words[i].text;
      auto prev = [&](size_t k) -> const std::string& {
        static const std::string kEmpty;
        return (i >= k) ? words[i - k].text : kEmpty;
      };
      if (is_keyword(w)) {
        add(static_cast<int>(ln), words[i].start, static_cast<int>(w.size()), "keyword");
      } else if (i > 0 && (prev(1) == "auto" || sem_type_keyword(prev(1)))) {
        add(static_cast<int>(ln), words[i].start, static_cast<int>(w.size()), "variable");
      } else if (i > 2 && prev(2) == "const" && (prev(1) == "auto" || sem_type_keyword(prev(1)))) {
        add(static_cast<int>(ln), words[i].start, static_cast<int>(w.size()), "variable");
      } else if (i > 1 && prev(2) == "auto" && prev(1) == "[") {
        add(static_cast<int>(ln), words[i].start, static_cast<int>(w.size()), "variable");
      } else if (i > 0 && (prev(1) == "struct" || prev(1) == "workflow" || prev(1) == "backend")) {
        add(static_cast<int>(ln), words[i].start, static_cast<int>(w.size()), "type");
      } else if (i + 1 < words.size() && words[i + 1].text == "(") {
        add(static_cast<int>(ln), words[i].start, static_cast<int>(w.size()), "function");
      } else if (i > 0 && sem_type_keyword(prev(1))) {
        add(static_cast<int>(ln), words[i].start, static_cast<int>(w.size()), "property");
      } else if (sem_upper_start(w)) {
        add(static_cast<int>(ln), words[i].start, static_cast<int>(w.size()), "type");
      }
    }
    for (const Word& n : scan_numbers(raw)) {
      add(static_cast<int>(ln), n.start, static_cast<int>(n.text.size()), "number");
    }
  }

  std::sort(toks.begin(), toks.end(), [](const Tok& a, const Tok& b) {
    return a.line != b.line ? a.line < b.line : a.start < b.start;
  });

  JVal data = JVal::array();
  int prev_line = 0, prev_start = 0;
  for (const Tok& t : toks) {
    const int dl = t.line - prev_line;
    const int ds = (dl == 0) ? t.start - prev_start : t.start;
    data.push(JVal::integer(dl));
    data.push(JVal::integer(ds));
    data.push(JVal::integer(t.length));
    data.push(JVal::integer(t.type));
    data.push(JVal::integer(0));
    prev_line = t.line;
    prev_start = t.start;
  }
  JVal result = JVal::object();
  result.set("data", std::move(data));
  return result.str();
}
}  // namespace hm

int hm_lsp_main() {
  using namespace hm;
  Server s;
  std::string body;
  while (read_frame(stdin, &body)) {
    size_t pos = 0;
    JVal msg;
    if (!parse_json(body.data(), body.size(), &msg, &pos) || msg.kind() != JVal::kObj) continue;
    const JVal* method = msg.find("method");
    const char* m = (method && method->kind() == JVal::kStr) ? method->as_str().c_str() : "";
    const JVal* idv = msg.find("id");
    const std::string id_json = (idv != nullptr) ? idv->str() : "null";

    if (std::strcmp(m, "initialize") == 0) {
      JVal result = JVal::object();
      result.set("capabilities", capabilities());
      JVal info = JVal::object();
      info.set("name", JVal::string("hermes-lsp"));
      info.set("version", JVal::string(hm_parse_version()));
      result.set("serverInfo", std::move(info));
      reply(stdout, id_json, result.str());
    } else if (std::strcmp(m, "initialized") == 0) {
      // 无需回应
    } else if (std::strcmp(m, "shutdown") == 0) {
      s.shutdown = true;
      reply(stdout, id_json, "null");
    } else if (std::strcmp(m, "exit") == 0) {
      return 0;
    } else if (std::strcmp(m, "textDocument/didOpen") == 0) {
      const JVal* p = msg.find("params");
      const JVal* td = p ? p->find("textDocument") : nullptr;
      const JVal* uri = td ? td->find("uri") : nullptr;
      const JVal* text = td ? td->find("text") : nullptr;
      if (uri && text && uri->kind() == JVal::kStr && text->kind() == JVal::kStr) {
        s.docs[uri->as_str()] = text->as_str();
        publish(stdout, &s, uri->as_str());
      }
    } else if (std::strcmp(m, "textDocument/didChange") == 0) {
      const JVal* p = msg.find("params");
      const JVal* td = p ? p->find("textDocument") : nullptr;
      const JVal* uri = td ? td->find("uri") : nullptr;
      const JVal* changes = p ? p->find("contentChanges") : nullptr;
      if (uri && uri->kind() == JVal::kStr && changes && changes->kind() == JVal::kArr &&
          changes->size() > 0) {
        const JVal* last = changes->at(changes->size() - 1);
        const JVal* text = last ? last->find("text") : nullptr;
        if (text && text->kind() == JVal::kStr) s.docs[uri->as_str()] = text->as_str();
        publish(stdout, &s, uri->as_str());
      }
    } else if (std::strcmp(m, "textDocument/hover") == 0) {
      const JVal* p = msg.find("params");
      const JVal* td = p ? p->find("textDocument") : nullptr;
      const JVal* uri = td ? td->find("uri") : nullptr;
      const JVal* posv = p ? p->find("position") : nullptr;
      const JVal* lv = posv ? posv->find("line") : nullptr;
      const JVal* cv = posv ? posv->find("character") : nullptr;
      std::string result = "null";
      if (uri && uri->kind() == JVal::kStr && lv && cv) {
        auto it = s.docs.find(uri->as_str());
        if (it != s.docs.end()) {
          const int line_no = static_cast<int>(lv->as_int(0));
          const int ch = static_cast<int>(cv->as_int(0));
          std::string dotted;
          if (hm::dotted_at(it->second, line_no, ch, &dotted)) {
            if (const char* d = hm::builtin_doc(dotted)) result = hover_text(d);
          }
          if (result == "null") {
            const std::string w = hm::word_at(it->second, line_no, ch);
            if (!w.empty()) {
              if (const char* d = hm::builtin_doc(w)) result = hm::hover_text(d);
              else if (hm::is_keyword(w)) {
                const char* d = hm::keyword_doc(w);
                result = hm::hover_text(d ? d : ("`" + w + "` — language keyword."));
              }
              // 用户 struct：扫 AST dump 的缩进块（与 Go 版同判据）。
              if (result == "null") {
                std::string dump;
                if (hm_parse_dump(it->second, uri->as_str(), &dump)) {
                  std::string doc;
                  if (hm::struct_doc(dump, w, &doc)) result = hm::hover_text(doc);
                }
              }
            }
          }
        }
      }
      reply(stdout, id_json, result);
    } else if (std::strcmp(m, "textDocument/semanticTokens/full") == 0) {
      const JVal* p = msg.find("params");
      const JVal* td = p ? p->find("textDocument") : nullptr;
      const JVal* uri = td ? td->find("uri") : nullptr;
      std::string result = "{\"data\":[]}";
      if (uri && uri->kind() == JVal::kStr) {
        auto it = s.docs.find(uri->as_str());
        if (it != s.docs.end()) result = hm::semantic_tokens(it->second);
      }
      reply(stdout, id_json, result);
    } else if (std::strcmp(m, "textDocument/documentSymbol") == 0) {
      const JVal* p = msg.find("params");
      const JVal* td = p ? p->find("textDocument") : nullptr;
      const JVal* uri = td ? td->find("uri") : nullptr;
      std::string result = "null";
      if (uri && uri->kind() == JVal::kStr) {
        auto it = s.docs.find(uri->as_str());
        if (it != s.docs.end()) {
          std::string dump;
          if (hm_parse_dump(it->second, uri->as_str(), &dump)) result = hm::dump_symbols(dump);
        }
      }
      reply(stdout, id_json, result);
    } else if (std::strcmp(m, "textDocument/completion") == 0) {
      reply(stdout, id_json, hm::completion_result());
    } else if (std::strcmp(m, "textDocument/didClose") == 0) {
      const JVal* p = msg.find("params");
      const JVal* td = p ? p->find("textDocument") : nullptr;
      const JVal* uri = td ? td->find("uri") : nullptr;
      if (uri && uri->kind() == JVal::kStr) {
        s.docs.erase(uri->as_str());
        std::string params = "{\"diagnostics\":[],\"uri\":" + JVal::string(uri->as_str()).str() + "}";
        notify(stdout, "textDocument/publishDiagnostics", params);
      }
    } else if (idv != nullptr) {
      // 已声明但尚未实现的方法：明确返回 null 结果（不静默丢弃，客户端会等）。
      reply(stdout, id_json, "null");
    }
  }
  return 0;
}

#ifdef HM_LSP_MAIN
int main() { return hm_lsp_main(); }
#endif
