// check_type.cpp —— 类型检查 pass（T11）。
//
// ## 检查项归属（哪些在这里、哪些必须在 Go 侧）
//
// 分工的唯一依据是**这个 pass 看得见什么**。它拿到的是一个文件的 AST，
// 外加 is_header 标志；它看不到别的文件、看不到链接进来的 .hbc、不知道
// 自己是不是 root unit。按这条线划：
//
//   在这里（单文件可判）
//     E4016  字段访问（struct 没有该字段 / 对非 struct 取字段）
//     E4021  返回类型与声明不一致
//     E3016  用了 tool.call / model.generate 却没链对应 std 模块
//            （#link 就写在同一个文件里）
//     E3019  地址字面量里带 user:pass@（值看得见）
//     E3005/E3017/E3020  各文件的语法限制（由 parser 在语法层拦）
//     E2018  auth env("K", 默认值)（parser 在语法层拦）
//     E3018  env 第二参非字面量（parser 只吃 STRING，语法层已保证）
//
//   必须在 Go 侧（需要跨文件或需要"我是谁"的信息）
//     E3002  include 环            —— 需要 include 图（internal/include）
//     E3015  #include "x.hermes"   —— 同上
//     E1009  路径逃逸              —— 同上
//     E3021/E3022  零个/多个 entry —— **需要知道这是不是 root unit**：
//                 库文件允许 0 个 entry，在单文件阶段报会误伤每个库
//     E3024  entry 未导出          —— 需要 .h 里有没有 export 声明；
//                 examples/support_bot 自己就没写 export，单文件判会误报
//     E3014  声明与定义不一致      —— .h 与 .hermes 的比对（internal/link）
//     E3013  input 类型未 export   —— 同上
//     E3012  export 写两次         —— 声明(.h) 与定义(.hermes) 各写一次才算
//
// **不去做"看得见就顺手查"的诱惑**：E3021 在单文件里判会误伤所有库文件，
// 而误报比漏报更糟 —— 它会让"用这套语言写库"变成不可能。
//
// 设计约束与其他 pass 一致：无异常、无 RTTI、无 IO、无全局可变状态。

#include "arena.h"
#include "ast.h"

#include <cstdarg>
#include <cstdio>
#include <cstring>
#include <deque>
#include <string>
#include <vector>

namespace hm {

namespace {

// Ty 是检查器内部的类型表示。用 arena 分配而不是值语义数组，
// 因为数组元素要能互相引用（`array<array<T>>` 虽被语言禁止，但表示层不该先假定）。
struct Ty {
  enum K { Int, Double, Bool, Str, Json, Cost, StructK, ArrayK, TupleK, HttpResK, Void, Snapshot, Unknown } k;
  int struct_idx;  // StructK：structs_ 的下标
  Ty* elem;        // ArrayK
};

struct ExtField {
  const char* name;
  const char* type;  // 类型名：基础类型或另一个 struct 的名字
};

struct StructInfo {
  const char* name;
  Node* decl;  // StructDecl，child = Field 链；**为空表示声明在别的模块**
  // 跨文件声明（D-027 推论）：单文件检查器看不到别的 .h，而"看不见就报未知"
  // 会把每个正常的多文件工程判错。外部声明由 Go 侧收集后传进来，这里只存名字。
  std::vector<ExtField> ext;
};

struct VarSym {
  const char* name;
  Ty* t;
  Loc loc;
  // const 绑定：整个作用域内只读。fallback 块不是例外（E4026）。
  //
  // 必须带 `= false` 默认值：VarSym 有十来个构造点（参数、catch、for 循环
  // 变量、解构、独立扫描器…），漏写一个就会读到未初始化字节，让 E4026
  // 随机误报或漏报 —— 这类 bug 只在某个优化级别下复现。
  bool is_const = false;
  // catch 绑定的错误对象（`catch (T e)` 的 e）：类型是 unknown（字段检查在
  // 别处），只能靠这个标记识别；`auto x = e;` 把错误对象搬出 catch → E4023。
  bool is_error = false;
  // 错误对象的**类型名**（HttpError / NetworkError / …）。§7.1.1 的字段表按类型
  // 分，所以字段检查必须知道是哪一种 —— `is_error` 只能回答"是不是错误对象"。
  //
  // 默认 nullptr 而不是空串：VarSym 有十来个构造点，漏写一个就会读到未初始化
  // 字节（与 is_const 同一类坑）。nullptr 时 err_binding_type_ 直接判"不是错误
  // 绑定"，行为退回改动之前。
  const char* err_type = nullptr;
};

class Checker {
 public:
  Checker(Arena& a, DiagBag& d, const char* file) : a_(a), d_(d), file_(file) {}

  // load_externals_ 解析 Go 侧传来的声明表：每行 `Struct\tField\tType`。
  //
  // 为什么用这个笨格式而不是 JSON：前端被定义为无 IO、无第三方库，为了一条
  // 声明表在 C++ 里手写一个 JSON 解析器，是为省一次 split 而引入一个必错的东西。
  // 生产者与消费者都是我们自己的代码，格式越笨越好验。
  void load_externals_(const char* text, size_t len) {
    std::string cur;
    for (size_t i = 0; i <= len; ++i) {
      const bool end = (i == len) || (text[i] == '\n');
      if (!end) {
        cur.push_back(text[i]);
        continue;
      }
      if (!cur.empty()) {
        // 编译期静态注入的 extern 声明行不是 struct 字段表：
        //   "extern\t<sym>\t<w|f>\t<ret>\t<t1>,<t2>,..."
        // 它有 4 个 tab，若照 struct 行处理会被 add_external_ 误建成一个
        // 名为 "extern"、字段为 <sym>、类型为 "w\t<ret>\t..." 的伪 struct。
        // 真正的注入由 api.cpp::inject_extern_decls 在 parse 之后完成，
        // 这里只需让 struct 行的既有行为保持不变。
        if (cur.compare(0, 7, "extern\t") == 0) {
          cur.clear();
          continue;
        }
        const size_t t1 = cur.find('\t');
        const size_t t2 = (t1 == std::string::npos) ? std::string::npos : cur.find('\t', t1 + 1);
        if (t1 != std::string::npos && t2 != std::string::npos) {
          ext_storage_.push_back(cur.substr(0, t1));
          ext_storage_.push_back(cur.substr(t1 + 1, t2 - t1 - 1));
          ext_storage_.push_back(cur.substr(t2 + 1));
          const std::string& a = ext_storage_[ext_storage_.size() - 3];
          const std::string& b = ext_storage_[ext_storage_.size() - 2];
          const std::string& c = ext_storage_[ext_storage_.size() - 1];
          add_external_(a.c_str(), b.c_str(), c.c_str());
        }
        cur.clear();
      }
    }
  }


  void run(Node* mod) {
    if (!mod) return;
    collect_(mod);
    collect_links_(mod);
    // E3030 / E3032 是声明与指令自身的性质，与「有没有被调用」无关 ——
    // 必须在 run() 里独立跑一遍，否则未被调用的非法 extern 会静默编过。
    check_extern_declarations_();
    // E4029（旧 float 标签防线）走一次独立全遍历，而不是挂在 type_of_ 上：
    // type_of_ 会被同一个表达式反复调用（比较两侧、return 统一、字段访问…），
    // 挂在那里会一处报好几条。独立遍历还顺带覆盖函数体与所有参数位 —— 旧
    // float 标签若漏过，codegen 会把类型名写进字符串，同样是静默损坏。
    scan_float_(mod);
    // 2.0.1：先收集信号声明，emit/on 的合法性全靠这张表。
    for (Node* c = mod->child; c; c = c->next) {
      if (c->kind == Kind::SignalDecl && c->text) signals_.push_back(c->text);
    }
    for (Node* c = mod->child; c; c = c->next) {
      if (c->kind == Kind::BackendDef) check_backend_addr_(c);
    }
    for (Node* c = mod->child; c; c = c->next) {
      if (c->kind == Kind::WorkflowDecl) check_workflow_(c);
    }
    // 2.0.1：顶层 `on Signal { … }`（全局槽）也必须下潜 —— 否则全局槽体里的
    // 类型错误零诊断编成 IR，而那正是 TestTypeCheckDiagnosesInsideEveryStatementShape
    // 的 on_slot 用例要钉住的东西。
    on_signals_.clear();
    in_workflow_ = false;
    loop_depth_ = 0;
    for (Node* c = mod->child; c; c = c->next) {
      if (c->kind == Kind::SignalDecl && c->text) {
        // 信号声明已在上面收集；这里不做重复动作。
      }
    }
    for (Node* c = mod->child; c; c = c->next) {
      if (c->kind == Kind::OnStmt) stmt_(c);
    }
  }

  // E4029：旧 `float` 标签的纵深防线（v2.0.0 起**不再**是「所有浮点字面量被拒」）。
  //
  // 正常路径：浮点类型名只有 `double`（IEEE-754 binary64），parser 给浮点字面量
  // 打的是 "double" 标签，codegen 的 LitExpr 分支把它编成位模式
  // {"k":"double","bits":"<16 位小写 hex>"}。IR 字节里没有 JSON 浮点数字，
  // V-7 / canonical / 签名三件事都不受影响。
  //
  // 那为什么还留这道扫描：`hm_codegen*` 系列入口不因 DiagBag 里有 error 而短路，
  // 诊断只是并进 out_diag、codegen 照跑、rc 仍是 0。宿主可以直接调 C ABI 忽略
  // 诊断拿走 IR。若哪天有人把某个 LitExpr 的标签退回旧的 "float"（或手工造 AST
  // 塞进来），codegen 的兜底会把类型名当值写进 lit 节点，产出
  // {"t":"string","v":"float"} —— 合法、全校验通过、运行期返回字符串 "float"，
  // 纯静默损坏。所以三道扫描器（这里 + codegen.cpp 的 gen_expr / 参数默认值折叠）
  // 结构原样保留，判据继续只盯 "float"，作为防坏 IR 流出编译器的最后一道门。
  void scan_float_(Node* n) {
    if (!n) return;
    if (n->kind == Kind::LitExpr && n->text &&
        std::strcmp(n->text, "float") == 0) {
      err("E4029",
          internf("legacy float label %g has no IR value representation", n->f64),
          n->loc,
          "This is a legacy-tag defense line, not a ban on floating point. Since "
          "v2.0.0 the only floating type is `double` and floating literals are "
          "emitted as the bit pattern {\"k\":\"double\",\"bits\":\"<16 lowercase "
          "hex digits>\"}; no JSON floating-point number ever appears in IR bytes. "
          "A LitExpr tagged \"float\" is the pre-v2.0.0 shape and must never reach "
          "the IR writer: it falls into the string fallback, producing "
          "{\"t\":\"string\",\"v\":\"float\"} — fully valid IR that returns the "
          "string \"float\" at runtime.",
          "Use `double` for floating-point values (literals 1.5 / 1.5e2 / 1e3 are "
          "supported); use cost or int micro-units for fixed-point quantities — "
          "all money must be cost.",
          "The `json.as_float` API name is kept as-is; it narrows to a double value.");
    }
    scan_float_chain_(n->child);
    scan_float_chain_(n->args);
    scan_float_chain_(n->type_args);
    scan_float_chain_(n->else_branch);
    scan_float_chain_(n->body);
    // subs **必须**走：parse_parallel_return() 把 `return [a, b]` 的表达式列表
    // 用 to_subs 落成定长数组，codegen.cpp:1051 又逐个送进 gen_expr。漏掉它，
    // parallel 分支里漏进来的旧 float 标签就会绕过这道防线直达 IR（同样会变成字符串 "float"）。
    // 元素按索引单独扫、不沿 next 走：to_subs 是**按值复制**整块 Node，复制
    // 过来的 next 还指着原来的链，顺着走会跑进无关节点 / 把同一次出现数两遍。
    for (int i = 0; i < n->n_subs; ++i) scan_float_(&n->subs[i]);
  }

  void scan_float_chain_(Node* n) {
    while (n) {
      Node* next = n->next;
      scan_float_(n);
      n = next;
    }
  }

  // ------------------------------------------------------------------
  // W5004：`if (order.status == 404)` 的**启发式**警告（§7.1、§12.1 L3）。
  //
  // 为什么是警告不是错误：业务字段恰好叫 `status`、且在 200 响应里真的等于
  // 404，类型上完全合法（罕见但存在）。规范 §12.1 L3 如实登记了这条松点 ——
  // "这里必须承认判定不了，不能假装判定"。所以只提示，不拒。
  //
  // 判据（三条同时成立）：
  //   ① 一侧是 `==` / `!=` 的整数字面量，且在 400–599（HTTP 错误码区间）；
  //      2xx/3xx 不在此列 —— `status == 200` 是完全正常的业务值。
  //   ② 另一侧是**最后一段字段名**形如 status / status_code / httpStatus 的
  //      字段访问。
  //   ③ 该字段的宿主类型是**用户 struct**。这一条把两种正确写法放行：
  //        · `catch (HttpError e) { if (e.status == 404) }` —— e 的类型是
  //          unknown（错误对象的字段检查在别处），不匹配 StructK；
  //        · `allow_status = [404]` 拿到的 `r.status` —— 宿主是 HttpResK，
  //          那正是 allow_status 存在的意义。
  //
  // 为什么挂在 type_of_ 里而不是像 E4029 那样走独立遍历：判据 ③ 需要**作用域
  // 里的变量类型**，而独立遍历跑在 check_workflow_ 之前，那时 scope_ 是空的，
  // 所有字段访问的宿主都是 unknown —— 独立遍历什么都判不出来。
  //
  // 代价是 type_of_ 会对同一个表达式反复调用（比较两侧、return 统一、字段访问），
  // 于是用两个去重表兜住：status_field_hits_ 按**节点指针**记"这段字段访问
  // 像状态码"，warned_status_cmp_ 记"这条比较已经报过"。没有第二张表，同一个
  // `if (o.status == 404)` 会报两三条。
  // ------------------------------------------------------------------

  // 字段名规范化：去掉下划线、转小写，于是 status_code / statusCode /
  // StatusCode 都归到 statuscode。
  //
  // 只认这四个名字，**不做"名字里含 status"的宽松匹配**：那会把
  // status_message、order_status_text 一起拖进来，噪声会把这条警告变成
  // 人人都要关掉的东西 —— 一条总被关掉的警告等于没有。
  static bool is_status_field_name_(const char* name) {
    if (!name) return false;
    char buf[32];
    size_t k = 0;
    for (const char* p = name; *p && k + 1 < sizeof(buf); ++p) {
      char c = *p;
      if (c == '_') continue;
      if (c >= 'A' && c <= 'Z') c = static_cast<char>(c - 'A' + 'a');
      buf[k++] = c;
    }
    buf[k] = '\0';
    return std::strcmp(buf, "status") == 0 || std::strcmp(buf, "statuscode") == 0 ||
           std::strcmp(buf, "httpstatus") == 0 || std::strcmp(buf, "httpstatuscode") == 0;
  }

  // HTTP 错误码区间。刻意不含 2xx/3xx：`status == 200` 是常见业务值，
  // 把它也警告掉，等于让这条警告对"订单状态"这类字段天天误报。
  static bool is_http_status_literal_(const Node* n) {
    return n && n->kind == Kind::LitExpr && n->text && std::strcmp(n->text, "int") == 0 &&
           n->i64 >= 400 && n->i64 <= 599;
  }

  // 记下"这个 PathExpr 的最后一段是状态码字段、宿主是用户 struct"。
  void note_status_field_(Node* path) {
    for (Node* p : status_field_hits_) {
      if (p == path) return;
    }
    status_field_hits_.push_back(path);
  }

  bool is_status_field_node_(const Node* n) const {
    for (const Node* p : status_field_hits_) {
      if (p == n) return true;
    }
    return false;
  }

  // 在 BinaryExpr 的 is_eq 分支里调用：两侧都已求过类型，去重表也已就位。
  void warn_http_status_(Node* e) {
    Node* l = e->child;
    Node* r = e->args;
    if (!l || !r) return;
    Node* path = nullptr;
    Node* lit = nullptr;
    // `404 == o.status` 和 `o.status == 404` 是同一个错误，两侧都要试。
    if (is_status_field_node_(l) && is_http_status_literal_(r)) {
      path = l;
      lit = r;
    } else if (is_status_field_node_(r) && is_http_status_literal_(l)) {
      path = r;
      lit = l;
    }
    if (!path || path->kind != Kind::PathExpr || !path->subs || path->n_subs < 2) return;
    for (Node* p : warned_status_cmp_) {
      if (p == e) return;
    }
    warned_status_cmp_.push_back(e);

    const Node& last = path->subs[path->n_subs - 1];
    const char* fname = last.text ? last.text : "?";
    err_warn_(
        "W5004",
        internf("field %s is compared with HTTP status %lld, but the default tool.call "
                "contract never binds this value for a non-2xx response",
                fname, static_cast<long long>(lit->i64)),
        e->loc,
        "Without allow_status, tool.call returns T only on success and throws HttpError "
        "otherwise, so on a 404 the flow leaves at the tool.call line and this field is "
        "never bound — the comparison can never be true. The HTTP status only reaches the "
        "call site through allow_status (HttpResult<T>.status). This is a heuristic "
        "warning, not an error: a business field that happens to be named status and "
        "really equals 404 is legal, and the compiler cannot tell the two apart "
        "(LANGUAGE.md 12.1 L3).",
        internf("list the code in the call's allow_status and compare the result's status: "
                "`tool.call<T>(..., allow_status = [%lld])` then `r.status == %lld`",
                static_cast<long long>(lit->i64), static_cast<long long>(lit->i64)),
        // 第二条现在可以推荐 `catch` 了：`e.status` 已按 §7.1.1 可读（E4034 之前
        // 那段历史见 err_field_ 的注释）。2026-11-10 之前这条刻意绕道，因为那个
        // 写法编不过 —— 推荐一个写不出来的改法等于教人踩第二个坑。
        internf("or catch the failure and read the status off the error object: "
                "`try { … } catch (HttpError e) { if (e.status == %lld) { … } }`",
                static_cast<long long>(lit->i64)));
  }

 private:
  Arena& a_;
  DiagBag& d_;
  const char* file_;

  std::vector<StructInfo> structs_;
  // 外部声明字符串的**稳定存储**：用 deque 而不是 vector，因为 vector 扩容会
  // 让 c_str() 指针失效，而 structs_ 里存的是这些指针。
  std::deque<std::string> ext_storage_;
  std::vector<VarSym> scope_;

  // ---- 2.0.1 Sema 状态 ----------------------------------------------------
  // loop_depth_：break/continue 的合法性。`break` 只能跳出一层循环，
  // 永远不能穿过 workflow 边界（那是 stop 的职责，两者语义正交）。
  int loop_depth_ = 0;
  // in_workflow_ / workflow_ret_：stop 只在 workflow 内合法，且 data 的类型
  // 必须与 workflow 声明的返回类型**严格一致** —— 否则 stop 会把一个类型不对
  // 的值塞进返回位，而那是 LLVM `ret` 的类型不匹配，属于"能编过但跑飞"。
  bool in_workflow_ = false;
  Ty* workflow_ret_ = nullptr;
  // 已声明的信号名（顶层 `export signal X;`）。emit/on 都必须命中它。
  std::vector<const char*> signals_;
  // 当前作用域内已绑定的槽，用于查重复 `on`（同一作用域重复绑定是错误）。
  std::vector<const char*> on_signals_;
  // 全单元所有槽的 (signal, chan) 对。chan 为 nullptr 表示全局槽。
  // 用途：emit/until 的频道若一个槽都不匹配，说明这个信号永远没人接 ——
  // 那不该是错误（可能由别的模块接），但**必须是警告**：静默发射是最难查的一类。
  std::vector<std::pair<const char*, const char*>> all_slots_;
  // function / workflow 体内的 void 返回值上下文。
  bool fn_returns_void_ = false;

  // 本 unit 里 #link <std/X> 出现过哪些 X。E3016 靠它判断
  // "用了 tool.call 却没链接 <std/net>" —— 这个判断只需要单文件信息。
  bool has_std_net_ = false;
  bool has_std_model_ = false;
  // 每个文件只报一次，不逐处刷屏。
  bool reported_net_ = false;
  bool reported_model_ = false;

  // W5004 的两张去重表（见 warn_http_status_ 的注释）。
  // status_field_hits_：宿主是用户 struct、最后一段字段名像状态码的 PathExpr 节点。
  // warned_status_cmp_：已经报过 W5004 的比较节点。
  std::vector<Node*> status_field_hits_;
  std::vector<Node*> warned_status_cmp_;

  Ty* declared_ret_ = nullptr;  // 当前 workflow 在 .h/.hermes 上写的返回类型

  // 已经从 `parallel` 块里退出去的名字。E5002 靠它区分"从没定义"与
  // "定义了但出了块" —— 两种情况的修复动作完全不同：前者是拼错或漏 include，
  // 后者是架构问题（跨并行分支共享中间结果本来就不成立）。
  std::vector<const char*> closed_;

  // 已经走出 `for` 循环、循环变量名随即失效的名字。E4028 靠它识别"循环外引用
  // 循环变量" —— 与 closed_ 同理：循环变量只活在循环体内，出循环后引用它读到的是
  // 一个不再存在的槽位（运行期未定义行为），编译期必须拦。
  std::vector<const char*> for_closed_;

  // 每层 `parallel` 进入时的作用域水位，栈尾 = **最内层**。E5010 只与栈尾比较。
  //
  // 为什么是栈而不是单个 size_t：嵌套时内层分支拿到的是外层分支作用域的副本，
  // 所以"外层 parallel 里声明的名字"对内层同样是体外绑定，写它同样是死写。
  // 只取最内层水位就自然覆盖这一点（内层 mark >= 外层 mark）。
  std::vector<size_t> parallel_marks_;
  // fallback 嵌套深度。目前只用于「已经在一个 fallback 块里」的诊断文案，
  // 以及未来禁止 fallback 内再套 fallback 时留的位置；语义判定不依赖它。
  int fallback_depth_ = 0;

  // 本 unit 的函数声明表（name → FunctionDecl）。E4030 在函数调用点比对实参类型
  // 与形参声明类型时需要它；collect_ 阶段一次性建好，type_of_ 调用时直接查。
  struct FnEntry { std::string name; Node* decl; };
  std::vector<FnEntry> fn_table_;
  // 同一 CallExpr 节点会被 type_of_ 反复求值（比较两侧 / return 统一 / 字段访问…），
  // 用这张表去重，避免一处调用报多条 E4030（与 scan_float_ 移出 type_of_ 同因）。
  std::vector<const Node*> checked_calls_;

  // ---- 基础 ----

  Ty* mk(Ty::K k) {
    Ty* t = make_node<Ty>(a_);
    if (t) {
      t->k = k;
      t->struct_idx = -1;
      t->elem = nullptr;
    }
    return t;
  }

  Ty* unknown() { return mk(Ty::Unknown); }

  // find_fn_ 在 fn_table_ 里按名字找函数声明。找不到返回 nullptr。
  // 同名函数已在 codegen / 装配期查重（验收 #5），这里只取第一个。
  Node* find_fn_(const char* name) {
    if (!name) return nullptr;
    for (const auto& kv : fn_table_) {
      if (kv.name == name) return kv.decl;
    }
    return nullptr;
  }

  // extern 声明表（name → 声明节点：FunctionDecl 或 WorkflowSig，is_extern）。
  // 裸调用 `sqrt(...)` 若命中此表，走 call_native（C ABI 直调），不进 fn_table_
  // （无体，不能按 call_fn 内联/递归；emit 时也不进 functions[] 表）。
  struct ExternEntry { std::string name; Node* decl; };
  std::vector<ExternEntry> extern_table_;
  Node* find_extern_(const char* name) {
    if (!name) return nullptr;
    for (const auto& kv : extern_table_) {
      if (kv.name == name) return kv.decl;
    }
    return nullptr;
  }

  // 本 unit 里出现过的 #link native "..." from "..." 节点（供 extern 的
  // E3031 / E3032 使用）。#link <std/*> 不在这里收集（那是 collect_links_ 的事）。
  std::vector<const Node*> native_links_;
  // E3032 只报一次（per module）：type_of_ 会对同一 CallExpr 反复求值。
  bool reported_link_header_ = false;

  // 从 #link native 的 verbatim text（native "archive" from "header"）里抽出
  // 两个带引号字符串：archive 与 header。任一缺失则对应指针置 nullptr。
  void parse_native_link_(const char* text, const char** archive, const char** header) {
    *archive = nullptr;
    *header = nullptr;
    if (!text) return;
    const char* q1 = strchr(text, '"');
    if (!q1) return;
    const char* q2 = strchr(q1 + 1, '"');
    if (!q2) return;
    *archive = a_.intern(q1 + 1, static_cast<size_t>(q2 - q1 - 1));
    const char* q3 = strchr(q2 + 1, '"');
    if (!q3) return;
    const char* q4 = strchr(q3 + 1, '"');
    if (!q4) return;
    *header = a_.intern(q3 + 1, static_cast<size_t>(q4 - q3 - 1));
  }

  // 取形参的类型节点：form A（workflow 风格）类型在 child，form B（C++ 风格）
  // 在 type_args。返回 nullptr 表示无类型信息（交给别处）。
  const Node* extern_param_type_(const Node* p) {
    if (!p || p->kind != Kind::Param) return nullptr;
    if (p->child && p->child->kind == Kind::Type) return p->child;
    if (p->type_args && p->type_args->kind == Kind::Type) return p->type_args;
    return nullptr;
  }

  // §3.1 闭集：允许跨 C ABI 边界的类型。
  //   标量：int / bool / string / double / cost（+ char/short/long 的 int 别名）
  //   @opaque struct：任意结构名（按指针传）
  // 拒绝：json / vector / 数组维度（i64 > 0）。
  bool c_abi_type_ok_(const Node* t) {
    if (!t || t->kind != Kind::Type) return false;
    if (t->i64 > 0) return false;  // 数组维度不允许跨边界
    const char* s = t->text ? t->text : "";
    if (strcmp(s, "json") == 0) return false;
    if (strcmp(s, "vector") == 0) return false;
    if (strcmp(s, "int") == 0 || strcmp(s, "bool") == 0 || strcmp(s, "string") == 0 ||
        strcmp(s, "double") == 0 || strcmp(s, "cost") == 0 ||
        strcmp(s, "char") == 0 || strcmp(s, "short") == 0 || strcmp(s, "long") == 0)
      return true;
    // 其余当作 @opaque struct（按指针传），合法跨边界。
    return true;
  }

  // E3031：extern 必须在本模块有 #link native。
  //
  // 故意留在**调用点**，不进 check_extern_declarations_：多文件形态
  // （EXTERN-C-ABI.md §1.3 正例 1）里 extern 声明在 net.h、#link native
  // 在 net.hermes，而类型检查是**单文件**跑的（api.cpp:run_parse 对每个
  // 文件各调一次 check_types）。若把它做成声明级，合法的 net.h 会被误判
  // 「缺 #link native」。真正的门在链接期（internal/link/link.go 的
  // hasNativeRequire），调用点这条只是让错误早一点、指到具体符号。
  // call 是**调用点**表达式（不是声明）：E3031 只在调用点触发，诊断也指到
  // 调用点。跨文件注入的 extern 声明带 loc.line == 0 哨兵、没有源码位置，
  // 若照旧用 ex->loc 会渲染出一条没有行号的 E3031；指到 call 才可读。
  void check_extern_link_(const Node* ex, const char* sym, const Node* call) {
    if (native_links_.empty()) {
      const Loc loc = (call && call->loc.line > 0) ? call->loc : ex->loc;
      err("E3031",
          internf("extern declaration %s has no matching #link native", sym ? sym : "?"),
          loc,
          "An extern symbol is resolved only from a #link native archive in the same module. "
          "Without it the C symbol is unresolved at link time — a build failure much later than "
          "this line.",
          "add `#link native \"<archive>\" from \"<header>\";` to this module");
    }
  }

  // 声明级 / 指令级校验（EXTERN-C-ABI.md §1.4 / §5）：
  //   E3030 —— 每条 extern 声明的形参与返回类型必须 ∈ §3.1 闭集；
  //   E3032 —— 每条 #link native 指令的 from 头必须是纯 C 头（.h）。
  //
  // 这两条是**声明与指令自身的性质**，与「这个 extern 有没有被调用」无关。
  // 之前它们挂在 type_of_ 的 CallExpr 分支里，于是实测两种源码都能 rc=0
  // 编过：① 声明了 `-> json` 的 extern 但从不调用；② 写了一条 .hpp 的
  // #link native 但没有任何 extern。把检查移到 run() 里独立跑一遍即可
  // 堵住，调用点只留 E3031（要看有没有 link）与 E4030（实参类型）。
  void check_extern_declarations_() {
    // ① E3030：遍历全部 extern 声明（含未调用的），查形参 + 返回类型。
    for (const auto& kv : extern_table_) {
      const Node* ex = kv.decl;
      // loc.line == 0 是编译期静态注入哨兵（api.cpp::inject_extern_decls）。
      // 注入节点不是本模块的源码声明：它来自别的 .h，那边的**原模块单独
      // check_types** 已经按真实行列检查过一遍。这里再跑一次，同一条非法
      // extern 会在多文件工程里报出两条 E3030（诊断重复）。
      if (ex->loc.line == 0) continue;
      for (const Node* p = ex->child; p; p = p->next) {
        if (p->kind != Kind::Param) continue;
        const Node* pt = extern_param_type_(p);
        if (pt && !c_abi_type_ok_(pt)) {
          err("E3030",
              internf("type %s cannot cross the C ABI boundary", pt->text ? pt->text : "?"),
              p->loc,
              "The C ABI boundary only allows int, bool, string, double, cost and "
              "@opaque struct (by pointer). json, vector<T> and generics have no stable "
              "C layout and would silently corrupt across the boundary.",
              "change the parameter to a C-ABI-compatible type");
        }
      }
      const Node* rt = ex->type_args;
      if (rt && !c_abi_type_ok_(rt)) {
        err("E3030",
            internf("return type %s cannot cross the C ABI boundary", rt->text ? rt->text : "?"),
            ex->loc,
            "The C ABI boundary only allows int, bool, string, double, cost and "
            "@opaque struct (by pointer). json, vector<T> and generics have no stable "
            "C layout and would silently corrupt across the boundary.",
            "change the return type to a C-ABI-compatible type");
      }
    }
    // ② E3032：遍历全部 #link native 指令，与 extern 是否存在无关。
    if (reported_link_header_) return;  // 每个模块只报一次，避免刷屏
    for (const Node* lk : native_links_) {
      const char* archive = nullptr;
      const char* header = nullptr;
      parse_native_link_(lk->text, &archive, &header);
      if (!header) continue;
      size_t n = strlen(header);
      bool is_pure_c = (n >= 2 && strcmp(header + n - 2, ".h") == 0);
      if (!is_pure_c) {
        reported_link_header_ = true;
        err("E3032",
            internf("native header %s is not a pure-C ABI header", header),
            lk->loc,
            "cgo only parses C headers; a C++ header (.hpp/.cpp/.cc) would fail in the C "
            "preprocessor and has no stable C ABI. The .h is the lowest common denominator "
            "shared by C, C++ and Go.",
            "use a pure-C header ending in .h, or write a thin extern \"C\" shim");
        return;
      }
    }
  }

  // children_of_ 复刻 codegen 的 children_of：child → type_args → args → else_branch。
  // 调用实参的 Param 节点把表达式挂在 `args` 槽（parser.cpp 1784/1790），所以
  // 取 vals[0] 就是实参表达式本身。
  std::vector<const Node*> children_of_(const Node* n) {
    std::vector<const Node*> out;
    if (!n) return out;
    for (const Node* c = n->child; c; c = c->next) out.push_back(c);
    for (const Node* c = n->type_args; c; c = c->next) out.push_back(c);
    for (const Node* c = n->args; c; c = c->next) out.push_back(c);
    if (n->else_branch) out.push_back(n->else_branch);
    return out;
  }

  // err 记录一条错误诊断。fix2 可选（diagnostics 的 "fix" 在 JSON 里是数组，
  // 一条诊断给两条修法时用它）；默认 nullptr 让既有调用点一个都不用改。
  // 末五参是 M2 的机器可应用编辑：edit_repl 非空时把 (span=行/列, replacement)
  // 写进 Diag，由 diag_to_json 输出为 edits[]。全部有默认值，既有调用点不变。
  void err(const char* code, const char* msg, Loc loc, const char* why, const char* fix,
           const char* fix2 = nullptr,
           const char* edit_repl = nullptr, int edit_line = 0, int edit_col = 0,
           int edit_end_line = 0, int edit_end_col = 0) {
    Diag d;
    memset(&d, 0, sizeof(d));
    d.severity = Severity::Error;
    d.code = code;
    d.message = msg;
    d.loc = loc;
    d.module = file_;
    d.why = why;
    d.fix = fix;
    d.fix2 = fix2;
    if (edit_repl) {
      d.edit_replacement = edit_repl;
      d.edit_line = edit_line;
      d.edit_col = edit_col;
      d.edit_end_line = edit_end_line;
      d.edit_end_col = edit_end_col;
    }
    diag_add(d_, a_, d);
  }

  // err_warn_ 是 err 的**警告**版本：同一条通道、同样的 why/fix 纪律，
  // 只是 severity 是 Warning（不阻塞构建，`--deny-warnings` 可升级）。
  //
  // 为什么不做成 err(..., Severity) 一个函数：调用点遍布全文件，加一个默认参数
  // 意味着任何一处手滑漏传都会把错误降级成警告 —— 而"错误被静默降级"是这类
  // 改动里最难发现的缺陷。两个名字，两种后果，读代码时不用看参数。
  void err_warn_(const char* code, const char* msg, Loc loc, const char* why, const char* fix,
                 const char* fix2 = nullptr) {
    Diag d;
    memset(&d, 0, sizeof(d));
    d.severity = Severity::Warning;
    d.code = code;
    d.message = msg;
    d.loc = loc;
    d.module = file_;
    d.why = why;
    d.fix = fix;
    d.fix2 = fix2;
    diag_add(d_, a_, d);
  }

  const char* internf(const char* fmt, ...) {
    char buf[512];
    va_list ap;
    va_start(ap, fmt);
    vsnprintf(buf, sizeof(buf), fmt, ap);
    va_end(ap);
    return a_.intern(buf, strlen(buf));
  }

  // ---- 声明收集 ----

  void collect_(Node* mod) {
    for (Node* c = mod->child; c; c = c->next) {
      if (c->kind == Kind::StructDecl) {
      const char* name = c->text ? c->text : "";
      // E4014：同一文件内重复声明同名 struct（单文件即可判定）。
      // 只与**本文件已收集的** struct 比（decl != nullptr）；外部模块经
      // load_externals_ 进来的 struct 其 decl 为 nullptr，不在此判（那是跨文件
      // 装配期的 E_IR_SYM_CONFLICT 职责）。struct 名是生成 JSON Schema `$defs`
      // 的键，重名会让键二义，而 schema 字节进请求哈希，故编译期直接拦下。
      for (size_t i = 0; i < structs_.size(); ++i) {
        if (structs_[i].name && structs_[i].decl && strcmp(structs_[i].name, name) == 0) {
          err("E4014", internf("duplicate struct declaration `%s` in this file", name), c->loc,
              "Struct names are keys in the generated JSON Schema `$defs`. Two structs with the "
              "same name in one file make that key ambiguous, and the schema bytes are hashed "
              "into the request signature — so the collision cannot be silently resolved.",
              "rename one of the structs, or split them across files");
          break;
        }
      }
      StructInfo si;
      si.name = name;
      si.decl = c;
      structs_.push_back(si);
      } else if (c->kind == Kind::FunctionDecl) {
        // extern 函数声明（无体）：只登记到 extern 表，不进 fn_table_
        // （不能按 call_fn 内联/递归；emit 时走 call_native，也不进 functions[]）。
        const char* name = c->text ? c->text : "";
        if (c->is_extern) {
          if (!find_extern_(name)) extern_table_.push_back({name, c});
        } else {
          // E4030 需要函数声明表：在调用点比对实参类型与形参声明类型。
          // 同名函数在 codegen / 装配期已查重（验收 #5），这里只建表，命中第一个。
          if (!find_fn_(name)) fn_table_.push_back({name, c});
        }
      } else if (c->kind == Kind::WorkflowSig) {
        // extern workflow 签名（无体）：登记到 extern 表，供 call_native 解析。
        if (c->is_extern) {
          const char* name = c->text ? c->text : "";
          if (!find_extern_(name)) extern_table_.push_back({name, c});
        }
      } else if (c->kind == Kind::Link) {
        // 记录 #link native "..." from "..."; 供 extern 的 E3031 / E3032 使用。
        if (c->text && strncmp(c->text, "native ", 7) == 0) native_links_.push_back(c);
      }
    }
  }

  // collect_links_ 记录出现过的 #link <std/*>。
  // Link 节点的 text 是 spec 原文，带尖括号：`<std/net>`。
  void collect_links_(Node* mod) {
    for (Node* c = mod->child; c; c = c->next) {
      if (c->kind != Kind::Link || !c->text) continue;
      const char* p = c->text;
      if (*p == '<') ++p;
      const char* e = p;
      while (*e && *e != '>') ++e;
      size_t n = static_cast<size_t>(e - p);
      if (n == 7 && strncmp(p, "std/net", 7) == 0) has_std_net_ = true;
      if (n == 9 && strncmp(p, "std/model", 9) == 0) has_std_model_ = true;
    }
  }

  // 优先返回本地声明（decl != nullptr，带 AST）的那条：跨文件场景里，
  // load_externals_ 会在 collect_ 之前先把同一 struct 以 extern 形式登记进
  // structs_（decl == nullptr）。若不优先本地，type_of_ 解析出来的 struct_idx
  // 会指到那条空壳，于是 list_fields_ / nearest_field_ 全都拿不到字段，
  // 拼错字段时既不报候选也不报 did-you-mean——而单测走 check_types 不设 ext，
  // 看不到这条空壳，所以单测绿、真实 ir 命令红。这是个只在真实管线暴露的坑。
  int find_struct_(const char* name) {
    int first = -1;
    for (size_t i = 0; i < structs_.size(); ++i) {
      if (structs_[i].name && strcmp(structs_[i].name, name) == 0) {
        if (first < 0) first = static_cast<int>(i);
        if (structs_[i].decl) return static_cast<int>(i);  // 本地声明优先
      }
    }
    return first;
  }

  // from_type_ 把一个 Type 节点翻成 Ty。数组维数是 Type.i64（parse_type 填的）。
  Ty* from_type_(Node* t) {
    if (!t || t->kind != Kind::Type) return unknown();
    const char* s = t->text ? t->text : "";
    Ty* base = ty_from_name_(s);
    if (!base) return unknown();
    for (int k = 0; k < static_cast<int>(t->i64); ++k) {
      Ty* arr = mk(Ty::ArrayK);
      if (!arr) break;
      arr->elem = base;
      base = arr;
    }
    return base;
  }

  // ty_from_name_ 只认一个名字（不含数组维度），外部声明与本地字段共用它 ——
  // 两份映射必然漂移，而这正是"同一个类型在两个地方理解不同"的来源。
  Ty* ty_from_name_(const char* s) {
    if (strcmp(s, "string") == 0) return mk(Ty::Str);
    if (strcmp(s, "int") == 0) return mk(Ty::Int);
    // char / short / long：C 风格整数宽度类型。类型检查按 int 处理（宽度截断
    // 发生在运行期/后端的 cast 上，不是类型系统的事）。
    if (strcmp(s, "char") == 0 || strcmp(s, "short") == 0 || strcmp(s, "long") == 0) return mk(Ty::Int);
    // 浮点类型名只有 `double`（IEEE-754 binary64）；`float` 已移除。
    if (strcmp(s, "double") == 0) return mk(Ty::Double);
    if (strcmp(s, "bool") == 0) return mk(Ty::Bool);
    if (strcmp(s, "cost") == 0) return mk(Ty::Cost);
    if (strcmp(s, "json") == 0) return mk(Ty::Json);
    int si = find_struct_(s);
    if (si < 0) return nullptr;
    Ty* t = mk(Ty::StructK);
    if (t) t->struct_idx = si;
    return t;
  }

  // struct_field_ 按名字找字段并返回类型；找不到返回 nullptr 且回填下标 -1。
  Ty* struct_field_(int si, const char* name) {
    if (si < 0 || si >= static_cast<int>(structs_.size())) return nullptr;
    if (!structs_[si].decl) {
      // 外部声明：字段表由 Go 侧收集后传入（没有 AST 可走）。
      for (const ExtField& f : structs_[si].ext) {
        if (f.name && strcmp(f.name, name) == 0) {
          Ty* t = ty_from_name_(f.type ? f.type : "");
          return t ? t : unknown();
        }
      }
      return nullptr;
    }
    for (Node* f = structs_[si].decl->child; f; f = f->next) {
      if (f->kind != Kind::Field) continue;
      if (f->text && strcmp(f->text, name) == 0) return from_type_(f->child);
    }
    return nullptr;
  }

  // add_external_ 登记一条来自别的模块的字段。同名 struct 已存在就并入
  // （本地声明优先：本地有 AST，字段表比外部那份更可信）。
  void add_external_(const char* sname, const char* fname, const char* ftype) {
    int si = find_struct_(sname);
    if (si < 0) {
      StructInfo si2;
      si2.name = sname;
      si2.decl = nullptr;
      structs_.push_back(si2);
      si = static_cast<int>(structs_.size()) - 1;
    }
    if (structs_[si].decl) return;  // 本地声明优先
    ExtField f;
    f.name = fname;
    f.type = ftype;
    structs_[si].ext.push_back(f);
  }

  const char* type_name_(Ty* t) {
    if (!t) return "?";
    switch (t->k) {
      case Ty::Int: return "int";
      case Ty::Double: return "double";
      case Ty::Bool: return "bool";
      case Ty::Str: return "string";
      case Ty::Json: return "json";
      case Ty::Cost: return "cost";
      case Ty::StructK:
        if (t->struct_idx >= 0 && t->struct_idx < static_cast<int>(structs_.size()))
          return structs_[t->struct_idx].name;
        return "struct?";
      case Ty::ArrayK: return "array";
      case Ty::TupleK: return "tuple";
      case Ty::HttpResK: return "httpresult";
      case Ty::Void: return "void";
      case Ty::Snapshot: return "snapshot";
      case Ty::Unknown: return "unknown";
    }
    return "?";
  }

  // list_fields_ 列出 struct 的字段名，用于"未知字段"的修复建议。
  // 不列字段的 AttributeError 是 Python 最让人恼火的一点：它说你错了，不说你有啥。
  const char* list_fields_(int si) {
    if (si < 0 || si >= static_cast<int>(structs_.size())) return "(none)";
    // `decl` 可以为空：从 externals（别的模块的头）登记进来的 struct 没有本地 AST。
    // 不挡它，`decl->child` 就是空指针解引用 —— 用户看到的不是"拼错字段名"的提示，
    // 而是**编译器崩溃**，而这两者之间差着一整轮调试。
    if (!structs_[si].decl) return "(none)";
    char buf[512];
    int used = 0;
    buf[0] = '\0';
    for (Node* f = structs_[si].decl->child; f; f = f->next) {
      if (f->kind != Kind::Field || !f->text) continue;
      int n = snprintf(buf + used, sizeof(buf) - used, "%s%s", used ? ", " : "", f->text);
      if (n > 0) used += n;
      if (used >= static_cast<int>(sizeof(buf)) - 2) break;
    }
    return a_.intern(buf, strlen(buf));
  }

  // nearest_field_ 在 struct 的字段名集合里找 Edit-Distance ≤ 2 的最近名字，
  // 用于 E4016 的 did-you-mean（"语法纠正"）。找不到返回 nullptr。
  // 只在本地声明可达时工作（外部 struct 没有 AST，decl == nullptr，直接返回 nullptr，
  // 此时本来就没有字段名可比对）。edit_distance 来自 ast.h。
  const char* nearest_field_(int si, const char* name) {
    if (si < 0 || si >= static_cast<int>(structs_.size())) return nullptr;
    if (!structs_[si].decl) return nullptr;
    const char* best = nullptr;
    int best_d = 3;  // 严格小于 3 ⇒ 只接受 ≤ 2
    for (Node* f = structs_[si].decl->child; f; f = f->next) {
      if (f->kind != Kind::Field || !f->text) continue;
      int d = edit_distance(name, strlen(name), f->text, strlen(f->text));
      if (d <= 2 && d < best_d) { best_d = d; best = f->text; }
    }
    return best;
  }

  Ty* lookup_(const char* name) {
    for (int i = static_cast<int>(scope_.size()) - 1; i >= 0; --i) {
      if (scope_[i].name && strcmp(scope_[i].name, name) == 0) return scope_[i].t;
    }
    return nullptr;
  }

  // 名字当前是否绑定到一个 catch 的错误对象（E4023 用）。取最内层同名绑定，
  // 与 lookup_ 一致；错误对象的类型是 unknown，只能靠 VarSym.is_error 识别。
  bool is_error_binding_(const char* name) {
    if (!name) return false;
    for (int i = static_cast<int>(scope_.size()) - 1; i >= 0; --i) {
      if (scope_[i].name && strcmp(scope_[i].name, name) == 0) return scope_[i].is_error;
    }
    return false;
  }

  // 名字当前绑定到的错误对象的**类型名**；不是错误绑定返回 nullptr。
  // 与 is_error_binding_ 同一条"取最内层"规则 —— 两处不一致就会让字段检查
  // 读到另一个绑定的类型（`catch (HttpError e)` 里再 `catch (ModelError e)`）。
  const char* err_binding_type_(const char* name) {
    if (!name) return nullptr;
    for (int i = static_cast<int>(scope_.size()) - 1; i >= 0; --i) {
      if (scope_[i].name && strcmp(scope_[i].name, name) == 0) {
        return scope_[i].is_error ? scope_[i].err_type : nullptr;
      }
    }
    return nullptr;
  }

  // ---- 错误对象的字段（LANGUAGE §7.1.1）------------------------------------
  //
  // `catch (HttpError e) { if (e.status == 404) { … } }` **必须写得出来**：这是
  // D-014 点名的要求，也是"订单不存在"这个最常见、最需要给用户一句人话的分支
  // 在语言里唯一的落地写法。此前它报 `E4016: cannot access field status on
  // unknown` —— 文档与样例都在推荐一个编不过的写法。
  //
  // `ready` 这一列是这张表的重点：**字段在规范里有，不等于运行期带得出来**。
  // 引擎的错误值（`engine.RunError.value()`）目前只填了 status，其余 §7.1.1
  // 的字段（url / method / body_prefix / attempts / provider / …）还没有人往里
  // 写。对它们：
  //   · 报 E4016（"没有这个字段"）是**假话** —— 规范说它存在，会把人赶去查拼写；
  //   · 静默放行是更坏的假话 —— 编得过，运行期 `field "url" does not exist`，
  //     而那是普通 error 不是 RunError，连 catch 都接不住。
  // 所以单独给 E4034：「规范有、运行期还没带上」。运行期真把某个字段填上时，
  // 只改这里的 ready 标记 —— `tests/compiler/parser_test.cpp::test_err_fields`
  // 两侧都钉住（ready 的能编、不 ready 的报 E4034）。
  //
  // 这张表是**唯一判定点**：字段类型、E4016 的候选清单、E4034 的判定全读它。
  struct ErrFieldDef {
    const char* type;
    const char* name;
    Ty::K kind;
    bool ready;
  };

  static const ErrFieldDef* err_field_table_(int* n) {
    static const ErrFieldDef kTable[] = {
        // NetworkError：连接失败/超时/对端重置（无 HTTP 响应）
        {"NetworkError", "kind", Ty::Str, false},
        {"NetworkError", "url", Ty::Str, false},
        {"NetworkError", "elapsed_ms", Ty::Int, false},
        // HttpError：收到响应但状态码不被接受。status 是唯一运行期已带的字段。
        {"HttpError", "status", Ty::Int, true},
        {"HttpError", "url", Ty::Str, false},
        {"HttpError", "method", Ty::Str, false},
        {"HttpError", "body_prefix", Ty::Str, false},
        {"HttpError", "attempts", Ty::Int, false},
        // ModelError：上游无状态码时 status 为 0
        {"ModelError", "provider", Ty::Str, false},
        {"ModelError", "model", Ty::Str, false},
        {"ModelError", "kind", Ty::Str, false},
        {"ModelError", "status", Ty::Int, false},
        // ContractError：schema 校验失败 / json.as_* 窄化失败 / 工具响应不符
        {"ContractError", "where", Ty::Str, false},
        {"ContractError", "expect", Ty::Str, false},
        {"ContractError", "got", Ty::Str, false},
        {"ContractError", "path", Ty::Str, false},
        // CircuitOpenError：熔断 open，请求未发出
        {"CircuitOpenError", "provider", Ty::Str, false},
        {"CircuitOpenError", "model", Ty::Str, false},
        {"CircuitOpenError", "open_until_ms", Ty::Int, false},
        // BudgetError 不可捕获（§7.3，E5006），字段只为 trace 与 explain 存在。
        {"BudgetError", "limit_micro_usd", Ty::Int, false},
        {"BudgetError", "spent_micro_usd", Ty::Int, false},
        {"BudgetError", "requested_micro_usd", Ty::Int, false},
    };
    if (n) *n = static_cast<int>(sizeof(kTable) / sizeof(kTable[0]));
    return kTable;
  }

  // 某类型的字段名清单（E4016 的 help 用）。只列**运行期已经带得出来**的，
  // 因为"你该写哪个"这个问题的答案只能是有用的那些；全列会把人引向 E4034。
  const char* list_ready_err_fields_(const char* etype) {
    int n = 0;
    const ErrFieldDef* t = err_field_table_(&n);
    std::string out;
    for (int i = 0; i < n; ++i) {
      if (std::strcmp(t[i].type, etype) != 0 || !t[i].ready) continue;
      if (!out.empty()) out += ", ";
      out += t[i].name;
    }
    if (out.empty()) return internf("(none yet)");
    return internf("%s", out.c_str());
  }

  // 某类型在规范里的全部字段名（E4016 的 "has:" 用）。
  const char* list_all_err_fields_(const char* etype) {
    int n = 0;
    const ErrFieldDef* t = err_field_table_(&n);
    std::string out;
    for (int i = 0; i < n; ++i) {
      if (std::strcmp(t[i].type, etype) != 0) continue;
      if (!out.empty()) out += ", ";
      out += t[i].name;
    }
    return internf("%s", out.c_str());
  }

  // 解析 `e.<field>`。返回 nullptr 表示"已报错，调用方按未知类型继续"。
  Ty* err_field_(const char* etype, Node& f) {
    const char* fname = f.text ? f.text : "?";
    // `catch (Error e)`：静态类型是根，而字段集合按子类型分 —— §7.1.1 里没有
    // 一个字段是全部子类型共有的（status 在 HttpError/ModelError 上有，
    // NetworkError 上就没有）。这里**猜不了**，只能要求换成具体类型。
    if (std::strcmp(etype, "Error") == 0) {
      err("E4016",
          internf("`Error` has no field %s: the static type of `e` here is the root type, "
                  "and its fields depend on which subtype was caught", fname),
          f.loc,
          "LANGUAGE.md 7.1.1 gives each error type its own field set, and there is no field "
          "common to all of them. Typing `e.status` on the root type would be a guess: it exists "
          "on HttpError and ModelError but not on NetworkError.",
          "catch the specific type instead: `catch (HttpError e) { if (e.status == 404) { … } }`",
          "or use one `catch` arm per type you actually handle");
      return nullptr;
    }
    int n = 0;
    const ErrFieldDef* t = err_field_table_(&n);
    const ErrFieldDef* hit = nullptr;
    for (int i = 0; i < n; ++i) {
      if (std::strcmp(t[i].type, etype) == 0 && std::strcmp(t[i].name, fname) == 0) {
        hit = &t[i];
        break;
      }
    }
    if (!hit) {
      err("E4016", internf("%s has no field %s", etype, fname), f.loc,
          "Each error type's field set is closed and written down in LANGUAGE.md 7.1.1. "
          "A field the type does not declare is not produced by the runtime, so reading it "
          "could only ever yield nothing.",
          internf("%s has: %s", etype, list_all_err_fields_(etype)),
          internf("fields the runtime already carries for %s: %s", etype, list_ready_err_fields_(etype)));
      return nullptr;
    }
    if (!hit->ready) {
      err("E4034",
          internf("field %s of %s is specified but the runtime does not produce it yet", fname, etype),
          f.loc,
          "LANGUAGE.md 7.1.1 declares this field, but the executor's error value does not carry it "
          "yet. Accepting the access would compile and then die at run time with `field does not "
          "exist` — a static checker that passes while the program fails is worse than a refusal.",
          internf("fields the runtime already carries for %s: %s", etype, list_ready_err_fields_(etype)),
          "or read the outcome as a normal value: with `allow_status = [404]` the status comes back "
          "on HttpResult<T>, so `r.status == 404` needs no error object at all");
      return nullptr;
    }
    return mk(hit->kind);
  }

  // ---- backend 地址（E3019）----

  // check_backend_addr_ 检查**看得见的字面量**：`backend x = "..."` 的地址，
  // 以及 `env("URL", "...")` 的默认值。`env("URL")` 单实参形式不查 ——
  // 值要到运行期才知道，编译期无从判断，硬查只能靠猜。
  //
  // 为什么这条值得一个错误码：地址会被写进 IR、进 .hbc、进签名产物、
  // 进 `hermes explain` 的输出。一个带 user:pass@ 的字面量会沿着所有这些
  // 路径扩散，而它的修复动作（换成 env）与"地址写错了"完全不同。
  void check_backend_addr_(Node* bd) {
    if (!bd) return;
    const char* name = bd->text ? bd->text : "?";
    check_credential_(bd->child, name);
    Node* addr = bd->child;
    if (addr && addr->kind == Kind::EnvExpr) {
      // **默认值在 `args` 上，不在 `child->next` 上**（见 parser.cpp 的
      // parse_env_clause：`env->child->text = 名; env->args->text = 默认值;`）。
      // 按兄弟链取会拿到 null 而静默跳过 —— 这正是"漏查默认值"那个 bug。
      Node* second = nullptr;
      if (addr->child && addr->child->next) second = addr->child->next;  // 兼容兄弟链写法
      if (!second) second = addr->args;
      if (!second && addr->n_subs >= 2) second = &addr->subs[1];          // 兼容 subs 写法
      if (second) check_credential_(second, name);
    }
  }

  void check_credential_(Node* lit, const char* backend_name) {
    // 无插值的字符串：text 就是内容、n_subs == 0。
    // 有插值的字符串：text 是类型标记 "string"、内容在 subs —— 那种情形
    // 地址含什么要到运行期才知道，不在这里判。
    if (!lit || lit->kind != Kind::LitExpr || lit->n_subs != 0 || !lit->text) return;
    const char* s = lit->text;
    const char* scheme = strstr(s, "://");
    if (!scheme) return;
    if (!strchr(scheme, '@')) return;
    err("E3019", "backend address carries credentials (user:pass@host)", lit->loc,
        "The address is written into the IR, the archive, and every explain output. A literal "
        "user:pass@ is how credentials leak into artifacts that get copied to build servers and "
        "attached to bug reports.",
        internf("move the whole URL into the environment: backend %s = env(\"URL\");", backend_name));
  }

  // ---- 表达式 ----

  Ty* type_of_(Node* e) {
    if (!e) return unknown();
    switch (e->kind) {
      case Kind::LitExpr: {
        // parse_lit 对无插值字符串把**内容**写进 text（n_subs == 0），
        // 对有插值字符串把**类型标记**写进 text、内容写进 subs。
        // 这里只按类型标记判断，不看 text 的内容。
        const char* t = e->text ? e->text : "";
        if (strcmp(t, "string") == 0) return mk(Ty::Str);
        if (strcmp(t, "int") == 0) return mk(Ty::Int);
        if (strcmp(t, "double") == 0) return mk(Ty::Double);
        if (strcmp(t, "bool") == 0) return mk(Ty::Bool);
        if (strcmp(t, "cost") == 0) return mk(Ty::Cost);
        return unknown();
      }

      // `unlimited` 是**上界标记**，不是一个值。唯一合法位置是
      // `model.agent(max_steps = unlimited)`，而那个位置根本不经过这里
      // （codegen 直接读参数节点）。所以走到类型检查的 `unlimited` 一定是被当成了
      // 值用 —— `auto x = unlimited;`、`f(unlimited)`、`unlimited + 1`。
      //
      // 报错而不是返回一个类型：给它一个类型（比如 int）等于承认这写法有含义，
      // 而它没有任何运行期表示 —— 编得出 IR 才是更坏的结果。
      // 返回 unknown() 是为了让上层（return 类型统一、字段访问）继续走成
      // "未知类型"，不在已经报过错之后崩在空指针上。
      case Kind::UnlimitedExpr: {
        err("E4024",
            "`unlimited` is not a value: it can only appear in the upper bound position of `model.agent(max_steps = unlimited)`",
            e->loc,
            "`unlimited` means \"this loop has no compile-time upper bound\"; it is not a constant you can assign to a variable. "
            "Allowing it in a value position would let someone write auto x = unlimited; x would then have no "
            "runtime representation, and that IR would become an absurd step count in the executor.",
            "Write the upper bound in place: model.agent(goal = \"...\", max_steps = unlimited).",
            "For \"run until a condition is met\", use a while loop with an explicit condition, not a constant placeholder.");
        return unknown();
      }

      case Kind::IdentExpr: {
        Ty* t = lookup_(e->text);
        if (t) return t;
        for (size_t i = 0; i < closed_.size(); ++i) {
          if (closed_[i] && e->text && strcmp(closed_[i], e->text) == 0) {
            err("E5002",
                internf("%s is declared inside a `parallel` block and is not visible here", e->text),
                e->loc,
                "Each branch of `parallel` runs concurrently, so a value produced inside the block "
                "does not exist as far as the code after it is concerned — the block's own "
                "`return [...]` is what carries results out. Referencing it here would read a slot "
                "that no branch owns.",
                "read the result from the block instead: `auto [a, b] = parallel { ... };`");
            return unknown();
          }
        }
        // E4028：循环变量出循环即回收。lookup_ 找不到、但 for_closed_ 命中，
        // 说明这是"已结束的 for 的循环变量"，不是"从没定义"——报前者而不是
        // 静默放过，否则 `for(...){} auto x = i;` 能把循环变量带出循环。
        for (size_t i = 0; i < for_closed_.size(); ++i) {
          if (for_closed_[i] && e->text && strcmp(for_closed_[i], e->text) == 0) {
            err("E4028",
                internf("`%s` is the loop variable of a `for` loop that has already ended", e->text),
                e->loc,
                "The loop variable is owned by the `for` header and exists only inside the loop body; its slot is reclaimed once the loop ends. "
                "Referencing it outside the loop reads a variable that no longer exists, which is undefined behavior at runtime.",
                "Store the value you need outside the loop in a variable declared before the loop: "
                "`auto last = 0; for (...) { last = i; }`.");
            return unknown();
          }
        }
        // 从没定义：可能是别的 .h 里的声明，单文件阶段不报。
        return unknown();
      }

      case Kind::PathExpr: {
        if (!e->subs || e->n_subs < 1) return unknown();
        Ty* cur = type_of_(&e->subs[0]);
        // `type_of_` 不认识某个表达式时会返回 nullptr（"暂时未知"）。
        // 不挡住它，下面第一句 `cur->k` 就是空指针解引用 —— 而症状是**编译器崩溃**，
        // 不是一条诊断，用户拿到的只有一个 SIGSEGV。
        if (!cur) return unknown();
        for (int i = 1; i < e->n_subs; ++i) {
          if (!cur) return unknown();  // 上一轮可能把 cur 置空（防御性，见下方各处）
          Node& f = e->subs[i];
          // 错误对象的第一段字段（§7.1.1）。必须放在所有 StructK / HttpResK
          // 判定**之前**：错误绑定的类型是 unknown，走到下面只会得到
          // "cannot access field status on unknown" —— 而那条消息在说"没有这个
          // 字段"，与"字段存在但运行期还没带上"是两件完全不同的事。
          //
          // 只在 i == 1 时接管：`e.status.foo` 的第二段是"对 int 取字段"，
          // 交给下面既有的 E4016 分支，语义正确且不用在这里再写一遍。
          if (i == 1 && e->subs[0].kind == Kind::IdentExpr) {
            const char* et = err_binding_type_(e->subs[0].text);
            if (et) {
              Ty* ft = err_field_(et, f);
              if (!ft) return unknown();
              cur = ft;
              continue;
            }
          }
          if (cur->k == Ty::Json) continue;  // json 任意字段都合法，类型仍是 json
          // `httpresult<T>` 是 {status, body, headers} 三元组（LANGUAGE §5.6）——
          // 它就是 allow_status 存在的意义：调用方要能写 `if (r.status == 404)`。
          // 不认这三个字段，allow_status 的返回值就没法用。
          if (cur->k == Ty::HttpResK) {
            const char* fname = f.text ? f.text : "";
            if (std::string(fname) == "status") { cur = mk(Ty::Int); continue; }
            if (std::string(fname) == "headers") { cur = mk(Ty::Json); continue; }
            // body 的元素类型目前用 elem 承载（与 array 共用同一个槽位）。
            if (std::string(fname) == "body") { cur = cur->elem ? cur->elem : mk(Ty::Json); continue; }
            err("E4016", internf("httpresult has no field %s", fname), f.loc,
                "HttpResult<T> has exactly status, body and headers.",
                "use r.status / r.body / r.headers");
            return unknown();
          }
          if (cur->k != Ty::StructK) {
            err("E4016",
                internf("cannot access field %s on %s", f.text ? f.text : "?", type_name_(cur)),
                f.loc,
                "Only struct and json values have fields. Accessing a field on another type "
                "almost always means the variable holds something other than you expect.",
                internf("check what %s is: it is %s here", f.text ? f.text : "?", type_name_(cur)));
            return unknown();
          }
          Ty* ft = struct_field_(cur->struct_idx, f.text ? f.text : "");
          if (!ft) {
            const char* sname = (cur->struct_idx >= 0) ? structs_[cur->struct_idx].name : "?";
            const char* fname = f.text ? f.text : "?";
            // M1：字段名拼错 → 给最近的字段名（did-you-mean / 语法纠正）。
            // 人类可读提示进 fix2（保持既有 fix[] 的完整候选列表），
            // 机器可应用编辑进 edit_*（M2）——span=拼错的标识符位置，replacement=建议名。
            const char* guess = nearest_field_(cur->struct_idx, fname);
            if (guess) {
              const char* dym = internf("did you mean `%s`? (other fields: %s)",
                                        guess, list_fields_(cur->struct_idx));
              err("E4016", internf("struct %s has no field %s", sname, fname), f.loc,
                  "Field access is checked against the struct declaration. A misspelled or removed "
                  "field would otherwise become `json null` at run time, and the workflow would keep "
                  "running with a silently missing value.",
                  internf("%s has: %s", sname, list_fields_(cur->struct_idx)),
                  dym,
                  guess, f.loc.line, f.loc.col, f.loc.end_line, f.loc.end_col);
            } else {
              err("E4016", internf("struct %s has no field %s", sname, fname), f.loc,
                  "Field access is checked against the struct declaration. A misspelled or removed "
                  "field would otherwise become `json null` at run time, and the workflow would keep "
                  "running with a silently missing value.",
                  internf("%s has: %s", sname, list_fields_(cur->struct_idx)));
            }
            return unknown();
          }
          // W5004 的素材在这里天然齐备：`cur` 是字段的**宿主类型**，`f.text` 是字段名，
          // 而且这段代码已经在类型检查里，不需要为了拿类型再递归一次 type_of_
          // （再递归会把 E4016 这类诊断报两遍）。
          //
          // 只记**最后一段**：`o.status` 记，`o.status.code` 里的 status 不记 ——
          // 后者比的是一个子字段，与 HTTP 状态码无关。宿主必须是用户 struct，
          // 这条把 HttpResult<T>（正确用法）与错误对象（unknown）都放行。
          if (ft && i == e->n_subs - 1 && is_status_field_name_(f.text)) {
            note_status_field_(e);
          }
          cur = ft;
        }
        return cur;
      }

      case Kind::ArrayLit: {
        // 定长数组：元素类型取第一个元素的类型（全同是常态）；空数组是 json 数组。
        Ty* arr = mk(Ty::ArrayK);
        if (e->child) {
          Ty* first = type_of_(e->child);
          arr->elem = (first && first->k != Ty::Unknown) ? first : mk(Ty::Json);
          for (const Node* c = e->child->next; c; c = c->next) {
            type_of_(const_cast<Node*>(c));  // 仍然逐个检查，避免漏掉内部错误
          }
        }
        return arr;
      }

      case Kind::IndexExpr: {
        Ty* base = e->child ? type_of_(e->child) : nullptr;
        if (e->text != nullptr) {
          // `j["k"]`：json 常量键，结果仍是 json
          return mk(Ty::Json);
        }
        // `xs[i]`：数组下标，结果是元素类型。基类型不是数组就报错 ——
        // 否则"对字符串取下标"这类写法会一路走到运行期。
        if (base == nullptr) return mk(Ty::Json);
        if (base->k == Ty::ArrayK) {
          if (e->args) {
            Ty* it = type_of_(e->args);
            if (it && it->k != Ty::Int && it->k != Ty::Unknown) {
              err("E4031", "array index must be an integer", e->loc,
                  "Indexing an array with a non-integer can never select an element.",
                  "use an int expression");
            }
          }
          return base->elem ? base->elem : mk(Ty::Json);
        }
        if (base->k == Ty::Json) return mk(Ty::Json);
        err("E4032", "cannot index a value that is not an array", e->loc,
            "Only arrays and json support indexing.",
            "check what the value is before indexing it");
        return mk(Ty::Json);
      }

      case Kind::BinaryExpr: {
        Ty* l = type_of_(e->child);
        Ty* r = type_of_(e->args);
        // type_of_ 在"暂时未知"时可能返回 nullptr（例如跨文件还没有声明）。
        // 直接 `l->k` 会变成**编译器崩溃**而不是一条诊断。
        if (!l || !r) return unknown();
        const char* op = e->text ? e->text : "";

        // === 比较 / 逻辑：上下文类型恒为 bool，但 §4.4 的"禁止"列要先拒 ===
        const bool is_eq = (strcmp(op, "==") == 0 || strcmp(op, "!=") == 0);
        const bool is_rel = (strcmp(op, "<") == 0 || strcmp(op, "<=") == 0 ||
                            strcmp(op, ">") == 0 || strcmp(op, ">=") == 0);
        const bool is_logic = (strcmp(op, "&&") == 0 || strcmp(op, "||") == 0);

        if (is_eq || is_rel || is_logic) {
          // 类型不可判定（任一 Unknown 或 Tuple）：交给别处或不误报。
          // Tuple 的元素类型不在 Ty 里携带（TupleK 无元素信息），所以从 parallel
          // 解构出的绑定可能暂时呈现为 tuple；此时无法判定是否跨类型，跳过而非误报。
          if (l->k != Ty::Unknown && r->k != Ty::Unknown &&
              l->k != Ty::TupleK && r->k != Ty::TupleK) {
            if (is_eq) {
              // 相等 / 不等：仅同类型（含 json==json）。跨类型 → E4010。
              if (!same_type_(l, r)) {
                err("E4010",
                    internf("cannot compare %s with %s using %s (different types)",
                            type_name_(l), type_name_(r), op),
                    e->loc,
                    "Equality and inequality require both operands to be the same type. A "
                    "cross-type comparison (e.g. an id against a count, or a struct against a "
                    "scalar) can never be true and is almost always a typo.",
                    "make both operands the same type, or convert one explicitly");
              } else if (l->k == Ty::Double) {
                // double 相等 → E4033（**硬错**，不是 W4001 警告）。
                //
                // §4.4 的禁止列原先把它写成"允许但 W4001"，而 W4001 从来没有被
                // 实现过（全仓只有文档提到它）—— 也就是说 `double == double` 一直是
                // **静默通过**。2026-11-10 拍板：升为硬错。
                //
                // 为什么不是警告：警告的语义是"能编，但建议改"，而浮点相等不是
                // 风格问题 —— 它的答案依赖最后一位，等于把"能不能证"交给运气，
                // 与 D-010.1 拒绝金额走浮点是同一条理由。语言能拒的就别留给猜。
                err("E4033",
                    internf("double %s double is not allowed: exact equality on binary "
                            "floating point is not a stable predicate", op),
                    e->loc,
                    "Floating-point equality cannot be proven at compile time, so it is "
                    "rejected rather than silently returning false. Rounding makes two values "
                    "that are mathematically equal compare unequal, and NaN is unequal to "
                    "itself: `x == x` can be false, and `x != x` quietly becomes the only NaN "
                    "test in the language. A predicate whose answer depends on the last bit is "
                    "the same class of implicit guess that D-010.1 forbids for money.",
                    "keep the value in exact units (int micro-units, or `cost`) and compare that",
                    "or compare with an explicit tolerance: use a `double` literal or a "
                    "value taken from the input, e.g. `json.as_float(input.eps)`; "
                    "equality itself is still rejected");
              } else if (l->k == Ty::Int) {
                // W5004（**启发式警告**，不是错误）：`o.status == 404`。
                // 只有 int⊗int 走到这里才可能命中；去重表在方法内部处理，
                // 因为 type_of_ 会对同一个比较反复调用。
                warn_http_status_(e);
              }
            } else if (is_rel) {
              // 关系比较：仅 int / double / cost / string。bool / json / struct → E4011。
              const bool ord =
                  (l->k == Ty::Int && r->k == Ty::Int) ||
                  (l->k == Ty::Double && r->k == Ty::Double) ||
                  (l->k == Ty::Cost && r->k == Ty::Cost) ||
                  (l->k == Ty::Str && r->k == Ty::Str);
              if (!ord) {
                err("E4011",
                    internf("cannot order-compare %s with %s", type_name_(l), type_name_(r)),
                    e->loc,
                    "Ordering comparisons (< <= > >=) are only defined for int, double, cost and "
                    "string (UTF-8 byte order). bool, json and struct have no total order, so the "
                    "comparison could never be evaluated — it would silently fall through to a "
                    "runtime ContractError on the first non-comparable value.",
                    "compare values of the same ordered type, or extract a comparable field first");
              }
            } else {  // is_logic
              // 逻辑 && ||：两侧必须 bool。无 truthiness → E4012。
              if (l->k != Ty::Bool || r->k != Ty::Bool) {
                err("E4012",
                    internf("logical %s requires bool operands, got %s and %s", op,
                            type_name_(l), type_name_(r)),
                    e->loc,
                    "&& and || are boolean connectives with no truthiness: a non-bool operand "
                    "would mean the language silently decides what counts as 'true', which is "
                    "exactly the class of implicit conversion that hides control-flow bugs.",
                    "convert the operands to bool explicitly (e.g. compare them), or use the "
                    "boolean result of a comparison");
              }
            }
          }
          return mk(Ty::Bool);  // 比较 / 逻辑上下文恒为 bool
        }

        // === 算术 + - * / %  ===
        if (l->k == Ty::Unknown || r->k == Ty::Unknown) return unknown();

        // %：仅 int。其他 → E4009。
        if (strcmp(op, "%") == 0) {
          if (l->k != Ty::Int || r->k != Ty::Int) {
            err("E4009", "operator % requires int operands", e->loc,
                "The remainder operator is only meaningful on integers. A remainder on cost, "
                "double, string, json or struct would have no well-defined result.",
                "use int operands for %");
          }
          return mk(Ty::Int);
        }

        // ---- 逐条拒绝 §4.4 禁止列（先拒，再落 loose 兜底）----
        const bool lc = (l->k == Ty::Cost), rc = (r->k == Ty::Cost);
        const bool lf = (l->k == Ty::Double), rf = (r->k == Ty::Double);
        const bool li = (l->k == Ty::Int), ri = (r->k == Ty::Int);
        const bool ls = (l->k == Ty::Str), rs = (r->k == Ty::Str);

        // E4008：cost 与 double 混合（金额不许混入浮点）。
        if ((lc && rf) || (rc && lf)) {
          err("E4008",
              internf("cost and double do not mix in arithmetic (%s %s %s)",
                      type_name_(l), op, type_name_(r)),
              e->loc,
              "cost is an integer micro-USD amount; mixing it with a double would require "
              "promoting cost to double, which reintroduces the rounding that D-010.1 exists "
              "to forbid. There is no implicit promotion in Hermes, so the operation can never "
              "be computed.",
              "keep amounts in int micro-units; if you need a double for display, convert at the "
              "json boundary with json.as_float");
          return unknown();
        }
        // E4006：int 与 double 混合（无隐式提升）。
        if ((li && rf) || (ri && lf)) {
          err("E4006",
              internf("int and double do not auto-promote (%s %s %s)",
                      type_name_(l), op, type_name_(r)),
              e->loc,
              "There is no implicit numeric promotion. Implicit int->double promotion is the same "
              "root cause as the money-rounding vulnerability (D-010.1): a value silently gains "
              "a fractional part and the budget bound stops being provable.",
              "convert the int to double explicitly (or the double to int) before the operation");
          return unknown();
        }
        // E4005：cost 与 int 的 +/-（D-010.1：把 int 当微元累加）。
        if ((lc && ri) || (rc && li)) {
          if (strcmp(op, "+") == 0 || strcmp(op, "-") == 0) {
            err("E4005",
                internf("cost cannot be added to / subtracted by a bare int (%s %s %s)",
                        type_name_(l), op, type_name_(r)),
                e->loc,
                "cost is micro-USD. Adding or subtracting a bare int treats that int as "
                "micro-units (e.g. `$1 + 1` becomes 1000001), which is the exact money-"
                "corruption bug D-010.1 outlaws. An int is only meaningful against cost as a "
                "count via * and /.",
                "multiply/divide by an int count (cost * n, cost / n), or keep money on both "
                "sides of the operator");
            return unknown();
          }
          // cost * int / cost / int 合法（int 是数量），落到下面 loose 兜底。
        }
        // E4007：string 参与算术（改用插值）。
        if (ls || rs) {
          err("E4007", internf("string cannot be used with arithmetic operator %s", op), e->loc,
              "Strings have no arithmetic. Concatenation is intentionally disallowed because it "
              "hides the encoding and locale of the join; the only string-combining construct is "
              "interpolation `{...}`.",
              "use interpolation: `\"...{expr}...\"`");
          return unknown();
        }

        // ---- loose 兜底（与既有行为一致：cost*cost / cost/cost 等未列入 §4.4 禁止列的
        //      组合仍按原样推导，不在此轮收紧范围）----
        if (l->k == Ty::Cost || r->k == Ty::Cost) return mk(Ty::Cost);
        if (l->k == Ty::Double || r->k == Ty::Double) return mk(Ty::Double);
        if (l->k == Ty::Int && r->k == Ty::Int) return mk(Ty::Int);
        if (l->k == Ty::Str && r->k == Ty::Str) return mk(Ty::Str);
        return unknown();
      }

      case Kind::UnaryExpr: {
        Ty* v = type_of_(e->child);
        const char* op = e->text ? e->text : "";
        // 未知类型交给别处诊断，不误报。
        if (!v || v->k == Ty::Unknown) {
          return (strcmp(op, "!") == 0) ? mk(Ty::Bool) : (v ? v : unknown());
        }
        if (strcmp(op, "!") == 0) {
          // E4012：逻辑非只接受 bool（无 truthiness）。
          if (v->k != Ty::Bool) {
            err("E4012",
                internf("logical ! requires a bool operand, got %s", type_name_(v)),
                e->loc,
                "! applies to bool and the language has no truthiness; negating int/string/json is undefined, "
                "and letting it through would quietly enable implicit rules like \"nonzero means true\", while still failing at runtime.",
                "Write the operand as a comparison (e.g. `!(x != 0)`) to get a bool.");
          }
          return mk(Ty::Bool);
        }
        // 一元负号：只对 int / cost / double 有意义。
        if (v->k != Ty::Int && v->k != Ty::Cost && v->k != Ty::Double) {
          err("E4007",
              internf("unary - requires a numeric operand (int/cost/double), got %s", type_name_(v)),
              e->loc,
              "Unary minus is arithmetic and is only defined for numeric types; negating string / json / bool has no result.",
              "Narrow to a numeric type before negating (e.g. json.as_int(x)).");
        }
        return v;
      }

      case Kind::CallExpr: {
        Node* callee = e->child;
        // 参数先全走一遍，保证里面的字段错误不会因为外层提前 return 被漏掉。
        // 注意 args 是 Param 节点（命名参数 `prompt = "x"`），不是表达式链。
        for (Node* arg = e->args; arg; arg = arg->next) type_of_(arg);

        // E3016：用了 tool.call / model.generate 却没链对应的 std 模块。
        // 这是**单文件**能判定的事（#link 就写在同一个文件里）。
        const char* cname = path_text(callee);
        if (cname) {
          if (strcmp(cname, "tool.call") == 0 && !has_std_net_ && !reported_net_) {
            reported_net_ = true;
            err("E3016", "tool.call needs #link <std/net>", e->loc,
                "A std module's declarations come from #include, but its implementation comes from "
                "#link. Using the call without linking it leaves the symbol unresolved at link "
                "time — which is a build failure much later than this line.",
                "add `#link <std/net>;` at the top of the file");
          }
          if (strcmp(cname, "model.generate") == 0 && !has_std_model_ && !reported_model_) {
            reported_model_ = true;
            err("E3016", "model.generate needs #link <std/model>", e->loc,
                "A std module's declarations come from #include, but its implementation comes from "
                "#link. Using the call without linking it leaves the symbol unresolved at link "
                "time — which is a build failure much later than this line.",
                "add `#link <std/model>;` at the top of the file");
          }
        }
        // **先看类型实参，再看被调者名字**：`<T>` 本身就是权威的类型来源，
        // 不依赖把 `model.generate` 拼成字符串（保留命名空间的 token 不保证带文本）。
        // 泛型调用的类型实参在 **child 链**上（`PathExpr` → `Type` → `Param`…），
        // 不在 `type_args` 槽位里 —— 这一点 codegen 早就用 children_of 处理过，
        // 类型检查这里之前没跟上，于是 `tool.call<T>` 的类型成了 unknown。
        // 子节点可能在 **child 链**、**type_args 链**或 **args 链**上
        // （codegen 的 children_of 就是这么收的，踩过三次）。只扫 child 链
        // 会让 `tool.call<T>()` 的类型实参看不见，类型退化成 unknown。
        Ty* inner = nullptr;
        bool hasAllow = false;
        for (const Node* chain : {e->child, e->type_args, e->args}) {
          for (const Node* c = chain; c; c = c->next) {
            if (c->kind == Kind::Type && !inner) inner = struct_from_type_args_(const_cast<Node*>(c));
            if (c->kind == Kind::Param && c->text && strcmp(c->text, "allow_status") == 0) hasAllow = true;
          }
        }
        if (inner) {
          // `allow_status` 非空时返回 **HttpResult<T>**（LANGUAGE §5.6）：
          // 没有它，调用方写 `r.status` 会得到 "cannot access field status on unknown"，
          // 而 allow_status 的全部意义就是让调用方能看 status。
          if (hasAllow) {
            Ty* h = mk(Ty::HttpResK);
            h->elem = inner;
            h->struct_idx = -1;
            return h;
          }
          return inner;
        }
        if (e->type_args) return struct_from_type_args_(e->type_args);
        // ---- 2.0.1：memory 事务 -------------------------------------------
        // checkpoint() 产出 Snapshot；rollback/commit 只接受 Snapshot。
        // Snapshot 是**不透明**的：它不能进 json、不能跨 workflow 传递，所以
        // 用独立类型而不是 json，任何"当成普通值处理"的写法都会在 Sema 处失败。
        if (cname && strncmp(cname, "memory.", 7) == 0) {
          if (strcmp(cname, "memory.checkpoint") == 0) {
            return mk(Ty::Snapshot);
          }
          if (strcmp(cname, "memory.rollback") == 0 || strcmp(cname, "memory.commit") == 0) {
            Node* first = nullptr;
            for (Node* arg = e->args; arg; arg = arg->next) { first = arg; break; }
            // 实参节点是 Param（`memory.rollback(snap)` 的 `snap` 在 child 上），
            // 直接 type_of_(Param) 拿到的是 unknown —— 必须取下挂的表达式。
            // 实参可能被包成 Param：值的落点随语法形态不同 —— 命名实参在
            // args，位置实参在 child。两处都看，别只赌一种（这里已经错一次）。
            if (first && first->kind == Kind::Param) {
              first = first->child ? first->child : first->args;
            }
            Ty* at = first ? type_of_(first) : nullptr;
            if (!at || at->k != Ty::Snapshot) {
              err("E4102", internf("%s takes a snapshot, got %s", cname + 7,
                                   at ? type_name_(at) : "nothing"), e->loc,
                  "A snapshot is the opaque handle returned by memory.checkpoint(). Anything else "
                  "(a json value, an int, a struct) is not a transaction boundary, and committing "
                  "it would silently do nothing.",
                  "pass the value you got from memory.checkpoint()");
            }
            return mk(Ty::Void);
          }
        }

        if (cname && strncmp(cname, "json.", 5) == 0) {
          if (strcmp(cname, "json.as_string") == 0) return mk(Ty::Str);
          if (strcmp(cname, "json.as_int") == 0) return mk(Ty::Int);
          if (strcmp(cname, "json.as_float") == 0) return mk(Ty::Double);
          if (strcmp(cname, "json.as_bool") == 0) return mk(Ty::Bool);
          if (strcmp(cname, "json.as_array") == 0) return mk(Ty::ArrayK);
          return mk(Ty::Json);
        }

        // ---- 用户函数调用的实参类型检查（E4030）----
        // 这是 type_of_ 的一部分：每次 type_of_(e) 被调用都可能走到这里，而同一
        // CallExpr 节点会被反复 type_of_（比较两侧 / return 统一 / 字段访问…），
        // 用 checked_calls_ 去重，避免一处调用报多条（与 scan_float_ 同因）。
        // 元数不一致由 codegen 的 E_IR_CODEGEN 报，这里只在元数一致时比类型，
        // 避免与装配期诊断重复、也避免越界。
        if (cname) {
          Node* fn = find_fn_(cname);
          if (fn && fn->type_args && fn->type_args->text &&
              strcmp(fn->type_args->text, "void") == 0) {
            // 返回 void 的用户函数：调用本身是语句，不产出值。
            return mk(Ty::Void);
          }
          if (fn) {
            bool seen = false;
            for (const Node* s : checked_calls_) if (s == e) { seen = true; break; }
            if (!seen) {
              checked_calls_.push_back(e);
              // 形参声明类型 + 名字（按声明顺序）。
              std::vector<Ty*> expect;
              std::vector<const char*> expectName;
              for (Node* p = fn->child; p; p = p->next) {
                if (p->kind != Kind::Param) continue;
                expect.push_back(p->type_args ? from_type_(p->type_args) : nullptr);
                expectName.push_back(p->text ? p->text : "");
              }
              // 实参表达式类型（位置参数；命名参数 codegen 已拒）。
              std::vector<Ty*> got;
              for (Node* a = e->args; a; a = a->next) {
                if (a->kind != Kind::Param) continue;
                std::vector<const Node*> vals = children_of_(a);
                if (vals.empty()) continue;
                got.push_back(type_of_(const_cast<Node*>(vals[0])));
              }
              if (expect.size() == got.size()) {
                for (size_t i = 0; i < expect.size(); ++i) {
                  Ty* exp = expect[i];
                  Ty* g = got[i];
                  if (!exp || !g) continue;                                   // 形参类型缺失：交给别处
                  if (g->k == Ty::Unknown || exp->k == Ty::Unknown) continue;  // 实参已报错 / 无法判定
                  if (!same_type_(exp, g)) {
                    err("E4030",
                        internf("argument %zu to `%s` has type %s, but parameter `%s` expects %s",
                                i + 1, cname, type_name_(g), expectName[i], type_name_(exp)),
                        e->loc,
                        "A call passes a value of one type where the function declares the parameter "
                        "as another. The language has no implicit argument conversion, so the mismatch "
                        "would only surface as a runtime ContractError, or as a silently wrong value. "
                        "Catching it at compile time keeps the type system meaningful.",
                        "pass a value of the declared parameter type, or change the parameter's declared type");
                  }
                }
              }
            }
            // 返回函数的声明返回类型（此前一律 unknown）；缺失则退回 unknown。
            if (fn->type_args) return from_type_(fn->type_args);
          }
          // ---- extern（C ABI，call_native）----
          // extern 声明不入 fn_table_，这里单独解析。命中则执行实参类型检查
          // （E4030 风格）与 #link native 存在性（E3031），并返回声明的返回
          // 类型（交由下游类型推导）。§3.1 闭集（E3030）与纯 C 头（E3032）
          // 是声明/指令级，统一由 run() 的 check_extern_declarations_() 负责。
          const Node* ex = find_extern_(cname);
          if (ex) {
            bool seen = false;
            for (const Node* s : checked_calls_) if (s == e) { seen = true; break; }
            if (!seen) {
              checked_calls_.push_back(e);
              // §3.1 闭集（E3030）已由 run() 里的 check_extern_declarations_()
              // 在**声明级**统一检查 —— 这里不再重复，否则被调用的 extern
              // 会收到两条同样的 E3030，且未调用的 extern 反而漏检。
              // ① 实参类型检查（复用 E4030 风格，比对声明形参类型）。
              std::vector<Ty*> expect;
              for (Node* p = ex->child; p; p = p->next) {
                if (p->kind != Kind::Param) continue;
                const Node* pt = extern_param_type_(p);
                expect.push_back(pt ? from_type_(const_cast<Node*>(pt)) : nullptr);
              }
              std::vector<Ty*> got;
              for (Node* a = e->args; a; a = a->next) {
                if (a->kind != Kind::Param) continue;
                std::vector<const Node*> vals = children_of_(a);
                if (vals.empty()) continue;
                got.push_back(type_of_(const_cast<Node*>(vals[0])));
              }
              if (expect.size() == got.size()) {
                for (size_t i = 0; i < expect.size(); ++i) {
                  Ty* exp = expect[i];
                  Ty* g = got[i];
                  if (!exp || !g) continue;
                  if (g->k == Ty::Unknown || exp->k == Ty::Unknown) continue;
                  if (!same_type_(exp, g)) {
                    err("E4030",
                        internf("argument %zu to `%s` has type %s, but the extern parameter expects %s",
                                i + 1, cname, type_name_(g), type_name_(exp)),
                        e->loc,
                        "An extern call passes a value of one type where the C function declares "
                        "the parameter as another. The language has no implicit argument conversion, "
                        "so the mismatch would surface as a silently wrong C value.",
                        "pass a value of the declared parameter type, or change the extern signature");
                  }
                }
              }
              // ② #link native 存在性（E3031）。E3032 是**指令级**检查，
              // 已在 check_extern_declarations_() 里对每条 #link native 跑过。
              check_extern_link_(ex, cname, e);
            }
            // 返回 extern 声明的返回类型（缺失则 unknown，交给下游）。
            return ex->type_args ? from_type_(ex->type_args) : unknown();
          }
        }
        return unknown();
      }

      case Kind::ParallelExpr: return parallel_(e);
      case Kind::FallbackExpr: return fallback_(e);
      default: return unknown();
    }
  }

  // ---- E5010 的独立写语句子树扫描 ----------------------------------------
  //
  // E5010 的**唯一命中点**在这里，由最外层 `parallel_()` 发起一次，覆盖所有
  // 含 `parallel` 的位置（含藏在 for / while / match 臂 / retry 体里的嵌套
  // `parallel`）。为什么不放到 stmt_() 的 AssignStmt / UpdateStmt case 里判：
  // 阶段 B 起 stmt_() 会下潜进这四类体，`block_()` 也会走到 parallel 体内的
  // 顶层写；两处都判就会对同一条写各报一条 E5010（重复诊断）。让独立扫描
  // 做唯一命中点，判据与两轮 QA 验证过的行为都不动。
  //
  // 扫描只找 AssignStmt / UpdateStmt，只调 check_parallel_write_() ——
  // 不调 type_of_、不触发任何其它诊断。判据仍是「目标绑定下标 >= 最内层
  // parallel 水位」，由扫描器自己把 scope_ 拼出来（LetStmt 解构 / for 循环
  // 变量 / catch 变量），check_parallel_write_() 一个字都不用改。
  //
  // 覆盖 Node 的**全部**子节点槽位（对照 ast.h::Node 逐个核对）：
  //   child / args / body / else_branch / type_args / subs[0..n_subs) / next 链。
  // 逐个槽位猜会漏 —— codegen.cpp 的 children_of 注释为这件事记过教训
  //（PathExpr 的段在 subs、ParallelReturn 的值在 subs、if 的条件在 args）。

  // 递归一条 next 兄弟链。
  void scan_seq_(Node* n) {
    for (Node* s = n; s; s = s->next) scan_one_(s);
  }

  // 递归一个节点的全部子槽位。next 由 scan_seq_ 负责，这里不重复走。
  void scan_slots_(Node* s) {
    if (!s) return;
    scan_seq_(s->child);
    scan_seq_(s->args);
    scan_seq_(s->body);
    scan_seq_(s->else_branch);
    scan_seq_(s->type_args);
    for (int i = 0; i < s->n_subs; ++i) scan_seq_(&s->subs[i]);
  }

  // 往 scope_ 里放一个只填 name 的绑定。E5010 判据只看 name 与下标，
  // 类型用不上（loc 仍填上，保持 VarSym 的字段纪律）。
  void scan_bind_(const char* name, Loc loc) {
    if (!name) return;
    VarSym v;
    v.name = name;
    v.t = nullptr;
    v.loc = loc;
    scope_.push_back(v);
  }

  // LetStmt 的绑定名在 Pattern.child 的 IdentExpr 链上（parse_pattern）：
  // 单绑定是一个 IdentExpr，解构 `auto [a, b] = ...` 是用 next 串起来的一串。
  // 必须把**所有**名字收进来，否则写 b 会因为找不到绑定而静默漏判。
  void scan_bind_pattern_(Node* pat) {
    if (!pat || pat->kind != Kind::Pattern) return;
    for (Node* id = pat->child; id; id = id->next) {
      if (id->kind == Kind::IdentExpr) scan_bind_(id->text, id->loc);
    }
  }

  void scan_one_(Node* s) {
    if (!s) return;
    switch (s->kind) {
      case Kind::AssignStmt:
      case Kind::UpdateStmt:
        // 源码里仅有的两种"写绑定"语句。右值（AssignStmt.args）照常递归：
        // 表达式里不会有 AssignStmt，但可能有嵌套 ParallelExpr。
        check_parallel_write_(s);
        scan_slots_(s);
        return;

      case Kind::LetStmt: {
        // 名字先入作用域再扫 init（含 init 就是嵌套 ParallelExpr 的情形）：
        // init 所在的嵌套 parallel 会把这些名字视作体外绑定，符合
        //「下标 >= 最内层水位」这一既有判据。
        scan_bind_pattern_(s->child);
        scan_slots_(s);  // child = Pattern（无写）+ args = init
        return;
      }

      case Kind::ForStmt: {
        // 循环变量归 for 所有：它在 s->text，进作用域后体内对它的写会解析
        // 到这个（>= 水位）绑定，不会被误判成写体外变量。
        //
        // 步进 `i++` 被 parse_for 折成 s->i64（int64 delta），**不是 AST
        // 节点、不是 AssignStmt/UpdateStmt** —— 没有可扫的 step 槽位，这里
        // 显式不合成任何写。s->child 是 init、s->args 是 cond，都是表达式。
        const size_t base = scope_.size();
        scan_bind_(s->text, s->loc);
        scan_slots_(s);  // child / args / body / else_branch / type_args / subs
        scope_.resize(base);   // 循环变量只活在 for 里
        return;
      }

      case Kind::CatchClause: {
        // catch (Error e) 的绑定在 child（IdentExpr.text），只在自己的
        // else_branch（catch 体）里活着 —— 与 stmt_() 的 TryStmt 一致。
        const size_t base = scope_.size();
        if (s->child && s->child->kind == Kind::IdentExpr) {
          scan_bind_(s->child->text, s->child->loc);
        }
        scan_slots_(s);
        scope_.resize(base);
        return;
      }

      case Kind::ParallelExpr: {
        // 嵌套 parallel：压入新水位（= 当前 scope_.size()），只看最内层。
        // 外层扫描递归到这里一次，内层 parallel_() 因「不是最外层」而不再
        // 发起扫描，于是同一条写只被判一次（不会 2 条变 4 条）。
        const size_t inner_base = scope_.size();
        parallel_marks_.push_back(inner_base);
        scan_slots_(s);  // child = 块语句列表
        parallel_marks_.pop_back();
        scope_.resize(inner_base);
        return;
      }

      default:
        scan_slots_(s);
        return;
    }
  }

  // 由最外层 parallel_() 调一次：记下进入时的作用域，扫完复原。
  // 扫描期间 push 的绑定（LetStmt / for / catch）都是临时拼出来的，
  // 不能泄漏给后面的 block_() 类型检查。
  void scan_parallel_writes_(Node* root) {
    const size_t base = scope_.size();
    scan_seq_(root);
    scope_.resize(base);
  }

  // parallel_ 处理 `parallel { ... }`：块内变量在出块时**关闭**，体内写体外
  // 绑定被**硬拒**（E5010）。
  //
  // 这是 E5002 与 E5010 共同的落点。记住进入时的作用域水位，出块后把新增的
  // 名字记进 closed_ 再截断 —— 于是块外引用它们时 lookup_ 失败、closed_ 命中，
  // 报出的是"出了块"而不是"从没定义"。
  //
  // **E5010 必须落在源码 AST、不能落 codegen**：codegen 看到的是内联之后的 IR，
  // 路线 A/B 内联会给被内联的函数体合成指向 `__hm_inline_r_N` / `__hm_inline_l_N`
  // 的 `assign` 节点。在 IR 上判"目标必须声明在本分支内"，会把
  // `auto v = pick(3);`（pick 是 if/return 决策树）这种用户一行赋值都没写的
  // 正常源码判错。源码 AST 里的 `CallExpr` 只是个表达式，`assign` 根本不存在。
  Ty* parallel_(Node* e) {
    const size_t mark = scope_.size();
    // 只有最外层 parallel 发起扫描：扫描器自己会带新水位递归进嵌套
    // ParallelExpr 的子树，因此一层扫描就覆盖了所有含 `parallel` 的位置
    // （含藏在 for/while/match 臂/retry 体里的嵌套 parallel）。若每层
    // parallel 都各发起一次，同一条写就会被内外两层各扫一遍、E5010 翻倍。
    // 这与 stmt_() 是否下潜无关：下潜后 block_() 路径仍不判 E5010。
    const bool outermost = parallel_marks_.empty();
    parallel_marks_.push_back(mark);
    if (outermost) scan_parallel_writes_(e->child);
    block_(e->child);
    parallel_marks_.pop_back();
    for (size_t i = mark; i < scope_.size(); ++i) closed_.push_back(scope_[i].name);
    scope_.resize(mark);
    return mk(Ty::TupleK);
  }

  // ---- fallback：错误边界（不是异常）--------------------------------------
  //
  // `main_expr fallback { ... }` 的类型就是 **main_expr 的类型**：降级块顶替
  // 的是值，不是控制流。三条约束都在这里判：
  //   1. 块里每个 `return e` 的类型必须等于 main_expr 的类型（E4025）；
  //   2. 块里不许给 const 绑定重新赋值（E4026）；
  //   3. 块可以读外部变量（读走 IdentExpr，不经这里，天然合法）。
  //
  // 为什么类型必须一致：不一致的话，降级路径只在**真的出错那天**才会暴露
  // 类型错误 —— 那正是错误边界最不该有的性质。正常路径测试全绿，线上第一次
  // 超时才发现 fallback 返回了 string 而调用方按 int 用。
  Ty* fallback_(Node* e) {
    Ty* t = type_of_(e->child);          // main_expr
    if (!t) return unknown();
    Node* body = e->args;                // Block
    if (!body) return t;

    // const 写检查必须在 block_() **之前**：此时 scope_ 里还是外层绑定。
    // 进去之后再查，块内新声明的同名变量会遮蔽外层 const，漏判。
    check_fallback_writes_(body->child);

    const size_t base = scope_.size();
    const int saved = fallback_depth_;
    fallback_depth_ = saved + 1;
    // 降级块里的 `return e` 是**从块里返回值**，不是从 workflow 返回。
    // 不掐掉 declared_ret_，块里的 return 会被 stmt_() 的 ReturnStmt case
    // 拿去和 workflow 的声明返回类型比，冒出 E4021 —— 而那个 return 根本
    // 不结束 workflow。E4025 在下面按 main_expr 的类型单独判。
    Ty* saved_ret = declared_ret_;
    declared_ret_ = nullptr;
    block_(body);
    declared_ret_ = saved_ret;
    fallback_depth_ = saved;
    scope_.resize(base);

    // `return` 的值在 **child**（与 stmt_() 的 ReturnStmt case 同款，
    // 别读 args —— 读 args 会恒得 nullptr，E4025 就永远不报）。
    //
    // 只扫块的一级语句：嵌套块（if / match 臂里的 return）由各自的
    // type_of_ / block_ 递归覆盖，这里重复扫只会产生重复诊断。
    for (Node* s = body->child; s; s = s->next) {
      if (s->kind != Kind::ReturnStmt) continue;
      Ty* rt = s->child ? type_of_(s->child) : nullptr;
      // 跳过**只针对 Unknown**：Unknown 的意思是"不知道类型"，此时报 E4025
      // 是假阳性 —— 典型例子是 `model.generate(prompt = "hi")` 没写 `<T>`
      // 时走 `return unknown()`，而规格里的示例 1 必须编得过。
      //
      // 但这条跳过**绝不能扩大**：两侧都是确定类型时必须比对。若哪天有人
      // 把这里改成"任一侧未知就整体不检查"或者干脆删掉下面的 same_type_，
      // `auto y = 1 fallback { return "s"; };` 就会静默通过 ——
      // TestTypeCheckRejectsFallbackReturnTypeMismatch 就是钉死这一点的钉子。
      if (!rt || rt->k == Ty::Unknown || t->k == Ty::Unknown) continue;
      if (!same_type_(rt, t)) {
        err("E4025", "fallback return type does not match the main expression", s->loc,
            "A fallback return value replaces the main expression's value, so the two types must match, "
            "otherwise the degraded path would only reveal a type error on the day something actually fails.",
            "Make the return value the same type as the main expression, or convert it explicitly.",
            "If the degraded path really must return another type, convert the whole fallback result to the main expression's type first.");
      }
    }
    return t;
  }

  // E4026：fallback 块内给 const 绑定重新赋值。
  //
  // 与 E5010 的区别要说清：E5010 是「写下去**不生效**」（parallel 分支拿到
  // 副本）；这里写入**会生效**，只是违反 const 契约。所以文案不能说"无效"。
  void check_fallback_writes_(Node* n) {
    for (Node* s = n; s; s = s->next) {
      if ((s->kind == Kind::AssignStmt || s->kind == Kind::UpdateStmt) && s->text) {
        for (int i = static_cast<int>(scope_.size()) - 1; i >= 0; --i) {
          if (!scope_[i].name || strcmp(scope_[i].name, s->text) != 0) continue;
          if (scope_[i].is_const) {
            err("E4026", "cannot reassign a const variable inside a fallback block", s->loc,
                "A const binding is read-only throughout its scope; the degraded path is no exception.",
                "Write the result to a new variable, or drop const.",
                "If the degraded path needs to change state, have it return a new value and let the caller rebind it.");
          }
          // 取最内层同名绑定（与 lookup_ 一致），不再往外找。
          break;
        }
      }
      check_fallback_writes_(s->child);
      check_fallback_writes_(s->args);
      check_fallback_writes_(s->body);
      check_fallback_writes_(s->else_branch);
      check_fallback_writes_(s->type_args);
      for (int i = 0; i < s->n_subs; ++i) check_fallback_writes_(&s->subs[i]);
    }
  }

  // E5010：`parallel` 体内写「体外绑定」。
  //
  // 判据（LANGUAGE §5.2）：目标绑定在 scope_ 中的下标 >= **最内层** parallel
  // 的水位 ⇒ 分支自己声明的，合法；< 水位 ⇒ 体外绑定，硬拒。
  //
  // 这不是"可疑写法"，是**可证无效**：运行期每个分支拿到父作用域的副本
  // （internal/engine/run.go 的 cloneFrames / runBranches），这次写入作用于副本，
  // 出分支即丢弃，恒等于零效果。只读才是 §5.2 真正成立的部分 —— 读走的是
  // IdentExpr，不经过这里，所以继续合法。
  void check_parallel_write_(Node* s) {
    if (parallel_marks_.empty()) return;  // 不在 parallel 体内：普通赋值，不管
    const char* name = s->text;
    if (!name) return;
    for (int i = static_cast<int>(scope_.size()) - 1; i >= 0; --i) {
      if (!scope_[i].name || strcmp(scope_[i].name, name) != 0) continue;
      // 与 lookup_ 一样取最内层的同名绑定；它 >= 水位就是分支自己声明的。
      if (static_cast<size_t>(i) >= parallel_marks_.back()) return;
      err("E5010",
          internf("cannot write to `%s` inside `parallel`: it is declared outside the block",
                  name),
          s->loc,
          "Each `parallel` branch runs on a copy of the enclosing scope (LANGUAGE §5.2). "
          "A write to a binding declared outside the block lands on that copy and is discarded "
          "when the branch ends — it is provably a no-op, with no error and no effect. Outer "
          "bindings being read-only is the entire reason `parallel` needs no locks and its "
          "concurrency bound is provable.",
          "carry the value out through the block's own `return [...]` and destructure it: "
          "`auto [v] = parallel { ... return [expr]; };` then use `v` after the block",
          "or declare a fresh binding inside the branch if the value is only needed there: "
          "`auto local = expr;`");
      return;
    }
    // 没找到绑定：可能是别的 .h 里的声明，也可能拼错。单文件检查器看不到别的
    // 文件，这里不猜（与 IdentExpr 的「从没定义」同一条纪律）—— 猜了会误报
    // 每个正常的多文件工程。
  }

  // struct_from_type_args_ 处理 model.generate<Intent>(...)：类型实参必须是 struct。
  Ty* struct_from_type_args_(Node* ta) {
    if (!ta) return mk(Ty::Json);
    const char* s = ta->text ? ta->text : "";
    int si = find_struct_(s);
    if (si < 0) return unknown();  // 单文件看不到别的 .h 里的 struct，不报错
    Ty* t = mk(Ty::StructK);
    if (t) t->struct_idx = si;
    return t;
  }

  // path_text 把 `model.generate` 这样的 PathExpr 拼回点分字符串。
  const char* path_text(Node* e) {
    if (!e) return nullptr;
    if (e->kind == Kind::IdentExpr) return e->text;
    if (e->kind != Kind::PathExpr || !e->subs || e->n_subs < 1) return nullptr;
    char buf[256];
    int used = 0;
    buf[0] = '\0';
    for (int i = 0; i < e->n_subs; ++i) {
      Node& p = e->subs[i];
      if (!p.text) continue;
      int n = snprintf(buf + used, sizeof(buf) - used, "%s%s", used ? "." : "", p.text);
      if (n > 0) used += n;
      if (used >= static_cast<int>(sizeof(buf)) - 2) break;
    }
    return a_.intern(buf, strlen(buf));
  }

  // ---- 语句 ----

  void block_(Node* b) {
    for (Node* s = b; s; s = s->next) stmt_(s);
  }

  void stmt_(Node* s) {
    if (!s) return;
    switch (s->kind) {
      case Kind::Block: block_(s->child); break;

      case Kind::LetStmt: {
        Ty* init = type_of_(s->args);
        require_value_(init, s->args ? s->args->loc : s->loc, "a value");
        // E4023：错误对象（catch 绑定）不能整只存进另一个变量 —— 它的生命周期
        // 只属于产生它的 catch 块（LANGUAGE §7.1.1 约束 2）。`auto s = e.status;`
        // 合法（字段是值），只有 `auto x = e;` 这种搬走错误对象本体才拒。
        // 裸标识符在 AST 里是**单段 PathExpr**，段在 `subs`（不是 child）：`auto x = e;`
        // 的 init 是 PathExpr{n_subs=1, subs=[IdentExpr "e"]}；`e.status` 是 n_subs=2。
        // 所以「错误对象本体」= PathExpr 恰有一段；多段是字段访问，放行。
        if (s->args && s->args->kind == Kind::PathExpr && s->args->n_subs == 1 &&
            s->args->subs && s->args->subs[0].text &&
            is_error_binding_(s->args->subs[0].text)) {
          const char* nm = s->args->subs[0].text;
          err("E4023", internf("cannot bind the error object `%s` to a variable", nm),
              s->args->loc,
              "An error object only lives inside the `catch` block that produced it. Copying it "
              "out would extend its lifetime past the failure it describes, so the trace's "
              "\"this error was recovered\" bookkeeping would no longer hold.",
              "read the fields you need (e.g. `e.status`) and bind those instead");
        }
        // 绑定的名字在 **Pattern.child（IdentExpr）链** 上，不在 Pattern.text 上
        // （见 parser.cpp 的 parse_pattern）：单绑定是一个 IdentExpr，解构
        // `auto [a, b] = ...` 是用 next 串起来的一串。**必须全收**：阶段 B 起
        // stmt_() 会下潜进 match 臂 / 循环体，只取第一个名字会让第二个及之后的
        // 名字漏绑，后续推导退化成 unknown，甚至冒出假诊断 ——
        // examples/support_bot 的 `auto [order, history]` 正是这种形态。
        // 收法与独立扫描器的 scan_bind_pattern_() 一致。
        Node* pat = s->child;
        if (pat && pat->kind == Kind::Pattern) {
          for (Node* id = pat->child; id; id = id->next) {
            if (id->kind != Kind::IdentExpr || !id->text) continue;
            VarSym v;
            v.name = id->text;
            v.t = init;
            v.loc = id->loc;
            v.is_const = s->is_const;
            scope_.push_back(v);
          }
          // 兜底：Pattern 上直接挂 text、child 为空的历史形态（parse_pattern
          // 的错误恢复路径）。正常路径 child 链已经收全，这里不会重复绑。
          if (!pat->child && pat->text) {
            VarSym v;
            v.name = pat->text;
            v.t = init;
            v.loc = pat->loc;
            v.is_const = s->is_const;
            scope_.push_back(v);
          }
        }
        break;
      }

      case Kind::ReturnStmt: {
        Ty* t = type_of_(s->child);
        if (s->child && is_void_(t)) {
          err("E4103", "a void call cannot be returned", s->child->loc,
              "Returning `void` would hand the caller a value that does not exist. Call it as a "
              "statement, then return an actual value.",
              "write the call on its own line");
        }
        if (!s->child && !fn_returns_void_ && in_workflow_ && declared_ret_ &&
            declared_ret_->k != Ty::Void) {
          err("E4104", "this workflow must return a value", s->loc,
              "The declared return type is not void, so a bare `return;` leaves the epilogue "
              "without a value and LLVM would emit an undefined result.",
              "return a value of the declared type");
        }
        // E4022：不能 return 一个 tuple。tuple 只由 `parallel` 产生，
        // 且**不可命名、不可存储**（IR.md §3.1）—— 它是 fork/join 的中间态，
        // 一旦被 return 出去，调用方拿到一个无法用语言类型描述的东西。
        if (t->k == Ty::TupleK) {
          err("E4022", "cannot return a tuple", s->loc,
              "A tuple exists only as the result of `parallel { ... }`, and it cannot be named or "
              "stored (IR.md §3.1). Returning one would hand the caller a value the type system "
              "has no way to describe — so the workflow's return contract could not be written down.",
              "destructure it first: `auto [a, b] = parallel { ... };` then return a single value");
          break;
        }
        // 数组是**合法的 JSON 值**：声明 `-> json` 的 workflow 可以返回数组。
        // 反过来不行（声明数组却返回标量），所以只放宽这一个方向。
        const bool arr_as_json =
            declared_ret_ && declared_ret_->k == Ty::Json && t && t->k == Ty::ArrayK;
        if (declared_ret_ && t->k != Ty::Unknown && !arr_as_json && !same_type_(t, declared_ret_)) {
          err("E4021",
              internf("return type mismatch: declared %s, this return gives %s",
                      type_name_(declared_ret_), type_name_(t)),
              s->loc,
              "The declared return type is the workflow's contract — it is what callers and the "
              "HTTP surface see. If the body can return two different types, the contract is "
              "wrong, not just this statement.",
              internf("make this return give %s, or change the declared type",
                      type_name_(declared_ret_)));
        }
        break;
      }

      case Kind::ExprStmt: type_of_(s->child); break;

      // `x = expr;` / `x++;` —— 源码里仅有的两种"写绑定"语句。
      //
      // E5010 不在这里判：它的**唯一命中点**是 `parallel_()` 发起的独立
      // 写语句子树扫描（scan_parallel_writes_），覆盖所有含 `parallel` 的
      // 位置。阶段 B 起 stmt_() 已下潜进 for / while / match 臂 / retry 体，
      // `block_()` 也会走到 parallel 体内的顶层写；若这里再判一次，就会和
      // 扫描器对同一条写各报一条 E5010（重复诊断）。让独立扫描保持唯一
      // 命中点，是为避免与 `block_()` 路径重复诊断，与是否走得到无关。
      // 遍历路径与「不给右值补做类型检查」的既有纪律都不变：这里仍然
      // 不调 type_of_、不触发任何其它诊断。
      case Kind::AssignStmt:
      case Kind::UpdateStmt: break;

      case Kind::IfStmt:
        // E4027：`if` 条件必须是 bool。原来完全不查，于是 `if (1)` /
        // `if (json.as_int(x))` 能编进 IR，直到执行器才因"条件非 bool"失败 ——
        // 那是在运行期才暴露、本可在编译期钉死的 bug。
        {
          Ty* cond = type_of_(s->child);
          check_cond_bool_(cond, s->child ? s->child->loc : s->loc);
        }
        block_(s->args);
        if (s->else_branch) {
          if (s->else_branch->kind == Kind::IfStmt) stmt_(s->else_branch);
          else block_(s->else_branch);
        }
        break;

      // ---- 2.0.1 控制流扩展 -------------------------------------------
      // break 只能跳出一层循环：它不能穿过 workflow 边界（那是 stop 的职责）。
      case Kind::BreakStmt:
        if (loop_depth_ == 0) {
          err("E4105", "break outside a loop", s->loc,
              "`break` leaves the innermost loop. Outside a loop there is nothing to leave, and "
              "crossing the workflow boundary is what `stop` exists for — mixing the two makes "
              "the control flow unreadable.",
              "use `workflow.stop(reason: …)` to end the workflow early");
        }
        break;
      case Kind::ContinueStmt:
        if (loop_depth_ == 0) {
          err("E4106", "continue outside a loop", s->loc,
              "`continue` jumps to the next iteration of the innermost loop; there is no loop here.",
              "remove it, or move it inside the loop");
        }
        break;
      // stop：reason 与 data 都是表达式，走同一套类型检查；两者的类型不做
      // 约束（reason 可以是 string，data 可以是任意可进 IR 的值）。
      case Kind::StopStmt: {
        if (s->child) type_of_(s->child);
        if (!in_workflow_) {
          err("E4107", "stop outside a workflow", s->loc,
              "`stop` ends the current workflow. At declaration level there is no workflow to end.",
              "move it inside a workflow body");
          break;
        }
        // data 的类型必须与声明的返回类型**严格一致**：stop 会直接写进返回位，
        // 类型不对就是 LLVM `ret` 的类型不匹配 —— 能编过、跑起来是垃圾。
        Ty* data = s->args ? type_of_(s->args) : nullptr;
        if (data && workflow_ret_ && !same_type_(data, workflow_ret_)) {
          err("E4108", "stop data does not match the workflow return type", s->args->loc,
              "`workflow.stop` returns its data to the caller, so the data has to be the same "
              "type the workflow declares. A mismatch compiles to a `ret` of the wrong type.",
              "pass a value of the declared return type");
        }
        break;
      }
      // on：槽体是 Block（child），必须下潜 —— 否则槽体里的类型错误零诊断。
      // 这条正是 TestStmtSwitchCoversEveryStatementShape 要守的东西。
      case Kind::OnStmt: {
        if (s->text && !has_signal_(s->text)) {
          err("E4109", internf("handler for undeclared signal `%s`", s->text), s->loc,
              "Binding a handler to a signal that was never declared can only be a typo; the "
              "handler would wait forever.",
              "declare it with `export signal <Name>;`");
        }
        for (const char* seen : on_signals_) {
          if (s->text && seen && strcmp(seen, s->text) == 0) {
            err("E4110", internf("duplicate handler for `%s` in the same scope", s->text), s->loc,
                "Two handlers in one scope would both be candidates, and which one runs is decided "
                "by binding order — that is not something a reader can see.",
                "merge them, or move one into a nested block");
            break;
          }
        }
        if (s->text) on_signals_.push_back(s->text);
        all_slots_.push_back({s->text, s->chan});
        // ---- 闭包转换的捕获分析 -------------------------------------------
        // 槽体最终要编成一个独立函数（static int hm_slot_N(HmCtx*)），所以它
        // 引用的**外层局部变量**必须被找出来，打包进上下文结构体。
        //
        // 判据：槽体里出现的标识符，若在当前作用域栈上能解析到、且**下标小于
        // 进入槽体时的水位**，它就是捕获。槽体内自己声明的变量在水位之上，
        // 不是捕获（它们本来就活在这个函数里）。
        const size_t cap_base = scope_.size();
        if (s->child) block_(s->child);
        std::vector<const char*> caps;
        collect_captures_(s->child, cap_base, caps);
        // 存到 type_args 链：每个捕获一个 IdentExpr 节点（text=名字，loc=首次出现处）。
        // 用 type_args 而不是新字段，是因为 Node 已有这个"附带子节点链"槽位，
        // 且 ast_dump 会自然打印出来 —— 捕获错了能一眼看见。
        Node* ct = nullptr;
        Node* ctail = nullptr;
        for (const char* name : caps) {
          void* mem = a_.alloc(sizeof(Node));
          if (!mem) break;
          Node* id = new (mem) Node();
          id->kind = Kind::IdentExpr;
          id->loc = s->loc;
          id->text = name;
          if (ctail) ctail->next = id; else ct = id;
          ctail = id;
        }
        s->type_args = ct;
        break;
      }
      // emit：负载实参逐个检查；信号名是否已声明留给 Sema 的符号表阶段。
      // sleep：时长必须是 int（毫秒）或 double（秒的小数，parser 已折算）。
      case Kind::SleepStmt: {
        Ty* d = type_of_(s->child);
        if (d && d->k != Ty::Int && d->k != Ty::Double && d->k != Ty::Unknown) {
          err("E4111", "sleep takes a duration", s->child ? s->child->loc : s->loc,
              "The duration is a number of milliseconds; anything else has no meaning to the "
              "runtime and would be silently coerced.",
              "pass an int (ms) or a literal with a unit, e.g. sleep(1500) or sleep(1.5s)");
        }
        break;
      }
      // until：信号必须已声明；timeout 是 int（毫秒）。
      case Kind::UntilStmt: {
        if (s->text && !has_signal_(s->text)) {
          err("E4109", internf("wait for undeclared signal `%s`", s->text), s->loc,
              "Waiting on a signal that was never declared can only be a typo; the wait would "
              "block until its timeout, every time.",
              "declare it with `export signal <Name>;`");
        }
        // 无 timeout 的 until 会一直阻塞：workflow 的上界是步数，不是墙上时间，
        // 所以一次无限等待能把整个产物挂死，而静态上界看起来"完全正常"。
        if (s->args == nullptr) {
          err_warn_("W4113", "until without a timeout can block forever", s->loc,
              "The workflow's bounds are step counts, not wall-clock time. An until with no "
              "timeout has no upper bound the compiler can see, so a missing emit hangs the "
              "artifact while the cost model still reports a finite run.",
              "add a bound: until(<Signal>) timeout(30s)",
              "if the wait is genuinely meant to be unbounded, keep it and make sure an emit is guaranteed.");
        }
        if (s->args) {
          Ty* t = type_of_(s->args);
          if (t && t->k != Ty::Int && t->k != Ty::Unknown) {
            err("E4111", "timeout takes milliseconds as an int", s->args->loc,
                "The timeout is compared against the monotonic clock in milliseconds; a string or "
                "a struct cannot be a deadline.",
                "pass an int, e.g. timeout(30000) or timeout(30s)");
          }
        }
        break;
      }
      case Kind::EmitStmt: {
        for (Node* a = s->args; a; a = a->next) type_of_(a);
        if (s->text && s->chan != nullptr && !slot_matches_(s->text, s->chan)) {
          err_warn_("W4112",
              internf("no handler for signal `%s` on channel \"%s\"", s->text, s->chan), s->loc,
              "The channel is hashed at compile time, so a typo produces a hash that nothing "
              "listens on. The emit will run and do nothing, silently.",
              internf("check the channel name, or add `on %s from \"%s\" { … }`", s->text, s->chan),
              "if another module handles it, ignore this warning.");
        }
        if (s->text && !has_signal_(s->text)) {
          err("E4109", internf("emit of undeclared signal `%s`", s->text), s->loc,
              "A signal has to be declared before anything can emit or handle it; otherwise a "
              "typo becomes a signal that never fires and never reports anything.",
              "declare it with `export signal <Name>;`");
        }
        break;
      }

      // 阶段 B：while / for 的体在 `body`（parse_while p:1085 / parse_for
      // p:1162），此前 stmt_() 根本没有这两个 case，落 default: break ——
      // 循环体里的一切源码类型错误都零诊断编成 IR。这里从零补一次遍历，
      // 每个体自带作用域水位（block_() 自己不 pop，见 c:865）。
      case Kind::WhileStmt: {
        // E4027：`while` 条件必须是 bool（同 IfStmt 的判定）。
        {
          Ty* cond = type_of_(s->child);            // cond
          check_cond_bool_(cond, s->child ? s->child->loc : s->loc);
        }
        const size_t base = scope_.size();
        ++loop_depth_;                 // break/continue 只在这里合法
        block_(s->body);               // body
        --loop_depth_;
        scope_.resize(base);
        break;
      }

      case Kind::ForStmt: {
        // for 头声明的类型（int / auto）被 parser 丢掉（p:1095-1099 只
        // advance 不存槽），循环变量类型只能由 init 推；推不出就 unknown。
        // 步进 i++ 折进 s->i64（p:1164），**不是 AST 节点**：不合成写、
        // 不查 E5010。cond 在 s->args（p:1160），体在 s->body（p:1162）。
        Ty* init_t = type_of_(s->child);   // init（p:1159）
        const size_t base = scope_.size();
        if (s->text) {
          VarSym v;
          v.name = s->text;                // 循环变量名（p:1158）
          v.t = init_t ? init_t : unknown();
          v.loc = s->loc;
          scope_.push_back(v);
        }
        type_of_(s->args);                 // cond（循环变量已在作用域里）
        ++loop_depth_;
        block_(s->body);                   // body
        --loop_depth_;
        scope_.resize(base);               // 循环变量与体内声明都不外泄
        // E4028：把循环变量名记进 for_closed_，供出循环后的引用识别
        // （见 IdentExpr 分支）。s->text 为 arena 里的稳定名字。
        if (s->text) for_closed_.push_back(s->text);
        break;
      }

      // 阶段 B：retry 的体在 `child`（parse_retry p:1305），不是 `args`；
      // 原来 block_(s->args) 遍历的是恒为空的槽位，体从来没被检查过。
      case Kind::RetryStmt: {
        const size_t base = scope_.size();
        block_(s->child);
        scope_.resize(base);
        break;
      }

      case Kind::TryStmt: {
        // 结构（parse_try）：child = try 体 Block；args = CatchClause 链；
        // 每个 CatchClause：text = 类型名，child = 变量 IdentExpr，
        // **else_branch = catch 体 Block**。
        //
        // 原来写的是 `block_(s->args)` —— 那等于把 CatchClause 当语句遍历，
        // try 的 catch 体从来没被检查过（块内的字段错误全部漏掉）。
        block_(s->child);

        size_t base = scope_.size();
        bool saw_error = false;
        for (Node* c = s->args; c; c = c->next) {
          if (c->kind != Kind::CatchClause) continue;
          size_t mark = scope_.size();

          // E4002：`catch (Error e)` 捕获除 BudgetError 外全部，所以排在它
          // 之后的任何 catch 都不可达。语言只有一层继承（6 个错误类型并列，
          // "兄弟互不匹配"），因此这是**唯一**可能写反的父子对 —— 也正因如此
          // 它值得硬错：写反会让专用分支永远进不去，错误静默降级到通用分支。
          if (saw_error) {
            const char* tname = c->text ? c->text : "?";
            err("E4002", internf("this catch (%s) is unreachable: `catch (Error e)` above already "
                                 "matches everything", tname),
                c->loc,
                "`Error` is the root of all catchable errors, so a catch placed after it can never "
                "run. The narrow branch silently degrades to the wide one — no error is reported, "
                "the wrong handler just quietly takes over.",
                "swap the two: put the specific type first and `catch (Error e)` last");
          }
          if (c->text && strcmp(c->text, "Error") == 0) saw_error = true;

          // catch 绑定的变量进作用域（类型暂为 unknown：错误对象本身没有
          // 结构类型，字段访问由 err_field_ 按 §7.1.1 的表单独判）。
          if (c->child && c->child->text) {
            VarSym v;
            v.name = c->child->text;
            v.t = unknown();
            v.loc = c->child->loc;
            v.is_error = true;  // 错误对象：只能读字段，不能整只存走（E4023）
            v.err_type = c->text;  // 字段表按类型分（§7.1.1）
            scope_.push_back(v);
          }
          if (c->else_branch) block_(c->else_branch);
          // 每个 catch 的绑定只活在自己的 body 里。
          scope_.resize(mark);
        }
        scope_.resize(base);
        break;
      }

      // parallel 作为语句出现时也要走一遍：否则块内的声明完全不进作用域，
      // 块内的类型错误也不会被发现 —— "看起来没问题，其实一个字都没查"。
      case Kind::ParallelExpr: parallel_(s); break;

      case Kind::MatchStmt: {
        Ty* subj_t = type_of_(s->child);
        // E4018：match 必须有 default 分支。
        //
        // 判定依据从"text 为空"改成 **is_nullable**：整数臂的 text 也是空的，
        // 用 text 判会把每个整数臂都当成 default —— 于是"没有 default"的检查
        // 永远通过，而真正的 default 又可能被算错。
        bool has_default = false;
        // E4020：重复 case 字面量（字符串臂与整型臂类型不同、严格相等，
        // 故以 "i:<v>" / "s:<v>" 为键分别计数，避免 "5" 与 5 误判重复）。
        std::vector<std::string> seen_cases;
        for (Node* arm = s->args; arm; arm = arm->next) {
          if (arm->kind != Kind::MatchArm) continue;
          if (arm->is_nullable) {
            has_default = true;
          } else {
            {
              const bool ia = (arm->text == nullptr);
              const std::string mkey = ia
                  ? ("i:" + std::to_string(arm->i64))
                  : ("s:" + std::string(arm->text ? arm->text : ""));
              bool dup = false;
              for (const auto& k : seen_cases) if (k == mkey) { dup = true; break; }
              if (dup) {
                const std::string label = ia
                    ? ("case " + std::to_string(arm->i64))
                    : ("case \"" + std::string(arm->text ? arm->text : "") + "\"");
                err("E4020", internf("%s appears more than once", label.c_str()), arm->loc,
                    "A match may only list each case label once. A second identical case can never "
                    "be reached, so it is dead code that silently shadows nothing — almost always a typo.",
                    "remove the duplicate case, or rename it to a distinct label");
              } else {
                seen_cases.push_back(mkey);
              }
            }
            if (subj_t && subj_t->k != Ty::Unknown && subj_t->k != Ty::Json) {
            // 臂与主语必须同类型：拿 int 臂去匹配 string 只会永远不命中，
            // 而那种"静默不匹配"要到运行期才表现为"走了 default"。
            const bool int_arm = (arm->text == nullptr);
            if (int_arm && subj_t->k != Ty::Int) {
              err("E4019", "integer case on a non-int value", arm->loc,
                  "Arms are compared with the matched value using strict equality, so a case "
                  "whose type differs from the value can never match.",
                  "use a string case, or match an int field instead");
            }
            if (!int_arm && subj_t->k != Ty::Str) {
              err("E4019", "string case on a non-string value", arm->loc,
                  "Arms are compared with the matched value using strict equality, so a case "
                  "whose type differs from the value can never match.",
                  "use an integer case, or match a string field instead");
            }
          }
          }  // 关闭上面 `} else {`：E4020 重复字面量检测 + E4019 臂/主语类型检查都在 else 分支内
          // 阶段 B：MatchArm 的体在 `child`（p:1245 case / p:1253 default），
          // 不在 `args`。原来 block_(arm->args) 遍历的是一个恒为空的槽位，
          // 于是 case 臂与 default 臂里的源码类型错误全部零诊断编成 IR。
          // 两个臂都是 s->args 链上的 MatchArm，这个循环都会走到。
          const size_t arm_base = scope_.size();
          block_(arm->child);
          scope_.resize(arm_base);
        }
        if (!has_default) {
          err("E4018", "`match` has no `default` branch", s->loc,
              "The matched value is a string, so 'all cases covered' cannot be proven statically. "
              "Without a default, an unexpected value has no branch to run — the workflow would "
              "fall through with nothing assigned, which is the kind of gap that only shows up "
              "on the one input nobody tried.",
              "add `default: { ... }` to handle whatever is left");
        }
        break;
      }

      // 阶段 C 第 3 条的评估结论：**保留 default，不去掉兜底**。
      //
      // 想去掉 default 的动机是"新增语句 Kind 没进 switch 就无法静默滑过"。
      // 但 `s->kind` 是 **ast.h::Kind 的全枚举**（语句类 + 约 25 个表达式类 +
      // 声明类共用），而 -Wall -Wextra -Werror 下的 -Wswitch 会在 switch 没有
      // default 且漏列枚举值时直接报错。去掉 default 就得把 CallExpr / LitExpr /
      // IdentExpr / BinaryExpr / ... 这些**永远不会出现在语句列表里**的表达式
      // Kind 全列成一句空的 `case ...: break;`：约 25 行纯噪声，每加一个表达式
      // Kind 都要回来改语句 switch，风险不可控。
      //
      // 真正兜住"新增语句形态"的是两道**可执行**守卫（比 default 更严：
      // default 只能保证"有 case"，保证不了"case 里真的下潜进体"）：
      //   · internal/native/parse_test.go::TestStatementShapeGuardCoversAllStatementKinds
      //     遍历 KindName 全枚举，新语句 Kind 未登记进 11 形态表 → 测试红；
      //   · internal/native/parse_test.go::TestStmtSwitchCoversEveryStatementShape
      //     文本扫描本函数的 case Kind:: 集合与 11 形态表对齐，少下潜一个体 → 红。
      // 无体 / 由父 case 代扫的语句 Kind（BreakStmt / ContinueStmt / Pattern /
      // MatchArm / CatchClause / ParallelReturn）在守卫里显式列白名单并注理由。
      default: break;
    }
  }

  // is_void_ / require_value_：void 只能作为**语句**出现，不能作为值。
  //
  // 为什么把判定收在一处：`auto a = voidf();` 只是最显眼的一处，真正会漏的是
  // 实参位、return 位、二元运算的一侧。散在各处写 if 必然漏几个，于是错误在
  // 更晚的 IR 装配期才炸，报的还是"节点缺字段"这种看不懂的话。
  bool is_void_(const Ty* t) const { return t && t->k == Ty::Void; }

  // collect_captures_ 在槽体子树里找出引用外层局部的标识符。
  //
  // 为什么要递归扫全子树而不是只看顶层语句：捕获可能藏在 if 条件、实参、
  // 字符串插值路径里。漏一个，编出来的槽函数就会引用一个不存在的变量，
  // 而 C 编译器只会报"undeclared identifier"——离真正原因（捕获分析漏了）很远。
  void collect_captures_(const Node* n, size_t base, std::vector<const char*>& out) {
    if (!n) return;
    if (n->kind == Kind::IdentExpr && n->text) {
      // 只认"外层局部"：下标 < base。全局（信号/函数名/struct）解析不到就不算捕获。
      for (size_t i = 0; i < base; ++i) {
        if (scope_[i].name && strcmp(scope_[i].name, n->text) == 0) {
          bool dup = false;
          for (const char* seen : out) {
            if (strcmp(seen, n->text) == 0) { dup = true; break; }
          }
          if (!dup) out.push_back(n->text);
          break;
        }
      }
    }
    // 赋值/自增的目标名字在 **text**，不是一个子节点（`tag = "hit"` 的 tag）。
    // 只看子树会漏掉"槽体**写入**外层变量"这一整类捕获，而写捕获比读更危险：
    // 不打包进上下文，槽函数就写不进调用方的变量，编译期才报 undeclared。
    if ((n->kind == Kind::AssignStmt || n->kind == Kind::UpdateStmt) && n->text) {
      for (size_t i = 0; i < base; ++i) {
        if (scope_[i].name && strcmp(scope_[i].name, n->text) == 0) {
          bool dup = false;
          for (const char* seen : out) {
            if (strcmp(seen, n->text) == 0) { dup = true; break; }
          }
          if (!dup) out.push_back(n->text);
          break;
        }
      }
    }
    for (const Node* c : children_of_(n)) collect_captures_(c, base, out);
    // PathExpr 的段在 **subs**，不在 child 链（`limit` 是单段 PathExpr）。漏掉
    // 这一处，捕获分析会把"用到了外层变量"整体看漏 —— 而且现象是槽函数里
    // 一个 undeclared identifier，离原因很远。
    for (int i = 0; i < n->n_subs; ++i) collect_captures_(&n->subs[i], base, out);
  }

  // slot_matches_：这个 (signal, channel) 有没有对应的槽。
  // 全局 emit（chan==nullptr）匹配任意全局槽；频道路由只匹配同频道槽。
  bool slot_matches_(const char* sig, const char* chan) const {
    for (const auto& sl : all_slots_) {
      if (sl.first == nullptr || sig == nullptr) continue;
      if (strcmp(sl.first, sig) != 0) continue;
      if (chan == nullptr) {
        if (sl.second == nullptr) return true;
        continue;
      }
      if (sl.second != nullptr && strcmp(sl.second, chan) == 0) return true;
    }
    return false;
  }

  bool has_signal_(const char* name) const {
    if (!name) return true;
    for (const char* s : signals_) {
      if (s && strcmp(s, name) == 0) return true;
    }
    return false;
  }

  void require_value_(Ty* t, Loc loc, const char* what) {
    if (!is_void_(t)) return;
    err("E4101", internf("a void call cannot be used as %s", what), loc,
        "`void` means \"this returns nothing\". There is no value to store, pass or compare, so "
        "using one here would leave a hole that only shows up when the IR is assembled.",
        "drop the assignment and call it as its own statement");
  }

  bool same_type_(Ty* a, Ty* b) {
    if (!a || !b) return true;
    if (a->k != b->k) return false;
    if (a->k == Ty::StructK) return a->struct_idx == b->struct_idx;
    return true;
  }

  // E4027：`if` / `while` 的条件必须是 bool。语言没有 truthiness —— 只有 bool
  // 才分真假。非 bool 条件若放行，会一直编进 IR，直到执行器的 if/while 分支
  // 才因"条件不是 bool"失败（run.go），而那本可以在编译期钉死。
  void check_cond_bool_(Ty* cond, Loc loc) {
    if (!cond || cond->k == Ty::Unknown) return;  // 未知类型交给别处诊断
    if (cond->k == Ty::Bool) return;
    err("E4027",
        internf("condition must be bool, got %s", type_name_(cond)),
        loc,
        "An if / while condition is a control-flow branch point, and the language has no truthiness: only bool has a true/false value. "
        "int / string / json / cost have no definition of \"being true\"; letting it through would let an implicit "
        "conversion like \"nonzero means true\" quietly decide the flow, while still failing at runtime for a non-bool condition.",
        "Write the condition as a comparison, e.g. `if (x != 0)`; narrow a json field with `json.as_bool(j)` before testing.");
  }

  void check_workflow_(Node* wf) {
    scope_.clear();
    for_closed_.clear();  // E4028 的循环变量名不跨 workflow 泄漏
    // 2.0.1：stop 只在这个作用域内合法；槽表按 workflow 重置（全局槽在
    // check() 里单独处理，二者不共享"同作用域"的概念）。
    const bool saved_in_wf = in_workflow_;
    const Ty* saved_ret = workflow_ret_;
    const std::vector<const char*> saved_on = on_signals_;
    const int saved_loop = loop_depth_;
    in_workflow_ = true;
    on_signals_.clear();
    loop_depth_ = 0;
    // 声明的返回类型存在 WorkflowDecl.type_args（parser 在 parse_workflow 里填的）。
    // 没有 `-> T` 的 workflow 允许由体推断，此时 declared_ret_ 为 null，E4021 不触发。
    declared_ret_ = wf->type_args ? from_type_(wf->type_args) : nullptr;
    workflow_ret_ = declared_ret_;   // E4108 用它比 stop 的 data 类型

    for (Node* p = wf->args; p; p = p->next) {
      if (p->kind != Kind::Param || !p->text) continue;
      VarSym v;
      v.name = p->text;
      v.t = from_type_(p->child);
      v.loc = p->loc;
      scope_.push_back(v);

      // W5005：input 被声明为 json 时，入口没有结构契约。这是规范允许的
      // 逃逸口（LANGUAGE.md §4.8），但 hermes serve 无法据此生成文档，
      // 也无法在入口挡住畸形请求。编译期唯一触发条件就是 input 的类型解析为 json。
      if (strcmp(p->text, "input") == 0 && v.t && v.t->k == Ty::Json) {
        err_warn_(
            "W5005",
            "entry has no structural contract: 'input' is declared as json, so hermes serve cannot derive an API schema from it and cannot reject malformed requests at the entry point",
            p->loc,
            "the type of 'input' is the HTTP request contract. Declared as json, the body is arbitrary JSON, so serve can neither generate an API schema nor validate fields at the entry point. This is a spec-permitted escape hatch (LANGUAGE.md §4.8), but it forfeits compile-time contract protection.",
            "declare a struct as the contract: 'input: SupportInput', where the fields are the request body shape; non-T? fields become required.",
            "if arbitrary JSON is genuinely needed, keep json but accept this warning; or use --deny-warnings to promote it to an error.");
      }
    }

    block_(wf->child);
    scope_.clear();
    // 恢复外层状态：workflow 可以嵌套编译（一个文件多个 workflow），
    // 状态泄漏会让下一个 workflow 的 break/stop 判定失真。
    in_workflow_ = saved_in_wf;
    workflow_ret_ = const_cast<Ty*>(saved_ret);
    on_signals_ = saved_on;
    loop_depth_ = saved_loop;
  }
};

}  // namespace

// 公开入口：在 parse 之后调用，把类型诊断并进同一个 DiagBag。
void check_types(Node* root, const char* filename, Arena& arena, DiagBag& diags) {
  Checker c(arena, diags, filename);
  c.run(root);
}

// check_types_with_externals 额外接受一张"别的模块声明了哪些 struct"的表。
// 单文件检查器看不到别的 .h —— 不喂这张表，每个正常的多文件工程都会被判
// "访问 unknown 的字段"（support_bot 正是这么红的）。
void check_types_with_externals(Node* root, const char* filename, Arena& arena,
                                DiagBag& diags, const char* ext, size_t ext_len) {
  Checker c(arena, diags, filename);
  if (ext && ext_len) c.load_externals_(ext, ext_len);
  c.run(root);
}

}  // namespace hm
