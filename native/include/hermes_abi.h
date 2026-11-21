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
 *   R5  分配与释放同侧：缓冲区始终由 C 侧的槽位持有（hm_proc_copy_* 只做 memcpy，
 *       调用方从不接手所有权），因此**没有**任何 free 函数需要配对；Rust 分配的
 *       只能 hm_*_free 释放；Go 侧永不 free 他人内存。
 *   R6  阻塞调用（hm_proc_wait）只允许出现在专用 goroutine，并发度受 bulkhead 上限
 *       约束；main 启动时 debug.SetMaxThreads(bulkheadTotal + GOMAXPROCS + 16)。
 *   R7  C → Go 无回调；Rust 不回调 C。所有通知走返回值或 fd。
 *   R8  版本：hm_abi_version() 的 major 必须相等；每个 struct 首字段 abi_size，
 *       接收方按 min(自身大小, 传入大小) 读写，永不按传入大小写。
 *
 * 版本演进规则（ARCH §11.5）：
 *   1. 只导出本文件声明的符号；编译加 -fvisibility=hidden。
 *   2. 永不删除、永不改名、永不修改已有导出函数的签名，永不修改已有结构体
 *      字段的顺序/类型/含义。新增只能走以下四条：
 *        (a) 加 `_v2` 函数；
 *        (b) 在 `*_res` 尾部加字段并同时提升 abi_size；
 *        (c) 新增**不接收也不返回布局结构体**的导出函数（`hm_proc_get_*`
 *            / `hm_proc_copy_*` 一族，出参全是标量或调用方提供的缓冲区）；
 *        (d) 在 `*_req` 尾部加字段并提升其 abi_size（旧调用方 abi_size 小
 *            ⇒ 新字段按"未指定"处理；**安全字段必须按"未指定 ⇒ 拒绝"处理**，
 *            范例见 native/c/win_spawn.c 的三道闸）。
 *      位面扩张（HM_SB_* / HM_TERM_* / HM_SETUP_* / HM_PROC_*）不算 ABI 变更，
 *      它是**值**的扩张，不影响任何布局。
 *      每个导出函数必须是 out-of-line 定义；用 static inline 或宏"实现"导出面
 *      视为 ABI 破裂（它会让"缺符号"不再在链接期暴露）。
 *   2b. 一次性例外（F-11b）。本例外**仅此一次**，且其成立条件只有一个：
 *      **已发布消费者集合为空**。
 *      被删除的符号，逐字：hm_spawn_wait、hm_spawn_res_free、
 *      hm_spawn_cancel、hm_spawn_free，以及类型 hm_spawn_res。
 *      唯一依据：scripts/release.sh:33-65 的发布包内容清单 —— 该区间只拷
 *      bin/hermes、bin/hermes-shim、lib/libhermesrt.a、lib/libhermesrt.{dylib,so}、
 *      lib/hermes_rt.h、include/hermes/{hermes.h,hermes_core.h}；
 *      **libhermes_native.a 与 native/include/hermes_abi.h 从不进入发布包**，
 *      include/hermes/ 下的两个头文件也不引用上述符号。因此破坏形态是
 *      编译/链接期硬失败。
 *      HM_ABI_MAJOR 保持 2（跨边界字节的**解释**未变），
 *      HM_ABI_BUILD_ID 必然变化（**形状**变了）。
 *      未来任何删除必须提交**同等强度**的证据（发布产物清单 + 全仓引用点扫描），
 *      否则只能走上面 (a)(b)(c)(d) 四条新增路径。
 *   3. 任何 ABI 变更必须同时更新 docs/C-ABI.md 与 scripts/expected_symbols.txt。
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
  HM_E_PANIC = -15,
  /* "结果还没算出来"。**不是** INVALID_ARG：那不是调用方的错。
   * F-11b 的行规：缺证据与证据缺席必须可区分，绝不用 0 冒充。 */
  HM_E_NOT_READY = -16
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
 * C：进程督导（R-NA-01）—— 不透明句柄 + Getter 形态（F-11b）
 *
 * 契约：hm_spawn 之后，"这次调用发生了什么"**只能**通过 getter 从 handle
 * 读出。没有任何结构体承载返回值。加一个新安全信号 = 加一个 bit +
 * （罕见时）加一个 getter；绝不动布局、绝不改既有签名。
 * R8 的措辞已同步修订，见文件头。
 *
 * 本文档的每个 hm_proc_* 都是**真实导出函数**（HM_EXPORT + out-of-line
 * 定义，定义在 native/c/supervisor.c 与 native/c/win_spawn.c）。
 * **禁止**把它们写成 static inline 或宏：那样不会产生导出符号，
 * "新 Go + 旧库" 就从**链接期硬失败**退化成**静默误读**，F-11 的
 * fail-open 洞会原地重开。这条是硬约束，有守卫测试。
 * ------------------------------------------------------------------ */

/* 沙箱开关位（请求面）。已用 3 位，剩余 29 位是未来安全信号唯一的
 * 请求通道：加信号 = 加一个 #define，不需要新符号、不需要改结构体。 */
#define HM_SB_NO_NET (1u << 0)        /* 禁网络 */
#define HM_SB_READONLY_TMP (1u << 1)  /* /tmp 只读 */
#define HM_SB_NO_NEW_PRIVS (1u << 2)  /* 禁止提权 */

/* 句柄状态（hm_proc_get_state 的取值）。**0 是非法值，getter 永不写出 0。** */
#define HM_PROC_RUNNING 1 /* spawn 已返回，hm_proc_wait 尚未完成 */
#define HM_PROC_REAPED 2  /* wait 已完成，所有 getter 可读 */
/*
 * 没有 HM_PROC_FREED，这是刻意的：free 之后 generation 自增，句柄变成
 * "一个过期的名字"，所有 getter 返回 HM_E_HANDLE_STALE。要把"已释放"
 * 表达成一个可读状态，需要一张永不清理的墓碑表 —— 那是无界内存。
 * R2 的 generation 已经用 O(1) 空间表达了同一件事，且把"对象不存在"
 * 正确地归类为**错误**而不是**状态**。
 */

/* 终止面（hm_proc_get_term 的 out_bits）。四者的来源是**同一枚 waitpid
 * 状态字**，所以由同一个函数一次读出，避免跨调用撕裂。 */
#define HM_TERM_EXITED (1u << 0)
#define HM_TERM_SIGNALED (1u << 1)
#define HM_TERM_ESCALATED (1u << 2)
#define HM_TERM_DEADLINE_HIT (1u << 3)

/* setup 面（hm_proc_get_setup 的 out_bits）。前三条互斥/互不蕴含：
 *   FAILED      = shim 写了 [HMIF][errno]      （setup 阶段自己失败）
 *   EXEC_FAILED = shim 写了裸 [errno]          （execve 失败，目标从未启动）
 *   EXEC_CONFIRMED = fd3 EOF 时子进程仍存活     （execve 确实发生了）
 * 定义见 hm_internal.h 的 fd3 协议节。
 *
 * CHANNEL_MEASURED 是"本次 spawn 具备 fd3 setup 通道"的事实位：
 *   - POSIX 恒置位（native/c/exec.c 填槽位时按 p->setup_fd >= 0 设置，
 *     **置一次并粘住**，之后只读不写 —— pump_setup 在 EOF 时会 close 掉
 *     setup_fd 并置 -1，若 getter 现算就会在 wait 之后恒得 0）；
 *   - Windows 恒不置位（无 hermes-shim）。
 * 它把"**没测量**"与"**测出来是 0**"分开 —— 没有它，Windows 上
 * EXEC_CONFIRMED 恒 0 会被误读成"execve 没发生"。这是本设计
 * "缺证据 ≠ 证据缺席"原则在平台差异上的落地。 */
#define HM_SETUP_FAILED (1u << 0)
#define HM_SETUP_EXEC_FAILED (1u << 1)
#define HM_SETUP_EXEC_CONFIRMED (1u << 2)
#define HM_SETUP_CHANNEL_MEASURED (1u << 3)

/* ⚠ **EXEC_CONFIRMED 缺失 ≠ 目标没有 execve。** 它只等于"我们没观测到"。
 *
 * 这个位是父进程**采样**出来的，不是内核通知、也不是 shim 明示：
 * native/c/supervisor.c 的等待循环每 `sleep_ms(2)`（:550）跑一次
 * pump_setup(:454) → 读 setup_eof(:469) → waitpid(WNOHANG)(:471)，只有采样点
 * 恰好落在"fd3 已关、子进程还没死"这个窗口里才置位。快工具的
 * `execve → exit` 常常整个落在同一个 2ms tick 内，于是**跑完了也确认不到**：
 * 真实 hermes-shim 下 `/bin/echo hi` 跨轮 **0/20 ~ 18/20**（同一目标、同一构建，
 * 不同轮次能差一个数量级；`/usr/bin/true` 是 0~2/20，`/bin/sh -c 'exit 40'` 恒
 * 20/20），而 `Wait()` 一次都没报错。**波动本身就是"采样推断"的证据** ——
 * 它承载不了"没执行"这种事实，只能承载"没观测到"。所以：
 *   - 允许的用法：**归因**（"这次没确认到 exec"），且只在
 *     `requested & 关键位 != 0` 时才需要用它；
 *   - 禁止的用法：当成"目标未执行"的功能性事实（计费、审计、"工具跑没跑"）。
 *     那需要协议级 fact（pre-exec 记录），见 docs/KNOWN-GAPS.md 的 F-11c 条目。
 * 这正是判据要求 `MEASURED && !EXEC_CONFIRMED && Signaled` 三合一的原因：
 * 只有"被信号杀死"这一种情形，"没确认到"才等价于"没跑"。
 */

typedef struct hm_spawn_req {
  /* 首字段：调用方填 sizeof(hm_spawn_req)，被调方按 min 读（R8）。 */
  uint32_t abi_size;

  /*
   * hermes-shim 的绝对路径。
   * NULL = 让 C 侧自己找（同目录 hermes-shim）。生产路径由 Go 显式传入，
   * 因为 T08 要求 "HERMES_SHIM 指向不存在路径时错误信息含期望路径"，
   * 这个错误必须在 Go 侧构造。（相对 ARCH §4.2 的新增字段，见 docs/C-ABI.md。）
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

HM_EXPORT hm_status hm_spawn(const hm_spawn_req *req, hm_handle *out_proc);

/* 阻塞直到子进程被收割。结果留在槽位里，用 getter 读，hm_proc_free 释放。
 * R6 的线程约束不变（只允许专用 goroutine）。前一次未返回时重入返回
 * HM_E_INVALID_ARG。返回 HM_OK 之后 hm_proc_get_state 才可能是 REAPED。 */
HM_EXPORT hm_status hm_proc_wait(hm_handle proc);

/* 线程安全，可从任意 goroutine 调；立即向 -pgid 发 SIGTERM。 */
HM_EXPORT hm_status hm_proc_cancel(hm_handle proc);

/* 归还槽位，generation++。
 *
 * **在 hm_proc_wait 之前调用是合法的**，语义等价于"取消 + 收尸"：向进程组
 * 发 SIGKILL 并 waitpid 收割，然后归还槽位，返回 HM_OK。之所以不做成"顺序
 * 错误就报错"：Free 是清理动作，而最需要它的场景恰恰是调用方在 wait 之前就
 * 决定不要这个结果了（父 context 取消、提前 return、finalizer 兜底）。这时
 * 报错的唯一效果是调用方忽略 err、孤儿进程留在机器上。
 *
 * 真正的顺序错误只有一种：仍有 getter 在跑（refcnt > 0）或 hm_proc_wait 还在
 * 另一个线程里阻塞 —— 此时清槽位就是 use-after-free，返回 HM_E_INVALID_ARG。
 *
 * **幂等**：对**已释放**（或压根不是本表签发的）句柄调用本函数是**无操作**，
 * 返回 HM_OK —— 语义上"没有东西可释放"不是错误。这与其它入口刻意相反：
 * hm_proc_wait / hm_proc_cancel / 全部 getter 对同一句柄返回
 * HM_E_HANDLE_STALE，因为"读一个已经不在的东西"绝不能返回零值当答案
 * （见上面 HM_PROC_* 那段的墓碑表讨论）。两者放在一起才是完整的：**清理可以
 * 重复，读取不可以**。finalizer 与显式 defer 会各调一次 free，报错的唯一效果
 * 是被忽略。 */
HM_EXPORT hm_status hm_proc_free(hm_handle proc);

/* ---- Getter：全部幂等、只读、out-of-line、在 wait 之后 free 之前有效 ---- */

/* out_state ∈ {HM_PROC_RUNNING, HM_PROC_REAPED}。 */
HM_EXPORT hm_status hm_proc_get_state(hm_handle proc, int32_t *out_state);

/* 一次读走 waitpid 状态字派生出的全部 4 个值。实现必须先把 4 个值
 * 取到局部变量，再统一写出 —— 不得边读边写。 */
HM_EXPORT hm_status hm_proc_get_term(hm_handle proc, uint32_t *out_bits,
                                     int32_t *out_exit_code,
                                     int32_t *out_term_signal,
                                     int32_t *out_killed_pgid);

HM_EXPORT hm_status hm_proc_get_deadline(hm_handle proc,
                                         uint64_t *out_fired_cycles);

/*
 * 字符串拷贝（**注意：与下面字节流拷贝的容量语义不同**）：
 *   *out_len 在 HM_OK 时 = 字符串长度，**不含**结尾 '\0'（与 strlen 一致）；
 *   需要的 cap = *out_len + 1（因为要拷那一个 '\0'）；
 *   buf != NULL && cap < *out_len + 1
 *       → 返回 HM_E_TOO_LARGE，*out_len 写"**所需 cap**"（即 len + 1），
 *         buf **一个字节都不写**（R3：绝不截断）；
 *   buf == NULL
 *       → 只写 *out_len（字符串长度），返回 HM_OK。
 * 两种路径下 *out_len 的含义不同（成功=字符数，TOO_LARGE=所需 cap），
 * 调用方**必须按错误码分支取用**，不得混用。Go 封装已按此分支，不泄漏。
 */
HM_EXPORT hm_status hm_proc_copy_deadline_source(hm_handle proc, char *buf,
                                                 size_t cap, size_t *out_len);

HM_EXPORT hm_status hm_proc_get_setup(hm_handle proc, uint32_t *out_bits,
                                      int32_t *out_setup_errno);

HM_EXPORT hm_status hm_proc_get_sb_requested(hm_handle proc, uint32_t *out_flags);
HM_EXPORT hm_status hm_proc_get_sb_acked(hm_handle proc, uint32_t *out_flags);
HM_EXPORT hm_status hm_proc_get_sb_ignored(hm_handle proc, uint32_t *out_flags);

/*
 * 字节流两阶段拷贝（**不适用**上面那条 +1 规则：字节流没有终止符）：
 *   out_len == NULL            → HM_E_INVALID_ARG，不写任何东西
 *   buf == NULL                → 只写 *out_len = 需要的字节数，返回 HM_OK
 *   buf != NULL && cap < 需要   → *out_len = 需要，HM_E_TOO_LARGE，buf 不写
 *   否则                        → memcpy，*out_len = 需要，返回 HM_OK
 * *out_len 在**所有**路径上都等于同一个意思：需要的/已写的字节数。
 *
 * R5 的"跨分配器 free"规则由此**整个消失**：内存始终在 C 侧槽位里，
 * 调用方只往里 memcpy，从不接手所有权，也没有任何 free 函数要配对。
 *
 * state != REAPED 时一律 HM_E_NOT_READY，且 *out_len 不写。
 */
HM_EXPORT hm_status hm_proc_copy_stdout(hm_handle proc, uint8_t *buf, size_t cap,
                                        size_t *out_len);
HM_EXPORT hm_status hm_proc_copy_stderr(hm_handle proc, uint8_t *buf, size_t cap,
                                        size_t *out_len);

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
