// 纯 C 版 std/runtime 预算超限检查（**非 Go 实现**）。
//
// 与 Go 版（gruntime，internal/budget.exceeds）语义逐字一致：
//   · 三维预算（in / out / micro-USD）独立判断；
//   · <=0 的维度视为不限；
//   · 任一维度加上 up 越过 lim 即返回 1。
//
// 这是 g*/G* 机制里"非 Go 版"的具体落地：选 variant=c 时，internal/budget
// 的预算检查走这里而不是 Go 实现，从而不拖 Go runtime。
//
// 注意：本文件刻意不放在 native/include/ 下——那里是 4 个冻结头，
// 受 ABI_BUILD_ID 哈希守卫，改一个字节都会逼 native/rust/src/lib.rs 重算。
#ifndef HERMES_RTBUDGET_H
#define HERMES_RTBUDGET_H

#ifdef __cplusplus
extern "C" {
#endif

// 返回 1 表示"加上 up 会越过 lim"，0 表示不越过。
int hermes_rt_budget_exceeded(long ui, long uo, long um,
                              long pi, long po, long pm,
                              long li, long lo, long lm);

// 测试可见计数器：C 版每被调用一次 +1，用于证明"选非 Go 版真走到非 Go 实现"。
long hermes_rt_budget_c_calls(void);

#ifdef __cplusplus
}
#endif
#endif
