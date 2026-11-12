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
void check_types(Node* root, const char* filename, Arena& arena, DiagBag& diags);
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

// count_diag_code 数条数而不是只看有没有：W5004 是**启发式**警告，
// "多报"和"漏报"同样是缺陷 —— 多报会把这条警告变成噪声，漏报等于没实现。
static int count_diag_code(const DiagBag& d, const char* code) {
  int n = 0;
  for (int i = 0; i < d.count; ++i) {
    if (d.items[i].code && strcmp(d.items[i].code, code) == 0) ++n;
  }
  return n;
}

static const Diag* find_diag_code(const DiagBag& d, const char* code) {
  for (int i = 0; i < d.count; ++i) {
    if (d.items[i].code && strcmp(d.items[i].code, code) == 0) return &d.items[i];
  }
  return nullptr;
}

// 诊断文案默认英文（docs/I18N.md）。C++ 前端的字符串直接写进 out_diag JSON，
// 不经过 Go 侧 catalog / HERMES_LANG，所以这一层必须自己守：message / why /
// fix / note 里出现 CJK 就是回归。只判 U+4E00–U+9FFF，避免把排版用的
// "—"（U+2014）、"…"（U+2026）误判成中文。
static bool has_cjk(const char* s) {
  if (!s) return false;
  const unsigned char* p = (const unsigned char*)s;
  for (; *p; ++p) {
    if (p[0] < 0xE4 || p[0] > 0xE9) continue;
    if (p[1] == 0 || p[2] == 0) break;  // 截断的 UTF-8 序列
    if ((p[1] & 0xC0) != 0x80 || (p[2] & 0xC0) != 0x80) continue;
    unsigned cp = ((p[0] & 0x0F) << 12) | ((p[1] & 0x3F) << 6) | (p[2] & 0x3F);
    if (cp >= 0x4E00 && cp <= 0x9FFF) return true;
  }
  return false;
}

// 返回第一个含 CJK 的字段名；全英文返回 nullptr。
static const char* cjk_field(const Diag* d) {
  if (!d) return nullptr;
  if (has_cjk(d->message)) return "message";
  if (has_cjk(d->why)) return "why";
  if (has_cjk(d->fix)) return "fix";
  if (has_cjk(d->fix2)) return "fix2";
  if (has_cjk(d->fix3)) return "fix3";
  for (int i = 0; i < d->n_notes; ++i) {
    if (d->notes && has_cjk(d->notes[i].message)) return "note.message";
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
      // 文案已统一英文：不再逐字断言中文措辞，改守「英文 + 结构 token」。
      check(d->message && strstr(d->message, "(") != nullptr && !cjk_field(d),
            "E2001 message should be English and mention '(': %s",
            d->message ? d->message : "(null)");
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
    if (d) check(d->message && !cjk_field(d),
                 "E2002 diagnostic must be English: %s",
                 d->message ? d->message : "(null)");
    delete r.arena;
  }
  // 6b: input 无类型 → E3023（逐字：LANGUAGE §11.2 ⑪）
  {
    const char* src = "export workflow support_bot(input, backend = homonoia) entry -> string;";
    PResult r = parse_source(src, strlen(src), "t.h", true);
    const Diag* d = find_diag_code(r.diags, "E3023");
    check(d != nullptr, "input 无类型应报 E3023");
    // 原文是「逐字断言定稿句」。文案英文化后逐字断言会与译文耦合，
    // 改为守「诊断码 + 英文 + 点名 input」，措辞自由度留给译文。
    if (d) check(d->message && strstr(d->message, "input") != nullptr && !cjk_field(d),
                 "E3023 must be English and name 'input': %s",
                 d->message ? d->message : "(null)");
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
      check(!cjk_field(d), "A4 E2018 diagnostic must be English: %s", d->message);
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
      check(d->message && !cjk_field(d),
            "A5 duplicate-clause diagnostic must be English: %s", d->message);
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

// 按 check_type.cpp::scan_float_ 的同一套槽位递归找浮点 LitExpr。
// 只走 child / args / type_args / else_branch / body 的**链**，不沿 subs 复制
// 节点里的 next 走（那会跑回原链）—— 与 scan_float_ 的注释同因。
static void find_lit_expr(Node* n, Node** out) {
  if (!n || *out) return;
  if (n->kind == Kind::LitExpr && n->text &&
      (strcmp(n->text, "double") == 0 || strcmp(n->text, "float") == 0)) {
    *out = n;
    return;
  }
  for (Node* c = n->child; c && !*out; c = c->next) find_lit_expr(c, out);
  for (Node* c = n->args; c && !*out; c = c->next) find_lit_expr(c, out);
  for (Node* c = n->type_args; c && !*out; c = c->next) find_lit_expr(c, out);
  for (Node* c = n->else_branch; c && !*out; c = c->next) find_lit_expr(c, out);
  for (Node* c = n->body; c && !*out; c = c->next) find_lit_expr(c, out);
}

// E4029 自 v2.0.0 起是「旧 float 标签的纵深防线」，不再拒所有浮点字面量。
// 这条同时钉住三件事：
//   1. `double` 字面量正常编出**位模式**（1.5 → 3ff8000000000000），不是
//      {"t":"string","v":"double"} 的字符串兜底；
//   2. 正常路径不再报 E4029（所有 parse 入口共用这道扫描）；
//   3. 把 AST 里那个标签手工改回旧的 "float" 后，独立全遍历仍然报 1 条
//      E4029 —— 证明三道防线没有被删掉、判据也没有偷偷改成盯 "double"。
static void test_e4029_float_literal() {
  printf("[E4029] double 字面量编出位模式 + 旧 float 标签仍被拒\n");
  const char* src = "workflow w() entry -> double { return 1.5; }";
  char dump[1 << 16] = {0};
  char diag[1 << 16] = {0};
  size_t dl = 0, gl = 0;
  int nc = 0;
  int rc = hm_parse(reinterpret_cast<const uint8_t*>(src), strlen(src), "f.hermes", 0,
                    dump, sizeof(dump), &dl, diag, sizeof(diag), &gl, &nc);
  check(rc == 0, "hm_parse 本身应成功（诊断走 diag 通道，实际 rc=%d）", rc);
  check(strstr(diag, "E4029") == nullptr,
        "double 字面量不该再报 E4029，实际：%.160s", diag);

  // codegen 必须编出位模式，且不能把类型名当值写进 lit.v。
  {
    PResult r = parse_source(src, strlen(src), "f.hermes", false);
    Node* wf = r.root ? r.root->child : nullptr;
    check(wf != nullptr, "应解析出 workflow 节点");
    if (wf) {
      hm::JVal out;
      std::string err;
      const bool ok = hm::codegen_workflow(wf, &out, &err);
      check(ok, "double 字面量应能 codegen：%s", err.c_str());
      const std::string js = out.str();
      check(js.find("\"t\":\"double\"") != std::string::npos,
            "lit 的 t 应是 double：%.240s", js.c_str());
      check(js.find("\"bits\":\"3ff8000000000000\"") != std::string::npos,
            "1.5 的位模式应是 3ff8000000000000：%.240s", js.c_str());
      check(js.find("\"v\":\"double\"") == std::string::npos,
            "double 不该落进字符串兜底：%.240s", js.c_str());
    }
    delete r.arena;
  }

  // 纵深防线：手工把标签改回旧的 "float"，check_types 必须仍报 1 条 E4029。
  // parse_source 只做 lex+parse，不跑 check_types，所以这里可以直接调用。
  {
    PResult r = parse_source(src, strlen(src), "g.hermes", false);
    Node* lit = nullptr;
    find_lit_expr(r.root, &lit);
    check(lit != nullptr, "应找到浮点字面量 LitExpr");
    if (lit) {
      lit->text = "float";
      hm::check_types(r.root, "g.hermes", *r.arena, r.diags);
      check(count_diag_code(r.diags, "E4029") == 1,
            "旧 float 标签应恰好报 1 条 E4029（实际 %d）",
            count_diag_code(r.diags, "E4029"));
    }
    delete r.arena;
  }
}

// E4033：`double == double` / `double != double` 是**硬错**，不是警告。
//
// 背景：§4.4 的禁止列原先写"允许但 W4001"，而 W4001 从来没有实现过 ——
// 于是浮点相等一直静默通过。2026-11-10 拍板升为硬错；v2.0.0 放开 double
// 字面量后这条**仍然必须保留**。这条测试钉两件事：
//   1. 正例必须报，且 `==` 与 `!=` 都报（不能只拦一个）；
//   2. **反例必须不报**：`<` 关系比较、int 相等、json 相等都继续合法。
//      只测正例的守卫会放过"把所有比较都拦掉"这种过度修复。
static void test_e4033_float_equality() {
  printf("[E4033] double 的 == / != 硬拒（关系比较不受影响）\n");

  auto count_code = [](const DiagBag& bag, const char* code) {
    return count_diag_code(bag, code);
  };

  // ---- 正例 1：== ----
  {
    const char* src =
        "export struct S { double x; }\n"
        "export workflow w(input: S) -> bool { return input.x == input.x; }\n";
    PResult r = parse_source(src, strlen(src), "feq.hermes", false);
    hm::check_types(r.root, "feq.hermes", *r.arena, r.diags);
    check(count_code(r.diags, "E4033") == 1, "double == double 应报 1 条 E4033（实际 %d）",
          count_code(r.diags, "E4033"));
    delete r.arena;
  }
  // ---- 正例 2：!= （`x != x` 曾是唯一的 NaN 判据，同样要拦）----
  {
    const char* src =
        "export struct S { double x; }\n"
        "export workflow w(input: S) -> bool { return input.x != input.x; }\n";
    PResult r = parse_source(src, strlen(src), "fne.hermes", false);
    hm::check_types(r.root, "fne.hermes", *r.arena, r.diags);
    check(count_code(r.diags, "E4033") == 1, "double != double 应报 1 条 E4033（实际 %d）",
          count_code(r.diags, "E4033"));
    delete r.arena;
  }
  // ---- 反例 1：关系比较仍合法（容差比较正是官方给的改法）----
  {
    const char* src =
        "export struct S { double x; }\n"
        "export workflow w(input: S) -> bool { return input.x < input.x; }\n";
    PResult r = parse_source(src, strlen(src), "flt.hermes", false);
    hm::check_types(r.root, "flt.hermes", *r.arena, r.diags);
    check(count_code(r.diags, "E4033") == 0, "double < double 不该被 E4033 拦（容差比较就是靠它）");
    delete r.arena;
  }
  // ---- 反例 2：int / string / json 的相等完全不受影响 ----
  {
    const char* src =
        "export workflow w(input: json) -> bool {\n"
        "    auto a = 1;\n"
        "    auto s = \"k\";\n"
        "    return a == a && s == s && input == input;\n"
        "}\n";
    PResult r = parse_source(src, strlen(src), "other.hermes", false);
    hm::check_types(r.root, "other.hermes", *r.arena, r.diags);
    check(count_code(r.diags, "E4033") == 0, "int/string/json 的相等不该被 E4033 误伤");
    delete r.arena;
  }
}

// W5004：`if (order.status == 404)` 的**启发式**警告（§7.1、§12.1 L3）。
//
// 这条警告的正确性有一半在"**不**报"上：它是启发式，误报会把整条警告变成
// 噪声，而噪声级的警告最后都会被人关掉 —— 等于没实现。所以正例反例同等重要：
//   正例：==、!=、以及把字面量写在左边（`404 == o.status`）；
//   反例：2xx（`status == 200` 是正常业务值）、非状态码字段名、
//         以及 allow_status 拿到的 `HttpResult<T>.status`（那正是它存在的意义）。
//
// 顺带钉住去重：type_of_ 会对同一个比较反复调用（if 条件一处、E4027 一处），
// 没有 warned_status_cmp_ 的话同一条会报两三次。count == 1 就是在守这个。
static void test_w5004_http_status() {
  printf("[W5004] 拿业务字段比 HTTP 状态码（启发式警告）\n");

  struct Case {
    const char* name;
    const char* src;
    int want;  // 期望的 W5004 条数
  };
  const char* order_decl = "export struct Order { string id; int status; int count; }\n";

  auto run_case = [&](const char* name, const char* src, int want) {
    PResult r = parse_source(src, strlen(src), "w5004.hermes", false);
    hm::check_types(r.root, "w5004.hermes", *r.arena, r.diags);
    const int got = count_diag_code(r.diags, "W5004");
    check(got == want, "%s：期望 %d 条 W5004，实际 %d 条", name, want, got);
    delete r.arena;
  };

  std::string head = order_decl;
  run_case("== 404",
           (head + "export workflow w(input: Order) -> string { if (input.status == 404) { "
                   "return \"a\"; } return \"b\"; }\n")
               .c_str(),
           1);
  run_case("!= 404",
           (head + "export workflow w(input: Order) -> string { if (input.status != 404) { "
                   "return \"a\"; } return \"b\"; }\n")
               .c_str(),
           1);
  run_case("字面量写在左边",
           (head + "export workflow w(input: Order) -> string { if (404 == input.status) { "
                   "return \"a\"; } return \"b\"; }\n")
               .c_str(),
           1);
  run_case("2xx 是正常业务值，不报",
           (head + "export workflow w(input: Order) -> string { if (input.status == 200) { "
                   "return \"a\"; } return \"b\"; }\n")
               .c_str(),
           0);
  run_case("字段名不是状态码，不报",
           (head + "export workflow w(input: Order) -> string { if (input.count == 404) { "
                   "return \"a\"; } return \"b\"; }\n")
               .c_str(),
           0);
  run_case("两个比较各报一条（不是去重过头的 1 条）",
           (head + "export workflow w(input: Order) -> string { if (input.status == 404) { "
                   "return \"a\"; } if (input.status == 500) { return \"c\"; } return \"b\"; "
                   "}\n")
               .c_str(),
           2);

  // allow_status 拿到的 HttpResult<T>.status 是**正确用法**，必须放行。
  // 这条比前几条都重要：allow_status 存在的理由就是让调用方能写这个比较。
  run_case("allow_status 的 r.status 不报",
           "#link <std/net>\n"
           "export struct Order { string id; int status; }\n"
           "export workflow w(input: Order) -> string {\n"
           "  auto r = tool.call<Order>(url = \"http://127.0.0.1:9/o/{input.id}\", "
           "method = \"GET\", allow_status = [404]);\n"
           "  if (r.status == 404) { return \"a\"; }\n"
           "  return \"b\";\n"
           "}\n",
           0);
}

// W5005：`input: json` 声明无结构契约（§4.8）。
//
// 编译期唯一触发条件就是 input 的类型解析为 json —— 不去碰运行期的
// 未知字段 / 类型不符校验（那是 serve 的运行期契约职责，不属于 W5005）。
// 正例反例同样要守：
//   正例：input 声明为 json，必报 1 条 W5005，且 severity 为 Warning（不阻塞）；
//   反例：input 声明为某已声明的 struct，不该报 W5005（丢失编译期契约保护的
//         只是 json 这个逃逸口）。
static void test_w5005_input_json() {
  printf("[W5005] input: json 入口无结构契约（警告，不阻塞）\n");

  // 正例：input 声明为 json。
  {
    const char* src =
        "export workflow w(input: json) -> bool {\n"
        "  return true;\n"
        "}\n";
    PResult r = parse_source(src, strlen(src), "w5005.hermes", false);
    hm::check_types(r.root, "w5005.hermes", *r.arena, r.diags);
    const int got = count_diag_code(r.diags, "W5005");
    check(got == 1, "input: json 应恰好报 1 条 W5005，实际 %d 条", got);
    const Diag* d = find_diag_code(r.diags, "W5005");
    check(d != nullptr, "应能找到 W5005 诊断");
    if (d) {
      check(d->severity == Severity::Warning,
            "W5005 必须是 Warning（不阻塞构建），实际 severity=%d",
            static_cast<int>(d->severity));
    }
    delete r.arena;
  }

  // 反例：input 声明为已声明的 struct，不应报 W5005。
  {
    const char* src =
        "export struct SupportInput { string q; int n; }\n"
        "export workflow w(input: SupportInput) -> bool {\n"
        "  return input.n > 0;\n"
        "}\n";
    PResult r = parse_source(src, strlen(src), "w5005b.hermes", false);
    hm::check_types(r.root, "w5005b.hermes", *r.arena, r.diags);
    const int got = count_diag_code(r.diags, "W5005");
    check(got == 0, "input: SupportInput 不应报 W5005，实际 %d 条", got);
    delete r.arena;
  }
}

// [E4016/E4034] 错误对象的字段（LANGUAGE §7.1.1）。
//
// 这条测试守的是**一个从未实现过的规范承诺**：D-014 点名要求
// `catch (HttpError e) { if (e.status == 404) }` 写得出来，而此前它报
// `E4016: cannot access field status on unknown` —— 文档（§7.1）与样例
// （examples/support_bot/README.md）都在推荐一个编不过的写法。
//
// 三层判据，缺一层都会退化成"看起来对"：
//   ① `e.status` 编得过（需求本身）；
//   ② 规范里有、运行期没带的字段报 E4034 —— **不是** E4016（后者是假话）；
//   ③ 字段集合**按类型分**：`ModelError.status` 不 ready、`NetworkError.status`
//      根本不存在。少了 ③，把 status 做成"所有错误都有"也能全绿。
static void test_err_fields() {
  printf("[E4016/E4034] 错误对象的字段访问（§7.1.1）\n");

  // 返回 E4034 与 E4016 各几条。用"条数"而不是"有没有"：多报与漏报同样是缺陷。
  auto run = [&](const char* ctype, const char* body, int* e4034, int* e4016) {
    std::string src = std::string("export workflow w(input: string) -> string {\n") +
                      "  try { auto a = 1; } catch (" + ctype + " e) { " + body + " }\n" +
                      "  return \"ok\";\n}\n";
    PResult r = parse_source(src.c_str(), src.size(), "errfield.hermes", false);
    hm::check_types(r.root, "errfield.hermes", *r.arena, r.diags);
    if (e4034) *e4034 = count_diag_code(r.diags, "E4034");
    if (e4016) *e4016 = count_diag_code(r.diags, "E4016");
    delete r.arena;
  };

  // ① D-014 的那一行：必须干净通过（一条诊断都没有）。
  {
    int a = -1, b = -1;
    run("HttpError", "if (e.status == 404) { return \"missing\"; }", &a, &b);
    check(a == 0 && b == 0, "HttpError.status 必须可读（实际 E4034=%d E4016=%d）", a, b);
  }
  // `auto s = e.status;` 也合法：int 是值类型（§7.1.1 约束 2）。
  {
    int a = -1, b = -1;
    run("HttpError", "auto s = e.status; if (s == 404) { return \"m\"; }", &a, &b);
    check(a == 0 && b == 0, "auto s = e.status 应合法（实际 E4034=%d E4016=%d）", a, b);
  }
  // ② 规范有、运行期没有 → E4034，且**不能**同时报 E4016。
  {
    int a = -1, b = -1;
    run("HttpError", "return e.url;", &a, &b);
    check(a == 1 && b == 0, "HttpError.url 应报 1 条 E4034（实际 E4034=%d E4016=%d）", a, b);
  }
  // ③ 拼错了 → E4016，不是 E4034（两条码不能互相吞掉）。
  {
    int a = -1, b = -1;
    run("HttpError", "return e.statuz;", &a, &b);
    check(a == 0 && b == 1, "拼错的字段应报 1 条 E4016（实际 E4034=%d E4016=%d）", a, b);
  }
  // ③ ModelError 也有 status，但运行期同样没带 → 仍是 E4034。
  // 这条钉住"ready 是按 (类型, 字段) 判的"，不是"status 这个字段名就放行"。
  {
    int a = -1, b = -1;
    run("ModelError", "return e.status;", &a, &b);
    check(a == 1 && b == 0, "ModelError.status 应报 1 条 E4034（实际 E4034=%d E4016=%d）", a, b);
  }
  // ③ NetworkError 根本没有 status 字段 → E4016（字段集合按类型封闭）。
  {
    int a = -1, b = -1;
    run("NetworkError", "return e.status;", &a, &b);
    check(a == 0 && b == 1, "NetworkError.status 应报 1 条 E4016（实际 E4034=%d E4016=%d）", a, b);
  }
  // 根类型：字段集合按子类型分，根上没有任何字段可读 → E4016。
  {
    int a = -1, b = -1;
    run("Error", "return e.status;", &a, &b);
    check(a == 0 && b == 1, "catch(Error e) 上的 e.status 应报 1 条 E4016（实际 E4034=%d E4016=%d）", a, b);
  }
  // 回归：E4023 不能被这次改动顺带废掉（错误对象仍然不可整只存走）。
  {
    std::string src =
        "export workflow w(input: string) -> string {\n"
        "  try { auto a = 1; } catch (HttpError e) { auto x = e; return \"m\"; }\n"
        "  return \"ok\";\n}\n";
    PResult r = parse_source(src.c_str(), src.size(), "errfield.hermes", false);
    hm::check_types(r.root, "errfield.hermes", *r.arena, r.diags);
    const int n = count_diag_code(r.diags, "E4023");
    check(n == 1, "auto x = e 仍应报 1 条 E4023（实际 %d 条）", n);
    delete r.arena;
  }
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
  //    seq=0 → decl=1 → lit=2 → return=3 → var=4
  check(js.find("\"id\":0,\"k\":\"seq\"") != std::string::npos, "节点 0 应是 seq");
  check(js.find("\"id\":1,\"k\":\"decl\"") != std::string::npos, "节点 1 应是 decl");
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

// agent_spec_of 从 workflow 的 canonical JSON 里切出 `"agent":{…}` 那一小段。
//
// 为什么要切：工作流级的 bounds 里也有一个 max_steps（= 语句节点数 + 1），
// 与 agent 规格里的 max_steps（循环上界）是两个完全不同的量。对整个 JSON 做
// 子串搜索会把两者混成一件，断言就失去意义。agent 规格对象里没有嵌套对象
// （goal 是整数 id、tools 是字符串数组、max_steps 是整数），所以第一个 '}'
// 就是它的结尾，不需要 JSON 解析器。
static std::string agent_spec_of(const std::string& js) {
  const std::size_t b = js.find("\"agent\":{");
  if (b == std::string::npos) return "";
  const std::size_t e = js.find('}', b);
  if (e == std::string::npos) return "";
  return js.substr(b, e - b + 1);
}

// ---------------------------- unlimited 上界 -------------------------------
//
// `unlimited` 是本语言唯一一个**只为上界存在**的保留字，它有三条互相约束的
// 契约，任何一条写反了都不会有别的检查发现：
//   ① `max_steps = unlimited` 编得出来，但必须带一条 warning（W2004）；
//   ② 产出的 agent 规格里**不许**出现 max_steps 键 —— 写 null 或写一个大数
//      都是在撒谎，而假的上界会让 Go 侧的复算/validate 拿到一个假前提；
//   ③ bounds.unbounded 必须是 true，且 span 只按**一次迭代**记（与 while
//      语句同一个先例），乘一个假想上限会撞上 `max_spans ≤ 64*max_steps+1`。
// 另外两条反向契约：`retry(unlimited)` 是硬错（E2015），值位置的 `unlimited`
// 也是硬错（E4024）。它们和 ① 的差别是**故意的**，见 parser.cpp 的注释。
static void test_unlimited_bound() {
  // ---- ① max_steps = unlimited：成功 + 一条 warning + 无 max_steps 键 ----
  {
    const char* src =
        "workflow main() -> string {\n"
        "    return model.agent(goal = \"hi\", max_steps = unlimited);\n"
        "}\n";
    PResult r = parse_source(src, strlen(src), "ul.hermes", false);
    check(r.root != nullptr, "unlimited 用例应解析出 AST");

    hm::JVal out;
    std::string err;
    hm::Loc eloc{};
    const bool ok = hm::codegen_workflow_in(r.root, r.root->child, &out, &err, &eloc,
                                            r.arena, &r.diags, "ul.hermes");
    check(ok, "max_steps = unlimited 应编得出来（err=%s）", err.c_str());
    if (ok) {
      const std::string js = out.str();
      const Diag* w = find_diag_code(r.diags, "W2004");
      check(w != nullptr, "max_steps = unlimited 应产出 W2004 警告");
      if (w) {
        check(w->severity == Severity::Warning, "W2004 的 severity 必须是 Warning（实际 %d）",
              static_cast<int>(w->severity));
        check(w->why != nullptr && w->fix != nullptr, "W2004 必须带 why 与 fix（可操作，不是复述）");
        check(w->loc.line == 2, "W2004 应指向 unlimited 所在的第 2 行（实际 %d）", w->loc.line);
      }
      // 警告不是错误：DiagBag 里不许出现任何 error。
      int errs = 0;
      for (int i = 0; i < r.diags.count; ++i) {
        if (r.diags.items[i].severity == Severity::Error) ++errs;
      }
      check(errs == 0, "unlimited 不许产生任何 error（实际 %d 条）", errs);
      // ② agent 规格里没有 max_steps 键：Go 侧把"缺失"读成"没有静态上界"。
      //
      // 断言必须**只看 agent 规格**：工作流级的 bounds 里也有一个 max_steps
      // （语句节点数的上界），对整个 JSON 搜 "max_steps" 会把它搜出来 ——
      // 那是两个完全不同的东西，混在一起检查等于什么都没检查。
      const std::string spec = agent_spec_of(js);
      check(!spec.empty(), "agent 规格应存在（%s）", js.c_str());
      check(spec.find("\"max_steps\"") == std::string::npos,
            "unlimited 的 agent 规格里不该有 max_steps 键（规格=%s）", spec.c_str());
      // ③ unbounded + 一次迭代的上界（k=1 时 span=2、model=1、tool=1）。
      check(js.find("\"unbounded\":true") != std::string::npos,
            "bounds 应带 unbounded:true");
      check(js.find("\"max_spans\":2") != std::string::npos,
            "unlimited 的 max_spans 应按一次迭代记（应出现 max_spans:2）");
      check(js.find("\"max_model_calls\":1") != std::string::npos,
            "unlimited 的 max_model_calls 应按一次迭代记（应出现 1）");
    }
    delete r.arena;
  }

  // ---- ② max_steps = 4：不回归（无警告、键在、无 unbounded） ----
  {
    const char* src =
        "workflow main() -> string {\n"
        "    return model.agent(goal = \"hi\", max_steps = 4);\n"
        "}\n";
    PResult r = parse_source(src, strlen(src), "b4.hermes", false);
    hm::JVal out;
    std::string err;
    hm::Loc eloc{};
    const bool ok = hm::codegen_workflow_in(r.root, r.root->child, &out, &err, &eloc,
                                            r.arena, &r.diags, "b4.hermes");
    check(ok, "max_steps = 4 应编得出来（err=%s）", err.c_str());
    if (ok) {
      const std::string js = out.str();
      check(find_diag_code(r.diags, "W2004") == nullptr, "常量的 max_steps 不该有 W2004");
      check(agent_spec_of(js).find("\"max_steps\":4") != std::string::npos,
            "agent 规格里应有 max_steps:4（规格=%s）", agent_spec_of(js).c_str());
      check(js.find("\"unbounded\"") == std::string::npos, "有界 agent 不该标 unbounded");
      check(js.find("\"max_spans\":8") != std::string::npos, "max_spans 应是 (1+1)*4=8");
    }
    delete r.arena;
  }

  // ---- ③ retry(unlimited)：硬错 E2015（不是警告） ----
  {
    const char* src =
        "workflow main() -> string {\n"
        "    retry(unlimited) { return \"x\"; }\n"
        "}\n";
    PResult r = parse_source(src, strlen(src), "ru.hermes", false);
    const Diag* d = find_diag_code(r.diags, "E2015");
    check(d != nullptr, "retry(unlimited) 应报 E2015");
    if (d) {
      check(d->severity == Severity::Error, "retry(unlimited) 必须是 error，不是 warning");
      check(d->message && !cjk_field(d),
            "E2015 diagnostic must be English (actual: %s)",
            d->message ? d->message : "");
    }
    delete r.arena;
  }

  // ---- ④ 值位置的 unlimited：类型检查硬错 E4024，且不崩 ----
  {
    const char* src =
        "workflow main() -> string {\n"
        "    auto x = unlimited;\n"
        "    return \"x\";\n"
        "}\n";
    PResult r = parse_source(src, strlen(src), "au.hermes", false);
    check(r.root != nullptr, "auto x = unlimited 应能解析到 AST（错误在语义层，不在语法层）");
    if (r.root) hm::check_types(r.root, "au.hermes", *r.arena, r.diags);
    const Diag* d = find_diag_code(r.diags, "E4024");
    check(d != nullptr, "值位置的 unlimited 应报 E4024");
    if (d) {
      check(d->severity == Severity::Error, "E4024 必须是 error");
      check(d->fix && strstr(d->fix, "max_steps = unlimited") != nullptr,
            "E4024 的 fix 应指向唯一合法的位置");
    }
    delete r.arena;
  }
}

// 找到模块里的第一个 WorkflowDecl（extern / #link 可能排在 workflow 之前）。
static bool is_workflow_decl(const Node* n) { return n && n->kind == Kind::WorkflowDecl; }

// ---------------------------- extern / call_native -----------------------------
//
// 反向融合（extern / #link native / call_native）的编译器前端里程碑：
//   · extern 声明（两种形态）必须解析成 is_extern 的声明节点（无体）；
//   · 对 extern 符号的调用必须编出 call_native 节点（fn / args / ret）；
//   · 含 call_native 的 workflow 必须标 unbounded；
//   · 类型闭集（E3030）/ 缺 #link native（E3031）/ 非纯 C 头（E3032）必须拦截。
static void test_extern_call_native() {
  // ---- ① 形态 B（C++ 风格）：extern double sqrt(double x); + 调用 → call_native ----
  {
    const char* src =
        "#link native \"math\" from \"math_tool.h\";\n"
        "extern double sqrt(double x);\n"
        "export workflow hypot(a: double, b: double) entry -> double {\n"
        "    return sqrt(a * a + b * b);\n"
        "}\n";
    PResult r = parse_source(src, strlen(src), "ext_b.hermes", false);
    check(r.root != nullptr, "extern(C++) 用例应解析出 AST");

    hm::check_types(r.root, "ext_b.hermes", *r.arena, r.diags);
    int errs = 0;
    for (int i = 0; i < r.diags.count; ++i)
      if (r.diags.items[i].severity == Severity::Error) ++errs;
    check(errs == 0, "extern(double) 类型检查应通过（实际 %d 条错误）", errs);
    if (errs == 0) {
      Node* wf = find_first(r.root, is_workflow_decl);
      check(wf != nullptr, "应解析出 workflow");
      hm::JVal out; std::string err; hm::Loc eloc{};
      const bool ok = hm::codegen_workflow_in(r.root, wf, &out, &err, &eloc,
                                              r.arena, &r.diags, "ext_b.hermes");
      check(ok, "extern 调用应编得出来（err=%s）", err.c_str());
      if (ok) {
        const std::string js = out.str();
        check(js.find("\"k\":\"call_native\"") != std::string::npos,
              "IR 应含 call_native 节点（%s）", js.c_str());
        check(js.find("\"fn\":\"sqrt\"") != std::string::npos,
              "call_native.fn 应为 sqrt（%s）", js.c_str());
        check(js.find("\"ret\":{\"k\":\"double\"}") != std::string::npos,
              "call_native.ret 应为 {k:double}（%s）", js.c_str());
        check(js.find("\"unbounded\":true") != std::string::npos,
              "含 call_native 的 workflow 应标 unbounded:true（%s）", js.c_str());
      }
    }
    delete r.arena;
  }

  // ---- ② 形态 A（workflow 风格）：extern workflow net_get(url: string) -> NetResp; ----
  {
    const char* src =
        "export struct NetResp { int status; string body; }\n"
        "#link native \"hermes_net\" from \"hermes/net.h\";\n"
        "extern workflow net_get(url: string) -> NetResp;\n"
        "export workflow fetch() entry -> NetResp {\n"
        "    return net_get(\"https://x\");\n"
        "}\n";
    PResult r = parse_source(src, strlen(src), "ext_a.hermes", false);
    check(r.root != nullptr, "extern(workflow) 用例应解析出 AST");
    hm::check_types(r.root, "ext_a.hermes", *r.arena, r.diags);
    int errs = 0;
    for (int i = 0; i < r.diags.count; ++i)
      if (r.diags.items[i].severity == Severity::Error) ++errs;
    check(errs == 0, "extern(workflow) 类型检查应通过（实际 %d 条错误）", errs);
    if (errs == 0) {
      Node* wf = find_first(r.root, is_workflow_decl);
      hm::JVal out; std::string err; hm::Loc eloc{};
      const bool ok = hm::codegen_workflow_in(r.root, wf, &out, &err, &eloc,
                                              r.arena, &r.diags, "ext_a.hermes");
      check(ok, "extern workflow 调用应编得出来（err=%s）", err.c_str());
      if (ok) {
        const std::string js = out.str();
        check(js.find("\"fn\":\"net_get\"") != std::string::npos,
              "call_native.fn 应为 net_get（%s）", js.c_str());
        // @opaque struct 返回：ret 为 struct 引用（按指针传）。
        check(js.find("\"ret\":{\"k\":\"struct\",\"ref\":\"NetResp\"}") != std::string::npos,
              "call_native.ret 应为 @opaque struct NetResp（%s）", js.c_str());
        check(js.find("\"unbounded\":true") != std::string::npos,
              "含 call_native 的 workflow 应标 unbounded:true（%s）", js.c_str());
      }
    }
    delete r.arena;
  }

  // ---- ③ E3031：extern 没有匹配的 #link native ----
  {
    const char* src =
        "extern double sqrt(double x);\n"
        "export workflow hypot(a: double) entry -> double { return sqrt(a); }\n";
    PResult r = parse_source(src, strlen(src), "ext_e1.hermes", false);
    check(r.root != nullptr, "E3031 用例应解析出 AST");
    hm::check_types(r.root, "ext_e1.hermes", *r.arena, r.diags);
    check(has_diag_code(r.diags, "E3031"), "缺 #link native 应报 E3031");
    delete r.arena;
  }

  // ---- ④ E3032：#link native 的 from 头不是纯 C 头（.hpp）----
  {
    const char* src =
        "#link native \"math\" from \"math_tool.hpp\";\n"
        "extern double sqrt(double x);\n"
        "export workflow hypot(a: double) entry -> double { return sqrt(a); }\n";
    PResult r = parse_source(src, strlen(src), "ext_e2.hermes", false);
    check(r.root != nullptr, "E3032 用例应解析出 AST");
    hm::check_types(r.root, "ext_e2.hermes", *r.arena, r.diags);
    check(has_diag_code(r.diags, "E3032"), "非纯 C 头应报 E3032");
    delete r.arena;
  }

  // ---- ⑤ export extern：可见性前缀与 extern 正交（§1.3 正例 1）----
  // 旧实现把 extern 分派放在 `export` 被消费之前，且用 stale 的 k 判定，
  // `export extern` 会落进 parse_toplevel 的 default 报 E1001。
  {
    const char* src =
        "#link native \"math\" from \"math_tool.h\"\n"
        "export extern int imul(int a, int b);\n"
        "export workflow probe() entry -> int { return imul(6, 7); }\n";
    PResult r = parse_source(src, strlen(src), "ext_export.hermes", false);
    check(r.root != nullptr, "export extern 应使用例解析出 AST");
    hm::check_types(r.root, "ext_export.hermes", *r.arena, r.diags);
    int errs = 0;
    for (int i = 0; i < r.diags.count; ++i)
      if (r.diags.items[i].severity == Severity::Error) ++errs;
    check(errs == 0, "export extern 不应报错（实际 %d 条错误）", errs);
    if (errs == 0) {
      Node* wf = find_first(r.root, is_workflow_decl);
      hm::JVal out; std::string err; hm::Loc eloc{};
      const bool ok = hm::codegen_workflow_in(r.root, wf, &out, &err, &eloc,
                                              r.arena, &r.diags, "ext_export.hermes");
      check(ok, "export extern 调用应编得出来（err=%s）", err.c_str());
      if (ok) check(out.str().find("\"fn\":\"imul\"") != std::string::npos,
                    "export extern 应仍编出 call_native（%s）", out.str().c_str());
    }
    delete r.arena;
  }

  // ---- ⑥ 未被调用的非法 extern：E3030 / E3032 仍必须报（声明级）----
  // 之前这两条挂在 CallExpr 的 type_of_ 分支，extern 不被调用就整条静默编过。
  // 这里**故意不调用** extern，确保检查确实在声明/指令级而不是调用点。
  {
    const char* src =
        "#link native \"math\" from \"math_tool.hpp\"\n"
        "extern workflow bad() -> json;\n"
        "export workflow probe() entry -> int { return 1; }\n";
    PResult r = parse_source(src, strlen(src), "ext_uncalled.hermes", false);
    check(r.root != nullptr, "未调用 extern 用例应解析出 AST");
    hm::check_types(r.root, "ext_uncalled.hermes", *r.arena, r.diags);
    check(has_diag_code(r.diags, "E3030"), "未被调用的 -> json extern 仍应报 E3030");
    check(has_diag_code(r.diags, "E3032"), "没有 extern 调用时 .hpp 指令仍应报 E3032");
    delete r.arena;
  }

  // ---- ⑦ E3030 不重复：被调用的非法 extern 只报一条（声明级，不是调用点）----
  {
    const char* src =
        "#link native \"math\" from \"math_tool.h\"\n"
        "extern int parse(json j);\n"
        "export workflow probe() entry -> int { return parse(1); }\n";
    PResult r = parse_source(src, strlen(src), "ext_dup.hermes", false);
    check(r.root != nullptr, "E3030 去重用例应解析出 AST");
    hm::check_types(r.root, "ext_dup.hermes", *r.arena, r.diags);
    int n3030 = 0;
    for (int i = 0; i < r.diags.count; ++i)
      if (r.diags.items[i].code && strcmp(r.diags.items[i].code, "E3030") == 0) ++n3030;
    check(n3030 == 1, "E3030 应恰好一条（声明级），实际 %d 条", n3030);
    delete r.arena;
  }
}

// ===========================================================================
// M1/M2：did-you-mean（语法纠正）+ 机器可应用编辑（修正提示）
// ===========================================================================
static void test_did_you_mean() {
  printf("[did-you-mean] 字段名 / 关键字 拼写纠正 + 结构化编辑\n");

  // ---- (a) 字段名拼错：r.statuss 应为 r.status ----
  {
    const char* src =
        "export struct OrderInfo { int order_id; string status; double amount; }\n"
        "export workflow w(input: OrderInfo) -> string {\n"
        "    auto x = input.statuss;\n"
        "    return \"x\";\n"
        "}\n";
    PResult r = parse_source(src, strlen(src), "dym.hermes", false);
    hm::check_types(r.root, "dym.hermes", *r.arena, r.diags);
    const Diag* d = find_diag_code(r.diags, "E4016");
    check(d != nullptr, "字段名拼错应报 E4016");
    if (d) {
      check(d->fix2 != nullptr, "E4016 应带 fix2（did-you-mean）");
      if (d->fix2) {
        check(strstr(d->fix2, "did you mean") != nullptr,
              "E4016 fix2 应含 'did you mean'：%s", d->fix2);
        check(strstr(d->fix2, "status") != nullptr,
              "E4016 fix2 应建议 'status'：%s", d->fix2);
        // M1 的同时保留原有完整候选列表（fix 仍是 "OrderInfo has: ..."）
        check(d->fix != nullptr && strstr(d->fix, "status") != nullptr,
              "E4016 fix 仍应含候选列表（不丢原 fix）");
      }
      // M2：结构化编辑 —— span=拼错标识符位置，replacement=建议名
      check(d->edit_replacement != nullptr, "E4016 应带机器可应用编辑");
      if (d->edit_replacement) {
        check(strcmp(d->edit_replacement, "status") == 0,
              "E4016 edit.replacement 应为 'status'（实际 %s）", d->edit_replacement);
        check(d->edit_line > 0 && d->edit_col > 0, "E4016 edit span 应指向 statuss");
      }
    }
    // diag_to_json 应把 edit 序列化进 edits[]
    char js[1 << 16];
    size_t jl = 0;
    diag_to_json(r.diags, js, sizeof(js), &jl);
    check(strstr(js, "\"edits\":[{\"span\":") != nullptr,
          "diag_to_json 应输出 edits[] 的 span");
    check(strstr(js, "\"replacement\":\"status\"") != nullptr,
          "diag_to_json 的 edits[] 应带 replacement=status");
    delete r.arena;
  }

  // ---- (d) 顶层关键字拼错：wihle → while ----
  {
    const char* src = "wihle x = 1;\n";
    PResult r = parse_source(src, strlen(src), "kw.hermes", false);
    const Diag* d = find_diag_code(r.diags, "E1001");
    check(d != nullptr, "顶层关键字拼错应报 E1001");
    if (d) {
      check(d->fix2 != nullptr, "E1001 应带 fix2（关键字 did-you-mean）");
      if (d->fix2) {
        check(strstr(d->fix2, "did you mean keyword `while`") != nullptr,
              "E1001 fix2 应建议 'while'：%s", d->fix2);
      }
    }
    delete r.arena;
  }

  // ---- 负向：正确字段不应产生 did-you-mean ----
  {
    const char* src =
        "export struct OrderInfo { int order_id; string status; double amount; }\n"
        "export workflow w(input: OrderInfo) -> string {\n"
        "    auto x = input.status;\n"
        "    return \"x\";\n"
        "}\n";
    PResult r = parse_source(src, strlen(src), "ok.hermes", false);
    hm::check_types(r.root, "ok.hermes", *r.arena, r.diags);
    const Diag* d = find_diag_code(r.diags, "E4016");
    check(d == nullptr, "正确字段名不应报 E4016");
    delete r.arena;
  }
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
  test_e4029_float_literal();
  test_e4033_float_equality();
  test_w5004_http_status();
  test_w5005_input_json();
  test_err_fields();
  test_structured_assertions(root);
  test_auth_clause();
  test_string_literal_text(root);
  test_codegen();
  test_unlimited_bound();
  test_extern_call_native();
  test_did_you_mean();

  printf("\n=== 结果: %d/%d 通过, %d 失败 ===\n", T.pass, T.total, T.fail);
  return T.fail == 0 ? 0 : 1;
}
