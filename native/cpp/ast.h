// ast.h —— Hermes 编译器前端的 AST 节点、词法 Token、诊断结构。
//
// 设计约束（ARCH §5.9）：无异常、无 RTTI、无 IO、无第三方库。
// AST 是 tagged struct：每个 Node 带 Kind + Loc + 一组按需使用的字段/指针。
// 这套结构是 C++ 前端的**内部**结构，不跨语言暴露（跨语言只传 IR 字节 + 诊断字节）。

#pragma once

#include <cstdint>
#include <cstddef>

namespace hm {

// ----------------------------------------------------------------------------
// 位置：精确到 end_col（T35 的 golden 诊断逐字比对依赖它）。
// ----------------------------------------------------------------------------
struct Loc {
  int line = 1;        // 1-based
  int col = 1;         // 1-based，指向 token 首字符
  int end_line = 1;
  int end_col = 1;     // 指向 token 末字符之后的位置
};

// ----------------------------------------------------------------------------
// 词法 Token 种类（LANGUAGE.md §2.8 实现契约）。
// ----------------------------------------------------------------------------
enum class Tok {
  // 字面量与标识
  EOF_, IDENT, INT, FLOAT, COST_LIT, STRING,
  INCLUDE, LINK,              // 行指令：spec 在 text，mode 在 sub(0=angle,1=quote)
  // 关键字（16）
  KW_STRUCT, KW_BACKEND, KW_WORKFLOW, KW_IF, KW_ELSE, KW_MATCH, KW_WHILE, KW_SECURITY, KW_ALLOW, KW_DENY, KW_WITH, KW_FOR, KW_IN, KW_BREAK, KW_CONTINUE, KW_CONST, KW_AUTO,
  KW_RETRY, KW_DELAY, KW_TRY, KW_CATCH, KW_RETURN, KW_PARALLEL, KW_EXPORT, KW_EXTERN,
  // fallback：错误边界，**不是异常**。
  //
  // 语义是「主表达式返回非零 C ABI 状态码时，改跑这个块并取它的返回值」。
  // 全程没有栈展开：前端本来就 -fno-exceptions 编译，失败是返回值不是 throw。
  // 做成保留字而不是上下文关键字，理由同 unlimited —— `auto fallback = 1;`
  // 会让同一个词在一个文件里代表两件事，而遮蔽只在运行期以怪值形态出现。
  KW_FALLBACK,
  // unlimited：**上界位置专用**的保留字（`model.agent(max_steps = unlimited)`）。
  //
  // 做成保留字而不是上下文关键字，是有意的：它的含义必须不可被遮蔽。
  // 若允许 `auto unlimited = 3;`，同一个词就会在一个文件里代表两件事，
  // 而第二件（变量）会静默盖住第一件（"没有编译期上界"）——这类遮蔽 bug
  // 只在运行期以"上界算出个 3"的形态出现，追不到源头。
  KW_UNLIMITED,
  // 字面量关键字
  KW_TRUE, KW_FALSE,
  // 类型名（6）
  TY_STRING, TY_INT, TY_FLOAT, TY_BOOL, TY_JSON, TY_COST,
  // 保留命名空间（4）：model / tool / json / input
  NS_MODEL, NS_TOOL, NS_JSON, NS_INPUT,
  // memory / vector：存储子系统的保留命名空间（D-035）。
  NS_MEMORY, NS_VECTOR,
  // 错误类型名（6）：仅 catch 位置
  ERR_NETWORKERROR, ERR_HTTPERROR, ERR_MODELERROR, ERR_CONTRACTERROR,
  ERR_CIRCUITOPENERROR, ERR_ERROR,
  // 上下文关键字（2）：entry / env —— 词法器不特判，按 IDENT 出，由语法器判定。
  // 通配：UNDERSCORE（仅 auto 解构丢弃位）。
  UNDERSCORE,
  // 标点
  LBRACE, RBRACE, LPAREN, RPAREN, LBRACKET, RBRACKET,
  SEMI, COMMA, DOT, COLON, AT,
  // 运算符
  LT, LE, GT, GE, EQEQ, NE, ANDAND, OROR, BANG, PLUS, MINUS, STAR, SLASH,
  INC, DEC, PLUS_EQ, MINUS_EQ,   // ++ -- += -=（只在 for 头合法，由 parser 约束）
  PERCENT, ARROW,
  QUESTION, EQ,                 // '?' 与 '='（字段可空 / auto 绑定）
  KW_CASE, KW_DEFAULT,          // match 臂关键字（16 关键字之一）
  // 词法错误占位（不应进入语法器；出现即表示前面已报过 diag）。
  ERROR,
};

// 字符串插值的一个片段：字面文本，或一条路径（ident(.ident)*）。
struct StringPart {
  bool is_path;        // true=插值路径，false=字面文本
  const char* text;   // 字面文本原文；或路径原文（含 '.'）
};

// 词法器产出的 token。字符串的插值片段存在 parts/ n_parts（arena 分配）。
struct Token {
  Tok kind;
  Loc loc;
  const char* text;     // IDENT/关键字文本、类型名、INCLUDE/LINK spec、字面量原文等
  int64_t i64;          // INT
  double f64;           // FLOAT
  int sub;              // INCLUDE/LINK 的 mode：0=angle 1=quote
  const StringPart* parts;  // STRING 的插值片段（指向 arena 数组）
  int n_parts;
};

// ----------------------------------------------------------------------------
// AST 节点
// ----------------------------------------------------------------------------
enum class Kind {
  Module,
  Include, Link,                 // 行指令
  StructDecl,
  FunctionDecl,
  SecurityDecl,
  SecurityRule, Field,             // struct + 字段
  Type,                          // 类型（含数组维数）
  BackendSig, BackendDef, EnvExpr,
  // ToolDecl：`export tool orders { description = "…"; url = "…"; method = "GET"; }`（D-034）。
  //   text = 工具名；child 链 = 字段（Param：text=字段名，child=LitExpr）。
  ToolDecl,
  //   BackendDef: child=地址（LitExpr 或 EnvExpr）, args=auth 子句的 EnvExpr（可空：
  //   无 auth 子句时为 nullptr；auth 的 EnvExpr 里 child=变量名、args 恒为 nullptr）。
  //   EnvExpr:   child=变量名 LitExpr, args=默认值 LitExpr（auth 位没有默认值）。
  WorkflowSig, WorkflowDecl, Param,
  Block, LetStmt, Pattern,
  MatchStmt, MatchArm,
  IfStmt, WhileStmt, ForStmt, BreakStmt, ContinueStmt, AssignStmt, UpdateStmt, RetryStmt, TryStmt, CatchClause, ReturnStmt, ExprStmt,
  CallExpr, PathExpr, LitExpr, IdentExpr, BinaryExpr, UnaryExpr, IndexExpr,
  // UnlimitedExpr：源码里的 `unlimited`（唯一合法位置：model.agent(max_steps = unlimited)）。
  //
  // 为什么是一个**独立的 Kind** 而不是一个 text 为魔法值的 LitExpr：
  // 假字面量会一路通过类型检查（它就是 int），于是任何接受 int 的位置都会
  // 静默接受它 —— 包括 `auto x = unlimited;` 这种没有意义却编得出来的写法。
  // 独立 Kind 让所有未处理的位置在 gen_expr 的兜底分支上**当场失败**，
  // 也让类型检查能明确报"这里不是上界位置"。
  UnlimitedExpr,
  ParallelExpr, ParallelReturn,
  // FallbackExpr：`main_expr fallback { ... }`。
  // 槽位约定：**child = main_expr**，**args = fallback_block**（Kind::Block）。
  // 与 IfStmt 的 args 不同 —— 这里 args 只有一个块，不是语句链。
  FallbackExpr,
  // ArrayLit：数组字面量 `[a, b, c]`。**只用于参数位**（如 tool.call 的
  // allow_status = [404, 409]）—— 语言没有数组值语义，元素必须是常量。
  ArrayLit,
  StringPartNode,                // 字符串插值片段（AST 内）
  Annotation,
};

struct Node {
  Kind kind;
  Loc loc;

  Node* next = nullptr;          // 兄弟（声明/参数/字段/语句列表）
  Node* child = nullptr;         // 长子（因 kind 而异）
  Node* else_branch = nullptr;   // if 的 else / elseif
  Node* body = nullptr;          // WhileStmt / ForStmt 的循环体（parse_while / parse_for）
  // args 因 kind 而异：CallExpr 首参、MatchStmt 的臂链（MatchArm 的体在
  // **child**，不在 args）、IfStmt 的 then 体、Let 的 Init 等。逐个核对
  // parser.cpp，别照这句注释推槽位 —— check_type.cpp::stmt_() 曾因读错
  // MatchArm / RetryStmt / While / For 的体槽位而整类跳过类型检查。
  Node* args = nullptr;
  Node* type_args = nullptr;     // CallExpr 的首类型实参
  Node* subs = nullptr;          // 定长多子节点数组（arena 分配）
  int n_subs = 0;

  const char* text = nullptr;    // 主字符串（名字/字面量/类型/spec/标注名）
  int64_t i64 = 0;
  double f64 = 0.0;
  bool is_export = false;
  bool is_entry = false;
  bool is_extern = false;     // extern 声明（C ABI 符号）：无体，走 call_native
  bool is_nullable = false;
  bool is_path = false;         // StringPartNode：true=路径
  bool is_mut = false;          // (deprecated) 旧 auto 标记
  bool is_const = false;        // const 声明：不可再赋值
  bool is_auto = false;
  const char* policy = nullptr;   // workflow 的 `with <Policy>`         // auto 声明：类型由 initializer 推导
  // 二元/一元运算的 opcode 复用 i64 字段（见 ast_dump / parser 约定）。
};

// ----------------------------------------------------------------------------
// 诊断（错误/警告/note）。结构见 LANGUAGE.md §11.1。
// ----------------------------------------------------------------------------
enum class Severity { Error, Warning, Note };

struct DiagNote {
  const char* module;   // 文件名
  int line;
  int col;
  const char* message;
};

struct Diag {
  Severity severity;
  const char* code;       // "E2001" 等
  const char* message;    // 一句可独立读懂的话
  Loc loc;
  const char* module;     // 文件名
  const char* why;        // "为什么"
  const char* fix;        // "怎么改"（单条；多条用 fix2/fix3）
  const char* fix2;
  const char* fix3;
  // M2：机器可应用的修正编辑（单条主编辑；多编辑超出范围）。
  // 仅当 edit_replacement != nullptr 时由 diag_to_json 输出为 edits[]。
  // 所有 Diag 构造点（err / emit_err / diag_error）都先 memset 整块为 0，
  // 所以这里不写默认初值也保证是 0/null。
  int edit_line, edit_col, edit_end_line, edit_end_col;
  const char* edit_replacement;
  const DiagNote* notes;  // 跨文件指回定义处等
  int n_notes;
};

// ---- M1：编辑距离与「最近候选」建议（零第三方依赖，纯手写的 Levenshtein）----
//
// edit_distance 计算两个 C 串的标准 Levenshtein 距离，但只在长度相近且较短时
// 真正计算（|na-nb|<=2 且 max(na,nb)<=32），否则直接返回大数——把复杂度钉死在
// O(小) 上，避免对长标识符做无意义的 O(n*m)，也不引入任何 STL 容器。
inline int edit_distance(const char* a, size_t na, const char* b, size_t nb) {
  if (na > 32 || nb > 32) return 100;
  long delta = static_cast<long>(na) - static_cast<long>(nb);
  if (delta < 0) delta = -delta;
  if (delta > 2) return 100;
  // 两行滚动 DP，定长 33（长度上限 32 + 1）。不分配、不依赖 STL。
  int prev[33], cur[33];
  for (size_t j = 0; j <= nb; ++j) prev[j] = static_cast<int>(j);
  for (size_t i = 1; i <= na; ++i) {
    cur[0] = static_cast<int>(i);
    const char ac = a[i - 1];
    for (size_t j = 1; j <= nb; ++j) {
      int cost = (ac == b[j - 1]) ? 0 : 1;
      int v = prev[j - 1] + cost;
      int del = prev[j] + 1;
      int ins = cur[j - 1] + 1;
      if (del < v) v = del;
      if (ins < v) v = ins;
      cur[j] = v;
    }
    for (size_t j = 0; j <= nb; ++j) prev[j] = cur[j];
  }
  return prev[nb];
}

// suggest_token 在「期望 token 表」（关键字 / 类型名 / 错误类型名，见 lexer.cpp
// 的 kExpectedTokens）里返回 Edit-Distance ≤ max_dist 的最近候选；找不到返回 nullptr。
// 定义见 lexer.cpp（它需要引用 kExpectedTokens 静态表，故不能放进本头文件）。
const char* suggest_token(const char* s, size_t n, int max_dist);

struct DiagBag {
  Diag* items = nullptr;
  int count = 0;
  int cap = 0;
};

// 跨 TU 的公共函数声明（lexer / parser / ast_dump 各自实现，供彼此调用）。
class Arena;  // 前向声明：定义见 arena.h
const char* kind_name(Kind k);
Diag* diag_add(DiagBag& bag, Arena& a, const Diag& d);

// check_type.cpp：类型检查 pass。在 parse 之后调用，诊断并进同一个 DiagBag。
void check_types(Node* root, const char* filename, Arena& arena, DiagBag& diags);

// check_types_with_externals 额外接受"别的模块声明了哪些 struct"的表
// （每行 `Struct\tField\tType`）。跨文件工程必须走这条口：
// 单文件检查器看不到别的 .h，不喂这张表就会把 import 来的类型判成 unknown。
void check_types_with_externals(Node* root, const char* filename, Arena& arena,
                                DiagBag& diags, const char* ext, size_t ext_len);

// check_type.cpp：类型检查 pass。在 parse 之后调用，诊断并进同一个 DiagBag。
void check_types(Node* root, const char* filename, Arena& arena, DiagBag& diags);

// check_idem.cpp：§5.7 的 retry 幂等证明（E5007 / E5009）。
// 顺序固定：parse → check_types → check_idempotency → codegen。
// 它需要类型检查后的 AST 形状与词法嵌套关系，但不需要 IR，因此不能放进 codegen。
void check_idempotency(Node* root, const char* filename, Arena& arena, DiagBag& diags);

}  // namespace hm
