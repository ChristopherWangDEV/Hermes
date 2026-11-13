package native

/*
#include "hermes_abi.h"
*/
import "C"

import "fmt"

// Arch 标识计数器来自哪条指令序列。
type Arch int32

const (
	ArchUnknown Arch = 0
	ArchARM64   Arch = 1
	ArchX8664   Arch = 2
)

func (a Arch) String() string {
	switch a {
	case ArchARM64:
		return "arm64"
	case ArchX8664:
		return "x86_64"
	default:
		return "unknown"
	}
}

// CycleNow 读一次单调硬件计数器（arm64 cntvct_el0 / x86-64 rdtscp）。
//
// 语义（DECISIONS D-002，容易被误解，写清楚在这里）：
// 它是**免 syscall 的单调墙钟**，不是 CPU 时间。所以：
//   - 不受 NTP 步进影响，也不需要 gettimeofday 的系统调用；
//   - 但进程睡在 IO 上时它照样在走 —— 拿它当"CPU 占用"用是错的。
//
// 这也是为什么不能用"给进程降频，看超时是否仍按预算触发"来验收：
// 降频不改变 cntvct / TSC 的频率。
func CycleNow() (uint64, error) {
	var out C.uint64_t
	if err := toError(C.hm_cycle_now(&out)); err != nil {
		return 0, err
	}
	return uint64(out), nil
}

// CycleFreq 返回计数器归一化后的频率（Hz），用于把刻度换算成时长。
func CycleFreq() (uint64, error) {
	var out C.uint64_t
	if err := toError(C.hm_cycle_freq(&out)); err != nil {
		return 0, err
	}
	if out == 0 {
		// 频率为 0 会让所有时长换算退化成"立即超时"，必须当错误报出来，
		// 不能让它变成一个看起来正常的 0。
		return 0, fmt.Errorf("%w: cycle frequency reported as 0", ErrInvalidArgument)
	}
	return uint64(out), nil
}

// CycleArch 返回计数器来自哪条指令序列，用于 doctor 与 trace 元数据。
func CycleArch() (Arch, error) {
	var out C.int32_t
	if err := toError(C.hm_cycle_arch(&out)); err != nil {
		return ArchUnknown, err
	}
	return Arch(out), nil
}

// TicksForDuration 把时长（纳秒）换算成计数器刻度。
//
// 用整数运算并向上取整：向下取整会让极短的 deadline 变成 0，而 0 在
// hm_spawn_req 里表示"无 deadline" —— 一个本该立刻超时的调用会变成永不超时。
// 这是那种"看起来只是少了一个 tick"的 bug，实际是把熔断关掉了。
func TicksForDuration(nanos uint64) (uint64, error) {
	hz, err := CycleFreq()
	if err != nil {
		return 0, err
	}
	if nanos == 0 {
		return 0, nil
	}
	// ticks = ceil(nanos * hz / 1e9)
	const billion = uint64(1_000_000_000)
	prod := nanos * hz
	ticks := prod / billion
	if prod%billion != 0 {
		ticks++
	}
	if ticks == 0 {
		ticks = 1
	}
	return ticks, nil
}
