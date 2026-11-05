// codegen.h —— AST → IR 节点图（T34 的第一刀）。
//
// 切分原则（按"谁看得见"）：
//   C++ 看得见 **AST**，所以它产出 workflow 的 nodes / vars / bounds；
//   Go 看得见 **文件系统**（include 图、模块字节、sha256），所以 modules / types /
//   exports / requires 与最终装配在 Go 侧（internal/ir 的 Emit）。
//
// 这条界线不是偷懒：`unit.modules[].sha256` 要读文件字节，而 C++ 前端被定义为
// **无 IO**（LANGUAGE §1.2）。硬把它塞进前端，就等于在无 IO 的层里开第一个口子。
#pragma once

#include <string>

#include "ast.h"
#include "json_write.h"

namespace hm {

// codegen_workflow 把一个 WorkflowDecl 编成 IR 的 workflow 对象（JVal）。
//
// 支持范围（这一刀刻意很小，先把链路打通）：
//   - 语句：Block / LetStmt（单个名字）/ ReturnStmt
//   - 表达式：字符串字面量（含插值）、int/bool 字面量、单个标识符（变量引用）
//
// 不支持的构造**不静默降级**：返回 false 并把原因写进 err。静默降级会让
// "编出来一份缺了东西的 IR"看起来像成功，而那就是最难查的一类 bug。
bool codegen_workflow_in(const Node* module, const Node* wf, JVal* out, std::string* err,
                         Loc* err_loc = nullptr);
bool codegen_workflow(const Node* wf, JVal* out, std::string* err);

// codegen_types 把模块顶层的 `struct` 声明编成类型表（JVal 数组）。
//
// 类型**引用**在 C++ 侧只写名字（`{"k":"struct","ref":"OrderInfo"}`），
// 因为 id 是"unit 内声明顺序"的产物，只有装配阶段（Go，它看得见按 include
// 顺序排好的全部模块）才能分配。Go 负责把 ref 解析成 id —— 这与 vars[].decl
// 的回填是同一条分工。
bool codegen_types(const Node* module, JVal* out, std::string* err);

// codegen_wfdecls 编出模块顶层的 workflow **声明**：{"name","exported","is_entry"}。
//
// 为什么需要它：语言规定 `export` 与 `entry` 写在 `.h` 的声明上，`.hermes` 里的
// 定义**不带**（LANGUAGE §3.5，D-014.2）。只看定义那一份，每个正常的工程都会被判
// "entry workflow 没有 export" —— support_bot 正是这么红的。
bool codegen_wfdecls(const Node* module, JVal* out, std::string* err);

// codegen_backends 编出模块里的 backend 声明/定义。
//
// `kind` 来自 `backend NAME : kind`（D-032）：它是**协议**（openai/anthropic/
// ollama/homonoia/custom），不是地址。IR 要求它是枚举，而它无法从地址推出来——
// 推出来的那个值会在"这台机器上碰巧能用"时看起来正确。
bool codegen_backends(const Node* module, JVal* out, std::string* err);

// codegen_tools 编出模块里的 `tool` 声明（D-034）。
// 工具声明与 agent 调用分开：模型看到的 schema 与运行期执行的 URL 必须同源。
bool codegen_tools(const Node* module, JVal* out, std::string* err);

// codegen_functions 把模块顶层的函数定义编成函数表（JVal 数组）。
//
// 为什么单独一口而不是塞进 workflow：函数是**unit 级**事实（跨 workflow 可见），
// 而 workflow 对象是"一次执行"。把函数塞进 workflow 会让同一份函数在多个
// workflow 里各出现一次，改名时必然漏一处；上一轮把它塞进 workflow JSON 还
// 直接撑坏了产物（见 docs/FUNCTIONS.md）。
//
// 输出的形状见 docs/FUNCTIONS.md §IR 形状。函数体里的外部调用
// （model/tool/memory/vector）目前**显式拒绝**：它们的调用次数要进 workflow 的
// bounds，而调用图内联还没实现 —— 先拒绝，不编一个错的上界。
bool codegen_functions(const Node* module, JVal* out, std::string* err, Loc* err_loc = nullptr);

}  // namespace hm
