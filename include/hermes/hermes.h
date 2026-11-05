/* hermes.h —— **统一入口**。用户只需要 include 这一个文件。
 *
 * 为什么需要它：C 和 C++ 常混在一个工程里，而 `hermes header` 会生成两份头
 * （`.h` 给 C，`.hpp` 给 C++）。让用户自己写 `#ifdef __cplusplus` 是把
 * 头文件生成器该做的事推给用户 —— 而大多数人在 `#include` 报错一次之后就走了。
 *
 * 用法：
 *   #include "hermes/hermes.h"
 *
 * 注意：**不要**再在外面包一层 `extern "C"`。生成的头已经包了，
 * 嵌套 `extern "C"` 在标准里是合法的，但某些 clang 版本在 `-pedantic` 下会警告。
 */
#ifndef HERMES_UMBRELLA_H
#define HERMES_UMBRELLA_H

/* 核心 ABI：状态码、字符串视图、buf 所有权、ABI 版本。C 与 C++ 共用。 */
#include "hermes/hermes_core.h"

#endif /* HERMES_UMBRELLA_H */
