/* hermes_core.h —— 对外 C ABI 的核心头。**手写、进版本库、冻结**。
 *
 * 为什么手写而不是生成：所有 `<unit>.h` 都 include 它，而它们各自会被重新生成。
 * 如果核心类型也由生成器产出，两个 unit 的头被同一个翻译单元 include 时就会出现
 * 重复定义 —— 而"哪些符号属于核心"这件事，本来就该由人定一次、之后不动。
 *
 * 方言：**C89 + <stdint.h>**（D-024）。不是 C99：
 *   · 没用 `//` 注释（C89 无行注释，`-std=c89 -pedantic` 直接报错，
 *     而报错会伪装成"客户编译器太老"）
 *   · 声明不放块中间、不用 `long long`（用 int64_t）
 *   · 没有 `_Thread_local`（MSVC 与 C89 都没有），用下面的宏
 *
 * 这个头**不 include 任何 C++ 标准库**，也不 include <string.h> 之外的东西。
 */
#ifndef HERMES_CORE_H
#define HERMES_CORE_H

/* 被 HermesScript 侧 include 时报错：这份头是给 C/C++ 编译器读的，
 * 而 HermesScript 的 `.h` 是另一套语法、同名不同目录（D-024）。 */
#ifdef HERMES_SCRIPT
#error "hermes_core.h is a C ABI header, not a HermesScript interface file"
#endif

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

/* 线程局部。MSVC 与 C89 都没有 _Thread_local，用宏吸收差异。 */
#if defined(_MSC_VER)
#define HERMES_THREAD_LOCAL __declspec(thread)
#elif defined(__STDC_VERSION__) && __STDC_VERSION__ >= 201112L
#define HERMES_THREAD_LOCAL _Thread_local
#else
#define HERMES_THREAD_LOCAL
#endif

/* 字符串视图。**必须带 len**：值里可以有 NUL，而 Go/Rust 的 string 本就不以 NUL 结尾。
 * 不带 len 的 `const char*` 会让"含 NUL 的字符串"在跨语言边界上被静默截断。 */
typedef struct hermes_str {
  const char* ptr;
  size_t len;
} hermes_str;

/* 由本库分配、由 hermes_buf_free 释放的字节块。
 * 传**指针**而不是值，是为了 free 之后能置 NULL —— 防 double free。 */
typedef struct hermes_buf {
  char* ptr;
  size_t len;
} hermes_buf;

/* 状态码。0 恒为成功；其余按 Attribution 的六个桶分组（与运行期错误码同源）。
 * 用 enum 而不是 #define：类型检查能挡住"把一个 int 当状态码传"。 */
typedef enum hermes_status {
  HERMES_OK = 0,
  HERMES_ERR_INVALID_ARG = 1,
  HERMES_ERR_MODEL_CONTRACT = 2,
  HERMES_ERR_UPSTREAM_DRIFT = 3,
  HERMES_ERR_TRANSPORT = 4,
  HERMES_ERR_POLICY = 5,
  HERMES_ERR_BUDGET = 6,
  HERMES_ERR_CIRCUIT = 7,
  HERMES_ERR_INTERNAL = 8
} hermes_status;

/* 释放一个 buf 并把 ptr/len 置 0。传 NULL 是合法的 no-op。 */
void hermes_buf_free(hermes_buf* b);

/* 分配由本库完成、由调用方用 hermes_rt_free 归还的字符串。 */
void hermes_rt_free(char* p);

/* 取最近一次错误的消息（线程局部）。返回值在下次调用前有效。 */
hermes_str hermes_last_error(void);

/* 执行一份内嵌的 IR（或 .hbc 字节）。
 *
 * 成功返回 0，*out_ptr 是 NUL 结尾的 JSON 结果（用 hermes_rt_free 归还）。
 * 失败返回非 0，*err_ptr 是错误文本（同样用 hermes_rt_free 归还）。
 *
 * 线程安全：本函数不读写任何全局可变状态，可并发调用。 */
int hermes_rt_run(char* ir_data, size_t ir_len,
                  char* input_data, size_t input_len,
                  char** out_ptr, char** err_ptr);

/* 直接从文件执行（.hbc 归档或 IR JSON）。签名其余部分与 hermes_rt_run 相同。 */
int hermes_rt_run_file(char* unit_path,
                       char* input_data, size_t input_len,
                       char** out_ptr, char** err_ptr);

/* 对外 ABI 的 major 版本：**major 相等即可**（不是严格相等）。
 * 第三方消费者不会与我们同步升级，前提不成立；结构体首字段 abi_size 用来兜住布局变化。 */
/* 没有 hermes_abi_major()：ABI 版本由 `hermes version` 的 abi 字段报告。
 *
 * 为什么不在库里再放一个常量：两个来源必然漂移，而漂移的表象是
 * "命令行说 1.0、库说别的" —— 那种矛盾比不给 API 更难排查。 */


#ifdef __cplusplus
}
#endif

#endif /* HERMES_CORE_H */
