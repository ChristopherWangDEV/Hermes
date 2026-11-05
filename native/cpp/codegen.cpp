#include <map>
#include "codegen.h"

#include <vector>

namespace hm {
namespace {

// Gen 是一次 workflow 的 codegen 状态机。
//
// 节点编号严格按 IR.md §4.2 的「节点编号规则」：**前序 DFS，父先于子**。
// 实现方式就是"先 alloc 父、再递归子、最后回填子 id"，与规则同形 ——
// 若改成"先算子再 alloc 父"，编号会整体后移，golden 立刻对不上，
// 而两边都是"合法 IR"，不会有任何检查报错。这就是那条规则必须写死的原因。
class Gen {
 public:
  explicit Gen(std::string* err) : err_(err) {}

  bool run(const Node* wf, JVal* out);
  // run_function 把一个 FunctionDecl 编成函数表的一项（见 codegen_functions）。
  // 每个 Gen 只编一个函数：nodes_/vars_ 的状态按"一次编译单元"设计，
  // 跨函数复用会让第二个函数的节点 id 从第一个的末尾开始 —— 那是一份
  // 合法但无法按函数校验的 IR。
  bool run_function(const Node* fn, JVal* out);
  void set_functions(const std::map<std::string, const Node*>* fns) { fns_ = fns; }
  // set_recursive 装入"哪些函数递归"的判定。为空表示**分析阶段**：
  // 此时所有调用都按 call_fn 记边，不做内联（否则分析会依赖分析结果）。
  void set_recursive(const std::map<std::string, bool>* rec) { fn_recursive_ = rec; }
  const Node* err_node() const { return err_node_; }
  // calls 是本 Gen 编过的调用边（caller → callee）。函数表需要它做递归判定。
  const std::map<std::string, std::vector<std::string>>& calls() const { return calls_; }

 private:
  bool fail(const std::string& why) {
    if (err_) *err_ = why;
    return false;
  }
  // fail_at 记录失败所在的 AST 节点：诊断要像 Rust 一样指到源码位置，
  // 而不是永远停在 1:1。
  bool fail_at(const Node* at, const std::string& why) {
    if (err_node_ == nullptr && at != nullptr) err_node_ = at;
    return fail(why);
  }

  JVal loc_of(const Node* n) {
    JVal l = JVal::object();
    l.set("m", JVal::integer(0));
    l.set("l", JVal::integer(n ? n->loc.line : 1));
    l.set("c", JVal::integer(n ? n->loc.col : 1));
    l.set("el", JVal::integer(n ? n->loc.end_line : 1));
    l.set("ec", JVal::integer(n ? n->loc.end_col : 1));
    return l;
  }

  JVal type_of(const Node* t);
  // fn_type_expr 与 type_of 的区别：struct 类型写成 {"k":"struct","ref":"X"}，
  // 由装配阶段换成 id（与 codegen_types 同一条分工）。函数签名可以带 struct，
  // 而 workflow.returns 的历史路径只写名字 —— 那条路径不动，避免 golden 漂移。
  JVal fn_type_expr(const Node* t);

  int alloc(const char* kind, const Node* at) {
    JVal n = JVal::object();
    n.set("id", JVal::integer(static_cast<std::int64_t>(nodes_.size())));
    n.set("k", JVal::string(kind));
    n.set("loc", loc_of(at));
    nodes_.push_back(std::move(n));
    span_.push_back(0);
    model_.push_back(0);
    tool_.push_back(0);
    return static_cast<int>(nodes_.size()) - 1;
  }

  // children_of 把"是子节点"的两种存放位置（child 链 / args 链）合成一个序列。
  //
  // 为什么需要它：ast_dump 只告诉你"这些是子节点"，不告诉你挂在哪个字段，
  // 而 if/match/parallel 在不同嵌套位置会把子节点放进不同槽位。逐个槽位猜
  // 已经踩了三次（PathExpr 的段在 subs、ParallelReturn 的值在 subs、
  // if 的条件在 args）。**照 dump 看层级、按两个槽位收**，比猜稳定。
  std::vector<const Node*> children_of(const Node* n);

  // lit_text 取一个"已折叠的字符串字面量"的文本。这些参数（precision/temperature）
  // 在语言里就是字面量，运行期不该再求值；取不到就返回空串，由调用方表达"未指定"。
  std::string lit_text(const Node* e);

  int var_index(const std::string& name);
  int gen_stmt(const Node* s);
  int gen_expr(const Node* e);
  // gen_path_ 把 `a.b.c` 编成嵌套 field 节点（IR.md §4.2）。
  int gen_path_(const Node* e);

  std::vector<JVal> nodes_;
  // span_ 与 nodes_ 一一对应：IR.md §5.1 的 SP 代数。
  // 为什么现在就算：max_spans 是**运行期断言**（len(spans) ≤ max_spans），
  // 算小了会让正确的 trace 被判成 compiler bug，算大了则上界失去意义。
  // 而且它**没人重算**（加载器只查不等式），所以错了不会有任何检查红。
  std::vector<int> span_;
  // 模型/工具调用次数的上界。代数与 SP 同构（seq 求和、分支取最大、retry 乘倍数），
  // 只有叶节点不同：call_model 记 1 次模型调用，call_tool 记 1 次工具调用。
  std::vector<int> model_;
  std::vector<int> tool_;
  int max_conc_ = 1;
  std::vector<std::string> vars_;  // 变量表（声明顺序 = 下标）
  // 变量 → 定长数组长度。用于**编译期越界检查**：
  // `let xs = [1,2,3]; xs[5]` 里的 5 是确定的越界，不该留到运行期。
  std::map<std::string, int> arr_len_;
  // 静态推导 for 的迭代次数：init 是整数字面量、cond 形如 `<ident> < K` /
  // `<ident> <= K`（K 是整数字面量）、步长非零。推不出来返回 -1 —— 调用方
  // 会把整个 workflow 标成 unbounded，而不是编一个假数字。
  static int for_iterations(const Node* init, const Node* cond, int64_t delta) {
    if (!init || !cond || delta == 0) return -1;
    if (init->kind != Kind::LitExpr || !init->text || std::string(init->text) != "int") return -1;
    if (cond->kind != Kind::BinaryExpr || !cond->text) return -1;
    const std::string sym = cond->text;
    if (sym != "<" && sym != "<=") return -1;
    const Node* rhs = cond->args;
    if (!rhs || rhs->kind != Kind::LitExpr || !rhs->text || std::string(rhs->text) != "int") return -1;
    const int64_t start = init->i64;
    const int64_t stop = rhs->i64;
    if (delta <= 0) return -1;   // 逆向循环暂不推导上界
    int64_t n = 0;
    if (sym == "<") {
      if (stop <= start) return 0;
      n = (stop - start + delta - 1) / delta;
    } else {
      if (stop < start) return 0;
      n = (stop - start) / delta + 1;
    }
    return static_cast<int>(n);
  }
  // 含无法静态定界的循环时置 true：这个 workflow 的上界从此是「未知」。
  const Node* module_ = nullptr;
  const Node* policy_ = nullptr;
 public:
  void set_module(const Node* m) { module_ = m; }
 private:
  bool unbounded_ = false;
  std::vector<std::string> unbounded_vars_;
  int stmtNodes_ = 0;
  // 函数表：name → FunctionDecl。裸调用 `greet(...)` 靠它解析成 call_fn。
  // 指针指向 codegen_functions 栈上的 map，生命周期覆盖本次 Gen 的全部调用。
  const std::map<std::string, const Node*>* fns_ = nullptr;
  const std::map<std::string, bool>* fn_recursive_ = nullptr;
  // 内联时形参名 → 实参节点 id。函数体是纯表达式，直接复用实参节点；
  // 引用多次会重新求值，所以实参必须已确认是纯的（见调用点检查）。
  std::map<std::string, int> inline_alias_;
  const Node* err_node_ = nullptr;
  std::string cur_fn_;   // 正在编的函数名（空 = 正在编 workflow）
  std::map<std::string, std::vector<std::string>> calls_;
  bool in_fn_ = false;
  // 函数参数的静态类型。工作流的 vars[] 一律写 json（历史行为），
  // 函数的 params 有真实类型，vars[] 里不该退化成 json。
  std::map<std::string, JVal> var_types_;
  std::string* err_;
};

JVal Gen::type_of(const Node* t) {
  JVal v = JVal::object();
  if (!t || !t->text) {
    v.set("k", JVal::string("json"));
    return v;
  }
  v.set("k", JVal::string(t->text));
  return v;
}

// is_builtin_type 是类型表里的 6 个基础类型（IR.md §3.1）。
// 不在表里的就是本 unit 内的 struct 引用 —— 也可能是拼错的类型名，
// 但那属于类型检查（T11）的职责，codegen 不重复判。
bool is_builtin_type(const std::string& t) {
  return t == "string" || t == "int" || t == "float" || t == "bool" ||
         t == "json" || t == "cost";
}

JVal Gen::fn_type_expr(const Node* t) {
  JVal v = JVal::object();
  if (!t || !t->text) {
    v.set("k", JVal::string("json"));
    return v;
  }
  const std::string name = t->text;
  if (is_builtin_type(name)) {
    v.set("k", JVal::string(name));
  } else {
    // 名字先留着，id 由装配阶段分配（与 codegen_types 的 ref 同一条分工）。
    v.set("k", JVal::string("struct"));
    v.set("ref", JVal::string(name));
  }
  // 数组维度记在 Type 节点的 i64 里（parse_type 填的）。忽略它，
  // `string[] tags` 会静默变成 `string` —— 与 codegen_types 同一条教训。
  for (int d = 0; d < static_cast<int>(t->i64); ++d) {
    JVal wrap = JVal::object();
    wrap.set("k", JVal::string("array"));
    wrap.set("of", std::move(v));
    v = std::move(wrap);
  }
  return v;
}

std::string Gen::lit_text(const Node* e) {
  if (!e) return "";
  if (e->n_subs > 0) {
    std::string out;
    for (int i = 0; i < e->n_subs; ++i) {
      if (e->subs[i].is_path) return "";  // 插值不是常量，不折叠
      if (e->subs[i].text) out += e->subs[i].text;
    }
    return out;
  }
  return e->text ? e->text : "";
}


// host_of 从 URL 里取主机名：`https://api.example.com/x` → `api.example.com`。
// 只处理字面量 URL；带插值的 URL 返回空串（那种只能在运行期判定）。
static std::string host_of(const std::string& url) {
  std::size_t p = url.find("://");
  if (p == std::string::npos) return "";
  std::size_t b = p + 3;
  std::size_t e = url.find_first_of("/?#", b);
  std::string host = url.substr(b, e == std::string::npos ? std::string::npos : e - b);
  std::size_t at = host.rfind('@');
  if (at != std::string::npos) host = host.substr(at + 1);
  std::size_t colon = host.find(':');
  if (colon != std::string::npos) host = host.substr(0, colon);
  return host;
}

// lit_text_of 取字符串字面量的文本；含插值时返回空串（那不是一个常量）。
static std::string lit_text_of(const Node* v) {
  if (!v || v->kind != Kind::LitExpr) return "";
  if (v->n_subs != 1) return "";
  if (v->subs[0].is_path) return "";
  return v->subs[0].text ? v->subs[0].text : "";
}

std::vector<const Node*> Gen::children_of(const Node* n) {
  std::vector<const Node*> out;
  // 顺序必须与 ast_dump 的遍历一致：child → type_args → args → else_branch。
  // 漏了 type_args，`model.generate<Intent>` 的类型实参就看不见 —— 于是 schema
  // 静默变成 null，约束整条丢掉，而 IR 依然合法。
  for (const Node* c = n->child; c; c = c->next) out.push_back(c);
  for (const Node* c = n->type_args; c; c = c->next) out.push_back(c);
  for (const Node* c = n->args; c; c = c->next) out.push_back(c);
  // `else` 有自己的字段（ast.h 的 else_branch），不在任何链上。
  // 漏了它，`if … else …` 会被编成"没有 else"的 if —— 一份能跑但语义少一半的 IR。
  if (n->else_branch) out.push_back(n->else_branch);
  return out;
}

int Gen::var_index(const std::string& name) {
  for (std::size_t i = 0; i < vars_.size(); ++i) {
    if (vars_[i] == name) return static_cast<int>(i);
  }
  vars_.push_back(name);
  return static_cast<int>(vars_.size()) - 1;
}

// gen_expr 只认三类：字面量、变量引用、插值。
// 其余一律失败 —— 见头文件最后一段的理由。
int Gen::gen_expr(const Node* e) {
  if (!e) {
    fail("表达式为空");
    return -1;
  }

  if (e->kind == Kind::LitExpr) {
    const std::string t = e->text ? e->text : "string";

    if (t == "bool") {
      // 布尔字面量的值存在 **is_nullable** 里（parser.cpp:1038 的写法），
      // 不在 i64 —— 读 i64 会永远得到 0，即"所有 true 都变成 false"，
      // 而且是一份完全合法的 IR。
      const int id = alloc("lit", e);
      nodes_[id].set("t", JVal::string("bool"));
      nodes_[id].set("v", JVal::boolean(e->is_nullable));
      return id;
    }
    if (t == "int" || t == "cost") {
      const int id = alloc("lit", e);
      nodes_[id].set("t", JVal::string(t));
      nodes_[id].set("v", JVal::integer(e->i64));
      return id;
    }

    // 字符串：n_subs == 0 时 text 就是内容（不是插值）；否则 subs 是片段，
    // 由 StringPartNode::is_path 区分"字面文本"与"字段路径"。
    if (e->n_subs == 0) {
      const int id = alloc("lit", e);
      nodes_[id].set("t", JVal::string("string"));
      nodes_[id].set("v", JVal::string(e->text ? e->text : ""));
      return id;
    }

    bool has_path = false;
    for (int i = 0; i < e->n_subs; ++i) {
      if (e->subs[i].is_path) has_path = true;
    }

    if (!has_path) {
      // 全是字面片段：拼成一个 lit，而不是留一个只有一个元素的 interp。
      std::string text;
      for (int i = 0; i < e->n_subs; ++i) {
        if (e->subs[i].text) text += e->subs[i].text;
      }
      const int id = alloc("lit", e);
      nodes_[id].set("t", JVal::string("string"));
      nodes_[id].set("v", JVal::string(text));
      return id;
    }

    const int id = alloc("interp", e);
    JVal parts = JVal::array();
    for (int i = 0; i < e->n_subs; ++i) {
      JVal p = JVal::object();
      if (e->subs[i].is_path) {
        JVal path = JVal::array();
        // path 文本是点分形式（"input.text"），拆成路径段。
        std::string cur;
        const std::string s = e->subs[i].text ? e->subs[i].text : "";
        // 内联形参出现在插值里是**无法静态展开**的：interp 存的是路径字符串，
        // 运行期按名字在当前作用域找变量，而内联没有为形参创建临时变量。
        // 编出来会通过语法检查、运行期报"变量不存在"——所以在这里拒绝。
        std::string firstSeg;
        for (const char ch : s) {
          if (ch == '.') break;
          firstSeg.push_back(ch);
        }
        if (inline_alias_.count(firstSeg) != 0) {
          fail_at(e, "parameter `" + firstSeg + "` appears in an interpolated string " +
                     "inside an inlined function: compiled mode creates no temporary " +
                     "variable for it; rewrite it as string concatenation with `+`");
          return -1;
        }
        for (char c : s) {
          if (c == '.') {
            if (!cur.empty()) path.push(JVal::string(cur));
            cur.clear();
          } else {
            cur.push_back(c);
          }
        }
        if (!cur.empty()) path.push(JVal::string(cur));
        p.set("path", std::move(path));
      } else {
        p.set("lit", JVal::string(e->subs[i].text ? e->subs[i].text : ""));
      }
      parts.push(std::move(p));
    }
    nodes_[id].set("parts", std::move(parts));
    return id;
  }

  if (e->kind == Kind::IndexExpr) {
    // 两种下标：`j["k"]`（json 常量键，节点 index）与 `xs[0]`（数组下标，节点 at）。
    const int of = gen_expr(e->child);
    if (of < 0) return -1;
    if (e->text != nullptr) {
      const int id = alloc("index", e);
      nodes_[id].set("of", JVal::integer(of));
      nodes_[id].set("key", JVal::string(e->text));
      span_[id] = span_[of];
      model_[id] = model_[of];
      tool_[id] = tool_[of];
      return id;
    }
    // 编译期越界检查：长度已知（字面量数组，或由字面量绑定的变量）且下标是整数字面量。
    if (e->args && e->args->kind == Kind::LitExpr && e->args->text &&
        std::string(e->args->text) == "int") {
      int n = -1;
      if (e->child && e->child->kind == Kind::ArrayLit) {
        n = 0;
        for (const Node* c = e->child->child; c; c = c->next) n++;
      } else if (e->child && e->child->kind == Kind::PathExpr && e->child->n_subs == 1 &&
                 e->child->subs[0].text) {
        auto it = arr_len_.find(e->child->subs[0].text);
        if (it != arr_len_.end()) n = it->second;
      }
      if (n >= 0 && (e->args->i64 < 0 || e->args->i64 >= n)) {
        fail("数组下标越界：长度 " + std::to_string(n) + "，下标 " +
             std::to_string((long long)e->args->i64));
        return -1;
      }
    }
    const int idx = gen_expr(e->args);
    if (idx < 0) return -1;
    const int id = alloc("at", e);
    nodes_[id].set("of", JVal::integer(of));
    nodes_[id].set("i", JVal::integer(idx));
    span_[id] = span_[of] + span_[idx];
    model_[id] = model_[of] + model_[idx];
    tool_[id] = tool_[of] + tool_[idx];
    return id;
  }

  if (e->kind == Kind::ArrayLit) {
    // `[a, b, c]` → array_lit 节点，元素按顺序是 kids。
    // 定长：元素个数就是长度，`len()` 因此可以在编译期折成常量。
    const int id = alloc("array_lit", e);
    JVal kids = JVal::array();
    for (const Node* c = e->child; c; c = c->next) {
      const int k = gen_expr(c);
      if (k < 0) return -1;
      kids.push(JVal::integer(k));
      span_[id] += span_[k];
      model_[id] += model_[k];
      tool_[id] += tool_[k];
    }
    nodes_[id].set("kids", std::move(kids));
    ++stmtNodes_;
    return id;
  }

  // 变量引用：PathExpr 包一个 IdentExpr，或直接 IdentExpr。
  // 变量引用：`PathExpr` 包一个 `IdentExpr`，或直接 `IdentExpr`。
  // 两个槽位都要看：PathExpr 的子节点可能挂在 child，也可能挂在 args ——
  // 字段归属这件事**必须 dump 确认，不能猜**（HANDOFF §4.4 已踩过四次）。
  const Node* ident = nullptr;
  if (e->kind == Kind::IdentExpr) {
    ident = e;
  } else if (e->kind == Kind::PathExpr) {
    // PathExpr 的路径段在 **subs**（dump 里显示为子节点，但字段是 subs）。
    // 单段 = 裸变量；多段是字段访问（`a.b`），这一刀不支持，明确报错。
    if (e->n_subs == 1 && e->subs[0].kind == Kind::IdentExpr) {
      ident = &e->subs[0];
    } else if (e->n_subs > 1) {
      // 字段访问链 `a.b.c` → 嵌套 field 节点。编号按"父先于子"：
      // 外层 field 先分配，再递归它的 of。
      return gen_path_(e);
    }
  }
  if (ident) {
    const std::string nm = ident->text ? ident->text : "";
    auto al = inline_alias_.find(nm);
    if (al != inline_alias_.end()) {
      // 内联形参：直接复用实参节点（实参已在调用点验证为纯表达式）。
      return al->second;
    }
    const int id = alloc("var", e);
    nodes_[id].set("var", JVal::integer(var_index(nm)));
    return id;
  }

  if (e->kind == Kind::ParallelExpr) {
    // 编号规则：先 alloc 父，再按 branches 左→右递归（IR.md §4.2）。
    // `ParallelReturn` 不是分支，它是"join 出几个值"的声明 —— 它的子节点数就是 join_k。
    const int id = alloc("parallel", e);
    ++stmtNodes_;
    JVal branches = JVal::array();
    std::vector<int> branchIds;
    std::vector<int> joinIds;
    int join = 0;
    // 两个槽位都要走：`ParallelReturn` 可能挂在 child 链上，也可能挂在 args 上
    // （字段归属必须 dump 确认，HANDOFF §4.4 已踩过四次）。
    const Node* slots[2] = {e->child, e->args};
    for (const Node* head : slots) {
      for (const Node* c = head; c; c = c->next) {
        if (c->kind == Kind::ParallelReturn) {
          // 被 join 的表达式在 **subs**（parser.cpp:899 用 to_subs 建），
          // 不在 child 链上 —— 读 child 会得到 0，而 0 个 join 值是一份
          // "看起来合法但没有返回值"的 parallel。
          //
          // 每个 join 表达式都要编成节点并记下 id（D-031）：只记元数的话，
          // 执行器知道"两元"却不知道该装哪两个值。
          for (int i = 0; i < c->n_subs; ++i) {
            const int jid = gen_expr(&c->subs[i]);
            if (jid < 0) return -1;
            joinIds.push_back(jid);
          }
          join += c->n_subs;
          continue;
        }
        const int b = gen_stmt(c);
        if (b < 0) return -1;
        branches.push(JVal::integer(b));
        branchIds.push_back(b);
      }
    }
    if (join == 0) {
      fail("parallel 没有 return [..]：join 出几个值必须显式写出来");
      return -1;
    }
    nodes_[id].set("branches", std::move(branches));
    {
      JVal jv = JVal::array();
      for (const int jid : joinIds) jv.push(JVal::integer(jid));
      nodes_[id].set("join", std::move(jv));
    }
    nodes_[id].set("join_k", JVal::integer(join));
    nodes_[id].set("max_par", JVal::integer(join));
    span_[id] = 1;  // 父 span
    for (const int bid : branchIds) {
      span_[id] += span_[bid];
      model_[id] += model_[bid];
      tool_[id] += tool_[bid];
    }
    if (join > max_conc_) max_conc_ = join;
    return id;
  }

  if (e->kind == Kind::CallExpr) {
    const std::vector<const Node*> k = children_of(e);
    const Node* path = nullptr;
    const Node* typeArg = nullptr;
    std::vector<const Node*> params;
    for (const Node* c : k) {
      if (c->kind == Kind::PathExpr && !path) path = c;
      else if (c->kind == Kind::Type && !typeArg) typeArg = c;
      else if (c->kind == Kind::Param) params.push_back(c);
    }
    if (!path || path->n_subs < 1) {
      fail("调用缺少被调者");
      return -1;
    }
    // 单段路径 = 裸函数调用（`len(x)`），ns 为空、fn 是函数名。
    const std::string ns = path->n_subs >= 2
                               ? (path->subs[0].text ? path->subs[0].text : "")
                               : "";
    const std::string fn = path->n_subs >= 2
                               ? (path->subs[1].text ? path->subs[1].text : "")
                               : (path->subs[0].text ? path->subs[0].text : "");

    // ---- len(x)：定长数组与字符串的长度 ----
    if (ns.empty() && fn == "len") {
      if (params.size() != 1) { fail("len() 需要且只需要一个参数"); return -1; }
      const std::vector<const Node*> vals = children_of(params[0]);
      if (vals.empty()) { fail("len() 缺少参数表达式"); return -1; }
      const int inner = gen_expr(vals[0]);
      if (inner < 0) return -1;
      const int id = alloc("len", e);
      nodes_[id].set("e", JVal::integer(inner));
      span_[id] = span_[inner];
      model_[id] = model_[inner];
      tool_[id] = tool_[inner];
      return id;
    }

    // ---- 用户函数 ----
    //
    // 非递归函数在**编译期展开**（Inline）：调用点直接变成函数体的表达式，
    // 运行期不存在任何动态派发。递归函数保留 call_fn，由深度上限 512 兜底 ——
    // 那是唯一无法静态展开的情况。
    if (ns.empty() && fns_ && fns_->count(fn) != 0) {
      const Node* target = fns_->at(fn);
      int want = 0;
      for (const Node* p = target->child; p; p = p->next) {
        if (p->kind == Kind::Param) ++want;
      }
      if (static_cast<int>(params.size()) != want) {
        fail_at(e, "function " + fn + " takes " + std::to_string(want) +
                   " argument(s), got " + std::to_string(params.size()));
        return -1;
      }
      // 分析阶段（fn_recursive_ == nullptr）只记调用边，不内联：否则
      // "哪些函数递归"的判定会依赖它自己。
      bool recursive = true;
      if (fn_recursive_ != nullptr) {
        auto it = fn_recursive_->find(fn);
        recursive = (it == fn_recursive_->end()) ? true : it->second;
      }

      if (!recursive) {
        // ---- 编译期展开：函数体必须是单一 `return <expr>;` ----
        const Node* body = target->body;
        const Node* only = (body && body->kind == Kind::Block) ? body->child : nullptr;
        const Node* retExpr = nullptr;
        if (only != nullptr && only->next == nullptr && only->kind == Kind::ReturnStmt) {
          retExpr = only->child;
        }
        if (retExpr == nullptr) {
          fail_at(e, "function `" + fn + "` cannot be inlined at compile time: compiled mode "
                     "requires a single `return <expr>;` body (statements or loops would need "
                     "a call frame, and non-recursive functions never get dynamic dispatch)");
          return -1;
        }
        std::vector<int> argIds;
        for (const Node* p : params) {
          if (p->text) {
            fail_at(e, "user functions take positional arguments only: call `" + fn +
                       "(...)` without `" + std::string(p->text) + " =`");
            return -1;
          }
          const std::vector<const Node*> vals = children_of(p);
          if (vals.empty()) {
            fail_at(e, "call to " + fn + " has an argument without an expression");
            return -1;
          }
          const int a = gen_expr(vals[0]);
          if (a < 0) return -1;
          // 内联后形参直接复用实参节点；引用多次会重新求值。带副作用的实参
          // （模型/工具/递归调用）必须先绑定到变量，否则会被执行多次。
          if (span_[a] != 0 || model_[a] != 0 || tool_[a] != 0) {
            fail_at(e, "argument " + std::to_string(argIds.size() + 1) + " to `" + fn +
                       "` is not pure: the inlined parameter may be evaluated more than once; "
                       "bind it to a variable first");
            return -1;
          }
          argIds.push_back(a);
        }
        std::vector<std::string> paramNames;
        for (const Node* p = target->child; p; p = p->next) {
          if (p->kind == Kind::Param) paramNames.push_back(p->text ? p->text : "");
        }
        std::map<std::string, int> saved = inline_alias_;
        inline_alias_.clear();
        for (std::size_t i = 0; i < paramNames.size(); ++i) {
          inline_alias_[paramNames[i]] = argIds[i];
        }
        const int inlined = gen_expr(retExpr);
        inline_alias_ = saved;
        return inlined;
      }

      // ---- 递归：保留 call_fn（唯一的动态路径，深度上限 512）----
      const int id = alloc("call_fn", e);
      ++stmtNodes_;
      std::vector<int> argIds;
      for (const Node* p : params) {
        if (p->text) {
          fail_at(e, "user functions take positional arguments only: call `" + fn +
                     "(...)` without `" + std::string(p->text) + " =`");
          return -1;
        }
        const std::vector<const Node*> vals = children_of(p);
        if (vals.empty()) {
          fail_at(e, "call to " + fn + " has an argument without an expression");
          return -1;
        }
        const int a = gen_expr(vals[0]);
        if (a < 0) return -1;
        argIds.push_back(a);
      }
      JVal args = JVal::array();
      for (const int a : argIds) args.push(JVal::integer(a));
      nodes_[id].set("fn", JVal::string(fn));
      nodes_[id].set("args", std::move(args));
      // 递归调用的次数不可静态定界：整个 workflow 标 unbounded。
      span_[id] = 1;
      unbounded_ = true;
      if (!cur_fn_.empty()) calls_[cur_fn_].push_back(fn);
      return id;
    }

    // `json.as_int/as_string/as_bool/as_float/j(o)` —— 语言里唯一能引入
    // 运行时类型错误的地方（LANGUAGE §5.6）。IR 里是 `cast` 节点（schema 早已定义），
    // 而 codegen/engine 之前都没实现：写出来就是"不支持的构造"，不是静默降级。
    if (ns == "json" && fn.rfind("as_", 0) == 0) {
      const std::string to = fn.substr(3);  // int / string / bool / float
      if (to != "int" && to != "string" && to != "bool" && to != "float") {
        fail("不认识的 json 窄化: json." + fn);
        return -1;
      }
      if (params.size() != 1) {
        fail("json." + fn + " 需要且只需要一个参数");
        return -1;
      }
      const std::vector<const Node*> vals = children_of(params[0]);
      if (vals.empty()) {
        fail("json." + fn + " 缺少参数表达式");
        return -1;
      }
      const int inner = gen_expr(vals[0]);
      if (inner < 0) return -1;
      const int id = alloc("cast", e);
      JVal t = JVal::object();
      t.set("k", JVal::string(to));
      nodes_[id].set("to", std::move(t));
      nodes_[id].set("e", JVal::integer(inner));
      span_[id] = span_[inner];
      model_[id] = model_[inner];
      tool_[id] = tool_[inner];
      return id;
    }
    if (ns == "model" && fn == "agent") {
      if (in_fn_) {
        fail("external calls (model.agent) are not supported inside a function yet: their call counts must reach workflow bounds, and inlining is not implemented");
        return -1;
      }
      // `model.agent(goal = "…", tools = ["orders"], max_steps = 4)`（D-034）。
      // 复用 CallExpr 的解析，不新增关键字 —— 与 model.generate 同一条路径。
      const int id = alloc("agent", e);
      int goal = -1;
      int maxSteps = 0;
      int maxParTools = 0;
      std::vector<std::string> toolNames;
      for (const Node* p : params) {
        const std::string name = p->text ? p->text : "";
        const std::vector<const Node*> vals = children_of(p);
        const Node* val = vals.empty() ? nullptr : vals[0];
        if (name == "goal") {
          if (!val) { fail("model.agent 的 goal 需要表达式"); return -1; }
          goal = gen_expr(val);
          if (goal < 0) return -1;
        } else if (name == "max_steps") {
          if (!val || val->kind != Kind::LitExpr || !val->text ||
              std::string(val->text) != "int") {
            fail("model.agent 的 max_steps 必须是整数字面量（上界要能在编译期算出来）");
            return -1;
          }
          maxSteps = static_cast<int>(val->i64);
        } else if (name == "max_parallel_tools") {
          if (!val || val->kind != Kind::LitExpr || !val->text ||
              std::string(val->text) != "int") {
            fail("model.agent 的 max_parallel_tools 必须是整数字面量");
            return -1;
          }
          maxParTools = static_cast<int>(val->i64);
        } else if (name == "tools") {
          if (!val || val->kind != Kind::ArrayLit) {
            fail("model.agent 的 tools 必须是字符串数组，如 [\"orders\"]");
            return -1;
          }
          for (const Node* el = val->child; el; el = el->next) {
            const std::string lit = (el->kind == Kind::LitExpr) ? lit_text(el) : "";
            if (lit.empty()) {
              fail("model.agent 的 tools 元素必须是字符串字面量");
              return -1;
            }
            if (policy_) {
              const Node* allowRule = nullptr;
              const Node* denyRule = nullptr;
              for (const Node* r = policy_->child; r; r = r->next) {
                if (r->kind != Kind::SecurityRule || !r->text) continue;
                if (strcmp(r->text, "tool.call") != 0) continue;
                if (r->is_export) allowRule = r; else denyRule = r;
              }
              for (const Node* v = denyRule ? denyRule->child : nullptr; v; v = v->next) {
                if (v->text && lit == v->text) {
                  fail("policy " + std::string(policy_->text ? policy_->text : "") +
                       " is not allowed to use tool " + lit + " (deny tool.call)");
                  return -1;
                }
              }
              if (allowRule) {
                bool listed = false;
                for (const Node* v = allowRule->child; v; v = v->next) {
                  if (v->text && lit == v->text) { listed = true; break; }
                }
                if (!listed) {
                  fail("policy " + std::string(policy_->text ? policy_->text : "") +
                       " does not allow tool " + lit + " (not in allow tool.call)");
                  return -1;
                }
              }
            }
            // ---- 策略：出网域名（编译期，仅字面量 URL）----
            if (policy_) {
              const Node* allowNet = nullptr;
              const Node* denyNet = nullptr;
              for (const Node* r = policy_->child; r; r = r->next) {
                if (r->kind != Kind::SecurityRule || !r->text) continue;
                if (strcmp(r->text, "network.outbound") != 0) continue;
                if (r->is_export) allowNet = r; else denyNet = r;
              }
              if (allowNet || denyNet) {
                // 找这个工具的声明，取它的 url 字面量。
                std::string url;
                for (const Node* d = module_ ? module_->child : nullptr; d; d = d->next) {
                  if (d->kind != Kind::ToolDecl || !d->text || lit != d->text) continue;
                  for (const Node* f = d->child; f; f = f->next) {
                    if (f->text && strcmp(f->text, "url") == 0 && f->child) {
                      url = lit_text_of(f->child);
                    }
                  }
                  break;
                }
                const std::string host = host_of(url);
                if (!host.empty()) {
                  for (const Node* v = denyNet ? denyNet->child : nullptr; v; v = v->next) {
                    if (v->text && host == v->text) {
                      fail("policy " + std::string(policy_->text ? policy_->text : "") +
                           " forbids egress to " + host + " (deny network.outbound)");
                      return -1;
                    }
                  }
                  if (allowNet) {
                    bool okHost = false;
                    for (const Node* v = allowNet->child; v; v = v->next) {
                      if (!v->text) continue;
                      const std::string allow = v->text;
                      // 允许写子域后缀：`example.com` 匹配 `api.example.com`。
                      if (host == allow ||
                          (host.size() > allow.size() &&
                           host.compare(host.size() - allow.size(), allow.size(), allow) == 0 &&
                           host[host.size() - allow.size() - 1] == '.')) {
                        okHost = true; break;
                      }
                    }
                    if (!okHost) {
                      fail("policy " + std::string(policy_->text ? policy_->text : "") +
                           " does not allow egress to " + host + " (not in allow network.outbound)");
                      return -1;
                    }
                  }
                }
              }
            }
            toolNames.push_back(lit);
          }
        } else {
          fail("model.agent 不认识参数 " + name);
          return -1;
        }
      }
      if (goal < 0) { fail("model.agent 缺少 goal"); return -1; }
      if (maxSteps <= 0) {
        fail("model.agent 的 max_steps 必须 > 0（它是循环上界，也是预算预留的依据）");
        return -1;
      }
      JVal spec = JVal::object();
      spec.set("goal", JVal::integer(goal));
      JVal toolsArr = JVal::array();
      for (const auto& t : toolNames) toolsArr.push(JVal::string(t));
      spec.set("tools", std::move(toolsArr));
      spec.set("max_steps", JVal::integer(maxSteps));
      if (maxParTools > 0) spec.set("max_parallel_tools", JVal::integer(maxParTools));
      nodes_[id].set("agent", std::move(spec));
      // 上界：每步最多一次模型 + 一次工具。
      const int k = maxParTools > 0 ? maxParTools : 1;
      span_[id] = (1 + k) * maxSteps;
      model_[id] = maxSteps;
      tool_[id] = k * maxSteps;
      return id;
    }

    // ---- memory.* / vector.*（存储子系统）----
    //
    // 走 CallExpr 而不是新关键字：语言的关键字数量是成本，而这两个命名空间
    // 本来就是"保留命名空间"清单里可以扩充的位置。
    if ((ns == "memory" || ns == "vector")) {
      if (in_fn_) {
        fail("external calls (memory/vector) are not supported inside a function yet: their call counts must reach workflow bounds, and inlining is not implemented");
        return -1;
      }
      const std::string op = fn;
      bool ok =
          (ns == "memory" && (op == "get" || op == "put" || op == "delete")) ||
          (ns == "vector" && (op == "put" || op == "query" || op == "delete"));
      if (!ok) {
        fail("不认识的存储内建 " + ns + "." + op +
             "（memory.get / memory.put / memory.delete / vector.put / vector.query / vector.delete）");
        return -1;
      }
      const int id = alloc("store_op", e);
      ++stmtNodes_;
      nodes_[id].set("op", JVal::string(ns + "." + op));
      JVal args = JVal::object();
      bool haveKey = false, haveValue = false;
      for (const Node* p : params) {
        const std::string name = p->text ? p->text : "";
        const std::vector<const Node*> vals = children_of(p);
        const Node* val = vals.empty() ? nullptr : vals[0];
        if (val == nullptr) { fail("存储内建的参数 " + name + " 没有值"); return -1; }
        if (name == "key" || name == "id" || name == "text") {
          const int v = gen_expr(val);
          if (v < 0) return -1;
          args.set(name.c_str(), JVal::integer(v));
          if (name == "key" || name == "id") haveKey = true;
          if (name == "text") haveValue = true;
        } else if (name == "value") {
          const int v = gen_expr(val);
          if (v < 0) return -1;
          args.set("value", JVal::integer(v));
          haveValue = true;
        } else if (name == "k") {
          if (!val || !val->text || std::string(val->text) != "int") {
            fail("vector.query 的 k 必须是整数字面量");
            return -1;
          }
          args.set("k", JVal::integer(val->i64));
        } else {
          fail("存储内建不支持参数 " + name);
          return -1;
        }
      }
      // 每个内建需要的参数不同：memory 要 key，vector.put 要 id + text，
      // vector.query 只要 text。一刀切要求 key 会把 query 判死。
      const bool needKey = (ns == "memory") || (ns == "vector" && op == "put");
      if (needKey && !haveKey) {
        fail(ns + "." + op + (op == "put" && ns == "vector" ? " 缺少 id" : " 缺少 key"));
        return -1;
      }
      if (ns == "memory" && op == "put" && !haveValue) { fail("memory.put 缺少 value"); return -1; }
      if (ns == "vector" && !haveValue) { fail("vector." + op + " 缺少 text"); return -1; }
      nodes_[id].set("args", std::move(args));
      // 账：一次存储操作 = 一个 span。带向量的那两种要调 embeddings，算一次模型调用。
      span_[id] = 1;
      model_[id] = (op == "put" && ns == "vector") || (op == "query") ? 1 : 0;
      tool_[id] = 0;
      return id;
    }

    if (ns == "json" && fn == "get") {
      fail("json.get 尚未实现（用 json.as_* 窄化后再取字段）");
      return -1;
    }

    if (ns == "model" && fn == "generate") {
      if (in_fn_) {
        fail("external calls (model.generate) are not supported inside a function yet: their call counts must reach workflow bounds, and inlining is not implemented");
        return -1;
      }
      const int id = alloc("call_model", e);
      ++stmtNodes_;

      int prompt = -1;
      std::string precision = "exact";
      int maxTokens = -1;
      std::string temperature;
      for (const Node* p : params) {
        const std::string name = p->text ? p->text : "";
        // Param 的值也可能挂在 args 而不是 child 上（第 N 次遇到同一个坑）。
        const std::vector<const Node*> vals = children_of(p);
        const Node* val = vals.empty() ? nullptr : vals[0];
        if (name == "prompt") {
          prompt = gen_expr(val);
          if (prompt < 0) return -1;
        } else if (name == "precision") {
          precision = lit_text(val);
        } else if (name == "max_tokens") {
          if (val && val->i64) maxTokens = static_cast<int>(val->i64);
        } else if (name == "temperature") {
          temperature = lit_text(val);
        } else {
          fail("model.generate 不支持参数 " + name);
          return -1;
        }
      }
      if (prompt < 0) {
        fail("model.generate 缺少 prompt");
        return -1;
      }

      JVal args = JVal::object();
      args.set("prompt", JVal::integer(prompt));
      args.set("precision", JVal::string(precision));
      args.set("max_tokens", JVal::integer(maxTokens));
      args.set("temperature", JVal::string(temperature));
      nodes_[id].set("args", std::move(args));
      // schema 用**名字**引用，下标由装配阶段分配（与 struct id 同一条分工）。
      if (typeArg && typeArg->text) {
        nodes_[id].set("schema_ref", JVal::string(typeArg->text));
      } else {
        nodes_[id].set("schema_ref", JVal::null());
      }
      nodes_[id].set("attempt_ctx", JVal::null());
      span_[id] = 1;
      model_[id] = 1;  // 一次 call_model = 一次模型调用
      return id;
    }

    if (ns == "tool" && fn == "call") {
      if (in_fn_) {
        fail("external calls (tool.call) are not supported inside a function yet: their call counts must reach workflow bounds, and inlining is not implemented");
        return -1;
      }
      const int id = alloc("call_tool", e);
      ++stmtNodes_;

      const Node* urlVal = nullptr;
      std::string method = "GET";
      int timeoutMs = 0;
      bool hasIdemKey = false;
      bool idempotent = false;
      std::vector<int> allowStatus;
      int headerNode = -1;
      int bodyNode = -1;

      for (const Node* p : params) {
        const std::string name = p->text ? p->text : "";
        const std::vector<const Node*> vals = children_of(p);
        const Node* val = vals.empty() ? nullptr : vals[0];
        if (name == "url") {
          urlVal = val;
        } else if (name == "method") {
          method = lit_text(val);
        } else if (name == "timeout") {
          if (val) timeoutMs = static_cast<int>(val->i64);
        } else if (name == "idempotent") {
          idempotent = val && val->is_nullable;
        } else if (name == "idem_key") {
          hasIdemKey = true;
        } else if (name == "allow_status") {
          // 数组字面量，元素必须是整数常量（LANGUAGE §5.6）。非整数就明确报错：
          // 静默丢掉一个状态码会让"这个响应算正常还是异常"在运行期随机变化。
          if (!val || val->kind != Kind::ArrayLit) {
            fail("allow_status 必须是数组字面量，如 [404, 409]");
            return -1;
          }
          for (const Node* e = val->child; e; e = e->next) {
            if (e->kind != Kind::LitExpr || !e->text || std::string(e->text) != "int") {
              fail("allow_status 的元素必须是整数字面量");
              return -1;
            }
            allowStatus.push_back(static_cast<int>(e->i64));
          }
        } else if (name == "header" || name == "body") {
          // 值是**表达式**（通常是变量或字段访问），在运行期求值 ——
          // 请求头/请求体往往由上游算出来，不是常量。
          if (!val) {
            fail("tool.call 的 " + name + " 需要一个表达式");
            return -1;
          }
          const int sub = gen_expr(val);
          if (sub < 0) return -1;
          if (name == "header") headerNode = sub; else bodyNode = sub;
        } else {
          fail("tool.call 不支持参数 " + name);
          return -1;
        }
      }
      if (!urlVal) {
        fail("tool.call 缺少 url");
        return -1;
      }

      const int urlNode = gen_expr(urlVal);
      if (urlNode < 0) return -1;

      // url_template 是**结构化片段**（IR.md §3.4）：字面文本 or 已解析的字段路径。
      // 运行期不该再解析一次模板；审计者也要能直接看出这个 URL 依赖了哪些字段。
      JVal tmpl = JVal::array();
      if (urlVal->n_subs == 0) {
        JVal part = JVal::object();
        part.set("lit", JVal::string(urlVal->text ? urlVal->text : ""));
        tmpl.push(std::move(part));
      } else {
        for (int i = 0; i < urlVal->n_subs; ++i) {
          const Node* sp = &urlVal->subs[i];
          JVal part = JVal::object();
          if (sp->is_path) {
            JVal path = JVal::array();
            std::string cur;
            const std::string seg = sp->text ? sp->text : "";
            for (char ch : seg) {
              if (ch == '.') {
                if (!cur.empty()) path.push(JVal::string(cur));
                cur.clear();
              } else {
                cur.push_back(ch);
              }
            }
            if (!cur.empty()) path.push(JVal::string(cur));
            part.set("path", std::move(path));
          } else {
            part.set("lit", JVal::string(sp->text ? sp->text : ""));
          }
          tmpl.push(std::move(part));
        }
      }

      // 工具名是合成的（D-029）：语言里没有 tool 声明，而 tools[] 需要一个键。
      char tname[64];
      std::snprintf(tname, sizeof(tname), "tool@0:%d:%d", e->loc.line, e->loc.col);

      // idem.mode 是编译期事实（LANGUAGE §5.7）：只读方法天然幂等；
      // 显式 idempotent=true 次之；给了 idem_key 则按 key。
      const char* idemMode = "readonly";
      if (method != "GET" && method != "HEAD") {
        if (hasIdemKey) {
          idemMode = "idem_key";
        } else if (idempotent) {
          idemMode = "idem_true";
        }
      }

      JVal decl = JVal::object();
      decl.set("name", JVal::string(tname));
      decl.set("method", JVal::string(method));
      decl.set("url_template", std::move(tmpl));
      decl.set("timeout_ms", JVal::integer(timeoutMs));
      decl.set("allow_status", JVal::array());
      JVal idem = JVal::object();
      idem.set("mode", JVal::string(idemMode));
      decl.set("idem", std::move(idem));
      JVal ld = JVal::object();
      ld.set("m", JVal::integer(0));
      ld.set("l", JVal::integer(e->loc.line));
      ld.set("c", JVal::integer(e->loc.col));
      ld.set("el", JVal::integer(e->loc.end_line));
      ld.set("ec", JVal::integer(e->loc.end_col));
      decl.set("decl_loc", std::move(ld));

      JVal allowArr = JVal::array();
      for (const int c : allowStatus) allowArr.push(JVal::integer(c));

      JVal args = JVal::object();
      args.set("url", JVal::integer(urlNode));
      args.set("method", JVal::string(method));
      args.set("timeout_ms", JVal::integer(timeoutMs));
      args.set("allow_status", std::move(allowArr));
      if (headerNode >= 0) {
        args.set("header", JVal::integer(headerNode));
      } else {
        args.set("header", JVal::null());
      }
      if (bodyNode >= 0) {
        args.set("body", JVal::integer(bodyNode));
      } else {
        args.set("body", JVal::null());
      }
      args.set("idem_key", JVal::null());

      nodes_[id].set("tool", JVal::string(tname));
      nodes_[id].set("args", std::move(args));
      // 返回类型：`<T>` 或 json；**allow_status 非空时包一层 httpresult**（§5.6）。
      // 这不是装饰：没有 allow_status 时非 2xx 抛 HttpError、不返回值；
      // 有它时列表内的状态码作为正常返回，调用方才能写 if (r.status == 404)。
      {
        JVal base = JVal::object();
        base.set("k", JVal::string(typeArg && typeArg->text ? typeArg->text : "json"));
        JVal rt = std::move(base);
        if (!allowStatus.empty()) {
          JVal wrap = JVal::object();
          wrap.set("k", JVal::string("httpresult"));
          wrap.set("of", std::move(rt));
          rt = std::move(wrap);
        }
        nodes_[id].set("ret", std::move(rt));
      }
      nodes_[id].set("attempt_ctx", JVal::null());
      nodes_[id].set("tool_decl", std::move(decl));
      span_[id] = 1;
      tool_[id] = 1;
      return id;
    }

    fail("不认识的调用 " + ns + "." + fn);
    return -1;
  }

  // 二元运算：IR 里是 binop（op / l / r）。之前**完全没实现** ——
  // 于是 `if (r.status == 404)` 这种最基本的分支写不出来，而报错说的是
  // "尚不支持这种表达式"，离"比较运算没实现"很远。
  if (e->kind == Kind::BinaryExpr) {
    const int id = alloc("binop", e);
    const int l = e->child ? gen_expr(e->child) : -1;
    if (l < 0) return -1;
    const int r = e->args ? gen_expr(e->args) : -1;
    if (r < 0) return -1;
    // 语法层的符号（`==` `<=` `&&` …）映射到 IR 的算子名（eq/le/and …）。
    const std::string sym = e->text ? e->text : "";
    std::string op;
    if (sym == "==") op = "eq";
    else if (sym == "!=") op = "ne";
    else if (sym == "<") op = "lt";
    else if (sym == "<=") op = "le";
    else if (sym == ">") op = "gt";
    else if (sym == ">=") op = "ge";
    else if (sym == "&&") op = "and";
    else if (sym == "||") op = "or";
    else if (sym == "+") op = "add";
    else if (sym == "-") op = "sub";
    else if (sym == "*") op = "mul";
    else if (sym == "/") op = "div";
    else if (sym == "%") op = "mod";
    else {
      fail("不支持的二元运算符: " + sym);
      return -1;
    }
    nodes_[id].set("op", JVal::string(op));
    nodes_[id].set("l", JVal::integer(l));
    nodes_[id].set("r", JVal::integer(r));
    span_[id] = span_[l] + span_[r];
    model_[id] = model_[l] + model_[r];
    tool_[id] = tool_[l] + tool_[r];
    return id;
  }

  fail(std::string("codegen 尚不支持这种表达式（kind=") + kind_name(e->kind) +
       ", child=" + (e->child ? kind_name(e->child->kind) : "null") +
       ", args=" + (e->args ? kind_name(e->args->kind) : "null") +
       ", child->next=" + (e->child && e->child->next ? "yes" : "no") + "）");
  return -1;
}

int Gen::gen_path_(const Node* e) {
  if (e->n_subs < 2) {
    fail("路径至少要有两段");
    return -1;
  }
  const Node* head = &e->subs[0];
  if (head->kind != Kind::IdentExpr) {
    fail("路径的第一段必须是标识符");
    return -1;
  }
  int base = -1;
  const std::string headName = head->text ? head->text : "";
  auto al = inline_alias_.find(headName);
  if (al != inline_alias_.end()) {
    base = al->second;
  } else {
    base = alloc("var", &e->subs[0]);
    nodes_[base].set("var", JVal::integer(var_index(headName)));
  }

  // 逐段包 field。**父先于子**的编号顺序在这里体现为"先建本层，再把上一层
  // 当作 of"——所以要从左往右建，每层的 of 指向已建好的上一环。
  for (int i = 1; i < e->n_subs; ++i) {
    const Node* seg = &e->subs[i];
    const int f = alloc("field", seg);
    nodes_[f].set("name", JVal::string(seg->text ? seg->text : ""));
    nodes_[f].set("of", JVal::integer(base));
    span_[f] = span_[base];
    model_[f] = model_[base];
    tool_[f] = tool_[base];
    base = f;
  }
  return base;
}

int Gen::gen_stmt(const Node* s) {
  if (!s) {
    fail("语句为空");
    return -1;
  }

  switch (s->kind) {
    case Kind::Block: {
      const int id = alloc("seq", s);
      JVal kids = JVal::array();
      std::vector<int> kidsIds;
      int sum = 0;
      for (const Node* k = s->child; k; k = k->next) {
        const int kid = gen_stmt(k);
        if (kid < 0) return -1;
        kids.push(JVal::integer(kid));
        kidsIds.push_back(kid);
        sum += span_[kid];
      }
      nodes_[id].set("kids", std::move(kids));
      span_[id] = sum;
      {
        int ms = 0, ts2 = 0;
        for (const int kid : kidsIds) {
          ms += model_[kid];
          ts2 += tool_[kid];
        }
        model_[id] = ms;
        tool_[id] = ts2;
      }
      return id;
    }
    case Kind::LetStmt: {
      // 解构（`let [a,b] = …`）这一刀不支持：它需要 destructure 节点与 parallel。
      // 明确报错，而不是把第一个名字当作单绑定 —— 那会静默丢掉其余绑定。
      const Node* pat = s->child;
      if (!pat || pat->kind != Kind::Pattern) {
        fail("let 缺少 Pattern");
        return -1;
      }

      // 数一数绑了几个名字：1 个是 `let x = …`，多个是 `let [a, b] = …`。
      int names = 0;
      for (const Node* n = pat->child; n; n = n->next) ++names;
      if (names <= 0) {
        fail("let 的 Pattern 里没有名字");
        return -1;
      }

      if (names == 1) {
        const Node* name = pat->child;
        if (name->kind != Kind::IdentExpr) {
          fail("let 的绑定名不是标识符");
          return -1;
        }
        const int id = alloc("let", s);
        ++stmtNodes_;
        const int vid = var_index(name->text ? name->text : "");
        // 记录"这个变量是定长数组，长度 N"——只记字面量，别的形态不猜。
        if (s->args && s->args->kind == Kind::ArrayLit && name->text) {
          int n = 0;
          for (const Node* c = s->args->child; c; c = c->next) n++;
          arr_len_[name->text] = n;
        }
        const int e = gen_expr(s->args);
        if (e < 0) return -1;
        nodes_[id].set("var", JVal::integer(vid));
        nodes_[id].set("e", JVal::integer(e));
        // 语义反转：**默认可变**，只有 const 不可变。
        if (s->is_const) {
          nodes_[id].set("const", JVal::boolean(true));
        } else {
          nodes_[id].set("mut", JVal::boolean(true));
        }
        span_[id] = span_[e];
        model_[id] = model_[e];
        tool_[id] = tool_[e];
        return id;
      }

      // 解构：D-026 的 B 形态（codegen 只写这一种）。`_` 丢弃位写成 JSON null，
      // 不用哨兵整数 —— -1 已经表示 input 了。
      const int id = alloc("destructure", s);
      ++stmtNodes_;
      JVal vars = JVal::array();
      for (const Node* n = pat->child; n; n = n->next) {
        if (n->kind != Kind::IdentExpr) {
          fail("解构的绑定名不是标识符");
          return -1;
        }
        const std::string name = n->text ? n->text : "";
        if (name == "_") {
          vars.push(JVal::null());
        } else {
          vars.push(JVal::integer(var_index(name)));
        }
      }
      const int e = gen_expr(s->args);
      if (e < 0) return -1;
      nodes_[id].set("vars", std::move(vars));
      nodes_[id].set("e", JVal::integer(e));
      span_[id] = span_[e];
      // 解构只是"把元组拆开绑定"，调用次数仍然是值里那些（被解构的 parallel）。
      model_[id] = model_[e];
      tool_[id] = tool_[e];
      return id;
    }
    case Kind::IfStmt: {
      // 子节点顺序：cond → then 块 → [else 块]；编号也按这个顺序（IR.md §4.2）。
      const std::vector<const Node*> k = children_of(s);
      const Node* cond = k.size() > 0 ? k[0] : nullptr;
      const Node* thenBlk = k.size() > 1 ? k[1] : nullptr;
      const Node* elseBlk = k.size() > 2 ? k[2] : nullptr;
      if (!cond || !thenBlk) {
        fail("if 缺少条件或 then 块");
        return -1;
      }
      const int id = alloc("if", s);
      ++stmtNodes_;
      const int c = gen_expr(cond);
      if (c < 0) return -1;
      const int t = gen_stmt(thenBlk);
      if (t < 0) return -1;
      const int e2 = elseBlk ? gen_stmt(elseBlk) : -1;
      if (elseBlk && e2 < 0) return -1;

      nodes_[id].set("c", JVal::integer(c));
      nodes_[id].set("then", JVal::integer(t));
      nodes_[id].set("elifs", JVal::array());
      if (e2 >= 0) {
        nodes_[id].set("else", JVal::integer(e2));
      } else {
        nodes_[id].set("else", JVal::null());
      }
      // SP(if) = 1 + max(then, else)：分支互斥，取最坏的一条。
      int best = span_[t];
      if (e2 >= 0 && span_[e2] > best) best = span_[e2];
      span_[id] = 1 + best;
      int mbest = model_[t];
      if (e2 >= 0 && model_[e2] > mbest) mbest = model_[e2];
      model_[id] = mbest;
      int tbest = tool_[t];
      if (e2 >= 0 && tool_[e2] > tbest) tbest = tool_[e2];
      tool_[id] = tbest;
      return id;
    }
    case Kind::UpdateStmt: {
      // `x++;` / `x--;` → assign(var, var ± 1)。复用 assign 节点，
      // 不新增 IR 种类：它表达的就是"把变量写成它自己加减一"。
      const int vid = var_index(s->text ? s->text : "");
      if (vid < 0) { fail("自增减的目标不是已声明的变量"); return -1; }
      const int litId = alloc("lit", s);
      nodes_[litId].set("t", JVal::string("int"));
      nodes_[litId].set("v", JVal::integer(1));
      const int varNode = alloc("var", s);
      nodes_[varNode].set("var", JVal::integer(vid));
      const int binId = alloc("binop", s);
      nodes_[binId].set("op", JVal::string(s->i64 >= 0 ? "add" : "sub"));
      nodes_[binId].set("l", JVal::integer(varNode));
      nodes_[binId].set("r", JVal::integer(litId));
      const int id = alloc("assign", s);
      ++stmtNodes_;
      nodes_[id].set("var", JVal::integer(vid));
      nodes_[id].set("e", JVal::integer(binId));
      span_[id] = 0;
      model_[id] = 0;
      tool_[id] = 0;
      return id;
    }
    case Kind::AssignStmt: {
      // 赋值：只能写 `let mut` 声明过的变量。**在生成阶段校验**（这里能查变量表）。
      const int vid = var_index(s->text ? s->text : "");
      if (vid < 0) {
        fail("赋值的目标不是已声明的变量: " + std::string(s->text ? s->text : ""));
        return -1;
      }
      const int e = gen_expr(s->args);
      if (e < 0) return -1;
      const int id = alloc("assign", s);
      ++stmtNodes_;
      nodes_[id].set("var", JVal::integer(vid));
      nodes_[id].set("e", JVal::integer(e));
      span_[id] = 0;
      model_[id] = 0;
      tool_[id] = 0;
      return id;
    }
    case Kind::BreakStmt: {
      const int id = alloc("break", s);
      ++stmtNodes_;
      span_[id] = 0;
      model_[id] = 0;
      tool_[id] = 0;
      return id;
    }
    case Kind::ContinueStmt: {
      const int id = alloc("continue", s);
      ++stmtNodes_;
      span_[id] = 0;
      model_[id] = 0;
      tool_[id] = 0;
      return id;
    }
    case Kind::WhileStmt: {
      // 无静态上界：迭代次数运行期才知道，所以整个 workflow 标 unbounded。
      const int id = alloc("while", s);
      ++stmtNodes_;
      const int c = gen_expr(s->child);
      if (c < 0) return -1;
      const int body = gen_stmt(s->body);
      if (body < 0) return -1;
      nodes_[id].set("c", JVal::integer(c));
      nodes_[id].set("body", JVal::integer(body));
      unbounded_ = true;
      span_[id] = 1 + span_[body];
      model_[id] = model_[body];
      tool_[id] = tool_[body];
      return id;
    }
    case Kind::ForStmt: {
      // for (let i = START; i < END; i++) { body }
      //
      // 循环变量由这个节点**独占**：IR 里 `var` 只出现在这里，循环体里
      // 没有任何赋值节点能改它 —— 所以 `var.decl` 唯一性不破。
      const int id = alloc("for", s);
      ++stmtNodes_;
      const int vid = var_index(s->text ? s->text : "");
      const int init = gen_expr(s->child);
      if (init < 0) return -1;
      const int cond = gen_expr(s->args);
      if (cond < 0) return -1;
      const int body = gen_stmt(s->body);
      if (body < 0) return -1;
      nodes_[id].set("var", JVal::integer(vid));
      nodes_[id].set("init", JVal::integer(init));
      nodes_[id].set("cond", JVal::integer(cond));
      nodes_[id].set("body", JVal::integer(body));
      nodes_[id].set("delta", JVal::integer(s->i64));

      // 上界：能算出来就乘，算不出来标 unbounded。
      const int n = for_iterations(s->child, s->args, s->i64);
      if (n < 0) {
        unbounded_ = true;
        span_[id] = 1 + span_[body];
        model_[id] = model_[body];
        tool_[id] = tool_[body];
      } else {
        span_[id] = 1 + n * span_[body];
        model_[id] = n * model_[body];
        tool_[id] = n * tool_[body];
      }
      if (span_[id] > max_conc_) { /* 顺序节点不产生并发 */ }
      return id;
    }
    case Kind::MatchStmt: {
      // 子节点可能分布在 child 与 args 两个槽位（dump 只显示"是子节点"，
      // 不显示挂在哪个字段）。两个槽位按顺序收进来，第一个是被匹配的表达式。
      const std::vector<const Node*> kids = children_of(s);
      if (kids.empty()) {
        fail("match 缺少被匹配的表达式");
        return -1;
      }
      const Node* subj = kids[0];
      const int id = alloc("match", s);
      ++stmtNodes_;
      const int sj = gen_expr(subj);
      if (sj < 0) return -1;

      JVal arms = JVal::array();
      int def = -1;
      int best = 0;
      int mmax = 0;
      int tmax = 0;
      for (std::size_t ki = 1; ki < kids.size(); ++ki) {
        const Node* a = kids[ki];
        if (a->kind != Kind::MatchArm) continue;
        // **空串 ⟺ default 臂**（IR.md §4.2）。这不是约定俗成：判"有没有 default"
        // 只有这一个依据，臂的数量说明不了任何事。
        const bool isDefault = a->is_nullable;
        const bool isInt = !isDefault && a->text == nullptr;
        const std::string lit = a->text ? a->text : "";
        const int b = gen_stmt(a->child);
        if (b < 0) return -1;
        if (span_[b] > best) best = span_[b];
        if (model_[b] > mmax) mmax = model_[b];
        if (tool_[b] > tmax) tmax = tool_[b];
        if (isDefault) {
          def = b;
        } else {
          JVal arm = JVal::object();
          if (isInt) {
            arm.set("num", JVal::integer(a->i64));
          } else {
            arm.set("lit", JVal::string(lit));
          }
          arm.set("body", JVal::integer(b));
          arms.push(std::move(arm));
        }
      }
      if (def < 0) {
        fail("match 缺少 default（E4018）：被匹配的是字符串，全覆盖无法静态证明");
        return -1;
      }
      nodes_[id].set("subj", JVal::integer(sj));
      nodes_[id].set("arms", std::move(arms));
      nodes_[id].set("default", JVal::integer(def));
      span_[id] = 1 + best;
      model_[id] = mmax;
      tool_[id] = tmax;
      return id;
    }
    case Kind::TryStmt: {
      // AST：body 在 child，catch 链在 **args**，每个 CatchClause 的类型名在
      // text、绑定的变量名在 child->text、catch 体挂在 **else_branch**。
      // 这三处都是"猜一次就会错"的位置（HANDOFF §4.4 已记过四次）。
      if (!s->child) {
        fail("try 缺少函数体");
        return -1;
      }
      const int id = alloc("try", s);
      ++stmtNodes_;
      const int body = gen_stmt(s->child);
      if (body < 0) return -1;

      JVal catches = JVal::array();
      int best = 0;
      int mbest = model_[body];
      int tbest = tool_[body];
      for (const Node* c = s->args; c; c = c->next) {
        if (c->kind != Kind::CatchClause) continue;
        const int cb = gen_stmt(c->else_branch);
        if (cb < 0) return -1;
        if (span_[cb] > best) best = span_[cb];
        // **相加，不是取大**：try 的语义是"体可能部分执行后才抛，再进 catch"，
        // 所以最坏情况 = 体里的调用 + catch 里的调用。取大等于假设两者互斥，
        // 而上界必须覆盖最坏情况。
        if (model_[cb] > mbest - model_[body]) mbest = model_[body] + model_[cb];
        if (tool_[cb] > tbest - tool_[body]) tbest = tool_[body] + tool_[cb];
        int vid = -1;
        if (c->child && c->child->text) {
          vid = var_index(c->child->text);
        }
        JVal arm = JVal::object();
        arm.set("t", JVal::string(c->text ? c->text : "Error"));
        arm.set("v", JVal::integer(vid));
        arm.set("body", JVal::integer(cb));
        catches.push(std::move(arm));
      }
      nodes_[id].set("body", JVal::integer(body));
      nodes_[id].set("catches", std::move(catches));
      // SP(try) = 1 + SP(body) + max SP(catch_i)
      span_[id] = 1 + span_[body] + best;
      // 调用上界**也要汇总**：之前只算了 span，于是 `try` 里的模型调用没进
      // max_model_calls —— 上界看着是 0，实际会调一次。上界偏小在运行期
      // 意味着被截断或被预算拒掉，而那是最难查的一类失败。
      model_[id] = mbest;
      tool_[id] = tbest;
      return id;
    }
    case Kind::RetryStmt: {
      const Node* body = s->child;
      if (!body) {
        fail("retry 缺少函数体");
        return -1;
      }
      const int id = alloc("retry", s);
      ++stmtNodes_;
      const int b = gen_stmt(body);
      if (b < 0) return -1;
      int n = static_cast<int>(s->i64);
      if (n < 0) n = 0;
      // delay 在 AST 里是 double（parser.cpp 用 f64 存），但 IR 禁浮点，
      // 而语法只接受整数字面量 —— 这里取整是**无损**的，不是四舍五入。
      const int delay = static_cast<int>(s->f64);
      nodes_[id].set("n", JVal::integer(n));
      nodes_[id].set("delay_ms", JVal::integer(delay));
      nodes_[id].set("body", JVal::integer(b));
      // SP(retry) = 1 + (n+1) × SP(body)
      span_[id] = 1 + (n + 1) * span_[b];
      model_[id] = (n + 1) * model_[b];
      tool_[id] = (n + 1) * tool_[b];
      return id;
    }
    case Kind::ExprStmt: {
      // 表达式语句：`memory.put(...)`、`vector.put(...)` 这类**有副作用**的调用
      // 不需要一个值，但要被执行。没有这一条，用户就得编一个假变量来接它 ——
      // 那会把"我只是想写进去"变成"我还得给它起个名字"。
      const int id = alloc("exprstmt", s);
      ++stmtNodes_;
      const int e = gen_expr(s->child);
      if (e < 0) return -1;
      nodes_[id].set("e", JVal::integer(e));
      span_[id] = span_[e];
      model_[id] = model_[e];
      tool_[id] = tool_[e];
      return id;
    }
    case Kind::ReturnStmt: {
      const int id = alloc("return", s);
      ++stmtNodes_;
      const int e = gen_expr(s->child);
      if (e < 0) return -1;
      nodes_[id].set("e", JVal::integer(e));
      span_[id] = span_[e];
      model_[id] = model_[e];
      tool_[id] = tool_[e];
      return id;
    }
    default:
      fail(std::string("codegen 尚不支持这种语句（kind=") + kind_name(s->kind) + "）");
      return -1;
  }
}

bool Gen::run(const Node* wf, JVal* out) {
  const Node* body = nullptr;
  for (const Node* c = wf->child; c; c = c->next) {
    if (c->kind == Kind::Block) body = c;
  }
  if (!body) return fail("workflow 没有函数体");

  // 策略必须在生成 body 之前解析（工具白名单要用）。
  if (wf->policy) {
    for (const Node* d = module_ ? module_->child : nullptr; d; d = d->next) {
      if (d->kind == Kind::SecurityDecl && d->text && strcmp(d->text, wf->policy) == 0) { policy_ = d; break; }
    }
  }
  const int root = gen_stmt(body);
  if (root < 0) return false;

  JVal w = JVal::object();
  w.set("name", JVal::string(wf->text ? wf->text : ""));
  // `with <Policy>`：名字必须能找到声明。找不到就**报错** —— 打错一个字母
  // 等于没有策略，而"没有策略"和"策略放过"在产物里长得一样。
  if (wf->policy) {
    const Node* decl = nullptr;
    for (const Node* d = module_ ? module_->child : nullptr; d; d = d->next) {
      if (d->kind == Kind::SecurityDecl && d->text && strcmp(d->text, wf->policy) == 0) { decl = d; break; }
    }
    if (!decl) return fail(std::string("`with` refers to an undeclared policy: ") + wf->policy);
    JVal pol = JVal::object();
    pol.set("name", JVal::string(wf->policy));
    JVal rules = JVal::array();
    for (const Node* r = decl->child; r; r = r->next) {
      if (r->kind != Kind::SecurityRule) continue;
      JVal one = JVal::object();
      one.set("effect", JVal::string(r->is_export ? "allow" : "deny"));
      one.set("cap", JVal::string(r->text ? r->text : ""));
      JVal vals = JVal::array();
      for (const Node* v = r->child; v; v = v->next) vals.push(JVal::string(v->text ? v->text : ""));
      one.set("values", std::move(vals));
      rules.push(std::move(one));
    }
    pol.set("rules", std::move(rules));
    w.set("security", std::move(pol));
  }
  w.set("exported", JVal::boolean(wf->is_export));
  w.set("is_entry", JVal::boolean(wf->is_entry));
  // backend 默认值来自参数 `backend = homonoia`。不读它，workflow.backend 永远
  // 是空串 —— 而空串在运行期意味着"没有默认 backend"，于是真实运行会拒绝起跑，
  // 症状却像"配置没给"。
  // 参数默认值：workflow 的 Param 挂在 **args** 链上（dump 确认过）。
  // 值可能是 PathExpr（`backend = homonoia` 里 homonoia 是个标识符路径），
  // 直接读 Param 的 text 只会拿到参数名 —— 那正是 backend 一直为空的原因。
  std::string defBackend;
  JVal params = JVal::array();
  // 参数所在的槽位**必须逐个试**：dump 只显示"是子节点"，不显示挂在哪个字段，
  // 而 WorkflowDecl 有 child/type_args/args/subs 四处可用。只读 args 会得到空列表
  // —— 症状是 workflow.backend 恒为空，看起来像"源码里没写"。
  std::vector<const Node*> paramNodes;
  {
    const Node* heads[3] = {wf->args, wf->child, wf->type_args};
    for (const Node* head : heads) {
      for (const Node* c = head; c; c = c->next) {
        if (c->kind == Kind::Param) paramNodes.push_back(c);
      }
    }
    for (int i = 0; i < wf->n_subs; ++i) {
      if (wf->subs[i].kind == Kind::Param) paramNodes.push_back(&wf->subs[i]);
    }
  }
  for (const Node* c : paramNodes) {
    if (!c->text) continue;
    const std::string pname = c->text;
    if (pname == "input") continue;  // input 是 workflow.input 的事
    JVal pv = JVal::object();
    pv.set("n", JVal::string(pname));
    JVal folded = JVal::object();
    // Param 的**值**也不在 child 上（dump 把 child/type_args/args/subs 合并显示，
    // 看着像 child 而已）。用 children_of 取第一个子节点。
    const std::vector<const Node*> pvals = children_of(c);
    const Node* v = pvals.empty() ? nullptr : pvals[0];
    if (v && v->kind == Kind::PathExpr) {
      // 单段路径 = 裸标识符；这里只可能是 backend 名。
      std::string name;
      for (const Node* seg = v->child; seg; seg = seg->next) {
        if (seg->kind == Kind::IdentExpr && seg->text) name = seg->text;
      }
      if (name.empty()) {
        for (int i = 0; i < v->n_subs; ++i) {
          if (v->subs[i].kind == Kind::IdentExpr && v->subs[i].text) name = v->subs[i].text;
        }
      }
      folded.set("k", JVal::string("backend"));
      folded.set("v", JVal::string(name));
      if (pname == "backend") defBackend = name;
    } else if (v && v->kind == Kind::LitExpr) {
      const std::string t = v->text ? v->text : "string";
      if (t == "int") {
        folded.set("k", JVal::string("int"));
        folded.set("v", JVal::integer(v->i64));
      } else if (t == "bool") {
        folded.set("k", JVal::string("bool"));
        folded.set("v", JVal::boolean(v->is_nullable));
      } else {
        folded.set("k", JVal::string("string"));
        folded.set("v", JVal::string(lit_text(v)));
      }
    } else {
      continue;  // 无法折叠的默认值：这一刀不支持，跳过而不是编一个假值
    }
    pv.set("v", std::move(folded));
    params.push(std::move(pv));
  }
  // ---- 策略：model.call 白名单（编译期）----
  //
  // backend 是**封闭集合**（源码里声明的那些），所以"这个 workflow 会调哪个
  // 模型后端"编译期就知道。允许列表存在时，默认 backend 必须在列表里。
  if (policy_ && !defBackend.empty()) {
    const Node* allowModel = nullptr;
    const Node* denyModel = nullptr;
    for (const Node* r = policy_->child; r; r = r->next) {
      if (r->kind != Kind::SecurityRule || !r->text) continue;
      if (strcmp(r->text, "model.call") != 0) continue;
      if (r->is_export) allowModel = r; else denyModel = r;
    }
    for (const Node* v = denyModel ? denyModel->child : nullptr; v; v = v->next) {
      if (v->text && defBackend == v->text) {
        return fail("policy " + std::string(policy_->text ? policy_->text : "") +
                    " forbids backend " + defBackend + " (deny model.call)");
      }
    }
    if (allowModel) {
      bool listed = false;
      for (const Node* v = allowModel->child; v; v = v->next) {
        if (v->text && defBackend == v->text) { listed = true; break; }
      }
      if (!listed) {
        return fail("policy " + std::string(policy_->text ? policy_->text : "") +
                    " does not allow backend " + defBackend + " (not in allow model.call)");
      }
    }
  }
  w.set("backend", JVal::string(defBackend));
  w.set("input", JVal::null());
  w.set("returns", type_of(wf->type_args));
  w.set("params", std::move(params));
  w.set("root", JVal::integer(root));

  JVal ns = JVal::array();
  for (auto& n : nodes_) ns.push(n);
  w.set("nodes", std::move(ns));

  JVal vs = JVal::array();
  for (std::size_t i = 0; i < vars_.size(); ++i) {
    JVal v = JVal::object();
    v.set("id", JVal::integer(static_cast<std::int64_t>(i)));
    v.set("name", JVal::string(vars_[i]));
    JVal t = JVal::object();
    t.set("k", JVal::string("json"));  // 这一刀没有类型检查结果可复用
    v.set("t", std::move(t));
    v.set("decl", JVal::integer(-1));  // 由调用方（Go 装配）按 let 节点回填
    v.set("scope", JVal::integer(root));
    vs.push(std::move(v));
  }
  w.set("vars", std::move(vs));

  JVal b = JVal::object();
  // 上界算不出来时**明说**。max_spans 仍然写一个数（运行期断言要用），
  // 但 unbounded=true 告诉读的人：那个数不是真正的上界。
  if (unbounded_) {
    b.set("unbounded", JVal::boolean(true));
  }
  // max_steps = **语句节点总数 + 根 seq**：每个语句节点最多执行一次
  // （retry 的重复执行已单独折算进调用上界）。stmtNodes_ 在 gen_stmt 入口累加，
  // 所以它数的是"生成了多少个语句节点"，与执行器实际会走的步数同源。
  //
  // 之前用 steps_，漏掉根 seq；又把 try 的 catch 当成互斥分支 —— 结果偏小，
  // 而偏小的上界在运行期意味着**截断**，那是最难查的一类失败。
  b.set("max_steps", JVal::integer(stmtNodes_ + 1));
  b.set("max_spans", JVal::integer(span_[root]));
  b.set("max_model_calls", JVal::integer(model_[root]));
  b.set("max_tool_calls", JVal::integer(tool_[root]));
  b.set("max_concurrency", JVal::integer(max_conc_));
  b.set("max_retry_expansion", JVal::integer(0));
  w.set("bounds", std::move(b));

  JVal as = JVal::object();
  as.set("input_bytes_upper", JVal::integer(1048576));
  as.set("tokens_per_byte_milli", JVal::integer(1000));
  as.set("max_tokens_default", JVal::integer(1024));
  JVal ec = JVal::object();
  ec.set("assumptions", std::move(as));
  // **这些必须与 bounds 同源**。上一版这里全写 0，而 bounds 里
  // max_model_calls 是真实计数 —— 于是 `hermes explain` 同时显示
  // "max_model_calls 2" 和 "model_calls_upper 0"，读的人不知道该信哪个。
  // 经济学上界要么算出来，要么明确写"不知道"，不能写一个看着像算过的 0。
  const long long modelCallsUpper = model_[root];
  ec.set("prompt_bytes_upper",
         JVal::integer(modelCallsUpper * 1048576 / 1000 * 1000));
  ec.set("tokens_in_upper", JVal::integer(modelCallsUpper * 1024));
  ec.set("tokens_out_upper", JVal::integer(modelCallsUpper * 1024));
  ec.set("model_calls_upper", JVal::integer(modelCallsUpper));
  ec.set("pricing_source", JVal::string("unknown"));
  w.set("economics", std::move(ec));

  w.set("interp_deps", JVal::array());
  w.set("loc", loc_of(wf));

  *out = std::move(w);
  return true;
}

// run_function 把一个 FunctionDecl 编成函数表的一项。
//
// AST 形状（tools/astdump 实测，见 docs/FUNCTIONS.md）：
//   child      = Param 链（参数名在 text，类型在 type_args）
//   type_args  = 返回类型
//   body       = 函数体 Block（不在 child 链上）
bool Gen::run_function(const Node* fn, JVal* out) {
  if (!fn || fn->kind != Kind::FunctionDecl) {
    return fail("run_function expects a FunctionDecl");
  }
  const Node* body = fn->body;
  if (!body) return fail("function has no body");

  JVal params = JVal::array();
  std::vector<const Node*> paramNodes;
  for (const Node* p = fn->child; p; p = p->next) {
    if (p->kind == Kind::Param) paramNodes.push_back(p);
  }
  for (const Node* p : paramNodes) {
    const std::string name = p->text ? p->text : "";
    if (name.empty()) return fail("function parameter needs a name");
    if (var_types_.count(name)) return fail("duplicate parameter name: " + name);
    const JVal pty = fn_type_expr(p->type_args);
    JVal pv = JVal::object();
    pv.set("n", JVal::string(name));
    pv.set("t", pty);
    params.push(std::move(pv));
    var_types_[name] = pty;
    // 参数先占变量槽：声明顺序 = 下标，与 workflow 的 input 是同一条约定。
    var_index(name);
  }

  in_fn_ = true;
  cur_fn_ = fn->text ? fn->text : "";
  const int root = gen_stmt(body);
  in_fn_ = false;
  if (root < 0) {
    // 错误定位在函数级（诊断的 loc 由 api 层贴到声明上）。把函数名写进消息，
    // 否则一个 unit 里多个函数时会说不清是哪一个编不出来。
    const std::string fname = fn->text ? fn->text : "";
    if (err_ && !err_->empty()) *err_ = "in function `" + fname + "`: " + *err_;
    return false;
  }

  JVal f = JVal::object();
  f.set("name", JVal::string(fn->text ? fn->text : ""));
  f.set("params", std::move(params));
  f.set("returns", fn_type_expr(fn->type_args));
  f.set("root", JVal::integer(root));
  // recursive 由 codegen_functions 在整张调用图上判定后回填。先写 false，
  // 让"不是递归"与"忘了写"在字节上仍可区分（缺字段会被读成 false）。
  f.set("recursive", JVal::boolean(false));

  JVal ns = JVal::array();
  for (auto& n : nodes_) ns.push(n);
  f.set("nodes", std::move(ns));

  JVal vs = JVal::array();
  for (std::size_t i = 0; i < vars_.size(); ++i) {
    JVal v = JVal::object();
    v.set("id", JVal::integer(static_cast<std::int64_t>(i)));
    v.set("name", JVal::string(vars_[i]));
    auto it = var_types_.find(vars_[i]);
    if (it != var_types_.end()) {
      v.set("t", it->second);
    } else {
      JVal t = JVal::object();
      t.set("k", JVal::string("json"));  // 与 workflow.vars 一致：这一刀没有类型检查结果
      v.set("t", std::move(t));
    }
    // 参数的 decl 指向函数体的根 seq：它表达"从函数入口起可见"，
    // 与 workflow 的 input 用 root 表示"从根可见"是同一条约定。
    // 局部变量由装配阶段按绑定节点回填（与 workflow.vars 同一条路径）。
    v.set("decl", JVal::integer(root));
    v.set("scope", JVal::integer(root));
    vs.push(std::move(v));
  }
  f.set("vars", std::move(vs));

  *out = std::move(f);
  return true;
}

}  // namespace

bool codegen_types(const Node* module, JVal* out, std::string* err) {
  JVal arr = JVal::array();
  for (const Node* d = module ? module->child : nullptr; d; d = d->next) {
    if (d->kind != Kind::StructDecl) continue;

    JVal t = JVal::object();
    t.set("name", JVal::string(d->text ? d->text : ""));
    t.set("exported", JVal::boolean(d->is_export));
    t.set("loc", [&] {
      JVal l = JVal::object();
      l.set("m", JVal::integer(0));
      l.set("l", JVal::integer(d->loc.line));
      l.set("c", JVal::integer(d->loc.col));
      l.set("el", JVal::integer(d->loc.end_line));
      l.set("ec", JVal::integer(d->loc.end_col));
      return l;
    }());

    JVal fields = JVal::array();
    for (const Node* f = d->child; f; f = f->next) {
      if (f->kind != Kind::Field) continue;
      JVal fv = JVal::object();
      fv.set("n", JVal::string(f->text ? f->text : ""));
      const std::string ty = (f->child && f->child->text) ? f->child->text : "";
      // 数组维度记在 **Type 节点的 i64** 里（parse_type 填的）。忽略它，
      // `string[] tags` 会静默变成 `string` —— 而"少了个数组"在 IR 里完全合法，
      // 到运行期才表现为"取到的不是一个列表"。
      const int dims = (f->child) ? static_cast<int>(f->child->i64) : 0;
      JVal tv = JVal::object();
      if (is_builtin_type(ty)) {
        tv.set("k", JVal::string(ty));
      } else {
        // 名字先留着，id 由装配阶段分配（见头文件说明）。
        tv.set("k", JVal::string("struct"));
        tv.set("ref", JVal::string(ty));
      }
      for (int d = 0; d < dims; ++d) {
        JVal wrap = JVal::object();
        wrap.set("k", JVal::string("array"));
        wrap.set("of", std::move(tv));
        tv = std::move(wrap);
      }
      fv.set("t", std::move(tv));
      fv.set("opt", JVal::boolean(f->is_nullable));
      fv.set("ann", JVal::array());
      fields.push(std::move(fv));
    }
    t.set("fields", std::move(fields));
    arr.push(std::move(t));
  }

  *out = std::move(arr);
  (void)err;
  return true;
}

bool codegen_wfdecls(const Node* module, JVal* out, std::string* err) {
  JVal arr = JVal::array();
  for (const Node* d = module ? module->child : nullptr; d; d = d->next) {
    // `.h` 里是 **WorkflowSig**（只有声明），`.hermes` 里是 **WorkflowDecl**（带体）。
    // 只认后者，`.h` 里那句 `export workflow … entry -> string;` 就整条看不见 ——
    // 于是"entry 没有 export"这种假错会在每个正常工程上出现。
    if (d->kind != Kind::WorkflowDecl && d->kind != Kind::WorkflowSig) continue;
    JVal w = JVal::object();
    w.set("name", JVal::string(d->text ? d->text : ""));
    w.set("exported", JVal::boolean(d->is_export));
    w.set("is_entry", JVal::boolean(d->is_entry));
    arr.push(std::move(w));
  }
  *out = std::move(arr);
  (void)err;
  return true;
}

bool codegen_backends(const Node* module, JVal* out, std::string* err) {
  JVal arr = JVal::array();
  for (const Node* d = module ? module->child : nullptr; d; d = d->next) {
    if (d->kind != Kind::BackendSig && d->kind != Kind::BackendDef) continue;
    JVal b = JVal::object();
    b.set("name", JVal::string(d->text ? d->text : ""));
    const char* kind = (d->type_args && d->type_args->text) ? d->type_args->text : "";
    b.set("kind", JVal::string(kind));
    b.set("exported", JVal::boolean(d->is_export));
    b.set("decl_form", JVal::string(d->kind == Kind::BackendSig ? "sig" : "def"));

    JVal l = JVal::object();
    l.set("m", JVal::integer(0));
    l.set("l", JVal::integer(d->loc.line));
    l.set("c", JVal::integer(d->loc.col));
    l.set("el", JVal::integer(d->loc.end_line));
    l.set("ec", JVal::integer(d->loc.end_col));
    b.set("loc", std::move(l));

    // 定义那一份才带地址。地址的**来源**（env/literal）原样搬进 IR，
    // 绝不在这里解析成真实 URL（IR.md §3.3 的可重现性要求）。
    if (d->kind == Kind::BackendDef && d->child) {
      const Node* env = d->child;
      JVal addr = JVal::object();
      if (env->kind == Kind::EnvExpr) {
        addr.set("src", JVal::string("env"));
        addr.set("env", JVal::string(env->child && env->child->text ? env->child->text : ""));
        addr.set("default", JVal::string(env->args && env->args->text ? env->args->text : ""));
      } else {
        addr.set("src", JVal::string("literal"));
        addr.set("value", JVal::string(env->text ? env->text : ""));
      }
      b.set("addr", std::move(addr));
    }
    arr.push(std::move(b));
  }
  *out = std::move(arr);
  (void)err;
  return true;
}

// codegen_tools：`export tool x { … }` 声明 → tools[] 表（D-034）。
//
// 为什么工具要有**声明**而不是内联在 agent 调用里：模型看到的 function schema
// 与运行期执行的 URL 必须是同一份东西。内联就会变成两处维护，而两处必然漂移。
bool codegen_tools(const Node* module, JVal* out, std::string* err) {
  JVal arr = JVal::array();
  for (const Node* d = module ? module->child : nullptr; d; d = d->next) {
    if (d->kind != Kind::ToolDecl) continue;
    JVal t = JVal::object();
    t.set("name", JVal::string(d->text ? d->text : ""));
    std::string description;
    std::string method = "GET";
    JVal tmpl = JVal::array();
    for (const Node* f = d->child; f; f = f->next) {
      const std::string fname = f->text ? f->text : "";
      const Node* v = f->child;
      if (!v || v->kind != Kind::LitExpr) continue;
      if (fname == "description") {
        description = lit_text_of(v);
      } else if (fname == "method") {
        const std::string m = lit_text_of(v);
        if (!m.empty()) method = m;
      } else if (fname == "url") {
        for (int i = 0; i < v->n_subs; ++i) {
          const Node& part = v->subs[i];
          JVal p = JVal::object();
          if (part.is_path) {
            JVal path = JVal::array();
            std::string text = part.text ? part.text : "";
            std::size_t start = 0;
            while (start <= text.size()) {
              std::size_t dot = text.find('.', start);
              if (dot == std::string::npos) {
                path.push(JVal::string(text.substr(start)));
                break;
              }
              path.push(JVal::string(text.substr(start, dot - start)));
              start = dot + 1;
            }
            p.set("path", std::move(path));
          } else {
            p.set("lit", JVal::string(part.text ? part.text : ""));
          }
          tmpl.push(std::move(p));
        }
      } else {
        if (err) *err = "tool 声明里有不认识的字段: " + fname;
        return false;
      }
    }
    t.set("description", JVal::string(description));
    t.set("method", JVal::string(method));
    t.set("url_template", std::move(tmpl));
    JVal loc = JVal::object();
    loc.set("m", JVal::integer(0));
    loc.set("l", JVal::integer(d->loc.line));
    loc.set("c", JVal::integer(d->loc.col));
    loc.set("el", JVal::integer(d->loc.end_line));
    loc.set("ec", JVal::integer(d->loc.end_col));
    t.set("decl_loc", std::move(loc));
    arr.push(std::move(t));
  }
  *out = std::move(arr);
  return true;
}

// ---- 函数表 ----
//
// 递归判定只在**调用图**上做：cur 能否沿调用边回到 start。
// 不做 SCC 合并是因为函数数量小（一个 unit 几十个），而这一定义最直白：
// "函数 f 是递归的" = "从 f 出发能回到 f"。
static bool fn_reaches(const std::string& start, const std::string& cur,
                       const std::map<std::string, std::vector<std::string>>& edges,
                       std::map<std::string, bool>& seen) {
  if (cur == start) return true;
  if (seen[cur]) return false;
  seen[cur] = true;
  auto it = edges.find(cur);
  if (it != edges.end()) {
    for (const auto& nx : it->second) {
      if (fn_reaches(start, nx, edges, seen)) return true;
    }
  }
  return false;
}

static bool fn_is_recursive(const std::string& name,
                            const std::map<std::string, std::vector<std::string>>& edges) {
  auto it = edges.find(name);
  if (it == edges.end()) return false;
  for (const auto& nx : it->second) {
    std::map<std::string, bool> seen;
    if (fn_reaches(name, nx, edges, seen)) return true;
  }
  return false;
}

// analyze_functions 建名字表并算出"哪些函数递归"。
//
// 两遍：先按分析模式（所有调用都记 call_fn）编一遍拿调用图，再判定递归。
// 这一步不做内联 —— 递归判定不能依赖内联结果。
static bool analyze_functions(const Node* module,
                              std::map<std::string, const Node*>* byName,
                              std::map<std::string, bool>* recursive,
                              std::string* err, Loc* err_loc) {
  std::vector<const Node*> decls;
  for (const Node* d = module ? module->child : nullptr; d; d = d->next) {
    if (d->kind != Kind::FunctionDecl) continue;
    const std::string name = d->text ? d->text : "";
    if (name.empty()) {
      if (err) *err = "function needs a name";
      if (err_loc) *err_loc = d->loc;
      return false;
    }
    if (byName->count(name) != 0) {
      if (err) {
        *err = "function " + name + " is defined twice (first at line " +
               std::to_string((*byName)[name]->loc.line) + ", again at line " +
               std::to_string(d->loc.line) + "; overloading is not supported)";
      }
      if (err_loc) *err_loc = d->loc;
      return false;
    }
    if (name == "len") {
      if (err) *err = "function cannot be named `len`: that is the builtin length function";
      if (err_loc) *err_loc = d->loc;
      return false;
    }
    (*byName)[name] = d;
    decls.push_back(d);
  }
  if (decls.empty()) return true;

  std::map<std::string, std::vector<std::string>> edges;
  for (const Node* fn : decls) {
    Gen g(err);
    g.set_functions(byName);  // fn_recursive_ 为空 = 分析模式
    JVal discard;
    if (!g.run_function(fn, &discard)) {
      if (err_loc && g.err_node()) *err_loc = g.err_node()->loc;
      return false;
    }
    for (const auto& kv : g.calls()) {
      std::vector<std::string>& dst = edges[kv.first];
      dst.insert(dst.end(), kv.second.begin(), kv.second.end());
    }
  }
  for (const Node* fn : decls) {
    const std::string name = fn->text ? fn->text : "";
    (*recursive)[name] = fn_is_recursive(name, edges);
  }
  return true;
}

bool codegen_functions(const Node* module, JVal* out, std::string* err, Loc* err_loc) {
  std::map<std::string, const Node*> byName;
  std::map<std::string, bool> recursive;
  if (!analyze_functions(module, &byName, &recursive, err, err_loc)) return false;

  // 逐个编函数表项。注意这里也要装入 recursive 判定：递归函数的函数体会
  // 被运行期执行，它内部对**非递归**函数的调用同样必须在编译期展开。
  JVal arr = JVal::array();
  for (const Node* d = module ? module->child : nullptr; d; d = d->next) {
    if (d->kind != Kind::FunctionDecl) continue;
    Gen g(err);
    g.set_functions(&byName);
    g.set_recursive(&recursive);
    JVal one;
    if (!g.run_function(d, &one)) {
      if (err_loc && g.err_node()) *err_loc = g.err_node()->loc;
      return false;
    }
    const std::string name = d->text ? d->text : "";
    one.set("recursive", JVal::boolean(recursive[name]));
    arr.push(std::move(one));
  }
  *out = std::move(arr);
  return true;
}

bool codegen_workflow_in(const Node* module, const Node* wf, JVal* out, std::string* err,
                         Loc* err_loc) {
  if (!wf || wf->kind != Kind::WorkflowDecl) {
    if (err) *err = "codegen_workflow_in 只接受 WorkflowDecl";
    return false;
  }
  // 函数是 unit 级事实：workflow 里的 `greet(...)` 要解析成"内联"或
  // call_fn（仅递归）。函数本身由 codegen_functions 单独编成一张表。
  std::map<std::string, const Node*> fns;
  std::map<std::string, bool> recursive;
  if (!analyze_functions(module, &fns, &recursive, err, err_loc)) return false;
  Gen g(err);
  g.set_module(module);
  g.set_functions(&fns);
  g.set_recursive(&recursive);
  const bool ok = g.run(wf, out);
  if (!ok && err_loc) {
    // 编译期展开失败要指到**调用点**（g.err_node），而不是 workflow 开头。
    *err_loc = g.err_node() ? g.err_node()->loc : wf->loc;
  }
  return ok;
}

bool codegen_workflow(const Node* wf, JVal* out, std::string* err) {
  if (!wf || wf->kind != Kind::WorkflowDecl) {
    if (err) *err = "codegen_workflow 只接受 WorkflowDecl";
    return false;
  }
  Gen g(err);
  return g.run(wf, out);
}

}  // namespace hm
