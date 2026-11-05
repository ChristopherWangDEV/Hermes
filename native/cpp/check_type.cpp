// check_type.cpp —— 类型检查 pass（T11）。
//
// ## 检查项归属（哪些在这里、哪些必须在 Go 侧）
//
// 分工的唯一依据是**这个 pass 看得见什么**。它拿到的是一个文件的 AST，
// 外加 is_header 标志；它看不到别的文件、看不到链接进来的 .hbc、不知道
// 自己是不是 root unit。按这条线划：
//
//   在这里（单文件可判）
//     E4016  字段访问（struct 没有该字段 / 对非 struct 取字段）
//     E4021  返回类型与声明不一致
//     E3016  用了 tool.call / model.generate 却没链对应 std 模块
//            （#link 就写在同一个文件里）
//     E3019  地址字面量里带 user:pass@（值看得见）
//     E3005/E3017/E3020  各文件的语法限制（由 parser 在语法层拦）
//     E2018  auth env("K", 默认值)（parser 在语法层拦）
//     E3018  env 第二参非字面量（parser 只吃 STRING，语法层已保证）
//
//   必须在 Go 侧（需要跨文件或需要"我是谁"的信息）
//     E3002  include 环            —— 需要 include 图（internal/include）
//     E3015  #include "x.hermes"   —— 同上
//     E1009  路径逃逸              —— 同上
//     E3021/E3022  零个/多个 entry —— **需要知道这是不是 root unit**：
//                 库文件允许 0 个 entry，在单文件阶段报会误伤每个库
//     E3024  entry 未导出          —— 需要 .h 里有没有 export 声明；
//                 examples/support_bot 自己就没写 export，单文件判会误报
//     E3014  声明与定义不一致      —— .h 与 .hermes 的比对（internal/link）
//     E3013  input 类型未 export   —— 同上
//     E3012  export 写两次         —— 声明(.h) 与定义(.hermes) 各写一次才算
//
// **不去做"看得见就顺手查"的诱惑**：E3021 在单文件里判会误伤所有库文件，
// 而误报比漏报更糟 —— 它会让"用这套语言写库"变成不可能。
//
// 设计约束与其他 pass 一致：无异常、无 RTTI、无 IO、无全局可变状态。

#include "arena.h"
#include "ast.h"

#include <cstdarg>
#include <cstdio>
#include <cstring>
#include <deque>
#include <string>
#include <vector>

namespace hm {

namespace {

// Ty 是检查器内部的类型表示。用 arena 分配而不是值语义数组，
// 因为数组元素要能互相引用（`array<array<T>>` 虽被语言禁止，但表示层不该先假定）。
struct Ty {
  enum K { Int, Float, Bool, Str, Json, Cost, StructK, ArrayK, TupleK, HttpResK, Unknown } k;
  int struct_idx;  // StructK：structs_ 的下标
  Ty* elem;        // ArrayK
};

struct ExtField {
  const char* name;
  const char* type;  // 类型名：基础类型或另一个 struct 的名字
};

struct StructInfo {
  const char* name;
  Node* decl;  // StructDecl，child = Field 链；**为空表示声明在别的模块**
  // 跨文件声明（D-027 推论）：单文件检查器看不到别的 .h，而"看不见就报未知"
  // 会把每个正常的多文件工程判错。外部声明由 Go 侧收集后传进来，这里只存名字。
  std::vector<ExtField> ext;
};

struct VarSym {
  const char* name;
  Ty* t;
  Loc loc;
};

class Checker {
 public:
  Checker(Arena& a, DiagBag& d, const char* file) : a_(a), d_(d), file_(file) {}

  // load_externals_ 解析 Go 侧传来的声明表：每行 `Struct\tField\tType`。
  //
  // 为什么用这个笨格式而不是 JSON：前端被定义为无 IO、无第三方库，为了一条
  // 声明表在 C++ 里手写一个 JSON 解析器，是为省一次 split 而引入一个必错的东西。
  // 生产者与消费者都是我们自己的代码，格式越笨越好验。
  void load_externals_(const char* text, size_t len) {
    std::string cur;
    for (size_t i = 0; i <= len; ++i) {
      const bool end = (i == len) || (text[i] == '\n');
      if (!end) {
        cur.push_back(text[i]);
        continue;
      }
      if (!cur.empty()) {
        const size_t t1 = cur.find('\t');
        const size_t t2 = (t1 == std::string::npos) ? std::string::npos : cur.find('\t', t1 + 1);
        if (t1 != std::string::npos && t2 != std::string::npos) {
          ext_storage_.push_back(cur.substr(0, t1));
          ext_storage_.push_back(cur.substr(t1 + 1, t2 - t1 - 1));
          ext_storage_.push_back(cur.substr(t2 + 1));
          const std::string& a = ext_storage_[ext_storage_.size() - 3];
          const std::string& b = ext_storage_[ext_storage_.size() - 2];
          const std::string& c = ext_storage_[ext_storage_.size() - 1];
          add_external_(a.c_str(), b.c_str(), c.c_str());
        }
        cur.clear();
      }
    }
  }


  void run(Node* mod) {
    if (!mod) return;
    collect_(mod);
    collect_links_(mod);
    for (Node* c = mod->child; c; c = c->next) {
      if (c->kind == Kind::BackendDef) check_backend_addr_(c);
    }
    for (Node* c = mod->child; c; c = c->next) {
      if (c->kind == Kind::WorkflowDecl) check_workflow_(c);
    }
  }

 private:
  Arena& a_;
  DiagBag& d_;
  const char* file_;

  std::vector<StructInfo> structs_;
  // 外部声明字符串的**稳定存储**：用 deque 而不是 vector，因为 vector 扩容会
  // 让 c_str() 指针失效，而 structs_ 里存的是这些指针。
  std::deque<std::string> ext_storage_;
  std::vector<VarSym> scope_;

  // 本 unit 里 #link <std/X> 出现过哪些 X。E3016 靠它判断
  // "用了 tool.call 却没链接 <std/net>" —— 这个判断只需要单文件信息。
  bool has_std_net_ = false;
  bool has_std_model_ = false;
  // 每个文件只报一次，不逐处刷屏。
  bool reported_net_ = false;
  bool reported_model_ = false;

  Ty* declared_ret_ = nullptr;  // 当前 workflow 在 .h/.hermes 上写的返回类型

  // 已经从 `parallel` 块里退出去的名字。E5002 靠它区分"从没定义"与
  // "定义了但出了块" —— 两种情况的修复动作完全不同：前者是拼错或漏 include，
  // 后者是架构问题（跨并行分支共享中间结果本来就不成立）。
  std::vector<const char*> closed_;

  // ---- 基础 ----

  Ty* mk(Ty::K k) {
    Ty* t = make_node<Ty>(a_);
    if (t) {
      t->k = k;
      t->struct_idx = -1;
      t->elem = nullptr;
    }
    return t;
  }

  Ty* unknown() { return mk(Ty::Unknown); }

  void err(const char* code, const char* msg, Loc loc, const char* why, const char* fix) {
    Diag d;
    memset(&d, 0, sizeof(d));
    d.severity = Severity::Error;
    d.code = code;
    d.message = msg;
    d.loc = loc;
    d.module = file_;
    d.why = why;
    d.fix = fix;
    diag_add(d_, a_, d);
  }

  const char* internf(const char* fmt, ...) {
    char buf[512];
    va_list ap;
    va_start(ap, fmt);
    vsnprintf(buf, sizeof(buf), fmt, ap);
    va_end(ap);
    return a_.intern(buf, strlen(buf));
  }

  // ---- 声明收集 ----

  void collect_(Node* mod) {
    for (Node* c = mod->child; c; c = c->next) {
      if (c->kind != Kind::StructDecl) continue;
      StructInfo si;
      si.name = c->text ? c->text : "";
      si.decl = c;
      structs_.push_back(si);
    }
  }

  // collect_links_ 记录出现过的 #link <std/*>。
  // Link 节点的 text 是 spec 原文，带尖括号：`<std/net>`。
  void collect_links_(Node* mod) {
    for (Node* c = mod->child; c; c = c->next) {
      if (c->kind != Kind::Link || !c->text) continue;
      const char* p = c->text;
      if (*p == '<') ++p;
      const char* e = p;
      while (*e && *e != '>') ++e;
      size_t n = static_cast<size_t>(e - p);
      if (n == 7 && strncmp(p, "std/net", 7) == 0) has_std_net_ = true;
      if (n == 9 && strncmp(p, "std/model", 9) == 0) has_std_model_ = true;
    }
  }

  int find_struct_(const char* name) {
    for (size_t i = 0; i < structs_.size(); ++i) {
      if (structs_[i].name && strcmp(structs_[i].name, name) == 0) return static_cast<int>(i);
    }
    return -1;
  }

  // from_type_ 把一个 Type 节点翻成 Ty。数组维数是 Type.i64（parse_type 填的）。
  Ty* from_type_(Node* t) {
    if (!t || t->kind != Kind::Type) return unknown();
    const char* s = t->text ? t->text : "";
    Ty* base = ty_from_name_(s);
    if (!base) return unknown();
    for (int k = 0; k < static_cast<int>(t->i64); ++k) {
      Ty* arr = mk(Ty::ArrayK);
      if (!arr) break;
      arr->elem = base;
      base = arr;
    }
    return base;
  }

  // ty_from_name_ 只认一个名字（不含数组维度），外部声明与本地字段共用它 ——
  // 两份映射必然漂移，而这正是"同一个类型在两个地方理解不同"的来源。
  Ty* ty_from_name_(const char* s) {
    if (strcmp(s, "string") == 0) return mk(Ty::Str);
    if (strcmp(s, "int") == 0) return mk(Ty::Int);
    if (strcmp(s, "float") == 0) return mk(Ty::Float);
    if (strcmp(s, "bool") == 0) return mk(Ty::Bool);
    if (strcmp(s, "cost") == 0) return mk(Ty::Cost);
    if (strcmp(s, "json") == 0) return mk(Ty::Json);
    int si = find_struct_(s);
    if (si < 0) return nullptr;
    Ty* t = mk(Ty::StructK);
    if (t) t->struct_idx = si;
    return t;
  }

  // struct_field_ 按名字找字段并返回类型；找不到返回 nullptr 且回填下标 -1。
  Ty* struct_field_(int si, const char* name) {
    if (si < 0 || si >= static_cast<int>(structs_.size())) return nullptr;
    if (!structs_[si].decl) {
      // 外部声明：字段表由 Go 侧收集后传入（没有 AST 可走）。
      for (const ExtField& f : structs_[si].ext) {
        if (f.name && strcmp(f.name, name) == 0) {
          Ty* t = ty_from_name_(f.type ? f.type : "");
          return t ? t : unknown();
        }
      }
      return nullptr;
    }
    for (Node* f = structs_[si].decl->child; f; f = f->next) {
      if (f->kind != Kind::Field) continue;
      if (f->text && strcmp(f->text, name) == 0) return from_type_(f->child);
    }
    return nullptr;
  }

  // add_external_ 登记一条来自别的模块的字段。同名 struct 已存在就并入
  // （本地声明优先：本地有 AST，字段表比外部那份更可信）。
  void add_external_(const char* sname, const char* fname, const char* ftype) {
    int si = find_struct_(sname);
    if (si < 0) {
      StructInfo si2;
      si2.name = sname;
      si2.decl = nullptr;
      structs_.push_back(si2);
      si = static_cast<int>(structs_.size()) - 1;
    }
    if (structs_[si].decl) return;  // 本地声明优先
    ExtField f;
    f.name = fname;
    f.type = ftype;
    structs_[si].ext.push_back(f);
  }

  const char* type_name_(Ty* t) {
    if (!t) return "?";
    switch (t->k) {
      case Ty::Int: return "int";
      case Ty::Float: return "float";
      case Ty::Bool: return "bool";
      case Ty::Str: return "string";
      case Ty::Json: return "json";
      case Ty::Cost: return "cost";
      case Ty::StructK:
        if (t->struct_idx >= 0 && t->struct_idx < static_cast<int>(structs_.size()))
          return structs_[t->struct_idx].name;
        return "struct?";
      case Ty::ArrayK: return "array";
      case Ty::TupleK: return "tuple";
      case Ty::HttpResK: return "httpresult";
      case Ty::Unknown: return "unknown";
    }
    return "?";
  }

  // list_fields_ 列出 struct 的字段名，用于"未知字段"的修复建议。
  // 不列字段的 AttributeError 是 Python 最让人恼火的一点：它说你错了，不说你有啥。
  const char* list_fields_(int si) {
    if (si < 0 || si >= static_cast<int>(structs_.size())) return "(none)";
    // `decl` 可以为空：从 externals（别的模块的头）登记进来的 struct 没有本地 AST。
    // 不挡它，`decl->child` 就是空指针解引用 —— 用户看到的不是"拼错字段名"的提示，
    // 而是**编译器崩溃**，而这两者之间差着一整轮调试。
    if (!structs_[si].decl) return "(none)";
    char buf[512];
    int used = 0;
    buf[0] = '\0';
    for (Node* f = structs_[si].decl->child; f; f = f->next) {
      if (f->kind != Kind::Field || !f->text) continue;
      int n = snprintf(buf + used, sizeof(buf) - used, "%s%s", used ? ", " : "", f->text);
      if (n > 0) used += n;
      if (used >= static_cast<int>(sizeof(buf)) - 2) break;
    }
    return a_.intern(buf, strlen(buf));
  }

  Ty* lookup_(const char* name) {
    for (int i = static_cast<int>(scope_.size()) - 1; i >= 0; --i) {
      if (scope_[i].name && strcmp(scope_[i].name, name) == 0) return scope_[i].t;
    }
    return nullptr;
  }

  // ---- backend 地址（E3019）----

  // check_backend_addr_ 检查**看得见的字面量**：`backend x = "..."` 的地址，
  // 以及 `env("URL", "...")` 的默认值。`env("URL")` 单实参形式不查 ——
  // 值要到运行期才知道，编译期无从判断，硬查只能靠猜。
  //
  // 为什么这条值得一个错误码：地址会被写进 IR、进 .hbc、进签名产物、
  // 进 `hermes explain` 的输出。一个带 user:pass@ 的字面量会沿着所有这些
  // 路径扩散，而它的修复动作（换成 env）与"地址写错了"完全不同。
  void check_backend_addr_(Node* bd) {
    if (!bd) return;
    const char* name = bd->text ? bd->text : "?";
    check_credential_(bd->child, name);
    Node* addr = bd->child;
    if (addr && addr->kind == Kind::EnvExpr) {
      // **默认值在 `args` 上，不在 `child->next` 上**（见 parser.cpp 的
      // parse_env_clause：`env->child->text = 名; env->args->text = 默认值;`）。
      // 按兄弟链取会拿到 null 而静默跳过 —— 这正是"漏查默认值"那个 bug。
      Node* second = nullptr;
      if (addr->child && addr->child->next) second = addr->child->next;  // 兼容兄弟链写法
      if (!second) second = addr->args;
      if (!second && addr->n_subs >= 2) second = &addr->subs[1];          // 兼容 subs 写法
      if (second) check_credential_(second, name);
    }
  }

  void check_credential_(Node* lit, const char* backend_name) {
    // 无插值的字符串：text 就是内容、n_subs == 0。
    // 有插值的字符串：text 是类型标记 "string"、内容在 subs —— 那种情形
    // 地址含什么要到运行期才知道，不在这里判。
    if (!lit || lit->kind != Kind::LitExpr || lit->n_subs != 0 || !lit->text) return;
    const char* s = lit->text;
    const char* scheme = strstr(s, "://");
    if (!scheme) return;
    if (!strchr(scheme, '@')) return;
    err("E3019", "backend address carries credentials (user:pass@host)", lit->loc,
        "The address is written into the IR, the archive, and every explain output. A literal "
        "user:pass@ is how credentials leak into artifacts that get copied to build servers and "
        "attached to bug reports.",
        internf("move the whole URL into the environment: backend %s = env(\"URL\");", backend_name));
  }

  // ---- 表达式 ----

  Ty* type_of_(Node* e) {
    if (!e) return unknown();
    switch (e->kind) {
      case Kind::LitExpr: {
        // parse_lit 对无插值字符串把**内容**写进 text（n_subs == 0），
        // 对有插值字符串把**类型标记**写进 text、内容写进 subs。
        // 这里只按类型标记判断，不看 text 的内容。
        const char* t = e->text ? e->text : "";
        if (strcmp(t, "string") == 0) return mk(Ty::Str);
        if (strcmp(t, "int") == 0) return mk(Ty::Int);
        if (strcmp(t, "float") == 0) return mk(Ty::Float);
        if (strcmp(t, "bool") == 0) return mk(Ty::Bool);
        if (strcmp(t, "cost") == 0) return mk(Ty::Cost);
        return unknown();
      }

      case Kind::IdentExpr: {
        Ty* t = lookup_(e->text);
        if (t) return t;
        for (size_t i = 0; i < closed_.size(); ++i) {
          if (closed_[i] && e->text && strcmp(closed_[i], e->text) == 0) {
            err("E5002",
                internf("%s is declared inside a `parallel` block and is not visible here", e->text),
                e->loc,
                "Each branch of `parallel` runs concurrently, so a value produced inside the block "
                "does not exist as far as the code after it is concerned — the block's own "
                "`return [...]` is what carries results out. Referencing it here would read a slot "
                "that no branch owns.",
                "read the result from the block instead: `auto [a, b] = parallel { ... };`");
            return unknown();
          }
        }
        // 从没定义：可能是别的 .h 里的声明，单文件阶段不报。
        return unknown();
      }

      case Kind::PathExpr: {
        if (!e->subs || e->n_subs < 1) return unknown();
        Ty* cur = type_of_(&e->subs[0]);
        // `type_of_` 不认识某个表达式时会返回 nullptr（"暂时未知"）。
        // 不挡住它，下面第一句 `cur->k` 就是空指针解引用 —— 而症状是**编译器崩溃**，
        // 不是一条诊断，用户拿到的只有一个 SIGSEGV。
        if (!cur) return unknown();
        for (int i = 1; i < e->n_subs; ++i) {
          if (!cur) return unknown();  // 上一轮可能把 cur 置空（防御性，见下方各处）
          Node& f = e->subs[i];
          if (cur->k == Ty::Json) continue;  // json 任意字段都合法，类型仍是 json
          // `httpresult<T>` 是 {status, body, headers} 三元组（LANGUAGE §5.6）——
          // 它就是 allow_status 存在的意义：调用方要能写 `if (r.status == 404)`。
          // 不认这三个字段，allow_status 的返回值就没法用。
          if (cur->k == Ty::HttpResK) {
            const char* fname = f.text ? f.text : "";
            if (std::string(fname) == "status") { cur = mk(Ty::Int); continue; }
            if (std::string(fname) == "headers") { cur = mk(Ty::Json); continue; }
            // body 的元素类型目前用 elem 承载（与 array 共用同一个槽位）。
            if (std::string(fname) == "body") { cur = cur->elem ? cur->elem : mk(Ty::Json); continue; }
            err("E4016", internf("httpresult has no field %s", fname), f.loc,
                "HttpResult<T> has exactly status, body and headers.",
                "use r.status / r.body / r.headers");
            return unknown();
          }
          if (cur->k != Ty::StructK) {
            err("E4016",
                internf("cannot access field %s on %s", f.text ? f.text : "?", type_name_(cur)),
                f.loc,
                "Only struct and json values have fields. Accessing a field on another type "
                "almost always means the variable holds something other than you expect.",
                internf("check what %s is: it is %s here", f.text ? f.text : "?", type_name_(cur)));
            return unknown();
          }
          Ty* ft = struct_field_(cur->struct_idx, f.text ? f.text : "");
          if (!ft) {
            const char* sname = (cur->struct_idx >= 0) ? structs_[cur->struct_idx].name : "?";
            err("E4016", internf("struct %s has no field %s", sname, f.text ? f.text : "?"), f.loc,
                "Field access is checked against the struct declaration. A misspelled or removed "
                "field would otherwise become `json null` at run time, and the workflow would keep "
                "running with a silently missing value.",
                internf("%s has: %s", sname, list_fields_(cur->struct_idx)));
            return unknown();
          }
          cur = ft;
        }
        return cur;
      }

      case Kind::ArrayLit: {
        // 定长数组：元素类型取第一个元素的类型（全同是常态）；空数组是 json 数组。
        Ty* arr = mk(Ty::ArrayK);
        if (e->child) {
          Ty* first = type_of_(e->child);
          arr->elem = (first && first->k != Ty::Unknown) ? first : mk(Ty::Json);
          for (const Node* c = e->child->next; c; c = c->next) {
            type_of_(const_cast<Node*>(c));  // 仍然逐个检查，避免漏掉内部错误
          }
        }
        return arr;
      }

      case Kind::IndexExpr: {
        Ty* base = e->child ? type_of_(e->child) : nullptr;
        if (e->text != nullptr) {
          // `j["k"]`：json 常量键，结果仍是 json
          return mk(Ty::Json);
        }
        // `xs[i]`：数组下标，结果是元素类型。基类型不是数组就报错 ——
        // 否则"对字符串取下标"这类写法会一路走到运行期。
        if (base == nullptr) return mk(Ty::Json);
        if (base->k == Ty::ArrayK) {
          if (e->args) {
            Ty* it = type_of_(e->args);
            if (it && it->k != Ty::Int && it->k != Ty::Unknown) {
              err("E4023", "array index must be an integer", e->loc,
                  "Indexing an array with a non-integer can never select an element.",
                  "use an int expression");
            }
          }
          return base->elem ? base->elem : mk(Ty::Json);
        }
        if (base->k == Ty::Json) return mk(Ty::Json);
        err("E4022", "cannot index a value that is not an array", e->loc,
            "Only arrays and json support indexing.",
            "check what the value is before indexing it");
        return mk(Ty::Json);
      }

      case Kind::BinaryExpr: {
        Ty* l = type_of_(e->child);
        Ty* r = type_of_(e->args);
        // type_of_ 在"暂时未知"时可能返回 nullptr（例如跨文件还没有声明）。
        // 直接 `l->k` 会变成**编译器崩溃**而不是一条诊断。
        if (!l || !r) return unknown();
        const char* op = e->text ? e->text : "";
        if (strcmp(op, "==") == 0 || strcmp(op, "!=") == 0 || strcmp(op, "<") == 0 ||
            strcmp(op, "<=") == 0 || strcmp(op, ">") == 0 || strcmp(op, ">=") == 0 ||
            strcmp(op, "&&") == 0 || strcmp(op, "||") == 0) {
          return mk(Ty::Bool);
        }
        if (l->k == Ty::Unknown || r->k == Ty::Unknown) return unknown();
        if (l->k == Ty::Cost || r->k == Ty::Cost) return mk(Ty::Cost);
        if (l->k == Ty::Float || r->k == Ty::Float) return mk(Ty::Float);
        if (l->k == Ty::Int && r->k == Ty::Int) return mk(Ty::Int);
        if (l->k == Ty::Str && r->k == Ty::Str) return mk(Ty::Str);
        return unknown();
      }

      case Kind::UnaryExpr: {
        Ty* v = type_of_(e->child);
        if (!v) return unknown();
        const char* op = e->text ? e->text : "";
        if (strcmp(op, "!") == 0) return mk(Ty::Bool);
        return v;
      }

      case Kind::CallExpr: {
        Node* callee = e->child;
        // 参数先全走一遍，保证里面的字段错误不会因为外层提前 return 被漏掉。
        // 注意 args 是 Param 节点（命名参数 `prompt = "x"`），不是表达式链。
        for (Node* arg = e->args; arg; arg = arg->next) type_of_(arg);

        // E3016：用了 tool.call / model.generate 却没链对应的 std 模块。
        // 这是**单文件**能判定的事（#link 就写在同一个文件里）。
        const char* cname = path_text(callee);
        if (cname) {
          if (strcmp(cname, "tool.call") == 0 && !has_std_net_ && !reported_net_) {
            reported_net_ = true;
            err("E3016", "tool.call needs #link <std/net>", e->loc,
                "A std module's declarations come from #include, but its implementation comes from "
                "#link. Using the call without linking it leaves the symbol unresolved at link "
                "time — which is a build failure much later than this line.",
                "add `#link <std/net>;` at the top of the file");
          }
          if (strcmp(cname, "model.generate") == 0 && !has_std_model_ && !reported_model_) {
            reported_model_ = true;
            err("E3016", "model.generate needs #link <std/model>", e->loc,
                "A std module's declarations come from #include, but its implementation comes from "
                "#link. Using the call without linking it leaves the symbol unresolved at link "
                "time — which is a build failure much later than this line.",
                "add `#link <std/model>;` at the top of the file");
          }
        }
        // **先看类型实参，再看被调者名字**：`<T>` 本身就是权威的类型来源，
        // 不依赖把 `model.generate` 拼成字符串（保留命名空间的 token 不保证带文本）。
        // 泛型调用的类型实参在 **child 链**上（`PathExpr` → `Type` → `Param`…），
        // 不在 `type_args` 槽位里 —— 这一点 codegen 早就用 children_of 处理过，
        // 类型检查这里之前没跟上，于是 `tool.call<T>` 的类型成了 unknown。
        // 子节点可能在 **child 链**、**type_args 链**或 **args 链**上
        // （codegen 的 children_of 就是这么收的，踩过三次）。只扫 child 链
        // 会让 `tool.call<T>()` 的类型实参看不见，类型退化成 unknown。
        Ty* inner = nullptr;
        bool hasAllow = false;
        for (const Node* chain : {e->child, e->type_args, e->args}) {
          for (const Node* c = chain; c; c = c->next) {
            if (c->kind == Kind::Type && !inner) inner = struct_from_type_args_(const_cast<Node*>(c));
            if (c->kind == Kind::Param && c->text && strcmp(c->text, "allow_status") == 0) hasAllow = true;
          }
        }
        if (inner) {
          // `allow_status` 非空时返回 **HttpResult<T>**（LANGUAGE §5.6）：
          // 没有它，调用方写 `r.status` 会得到 "cannot access field status on unknown"，
          // 而 allow_status 的全部意义就是让调用方能看 status。
          if (hasAllow) {
            Ty* h = mk(Ty::HttpResK);
            h->elem = inner;
            h->struct_idx = -1;
            return h;
          }
          return inner;
        }
        if (e->type_args) return struct_from_type_args_(e->type_args);
        if (cname && strncmp(cname, "json.", 5) == 0) {
          if (strcmp(cname, "json.as_string") == 0) return mk(Ty::Str);
          if (strcmp(cname, "json.as_int") == 0) return mk(Ty::Int);
          if (strcmp(cname, "json.as_float") == 0) return mk(Ty::Float);
          if (strcmp(cname, "json.as_bool") == 0) return mk(Ty::Bool);
          if (strcmp(cname, "json.as_array") == 0) return mk(Ty::ArrayK);
          return mk(Ty::Json);
        }
        return unknown();
      }

      case Kind::ParallelExpr: return parallel_(e);
      default: return unknown();
    }
  }

  // parallel_ 处理 `parallel { ... }`：块内变量在出块时**关闭**。
  //
  // 这是 E5002 的落点。记住进入时的作用域水位，出块后把新增的名字记进
  // closed_ 再截断 —— 于是块外引用它们时 lookup_ 失败、closed_ 命中，
  // 报出的是"出了块"而不是"从没定义"。
  Ty* parallel_(Node* e) {
    size_t mark = scope_.size();
    block_(e->child);
    for (size_t i = mark; i < scope_.size(); ++i) closed_.push_back(scope_[i].name);
    scope_.resize(mark);
    return mk(Ty::TupleK);
  }

  // struct_from_type_args_ 处理 model.generate<Intent>(...)：类型实参必须是 struct。
  Ty* struct_from_type_args_(Node* ta) {
    if (!ta) return mk(Ty::Json);
    const char* s = ta->text ? ta->text : "";
    int si = find_struct_(s);
    if (si < 0) return unknown();  // 单文件看不到别的 .h 里的 struct，不报错
    Ty* t = mk(Ty::StructK);
    if (t) t->struct_idx = si;
    return t;
  }

  // path_text 把 `model.generate` 这样的 PathExpr 拼回点分字符串。
  const char* path_text(Node* e) {
    if (!e) return nullptr;
    if (e->kind == Kind::IdentExpr) return e->text;
    if (e->kind != Kind::PathExpr || !e->subs || e->n_subs < 1) return nullptr;
    char buf[256];
    int used = 0;
    buf[0] = '\0';
    for (int i = 0; i < e->n_subs; ++i) {
      Node& p = e->subs[i];
      if (!p.text) continue;
      int n = snprintf(buf + used, sizeof(buf) - used, "%s%s", used ? "." : "", p.text);
      if (n > 0) used += n;
      if (used >= static_cast<int>(sizeof(buf)) - 2) break;
    }
    return a_.intern(buf, strlen(buf));
  }

  // ---- 语句 ----

  void block_(Node* b) {
    for (Node* s = b; s; s = s->next) stmt_(s);
  }

  void stmt_(Node* s) {
    if (!s) return;
    switch (s->kind) {
      case Kind::Block: block_(s->child); break;

      case Kind::LetStmt: {
        Ty* init = type_of_(s->args);
        // 绑定的名字在 **Pattern.child（IdentExpr）** 上，不在 Pattern.text 上
        // （见 parser.cpp 的 parse_pattern）。读错字段的后果是静默失效：
        // 每个 let 变量都没进作用域，后续推导全退化成 unknown，而且不报错。
        Node* pat = s->child;
        const char* pname = nullptr;
        if (pat && pat->kind == Kind::Pattern) {
          if (pat->child && pat->child->kind == Kind::IdentExpr) pname = pat->child->text;
          if (!pname) pname = pat->text;
        }
        if (pname) {
          VarSym v;
          v.name = pname;
          v.t = init;
          v.loc = pat->loc;
          scope_.push_back(v);
        }
        break;
      }

      case Kind::ReturnStmt: {
        Ty* t = type_of_(s->child);
        // E4022：不能 return 一个 tuple。tuple 只由 `parallel` 产生，
        // 且**不可命名、不可存储**（IR.md §3.1）—— 它是 fork/join 的中间态，
        // 一旦被 return 出去，调用方拿到一个无法用语言类型描述的东西。
        if (t->k == Ty::TupleK) {
          err("E4022", "cannot return a tuple", s->loc,
              "A tuple exists only as the result of `parallel { ... }`, and it cannot be named or "
              "stored (IR.md §3.1). Returning one would hand the caller a value the type system "
              "has no way to describe — so the workflow's return contract could not be written down.",
              "destructure it first: `let [a, b] = parallel { ... };` then return a single value");
          break;
        }
        // 数组是**合法的 JSON 值**：声明 `-> json` 的 workflow 可以返回数组。
        // 反过来不行（声明数组却返回标量），所以只放宽这一个方向。
        const bool arr_as_json =
            declared_ret_ && declared_ret_->k == Ty::Json && t && t->k == Ty::ArrayK;
        if (declared_ret_ && t->k != Ty::Unknown && !arr_as_json && !same_type_(t, declared_ret_)) {
          err("E4021",
              internf("return type mismatch: declared %s, this return gives %s",
                      type_name_(declared_ret_), type_name_(t)),
              s->loc,
              "The declared return type is the workflow's contract — it is what callers and the "
              "HTTP surface see. If the body can return two different types, the contract is "
              "wrong, not just this statement.",
              internf("make this return give %s, or change the declared type",
                      type_name_(declared_ret_)));
        }
        break;
      }

      case Kind::ExprStmt: type_of_(s->child); break;

      case Kind::IfStmt:
        type_of_(s->child);
        block_(s->args);
        if (s->else_branch) {
          if (s->else_branch->kind == Kind::IfStmt) stmt_(s->else_branch);
          else block_(s->else_branch);
        }
        break;

      case Kind::RetryStmt: block_(s->args); break;

      case Kind::TryStmt: {
        // 结构（parse_try）：child = try 体 Block；args = CatchClause 链；
        // 每个 CatchClause：text = 类型名，child = 变量 IdentExpr，
        // **else_branch = catch 体 Block**。
        //
        // 原来写的是 `block_(s->args)` —— 那等于把 CatchClause 当语句遍历，
        // try 的 catch 体从来没被检查过（块内的字段错误全部漏掉）。
        block_(s->child);

        size_t base = scope_.size();
        bool saw_error = false;
        for (Node* c = s->args; c; c = c->next) {
          if (c->kind != Kind::CatchClause) continue;
          size_t mark = scope_.size();

          // E4002：`catch (Error e)` 捕获除 BudgetError 外全部，所以排在它
          // 之后的任何 catch 都不可达。语言只有一层继承（6 个错误类型并列，
          // "兄弟互不匹配"），因此这是**唯一**可能写反的父子对 —— 也正因如此
          // 它值得硬错：写反会让专用分支永远进不去，错误静默降级到通用分支。
          if (saw_error) {
            const char* tname = c->text ? c->text : "?";
            err("E4002", internf("this catch (%s) is unreachable: `catch (Error e)` above already "
                                 "matches everything", tname),
                c->loc,
                "`Error` is the root of all catchable errors, so a catch placed after it can never "
                "run. The narrow branch silently degrades to the wide one — no error is reported, "
                "the wrong handler just quietly takes over.",
                "swap the two: put the specific type first and `catch (Error e)` last");
          }
          if (c->text && strcmp(c->text, "Error") == 0) saw_error = true;

          // catch 绑定的变量进作用域（类型暂为 unknown：错误类型的字段
          // 访问是 T11 ③ 的事，这里不猜层级）。
          if (c->child && c->child->text) {
            VarSym v;
            v.name = c->child->text;
            v.t = unknown();
            v.loc = c->child->loc;
            scope_.push_back(v);
          }
          if (c->else_branch) block_(c->else_branch);
          // 每个 catch 的绑定只活在自己的 body 里。
          scope_.resize(mark);
        }
        scope_.resize(base);
        break;
      }

      // parallel 作为语句出现时也要走一遍：否则块内的 let 完全不进作用域，
      // 块内的类型错误也不会被发现 —— "看起来没问题，其实一个字都没查"。
      case Kind::ParallelExpr: parallel_(s); break;

      case Kind::MatchStmt: {
        Ty* subj_t = type_of_(s->child);
        // E4018：match 必须有 default 分支。
        //
        // 判定依据从"text 为空"改成 **is_nullable**：整数臂的 text 也是空的，
        // 用 text 判会把每个整数臂都当成 default —— 于是"没有 default"的检查
        // 永远通过，而真正的 default 又可能被算错。
        bool has_default = false;
        for (Node* arm = s->args; arm; arm = arm->next) {
          if (arm->kind != Kind::MatchArm) continue;
          if (arm->is_nullable) {
            has_default = true;
          } else if (subj_t && subj_t->k != Ty::Unknown && subj_t->k != Ty::Json) {
            // 臂与主语必须同类型：拿 int 臂去匹配 string 只会永远不命中，
            // 而那种"静默不匹配"要到运行期才表现为"走了 default"。
            const bool int_arm = (arm->text == nullptr);
            if (int_arm && subj_t->k != Ty::Int) {
              err("E4019", "integer case on a non-int value", arm->loc,
                  "Arms are compared with the matched value using strict equality, so a case "
                  "whose type differs from the value can never match.",
                  "use a string case, or match an int field instead");
            }
            if (!int_arm && subj_t->k != Ty::Str) {
              err("E4019", "string case on a non-string value", arm->loc,
                  "Arms are compared with the matched value using strict equality, so a case "
                  "whose type differs from the value can never match.",
                  "use an integer case, or match a string field instead");
            }
          }
          block_(arm->args);
        }
        if (!has_default) {
          err("E4018", "`match` has no `default` branch", s->loc,
              "The matched value is a string, so 'all cases covered' cannot be proven statically. "
              "Without a default, an unexpected value has no branch to run — the workflow would "
              "fall through with nothing assigned, which is the kind of gap that only shows up "
              "on the one input nobody tried.",
              "add `default: { ... }` to handle whatever is left");
        }
        break;
      }

      default: break;
    }
  }

  bool same_type_(Ty* a, Ty* b) {
    if (!a || !b) return true;
    if (a->k != b->k) return false;
    if (a->k == Ty::StructK) return a->struct_idx == b->struct_idx;
    return true;
  }

  void check_workflow_(Node* wf) {
    scope_.clear();
    // 声明的返回类型存在 WorkflowDecl.type_args（parser 在 parse_workflow 里填的）。
    // 没有 `-> T` 的 workflow 允许由体推断，此时 declared_ret_ 为 null，E4021 不触发。
    declared_ret_ = wf->type_args ? from_type_(wf->type_args) : nullptr;

    for (Node* p = wf->args; p; p = p->next) {
      if (p->kind != Kind::Param || !p->text) continue;
      VarSym v;
      v.name = p->text;
      v.t = from_type_(p->child);
      v.loc = p->loc;
      scope_.push_back(v);
    }

    block_(wf->child);
    scope_.clear();
  }
};

}  // namespace

// 公开入口：在 parse 之后调用，把类型诊断并进同一个 DiagBag。
void check_types(Node* root, const char* filename, Arena& arena, DiagBag& diags) {
  Checker c(arena, diags, filename);
  c.run(root);
}

// check_types_with_externals 额外接受一张"别的模块声明了哪些 struct"的表。
// 单文件检查器看不到别的 .h —— 不喂这张表，每个正常的多文件工程都会被判
// "访问 unknown 的字段"（support_bot 正是这么红的）。
void check_types_with_externals(Node* root, const char* filename, Arena& arena,
                                DiagBag& diags, const char* ext, size_t ext_len) {
  Checker c(arena, diags, filename);
  if (ext && ext_len) c.load_externals_(ext, ext_len);
  c.run(root);
}

}  // namespace hm
