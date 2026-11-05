/*
 * cycle.c —— 单调硬件计数器的平台分派与降频归一化（R-NA-02 / D-002）。
 *
 * 语义提醒（DECISIONS D-002，必须照抄进任何对外表述）：
 *   hm_cycle_now() 返回的是**免 syscall 的单调计数器**（arm64 cntvct_el0 /
 *   x86-64 rdtscp）。它是"不受 NTP 步进影响的墙钟"，**不是 CPU 时间**。
 *   macOS 不允许用户态读 PMCCNTR_EL0，因此在目标平台上无法实现真正的
 *   CPU 时间计量。任何"降频后仍由计数触发"的验收在 macOS 上都是错的。
 *
 * 两个 arch 分支都必须能被编译到（R-NF-03）：
 *   · 宿主机分支由 build.sh 真实汇编并参与链接；
 *   · 另一分支由 build.sh 用 -DHM_FORCE_ARCH_* 做 -fsyntax-only 检查，
 *     同时用 -arch 交叉汇编对应的 .S。
 *   用 -D__x86_64__ 强行改宏会污染系统头，所以这里提供显式的 HM_FORCE_ARCH_*
 *   开关，两者完全等价。
 */

/* glibc 在严格 ISO 模式（-std=c11）下**默认不暴露 POSIX 符号**：
 * `clock_gettime` 与 `CLOCK_MONOTONIC` 会变成"未声明"，而 macOS 的头文件
 * 无条件暴露它们 —— 于是同一份代码在 macOS 上编得过、在 Linux 上编不过。
 * 必须在**任何 include 之前**定义这个宏，之后包含才生效。 */
#if defined(__linux__) && !defined(_POSIX_C_SOURCE)
#define _POSIX_C_SOURCE 199309L
#endif

#include "hm_internal.h"

#include <errno.h>
#include <stdint.h>
#include <time.h>

#if defined(HM_FORCE_ARCH_X86_64) || (defined(__x86_64__) && !defined(__aarch64__))
#define HM_ARCH_TARGET_X86_64 1
#elif defined(HM_FORCE_ARCH_ARM64) || defined(__aarch64__)
#define HM_ARCH_TARGET_ARM64 1
#else
#error "unsupported architecture: hermes needs arm64 or x86_64"
#endif

/* 由 native/c/cycle_arm64.S / cycle_x86_64.S 提供。 */
uint64_t hermes_hw_counter(void);

#if defined(HM_ARCH_TARGET_ARM64)
uint64_t hermes_hw_counter_freq_asm(void); /* 读 cntfrq_el0 */
#endif

static uint64_t g_freq_cache = 0; /* 0 = 尚未标定 */

#if defined(HM_ARCH_TARGET_X86_64)
/*
 * x86-64 没有直读 TSC 频率的寄存器。用一段 ~20 ms 的忙等采样折算。
 * 这段代码在 macOS/arm64 上只做语法检查（不运行），因此不追求精度：
 * 它的唯一用途是把"计数器刻度"折算成"deadline 毫秒"。
 */
static uint64_t hermes_calibrate_tsc(void) {
  struct timespec a;
  struct timespec b;
  uint64_t c0;
  uint64_t c1;
  long long elapsed_ns = 0;

  if (clock_gettime(CLOCK_MONOTONIC, &a) != 0) {
    return 0;
  }
  c0 = hermes_hw_counter();
  do {
    if (clock_gettime(CLOCK_MONOTONIC, &b) != 0) {
      return 0;
    }
    elapsed_ns = (long long)(b.tv_sec - a.tv_sec) * 1000000000LL +
                 (long long)(b.tv_nsec - a.tv_nsec);
  } while (elapsed_ns < 20000000LL);
  c1 = hermes_hw_counter();

  if (elapsed_ns <= 0 || c1 <= c0) {
    return 0;
  }
  /* 频率 = Δcycles / Δs = Δcycles * 1e9 / Δns */
  return (uint64_t)(((double)(c1 - c0) * 1e9) / (double)elapsed_ns);
}
#endif /* HM_ARCH_TARGET_X86_64 */

uint64_t hermes_hw_counter_freq(void) {
  uint64_t cached = __atomic_load_n(&g_freq_cache, __ATOMIC_ACQUIRE);
  uint64_t f;

  if (cached != 0) {
    return cached;
  }

#if defined(HM_ARCH_TARGET_ARM64)
  f = hermes_hw_counter_freq_asm();
#else
  f = hermes_calibrate_tsc();
#endif

  if (f == 0) {
    return 0; /* 未知：调用方必须退到 wall backstop */
  }
  __atomic_store_n(&g_freq_cache, f, __ATOMIC_RELEASE);
  return f;
}

hm_status hm_cycle_now(uint64_t *out) {
  if (out == NULL) {
    return HM_E_INVALID_ARG;
  }
  *out = hermes_hw_counter();
  return HM_OK;
}

hm_status hm_cycle_freq(uint64_t *out_hz) {
  uint64_t f;

  if (out_hz == NULL) {
    return HM_E_INVALID_ARG;
  }
  f = hermes_hw_counter_freq();
  if (f == 0) {
    *out_hz = 0;
    return HM_E_IO;
  }
  *out_hz = f;
  return HM_OK;
}

hm_status hm_cycle_arch(int32_t *out) {
  if (out == NULL) {
    return HM_E_INVALID_ARG;
  }
#if defined(HM_ARCH_TARGET_ARM64)
  *out = HM_ARCH_ARM64;
#else
  *out = HM_ARCH_X86_64;
#endif
  return HM_OK;
}
