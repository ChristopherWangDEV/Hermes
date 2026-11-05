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
#include <sys/types.h>

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
 * 在编译期把 ABI 的结构体大小固定下来；一旦有人在 header 里插字段而忘了
 * 提升 HM_ABI_MINOR，这些断言会让构建失败。
 * ------------------------------------------------------------------ */

uint32_t hermes_abi_layout_fingerprint(void);
uint64_t hermes_abi_header_hash(void);

#endif /* HM_INTERNAL_H */
