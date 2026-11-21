#ifndef HERMES_NET_H
#define HERMES_NET_H

/* S2: libhermesnet —— 编译产物调用外部世界（工具 / 模型）的批量 C ABI。
 *
 * 为什么是"批量 ABI"而不是回调：cgo 跨界一次约 1 µs，只能定在"一次算子/工具
 * 调用"的粒度，绝不进 trace / budget / 每 token 的热路径（NATIVE-BACKEND §3）。
 *
 * 约定：
 *   - req_json 是 UTF-8 JSON（不含 NUL），req_len 是其字节数。
 *   - 成功（返回 0）：*out 指向结果 JSON，调用方用 hermes_net_free 释放。
 *   - 失败（返回非 0）：*err 指向错误文本，调用方同样用 hermes_net_free 释放。
 *   - 状态码与 hermes_status 同义：0=OK 1=Contract 2=Network 3=Http 4=Model
 *     5=Circuit 8=Internal。编译产物据此置错误槽，供 try/catch/fallback 分派。
 */

#include <stddef.h>

#ifdef __cplusplus
extern "C" {
#endif

int hermes_net_tool(const char* req_json, size_t req_len, char** out, char** err);
int hermes_net_model(const char* req_json, size_t req_len, char** out, char** err);
void hermes_net_free(char* p);

#ifdef __cplusplus
}
#endif

#endif /* HERMES_NET_H */
