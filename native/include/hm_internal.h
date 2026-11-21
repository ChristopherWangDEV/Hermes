/*
 * hm_internal.h —— Hermes 原生层 **内部** 头，不导出、不被 Go 看见。
 *
 * 只有 native/c 下的 .c 文件与 hermes-shim 会包含它。放在这里的东西意味着
 * "不是 ABI 的一部分，可以随时改"。任何需要跨语言稳定的东西都必须
 * 放在 hermes_abi.h。
 */

/* ---- 平台特性宏：必须在**任何系统头之前**定义 ----------------------------
 *
 * glibc 在严格 ISO 模式（-std=c11）下默认不暴露 POSIX 符号，于是
 * clock_gettime / nanosleep / kill / sigemptyset 都会变成"未声明"。
 * macOS 的库头文件无条件暴露它们 —— 同一份代码因此会在 macOS 上编得过、
 * 在 Linux 上编不过。
 *
 * 放在公共头里而不是逐文件写：漏一个文件就多一轮"Linux 上又红一条"。
 */
#if defined(__linux__)
#ifndef _GNU_SOURCE
#define _GNU_SOURCE 1
#endif
#ifndef _POSIX_C_SOURCE
#define _POSIX_C_SOURCE 200809L
#endif
#endif

#ifndef HM_INTERNAL_H
#define HM_INTERNAL_H

#include <stddef.h>
#include <stdint.h>

#if defined(_WIN32)
/* Windows 没有 <sys/types.h> 与 pid_t。这里给出等价的 typedef：
 * 进程标识在 Win32 里是 DWORD，语义同样是"一个不透明整数"。 */
typedef unsigned long pid_t;
#else
#include <sys/types.h>
#endif

#include "hermes_abi.h"

/* ------------------------------------------------------------------ *
 * handle 打包（与 Rust 侧 handle.rs 的位布局必须一致）
 * ------------------------------------------------------------------ */

#define HM_SLOT_MASK 0xffffffffull

static inline hm_handle hermes_handle_pack(uint32_t slot, uint32_t gen) {
  return ((uint64_t)gen << 32) | (uint64_t)slot;
}

static inline uint32_t hermes_handle_slot(hm_handle h) {
  return (uint32_t)(h & HM_SLOT_MASK);
}

static inline uint32_t hermes_handle_gen(hm_handle h) {
  return (uint32_t)(h >> 32);
}

/* ------------------------------------------------------------------ *
 * hermes-shim 的约定（与 native/c/shim.c、supervisor.c 共享）
 *
 * fd 3 是 "setup 错误通道"：
 *   - setup（rlimit / 沙箱 / umask）全部成功 → shim 给自己 fd 3 打 FD_CLOEXEC
 *     然后 execve。execve 成功 → fd 3 被内核关闭 → 父进程读到 **EOF**。
 *   - setup 或 execve 失败 → shim 向 fd 3 写 4 字节小端 errno 后 _exit。
 * 父进程据此区分"工具自己 exit(1)"与"我们没把工具启动起来"。
 * ------------------------------------------------------------------ */

#define HM_SHIM_ERRFD 3
#define HM_SHIM_ERRFD_PAYLOAD 4 /* 小端 u32 errno */

/* F-11：fd 3 的带 tag 加性记录。
 *   [HM_SHIM_SETUP_TAG_IGNORED:u32 LE][ignored_flags:u32 LE] = 8 字节。
 * tag 选 0x484D4947u（"HMIG"），远大于任何真实 errno，因此不可能与
 * "[errno:u32 LE] 4 字节"这条既有记录混淆。EOF = execve 成功的语义不变。 */
#define HM_SHIM_SETUP_TAG_IGNORED 0x484D4947u
#define HM_SHIM_IGNORED_RECORD 8

/* F-11b：fd 3 上的**正面握手**与**setup 失败**记录，与上面那条同形（8 字节）。
 *   [HM_SHIM_SETUP_TAG_ACK   ][applied :u32 LE]  = shim 实际施加的 HM_SB_* 位
 *   [HM_SHIM_SETUP_TAG_FAILED][errno   :u32 LE]  = setup 阶段失败的 errno
 *
 * 为什么必须新增 ACK：F-11 只能证明"平台自报了没施加的位"，无法证明"施加过"。
 * 一个吞掉 --sandbox、fd 3 一字不写的 shim 与"请求了 0 位"在通道上完全同形，
 * 于是静默放行。ACK 提供的是**正面证据**：没有 ACK = 没有证据 = 拒绝。
 *
 * tag 仍远大于任何真实 errno，因此三种带 tag 记录与既有的裸 4 字节 errno
 * 记录（execve 失败）不可能互相混淆。
 *
 * 字节序提醒（写测试注入时）：0x484D4947 落盘是 "GIMH"，0x484D4941 是 "AIMH"，
 * 0x484D4946 是 "FIMH" —— 都是小端。 */
#define HM_SHIM_SETUP_TAG_ACKED 0x484D4941u
#define HM_SHIM_SETUP_TAG_FAILED 0x484D4946u
/* 所有**带 tag** 的 fd 3 记录都是 8 字节：u32 tag + u32 payload。 */
#define HM_SHIM_ACKED_RECORD 8
#define HM_SHIM_FAILED_RECORD 8

/* 被忽略就必须在**进程启动之前**拒绝的关键沙箱位。
 * NO_NEW_PRIVS 不在此列：Linux 在"未请求 NNP"时反而会把它记成 ignored
 * （sandbox.c 的记账反常），把它算进关键位会误拒所有正常调用。 */
#define HM_SB_CRITICAL_MASK (HM_SB_NO_NET | HM_SB_READONLY_TMP)

/* shim 退出码（只有在 shim **自己** 退出时才有意义） */
#define HM_SHIM_EXIT_OK 0
#define HM_SHIM_EXIT_SELFTEST_FAIL 1
#define HM_SHIM_EXIT_SETUP 40   /* setup 失败；errno 已写到 fd 3 */
#define HM_SHIM_EXIT_EXEC 41    /* execve 失败；errno 已写到 fd 3 */
#define HM_SHIM_EXIT_ALLOC_BLOCKED 42   /* --alloc-test：分配被挡住（限制生效） */
#define HM_SHIM_EXIT_LIMIT_UNSUPPORTED 43 /* --alloc-test：该 rlimit 平台不支持 */
#define HM_SHIM_EXIT_ALLOC_OK 44        /* --alloc-test：分配竟然成功（限制未生效） */

/* ------------------------------------------------------------------ *
 * 内部汇编原语（native/c/cycle_*.S 提供，名字刻意不带 hm_ 前缀，
 * 以免污染 "Rust 只做 4 件事" 的符号守卫）
 * ------------------------------------------------------------------ */

uint64_t hermes_hw_counter(void);      /* 当前计数器读数 */
uint64_t hermes_hw_counter_freq(void); /* 计数器频率（Hz）；0 = 未知 */

/* ------------------------------------------------------------------ *
 * errno / 沙箱
 * ------------------------------------------------------------------ */

hm_status hermes_errno_to_status(int err);
hm_status hermes_errno_to_spawn_status(int err);

/* out_ignored 置位表示"请求了但平台不支持"的沙箱开关。 */
hm_status hermes_sandbox_apply(uint32_t flags, uint32_t *out_ignored);

/* ------------------------------------------------------------------ *
 * 进程表（supervisor.c 实现，exec.c 使用）
 *
 * 全部字段由 hermes_proc_table_lock()/unlock() 保护。
 * ------------------------------------------------------------------ */

#define HM_MAX_PROCS 256
#define HM_DEFAULT_STDOUT_CAP (1u << 20)
#define HM_DEFAULT_STDERR_CAP (1u << 20)
#define HM_DEFAULT_KILL_GRACE_MS 500u

typedef struct hm_proc {
  uint32_t gen;      /* generation；0 表示该槽位从未用过 */
  int used;          /* 1 = 槽位被占用 */
  int reaped;        /* 1 = 已 waitpid 到 */
  int waiting;       /* 1 = 已有 goroutine 阻塞在 hm_spawn_wait */
  int refcnt;        /* 正在使用该槽位的调用数 */
  pid_t pid;
  pid_t pgid;

  int cancelled;   /* hm_spawn_cancel 置位 */
  int term_sent;   /* 已向进程组发过 SIGTERM */
  int killed;      /* 已向进程组发过 SIGKILL */
  uint64_t deadline_cycles;
  uint64_t wall_backstop_ns;
  uint64_t start_cycles;
  uint32_t kill_grace_ms;

  int in_fd;    /* 父进程侧写端（stdin），写完关闭 */
  int out_fd;   /* 父进程侧读端（stdout） */
  int err_fd;   /* 父进程侧读端（stderr） */
  int setup_fd; /* 父进程侧读端（fd 3 错误通道） */

  uint8_t *out_buf;
  size_t out_len;
  size_t out_cap;
  uint8_t *err_buf;
  size_t err_len;
  size_t err_cap;

  /* stdin 的非阻塞写入缓冲（由 exec.c 复制进来，wait 循环负责写完并关闭） */
  uint8_t *in_buf;
  size_t in_len;
  size_t in_off;

  int status;         /* waitpid 的原始 status */
  int setup_errno;    /* 从 fd 3 读到的 errno；0 = EOF（setup 成功） */
  int setup_closed;   /* 1 = fd 3 已读到 EOF */
  uint32_t limits_unsupported; /* 平台不支持而被跳过的 rlimit 位 */

  /* F-11：shim 经 fd 3 回报的"请求了但没施加"的沙箱位（按位或累加）。
   * setup_acc/setup_acc_len 让 pump_setup 能容忍一次 read 把带 tag 记录
   * 拆开（或把"成功记录 + execve 失败二次写"粘在一起）。
   * 三者在 hermes_proc_alloc_locked() 的 memset 里一并归零，不会跨槽泄漏。 */
  uint32_t sandbox_ignored;
  uint8_t setup_acc[16];
  size_t setup_acc_len;

  /* F-11b：沙箱 setup 的**正面证据**。这些值没有任何结构体承载，只经
   * hm_proc_get_* 从槽位读出（v4：句柄 + Getter 形态）。
   *
   * sb_requested  —— exec.c 填槽位时从 hm_spawn_req **原样**抄一份（不得
   *                  归一化：Go 侧判据②依赖"逐位相等"才能发现 native 层
   *                  把请求丢掉/改写了）。
   * sb_acked      —— shim 的 [HMIA] 记录按位或累加 = 实际施加的位。
   * setup_failed  —— shim 写过 [HMIF] 记录（setup 阶段失败）。
   * setup_exec_failed —— 收到裸 4 字节 errno = execve 失败（**目标从未启动**）。
   *                  与 setup_failed 互不蕴含：execve 失败时 setup 是成功的。
   * setup_eof     —— fd 3 读到 EOF（**不代表** execve 成功，见下）。
   * setup_eof_while_alive —— EOF 时子进程仍然存活 = execve 已发生。
   *                  E3（execve 前被 SIGKILL）与 E4（execve 后被杀死）
   *                  在 fd 3 通道上不可区分，只能靠 EOF 的**时序**消歧。
   * setup_channel —— 本次 spawn 是否**具备** fd3 setup 通道。由 exec.c 填
   *                  槽位时按 p->setup_fd >= 0 **置一次并粘住**，之后只读
   *                  不写：pump_setup 读到 EOF 会 close 并置 setup_fd = -1，
   *                  若 getter 现算，POSIX 上会在 wait 之后恒得 0，把
   *                  "没测量" 与 "测出来是 0" 混为一谈。
   * 全部由 hermes_proc_alloc_locked() 的 memset 一并归零，不跨槽泄漏。 */
  uint32_t sb_requested;
  uint32_t sb_acked;
  uint32_t setup_failed;
  uint32_t setup_exec_failed;
  uint32_t setup_eof;
  uint32_t setup_eof_while_alive;
  uint32_t setup_channel;

  /* F-11b：从 waitpid 状态字派生、但**不能**由 status 反算的终止面派生值。
   * 它们原来只是 hm_proc_wait 的局部变量（随 hm_spawn_res 一起返回），
   * 现在必须留在槽位里供 hm_proc_get_term / hm_proc_get_deadline /
   * hm_proc_copy_deadline_source 读。同样靠 memset 归零。
   *   escalated 不设独立字段：p->killed 的语义就是"已向进程组发过 SIGKILL"，
   *   与旧代码里 escalated 的置位点逐一对应，重复存两份只会制造分歧。 */
  int deadline_hit;
  uint64_t fired_cycles;
  pid_t killed_pgid;
  int deadline_source_kind; /* 0 = "" | 1 = "cycle" | 2 = "wall" */
} hm_proc;

void hermes_proc_table_lock(void);
void hermes_proc_table_unlock(void);

/* 分配槽位；返回槽位下标，失败 -1。需持有表锁。 */
int hermes_proc_alloc_locked(void);
/* 释放槽位（generation++，generation 到顶则退役该槽位）。需持有表锁。 */
void hermes_proc_release_locked(int slot);
/* 直接取槽位指针（不增 refcnt）。需持有表锁。exec.c 在填槽位时用。 */
hm_proc *hermes_proc_storage(int slot);

/* 查找并增加 refcnt；失败返回 NULL。内部加锁。用 hermes_proc_put 归还。 */
hm_proc *hermes_proc_acquire(hm_handle h);
void hermes_proc_put(hm_proc *p);

/* 进程表统计（测试与 doctor 用；不是 ABI）。 */
typedef struct hm_proc_stats {
  int live_slots;
  int total_spawns;
  int total_cancels;
} hm_proc_stats;

void hermes_proc_stats_read(hm_proc_stats *out);

/* ------------------------------------------------------------------ *
 * 版本自检（abi_version.c）
 *
 * 在编译期把 ABI 的关键布局量固定下来（首字段偏移、标量宽度、长度上限的
 * 2 的幂），并把它们混进 hermes_abi_layout_fingerprint()。
 *
 * 注意：这里**没有**任何断言检查 HM_ABI_MINOR，也没有断言
 * sizeof(hm_spawn_res)。minor 恒为 0；结构体**尾部**追加字段靠首字段
 * abi_size + min-size 读写（R8）保持兼容，布局变化由 fingerprint 在
 * doctor / shim --self-test 里暴露，不是靠"忘了提升 MINOR 断言会失败"
 * —— 那句话从来不成立。旧注释把它写反了，这里更正。
 * ------------------------------------------------------------------ */

uint32_t hermes_abi_layout_fingerprint(void);
uint64_t hermes_abi_header_hash(void);

#endif /* HM_INTERNAL_H */
