#include <map>
#include "codegen.h"

#include <cstdint>
#include <cstdio>
#include <cstring>
#include <vector>

namespace hm {
namespace {

// doubleBitsHex 把 IEEE-754 binary64 的**位模式**格式化成 16 位小写 hex 字符串。
//
// 与 Go 侧 `math.Float64bits(x)` + `%016x` 完全同形；schema 用
// ^[0-9a-f]{16}$ 钉死宽度/端序。IR 里绝不能放 JSON 浮点数字：V-7 扫 .eE、
// canon.FromAny 遇 json.Number 带小数点直接 ErrFloat、签名链更要求同一份字节。
static std::string doubleBitsHex(double x) {
  std::uint64_t u = 0;
  static_assert(sizeof(double) == sizeof(u), "IEEE-754 binary64 required");
  std::memcpy(&u, &x, sizeof(u));
  char buf[17] = {0};
  std::snprintf(buf, sizeof(buf), "%016llx", static_cast<unsigned long long>(u));
  return std::string(buf);
}

// Gen 是一次 workflow 的 codegen 状态机。
//
// 节点编号严格按 IR.md §4.2 的「节点编号规则」：**前序 DFS，父先于子**。
// 实现方式就是"先 alloc 父、再递归子、最后回填子 id"，与规则同形 ——
// 若改成"先算子再 alloc 父"，编号会整体后移，golden 立刻对不上，
// 而两边都是"合法 IR"，不会有任何检查报错。这就是那条规则必须写死的原因。
class Gen {
 public:
  // arena/diags 是 **warning 通道**（见 codegen.h 的说明）。两者都为空时这个
  // Gen 不产出任何诊断，行为与"只有 err"时逐字节相同 —— 这是刻意的：
  // 分析阶段（analyze_functions）和函数表编译不需要 warning，也不该因为
  // 多了一个通道就让产物变化。
  explicit Gen(std::string* err, Arena* arena = nullptr, DiagBag* diags = nullptr,
               const char* diag_module = nullptr)
      : err_(err), arena_(arena), diags_(diags), diag_module_(diag_module) {}

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
  // extern 声明表：name → 声明节点（FunctionDecl 或 WorkflowSig，is_extern）。
  // 裸调用 `sqrt(...)` 若命中此表，编成 call_native（而非 call_fn）。
  const std::map<std::string, const Node*>* extern_fns_ = nullptr;
  void set_extern_fns(const std::map<std::string, const Node*>* ex) { extern_fns_ = ex; }
  const Node* err_node() const { return err_node_; }
  // calls 是本 Gen 编过的调用边（caller → callee）。函数表需要它做递归判定。
  const std::map<std::string, std::vector<std::string>>& calls() const { return calls_; }

 private:
  // export signal 名单：只有监听它们的槽需要进全局表（跨单元可见）。
  // 普通局部信号的槽留在单元内，走编译期配对 —— 那条路径零开销。
  std::vector<const char*> exported_signals_;
  void collect_exported_signals(const Node* mod);
  bool is_exported_signal(const char* name) const;

  // ---- 编译期函数内联归约时的「当前块出口」----
  //
  // nodes_ 是扁平节点池，本身没有顺序语义；一条语句是否执行、按什么顺序执行，
  // 完全由 seq.kids 里的 id 引用决定。只 alloc 不挂 kids 的节点是孤儿，
  // 运行期永不执行。所以 gen_expr 期间要追加合成语句，必须能找到"当前正在
  // 生成的那个块"的 kids / kidsIds / span 累加器。
  //
  // 这组指针只在 case Kind::Block（以及归约器自己造的 seq）的栈帧内存活，
  // 必须在 std::move(kids) 之前恢复（嵌套块用 saved/restore，参照 inline_alias_）。
  struct BlockOut {
    JVal* kids = nullptr;
    std::vector<int>* kidsIds = nullptr;
    int* spanSum = nullptr;
  };

  // 归约器造的每个 seq 都要有同样的三件套，包成一个帧让 begin/end 成对使用。
  struct SeqFrame {
    int id = -1;
    JVal kids = JVal::array();
    std::vector<int> kidsIds;
    int sum = 0;
    BlockOut saved;
  };

  // 把一条合成语句挂到当前块；当前不在块里时返回 false（调用方必须报错，
  // 不能静默丢弃 —— 丢掉的语句会让结果变量永远停在零值）。
  bool emit_stmt(int id) {
    if (cur_block_.kids == nullptr) return false;
    cur_block_.kids->push(JVal::integer(id));
    cur_block_.kidsIds->push_back(id);
    *cur_block_.spanSum += span_[id];
    return true;
  }

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

  // emit_warn 追加一条**警告**诊断。字段顺序与 Lexer::emit_warn 完全一致
  // （severity/message/loc/module/why/fix），只多一个 fix2。
  //
  // 为什么 warning 必须走 DiagBag 而不是写进 err_：err_ 只有一个槽位，
  // 而且它的语义是"编译失败"。把警告塞进去，调用方要么当成失败（误报），
  // 要么忽略（静默丢掉警告）—— 两种都比不加这条警告更糟。
  //
  // 没有通道时**静默跳过**：调用方没给 DiagBag 就表示它不收集诊断。
  // 注意这里不能借用 fail() 报"内部错误"—— 警告不是失败。
  void emit_warn(const char* code, const char* msg, Loc loc, const char* why, const char* fix,
                 const char* fix2) {
    if (diags_ == nullptr || arena_ == nullptr) return;
    Diag dg;
    memset(&dg, 0, sizeof(dg));
    dg.severity = Severity::Warning;
    dg.code = code;
    dg.message = msg;
    dg.loc = loc;
    dg.module = diag_module_;
    dg.why = why;
    dg.fix = fix;
    dg.fix2 = fix2;
    diag_add(*diags_, *arena_, dg);
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
    // steps_ 与 nodes_ 一一对齐：最坏执行路径上的语句节点数（含 seq 自身）。
    // 默认 1（叶子语句 / 空 seq 的最坏步数）；复合节点（seq/if/for/while）在
    // 各自分支里覆盖。仅用于 for 循环的步数上界乘算，不干预路线 A 的
    // stmtNodes_+1 机制（branch 因此保持 max_steps==14）。
    steps_.push_back(1);
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

  // var_index 只负责"表里有没有这个名字"这一件事，**不再用于声明**。
  // 声明一律走 decl_var：遮蔽（内层同名声明）必须得到自己的变量槽，否则两个
  // 绑定共用一个 id，装配期只能把 vars[].decl 指向其中一个，校验器（V-5 反向）
  // 当场判它坏 IR —— 而源码是完全合法的。
  int var_index(const std::string& name);
  // decl_var **分配全新槽位**并压进当前作用域（名字可以重复）。
  int decl_var(const std::string& name);
  // ref_var 取**最内层**同名绑定（与 check_type.cpp::lookup_ 同一条规则：
  // 两处不一致就会编出"读到另一个变量"的 IR，而它完全合法、跑得通、结果是错的）。
  // 内联模式下查不到返回 -1（函数无闭包）；非内联时退回 var_index（input 等隐式根变量）。
  int ref_var(const std::string& name);
  // find_var 与 ref_var 同序查找但**不创建**，用于"这个名字有没有已知长度"这类查询。
  int find_var(const std::string& name) const;
  void push_scope() { scope_marks_.push_back(binds_.size()); }
  void pop_scope();
  // 路线 B：内联函数体时，变量名经 inline_locals_/inline_param_var_ 解析到
  // __hm_inline_l_N 局部变量（避免与工作流变量撞名）；非内联时退化为 var_index。
  // 2.0.0 起它就是 ref_var：作用域栈优先（内联体里的遮蔽也能正确解析）。
  int lookup_var(const std::string& name);
  // 路线 B：为一个源码局部变量名分配新的 __hm_inline_l_N 并登记映射。
  int declare_local(const std::string& name);
  int gen_stmt(const Node* s);
  int gen_expr(const Node* e);
  // gen_path_ 把 `a.b.c` 编成嵌套 field 节点（IR.md §4.2）。
  int gen_path_(const Node* e);
  // if/return 决策树归约：把一条语句链改写成「给结果变量赋值」的 seq。
  // head 是语句链首（next 是兄弟链），vid 是结果变量下标，at 是调用点，
  // fn 只用于诊断。
  int reduce_chain(const Node* head, int vid, const Node* at, const std::string& fn);
  // 路线 B：把含中间语句 / 循环 / 早退的一般函数体归约成内联 seq（call_fn==0）。
  // chain 是函数体语句链首，target 是 FunctionDecl，paramNames/argIds 是形参
  // 名与已编译的实参节点，at 是调用点，fn 只用于诊断。返回结果变量节点 id。
  int inline_body_b(const Node* chain, const Node* target,
                    const std::vector<std::string>& paramNames,
                    const std::vector<int>& argIds, int vid,
                    const Node* at, const std::string& fn);
  // 路线 A 的 if/return 决策树入口（复用 reduce_chain 生成「结果变量 + if」）。
  int inline_route_a_tree(const Node* chain, const Node* target,
                          const std::vector<std::string>& paramNames,
                          const std::vector<int>& argIds, const Node* at, const std::string& fn);
  // 形状判定：函数体是否纯 if/return 决策树（走路线 A）。与 reduce_chain 的
  // 接受条件严格一致（否则会挑中 reduce_chain 会拒绝的形状）。
  bool is_if_return_shape(const Node* head) const;
  // 诚实守卫：函数体是否每条路径都以 return 结束（否则会静默返回零值）。
  bool body_always_returns(const Node* head) const;
  // 路线 B 把函数体语句链归约成带「早退标志」守卫的内联 seq：每条语句之后用
  // `if (__early) {} else { <余下> }` 跳过已早退后的语句。返回包裹 seq 的节点 id。
  int reduce_general(const Node* head, const Node* at, const std::string& fn);
  // 归约器里每个 seq 都必须与 case Kind::Block 同构地切换 cur_block_：
  // 生成子语句期间若再触发内联，合成语句要排在本 seq 后续语句之前。
  void begin_seq(SeqFrame* f, const Node* at);
  int end_seq(SeqFrame* f);
  // 归约里出现的表达式必须是纯的（与实参纯度检查同一条理由：函数体里的
  // model/tool/memory/vector 调用必须进上界，不能悄悄漏算）。
  bool pure_expr(int id, const Node* at, const std::string& fn);

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
  std::vector<std::string> vars_;  // 变量表（声明顺序 = 下标；**同名可多槽**）
  // 源码作用域栈：名字 → 变量槽。内层同名声明压栈、出块出栈，引用一律取最内层。
  //
  // 为什么必须是栈而不是"重名就报错"：`catch (ModelError e)` 在两层 try 里各写
  // 一次是再正常不过的写法，`if (…) { auto x = …; }` 里的 x 也一样。语言文档
  // §5.2 写的是"独立词法作用域、出块即关闭"，类型检查器（check_type.cpp 的
  // scope_/lookup_）早就按遮蔽实现了；只有 codegen 把名字当主键，于是合法的源码
  // 会编出两个绑定共用一个槽的 IR，装配期回填 decl 时只能指向其中一个。
  std::vector<std::pair<std::string, int>> binds_;
  std::vector<std::size_t> scope_marks_;
  // 变量槽 → 定长数组长度。用于**编译期越界检查**：
  // `auto xs = [1,2,3]; xs[5]` 里的 5 是确定的越界，不该留到运行期。
  // 键是**槽位**不是名字：遮蔽之后同名变量是两件东西，长度也可能不同。
  std::map<int, int> arr_len_;
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
  void set_module(const Node* m) {
    module_ = m;
    exported_signals_.clear();
    if (m) collect_exported_signals(m);
  }
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
  // 路线 A 用这个（形参只读）；路线 B 改用 inline_param_var_（形参是可变局部变量）。
  std::map<std::string, int> inline_alias_;
  // 路线 B 内联参数：形参名 → 局部变量下标。与 inline_alias_ 互斥：路线 B 的
  // 函数体可能对形参赋值，所以形参要绑定成真正的局部可变变量。
  std::map<std::string, int> inline_param_var_;
  // 路线 B 内联函数体的局部变量：源码变量名 → 局部变量下标（__hm_inline_l_N）。
  // 改名避免与工作流变量撞名（函数体在 workflow 帧里原地展开）。
  std::map<std::string, int> inline_locals_;
  // 路线 B 标志：正在编内联函数体。为真时 gen_stmt/gen_expr 走内联语义
  // （return 改写成「赋值结果变量 + 置早退标志」、变量经 inline_param_var_/
  // inline_locals_ 解析）。
  bool reducing_ = false;
  // 路线 B 合成的结果变量与早退标志的下标。
  int inline_result_vid_ = -1;
  int inline_early_vid_ = -1;
  // 每个语句节点在「最坏执行路径」上的步数（含自身、含 seq 节点）。
  std::vector<int> steps_;
  // inline 状态整体快照 / 还原守卫：在退出（含错误早退）时把 inline 相关成员
  // 还原成进入前的值，与嵌套内联的快照/还原互不干扰。
  struct InlineGuard {
    Gen* g;
    std::map<std::string, int> alias, param, locals;
    // 作用域栈也要一起快照：内联体里的声明（含形参）属于**这次内联**，退出后
    // 必须消失，否则下一次内联 / 后面的 workflow 语句会解析到上一次的槽位。
    // 只存长度即可——栈只会被 append / resize，不会重排已有元素。
    std::size_t binds, marks;
    bool reducing;
    int rv, ev;
    InlineGuard(Gen* self)
        : g(self), alias(self->inline_alias_), param(self->inline_param_var_),
          locals(self->inline_locals_), binds(self->binds_.size()),
          marks(self->scope_marks_.size()), reducing(self->reducing_),
          rv(self->inline_result_vid_), ev(self->inline_early_vid_) {}
    ~InlineGuard() {
      g->inline_alias_ = std::move(alias);
      g->inline_param_var_ = std::move(param);
      g->inline_locals_ = std::move(locals);
      g->binds_.resize(binds);
      g->scope_marks_.resize(marks);
      g->reducing_ = reducing;
      g->inline_result_vid_ = rv;
      g->inline_early_vid_ = ev;
    }
  };
  const Node* err_node_ = nullptr;
  std::string cur_fn_;   // 正在编的函数名（空 = 正在编 workflow）
  std::map<std::string, std::vector<std::string>> calls_;
  bool in_fn_ = false;
  // 函数参数的静态类型。工作流的 vars[] 一律写 json（历史行为），
  // 函数的 params 有真实类型，vars[] 里不该退化成 json。
  std::map<std::string, JVal> var_types_;
  // 归约期间指向"当前正在生成的块"的 kids / kidsIds / span 累加器。
  BlockOut cur_block_;
  // 合成结果变量名 __hm_inline_r_<N> 的序号。前缀是编译器保留字，
  // 且符合 schemas/ir.schema.json 的 $defs.var.name 正则
  // （^[A-Za-z_][A-Za-z0-9_]*$；`$` 不合法，不能用 $hm_inline_r_）。
  // 因为前缀本身是合法标识符，生成时仍要扫 vars_ 防源码撞名。
  // vars[].decl 由装配阶段按绑定节点 id 回填，与名字无关。
  int inline_result_seq_ = 0;
  // 路线 B 合成的局部变量序号（__hm_inline_l_N）。单调递增，嵌套内联也不撞名。
  int inline_local_seq_ = 0;
  std::string* err_;
  // warning 通道（见构造函数）。arena_ 只用于 diag_add 的存储分配。
  Arena* arena_ = nullptr;
  DiagBag* diags_ = nullptr;
  const char* diag_module_ = nullptr;
};


// fnv1a64 是频道名的**编译期**哈希：`emit(S) on "orders"` 与
// `on(S) from "orders"` 在 IR 里是同一个 u64，运行期只做整数比较。
// 不用 std::hash：它的值不保证跨编译器/跨版本稳定，而 IR 是可存档的契约。
uint64_t fnv1a64(const char* s) {
  uint64_t h = 1469598103934665603ULL;
  for (const unsigned char* p = reinterpret_cast<const unsigned char*>(s ? s : ""); *p; ++p) {
    h ^= static_cast<uint64_t>(*p);
    h *= 1099511628211ULL;
  }
  return h;
}

// 收集 `export signal X;` 的名字：只有这些信号的槽需要进全局表（跨单元可见）。
void Gen::collect_exported_signals(const Node* mod) {
  for (const Node* c = mod->child; c; c = c->next) {
    if (c->kind == Kind::SignalDecl && c->is_export && c->text) {
      exported_signals_.push_back(c->text);
    }
  }
}

bool Gen::is_exported_signal(const char* name) const {
  if (!name) return false;
  for (const char* s : exported_signals_) {
    if (std::strcmp(s, name) == 0) return true;
  }
  return false;
}

JVal Gen::type_of(const Node* t) {
  JVal v = JVal::object();
  if (!t || !t->text) {
    v.set("k", JVal::string("json"));
    return v;
  }
  v.set("k", JVal::string(t->text));
  return v;
}

// is_builtin_type 是类型表里的 6 个基础类型（IR.md §3.1）。浮点类型名只有 `double`。
// 不在表里的就是本 unit 内的 struct 引用 —— 也可能是拼错的类型名，
// 但那属于类型检查（T11）的职责，codegen 不重复判。
bool is_builtin_type(const std::string& t) {
  return t == "string" || t == "int" || t == "double" ||
         t == "bool" || t == "json" || t == "cost" || t == "char" ||
         t == "short" || t == "long" ||
         // 2.0.1：void 只做函数返回类型（值类型里没有它，Sema 用 E4101 拦值用法）。
         // 不把它算进内建类型，装配阶段会把它当 struct 名字去查表，报
         // "unknown type void" —— 那句报错完全指不到真正的类型系统位置。
         t == "void";
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

// template_parts 把「字符串字面量（可插值）」翻成 IR 的结构化片段数组。
//
// url_template 与 idem.key 共用同一套形状（IR.md §3.4 的 urlTemplatePart）：
// 都是「编译期可见的模板 + 运行期按路径取值」，绝不把带 {} 的文本原样丢给
// 运行期再解析一次。抽成一个函数是为了防止两处实现各自漂移 —— 曾有过
// 检查器读了 AST、codegen 却按另一套槽位读的教训。
static JVal template_parts(const Node* v) {
  JVal tmpl = JVal::array();
  if (!v) return tmpl;
  if (v->n_subs == 0) {
    JVal part = JVal::object();
    part.set("lit", JVal::string(v->text ? v->text : ""));
    tmpl.push(std::move(part));
    return tmpl;
  }
  for (int i = 0; i < v->n_subs; ++i) {
    const Node* sp = &v->subs[i];
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
  return tmpl;
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
  // 隐式根变量（`input`）走这条路：它**没有声明点**，第一次被引用时才入表。
  // 不压 binds_：它不是任何作用域里的声明，出块也不该消失。
  vars_.push_back(name);
  return static_cast<int>(vars_.size()) - 1;
}

int Gen::decl_var(const std::string& name) {
  // 永远分配新槽：同名遮蔽得到独立槽位，两个绑定各有自己的 decl 节点。
  vars_.push_back(name);
  const int id = static_cast<int>(vars_.size()) - 1;
  binds_.push_back(std::make_pair(name, id));
  return id;
}

int Gen::find_var(const std::string& name) const {
  for (int i = static_cast<int>(binds_.size()) - 1; i >= 0; --i) {
    if (binds_[i].first == name) return binds_[i].second;
  }
  return -1;
}

int Gen::ref_var(const std::string& name) {
  const int id = find_var(name);
  if (id >= 0) return id;
  if (reducing_) {
    // 内联体的形参/局部变量经 declare_local 登记（同时压 binds_），下面两条
    // 只是兜底：路线 B 的 inline_param_var_ 与 inline_locals_。
    auto p = inline_param_var_.find(name);
    if (p != inline_param_var_.end()) return p->second;
    auto l = inline_locals_.find(name);
    if (l != inline_locals_.end()) return l->second;
    return -1;  // 函数无闭包：查不到就是未绑定引用，由调用方报错
  }
  return var_index(name);
}

void Gen::pop_scope() {
  if (scope_marks_.empty()) return;
  binds_.resize(scope_marks_.back());
  scope_marks_.pop_back();
}

// 支持范围变了，形状拒绝的文案必须如实描述（原文只说"单一 return"会撒谎）。
// reduce_chain 的所有形状失败共用这一句，便于诊断措辞保持一致。
static std::string inline_shape_error(const std::string& fn) {
  return "function `" + fn + "` cannot be inlined at compile time: compiled mode "
         "supports a single `return`, an if/return decision tree, or a statement "
         "sequence ending in one `return`; early returns nested inside branches or "
         "loops, and paths without a return, need a call frame — and non-recursive "
         "functions never get dynamic dispatch";
}

// 路线 B 不支持「早退」：出现在分支/循环里的 return 不是末尾那一条，归约会
// 把它错编成 workflow 级 return（提前退出整个 workflow，而不是这个函数）。
// 所以内联前先扫一次子树，含嵌套 return 的语句一律拒绝，宁可保守也不做错。
static bool stmt_subtree_has_return(const Node* s) {
  if (!s) return false;
  if (s->kind == Kind::ReturnStmt) return true;
  for (const Node* c = s->child; c; c = c->next) {
    if (stmt_subtree_has_return(c)) return true;
  }
  for (const Node* c = s->args; c; c = c->next) {
    if (stmt_subtree_has_return(c)) return true;
  }
  if (stmt_subtree_has_return(s->body)) return true;
  if (stmt_subtree_has_return(s->else_branch)) return true;
  for (int i = 0; i < s->n_subs; ++i) {
    if (stmt_subtree_has_return(&s->subs[i])) return true;
  }
  return false;
}

// 开始归约用的一个 seq 节点。alloc 之后立刻把 cur_block_ 切到自己的 kids：
// 期间 gen_expr 若触发嵌套内联，合成语句会排在本 seq 后续语句之前。
// seq 不 ++stmtNodes_，与 case Kind::Block 完全一致。
void Gen::begin_seq(SeqFrame* f, const Node* at) {
  f->id = alloc("seq", at);
  f->kids = JVal::array();
  f->kidsIds.clear();
  f->sum = 0;
  f->saved = cur_block_;
  cur_block_ = BlockOut{&f->kids, &f->kidsIds, &f->sum};
}

// 回填 kids 与三个上界数组；必须在 std::move(kids) 之前恢复 cur_block_。
int Gen::end_seq(SeqFrame* f) {
  cur_block_ = f->saved;
  nodes_[f->id].set("kids", std::move(f->kids));
  span_[f->id] = f->sum;
  int ms = 0;
  int ts = 0;
  for (const int kid : f->kidsIds) {
    ms += model_[kid];
    ts += tool_[kid];
  }
  model_[f->id] = ms;
  tool_[f->id] = ts;
  return f->id;
}

bool Gen::pure_expr(int id, const Node* at, const std::string& fn) {
  if (id < 0) return false;  // 诊断已由 gen_expr 给出，不要覆盖它
  if (span_[id] != 0 || model_[id] != 0 || tool_[id] != 0) {
    fail_at(at, "function `" + fn + "` cannot be inlined at compile time: its if/return "
               "body contains an impure expression (model/tool/memory/vector calls); "
               "compiled mode has to account for every call in the static bounds");
    return false;
  }
  return true;
}

// 把一条 if/return 语句链归约成「给结果变量 vid 赋值」的 seq。
//
// AST（parse_block / parse_if，parser.cpp:895-909 / 1043-1052）：
//   Block 的语句是 child 起始的 next 兄弟链；
//   IfStmt 的 cond 在 child、then Block 在 args、else 在 else_branch；
//   `else if` 时 else_branch 直接是另一个 IfStmt（不是 Block），两种都认。
//
// 关键： `if (c) { return A; } return 0;` 的链是 [IfStmt, ReturnStmt]，
// **必须**把 if 之后的语句链当作 else 分支。归约成
// `if (c) { $r = A; } $r = 0;` 会让 then 的结果被后面的赋值覆盖。
//
// 每个 seq 都与 case Kind::Block 同构地切换 cur_block_，因此嵌套内联
// （分支表达式里再调用另一个 if/return 函数）挂到的 kids 也是对的。
int Gen::reduce_chain(const Node* head, int vid, const Node* at, const std::string& fn) {
  if (!head) {
    fail_at(at, inline_shape_error(fn));
    return -1;
  }

  // 链首是 return：必须是链尾，否则后面还有语句就是不可归约的形状。
  if (head->kind == Kind::ReturnStmt) {
    if (head->next != nullptr) {
      fail_at(at, inline_shape_error(fn));
      return -1;
    }
    SeqFrame f;
    begin_seq(&f, at);
    const int e = gen_expr(head->child);
    if (e < 0 || !pure_expr(e, at, fn)) {
      cur_block_ = f.saved;
      return -1;
    }
    const int id = alloc("assign", head);
    ++stmtNodes_;
    nodes_[id].set("var", JVal::integer(vid));
    nodes_[id].set("e", JVal::integer(e));
    // 与 AssignStmt 同一条规则：assign 的 span/model/tool **恒等于 e 的**。
    // 这里有 pure_expr(e) 保证 e 是纯的（三个值都是 0），所以传播与写 0 等价 ——
    // 但写成传播，Go 侧的复算（bounds.go 的 assign 分支不看节点从哪来）就
    // 永远不可能与这里分岔：少一处"两边各写一个数"的地方，就少一处漂移。
    span_[id] = span_[e];
    model_[id] = model_[e];
    tool_[id] = tool_[e];
    if (!emit_stmt(id)) {
      cur_block_ = f.saved;
      fail_at(at, "internal error: inlined if/return reduction has no enclosing block");
      return -1;
    }
    return end_seq(&f);
  }

  // 链首是 if：整个链被消耗 —— 剩余语句（或显式 else）变成 else 分支。
  if (head->kind == Kind::IfStmt) {
    const Node* cond = head->child;
    const Node* thenBlk = head->args;
    const Node* elseNode = head->else_branch;
    if (!cond || !thenBlk) {
      fail_at(at, inline_shape_error(fn));
      return -1;
    }
    // 有 else 时 else 之后不许再有语句；没有 else 时后面必须还有语句
    // （每条路径都必须 return，否则运行期会读到零值而不是编译期报错）。
    if ((elseNode != nullptr && head->next != nullptr) ||
        (elseNode == nullptr && head->next == nullptr)) {
      fail_at(at, inline_shape_error(fn));
      return -1;
    }

    SeqFrame f;
    begin_seq(&f, at);
    const int c = gen_expr(cond);
    if (c < 0 || !pure_expr(c, at, fn)) {
      cur_block_ = f.saved;
      return -1;
    }
    const Node* thenChain = (thenBlk->kind == Kind::Block) ? thenBlk->child : thenBlk;
    const int t = reduce_chain(thenChain, vid, at, fn);
    if (t < 0) {
      cur_block_ = f.saved;
      return -1;
    }
    const Node* elseChain = nullptr;
    if (elseNode != nullptr) {
      elseChain = (elseNode->kind == Kind::Block) ? elseNode->child : elseNode;
    } else {
      elseChain = head->next;
    }
    const int e2 = reduce_chain(elseChain, vid, at, fn);
    if (e2 < 0) {
      cur_block_ = f.saved;
      return -1;
    }

    // 写全 c/then/elifs/else 是**工程约定**（与 codegen 现有 IfStmt 产物、
    // golden 一致最稳），不是 schema / 执行器要求：ir.schema.json 的 nodeIf
    // required 只有 id/k/loc/c/then，elifs/else 都是 optional，engine 也只在
    // Else != nil 时才走 else。这里不加任何运行时断言去卡这两个字段。
    //
    // span = 1 + max(分支)、model/tool 取分支最大值：if 是**语句**节点，
    // 分支互斥，所以取 max；表达式复合节点（binop/array_lit/parallel）一律
    // 取 sum。if 产生 1 个 trace span（docs/IR.md §5.1），记 0 会让运行期
    // 断言 len(spans) ≤ max_spans 误报。
    const int id = alloc("if", head);
    ++stmtNodes_;
    nodes_[id].set("c", JVal::integer(c));
    nodes_[id].set("then", JVal::integer(t));
    nodes_[id].set("elifs", JVal::array());
    nodes_[id].set("else", JVal::integer(e2));
    int best = span_[t];
    if (span_[e2] > best) best = span_[e2];
    span_[id] = 1 + best;
    int mbest = model_[t];
    if (model_[e2] > mbest) mbest = model_[e2];
    model_[id] = mbest;
    int tbest = tool_[t];
    if (tool_[e2] > tbest) tbest = tool_[e2];
    tool_[id] = tbest;
    if (!emit_stmt(id)) {
      cur_block_ = f.saved;
      fail_at(at, "internal error: inlined if/return reduction has no enclosing block");
      return -1;
    }
    return end_seq(&f);
  }

  // 其它语句（LetStmt / WhileStmt / ForStmt / ExprStmt / ...）：保持编译期拒绝。
  fail_at(at, inline_shape_error(fn));
  return -1;
}

// ---- 路线 B：一般函数体内联 ----
//
// 与路线 A 的区别：函数体不是纯 if/return 决策树，而是「若干语句 + 末尾一条
// return」。形参/局部变量会被改名成 __hm_inline_l_N 的 workflow 变量，否则
// 两个函数用同名局部、或与工作流变量撞名，都会串味。
//
// 步数诚实性：直线代码（decl/assign/expr/update）每个节点只执行一次，
// stmtNodes_+1 是精确上界；一旦出现循环/分支，同一节点会被重复执行而
// stmtNodes_ 只记一次 —— 此时把整个 workflow 标 unbounded（max_steps 不再是
// 承诺，bounds_test 对 Unbounded 跳过），绝不写一个偏小的有限上界。

// 解析一个源码变量名到局部变量下标；内联模式下只认形参/局部（函数无闭包）。
// 2.0.0 起统一走 ref_var：作用域栈优先，内联体里的同名遮蔽也能解析到内层。
int Gen::lookup_var(const std::string& name) {
  return ref_var(name);
}

// 为一个源码局部变量名分配新的 workload 级变量 __hm_inline_l_N，并登记映射。
int Gen::declare_local(const std::string& name) {
  std::string fresh;
  for (;;) {
    fresh = "__hm_inline_l_" + std::to_string(inline_local_seq_++);
    bool taken = false;
    for (const std::string& v : vars_) {
      if (v == fresh) { taken = true; break; }
    }
    if (!taken) break;
  }
  // 槽位用变量表里的**合成名**（__hm_inline_l_N 保证不与已有变量撞名），但作用域
  // 栈按**源码名**登记：内联体里再声明一次同名局部变量（遮蔽）时，内层必须解析到
  // 自己的槽，否则两处写同一个槽会串味，而 IR 完全合法、加载期没有任何检查会红。
  vars_.push_back(fresh);
  const int vid = static_cast<int>(vars_.size()) - 1;
  binds_.push_back(std::make_pair(name, vid));
  inline_locals_[name] = vid;
  return vid;
}

// 形状判定：与 reduce_chain 的接受条件严格一致，用来在 A/B 之间选路。
bool Gen::is_if_return_shape(const Node* head) const {
  if (!head) return false;
  if (head->kind == Kind::ReturnStmt) return head->next == nullptr;
  if (head->kind != Kind::IfStmt) return false;
  const Node* cond = head->child;
  const Node* thenBlk = head->args;
  const Node* elseNode = head->else_branch;
  if (!cond || !thenBlk) return false;
  if ((elseNode != nullptr && head->next != nullptr) ||
      (elseNode == nullptr && head->next == nullptr)) {
    return false;
  }
  const Node* thenChain = (thenBlk->kind == Kind::Block) ? thenBlk->child : thenBlk;
  if (!is_if_return_shape(thenChain)) return false;
  const Node* elseChain = (elseNode != nullptr)
                              ? ((elseNode->kind == Kind::Block) ? elseNode->child : elseNode)
                              : head->next;
  return is_if_return_shape(elseChain);
}

// 把「语句链 + 末尾 return」归约成一个内联 seq：形参绑定 + 各语句 + 结果赋值。
// 返回该 seq 的节点 id；调用方负责 emit 到当前块（并补 +1 wrapper 步）。
int Gen::inline_body_b(const Node* chain, const Node* target,
                       const std::vector<std::string>& paramNames,
                       const std::vector<int>& argIds, int vid,
                       const Node* at, const std::string& fn) {
  (void)target;
  InlineGuard guard(this);  // 退出（含错误早退）时还原 inline 状态
  inline_alias_.clear();
  inline_param_var_.clear();
  inline_locals_.clear();
  reducing_ = true;

  SeqFrame f;
  begin_seq(&f, at);

  // 1) 形参绑定成可被函数体赋值的局部变量：decl __hm_inline_l_N = <实参>;
  for (std::size_t i = 0; i < paramNames.size(); ++i) {
    const int pv = declare_local(paramNames[i]);
    const int letId = alloc("decl", at);
    ++stmtNodes_;
    nodes_[letId].set("var", JVal::integer(pv));
    nodes_[letId].set("e", JVal::integer(argIds[i]));
    // 合成绑定（内联函数的形参副本）没有源码 const 语义 → 恒 false。
    // 2.0.0 起 IR 用 `const` 表示不可变（对应源码 `const auto`），不再写 `mut`。
    nodes_[letId].set("const", JVal::boolean(false));
    span_[letId] = 0;
    model_[letId] = 0;
    tool_[letId] = 0;
    if (!emit_stmt(letId)) {
      cur_block_ = f.saved;
      fail_at(at, "internal error: route B has no block for a parameter binding");
      return -1;
    }
  }

  // 2) 必须恰好以一条末尾 return 收尾（否则会静默返回零值）。
  const Node* last = chain;
  while (last && last->next) last = last->next;
  if (last == nullptr || last->kind != Kind::ReturnStmt) {
    cur_block_ = f.saved;
    fail_at(at, inline_shape_error(fn));
    return -1;
  }

  bool unboundedBody = false;
  for (const Node* s = chain; s; s = s->next) {
    if (s->kind == Kind::ReturnStmt) {
      if (s != last) {  // 早退：return 之后还有语句，本子集不支持
        cur_block_ = f.saved;
        fail_at(at, inline_shape_error(fn));
        return -1;
      }
      const int e = gen_expr(s->child);
      if (e < 0) { cur_block_ = f.saved; return -1; }
      const int asg = alloc("assign", s);
      ++stmtNodes_;
      nodes_[asg].set("var", JVal::integer(vid));
      nodes_[asg].set("e", JVal::integer(e));
      // 与 AssignStmt 同一条规则（span/model/tool 恒等于 e 的）。
      //
      // 这一处**不是**形式上的统一：路线 B 的末尾 return 表达式没有过
      // pure_expr 检查，所以 `return parallel { … };` 这种带 span 的右值
      // 在旧代码里会被记成 0 —— 那是和 AssignStmt 一模一样的假上界。
      span_[asg] = span_[e];
      model_[asg] = model_[e];
      tool_[asg] = tool_[e];
      if (!emit_stmt(asg)) {
        cur_block_ = f.saved;
        fail_at(at, "internal error: route B has no block for the result assign");
        return -1;
      }
      continue;
    }
    if (stmt_subtree_has_return(s)) {
      // 早退不在本子集：拒绝，而不是错编成 workflow 级 return。
      cur_block_ = f.saved;
      fail_at(at, inline_shape_error(fn));
      return -1;
    }
    if (s->kind == Kind::MatchStmt || s->kind == Kind::TryStmt ||
        s->kind == Kind::RetryStmt) {
      cur_block_ = f.saved;
      fail_at(at, inline_shape_error(fn));
      return -1;
    }
    if (s->kind == Kind::WhileStmt || s->kind == Kind::ForStmt ||
        s->kind == Kind::IfStmt) {
      // 分支/循环会让节点重复执行，而 stmtNodes_ 只记一次 —— 上界不再精确。
      unboundedBody = true;
    }
    const int sid = gen_stmt(s);
    if (sid < 0) { cur_block_ = f.saved; return -1; }
    if (!emit_stmt(sid)) {
      cur_block_ = f.saved;
      fail_at(at, "internal error: route B has no block for a statement");
      return -1;
    }
  }
  if (unboundedBody) unbounded_ = true;
  return end_seq(&f);
}

// gen_expr 只认三类：字面量、变量引用、插值。
// 其余一律失败 —— 见头文件最后一段的理由。
int Gen::gen_expr(const Node* e) {
  if (!e) {
    fail("expression is empty");
    return -1;
  }

  // `unlimited` 只在 model.agent 的 max_steps 位置有意义，而那个位置由下面的
  // agent 分支**直接读参数节点**（见 agent 分支里的 max_steps 处理），不会走到
  // 这里。所以走到这里就说明它被当成了值用 —— 明确拒绝，而不是让它掉进兜底的
  // "尚不支持这种表达式"里（那条消息说的是"还没实现"，而这里是"永远不合法"，
  // 两者对用户意味着完全不同的下一步）。
  if (e->kind == Kind::UnlimitedExpr) {
    fail_at(e, "`unlimited` is not a value: it may only appear as the upper bound in "
               "model.agent(max_steps = unlimited); to run until a condition is met, use a while loop with an explicit condition");
    return -1;
  }

  if (e->kind == Kind::LitExpr) {
    const std::string t = e->text ? e->text : "string";

    // E4029 的**纵深防线**（v2.0.0 起不再是「所有浮点字面量被拒」）。
    // 正常路径：parser 给浮点字面量打 "double" 标签，下面发位模式。这里只拦
    // 旧的 "float" 标签 —— hm_codegen_* 系列入口**不因 DiagBag 里有 error 而
    // 短路**：诊断只是并进 out_diag，codegen 照跑、rc 仍是 0。直接调 C ABI 的
    // 宿主可以忽略 diag、拿走 IR；若旧标签漏进来，兜底会写出
    // {"t":"string","v":"float"}（合法、全校验过、运行期返回字符串 "float"）。
    // 宁可 rc=1，也不让静默损坏流出编译器。三道扫描器结构原样保留。
    if (t == "float") {
      fail_at(e, "legacy float label has no IR value representation (E4029): since "
                 "v2.0.0 floating literals are tagged \"double\" and emitted as the "
                 "bit pattern {\"k\":\"double\",\"bits\":\"<16 lowercase hex>\"}. "
                 "An old \"float\" tag would fall into the string fallback and write "
                 "the type name as the value, producing IR that passes every check "
                 "but returns \"float\" at runtime. This is a defense line, not a "
                 "ban on double literals.");
      return -1;
    }

    // double 字面量 → 位模式。必须放在末尾的字符串兜底之前，否则会编出
    // {"t":"string","v":"double"}。bits 是 16 位小写 hex 的 ASCII 字符串，
    // 不是 JSON 数字，V-7 / canonical / 签名都不受影响。
    if (t == "double") {
      const int id = alloc("lit", e);
      nodes_[id].set("t", JVal::string("double"));
      JVal bits = JVal::object();
      bits.set("k", JVal::string("double"));
      bits.set("bits", JVal::string(doubleBitsHex(e->f64)));
      nodes_[id].set("v", std::move(bits));
      return id;
    }

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
        // 按**槽位**取长度：同名遮蔽的两个数组长度可能不同，按名字取会拿到
        // 另一层的长度，于是越界的写被判成合法（或反之）。
        const int vid = find_var(e->child->subs[0].text);
        auto it = vid >= 0 ? arr_len_.find(vid) : arr_len_.end();
        if (it != arr_len_.end()) n = it->second;
      }
      if (n >= 0 && (e->args->i64 < 0 || e->args->i64 >= n)) {
        fail("array index out of range: length " + std::to_string(n) + ", index " +
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
      // 路线 A 内联形参：直接复用实参节点（实参已在调用点验证为纯表达式）。
      return al->second;
    }
    if (reducing_) {
      // 路线 B：形参 / 局部变量都绑定成真正的局部可变变量，引用时生成 var 节点。
      // 先查作用域栈（最内层同名绑定）：内联体里再声明一次同名局部变量（遮蔽）
      // 时，裸引用必须解析到内层那个槽。
      const int bound = find_var(nm);
      if (bound >= 0) {
        const int id = alloc("var", e);
        nodes_[id].set("var", JVal::integer(bound));
        return id;
      }
      auto pv = inline_param_var_.find(nm);
      if (pv != inline_param_var_.end()) {
        const int id = alloc("var", e);
        nodes_[id].set("var", JVal::integer(pv->second));
        return id;
      }
      auto lv = inline_locals_.find(nm);
      if (lv != inline_locals_.end()) {
        const int id = alloc("var", e);
        nodes_[id].set("var", JVal::integer(lv->second));
        return id;
      }
      // 函数体里引用的变量必须是形参或局部变量；否则是未绑定引用。
      fail_at(e, "inlined function refers to `" + nm +
                   "`, which is neither a parameter nor a local variable");
      return -1;
    }
    const int id = alloc("var", e);
    // 取最内层同名绑定（遮蔽）。input 这类隐式根变量没有声明点，ref_var 会
    // 在非内联模式下退回 var_index —— 与 2.0.0 之前逐字相同的行为。
    nodes_[id].set("var", JVal::integer(ref_var(nm)));
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
      fail("parallel has no return [..]: the number of joined values must be written explicitly");
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

  if (e->kind == Kind::FallbackExpr) {
    // `main_expr fallback { ... }`：错误边界，不是异常。
    //
    // IR 形状与 `try` 同构：两条路径都是普通节点，执行器先跑 main，
    // 拿到非零状态码再跑 body。**没有 throw、没有栈展开** —— 失败是返回值。
    //
    // 编号规则与 parallel 一致：先 alloc 父，再按 main → body 递归，
    // 保证同一份源码每次编译出同样的字节。
    const int id = alloc("fallback", e);
    const int main = gen_expr(e->child);
    if (main < 0) return -1;
    const int body = gen_stmt(e->args);
    if (body < 0) return -1;
    nodes_[id].set("main", JVal::integer(main));
    nodes_[id].set("body", JVal::integer(body));
    // 预算取**相加**不取 max：出错时 main 可能已经花掉一部分预算
    // （比如模型调了、工具超时了），然后 body 再跑一遍。取 max 会低估
    // 上界，而低估的上界会让运行期把合法行为判成超界。
    //
    // `+1` 是**这个节点自己**算一 span，与 try 一侧同款（bounds.go 的
    // `case "try": v = 1`）。漏掉它会被 E_IR_RECOMPUTE_DRIFT 当场抓住 ——
    // 那正是派生字段复算存在的意义。
    span_[id] = 1;
    span_[id] += span_[main] + span_[body];
    model_[id] = model_[main] + model_[body];
    tool_[id] = tool_[main] + tool_[body];
    steps_[id] = steps_[main] + steps_[body];
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
      fail("call is missing a callee");
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
      if (params.size() != 1) { fail("len() requires exactly one argument"); return -1; }
      const std::vector<const Node*> vals = children_of(params[0]);
      if (vals.empty()) { fail("len() is missing its argument expression"); return -1; }
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
        // ---- 编译期展开 ----
        // 两种可接受形态：
        //   ① 函数体恰好一条 `return <expr>;`（原有路径，产物字节不变）；
        //   ② if/return 决策树：每条路径都以 `return <expr>;` 结束，
        //      归约成「结果变量 + if 语句」，见 Gen::reduce_chain。
        const Node* body = target->body;
        const Node* only = (body && body->kind == Kind::Block) ? body->child : nullptr;
        const Node* retExpr = nullptr;
        if (only != nullptr && only->next == nullptr && only->kind == Kind::ReturnStmt) {
          retExpr = only->child;
        }
        // 非单一 return 不再立刻拒绝：交给 reduce_chain 判定是不是可归约的
        // if/return 决策树（形状不符时由它给出同一条 E_IR_CODEGEN 诊断）。
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
        if (retExpr != nullptr) {
          const int inlined = gen_expr(retExpr);
          inline_alias_ = saved;
          return inlined;
        }

        // ---- 形态 ③：void 函数 -------------------------------------------
        // void 函数**没有值**，所以不能合成结果变量，也不该产生 insertvalue /
        // ret 缝合。做法是把函数体的语句直接展开进当前块，整体包成一个 seq：
        // 调用点要的是一个"被执行了"的表达式节点，seq 正好是这个意思。
        //
        // 裸 `return;` 在内联后等于"这一支到此为止"，直接跳过 —— 生成 ret 会
        // 把外层的 workflow 提前结束，那是 stop 的语义，不是 void 的。
        {
          const Node* rtNode = target->type_args;
          const char* rtText = (rtNode && rtNode->text) ? rtNode->text : "";
          if (std::strcmp(rtText, "void") == 0) {
            if (body == nullptr || body->kind != Kind::Block) {
              inline_alias_ = saved;
              fail_at(e, "void function `" + fn + "` has no body to inline");
              return -1;
            }
            SeqFrame f;
            begin_seq(&f, e);
            for (const Node* st = body->child; st; st = st->next) {
              if (st->kind == Kind::ReturnStmt) {
                if (st->child != nullptr) {
                  // Sema 已经用 E4103 拦过；这里是纵深防线，不静默吞。
                  cur_block_ = f.saved;
                  inline_alias_ = saved;
                  fail_at(e, "void function `" + fn + "` returns a value");
                  return -1;
                }
                continue;
              }
              if (gen_stmt(st) < 0) {
                cur_block_ = f.saved;
                inline_alias_ = saved;
                return -1;
              }
            }
            const int seqId = end_seq(&f);
            inline_alias_ = saved;
            return seqId;
          }
        }

        // ---- 形态 ②：if/return 决策树 ----
        // 归约后表达式位置引用合成结果变量 __r；为它写值的语句必须追加到
        // **当前正在生成的块**，排在引用它的那条语句之前。
        //
        // 合成节点（decl/seq/assign/if）的 id 会大于当前语句节点、执行顺序却
        // 在前 —— 这是 IR.md §4.2「节点编号 = 前序 DFS + 固定字段序」的
        // **编译器合成节点例外**：执行顺序由 seq.kids 的 id 引用决定，编号
        // 只保证同一源码仍编出同样字节（确定性不受影响）。
        const Node* retType = target->type_args;
        const std::string rt = (retType && retType->text) ? retType->text : "";
        // double 是浮点值，没有零值定义用的标量零值合成规则；json/struct
        // 没有零值定义，数组类型也没有 —— 一律保持拒绝，不猜。
        // 诊断里的类型名要带上数组维数：parser 把维数放在 type_args->i64，
        // text 只有基类型名（"int"），不处理会把 int[] 错打成 int。
        const std::string rtShown =
            (retType != nullptr && retType->i64 > 0) ? rt + "[]" : rt;
        if (retType == nullptr || retType->i64 != 0 ||
            (rt != "int" && rt != "cost" && rt != "bool" && rt != "string")) {
          inline_alias_ = saved;
          fail_at(e, "function `" + fn + "` cannot be inlined at compile time: return type `" +
                     rtShown + "` has no zero value for a synthesized result variable "
                     "(compiled mode supports int, cost, bool, string)");
          return -1;
        }
        // __hm_inline_r_ 是编译器保留前缀（见成员区注释）：合法标识符、
        // 符合 ir.schema.json 的 var.name 正则，但仍可能与源码变量同名 ——
        // 撞了就继续递增序号。var_index 在名字不存在时 push 并返回下标，
        // 恒 >= 0（它不返回 -1；-1 是 input 的哨兵，不是错误码）。
        std::string rname;
        for (;;) {
          rname = "__hm_inline_r_" + std::to_string(inline_result_seq_++);
          bool taken = false;
          for (const std::string& v : vars_) {
            if (v == rname) { taken = true; break; }
          }
          if (!taken) break;
        }
        const int vid = var_index(rname);

        // decl __r = <零值>;（const:false；span/model/tool 全 0，与 LetStmt 模板同构）
        const int letId = alloc("decl", e);
        ++stmtNodes_;
        const int zero = alloc("lit", e);
        if (rt == "bool") {
          nodes_[zero].set("t", JVal::string("bool"));
          nodes_[zero].set("v", JVal::boolean(false));
        } else if (rt == "string") {
          nodes_[zero].set("t", JVal::string("string"));
          nodes_[zero].set("v", JVal::string(""));
        } else {
          nodes_[zero].set("t", JVal::string(rt));
          nodes_[zero].set("v", JVal::integer(0));
        }
        nodes_[letId].set("var", JVal::integer(vid));
        nodes_[letId].set("e", JVal::integer(zero));
        // 合成绑定（内联归约的零值累加器）没有源码 const 语义 → 恒 false。
        nodes_[letId].set("const", JVal::boolean(false));
        span_[letId] = 0;
        model_[letId] = 0;
        tool_[letId] = 0;
        if (!emit_stmt(letId)) {
          inline_alias_ = saved;
          fail_at(e, "internal error: inlined function `" + fn +
                     "` has no enclosing block for its synthesized decl");
          return -1;
        }

        // decl 先挂、归约出的 seq 后挂；两者都排在引用 __r 的当前语句之前。
        const Node* chain = (body && body->kind == Kind::Block) ? body->child : nullptr;
        int seqId;
        if (is_if_return_shape(chain)) {
          // 路线 A：纯 if/return 决策树。
          seqId = reduce_chain(chain, vid, e, fn);
        } else {
          // 路线 B：语句序列 + 末尾 return。形参/局部改名成 workflow 变量；
          // 含循环/分支时 inline_body_b 会把整条 workflow 标 unbounded。
          seqId = inline_body_b(chain, target, paramNames, argIds, vid, e, fn);
        }
        if (seqId < 0 || !emit_stmt(seqId)) {
          inline_alias_ = saved;
          if (seqId >= 0) {
            fail_at(e, "internal error: inlined function `" + fn +
                       "` has no enclosing block for its synthesized chain");
          }
          return -1;
        }
        // 关键上界修正：reduce_chain 为每个内联调用点向当前块 emit 一个
        // **无条件执行**的 wrapper seq（归约出的 if 链被包成一个 seq）。这个
        // seq 没有互斥的兄弟语句来抵消它在 stmtNodes_ 里的缺额 —— 普通
        // if/else 的 branch seq 未计步，恰好被「未执行的另一分支里已计步的
        // 语句」抵消，长期没暴露；但 wrapper seq 总是执行，净欠 1 步。
        // 引擎 run.go 的 stmt() 对每个语句节点（含递归进入的 seq）逐节点计步，
        // 所以这里补恰好一次 ++stmtNodes_，使 max_steps = stmtNodes_ + 1 与
        // 运行期步数同源。路线 B 若引入早退标志 / 循环 / 其它无条件 seq，
        // 抵消关系会变，必须同步重算这里（以及任何新的 emit 点）。
        ++stmtNodes_;
        const int resultId = alloc("var", e);
        nodes_[resultId].set("var", JVal::integer(vid));
        inline_alias_ = saved;
        return resultId;
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

    // ---- extern（C ABI，call_native）----
    // 与原生 C 符号直调：不经过解释器、不经运行期 dlsym，符号体由链接期解析。
    // 与原生 user-function 的 call_fn 同形（args 按形参顺序、fn 为符号名），但 k=call_native，
    // 且**不**把符号加入 functions[]（符号体在 native 归档里，链接期解析）。
    // 含它的 workflow 必须标 unbounded（终止性不可证，见 EXTERN-C-ABI.md §4.2）。
    if (ns.empty() && extern_fns_ && extern_fns_->count(fn) != 0) {
      const Node* target = extern_fns_->at(fn);
      int want = 0;
      for (const Node* p = target->child; p; p = p->next) {
        if (p->kind == Kind::Param) ++want;
      }
      if (static_cast<int>(params.size()) != want) {
        fail_at(e, "extern function " + fn + " takes " + std::to_string(want) +
                   " argument(s), got " + std::to_string(params.size()));
        return -1;
      }
      const int id = alloc("call_native", e);
      ++stmtNodes_;
      std::vector<int> argIds;
      for (const Node* p : params) {
        if (p->text) {
          fail_at(e, "extern functions take positional arguments only: call `" + fn +
                     "(...)` without `" + std::string(p->text) + " =`");
          return -1;
        }
        const std::vector<const Node*> vals = children_of(p);
        if (vals.empty()) {
          fail_at(e, "call to extern " + fn + " has an argument without an expression");
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
      // ret：返回类型表达式（标量 / @opaque struct 指针）。form B 在 type_args，
      // form A（extern workflow）也在 type_args 存返回类型。
      nodes_[id].set("ret", fn_type_expr(target->type_args));
      // arg_types：按形参顺序的**静态类型表**。args 只存节点 id，AOT 的 C
      // 后端要生成编译期写死的 extern 原型，必须知道每个形参的 C 类型；
      // 没有这张表就只能做运行期类型分发（红线禁止）。
      // form A（WorkflowSig）类型在 Param::child，form B（FunctionDecl）在
      // Param::type_args —— 与 check_type.cpp::extern_param_type_ 同一条槽位约定。
      // 跨文件注入的 extern 节点已还原成带 Param 链的 AST，同样走这条路径。
      JVal at = JVal::array();
      for (const Node* p = target->child; p; p = p->next) {
        if (p->kind != Kind::Param) continue;
        const Node* ty = nullptr;
        if (p->child && p->child->kind == Kind::Type) ty = p->child;
        else if (p->type_args && p->type_args->kind == Kind::Type) ty = p->type_args;
        at.push(fn_type_expr(ty));
      }
      nodes_[id].set("arg_types", std::move(at));
      // 与原生 call_fn 一致：边界一次 span，整个 workflow 标 unbounded。
      span_[id] = 1;
      unbounded_ = true;
      return id;
    }

    // `json.as_int/as_string/as_bool/as_float/j(o)` —— 语言里唯一能引入
    // 运行时类型错误的地方（LANGUAGE §5.6）。IR 里是 `cast` 节点（schema 早已定义），
    // 而 codegen/engine 之前都没实现：写出来就是"不支持的构造"，不是静默降级。
    if (ns == "json" && fn.rfind("as_", 0) == 0) {
      // `json.as_float` 是 API 名（标识符）保留原名；它窄化到的**类型**现在是
      // double，所以 cast 节点的 to 必须写成 "double"，不能把 API 名当类型名。
      const std::string to = fn.substr(3);  // int / string / bool / float (API 名)
      if (to != "int" && to != "string" && to != "bool" && to != "float") {
        fail("unknown json narrowing: json." + fn);
        return -1;
      }
      if (params.size() != 1) {
        fail("json." + fn + " requires exactly one argument");
        return -1;
      }
      const std::vector<const Node*> vals = children_of(params[0]);
      if (vals.empty()) {
        fail("json." + fn + " is missing its argument expression");
        return -1;
      }
      const int inner = gen_expr(vals[0]);
      if (inner < 0) return -1;
      const int id = alloc("cast", e);
      JVal t = JVal::object();
      const std::string castTo = (to == "float") ? "double" : to;
      t.set("k", JVal::string(castTo));
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
      // unlimited 的三件事：一个标记、warning 的位置（`unlimited` 这个词本身
      // 的位置，而不是整条调用 —— 诊断要指向用户改得动的那几个字符）、
      // 以及"写了两次"的判定依据。
      bool unlimitedSteps = false;
      const Node* unlimitedAt = nullptr;
      std::vector<std::string> toolNames;
      for (const Node* p : params) {
        const std::string name = p->text ? p->text : "";
        const std::vector<const Node*> vals = children_of(p);
        const Node* val = vals.empty() ? nullptr : vals[0];
        if (name == "goal") {
          if (!val) { fail("model.agent goal requires an expression"); return -1; }
          goal = gen_expr(val);
          if (goal < 0) return -1;
        } else if (name == "max_steps") {
          if (val != nullptr && val->kind == Kind::UnlimitedExpr) {
            // `max_steps = unlimited`：编得出来，但代价必须说出来 —— 见下面的 W2004。
            // 同一个键写两次是真歧义（写的人以为后一次覆盖前一次，而"覆盖"对
            // "有界/无界"这个二分没有意义），所以明确拒绝混写。
            if (maxSteps != 0) {
              fail_at(val, "model.agent max_steps was given both a constant and unlimited: "
                           "the upper bound must be either provable or explicitly absent; the two cannot both hold");
              return -1;
            }
            unlimitedSteps = true;
            unlimitedAt = val;
            continue;
          }
          if (unlimitedSteps) {
            fail_at(p, "model.agent max_steps was given both unlimited and a constant: "
                       "the upper bound must be either provable or explicitly absent; the two cannot both hold");
            return -1;
          }
          if (!val || val->kind != Kind::LitExpr || !val->text ||
              std::string(val->text) != "int") {
            fail("model.agent max_steps must be an integer literal or unlimited"
                 " (a constant keeps the upper bound provable; unlimited states that there is no upper bound)");
            return -1;
          }
          maxSteps = static_cast<int>(val->i64);
        } else if (name == "max_parallel_tools") {
          if (!val || val->kind != Kind::LitExpr || !val->text ||
              std::string(val->text) != "int") {
            fail("model.agent max_parallel_tools must be an integer literal");
            return -1;
          }
          maxParTools = static_cast<int>(val->i64);
        } else if (name == "tools") {
          if (!val || val->kind != Kind::ArrayLit) {
            fail("model.agent tools must be an array of strings, e.g. [\"orders\"]");
            return -1;
          }
          for (const Node* el = val->child; el; el = el->next) {
            const std::string lit = (el->kind == Kind::LitExpr) ? lit_text(el) : "";
            if (lit.empty()) {
              fail("model.agent tools elements must be string literals");
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
          fail("model.agent does not recognize parameter " + name);
          return -1;
        }
      }
      if (goal < 0) { fail("model.agent is missing goal"); return -1; }
      // unlimited 时 maxSteps 保持 0，**不能**让下面那条 `> 0` 检查把它判死；
      // 反过来，没写 max_steps 时它也必须是 0，所以这条检查仍然必要。
      if (!unlimitedSteps && maxSteps <= 0) {
        fail("model.agent max_steps must be > 0 (it is the loop upper bound and the basis for reserving budget)");
        return -1;
      }
      JVal spec = JVal::object();
      spec.set("goal", JVal::integer(goal));
      JVal toolsArr = JVal::array();
      for (const auto& t : toolNames) toolsArr.push(JVal::string(t));
      spec.set("tools", std::move(toolsArr));
      if (!unlimitedSteps) spec.set("max_steps", JVal::integer(maxSteps));
      if (maxParTools > 0) spec.set("max_parallel_tools", JVal::integer(maxParTools));
      nodes_[id].set("agent", std::move(spec));
      // 上界：每步最多一次模型 + 一次工具。
      const int k = maxParTools > 0 ? maxParTools : 1;
      if (unlimitedSteps) {
        // ---- unlimited：**不写 max_steps 键** ----
        //
        // 三个选择，只有一个是诚实的：
        //   ① 写 null  → IR 里 agent.max_steps 的类型是 int，"没有"要由**缺失**
        //      表达；null 是另一种形状，读的人得为它多写一个分支。
        //   ② 写一个大数（比如 1e6）→ 撒谎，而且它会被 Go 侧 validate 的
        //      `max_spans ≤ 64*max_steps+1` 与 RecomputeMaxSpans 的一致性检查
        //      判成漂移：一个假的步数上界等于给整份 IR 埋了一个假前提。
        //   ③ 省略这个键 → Go 侧把"缺失"读成"没有静态上界"，正是我们要说的意思。
        //
        // 所以走 ③。warning 也在这里发：它是这个决定唯一的对外解释。
        emit_warn("W2004",
                  "max_steps = unlimited: this agent no longer has a statically known upper bound on steps",
                  unlimitedAt ? unlimitedAt->loc : e->loc,
                  "Two things are no longer provable: (1) max_spans of this workflow is no longer a provable "
                  "upper bound on trace length (how many steps the agent runs depends on the model and is not visible at compile time); (2) the budget cannot be reserved "
                  "statically - the cost upper bound must use the worst case, and the worst case is now unknown. So the cost of this workflow"
                  " has only one answer - unknown, not a known large value: do not use the numbers in the output "
                  "for budgeting or capacity planning.",
                  "To get a provable cost upper bound, use a constant: max_steps = 4.",
                  "If you really need to run to completion, keep unlimited - but accept that `hermes explain` will show"
                  " unknown here instead of a number.");
        // workflow 整体标 unbounded：与 while 语句同一条路径（含 IR 的
        // "unbounded": true）。这不是装饰 —— 它告诉读 IR 的人"bounds 里那些数字
        // 不全是可证明的上界"。
        unbounded_ = true;
      }
      // unlimited 的 span/model/tool 上界：**照 while 语句的做法**（codegen 的
      // while 分支：上界只按「一次迭代」记，绝不乘一个假想上限）。
      // 乘一个假的 maxSteps 会立刻撞上 Go 侧 validate 的
      // `max_spans ≤ 64*max_steps+1`，那条检查本来就是用来抓"编了个假上界"的。
      // 所以把"步数"取 1：一次模型调用 + 最多 k 次工具调用，与有界情况
      // maxSteps=1 时逐字相同 —— 一个先例，不是两个。
      const int stepsForBounds = unlimitedSteps ? 1 : maxSteps;
      span_[id] = (1 + k) * stepsForBounds;
      model_[id] = stepsForBounds;
      tool_[id] = k * stepsForBounds;
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
      // 2.0.1 事务内建：checkpoint 无参、rollback/commit 收一个快照。
      // 它们产出/消费的 Snapshot 是不透明值，所以不进 store_op（那是 KV 存储），
      // 单独一个 mem_txn 节点，运行期据此切事务边界。
      if (ns == "memory" && (op == "checkpoint" || op == "rollback" || op == "commit")) {
        const int id = alloc("mem_txn", e);
        ++stmtNodes_;
        nodes_[id].set("op", JVal::string(op));
        if (op != "checkpoint") {
          const Node* val = nullptr;
          for (const Node* p : params) {
            const std::vector<const Node*> vals = children_of(p);
            if (!vals.empty()) { val = vals[0]; break; }
            if (p->args != nullptr) { val = p->args; break; }
          }
          if (val == nullptr) { fail("memory." + op + " needs a snapshot"); return -1; }
          const int v = gen_expr(val);
          if (v < 0) return -1;
          nodes_[id].set("e", JVal::integer(v));
        }
        span_[id] = 0;
        model_[id] = 0;
        tool_[id] = 0;
        return id;
      }
      bool ok =
          (ns == "memory" && (op == "get" || op == "put" || op == "delete")) ||
          (ns == "vector" && (op == "put" || op == "query" || op == "delete"));
      if (!ok) {
        fail("unknown storage builtin " + ns + "." + op +
             " (memory.get / memory.put / memory.delete / vector.put / vector.query / vector.delete)");
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
        if (val == nullptr) { fail("storage builtin parameter " + name + " has no value"); return -1; }
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
            fail("vector.query k must be an integer literal");
            return -1;
          }
          args.set("k", JVal::integer(val->i64));
        } else {
          fail("storage builtin does not support parameter " + name);
          return -1;
        }
      }
      // 每个内建需要的参数不同：memory 要 key，vector.put 要 id + text，
      // vector.query 只要 text。一刀切要求 key 会把 query 判死。
      const bool needKey = (ns == "memory") || (ns == "vector" && op == "put");
      if (needKey && !haveKey) {
        fail(ns + "." + op + (op == "put" && ns == "vector" ? " missing id" : " missing key"));
        return -1;
      }
      if (ns == "memory" && op == "put" && !haveValue) { fail("memory.put is missing value"); return -1; }
      if (ns == "vector" && !haveValue) { fail("vector." + op + " missing text"); return -1; }
      nodes_[id].set("args", std::move(args));
      // 账：一次存储操作 = 一个 span。带向量的那两种要调 embeddings，算一次模型调用。
      span_[id] = 1;
      model_[id] = (op == "put" && ns == "vector") || (op == "query") ? 1 : 0;
      tool_[id] = 0;
      return id;
    }

    if (ns == "json" && fn == "get") {
      fail("json.get is not implemented yet (narrow with json.as_* before reading a field)");
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
          fail("model.generate does not support parameter " + name);
          return -1;
        }
      }
      if (prompt < 0) {
        fail("model.generate is missing prompt");
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
      const Node* idemKeyVal = nullptr;
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
          // 保留表达式本身：IR 里既要 key 的结构化片段（审计可读），
          // 也要一个运行期可求值的节点（args.idem_key，引擎据此注入
          // Idempotency-Key 头）。只记一个 bool 会让 §5.7 的第 ② 条
          // 变成「模式写着 idem_key，实际什么都没注入」的假证明。
          idemKeyVal = val;
        } else if (name == "allow_status") {
          // 数组字面量，元素必须是整数常量（LANGUAGE §5.6）。非整数就明确报错：
          // 静默丢掉一个状态码会让"这个响应算正常还是异常"在运行期随机变化。
          if (!val || val->kind != Kind::ArrayLit) {
            fail("allow_status must be an array literal, e.g. [404, 409]");
            return -1;
          }
          for (const Node* e = val->child; e; e = e->next) {
            if (e->kind != Kind::LitExpr || !e->text || std::string(e->text) != "int") {
              fail("allow_status elements must be integer literals");
              return -1;
            }
            allowStatus.push_back(static_cast<int>(e->i64));
          }
        } else if (name == "header" || name == "body") {
          // 值是**表达式**（通常是变量或字段访问），在运行期求值 ——
          // 请求头/请求体往往由上游算出来，不是常量。
          if (!val) {
            fail("tool.call " + name + " requires an expression");
            return -1;
          }
          const int sub = gen_expr(val);
          if (sub < 0) return -1;
          if (name == "header") headerNode = sub; else bodyNode = sub;
        } else {
          fail("tool.call does not support parameter " + name);
          return -1;
        }
      }
      if (!urlVal) {
        fail("tool.call is missing url");
        return -1;
      }

      const int urlNode = gen_expr(urlVal);
      if (urlNode < 0) return -1;

      // url_template 是**结构化片段**（IR.md §3.4）：字面文本 or 已解析的字段路径。
      // 运行期不该再解析一次模板；审计者也要能直接看出这个 URL 依赖了哪些字段。
      JVal tmpl = template_parts(urlVal);

      // idem_key 的运行期求值节点。放在 urlNode 之后是为了不改既有 golden 的
      // 节点编号（既有 fixture 都没有 idem_key，所以编号完全不动）。
      int idemKeyNode = -1;
      if (idemKeyVal) {
        idemKeyNode = gen_expr(idemKeyVal);
        if (idemKeyNode < 0) return -1;
      }

      // 工具名是合成的（D-029）：语言里没有 tool 声明，而 tools[] 需要一个键。
      char tname[64];
      std::snprintf(tname, sizeof(tname), "tool@0:%d:%d", e->loc.line, e->loc.col);

      // idem.mode 是**编译期事实**（LANGUAGE §5.7），不是「希望它幂等」：
      //   readonly  —— GET / HEAD，方法语义天然只读；
      //   idem_key  —— 作者给了纯幂等键，引擎每次重发注入同名头；
      //   idem_true —— 作者声明上游幂等；
      //   none      —— 以上都不成立。
      //
      // 这里原来无条件以 "readonly" 打底，于是 `method = "POST"` 且不写任何
      // 声明的调用会被标成只读 —— 一份**明确说谎**的 IR：下游照着它重试就会
      // 重复扣款。check_idem.cpp 现在会在 retry 内判死 E5007，但 retry 外的
      // 调用同样不许被贴假标签，所以落到 none。
      const char* idemMode = "none";
      if (idemKeyVal) {
        idemMode = "idem_key";
      } else if (method == "GET" || method == "HEAD") {
        idemMode = "readonly";
      } else if (idempotent) {
        idemMode = "idem_true";
      }

      JVal decl = JVal::object();
      decl.set("name", JVal::string(tname));
      decl.set("method", JVal::string(method));
      decl.set("url_template", std::move(tmpl));
      decl.set("timeout_ms", JVal::integer(timeoutMs));
      decl.set("allow_status", JVal::array());
      JVal idem = JVal::object();
      idem.set("mode", JVal::string(idemMode));
      // schema 的 allOf：mode == idem_key 时必须带 key，其余模式不许带。
      if (idemKeyVal) idem.set("key", template_parts(idemKeyVal));
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
      if (idemKeyNode >= 0) {
        args.set("idem_key", JVal::integer(idemKeyNode));
      } else {
        args.set("idem_key", JVal::null());
      }

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

    fail("unknown call " + ns + "." + fn);
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
      fail("unsupported binary operator: " + sym);
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

  // 一元运算：IR 里是 unop（op / e）。之前**完全没实现** —— 于是 `!flag`、
  // `-x` 这种最基本的写法编不出来，报错还说"尚不支持这种表达式"。
  if (e->kind == Kind::UnaryExpr) {
    const int id = alloc("unop", e);
    const int operand = e->child ? gen_expr(e->child) : -1;
    if (operand < 0) return -1;
    const std::string sym = e->text ? e->text : "";
    std::string op;
    if (sym == "!") op = "not";
    else if (sym == "-") op = "neg";
    else {
      fail("unsupported unary operator: " + sym);
      return -1;
    }
    nodes_[id].set("op", JVal::string(op));
    nodes_[id].set("e", JVal::integer(operand));
    span_[id] = span_[operand];
    model_[id] = model_[operand];
    tool_[id] = tool_[operand];
    return id;
  }

  fail(std::string("codegen does not support this expression yet (kind=") + kind_name(e->kind) +
       ", child=" + (e->child ? kind_name(e->child->kind) : "null") +
       ", args=" + (e->args ? kind_name(e->args->kind) : "null") +
       ", child->next=" + (e->child && e->child->next ? "yes" : "no") + ")");
  return -1;
}

int Gen::gen_path_(const Node* e) {
  if (e->n_subs < 2) {
    fail("path must have at least two segments");
    return -1;
  }
  const Node* head = &e->subs[0];
  if (head->kind != Kind::IdentExpr) {
    fail("the first segment of a path must be an identifier");
    return -1;
  }
  int base = -1;
  const std::string headName = head->text ? head->text : "";
  auto al = inline_alias_.find(headName);
  if (al != inline_alias_.end()) {
    base = al->second;
  } else {
    // 路径首段同样是**引用**：`e.status` 里的 e 要取最内层同名绑定，
    // 否则遮蔽之后字段读的是外层那一个（IR 合法、跑得通、值是错的）。
    const int vid = ref_var(headName);
    if (vid < 0) {
      // 内联模式下 ref_var 查不到会返回 -1，而 IR 里 **-1 表示 input**：
      // 静默发出去等于把"未绑定引用"编成"读请求入参"，必须在这里拦住。
      fail_at(&e->subs[0], "inlined function refers to `" + headName +
                               "`, which is neither a parameter nor a local variable");
      return -1;
    }
    base = alloc("var", &e->subs[0]);
    nodes_[base].set("var", JVal::integer(vid));
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
    fail("statement is empty");
    return -1;
  }

  switch (s->kind) {
    case Kind::Block: {
      const int id = alloc("seq", s);
      JVal kids = JVal::array();
      std::vector<int> kidsIds;
      int sum = 0;
      // 词法作用域：块内声明的名字出块即关闭（LANGUAGE §5.2 的 `parallel` 分支、
      // if/while/for 的体、try/catch 的体都走这里）。出块不 pop 的话，内层声明的
      // 名字会漏到后面的兄弟语句上 —— `if (…) { auto x = …; } return x;` 会编过，
      // 而 x 只在条件为真时才被赋值。
      push_scope();
      // gen_expr 期间可能往本块的 kids 追加合成语句（编译期函数内联归约）。
      // 当前语句的 kid 是在 gen_stmt(k) **返回之后**才 push 的，所以合成语句
      // 天然排在引用它的那条语句之前，正是结果变量需要的顺序。
      const BlockOut saved = cur_block_;
      cur_block_ = BlockOut{&kids, &kidsIds, &sum};
      for (const Node* k = s->child; k; k = k->next) {
        const int kid = gen_stmt(k);
        if (kid < 0) { cur_block_ = saved; pop_scope(); return -1; }
        kids.push(JVal::integer(kid));
        kidsIds.push_back(kid);
        sum += span_[kid];
      }
      cur_block_ = saved;                    // 必须在 std::move(kids) 之前
      pop_scope();
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
      // 解构（`auto [a,b] = …`）这一刀不支持：它需要 destructure 节点与 parallel。
      // 明确报错，而不是把第一个名字当作单绑定 —— 那会静默丢掉其余绑定。
      const Node* pat = s->child;
      if (!pat || pat->kind != Kind::Pattern) {
        fail("declaration is missing a binding name");
        return -1;
      }

      // 数一数绑了几个名字：1 个是 `auto x = …`，多个是 `auto [a, b] = …`。
      int names = 0;
      for (const Node* n = pat->child; n; n = n->next) ++names;
      if (names <= 0) {
        fail("declaration has no binding name");
        return -1;
      }

      if (names == 1) {
        const Node* name = pat->child;
        if (name->kind != Kind::IdentExpr) {
          fail("binding name is not an identifier");
          return -1;
        }
        const int id = alloc("decl", s);
        ++stmtNodes_;
        // 路线 B：内联函数体里的局部变量改名成 __hm_inline_l_N，避免与工作流
        // 变量撞名（函数体在 workflow 帧里原地展开）。
        // 非内联：decl_var 分配**新槽**（声明一次一个槽，遮蔽因此各有各的 id）。
        const int vid = reducing_ ? declare_local(name->text ? name->text : "")
                                  : decl_var(name->text ? name->text : "");
        // 记录"这个变量是定长数组，长度 N"——只记字面量，别的形态不猜。
        // 键是槽位而不是名字：遮蔽之后同名变量是两件东西。
        if (s->args && s->args->kind == Kind::ArrayLit) {
          int n = 0;
          for (const Node* c = s->args->child; c; c = c->next) n++;
          arr_len_[vid] = n;
        }
        int e = gen_expr(s->args);
        if (e < 0) return -1;
        // char / short / long：在声明处裹一层 cast，让**运行期与 C 后端**按宽度
        // 截断（类型检查仍按 int）。auto 声明 s->text 为空，不裹。
        if (s->text) {
          const char* ty = s->text;
          if (strcmp(ty, "char") == 0 || strcmp(ty, "short") == 0 ||
              strcmp(ty, "long") == 0) {
            const int cid = alloc("cast", s);
            JVal to = JVal::object();
            to.set("k", JVal::string(ty));
            nodes_[cid].set("to", std::move(to));
            nodes_[cid].set("e", JVal::integer(e));
            span_[cid] = span_[e];
            model_[cid] = model_[e];
            tool_[cid] = tool_[e];
            e = cid;
          }
        }
        nodes_[id].set("var", JVal::integer(vid));
        nodes_[id].set("e", JVal::integer(e));
        // 2.0.0：IR 用 `const` 表示不可变，与源码 `const auto` 一一对应；
        // `decl` 对应 `auto`。**始终写出这个字段**（true/false），
        // 不再用 `mut` —— "缺字段 = 可变"会让"忘了写"和"故意可变"长得一样。
        nodes_[id].set("const", JVal::boolean(s->is_const));
        span_[id] = span_[e];
        model_[id] = model_[e];
        tool_[id] = tool_[e];
        return id;
      }

      // 解构：D-026 的 B 形态（codegen 只写这一种）。`_` 丢弃位写成 JSON null，
      // 不用哨兵整数 —— -1 已经表示 input 了。
      const int id = alloc("destructure", s);
      ++stmtNodes_;
      // 解构与 `decl` 同语义：`const auto [a, b] = …` 两个名字都不可写。
      nodes_[id].set("const", JVal::boolean(s->is_const));
      JVal vars = JVal::array();
      for (const Node* n = pat->child; n; n = n->next) {
        if (n->kind != Kind::IdentExpr) {
          fail("destructured binding name is not an identifier");
          return -1;
        }
        const std::string name = n->text ? n->text : "";
        if (name == "_") {
          vars.push(JVal::null());
        } else {
          // 每个绑定名一个新槽：解构写在嵌套块里时同样可以遮蔽外层同名变量。
          vars.push(JVal::integer(reducing_ ? declare_local(name) : decl_var(name)));
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
        fail("if is missing a condition or then block");
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
      const int vid = lookup_var(s->text ? s->text : "");
      if (vid < 0) { fail("increment/decrement target is not a declared variable"); return -1; }
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
      // 赋值：只能写**可变**（非 `const auto`）声明过的变量。**在生成阶段校验**（这里能查变量表）。
      const int vid = lookup_var(s->text ? s->text : "");
      if (vid < 0) {
        fail("assignment target is not a declared variable: " + std::string(s->text ? s->text : ""));
        return -1;
      }
      const int e = gen_expr(s->args);
      if (e < 0) return -1;
      const int id = alloc("assign", s);
      ++stmtNodes_;
      nodes_[id].set("var", JVal::integer(vid));
      nodes_[id].set("e", JVal::integer(e));
      // 上界**必须传播 e**，不能写死 0。
      //
      // 执行器里 assign 的右值是**真的会被求值**的（engine/run.go 的
      // `case "assign": r.expr(*n.E)`），所以 `x = parallel { … }` 里的
      // parallel 会实打实产生 span。写死 0 等于给整份 IR 一个**假上界**：
      // max_spans 偏小 → 运行期 `len(spans) ≤ max_spans` 会把合法行为判成
      // 超界，而且加载期没有任何检查会红（偏差 1 永远满足
      // `max_spans ≤ 64×max_steps+1`）。
      //
      // 形状与 bounds.go 的 `case "decl", "destructure", "assign":` **逐字
      // 对应**：`v = absorbSum(0, span(e))` —— 只加 e，**不给 assign 自己加
      // 1**（对照 try / fallback：那两个才各算自己一个 span）。多写一个
      // `1 +` 会被 E_IR_RECOMPUTE_DRIFT 当场抓住，那正是派生字段复算存在的
      // 意义。decl / destructure（本文件上面的 LetStmt 分支）早就是这个写法，
      // assign 是唯一漏掉的第三种绑定形态。
      span_[id] = span_[e];
      model_[id] = model_[e];
      tool_[id] = tool_[e];
      return id;
    }
    // ---- 2.0.1 控制流节点 ------------------------------------------------
    // sleep：时长的表达式在 child。IR 里叫 ms，单位在编译期已经折好。
    case Kind::SleepStmt: {
      const int id = alloc("sleep", s);
      ++stmtNodes_;
      const int ms = gen_expr(s->child);
      if (ms < 0) return -1;
      nodes_[id].set("ms", JVal::integer(ms));
      span_[id] = 0;
      model_[id] = 0;
      tool_[id] = 0;
      return id;
    }
    // until：等待一个信号；timeout 可空（空 = 无限等，运行期仍受 workflow 上界约束）。
    case Kind::UntilStmt: {
      const int id = alloc("until", s);
      ++stmtNodes_;
      nodes_[id].set("signal", JVal::string(s->text ? s->text : ""));
      if (s->chan != nullptr) {
        nodes_[id].set("chan", JVal::integer(static_cast<int64_t>(fnv1a64(s->chan))));
      }
      if (s->args != nullptr) {
        const int t = gen_expr(s->args);
        if (t < 0) return -1;
        nodes_[id].set("timeout_ms", JVal::integer(t));
      }
      span_[id] = 0;
      model_[id] = 0;
      tool_[id] = 0;
      return id;
    }

    case Kind::StopStmt: {
      const int id = alloc("stop", s);
      ++stmtNodes_;
      const int reason = gen_expr(s->child);
      if (reason < 0) return -1;
      nodes_[id].set("e", JVal::integer(reason));
      if (s->args != nullptr) {
        const int data = gen_expr(s->args);
        if (data < 0) return -1;
        nodes_[id].set("data", JVal::integer(data));
      }
      span_[id] = 0;
      model_[id] = 0;
      tool_[id] = 0;
      return id;
    }
    case Kind::EmitStmt: {
      const int id = alloc("emit", s);
      ++stmtNodes_;
      nodes_[id].set("signal", JVal::string(s->text ? s->text : ""));
      // 路由：0=全局冒泡、1=频道、2=定向。数字是 IR 的对外契约，别重排。
      nodes_[id].set("route", JVal::integer(static_cast<int64_t>(s->route)));
      if (is_exported_signal(s->text)) nodes_[id].set("exported", JVal::boolean(true));
      if (s->chan != nullptr) {
        nodes_[id].set("chan", JVal::integer(static_cast<int64_t>(fnv1a64(s->chan))));
      }
      if (s->route == SignalRoute::Target) {
        if (s->child == nullptr) { fail("emit ... to needs a target context"); return -1; }
        const int tgt = gen_expr(s->child);
        if (tgt < 0) return -1;
        nodes_[id].set("to", JVal::integer(tgt));
      }
      JVal arr = JVal::array();
      for (const Node* a = s->args; a; a = a->next) {
        const int e = gen_expr(a);
        if (e < 0) return -1;
        arr.push(JVal::integer(e));
      }
      nodes_[id].set("args", arr);
      span_[id] = 0;
      model_[id] = 0;
      tool_[id] = 0;
      return id;
    }
    case Kind::OnStmt: {
      const int id = alloc("on", s);
      ++stmtNodes_;
      nodes_[id].set("signal", JVal::string(s->text ? s->text : ""));
      if (s->chan != nullptr) {
        nodes_[id].set("chan", JVal::integer(static_cast<int64_t>(fnv1a64(s->chan))));
      }
      // 捕获清单（闭包转换）：Sema 把"槽体引用的外层局部变量"挂在 type_args 上。
      // IR 里保留**名字**而不是变量下标：槽函数要按名字建上下文结构体字段，
      // 而下标是每个函数各自的编号空间，跨函数没有意义。
      if (is_exported_signal(s->text)) nodes_[id].set("exported", JVal::boolean(true));
      if (s->type_args != nullptr) {
        JVal caps = JVal::array();
        for (const Node* c = s->type_args; c; c = c->next) {
          if (c->kind == Kind::IdentExpr && c->text) caps.push(JVal::string(c->text));
        }
        nodes_[id].set("captures", caps);
      }
      const int body = gen_stmt(s->child);   // 槽体是 Block
      if (body < 0) return -1;
      nodes_[id].set("body", JVal::integer(body));
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
      // for (auto i = START; i < END; i++) { body }
      //
      // 循环变量由这个节点**独占**：IR 里 `var` 只出现在这里，循环体里
      // 没有任何赋值节点能改它 —— 所以 `var.decl` 唯一性不破。
      const int id = alloc("for", s);
      ++stmtNodes_;
      // 循环变量只在循环体内可见（§3.1：出循环即回收，E4028 就是它的编译期守卫）。
      // 用一层作用域把"for 变量"与"循环体里声明的名字"都关在里面：
      // 兄弟语句里的同名变量必须解析到它自己的槽，而不是这个循环变量。
      push_scope();
      // 循环变量由 for 自身声明；内联时同样改名为 __hm_inline_l_N。
      const int vid = reducing_ ? declare_local(s->text ? s->text : "")
                                : decl_var(s->text ? s->text : "");
      const int init = gen_expr(s->child);
      if (init < 0) return -1;
      const int cond = gen_expr(s->args);
      if (cond < 0) return -1;
      const int body = gen_stmt(s->body);
      if (body < 0) return -1;
      // 循环变量与体内声明到这里全部关闭（与 E4028 的"出循环即回收"同一件事）。
      pop_scope();
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
        fail("match is missing the expression to match");
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
        fail("match is missing default (E4018): the subject is a string, so exhaustiveness cannot be proven statically");
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
        fail("try is missing a body");
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
        // 错误对象**只活在这个 catch 块里**（E4023 就是它的编译期守卫：把错误
        // 对象整只存走不行）。所以先开一层作用域、把绑定放进去，再编块体。
        // 同名 `catch (ModelError e)` 在多层 try 里各写一次是正常写法 —— 它们
        // 必须是**不同的槽**，否则两处绑定共用一个 id，装配期就要报假 bug。
        push_scope();
        int vid = -1;
        if (c->child && c->child->text) {
          vid = reducing_ ? declare_local(c->child->text) : decl_var(c->child->text);
        }
        const int cb = gen_stmt(c->else_branch);
        pop_scope();
        if (cb < 0) return -1;
        if (span_[cb] > best) best = span_[cb];
        // **相加，不是取大**：try 的语义是"体可能部分执行后才抛，再进 catch"，
        // 所以最坏情况 = 体里的调用 + catch 里的调用。取大等于假设两者互斥，
        // 而上界必须覆盖最坏情况。
        if (model_[cb] > mbest - model_[body]) mbest = model_[body] + model_[cb];
        if (tool_[cb] > tbest - tool_[body]) tbest = tool_[body] + tool_[cb];
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
        fail("retry is missing a body");
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
      fail(std::string("codegen does not support this statement yet (kind=") + kind_name(s->kind) + ")");
      return -1;
  }
}

bool Gen::run(const Node* wf, JVal* out) {
  const Node* body = nullptr;
  for (const Node* c = wf->child; c; c = c->next) {
    if (c->kind == Kind::Block) body = c;
  }
  if (!body) return fail("workflow has no body");

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
      // 同 E4029 的纵深防线：workflow 参数默认值**不走 gen_expr**，却一样会写进
      // IR 的 params[].v。旧的 "float" 标签若漏到这里，lit_text() 会把类型名
      // "float" 当值写进去；这是第二条泄漏路径，只守 gen_expr 等于没守。
      // 正常路径的标签是 "double"，按位模式折叠（与 constValue 同形）。
      if (t == "float") {
        fail_at(v, "legacy float label cannot be a workflow parameter default "
                   "(E4029): this is the pre-v2.0.0 shape, not a ban on double");
        return false;
      }
      if (t == "double") {
        folded.set("k", JVal::string("double"));
        folded.set("bits", JVal::string(doubleBitsHex(v->f64)));
      } else if (t == "int") {
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
    v.set("decl", JVal::integer(-1));  // 由调用方（Go 装配）按 decl 节点回填
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
  // 形参属于函数这一层作用域：它们要能被函数体里同名的局部声明遮蔽，
  // 也要在函数编完之后消失（同一次编译里还有别的函数 / workflow）。
  push_scope();
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
    decl_var(name);
  }

  in_fn_ = true;
  cur_fn_ = fn->text ? fn->text : "";
  const int root = gen_stmt(body);
  in_fn_ = false;
  // 函数这一层作用域到此结束（形参 + 所有体内声明）。下一个函数 / workflow
  // 从干净的作用域栈开始，不会解析到这一个的槽位。
  pop_scope();
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
  // ------------------------------------------------------------------------
  // 跨文件 extern 符号表（EXTERN-C-ABI.md §1.3 正例 1）。
  //
  // hm_codegen_ext / hm_codegen_functions 只拿到**入口那一棵 AST**，而 extern
  // 声明住在被 #include 的 `.h` 里 —— 另一棵从未传入的 AST。codegen 的
  // extern_fns_ 只看 `module->child`，于是入口里的调用被判成 unknown call。
  // Go 侧唯一能跨文件传数据的通道是 ext_types 文本（`Struct\tField\tType`），
  // 所以这里把本模块的 extern 签名序列化成一行，交给 Go 拼进 ext_types，
  // 由 api.cpp::inject_extern_decls 在**编译期**静态注入入口 AST。
  //
  // 上面那批 {name,exported,is_entry} 项必须逐字节不变：Go 侧 collected
  // exportedDecls 的循环按 `name` 认项。这批新项**故意不设 name**，只给
  // `sym`，否则一个 extern 声明会被误收成一条 workflow 导出标注。
  // ------------------------------------------------------------------------
  for (const Node* d = module ? module->child : nullptr; d; d = d->next) {
    if (!d->is_extern || !d->text || d->text[0] == '\0') continue;
    if (d->kind != Kind::WorkflowSig && d->kind != Kind::FunctionDecl) continue;
    const char form = (d->kind == Kind::WorkflowSig) ? 'w' : 'f';
    // 返回类型：form A / form B 都在 type_args（parse_extern 的形状）。
    // 缺返回类型是解析错误（那时本模块自己的 check_types 已经报过），
    // 不编一条半截的注入行，避免注入出一个空类型名的伪符号。
    if (!d->type_args || !d->type_args->text || d->type_args->text[0] == '\0') continue;

    std::string line = "extern\t";
    line += d->text;
    line += '\t';
    line += form;
    line += '\t';
    line += d->type_args->text;
    line += '\t';
    // 形参类型按声明顺序；form A 在 Param::child，form B 在 Param::type_args
    // （与 check_type.cpp::extern_param_type_ 同一条槽位约定）。
    bool first = true;
    for (const Node* p = d->child; p; p = p->next) {
      if (p->kind != Kind::Param) continue;
      const Node* ty = (form == 'w') ? p->child : p->type_args;
      const char* tn = (ty && ty->text) ? ty->text : "";
      if (!first) line += ',';
      line += tn;
      first = false;
    }

    JVal e = JVal::object();
    e.set("extern", JVal::boolean(true));
    e.set("sym", JVal::string(d->text));
    // 制表符由 write_escaped 写成 JSON 的 `\t`；Go 侧 json.Unmarshal 还原成
    // 真 tab，拼进 ext_types 后 C++ 注入器按 `\t` 切段。类型名只取内建名或
    // struct 名（C ABI 闭集），不序列化复杂类型表达式。
    e.set("ext_line", JVal::string(line));
    arr.push(std::move(e));
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
        if (err) *err = "tool declaration has an unknown field: " + fname;
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
    if (d->is_extern) continue;  // extern 无体，不进函数表 / 递归分析
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

  // extern 声明表：供函数体内对 extern 的调用编成 call_native（不进 functions[]）。
  std::map<std::string, const Node*> extern_fns;
  for (const Node* d = module ? module->child : nullptr; d; d = d->next) {
    if (d->is_extern && (d->kind == Kind::FunctionDecl || d->kind == Kind::WorkflowSig) && d->text) {
      extern_fns[d->text] = d;
    }
  }

  // 逐个编函数表项。注意这里也要装入 recursive 判定：递归函数的函数体会
  // 被运行期执行，它内部对**非递归**函数的调用同样必须在编译期展开。
  JVal arr = JVal::array();
  for (const Node* d = module ? module->child : nullptr; d; d = d->next) {
    if (d->kind != Kind::FunctionDecl) continue;
    if (d->is_extern) continue;  // extern 无体，不进 functions[]
    Gen g(err);
    g.set_functions(&byName);
    g.set_recursive(&recursive);
    g.set_extern_fns(&extern_fns);
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
                         Loc* err_loc, Arena* arena, DiagBag* diags,
                         const char* diag_module) {
  if (!wf || wf->kind != Kind::WorkflowDecl) {
    if (err) *err = "codegen_workflow_in only accepts WorkflowDecl";
    return false;
  }
  // 函数是 unit 级事实：workflow 里的 `greet(...)` 要解析成"内联"或
  // call_fn（仅递归）。函数本身由 codegen_functions 单独编成一张表。
  std::map<std::string, const Node*> fns;
  std::map<std::string, bool> recursive;
  if (!analyze_functions(module, &fns, &recursive, err, err_loc)) return false;
  // extern 声明表：供 call_native 解析（不进 functions[]）。
  std::map<std::string, const Node*> extern_fns;
  for (const Node* d = module ? module->child : nullptr; d; d = d->next) {
    if (d->is_extern && (d->kind == Kind::FunctionDecl || d->kind == Kind::WorkflowSig) && d->text) {
      extern_fns[d->text] = d;
    }
  }
  // 分析阶段的 Gen 不接诊断通道：它只编函数体、不产 workflow 对象，任何写进
  // DiagBag 的警告都会在"正式编一遍"时再出现一次（同一条警告报两遍）。
  Gen g(err, arena, diags, diag_module);
  g.set_module(module);
  g.set_functions(&fns);
  g.set_recursive(&recursive);
  g.set_extern_fns(&extern_fns);
  const bool ok = g.run(wf, out);
  if (!ok && err_loc) {
    // 编译期展开失败要指到**调用点**（g.err_node），而不是 workflow 开头。
    *err_loc = g.err_node() ? g.err_node()->loc : wf->loc;
  }
  return ok;
}

bool codegen_workflow(const Node* wf, JVal* out, std::string* err) {
  if (!wf || wf->kind != Kind::WorkflowDecl) {
    if (err) *err = "codegen_workflow only accepts WorkflowDecl";
    return false;
  }
  Gen g(err);
  return g.run(wf, out);
}

}  // namespace hm
