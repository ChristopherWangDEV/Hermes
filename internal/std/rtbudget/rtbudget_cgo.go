// Package rtbudget 是 std/runtime 预算检查的**纯 C 非 Go 实现**的 cgo 桥。
//
// 当 App 层选择 variant=c（HERMES_STD_VARIANT=c）时，internal/budget 的预算
// 超限判断改走这里的 C 实现（hermes_rt_budget_exceeded），从而不拖 Go 版 gruntime
// 的逻辑。默认 variant=go 时 internal/budget 仍走 Go 实现，行为与该 C 实现逐字一致
// （见 rtbudget.c 与 internal/budget.exceeds）。
//
// 这是 g*/G* 机制里"非 Go 版标准库"的具体落地之一：std/runtime 的预算引擎有
// 一个 Go 实现（gruntime，= internal/budget）和一个纯 C 实现（本包），由 App 层
// 通过 HERMES_STD_VARIANT 在运行期二选一。
package rtbudget

/*
#include "rtbudget.h"
*/
import "C"

// Exceeded 报告"used+up 是否越过 lim"。参数顺序：used(三维) → up(三维) → lim(三维)。
// <=0 的 lim 维度视为不限。语义与 internal/budget.exceeds 逐字一致。
func Exceeded(usedIn, usedOut, usedUSD, upIn, upOut, upUSD, limIn, limOut, limUSD int64) bool {
	return C.hermes_rt_budget_exceeded(
		C.long(usedIn), C.long(usedOut), C.long(usedUSD),
		C.long(upIn), C.long(upOut), C.long(upUSD),
		C.long(limIn), C.long(limOut), C.long(limUSD),
	) != 0
}

// Calls 返回 C 版预算检查被调用的累计次数。
// 仅用于测试证明"选非 Go 版时真走到了非 Go 实现"（计数器只在 C 版被调用时自增）。
func Calls() int64 {
	return int64(C.hermes_rt_budget_c_calls())
}
