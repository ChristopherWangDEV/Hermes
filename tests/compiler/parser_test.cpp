// parser_test.cpp —— T09 的 C++ 单测驱动（不依赖 cgo）。
//
// 通过 tests/compiler/run.sh 用 clang++ 直接编译本文件 + native/cxx/*.cpp，
// 链接成独立可执行，运行后逐条验证 LANGUAGE.md / ARCH 的九条验收。
//
// 设计：hm_parse_* 的内部函数（lex/parse/ast_dump）可直接被 C++ 单测驱动，
// 因此这里不经由 Go，纯 C++ 验证（与 ARCH §5.9 "C++ 单测可直接驱动"一致）。

#include "arena.h"
#include "ast.h"
#include "codegen.h"
#include "json_write.h"

#include <cstdarg>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <string>
#include <vector>

// 声明 native/cxx 里定义的函数（与库一同编译）。
namespace hm {
void lex(const uint8_t* src, size_t len, const char* filename, Arena& arena,
         DiagBag& diags, std::vector<Token>& out);
Node* parse(const std::vector<Token>& toks, const char* filename, bool is_header,
            Arena& arena, DiagBag& diags);
void ast_dump(const Node* root, char* out, size_t cap, size_t* out_len);
int ast_count(const Node* root);
void diag_to_json(const DiagBag& bag, char* out, size_t cap, size_t* out_len);
}  // namespace hm

// 公开 C ABI（native/cxx/api.cpp）—— 验证前端作为纯函数（字节→字节）可用。
extern "C" {
const char* hm_parse_version();
int hm_parse(const uint8_t* src, size_t len, const char* filename, int is_header,
             char* out_dump, size_t dump_cap, size_t* dump_len, char* out_diag,
             size_t diag_cap, size_t* diag_len, int* node_count);
int hm_parse_count(const uint8_t* src, size_t len, const char* filename, int is_header,
                   int* count);
}  // extern "C"

using namespace hm;

// ----------------------------- 测试框架 ------------------------------------
struct Tester {
  int pass = 0;
  int fail = 0;
  int total = 0;
};
static Tester T;

static void check(bool cond, const char* fmt, ...) {
  T.total++;
  if (cond) {
    T.pass++;
  } else {
    T.fail++;
    va_list ap;
    va_start(ap, fmt);
    printf("  FAIL: ");
    vprintf(fmt, ap);
    printf("\n");
    va_end(ap);
  }
}

// ----------------------------- 解析辅助 ------------------------------------
static bool read_file(const char* path, std::string& out) {
  FILE* f = fopen(path, "rb");
  if (!f) return false;
  fseek(f, 0, SEEK_END);
  long sz = ftell(f);
  fseek(f, 0, SEEK_SET);
  if (sz < 0) { fclose(f); return false; }
  out.resize(static_cast<size_t>(sz));
  size_t got = fread(&out[0], 1, static_cast<size_t>(sz), f);
  fclose(f);
  out.resize(got);
  return true;
}

struct PResult {
  Arena* arena;
  DiagBag diags;
  std::vector<Token> toks;
  Node* root;
};

static PResult parse_source(const char* src, size_t len, const char* fn, bool is_header) {
  PResult r;
  r.arena = new Arena(128 * 1024 * 1024);  // 128 MiB bump 池
  r.diags = DiagBag{};
  r.root = nullptr;
  if (r.arena->oom()) { check(false, "arena OOM at start"); return r; }
  lex(reinterpret_cast<const uint8_t*>(src), len, fn, *r.arena, r.diags, r.toks);
  if (!r.arena->oom()) {
    r.root = parse(r.toks, fn, is_header, *r.arena, r.diags);
  }
  return r;
}

// ----------------------------- AST 遍历 ------------------------------------
static Node* find_first(Node* n, bool (*pred)(const Node*)) {
  if (!n) return nullptr;
  if (pred(n)) return n;
  Node* r = nullptr;
  for (Node* c = n->child; c; c = c->next) if ((r = find_first(c, pred))) return r;
  for (Node* c = n->type_args; c; c = c->next) if ((r = find_first(c, pred))) return r;
  for (Node* c = n->args; c; c = c->next) if ((r = find_first(c, pred))) return r;
  if (n->else_branch && (r = find_first(n->else_branch, pred))) return r;
  for (int k = 0; k < n->n_subs; ++k) if ((r = find_first(&n->subs[k], pred))) return r;
  return nullptr;
}

static int count_kind(Node* n, Kind k) {
  if (!n) return 0;
  int c = (n->kind == k) ? 1 : 0;
  for (Node* ch = n->child; ch; ch = ch->next) c += count_kind(ch, k);
  for (Node* ch = n->type_args; ch; ch = ch->next) c += count_kind(ch, k);
  for (Node* ch = n->args; ch; ch = ch->next) c += count_kind(ch, k);
  if (n->else_branch) c += count_kind(n->else_branch, k);
  for (int kk = 0; kk < n->n_subs; ++kk) c += count_kind(&n->subs[kk], k);
  return c;
}

static bool has_diag_code(const DiagBag& d, const char* code) {
  for (int i = 0; i < d.count; ++i) {
    if (d.items[i].code && strcmp(d.items[i].code, code) == 0) return true;
  }
  return false;
}

static const Diag* find_diag_code(const DiagBag& d, const char* code) {
  for (int i = 0; i < d.count; ++i) {
    if (d.items[i].code && strcmp(d.items[i].code, code) == 0) return &d.items[i];
  }
  return nullptr;
}

// ===========================================================================
// 验收 #1：examples/support_bot 四个文件全部解析成功 + AST 节点数 golden
// ===========================================================================
static void test_examples_parse(const char* root) {
  printf("[#1] examples/support_bot 四文件解析 + golden\n");
  const char* files[4] = {
      "examples/support_bot/models/order.h",
      "examples/support_bot/models/intent.h",
      "examples/support_bot/support.h",
      "examples/support_bot/support.hermes",
  };
  bool is_hdr[4] = {true, true, true, false};
  int ok = 0;
  for (int i = 0; i < 4; ++i) {
    std::string full = std::string(root) + "/" + files[i];
    std::string src;
    bool rd = read_file(full.c_str(), src);
    check(rd, "读取 %s 失败", files[i]);
    if (!rd) continue;
    PResult r = parse_source(src.c_str(), src.size(), files[i], is_hdr[i]);
    bool no_err = (r.diags.count == 0);
    if (no_err) ok++;
    check(no_err, "%s 解析出现 %d 条诊断", files[i], r.diags.count);
    if (!no_err) {
      for (int k = 0; k < r.diags.count; ++k)
        printf("      %s: %s\n", r.diags.items[k].code, r.diags.items[k].message);
    }
    delete r.arena;
  }
  check(ok == 4, "应有 4 个文件零错误解析，实际 %d", ok);

  // golden：把 support.hermes 的 AST dump 与 committed golden 比对。
  std::string full = std::string(root) + "/examples/support_bot/support.hermes";
  std::string src;
  read_file(full.c_str(), src);
  PResult r = parse_source(src.c_str(), src.size(), "examples/support_bot/support.hermes", false);
  char buf[1 << 20];
  size_t len = 0;
  ast_dump(r.root, buf, sizeof(buf), &len);
  std::string golden_path = std::string(root) + "/tests/compiler/ast-support_bot.golden";
  FILE* gf = fopen(golden_path.c_str(), "rb");
  if (!gf) {
    // bootstrap：首次运行写入 golden（后续 CI 走比对分支）。
    FILE* wf = fopen(golden_path.c_str(), "wb");
    if (wf) { fwrite(buf, 1, len, wf); fclose(wf); }
    check(true, "golden 已生成（首次运行）: %s", "ast-support_bot.golden");
  } else {
    fseek(gf, 0, SEEK_END);
    long gsz = ftell(gf);
    fseek(gf, 0, SEEK_SET);
    std::string g((size_t)gsz, '\0');
    // fread 在部分平台上带 warn_unused_result；-Werror 下不能忽略返回值。
    // 顺手把它变成一条真实断言：读不满就是 golden 文件有问题。
    size_t got = fread(&g[0], 1, (size_t)gsz, gf);
    fclose(gf);
    check(got == (size_t)gsz, "golden 读取不完整（%zu/%ld）", got, gsz);
    bool match = (g.size() == len && memcmp(g.data(), buf, len) == 0);
    check(match, "support.hermes AST dump 与 golden 不一致（%zu vs %zu 字节）", len, g.size());
  }
  int nodes = ast_count(r.root);
  printf("      support.hermes AST 节点数 = %d\n", nodes);
  check(nodes > 0, "节点数应 > 0");
  delete r.arena;
}

// ===========================================================================
// 验收 #2：双模式词法（字符串插值）
// ===========================================================================
static void test_dual_mode_lex() {
  printf("[#2] 双模式词法（字符串插值）\n");
  // "a{{b" 应得字面 "a{b"（未闭合，但已收集字面）
  {
    const char* src = "x = \"a{{b\";";
    PResult r = parse_source(src, strlen(src), "t.h", true);
    bool found = false;
    for (size_t i = 0; i < r.toks.size(); ++i) {
      if (r.toks[i].kind == Tok::STRING) {
        // 期望恰好一个字面片段，内容为 "a{b"
        if (r.toks[i].n_parts == 1 && !r.toks[i].parts[0].is_path &&
            strcmp(r.toks[i].parts[0].text, "a{b") == 0) {
          found = true;
        }
      }
    }
    check(found, "\"a{{b\" 应解析为字面 'a{b'（单 Lit 段）");
    delete r.arena;
  }
  // "x{y}" 应得 Lit("x") + Path("y") 两段
  {
    const char* src = "x = \"x{y}\";";
    PResult r = parse_source(src, strlen(src), "t.h", true);
    bool found = false;
    for (size_t i = 0; i < r.toks.size(); ++i) {
      if (r.toks[i].kind == Tok::STRING) {
        if (r.toks[i].n_parts == 2 &&
            !r.toks[i].parts[0].is_path && strcmp(r.toks[i].parts[0].text, "x") == 0 &&
            r.toks[i].parts[1].is_path && strcmp(r.toks[i].parts[1].text, "y") == 0) {
          found = true;
        }
      }
    }
    check(found, "\"x{y}\" 应得 Lit(\"x\") + Path(\"y\") 两段（注：规范 §2.6 I-1/I-2 实为 2 段，非 3 段，见报告）");
    delete r.arena;
  }
}

// ===========================================================================
// 验收 #3：<T> 消歧
// ===========================================================================
static void test_angle_disambiguation() {
  printf("[#3] <T> 消歧（类型实参 vs 关系运算）\n");
  // 3a: model.generate<Intent>(...) 是类型实参
  {
    const char* src = "workflow w() { auto i = model.generate<Intent>(prompt = \"x\", precision = \"exact\"); }";
    PResult r = parse_source(src, strlen(src), "t.h", false);
    bool ok = false;
    Node* call = find_first(r.root, [](const Node* n) {
      return n->kind == Kind::CallExpr && n->type_args != nullptr &&
             n->type_args->kind == Kind::Type &&
             n->type_args->text && strcmp(n->type_args->text, "Intent") == 0;
    });
    ok = (call != nullptr);
    check(ok, "model.generate<Intent>(...) 应解析为带类型实参 Intent 的调用");
    check(!has_diag_code(r.diags, "E2001"), "合法类型实参不应报 E2001");
    delete r.arena;
  }
  // 3b: a < b 是 LT 关系运算（无 E2001）
  {
    const char* src = "workflow w() { auto r = a < b; }";
    PResult r = parse_source(src, strlen(src), "t.h", false);
    Node* lt = find_first(r.root, [](const Node* n) {
      return n->kind == Kind::BinaryExpr && n->text && strcmp(n->text, "<") == 0;
    });
    check(lt != nullptr, "a < b 应解析为 LT 二元表达式");
    check(!has_diag_code(r.diags, "E2001"), "a < b 不应报 E2001");
    delete r.arena;
  }
  // 3c: model.generate<Intent(x) 缺 '>' → E2001
  {
    const char* src = "workflow w() { auto i = model.generate<Intent(x); }";
    PResult r = parse_source(src, strlen(src), "t.h", false);
    const Diag* d = find_diag_code(r.diags, "E2001");
    check(d != nullptr, "model.generate<Intent( 应报 E2001");
    if (d) {
      check(strstr(d->message, "期望") != nullptr && strstr(d->message, "(") != nullptr,
            "E2001 信息应含 '期望' 与 '('：%s", d->message);
    }
    delete r.arena;
  }
}

// ===========================================================================
// 验收 #4：上下文关键字
// ===========================================================================
static void test_contextual_keywords() {
  printf("[#4] 上下文关键字（entry / env）\n");
  // 4a: ) entry { → entry 标注
  {
    const char* src = "workflow f() entry { }";
    PResult r = parse_source(src, strlen(src), "t.h", false);
    Node* wf = find_first(r.root, [](const Node* n) {
      return (n->kind == Kind::WorkflowDecl || n->kind == Kind::WorkflowSig) &&
             n->text && strcmp(n->text, "f") == 0;
    });
    check(wf != nullptr && wf->is_entry == true, "') entry {' 应判定为 entry 标注");
    delete r.arena;
  }
  // 4b: ) { → 无 entry
  {
    const char* src = "workflow f() { }";
    PResult r = parse_source(src, strlen(src), "t.h", false);
    Node* wf = find_first(r.root, [](const Node* n) {
      return (n->kind == Kind::WorkflowDecl || n->kind == Kind::WorkflowSig) &&
             n->text && strcmp(n->text, "f") == 0;
    });
    check(wf != nullptr && wf->is_entry == false, "') {' 不应判定为 entry");
    delete r.arena;
  }
  // 4c: auto env = 1; 里 env 是普通 Ident（不报错）
  {
    const char* src = "workflow f() { auto env = 1; }";
    PResult r = parse_source(src, strlen(src), "t.h", false);
    check(r.diags.count == 0, "auto env = 1; 中 env 应作普通标识，不应报错（实际 %d 条）", r.diags.count);
    Node* v = find_first(r.root, [](const Node* n) { return n->kind == Kind::LetStmt; });
    bool env_ok = false;
    if (v && v->child && v->child->kind == Kind::Pattern && v->child->child) {
      env_ok = (v->child->child->text && strcmp(v->child->child->text, "env") == 0);
    }
    check(env_ok, "auto 的模式标识应为 'env'");
    delete r.arena;
  }
}

// ===========================================================================
// 验收 #5：#include / #link 单 token + #link 缺 ; → E2002
// ===========================================================================
static void test_directives() {
  printf("[#5] #include / #link 词法 + 缺 ; 诊断\n");
  // 5a: #include <std/net.h> 单 token
  {
    const char* src = "#include <std/net.h>\nworkflow f() {}";
    PResult r = parse_source(src, strlen(src), "t.h", true);
    int inc = 0; bool ok_spec = false;
    for (size_t i = 0; i < r.toks.size(); ++i) {
      if (r.toks[i].kind == Tok::INCLUDE) {
        inc++;
        if (r.toks[i].text && strcmp(r.toks[i].text, "<std/net.h>") == 0 && r.toks[i].sub == 0) ok_spec = true;
      }
    }
    check(inc == 1, "#include <std/net.h> 应恰好成 1 个 INCLUDE token（实际 %d）", inc);
    check(ok_spec, "INCLUDE 的 spec 应为 '<std/net.h>' 且 mode=angle");
    check(!has_diag_code(r.diags, "E2002"), "#include 合法不应报 E2002");
    delete r.arena;
  }
  // 5b: #link <std/net>; 单 token
  {
    const char* src = "#link <std/net>;\nworkflow f() {}";
    PResult r = parse_source(src, strlen(src), "t.hermes", false);
    int lnk = 0; bool ok_spec = false;
    for (size_t i = 0; i < r.toks.size(); ++i) {
      if (r.toks[i].kind == Tok::LINK) {
        lnk++;
        if (r.toks[i].text && strcmp(r.toks[i].text, "<std/net>") == 0 && r.toks[i].sub == 0) ok_spec = true;
      }
    }
    check(lnk == 1, "#link <std/net>; 应恰好成 1 个 LINK token（实际 %d）", lnk);
    check(ok_spec, "LINK 的 spec 应为 '<std/net>' 且 mode=angle");
    check(!has_diag_code(r.diags, "E2002"), "#link 合法不应报 E2002");
    delete r.arena;
  }
  // 5c: #link 的分号是**可选**的 —— 新写法不带分号，旧写法继续接受。
  {
    const char* src = "#link <std/net>\nworkflow f() {}";
    PResult r = parse_source(src, strlen(src), "t.hermes", false);
    check(!has_diag_code(r.diags, "E2002"), "#link 不带分号不应报错");
    int lnk = 0;
    for (size_t i = 0; i < r.toks.size(); ++i) {
      if (r.toks[i].kind == Tok::LINK) lnk++;
    }
    check(lnk == 1, "不带分号的 #link 也应成 1 个 LINK token（实际 %d）", lnk);
    delete r.arena;
  }
}

// ===========================================================================
// 验收 #6：workflow 名用引号 → E2002；input 无类型 → E3023
// ===========================================================================
static void test_bad_workflow() {
  printf("[#6] workflow 名引号 / input 无类型 诊断\n");
  // 6a: 字符串名 → E2002
  {
    const char* src = "workflow \"x\" () -> string;";
    PResult r = parse_source(src, strlen(src), "t.h", true);
    const Diag* d = find_diag_code(r.diags, "E2002");
    check(d != nullptr, "workflow 用引号名应报 E2002");
    if (d) check(strstr(d->message, "标识符") != nullptr, "E2002 应指明名字是标识符：%s", d->message);
    delete r.arena;
  }
  // 6b: input 无类型 → E3023（逐字：LANGUAGE §11.2 ⑪）
  {
    const char* src = "export workflow support_bot(input, backend = homonoia) entry -> string;";
    PResult r = parse_source(src, strlen(src), "t.h", true);
    const Diag* d = find_diag_code(r.diags, "E3023");
    check(d != nullptr, "input 无类型应报 E3023");
    if (d) check(strstr(d->message, "input 参数必须写类型") != nullptr, "E3023 文案应逐字 'input 参数必须写类型'：%s", d->message);
    delete r.arena;
  }
}

// ===========================================================================
// 验收 #7：每条诊断含 line/col/end_line/end_col/code/message/hint
// ===========================================================================
static void test_diag_shape() {
  printf("[#7] 诊断结构完整性\n");
  const char* src = "workflow w() { auto i = model.generate<Intent(x); }";
  PResult r = parse_source(src, strlen(src), "t.h", false);
  // 任意产生诊断的输入即可；这里一定有 E2001
  check(r.diags.count > 0, "应至少产生一条诊断用于结构校验");
  bool shape_ok = true;
  for (int i = 0; i < r.diags.count; ++i) {
    const Diag& d = r.diags.items[i];
    if (!d.code || !d.message) shape_ok = false;
    if (d.loc.line <= 0 || d.loc.col <= 0 || d.loc.end_line <= 0 || d.loc.end_col <= 0) shape_ok = false;
    if (!d.why || !d.fix) shape_ok = false;  // why/fix 充当 hint
  }
  check(shape_ok, "每条诊断须含 code/message/line/col/end_line/end_col/why/fix");
  delete r.arena;
}

// ===========================================================================
// 验收 #8：语法错误后仍报出后续错误（>= 3 条，--keep-going 精神）
// ===========================================================================
static void test_keep_going() {
  printf("[#8] 错误恢复（一次报出多条）\n");
  const char* src =
      "workflow \"a\" ();\n"
      "struct { int x; };\n"
      "backend foo\n";
  PResult r = parse_source(src, strlen(src), "t.h", true);
  check(r.diags.count >= 3, "应报出 >= 3 条错误（实际 %d）", r.diags.count);
  check(r.root != nullptr, "出错后解析器仍应产出 Module 根（不中断）");
  delete r.arena;
}

// ===========================================================================
// 验收 #9：8 MiB / 2M token 上限各自报码
// ===========================================================================
static void test_limits() {
  printf("[#9] 8 MiB / 2M token 上限\n");
  // 9a: >8 MiB 源（全是行注释，几乎不产生 token）→ E1002，无 E1003
  {
    std::string big;
    big.reserve(9 * 1024 * 1024);
    while (big.size() < 9u * 1024u * 1024u) big += "// comment line\n";
    PResult r = parse_source(big.c_str(), big.size(), "big.h", true);
    check(has_diag_code(r.diags, "E1002"), ">8MiB 源应报 E1002");
    check(!has_diag_code(r.diags, "E1003"), "纯注释 >8MiB 不应触发 token 上限 E1003");
    delete r.arena;
  }
  // 9b: >2M token（"1 " 重复，<8MiB）→ E1003，无 E1002
  {
    std::string big;
    for (int i = 0; i < 2000050; ++i) big += "1 ";
    PResult r = parse_source(big.c_str(), big.size(), "tok.h", true);
    check(has_diag_code(r.diags, "E1003"), ">2M token 应报 E1003");
    check(!has_diag_code(r.diags, "E1002"), "2M 小 token 不应触发 8MiB 的 E1002");
    delete r.arena;
  }
}

// ===========================================================================
// 六条结构化断言（golden 之外的真正证明）
// ===========================================================================
static void test_structured_assertions(const char* root) {
  printf("[结构化断言] 六条手写断言\n");
  // 1. support.hermes 恰好 2 个 LINK，spec 为 <std/net> 与 <std/model>（不含 <std/runtime>）
  {
    std::string full = std::string(root) + "/examples/support_bot/support.hermes";
    std::string src; read_file(full.c_str(), src);
    PResult r = parse_source(src.c_str(), src.size(), "support.hermes", false);
    int nlink = count_kind(r.root, Kind::Link);
    check(nlink == 2, "support.hermes 应恰好 2 个 #link（实际 %d）", nlink);
    bool has_net = false, has_model = false, has_runtime = false;
    // 全树扫描 link
    std::vector<const char*> specs;
    auto scan = [&](auto& self, Node* n) -> void {
      if (!n) return;
      if (n->kind == Kind::Link && n->text) specs.push_back(n->text);
      for (Node* c = n->child; c; c = c->next) self(self, c);
      for (Node* c = n->type_args; c; c = c->next) self(self, c);
      for (Node* c = n->args; c; c = c->next) self(self, c);
      if (n->else_branch) self(self, n->else_branch);
      for (int k = 0; k < n->n_subs; ++k) self(self, &n->subs[k]);
    };
    scan(scan, r.root);
    for (auto s : specs) {
      if (strcmp(s, "<std/net>") == 0) has_net = true;
      if (strcmp(s, "<std/model>") == 0) has_model = true;
      if (strstr(s, "runtime") != nullptr) has_runtime = true;
    }
    check(has_net && has_model, "#link 应含 <std/net> 与 <std/model>");
    check(!has_runtime, "#link 不应含 <std/runtime>（它是隐式内置）");
    delete r.arena;
  }
  // 2. support_bot 的 entry 标注为真
  {
    std::string full = std::string(root) + "/examples/support_bot/support.hermes";
    std::string src; read_file(full.c_str(), src);
    PResult r = parse_source(src.c_str(), src.size(), "support.hermes", false);
    Node* wf = find_first(r.root, [](const Node* n) {
      return (n->kind == Kind::WorkflowDecl) && n->text && strcmp(n->text, "support_bot") == 0;
    });
    check(wf != nullptr && wf->is_entry == true, "workflow support_bot 应有 entry 标注");
    delete r.arena;
  }
  // 3. Intent 字段名集合 == {action, order}
  {
    std::string full = std::string(root) + "/examples/support_bot/models/intent.h";
    std::string src; read_file(full.c_str(), src);
    PResult r = parse_source(src.c_str(), src.size(), "intent.h", true);
    Node* st = find_first(r.root, [](const Node* n) {
      return n->kind == Kind::StructDecl && n->text && strcmp(n->text, "Intent") == 0;
    });
    bool ok = false;
    if (st) {
      int nf = 0; bool a = false, o = false;
      for (Node* f = st->child; f; f = f->next) {
        nf++;
        if (f->text && strcmp(f->text, "action") == 0) a = true;
        if (f->text && strcmp(f->text, "order") == 0) o = true;
      }
      ok = (nf == 2 && a && o);
    }
    check(ok, "struct Intent 字段名集合应为 {action, order}");
    delete r.arena;
  }
  // 4. match 节点臂数 == 2，最后一臂 default
  {
    std::string full = std::string(root) + "/examples/support_bot/support.hermes";
    std::string src; read_file(full.c_str(), src);
    PResult r = parse_source(src.c_str(), src.size(), "support.hermes", false);
    Node* m = find_first(r.root, [](const Node* n) { return n->kind == Kind::MatchStmt; });
    int narms = 0; bool last_default = false;
    if (m) {
      Node* a = m->args;
      for (; a; a = a->next) { narms++; }
      // 重新取末臂
      for (a = m->args; a && a->next; a = a->next) {}
      if (a) last_default = (a->is_nullable == true);  // 用 is_nullable 作 default 标记
    }
    check(narms == 2, "match 应有 2 条臂（实际 %d）", narms);
    check(last_default, "match 最后一臂应为 default");
    delete r.arena;
  }
  // 5. model.generate<Intent> 的类型实参是 Intent（不是 LT 表达式）
  {
    std::string full = std::string(root) + "/examples/support_bot/support.hermes";
    std::string src; read_file(full.c_str(), src);
    PResult r = parse_source(src.c_str(), src.size(), "support.hermes", false);
    Node* call = find_first(r.root, [](const Node* n) {
      if (n->kind != Kind::CallExpr || !n->child) return false;
      // callee 是 model.generate
      Node* callee = n->child;
      if (callee->kind != Kind::PathExpr) return false;
      bool is_gen = false;
      if (callee->n_subs >= 2 && callee->subs[1].text && strcmp(callee->subs[1].text, "generate") == 0) is_gen = true;
      return is_gen && n->type_args && n->type_args->kind == Kind::Type &&
             n->type_args->text && strcmp(n->type_args->text, "Intent") == 0;
    });
    check(call != nullptr, "model.generate<Intent> 的类型实参是 Intent 而非 LT 表达式");
    delete r.arena;
  }
  // 6. OrderInfo 字段名集合 == {order_id, status, amount}
  {
    std::string full = std::string(root) + "/examples/support_bot/models/order.h";
    std::string src; read_file(full.c_str(), src);
    PResult r = parse_source(src.c_str(), src.size(), "order.h", true);
    Node* st = find_first(r.root, [](const Node* n) {
      return n->kind == Kind::StructDecl && n->text && strcmp(n->text, "OrderInfo") == 0;
    });
    bool ok = false;
    if (st) {
      int nf = 0; bool o = false, s = false, a = false;
      for (Node* f = st->child; f; f = f->next) {
        nf++;
        if (f->text && strcmp(f->text, "order_id") == 0) o = true;
        if (f->text && strcmp(f->text, "status") == 0) s = true;
        if (f->text && strcmp(f->text, "amount") == 0) a = true;
      }
      ok = (nf == 3 && o && s && a);
    }
    check(ok, "struct OrderInfo 字段名集合应为 {order_id, status, amount}");
    delete r.arena;
  }
}

// ===========================================================================
// auth 子句（LANGUAGE §3.1 注 8）：六条手写结构化断言
//
//   BackendDef = "backend" Ident "=" ( StringLit | EnvExpr ) [ "auth" EnvExpr ] ";" .
//
// 为什么不能只靠 golden：golden 只证明"同一份输入得到同一份 dump"，它无法证明
// "多给一个默认值必须被拒"——拒的那条根本没有 dump。所以下面六条逐项断言：
// dump 里看得见的槽位（BackendDef.args / EnvExpr.child / EnvExpr.args）
// 与看不见的诊断（E2018 的码与定稿文案）都要钉住。
// ===========================================================================
static void test_auth_clause() {
  printf("[auth] backend 定义的 auth 子句（§3.1 注 8）\n");

  // A1: env 地址 + auth env("K") → 成功；args 是 EnvExpr，child="K"，自身无默认值
  {
    const char* src = "backend a = env(\"U\", \"http://x\") auth env(\"K\");";
    PResult r = parse_source(src, strlen(src), "t.hermes", false);
    check(r.diags.count == 0, "A1 'env 地址 + auth' 应零诊断（实际 %d 条）", r.diags.count);
    Node* bd = nullptr;
    for (Node* c = (r.root ? r.root->child : nullptr); c; c = c->next) {
      if (c->kind == Kind::BackendDef && c->text && strcmp(c->text, "a") == 0) { bd = c; break; }
    }
    check(bd != nullptr, "A1 应产出名为 a 的 BackendDef");
    check(bd && bd->child && bd->child->kind == Kind::EnvExpr,
          "A1 BackendDef.child 仍是地址（EnvExpr）");
    check(bd && bd->args != nullptr, "A1 BackendDef.args 应挂 auth 子句（非空）");
    check(bd && bd->args && bd->args->kind == Kind::EnvExpr,
          "A1 auth 节点应是 EnvExpr");
    check(bd && bd->args && bd->args->child && bd->args->child->text &&
          strcmp(bd->args->child->text, "K") == 0,
          "A1 auth 的变量名应是 K");
    check(bd && bd->args && bd->args->args == nullptr,
          "A1 auth 的 env 不许有默认值（其 args 必须为空）");
    delete r.arena;
  }

  // A2: 字符串字面量地址 + auth 也合法（已裁决语义，不加限制）
  {
    const char* src = "backend a = \"http://127.0.0.1:8080\" auth env(\"K\");";
    PResult r = parse_source(src, strlen(src), "t.hermes", false);
    check(r.diags.count == 0, "A2 字面量地址带 auth 应零诊断（实际 %d 条）", r.diags.count);
    Node* bd = nullptr;
    for (Node* c = (r.root ? r.root->child : nullptr); c; c = c->next) {
      if (c->kind == Kind::BackendDef && c->text && strcmp(c->text, "a") == 0) { bd = c; break; }
    }
    check(bd && bd->child && bd->child->kind == Kind::LitExpr,
          "A2 字面量地址应仍解析成 LitExpr");
    check(bd && bd->args && bd->args->kind == Kind::EnvExpr,
          "A2 字面量地址后也能挂 auth 的 EnvExpr");
    delete r.arena;
  }

  // A3: 无 auth 子句 → args 必须是 nullptr（新逻辑不许把槽位"顺便"填上）
  {
    const char* src = "backend a = env(\"U\", \"http://x\");";
    PResult r = parse_source(src, strlen(src), "t.hermes", false);
    check(r.diags.count == 0, "A3 无子句应零诊断（实际 %d 条）", r.diags.count);
    Node* bd = nullptr;
    for (Node* c = (r.root ? r.root->child : nullptr); c; c = c->next) {
      if (c->kind == Kind::BackendDef && c->text && strcmp(c->text, "a") == 0) { bd = c; break; }
    }
    check(bd != nullptr, "A3 应产出名为 a 的 BackendDef");
    check(bd && bd->args == nullptr, "A3 无 auth 子句时 BackendDef.args 必须为 nullptr");
    delete r.arena;
  }

  // A4: auth 的 env 给了第二实参（默认值）→ E2018，且文案里必须出现 "401"
  {
    const char* src = "backend a = env(\"U\", \"http://x\") auth env(\"K\", \"d\");";
    PResult r = parse_source(src, strlen(src), "t.hermes", false);
    const Diag* d = find_diag_code(r.diags, "E2018");
    check(d != nullptr, "A4 auth env 给默认值应报 E2018");
    if (d) {
      check(strstr(d->message, "401") != nullptr,
            "A4 E2018 文案必须说清后果（含 '401'）：%s", d->message);
      check(strstr(d->message, "密钥没有安全默认值") != nullptr,
            "A4 E2018 文案必须以定稿句开头：%s", d->message);
    }
    check(!has_diag_code(r.diags, "E1001"),
          "A4 不许多报一条 E1001（E2018 已经把恢复做干净）");
    delete r.arena;
  }

  // A5: 重复 auth 子句 → 同一个 E2018
  {
    const char* src =
        "backend a = env(\"U\", \"http://x\") auth env(\"K\") auth env(\"K2\");";
    PResult r = parse_source(src, strlen(src), "t.hermes", false);
    const Diag* d = find_diag_code(r.diags, "E2018");
    check(d != nullptr, "A5 重复 auth 子句应报 E2018");
    if (d) {
      check(strstr(d->message, "最多一个 auth 子句") != nullptr,
            "A5 重复子句的文案应点名'最多一个'：%s", d->message);
    }
    delete r.arena;
  }

  // A6: 上下文关键字反例 —— auth 只在"backend 地址之后"才被当子句
  //     6a: `auto auth = 1;` 里 auth 是普通标识符
  {
    const char* src = "workflow w() { auto auth = 1; }";
    PResult r = parse_source(src, strlen(src), "t.hermes", false);
    check(r.diags.count == 0, "A6a auto auth = 1; 不应报错（实际 %d 条）", r.diags.count);
    Node* v = find_first(r.root, [](const Node* n) { return n->kind == Kind::LetStmt; });
    bool pat_ok = false;
    if (v && v->child && v->child->kind == Kind::Pattern && v->child->child) {
      pat_ok = (v->child->child->text && strcmp(v->child->child->text, "auth") == 0);
    }
    check(pat_ok, "A6a auto 的模式标识应为普通标识符 'auth'");
    delete r.arena;
  }
  //     6b: 同一文件里 backend 带 auth 子句 + body 里用名叫 auth 的变量，两者共存
  {
    const char* src =
        "backend a = env(\"U\", \"http://x\") auth env(\"K\");\n"
        "workflow w() { auto auth = 1; auto b = auth; }";
    PResult r = parse_source(src, strlen(src), "t.hermes", false);
    check(r.diags.count == 0, "A6b backend 子句与 auto auth=1 共存应零诊断（实际 %d 条）",
          r.diags.count);
    Node* bd = nullptr;
    for (Node* c = (r.root ? r.root->child : nullptr); c; c = c->next) {
      if (c->kind == Kind::BackendDef) bd = c;
    }
    check(bd && bd->args && bd->args->kind == Kind::EnvExpr,
          "A6b backend 那一句仍应拿到 auth 子句");
    // workflow 体里的 `auto b = auth;` 的初值应是路径表达式 auth（不是 env 调用）
    bool ref_ok = false;
    Node* let_b = find_first(r.root, [](const Node* n) {
      if (n->kind != Kind::LetStmt || !n->args) return false;
      Node* p = n->child;
      return p && p->child && p->child->text && strcmp(p->child->text, "b") == 0;
    });
    // PathExpr 的分量落在 subs（不是 child）：单分量路径 n_subs == 1。
    if (let_b && let_b->args->kind == Kind::PathExpr && let_b->args->n_subs == 1 &&
        let_b->args->subs[0].text) {
      ref_ok = (strcmp(let_b->args->subs[0].text, "auth") == 0);
    }
    check(ref_ok, "A6b `auto b = auth;` 的初值应是标识符引用 auth");
    delete r.arena;
  }

  // A7: 地址与 auth 用同一个环境变量名是**允许**的（§3.1 注 8 边界一：
  //     这不是可静态判定的错，同变量装两个值只有运行期解析出来才知道）
  {
    const char* src = "backend h = env(\"H\", \"http://x\") auth env(\"H\");";
    PResult r = parse_source(src, strlen(src), "t.hermes", false);
    check(r.diags.count == 0, "A7 地址与 auth 同名不该报错（实际 %d 条）", r.diags.count);
    delete r.arena;
  }
}

// ===========================================================================
// 字符串内容怎么取（`Token::text` 对 STRING 恒空，内容只在 parts）
//
// 约定（**不是"尚未实现"**）：`Tok::STRING` 的 `Token::text` 恒为 nullptr。
// 带插值的字符串不存在单一标量内容（`"a{x}b"` 拼成什么都是错的），所以内容只能
// 住在 `parts`；`text` 留空恰好把这条事实建模清楚。给无插值的字符串补一份 `text`
// 等于给 `parts` 加缓存，而缓存总有一天不同步——所以正确的做法是：**产生方不动，
// 改所有假定它有值的读法**，统一走 `str_const_text()`。
//
// 本节的断言分两类：
//   · 正向：所有"只接受常量"的位置（env 名、默认地址、auth 名、字面量地址、
//     case 标签）在 AST 里都拿到**真值**；
//   · 负向（守卫）：**每一个** STRING token 的 `text` 都是 nullptr —— 将来有人
//     "顺手"在词法器里补一份 text，这条立刻红。
//
// AST 里的分工别混：表达式位置的 `LitExpr.text` 是**类型标记**（"string"），
// 内容在 `subs` 的 `StringPartNode` 里（既有契约，本节不改）；backend 地址 /
// EnvExpr 的子 LitExpr / MatchArm 标签才是把内容直接放进 text 的。
// ===========================================================================
static void test_string_literal_text(const char* root) {
  printf("[str] 字符串内容的唯一读法（Token::text 恒空，内容在 parts）\n");

  // S1/S2: 地址位 env 的变量名与默认地址都必须是真值（golden 里变的正是这两行）。
  {
    std::string full = std::string(root) + "/examples/support_bot/support.hermes";
    std::string src; read_file(full.c_str(), src);
    PResult r = parse_source(src.c_str(), src.size(), "support.hermes", false);
    check(r.diags.count == 0, "S1 support.hermes 应零诊断（实际 %d 条）", r.diags.count);
    Node* env = find_first(r.root, [](const Node* n) { return n->kind == Kind::EnvExpr; });
    check(env != nullptr, "S1 应找到地址位的 EnvExpr");
    check(env && env->child && env->child->text &&
          strcmp(env->child->text, "HOMONOIA_URL") == 0,
          "S1 env 的变量名应为 HOMONOIA_URL（实际 %s）",
          (env && env->child && env->child->text) ? env->child->text : "(null)");
    check(env && env->args && env->args->text &&
          strcmp(env->args->text, "http://127.0.0.1:8080") == 0,
          "S2 env 的默认地址应为 http://127.0.0.1:8080（实际 %s）",
          (env && env->args && env->args->text) ? env->args->text : "(null)");
    delete r.arena;
  }

  // S3: 空串与"非常量"必须是两件事（在读取器层面区分，token 层面两者 text 都是
  //     nullptr —— 这正是上一条约定）：
  //       · `env("", "http://x")` → 空串**是**常量，读取器给出 ""（非空指针）
  //       · `env("U{x}", "http://y")` → 插值**不是**常量 → E3018
  //     如果实现把这两件事混成同一个值，第二个会静默通过、第一个会莫名失败。
  {
    const char* src = "backend a = env(\"\", \"http://x\");";
    PResult r = parse_source(src, strlen(src), "t.hermes", false);
    check(r.diags.count == 0, "S3 空串实参应零诊断（实际 %d 条）", r.diags.count);
    Node* env = find_first(r.root, [](const Node* n) { return n->kind == Kind::EnvExpr; });
    const char* nm = (env && env->child) ? env->child->text : nullptr;
    check(nm != nullptr, "S3 空串是常量：读取器必须给出非空指针（空串与非常量不同）");
    check(nm != nullptr && nm[0] == '\0', "S3 空串的内容必须是空串");
    delete r.arena;
  }
  {
    const char* src = "backend a = env(\"U{x}\", \"http://y\");";
    PResult r = parse_source(src, strlen(src), "t.hermes", false);
    check(has_diag_code(r.diags, "E3018"),
          "S3 插值实参不是常量：应报 E3018（实际 %d 条诊断）", r.diags.count);
    Node* env = find_first(r.root, [](const Node* n) { return n->kind == Kind::EnvExpr; });
    check(env == nullptr || env->child == nullptr || env->child->text == nullptr,
          "S3 插值实参不得被当成常量塞进 AST");
    delete r.arena;
  }

  // S4: method = "GET" —— T32 的前提。（token 级 text 恒为 nullptr，由 S7 统一
  //     守卫，这里不重复断言。）AST 级：实参值 LitExpr.text 是**类型标记**
  //     "string"，真正的内容在 subs[0]。T32 若读 LitExpr.text 期待 "GET"
  //     会永远读到 "string"。
  {
    std::string full = std::string(root) + "/examples/support_bot/support.hermes";
    std::string src; read_file(full.c_str(), src);
    PResult r = parse_source(src.c_str(), src.size(), "support.hermes", false);
    Node* arg = find_first(r.root, [](const Node* n) {
      return n->kind == Kind::Param && n->text && strcmp(n->text, "method") == 0;
    });
    check(arg != nullptr, "S4 应找到名为 method 的实参");
    bool ast_ok = false;
    if (arg && arg->args && arg->args->kind == Kind::LitExpr) {
      Node* lit = arg->args;
      ast_ok = (lit->text && strcmp(lit->text, "string") == 0) &&  // 类型标记
               lit->n_subs == 1 && lit->subs[0].text &&
               strcmp(lit->subs[0].text, "GET") == 0;              // 真正的内容
    }
    check(ast_ok, "S4 method 的实参应是 LitExpr(text=\"string\") + subs[0]=\"GET\"");
    delete r.arena;
  }

  // S6: case "refund" 的标签走 MatchArm.text —— 又一个直接吃 token text 的消费点。
  {
    std::string full = std::string(root) + "/examples/support_bot/support.hermes";
    std::string src; read_file(full.c_str(), src);
    PResult r = parse_source(src.c_str(), src.size(), "support.hermes", false);
    Node* arm = find_first(r.root, [](const Node* n) {
      return n->kind == Kind::MatchArm && n->text != nullptr;
    });
    check(arm && arm->text && strcmp(arm->text, "refund") == 0,
          "S6 case 臂的标签应为 refund（实际 %s）",
          (arm && arm->text) ? arm->text : "(null)");
    delete r.arena;
  }

  // S7 = TestStringTokenTextIsAlwaysNull（**约定守卫**，不是"尚未实现"）。
  //
  // 对 examples 里**每一个** STRING token（`"HOMONOIA_URL"`、`"http://…"`、prompt、
  // `"GET"`、以及空串 `""`）断言 `text == nullptr`，同时断言内容确实能从 `parts`
  // 拼回来（无插值 ⇒ 至多一个非 path 片段；有插值 ⇒ 至少一个 path 片段）。
  //
  // 为什么这条重要：统一走 `str_const_text()` 的前提就是"没人再从 text 直读"。
  // 将来有人为了省一次调用，在词法器里"顺手"给无插值字符串补一份 text，这条
  // 立刻红 —— 因为那会造出**两份表示**，而两份表示总有一天不同步。空串 `""`
  // 也在断言范围内（它 n_parts == 0，text 仍是 nullptr）。
  {
    std::string full = std::string(root) + "/examples/support_bot/support.hermes";
    std::string src; read_file(full.c_str(), src);
    PResult r = parse_source(src.c_str(), src.size(), "support.hermes", false);
    int total = 0, konst = 0, interp = 0;
    bool text_all_null = true;
    bool parts_ok = true;
    for (size_t i = 0; i < r.toks.size(); ++i) {
      const Token& tk = r.toks[i];
      if (tk.kind != Tok::STRING) continue;
      total++;
      if (tk.text != nullptr) text_all_null = false;   // 有人给 text 填了值 → 破约定
      if (tk.n_parts < 0) parts_ok = false;
      bool has_path = false;
      for (int k = 0; k < tk.n_parts; ++k) {
        if (tk.parts[k].is_path) has_path = true;
        else if (tk.parts[k].text == nullptr) parts_ok = false;  // 字面片段必须有内容
      }
      if (has_path) {
        interp++;
      } else {
        konst++;
        if (tk.n_parts > 1) parts_ok = false;  // 无插值却多段 → 分段假设破了
      }
    }
    check(text_all_null,
          "S7/TestStringTokenTextIsAlwaysNull: 每个 STRING token 的 text 必须是 nullptr"
          "（total=%d）—— text 不是 parts 的缓存", total);
    check(parts_ok, "S7 STRING 的内容必须能从 parts 拼回（无插值 ≤1 段、字面段非空）");
    check(konst >= 5, "S7 support.hermes 至少应有 5 个常量字符串（实际 %d）", konst);
    check(interp >= 1, "S7 support.hermes 至少应有 1 个插值字符串（实际 %d）", interp);
    check(konst + interp == total, "S7 常量与插值应覆盖全部 STRING token（%d+%d vs %d）",
          konst, interp, total);
    delete r.arena;
  }

  // S7b: 空串 "" 也在守卫范围内：text 仍为 nullptr，n_parts == 0，
  //      但读取器（str_const_text）给出的是**非空指针指向空串**（见 S3）。
  {
    const char* src = "workflow w() { auto a = \"\"; }";
    PResult r = parse_source(src, strlen(src), "t.hermes", false);
    const Token* s = nullptr;
    for (size_t i = 0; i < r.toks.size(); ++i) {
      if (r.toks[i].kind == Tok::STRING) { s = &r.toks[i]; break; }
    }
    check(s != nullptr, "S7b 应找到一个 STRING token");
    check(s && s->text == nullptr, "S7b 空串的 token text 同样是 nullptr");
    check(s && s->n_parts == 0, "S7b 空串不应有任何片段（实际 %d）", s ? s->n_parts : -1);
    delete r.arena;
  }

  // S8: 字面量地址（不带 env）：内容也必须经 str_const_text 取到。
  {
    const char* src = "backend a = \"http://127.0.0.1:8080\" auth env(\"K\");";
    PResult r = parse_source(src, strlen(src), "t.hermes", false);
    Node* bd = nullptr;
    for (Node* c = (r.root ? r.root->child : nullptr); c; c = c->next) {
      if (c->kind == Kind::BackendDef) bd = c;
    }
    check(bd && bd->child && bd->child->text &&
          strcmp(bd->child->text, "http://127.0.0.1:8080") == 0,
          "S8 字面量地址的 LitExpr.text 应为地址原文（实际 %s）",
          (bd && bd->child && bd->child->text) ? bd->child->text : "(null)");
    delete r.arena;
  }
}

// ===========================================================================
// 公开 C ABI 冒烟（验证 hm_parse_* 纯函数接口端到端可用）
// ===========================================================================
static void test_c_api() {
  printf("[CAPI] 公开 C ABI 冒烟\n");
  check(strcmp(hm_parse_version(), "hermes-parser/0.1") == 0,
        "hm_parse_version 应返回 hermes-parser/0.1");
  const char* src = "workflow w() { auto x = 1; }";
  char dump[1 << 16];
  size_t dl = 0;
  char diag[1 << 16];
  size_t dl2 = 0;
  int nc = 0;
  int rc = hm_parse(reinterpret_cast<const uint8_t*>(src), strlen(src), "t.hermes", 0,
                    dump, sizeof(dump), &dl, diag, sizeof(diag), &dl2, &nc);
  check(rc == 0, "hm_parse 应返回 0（实际 %d）", rc);
  check(dl > 0, "hm_parse 应产出非空 AST dump（%zu 字节）", dl);
  check(nc > 0, "hm_parse 应产出 >0 节点（%d）", nc);
  int c2 = 0;
  int rc2 = hm_parse_count(reinterpret_cast<const uint8_t*>(src), strlen(src),
                           "t.hermes", 0, &c2);
  check(rc2 == 0 && c2 == nc, "hm_parse_count 应与 hm_parse 的节点数一致（%d vs %d）", c2, nc);
}

// ----------------------------- 入口 ----------------------------------------

// ---------------------------- T34 codegen（第一刀）---------------------------
//
// 这一刀只覆盖 seq / auto / return / 字面量 / 变量引用，但**链路是完整的**：
// 真实源码 → 词法 → 语法 → AST → canonical JSON。
// 断言重点是"canonical"与"编号规则"这两件最容易静默出错的事。
static void test_codegen() {
  const char* src =
      "workflow hello() entry -> string {\n"
      "    auto msg = \"hi\";\n"
      "    return msg;\n"
      "}\n";
  PResult r = parse_source(src, strlen(src), "hello.hermes", false);
  int errs = 0;
  for (int i = 0; i < r.diags.count; ++i) {
    if (r.diags.items[i].severity == Severity::Error) ++errs;
  }
  check(errs == 0, "codegen 用例本身应无语法错误（实际 %d 条）", errs);

  Node* wf = r.root ? r.root->child : nullptr;
  check(wf && wf->kind == Kind::WorkflowDecl, "应解析出一个 WorkflowDecl");

  hm::JVal out;
  std::string err;
  bool ok = hm::codegen_workflow(wf, &out, &err);
  check(ok, "codegen 应成功：%s", err.c_str());
  if (!ok) { delete r.arena; return; }

  const std::string js = out.str();

  // ① canonical：键按字节序，第一个键必须是 "backend"。
  check(js.rfind("{\"backend\":", 0) == 0, "workflow 对象的键序不是字节序：%.60s", js.c_str());
  check(js.find('\n') == std::string::npos, "canonical JSON 不该含换行");

  // ② 编号规则（IR.md §4.2）：前序 DFS，父先于子。
  //    seq=0 → let=1 → lit=2 → return=3 → var=4
  check(js.find("\"id\":0,\"k\":\"seq\"") != std::string::npos, "节点 0 应是 seq");
  check(js.find("\"id\":1,\"k\":\"let\"") != std::string::npos, "节点 1 应是 let");
  check(js.find("\"id\":2,\"k\":\"lit\"") != std::string::npos, "节点 2 应是 lit");
  check(js.find("\"id\":3,\"k\":\"return\"") != std::string::npos, "节点 3 应是 return");
  check(js.find("\"id\":4,\"k\":\"var\"") != std::string::npos, "节点 4 应是 var");

  // ③ 变量表：声明顺序即下标。
  check(js.find("\"name\":\"msg\"") != std::string::npos, "变量表里应有 msg");
  check(js.find("\"v\":\"hi\"") != std::string::npos, "字符串字面量的值应原样保留");

  // ④ 幂等：同一 AST 两次 codegen 字节相等（这是 golden 能成立的前提）。
  hm::JVal again;
  std::string err2;
  check(hm::codegen_workflow(wf, &again, &err2) && again.str() == js,
        "两次 codegen 应得到完全相同的字节");

  delete r.arena;
}

int main(int argc, char** argv) {
  const char* root = (argc > 1) ? argv[1] : ".";
  printf("=== Hermes 编译器前端 T09 验收测试 (root=%s) ===\n", root);

  test_examples_parse(root);
  test_dual_mode_lex();
  test_angle_disambiguation();
  test_contextual_keywords();
  test_directives();
  test_bad_workflow();
  test_diag_shape();
  test_keep_going();
  test_limits();
  test_c_api();
  test_structured_assertions(root);
  test_auth_clause();
  test_string_literal_text(root);
  test_codegen();

  printf("\n=== 结果: %d/%d 通过, %d 失败 ===\n", T.pass, T.total, T.fail);
  return T.fail == 0 ? 0 : 1;
}
