// backend_c.cpp —— IR（规范 JSON）→ C 源码。**去 Go 化的目标位置**。
//
// 今天这一步由 Go 实现（internal/backend/c.go，约 2100 行）。本文件是它的 C++ 归宿，
// 按路线图分步移植：
//
//   第 1 步（本文件当前状态）：prelude + 类型映射 + 字面量/变量 + seq/decl/return
//   第 2 步：语句（if/while/for/match/try/retry/...）
//   第 3 步：表达式 + 调用 + native
//
// 判据只有一个：internal/backend/equiv_test.go 的**逐字节等价**。C++ 侧一旦声称
// 支持某份 IR，输出必须与 Go 版完全相同。所以这里对"未移植的节点"一律返回
// code 1（未覆盖），而不是编出半截产物 —— 半截产物会让等价性测试失去意义。
#include "hermes_parser.h"
#include "json_write.h"

// unit AOT 用的 C 片段（逐字节取自 Go 侧，脚本抽取）
#include "backend_unit_texts.cpp"

#include <charconv>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <map>
#include <string>
#include <utility>
#include <vector>

namespace hm {
namespace {

// prelude 逐字节取自 Go 版（见 backend_c_prelude.cpp 的说明）。
const char kPrelude[] =
#include "backend_c_prelude.cpp"
    ;

// 同上，网络运行期段（只有用到 call_model / call_tool 时才追加）。
const char kNetPrelude[] =
#include "backend_c_net_prelude.cpp"
    ;

std::string ind(int depth) { return std::string(static_cast<size_t>(depth) * 2, ' '); }

// ---- IR 读取的便捷包装 --------------------------------------------------
// 每个访问器在字段缺失时返回兜底值，并把"这是不是一个可移植的 IR"的判断
// 留给调用点 —— 缺字段往往就意味着这个节点还没移植，应当返回未覆盖。

const JVal* field(const JVal* n, const char* key) {
  if (n == nullptr || n->kind() != JVal::kObj) return nullptr;
  return n->find(std::string(key));
}

std::int64_t int_field(const JVal* n, const char* key, std::int64_t fallback) {
  const JVal* v = field(n, key);
  return v ? v->as_int(fallback) : fallback;
}

bool has_int_field(const JVal* n, const char* key) {
  const JVal* v = field(n, key);
  return v != nullptr && v->kind() == JVal::kInt;
}

const char* str_field(const JVal* n, const char* key) {
  const JVal* v = field(n, key);
  return v ? v->as_str().c_str() : nullptr;
}

// c_escape 与 Go 侧 cEscape 同义：把工作流名写进 C 字符串字面量。
std::string c_escape(const char* s) {
  std::string out;
  for (const unsigned char* p = reinterpret_cast<const unsigned char*>(s ? s : ""); *p; ++p) {
    switch (*p) {
      case '\\': out += "\\\\"; break;
      case '"': out += "\\\""; break;
      case '\n': out += "\\n"; break;
      case '\r': out += "\\r"; break;
      case '\t': out += "\\t"; break;
      default: out.push_back(static_cast<char>(*p)); break;
    }
  }
  return out;
}

// Gen 持有这次生成的上下文：节点表 + 输出缓冲。
// hash_name 是频道/信号名的编译期 FNV-1a 64，必须与 C++ 前端的 fnv1a64
// 以及 Go 侧的 hashName **逐位一致** —— IR 里的 chan 就是它算的。
std::uint64_t hash_name(const char* s) {
  std::uint64_t h = 1469598103934665603ULL;
  for (const unsigned char* p = reinterpret_cast<const unsigned char*>(s ? s : ""); *p; ++p) {
    h ^= static_cast<std::uint64_t>(*p);
    h *= 1099511628211ULL;
  }
  return h;
}

// chan_key 对应 Go 的 chanKey：频道路由返回频道哈希，全局路由返回 0。
// 注册与派发必须用同一个键，否则全局派发永远匹配不上。
std::uint64_t chan_key(const JVal* n) {
  const JVal* r = field(n, "route");
  const JVal* ch = field(n, "chan");
  if (r != nullptr && r->kind() == JVal::kInt && r->as_int(0) == 1 && ch != nullptr &&
      ch->kind() == JVal::kInt) {
    return static_cast<std::uint64_t>(ch->as_int(0));
  }
  return 0;
}

// sig_state_key 对应 Go 的 sigStateName：FNV(signal) ^ FNV(chan)。
std::uint64_t sig_state_key(const JVal* n) {
  std::uint64_t k = hash_name(str_field(n, "signal"));
  const JVal* ch = field(n, "chan");
  if (ch != nullptr && ch->kind() == JVal::kInt) k ^= static_cast<std::uint64_t>(ch->as_int(0));
  return k;
}

// uses_net 与 Go 侧 UsesNet 同义：产物是否引用了网络运行期。
bool uses_net(const JVal* nodes) {
  if (nodes == nullptr) return false;
  for (size_t i = 0; i < nodes->size(); ++i) {
    const char* k = str_field(nodes->at(i), "k");
    if (k == nullptr) continue;
    if (std::strcmp(k, "call_tool") == 0 || std::strcmp(k, "call_model") == 0) return true;
  }
  return false;
}

// err_kind_cond 把 CatchType 名字映射成"错误槽命中"的 C 条件。
// 与引擎的 catchMatches 同义：类型精确匹配，或 catch(Error) 兜底全部错误。
const char* err_kind_cond(const char* t) {
  if (t == nullptr) return "hm_error.code != 0";
  if (std::strcmp(t, "ContractError") == 0) return "hm_error.code == 1";
  if (std::strcmp(t, "NetworkError") == 0) return "hm_error.code == 2";
  if (std::strcmp(t, "HttpError") == 0) return "hm_error.code == 3";
  if (std::strcmp(t, "ModelError") == 0) return "hm_error.code == 4";
  if (std::strcmp(t, "CircuitOpenError") == 0) return "hm_error.code == 5";
  return "hm_error.code != 0";
}

const char* binop_fn(const char* op) {
  static const struct {
    const char* name;
    const char* fn;
  } kTable[] = {{"add", "op_add"}, {"sub", "op_sub"}, {"mul", "op_mul"}, {"div", "op_div"},
                {"mod", "op_mod"}, {"eq", "op_eq"},   {"ne", "op_ne"},   {"lt", "op_lt"},
                {"le", "op_le"},   {"gt", "op_gt"},   {"ge", "op_ge"},   {"and", "op_and"},
                {"or", "op_or"},   {"not", "op_not"}, {"neg", "op_neg"}};
  for (const auto& e : kTable) {
    if (std::strcmp(e.name, op) == 0) return e.fn;
  }
  return nullptr;
}

// ---- call_native（extern / #link native）----------------------------------
// 编译期静态调用：extern 原型写死在 C 前导之后，符号由链接期解析。
// 不生成任何运行期符号表 / 类型分发 / dlsym —— 这也是"零解释器"能被 grep
// 验证的原因。
struct NativeCTy {
  const char* ctype;
  const char* conv;
  const char* wrap;
};

// native_type_name 取类型表达式的 k（错误信息里要指名道姓）。
const char* native_type_name(const JVal* t) {
  if (t == nullptr) return "?";
  const JVal* k = t->find("k");
  if (k == nullptr || k->kind() != JVal::kStr) return "?";
  return k->as_str().c_str();
}

const NativeCTy* native_cty(const JVal* t) {
  static const struct {
    const char* k;
    NativeCTy v;
  } kTable[] = {
      {"int", {"long", "hm_as_int", "hm_int"}},
      {"bool", {"int", "hm_truthy", "hm_bool"}},
      {"string", {"const char*", "hm_as_str", "hm_str"}},
      {"double", {"double", "hm_as_double", "hm_double"}},
      {"char", {"signed char", "hm_as_int", "hm_int"}},
      {"short", {"short", "hm_as_int", "hm_int"}},
      {"long", {"long", "hm_as_int", "hm_int"}},
  };
  if (t == nullptr) return nullptr;
  const JVal* k = t->find("k");
  if (k == nullptr || k->kind() != JVal::kStr) return nullptr;
  for (const auto& e : kTable) {
    if (k->as_str() == e.k) return &e.v;
  }
  return nullptr;
}

bool c_ident_ok(const char* s) {
  if (s == nullptr || *s == '\0') return false;
  if (!((*s >= 'A' && *s <= 'Z') || (*s >= 'a' && *s <= 'z') || *s == '_')) return false;
  for (const char* q = s + 1; *q; ++q) {
    if (!((*q >= 'A' && *q <= 'Z') || (*q >= 'a' && *q <= 'z') || (*q >= '0' && *q <= '9') ||
          *q == '_')) {
      return false;
    }
  }
  return true;
}

// native_prototypes 产出 extern 原型块（同一符号只写一次）。
bool native_prototypes(const JVal* nodes, std::string* out, std::string* err) {
  out->clear();
  if (nodes == nullptr) return true;
  std::vector<std::string> seen;
  for (size_t i = 0; i < nodes->size(); ++i) {
    const JVal* n = nodes->at(i);
    const char* k = str_field(n, "k");
    if (k == nullptr || std::strcmp(k, "call_native") != 0) continue;
    const char* fn = str_field(n, "fn");
    if (fn == nullptr || !c_ident_ok(fn)) { *err = "call_native symbol is not a valid C identifier"; return false; }
    bool dup = false;
    for (const std::string& s2 : seen) { if (s2 == fn) { dup = true; break; } }
    if (dup) continue;
    seen.push_back(fn);
    const NativeCTy* ret = native_cty(field(n, "ret"));
    if (ret == nullptr) {
      *err = std::string("call_native type \"") + native_type_name(field(n, "ret")) +
             "\" is not in the AOT subset";
      return false;
    }
    std::string params;
    const JVal* ats = field(n, "arg_types");
    if (ats != nullptr) {
      for (size_t j = 0; j < ats->size(); ++j) {
        const NativeCTy* at = native_cty(ats->at(j));
        if (at == nullptr) {
          *err = std::string("call_native type \"") + native_type_name(ats->at(j)) +
                 "\" is not in the AOT subset";
          return false;
        }
        if (!params.empty()) params += ", ";
        params += at->ctype;
      }
    }
    // C99 里 (void) 才是"无参"；() 是不定参，会让原型失去约束力。
    if (params.empty()) params = "void";
    *out += std::string("extern ") + ret->ctype + " " + fn + "(" + params + ");\n";
  }
  return true;
}

class Gen {
 public:
  // 槽函数生成用：以"槽函数内部"身份工作。
  void enter_slot(const std::vector<int64_t>& caps);
  void set_slot_export_only(bool) {}
  bool slot_body(int64_t id, std::string* out, std::string* err);

  Gen(const JVal* wf, const JVal* nodes, const JVal* doc) : wf_(wf), nodes_(nodes), doc_(doc) {
    if (nodes != nullptr) {
      for (size_t i = 0; i < nodes->size(); ++i) {
        const JVal* n = nodes->at(i);
        const JVal* id = n ? n->find("id") : nullptr;
        if (id != nullptr && id->kind() == JVal::kInt) nodes_by_id_[id->as_int(-1)] = n;
      }
    }
    const JVal* vs = field(wf, "vars");
    if (vs != nullptr) {
      for (size_t i = 0; i < vs->size(); ++i) {
        const JVal* v = vs->at(i);
        const char* nm = str_field(v, "name");
        const JVal* id = field(v, "id");
        if (nm != nullptr && id != nullptr && id->kind() == JVal::kInt) {
          vars_.push_back({std::string(nm), id->as_int(-1)});
        }
      }
    }
  }

  bool body(std::string* out, std::string* err);
  bool stmts_only(int64_t root_id, std::string* out, std::string* err);

 private:
  // errLabel：当前"错误去向"标签。默认 hm_fail；retry 期间压入自己的续跑标签。
  const std::string& err_label() const {
    return err_stack_.empty() ? kFailLabel() : err_stack_.back();
  }
  static const std::string& kFailLabel() {
    static const std::string k("hm_fail");
    return k;
  }

  // 变量名 → 变量下标。插值 `{name}` 要靠它解析成 var_N（与 Go 版同一判据：
  // 只认单段 path，多段留给后续步骤）。
  int64_t var_id(const std::string& name) const {
    for (const auto& kv : vars_) {
      if (kv.first == name) return kv.second;
    }
    return -1;
  }

  // 按 **id** 查节点，不按下标：真实 IR 的 id 是稠密的（0..n-1），但手写
  // 测试 IR 与将来可能出现的稀疏 id 不保证这点。按下标查会在稀疏 id 上
  // 报"dangling node id" —— 一个看起来像坏 IR 的假错误。
  const JVal* node(int64_t id) const {
    const auto it = nodes_by_id_.find(id);
    return it == nodes_by_id_.end() ? nullptr : it->second;
  }

  // 返回 false + 设置 err_ 表示"未覆盖"（对应 C ABI 的 1）。
  bool stmt(int64_t id, int depth);
  bool expr(int64_t id, std::string* out);

 public:
  // contracts_c 把契约检查渲染成 C 文本（D-036）。
  //
  //   require：body 之前跑——前置条件不成立时不该先执行任何东西。
  //   ensure ：hm_done 之前跑。返回值已在 hm_result 里，先写进 result 槽再判条件，
  //            于是**所有 return 路径共用这一处检查**。
  //
  // 失败一律 hm_set_error(1, ...)：C 侧的 1 就是 ContractError（与除零同码表），
  // 因此能被 try { } catch (ContractError e) 原样捕获——不新增错误传播机制。
  bool contracts_c(const char* which, std::string* out, int depth);

  // input 由调用方（build 里的契约前置）解析时置位，body 不再重复。
  void set_input_extern() { input_extern_ = true; }

  // 变量声明是否已由调用方 hoist 到函数顶部。
  // workflow 路径会 hoist（契约检查在 body 之前就要用 input），而 unit AOT
  // 路径（函数的静态库产物）沿用"声明即定义"的老形状——两条路径的 C 输出
  // 本来就不一样，不能用一个开关盖住。
  void set_hoisted() { hoisted_ = true; }

 private:
  // contracts_c 把契约检查渲染成 C 文本（D-036）。
  //
  //   require：body 之前、hm_clear_error() 之后跑——前置条件不成立时不该先执行任何东西。
  //   ensure ：hm_done 之前跑。返回值已在 hm_result 里，先写进 result 槽再判条件，
  //            于是**所有 return 路径共用这一处检查**。
  //
  // 失败一律 hm_set_error(1, ...)：C 侧的 1 就是 ContractError（与除零同码表），
  // 因此能被 try { } catch (ContractError e) 原样捕获——不新增错误传播机制。

  bool expr_or_fallback(int64_t id, std::string* out, int depth);
  bool emit_signal(const JVal* n, int depth);
  bool is_capture(int64_t id) const;

  const JVal* wf_;
  const JVal* nodes_;
  std::map<int64_t, const JVal*> nodes_by_id_;
  const JVal* doc_ = nullptr;
  std::vector<std::string> err_stack_;
  std::vector<std::pair<std::string, int64_t>> vars_;
  // catch 绑定的错误对象 → 快照变量名（让 `e.status` 读到快照而不是清掉后的全局槽）。
  std::vector<std::pair<int64_t, std::string>> catch_vars_;
  // fallback 的 return 去向栈：降级块里的 return 只带出该块的值。
  struct RetCtx {
    std::string res;
    std::string lbl;
  };
  std::vector<RetCtx> ret_stack_;
  // 当前语句深度：fallback 作为表达式被降级时，生成的语句要落在当前缩进上。
  int depth_ = 1;
  // fatal_：区分"坏 IR / 超出 AOT 子集"（调用方不该回退）与"节点尚未移植"。
  // 混用会让真实错误被当成"未覆盖"，用户看到的是一句含糊的迁移提示。
  bool fatal_ = false;
  bool input_extern_ = false;
  bool hoisted_ = false;
  bool in_slot_ = false;
  std::vector<int64_t> cap_;
 public:
  bool fatal() const { return fatal_; }

 private:
  std::string* out_ = nullptr;
  std::string err_;
};

bool Gen::stmt(int64_t id, int depth) {
  const JVal* n = node(id);
  if (n == nullptr) { err_ = "dangling node id " + std::to_string(id); return false; }
  const char* k = str_field(n, "k");
  if (k == nullptr) { err_ = "node without kind"; return false; }
  const std::string p = ind(depth);
  depth_ = depth;

  // 与引擎的 stmt() 同步：每进入一个语句节点就计一步。
  *out_ += p + "hm_steps++;\n";

  if (std::strcmp(k, "seq") == 0) {
    // 空 seq 是合法的（`{}` 体、空 block）。`kids` 为空时 IR 里**没有这个键**
    // （omitempty），所以"没有 kids"等于"没有语句"，不是坏节点。
    const JVal* kids = field(n, "kids");
    if (kids == nullptr) return true;
    if (kids->kind() != JVal::kArr) { err_ = "seq kids is not an array"; return false; }
    for (size_t i = 0; i < kids->size(); ++i) {
      const JVal* kid = kids->at(i);
      if (kid == nullptr || kid->kind() != JVal::kInt) { err_ = "seq kid is not a node id"; return false; }
      if (!stmt(kid->as_int(0), depth)) return false;
    }
    return true;
  }
  if (std::strcmp(k, "decl") == 0) {
    if (!has_int_field(n, "var")) return false;
    const int64_t var = int_field(n, "var", 0);
    std::string e;
    if (!expr(int_field(n, "e", -1), &e)) return false;
    char buf[64];
    if (hoisted_) {
      // 声明已由 build() hoist 到 main 顶部，这里只赋值。
      std::snprintf(buf, sizeof(buf), "var_%lld = ", static_cast<long long>(var));
    } else {
      std::snprintf(buf, sizeof(buf), "Val var_%lld = ", static_cast<long long>(var));
    }
    *out_ += p + buf + e + ";\n";
    *out_ += p + "if (hm_error.code) goto " + err_label() + ";\n";
    return true;
  }
  if (std::strcmp(k, "return") == 0) {
    std::string e;
    if (!expr(int_field(n, "e", -1), &e)) return false;
    // fallback 降级块里的 return：只带出该块的值。
    if (!ret_stack_.empty()) {
      const RetCtx& top = ret_stack_.back();
      *out_ += p + "{ Val hm_r = " + e + "; if (hm_error.code) goto " + err_label() + "; " +
                top.res + " = hm_r; goto " + top.lbl + "; }\n";
      return true;
    }
    // 错误去向用 err_label()：在 retry 体内时是那一层的续跑标签，不是 hm_fail。
    // 写死 hm_fail 会让重试直接跳出整个 workflow —— 与 Go 版语义不符。
    *out_ += p + "{ Val hm_r = " + e + "; if (hm_error.code) goto " + err_label() +
              "; hm_result = hm_r; goto hm_done; }\n";
    return true;
  }

  if (std::strcmp(k, "if") == 0) {
    std::string cond;
    if (!expr(int_field(n, "c", -1), &cond)) return false;
    *out_ += p + "if (hm_truthy(" + cond + ")) {\n";
    if (!stmt(int_field(n, "then", -1), depth + 1)) return false;
    const JVal* elifs = field(n, "elifs");
    if (elifs != nullptr) {
      for (size_t i = 0; i < elifs->size(); ++i) {
        const JVal* arm = elifs->at(i);
        std::string ec;
        if (!expr(int_field(arm, "c", -1), &ec)) return false;
        *out_ += p + "} else if (hm_truthy(" + ec + ")) {\n";
        if (!stmt(int_field(arm, "body", -1), depth + 1)) return false;
      }
    }
    if (has_int_field(n, "else")) {
      *out_ += p + "} else {\n";
      if (!stmt(int_field(n, "else", -1), depth + 1)) return false;
    }
    *out_ += p + "}\n";
    return true;
  }
  if (std::strcmp(k, "match") == 0) {
    if (!has_int_field(n, "subj")) { err_ = "match without subject"; return false; }
    std::string sv;
    if (!expr(int_field(n, "subj", -1), &sv)) return false;
    const int64_t nid = int_field(n, "id", 0);
    std::string mv = "hm_m_" + std::to_string(nid);
    *out_ += p + "Val " + mv + " = " + sv + ";\n";
    const JVal* arms = field(n, "arms");
    bool first = true;
    if (arms != nullptr) {
      for (size_t i = 0; i < arms->size(); ++i) {
        const JVal* arm = arms->at(i);
        std::string cmp;
        const JVal* num = field(arm, "num");
        if (num != nullptr && num->kind() == JVal::kInt) {
          cmp = "(" + mv + ".k==0 && " + mv + ".i==" + std::to_string(num->as_int(0)) + "L)";
        } else {
          const char* lit = str_field(arm, "lit");
          cmp = "(" + mv + ".k==2 && strcmp(" + mv + ".s?" + mv + ".s:\"\", \"" +
                c_escape(lit) + "\")==0)";
        }
        if (first) { *out_ += p + "if " + cmp + " {\n"; first = false; }
        else { *out_ += p + "} else if " + cmp + " {\n"; }
        if (!stmt(int_field(arm, "body", -1), depth + 1)) return false;
      }
    }
    if (has_int_field(n, "default")) {
      if (first) { *out_ += p + "{\n"; } else { *out_ += p + "} else {\n"; }
      if (!stmt(int_field(n, "default", -1), depth + 1)) return false;
      first = false;
    }
    if (!first) { *out_ += p + "}\n"; }
    return true;
  }
  if (std::strcmp(k, "retry") == 0) {
    if (!has_int_field(n, "body") || !has_int_field(n, "n")) { err_ = "malformed retry"; return false; }
    const int64_t nid = int_field(n, "id", 0);
    const std::string cont = "hm_retry_" + std::to_string(nid);
    const std::string iv = "hm_attempt_" + std::to_string(nid);
    const std::string inn = ind(depth + 1);
    *out_ += p + "{ long " + iv + " = 0; for (" + iv + " = 0; " + iv + " <= " +
              std::to_string(int_field(n, "n", 0)) + "; " + iv + "++) {\n";
    *out_ += inn + "hm_clear_error();\n";
    err_stack_.push_back(cont);
    if (!stmt(int_field(n, "body", -1), depth + 1)) return false;
    err_stack_.pop_back();
    *out_ += inn + cont + ": ;\n";
    *out_ += inn + "if (!hm_error.code) break;\n";
    *out_ += p + "}\n";
    *out_ += p + "if (hm_error.code) goto " + err_label() + ";\n";
    *out_ += p + "}\n";
    return true;
  }
  if (std::strcmp(k, "try") == 0) {
    if (!has_int_field(n, "body")) { err_ = "malformed try"; return false; }
    const std::string nid = std::to_string(int_field(n, "id", 0));
    const std::string catch_lbl = "hm_catch_" + nid;
    const std::string end_lbl = "hm_tryend_" + nid;
    // body 里任何语句置起错误槽 → 跳到本 try 的 catch 标签。
    err_stack_.push_back(catch_lbl);
    if (!stmt(int_field(n, "body", -1), depth + 1)) return false;
    err_stack_.pop_back();
    *out_ += p + "goto " + end_lbl + ";\n";
    // C 标签必须顶格（列 0）：加缩进虽然仍是合法 C，但会让产物字节与 Go 版不同 ——
    // 而差异没有任何语义价值，只是让比对噪声变大。
    *out_ += catch_lbl + ":\n";
    const std::string snap = "hm_errsnap_" + nid;
    const std::string cp = ind(depth + 1);
    bool first = true;
    const JVal* catches = field(n, "catches");
    if (catches != nullptr) {
      for (size_t i = 0; i < catches->size(); ++i) {
        const JVal* c = catches->at(i);
        const char* t = str_field(c, "t");
        const std::string cond = err_kind_cond(t);
        if (first) { *out_ += p + "if (" + cond + ") {\n"; first = false; }
        else { *out_ += p + "} else if (" + cond + ") {\n"; }
        *out_ += cp + "HermesError " + snap + " = hm_error_snapshot();\n";
        const int64_t v = int_field(c, "v", -1);
        *out_ += cp + "Val var_" + std::to_string(v) + " = hm_str(\"\");\n";
        *out_ += cp + "(void)var_" + std::to_string(v) + ";\n";
        *out_ += cp + "hm_clear_error();\n";
        if (v >= 0) catch_vars_.push_back({v, snap});
        if (!stmt(int_field(c, "body", -1), depth + 1)) return false;
      }
    }
    if (first) {
      // 没有 catch 子句：错误继续向外传播。
      *out_ += p + "goto " + err_label() + ";\n";
    } else {
      *out_ += p + "} else {\n";
      *out_ += ind(depth + 1) + "goto " + err_label() + ";\n";
      *out_ += p + "}\n";
    }
    *out_ += end_lbl + ":\n";
    return true;
  }
  if (std::strcmp(k, "while") == 0) {
    if (!has_int_field(n, "c") || !has_int_field(n, "body")) { err_ = "malformed while"; return false; }
    std::string cond;
    if (!expr(int_field(n, "c", -1), &cond)) return false;
    *out_ += p + "while (hm_truthy(" + cond + ")) {\n";
    if (!stmt(int_field(n, "body", -1), depth + 1)) return false;
    *out_ += p + "}\n";
    return true;
  }
  if (std::strcmp(k, "assign") == 0) {
    if (!has_int_field(n, "var")) { err_ = "assign without var"; return false; }
    std::string e;
    if (!expr_or_fallback(int_field(n, "e", -1), &e, depth)) return false;
    const int64_t lhs_id = int_field(n, "var", 0);
    const std::string lhs = (in_slot_ && is_capture(lhs_id)) ? ("c->v" + std::to_string(lhs_id))
                                                            : ("var_" + std::to_string(lhs_id));
    *out_ += p + lhs + " = " + e + ";\n";
    *out_ += p + "if (hm_error.code) goto " + err_label() + ";\n";
    return true;
  }
  if (std::strcmp(k, "exprstmt") == 0) {
    // 表达式语句：只要副作用。void 函数内联后产出的是一个 seq（语句链），
    // 那种情形直接把 seq 当语句发；否则求值后丢弃。
    if (!has_int_field(n, "e")) { err_ = "exprstmt without expression"; return false; }
    const int64_t eid = int_field(n, "e", -1);
    const JVal* en = node(eid);
    if (en != nullptr) {
      const char* ek = str_field(en, "k");
      if (ek != nullptr && std::strcmp(ek, "seq") == 0) return stmt(eid, depth);
    }
    std::string e;
    if (!expr(eid, &e)) return false;
    *out_ += p + "{ Val hm_discard = " + e + "; (void)hm_discard; }\n";
    return true;
  }
  if (std::strcmp(k, "for") == 0) {
    if (!has_int_field(n, "init") || !has_int_field(n, "cond") || !has_int_field(n, "delta") ||
        !has_int_field(n, "var") || !has_int_field(n, "body")) {
      err_ = "malformed for"; return false;
    }
    // C 的 for-post 正好是 continue 的目标（跳到步进而非条件），所以 for 用
    // C 的 for 头，不用 while 手工累加 —— 后者会让 continue 跳过步进。
    std::string init_v, cond;
    if (!expr(int_field(n, "init", -1), &init_v)) return false;
    if (!expr(int_field(n, "cond", -1), &cond)) return false;
    const int64_t var = int_field(n, "var", 0);
    const std::string vname = "var_" + std::to_string(var);
    *out_ += p + "for (Val " + vname + " = " + init_v + "; hm_truthy(" + cond + "); " +
              vname + " = hm_int(" + vname + ".i + (" + std::to_string(int_field(n, "delta", 0)) +
              "L))) {\n";
    if (!stmt(int_field(n, "body", -1), depth + 1)) return false;
    *out_ += p + "}\n";
    return true;
  }
  if (std::strcmp(k, "break") == 0) {
    *out_ += p + "break;\n";
    return true;
  }
  if (std::strcmp(k, "continue") == 0) {
    *out_ += p + "continue;\n";
    return true;
  }
  if (std::strcmp(k, "stop") == 0) {
    // workflow.stop(reason, data)：跳到 epilogue，data 成为返回值。
    const JVal* data = field(n, "data");
    if (data != nullptr && data->kind() == JVal::kInt) {
      std::string d;
      if (!expr(data->as_int(-1), &d)) return false;
      *out_ += p + "hm_result = " + d + ";\n";
    }
    *out_ += p + "goto hm_done;\n";
    return true;
  }
  if (std::strcmp(k, "on") == 0) {
    // 槽函数是**静态**定义（见 slots 段），这里只留一个取址引用，
    // 免得 -Wunused-function 在用户产物里冒出来。
    *out_ += p + "(void)&hm_slot_" + std::to_string(int_field(n, "id", 0)) + ";\n";
    return true;
  }
  if (std::strcmp(k, "sleep") == 0) {
    if (!has_int_field(n, "ms")) { err_ = "malformed sleep"; return false; }
    std::string ms;
    if (!expr(int_field(n, "ms", -1), &ms)) return false;
    *out_ += p + "hm_sleep_ms(" + ms + ".i);\n";
    return true;
  }
  if (std::strcmp(k, "until") == 0) {
    const char* sig = str_field(n, "signal");
    if (sig == nullptr) { err_ = "until without signal"; return false; }
    // 状态名与 Go 版同一算法：FNV(signal) ^ FNV(chan)。
    const std::string st = "hm_sig_" + std::to_string(sig_state_key(n));
    std::string timeout = "-1";
    const JVal* to = field(n, "timeout_ms");
    if (to != nullptr && to->kind() == JVal::kInt) {
      std::string t;
      if (!expr(to->as_int(-1), &t)) return false;
      timeout = t + ".i";
    }
    *out_ += p + "hm_pal_cond_wait_flag(&" + st + ".cv, &" + st + ".flag, " + timeout + ");\n";
    return true;
  }
  if (std::strcmp(k, "emit") == 0) {
    if (!emit_signal(n, depth)) return false;
    return true;
  }
  if (std::strcmp(k, "mem_txn") == 0) {
    // 与 Go 版同一行为：存储层的快照/回滚尚未实现，**明确拒绝**而不是编成
    // 空操作 —— 空操作会让 rollback 静默失效，那比报错坏得多。
    const JVal* op = field(n, "op");
    err_ = std::string("backend(c): memory.") + (op ? op->as_str().c_str() : "?") +
           " is not supported by the AOT backend yet (use hermes run, or wait for 2.0.2)";
    return false;
  }

  // 其余节点属于第 2、3 步 —— 如实报"未覆盖"，让调用方回落 Go 版。
  err_ = std::string("node kind not ported yet: ") + k;
  return false;
}

// contracts_c：渲染契约检查。which = "require" | "ensure"。
bool Gen::contracts_c(const char* which, std::string* out, int depth) {
  const JVal* cs = wf_ != nullptr ? wf_->find("contracts") : nullptr;
  if (cs == nullptr || cs->kind() != JVal::kArr) return true;
  const JVal* rv = wf_->find("contract_result_var");
  const long long rv_id = (rv != nullptr && rv->kind() == JVal::kInt) ? rv->as_int(-1) : -1;
  const std::string p(depth * 2, ' ');
  if (std::strcmp(which, "ensure") == 0 && rv_id >= 0) {
    *out += p + "var_" + std::to_string(rv_id) + " = hm_result;\n";
  }
  for (size_t i = 0; i < cs->size(); ++i) {
    const JVal* c = cs->at(i);
    if (c == nullptr || c->kind() != JVal::kObj) continue;
    const JVal* clause = c->find("clause");
    if (clause == nullptr || clause->kind() != JVal::kStr) continue;
    if (clause->as_str() != which) continue;
    const JVal* cond = c->find("cond");
    if (cond == nullptr || cond->kind() != JVal::kInt) continue;
    std::string e;
    if (!expr(cond->as_int(-1), &e)) return false;
    *out += p + "if (!hm_truthy(" + e + ")) {\n";
    *out += p + "  hm_set_error(1, \"contract violation: " + std::string(which) + "\");\n";
    *out += p + "  goto " + err_label() + ";\n";
    *out += p + "}\n";
  }
  return true;
}

bool Gen::expr(int64_t id, std::string* out) {
  const JVal* n = node(id);
  if (n == nullptr) { err_ = "dangling node id " + std::to_string(id); return false; }
  const char* k = str_field(n, "k");
  if (k == nullptr) { err_ = "expr node without kind"; return false; }

  if (std::strcmp(k, "var") == 0) {
    if (!has_int_field(n, "var")) return false;
    const int64_t vid = int_field(n, "var", 0);
    if (in_slot_ && is_capture(vid)) {
      *out = "c->v" + std::to_string(vid);
      return true;
    }
    char buf[32];
    std::snprintf(buf, sizeof(buf), "var_%lld", static_cast<long long>(vid));
    *out = buf;
    return true;
  }
  if (std::strcmp(k, "lit") == 0) {
    const char* t = str_field(n, "t");
    if (t == nullptr) { err_ = "lit without type tag"; return false; }
    const JVal* v = field(n, "v");
    if (std::strcmp(t, "int") == 0) {
      if (v == nullptr || v->kind() != JVal::kInt) return false;
      char buf[40];
      std::snprintf(buf, sizeof(buf), "hm_int(%lldL)", static_cast<long long>(v->as_int(0)));
      *out = buf;
      return true;
    }
    if (std::strcmp(t, "bool") == 0) {
      if (v == nullptr || v->kind() != JVal::kBool) return false;
      *out = v->as_bool(false) ? "hm_bool(1)" : "hm_bool(0)";
      return true;
    }
    if (std::strcmp(t, "string") == 0) {
      if (v == nullptr || v->kind() != JVal::kStr) return false;
      *out = "hm_str(\"" + c_escape(v->as_str().c_str()) + "\")";
      return true;
    }
    if (std::strcmp(t, "double") == 0) {
      // IR 里的 double 常量是位模式对象 {"k":"double","bits":"<16 位小写 hex>"}。
      // bits 是字符串而不是 JSON 数字 —— canon 不允许浮点 JSON 数字。
      if (v == nullptr || v->kind() != JVal::kObj) { err_ = "double literal needs a bit pattern"; return false; }
      const JVal* bk = v->find("k");
      const JVal* bits = v->find("bits");
      if (bk == nullptr || bk->kind() != JVal::kStr || bk->as_str() != "double" || bits == nullptr ||
          bits->kind() != JVal::kStr || bits->as_str().size() != 16) {
        err_ = "double literal needs {\"k\":\"double\",\"bits\":\"<16 hex>\"}";
        return false;
      }
      const std::string bs = bits->as_str();
      std::uint64_t u = 0;
      for (char c : bs) {
        if (!((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f'))) {
          err_ = "double bits is not 16 lowercase hex digits";
          return false;
        }
        u = (u << 4) | static_cast<std::uint64_t>(c <= '9' ? c - '0' : c - 'a' + 10);
      }
      // 指数全 1 = NaN/±Inf：最短往返格式化会吐 "+Inf"/"NaN"，那不是合法 C 字面量。
      if (((u >> 52) & 0x7ff) == 0x7ff) {
        *out = "hm_double_from_bits(0x" + bs + "ULL)";
        return true;
      }
      double x;
      std::memcpy(&x, &u, sizeof(x));
      // 最短往返（与 Go 的 strconv.FormatFloat(x,'g',-1,64) 同义）：
      // 用 %f 会丢精度，用 %v 不是 C 字面量。
      char num[64];
      auto res = std::to_chars(num, num + sizeof(num), x, std::chars_format::general);
      if (res.ec != std::errc()) { err_ = "double formatting failed"; return false; }
      *out = "hm_double(" + std::string(num, res.ptr) + ")";
      return true;
    }
    err_ = std::string("literal type not ported yet: ") + t;
    return false;
  }
  if (std::strcmp(k, "binop") == 0) {
    const char* op = str_field(n, "op");
    if (op == nullptr || !has_int_field(n, "l") || !has_int_field(n, "r")) {
      err_ = "malformed binop"; return false;
    }
    std::string l, r;
    if (!expr(int_field(n, "l", -1), &l)) return false;
    if (!expr(int_field(n, "r", -1), &r)) return false;
    const char* fn = binop_fn(op);
    if (fn == nullptr) { err_ = std::string("binop not ported yet: ") + op; return false; }
    *out = std::string(fn) + "(" + l + ", " + r + ")";
    return true;
  }
  if (std::strcmp(k, "call_native") == 0) {
    const char* fn = str_field(n, "fn");
    if (fn == nullptr || !c_ident_ok(fn)) { err_ = "call_native without valid fn"; return false; }
    const NativeCTy* ret = native_cty(field(n, "ret"));
    if (ret == nullptr) {
      fatal_ = true;
      err_ = std::string("call_native type \"") + native_type_name(field(n, "ret")) +
             "\" is not in the AOT subset";
      return false;
    }
    const JVal* args = field(n, "args");
    const JVal* ats = field(n, "arg_types");
    const size_t argc = args ? args->size() : 0;
    // args 与 arg_types 必须一一对应：不一致就是坏 IR，按短的那侧生成
    // 会编出一个参数不匹配的 C 调用。
    if (ats == nullptr || ats->size() != argc) { err_ = "call_native args/arg_types mismatch"; return false; }
    std::string call;
    for (size_t i = 0; i < argc; ++i) {
      const NativeCTy* at = native_cty(ats->at(i));
      if (at == nullptr) {
        fatal_ = true;
        err_ = std::string("call_native type \"") + native_type_name(ats->at(i)) +
               "\" is not in the AOT subset";
        return false;
      }
      const JVal* a = args->at(i);
      if (a == nullptr || a->kind() != JVal::kInt) { err_ = "call_native malformed args"; return false; }
      std::string e;
      if (!expr(a->as_int(-1), &e)) return false;
      if (i > 0) call += ", ";
      call += std::string(at->conv) + "(" + e + ")";
    }
    *out = std::string(ret->wrap) + "(" + fn + "(" + call + "))";
    return true;
  }
  if (std::strcmp(k, "unop") == 0) {
    const char* op = str_field(n, "op");
    if (op == nullptr || !has_int_field(n, "e")) { err_ = "malformed unop"; return false; }
    std::string inner;
    if (!expr(int_field(n, "e", -1), &inner)) return false;
    const char* fn = binop_fn(op);  // unop 复用同一张表：op_not / op_neg
    if (fn == nullptr) { err_ = std::string("unop not ported yet: ") + op; return false; }
    *out = std::string(fn) + "(" + inner + ")";
    return true;
  }
  if (std::strcmp(k, "interp") == 0) {
    std::string acc = "hm_str(\"\")";
    const JVal* parts = field(n, "parts");
    if (parts == nullptr) { err_ = "interp without parts"; return false; }
    for (size_t i = 0; i < parts->size(); ++i) {
      const JVal* part = parts->at(i);
      const JVal* path = field(part, "path");
      if (path != nullptr && path->size() > 0) {
        if (path->size() != 1) {
          // 与 Go 版同一判据：多段路径未支持，如实报未覆盖。
          err_ = "nested interpolation path not ported yet"; return false;
        }
        const JVal* seg = path->at(0);
        if (seg == nullptr || seg->kind() != JVal::kStr) return false;
        const int64_t vid = var_id(seg->as_str());
        if (vid < 0) { err_ = "interpolation references unknown variable"; return false; }
        acc = "hm_cat(" + acc + ", var_" + std::to_string(vid) + ")";
      } else {
        const char* lit = str_field(part, "lit");
        acc = "hm_cat(" + acc + ", hm_str(\"" + c_escape(lit) + "\"))";
      }
    }
    *out = acc;
    return true;
  }
  if (std::strcmp(k, "cast") == 0) {
    std::string inner;
    if (!expr(int_field(n, "e", -1), &inner)) return false;
    const JVal* to = field(n, "to");
    const char* tk = to ? str_field(to, "k") : nullptr;
    if (tk == nullptr) { *out = inner; return true; }
    if (std::strcmp(tk, "char") == 0) { *out = "hm_int((long)(signed char)((" + inner + ").i))"; return true; }
    if (std::strcmp(tk, "short") == 0) { *out = "hm_int((long)(short)((" + inner + ").i))"; return true; }
    if (std::strcmp(tk, "int") == 0) { *out = "hm_int(hm_as_int(" + inner + "))"; return true; }
    if (std::strcmp(tk, "string") == 0) { *out = "hm_str(hm_as_str(" + inner + "))"; return true; }
    if (std::strcmp(tk, "bool") == 0) { *out = "hm_bool(hm_truthy(" + inner + "))"; return true; }
    if (std::strcmp(tk, "double") == 0) { *out = "hm_double(hm_as_double(" + inner + "))"; return true; }
    *out = inner;
    return true;
  }
  if (std::strcmp(k, "field") == 0) {
    const char* name = str_field(n, "name");
    if (name == nullptr || !has_int_field(n, "of")) { err_ = "malformed field access"; return false; }
    // catch 绑定的错误对象：错误在 catch 臂清错前已按值快照（catch_vars_）。
    // `e.status` 是唯一放行的字段，读快照的 status；其余字段必须**显式拒绝** ——
    // 静默走 hm_field 会变成 null，`if (e.url == ...)` 编得过、跑出错值。
    {
      const JVal* base = node(int_field(n, "of", -1));
      const char* bk = base ? str_field(base, "k") : nullptr;
      if (bk != nullptr && std::strcmp(bk, "var") == 0 && has_int_field(base, "var")) {
        const int64_t vid = int_field(base, "var", -1);
        for (const auto& kv : catch_vars_) {
          if (kv.first != vid) continue;
          if (std::strcmp(name, "status") == 0) {
            *out = "hm_int((long)" + kv.second + ".status)";
            return true;
          }
          err_ = std::string("backend(c): field \"") + name +
                 "\" of a caught error is not carried by the AOT backend yet (frontend E4034 "
                 "also does not allow it); only `status` is available on the error object in "
                 "AOT output; use `hermes run` for interpreter output";
          return false;
        }
      }
    }
    std::string of;
    if (!expr(int_field(n, "of", -1), &of)) return false;
    *out = "hm_field(" + of + ", \"" + c_escape(name) + "\")";
    return true;
  }
  if (std::strcmp(k, "call_model") == 0) {
    const JVal* args = field(n, "args");
    if (args == nullptr || args->kind() != JVal::kObj) { err_ = "call_model without args"; return false; }
    const JVal* prompt = field(args, "prompt");
    if (prompt == nullptr || prompt->kind() != JVal::kInt) { err_ = "call_model without prompt"; return false; }
    std::string prompt_e;
    if (!expr(prompt->as_int(-1), &prompt_e)) return false;
    const JVal* mt = field(args, "max_tokens");
    const long long max_tok = (mt != nullptr && mt->kind() == JVal::kInt) ? mt->as_int(-1) : -1;
    std::string schema = "null";
    const JVal* sch = field(n, "schema");
    if (sch != nullptr && sch->kind() == JVal::kInt) {
      const JVal* schemas = field(doc_, "schemas");
      const JVal* sdoc = schemas ? schemas->at(static_cast<size_t>(sch->as_int(-1))) : nullptr;
      const JVal* js = sdoc ? field(sdoc, "json") : nullptr;
      // schema 在 IR 里是**对象**（Go 侧是 json.RawMessage），不是字符串。
      // 用 canonical 写出器还原成文本 —— IR 里的 schema 本身就是 canonical 的
      // （键已排序），所以还原出的字节与 Go 版取原始字节完全一致。
      if (js != nullptr) schema = js->str();
    }
    *out = "hm_net_model_call(" + prompt_e + ", \"" + c_escape(str_field(args, "precision")) +
           "\", " + std::to_string(max_tok) + ", \"" + c_escape(str_field(args, "temperature")) +
           "\", \"" + c_escape(schema.c_str()) + "\", \"" + c_escape(str_field(wf_, "backend")) +
           "\", \"" + c_escape(str_field(wf_, "name")) + "\")";
    return true;
  }
  if (std::strcmp(k, "call_tool") == 0) {
    const JVal* args = field(n, "args");
    if (args == nullptr || args->kind() != JVal::kObj) { err_ = "call_tool without args"; return false; }
    const JVal* url = field(args, "url");
    if (url == nullptr || url->kind() != JVal::kInt) { err_ = "call_tool without url"; return false; }
    std::string url_e;
    if (!expr(url->as_int(-1), &url_e)) return false;
    std::string header_e = "hm_jsonv(\"null\")";
    const JVal* hdr = field(args, "header");
    if (hdr != nullptr && hdr->kind() == JVal::kInt && !expr(hdr->as_int(-1), &header_e)) return false;
    std::string body_e = "hm_jsonv(\"null\")";
    const JVal* bd = field(args, "body");
    if (bd != nullptr && bd->kind() == JVal::kInt && !expr(bd->as_int(-1), &body_e)) return false;
    std::string allow = "[";
    const JVal* as = field(args, "allow_status");
    if (as != nullptr) {
      for (size_t i = 0; i < as->size(); ++i) {
        const JVal* v = as->at(i);
        if (i > 0) allow += ",";
        allow += std::to_string(v ? v->as_int(0) : 0);
      }
    }
    allow += "]";
    const JVal* to = field(args, "timeout_ms");
    const long long timeout = (to != nullptr && to->kind() == JVal::kInt) ? to->as_int(0) : 0;
    const char* tool = str_field(n, "tool");
    *out = "hm_net_tool_call(" + url_e + ", " + header_e + ", " + body_e + ", \"" +
           c_escape(str_field(args, "method")) + "\", \"" + c_escape(allow.c_str()) + "\", " +
           std::to_string(timeout) + ", \"" + c_escape(str_field(wf_, "name")) + "\", \"" +
           c_escape(tool) + "\")";
    return true;
  }
  if (std::strcmp(k, "fallback") == 0) {
    // fallback 是**表达式**，但要降级成两条语句：一个结果变量 + 主表达式试跑。
    // 返回的是结果变量名（调用方把它当普通的 var 用）。
    if (!has_int_field(n, "main") || !has_int_field(n, "body")) {
      err_ = "malformed fallback"; return false;
    }
    const std::string nid = std::to_string(int_field(n, "id", 0));
    const std::string fb = "hm_fb_" + nid;
    const std::string mn = "hm_mn_" + nid;
    const std::string end = "hm_fbend_" + nid;
    std::string main_e;
    if (!expr(int_field(n, "main", -1), &main_e)) return false;
    const std::string p0 = ind(depth_);
    *out_ += p0 + "Val " + fb + " = hm_int(0);\n";
    *out_ += p0 + "{ Val " + mn + " = " + main_e + "; if (!hm_error.code) { " + fb + " = " +
              mn + "; goto " + end + "; } hm_clear_error(); }\n";
    // 降级块里的 return 只带出**本块**的值（写临时变量、跳本块收尾），
    // 不是 workflow 的返回 —— 这是 fallback 与 try 的语义差别。
    ret_stack_.push_back({fb, end});
    if (!stmt(int_field(n, "body", -1), depth_)) return false;
    ret_stack_.pop_back();
    *out_ += p0 + end + ": ;\n";
    *out = fb;
    return true;
  }
  err_ = std::string("expr kind not ported yet: ") + k;
  return false;
}

// expr_or_fallback 对应 Go 的 exprOrFallback：如果这个节点是 fallback，
// 先把它降级成语句（生成临时变量与分支），再把变量名当表达式返回。
bool Gen::expr_or_fallback(int64_t id, std::string* out, int depth) {
  const JVal* n = node(id);
  if (n != nullptr) {
    const char* k = str_field(n, "k");
    if (k != nullptr && std::strcmp(k, "fallback") == 0) {
      depth_ = depth;
      return expr(id, out);
    }
  }
  return expr(id, out);
}

// slots_for 对应 Go 的 slotsFor：在**编译期**把 emit 与 on 配对。
// 全局 emit 只命中全局槽；频道 emit 只命中同频道槽（用 IR 里的哈希直接比）。
struct SlotEntry {
  int64_t on_id;
  std::vector<int64_t> cap_ids;
};

void Gen::enter_slot(const std::vector<int64_t>& caps) {
  in_slot_ = true;
  cap_.assign(caps.begin(), caps.end());
  err_stack_.clear();
  ret_stack_.clear();
}

bool Gen::is_capture(int64_t id) const {
  for (int64_t c : cap_) if (c == id) return true;
  return false;
}

bool Gen::slot_body(int64_t id, std::string* out, std::string* err) {
  out_ = out;
  depth_ = 1;
  if (!stmt(id, 1)) { *err = err_; return false; }
  return true;
}

bool Gen::emit_signal(const JVal* n, int depth) {
  const char* sig = str_field(n, "signal");
  if (sig == nullptr) { err_ = "emit without signal"; return false; }
  const std::string p = ind(depth);
  const JVal* route = field(n, "route");
  const bool want_chan = route != nullptr && route->kind() == JVal::kInt && route->as_int(0) == 1;

  std::vector<SlotEntry> entries;
  {
    const JVal* nodes = nodes_;
    if (nodes != nullptr) {
      for (size_t i = 0; i < nodes->size(); ++i) {
        const JVal* on = nodes->at(i);
        const char* ok = str_field(on, "k");
        if (ok == nullptr || std::strcmp(ok, "on") != 0) continue;
        const char* osig = str_field(on, "signal");
        if (osig == nullptr || std::strcmp(osig, sig) != 0) continue;
        const JVal* och = field(on, "chan");
        const bool have_chan = och != nullptr && och->kind() == JVal::kInt;
        if (want_chan != have_chan) continue;
        SlotEntry e;
        e.on_id = int_field(on, "id", 0);
        const JVal* caps = field(on, "captures");
        if (caps != nullptr) {
          for (size_t c = 0; c < caps->size(); ++c) {
            const JVal* cap = caps->at(c);
            if (cap == nullptr || cap->kind() != JVal::kStr) continue;
            const int64_t vid = var_id(cap->as_str());
            if (vid >= 0) e.cap_ids.push_back(vid);
          }
        }
        entries.push_back(std::move(e));
      }
    }
  }

  if (entries.empty()) {
    *out_ += p + "(void)0; /* emit " + sig + ": no handler in this unit */\n";
    return true;
  }
  const std::string skip = "hm_emit_skip_" + std::to_string(int_field(n, "id", 0));
  for (const SlotEntry& e : entries) {
    const std::string ctx = "c" + std::to_string(e.on_id);
    if (!e.cap_ids.empty()) {
      *out_ += p + "{ struct hm_ctx_" + std::to_string(e.on_id) + " " + ctx + ";\n";
      for (int64_t id : e.cap_ids) {
        *out_ += p + "  " + ctx + ".v" + std::to_string(id) + " = var_" + std::to_string(id) + ";\n";
      }
      // 回调写完**必须回写**：槽捕获的是变量而不是值的快照。
      *out_ += p + "  int r" + std::to_string(e.on_id) + " = hm_slot_" + std::to_string(e.on_id) +
                "(&" + ctx + ");\n";
      for (int64_t id : e.cap_ids) {
        *out_ += p + "  var_" + std::to_string(id) + " = " + ctx + ".v" + std::to_string(id) + ";\n";
      }
      *out_ += p + "  if (r" + std::to_string(e.on_id) + ") goto " + skip + "; }\n";
    } else {
      *out_ += p + "if (hm_slot_" + std::to_string(e.on_id) + "(0)) goto " + skip + ";\n";
    }
  }
  // 全局兜底：静态配对没命中时才查全局表。认领的槽会 goto skip，
  // 跳过这一行 —— 所以同单元的槽不会被调用两次。
  char buf[96];
  std::snprintf(buf, sizeof(buf), "hm_slot_dispatch(0x%016llxULL, 0x%016llxULL);\n",
                static_cast<unsigned long long>(hash_name(sig)),
                static_cast<unsigned long long>(sig_state_key(n) ^ hash_name(sig)));
  *out_ += p + buf;
  *out_ += skip + ": ;\n";
  // 唤醒等待该 (signal, channel) 的 until。
  const std::string st = "hm_sig_" + std::to_string(sig_state_key(n));
  *out_ += p + "hm_pal_cond_set_flag(&" + st + ".cv, &" + st + ".flag);\n";
  return true;
}

// gen_slots 对应 Go 的 genSlots：每个 `on` 生成一个上下文结构体 + 一个静态槽函数。
// 槽函数签名统一 `static int hm_slot_N(void* raw)`，返回 1 = 已处理（短路）。
std::string gen_slots(const JVal* doc, const JVal* wf) {
  std::string out;
  const JVal* wfs = field(doc, "workflows");
  if (wfs == nullptr) return out;
  for (size_t wi = 0; wi < wfs->size(); ++wi) {
    const JVal* w = wfs->at(wi);
    const JVal* nodes = field(w, "nodes");
    if (nodes == nullptr) continue;
    const JVal* vars = field(w, "vars");
    for (size_t ni = 0; ni < nodes->size(); ++ni) {
      const JVal* n = nodes->at(ni);
      const char* k = str_field(n, "k");
      if (k == nullptr || std::strcmp(k, "on") != 0) continue;
      if (!has_int_field(n, "body")) continue;
      const int64_t id = int_field(n, "id", 0);

      std::vector<int64_t> idx;
      const JVal* caps = field(n, "captures");
      if (caps != nullptr && vars != nullptr) {
        for (size_t c = 0; c < caps->size(); ++c) {
          const JVal* cap = caps->at(c);
          if (cap == nullptr || cap->kind() != JVal::kStr) continue;
          for (size_t vi = 0; vi < vars->size(); ++vi) {
            const JVal* v = vars->at(vi);
            const char* nm = str_field(v, "name");
            if (nm != nullptr && cap->as_str() == nm) {
              idx.push_back(int_field(v, "id", -1));
              break;
            }
          }
        }
      }

      const JVal* exp = field(n, "exported");
      if (exp != nullptr && exp->kind() == JVal::kBool && exp->as_bool(false)) {
        out += "static hm_global_slot hm_greg_" + std::to_string(id) + ";\n";
      }
      out += "struct hm_ctx_" + std::to_string(id) + " {\n";
      if (idx.empty()) out += "  int unused;\n";
      for (int64_t vid : idx) out += "  Val v" + std::to_string(vid) + ";\n";
      out += "};\n";

      Gen sub(w, nodes, doc);
      sub.enter_slot(idx);
      out += "static int hm_slot_" + std::to_string(id) + "(void* raw) {\n";
      if (!idx.empty()) {
        const std::string ct = "struct hm_ctx_" + std::to_string(id);
        out += "  " + ct + "* c = (" + ct + "*)raw; (void)c;\n";
      } else {
        out += "  (void)raw;\n";
      }
      std::string sb, err;
      sub.set_slot_export_only(true);
      if (!sub.slot_body(int_field(n, "body", -1), &sb, &err)) continue;
      out += sb;
      out += "  return 1;\n";
      if (sb.find("goto hm_fail") != std::string::npos) out += "hm_fail:\n  return -1;\n";
      out += "}\n";
    }
  }
  (void)wf;
  return out;
}

// signal_states 对应 Go 的 signalStates：本单元用到的 (signal, channel) 组合，
// 每个一份条件变量 + 边沿标志。键是 FNV(signal) ^ FNV(chan)。
std::vector<std::uint64_t> signal_states(const JVal* nodes) {
  std::vector<std::uint64_t> keys;
  if (nodes == nullptr) return keys;
  for (size_t i = 0; i < nodes->size(); ++i) {
    const JVal* n = nodes->at(i);
    const char* k = str_field(n, "k");
    if (k == nullptr) continue;
    if (std::strcmp(k, "emit") != 0 && std::strcmp(k, "until") != 0) continue;
    const std::uint64_t key = sig_state_key(n);
    bool seen = false;
    for (std::uint64_t s : keys) if (s == key) { seen = true; break; }
    if (!seen) keys.push_back(key);
  }
  return keys;
}

// stmts_only 只生成语句（不生成入口序言）。unit AOT 的包装函数自己写序言 ——
// 序言里的 `hm_steps = 0; Val hm_result = ...` 若重复声明，产物直接编不过。
bool Gen::stmts_only(int64_t root_id, std::string* out, std::string* err) {
  out_ = out;
  depth_ = 1;
  if (!stmt(root_id, 1)) { *err = err_; return false; }
  return true;
}

bool Gen::body(std::string* out, std::string* err) {
  out_ = out;
  const JVal* root = field(wf_, "root");
  if (root == nullptr || root->kind() != JVal::kInt) { err_ = "workflow without root"; return false; }
  *out_ += "  hm_steps = 0;\n  Val hm_result = hm_int(0);\n";
  // 等待状态在入口初始化：之后 until 才能在同一把锁里清标志并进入等待。
  for (std::uint64_t key : signal_states(nodes_)) {
    *out_ += "  hm_pal_cond_init(&hm_sig_" + std::to_string(key) + ".cv);\n";
  }
  // 方案 A：只有监听 `export signal` 的槽进全局表；局部信号的槽留在单元内
  // 走编译期配对（那条路径零开销）。
  if (nodes_ != nullptr) {
    for (size_t i = 0; i < nodes_->size(); ++i) {
      const JVal* on = nodes_->at(i);
      const char* k = str_field(on, "k");
      if (k == nullptr || std::strcmp(k, "on") != 0) continue;
      const JVal* exp = field(on, "exported");
      if (exp == nullptr || exp->kind() != JVal::kBool || !exp->as_bool(false)) continue;
      char buf[256];
      std::snprintf(buf, sizeof(buf),
                    "  hm_slot_register(0x%016llxULL, 0x%016llxULL, hm_slot_%lld, (void*)0, &hm_greg_%lld);\n",
                    static_cast<unsigned long long>(hash_name(str_field(on, "signal"))),
                    static_cast<unsigned long long>(chan_key(on)),
                    static_cast<long long>(int_field(on, "id", 0)),
                    static_cast<long long>(int_field(on, "id", 0)));
      *out_ += buf;
    }
  }
  // 入口变量 `input` 不是 decl 绑定的：它由调用方在入口赋值，缺了它
  // 任何引用 input 的产物都编不过（"undeclared var_N"）。
  const JVal* vars = field(wf_, "vars");
  if (vars != nullptr) {
    for (size_t i = 0; i < vars->size(); ++i) {
      const JVal* v = vars->at(i);
      const char* nm = str_field(v, "name");
      const JVal* id = field(v, "id");
      if (nm != nullptr && std::strcmp(nm, "input") == 0 && id != nullptr && id->kind() == JVal::kInt) {
        // 声明在 build() 的 hoist 里；解析是否由外部完成见 input_extern_。
        if (!input_extern_) {
          *out_ += "  var_" + std::to_string(id->as_int(-1)) +
                   " = hm_parse_input(argc > 1 ? argv[1] : 0);\n";
        }
      }
    }
  }
  if (!stmt(root->as_int(0), 1)) { *err = err_; return false; }
  return true;
}

// 选入口 workflow：is_entry 为真者优先，否则第一个。
const JVal* pick_entry(const JVal* doc) {
  const JVal* wfs = field(doc, "workflows");
  if (wfs == nullptr || wfs->size() == 0) return nullptr;
  for (size_t i = 0; i < wfs->size(); ++i) {
    const JVal* w = wfs->at(i);
    const JVal* e = field(w, "is_entry");
    if (e != nullptr && e->kind() == JVal::kBool && e->as_bool(false)) return w;
  }
  return wfs->at(0);
}

// ---- unit AOT：把**一个导出函数**编成 C ABI 实现 ------------------------
// 对应 Go 的 internal/backend/unit.go。与 `hermes compile` 的区别：这里产出的是
// 静态库成员 `hermes_status hermes_<unit>_<fn>(const hermes_buf*, hermes_buf*)`，
// 由宿主的 C 程序在链接期直接调用 —— 不经任何运行期分发。
//
// 复用同一个 Gen：把函数当成"没有外部调用的 workflow"来看，语句生成路径完全
// 共享。两份语句生成器迟早会说出不同的话，而那种分歧只在运行时暴露。
int emit_unit(const char* ir_json, size_t len, const char* unit, const char* fn_name,
              std::string* out, std::string* err) {
  JVal root;
  size_t pos = 0;
  if (!parse_json(ir_json, len, &root, &pos)) { *err = "IR is not valid JSON"; return 1; }
  const JVal* functions = root.find("functions");
  if (functions == nullptr || functions->kind() != JVal::kArr) {
    *err = "unit AOT: this unit has no functions[]";
    return 1;
  }
  const JVal* fn = nullptr;
  for (size_t i = 0; i < functions->size(); ++i) {
    const JVal* f = functions->at(i);
    const JVal* nm = f ? f->find("name") : nullptr;
    if (nm != nullptr && nm->kind() == JVal::kStr && nm->as_str() == (fn_name ? fn_name : "")) {
      fn = f;
      break;
    }
  }
  if (fn == nullptr) { *err = std::string("unit AOT: function \"") + (fn_name ? fn_name : "") + "\" is not in functions[]"; return 1; }
  const JVal* params = fn->find("params");
  const size_t nparams = params ? params->size() : 0;
  if (nparams != 1) {
    *err = "unit AOT: only single-parameter functions are supported yet";
    return 1;
  }
  const JVal* rec = fn->find("recursive");
  if (rec != nullptr && rec->kind() == JVal::kBool && rec->as_bool(false)) {
    *err = "unit AOT: recursive functions are not supported yet";
    return 1;
  }
  const JVal* pname = params->at(0) ? params->at(0)->find("n") : nullptr;
  const char* pn = (pname != nullptr && pname->kind() == JVal::kStr) ? pname->as_str().c_str() : "";
  const JVal* vars = fn->find("vars");
  int64_t param_var = -1;
  if (vars != nullptr) {
    for (size_t i = 0; i < vars->size(); ++i) {
      const JVal* v = vars->at(i);
      const JVal* nm = v ? v->find("name") : nullptr;
      const JVal* id = v ? v->find("id") : nullptr;
      if (nm != nullptr && nm->kind() == JVal::kStr && nm->as_str() == pn && id != nullptr &&
          id->kind() == JVal::kInt) {
        param_var = id->as_int(-1);
        break;
      }
    }
  }
  if (param_var < 0) { *err = std::string("unit AOT: parameter \"") + pn + "\" has no variable slot"; return 1; }

  // 把函数包装成 workflow 形状，让 Gen 复用同一条语句生成路径。
  JVal wf = JVal::object();
  {
    const JVal* nm = fn->find("name");
    wf.set("name", nm != nullptr ? *nm : JVal::string(""));
    const JVal* r = fn->find("root");
    wf.set("root", r != nullptr ? *r : JVal::integer(0));
    const JVal* nodes = fn->find("nodes");
    wf.set("nodes", nodes != nullptr ? *nodes : JVal::array());
    wf.set("vars", vars != nullptr ? *vars : JVal::array());
    const JVal* ret = fn->find("returns");
    if (ret != nullptr) wf.set("returns", *ret);
  }

  std::string body, gerr;
  bool g_fatal = false;
  Gen g(&wf, wf.find("nodes"), &root);
  const JVal* wfroot = wf.find("root");
  const int64_t root_id = (wfroot != nullptr && wfroot->kind() == JVal::kInt) ? wfroot->as_int(0) : 0;
  if (!g.stmts_only(root_id, &body, &gerr)) {
    *err = gerr;
    return g_fatal ? 2 : 1;
  }

  *out = "/* Generated by hermes (unit AOT: .hbc as a static library); do not edit. */\n";
  *out += "#include <stdio.h>\n#include <stdlib.h>\n#include <string.h>\n\n";
  *out += "#include \"hermes/hermes_core.h\"\n\n";
  *out += kUnitErrorSlotDecl;
  *out += kUnitErrorSlotFallback;
  *out += "\n";
  *out += kPrelude;
  *out += kUnitToJSON;
  *out += "/* unit AOT: " + std::string(unit ? unit : "") + "." + std::string(fn_name ? fn_name : "") + " */\n";
  *out += "hermes_status hermes_" + std::string(unit ? unit : "") + "_" +
          std::string(fn_name ? fn_name : "") + "(const hermes_buf* in, hermes_buf* out) {\n";
  *out += "  long hm_steps = 0; Val hm_result = hm_int(0);\n";
  *out += "  char* hm_inb = 0; const char* hm_in = 0;\n";
  *out += "  if (in && in->ptr) { size_t hm_n = in->len; hm_inb = (char*)malloc(hm_n + 1); memcpy(hm_inb, in->ptr, hm_n); hm_inb[hm_n] = 0; hm_in = hm_inb; }\n";
  *out += "  Val var_" + std::to_string(param_var) + " = hm_parse_input(hm_in);\n";
  *out += "  hm_clear_error();\n";
  *out += "  hermes_last_error_clear();\n";
  *out += body;
  *out += "  goto hm_done;\n";
  *out += "hm_fail:\n";
  *out += "  { HermesError hm_e = hm_error_snapshot();\n";
  *out += "    size_t hm_e_len;\n";
  *out += "    hm_e_len = hermes_last_error_bounded_len(hm_e.msg);\n";
  *out += "    if (hm_e_len == 0) {\n";
  *out += "      hermes_last_error_publish((int32_t)hm_e.code, (int32_t)hm_e.status,\n";
  *out += "        \"AOT unit failed without error text\",\n";
  *out += "        sizeof(\"AOT unit failed without error text\") - 1);\n";
  *out += "    } else {\n";
  *out += "      hermes_last_error_publish((int32_t)hm_e.code, (int32_t)hm_e.status, hm_e.msg, hm_e_len);\n";
  *out += "    }\n";
  *out += "    if (hm_inb) free(hm_inb);\n";
  *out += "    return HERMES_ERR_INTERNAL; }\n";
  *out += "hm_done:\n";
  *out += "  (void)hm_steps;\n";
  *out += "  if (out) { out->ptr = hm_to_json(hm_result); out->len = strlen(out->ptr); }\n";
  *out += "  if (hm_inb) free(hm_inb);\n";
  *out += "  return HERMES_OK;\n";
  *out += "}\n";
  (void)len;
  return 0;
}

bool build(const char* ir_json, size_t len, std::string* out, std::string* err, bool* fatal) {
  JVal doc;
  size_t pos = 0;
  if (!parse_json(ir_json, len, &doc, &pos)) {
    *err = "IR is not valid canonical JSON (offset " + std::to_string(pos) + ")";
    return false;  // 致命
  }
  const JVal* wf = pick_entry(&doc);
  if (wf == nullptr) { *err = "no workflow in IR"; return false; }
  const JVal* nodes = field(wf, "nodes");
  if (nodes == nullptr) { *err = "workflow without nodes"; return false; }
  const char* name = str_field(wf, "name");

  std::string body;
  Gen g(wf, nodes, &doc);
  // input 由下面的 main 前置段解析（契约要在 body 之前读到它），所以告诉 Gen
  // 不要在自己的 body 里再解析一遍——**必须在 g.body() 之前调用**，
  // 否则标志置得太晚，body 已经生成完了。
  g.set_input_extern();
  g.set_hoisted();
  if (!g.body(&body, err)) {
    // 失败必须带原因：空的 err 会让调用方看到"未覆盖（无说明）"，
    // 那种提示既不能定位也不能判定该不该回落。
    if (err->empty()) *err = "IR assembly failed without a recorded reason";
    *fatal = g.fatal() || *fatal;
    return false;
  }

  *out = "/* Generated by hermes compile --backend=c; **do not edit**. */\n";
  *out += "/* Self-contained: values + control flow lowered to C; no IR, no Go runtime. */\n\n";
  *out += kPrelude;
  {
    std::string protos, perr;
    if (!native_prototypes(nodes, &protos, &perr)) {
      // 原型写不出来是**坏 IR / 超出 AOT 子集**，不是"节点没移植"：
      // 前者调用方不该回退到另一条实现（它同样编不出来）。
      *err = "backend(c): " + perr;
      *fatal = true;
      return false;
    }
    if (!protos.empty()) {
      *out += "\n/* Native C ABI prototypes: emitted at compile time, resolved by the linker. */\n";
      *out += "/* Duplicate symbols with different signatures keep the first declaration; */\n";
      *out += "/* signature-conflict diagnostics are tracked separately. */\n";
      *out += protos;
    }
  }

  // 只有用到 call_model / call_tool 的产物才带网络运行期（它引用 hermes_net_*）。
  if (uses_net(nodes)) *out += kNetPrelude;

  // 槽函数先于 hm_steps 生成：槽体里的一步也要计数，而它不在 main 的栈帧里。
  const std::string slots = gen_slots(&doc, wf);
  *out += "\nstatic long hm_steps = 0;\n";
  // 每个 (signal, channel) 一份等待状态：条件变量 + 边沿标志。
  for (std::uint64_t key : signal_states(nodes)) {
    *out += "static struct { hm_pal_cond_t cv; int flag; } hm_sig_" + std::to_string(key) + ";\n";
  }
  if (!slots.empty()) {
    *out += "\n/* Signal slots: closure-converted. No heap, no dictionary. */\n";
    *out += slots;
  }
  *out += "\nint main(int argc, char** argv) {\n  (void)argc; (void)argv;\n";
  *out += "  hm_clear_error();\n";
  // 变量声明统一提到顶部：契约检查（require）在 body 之前跑，而它引用的是
  // body 里声明的变量（`input` 就在第一条 decl 里）。不 hoist 的话 require
  // 会用到还没声明的 var_N —— 而那是 C 的编译错误，离"契约"很远。
  {
    const JVal* vs = wf->find("vars");
    if (vs != nullptr && vs->kind() == JVal::kArr) {
      for (size_t i = 0; i < vs->size(); ++i) {
        const JVal* v = vs->at(i);
        const JVal* id = v ? v->find("id") : nullptr;
        if (id == nullptr || id->kind() != JVal::kInt) continue;
        *out += "  Val var_" + std::to_string(id->as_int(-1)) + " = hm_int(0);\n";
      }
    }
  }
  // input 必须在 require 之前解析：契约检查要读它。顺序是
  //   清错误 → 解析 input → require → body → ensure
  // 少了这一步，require 读到的是零值，于是"合法输入也被判违约"。
  {
    const JVal* vs = wf->find("vars");
    if (vs != nullptr && vs->kind() == JVal::kArr) {
      for (size_t i = 0; i < vs->size(); ++i) {
        const JVal* v = vs->at(i);
        const char* nm = v ? str_field(v, "name") : nullptr;
        const JVal* id = v ? v->find("id") : nullptr;
        if (nm != nullptr && std::strcmp(nm, "input") == 0 && id != nullptr &&
            id->kind() == JVal::kInt) {
          *out += "  var_" + std::to_string(id->as_int(-1)) +
                  " = hm_parse_input(argc > 1 ? argv[1] : 0);\n";
          g.set_input_extern();
        }
      }
    }
  }
  // require：前置条件不成立时不该先执行任何东西，所以紧跟在清错误之后。
  {
    std::string checks;
    if (!g.contracts_c("require", &checks, 1)) {
      *err = "backend(c): cannot lower a require clause";
      *fatal = true;
      return false;
    }
    *out += checks;
  }
  *out += body;
  *out += "  goto hm_done;\n";
  *out += "hm_fail:\n";
  *out += "  { HermesError hm_e = hm_error_snapshot();\n";
  *out += "    fprintf(stderr, \"hermes: %s\\n\", hm_e.msg[0] ? hm_e.msg : \"runtime error\");\n";
  *out += "    return hm_e.code ? (int)hm_e.code : 1; }\n";
  *out += "hm_done:\n";
  *out += "  if(hm_result.k==4 && !hm_double_finite(hm_result.d)){ hm_set_error(1,\"non-finite double at JSON boundary\"); goto hm_fail; }\n";
  // ensure：所有 return 路径都汇到这里，所以只在这一处检查。
  {
    std::string checks;
    if (!g.contracts_c("ensure", &checks, 1)) {
      *err = "backend(c): cannot lower an ensure clause";
      *fatal = true;
      return false;
    }
    *out += checks;
  }
  *out += "  printf(\"{\\\"steps\\\":%ld,\\\"value\\\":\", hm_steps);\n";
  *out += "  hm_print_json(hm_result);\n";
  *out += "  printf(\",\\\"workflow\\\":\\\"" + c_escape(name) + "\\\"}\\n\");\n";
  *out += "  return 0;\n";
  *out += "}\n";
  return true;
}

bool put(const std::string& text, char* out, size_t cap, size_t* out_len) {
  if (out_len) *out_len = text.size();
  if (out == nullptr || cap < text.size()) return false;
  if (!text.empty()) std::memcpy(out, text.data(), text.size());
  return true;
}

}  // namespace
}  // namespace hm

// 导出面必须显式放开可见性：前端整体用 -fvisibility=hidden 编译。
// 不加这条，符号会变成局部符号 —— 链接期报"未定义"，而 nm 里看着"编过了"。
#pragma GCC visibility push(default)
// hm_backend_unit_emit：把 functions[] 里的一个导出函数编成 C ABI 实现。
// 返回码与 hm_backend_c_emit 同约定：0 成功 / 1 未覆盖（回落 Go）/ 2 致命。
extern "C" int hm_backend_unit_emit(const char* ir_json, size_t len, const char* unit,
                                    const char* fn_name, char* out, size_t cap, size_t* out_len,
                                    char* out_err, size_t err_cap, size_t* err_len) {
  if (ir_json == nullptr) {
    hm::put("hm_backend_unit_emit: nil IR", out_err, err_cap, err_len);
    return 2;
  }
  std::string c_src, err;
  const int rc = hm::emit_unit(ir_json, len, unit, fn_name, &c_src, &err);
  if (rc != 0) {
    hm::put(err, out_err, err_cap, err_len);
    if (out_len) *out_len = 0;
    return rc;
  }
  if (!hm::put(c_src, out, cap, out_len)) {
    hm::put("unit AOT: output buffer too small", out_err, err_cap, err_len);
    return 2;
  }
  if (err_len) *err_len = 0;
  return 0;
}

extern "C" int hm_backend_c_emit(const char* ir_json, size_t len,
                                 char* out, size_t cap, size_t* out_len,
                                 char* out_err, size_t err_cap, size_t* err_len) {
  if (ir_json == nullptr) {
    hm::put("hm_backend_c_emit: nil IR", out_err, err_cap, err_len);
    return 2;
  }
  std::string c_src;
  std::string err;
  bool fatal = false;
  if (!hm::build(ir_json, len, &c_src, &err, &fatal)) {
    if (fatal) {
      hm::put(err, out_err, err_cap, err_len);
      if (out_len) *out_len = 0;
      return 2;
    }
    // 注意两段式的含义：build 失败分两类，这里用 err 是否为空区分不现实，
    // 所以由 build 的返回语义决定 —— 见下面的注释。
    if (!hm::put(err, out_err, err_cap, err_len)) return 2;
    if (out_len) *out_len = 0;
    return 1;  // 未覆盖：调用方回落 Go 版
  }
  if (!hm::put(c_src, out, cap, out_len)) {
    hm::put("output buffer too small", out_err, err_cap, err_len);
    return 2;
  }
  if (err_len) *err_len = 0;
  return 0;
}
#pragma GCC visibility pop
