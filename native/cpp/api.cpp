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
    if (ext_types && ext_len) {
      hm::check_types_with_externals(root, filename, *a, d, ext_types, ext_len);
    } else {
      hm::check_types(root, filename, *a, d);
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
    dd.message = "一个 unit 只能有一个 workflow（发现多个，第二个起会被丢弃）";
    dd.why = "装配层按单 workflow 的 IR 组织；多出来的那份不会进产物，"
             "而它看起来完全正常 —— 那是静默丢数据。";
    dd.fix = "把每个 workflow 拆成独立的 .hermes 文件（各自 build 一次）。";
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
  const bool ok = hm::codegen_workflow_in(root, wf, &js, &err, &eloc);
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
  if (out_diag) hm::diag_to_json(d, out_diag, diag_cap, diag_len);
  if (wfs.empty()) { del_arena(a); return 2; }

  hm::JVal arr = hm::JVal::array();
  for (const hm::Node* wf : wfs) {
    hm::JVal js;
    std::string err;
    hm::Loc eloc{};
    if (!hm::codegen_workflow_in(root, wf, &js, &err, &eloc)) {
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
