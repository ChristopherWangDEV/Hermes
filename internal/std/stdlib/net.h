// <std/net.h> —— tool.call 的能力声明。
//
// 这个文件是**声明载体**，不是实现：`tool.call` / `HttpError` / `NetworkError`
// 是语言的保留命名空间（LANGUAGE §3.3），由编译器内建处理，不存在"用户可覆写
// 的实现"。它的存在有两件事要做：
//   ① 让 `#include <std/net.h>` 有一个**可哈希的实体**，从而进
//      `unit.modules[]` 与 `requires[].member_sha256`；
//   ② 给人一个看到"这个模块到底提供什么"的地方。
//
// 语言层面没有可写的声明语法对应内建命名空间，所以这里只有注释。这不是偷懒：
// 写一条假的 `struct HttpError { ... }` 会与内建的类型检查打架。
