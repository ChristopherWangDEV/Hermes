// check_idem.cpp —— §5.7 `retry` 幂等证明（E5007 / E5009）。
//
// ## 为什么是独立 pass，而不是塞进 check_type.cpp
//
// 它需要「词法嵌套关系」（哪个 tool.call 落在哪个 retry 的子树里）和
// 「调用点声明」（method / idempotent / idem_key），这两样都不是类型推导。
// 类型检查器按语句树递归时已经在管作用域与 Ty，把重试上下文再压进去会让
// 两类不相关的状态互相污染。放在类型检查之后、codegen 之前，正好是
// LANGUAGE.md §2 五阶段链路里第 ③ 步的位置。
//
// ## 为什么必须编译期判死
//
// 运行期能拿到的只有 URL 和方法字符串，而「这个调用点重发一次安全吗」是
// **声明性事实**：由作者声明（idempotent / idem_key）或由方法语义推定
// （GET / HEAD 只读）。运行期没法补这个事实，只能靠猜。猜错的代价是重复扣款，
// 所以这里按项目一贯的「宁可拒绝，不要猜」给出硬错（R-CB-06 的编译期化）。
//
// ## 纯度判定的失败方向
//
// Pure(e) 认不出来的东西一律判**不纯**。同名变量若在任意一处被绑到不纯初
// 始化式，这个名字也整体判不纯（见 bindings_ 的合并规则）—— 作用域信息不足
// 时宁可误报一条 E5009，也不能让一个「重试时会变」的幂等键混进 IR。
//
// 设计约束与其他 pass 一致：无异常、无 RTTI、无 IO、无全局可变状态。

#include "arena.h"
#include "ast.h"

#include <cstdarg>
#include <cstdio>
#include <cstring>
#include <string>
#include <utility>
#include <vector>

namespace hm {
namespace {

bool eq(const char* a, const char* b) {
  if (a == nullptr || b == nullptr) return a == b;
  return std::strcmp(a, b) == 0;
}

// dotted 的第一段。插值片段里的路径是点分原文（"input.user_id"），
// 这里只需要根名字就能判定纯净性。
std::string first_seg(const char* dotted) {
  std::string s = dotted ? dotted : "";
  const std::size_t dot = s.find('.');
  if (dot != std::string::npos) s.resize(dot);
  return s;
}

struct Binding {
  std::string name;
  bool all_pure;  // 该名字到目前为止**所有**初始化式都纯
};

class IdemCheck {
 public:
  IdemCheck(const char* file, Arena& a, DiagBag& d) : file_(file), a_(a), d_(d) {}

  void run(Node* root) { scan(root); }

 private:
  const char* file_;
  Arena& a_;
  DiagBag& d_;
  std::vector<const Node*> retry_;  // 词法嵌套的 retry 栈（back = 最内层）
  std::vector<Binding> bindings_;

  // ---- 诊断 -------------------------------------------------------------

  const char* internf(const char* fmt, ...) {
    char buf[512];
    va_list ap;
    va_start(ap, fmt);
    std::vsnprintf(buf, sizeof(buf), fmt, ap);
    va_end(ap);
    return a_.intern(buf, std::strlen(buf));
  }

  void err(const char* code, const char* msg, Loc loc, const char* why,
           const char* fix, const char* fix2 = nullptr, const char* fix3 = nullptr,
           const DiagNote* notes = nullptr, int n_notes = 0) {
    Diag d;
    std::memset(&d, 0, sizeof(d));
    d.severity = Severity::Error;
    d.code = code;
    d.message = msg;
    d.loc = loc;
    d.module = file_;
    d.why = why;
    d.fix = fix;
    d.fix2 = fix2;
    d.fix3 = fix3;
    d.notes = notes;
    d.n_notes = n_notes;
    diag_add(d_, a_, d);
  }

  // ---- Pure(e)（LANGUAGE §5.7）------------------------------------------

  bool pure_name(const char* name) const {
    if (eq(name, "input")) return true;  // input 在一次 run 内固定
    for (const Binding& b : bindings_) {
      if (b.name == (name ? name : "")) return b.all_pure;
    }
    return false;
  }

  bool pure_path(const char* dotted) const {
    if (!dotted || !dotted[0]) return false;
    return pure_name(first_seg(dotted).c_str());
  }

  bool pure_expr(const Node* e) const {
    if (!e) return false;
    switch (e->kind) {
      case Kind::LitExpr: {
        // 无插值片段 → 常量；有片段时逐个检查插值路径（字面片段恒纯）。
        for (int i = 0; i < e->n_subs; ++i) {
          if (e->subs[i].is_path && !pure_path(e->subs[i].text)) return false;
        }
        return true;
      }
      case Kind::IdentExpr:
        return pure_name(e->text);
      case Kind::PathExpr:
        return e->n_subs >= 1 && pure_name(e->subs[0].text);
      case Kind::BinaryExpr:
        return pure_expr(e->child) && pure_expr(e->args);
      case Kind::UnaryExpr:
        return pure_expr(e->child);
      case Kind::IndexExpr:
        return pure_expr(e->child) && (!e->args || pure_expr(e->args));
      default:
        // model.generate / tool.call / json.* / 用户函数 / fallback / parallel
        // ……一律不纯。Pure(其他) = false，这是规格写明的失败方向。
        return false;
    }
  }

  void bind(const Node* let) {
    const Node* pat = let->child;  // Pattern
    const Node* id = pat ? pat->child : nullptr;  // 单名绑定；解构 [a,b] 有链
    if (!id) return;
    const bool p = pure_expr(let->args);
    // 解构出来的名字按同一个初始化式记；整条链都是同一个值。
    for (const Node* n = id; n; n = n->next) {
      if (!n->text) continue;
      const std::string name = n->text;
      bool found = false;
      for (Binding& b : bindings_) {
        if (b.name == name) {
          b.all_pure = b.all_pure && p;  // 任一次不纯 ⇒ 整体不纯
          found = true;
          break;
        }
      }
      if (!found) bindings_.push_back(Binding{name, p});
    }
  }

  // ---- AST 遍历 ---------------------------------------------------------
  //
  // scan() 只看**一个**节点及其子树，不碰 next；链一律由 scan_chain() 走。
  // 把 next 混进 scan() 会让 Block 的逐语句遍历与链遍历重复访问同一棵子树，
  // 于是同一个 tool.call 报两次 E5007。
  void scan(Node* n) {
    if (!n) return;
    if (n->kind == Kind::RetryStmt) {
      retry_.push_back(n);
      scan(n->child);  // body（Block）
      retry_.pop_back();
      // retry 没有别的槽位，但保持与通用遍历一致，防止以后加字段时漏掉。
      scan_chain(n->args);
      scan_chain(n->type_args);
      scan_chain(n->else_branch);
      scan_chain(n->body);
      return;
    }
    if (n->kind == Kind::LetStmt) bind(n);
    if (n->kind == Kind::CallExpr) check_call(n);
    scan_chain(n->child);
    scan_chain(n->args);
    scan_chain(n->type_args);
    scan_chain(n->else_branch);
    scan_chain(n->body);
    // subs 是定长数组，元素（IdentExpr / StringPartNode）没有子节点，
    // 它们的文本已由 pure_expr / pure_path 直接读，不需要再递归。
  }

  void scan_chain(Node* n) {
    while (n) {
      Node* next = n->next;
      scan(n);
      n = next;
    }
  }

  // ---- tool.call 检查 ---------------------------------------------------

  static const Node* param_value(const Node* p) { return p->args; }

  bool is_tool_call(const Node* call) const {
    const Node* callee = call->child;
    if (!callee || callee->kind != Kind::PathExpr || callee->n_subs < 2) return false;
    return eq(callee->subs[0].text, "tool") && eq(callee->subs[1].text, "call");
  }

  static bool valid_method(const std::string& m) {
    return m == "GET" || m == "HEAD" || m == "POST" ||
           m == "PUT" || m == "PATCH" || m == "DELETE";
  }

  // 字符串字面量折叠成常量文本；含插值或不是字面量时 known=false。
  // method 必须是编译期已知的 6 个字面量之一（D-014.7），否则「只读」不成立。
  std::string const_string(const Node* v, bool* known) const {
    *known = false;
    if (!v || v->kind != Kind::LitExpr || !eq(v->text, "string")) return "";
    std::string out;
    if (v->n_subs == 0) {
      // 没有片段的空串；text 是类型名，不是内容。
      *known = true;
      return "";
    }
    for (int i = 0; i < v->n_subs; ++i) {
      if (v->subs[i].is_path) return "";
      if (v->subs[i].text) out += v->subs[i].text;
    }
    *known = true;
    return out;
  }

  // 让 caret 覆盖整个调用（CallExpr.loc 只到 `tool` 这个首 token）。
  Loc call_span(const Node* call) const {
    Loc l = call->loc;
    for (const Node* p = call->args; p; p = p->next) {
      if (p->loc.end_line > l.end_line ||
          (p->loc.end_line == l.end_line && p->loc.end_col > l.end_col)) {
        l.end_line = p->loc.end_line;
        l.end_col = p->loc.end_col;
      }
    }
    return l;
  }

  void check_call(const Node* call) {
    if (!is_tool_call(call)) return;

    const Node* methodArg = nullptr;
    const Node* keyArg = nullptr;
    const Node* idemTrueArg = nullptr;
    for (const Node* p = call->args; p; p = p->next) {
      const char* nm = p->text ? p->text : "";
      if (eq(nm, "method")) methodArg = param_value(p);
      else if (eq(nm, "idem_key")) keyArg = param_value(p);
      else if (eq(nm, "idempotent")) idemTrueArg = param_value(p);
    }

    // ② idem_key 的纯净性是**独立于 retry** 的声明约束：键若由模型输出或工具
    // 响应决定，每次重算都会变，去重失效。先报最具体的 E5009，避免同一行
    // 再叠一条 E5007（两条修法指向同一个动作，只会制造噪声）。
    if (keyArg && !pure_expr(keyArg)) {
      err("E5009", "idempotency key must be derived from input", keyArg->loc,
          "If the key comes from a model output or a tool response, every retry "
          "computes a different value and the server's Idempotency-Key "
          "deduplication stops working — the same request runs more than once.",
          "Use a pure expression over input: idem_key = \"{input.user_id}\".",
          "Or use a pure alias of input: const string k = input.user_id; "
          "idem_key = \"{k}\".",
          "If no retry is needed, move the call out of the retry block; "
          "read-only methods (GET / HEAD) do not need an idempotency key.");
      return;
    }

    // D-014.7 的前置条件：`method` 必须是编译期已知的字面量。
    // 这条不是格式洁癖 —— §5.7 的第 ③ 条「只读天然幂等」完全建立在
    // 「编译器知道这是 GET/HEAD」上。若 method 可以由运行期表达式决定，
    // readonly 就只是一个注释，retry 包着 POST 也拦不住。
    // E2016 / E2017 在附录 A 里登记为语法码，但语法器只看到一串通用实参、
    // 不知道哪个是 tool.call；真正读这个参数的是本 pass，所以判在这里。
    std::string method = "GET";  // 省略 method 默认 GET
    bool methodKnown = true;
    if (methodArg) method = const_string(methodArg, &methodKnown);
    if (!methodKnown) {
      err("E2016", "tool.call method must be a string literal", methodArg->loc,
          "Idempotency proofs depend on knowing at compile time that this is a "
          "read-only method. If method is a variable or an interpolated "
          "expression, the actual method is only known at runtime and the "
          "\"readonly\" label becomes unsubstantiated.",
          "Write one of the six literals: \"GET\" / \"HEAD\" / \"POST\" / "
          "\"PUT\" / \"PATCH\" / \"DELETE\".",
          "Omit method entirely — it defaults to \"GET\".");
      return;
    }
    if (!valid_method(method)) {
      err("E2017", "tool.call method is not a valid HTTP method",
          methodArg ? methodArg->loc : call->loc,
          "Only the six literals are allowed. Letting an unknown method into IR "
          "removes the premise of the read-only-implies-idempotent inference; "
          "IR.md section 3.4 pins method to the same set.",
          "Use one of \"GET\" / \"HEAD\" / \"POST\" / \"PUT\" / \"PATCH\" / "
          "\"DELETE\".");
      return;
    }

    const bool readonly = method == "GET" || method == "HEAD";
    const bool idemTrue = idemTrueArg && idemTrueArg->kind == Kind::LitExpr &&
                          eq(idemTrueArg->text, "bool") && idemTrueArg->is_nullable;
    const bool proved = readonly || idemTrue || (keyArg != nullptr);

    if (proved || retry_.empty()) return;

    const Node* r = retry_.back();
    const long long attempts = static_cast<long long>(r->i64) + 1;

    DiagNote* note = static_cast<DiagNote*>(a_.alloc(sizeof(DiagNote)));
    if (note) {
      note->module = file_;
      note->line = r->loc.line;
      note->col = r->loc.col;
      note->message = internf("this call sits inside retry(%lld) and may run up to %lld times",
                              static_cast<long long>(r->i64), attempts);
    }

    err("E5007", "tool call inside retry has no idempotency proof", call_span(call),
        "POST / PUT / PATCH / DELETE are resubmitted on every retry. The runtime "
        "cannot decide whether a call is safe to retry — idempotency is a "
        "declarative fact that only the author or the method semantics can "
        "supply. The language has the static call site, so it rejects here "
        "rather than letting duplicate charges happen in production.",
        "If the upstream really is idempotent, add idempotent = true",
        "Have the engine inject a key: idem_key = \"{input.user_id}\" "
        "(must depend only on input, never on model output or tool responses)",
        "If the method is read-only, use method = \"GET\" / \"HEAD\"",
        note, note ? 1 : 0);
  }
};

}  // namespace

void check_idempotency(Node* root, const char* filename, Arena& arena, DiagBag& diags) {
  IdemCheck c(filename, arena, diags);
  c.run(root);
}

}  // namespace hm
