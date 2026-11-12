/*
 * hermes_abi.h —— Hermes 原生层唯一公开 C ABI 头文件。
 *
 * 这是全仓**唯一**允许被 Go(cgo)、C、Rust 三方共同看见的契约文件。
 * 修改本文件的任何内容都会改变 hm_abi_build_id()（头文件集合内容哈希），
 * 从而让 tests/guard 立刻变红 —— 这是刻意的。
 *
 * 所有权与生命周期规则（ARCH §4.4 的 R1–R8，逐条都是硬约束）：
 *   R1  Go 绝不让 C/Rust 保存 Go 指针；活过调用期的数据必须在不透明 handle 后面。
 *   R2  跨边界对象一律 uint64 handle：slot(低 32) | generation(高 32)，0 非法。
 *       generation 不匹配返回 HM_E_HANDLE_STALE（命名错误，不是 UB）。
 *   R3  长度在 ABI 层硬限制：sensitive ≤ 1 MiB、view ≤ 16 MiB、chain block ≤ 1 MiB。
 *       超限返回 HM_E_TOO_LARGE，**绝不截断**（截断会让哈希静默对不上）。
 *   R4  借用 vs 拥有：hm_view_borrow 的指针只在本次调用内有效；跨调用必须走
 *       hm_view_new / hm_sensitive_new（拥有 → handle）。
 *   R5  分配与释放同侧：C malloc 的只能 hm_spawn_res_free 释放；Rust 分配的只能
 *       hm_*_free 释放；Go 侧永不 free 他人内存。
 *   R6  阻塞调用（hm_spawn_wait）只允许出现在专用 goroutine，并发度受 bulkhead 上限
 *       约束；main 启动时 debug.SetMaxThreads(bulkheadTotal + GOMAXPROCS + 16)。
 *   R7  C → Go 无回调；Rust 不回调 C。所有通知走返回值或 fd。
 *   R8  版本：hm_abi_version() 的 major 必须相等；每个 struct 首字段 abi_size，
 *       接收方按 min(自身大小, 传入大小) 读写，永不按传入大小写。
 *
 * 版本演进规则（ARCH §11.5）：
 *   1. 只导出本文件声明的符号；编译加 -fvisibility=hidden。
 *   2. 永不修改已有函数签名或结构体字段的顺序/类型/含义。新增只能：
 *      加 _v2 函数，或在 *_res 尾部加字段并同时提升 abi_size。
 *   3. 任何 ABI 变更必须同时更新 docs/ABI.md 与 scripts/expected_symbols.txt。
 */

#ifndef HERMES_ABI_H
#define HERMES_ABI_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

/* ------------------------------------------------------------------ *
 * 版本
 * ------------------------------------------------------------------ */

/*
 * 2.0：**布局没变，承诺变了**。
 *
 * 结构体字段、函数签名、导出符号与 1.0 逐字节相同，所以这不是一次"加字段"
 * 意义上的 break。升号的是**跨过这道边界的字节由哪一版 IR 解释**这件事：
 * 2.0.0 的 IR 用 `decl` / `const` / `fallback` 描述程序，`ir_version` 从 1 升到 2
 * 且明确不做向前兼容（见 internal/ir/validate.go 的 checkVersions）。
 * 1.x 的运行时库按旧字段语义去读 v2 的 IR，得到的是"看起来对的错答案"。
 *
 * `.hbc` 成员的 `abi_version` 与 IR 里 `requires[].abi_version` 描述的就是
 * "这份产物消费哪一版 IR 形状"，所以它们必须一起变成 2（唯一事实源：
 * internal/archive 的 ABIVersionSupported，link 与两个 emitter 都引用它）。
 * 继续写 1 的唯一后果是让 `abi_version=1` 这句话变成假话。
 *
 * minor 仍是 0：ABI 的 minor 从不被解释成"兼容"，任何变化都要动 major
 * （`abi_version` 要求**完全相等**，见 docs/IR.md §9.5 与 docs/C-ABI.md）。
 */
#define HM_ABI_MAJOR 2
#define HM_ABI_MINOR 0

/* hm_abi_version() 的打包方式：(major << 16) | minor */
#define HM_ABI_MAKE_VERSION(major, minor) \
  ((uint32_t)(((uint32_t)(major) << 16) | ((uint32_t)(minor) & 0xffffu)))

/* 默认可见性：构建使用 -fvisibility=hidden，只有带 HM_EXPORT 的符号被导出。 */
#if defined(_WIN32)
#define HM_EXPORT
#else
#define HM_EXPORT __attribute__((visibility("default")))
#endif

/* ------------------------------------------------------------------ *
 * 状态码
 *
 * 0 = HM_OK；负数为错误。与 internal/errs 的 Go 侧常量由
 * tools/gen_errcodes 双向校验（T30）；在那之前，两侧的数值以本文件为准。
 * ------------------------------------------------------------------ */

typedef int32_t hm_status;

enum {
  HM_OK = 0,
  HM_E_INVALID_ARG = -1,
  HM_E_ABI_VERSION = -2,
  HM_E_HANDLE_STALE = -3,
  HM_E_TOO_LARGE = -4,
  HM_E_NOMEM = -5,
  HM_E_IO = -6,
  HM_E_SPAWN = -10,
  HM_E_DEADLINE = -11,
  HM_E_SANDBOX_DENIED = -12,
  HM_E_KILLED = -13,
  HM_E_RLIMIT_SETUP = -14,
  HM_E_PANIC = -15
};

/* ------------------------------------------------------------------ *
 * Handle
 *
 * 0 永远非法。低 32 位是槽位下标，高 32 位是 generation。
 * ------------------------------------------------------------------ */

typedef uint64_t hm_handle;

#define HM_HANDLE_INVALID ((hm_handle)0)

/* ------------------------------------------------------------------ *
 * 长度硬上限（R3）
 * ------------------------------------------------------------------ */

#define HM_SENSITIVE_MAX (1u << 20)   /* 1 MiB  —— 单个敏感缓冲 */
#define HM_VIEW_MAX (16u << 20)       /* 16 MiB —— 单个只读视图 */
#define HM_CHAIN_BLOCK_MAX (1u << 20) /* 1 MiB  —— 单个哈希块 */

/* ------------------------------------------------------------------ *
 * 版本与初始化
 * ------------------------------------------------------------------ */

HM_EXPORT uint32_t hm_abi_version(void);   /* (major << 16) | minor */
HM_EXPORT const char *hm_abi_build_id(void); /* 静态字符串，头文件集合内容哈希 */

/*
 * 校验 ABI 版本并初始化原生运行时。
 *
 * abi_expected 必须是调用方编译期看到的 hm_abi_version()。
 * major 不相等 → HM_E_ABI_VERSION（不提供"降级试试"这条路）。
 * flags 保留，必须为 0 → 否则 HM_E_INVALID_ARG。
 * 幂等：重复调用返回同一结果。
 */
HM_EXPORT hm_status hm_runtime_init(uint32_t abi_expected, uint64_t flags);
HM_EXPORT void hm_runtime_shutdown(void);

/* ------------------------------------------------------------------ *
 * Rust：敏感缓冲（R-NA-04）
 *
 * 语义：内容被 mlock 锁页（可能失败 → 降级），释放时先 zeroize 再归还槽位，
 * generation++。free 之后用旧 handle 访问返回 HM_E_HANDLE_STALE。
 * ------------------------------------------------------------------ */

/*
 * 从 bytes 复制 len 字节建立敏感缓冲。
 * no_mlock != 0 时跳过锁定（用于测试降级路径）。
 * 返回 HM_E_TOO_LARGE 当 len > HM_SENSITIVE_MAX（绝不截断）。
 */
HM_EXPORT hm_status hm_sensitive_new(const uint8_t *bytes, size_t len,
                                     int32_t no_mlock, hm_handle *out);
HM_EXPORT hm_status hm_sensitive_len(hm_handle h, size_t *out_len);

/* 把内容**复制**到 dst（契约允许复制；这是唯一合法的读取方式）。 */
HM_EXPORT hm_status hm_sensitive_read(hm_handle h, uint8_t *dst, size_t cap,
                                      size_t *out_written);

/* out_locked = 1 表示该缓冲当前确实被 mlock 锁页；0 表示降级。
 * 调用方在拿到 0 时必须写 trace 事件 security.mlock_unavailable。 */
HM_EXPORT hm_status hm_sensitive_locked(hm_handle h, int32_t *out_locked);

/* zeroize 后归还 slot，generation++。 */
HM_EXPORT hm_status hm_sensitive_free(hm_handle h);

/* ------------------------------------------------------------------ *
 * Rust：只读零拷贝视图
 * ------------------------------------------------------------------ */

HM_EXPORT hm_status hm_view_new(const uint8_t *bytes, size_t len, hm_handle *out);

/* 切片共享同一份底层分配（不复制）；越界 → HM_E_INVALID_ARG。 */
HM_EXPORT hm_status hm_view_slice(hm_handle v, size_t off, size_t len,
                                  hm_handle *out);

/*
 * 借用：*out_ptr 只在本次调用内有效，C 侧禁止保存（规则 R4）。
 * 注意出参类型是 const uint8_t **——本 ABI **不存在**任何返回可变指针的函数。
 */
HM_EXPORT hm_status hm_view_borrow(hm_handle v, const uint8_t **out_ptr,
                                   size_t *out_len);

HM_EXPORT hm_status hm_view_free(hm_handle v);

/* ------------------------------------------------------------------ *
 * Rust：哈希链（R-TR-04），无状态
 *
 *   block_hash(prev, index, block) =
 *       SHA-256( DOMAIN_BYTES ‖ 0x00 ‖ prev[32] ‖ u64le(index)
 *                ‖ u64le(len(block)) ‖ block )
 *   final_hash(prev, count, total_len) =
 *       SHA-256( DOMAIN_BYTES ‖ 0x00 ‖ prev[32] ‖ u64le(count)
 *                ‖ u64le(total_len) ‖ b"hermes.trace.final" )
 *   DOMAIN_STRING = "hermes.trace.v1"
 *   创世块 prev = 32 个 0 字节。
 *
 * 域分隔串参与哈希：改它，schemas/golden/chain-*.json 立刻对不上。
 * 详见 docs/CANONICAL.md。
 * ------------------------------------------------------------------ */

#define HM_CHAIN_DOMAIN "hermes.trace.v1"
#define HM_CHAIN_FINAL_TAG "hermes.trace.final"

/* len > HM_CHAIN_BLOCK_MAX → HM_E_TOO_LARGE（绝不截断）。 */
HM_EXPORT hm_status hm_chain_hash(const uint8_t prev[32], uint64_t index,
                                  const uint8_t *block, size_t len,
                                  uint8_t out[32]);
HM_EXPORT hm_status hm_chain_final(const uint8_t prev[32], uint64_t count,
                                   uint64_t total_len, uint8_t out[32]);

/* ------------------------------------------------------------------ *
 * C + 汇编：单调硬件计数器（R-NA-02 / DECISIONS D-002）
 *
 * 语义：免 syscall 的单调计数器（arm64 cntvct_el0 / x86-64 rdtscp），
 * 是"不受 NTP 步进影响的墙钟"，**不是 CPU 时间**。
 * ------------------------------------------------------------------ */

HM_EXPORT hm_status hm_cycle_now(uint64_t *out);
HM_EXPORT hm_status hm_cycle_freq(uint64_t *out_hz); /* 归一化为 Hz */
HM_EXPORT hm_status hm_cycle_arch(int32_t *out);     /* 1=arm64 2=x86_64 */

#define HM_ARCH_ARM64 1
#define HM_ARCH_X86_64 2

/* ------------------------------------------------------------------ *
 * C：进程督导（R-NA-01）
 * ------------------------------------------------------------------ */

/* 沙箱开关位 */
#define HM_SB_NO_NET (1u << 0)        /* 禁网络 */
#define HM_SB_READONLY_TMP (1u << 1)  /* /tmp 只读 */
#define HM_SB_NO_NEW_PRIVS (1u << 2)  /* 禁止提权 */

typedef struct hm_spawn_req {
  /* 首字段：调用方填 sizeof(hm_spawn_req)，被调方按 min 读（R8）。 */
  uint32_t abi_size;

  /*
   * hermes-shim 的绝对路径。
   * NULL = 让 C 侧自己找（同目录 hermes-shim）。生产路径由 Go 显式传入，
   * 因为 T08 要求 "HERMES_SHIM 指向不存在路径时错误信息含期望路径"，
   * 这个错误必须在 Go 侧构造。（相对 ARCH §4.2 的新增字段，见 docs/ABI.md。）
   */
  const char *shim;

  const char *exe;         /* 目标可执行文件绝对路径；argv[0] 由调用方给 */
  const char *const *argv; /* 以 NULL 结尾；argv[0] 通常等于 exe */
  const char *const *envp; /* NULL = **空环境**（不是继承） */
  const char *cwd;         /* NULL = 继承调用方 cwd */

  /*
   * deadline_cycles 是**相对**时长（从 spawn 返回后开始计），单位是
   * hm_cycle_now() 的刻度。0 = 无 deadline。
   * wall_backstop_ms 只在计数器不可用时兜底；触发时 deadline_source="wall"。
   */
  uint64_t deadline_cycles;
  uint64_t wall_backstop_ms;

  uint64_t rl_as_bytes; /* 0 = 不设置 */
  uint64_t rl_cpu_sec;
  uint64_t rl_nofile;
  uint64_t rl_nproc;
  uint64_t rl_fsize;

  uint32_t kill_grace_ms; /* SIGTERM → 等待 → SIGKILL(-pgid)；0 = 默认 500 */
  uint32_t sandbox_flags; /* HM_SB_* 位或 */

  const uint8_t *stdin_bytes; /* 写完即关闭子进程 stdin；NULL = /dev/null */
  size_t stdin_len;

  size_t stdout_cap; /* 0 = 默认 1 MiB；超出部分丢弃并把长度夹到 cap */
  size_t stderr_cap;
} hm_spawn_req;

typedef struct hm_spawn_res {
  /* 调用方必须先填 sizeof(hm_spawn_res)；被调方按 min 写（R8）。 */
  uint32_t abi_size;

  int32_t exited;     /* 1 = 正常退出（exit_code 有效） */
  int32_t exit_code;
  int32_t signaled;   /* 1 = 被信号终止 */
  int32_t term_signal;/* WTERMSIG(status) */
  int32_t escalated;  /* 1 = 我们升级到了 SIGKILL */
  int32_t deadline_hit; /* 1 = 因 deadline 或 wall 兜底而杀（主动 cancel 看 killed_pgid） */
  int32_t killed_pgid;  /* 被杀掉的进程组；0 = 没杀过 */

  const char *deadline_source; /* "cycle" | "wall" | "" （静态字符串） */
  uint64_t fired_cycles;       /* 触发时的计数器读数 */

  int32_t setup_errno; /* shim 通过 fd 3 回报的 setup 失败 errno；0 = 成功 */

  /* 由 C malloc，必须 hm_spawn_res_free 释放（R5）。 */
  uint8_t *stdout_bytes;
  size_t stdout_len;
  uint8_t *stderr_bytes;
  size_t stderr_len;
} hm_spawn_res;

HM_EXPORT hm_status hm_spawn(const hm_spawn_req *req, hm_handle *out_proc);

/* 线程安全，可从任意 goroutine 调；立即向 -pgid 发 SIGTERM。 */
HM_EXPORT hm_status hm_spawn_cancel(hm_handle proc);

/* 阻塞直到子进程被收割；结果所有权转给调用方（用 hm_spawn_res_free 释放）。 */
HM_EXPORT hm_status hm_spawn_wait(hm_handle proc, hm_spawn_res *out);

HM_EXPORT void hm_spawn_res_free(hm_spawn_res *res);

/* 归还进程槽位，generation++。必须在 wait 之后调用。 */
HM_EXPORT hm_status hm_spawn_free(hm_handle proc);

/* ------------------------------------------------------------------ *
 * 调试钩子（T03 的零化验证）
 *
 * 生产代码路径**不调用**它；tests/guard 有一条测试断言除 tests/ 与
 * internal/native 的 _test.go 之外没有文件引用 hm_debug_peek。
 * 语义：把最近一次 hm_sensitive_free 释放的那块内存（已 zeroize）的字节
 * 复制到 dst。没有历史记录时返回 HM_E_INVALID_ARG。
 * 上限同样是 HM_SENSITIVE_MAX。
 * ------------------------------------------------------------------ */

HM_EXPORT hm_status hm_debug_peek(uint8_t *dst, size_t cap, size_t *out_len);

/* ------------------------------------------------------------------ *
 * 仅供 hermes-shim 自身与 hm_internal.h 使用（不在导出面里）
 * ------------------------------------------------------------------ */

#ifdef __cplusplus
} /* extern "C" */
#endif

#endif /* HERMES_ABI_H */
