/* hermes_parser.h —— libhermes_parser.a 的公开 C ABI（ARCH §4.5）。
 *
 * 这是 Hermes 编译器前端对外的**唯一**头文件。Go 侧通过 cgo 调它，
 * Python 绑定 / 编辑器插件也可以通过它单独复用前端（不链接 Go、不链 Rust）。
 *
 * 契约（ARCH §4.5，逐条都是硬约束）：
 *
 *   1. 纯函数。同样输入永远产出同样的字节 —— golden 比对直接比字节，
 *      不需要 fixture server。**不读文件、不打印、无全局可变状态。**
 *      （注意：`#include` 的文件读取与环检测在 Go 侧的 internal/include 里做，
 *        不在这里 —— 前端拿到的永远是**已读入内存的字节**。见 IR.md §9.6：
 *        "C++ 前端只做词法/语法/类型/codegen，不做任何跨文件、任何 IO、任何签名"。）
 *
 *   2. 可重入、线程安全。每次调用拥有独立的 Arena，返回前整体释放。
 *      因此 hermes test --parallel 与将来的 serve 可以并发编译。
 *
 *   3. 两个出参**总是被写**（成功时 out_diag 也可能非空 —— 有 warning）。
 *      **缓冲不足时不会报"需要多大"**：*_len 回填的是**实际写入的字节数（含 NUL）**，
 *      且在溢出时它等于 cap —— 也就是说调用方无法从返回值区分"刚好装满"与"被截断"。
 *      判定方式是 `*len >= cap` 即视为可能截断，**按 2 倍扩容重试**，直到 `*len < cap`。
 *      永不假设一次成功。（这是既有实现的真实行为，别把它当成"会告诉你要多大"。）
 *
 *   4. 诊断与 AST 都走**字节**，不走结构体数组 —— 这样加一个诊断字段
 *      不需要演进任何跨语言 ABI（没有 abi_size、没有版本协商）。
 *
 * 本文件只依赖 <stddef.h> 与 <stdint.h>，可被 C89 + <stdint.h> 编译，
 * 也可被 C++ 直接 include（已带 extern "C"）。
 */

#ifndef HERMES_PARSER_H
#define HERMES_PARSER_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

/* 版本串，形如 "hermes-parser/0.1"。指针指向静态常量，调用方不得 free。 */
const char* hm_parse_version(void);

/* 把 Node::Kind 的整数值翻成名字。用于 Go 侧解析 AST dump 时对照。
 * 成功返回 0；out 为 NULL 或 cap 为 0 返回 -1；kind 非法返回 "?"（仍返回 0）。 */
int hm_parse_kind_name(int kind, char* out, size_t cap);

/* 主入口：一次性产出 AST dump + 诊断 JSON + 节点数。
 *
 *   src/len     源字节（**不需要** NUL 结尾）
 *   filename    用于诊断里的文件名，不参与解析语义
 *   is_header   非 0 表示按 .h（接口文件）的语法限制解析：
 *               带体 workflow -> E3005、语句 -> E3006、#link -> E3017、
 *               带地址的 backend -> E3020
 *
 * 任一出参为 NULL 时该项被跳过（对应的 *_len 写 0）。
 * 返回 0 成功；-2 表示 Arena 分配失败（内存不足）。
 */
int hm_parse(const uint8_t* src, size_t len, const char* filename, int is_header,
             char* out_dump, size_t dump_cap, size_t* dump_len,
             char* out_diag, size_t diag_cap, size_t* diag_len,
             int* node_count);

/* hm_codegen：解析并把**第一个** workflow 编成 IR 的 workflow 对象（canonical JSON）。
 * 返回 0 成功 / 1 失败（原因在 out_diag 的 JSON 数组里）/ 2 源码里没有 workflow。
 * 两段式约定同 hm_parse_*：*out_len > cap 表示被截断，按返回值扩容重试。 */
int hm_codegen(const uint8_t* src, size_t len, const char* filename, int is_header,
               char* out, size_t cap, size_t* out_len,
               char* out_diag, size_t diag_cap, size_t* diag_len);

/* hm_codegen_tools：模块里的 tool 声明（name / description / method / url_template）。
 * 工具必须有**声明**而不是内联在 agent 调用里：模型看到的 schema 与运行期执行的
 * URL 必须是同一份东西。 */
int hm_codegen_tools(const uint8_t* src, size_t len, const char* filename, int is_header,
                     char* out, size_t cap, size_t* out_len,
                     char* out_diag, size_t diag_cap, size_t* diag_len);

/* hm_codegen_backends：模块里的 backend 声明/定义（name / kind / addr 来源）。 */
int hm_codegen_backends(const uint8_t* src, size_t len, const char* filename, int is_header,
                        char* out, size_t cap, size_t* out_len,
                        char* out_diag, size_t diag_cap, size_t* diag_len);

/* hm_codegen_wfdecls：顶层 workflow 声明（name / exported / is_entry）。
 * 用于跨文件装配：export 与 entry 标注写在 .h 的声明上，定义不带。 */
int hm_codegen_wfdecls(const uint8_t* src, size_t len, const char* filename, int is_header,
                       char* out, size_t cap, size_t* out_len,
                       char* out_diag, size_t diag_cap, size_t* diag_len);

/* hm_codegen_ext：同 hm_codegen，但接受外部声明表（每行 `Struct\tField\tType`）。
 * 跨文件工程必须走这条口：单文件类型检查器看不到别的 .h。 */
int hm_codegen_ext(const uint8_t* src, size_t len, const char* filename, int is_header,
                   const char* ext_types, size_t ext_len,
                   char* out, size_t cap, size_t* out_len,
                   char* out_diag, size_t diag_cap, size_t* diag_len);

/* hm_codegen_functions：把模块顶层的函数定义编成函数表（JSON 数组）。
 *
 * 输出是 unit 级的表：每项 {name, params, returns, root, nodes, vars, recursive}。
 * 与 hm_codegen_ext 的 workflow 输出**分开**：函数跨 workflow 可见，塞进某一份
 * workflow 会让同一份函数出现多次。返回 0 成功 / 1 失败（原因在 out_diag）。
 * 没有函数时返回 0 和空数组 "[]"。
 *
 * ext_types 与 hm_codegen_ext 同义：函数签名里的 struct 可能来自别的 .h。 */
int hm_codegen_functions(const uint8_t* src, size_t len, const char* filename, int is_header,
                         const char* ext_types, size_t ext_len,
                         char* out, size_t cap, size_t* out_len,
                         char* out_diag, size_t diag_cap, size_t* diag_len);

/* hm_codegen_types：把顶层 struct 编成类型表（JSON 数组）。返回码同 hm_codegen。 */
int hm_codegen_types(const uint8_t* src, size_t len, const char* filename, int is_header,
                     char* out, size_t cap, size_t* out_len,
                     char* out_diag, size_t diag_cap, size_t* diag_len);

/* 只要 AST dump。语义同 hm_parse 的对应部分。 */
int hm_parse_dump(const uint8_t* src, size_t len, const char* filename,
                  int is_header, char* out, size_t cap, size_t* out_len);

/* 只要诊断 JSON。语义同 hm_parse 的对应部分。 */
int hm_parse_diag(const uint8_t* src, size_t len, const char* filename,
                  int is_header, char* out, size_t cap, size_t* out_len);

/* 只要 AST 节点数（golden 的 "节点数 == K" 断言用）。 */
int hm_parse_count(const uint8_t* src, size_t len, const char* filename,
                   int is_header, int* count);

#ifdef __cplusplus
} /* extern "C" */
#endif

#endif /* HERMES_PARSER_H */
