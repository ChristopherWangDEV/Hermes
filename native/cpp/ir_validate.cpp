// ir_validate.cpp —— IR 静态校验的 C++ 第二份实现（mirror internal/ir/validate.go）。
//
// 「去 Go 化」的一部分：internal/ir/validate.go 的三阶段校验（形状 → 图不变量 →
// 派生复算）在 C++ 里重做一份，与 Go 版逐条对应，由 cmd/hermes/ir_build.go 把
// C++ 诊断作为默认、Go 作为回落。判据与 TestCValidateEquivalence 一致：对同一份
// 合法 IR，两边产出**完全相同**的诊断集合（合法 IR 两边都是空集）。
//
// 复算边界（ARCH §11 六轴 / IR.md §5.1）：
//   - 本文件随 libhermes_parser.a 编译，带 -fno-exceptions -fno-rtti，
//     不依赖异常 / RTTI / std::function / 全局可变状态。
//   - 复用同库里的 JVal（json_write.h + json_read.cpp）解析已装配的 IR JSON，
//     与 ir_assemble.cpp 同一套解析器。
//
// 诊断形状对齐 nativeDiag（cmd/hermes/ir_build.go 的 nativeDiagsToDiag 所期望的
// 扁平 JSON）：severity / code / message / line / col / end_line / end_col /
// module / why / fix / notes。
#include "hermes_parser.h"
#include "json_write.h"

#include <cstdint>
#include <cstdarg>
#include <cstring>
#include <map>
#include <string>
#include <vector>

namespace hm {
namespace {

// 与 ir_assemble.cpp 同义：把文本按"尝试 → 扩容"写进调用方缓冲。
bool put(const std::string& text, char* out, size_t cap, size_t* out_len) {
  if (out_len) *out_len = text.size();
  if (out == nullptr || cap < text.size()) return false;
  if (!text.empty()) std::memcpy(out, text.data(), text.size());
  return true;
}

// ---- 诊断记录 ------------------------------------------------------------
enum Sev { kError = 0, kWarning = 1, kNote = 2 };

struct Diag {
  Sev sev;
  std::string code;
  std::string message;
  int line, col, end_line, end_col;
  std::string module;
  std::string why;
  std::vector<std::string> fix;
  // Go catalog 渲染所需的实例值。C++ 只产结构化事实，文案仍由 Go 的
  // diag.New(code, ..., args) 按 catalog 模板渲染；缺 key 会留下 {placeholder}。
  std::map<std::string, std::string> args;
};

// 与 Go diag.New 的文案表镜像。合法 IR 不触发任何诊断，所以文案只在
// 非法 IR 上可见；这里给出与 catalog.go 一致的英文，确保"默认路径"与 Go 同义。
// 每个码给出 Text / Why / Fix；{arg} 由调用方用 danglingArgs 等填入。
struct DiagText { const char* text; const char* why; const char* fix1; const char* fix2; };

static const char* sev_str(Sev s) {
  switch (s) {
    case kWarning: return "warning";
    case kNote: return "note";
    default: return "error";
  }
}

// 简单的 {arg} 模板替换：把 message 模板里的 {k} 换成 values[k]。
std::string fill(const char* tpl, const std::map<std::string, std::string>& v) {
  std::string out;
  for (const char* p = tpl; *p; ++p) {
    if (*p == '{') {
      const char* q = p + 1;
      std::string key;
      while (*q && *q != '}') { key.push_back(*q); ++q; }
      if (*q == '}') {
        auto it = v.find(key);
        // 与 Go subst 一致：没有对应 key 时**原样保留**（比如文案里的
        // JSON 片段 {"k":"double",...}），不能把整段吞掉。
        if (it != v.end()) out += it->second;
        else out.append(p, q - p + 1);
        p = q;
        continue;
      }
    }
    out.push_back(*p);
  }
  return out;
}

// ---- 取字段的小助手 ------------------------------------------------------
int64_t jint(const JVal* v, int64_t def) { return v ? v->as_int(def) : def; }
std::string jstr(const JVal* v) { return v ? v->as_str() : std::string(); }
bool jbool(const JVal* v, bool def) { return v ? v->as_bool(def) : def; }

// 节点 / 类型 / workflow 的 loc：IR 的 Loc 是 {m,l,c,el,ec}。
struct LocRec { int m, l, c, el, ec; };
LocRec loc_of(const JVal* holder) {
  LocRec r{0, 0, 0, 0, 0};
  const JVal* loc = holder ? holder->find("loc") : nullptr;
  if (loc != nullptr && loc->kind() == JVal::kObj) {
    r.m = (int)jint(loc->find("m"), 0);
    r.l = (int)jint(loc->find("l"), 0);
    r.c = (int)jint(loc->find("c"), 0);
    r.el = (int)jint(loc->find("el"), 0);
    r.ec = (int)jint(loc->find("ec"), 0);
  }
  return r;
}

std::string itoa(int64_t n) {
  if (n == 0) return "0";
  bool neg = n < 0;
  if (neg) n = -n;
  char buf[24];
  int i = 23;
  buf[i] = 0;
  while (n > 0) { buf[--i] = char('0' + (n % 10)); n /= 10; }
  if (neg) buf[--i] = '-';
  return std::string(buf + i);
}

std::string range_str(int n) {
  if (n == 0) return "no entries at all";
  if (n == 1) return "1 entry (0)";
  return itoa(n) + " entries (0.." + itoa(n - 1) + ")";
}

std::string pathf(const char* fmt, ...) {
  // 精简版：只支持 %d / %s，够用即可（路径只在 what / from 里当定位字符串）。
  va_list ap;
  va_start(ap, fmt);
  std::string out;
  for (const char* p = fmt; *p; ++p) {
    if (*p != '%') { out.push_back(*p); continue; }
    ++p;
    if (*p == 'd') out += itoa(va_arg(ap, int));
    else if (*p == 's') out += std::string(va_arg(ap, const char*));
    else out.push_back(*p);
  }
  va_end(ap);
  return out;
}

// ---- 文档的"非 AST 事实"（决定 module 名 / 模块路径）---------------------
struct Ctx {
  std::string unit_name;
  std::vector<std::string> module_paths;  // doc.unit.modules[].path
  std::vector<Diag> diags;
  int max_diags = 100;
  int dropped = 0;
};

// 镜像 Go 的 dloc/add 行为：节点/引用级诊断的模块字段一律为空（Go 的 dloc
// 不设置 Module），只有顶层诊断（版本闸 / V-6 / V-11 / panel 截断）显式用
// unit_name。这里默认返回空串，特殊码在调用后用 c.diags.back().module 覆盖。
std::string module_name(const Ctx& c, const LocRec& loc) {
  (void)c; (void)loc;
  return "";
}

// dloc：IR 的 Loc → 诊断位置（容忍 m 越界，与 Go 一致）。
//
// Diag 必须零初始化：has_loc=false 的诊断不写四个位置字段，若用 `Diag d;`
// 会把栈垃圾序列化成 line=73631226 / end_line=3038 这种假行号。
// args 走默认空 map，保证不经过 add_diag_filled 的老调用点形状不变。
void add_diag(Ctx& c, Sev sev, const std::string& code, const std::string& message,
              const std::string& why, const std::vector<std::string>& fix,
              const LocRec& loc, bool has_loc,
              const std::map<std::string, std::string>& args
                  = std::map<std::string, std::string>()) {
  if ((int)c.diags.size() >= c.max_diags) { c.dropped++; return; }
  Diag d{};
  d.sev = sev;
  d.code = code;
  d.message = message;
  d.why = why;
  d.fix = fix;
  d.args = args;
  if (has_loc) {
    d.line = loc.l; d.col = loc.c; d.end_line = loc.el; d.end_col = loc.ec;
    d.module = module_name(c, loc);
  }
  c.diags.push_back(std::move(d));
}

// add_diag_filled 把模板渲染与 args 结构化透传合成一步：message 仍按模板填好
// （前端诊断读它），同时把原样 args 存进 Diag.args（加载期诊断由 Go catalog
// 重渲染）。截断判断完全交给 add_diag，不在封装里另数 dropped。
void add_diag_filled(Ctx& c, Sev sev, const std::string& code, const char* tpl,
                     const std::map<std::string, std::string>& args,
                     const std::string& why, const std::vector<std::string>& fix,
                     const LocRec& loc, bool has_loc) {
  add_diag(c, sev, code, fill(tpl, args), why, fix, loc, has_loc, args);
}

// ---------------------------------------------------------------------------
// V-7 浮点扫描（mirror internal/ir/load.go::scanFloats）。
//
// IR 字节里不允许出现浮点 JSON 数字（P-1）。parse_json 会拒绝浮点数字，但
// scanFloats 在解析之前给出"哪个路径有浮点"的友好诊断。旧实现只维护一个
// push/pop 的 cur_path、从不写对象键名，数组也不记下标 —— 任何命中的 path
// 都是空串，用户只能看到 "at : 1.5"。这里改成真正的递归遍历：对象键 + 数组
// 下标组成路径，并且**全量收集**所有命中（Go 侧也是全量）。
//
// 不能在浮点 IR 上直接用 parse_json（它拒绝浮点），所以这里有一份只认结构、
// 不构造值树的严格字节扫描器；语法不对时返回 false，交给后面的 parse_json /
// E_IR_LOAD_FAILED 处理，避免对非 JSON 抢报浮点。
struct FloatHit {
  std::string path;
  std::string preview;
};

void scan_skip_ws(const char* t, size_t len, size_t& i) {
  while (i < len && (t[i] == ' ' || t[i] == '\t' || t[i] == '\n' || t[i] == '\r')) ++i;
}

bool scan_is_digit(char c) { return c >= '0' && c <= '9'; }

// 跳过一个完整的 JSON 字符串（含两端的引号），转义的引号不当作结束。
bool scan_skip_string(const char* t, size_t len, size_t& i) {
  if (i >= len || t[i] != '"') return false;
  ++i;
  bool escaped = false;
  while (i < len) {
    char c = t[i++];
    if (escaped) { escaped = false; continue; }
    if (c == '\\') { escaped = true; continue; }
    if (c == '"') return true;
  }
  return false;
}

// 读对象键的**原文字节**（不含引号）。IR 的键都是 ASCII 标识符，不做 \u 解码。
bool scan_read_key(const char* t, size_t len, size_t& i, std::string* key) {
  if (i >= len || t[i] != '"') return false;
  ++i;
  bool escaped = false;
  while (i < len) {
    char c = t[i++];
    if (escaped) { escaped = false; key->push_back(c); continue; }
    if (c == '\\') { escaped = true; continue; }
    if (c == '"') return true;
    key->push_back(c);
  }
  return false;
}

void scan_number(const char* t, size_t len, size_t& i, const std::string& path,
                 std::vector<FloatHit>& hits) {
  const size_t start = i;
  if (t[i] == '-') ++i;
  if (i >= len) return;
  if (t[i] == '0') ++i;
  else if (scan_is_digit(t[i])) {
    while (i < len && scan_is_digit(t[i])) ++i;
  } else return;
  bool is_float = false;
  if (i < len && t[i] == '.') {
    is_float = true;
    ++i;
    if (i >= len || !scan_is_digit(t[i])) return;
    while (i < len && scan_is_digit(t[i])) ++i;
  }
  if (i < len && (t[i] == 'e' || t[i] == 'E')) {
    is_float = true;
    ++i;
    if (i < len && (t[i] == '+' || t[i] == '-')) ++i;
    if (i >= len || !scan_is_digit(t[i])) return;
    while (i < len && scan_is_digit(t[i])) ++i;
  }
  if (!is_float) return;
  FloatHit hit;
  hit.path = path;
  hit.preview.assign(t + start, (i < len ? i : len) - start);
  if (hit.preview.size() > 24) hit.preview = hit.preview.substr(0, 24) + "…";
  hits.push_back(std::move(hit));
}

bool scan_value(const char* t, size_t len, size_t& i, const std::string& path,
                std::vector<FloatHit>& hits);

bool scan_object(const char* t, size_t len, size_t& i, const std::string& path,
                 std::vector<FloatHit>& hits) {
  ++i;  // '{'
  scan_skip_ws(t, len, i);
  if (i < len && t[i] == '}') { ++i; return true; }
  for (;;) {
    scan_skip_ws(t, len, i);
    std::string key;
    if (!scan_read_key(t, len, i, &key)) return false;
    scan_skip_ws(t, len, i);
    if (i >= len || t[i] != ':') return false;
    ++i;
    const std::string child = path.empty() ? key : path + "." + key;
    if (!scan_value(t, len, i, child, hits)) return false;
    scan_skip_ws(t, len, i);
    if (i < len && t[i] == ',') { ++i; continue; }
    if (i < len && t[i] == '}') { ++i; return true; }
    return false;
  }
}

bool scan_array(const char* t, size_t len, size_t& i, const std::string& path,
                std::vector<FloatHit>& hits) {
  ++i;  // '['
  scan_skip_ws(t, len, i);
  if (i < len && t[i] == ']') { ++i; return true; }
  size_t index = 0;
  for (;;) {
    const std::string child = path + "[" + itoa((int64_t)index++) + "]";
    if (!scan_value(t, len, i, child, hits)) return false;
    scan_skip_ws(t, len, i);
    if (i < len && t[i] == ',') { ++i; continue; }
    if (i < len && t[i] == ']') { ++i; return true; }
    return false;
  }
}

bool scan_value(const char* t, size_t len, size_t& i, const std::string& path,
                std::vector<FloatHit>& hits) {
  scan_skip_ws(t, len, i);
  if (i >= len) return false;
  const char c = t[i];
  if (c == '{') return scan_object(t, len, i, path, hits);
  if (c == '[') return scan_array(t, len, i, path, hits);
  if (c == '"') return scan_skip_string(t, len, i);
  if (c == '-' || scan_is_digit(c)) { scan_number(t, len, i, path, hits); return true; }
  static const char* lits[] = {"true", "false", "null"};
  for (const char* lit : lits) {
    const size_t n = std::strlen(lit);
    if (i + n <= len && std::memcmp(t + i, lit, n) == 0) { i += n; return true; }
  }
  return false;
}

bool scan_floats(const char* text, size_t len, std::vector<FloatHit>& hits) {
  hits.clear();
  size_t i = 0;
  if (!scan_value(text, len, i, "", hits)) { hits.clear(); return false; }
  scan_skip_ws(text, len, i);
  if (i != len) { hits.clear(); return false; }
  return !hits.empty();
}

// ---------------------------------------------------------------------------
// 阶段 1：形状与位置（V-6）。越界 loc 会让渲染器崩，所以最先查。
void stage_shape(Ctx& c, const JVal* doc) {
  const JVal* types = doc->find("types");
  const JVal* workflows = doc->find("workflows");
  const JVal* unit = doc->find("unit");
  int nmod = 0;
  if (unit != nullptr) {
    const JVal* mods = unit->find("modules");
    if (mods != nullptr && mods->kind() == JVal::kArr) nmod = (int)mods->size();
  }
  auto check = [&](const LocRec& loc, const std::string& what, const std::string& p) {
    if (nmod == 0) {
      add_diag_filled(c, kError, "E_IR_BAD_LOC",
               "{what} has loc.m={m} but the unit has 0 modules",
                    {{"what", what}, {"m", itoa(loc.m)},
                     {"count", "0"}, {"last", "none"}},
               "Every Loc.m must index into unit.modules. A negative or out-of-range m means the IR was truncated, hand-edited, or produced by a frontend that is one version out of step; the diagnostic renderer itself would otherwise crash on an out-of-range array read.",
               {"fix the reference at " + p, "regenerate the IR with the current compiler"},
               loc, true);
      c.diags.back().module = c.unit_name;
      return;
    }
    if (loc.m < 0 || loc.m >= nmod) {
      add_diag_filled(c, kError, "E_IR_BAD_LOC",
               "loc.m={m} is out of range for {what} (unit has {count} modules, valid 0..{last})",
                    {{"what", what}, {"m", itoa(loc.m)}, {"count", itoa(nmod)}, {"last", itoa(nmod - 1)}},
               "Every Loc.m must index into unit.modules. An out-of-range m means the IR was truncated, hand-edited, or produced by a frontend that is one version out of step; the diagnostic renderer itself would otherwise crash on an out-of-range array read.",
               {"fix the reference at " + p, "regenerate the IR with the current compiler"},
               loc, true);
      c.diags.back().module = c.unit_name;
    }
  };
  if (types != nullptr && types->kind() == JVal::kArr) {
    for (size_t ti = 0; ti < types->size(); ++ti) {
      const JVal* t = types->at(ti);
      check(loc_of(t), pathf("types[%d]", (int)ti), pathf("types[%d].loc", (int)ti));
    }
  }
  if (workflows != nullptr && workflows->kind() == JVal::kArr) {
    for (size_t wi = 0; wi < workflows->size(); ++wi) {
      const JVal* w = workflows->at(wi);
      std::string wp = pathf("workflows[%d]", (int)wi);
      check(loc_of(w), wp, wp + ".loc");
      const JVal* nodes = w ? w->find("nodes") : nullptr;
      if (nodes != nullptr && nodes->kind() == JVal::kArr) {
        for (size_t ni = 0; ni < nodes->size(); ++ni) {
          const JVal* n = nodes->at(ni);
          check(loc_of(n), pathf("workflows[%d].nodes[%d]", (int)wi, (int)ni),
                pathf("workflows[%d].nodes[%d].loc", (int)wi, (int)ni));
        }
      }
    }
  }
}

// ---------------------------------------------------------------------------
// 阶段 2：图不变量。索引 + 全部引用解析 + 各 V-* 检查。
struct Index {
  std::map<std::string, int> tool_by_name;
  std::map<std::string, int> backend_by_name;
  std::map<std::string, int> function_by_name;
};

void build_index(Ctx& c, const JVal* doc, Index& idx) {
  (void)c;
  const JVal* tools = doc->find("tools");
  if (tools != nullptr && tools->kind() == JVal::kArr)
    for (size_t i = 0; i < tools->size(); ++i) {
      const JVal* t = tools->at(i);
      idx.tool_by_name[jstr(t->find("name"))] = (int)i;
    }
  const JVal* backends = doc->find("backends");
  if (backends != nullptr && backends->kind() == JVal::kArr)
    for (size_t i = 0; i < backends->size(); ++i) {
      const JVal* b = backends->at(i);
      idx.backend_by_name[jstr(b->find("name"))] = (int)i;
    }
  const JVal* functions = doc->find("functions");
  if (functions != nullptr && functions->kind() == JVal::kArr)
    for (size_t i = 0; i < functions->size(); ++i) {
      const JVal* f = functions->at(i);
      idx.function_by_name[jstr(f->find("name"))] = (int)i;
    }
}

// Table 分类（镜像 internal/ir/refs.go）。
enum Table { kTNode, kTVar, kTType, kTSchema, kTTool, kTBackend, kTFunction, kTNative };
bool is_namespace(Table t) { return t == kTTool || t == kTBackend || t == kTFunction || t == kTNative; }
const char* table_name(Table t) {
  switch (t) {
    case kTNode: return "nodes";
    case kTVar: return "vars";
    case kTType: return "types";
    case kTSchema: return "schemas";
    case kTTool: return "tools";
    case kTBackend: return "backends";
    case kTFunction: return "functions";
    case kTNative: return "natives";
  }
  return "unknown";
}

struct Ref {
  Table table;
  int wf;
  int index;
  std::string name;
  std::string from;
  LocRec loc;
  bool allow_input;
};

// 类型表达式递归展开（appendTypeRefs）。
void append_type_refs(std::vector<Ref>& refs, int wf, const JVal* t, const std::string& p, const LocRec& loc, const JVal* types) {
  if (t == nullptr || t->kind() != JVal::kObj) return;
  const JVal* k = t->find("k");
  if (k == nullptr || k->kind() != JVal::kStr) return;
  std::string ks = k->as_str();
  if (ks == "struct") {
    const JVal* id = t->find("id");
    if (id != nullptr && id->kind() == JVal::kInt)
      refs.push_back({kTType, wf, (int)id->as_int(-1), "", p + ".id", loc, false});
  } else if (ks == "array" || ks == "httpresult") {
    const JVal* of = t->find("of");
    if (of != nullptr && of->kind() == JVal::kArr) {
      for (size_t i = 0; i < of->size(); ++i) {
        const JVal* sub = of->at(i);
        if (sub != nullptr && sub->kind() == JVal::kObj)
          append_type_refs(refs, wf, sub, p + ".of", loc, types);
      }
    } else if (of != nullptr && of->kind() == JVal::kObj) {
      append_type_refs(refs, wf, of, p + ".of", loc, types);
    }
  } else if (ks == "tuple") {
    const JVal* of = t->find("of");
    if (of != nullptr && of->kind() == JVal::kArr)
      for (size_t i = 0; i < of->size(); ++i) {
        const JVal* sub = of->at(i);
        if (sub != nullptr && sub->kind() == JVal::kObj)
          append_type_refs(refs, wf, sub, pathf("%s.of[%d]", p.c_str(), (int)i), loc, types);
      }
  }
}

// 读 node 上的 `args`（可能是对象或数组的整数 id 表），收集其中的非负整数 id。
void append_arg_ids(std::vector<Ref>& refs, const JVal* n, const std::string& p, int wf, const LocRec& loc) {
  const JVal* args = n->find("args");
  if (args == nullptr) return;
  if (args->kind() == JVal::kArr) {
    for (size_t i = 0; i < args->size(); ++i) {
      const JVal* a = args->at(i);
      if (a != nullptr && a->kind() == JVal::kInt && a->as_int(-1) >= 0)
        refs.push_back({kTNode, wf, (int)a->as_int(-1), "", pathf("%s.args[%d]", p.c_str(), (int)i), loc, false});
    }
  } else if (args->kind() == JVal::kObj) {
    std::vector<std::string> keys;
    args->keys(&keys);
    for (const std::string& key : keys) {
      const JVal* v = args->find(key);
      if (v != nullptr && v->kind() == JVal::kInt && v->as_int(-1) >= 0)
        refs.push_back({kTNode, wf, (int)v->as_int(-1), "", pathf("%s.args.%s", p.c_str(), key.c_str()), loc, false});
    }
  }
}

// appendNodeRefs：按 node kind 分出它的全部引用（新增 kind 必须在这里加 case）。
void append_node_refs(std::vector<Ref>& refs, int wf, const JVal* n, const std::string& p) {
  if (n == nullptr || n->kind() != JVal::kObj) return;
  const JVal* k = n->find("k");
  if (k == nullptr || k->kind() != JVal::kStr) return;
  std::string ks = k->as_str();
  LocRec loc = loc_of(n);
  auto node = [&](int idx, const std::string& field) {
    refs.push_back({kTNode, wf, idx, "", p + "." + field, loc, false});
  };
  auto node_ptr = [&](const JVal* idx, const std::string& field) {
    if (idx != nullptr && idx->kind() == JVal::kInt) node((int)idx->as_int(-1), field);
  };
  auto var_ref = [&](int idx, const std::string& field, bool allow) {
    refs.push_back({kTVar, wf, idx, "", p + "." + field, loc, allow});
  };
  auto var_ptr_ref = [&](const JVal* idx, const std::string& field) {
    if (idx != nullptr && idx->kind() == JVal::kInt) var_ref((int)idx->as_int(-1), field, true);
  };

  if (ks == "agent") {
    const JVal* agent = n->find("agent");
    if (agent != nullptr && agent->kind() == JVal::kObj)
      node_ptr(agent->find("goal"), "agent.goal");
  } else if (ks == "array_lit" || ks == "seq") {
    const JVal* kids = n->find("kids");
    if (kids != nullptr && kids->kind() == JVal::kArr)
      for (size_t i = 0; i < kids->size(); ++i) {
        const JVal* kd = kids->at(i);
        if (kd != nullptr && kd->kind() == JVal::kInt) node((int)kd->as_int(-1), pathf("kids[%d]", (int)i));
      }
  } else if (ks == "assign" || ks == "len" || ks == "exprstmt" || ks == "return" ||
             ks == "stop" || ks == "mem_txn" || ks == "unop" ||
             ks == "on" || ks == "sleep" || ks == "until" || ks == "at") {
    if (ks == "stop") { node_ptr(n->find("e"), "e"); node_ptr(n->find("data"), "data"); }
    else if (ks == "at") { node_ptr(n->find("of"), "of"); node_ptr(n->find("i"), "i"); }
    else if (ks == "sleep") node_ptr(n->find("ms"), "ms");
    else if (ks == "until") node_ptr(n->find("timeout_ms"), "timeout_ms");
    else if (ks == "on") node_ptr(n->find("body"), "body");
    else node_ptr(n->find("e"), "e");
  } else if (ks == "store_op") {
    append_arg_ids(refs, n, p, wf, loc);
  } else if (ks == "while") {
    node_ptr(n->find("c"), "c"); node_ptr(n->find("body"), "body");
  } else if (ks == "for") {
    node_ptr(n->find("init"), "init"); node_ptr(n->find("cond"), "cond"); node_ptr(n->find("body"), "body");
  } else if (ks == "break" || ks == "continue" || ks == "nop" || ks == "lit" || ks == "interp") {
    // 无引用
  } else if (ks == "decl") {
    const JVal* var = n->find("var");
    if (var != nullptr && var->kind() == JVal::kInt) var_ref((int)var->as_int(-1), "var", true);
    const JVal* vars = n->find("vars");
    if (vars != nullptr && vars->kind() == JVal::kArr)
      for (size_t i = 0; i < vars->size(); ++i) {
        const JVal* bp = vars->at(i);
        var_ptr_ref(bp, pathf("vars[%d]", (int)i));
      }
    node_ptr(n->find("e"), "e");
  } else if (ks == "destructure") {
    const JVal* vars = n->find("vars");
    if (vars != nullptr && vars->kind() == JVal::kArr)
      for (size_t i = 0; i < vars->size(); ++i) {
        const JVal* bp = vars->at(i);
        var_ptr_ref(bp, pathf("vars[%d]", (int)i));
      }
    node_ptr(n->find("e"), "e");
  } else if (ks == "if") {
    node_ptr(n->find("c"), "c"); node_ptr(n->find("then"), "then");
    const JVal* elifs = n->find("elifs");
    if (elifs != nullptr && elifs->kind() == JVal::kArr)
      for (size_t i = 0; i < elifs->size(); ++i) {
        const JVal* e = elifs->at(i);
        if (e == nullptr) continue;
        node_ptr(e->find("c"), pathf("elifs[%d].c", (int)i));
        node_ptr(e->find("body"), pathf("elifs[%d].body", (int)i));
      }
    node_ptr(n->find("else"), "else");
  } else if (ks == "match") {
    node_ptr(n->find("subj"), "subj");
    const JVal* arms = n->find("arms");
    if (arms != nullptr && arms->kind() == JVal::kArr)
      for (size_t i = 0; i < arms->size(); ++i) {
        const JVal* a = arms->at(i);
        if (a == nullptr) continue;
        node_ptr(a->find("body"), pathf("arms[%d].body", (int)i));
      }
    node_ptr(n->find("default"), "default");
  } else if (ks == "parallel") {
    const JVal* branches = n->find("branches");
    if (branches != nullptr && branches->kind() == JVal::kArr)
      for (size_t i = 0; i < branches->size(); ++i) {
        const JVal* b = branches->at(i);
        if (b != nullptr && b->kind() == JVal::kInt) node((int)b->as_int(-1), pathf("branches[%d]", (int)i));
      }
    const JVal* join = n->find("join");
    if (join != nullptr && join->kind() == JVal::kArr)
      for (size_t i = 0; i < join->size(); ++i) {
        const JVal* j = join->at(i);
        if (j != nullptr && j->kind() == JVal::kInt) node((int)j->as_int(-1), pathf("join[%d]", (int)i));
      }
    const JVal* bind = n->find("bind");
    if (bind != nullptr && bind->kind() == JVal::kArr)
      for (size_t i = 0; i < bind->size(); ++i) {
        const JVal* b = bind->at(i);
        var_ptr_ref(b, pathf("bind[%d]", (int)i));
      }
  } else if (ks == "retry") {
    node_ptr(n->find("body"), "body");
  } else if (ks == "try") {
    node_ptr(n->find("body"), "body");
    const JVal* catches = n->find("catches");
    if (catches != nullptr && catches->kind() == JVal::kArr)
      for (size_t i = 0; i < catches->size(); ++i) {
        const JVal* cc = catches->at(i);
        if (cc == nullptr) continue;
        const JVal* cv = cc->find("v");
        if (cv != nullptr && cv->kind() == JVal::kInt) var_ref((int)cv->as_int(-1), pathf("catches[%d].v", (int)i), false);
        node_ptr(cc->find("body"), pathf("catches[%d].body", (int)i));
      }
  } else if (ks == "fallback") {
    node_ptr(n->find("main"), "main"); node_ptr(n->find("body"), "body");
  } else if (ks == "emit") {
    // Route=2 的定向目标：Go 的 It.Ref 是 Node.Target，JSON 键是 "target"
    // （不是 "to" —— "to" 是 call_tool / cast 的类型字段）。这里曾经读 "to"，
    // 于是 emit 的 target 引用从来没被 V-2 看见过。
    node_ptr(n->find("target"), "to");
    append_arg_ids(refs, n, p, wf, loc);
  } else if (ks == "var") {
    const JVal* var = n->find("var");
    if (var != nullptr && var->kind() == JVal::kInt) var_ref((int)var->as_int(-1), "var", true);
  } else if (ks == "field" || ks == "index") {
    node_ptr(n->find("of"), "of");
  } else if (ks == "binop") {
    node_ptr(n->find("l"), "l"); node_ptr(n->find("r"), "r");
  } else if (ks == "call_model") {
    const JVal* schema = n->find("schema");
    if (schema != nullptr && schema->kind() == JVal::kInt)
      refs.push_back({kTSchema, wf, (int)schema->as_int(-1), "", p + ".schema", loc, false});
    // Go 的 ModelArgs 只把 args.prompt 当节点引用（与 call_tool 不同）。
    const JVal* args = n->find("args");
    if (args != nullptr && args->kind() == JVal::kObj)
      node_ptr(args->find("prompt"), "args.prompt");
    const JVal* att = n->find("attempt_ctx");
    if (att != nullptr && att->kind() == JVal::kObj)
      node_ptr(att->find("retry_node"), "attempt_ctx.retry_node");
  } else if (ks == "call_tool") {
    const JVal* tool = n->find("tool");
    if (tool != nullptr && tool->kind() == JVal::kStr)
      refs.push_back({kTTool, wf, 0, tool->as_str(), p + ".tool", loc, false});
    // Go 的 ToolArgs 只把 args.url/header/body/idem_key 当节点引用。
    const JVal* args = n->find("args");
    if (args != nullptr && args->kind() == JVal::kObj) {
      node_ptr(args->find("url"), "args.url");
      node_ptr(args->find("header"), "args.header");
      node_ptr(args->find("body"), "args.body");
      node_ptr(args->find("idem_key"), "args.idem_key");
    }
    const JVal* ret = n->find("ret");
    if (ret != nullptr && ret->kind() == JVal::kObj) append_type_refs(refs, wf, ret, p + ".ret", loc, nullptr);
    const JVal* att = n->find("attempt_ctx");
    if (att != nullptr && att->kind() == JVal::kObj)
      node_ptr(att->find("retry_node"), "attempt_ctx.retry_node");
  } else if (ks == "call_fn") {
    const JVal* fn = n->find("fn");
    if (fn != nullptr && fn->kind() == JVal::kStr)
      refs.push_back({kTFunction, wf, 0, fn->as_str(), p + ".fn", loc, false});
    append_arg_ids(refs, n, p, wf, loc);
  } else if (ks == "call_native") {
    const JVal* fn = n->find("fn");
    if (fn != nullptr && fn->kind() == JVal::kStr)
      refs.push_back({kTNative, wf, 0, fn->as_str(), p + ".fn", loc, false});
    append_arg_ids(refs, n, p, wf, loc);
    const JVal* ret = n->find("ret");
    if (ret != nullptr && ret->kind() == JVal::kObj) append_type_refs(refs, wf, ret, p + ".ret", loc, nullptr);
    const JVal* ats = n->find("arg_types");
    if (ats != nullptr && ats->kind() == JVal::kArr)
      for (size_t i = 0; i < ats->size(); ++i) {
        const JVal* sub = ats->at(i);
        if (sub != nullptr && sub->kind() == JVal::kObj)
          append_type_refs(refs, wf, sub, pathf("%s.arg_types[%d]", p.c_str(), (int)i), loc, nullptr);
      }
  } else if (ks == "call_json") {
    append_arg_ids(refs, n, p, wf, loc);
  } else if (ks == "cast") {
    node_ptr(n->find("e"), "e");
    const JVal* to = n->find("to");
    if (to != nullptr && to->kind() == JVal::kObj) append_type_refs(refs, wf, to, p + ".to", loc, nullptr);
  }
}

void collect_refs(const JVal* doc, std::vector<Ref>& refs, const Ctx& c) {
  (void)c;
  const JVal* types = doc->find("types");
  if (types != nullptr && types->kind() == JVal::kArr) {
    for (size_t ti = 0; ti < types->size(); ++ti) {
      const JVal* t = types->at(ti);
      const JVal* fields = t ? t->find("fields") : nullptr;
      if (fields != nullptr && fields->kind() == JVal::kArr)
        for (size_t fi = 0; fi < fields->size(); ++fi) {
          const JVal* f = fields->at(fi);
          const JVal* ft = f ? f->find("t") : nullptr;
          if (ft != nullptr && ft->kind() == JVal::kObj)
            append_type_refs(refs, -1, ft, pathf("types[%d].fields[%d].t", (int)ti, (int)fi), loc_of(t), nullptr);
        }
    }
  }
  const JVal* workflows = doc->find("workflows");
  if (workflows != nullptr && workflows->kind() == JVal::kArr) {
    for (size_t wi = 0; wi < workflows->size(); ++wi) {
      const JVal* w = workflows->at(wi);
      std::string p = pathf("workflows[%d]", (int)wi);
      const JVal* backend = w ? w->find("backend") : nullptr;
      if (backend != nullptr && backend->kind() == JVal::kStr && !backend->as_str().empty())
        refs.push_back({kTBackend, (int)wi, 0, backend->as_str(), p + ".backend", loc_of(w), false});
      const JVal* input = w ? w->find("input") : nullptr;
      if (input != nullptr && input->kind() == JVal::kObj)
        append_type_refs(refs, (int)wi, input, p + ".input", loc_of(w), nullptr);
      const JVal* ret = w ? w->find("returns") : nullptr;
      if (ret != nullptr && ret->kind() == JVal::kObj)
        append_type_refs(refs, (int)wi, ret, p + ".returns", loc_of(w), nullptr);
      const JVal* root = w ? w->find("root") : nullptr;
      if (root != nullptr && root->kind() == JVal::kInt)
        refs.push_back({kTNode, (int)wi, (int)root->as_int(-1), "", p + ".root", loc_of(w), false});
      const JVal* nodes = w ? w->find("nodes") : nullptr;
      if (nodes != nullptr && nodes->kind() == JVal::kArr)
        for (size_t ni = 0; ni < nodes->size(); ++ni) {
          const JVal* n = nodes->at(ni);
          append_node_refs(refs, (int)wi, n, pathf("%s.nodes[%d]", p.c_str(), (int)ni));
        }
      const JVal* vars = w ? w->find("vars") : nullptr;
      if (vars != nullptr && vars->kind() == JVal::kArr)
        for (size_t vi = 0; vi < vars->size(); ++vi) {
          const JVal* v = vars->at(vi);
          std::string vp = pathf("%s.vars[%d]", p.c_str(), (int)vi);
          const JVal* vt = v ? v->find("t") : nullptr;
          if (vt != nullptr && vt->kind() == JVal::kObj) append_type_refs(refs, (int)wi, vt, vp + ".t", loc_of(w), nullptr);
          const JVal* decl = v ? v->find("decl") : nullptr;
          if (decl != nullptr && decl->kind() == JVal::kInt)
            refs.push_back({kTNode, (int)wi, (int)decl->as_int(-1), "", vp + ".decl", loc_of(w), false});
        }
    }
  }
}

// 节点表的"按 workflow 的表大小"。
int table_size(const JVal* doc, const Ref& r) {
  switch (r.table) {
    case kTNode:
    case kTVar: {
      const JVal* workflows = doc->find("workflows");
      if (r.wf >= 0 && workflows != nullptr && workflows->kind() == JVal::kArr &&
          r.wf < (int)workflows->size()) {
        const JVal* w = workflows->at(r.wf);
        const char* key = (r.table == kTNode) ? "nodes" : "vars";
        const JVal* arr = w ? w->find(key) : nullptr;
        if (arr != nullptr && arr->kind() == JVal::kArr) return (int)arr->size();
      }
      return 0;
    }
    case kTType: {
      const JVal* t = doc->find("types");
      return (t && t->kind() == JVal::kArr) ? (int)t->size() : 0;
    }
    case kTSchema: {
      const JVal* s = doc->find("schemas");
      return (s && s->kind() == JVal::kArr) ? (int)s->size() : 0;
    }
    default:
      // 名字表（tool/backend/function/native）与 type 表不会走到这里：
      // resolve_refs 在调用 table_size 前已对 is_namespace 与 kTType 分支 continue。
      return 0;
  }
  return 0;
}

void resolve_refs(Ctx& c, const JVal* doc, const std::vector<Ref>& refs, const Index& idx) {
  for (const Ref& r : refs) {
    if (is_namespace(r.table)) {
      bool ok = false;
      switch (r.table) {
        case kTTool: ok = idx.tool_by_name.count(r.name) > 0; break;
        case kTBackend: ok = idx.backend_by_name.count(r.name) > 0; break;
        case kTFunction: ok = idx.function_by_name.count(r.name) > 0; break;
        case kTNative: ok = true; break;  // 反向融合：加载期从宽，链接期才校验
        default: break;
      }
      if (!ok) {
        std::string what = r.from;
        std::string table = table_name(r.table);
        int size = 0;
        if (r.table == kTTool) size = (int)idx.tool_by_name.size();
        else if (r.table == kTBackend) size = (int)idx.backend_by_name.size();
        else if (r.table == kTFunction) size = (int)idx.function_by_name.size();
        std::string range = range_str(size);
        add_diag_filled(c, kError, "E_IR_DANGLING_REF",
                 "{what} points at {id}, but {table} has {range}",
                      {{"what", what}, {"id", r.name}, {"table", table}, {"range", range},
                       {"path", what}},
                 "Every id in the IR's reference positions must index into the table it names. A dangling id means the IR was truncated, hand-edited, or produced by a frontend that is one version out of step.",
                 {"remove the reference at " + r.from, "or add the missing entry — the table must stay consistent with its references"},
                 r.loc, true);
      }
      continue;
    }
    if (r.table == kTType) {
      // type id 从 1 起；检查存在性。
      const JVal* types = doc->find("types");
      bool found = false;
      if (types != nullptr && types->kind() == JVal::kArr)
        for (size_t i = 0; i < types->size(); ++i) {
          const JVal* t = types->at(i);
          const JVal* id = t ? t->find("id") : nullptr;
          if (id != nullptr && id->kind() == JVal::kInt && (int)id->as_int(-1) == r.index) { found = true; break; }
        }
      if (!found) {
        int size = (types && types->kind() == JVal::kArr) ? (int)types->size() : 0;
        add_diag_filled(c, kError, "E_IR_DANGLING_REF",
                 "{what} points at {id}, but {table} has {range}",
                      {{"what", r.from}, {"id", itoa(r.index)}, {"table", "types"},
                       {"range", range_str(size)}, {"path", r.from}},
                 "Every id in the IR's reference positions must index into the table it names. A dangling id means the IR was truncated, hand-edited, or produced by a frontend that is one version out of step.",
                 {"remove the reference at " + r.from, "or add the missing entry — the table must stay consistent with its references"},
                 r.loc, true);
      }
      continue;
    }
    int size = table_size(doc, r);
    if (r.allow_input && r.index == -1) continue;  // input 哨兵
    if (r.index < 0 || r.index >= size) {
      add_diag_filled(c, kError, "E_IR_DANGLING_REF",
               "{what} points at {id}, but {table} has {range}",
                    {{"what", r.from}, {"id", itoa(r.index)}, {"table", table_name(r.table)},
                     {"range", range_str(size)}, {"path", r.from}},
               "Every id in the IR's reference positions must index into the table it names. A dangling id means the IR was truncated, hand-edited, or produced by a frontend that is one version out of step.",
               {"remove the reference at " + r.from, "or add the missing entry — the table must stay consistent with its references"},
               r.loc, true);
    }
  }
}

// checkFunctionCalls：call_fn 只许指向 recursive=true 的函数。
void check_function_calls(Ctx& c, const JVal* doc, const Index& idx) {
  (void)idx;
  // 先按名字建函数是否递归的表。
  std::map<std::string, bool> recursive_of;
  const JVal* functions = doc->find("functions");
  if (functions != nullptr && functions->kind() == JVal::kArr)
    for (size_t i = 0; i < functions->size(); ++i) {
      const JVal* f = functions->at(i);
      recursive_of[jstr(f->find("name"))] = jbool(f->find("recursive"), false);
    }
  // workflow 节点里的 call_fn。
  const JVal* workflows = doc->find("workflows");
  if (workflows != nullptr && workflows->kind() == JVal::kArr) {
    for (size_t wi = 0; wi < workflows->size(); ++wi) {
      const JVal* w = workflows->at(wi);
      const JVal* nodes = w ? w->find("nodes") : nullptr;
      if (nodes == nullptr || nodes->kind() != JVal::kArr) continue;
      for (size_t ni = 0; ni < nodes->size(); ++ni) {
        const JVal* n = nodes->at(ni);
        if (n == nullptr || n->kind() != JVal::kObj) continue;
        const JVal* k = n->find("k");
        if (k == nullptr || k->kind() != JVal::kStr || k->as_str() != "call_fn") continue;
        const JVal* fn = n->find("fn");
        if (fn == nullptr || fn->kind() != JVal::kStr) continue;
        auto it = recursive_of.find(fn->as_str());
        if (it == recursive_of.end()) continue;  // 悬空由 V-2 报
        if (it->second) continue;  // 递归函数允许 call_fn
        std::string where = pathf("workflows[%d].nodes[%d]", (int)wi, (int)ni);
        add_diag_filled(c, kError, "E_IR_DYNAMIC_CALL",
                 "non-recursive function {fn} is called through call_fn at {where}: compiled mode must inline it",
                      {{"fn", fn->as_str()}, {"where", where}},
                 "Hermes is a compiled language: non-recursive functions are expanded at compile time so the runtime has no dynamic dispatch. A call_fn to a non-recursive function means the IR came from an older compiler, was hand-edited, or codegen has a bug.",
                 {"rebuild the IR with the current compiler (non-recursive calls are inlined)",
                  "if " + fn->as_str() + " really recurses, mark it recursive"},
                 loc_of(n), true);
      }
    }
  }
  // 函数体里的 call_fn：悬空或未递归都要报。
  if (functions != nullptr && functions->kind() == JVal::kArr) {
    for (size_t fi = 0; fi < functions->size(); ++fi) {
      const JVal* fn = functions->at(fi);
      const JVal* nodes = fn ? fn->find("nodes") : nullptr;
      if (nodes == nullptr || nodes->kind() != JVal::kArr) continue;
      for (size_t ni = 0; ni < nodes->size(); ++ni) {
        const JVal* n = nodes->at(ni);
        if (n == nullptr || n->kind() != JVal::kObj) continue;
        const JVal* k = n->find("k");
        if (k == nullptr || k->kind() != JVal::kStr || k->as_str() != "call_fn") continue;
        std::string where = pathf("functions[%d].nodes[%d]", (int)fi, (int)ni);
        const JVal* fnname = n->find("fn");
        if (fnname == nullptr || fnname->kind() != JVal::kStr) continue;
        auto it = recursive_of.find(fnname->as_str());
        if (it == recursive_of.end()) {
          add_diag_filled(c, kError, "E_IR_DANGLING_REF",
                   "{what} points at {id}, but {table} has {range}",
                        {{"what", where + ".fn"}, {"id", fnname->as_str()}, {"table", "functions"},
                         {"range", range_str((int)functions->size())}, {"path", where + ".fn"}},
                   "Every id in the IR's reference positions must index into the table it names.",
                   {"remove the reference at " + where + ".fn", "or add the missing function"},
                   loc_of(n), true);
          continue;
        }
        if (it->second) continue;
        add_diag_filled(c, kError, "E_IR_DYNAMIC_CALL",
                 "non-recursive function {fn} is called through call_fn at {where}: compiled mode must inline it",
                      {{"fn", fnname->as_str()}, {"where", where}},
                 "Hermes is a compiled language: non-recursive functions are expanded at compile time.",
                 {"rebuild the IR with the current compiler", "if " + fnname->as_str() + " really recurses, mark it recursive"},
                 loc_of(n), true);
      }
    }
  }
}

// V-1：node id == 下标，且 nodes/vars 各自 id 唯一。
void check_workflow_ids(Ctx& c, const JVal* doc) {
  const JVal* workflows = doc->find("workflows");
  if (workflows == nullptr || workflows->kind() != JVal::kArr) return;
  for (size_t wi = 0; wi < workflows->size(); ++wi) {
    const JVal* w = workflows->at(wi);
    const JVal* nodes = w ? w->find("nodes") : nullptr;
    if (nodes == nullptr || nodes->kind() != JVal::kArr) continue;
    std::map<int, int> seen;
    for (size_t ni = 0; ni < nodes->size(); ++ni) {
      const JVal* n = nodes->at(ni);
      const JVal* idv = n ? n->find("id") : nullptr;
      if (idv == nullptr || idv->kind() != JVal::kInt) continue;
      int id = (int)idv->as_int(-1);
      if (id != (int)ni) {
        add_diag_filled(c, kError, "E_IR_DUP_ID",
                 "node {id} at position {first} has id != index (expected {last})",
                      {{"id", itoa(id)}, {"wf", jstr(w->find("name"))},
                       {"first", pathf("nodes[%d]", (int)ni)}, {"second", pathf("nodes[%d]", (int)ni)},
                       {"last", itoa((int)nodes->size() - 1)}},
                 "IR.md requires nodes to be a flat array where id == index. A mismatched id means the IR was truncated, hand-edited, or produced by a frontend that is one version out of step.",
                 {"renumber the node so id == index", "regenerate the IR with the current compiler"},
                 loc_of(n), true);
      }
      if (seen.count(id)) {
        add_diag_filled(c, kError, "E_IR_DUP_ID",
                 "node id {id} is duplicated at {first} and {second}",
                      {{"id", itoa(id)}, {"first", pathf("nodes[%d]", seen[id])}, {"second", pathf("nodes[%d]", (int)ni)}, {"last", itoa((int)nodes->size() - 1)}, {"wf", jstr(w->find("name"))}},
                 "IR.md requires node ids to be unique within a workflow.",
                 {"remove the duplicate node", "renumber so all ids are unique"},
                 loc_of(n), true);
      } else {
        seen[id] = (int)ni;
      }
    }
    const JVal* vars = w ? w->find("vars") : nullptr;
    if (vars != nullptr && vars->kind() == JVal::kArr) {
      std::map<int, int> vseen;
      for (size_t vi = 0; vi < vars->size(); ++vi) {
        const JVal* v = vars->at(vi);
        const JVal* idv = v ? v->find("id") : nullptr;
        if (idv == nullptr || idv->kind() != JVal::kInt) continue;
        int id = (int)idv->as_int(-1);
        if (vseen.count(id)) {
          add_diag_filled(c, kError, "E_IR_DUP_ID",
                   "var id {id} is duplicated at {first} and {second}",
                        {{"id", itoa(id)}, {"first", pathf("vars[%d]", vseen[id])}, {"second", pathf("vars[%d]", (int)vi)}, {"last", itoa((int)vars->size() - 1)}, {"wf", jstr(w->find("name"))}},
                   "IR.md requires var ids to be unique within a workflow.",
                   {"remove the duplicate var", "renumber so all ids are unique"},
                   loc_of(w), true);
        } else {
          vseen[id] = (int)vi;
        }
      }
    }
  }
}

// V-11：四个命名空间各自唯一。
void check_exports_uniqueness(Ctx& c, const JVal* doc) {
  struct NS { const char* name; const char* key; };
  NS nss[4] = {{"types", "types"}, {"workflows", "workflows"}, {"backends", "backends"}, {"tools", "tools"}};
  const JVal* exports = doc->find("exports");
  for (auto& ns : nss) {
    const JVal* list = exports ? exports->find(ns.key) : nullptr;
    if (list == nullptr || list->kind() != JVal::kArr) continue;
    std::map<std::string, int> seen;
    for (size_t i = 0; i < list->size(); ++i) {
      const JVal* s = list->at(i);
      if (s == nullptr || s->kind() != JVal::kStr) continue;
      std::string name = s->as_str();
      if (seen.count(name)) {
        add_diag_filled(c, kError, "E_IR_SYM_CONFLICT",
                 "symbol {name} appears {count} times in exports.{namespace} (at {places})",
                      {{"name", name}, {"count", "2"}, {"namespace", ns.name},
                       {"places", pathf("exports.%s[%d] and [%d]", ns.key, seen[name], (int)i)}},
                 "Each export namespace must list each symbol at most once. A duplicate means the exported surface is ambiguous.",
                 {"remove the duplicate export " + name, "regenerate the IR with the current compiler"},
                 LocRec{0,0,0,0,0}, false);
        c.diags.back().module = c.unit_name;
      } else {
        seen[name] = (int)i;
      }
    }
  }
}

// V-4：root 存在且是 seq。
void check_root(Ctx& c, const JVal* doc) {
  const JVal* workflows = doc->find("workflows");
  if (workflows == nullptr || workflows->kind() != JVal::kArr) return;
  for (size_t wi = 0; wi < workflows->size(); ++wi) {
    const JVal* w = workflows->at(wi);
    const JVal* root = w ? w->find("root") : nullptr;
    const JVal* nodes = w ? w->find("nodes") : nullptr;
    if (root == nullptr || root->kind() != JVal::kInt || nodes == nullptr || nodes->kind() != JVal::kArr) continue;
    int rid = (int)root->as_int(-1);
    std::string wp = pathf("workflows[%d]", (int)wi);
    if (rid < 0 || rid >= (int)nodes->size()) {
      add_diag_filled(c, kError, "E_IR_BAD_ROOT",
               "workflow {wf}: root={root}, but {reason}",
                    {{"wf", jstr(w->find("name"))}, {"root", itoa(rid)}, {"reason", "no such node"}},
               "The executor starts at root and expects a statement list there, i.e. a node with k=\"seq\". Any other kind has no defined entry semantics.",
               {"point root at the first `seq` node", "or wrap the body in a `seq` node"},
               loc_of(w), true);
      continue;
    }
    const JVal* rn = nodes->at(rid);
    const JVal* k = rn ? rn->find("k") : nullptr;
    if (k == nullptr || k->kind() != JVal::kStr || k->as_str() != "seq") {
      add_diag_filled(c, kError, "E_IR_BAD_ROOT",
               "workflow {wf}: root={root}, but {reason}",
                    {{"wf", jstr(w->find("name"))}, {"root", itoa(rid)},
                     {"reason", "node " + itoa(rid) + " has k=" + (k && k->kind()==JVal::kStr?k->as_str():"<none>") + ", not \"seq\""}},
               "The executor starts at root and expects a statement list there, i.e. a node with k=\"seq\".",
               {"point root at the first `seq` node", "or wrap the body in a `seq` node"},
               loc_of(w), true);
    }
  }
}

// boundVars：返回节点绑定的变量 id（不含 -1 input，也不含 null 丢弃位）。
void bound_vars(const JVal* n, std::vector<int>& ids, bool& has_binding) {
  if (n == nullptr || n->kind() != JVal::kObj) return;
  const JVal* k = n->find("k");
  if (k == nullptr || k->kind() != JVal::kStr) return;
  std::string ks = k->as_str();
  auto add = [&](const JVal* p) {
    if (p != nullptr && p->kind() == JVal::kInt && p->as_int(-1) >= 0) ids.push_back((int)p->as_int(-1));
  };
  if (ks == "decl") {
    has_binding = (n->find("var") != nullptr) || (n->find("vars") != nullptr);
    add(n->find("var"));
    const JVal* vs = n->find("vars");
    if (vs != nullptr && vs->kind() == JVal::kArr)
      for (size_t i = 0; i < vs->size(); ++i) add(vs->at(i));
  } else if (ks == "for") {
    has_binding = (n->find("var") != nullptr);
    add(n->find("var"));
  } else if (ks == "destructure") {
    has_binding = true;
    const JVal* vs = n->find("vars");
    if (vs != nullptr && vs->kind() == JVal::kArr)
      for (size_t i = 0; i < vs->size(); ++i) add(vs->at(i));
  } else if (ks == "parallel") {
    has_binding = (n->find("bind") != nullptr);
    const JVal* bind = n->find("bind");
    if (bind != nullptr && bind->kind() == JVal::kArr)
      for (size_t i = 0; i < bind->size(); ++i) add(bind->at(i));
  } else if (ks == "try") {
    const JVal* catches = n->find("catches");
    if (catches != nullptr && catches->kind() == JVal::kArr)
      for (size_t i = 0; i < catches->size(); ++i) {
        const JVal* cc = catches->at(i);
        const JVal* cv = cc ? cc->find("v") : nullptr;
        if (cv != nullptr && cv->kind() == JVal::kInt && cv->as_int(-1) >= 0) {
          ids.push_back((int)cv->as_int(-1));
          has_binding = true;
        }
      }
  } else if (ks == "fallback") {
    // fallback 不绑定变量
  }
}

// V-5：var.decl ↔ decl.var 双向。
void check_var_binding(Ctx& c, const JVal* doc) {
  const JVal* workflows = doc->find("workflows");
  if (workflows == nullptr || workflows->kind() != JVal::kArr) return;
  for (size_t wi = 0; wi < workflows->size(); ++wi) {
    const JVal* w = workflows->at(wi);
    const JVal* nodes = w ? w->find("nodes") : nullptr;
    const JVal* vars = w ? w->find("vars") : nullptr;
    if (nodes == nullptr || nodes->kind() != JVal::kArr || vars == nullptr || vars->kind() != JVal::kArr) continue;
    std::string p = pathf("workflows[%d]", (int)wi);
    // 正向：vars[i].decl 指向的节点必须绑定了这个变量。
    for (size_t vi = 0; vi < vars->size(); ++vi) {
      const JVal* vr = vars->at(vi);
      const JVal* decl = vr ? vr->find("decl") : nullptr;
      const JVal* vrid = vr ? vr->find("id") : nullptr;
      if (decl == nullptr || decl->kind() != JVal::kInt || vrid == nullptr || vrid->kind() != JVal::kInt) continue;
      int decli = (int)decl->as_int(-1);
      int varid = (int)vrid->as_int(-1);
      if (decli < 0 || decli >= (int)nodes->size()) continue;  // 悬空由 V-2 报
      const JVal* n = nodes->at(decli);
      std::vector<int> bids; bool has_binding = false;
      bound_vars(n, bids, has_binding);
      const JVal* vrname = vr->find("name");
      std::string name = vrname && vrname->kind()==JVal::kStr ? vrname->as_str() : "";
      // input 例外：decl 指向 root 且名字为 input。
      const JVal* root = w->find("root");
      if (!has_binding && name == "input" && root != nullptr && root->kind() == JVal::kInt &&
          decli == (int)root->as_int(-1)) {
        continue;
      }
      if (!has_binding) {
        add_diag_filled(c, kError, "E_IR_VAR_MISMATCH",
                 "var {name} (id {var}): decl points at node {first}, which binds no variable",
                      {{"name", name}, {"var", itoa(varid)}, {"first", itoa(decli)},
                       {"second", itoa(decli)},
                       {"detail", "decl points at node " + itoa(decli) + ", which binds no variable"}},
                 "V-5 is bidirectional: vars[i].decl must point at a node that actually binds this variable. A mismatch means the IR was truncated, hand-edited, or produced by a frontend that is one version out of step.",
                 {"point decl at the binding node", "regenerate the IR with the current compiler"},
                 loc_of(w), true);
        continue;
      }
      bool found = false;
      for (int b : bids) if (b == varid) { found = true; break; }
      if (!found) {
        std::string idlist = "[";
        for (size_t i = 0; i < bids.size(); ++i) { if (i) idlist += " "; idlist += itoa(bids[i]); }
        idlist += "]";
        add_diag_filled(c, kError, "E_IR_VAR_MISMATCH",
                 "var {name} (id {var}): the binding node {first} binds vars {detail}, expected to include {var}",
                      {{"name", name}, {"var", itoa(varid)}, {"first", itoa(decli)}, {"detail", idlist}},
                 "V-5 is bidirectional: vars[i].decl must point at the node that binds this exact variable.",
                 {"fix the binding node", "regenerate the IR with the current compiler"},
                 loc_of(w), true);
      }
    }
    // 反向：每个绑定节点的变量必须在 vars[] 里，且 decl 回指。
    for (size_t ni = 0; ni < nodes->size(); ++ni) {
      const JVal* n = nodes->at(ni);
      std::vector<int> bids; bool has_binding = false;
      bound_vars(n, bids, has_binding);
      if (bids.empty()) continue;
      std::map<int, bool> seen;
      for (int vid : bids) {
        if (seen[vid]) {
          add_diag_filled(c, kError, "E_IR_VAR_MISMATCH",
                   "node {first} binds var {var} more than once",
                        {{"name", ""}, {"var", itoa(vid)}, {"first", itoa((int)ni)},
                         {"second", itoa((int)ni)},
                         {"detail", "node " + itoa((int)ni) + " binds var " + itoa(vid) + " more than once"}},
                   "A binding node must bind each variable at most once.",
                   {"remove the duplicate binding", "regenerate the IR with the current compiler"},
                   loc_of(n), true);
          continue;
        }
        seen[vid] = true;
        if (vid >= (int)vars->size()) continue;  // 悬空由 V-2 报
        int found_decl = -1;
        for (size_t vi = 0; vi < vars->size(); ++vi) {
          const JVal* v = vars->at(vi);
          const JVal* vidv = v ? v->find("id") : nullptr;
          if (vidv != nullptr && vidv->kind() == JVal::kInt && (int)vidv->as_int(-1) == vid) { found_decl = (int)vi; break; }
        }
        if (found_decl < 0) continue;
        const JVal* vr = vars->at(found_decl);
        const JVal* vdecl = vr ? vr->find("decl") : nullptr;
        if (vdecl != nullptr && vdecl->kind() == JVal::kInt && (int)vdecl->as_int(-1) != (int)ni) {
          add_diag_filled(c, kError, "E_IR_VAR_MISMATCH",
                   "var {name} (id {var}): decl points at node {first}, but this binding node is {second}",
                        {{"name", jstr(vr->find("name"))}, {"var", itoa(vid)},
                         {"first", itoa((int)vdecl->as_int(-1))}, {"second", itoa((int)ni)},
                         {"detail", "decl points at node " + itoa((int)vdecl->as_int(-1)) +
                                    ", but this binding node is " + itoa((int)ni)}},
                   "V-5 is bidirectional: the var's decl must point back at the binding node.",
                   {"fix the var's decl", "regenerate the IR with the current compiler"},
                   loc_of(n), true);
        }
      }
    }
    // 形态互斥：decl 不许同时写 var 与 vars。
    for (size_t ni = 0; ni < nodes->size(); ++ni) {
      const JVal* n = nodes->at(ni);
      const JVal* k = n ? n->find("k") : nullptr;
      if (k != nullptr && k->kind() == JVal::kStr && k->as_str() == "decl" &&
          n->find("var") != nullptr && n->find("vars") != nullptr) {
        const JVal* var = n->find("var");
        int varid = (var && var->kind()==JVal::kInt) ? (int)var->as_int(-1) : 0;
        add_diag_filled(c, kError, "E_IR_VAR_MISMATCH",
                 "decl node {first} has both var and vars; exactly one binding shape is allowed",
                      {{"name", ""}, {"var", itoa(varid)}, {"first", itoa((int)ni)},
                       {"second", itoa((int)ni)},
                       {"detail", "decl node " + itoa((int)ni) + " has both var and vars; exactly one binding shape is allowed"}},
                 "A decl node must use exactly one binding shape (var or vars), not both.",
                 {"use either var or vars", "regenerate the IR with the current compiler"},
                 loc_of(n), true);
      }
    }
  }
}

// 节点直接后继（用于环检测）。只走**控制流边** —— 这是 V-3 要的边集：
// 表达式槽位（args.body / fallback.main / join …）不参与"循环"的定义，
// 把它们并进来会造出假环。
//
// ⚠ 这不是 Go 的 nodeSuccessors。V-12（retry 幂等）要的是**节点引用边**全集，
// 由下面的 node_ref_successors 提供；两者混用会让未证明的 POST 藏进
// 表达式槽位里绕过检查。
void successors(const JVal* n, std::vector<int>& out) {
  if (n == nullptr || n->kind() != JVal::kObj) return;
  auto add = [&](const JVal* p) { if (p != nullptr && p->kind() == JVal::kInt) out.push_back((int)p->as_int(-1)); };
  const JVal* kids = n->find("kids");
  if (kids != nullptr && kids->kind() == JVal::kArr)
    for (size_t i = 0; i < kids->size(); ++i) { const JVal* kd = kids->at(i); add(kd); }
  const JVal* branches = n->find("branches");
  if (branches != nullptr && branches->kind() == JVal::kArr)
    for (size_t i = 0; i < branches->size(); ++i) { const JVal* b = branches->at(i); add(b); }
  add(n->find("e")); add(n->find("c")); add(n->find("then")); add(n->find("else"));
  add(n->find("subj")); add(n->find("default")); add(n->find("body"));
  const JVal* elifs = n->find("elifs");
  if (elifs != nullptr && elifs->kind() == JVal::kArr)
    for (size_t i = 0; i < elifs->size(); ++i) {
      const JVal* e = elifs->at(i);
      add(e ? e->find("c") : nullptr); add(e ? e->find("body") : nullptr);
    }
  const JVal* arms = n->find("arms");
  if (arms != nullptr && arms->kind() == JVal::kArr)
    for (size_t i = 0; i < arms->size(); ++i) {
      const JVal* a = arms->at(i);
      add(a ? a->find("body") : nullptr);
    }
  const JVal* catches = n->find("catches");
  if (catches != nullptr && catches->kind() == JVal::kArr)
    for (size_t i = 0; i < catches->size(); ++i) {
      const JVal* cc = catches->at(i);
      add(cc ? cc->find("body") : nullptr);
    }
  add(n->find("of")); add(n->find("l")); add(n->find("r"));
}

// 节点的**节点表引用全集**（V-12 retry 幂等专用），镜像 Go
// internal/ir/refs.go::nodeSuccessors —— 也就是 appendNodeRefs 里 TableNode
// 的那些边，排除 attempt_ctx.retry_node。
//
// 为什么不复用上面的 successors()：那份是**控制流**边集（V-3 要的正是它），
// 少了 fallback.main、call_tool.args.*、call_model.args.prompt、args[]、
// parallel.join、sleep.ms、until.timeout_ms、stop.data、emit.target/args[]
// 这些表达式槽位。V-12 的契约是"从 retry.body 沿**节点引用边**可达"
// （docs/LANGUAGE.md:1738），少一条边就等于留一个"把未证明的 POST 藏进
// 表达式槽位里绕过幂等检查"的口子 —— refs.go 的注释写的也是同一句话。
//
// result 是节点下标（与 V-12 的 by_id 查表一致）。
void node_ref_successors(const JVal* n, std::vector<int>& out) {
  if (n == nullptr || n->kind() != JVal::kObj) return;
  std::vector<Ref> refs;
  append_node_refs(refs, 0, n, "n");
  for (const Ref& r : refs) {
    if (r.table != kTNode) continue;  // vars / tools / schemas / types 不是节点后继
    // attempt_ctx.retry_node 是编译期打的**反向注解**（指回包含这次调用的
    // retry），不是执行时会走到的边。顺着它走会绕回 retry 节点本身，
    // 在嵌套 retry 的 IR 里还会把别的 retry 子树误算进当前可达集。
    static const std::string kBackRef = "attempt_ctx.retry_node";
    if (r.from.size() >= kBackRef.size() &&
        r.from.compare(r.from.size() - kBackRef.size(), kBackRef.size(), kBackRef) == 0)
      continue;
    out.push_back(r.index);
  }
}

// V-3：环检测（迭代 DFS + 三色标记，与 Go 一致）。
void check_acyclic(Ctx& c, const JVal* doc) {
  const JVal* workflows = doc->find("workflows");
  if (workflows == nullptr || workflows->kind() != JVal::kArr) return;
  for (size_t wi = 0; wi < workflows->size(); ++wi) {
    const JVal* w = workflows->at(wi);
    const JVal* nodes = w ? w->find("nodes") : nullptr;
    if (nodes == nullptr || nodes->kind() != JVal::kArr) continue;
    int nn = (int)nodes->size();
    std::vector<int8_t> color(nn, 0);
    struct Frame { int node; int next; };
    for (int start = 0; start < nn; ++start) {
      if (color[start] != 0) continue;
      std::vector<Frame> stack; stack.push_back({start, 0});
      color[start] = 1;
      while (!stack.empty()) {
        Frame& top = stack.back();
        std::vector<int> succ; successors(nodes->at(top.node), succ);
        if (top.next >= (int)succ.size()) { color[top.node] = 2; stack.pop_back(); continue; }
        int nxt = succ[top.next++];
        if (nxt < 0 || nxt >= nn) continue;  // 悬空由 V-2 报
        switch (color[nxt]) {
          case 0: color[nxt] = 1; stack.push_back({nxt, 0}); break;
          case 1: {
            std::string chain; bool started = false;
            for (auto& f : stack) {
              if (f.node == nxt) started = true;
              if (started) { if (!chain.empty()) chain += " -> "; chain += itoa(f.node); }
            }
            chain += " -> " + itoa(nxt);
            add_diag_filled(c, kError, "E_IR_CYCLE",
                     "workflow {wf}: the node graph has a cycle — {chain}",
                          {{"wf", jstr(w->find("name"))}, {"chain", chain}, {"edge", itoa(top.node) + " -> " + itoa(nxt)}},
                     "The IR must be a DAG (IR.md V-3). With a cycle the executor can re-enter the same node forever, so neither a deadline nor a budget can bound the run.",
                     {"break the cycle by removing the edge " + itoa(top.node) + " -> " + itoa(nxt),
                      "if you need repetition, use `retry` — it is bounded"},
                     loc_of(nodes->at(top.node)), true);
            goto done_wf;  // 一个 workflow 报一条环就够
          }
        }
      }
    }
    done_wf:;
  }
}

// checkMutAssign：const 变量不可再赋值（E_IR_ASSIGN_IMMUTABLE）。
void check_mut_assign(Ctx& c, const JVal* doc) {
  const JVal* workflows = doc->find("workflows");
  if (workflows == nullptr || workflows->kind() != JVal::kArr) return;
  for (size_t wi = 0; wi < workflows->size(); ++wi) {
    const JVal* w = workflows->at(wi);
    const JVal* nodes = w ? w->find("nodes") : nullptr;
    if (nodes == nullptr || nodes->kind() != JVal::kArr) continue;
    std::map<int, bool> mut;  // var id → 是否可变
    for (size_t i = 0; i < nodes->size(); ++i) {
      const JVal* n = nodes->at(i);
      const JVal* k = n ? n->find("k") : nullptr;
      if (k != nullptr && k->kind() == JVal::kStr && k->as_str() == "decl") {
        const JVal* var = n->find("var");
        if (var != nullptr && var->kind() == JVal::kInt) {
          const JVal* con = n->find("const");
          bool is_const = (con != nullptr && con->kind() == JVal::kBool) ? con->as_bool(false) : false;
          mut[(int)var->as_int(-1)] = !is_const;
        }
      }
    }
    for (size_t i = 0; i < nodes->size(); ++i) {
      const JVal* n = nodes->at(i);
      const JVal* k = n ? n->find("k") : nullptr;
      if (k == nullptr || k->kind() != JVal::kStr || k->as_str() != "assign") continue;
      const JVal* var = n->find("var");
      if (var == nullptr || var->kind() != JVal::kInt) continue;
      int vid = (int)var->as_int(-1);
      auto it = mut.find(vid);
      if (it != mut.end() && !it->second) {
        add_diag_filled(c, kError, "E_IR_ASSIGN_IMMUTABLE",
                 "Cannot mutate const variable", {},
                 "Immutable bindings are the basis for static inference: only variables declared `mut` may change at runtime.",
                 {"Remove `const`, or assign to a new variable instead."},
                 loc_of(n), true);
      }
    }
  }
}

// V-9：exported ↔ exports 双向。
void check_export_bidirectional(Ctx& c, const JVal* doc) {
  const JVal* workflows = doc->find("workflows");
  const JVal* exports = doc->find("exports");
  if (workflows == nullptr || workflows->kind() != JVal::kArr) return;
  std::map<std::string, bool> exported;
  if (exports != nullptr) {
    const JVal* ew = exports->find("workflows");
    if (ew != nullptr && ew->kind() == JVal::kArr)
      for (size_t i = 0; i < ew->size(); ++i) {
        const JVal* s = ew->at(i);
        if (s && s->kind() == JVal::kStr) exported[s->as_str()] = true;
      }
  }
  std::map<std::string, int> wf_by_name;
  for (size_t wi = 0; wi < workflows->size(); ++wi) {
    const JVal* w = workflows->at(wi);
    const JVal* nm = w ? w->find("name") : nullptr;
    if (nm && nm->kind() == JVal::kStr) wf_by_name[nm->as_str()] = (int)wi;
  }
  for (size_t wi = 0; wi < workflows->size(); ++wi) {
    const JVal* w = workflows->at(wi);
    const JVal* nm = w ? w->find("name") : nullptr;
    const JVal* exp = w ? w->find("exported") : nullptr;
    if (nm == nullptr || nm->kind() != JVal::kStr || exp == nullptr) continue;
    bool is_exported = exp->as_bool(false);
    bool in_table = exported.count(nm->as_str()) > 0;
    if (is_exported == in_table) continue;
    std::string where, verb, want, action, target;
    if (is_exported) { where = "exports.workflows"; verb = "does not list"; want = "false"; action = "remove"; target = "from exports.workflows"; }
    else { where = "exports.workflows"; verb = "already lists"; want = "true"; action = "add"; target = "to exports.workflows"; }
    add_diag_filled(c, kError, "E_IR_EXPORT_MISMATCH",
             "workflow {name} is exported={exported} but {where} {verb} it ({want}: {action} {target})",
                  {{"name", nm->as_str()}, {"exported", is_exported ? "true" : "false"}, {"where", where},
                   {"verb", verb}, {"want", want}, {"action", action}, {"target", target}},
             "V-9 is bidirectional: a workflow's exported flag and its presence in exports.workflows must agree.",
             {"make the workflow's exported flag and exports.workflows agree", "regenerate the IR with the current compiler"},
             loc_of(w), true);
  }
  // 反向：exports.workflows 里出现了不存在的 workflow 名。
  if (exports != nullptr) {
    const JVal* ew = exports->find("workflows");
    if (ew != nullptr && ew->kind() == JVal::kArr)
      for (size_t i = 0; i < ew->size(); ++i) {
        const JVal* s = ew->at(i);
        if (s == nullptr || s->kind() != JVal::kStr) continue;
        if (wf_by_name.count(s->as_str()) == 0) {
          add_diag_filled(c, kError, "E_IR_EXPORT_MISMATCH",
                   "workflow {name} is listed in exports.workflows but {where} {verb} it ({want}: {action} {target})",
                        {{"name", s->as_str()}, {"exported", "?"}, {"where", "workflows[]"}, {"verb", "has no entry for"},
                         {"want", "n/a"}, {"action", "remove"}, {"target", "from exports.workflows"}},
                   "V-9 is bidirectional: every name in exports.workflows must be a real workflow.",
                   {"remove " + s->as_str() + " from exports.workflows", "regenerate the IR with the current compiler"},
                   LocRec{0,0,0,0,0}, false);
          c.diags.back().module = c.unit_name;
        }
      }
  }
}

// V-10：至多一个 is_entry，且必须 exported。
void check_entry(Ctx& c, const JVal* doc) {
  const JVal* workflows = doc->find("workflows");
  if (workflows == nullptr || workflows->kind() != JVal::kArr) return;
  std::vector<int> entries;
  for (size_t wi = 0; wi < workflows->size(); ++wi) {
    const JVal* w = workflows->at(wi);
    const JVal* e = w ? w->find("is_entry") : nullptr;
    if (e != nullptr && e->as_bool(false)) entries.push_back((int)wi);
  }
  if (entries.size() > 1) {
    std::string names;
    for (size_t i = 0; i < entries.size(); ++i) {
      if (i) names += ", ";
      const JVal* w = workflows->at(entries[i]);
      names += jstr(w->find("name"));
    }
    const JVal* w = workflows->at(entries[1]);
    add_diag_filled(c, kError, "E_IR_ENTRY",
             "workflow {name}: {reason}",
                  {{"name", names}, {"reason", itoa((int)entries.size()) + " workflows are marked as entry: " + names}},
             "V-10 allows at most one entry workflow. More than one means there is no single default entry point.",
             {"mark exactly one workflow as entry", "regenerate the IR with the current compiler"},
             loc_of(w), true);
  }
  for (int wi : entries) {
    const JVal* w = workflows->at(wi);
    const JVal* e = w ? w->find("exported") : nullptr;
    if (!(e != nullptr && e->as_bool(false))) {
      add_diag_filled(c, kError, "E_IR_ENTRY",
               "workflow {name}: the entry workflow is not exported",
                    {{"name", jstr(w->find("name"))}, {"reason", "the entry workflow is not exported"}},
               "V-10 requires the entry workflow to be exported so it can be referenced by name.",
               {"export the entry workflow", "regenerate the IR with the current compiler"},
               loc_of(w), true);
    }
  }
}

// ---- 阶段 3：派生字段复算（mirror internal/ir/bounds.go）-----------------
const int kBoundUnrec = -2;
const int kBoundUnk = -1;

int absorb_sum(int acc, int s) {
  if (acc == kBoundUnrec || s == kBoundUnrec) return kBoundUnrec;
  if (acc == kBoundUnk || s == kBoundUnk) return kBoundUnk;
  return acc + s;
}
int absorb_max(int acc, int s) {
  if (acc == kBoundUnrec || s == kBoundUnrec) return kBoundUnrec;
  if (acc == kBoundUnk || s == kBoundUnk) return kBoundUnk;
  if (s > acc) return s;
  return acc;
}
int scaled_bound(int s, int64_t factor) {
  if (s < 0) return s;
  if (factor < 0) return kBoundUnk;
  int64_t prod = (int64_t)s * factor;
  if (prod > INT64_MAX) return kBoundUnk;
  return (int)prod;
}

bool parse_int64(const std::string& s, int64_t* out) {
  if (s.empty()) return false;
  size_t i = 0;
  while (i < s.size() && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r')) ++i;
  bool neg = false;
  if (i < s.size() && s[i] == '+') ++i;
  else if (i < s.size() && s[i] == '-') { neg = true; ++i; }
  if (i >= s.size()) return false;
  int64_t val = 0; size_t start = i;
  while (i < s.size()) {
    char ch = s[i];
    if (ch < '0' || ch > '9') break;
    int64_t d = ch - '0';
    if (val > (INT64_MAX - d) / 10) return false;
    val = val * 10 + d; ++i;
  }
  if (i == start) return false;
  while (i < s.size() && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r')) ++i;
  if (i != s.size()) return false;
  *out = neg ? -val : val;
  return true;
}

bool lit_int_value(const JVal* n, int64_t* out) {
  if (n == nullptr || n->kind() != JVal::kObj) return false;
  const JVal* k = n->find("k");
  const JVal* t = n->find("t");
  const JVal* v = n->find("v");
  if (k == nullptr || k->kind() != JVal::kStr || k->as_str() != "lit") return false;
  if (t == nullptr || t->kind() != JVal::kStr || t->as_str() != "int") return false;
  if (v == nullptr) return false;
  if (v->kind() == JVal::kInt) { *out = v->as_int(0); return true; }
  if (v->kind() == JVal::kStr) { int64_t x; if (parse_int64(v->as_str(), &x)) { *out = x; return true; } }
  return false;
}

int for_trip_count(const std::map<int,int>& pos_by_id, const std::vector<const JVal*>& nodes, const JVal* n) {
  if (n == nullptr) return kBoundUnk;
  const JVal* initf = n->find("init");
  const JVal* condf = n->find("cond");
  if (initf == nullptr || condf == nullptr) return kBoundUnk;
  int64_t delta = 1;
  const JVal* df = n->find("delta");
  if (df != nullptr && df->kind() == JVal::kInt) delta = df->as_int(1);
  if (delta <= 0) return kBoundUnk;
  auto resolve = [&](const JVal* f) -> const JVal* {
    if (f == nullptr || f->kind() != JVal::kInt) return nullptr;
    auto it = pos_by_id.find((int)f->as_int(-1));
    if (it == pos_by_id.end()) return nullptr;
    int p = it->second;
    if (p < 0 || p >= (int)nodes.size()) return nullptr;
    return nodes[p];
  };
  const JVal* init = resolve(initf);
  if (init == nullptr) return kBoundUnk;
  int64_t start; if (!lit_int_value(init, &start)) return kBoundUnk;
  const JVal* cond = resolve(condf);
  if (cond == nullptr || cond->kind() != JVal::kObj) return kBoundUnk;
  const JVal* op = cond->find("op");
  const JVal* rf = cond->find("r");
  if (op == nullptr || op->kind() != JVal::kStr || rf == nullptr) return kBoundUnk;
  bool inclusive = false;
  std::string ops = op->as_str();
  if (ops == "lt") inclusive = false;
  else if (ops == "le") inclusive = true;
  else return kBoundUnk;
  const JVal* rhs = resolve(rf);
  if (rhs == nullptr) return kBoundUnk;
  int64_t stop; if (!lit_int_value(rhs, &stop)) return kBoundUnk;
  int64_t trip;
  if (!inclusive) { if (stop <= start) return 0; trip = (stop - start + delta - 1) / delta; }
  else { if (stop < start) return 0; trip = (stop - start) / delta + 1; }
  if (trip < 0 || trip > INT64_MAX) return kBoundUnk;
  return (int)trip;
}

bool agent_static_steps(const JVal* agent, int* steps) {
  if (agent == nullptr || agent->kind() != JVal::kObj) return false;
  const JVal* ms = agent->find("max_steps");
  if (ms == nullptr || ms->kind() != JVal::kInt) return false;
  int64_t v = ms->as_int(0);
  if (v <= 0) return false;
  *steps = (int)v;
  return true;
}

int child_pos(const JVal* n, const char* key, const std::map<int,int>& pos_by_id) {
  if (n == nullptr) return -1;
  const JVal* f = n->find(key);
  if (f == nullptr || f->kind() != JVal::kInt) return -1;
  auto it = pos_by_id.find((int)f->as_int(-1));
  return it == pos_by_id.end() ? -1 : it->second;
}

int span_rec(int pos, const std::vector<const JVal*>& nodes, const std::map<int,int>& pos_by_id, std::vector<int>& memo) {
  if (pos < 0 || pos >= (int)nodes.size()) return 0;
  if (memo[pos] != -100) return memo[pos];
  memo[pos] = kBoundUnk;
  const JVal* n = nodes[pos];
  if (n == nullptr || n->kind() != JVal::kObj) { memo[pos] = 0; return 0; }
  const JVal* kf = n->find("k");
  if (kf == nullptr || kf->kind() != JVal::kStr) { memo[pos] = kBoundUnrec; return kBoundUnrec; }
  std::string k = kf->as_str();
  int v = 0;
  auto cpid = [&](const char* key) -> int { return child_pos(n, key, pos_by_id); };
  if (k == "lit" || k == "var" || k == "interp" || k == "nop") v = 0;
  else if (k == "field" || k == "index") { int of = cpid("of"); if (of >= 0) v = absorb_sum(v, span_rec(of, nodes, pos_by_id, memo)); }
  else if (k == "stop") { int e = cpid("e"); if (e >= 0) v = absorb_sum(v, span_rec(e, nodes, pos_by_id, memo)); int d = cpid("data"); if (d >= 0) v = absorb_sum(v, span_rec(d, nodes, pos_by_id, memo)); }
  else if (k == "mem_txn") { int e = cpid("e"); if (e >= 0) v = absorb_sum(v, span_rec(e, nodes, pos_by_id, memo)); }
  else if (k == "emit") { const JVal* args = n->find("args"); if (args != nullptr && args->kind()==JVal::kArr) for (size_t i=0;i<args->size();++i){ const JVal* a=args->at(i); if (a && a->kind()==JVal::kInt) v = absorb_sum(v, span_rec((int)a->as_int(-1), nodes, pos_by_id, memo)); } }
  else if (k == "on") v = absorb_sum(v, 0);
  else if (k == "sleep") { int ms = cpid("ms"); if (ms >= 0) v = absorb_sum(v, span_rec(ms, nodes, pos_by_id, memo)); }
  else if (k == "until") { int tm = cpid("timeout_ms"); if (tm >= 0) v = absorb_sum(v, span_rec(tm, nodes, pos_by_id, memo)); }
  else if (k == "cast" || k == "unop" || k == "len") { int e = cpid("e"); if (e >= 0) v = absorb_sum(v, span_rec(e, nodes, pos_by_id, memo)); }
  else if (k == "binop") { int l = cpid("l"); if (l >= 0) v = absorb_sum(v, span_rec(l, nodes, pos_by_id, memo)); int r = cpid("r"); if (r >= 0) v = absorb_sum(v, span_rec(r, nodes, pos_by_id, memo)); }
  else if (k == "at") { int of = cpid("of"); if (of >= 0) v = absorb_sum(v, span_rec(of, nodes, pos_by_id, memo)); int i = cpid("i"); if (i >= 0) v = absorb_sum(v, span_rec(i, nodes, pos_by_id, memo)); }
  else if (k == "array_lit") { const JVal* kids = n->find("kids"); if (kids && kids->kind()==JVal::kArr) for (size_t i=0;i<kids->size();++i){ const JVal* kd=kids->at(i); if (kd && kd->kind()==JVal::kInt) v = absorb_sum(v, span_rec((int)kd->as_int(-1), nodes, pos_by_id, memo)); } }
  else if (k == "call_model" || k == "call_tool" || k == "call_fn" || k == "call_native" || k == "store_op") v = 1;
  else if (k == "decl" || k == "destructure" || k == "assign" || k == "return" || k == "exprstmt") { int e = cpid("e"); if (e >= 0) v = absorb_sum(v, span_rec(e, nodes, pos_by_id, memo)); }
  else if (k == "seq") { const JVal* kids = n->find("kids"); if (kids && kids->kind()==JVal::kArr) for (size_t i=0;i<kids->size();++i){ const JVal* kd=kids->at(i); if (kd && kd->kind()==JVal::kInt) v = absorb_sum(v, span_rec((int)kd->as_int(-1), nodes, pos_by_id, memo)); } }
  else if (k == "if") {
    int best = 0; int then = cpid("then"); if (then >= 0) best = absorb_max(best, span_rec(then, nodes, pos_by_id, memo));
    const JVal* elifs = n->find("elifs"); if (elifs && elifs->kind()==JVal::kArr) for (size_t i=0;i<elifs->size();++i){ const JVal* e=elifs->at(i); const JVal* b=e?e->find("body"):nullptr; if (b && b->kind()==JVal::kInt) best = absorb_max(best, span_rec((int)b->as_int(-1), nodes, pos_by_id, memo)); }
    int els = cpid("else"); if (els >= 0) best = absorb_max(best, span_rec(els, nodes, pos_by_id, memo)); v = absorb_sum(1, best);
  }
  else if (k == "match") {
    int best = 0; const JVal* arms = n->find("arms"); if (arms && arms->kind()==JVal::kArr) for (size_t i=0;i<arms->size();++i){ const JVal* a=arms->at(i); const JVal* b=a?a->find("body"):nullptr; if (b && b->kind()==JVal::kInt) best = absorb_max(best, span_rec((int)b->as_int(-1), nodes, pos_by_id, memo)); }
    int def = cpid("default"); if (def >= 0) best = absorb_max(best, span_rec(def, nodes, pos_by_id, memo)); v = absorb_sum(1, best);
  }
  else if (k == "parallel") {
    v = 1; const JVal* branches = n->find("branches"); if (branches && branches->kind()==JVal::kArr) for (size_t i=0;i<branches->size();++i){ const JVal* b=branches->at(i); if (b && b->kind()==JVal::kInt) v = absorb_sum(v, span_rec((int)b->as_int(-1), nodes, pos_by_id, memo)); }
  }
  else if (k == "retry") { int body = cpid("body"); int bv = body >= 0 ? span_rec(body, nodes, pos_by_id, memo) : 0; int attempts = 1; const JVal* nf = n->find("n"); if (nf && nf->kind()==JVal::kInt){ int64_t nv=nf->as_int(-1); if (nv>=0) attempts=(int)nv+1; } v = absorb_sum(1, scaled_bound(bv, attempts)); }
  else if (k == "for") { int body = cpid("body"); int bv = body >= 0 ? span_rec(body, nodes, pos_by_id, memo) : 0; v = absorb_sum(1, scaled_bound(bv, for_trip_count(pos_by_id, nodes, n))); }
  else if (k == "while") v = kBoundUnk;
  else if (k == "break" || k == "continue") v = 0;
  else if (k == "agent") {
    const JVal* agent = n->find("agent"); int steps;
    if (agent == nullptr || !agent_static_steps(agent, &steps)) v = kBoundUnk;
    else { int kk = 1; const JVal* mpt = agent->find("max_parallel_tools"); if (mpt && mpt->kind()==JVal::kInt){ int64_t kv=mpt->as_int(1); if (kv>=1) kk=(int)kv; } v = (1+kk)*steps; }
  }
  else if (k == "try") {
    v = 1; int body = cpid("body"); if (body >= 0) v = absorb_sum(v, span_rec(body, nodes, pos_by_id, memo));
    int best = 0; const JVal* catches = n->find("catches"); if (catches && catches->kind()==JVal::kArr) for (size_t i=0;i<catches->size();++i){ const JVal* cc=catches->at(i); const JVal* b=cc?cc->find("body"):nullptr; if (b && b->kind()==JVal::kInt) best = absorb_max(best, span_rec((int)b->as_int(-1), nodes, pos_by_id, memo)); }
    v = absorb_sum(v, best);
  }
  else if (k == "fallback") { v = 1; int mainn = cpid("main"); if (mainn >= 0) v = absorb_sum(v, span_rec(mainn, nodes, pos_by_id, memo)); int body = cpid("body"); if (body >= 0) v = absorb_sum(v, span_rec(body, nodes, pos_by_id, memo)); }
  else v = kBoundUnrec;
  memo[pos] = v;
  return v;
}

struct CallPair { int m, t; };
CallPair calls_rec(int pos, const std::vector<const JVal*>& nodes, const std::map<int,int>& pos_by_id, std::vector<CallPair>& memo) {
  if (pos < 0 || pos >= (int)nodes.size()) return {0, 0};
  if (memo[pos].m != -100) return memo[pos];
  memo[pos] = {kBoundUnk, kBoundUnk};
  const JVal* n = nodes[pos];
  if (n == nullptr || n->kind() != JVal::kObj) { memo[pos] = {0,0}; return {0,0}; }
  const JVal* kf = n->find("k");
  if (kf == nullptr || kf->kind() != JVal::kStr) { memo[pos] = {kBoundUnrec, kBoundUnrec}; return memo[pos]; }
  std::string k = kf->as_str();
  CallPair r = {0, 0};
  auto cpid = [&](const char* key) -> int { return child_pos(n, key, pos_by_id); };
  auto add = [&](CallPair a, CallPair b) -> CallPair { return { absorb_sum(a.m, b.m), absorb_sum(a.t, b.t) }; };
  auto mx  = [&](CallPair a, CallPair b) -> CallPair { return { absorb_max(a.m, b.m), absorb_max(a.t, b.t) }; };
  auto mul = [&](CallPair a, int64_t nn) -> CallPair { return { scaled_bound(a.m, nn), scaled_bound(a.t, nn) }; };
  if (k == "call_model") r = {1, 0};
  else if (k == "call_tool") r = {0, 1};
  else if (k == "stop" || k == "mem_txn" || k == "emit" || k == "on" || k == "sleep" || k == "until") r = {0,0};
  else if (k == "lit" || k == "var" || k == "interp" || k == "nop" || k == "break" || k == "continue") r = {0,0};
  else if (k == "call_fn" || k == "call_native") r = {0,0};
  else if (k == "store_op") { const JVal* op = n->find("op"); if (op && op->kind()==JVal::kStr){ std::string ops=op->as_str(); if (ops=="vector.put"||ops=="vector.query") r={1,0}; } }
  else if (k == "field" || k == "index") { int of = cpid("of"); if (of>=0) r = calls_rec(of, nodes, pos_by_id, memo); }
  else if (k == "cast" || k == "unop" || k == "len") { int e = cpid("e"); if (e>=0) r = calls_rec(e, nodes, pos_by_id, memo); }
  else if (k == "binop") { int l = cpid("l"); if (l>=0) r = add(calls_rec(l, nodes, pos_by_id, memo), r); int rr = cpid("r"); if (rr>=0) r = add(calls_rec(rr, nodes, pos_by_id, memo), r); }
  else if (k == "at") { int of = cpid("of"); if (of>=0) r = add(calls_rec(of, nodes, pos_by_id, memo), r); int i = cpid("i"); if (i>=0) r = add(calls_rec(i, nodes, pos_by_id, memo), r); }
  else if (k == "array_lit") { const JVal* kids = n->find("kids"); if (kids && kids->kind()==JVal::kArr) for (size_t i=0;i<kids->size();++i){ const JVal* kd=kids->at(i); if (kd && kd->kind()==JVal::kInt) r = add(calls_rec((int)kd->as_int(-1), nodes, pos_by_id, memo), r); } }
  else if (k == "decl" || k == "destructure" || k == "assign" || k == "return" || k == "exprstmt") { int e = cpid("e"); if (e>=0) r = calls_rec(e, nodes, pos_by_id, memo); }
  else if (k == "seq") { const JVal* kids = n->find("kids"); if (kids && kids->kind()==JVal::kArr) for (size_t i=0;i<kids->size();++i){ const JVal* kd=kids->at(i); if (kd && kd->kind()==JVal::kInt) r = add(calls_rec((int)kd->as_int(-1), nodes, pos_by_id, memo), r); } }
  else if (k == "if") {
    int then = cpid("then"); if (then>=0) r = calls_rec(then, nodes, pos_by_id, memo);
    const JVal* elifs = n->find("elifs"); if (elifs && elifs->kind()==JVal::kArr) for (size_t i=0;i<elifs->size();++i){ const JVal* e=elifs->at(i); const JVal* b=e?e->find("body"):nullptr; if (b && b->kind()==JVal::kInt) r = mx(r, calls_rec((int)b->as_int(-1), nodes, pos_by_id, memo)); }
    int els = cpid("else"); if (els>=0) r = mx(r, calls_rec(els, nodes, pos_by_id, memo));
  }
  else if (k == "match") { const JVal* arms = n->find("arms"); if (arms && arms->kind()==JVal::kArr) for (size_t i=0;i<arms->size();++i){ const JVal* a=arms->at(i); const JVal* b=a?a->find("body"):nullptr; if (b && b->kind()==JVal::kInt) r = mx(r, calls_rec((int)b->as_int(-1), nodes, pos_by_id, memo)); } int def = cpid("default"); if (def>=0) r = mx(r, calls_rec(def, nodes, pos_by_id, memo)); }
  else if (k == "parallel") { const JVal* branches = n->find("branches"); if (branches && branches->kind()==JVal::kArr) for (size_t i=0;i<branches->size();++i){ const JVal* b=branches->at(i); if (b && b->kind()==JVal::kInt) r = add(r, calls_rec((int)b->as_int(-1), nodes, pos_by_id, memo)); } }
  else if (k == "for") { int trip = for_trip_count(pos_by_id, nodes, n); if (trip<0) r = {kBoundUnk, kBoundUnk}; else { int body = cpid("body"); r = (body>=0)? calls_rec(body, nodes, pos_by_id, memo) : CallPair{0,0}; r = mul(r, trip); } }
  else if (k == "while") r = {kBoundUnk, kBoundUnk};
  else if (k == "agent") { const JVal* agent = n->find("agent"); int steps; if (agent==nullptr || !agent_static_steps(agent, &steps)) r = {kBoundUnk, kBoundUnk}; else { int kk=1; const JVal* mpt=agent->find("max_parallel_tools"); if (mpt && mpt->kind()==JVal::kInt){ int64_t kv=mpt->as_int(1); if (kv>=1) kk=(int)kv; } r = {steps, kk*steps}; } }
  else if (k == "try") { int body = cpid("body"); if (body>=0) r = calls_rec(body, nodes, pos_by_id, memo); CallPair best={0,0}; const JVal* catches = n->find("catches"); if (catches && catches->kind()==JVal::kArr) for (size_t i=0;i<catches->size();++i){ const JVal* cc=catches->at(i); const JVal* b=cc?cc->find("body"):nullptr; if (b && b->kind()==JVal::kInt) best = mx(best, calls_rec((int)b->as_int(-1), nodes, pos_by_id, memo)); } r = add(r, best); }
  else if (k == "retry") { int attempts=1; const JVal* nf=n->find("n"); if (nf && nf->kind()==JVal::kInt){ int64_t nv=nf->as_int(-1); if (nv>=0) attempts=(int)nv+1; } int body=cpid("body"); if (body>=0) r = mul(calls_rec(body, nodes, pos_by_id, memo), attempts); }
  else if (k == "fallback") { int mainn = cpid("main"); if (mainn>=0) r = add(r, calls_rec(mainn, nodes, pos_by_id, memo)); int body = cpid("body"); if (body>=0) r = add(r, calls_rec(body, nodes, pos_by_id, memo)); }
  else r = {kBoundUnrec, kBoundUnrec};
  memo[pos] = r;
  return r;
}

void stage_derived(Ctx& c, const JVal* doc) {
  const JVal* workflows = doc->find("workflows");
  if (workflows == nullptr || workflows->kind() != JVal::kArr) return;
  for (size_t wi = 0; wi < workflows->size(); ++wi) {
    const JVal* w = workflows->at(wi);
    const JVal* nodesv = w ? w->find("nodes") : nullptr;
    const JVal* boundsv = w ? w->find("bounds") : nullptr;
    if (nodesv == nullptr || nodesv->kind() != JVal::kArr) continue;
    std::vector<const JVal*> nodes(nodesv->size());
    std::map<int,int> pos_by_id;
    // 派生复算的图遍历必须与 Go bounds.go 同义：root / kids / 各 child 引用的
    // 整数都当作**数组下标**直接索引 w.Nodes[id]，不是"按 node.id 查表"。
    // 合法 IR 里 id == 下标，两种写法看不出差别；但被篡改的 IR 里 id 会漂，
    // 若这里建 id→下标映射，复算会静默走到错的节点上，绕过上界校验。
    // 因此这张表刻意建恒等映射：查 i 就是 nodes[i]。
    for (size_t i = 0; i < nodesv->size(); ++i) {
      nodes[i] = nodesv->at(i);
      pos_by_id[(int)i] = (int)i;
    }
    const JVal* rootv = w->find("root");
    int root_pos = -1;
    if (rootv != nullptr && rootv->kind() == JVal::kInt) {
      auto it = pos_by_id.find((int)rootv->as_int(-1));
      if (it != pos_by_id.end()) root_pos = it->second;
    }
    // root 悬空时 Go 的 RecomputeMaxSpans/RecomputeCalls 返回 boundUnrecognized，
    // stageDerived 因此仍会报 E_IR_RECOMPUTE_DRIFT（"这个上界没被验证过"）。
    // 此处不能 continue —— 那会整段跳过派生阶段，让 C++ 默认路径比 Go 少报
    // 一条安全诊断，等于悄悄放宽了校验。
    const bool root_missing = (root_pos < 0);
    int max_steps = 0, max_spans = 0, max_model_calls = 0, max_tool_calls = 0;
    if (boundsv != nullptr && boundsv->kind() == JVal::kObj) {
      max_steps = (int)jint(boundsv->find("max_steps"), 0);
      max_spans = (int)jint(boundsv->find("max_spans"), 0);
      max_model_calls = (int)jint(boundsv->find("max_model_calls"), 0);
      max_tool_calls = (int)jint(boundsv->find("max_tool_calls"), 0);
    }
    if (max_steps > (int)nodes.size()) {
      add_diag_filled(c, kError, "E_IR_RECOMPUTE_DRIFT",
               "bounds.max_steps claims {claimed} but node count is {actual} or less",
                    {{"field", "bounds.max_steps"}, {"claimed", itoa(max_steps)}, {"actual", itoa((int)nodes.size())}},
               "bounds.max_steps cannot exceed the number of nodes (each node executes at most once in an acyclic graph). A violation means the IR was truncated, hand-edited, or produced by a frontend that is one version out of step.",
               {"fix max_steps", "regenerate the IR with the current compiler"},
               loc_of(w), true);
    }
    std::vector<int> span_memo(nodes.size(), -100);
    std::vector<CallPair> call_memo(nodes.size(), {-100, -100});
    // root 缺失 ⇒ 与 Go 的 boundUnrecognized 同义；其余检查照常跑完。
    int got = root_missing ? kBoundUnrec : span_rec(root_pos, nodes, pos_by_id, span_memo);
    CallPair cp = root_missing ? CallPair{kBoundUnrec, kBoundUnrec}
                               : calls_rec(root_pos, nodes, pos_by_id, call_memo);
    if (got == kBoundUnrec) {
      add_diag_filled(c, kError, "E_IR_RECOMPUTE_DRIFT",
               "{field} says {claimed}, but recomputing from the IR gives {actual}",
               {{"field", "bounds.max_spans"}, {"claimed", itoa(max_spans)},
                {"actual", "nothing: the recompute met a node kind it does not recognize, "
                           "so this bound was never verified. That is a Hermes compiler bug."}},
               "bounds.max_spans was never verified: the recompute met a node kind it does not recognize, so this bound was never verified. That is a Hermes compiler bug.",
               {"report this as a compiler bug", "regenerate the IR with the current compiler"},
               loc_of(w), true);
    } else if (got >= 0 && got != max_spans && max_spans <= 64*max_steps+1) {
      add_diag_filled(c, kError, "E_IR_RECOMPUTE_DRIFT",
               "bounds.max_spans claims {claimed} but recompute yields {actual}",
                    {{"field", "bounds.max_spans"}, {"claimed", itoa(max_spans)}, {"actual", itoa(got)}},
               "bounds.max_spans must match the value recomputed from the node graph. A mismatch means the IR was truncated, hand-edited, or produced by a frontend that is one version out of step.",
               {"fix max_spans", "regenerate the IR with the current compiler"},
               loc_of(w), true);
    }
    if (cp.m == kBoundUnrec || cp.t == kBoundUnrec) {
      std::string field = "bounds.max_model_calls";
      std::string claimed = itoa(max_model_calls);
      if (cp.m != kBoundUnrec) { field = "bounds.max_tool_calls"; claimed = itoa(max_tool_calls); }
      add_diag_filled(c, kError, "E_IR_RECOMPUTE_DRIFT",
               "{field} says {claimed}, but recomputing from the IR gives {actual}",
               {{"field", field}, {"claimed", claimed},
                {"actual", "nothing: the call-count recompute met a node kind it does not recognize, "
                           "so this bound was never verified. That is a Hermes compiler bug."}},
               "the call-count recompute met a node kind it does not recognize, so this bound was never verified. That is a Hermes compiler bug.",
               {"report this as a compiler bug", "regenerate the IR with the current compiler"},
               loc_of(w), true);
    }
    if (max_spans > 64*max_steps+1) {
      add_diag_filled(c, kError, "E_IR_RECOMPUTE_DRIFT",
               "bounds.max_spans claims {claimed} but may be at most {actual}",
                    {{"field", "bounds.max_spans"}, {"claimed", itoa(max_spans)}, {"actual", itoa(64*max_steps+1)}},
               "IR.md §5.1: max_spans <= 64*max_steps + 1.",
               {"fix max_spans", "regenerate the IR with the current compiler"},
               loc_of(w), true);
    }
  }
}

// checkVersions：ir_version 与 language_version 都必须 == 2。
void check_versions(Ctx& c, const JVal* doc) {
  int ir_version = (int)jint(doc->find("ir_version"), 0);
  if (ir_version != 2) {
    add_diag_filled(c, kError, "E_IR_VERSION",
             "{field} is {got}, expected {want}",
                  {{"field", "ir_version"}, {"got", itoa(ir_version)}, {"want", "2"}},
             "This compiler only accepts IR version 2.x (2.0.0 is the LTS). Older IR is rejected outright.",
             {"regenerate the IR with the current compiler"},
             LocRec{0,0,0,0,0}, false);
    c.diags.back().module = c.unit_name;
  }
  const JVal* unit = doc->find("unit");
  int lang_version = (unit != nullptr) ? (int)jint(unit->find("language_version"), 0) : 0;
  if (lang_version != 2) {
    add_diag_filled(c, kError, "E_IR_VERSION",
             "{field} is {got}, expected {want}",
                  {{"field", "language_version"}, {"got", itoa(lang_version)}, {"want", "2"}},
             "This compiler only accepts language version 2.x. Older language versions are rejected outright.",
             {"regenerate the IR with the current compiler"},
             LocRec{0,0,0,0,0}, false);
    c.diags.back().module = c.unit_name;
  }
}

// E5007 产物侧：retry 子树内有无幂等证明的 call_tool。
void check_retry_idempotency(Ctx& c, const JVal* doc, const Index& idx) {
  const JVal* workflows = doc->find("workflows");
  if (workflows == nullptr || workflows->kind() != JVal::kArr) return;
  for (size_t wi = 0; wi < workflows->size(); ++wi) {
    const JVal* w = workflows->at(wi);
    const JVal* nodes = w ? w->find("nodes") : nullptr;
    if (nodes == nullptr || nodes->kind() != JVal::kArr) continue;
    std::map<int, const JVal*> by_id;
    for (size_t i = 0; i < nodes->size(); ++i) {
      const JVal* n = nodes->at(i);
      const JVal* idf = n ? n->find("id") : nullptr;
      if (idf != nullptr && idf->kind() == JVal::kInt) by_id[(int)idf->as_int(-1)] = n;
    }
    std::map<int, bool> reported;
    for (size_t ri = 0; ri < nodes->size(); ++ri) {
      const JVal* r = nodes->at(ri);
      const JVal* k = r ? r->find("k") : nullptr;
      if (k == nullptr || k->kind() != JVal::kStr || k->as_str() != "retry") continue;
      const JVal* body = r->find("body");
      if (body == nullptr || body->kind() != JVal::kInt) continue;
      std::map<int, bool> seen;
      std::vector<int> stack; stack.push_back((int)body->as_int(-1));
      while (!stack.empty()) {
        int id = stack.back(); stack.pop_back();
        if (seen[id]) continue;
        seen[id] = true;
        const JVal* n = by_id[id];
        if (n == nullptr) continue;
        const JVal* nk = n->find("k");
        if (nk != nullptr && nk->kind() == JVal::kStr && nk->as_str() == "call_tool" && !reported[id]) {
          // 这里**不能**用 continue 收尾：Go 的 checkToolIdemAtRetry 是"从助手
          // 函数返回"，遍历边的那一步照跑。用 continue 会把这条 call_tool 的
          // 后继整段跳过 —— 一个已证明的外层 call_tool 里嵌一个未证明的内层
          // 调用（args.body）就永远查不到。所以只做条件判断，不做流程跳转。
          const JVal* tool = n->find("tool");
          std::string name = (tool && tool->kind()==JVal::kStr) ? tool->as_str() : "";
          auto it = idx.tool_by_name.find(name);
          const JVal* tools = doc->find("tools");
          const JVal* tdef = (it != idx.tool_by_name.end() && tools && tools->kind()==JVal::kArr)
                                 ? tools->at(it->second) : nullptr;
          std::string mode;
          const JVal* idem = tdef ? tdef->find("idem") : nullptr;
          if (idem != nullptr && idem->kind() == JVal::kObj) {
            const JVal* m = idem->find("mode");
            if (m && m->kind()==JVal::kStr) mode = m->as_str();
          }
          // 三种"已证明"放过；tool 名悬空由 V-2 报，这里不抢活。
          const bool proven = (mode == "readonly" || mode == "idem_true" || mode == "idem_key");
          if (it != idx.tool_by_name.end() && !proven) {
            if (mode.empty()) mode = "(missing)";
            reported[id] = true;
            const JVal* ridv = r->find("id");
            int rid = (ridv && ridv->kind()==JVal::kInt) ? (int)ridv->as_int(-1) : 0;
            add_diag_filled(c, kError, "E_IR_RETRY_UNPROVEN",
                     "retry node {retry} can execute call_tool {call} more than once, but tool {tool} declares idem.mode={mode} — there is no idempotency proof",
                          {{"retry", itoa(rid)}, {"call", itoa(id)}, {"tool", name}, {"mode", mode}},
                     "A retry re-runs its whole body whenever a RunError comes back. If that body contains a tool call without a proof of idempotency, every 5xx repeats the same POST: duplicate charges, duplicate side effects.",
                     {"move call_tool " + itoa(id) + " out of the retry subtree", "give " + name + " a real proof and rebuild the IR"},
                     loc_of(n), true);
          }
        }
        std::vector<int> succ; node_ref_successors(n, succ);
        for (int s : succ) if (!seen[s]) stack.push_back(s);
      }
    }
  }
}

// 把诊断列表序列化成扁平 JSON 数组（对齐 nativeDiag）。
JVal diag_to_jval(const Diag& d) {
  JVal o = JVal::object();
  o.set("severity", JVal::string(sev_str(d.sev)));
  o.set("code", JVal::string(d.code));
  o.set("message", JVal::string(d.message));
  o.set("line", JVal::integer(d.line));
  o.set("col", JVal::integer(d.col));
  o.set("end_line", JVal::integer(d.end_line));
  o.set("end_col", JVal::integer(d.end_col));
  o.set("module", JVal::string(d.module));
  o.set("why", JVal::string(d.why));
  // 结构化事实：Go 侧 nativeDiag.Args 直接喂给 diag.New 按 catalog 重渲染。
  JVal args = JVal::object();
  for (const auto& kv : d.args) args.set(kv.first, JVal::string(kv.second));
  o.set("args", std::move(args));
  JVal fix = JVal::array();
  for (const auto& f : d.fix) fix.push(JVal::string(f));
  o.set("fix", std::move(fix));
  o.set("notes", JVal::array());
  return o;
}

// ---------------------------------------------------------------------------
// 类型闸门（SPEC §2.2）。JVal 是无 schema 的：as_int 对非整数值返回 fallback，
// 于是 `"max_steps":"one"` 会被当成 0 而**完全放过**（最危险的 fail-open）。
// 本阶段只对 Go Unmarshal 必拒且最关键的三类位置做 C++ 结构化 emit：
// ir_version / unit.language_version / bounds 的整数字段（bounds.unbounded 是
// 合法 bool，刻意不查）。其余字段类型错的兜底留给阶段 1 的 Go 数据绑定层。
const char* json_kind_name(JVal::Kind k) {
  switch (k) {
    case JVal::kNull: return "null";
    case JVal::kBool: return "a boolean";
    case JVal::kInt: return "an integer";
    case JVal::kStr: return "a string";
    case JVal::kArr: return "an array";
    case JVal::kObj: return "an object";
  }
  return "an unknown value";
}

void emit_load_failed(Ctx& c, const std::string& detail) {
  add_diag_filled(c, kError, "E_IR_LOAD_FAILED",
           "the IR bytes are not well-formed JSON for this schema: {detail}",
           {{"detail", detail}},
           "The loader decodes into a fixed shape. When a field has the wrong JSON type, decoding stops at the first mismatch and nothing after it is checked — so this is the only diagnostic you get until it is fixed.",
           {"fix the reported field; the surrounding IR is probably fine",
            "or regenerate the artifact if it was hand-edited"},
           LocRec{0,0,0,0,0}, false);
  c.diags.back().module = c.unit_name;
}

bool check_int_field(Ctx& c, const JVal* holder, const char* key, const std::string& path) {
  if (holder == nullptr || holder->kind() != JVal::kObj) return false;
  const JVal* v = holder->find(key);
  if (v == nullptr || v->kind() == JVal::kInt) return false;
  emit_load_failed(c, path + ": expected an integer, got " + json_kind_name(v->kind()));
  return true;
}

// 返回 true 表示发现类型错；调用方必须跳过 run_validate（Go Load 在 Unmarshal
// 失败后也是直接返回，后续三阶段一条都不跑）。
bool check_json_types(Ctx& c, const JVal* doc) {
  if (check_int_field(c, doc, "ir_version", "ir_version")) return true;
  const JVal* unit = doc->find("unit");
  if (check_int_field(c, unit, "language_version", "unit.language_version")) return true;
  static const char* kBoundsIntFields[] = {
      "max_steps", "max_spans", "max_model_calls",
      "max_tool_calls", "max_concurrency", "max_retry_expansion"};
  const JVal* workflows = doc->find("workflows");
  if (workflows != nullptr && workflows->kind() == JVal::kArr) {
    for (size_t wi = 0; wi < workflows->size(); ++wi) {
      const JVal* w = workflows->at(wi);
      const JVal* bounds = w ? w->find("bounds") : nullptr;
      if (bounds == nullptr || bounds->kind() != JVal::kObj) continue;
      for (const char* key : kBoundsIntFields) {
        if (check_int_field(c, bounds, key,
                            pathf("workflows[%d].bounds.%s", (int)wi, key))) return true;
      }
    }
  }
  return false;
}

void run_validate(Ctx& c, const JVal* doc) {
  // 阶段 0：V-7 浮点扫描（mirror internal/ir/load.go）。
  // （实际浮点扫描在调用方对字节做，这里若传入已解析的 doc 无法再扫；保持与
  //  Go Load 的"先扫字节再解析"一致，浮点扫描在 hm_ir_validate 入口对原文做。）
  // 阶段 1：版本闸。
  check_versions(c, doc);
  if (!c.diags.empty()) return;  // 版本不对就不往下走（与 Go 一致）
  // 阶段 2：形状 → 图不变量。
  stage_shape(c, doc);
  Index idx;
  build_index(c, doc, idx);
  check_function_calls(c, doc, idx);
  check_workflow_ids(c, doc);
  std::vector<Ref> refs;
  collect_refs(doc, refs, c);
  resolve_refs(c, doc, refs, idx);
  check_exports_uniqueness(c, doc);
  check_root(c, doc);
  check_var_binding(c, doc);
  check_acyclic(c, doc);
  check_mut_assign(c, doc);
  check_retry_idempotency(c, doc, idx);
  check_export_bidirectional(c, doc);
  check_entry(c, doc);
  // 阶段 3：派生复算。
  stage_derived(c, doc);
}

}  // namespace hm (anonymous)
}  // namespace hm

#pragma GCC visibility push(default)
extern "C" int hm_ir_validate(const uint8_t* ir_json, size_t len,
                              char* out, size_t cap, size_t* out_len,
                              char* out_err, size_t err_cap, size_t* err_len) {
  using namespace hm;
  if (ir_json == nullptr) {
    put("hm_ir_validate: nil input", out_err, err_cap, err_len);
    return 2;
  }
  Ctx c;
  // V-7 浮点扫描（在字节层做，与 Go Load 一致）：命中全部报，路径含键名/下标。
  std::vector<FloatHit> hits;
  if (scan_floats(reinterpret_cast<const char*>(ir_json), len, hits)) {
    for (const FloatHit& h : hits) {
      add_diag_filled(c, kError, "E_IR_FLOAT",
               "IR contains a floating-point JSON number at {path}: {preview}",
                    {{"path", h.path}, {"preview", h.preview}},
               "P-1 forbids floating-point JSON numbers in IR. A float constant must be encoded as a bit-pattern object {\"k\":\"double\",\"bits\":\"...\"}.",
               {"remove the float", "regenerate the IR with the current compiler"},
               LocRec{0,0,0,0,0}, false);
      c.diags.back().module = c.unit_name;
    }
  } else {
    JVal doc;
    size_t pos = 0;
    if (!parse_json(reinterpret_cast<const char*>(ir_json), len, &doc, &pos)) {
      // 用户输入错（不是基础设施故障）：rc=0 + 结构化诊断，与 Go Load 的
      // Unmarshal 失败同码。rc=2 只留给空输入 / 出参缓冲不足。
      emit_load_failed(c, "input is not valid JSON");
    } else if (doc.kind() != JVal::kObj) {
      emit_load_failed(c, "input is not a JSON object");
    } else {
      const JVal* unit = doc.find("unit");
      if (unit != nullptr && unit->kind() == JVal::kObj) {
        const JVal* nm = unit->find("name");
        if (nm && nm->kind() == JVal::kStr) c.unit_name = nm->as_str();
        const JVal* mods = unit->find("modules");
        if (mods && mods->kind() == JVal::kArr)
          for (size_t i = 0; i < mods->size(); ++i) {
            const JVal* m = mods->at(i);
            const JVal* mp = m ? m->find("path") : nullptr;
            c.module_paths.push_back(mp && mp->kind()==JVal::kStr ? mp->as_str() : "");
          }
      }
      // 类型错时 Go 的 Unmarshal 会直接失败、后续校验一条不跑；C++ 必须同形。
      if (!check_json_types(c, &doc)) run_validate(c, &doc);
    }
  }
  if (c.dropped > 0) {
    // 截断汇总必须**绕过** max_diags：add_diag 的第一句是
    // `if (diags.size() >= max_diags) { dropped++; return; }`，而 dropped > 0
    // 恰好意味着已经满了 —— 走 add_diag 就等于让这条警告永远发不出来。
    // 它本身是"还有几条没显示"的唯一说明，不是又一条待丢弃的诊断。
    // Go 的 finish() 也是直接 append（走 v.ds，不走 v.add），此处保持同义。
    std::map<std::string, std::string> args{
        {"shown", itoa((int)c.diags.size())}, {"dropped", itoa(c.dropped)}};
    Diag d{};
    d.sev = kWarning;
    d.code = "W_IR_PANEL_TRUNCATED";
    d.message = fill("validation produced {shown} diagnostics, {dropped} were dropped (limit reached)", args);
    d.why = "Too many diagnostics were produced; the rest were dropped to bound memory.";
    d.fix = {"fix the underlying errors", "raise the diagnostic limit"};
    d.args = args;
    d.module = c.unit_name;
    c.diags.push_back(std::move(d));
  }
  JVal arr = JVal::array();
  for (const auto& d : c.diags) arr.push(diag_to_jval(d));
  const std::string text = arr.str();
  if (!put(text, out, cap, out_len)) {
    put("IR validation: output buffer too small", out_err, err_cap, err_len);
    return 2;
  }
  if (err_len) *err_len = 0;
  return 0;
}
#pragma GCC visibility pop
