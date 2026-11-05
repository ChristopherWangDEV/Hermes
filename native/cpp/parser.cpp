// parser.cpp —— Hermes 递归下降语法分析器（T09）。
//
// 硬约束：零 IO、零全局可变状态、无异常、无 RTTI。输入是词法器产出的 token 序列，
// 输出是 arena 分配的 AST（Node 树）+ 诊断列表。出错时仍尽力恢复（--keep-going），
// 继续解析以报出多个错误。
//
// 关键判定：
//   · <T> 消歧（LANGUAGE §3.4 R-TY-1..4，LL(3)，无回溯需）：只有左侧是内建被调者
//     (model./tool./json.) 且 `<` `>` `(` 三段齐备时才是类型实参；否则 `<` 是关系运算 LT。
//   · 上下文关键字 entry / env 由词法器按 IDENT 出，这里按位置判定。
//   · #link 只允许在 .hermes（header 里出现 → E3017）。

#include "ast.h"
#include "arena.h"

#include <cstddef>
#include <cstdio>
#include <cstring>
#include <vector>

namespace hm {

const char* kind_name(Kind k) {
  switch (k) {
    case Kind::Module: return "Module";
    case Kind::Include: return "Include";
    case Kind::Link: return "Link";
    case Kind::StructDecl: return "StructDecl";
    case Kind::FunctionDecl: return "FunctionDecl";
    case Kind::SecurityDecl: return "SecurityDecl";
    case Kind::SecurityRule: return "SecurityRule";
    case Kind::Field: return "Field";
    case Kind::Type: return "Type";
    case Kind::BackendSig: return "BackendSig";
    case Kind::BackendDef: return "BackendDef";
    case Kind::ToolDecl: return "ToolDecl";
    case Kind::EnvExpr: return "EnvExpr";
    case Kind::WorkflowSig: return "WorkflowSig";
    case Kind::WorkflowDecl: return "WorkflowDecl";
    case Kind::Param: return "Param";
    case Kind::Block: return "Block";
    case Kind::LetStmt: return "LetStmt";
    case Kind::Pattern: return "Pattern";
    case Kind::MatchStmt: return "MatchStmt";
    case Kind::MatchArm: return "MatchArm";
    case Kind::IfStmt: return "IfStmt";
    case Kind::ForStmt: return "ForStmt";
    case Kind::WhileStmt: return "WhileStmt";
    case Kind::BreakStmt: return "BreakStmt";
    case Kind::AssignStmt: return "AssignStmt";
    case Kind::UpdateStmt: return "UpdateStmt";
    case Kind::ContinueStmt: return "ContinueStmt";
    case Kind::RetryStmt: return "RetryStmt";
    case Kind::TryStmt: return "TryStmt";
    case Kind::CatchClause: return "CatchClause";
    case Kind::ReturnStmt: return "ReturnStmt";
    case Kind::ExprStmt: return "ExprStmt";
    case Kind::CallExpr: return "CallExpr";
    case Kind::PathExpr: return "PathExpr";
    case Kind::LitExpr: return "LitExpr";
    case Kind::IdentExpr: return "IdentExpr";
    case Kind::BinaryExpr: return "BinaryExpr";
    case Kind::UnaryExpr: return "UnaryExpr";
    case Kind::IndexExpr: return "IndexExpr";
    case Kind::ParallelExpr: return "ParallelExpr";
    case Kind::ParallelReturn: return "ParallelReturn";
    case Kind::ArrayLit: return "ArrayLit";
    case Kind::StringPartNode: return "StringPartNode";
    case Kind::Annotation: return "Annotation";
  }
  return "?";
}

namespace {

// E2018 的定稿文案（LANGUAGE §3.1 注 8，逐字）：密钥没有安全默认值。
// 它属 E20xx 语法族，由语法器抛出（与 E3018 报"env 第二参数"同一模式）——
// 这里不是类型错误，是"这种写法本身就不该存在"。
const char* const kAuthNoDefault =
    "密钥没有安全默认值：给了默认就等于允许没配也能跑，而那只会在运行期变成一个 401";

struct Parser {
  const std::vector<Token>& t;
  size_t i = 0;
  const char* filename;
  bool is_header;
  Arena& arena;
  DiagBag& diags;

  Parser(const std::vector<Token>& toks, const char* fn, bool hdr, Arena& a, DiagBag& d)
      : t(toks), filename(fn), is_header(hdr), arena(a), diags(d) {}

  const Token& cur() const { return (i < t.size()) ? t[i] : t.back(); }
  const Token& peek(int n) const {
    size_t j = i + static_cast<size_t>(n);
    return (j < t.size()) ? t[j] : t.back();
  }
  void adv() { if (i < t.size()) i++; }
  bool eof() const { return cur().kind == Tok::EOF_; }

  void err(const char* code, const char* msg, Loc loc, const char* why, const char* fix) {
    Diag d;
    memset(&d, 0, sizeof(d));
    d.severity = Severity::Error;
    d.code = code;
    d.message = msg;
    d.loc = loc;
    d.module = filename;
    d.why = why;
    d.fix = fix;
    diag_add(diags, arena, d);
  }

  Node* new_node(Kind k, Loc loc) {
    Node* n = make_node<Node>(arena);
    if (n == nullptr) return nullptr;
    n->kind = k;
    n->loc = loc;
    return n;
  }

  const char* lit_str(const char* s) {
    return arena.intern(s, s ? strlen(s) : 0);
  }

  // ---- 字符串 token 的唯一读法 --------------------------------------------
  //
  // 约定（不是"尚未实现"）：`Tok::STRING` 的 `Token::text` **恒为 nullptr**。
  // 带插值的字符串不存在单一标量内容（`"a{x}b"` 拼成什么都是错的），所以内容
  // 只能住在 `parts`；`text` 留空恰好把这条事实建模清楚了。给"无插值的字符串"
  // 补一份 `text` 等于给 `parts` 加一份缓存，而缓存要维护同步。
  //
  // 因此凡是要读字符串内容的地方，一律走 `str_const_text()`：
  //   · 无片段（空串 ""）      → 指向空串（**非空指针**，值为 ""）
  //   · 单个字面片段           → parts[0].text
  //   · 含插值 / 多片段        → nullptr（不是常量，任何当常量用的地方都该失败）
  // 空串与"非常量"必须是两个不同的值，否则下游无法区分"用户写了空串"与
  // "这里根本不可能是常量"。
  const char* str_const_text(const Token& tk) {
    if (tk.kind != Tok::STRING) return nullptr;
    if (tk.parts == nullptr || tk.n_parts == 0) return lit_str("");
    if (tk.n_parts != 1 || tk.parts[0].is_path) return nullptr;
    return tk.parts[0].text;
  }

  // "此处只接受常量字符串"的位置（env 两个实参、auth 的密钥名）的取值 + 断言。
  // 取不到就立刻报诊断并返回 nullptr —— 绝不让空指针一路传进 AST/IR（那会生成
  // 一份合法但全错的产物，且不报错）。
  const char* const_str_or_diag() {
    const char* v = str_const_text(cur());
    if (v != nullptr) return v;
    err("E3018", "env 的实参必须是常量字符串：插值不是常量", cur().loc,
        "env() 要能被静态审计：不解析任何东西就该读出地址与变量名，"
        "含插值的字符串做不到这一点。",
        "把插值换成字面量，或把插值移到调用参数里。");
    return nullptr;
  }

  void append(Node*& head, Node*& tail, Node* n) {
    if (!n) return;
    if (!head) { head = n; tail = n; }
    else { tail->next = n; tail = n; }
  }

  // 把指针数组落盘成 arena 数组（subs）。
  Node* to_subs(const std::vector<Node*>& v, int& n_out) {
    n_out = static_cast<int>(v.size());
    if (v.empty()) return nullptr;
    Node* arr = static_cast<Node*>(arena.alloc(sizeof(Node) * v.size()));
    if (arr == nullptr) { n_out = 0; return nullptr; }
    for (size_t k = 0; k < v.size(); ++k) arr[k] = *v[k];
    return arr;
  }

  // ---- 同步（错误恢复）---------------------------------------------------
  void sync_stmt() {
    while (!eof() && cur().kind != Tok::SEMI && cur().kind != Tok::RBRACE) adv();
    if (cur().kind == Tok::SEMI) adv();
  }

  // 跳过一个平衡的 `{ ... }`。进入时 cur() 必须是 '{'；返回时 cur() 在匹配的 '}' 之后。
  //
  // 关键：**先 adv 再判断**。旧实现在循环体里分情况 adv，一旦 cur() 落到
  // RBRACE 且 depth 已是 0，就会「depth-- → adv → 判断 → 不 break → 再 adv」，
  // 看着有推进，实际在 `i` 已到末尾时 adv() 是空操作（adv 只在 i < size 时前进），
  // 于是零推进死循环。这个写法每一次循环都必然前进一格，不依赖任何外部条件。
  void skip_balanced_block() {
    if (cur().kind != Tok::LBRACE) return;
    int depth = 0;
    while (!eof()) {
      Tok k = cur().kind;
      adv();
      if (k == Tok::LBRACE) {
        depth++;
      } else if (k == Tok::RBRACE) {
        if (--depth == 0) return;
      }
    }
  }

  void sync_toplevel() {
    while (!eof()) {
      Tok k = cur().kind;
      if (k == Tok::SEMI || k == Tok::RBRACE ||
          k == Tok::INCLUDE || k == Tok::LINK ||
          k == Tok::KW_EXPORT || k == Tok::KW_STRUCT ||
          k == Tok::KW_BACKEND || k == Tok::KW_WORKFLOW) break;
      adv();
    }
    // 顶层出现孤立的 '}' 时**必须消费掉它**。
    // 否则 sync_toplevel 会「看到 RBRACE 就 break、却不动游标」，
    // 而 parse_module 的下一次 parse_toplevel 又会在这个 '}' 上报一条错、
    // 再调一次 sync_toplevel —— 零推进死循环，每圈刷一条诊断。
    // （2026-11-03 实测：`.h` 里一个带体 workflow 就能让诊断 JSON 超过 64 MiB。）
    if (cur().kind == Tok::SEMI || cur().kind == Tok::RBRACE) adv();
  }

  bool expect(Tok k, const char* code, const char* msg) {
    if (cur().kind == k) { adv(); return true; }
    err(code, msg, cur().loc,
        "语法要求此处出现对应符号。", "检查是否漏写或拼写错误。");
    return false;
  }

  // ---- 类型 ----------------------------------------------------------------
  Node* parse_type() {
    Tok k = cur().kind;
    if (k != Tok::TY_STRING && k != Tok::TY_INT && k != Tok::TY_FLOAT &&
        k != Tok::TY_BOOL && k != Tok::TY_COST && k != Tok::NS_JSON &&
        k != Tok::IDENT) return nullptr;
    Loc loc = cur().loc;
    Node* n = new_node(Kind::Type, loc);
    if (!n) return nullptr;
    n->text = cur().text;
    adv();
    // 数组维数
    while (cur().kind == Tok::LBRACKET) {
      Loc b = cur().loc;
      adv();
      if (!expect(Tok::RBRACKET, "E1001", "数组类型缺少 ']'")) { n->i64 = 0; }
      else n->i64 += 1;
      (void)b;
    }
    return n;
  }

  // ---- 顶层 ----------------------------------------------------------------
  Node* parse_module() {
    if (arena.oom()) return nullptr;
    Node* mod = new_node(Kind::Module, {1, 1, 1, 1});
    if (!mod) return nullptr;
    Node* head = nullptr, *tail = nullptr;
    while (!eof()) {
      size_t before = i;
      Node* d = parse_toplevel();
      if (d) append(head, tail, d);
      else sync_toplevel();
      if (arena.oom()) break;
      // 零推进兜底：这一轮既没产出声明、也没能移动游标。
      // 结构上不该发生（parse_toplevel 失败后 sync_toplevel 必然消费至少一格），
      // 但"每个恢复函数都会推进"这个假设太脆弱 —— 少一处 adv 就是死循环 +
      // 诊断爆炸（2026-11-03 真的踩到过）。这里强制推进一格并如实报一条内部诊断，
      // 而不是静默 advance：静默会让真 bug 永远藏起来。
      if (i == before) {
        err("E1001", "解析器无法推进（内部不一致）", cur().loc,
            "这是一个解析器缺陷：某个恢复路径没有消费任何 token。",
            "请报告该源码，它会让编译器陷入零推进循环。");
        adv();
      }
    }
    mod->child = head;
    return mod;
  }

  Node* parse_toplevel() {
    Tok k = cur().kind;
    if (k == Tok::INCLUDE) return parse_include();
    if (k == Tok::LINK) return parse_link();
    bool exp = false;
    if (k == Tok::KW_EXPORT) { exp = true; adv(); }
    switch (cur().kind) {
      case Tok::KW_STRUCT: return parse_struct(exp);
      // C++ 风格函数：`string f(int x) { ... }`，以**类型**开头。
      case Tok::TY_INT:
      case Tok::TY_STRING:
      case Tok::TY_BOOL:
      case Tok::TY_FLOAT:
      case Tok::TY_JSON:
      case Tok::TY_COST:
        return parse_function(exp);
      case Tok::KW_SECURITY: return parse_security(exp);
      case Tok::KW_BACKEND: return parse_backend(exp);
      // `tool` 是保留命名空间 token（NS_TOOL）。在 `export` 之后它会出现在
      // 一个**语法上只允许声明关键字**的位置，所以判定无歧义 —— 与 `entry` / `env`
      // 那两个上下文关键字同一个道理。
      case Tok::NS_TOOL: return parse_tool_decl(exp);
      case Tok::KW_WORKFLOW: return parse_workflow(exp);
      default: break;
    }
    err("E1001", "期望声明（struct/backend/workflow 或 #include/#link）", cur().loc,
        "文件顶层只能是声明或指令。", "检查顶层语法。");
    return nullptr;
  }

  Node* parse_include() {
    const Token& tk = cur();
    Node* n = new_node(Kind::Include, tk.loc);
    if (!n) return nullptr;
    n->text = tk.text;
    n->i64 = tk.sub;  // 0=angle 1=quote
    adv();
    return n;
  }

  Node* parse_link() {
    const Token& tk = cur();
    Node* n = new_node(Kind::Link, tk.loc);
    if (!n) return nullptr;
    n->text = tk.text;
    n->i64 = tk.sub;
    if (is_header) {
      err("E3017", "#link 只允许出现在 .hermes 里", tk.loc,
          "#include 给声明，#link 给实现；写在 .h 会让每个 include 者继承未选的链接依赖。",
          "把 #link 移到 .hermes 实现文件。");
    }
    adv();
    return n;
  }

  Node* parse_security(bool exp) {
    const Loc loc = cur().loc;
    adv();
    if (cur().kind != Tok::IDENT) { err("E1001", "security needs a name", cur().loc, "A policy is referenced by name from `with`.", "write: export security Guard { ... }"); return nullptr; }
    const char* name = cur().text;
    adv();
    if (!expect(Tok::LBRACE, "E1001", "security 缺少 '{'")) return nullptr;
    Node* head = nullptr; Node* tail = nullptr;
    while (cur().kind != Tok::RBRACE && !eof()) {
      const bool isAllow = cur().kind == Tok::KW_ALLOW;
      if (!isAllow && cur().kind != Tok::KW_DENY) {
        err("E1001", "only allow / deny are accepted in security", cur().loc, "A policy is a set of allow/deny rules.", "use allow or deny"); return nullptr;
      }
      const Loc rl = cur().loc;
      adv();
      std::string cap;
      while (true) {
        if (cur().kind != Tok::IDENT && cur().kind != Tok::NS_TOOL && cur().kind != Tok::NS_MODEL) break;
        cap += cur().text ? cur().text : "";
        adv();
        if (cur().kind == Tok::DOT) { cap += "."; adv(); continue; }
        break;
      }
      if (cap.empty()) { err("E1001", "allow/deny needs a capability name", cur().loc, "The capability names the kind of operation being constrained.", "write: allow tool.call = [..]"); return nullptr; }
      // 能力名必须是**语言里真实存在**的操作。
      //
      // 为什么必须校验：写一条 `deny file.write` 而语言根本没有文件写，
      // 用户会以为文件被保护了 —— 那是"看起来有保护"胜过"知道没保护"的陷阱。
      // 不支持的策略项必须立刻报错，而不是静静收下。
      if (cap != "tool.call" && cap != "network.outbound" && cap != "model.call") {
        err("E1001", "unknown security capability", rl,
            "The capability must name an operation the language actually has; an unknown name is a policy that silently does nothing.",
            ("supported: tool.call / network.outbound / model.call (got " + cap + ")").c_str());
        return nullptr;
      }
      if (!expect(Tok::EQ, "E1001", "allow/deny 缺少 '='")) return nullptr;
      Node* list = nullptr; Node* h2 = nullptr; Node* t2 = nullptr;
      if (cur().kind == Tok::LBRACKET) {
        adv();
        while (cur().kind != Tok::RBRACKET && !eof()) {
          if (cur().kind != Tok::STRING) { err("E1001", "policy values must be strings", cur().loc, "Tool names, hosts, and paths are strings.", "write: [\"search_web\"]"); return nullptr; }
          Node* lit = new_node(Kind::LitExpr, cur().loc);
          if (lit) { lit->text = arena.intern(str_const_text(cur()), strlen(str_const_text(cur()))); }
          append(h2, t2, lit);
          adv();
          if (cur().kind == Tok::COMMA) { adv(); continue; }
          break;
        }
        if (!expect(Tok::RBRACKET, "E1001", "策略列表缺少 ']'")) return nullptr;
        list = h2;
      } else { err("E1001", "allow/deny needs a list", cur().loc, "A rule is a set of concrete values.", "write: = [\"a\", \"b\"]"); return nullptr; }
      if (!expect(Tok::SEMI, "E1001", "policy rule is missing ';'")) return nullptr;
      Node* rule = new_node(Kind::SecurityRule, rl);
      if (rule) { rule->text = arena.intern(cap.c_str(), cap.size()); rule->is_export = isAllow; rule->child = list; }
      append(head, tail, rule);
    }
    if (!expect(Tok::RBRACE, "E1001", "security 缺少 '}'")) return nullptr;
    Node* n = new_node(Kind::SecurityDecl, loc);
    if (!n) return nullptr;
    n->text = name; n->is_export = exp; n->child = head;
    return n;
  }

  // C++ 风格函数定义：`string greet(string name) { ... }`
  //
  // 定义即声明（两遍扫描后再解析调用），不做前向声明语法。
  Node* parse_function(bool exp) {
    const Loc loc = cur().loc;
    Node* rt = parse_type();
    if (!rt) return nullptr;
    if (cur().kind != Tok::IDENT) {
      err("E1001", "function needs a name", cur().loc, "", "");
      return nullptr;
    }
    const char* name = cur().text;
    adv();
    if (!expect(Tok::LPAREN, "E1001", "function needs '('")) return nullptr;
    // 参数是 C++ 顺序：`Type name`（与 workflow 的 `name: Type` 不同）。
    Node* params = nullptr; Node* ptail = nullptr;
    while (cur().kind != Tok::RPAREN && !eof()) {
      Node* ty = parse_type();
      if (!ty) return nullptr;
      if (cur().kind != Tok::IDENT) {
        err("E1001", "parameter needs a name", cur().loc,
            "Function parameters are written `Type name`, as in C++.", "write: string name");
        return nullptr;
      }
      Node* prm = new_node(Kind::Param, cur().loc);
      if (!prm) return nullptr;
      prm->text = cur().text;
      prm->type_args = ty;
      adv();
      append(params, ptail, prm);
      if (cur().kind == Tok::COMMA) { adv(); continue; }
      break;
    }
    if (!expect(Tok::RPAREN, "E1001", "function needs ')'")) return nullptr;
    Node* body = parse_block();
    if (!body) return nullptr;
    Node* n = new_node(Kind::FunctionDecl, loc);
    if (!n) return nullptr;
    n->text = name;
    n->is_export = exp;
    n->type_args = rt;
    n->child = params;
    n->body = body;
    return n;
  }

  Node* parse_struct(bool exp) {
    Loc loc = cur().loc;
    adv();  // struct
    const char* name = nullptr;
    if (cur().kind == Tok::IDENT) { name = cur().text; adv(); }
    else { err("E1001", "struct 缺少名字", cur().loc, nullptr, nullptr); }
    if (!expect(Tok::LBRACE, "E1001", "struct 缺少 '{'")) { sync_stmt(); return nullptr; }
    Node* head = nullptr, *tail = nullptr;
    while (cur().kind != Tok::RBRACE && !eof()) {
      Node* f = parse_field();
      if (f) append(head, tail, f);
      else { sync_stmt(); }
    }
    if (!expect(Tok::RBRACE, "E1001", "struct 缺少 '}'")) { return nullptr; }
    Node* n = new_node(Kind::StructDecl, loc);
    if (!n) return nullptr;
    n->text = name;
    n->is_export = exp;
    n->child = head;
    return n;
  }

  Node* parse_field() {
    Loc loc = cur().loc;
    Node* ty = parse_type();
    if (!ty) {
      err("E1001", "字段缺少类型", cur().loc, nullptr, nullptr);
      sync_stmt();
      return nullptr;
    }
    const char* fname = nullptr;
    if (cur().kind == Tok::IDENT) { fname = cur().text; adv(); }
    else { err("E1001", "字段缺少名字", cur().loc, nullptr, nullptr); sync_stmt(); return nullptr; }
    Node* n = new_node(Kind::Field, loc);
    if (!n) return nullptr;
    n->text = fname;
    n->child = ty;  // 类型
    if (cur().kind == Tok::QUESTION) { n->is_nullable = true; adv(); }
    // 注解：@ ident [ ( Lit {, Lit} ) ]
    Node* ann_head = nullptr, *ann_tail = nullptr;
    while (cur().kind == Tok::AT) {
      Loc al = cur().loc; adv();
      const char* aname = (cur().kind == Tok::IDENT) ? cur().text : "";
      if (cur().kind == Tok::IDENT) adv();
      Node* an = new_node(Kind::Annotation, al);
      if (an) { an->text = aname; }
      if (cur().kind == Tok::LPAREN) {
        adv();
        // 跳过字面量列表（不展开解析细节）
        int depth = 1;
        while (!eof() && depth > 0) {
          if (cur().kind == Tok::LPAREN) depth++;
          else if (cur().kind == Tok::RPAREN) { depth--; if (depth == 0) { adv(); break; } }
          adv();
        }
      }
      if (an) append(ann_head, ann_tail, an);
    }
    if (ann_head) n->args = ann_head;  // 字段注解挂在 args
    if (!expect(Tok::SEMI, "E1001", "字段缺少 ';'")) { sync_stmt(); }
    return n;
  }

  // ---- backend 的 auth 子句（LANGUAGE §3.1 注 8）----------------------------
  //
  //   BackendDef = "backend" Ident "=" ( StringLit | EnvExpr ) [ "auth" EnvExpr ] ";" .
  //
  // `auth` 是**上下文关键字**（不进 16 关键字表）：地址部分读完后，按文法合法的
  // 下一个 token 只有 ';'，所以此处见到 IDENT("auth") 就只可能是子句开始。
  // 判定由位置决定而非词法决定，因此 `let auth = 1;` 里 `auth` 仍是普通标识符。
  //
  // 右值**必须是 env(名) 单实参形式**：密钥没有安全默认值，给了默认就等于允许
  // 没配也能跑，而那只会在运行期变成一个 401（E2018）。
  //
  // AST 复用现有槽位：返回的 EnvExpr 里 child=变量名、args 恒为 nullptr
  // （auth 位的 env 没有默认值，这正是本子句的全部要点）。
  Node* parse_auth_clause() {
    adv();  // auth
    if (!(cur().kind == Tok::IDENT && cur().text && strcmp(cur().text, "env") == 0)) {
      err("E2018",
          "auth 子句的右值必须是 env(\"名\")：密钥只从环境来，不能省也不能写成别的表达式",
          cur().loc,
          "auth 回答的是「密钥从哪个环境变量来」；没有这个问题，backend 就能在"
          "没配密钥的情况下跑起来，到运行期才变成一个 401。",
          "写成 auth env(\"HOMONOIA_KEY\")。");
      // 恢复：右值不是 env(...) 时把这一句剩下的 token 一并吃掉，否则紧接着的
      // ';' 检查会再报一条"backend 定义缺少 ';'"，把一条真错变成三条假错。
      while (!eof() && cur().kind != Tok::SEMI && cur().kind != Tok::RBRACE) adv();
      return nullptr;
    }
    Loc env_loc = cur().loc;
    adv();  // env
    Node* env = new_node(Kind::EnvExpr, env_loc);
    if (env == nullptr) return nullptr;
    if (!expect(Tok::LPAREN, "E1001", "env 缺少 '('")) return env;
    if (cur().kind == Tok::STRING) {
      env->child = new_node(Kind::LitExpr, cur().loc);
      // 密钥名必须是常量：key 名带插值就无法静态审计"密钥从哪来"。
      if (env->child) { env->child->text = const_str_or_diag(); }
      adv();
    } else {
      err("E2018",
          "auth 的 env 实参必须是环境变量名的字符串字面量（可静态审计）",
          cur().loc,
          "不解析任何东西就要能读出这个 backend 的密钥来自哪个变量。",
          "写成 auth env(\"HOMONOIA_KEY\")。");
    }
    // 第二实参 = 默认值 → E2018。逗号与第二个字符串两种写法都要拦住：
    // 无论哪种，语义都是"密钥有默认值"，而那正是 §3.1 注 8 禁止的。
    if (cur().kind == Tok::COMMA || cur().kind == Tok::STRING) {
      err("E2018", kAuthNoDefault, cur().loc,
          "密钥没有安全默认值：给了默认就等于允许没配也能跑，而那只会在运行期变成一个 401。",
          "删掉第二实参，只留 auth env(\"名\")。");
      // 恢复：吃掉多余实参直到 ')' 或 ';'，保证后续仍能报出别的错（--keep-going）。
      while (!eof() && cur().kind != Tok::RPAREN && cur().kind != Tok::SEMI) adv();
    }
    if (cur().kind == Tok::RPAREN) adv();
    else err("E1001", "auth 的 env 缺少 ')'", cur().loc,
             "env(...) 必须闭合括号。", "补上 ')'。");
    return env;
  }

  // parse_tool_decl 解析 `tool <name> { <field> = "<value>"; … }`。
  //
  // 字段固定三个：description / url / method。url 允许插值（`{id}`），
  // 那些片段在运行期由模型给的参数填 —— 因为参数不在 IR 节点图里。
  Node* parse_tool_decl(bool exp) {
    Loc loc = cur().loc;
    adv();  // tool
    if (cur().kind != Tok::IDENT) {
      err("E3009", "tool 声明缺少名字", cur().loc, nullptr, nullptr);
      return nullptr;
    }
    Node* n = new_node(Kind::ToolDecl, loc);
    if (!n) return nullptr;
    // 名字是 IDENT，不是 STRING：`str_const_text` 只认字符串 token，
    // 用错会拿到 nullptr，于是整条声明变成"字段名为空"。
    n->text = cur().text;
    n->is_export = exp;
    adv();
    if (!expect(Tok::LBRACE, "E1001", "tool 声明缺少 '{'")) return nullptr;
    Node* head = nullptr;
    Node* tail = nullptr;
    while (cur().kind != Tok::RBRACE && cur().kind != Tok::EOF_) {
      if (cur().kind != Tok::IDENT) {
        err("E3009", "tool 字段必须是 name = \"value\"; 的形式", cur().loc, nullptr, nullptr);
        return nullptr;
      }
      Loc fl = cur().loc;
      Node* f = new_node(Kind::Param, fl);
      if (f) f->text = cur().text;  // 同上：字段名是 IDENT
      adv();
      if (!expect(Tok::EQ, "E1001", "tool 字段缺少 '='")) return nullptr;
      if (cur().kind != Tok::STRING) {
        err("E3009", "tool 字段的值必须是字符串字面量", cur().loc, nullptr, nullptr);
        return nullptr;
      }
      // 字符串 token 的内容只住在 parts 里（见 str_const_text 的注释），
      // 所以这里跟 parse_literal 一样，把片段做成 StringPartNode 链挂到 subs。
      Node* v = new_node(Kind::LitExpr, cur().loc);
      if (v) {
        v->text = lit_str("string");
        if (cur().parts && cur().n_parts > 0) {
          std::vector<Node*> parts;
          for (int p = 0; p < cur().n_parts; ++p) {
            Node* sp = new_node(Kind::StringPartNode, cur().loc);
            if (sp) { sp->is_path = cur().parts[p].is_path; sp->text = cur().parts[p].text; }
            parts.push_back(sp);
          }
          int ns = 0;
          v->subs = to_subs(parts, ns);
          v->n_subs = ns;
        }
      }
      adv();
      if (f) f->child = v;
      append(head, tail, f);
      if (cur().kind == Tok::SEMI) adv();
    }
    expect(Tok::RBRACE, "E1001", "tool 声明缺少 '}'");
    if (n) n->child = head;
    return n;
  }

  Node* parse_backend(bool exp) {
    Loc loc = cur().loc;
    adv();  // backend
    const char* name = nullptr;
    if (cur().kind == Tok::IDENT) { name = cur().text; adv(); }
    else { err("E1001", "backend 缺少名字", cur().loc, nullptr, nullptr); }
    // 可选的 `: kind`（D-032）。kind 是**协议**，不是地址：
    // 地址说"发到哪"，kind 说"用什么协议说话"。二者必须分开写，
    // 因为 IR 要求 kind 是五选一的枚举，而它无法从地址推出来。
    const char* bkind = nullptr;
    if (cur().kind == Tok::COLON) {
      adv();
      if (cur().kind == Tok::IDENT) { bkind = cur().text; adv(); }
      else { err("E1001", "backend 的 : 之后缺少 kind", cur().loc, nullptr, nullptr); }
    }
    if (is_header) {
      if (cur().kind == Tok::EQ) {
        err("E3020", "header 里不能有 backend 定义（地址）", cur().loc,
            "地址走环境注入，声明应只在 .h，定义只在 .hermes。",
            "把 '= ...' 移到 .hermes。");
        // 仍尝试吃掉定义以恢复
        adv();
        if (cur().kind == Tok::STRING) adv();
        else if (cur().kind == Tok::IDENT && cur().text && strcmp(cur().text, "env") == 0) {
          adv();
          // env(...)
          if (cur().kind == Tok::LPAREN) adv();
          if (cur().kind == Tok::STRING) adv();
          if (cur().kind == Tok::COMMA) adv();
          if (cur().kind == Tok::STRING) adv();
          if (cur().kind == Tok::RPAREN) adv();
        }
        expect(Tok::SEMI, "E1001", "缺少 ';'");
        Node* n = new_node(Kind::BackendDef, loc);
        if (n) {
          n->text = name;
          n->is_export = exp;
          if (bkind) {
            Node* k = new_node(Kind::IdentExpr, loc);
            if (k) { k->text = bkind; n->type_args = k; }
          }
        }
        return n;
      }
      expect(Tok::SEMI, "E1001", "backend 声明缺少 ';'");
      Node* n = new_node(Kind::BackendSig, loc);
      if (!n) return nullptr;
      n->text = name;
      n->is_export = exp;
      if (bkind) {
        Node* k = new_node(Kind::IdentExpr, loc);
        if (k) { k->text = bkind; n->type_args = k; }
      }
      return n;
    }
    // impl：必须有 '='
    if (!expect(Tok::EQ, "E1001", "backend 定义缺少 '='")) { return nullptr; }
    Node* val = nullptr;
    if (cur().kind == Tok::STRING) {
      val = new_node(Kind::LitExpr, cur().loc);
      // 字面量地址：内容走 str_const_text（Token::text 对 STRING 恒空，见该函数）。
      if (val) { val->text = str_const_text(cur()); }
      adv();
    } else if (cur().kind == Tok::IDENT && cur().text && strcmp(cur().text, "env") == 0) {
      Loc el = cur().loc; adv();
      Node* env = new_node(Kind::EnvExpr, el);
      if (env) {
        if (expect(Tok::LPAREN, "E1001", "env 缺少 '('")) {
          if (cur().kind == Tok::STRING) { env->child = new_node(Kind::LitExpr, cur().loc);
            if (env->child) {
              env->child->text = const_str_or_diag();
            }
            adv(); }
          else err("E3018", "env 第一参数必须是字符串字面量", cur().loc,
                   "env() 的两个参数都必须是字符串字面量（可静态审计）。", "写成 env(\"URL\", \"默认\")。");
          if (cur().kind == Tok::COMMA) adv();
          if (cur().kind == Tok::STRING) { env->args = new_node(Kind::LitExpr, cur().loc);
            if (env->args) {
              env->args->text = const_str_or_diag();
            }
            adv(); }
          else err("E3018", "env 第二参数必须是字符串字面量", cur().loc,
                   "第二参数是缺省地址，必须是字符串字面量。", "写成 env(\"URL\", \"默认\")。");
          if (cur().kind == Tok::RPAREN) adv();
          else err("E1001", "env 缺少 ')'", cur().loc, nullptr, nullptr);
        }
        val = env;
      }
    } else {
      err("E1001", "backend 定义值非法（期望字符串或 env(...)）", cur().loc, nullptr, nullptr);
    }
    // auth 子句：地址之后合法的下一个 token 只有 ';'，所以这里见到 IDENT("auth")
    // 就一定是子句开始（上下文关键字，不进 16 关键字表；`let auth = 1;` 不受影响）。
    // 一个 backend 定义最多一个子句，重复 → 同一个 E2018。
    Node* auth = nullptr;
    bool seen_auth = false;
    while (cur().kind == Tok::IDENT && cur().text && strcmp(cur().text, "auth") == 0) {
      if (seen_auth) {
        err("E2018",
            "backend 定义最多一个 auth 子句：重复的 auth 不生效，只会让人以为有两个密钥来源",
            cur().loc,
            "密钥来源只有一个。写第二个 auth 不会让它在没配时跑起来，"
            "只会让读代码的人以为密钥有两个来源。",
            "删掉重复的 auth 子句，只保留第一个 auth env(\"名\")。");
        (void)parse_auth_clause();  // 吃掉重复子句，保证后面仍能报别的错
      } else {
        seen_auth = true;
        auth = parse_auth_clause();
      }
    }
    expect(Tok::SEMI, "E1001", "backend 定义缺少 ';'");
    Node* n = new_node(Kind::BackendDef, loc);
    if (!n) return nullptr;
    n->text = name;
    n->is_export = exp;
    n->child = val;    // 地址：LitExpr 或 EnvExpr
    n->args = auth;    // auth 子句的 EnvExpr；无子句则为 nullptr
    if (bkind) {
      // kind 放 type_args：child 已被地址占用，args 被 auth 占用。
      // 抢这两个槽位会让"地址为空"或"auth 丢失"—— 都是静默的。
      Node* k = new_node(Kind::IdentExpr, loc);
      if (k) { k->text = bkind; n->type_args = k; }
    }
    return n;
  }

  Node* parse_workflow(bool exp) {
    Loc loc = cur().loc;
    adv();  // workflow
    const char* name = nullptr;
    if (cur().kind == Tok::STRING) {
      // 名字用了字符串字面量 → E2002
      err("E2002", "workflow 名是标识符，不要引号", cur().loc,
          "名字进符号表后是标识符，符号名会有二义。",
          "改成 workflow support_bot(...)（去掉引号）。");
      name = str_const_text(cur());
      adv();
    } else if (cur().kind == Tok::IDENT) {
      name = cur().text; adv();
    } else {
      err("E1001", "workflow 缺少名字", cur().loc, nullptr, nullptr);
    }
    if (!expect(Tok::LPAREN, "E1001", "workflow 缺少 '('")) { return nullptr; }
    Node* params = nullptr, *ptail = nullptr;
    while (cur().kind != Tok::RPAREN && !eof()) {
      Node* p = parse_param();
      if (p) append(params, ptail, p);
      else { if (cur().kind == Tok::COMMA) adv(); else sync_stmt(); }
    }
    if (!expect(Tok::RPAREN, "E1001", "workflow 缺少 ')'")) { return nullptr; }

    const char* policy = nullptr;
    if (cur().kind == Tok::KW_WITH) {
      adv();
      if (cur().kind != Tok::IDENT) { err("E1001", "`with` needs a policy name", cur().loc, "`with` declares which policy a workflow is bound by.", "write: with Guard"); return nullptr; }
      policy = cur().text;
      adv();
    }

    // entry 标注（上下文关键字，仅此处）
    bool entry = false;
    if (cur().kind == Tok::IDENT && cur().text && strcmp(cur().text, "entry") == 0 &&
        (peek(1).kind == Tok::LBRACE || peek(1).kind == Tok::ARROW)) {
      entry = true;
      adv();  // entry
    }

    if (is_header) {
      if (cur().kind == Tok::LBRACE) {
        // 无返回类型却直接带体：`workflow f() { ... }`
        err("E3005", "header 里 workflow 不能有体", cur().loc,
            ".h 只放签名（无体）；实现在 .hermes。", "删掉 '{...}' 只保留签名。");
        skip_balanced_block();
        Node* n = new_node(Kind::WorkflowSig, loc);
        if (n) { n->text = name; n->is_export = exp; n->is_entry = entry; n->policy = policy; }
        return n;
      }
      if (!expect(Tok::ARROW, "E1001", "workflow 签名缺少 '->'")) { return nullptr; }
      Node* rt = parse_type();
      if (!rt) err("E1001", "workflow 签名缺少返回类型", cur().loc, nullptr, nullptr);
      // **E3005 真正的判定点在这里**：语法上体出现在返回类型之后（`-> T { ... }`），
      // 所以参数列表之后看到的是 '->' 而不是 '{'。旧实现只在参数后查 LBRACE，
      // 于是 `.h` 里写带体 workflow 会掉进"缺 ';'"分支 —— 报错了，但**没消费 `{...}`**，
      // 后面整个体被当成顶层垃圾，才是诊断爆炸的起点。
      if (cur().kind == Tok::LBRACE) {
        err("E3005", "header 里 workflow 不能有体", cur().loc,
            ".h 只放签名（无体）；实现在 .hermes。", "删掉 '{...}' 只保留签名。");
        skip_balanced_block();
        expect(Tok::SEMI, "E1001", "workflow 签名缺少 ';'");
        Node* n = new_node(Kind::WorkflowSig, loc);
        if (n) { n->text = name; n->is_export = exp; n->is_entry = entry; n->policy = policy; }
        return n;
      }
      expect(Tok::SEMI, "E1001", "workflow 签名缺少 ';'");
      Node* n = new_node(Kind::WorkflowSig, loc);
      if (!n) return nullptr;
      n->text = name;
      n->is_export = exp;
      n->is_entry = entry;
      n->policy = policy;
      return n;
    }
    // impl：可选 -> Type 然后必须有体
    Node* ret_ty = nullptr;
    if (cur().kind == Tok::ARROW) {
      adv();
      // **必须保留**：T11 的 E4014/E4021 要拿它当契约比对返回值。
      // 早期版本这里直接丢弃 parse_type() 的结果，导致检查器无从判断
      // "声明 string、实际返回 int" —— 那个信息就在这一行被扔掉的。
      ret_ty = parse_type();
    }
    if (cur().kind == Tok::LBRACE) {
      Node* body = parse_block();
      Node* n = new_node(Kind::WorkflowDecl, loc);
      if (!n) return nullptr;
      n->text = name;
      n->is_export = exp;
      n->is_entry = entry;
      n->policy = policy;
      n->child = body;
      n->args = params;
      n->type_args = ret_ty;  // 声明返回类型（WorkflowDecl 专用槽位）
      return n;
    }
    err("E3005", "impl 里 workflow 必须有体 '{' 或返回类型后接 '{'", cur().loc, nullptr, nullptr);
    return nullptr;
  }

  Node* parse_param() {
    Loc loc = cur().loc;
    if (cur().kind == Tok::NS_INPUT) {
      adv();  // input
      Node* n = new_node(Kind::Param, loc);
      if (n) n->text = lit_str("input");
      if (cur().kind == Tok::COLON) {
        adv();
        Node* ty = parse_type();
        if (!ty) {
          err("E3023", "input 参数必须写类型", cur().loc,
              "input 的类型同时是本 workflow 的 HTTP 请求契约——hermes serve 的 API schema 由它生成。",
              "写成 input: SupportInput。");
        }
        if (n) n->child = ty;
        if (cur().kind == Tok::COMMA) adv();
        return n;
      }
      // 缺 ':' 即缺类型（LANGUAGE §11.2 ⑪ 逐字）
      err("E3023", "input 参数必须写类型", cur().loc,
          "input 的类型同时是本 workflow 的 HTTP 请求契约——hermes serve 的 API schema 由它生成。",
          "写成 input: SupportInput。");
      if (cur().kind == Tok::COMMA) adv();
      return n;
    }
    // 形参名可以是普通 Ident，也可以是关键字 backend（workflow 签名里的默认后端绑定
    // `backend = homonoia`）。backend 是声明关键字，但作为签名形参名时按位置判定。
    if (cur().kind == Tok::IDENT || cur().kind == Tok::KW_BACKEND) {
      const char* pname = cur().text; adv();
      if (!expect(Tok::EQ, "E1001", "参数缺少 '='")) { return nullptr; }
      Node* e = parse_expr();
      Node* n = new_node(Kind::Param, loc);
      if (!n) return nullptr;
      n->text = pname;
      n->args = e;  // 默认值表达式
      if (cur().kind == Tok::COMMA) adv();
      return n;
    }
    err("E1001", "参数非法", cur().loc, nullptr, nullptr);
    adv();
    return nullptr;
  }

  // ---- 语句 ----------------------------------------------------------------
  Node* parse_block() {
    Loc loc = cur().loc;
    if (!expect(Tok::LBRACE, "E1001", "缺少 '{'")) { return nullptr; }
    Node* head = nullptr, *tail = nullptr;
    while (cur().kind != Tok::RBRACE && !eof()) {
      Node* s = parse_stmt();
      if (s) append(head, tail, s);
      else sync_stmt();
    }
    if (!expect(Tok::RBRACE, "E1001", "缺少 '}'")) { return nullptr; }
    Node* n = new_node(Kind::Block, loc);
    if (!n) return nullptr;
    n->child = head;
    return n;
  }

  Node* parse_stmt() {
    switch (cur().kind) {
      case Tok::KW_CONST:
      case Tok::KW_AUTO:
      case Tok::TY_INT:
      case Tok::TY_STRING:
      case Tok::TY_BOOL:
      case Tok::TY_FLOAT:
      case Tok::TY_JSON:
      case Tok::TY_COST:
        return parse_vardecl();
      case Tok::IDENT:
        // 语句以标识符开头时只有两种可能：`x++;` / `x--;` 或 `x = expr;`。
        if (peek(1).kind == Tok::INC || peek(1).kind == Tok::DEC) return parse_update();
        if (peek(1).kind == Tok::EQ) return parse_assign();
        break;
      case Tok::KW_IF: return parse_if();
      case Tok::KW_WHILE: return parse_while();
      case Tok::KW_FOR: return parse_for();
      case Tok::KW_BREAK: return parse_break_continue(true);
      case Tok::KW_CONTINUE: return parse_break_continue(false);
      case Tok::KW_MATCH: return parse_match();
      case Tok::KW_RETRY: return parse_retry();
      case Tok::KW_TRY: return parse_try();
      case Tok::KW_RETURN:
        // parallel 内的 return [..]
        if (peek(1).kind == Tok::LBRACKET) return parse_parallel_return();
        return parse_return();
      default: break;
    }
    // 表达式语句
    Node* e = parse_expr();
    expect(Tok::SEMI, "E1001", "语句缺少 ';'");
    Node* n = new_node(Kind::ExprStmt, cur().loc);
    if (!n) return nullptr;
    n->child = e;
    return n;
  }

  // C++ 风格声明：`[const] [Type|auto] name = expr;`
  //
  // 与旧 `let` 的差别不只是写法：**默认可变**，`const` 才不可变。
  // 类型写成 `auto` 时由 initializer 推导（编译期推导，不是动态类型）。
  Node* parse_vardecl() {
    const Loc loc = cur().loc;
    bool isConst = false;
    if (cur().kind == Tok::KW_CONST) { isConst = true; adv(); }

    std::string typeName;
    bool isAuto = false;
    if (cur().kind == Tok::KW_AUTO) { isAuto = true; adv(); }
    else if (cur().kind == Tok::TY_INT) { typeName = "int"; adv(); }
    else if (cur().kind == Tok::TY_STRING) { typeName = "string"; adv(); }
    else if (cur().kind == Tok::TY_BOOL) { typeName = "bool"; adv(); }
    else if (cur().kind == Tok::TY_FLOAT) { typeName = "float"; adv(); }
    else if (cur().kind == Tok::TY_JSON) { typeName = "json"; adv(); }
    else if (cur().kind == Tok::TY_COST) { typeName = "cost"; adv(); }
    else {
      err("E1001", "变量声明需要类型（或 auto）", cur().loc,
          "形如 int count = 0; / auto x = ...;", "补上类型或 auto");
      return nullptr;
    }

    Node* pat = parse_pattern();
    if (!pat) return nullptr;
    if (!expect(Tok::EQ, "E1001", "变量声明缺少 '='")) return nullptr;
    Node* init = parse_expr();
    if (!init) return nullptr;
    expect(Tok::SEMI, "E1001", "变量声明缺少 ';'");
    Node* n = new_node(Kind::LetStmt, loc);
    if (!n) return nullptr;
    n->child = pat;
    n->args = init;
    n->is_const = isConst;
    n->is_auto = isAuto;
    if (!typeName.empty()) n->text = arena.intern(typeName.c_str(), typeName.size());
    return n;
  }

  // `x++;` / `x--;` —— 自增减语句。
  Node* parse_update() {
    const Loc loc = cur().loc;
    const char* name = cur().text;
    adv();
    const bool inc = cur().kind == Tok::INC;
    adv();
    if (!expect(Tok::SEMI, "E1001", "自增减缺少 ';'")) return nullptr;
    Node* n = new_node(Kind::UpdateStmt, loc);
    if (!n) return nullptr;
    n->text = name;
    n->i64 = inc ? 1 : -1;
    return n;
  }

  Node* parse_pattern() {
    Loc loc = cur().loc;
    Node* n = new_node(Kind::Pattern, loc);
    if (!n) return nullptr;
    if (cur().kind == Tok::LBRACKET) {
      adv();
      Node* head = nullptr, *tail = nullptr;
      while (cur().kind != Tok::RBRACKET && !eof()) {
        if (cur().kind == Tok::IDENT || cur().kind == Tok::UNDERSCORE) {
          Node* id = new_node(Kind::IdentExpr, cur().loc);
          if (id) { id->text = cur().text; }
          append(head, tail, id);
        }
        adv();
        if (cur().kind == Tok::COMMA) adv();
      }
      expect(Tok::RBRACKET, "E1001", "解构缺少 ']'");
      n->child = head;
      return n;
    }
    if (cur().kind == Tok::IDENT || cur().kind == Tok::UNDERSCORE) {
      Node* id = new_node(Kind::IdentExpr, cur().loc);
      if (id) id->text = cur().text;
      n->child = id;
      adv();
      return n;
    }
    err("E1001", "let 模式非法（期望标识符或 [..]）", cur().loc, nullptr, nullptr);
    return n;
  }

  Node* parse_if() {
    Loc loc = cur().loc;
    adv();  // if
    if (!expect(Tok::LPAREN, "E1001", "if 缺少 '('")) { return nullptr; }
    Node* cond = parse_expr();
    if (!expect(Tok::RPAREN, "E1001", "if 缺少 ')'")) { return nullptr; }
    Node* then_b = parse_block();
    Node* n = new_node(Kind::IfStmt, loc);
    if (!n) return nullptr;
    n->child = cond;
    n->args = then_b;  // then block
    if (cur().kind == Tok::KW_ELSE) {
      adv();
      if (cur().kind == Tok::KW_IF) {
        n->else_branch = parse_if();  // elseif
      } else {
        n->else_branch = parse_block();
      }
    }
    return n;
  }

  // for (let i = 0; i < 10; i++) { ... }
  //
  // 循环变量归 for 所有：只有头部的步进子句能改它，循环体里改不了。
  // 这样 IR 的 `var.decl` 唯一性不破，也就不需要可变变量。
  //
  // 步进只接受这些形态（`k` 是整数字面量）：
  //   i++ / ++i / i-- / --i / i += k / i -= k
  // while (cond) { ... } —— 无静态上界的循环。
  //
  // 它的迭代次数取决于运行期状态，所以整个 workflow 的 max_spans 变成"未知"
  // （由 codegen 标记 unbounded）。运行期由 maxForIterations 兜底，不允许真的死循环。
  Node* parse_while() {
    const Loc loc = cur().loc;
    adv();  // while
    if (!expect(Tok::LPAREN, "E1001", "while 缺少 '('")) return nullptr;
    Node* cond = parse_expr();
    if (!cond) return nullptr;
    if (!expect(Tok::RPAREN, "E1001", "while 缺少 ')'")) return nullptr;
    Node* n = new_node(Kind::WhileStmt, loc);
    if (!n) return nullptr;
    n->child = cond;
    ++loop_depth_;
    n->body = parse_block();
    --loop_depth_;
    return n;
  }

  Node* parse_for() {
    Loc loc = cur().loc;
    adv();  // for
    if (!expect(Tok::LPAREN, "E1001", "for 缺少 '('")) return nullptr;
    // C++ 风格三段式：`for (int i = 0; i < 10; i++)`。
    if (cur().kind != Tok::TY_INT && cur().kind != Tok::KW_AUTO) {
      err("E1001", "for 头需要类型（int 或 auto）", cur().loc,
          "形如 for (int i = 0; i < 10; i++)", "补上 int 或 auto");
      return nullptr;
    }
    adv();
    if (cur().kind != Tok::IDENT) {
      err("E1001", "for 缺少循环变量名", cur().loc, "形如 for (int i = 0; ...)", "补上变量名");
      return nullptr;
    }
    const char* var = cur().text;
    adv();
    if (!expect(Tok::EQ, "E1001", "for 的初始化缺少 '='")) return nullptr;
    Node* init = parse_expr();
    if (!init) return nullptr;
    if (!expect(Tok::SEMI, "E1001", "for 的初始化后面需要 ';'")) return nullptr;

    Node* cond = parse_expr();
    if (!cond) return nullptr;
    if (!expect(Tok::SEMI, "E1001", "for 的条件后面需要 ';'")) return nullptr;

    // 步进子句：i++ / ++i / i-- / --i / i += k / i -= k
    //
    // 前缀与后缀在这里**语义相同**：步进发生在迭代末尾，表达式值不被使用。
    int64_t delta = 0;
    if (cur().kind == Tok::IDENT &&
        (peek(1).kind == Tok::INC || peek(1).kind == Tok::DEC ||
         peek(1).kind == Tok::PLUS_EQ || peek(1).kind == Tok::MINUS_EQ)) {
      const Tok op = peek(1).kind;
      if (cur().text && var && strcmp(cur().text, var) != 0) {
        err("E1001", "for 的步进子句改的不是循环变量", cur().loc,
            "步进只能改循环变量本身", "改成同一个名字");
      }
      adv();  // i
      adv();  // ++ / -- / += / -=
      if (op == Tok::INC) delta = 1;
      else if (op == Tok::DEC) delta = -1;
      else {
        if (cur().kind != Tok::INT) {
          err("E1001", "for 的步长必须是整数字面量", cur().loc,
              "上界要能在编译期算出来，所以步长不能是运行期值", "写成数字");
          return nullptr;
        }
        delta = (op == Tok::PLUS_EQ ? 1 : -1) * cur().i64;
        adv();
      }
    } else if (cur().kind == Tok::INC || cur().kind == Tok::DEC) {
      delta = (cur().kind == Tok::INC) ? 1 : -1;
      adv();
      if (cur().kind != Tok::IDENT) {
        err("E1001", "自增/自减后面需要循环变量名", cur().loc, "形如 ++i", "补上变量名");
        return nullptr;
      }
      adv();
    } else {
      err("E1001", "无法识别的 for 步进子句", cur().loc,
          "只接受 i++ / ++i / i-- / --i / i += k / i -= k", "换成上面任一种");
      return nullptr;
    }
    if (!expect(Tok::RPAREN, "E1001", "for 缺少 ')'")) return nullptr;

    Node* n = new_node(Kind::ForStmt, loc);
    if (!n) return nullptr;
    n->text = var;
    n->child = init;
    n->args = cond;
    ++loop_depth_;
    n->body = parse_block();
    --loop_depth_;
    n->i64 = delta;
    return n;
  }

  // break / continue —— 只允许出现在循环体内。
  //
  // 在**解析期**就拒绝（用 loop_depth_ 计数），而不是留到 codegen 或运行期：
  // 跳出无处可去时，语义是未定义的，最好的处理是根本不接受这个程序。
  Node* parse_break_continue(bool isBreak) {
    const Loc loc = cur().loc;
    adv();
    if (loop_depth_ == 0) {
      err("E1001", isBreak ? "break 只能出现在循环里" : "continue 只能出现在循环里", loc,
          "循环外的 break/continue 没有可跳出的目标。", "删掉它，或放进 for 循环体里");
      return nullptr;
    }
    if (!expect(Tok::SEMI, "E1001", isBreak ? "break 后面需要 ';'" : "continue 后面需要 ';'")) {
      return nullptr;
    }
    return new_node(isBreak ? Kind::BreakStmt : Kind::ContinueStmt, loc);
  }

  // `x = expr;` —— 只能改写 `let mut` 声明的变量。
  //
  // 解析期不查"你有没有 mut"（那要符号表），但**要求左边是单个标识符**，
  // 这样"给字段赋值""给解构出来的名字赋值"这类歧义根本不进入语法。
  Node* parse_assign() {
    const Loc loc = cur().loc;
    const char* name = cur().text;
    adv();  // 名字
    adv();  // '='
    Node* e = parse_expr();
    if (!e) return nullptr;
    if (!expect(Tok::SEMI, "E1001", "赋值缺少 ';'")) return nullptr;
    Node* n = new_node(Kind::AssignStmt, loc);
    if (!n) return nullptr;
    n->text = name;
    n->args = e;
    return n;
  }

  Node* parse_match() {
    Loc loc = cur().loc;
    adv();  // match
    if (!expect(Tok::LPAREN, "E1001", "match 缺少 '('")) { return nullptr; }
    Node* e = parse_expr();
    if (!expect(Tok::RPAREN, "E1001", "match 缺少 ')'")) { return nullptr; }
    if (!expect(Tok::LBRACE, "E1001", "match 缺少 '{'")) { return nullptr; }
    Node* head = nullptr, *tail = nullptr;
    while (cur().kind != Tok::RBRACE && !eof()) {
      Node* arm = parse_match_arm();
      if (arm) append(head, tail, arm);
      else sync_stmt();
    }
    expect(Tok::RBRACE, "E1001", "match 缺少 '}'");
    Node* n = new_node(Kind::MatchStmt, loc);
    if (!n) return nullptr;
    n->child = e;
    n->args = head;  // 臂列表
    return n;
  }

  Node* parse_match_arm() {
    Loc loc = cur().loc;
    Node* n = new_node(Kind::MatchArm, loc);
    if (!n) return nullptr;
    if (cur().kind == Tok::KW_CASE) {
      adv();
      // case 接受字符串或整数字面量。整数臂把值放在 i64 里、text 留 null，
      // 与 default 臂区分靠 is_nullable（default 才是 true）——
      // 这样 `case ""`（空字符串）仍然能写成字符串臂，不会被当成 default。
      if (cur().kind == Tok::STRING) {
        n->text = str_const_text(cur());
        adv();
      } else if (cur().kind == Tok::INT) {
        n->i64 = cur().i64;
        adv();
      } else {
        err("E1001", "case 期望字符串或整数字面量", cur().loc, nullptr, nullptr);
      }
      expect(Tok::COLON, "E1001", "case 缺少 ':'");
      n->child = parse_block();
      return n;
    }
    if (cur().kind == Tok::KW_DEFAULT) {
      adv();
      // 用 is_nullable 复用作 "is_default" 标记（MatchArm 专用）
      n->is_nullable = true;
      expect(Tok::COLON, "E1001", "default 缺少 ':'");
      n->child = parse_block();
      return n;
    }
    err("E1001", "match 臂必须是 case \"x\": 或 default:", cur().loc, nullptr, nullptr);
    adv();
    return nullptr;
  }

  Node* parse_retry() {
    Loc loc = cur().loc;
    adv();  // retry
    int64_t cnt = 0;
    double dly = 0.0;
    if (!expect(Tok::LPAREN, "E1001", "retry 缺少 '('")) { return nullptr; }
    if (cur().kind == Tok::INT) { cnt = cur().i64; adv(); }
    else err("E2015", "retry 次数必须是整数字面量", cur().loc,
             "上界与成本必须编译期可算。", "写成 retry(2)。");
    if (cur().kind == Tok::COMMA) {
      adv();
      if (cur().kind == Tok::KW_DELAY) {
        adv();
        if (!expect(Tok::EQ, "E1001", "delay 缺少 '='")) {}
        if (cur().kind == Tok::INT) { dly = static_cast<double>(cur().i64); adv(); }
        else err("E2015", "delay 必须是整数字面量", cur().loc, nullptr, nullptr);
      }
    }
    if (!expect(Tok::RPAREN, "E1001", "retry 缺少 ')'")) { return nullptr; }
    Node* body = parse_block();
    Node* n = new_node(Kind::RetryStmt, loc);
    if (!n) return nullptr;
    n->child = body;
    n->i64 = cnt;
    n->f64 = dly;
    return n;
  }

  Node* parse_try() {
    Loc loc = cur().loc;
    adv();  // try
    Node* body = parse_block();
    Node* head = nullptr, *tail = nullptr;
    while (cur().kind == Tok::KW_CATCH) {
      Loc cl = cur().loc; adv();
      if (!expect(Tok::LPAREN, "E1001", "catch 缺少 '('")) { break; }
      const char* ctype = (cur().kind == Tok::ERR_NETWORKERROR || cur().kind == Tok::ERR_HTTPERROR ||
          cur().kind == Tok::ERR_MODELERROR || cur().kind == Tok::ERR_CONTRACTERROR ||
          cur().kind == Tok::ERR_CIRCUITOPENERROR || cur().kind == Tok::ERR_ERROR)
          ? cur().text : "";
      if (ctype[0] == '\0') err("E1001", "catch 类型非法", cur().loc, nullptr, nullptr);
      else adv();
      const char* vname = (cur().kind == Tok::IDENT) ? cur().text : "";
      if (cur().kind == Tok::IDENT) adv();
      if (!expect(Tok::RPAREN, "E1001", "catch 缺少 ')'")) { break; }
      Node* cb = parse_block();
      Node* cc = new_node(Kind::CatchClause, cl);
      if (cc) { cc->text = ctype; cc->child = new_node(Kind::IdentExpr, cl);
                if (cc->child) static_cast<Node*>(cc->child)->text = vname;
                cc->else_branch = cb; }
      if (cc) append(head, tail, cc);
    }
    Node* n = new_node(Kind::TryStmt, loc);
    if (!n) return nullptr;
    n->child = body;
    n->args = head;
    return n;
  }

  Node* parse_return() {
    Loc loc = cur().loc;
    adv();  // return
    Node* e = parse_expr();
    expect(Tok::SEMI, "E1001", "return 缺少 ';'");
    Node* n = new_node(Kind::ReturnStmt, loc);
    if (!n) return nullptr;
    n->child = e;
    return n;
  }

  Node* parse_parallel_return() {
    Loc loc = cur().loc;
    adv();  // return
    adv();  // [
    std::vector<Node*> exprs;
    while (cur().kind != Tok::RBRACKET && !eof()) {
      Node* e = parse_expr();
      if (e) exprs.push_back(e);
      if (cur().kind == Tok::COMMA) adv();
    }
    expect(Tok::RBRACKET, "E1001", "return [..] 缺少 ']'");
    expect(Tok::SEMI, "E1001", "return [..] 缺少 ';'");
    int nsubs = 0;
    Node* subs = to_subs(exprs, nsubs);
    Node* n = new_node(Kind::ParallelReturn, loc);
    if (!n) return nullptr;
    n->subs = subs;
    n->n_subs = nsubs;
    return n;
  }

  // ---- 表达式（优先级）-----------------------------------------------------
  Node* parse_expr() { return parse_or(); }

  Node* parse_or() {
    Node* left = parse_and();
    while (cur().kind == Tok::OROR) {
      Loc l = cur().loc; adv();
      Node* right = parse_and();
      Node* n = new_node(Kind::BinaryExpr, l);
      if (n) { n->text = lit_str("||"); n->child = left; n->args = right; }
      left = n;
    }
    return left;
  }
  Node* parse_and() {
    Node* left = parse_eq();
    while (cur().kind == Tok::ANDAND) {
      Loc l = cur().loc; adv();
      Node* right = parse_eq();
      Node* n = new_node(Kind::BinaryExpr, l);
      if (n) { n->text = lit_str("&&"); n->child = left; n->args = right; }
      left = n;
    }
    return left;
  }
  Node* parse_eq() {
    Node* left = parse_rel();
    while (cur().kind == Tok::EQEQ || cur().kind == Tok::NE) {
      Loc l = cur().loc; const char* op = (cur().kind == Tok::EQEQ) ? "==" : "!=";
      adv();
      Node* right = parse_rel();
      Node* n = new_node(Kind::BinaryExpr, l);
      if (n) { n->text = lit_str(op); n->child = left; n->args = right; }
      left = n;
    }
    return left;
  }
  Node* parse_rel() {
    Node* left = parse_add();
    while (cur().kind == Tok::LT || cur().kind == Tok::LE ||
           cur().kind == Tok::GT || cur().kind == Tok::GE) {
      Loc l = cur().loc; const char* op = cur().kind == Tok::LT ? "<"
          : cur().kind == Tok::LE ? "<=" : cur().kind == Tok::GT ? ">" : ">=";
      adv();
      Node* right = parse_add();
      Node* n = new_node(Kind::BinaryExpr, l);
      if (n) { n->text = lit_str(op); n->child = left; n->args = right; }
      left = n;
    }
    return left;
  }
  Node* parse_add() {
    Node* left = parse_mul();
    while (cur().kind == Tok::PLUS || cur().kind == Tok::MINUS) {
      Loc l = cur().loc; const char* op = (cur().kind == Tok::PLUS) ? "+" : "-";
      adv();
      Node* right = parse_mul();
      Node* n = new_node(Kind::BinaryExpr, l);
      if (n) { n->text = lit_str(op); n->child = left; n->args = right; }
      left = n;
    }
    return left;
  }
  Node* parse_mul() {
    Node* left = parse_unary();
    while (cur().kind == Tok::STAR || cur().kind == Tok::SLASH || cur().kind == Tok::PERCENT) {
      Loc l = cur().loc; const char* op = cur().kind == Tok::STAR ? "*"
          : cur().kind == Tok::SLASH ? "/" : "%";
      adv();
      Node* right = parse_unary();
      Node* n = new_node(Kind::BinaryExpr, l);
      if (n) { n->text = lit_str(op); n->child = left; n->args = right; }
      left = n;
    }
    return left;
  }
  Node* parse_unary() {
    if (cur().kind == Tok::BANG || cur().kind == Tok::MINUS) {
      Loc l = cur().loc; const char* op = (cur().kind == Tok::BANG) ? "!" : "-";
      adv();
      Node* operand = parse_unary();
      Node* n = new_node(Kind::UnaryExpr, l);
      if (n) { n->text = lit_str(op); n->child = operand; }
      return n;
    }
    return parse_postfix();
  }
  Node* parse_postfix() {
    Node* base = parse_primary();
    while (cur().kind == Tok::DOT || cur().kind == Tok::LBRACKET) {
      if (cur().kind == Tok::DOT) {
        Loc l = cur().loc; adv();
        if (cur().kind == Tok::IDENT) {
          Node* field = new_node(Kind::IdentExpr, cur().loc);
          if (field) field->text = cur().text;
          adv();
          Node* n = new_node(Kind::PathExpr, l);
          // 把 base + field 组合为 PathExpr（subs = [base, field]）
          std::vector<Node*> comps;
          if (base && base->kind == Kind::PathExpr && base->subs) {
            for (int k = 0; k < base->n_subs; ++k) comps.push_back(&base->subs[k]);
          } else if (base) {
            comps.push_back(base);
          }
          if (field) comps.push_back(field);
          int ns = 0;
          Node* sub = to_subs(comps, ns);
          if (n) { n->subs = sub; n->n_subs = ns; }
          base = n;
        } else { err("E1001", "'.' 后期望标识符", cur().loc, nullptr, nullptr); break; }
      } else {  // LBRACKET：字符串 → json 常量键；其它 → 数组下标
        Loc l = cur().loc; adv();
        if (cur().kind == Tok::STRING) {
          const char* key = str_const_text(cur());
          adv();
          if (!expect(Tok::RBRACKET, "E2010", "json 字段访问缺少 ']'")) { break; }
          Node* n = new_node(Kind::IndexExpr, l);
          if (n) { n->text = key; n->child = base; }
          base = n;
        } else {
          // 数组下标：`xs[0]`。下标可以是任意整数表达式，但**字面量**会在
          // codegen 阶段和已知长度对账（越界是编译期错误，不是运行期崩溃）。
          Node* idx = parse_or();
          if (!idx) break;
          if (!expect(Tok::RBRACKET, "E1001", "下标缺少 ']'")) break;
          Node* n = new_node(Kind::IndexExpr, l);
          if (n) { n->text = nullptr; n->child = base; n->args = idx; }
          base = n;
        }
      }
    }
    return base;
  }
  Node* parse_array_lit() {
    Loc loc = cur().loc;
    expect(Tok::LBRACKET, "E1001", "数组字面量缺少 '['");
    Node* head = nullptr;
    Node* tail = nullptr;
    while (cur().kind != Tok::RBRACKET && cur().kind != Tok::EOF_) {
      Node* e = parse_or();
      if (!e) break;
      append(head, tail, e);
      if (cur().kind == Tok::COMMA) { adv(); continue; }
      break;
    }
    expect(Tok::RBRACKET, "E1001", "数组字面量缺少 ']'");
    Node* n = new_node(Kind::ArrayLit, loc);
    if (n) { n->child = head; }
    return n;
  }

  Node* parse_primary() {
    Tok k = cur().kind;
    // `input` 单独作为表达式：`tool.call(header = input, body = input)` 这种
    // "把整个输入对象传下去"是最常见的用法。之前只有 `input.field` 能解析，
    // 于是想传整个对象就必须拆成字段一个个拼 —— 而没人会那么写。
    if (k == Tok::NS_INPUT) {
      Loc loc = cur().loc;
      adv();
      Node* n = new_node(Kind::IdentExpr, loc);
      if (n) n->text = lit_str("input");
      return n;
    }
    if (k == Tok::LBRACKET) {
      // 数组字面量只出现在参数位（allow_status = [404, 409]）。放在 primary
      // 是为了让它在任何表达式位置都能被解析 —— 但**语义上**它必须由调用方
      // 判定是否合法（codegen 只接受常量元素，别处会明确报错）。
      return parse_array_lit();
    }
    if (k == Tok::LPAREN) {
      adv();
      Node* e = parse_expr();
      expect(Tok::RPAREN, "E1001", "缺少 ')'");
      return e;
    }
    if (k == Tok::KW_TRUE || k == Tok::KW_FALSE) {
      Node* n = new_node(Kind::LitExpr, cur().loc);
      if (n) { n->text = lit_str("bool"); n->is_nullable = (k == Tok::KW_TRUE); }
      adv();
      return n;
    }
    if (k == Tok::INT || k == Tok::FLOAT || k == Tok::COST_LIT || k == Tok::STRING) {
      return parse_lit();
    }
    if (k == Tok::KW_PARALLEL) return parse_parallel();
    if (k == Tok::NS_MODEL || k == Tok::NS_TOOL || k == Tok::NS_JSON ||
        k == Tok::NS_MEMORY || k == Tok::NS_VECTOR) {
      return parse_call_or_path();
    }
    if (k == Tok::IDENT) return parse_call_or_path();
    err("E1001", "意外的表达式起始", cur().loc, nullptr, nullptr);
    adv();
    return nullptr;
  }

  Node* parse_lit() {
    const Token& tk = cur();
    Node* n = new_node(Kind::LitExpr, tk.loc);
    if (!n) return nullptr;
    if (tk.kind == Tok::INT) { n->text = lit_str("int"); n->i64 = tk.i64; }
    else if (tk.kind == Tok::FLOAT) { n->text = lit_str("float"); n->f64 = tk.f64; }
    else if (tk.kind == Tok::COST_LIT) { n->text = lit_str("cost"); n->i64 = tk.i64; }
    else if (tk.kind == Tok::STRING) {
      n->text = lit_str("string");
      // 插值片段 → subs
      if (tk.parts && tk.n_parts > 0) {
        std::vector<Node*> parts;
        for (int p = 0; p < tk.n_parts; ++p) {
          Node* sp = new_node(Kind::StringPartNode, tk.loc);
          if (sp) { sp->is_path = tk.parts[p].is_path; sp->text = tk.parts[p].text; }
          if (sp) parts.push_back(sp);
        }
        int ns = 0;
        n->subs = to_subs(parts, ns);
        n->n_subs = ns;
      }
    }
    adv();
    return n;
  }

  Node* parse_path() {
    // (NS_MODEL | NS_TOOL | NS_JSON | IDENT) { . IDENT }*
    // 首项允许内建命名空间 token（model./tool./json.），其余分量必须是普通 IDENT。
    Loc loc = cur().loc;
    std::vector<Node*> comps;
    if (cur().kind == Tok::IDENT || cur().kind == Tok::NS_MODEL || cur().kind == Tok::NS_MEMORY ||
        cur().kind == Tok::NS_VECTOR ||
        cur().kind == Tok::NS_TOOL || cur().kind == Tok::NS_JSON) {
      Node* id = new_node(Kind::IdentExpr, cur().loc);
      if (id) id->text = cur().text;
      comps.push_back(id);
      adv();
    } else { err("E1001", "期望标识符", cur().loc, nullptr, nullptr); }
    while (cur().kind == Tok::DOT) {
      adv();
      if (cur().kind == Tok::IDENT) {
        Node* id = new_node(Kind::IdentExpr, cur().loc);
        if (id) id->text = cur().text;
        comps.push_back(id);
        adv();
      } else { err("E1001", "'.' 后期望标识符", cur().loc, nullptr, nullptr); break; }
    }
    int ns = 0;
    Node* subs = to_subs(comps, ns);
    Node* n = new_node(Kind::PathExpr, loc);
    if (!n) return nullptr;
    n->subs = subs;
    n->n_subs = ns;
    return n;
  }

  // 处理内建被调者：model./tool./json.ident，可选 <T>，必须 (args)
  int loop_depth_ = 0;  // 当前嵌套的循环深度（break/continue 的合法性）

  Node* parse_call_or_path() {
    Loc loc = cur().loc;
    // 裸函数调用：`len(xs)`。必须是 IDENT 紧跟 '('，否则退回路径。
    // 先记下 token 下标，退回来时不会多消费。
    if (cur().kind == Tok::IDENT) {
      size_t save = i;
      Node* callee = parse_path();
      if (cur().kind == Tok::LPAREN) {
        adv();
        Node* args = parse_args();
        expect(Tok::RPAREN, "E1001", "调用缺少 ')'");
        Node* n = new_node(Kind::CallExpr, loc);
        if (n) { n->child = callee; n->args = args; }
        return n;
      }
      i = save;
      return parse_path();
    }
    // callee：ns . ident
    if (cur().kind != Tok::NS_MODEL && cur().kind != Tok::NS_TOOL && cur().kind != Tok::NS_JSON &&
      cur().kind != Tok::NS_MEMORY && cur().kind != Tok::NS_VECTOR) {
      return parse_path();
    }
    Node* callee = parse_path();  // ns.ident（parse_path 已吃掉 ns 和 '.' 和 ident）
    // 尝试 <T> 类型实参
    if (cur().kind == Tok::LT) {
      size_t save = i;
      adv();  // <
      Node* ty = parse_type();
      if (ty) {
        if (cur().kind == Tok::GT) {
          adv();  // >
          if (cur().kind == Tok::LPAREN) {
            // 完整类型实参调用
            adv();  // (
            Node* args = parse_args();
            expect(Tok::RPAREN, "E1001", "调用缺少 ')'");
            Node* n = new_node(Kind::CallExpr, loc);
            if (n) { n->child = callee; n->type_args = ty; n->args = args; }
            return n;
          } else {
            // <T> 但缺 '(' → E2001 期望 '('
            err("E2001", "类型实参列表后期望 '('", cur().loc,
                "model.generate<T> 的形态是 '<' 类型 '>' 紧接 '('。",
                "补上 '('：model.generate<Intent>(...)。");
            // 恢复：跳到 ) 或 ; 
            while (!eof() && cur().kind != Tok::RPAREN && cur().kind != Tok::SEMI &&
                   cur().kind != Tok::RBRACE) adv();
            if (cur().kind == Tok::RPAREN) adv();
            Node* n = new_node(Kind::CallExpr, loc);
            if (n) { n->child = callee; n->type_args = ty; }
            return n;
          }
        } else {
          // <T 但缺 '>' → E2001 期望 '>'
          char buf[160];
          // 诊断里要显示"读到了什么"：STRING 走 str_const_text 拿内容，
          // 取不到（插值串）时退回 token 名，不留空指针也不显示空引号。
          const char* sc = str_const_text(cur());
          const char* got = (sc && sc[0]) ? sc : tok_name(cur().kind);
          snprintf(buf, sizeof(buf), "类型实参列表未闭合（期望 '>'，读到 '%s'）", got);
          const char* msg = arena.intern(buf, strlen(buf));
          err("E2001", msg, cur().loc,
              "model.generate<T> 的形态是 '<' 类型 '>' 紧接 '('。",
              "补上 '>'：model.generate<Intent>(...)。");
          // 恢复
          while (!eof() && cur().kind != Tok::RPAREN && cur().kind != Tok::SEMI &&
                 cur().kind != Tok::RBRACE) adv();
          if (cur().kind == Tok::RPAREN) adv();
          Node* n = new_node(Kind::CallExpr, loc);
          if (n) { n->child = callee; n->type_args = ty; }
          return n;
        }
      } else {
        // < 后不是合法类型（如 <5）→ 回退：model.generate 作为 PathExpr，< 交给 rel 当 LT
        i = save;
        return callee;
      }
    }
    // 无类型实参：必须是普通调用 (args)
    if (cur().kind == Tok::LPAREN) {
      adv();
      Node* args = parse_args();
      expect(Tok::RPAREN, "E1001", "调用缺少 ')'");
      Node* n = new_node(Kind::CallExpr, loc);
      if (n) { n->child = callee; n->args = args; }
      return n;
    }
    // 既不是类型实参也不是调用：内建被调者必须被调用
    err("E1001", "内建被调者必须被调用（期望 '(' 或 '<'）", cur().loc,
        "model./tool./json. 只能作调用前缀。", "写成 model.generate(...)。");
    return callee;
  }

  const char* tok_name(Tok k) {
    switch (k) {
      case Tok::LPAREN: return "(";
      case Tok::RPAREN: return ")";
      case Tok::LBRACE: return "{";
      case Tok::RBRACE: return "}";
      case Tok::SEMI: return ";";
      case Tok::COMMA: return ",";
      case Tok::ARROW: return "->";
      default: return "?";
    }
  }

  Node* parse_args() {
    Node* head = nullptr, *tail = nullptr;
    while (cur().kind != Tok::RPAREN && !eof()) {
      if (cur().kind == Tok::IDENT && peek(1).kind == Tok::EQ) {
        const char* aname = cur().text; Loc al = cur().loc;
        adv(); adv();  // name =
        Node* e = parse_expr();
        Node* a = new_node(Kind::Param, al);  // 复用 Param 表示实参(name=expr)
        if (a) { a->text = aname; a->args = e; }
        if (a) append(head, tail, a);
      } else {
        // 位置实参（本语言要求命名，但容错吃掉）
        Node* e = parse_expr();
        Node* a = new_node(Kind::Param, cur().loc);
        if (a) a->args = e;
        append(head, tail, a);
      }
      if (cur().kind == Tok::COMMA) adv();
      else break;
    }
    return head;
  }

  Node* parse_parallel() {
    Loc loc = cur().loc;
    adv();  // parallel
    if (!expect(Tok::LBRACE, "E1001", "parallel 缺少 '{'")) { return nullptr; }
    Node* head = nullptr, *tail = nullptr;
    while (cur().kind != Tok::RBRACE && !eof()) {
      if (cur().kind == Tok::KW_RETURN && peek(1).kind == Tok::LBRACKET) {
        Node* pr = parse_parallel_return();
        append(head, tail, pr);
        continue;
      }
      Node* s = parse_stmt();
      if (s) append(head, tail, s);
      else sync_stmt();
    }
    expect(Tok::RBRACE, "E1001", "parallel 缺少 '}'");
    Node* n = new_node(Kind::ParallelExpr, loc);
    if (!n) return nullptr;
    n->child = head;  // 块语句列表
    return n;
  }
};

}  // namespace

// 公共入口：解析一份文件。is_header 由调用方按扩展名判定。
Node* parse(const std::vector<Token>& toks, const char* filename, bool is_header,
            Arena& arena, DiagBag& diags) {
  Parser p(toks, filename, is_header, arena, diags);
  // 记录 entry 标注：在 parse_workflow 内部处理（见 entry_flag_ 逻辑）
  return p.parse_module();
}

}  // namespace hm
