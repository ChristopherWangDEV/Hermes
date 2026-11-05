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
  KW_RETRY, KW_DELAY, KW_TRY, KW_CATCH, KW_RETURN, KW_PARALLEL, KW_EXPORT,
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
  ParallelExpr, ParallelReturn,
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
  Node* body = nullptr;          // for 的循环体
  Node* args = nullptr;          // CallExpr 的首参；MatchArm 的 body Block；Let 的 Init
  Node* type_args = nullptr;     // CallExpr 的首类型实参
  Node* subs = nullptr;          // 定长多子节点数组（arena 分配）
  int n_subs = 0;

  const char* text = nullptr;    // 主字符串（名字/字面量/类型/spec/标注名）
  int64_t i64 = 0;
  double f64 = 0.0;
  bool is_export = false;
  bool is_entry = false;
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
  const DiagNote* notes;  // 跨文件指回定义处等
  int n_notes;
};

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

}  // namespace hm
