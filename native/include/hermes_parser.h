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
/* hm_fmt_expand：把 #bind 展开成等价的 #link，其余字节原样保留。
 * 返回 0 成功；-4 缓冲区不够（*out_len 给出所需字节数，含结尾 NUL）。 */
int hm_fmt_expand(const uint8_t* src, size_t len, char* out, size_t cap, size_t* out_len);

/* hm_deps_scan：扫顶层 #include / #link，产出 JSON 数组
 * （[{kind,spec,angle,line,col}, ...]）。返回 0 / -4（缓冲不足）。 */
/* hm_archive_index_sha256：从 .hbc 字节里取索引区并算 sha256（64 位十六进制）。
 * 返回 0 成功；2 不是归档；3 版本不认识；4 结构越界；5 哈希失败。
 * **无 IO**：收字节，读文件是调用方的事。 */
/* hm_archive_write：写 .hbc 归档。返回 0 / -1（参数或 JSON 非法）/ -2（空归档）
 * / -3（哈希失败）/ -4（缓冲不足，*out_len 给出所需大小）。 */
/* hm_linkpolicy_match：许可红线闭集判据。命中返回 1（写出规则名与许可），
 * 未命中 0，参数非法 -1。规则与 internal/linkpolicy 逐条一致。 */
int hm_linkpolicy_match(const char* lib, char* out_name, size_t name_cap, char* out_licence,
                        size_t lic_cap);

/* hm_archive_read：校验 .hbc 结构并透出索引里的 members JSON 数组。
 * 返回 0 / -4（缓冲不足）/ 2（不是归档）/ 3（版本不认识）/ 4（结构/哈希/JSON 有问题）。 */
/* hm_header_gen_c：从 IR JSON 生成 C 头（与 internal/header.GenC 逐字节一致）。 */
int hm_header_gen_c(const char* ir_json, size_t len, char* out, size_t cap, size_t* out_len);

/* hm_header_gen_shim：生成 unit 的 C 实现层（per-unit 符号 → hermes_rt_run）。 */
/* hm_layout_*：资源定位（顺序与 internal/layout 一致）。
 * header_for 返回 1 命中 / 0 未命中；resolve 返回 0。 */
/* hm_target_parse：--target 分类（与 internal/toolchain.ParseTarget 逐字一致）。
 * 返回 0 成功（out_json）/ 1 失败（out_err）/ -1 参数非法 / -4 缓冲不足。 */
/* hm_link_plan：compile 的唯一链接面决策点。返回 0=plan / 1=拒绝 / -1 / -4。
 * 拒绝时 out_json 是 {"code":..,"message":..}。 */
/* hm_ir_uses_net：workflow 节点里是否出现 call_tool / call_model。
 * 返回 1 需要网络 / 0 不需要 / -1 参数或 JSON 非法。 */
/* hm_check_hostonly：裸机目标上拒绝 sleep/until/emit-to（定稿文案逐字）。
 * 返回 0 通过 / 1 拒绝（out_msg）/ -1 参数或 JSON 非法。 */
/* hm_check_compiled_static：编译型产物不得残留指向非递归函数的 call_fn。0 通过 / 1 拒绝 / -1。 */
/* hm_product_link_gate：产物侧兜底（fail-closed）。0 通过 / 1 拒绝 / -1 参数非法。 */
int hm_product_link_gate(const char* out_path, int used_dynamic, const char* bare_metal_json,
                         const char* plan_json, char* out_msg, size_t cap, size_t* out_len);

int hm_check_compiled_static(const char* ir_json, size_t len, char* out_msg, size_t cap,
                             size_t* out_len);

/* hm_import_plan：把 homonoia 计划 JSON 转成 .hermes 源码（与 internal/importhomo 一致）。
 * 返回 0 成功 / 1 不是可用计划（out_err）/ -1 / -4。 */
/* hm_explain_ir：渲染 IR 契约（人读表或 --json）。0 成功 / 1 找不到 workflow / -1 / -4。 */
int hm_explain_ir(const char* ir_json, size_t len, const char* wf_name, int as_json,
                  const char* (*tr)(const char*), char* out, size_t cap, size_t* out_len,
                  char* out_err, size_t err_cap, size_t* err_len);

int hm_import_plan(const char* json, size_t len, char* out_src, size_t cap, size_t* out_len,
                   char* out_warn, size_t warn_cap, size_t* warn_len, char* out_err, size_t err_cap,
                   size_t* err_len);

int hm_check_hostonly(const char* ir_json, size_t len, int bare_metal, char* out_msg, size_t cap,
                      size_t* out_len);

/* hm_check_pure_c_abi：纯 C ABI 闸门（C++ ABI 运行时一律拒绝）。返回 0/1/-1。 */
int hm_check_pure_c_abi(const char* flag, const char* lib, char* out, size_t cap, size_t* out_len);

/* hm_static_libc_gate：Linux 上静态链 glibc 的许可闸门。返回 0/1/-1。 */
int hm_static_libc_gate(const char* goos, int is_static, char* out, size_t cap, size_t* out_len);

/* hm_link_input_gate：--link-* 每一个路径的输入侧闸门。返回 0/1/-1。 */
int hm_link_input_gate(const char* flag, const char* lib, char* out, size_t cap, size_t* out_len);

int hm_ir_uses_net(const char* ir_json, size_t len);

int hm_link_plan(const char* target_json, int need_net, const char* net_lib, const char* static_csv,
                 const char* dynamic_csv, char* out_json, size_t cap, size_t* out_len);

/* hm_linkpolicy_detail：命中时输出 {name,licence,reason,source}（未命中输出空串）。 */
int hm_linkpolicy_detail(const char* lib, char* out, size_t cap, size_t* out_len);

int hm_target_parse(const char* raw, const char* goos, const char* goarch, char* out_json,
                    size_t cap, size_t* out_len, char* out_err, size_t err_cap, size_t* err_len);

int hm_layout_header_for(const char* lib_path, char* out, size_t cap);
int hm_layout_resolve(const char* exec_path, const char* cwd, char* out_root, size_t root_cap,
                      char* out_lib, size_t lib_cap, char* out_include, size_t inc_cap,
                      char* out_shim, size_t shim_cap);

/* hm_header_core_impl：运行时核心实现正文（C89）。 */
int hm_header_core_impl(char* out, size_t cap, size_t* out_len);

/* hm_header_gen_cpp：C++ 入口头（<unit>.hpp，便利层默认关闭）。 */
int hm_header_gen_cpp(const char* ir_json, size_t len, char* out, size_t cap, size_t* out_len);

int hm_header_gen_shim(const char* ir_json, size_t len, const unsigned char* ir_bytes,
                       size_t ir_len, char* out, size_t cap, size_t* out_len);

int hm_archive_read(const unsigned char* data, size_t len, char* out, size_t cap,
                    size_t* out_len);

int hm_archive_write(const char* members_json, size_t len, unsigned char* out, size_t cap,
                     size_t* out_len);

int hm_archive_index_sha256(const unsigned char* data, size_t len, char* out_hex);

/* hm_diag_snippet：从源码里截出诊断位置片段（行号 + ^ 指示）。
 * 返回 0 / -4（缓冲不足，*out_len 给出所需大小）/-1（参数非法）。 */
/* hm_diag_render_json_src：把诊断 JSON 数组 + 源码渲染成完整人读文本
 * （片段 + 文案）。返回 0 / -1（参数或 JSON 非法）/-4（缓冲不足）。 */
int hm_diag_render_json_src(const char* src, size_t src_len, const char* json, size_t json_len,
                        const char* lang, char* out, size_t cap, size_t* out_len);

/* hm_diag_render_json_keep_going：--keep-going 口径（至多 50 条，不提示首错即停）。 */
/* hm_diag_render_json_all：全部渲染（ir 口径，不截断、不提示）。 */
int hm_diag_render_json_all(const char* src, size_t src_len, const char* json,
                            size_t json_len, char* out, size_t cap, size_t* out_len);

int hm_diag_render_json_keep_going(const char* src, size_t src_len, const char* json,
                                   size_t json_len, char* out, size_t cap, size_t* out_len);

int hm_diag_snippet(const char* src, size_t len, int line, int col, int end_col,
                    char* out, size_t cap, size_t* out_len);

int hm_deps_scan(const uint8_t* src, size_t len, const char* filename, int is_header,
                 char* out, size_t cap, size_t* out_len, char* out_diag, size_t diag_cap,
                 size_t* diag_len);

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

/* hm_schema_validate：用 schema_json 校验 value_json。
 * 返回 0 = 通过；1 = 该 schema 用了 C++ 版未覆盖的关键字（调用方回落 Go 实现）；
 * 2 = 校验失败（out_err 是 "path: reason"）；3 = schema/value 不是合法 JSON。 */
int hm_schema_validate(const char* schema_json, size_t schema_len,
                       const char* value_json, size_t value_len,
                       char* out_err, size_t err_cap, size_t* err_len);

/* hm_backend_unit_emit：把 IR 的 functions[] 里一个导出函数编成 C ABI 实现
 * （`hermes cc` 的 .hbc 静态库成员）。返回码同 hm_backend_c_emit：0/1/2。 */
int hm_backend_unit_emit(const char* ir_json, size_t len, const char* unit,
                         const char* fn_name, char* out, size_t cap, size_t* out_len,
                         char* out_err, size_t err_cap, size_t* err_len);

/* hm_ir_assemble_final：把源码直接编成**最终的规范 IR JSON**。
 *
 * 存在的理由：今天这条链路是 源码 → C++ 前端（workflow JSON）→ Go 装配
 * （internal/ir：上界重算、schema 引用解析、canonical 写出）→ 结构体 → JSON。
 * 目标是把中间那一段整体搬进 C++：前端直接吐出最终 IR，跨语言只剩一次读取。
 *
 * 返回码与 hm_backend_c_emit 同约定：
 *   0  成功，out 是最终 IR（与 `hermes ir` 逐字节相同）
 *   1  这份源码用了 C++ 装配**尚未覆盖**的形态（迁移中，调用方回落 Go）
 *   2  致命错误（缓冲不足 / IR 结构损坏）
 *
 * facts_json 是"只有调用方才知道的事实"，一个对象：
 *   {"modules":[…], "requires":[…], "imports":[…]}
 * 由调用方提供 —— LANGUAGE §1.2 规定 C++ 前端**无 IO**：读文件、算 sha256、
 * 解析 #link 的成员摘要都属于调用方。装配本身（节点重排、类型 id 与摘要、
 * schema 生成、canonical 写出）在 C++ 里做。
 *
 * 迁移判据：internal/ir 的等价性测试按**字节**比对 41 份基线。 */
/* IR 里记录"谁编的"。与 cmd/hermes 的 semver 必须一致（一条守卫盯着）。 */
#define HERMES_IR_COMPILER_VERSION "2.0.1"

int hm_ir_assemble_final(const uint8_t* src, size_t len, const char* filename,
                         const char* facts_json, size_t facts_len,
                         char* out, size_t cap, size_t* out_len,
                         char* out_err, size_t err_cap, size_t* err_len);

/* hm_ir_validate：对已装配的**规范 IR JSON** 跑完整三阶段静态校验
 * （形状 → 图不变量 → 派生复算），等价于 Go 的 internal/ir.Validate。
 *
 * 存在的理由："去 Go 化"把 internal/ir/validate.go 整体搬到 C++。Go 侧
 * 的 ir.Load / ir.Validate 是今天唯一一份校验实现；C++ 这份是第二份，
 * 由 cmd/hermes 的等价性测试比对 41 份基线的诊断集合（合法 IR 两边皆空）。
 *
 * 与 hm_ir_assemble_final 不同，校验**无"未覆盖"概念**——任何已装配 IR 都能
 * 被校验，所以没有 rc=1。返回码：
 *   0  成功，out 是诊断 JSON 数组（扁平形状，见 cmd/hermes/ir_build.go 的
 *       nativeDiag；合法 IR 是 "[]"）
 *   2  致命错误（输入为空 / IR 不是合法 JSON / 出参缓冲不足），原因在 out_err
 *
 * out_len / err_len 回填**实际写入字节数**（与其它入口同一条契约：
 * 缓冲不足时不截断，回填所需大小并返回 2）。 */
int hm_ir_validate(const uint8_t* ir_json, size_t len,
                   char* out, size_t cap, size_t* out_len,
                   char* out_err, size_t err_cap, size_t* err_len);

/* hm_backend_c_emit：把一份**已装配的 IR**（规范 JSON）编成 C 源码。
 *
 * 存在的理由：这是"去 Go 化"的迁移缝。IR → C 这一步今天由 Go 实现
 * （internal/backend/c.go），目标位置是 C++（native/cpp/backend_c.cpp）。
 * 迁移期间两条实现并存，由一条等价性测试逐字节比对，直到 C++ 版本覆盖
 * 全部 golden 才切换默认、删除 Go 版。
 *
 * 返回码：
 *   0  成功，out 里是完整 C 源码
 *   1  这份 IR 用了 C++ 版本**尚未覆盖**的节点（迁移中；调用方应回落 Go 版）
 *   2  致命错误（出参缓冲不足、IR 结构损坏），原因在 out_err
 *
 * out_len / err_len 回填**实际写入字节数**（与其它入口同一条契约：
 * 缓冲不足时不截断，回填所需大小并返回 2）。 */
int hm_backend_c_emit(const char* ir_json, size_t len,
                      char* out, size_t cap, size_t* out_len,
                      char* out_err, size_t err_cap, size_t* err_len);

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
