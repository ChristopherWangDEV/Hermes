// 纯 C 版 std/runtime 预算检查的实现（见 rtbudget.h）。
#include "rtbudget.h"

static long c_calls = 0;

int hermes_rt_budget_exceeded(long ui, long uo, long um,
                              long pi, long po, long pm,
                              long li, long lo, long lm) {
    c_calls++;
    if (li > 0 && ui + pi > li) return 1;
    if (lo > 0 && uo + po > lo) return 1;
    if (lm > 0 && um + pm > lm) return 1;
    return 0;
}

long hermes_rt_budget_c_calls(void) { return c_calls; }
