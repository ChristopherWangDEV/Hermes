// api.cpp —— libhermes_parser.a 的公开 C ABI（供 Go cgo / 宿主调用，对应 T35 的边界）。
//
// 契约见 native/include/hermes_parser.h（公开头，单一事实源）：
// 编译器前端是一个**纯函数**——输入源字节 + 文件名，输出
//   · AST 的确定性文本 dump（golden 比对 / 调试）
//   · 诊断的 JSON 文本
//   · AST 节点数
// 不读文件、不打印、无全局可变状态。每次调用拥有独立的 Arena（返回前整体释放）。
//
// 本文件导出 6 个 hm_parse_* 的 C 链接符号（见 scripts/expected_symbols_parser.txt）。
// 内部实现全部复用 hm::lex / hm::parse / hm::ast_dump / hm::ast_count /
// hm::diag_to_json / hm::kind_name，不重复任何解析逻辑。

#include "arena.h"
#include "ast.h"
#include "hermes_parser.h"
#include "codegen.h"  // 声明与定义同源：签名漂移在编译期就红

#include <cstddef>
#include <cstdlib>
#include <cstring>
#include <new>
#include <string>
#include <utility>
#include <vector>

namespace hm {
void lex(const uint8_t* src, size_t len, const char* filename, Arena& arena,
         DiagBag& diags, std::vector<Token>& out);
Node* parse(const std::vector<Token>& toks, const char* filename, bool is_header,
            Arena& arena, DiagBag& diags);
void ast_dump(const Node* root, char* out, size_t cap, size_t* out_len);
int ast_count(const Node* root);
void diag_to_json(const DiagBag& bag, char* out, size_t cap, size_t* out_len);
}  // namespace hm

namespace {

// 用 malloc + placement new 构造 Arena，避免任何可能走异常的 operator new 路径
// （-fno-exceptions 下 new 失败会直接 terminate）。
hm::Arena* new_arena(size_t cap) {
  void* mem = malloc(sizeof(hm::Arena));
  if (!mem) return nullptr;
  return new (mem) hm::Arena(cap);
}

void del_arena(hm::Arena* a) {
  if (a) {
    a->~Arena();
    free(a);
  }
}

// ---------------------------------------------------------------------------
// 编译期静态符号注入（EXTERN-C-ABI.md §1.3 正例 1）。
//
// hm_codegen_ext / hm_codegen_functions 只拿到入口那一份源码，而 extern 声明
// 住在被 #include 的 `.h` 里。codegen 的 extern_fns_ 只遍历 `module->child`，
// 于是入口里对跨文件 extern 的调用被判成 "unknown call"。
//
// 唯一的跨文件数据通道是 ext_types（`Struct\tField\tType`，见
// native/include/hermes_parser.h:88-93，该头冻结不改）。Go 侧把各模块的
// extern 签名也拼成文本行：
//     extern\t<name>\t<w|f>\t<ret>\t<t1>,<t2>,...
// 本函数在 `hm::parse` 之后、`check_types_with_externals` 之前把这些行**静态**
// 还原成 AST 声明节点，挂到入口 root->child 链尾。没有 dlsym、没有运行期
// 符号解析、没有树遍历解释路径 —— 只是把编译期已知的事实补进 AST，
// 之后 codegen 仍按原来的「查不到就拒绝」规则工作。
//
// loc.line = 0 是注入哨兵：真实源码声明的行号 ≥ 1。
// check_type.cpp::check_extern_declarations_ 靠它跳过注入节点，避免同一条
// E3030 在原模块检查过之后又报一遍。
// ---------------------------------------------------------------------------
void inject_extern_decls(hm::Node* root, hm::Arena& a,
                         const char* ext_types, size_t ext_len) {
  if (!root || !ext_types || ext_len == 0) return;
  const std::string text(ext_types, ext_len);

  hm::Loc sentinel{};
  sentinel.line = 0;
  sentinel.col = 0;
  sentinel.end_line = 0;
  sentinel.end_col = 0;

  // 与 parser.cpp::new_node 同一条分配方式：Arena + placement new。
  // 不引入 operator new / 异常路径（-fno-exceptions -fno-rtti）。
  auto make = [&](hm::Kind k) -> hm::Node* {
    hm::Node* n = hm::make_node<hm::Node>(a);
    if (!n) return nullptr;
    n->kind = k;
    n->loc = sentinel;
    return n;
  };
  auto intern = [&](const std::string& s) -> const char* {
    return a.intern(s.data(), s.size());
  };

  hm::Node* tail = nullptr;
  for (hm::Node* c = root->child; c; c = c->next) tail = c;

  size_t pos = 0;
  while (pos <= text.size() && !a.oom()) {
    const size_t nl = text.find('\n', pos);
    const size_t end = (nl == std::string::npos) ? text.size() : nl;
    const std::string line = text.substr(pos, end - pos);
    pos = (nl == std::string::npos) ? text.size() + 1 : nl + 1;

    if (line.compare(0, 7, "extern\t") != 0) continue;
    // extern\tNAME\tFORM\tRET\tPARAMS_CSV —— 恰好 4 个 tab。
    const size_t t1 = line.find('\t', 7);
    const size_t t2 = (t1 == std::string::npos) ? std::string::npos
                                                : line.find('\t', t1 + 1);
    const size_t t3 = (t2 == std::string::npos) ? std::string::npos
                                                : line.find('\t', t2 + 1);
    if (t1 == std::string::npos || t2 == std::string::npos ||
        t3 == std::string::npos) continue;
    const std::string name = line.substr(7, t1 - 7);
    const std::string form = line.substr(t1 + 1, t2 - t1 - 1);
    const std::string ret = line.substr(t2 + 1, t3 - t2 - 1);
    const std::string params = line.substr(t3 + 1);
    if (name.empty() || ret.empty() || (form != "w" && form != "f")) continue;

    hm::Node* decl = make(form == "w" ? hm::Kind::WorkflowSig
                                      : hm::Kind::FunctionDecl);
    hm::Node* ret_ty = make(hm::Kind::Type);
    if (!decl || !ret_ty) return;
    decl->text = intern(name);
    decl->is_extern = true;
    decl->is_export = false;
    decl->is_entry = false;
    ret_ty->text = intern(ret);
    decl->type_args = ret_ty;  // form A / form B 的返回类型同槽位

    hm::Node* head = nullptr;
    hm::Node* ptail = nullptr;
    size_t q = 0;
    while (q <= params.size()) {
      const size_t comma = params.find(',', q);
      const size_t stop = (comma == std::string::npos) ? params.size() : comma;
      if (stop > q) {
        hm::Node* prm = make(hm::Kind::Param);
        hm::Node* prm_ty = make(hm::Kind::Type);
        if (!prm || !prm_ty) return;
        prm->text = nullptr;  // 位置参数，不带名字
        prm_ty->text = intern(params.substr(q, stop - q));
        if (form == "w") prm->child = prm_ty;      // 形态 A：类型在 child
        else prm->type_args = prm_ty;              // 形态 B：类型在 type_args
        if (!head) head = prm;
        else ptail->next = prm;
        ptail = prm;
      }
      if (comma == std::string::npos) break;
      q = comma + 1;
    }
    decl->child = head;

    if (!tail) root->child = decl;
    else tail->next = decl;
    tail = decl;
  }
}

// 一次性：词法 + 语法，得到 AST 根 + 诊断袋 + token 序列，全部挂在临时 Arena 上。
int run_parse(const uint8_t* src, size_t len, const char* filename, bool is_header,
              hm::Node** root_out, hm::Arena** arena_out, hm::DiagBag* diag_out,
              std::vector<hm::Token>* toks_out, const char* ext_types = nullptr,
              size_t ext_len = 0) {
  hm::Arena* a = new_arena(128u * 1024u * 1024u);
  if (!a) return -2;
  hm::DiagBag d{};
  std::vector<hm::Token> toks;
  hm::lex(src, len, filename, *a, d, toks);
  hm::Node* root = nullptr;
  if (!a->oom()) root = hm::parse(toks, filename, is_header, *a, d);
  // 类型检查与语法诊断**并进同一个 DiagBag**：一次 hm_parse 给出全部问题。
  // 拆成两个入口会让调用方必须调两次，而"哪个先跑"又会引入顺序依赖。
  if (!a->oom() && root) {
    // 顺序固定：parse → 静态注入跨文件 extern 声明 → 类型检查 → 幂等证明。
    // 注入必须在 check_types_with_externals 之前：后者要看到注入节点，才能把
    // 入口里的 extern 调用按 C ABI 检查（E3031 / E4030）而不是判成 unknown。
    if (ext_types && ext_len) {
      inject_extern_decls(root, *a, ext_types, ext_len);
    }
    if (!a->oom()) {
      if (ext_types && ext_len) {
        hm::check_types_with_externals(root, filename, *a, d, ext_types, ext_len);
      } else {
        hm::check_types(root, filename, *a, d);
      }
      // §5.7：幂等证明在类型检查之后、codegen 之前。放在这里而不是某个
      // hm_codegen_* 入口里，是因为所有入口都经 run_parse —— 少挂一个入口
      // 就等于给「换个子命令绕过重复扣款检查」留了一条路。
      hm::check_idempotency(root, filename, *a, d);
    }
  }
  *arena_out = a;
  *diag_out = d;
  *toks_out = std::move(toks);
  *root_out = root;
  return 0;
}

}  // namespace

#pragma GCC visibility push(default)

extern "C" {

const char* hm_parse_version() { return "hermes-parser/0.1"; }

int hm_parse_kind_name(int kind, char* out, size_t cap) {
  if (!out || cap == 0) return -1;
  const char* s = hm::kind_name(static_cast<hm::Kind>(kind));
  if (!s) s = "?";
  size_t n = strlen(s);
  if (n + 1 > cap) n = cap - 1;
  memcpy(out, s, n);
  out[n] = '\0';
  return 0;
}

// 主入口：一次性产出 dump + diag + 节点数。
int hm_parse(const uint8_t* src, size_t len, const char* filename, int is_header,
             char* out_dump, size_t dump_cap, size_t* dump_len, char* out_diag,
             size_t diag_cap, size_t* diag_len, int* node_count) {
  hm::Node* root = nullptr;
  hm::Arena* a = nullptr;
  hm::DiagBag d{};
  std::vector<hm::Token> toks;
  int rc = run_parse(src, len, filename, is_header != 0, &root, &a, &d, &toks);
  if (rc != 0) return rc;
  if (node_count) *node_count = hm::ast_count(root);
  if (out_dump)
    hm::ast_dump(root, out_dump, dump_cap, dump_len);
  else if (dump_len)
    *dump_len = 0;
  if (out_diag)
    hm::diag_to_json(d, out_diag, diag_cap, diag_len);
  else if (diag_len)
    *diag_len = 0;
  del_arena(a);
  return 0;
}

// hm_codegen：解析一份源码并把**第一个 workflow** 编成 IR 的 workflow 对象（JSON）。
//
// 为什么给"第一个"而不是"全部"：一次 hm_parse 产出的 AST 里可能有多个 workflow，
// 而 Go 侧装配时需要**逐个**带位置信息地报错（"第 2 个 workflow 第 17 行不支持"）。
// 返回"一个"能让调用方精确知道失败发生在哪个 workflow；批量版等 T34 收口再加。
//
// 返回码：
//   0 成功（out 是 canonical JSON）
//   1 失败（out_diag 是 JSON 数组，含解析错误或 codegen 不支持的原因）
//   2 源码里没有 workflow
int hm_codegen(const uint8_t* src, size_t len, const char* filename, int is_header,
               char* out, size_t cap, size_t* out_len,
               char* out_diag, size_t diag_cap, size_t* diag_len) {
  hm::Node* root = nullptr;
  hm::Arena* a = nullptr;
  hm::DiagBag d{};
  std::vector<hm::Token> toks;
  int rc = run_parse(src, len, filename, is_header != 0, &root, &a, &d, &toks);
  if (rc != 0) return rc;

  // 一个 unit 只能有一个 workflow：装配层目前只接受单 workflow 的 JSON。
  //
  // 以前这里是 `break` —— 第二个 workflow 被**静默丢弃**，用户写了两段逻辑，
  // 编出来的产物里只有一段。静默丢数据是最坏的失败形态：它不报错，只在运行期
  // 表现为"我的另一个 workflow 去哪了"。所以现在显式报错并指出数量。
  const hm::Node* wf = nullptr;
  const hm::Node* second = nullptr;
  int wfCount = 0;
  for (const hm::Node* c = root ? root->child : nullptr; c; c = c->next) {
    if (c->kind == hm::Kind::WorkflowDecl) {
      wfCount++;
      if (!wf) { wf = c; } else if (!second) { second = c; }
    }
  }
  if (wfCount > 1) {
    hm::Diag dd{};
    dd.severity = hm::Severity::Error;
    dd.code = "E3101";
    dd.message = "a compilation unit may contain only one workflow (multiple found; the rest would be dropped)";
    dd.why = "the assembly stage organizes IR around a single workflow; the extra one never reaches the output, "
             "and it looks perfectly normal - that is data silently dropped.";
    dd.fix = "Split each workflow into a separate .hermes file and build each one on its own.";
    dd.loc = second ? second->loc : wf->loc;
    hm::diag_add(d, *a, dd);
    if (out_diag) hm::diag_to_json(d, out_diag, diag_cap, diag_len);
    del_arena(a);
    return 1;
  }

  if (!wf) {
    if (out_diag) hm::diag_to_json(d, out_diag, diag_cap, diag_len);
    del_arena(a);
    return 2;
  }

  hm::JVal js;
  std::string err;
  hm::Loc eloc{};
  // arena + diags 是 codegen 的**警告通道**：codegen 现在会产出 W2004 这类
  // "编得出来但要告诉你代价"的诊断。它们与解析/类型检查的诊断进同一个 DiagBag，
  // 所以 out_diag 里天然按发生顺序排好了，调用方只需要一条读法（JSON 数组）。
  // 注意 hermes_parser.h 的契约早就允许"成功时 out_diag 非空"，形状不变。
  const bool ok = hm::codegen_workflow_in(root, wf, &js, &err, &eloc, a, &d, filename);
  if (!ok) {
    // 把 codegen 的拒绝原因也做成一条诊断，让调用方只有一条读法（JSON 数组），
    // 不必区分"解析错误"与"codegen 错误"两种形态 —— 两种形态是同一类错误的
    // 两个来源，客户端不该为此写两个分支。
    hm::Diag dd{};
    dd.severity = hm::Severity::Error;
    dd.code = "E_IR_CODEGEN";
    dd.message = err.c_str();
    dd.loc = eloc.line > 0 ? eloc : wf->loc;
    dd.module = filename;
    hm::diag_add(d, *a, dd);
  }

  if (out_diag) hm::diag_to_json(d, out_diag, diag_cap, diag_len);
  if (!ok) { del_arena(a); return 1; }

  if (out) *out_len = js.write(out, cap);
  else if (out_len) *out_len = 0;
  del_arena(a);
  return 0;
}

// hm_codegen_types：解析 `.h`（或任何模块）并把顶层 struct 编成类型表（JSON 数组）。
// 类型引用只带名字（{"k":"struct","ref":"OrderInfo"}），id 由装配阶段分配。
int hm_codegen_types(const uint8_t* src, size_t len, const char* filename, int is_header,
                     char* out, size_t cap, size_t* out_len,
                     char* out_diag, size_t diag_cap, size_t* diag_len) {
  hm::Node* root = nullptr;
  hm::Arena* a = nullptr;
  hm::DiagBag d{};
  std::vector<hm::Token> toks;
  int rc = run_parse(src, len, filename, is_header != 0, &root, &a, &d, &toks);
  if (rc != 0) return rc;

  hm::JVal js;
  std::string err;
  if (!hm::codegen_types(root, &js, &err)) {
    hm::Diag dd{};
    dd.severity = hm::Severity::Error;
    dd.code = "E_IR_CODEGEN";
    dd.message = err.c_str();
    hm::diag_add(d, *a, dd);
    if (out_diag) hm::diag_to_json(d, out_diag, diag_cap, diag_len);
    del_arena(a);
    return 1;
  }
  if (out_diag) hm::diag_to_json(d, out_diag, diag_cap, diag_len);
  if (out) *out_len = js.write(out, cap);
  else if (out_len) *out_len = 0;
  del_arena(a);
  return 0;
}

// hm_codegen_ext：与 hm_codegen 相同，但额外接受一张"别的模块声明了哪些 struct"
// 的表（每行 `Struct\tField\tType`）。跨文件工程必须走这条口，否则单文件检查器
// 会把所有来自 .h 的类型判成 unknown。
int hm_codegen_ext(const uint8_t* src, size_t len, const char* filename, int is_header,
                   const char* ext_types, size_t ext_len,
                   char* out, size_t cap, size_t* out_len,
                   char* out_diag, size_t diag_cap, size_t* diag_len) {
  hm::Node* root = nullptr;
  hm::Arena* a = nullptr;
  hm::DiagBag d{};
  std::vector<hm::Token> toks;
  int rc = run_parse(src, len, filename, is_header != 0, &root, &a, &d, &toks,
                     ext_types, ext_len);
  if (rc != 0) return rc;

  // 一个 unit 可以有**多个** workflow（D-037）。每个 WorkflowDecl 编成一段，
  // 一起放进数组交给装配层。
  //
  // 历史：最早是 `break`（第二个被静默丢弃）→ 后来改成报 E3101（不再静默）
  // → 现在真正支持多个。中间那一步值得记着：静默丢数据比报错危险得多。
  std::vector<const hm::Node*> wfs;
  for (const hm::Node* c = root ? root->child : nullptr; c; c = c->next) {
    if (c->kind == hm::Kind::WorkflowDecl) wfs.push_back(c);
  }
  // **顺序很重要**：诊断 JSON 必须在 codegen **之后**序列化。
  //
  // 这里原来是在循环之前就 diag_to_json 一次 —— 当时 codegen 只会往 err 里写
  // 错误、从不往 DiagBag 里写东西，所以"早点序列化"看不出问题。现在 codegen
  // 会追加 W2004 这类**警告**（max_steps = unlimited），提前序列化就意味着
  // 每一条 codegen 警告都被静默丢掉：用户拿到 exit 0、产物也对了，唯独永远
  // 看不到那条解释"你失去了什么"的诊断。诊断被静默丢弃比没有诊断更坏。
  if (wfs.empty()) {
    if (out_diag) hm::diag_to_json(d, out_diag, diag_cap, diag_len);
    del_arena(a);
    return 2;
  }

  hm::JVal arr = hm::JVal::array();
  for (const hm::Node* wf : wfs) {
    hm::JVal js;
    std::string err;
    hm::Loc eloc{};
    if (!hm::codegen_workflow_in(root, wf, &js, &err, &eloc, a, &d, filename)) {
      hm::Diag dd{};
      dd.severity = hm::Severity::Error;
      dd.code = "E_IR_CODEGEN";
      dd.message = err.c_str();
      dd.loc = eloc.line > 0 ? eloc : wf->loc;
      dd.module = filename;
      hm::diag_add(d, *a, dd);
      if (out_diag) hm::diag_to_json(d, out_diag, diag_cap, diag_len);
      del_arena(a);
      return 1;
    }
    arr.push(std::move(js));
  }
  // 成功路径同样要序列化：失败路径各有一份是为了尽早返回，成功路径在这里。
  if (out_diag) hm::diag_to_json(d, out_diag, diag_cap, diag_len);
  if (out) *out_len = arr.write(out, cap);
  else if (out_len) *out_len = 0;
  del_arena(a);
  return 0;
}

// hm_codegen_functions：解析一个模块并把顶层函数编成函数表（JSON 数组）。
//
// 为什么是**新函数**而不是给 hm_codegen_ext 加参数：后者已经有 Go cgo 与
// 外部宿主的调用点，改签名会把"加一个输出"变成"改所有调用方"；而上一轮在
// 已有文本里找插入点连错四次（见 docs/FUNCTIONS.md）。新函数整段写出来，
// 不需要猜任何语法边界。
//
// ext_types 与 hm_codegen_ext 同形：函数签名里的 struct 可能来自别的 .h。
int hm_codegen_functions(const uint8_t* src, size_t len, const char* filename, int is_header,
                         const char* ext_types, size_t ext_len,
                         char* out, size_t cap, size_t* out_len,
                         char* out_diag, size_t diag_cap, size_t* diag_len) {
  hm::Node* root = nullptr;
  hm::Arena* a = nullptr;
  hm::DiagBag d{};
  std::vector<hm::Token> toks;
  int rc = run_parse(src, len, filename, is_header != 0, &root, &a, &d, &toks,
                     ext_types, ext_len);
  if (rc != 0) return rc;

  hm::JVal js;
  std::string err;
  hm::Loc eloc{};
  if (!hm::codegen_functions(root, &js, &err, &eloc)) {
    hm::Diag dd{};
    dd.severity = hm::Severity::Error;
    dd.code = "E_IR_CODEGEN";
    dd.message = err.c_str();
    dd.loc = eloc;
    dd.module = filename;
    hm::diag_add(d, *a, dd);
    if (out_diag) hm::diag_to_json(d, out_diag, diag_cap, diag_len);
    del_arena(a);
    return 1;
  }
  if (out_diag) hm::diag_to_json(d, out_diag, diag_cap, diag_len);
  if (out) *out_len = js.write(out, cap);
  else if (out_len) *out_len = 0;
  del_arena(a);
  return 0;
}

// hm_codegen_wfdecls：模块顶层 workflow 声明（名字 / exported / is_entry）。
// 跨文件工程靠它把 `.h` 里的 export 与 entry 标注带回给装配阶段。
int hm_codegen_wfdecls(const uint8_t* src, size_t len, const char* filename, int is_header,
                       char* out, size_t cap, size_t* out_len,
                       char* out_diag, size_t diag_cap, size_t* diag_len) {
  hm::Node* root = nullptr;
  hm::Arena* a = nullptr;
  hm::DiagBag d{};
  std::vector<hm::Token> toks;
  int rc = run_parse(src, len, filename, is_header != 0, &root, &a, &d, &toks);
  if (rc != 0) return rc;
  hm::JVal js;
  std::string err;
  hm::codegen_wfdecls(root, &js, &err);
  if (out_diag) hm::diag_to_json(d, out_diag, diag_cap, diag_len);
  if (out) *out_len = js.write(out, cap);
  else if (out_len) *out_len = 0;
  del_arena(a);
  return 0;
}

// hm_codegen_backends：模块里的 backend 声明/定义（名字、kind、地址来源）。
int hm_codegen_backends(const uint8_t* src, size_t len, const char* filename, int is_header,
                        char* out, size_t cap, size_t* out_len,
                        char* out_diag, size_t diag_cap, size_t* diag_len) {
  hm::Node* root = nullptr;
  hm::Arena* a = nullptr;
  hm::DiagBag d{};
  std::vector<hm::Token> toks;
  int rc = run_parse(src, len, filename, is_header != 0, &root, &a, &d, &toks);
  if (rc != 0) return rc;
  hm::JVal js;
  std::string err;
  hm::codegen_backends(root, &js, &err);
  if (out_diag) hm::diag_to_json(d, out_diag, diag_cap, diag_len);
  if (out) *out_len = js.write(out, cap);
  else if (out_len) *out_len = 0;
  del_arena(a);
  return 0;
}

// hm_codegen_tools：模块里的 `tool` 声明（名字、描述、method、URL 模板）。
int hm_codegen_tools(const uint8_t* src, size_t len, const char* filename, int is_header,
                     char* out, size_t cap, size_t* out_len,
                     char* out_diag, size_t diag_cap, size_t* diag_len) {
  hm::Node* root = nullptr;
  hm::Arena* a = nullptr;
  hm::DiagBag d{};
  std::vector<hm::Token> toks;
  int rc = run_parse(src, len, filename, is_header != 0, &root, &a, &d, &toks);
  if (rc != 0) return rc;
  hm::JVal js;
  std::string err;
  if (!hm::codegen_tools(root, &js, &err)) {
    // 字段不认识是**用户写错**，走诊断通道（codegen 的 err 字符串），
    // 不要伪装成内部错误。
    hm::Diag diag{};
    diag.code = "E3009";
    diag.severity = hm::Severity::Error;
    diag.loc.line = 1;
    diag.loc.col = 1;
    diag.message = err.c_str();
    hm::diag_add(d, *a, diag);
  }
  if (out_diag) hm::diag_to_json(d, out_diag, diag_cap, diag_len);
  if (out) *out_len = js.write(out, cap);
  else if (out_len) *out_len = 0;
  del_arena(a);
  return 0;
}

int hm_parse_dump(const uint8_t* src, size_t len, const char* filename,
                  int is_header, char* out, size_t cap, size_t* out_len) {
  hm::Node* root = nullptr;
  hm::Arena* a = nullptr;
  hm::DiagBag d{};
  std::vector<hm::Token> toks;
  int rc = run_parse(src, len, filename, is_header != 0, &root, &a, &d, &toks);
  if (rc != 0) return rc;
  if (out)
    hm::ast_dump(root, out, cap, out_len);
  else if (out_len)
    *out_len = 0;
  del_arena(a);
  return 0;
}

int hm_parse_diag(const uint8_t* src, size_t len, const char* filename,
                  int is_header, char* out, size_t cap, size_t* out_len) {
  hm::Node* root = nullptr;
  hm::Arena* a = nullptr;
  hm::DiagBag d{};
  std::vector<hm::Token> toks;
  int rc = run_parse(src, len, filename, is_header != 0, &root, &a, &d, &toks);
  if (rc != 0) return rc;
  if (out)
    hm::diag_to_json(d, out, cap, out_len);
  else if (out_len)
    *out_len = 0;
  del_arena(a);
  return 0;
}

int hm_parse_count(const uint8_t* src, size_t len, const char* filename,
                   int is_header, int* count) {
  hm::Node* root = nullptr;
  hm::Arena* a = nullptr;
  hm::DiagBag d{};
  std::vector<hm::Token> toks;
  int rc = run_parse(src, len, filename, is_header != 0, &root, &a, &d, &toks);
  if (rc != 0) return rc;
  if (count) *count = hm::ast_count(root);
  del_arena(a);
  return 0;
}

}  // extern "C"

#pragma GCC visibility pop
