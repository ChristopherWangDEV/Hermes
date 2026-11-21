/* win_spawn.c —— Windows 上的进程派发（hm_spawn_* / hm_proc_* 的 Win32 实现）。
 *
 * 为什么单独一个文件而不是往 exec.c 里塞 #ifdef：exec.c 的语义是
 * "posix_spawn + 进程组 + 信号"，Windows 上这三样都不存在。硬塞会让两份
 * 逻辑互相干扰；分文件之后，对外导出的符号集合完全一致（含 F-11b 的
 * 全部 getter），调用方不区分平台。
 *
 * ⚠ 诚实标注（D4 / §3.4 / §7.3 / §8.4 四处口径一致）：本文件整段被
 * `#if defined(_WIN32)` 包住，**本机没有 MinGW，无法编译、无法执行验证**。
 * 本轮 Windows 侧是"镜像式实现 + 代码审查"，不是"已验证"。
 * 尤其是 D4 新增的 refcnt/waiting 与 slot_acquire/slot_put：它们逐行镜像
 * native/c/supervisor.c 的 hermes_proc_acquire/hermes_proc_put，但**没有**
 * 在任何机器上跑过。任何"Windows 侧已验证通过"的表述都是假的。
 *
 * 语义对齐（与 POSIX 版逐条对应）：
 *   - 新进程组     → Job Object（KILL_ON_JOB_CLOSE，杀整棵进程树）
 *   - SIGTERM 升级 → TerminateJobObject（Windows 没有可捕获的 SIGTERM 供我们发）
 *   - deadline     → 计数器（__rdtscp）折算毫秒 + wall 兜底
 *   - stdout/stderr→ 匿名管道 + 读线程
 *   - stdin        → 匿名管道，写完关闭（NULL = NUL 设备）
 */
#if defined(_WIN32)

#include "hm_internal.h"

#include <windows.h>
#include <stdlib.h>
#include <string.h>

#define HM_WIN_MAX_PROCS 64

typedef struct win_slot {
  uint32_t gen;      /* 0 = 从未使用 */
  int used;
  HANDLE proc;       /* 进程句柄（可等待） */
  HANDLE job;        /* Job Object（杀树） */
  HANDLE out_rd, err_rd;
  HANDLE out_thread, err_thread;
  DWORD pid;
  int cancelled;
  int killed;
  int exited;
  DWORD exit_code;
  uint64_t deadline_cycles;
  uint64_t wall_backstop_ms;
  uint8_t *out_buf;
  size_t out_len, out_cap;
  uint8_t *err_buf;
  size_t err_len, err_cap;

  /* F-11b（D4）：引用计数与"wait 在飞"标记 —— **本轮必做**。
   *
   * getter 化之后每一次读都要解引用槽位，而 hm_proc_free 会 memset/归还
   * 槽位。没有这两个字段，free-during-getter 就是 use-after-free。
   * 语义与 POSIX 的 hm_proc.refcnt / hm_proc.waiting 逐行对应：
   *   - slot_acquire：锁内校验后 refcnt++；
   *   - slot_put    ：锁内 if (refcnt > 0) refcnt--；
   *   - hm_proc_wait：进入时 waiting=1，结束时清零；
   *   - hm_proc_free：refcnt > 0 || waiting ⇒ HM_E_INVALID_ARG。 */
  int refcnt;
  int waiting;

  /* F-11b：与 POSIX 侧 hm_proc 的 setup_* / sb_* 字段同义。
   * Windows 没有 hermes-shim（shim.c 整文件被 #if !defined(_WIN32) 排除），
   * 而 hm_spawn 又在 CreateProcessA 之前就拒掉任何非零 sandbox_flags，
   * 所以能走到 wait 的请求一定"没请求、没施加、没有 ACK"——全 0 是诚实的。
   * 保留字段而不是省略，是为了让 getter 的语义跨平台一致：
   * 永远返回一个**答案**，而不是让调用方猜"这个平台会不会答"。
   *
   * setup_bits 恒为 0：**HM_SETUP_CHANNEL_MEASURED 在 Windows 上永远不置位**。
   * 这正是 Go 侧 ErrToolNotStarted 判据的豁免开关（D5）：没有 fd3 通道就
   * 没有"exec 确认"这个概念，把 EXEC_CONFIRMED=0 当成"目标没启动"会把每次
   * 主动取消都误报成未启动。 */
  uint32_t sb_requested;
  uint32_t sb_acked;
  uint32_t setup_bits;

  /* 终止面派生值：deadline 触发标记。旧代码把它放在 hm_spawn_wait 的局部
   * 变量里（随 hm_spawn_res 返回），现在必须留在槽位里供 getter 读。 */
  int deadline_hit;
} win_slot;

static win_slot g_slots[HM_WIN_MAX_PROCS];
static SRWLOCK g_lock = SRWLOCK_INIT;

static void slot_lock(void) { AcquireSRWLockExclusive(&g_lock); }
static void slot_unlock(void) { ReleaseSRWLockExclusive(&g_lock); }

/* 读线程：把管道里的字节读到可增长缓冲。缓冲只在 wait 之后被读取，
 * 所以线程写、主线程读之间用"线程 join 后再读"来同步，不需要额外锁。 */
typedef struct reader_arg {
  HANDLE pipe;
  uint8_t **buf;
  size_t *len;
  size_t *cap;
} reader_arg;

static DWORD WINAPI reader_main(LPVOID p) {
  reader_arg *a = (reader_arg *)p;
  uint8_t chunk[4096];
  DWORD got = 0;
  for (;;) {
    if (!ReadFile(a->pipe, chunk, sizeof(chunk), &got, NULL) || got == 0) break;
    if (*a->len + got > *a->cap) {
      size_t ncap = (*a->cap == 0) ? 8192 : (*a->cap * 2);
      while (ncap < *a->len + got) ncap *= 2;
      uint8_t *nb = (uint8_t *)realloc(*a->buf, ncap);
      if (!nb) break;
      *a->buf = nb;
      *a->cap = ncap;
    }
    memcpy(*a->buf + *a->len, chunk, got);
    *a->len += got;
  }
  free(a);
  return 0;
}

/* 把 argv 拼成 CreateProcessW 需要的命令行（按 Windows 规则加引号）。 */
static char *build_cmdline(const char *const *argv) {
  size_t need = 1;
  for (size_t i = 0; argv[i]; ++i) {
    need += strlen(argv[i]) * 2 + 4;
  }
  char *out = (char *)malloc(need);
  if (!out) return NULL;
  size_t pos = 0;
  for (size_t i = 0; argv[i]; ++i) {
    if (i) out[pos++] = ' ';
    const char *s = argv[i];
    int need_quote = (*s == '\0') || strchr(s, ' ') != NULL || strchr(s, '\t') != NULL;
    if (need_quote) out[pos++] = '"';
    for (; *s; ++s) {
      if (*s == '"') out[pos++] = '\\';
      out[pos++] = *s;
    }
    if (need_quote) out[pos++] = '"';
  }
  out[pos] = '\0';
  return out;
}

/* envp（NULL = 空环境）→ 双 NUL 结尾的块。 */
static char *build_env(const char *const *envp) {
  if (envp == NULL) {
    char *e = (char *)calloc(1, 2);
    return e;
  }
  size_t need = 1;
  for (size_t i = 0; envp[i]; ++i) need += strlen(envp[i]) + 1;
  char *out = (char *)malloc(need + 1);
  if (!out) return NULL;
  size_t pos = 0;
  for (size_t i = 0; envp[i]; ++i) {
    size_t n = strlen(envp[i]);
    memcpy(out + pos, envp[i], n);
    pos += n;
    out[pos++] = '\0';
  }
  out[pos] = '\0';
  return out;
}

static void close_pipe(HANDLE *h) {
  if (*h && *h != INVALID_HANDLE_VALUE) {
    CloseHandle(*h);
    *h = NULL;
  }
}

hm_status hm_spawn(const hm_spawn_req *req, hm_handle *out_proc) {
  if (!req || !out_proc) return HM_E_INVALID_ARG;

  /* F-11 A 组③：三步顺序固定（qa-f11 已把验收判据按字面预固化）。
   *
   * 第 1 步 —— 既有版本闸，阈值 sizeof(hm_spawn_req)-64 = 80，
   * 保持在最前、原样不动（它是既有兼容契约，本轮不提高全局阈值）。 */
  if (req->abi_size < sizeof(hm_spawn_req) - 64) {
    return HM_E_ABI_VERSION;
  }
  /* 第 2 步 —— 字段存在性。sandbox_flags 在 offset 108、宽 4，安全读需
   * abi_size >= 112；仅靠第 1 步的 80 阈值不足以保证，故单独判一次。
   * 失败必须 **return error**：绝不能把它写成「字段存在才检查 guard」的条件
   * （那会是 D.2 点名的结构性 fail-open：abi_size 不覆盖该字段时 guard
   * 被跳过、进程照常创建）。意图未知 → 保守拒绝，也不越界读。
   * offsetof 由 hm_internal.h 的 <stddef.h> 提供；不手写魔数 112。
   * hm_spawn_req 本次未改，所有历史版本 sizeof 都是 144，Go Spawn()
   * 恒填 abi_size = sizeof(req) = 144，故不会误拒任何真实调用方。 */
  if (req->abi_size <
      offsetof(hm_spawn_req, sandbox_flags) + sizeof(req->sandbox_flags)) {
    return HM_E_ABI_VERSION; /* 保守拒绝，不越界读，不 fallthrough */
  }
  /* 第 3 步 —— guard：此时字段一定存在，无条件执行。
   *
   * F-11 P0：Windows 没有 hermes-shim（shim.c 整文件被 #if !defined(_WIN32)
   * 排除），sandbox.c 里那个"全部 ignored"的 _WIN32 分支事实上不可达，
   * 因此本文件既不会产生 ignored 记录、也不会做任何预执行拒绝。
   * 若不在**进程创建之前**硬拒，新库的 abi_size 仍为 96 而 ignored 恒为 0，
   * Go 侧的按位交集与 abi_size 守卫**都检不出**静默放行 —— 那是永久性的
   * fail-open。所以任何非零 sandbox_flags 一律在 CreateProcessA 之前拒绝。
   * Go 侧 Spawn() 把 HM_E_SANDBOX_DENIED 映射成与 POSIX 相同的 errs 错误。 */
  if (req->sandbox_flags != 0) {
    return HM_E_SANDBOX_DENIED;
  }

  if (!req->exe || !req->argv) return HM_E_INVALID_ARG;

  char *cmd = build_cmdline(req->argv);
  if (!cmd) return HM_E_NOMEM;
  char *env = build_env(req->envp);
  if (!env) {
    free(cmd);
    return HM_E_NOMEM;
  }

  SECURITY_ATTRIBUTES sa;
  memset(&sa, 0, sizeof(sa));
  sa.nLength = sizeof(sa);
  sa.bInheritHandle = TRUE;

  HANDLE out_wr = NULL, err_wr = NULL, in_rd = NULL;
  HANDLE out_rd = NULL, err_rd = NULL, in_wr = NULL;
  HANDLE proc = NULL, thread = NULL, job = NULL;
  int slot = -1;
  hm_status st = HM_E_SPAWN;

  if (!CreatePipe(&out_rd, &out_wr, &sa, 0)) goto done;
  if (!CreatePipe(&err_rd, &err_wr, &sa, 0)) goto done;
  if (!CreatePipe(&in_rd, &in_wr, &sa, 0)) goto done;
  /* 父进程侧的三个句柄不允许被子进程继承。 */
  SetHandleInformation(out_rd, HANDLE_FLAG_INHERIT, 0);
  SetHandleInformation(err_rd, HANDLE_FLAG_INHERIT, 0);
  SetHandleInformation(in_wr, HANDLE_FLAG_INHERIT, 0);

  STARTUPINFOA si;
  PROCESS_INFORMATION pi;
  memset(&si, 0, sizeof(si));
  si.cb = sizeof(si);
  si.dwFlags = STARTF_USESTDHANDLES;
  si.hStdOutput = out_wr;
  si.hStdError = err_wr;
  si.hStdInput = in_rd;

  DWORD flags = CREATE_NO_WINDOW | CREATE_SUSPENDED | CREATE_NEW_PROCESS_GROUP;
  if (!CreateProcessA(req->exe, cmd, NULL, NULL, TRUE, flags, env,
                      req->cwd, &si, &pi)) {
    st = HM_E_SPAWN;
    goto done;
  }
  proc = pi.hProcess;
  thread = pi.hThread;

  /* Job Object：父进程退出时整棵树一起结束，避免遗留子进程。 */
  job = CreateJobObjectA(NULL, NULL);
  if (job) {
    JOBOBJECT_EXTENDED_LIMIT_INFORMATION li;
    memset(&li, 0, sizeof(li));
    li.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE;
    SetInformationJobObject(job, JobObjectExtendedLimitInformation, &li, sizeof(li));
    AssignProcessToJobObject(job, proc);
  }

  /* stdin：把请求里的字节写完后关闭写端（NULL = 立即关闭，子进程读到 EOF）。 */
  if (req->stdin_bytes && req->stdin_len > 0) {
    DWORD written = 0;
    WriteFile(in_wr, req->stdin_bytes, (DWORD)req->stdin_len, &written, NULL);
  }
  close_pipe(&in_wr);

  ResumeThread(thread);

  slot_lock();
  for (int i = 0; i < HM_WIN_MAX_PROCS; ++i) {
    if (!g_slots[i].used) {
      slot = i;
      g_slots[i].used = 1;
      g_slots[i].gen = (g_slots[i].gen == 0) ? 1 : g_slots[i].gen;
      g_slots[i].proc = proc;
      g_slots[i].job = job;
      g_slots[i].out_rd = out_rd;
      g_slots[i].err_rd = err_rd;
      g_slots[i].pid = pi.dwProcessId;
      g_slots[i].cancelled = 0;
      g_slots[i].killed = 0;
      g_slots[i].exited = 0;
      g_slots[i].exit_code = 0;
      g_slots[i].deadline_cycles = req->deadline_cycles;
      g_slots[i].wall_backstop_ms = req->wall_backstop_ms;
      g_slots[i].out_buf = NULL;
      g_slots[i].out_len = g_slots[i].out_cap = 0;
      g_slots[i].err_buf = NULL;
      g_slots[i].err_len = g_slots[i].err_cap = 0;
      /* F-11b：槽位复用必须显式归零，不能指望上一次的残留。
       * sb_requested 同样为 0：非零请求在 hm_spawn 就被拒了，走不到这里。 */
      g_slots[i].sb_requested = 0;
      g_slots[i].sb_acked = 0;
      g_slots[i].setup_bits = 0;
      g_slots[i].deadline_hit = 0;
      g_slots[i].refcnt = 0;
      g_slots[i].waiting = 0;
      if (out_rd) {
        reader_arg *ra = (reader_arg *)calloc(1, sizeof(reader_arg));
        if (ra) {
          ra->pipe = out_rd;
          ra->buf = &g_slots[i].out_buf;
          ra->len = &g_slots[i].out_len;
          ra->cap = &g_slots[i].out_cap;
          g_slots[i].out_thread = CreateThread(NULL, 0, reader_main, ra, 0, NULL);
        }
      }
      if (err_rd) {
        reader_arg *ra = (reader_arg *)calloc(1, sizeof(reader_arg));
        if (ra) {
          ra->pipe = err_rd;
          ra->buf = &g_slots[i].err_buf;
          ra->len = &g_slots[i].err_len;
          ra->cap = &g_slots[i].err_cap;
          g_slots[i].err_thread = CreateThread(NULL, 0, reader_main, ra, 0, NULL);
        }
      }
      break;
    }
  }
  slot_unlock();

  if (slot < 0) {
    TerminateProcess(proc, 1);
    st = HM_E_SPAWN;
    goto done;
  }
  *out_proc = hermes_handle_pack((uint32_t)slot, g_slots[slot].gen);
  st = HM_OK;

done:
  if (st != HM_OK) {
    if (thread) CloseHandle(thread);
    if (proc) CloseHandle(proc);
    if (job) CloseHandle(job);
    close_pipe(&out_rd); close_pipe(&err_rd);
  }
  close_pipe(&out_wr); close_pipe(&err_wr); close_pipe(&in_rd);
  free(cmd); free(env);
  return st;
}

static win_slot *slot_of(hm_handle h, uint32_t *gen_out) {
  uint32_t slot = (uint32_t)(h & 0xffffffffu);
  uint32_t gen = (uint32_t)(h >> 32);
  if (slot >= HM_WIN_MAX_PROCS) return NULL;
  if (!g_slots[slot].used || g_slots[slot].gen != gen) return NULL;
  if (gen_out) *gen_out = gen;
  return &g_slots[slot];
}

/* ------------------------------------------------------------------ *
 * slot_acquire / slot_put —— D4：逐行镜像 POSIX 的
 * hermes_proc_acquire / hermes_proc_put（native/c/supervisor.c:113-148）
 *
 * 镜像点必须一一对应，改一边就要改另一边：
 *   POSIX `!p->used || p->gen != gen` → NULL   ⇔ 这里 slot_of 返回 NULL
 *   POSIX `p->refcnt++`（锁内）               ⇔ 这里 s->refcnt++
 *   POSIX `if (p->refcnt > 0) p->refcnt--`    ⇔ 这里同形
 *   两者都在锁内改、锁**外**返回指针          ⇔ 同
 *
 * 为什么必须有：getter 每次读都解引用槽位，而 hm_proc_free 会清空槽位。
 * 没有 refcnt 就是 free-during-getter 的 use-after-free。
 * ⚠ 本机无 MinGW，本段**未编译、未执行**，仅代码审查。
 * ------------------------------------------------------------------ */

static win_slot *slot_acquire(hm_handle h) {
  win_slot *s;
  slot_lock();
  s = slot_of(h, NULL);
  if (!s) {
    slot_unlock();
    return NULL;
  }
  s->refcnt++;
  slot_unlock();
  return s;
}

static void slot_put(win_slot *s) {
  if (!s) return;
  slot_lock();
  if (s->refcnt > 0) s->refcnt--;
  slot_unlock();
}

/* getter 共用：acquire + 校验已收割。成功时持 refcnt（调用方 slot_put）。
 * 失败返回 NULL，*st 给出 HM_E_HANDLE_STALE / HM_E_NOT_READY，且调用方
 * **一个出参都不许写**（§3.2 真值表：未就绪绝不表现为 0）。 */
static win_slot *getter_acquire_reaped(hm_handle proc, hm_status *st) {
  win_slot *s = slot_acquire(proc);
  if (!s) {
    *st = HM_E_HANDLE_STALE;
    return NULL;
  }
  slot_lock();
  if (!s->exited) {
    slot_unlock();
    slot_put(s);
    *st = HM_E_NOT_READY;
    return NULL;
  }
  slot_unlock();
  *st = HM_OK;
  return s;
}

hm_status hm_proc_cancel(hm_handle proc) {
  win_slot *s = slot_acquire(proc);
  if (!s) return HM_E_HANDLE_STALE;
  slot_lock();
  s->cancelled = 1;
  slot_unlock();
  if (s->job) TerminateJobObject(s->job, 1);
  else if (s->proc) TerminateProcess(s->proc, 1);
  slot_put(s);
  return HM_OK;
}

/* v4：无出参。结果就地写槽位，getter 随后读；exited=1 是最后一次写。 */
hm_status hm_proc_wait(hm_handle proc) {
  win_slot *s;
  DWORD timeout;
  DWORD wr;
  DWORD code = 0;
  int deadline_hit = 0;

  s = slot_acquire(proc);
  if (!s) return HM_E_HANDLE_STALE;

  slot_lock();
  if (s->waiting) {
    slot_unlock();
    slot_put(s);
    return HM_E_INVALID_ARG;
  }
  s->waiting = 1;
  /* deadline：Windows 只有 wall 兜底（没有周期计数）。 */
  timeout = INFINITE;
  if (s->wall_backstop_ms > 0 && s->wall_backstop_ms < 0x7fffffff) {
    timeout = (DWORD)s->wall_backstop_ms;
  }
  slot_unlock();

  wr = WaitForSingleObject(s->proc, timeout);
  if (wr == WAIT_TIMEOUT) {
    deadline_hit = 1;
    if (s->job) TerminateJobObject(s->job, 1);
    else TerminateProcess(s->proc, 1);
    WaitForSingleObject(s->proc, 5000);
  }
  GetExitCodeProcess(s->proc, &code);

  /* 读线程在子进程退出后读到 EOF 自然结束；join 之后再读缓冲。 */
  if (s->out_thread) {
    WaitForSingleObject(s->out_thread, 5000);
    CloseHandle(s->out_thread);
    s->out_thread = NULL;
  }
  if (s->err_thread) {
    WaitForSingleObject(s->err_thread, 5000);
    CloseHandle(s->err_thread);
    s->err_thread = NULL;
  }

  /* 填槽位。**缓冲区不在这里移交给调用方**（v4：不像 v3 那样置 NULL）：
   * 它们由 hm_proc_copy_stdout/stderr 拷出、由 hm_proc_free 释放。
   * 带非零 sandbox_flags 的请求已在 hm_spawn 被拒，走不到这里，所以
   * setup_bits 恒 0（含 CHANNEL_MEASURED 不置位）。 */
  slot_lock();
  s->deadline_hit = deadline_hit;
  s->exit_code = code;
  s->setup_bits = 0;
  s->sb_requested = 0;
  s->sb_acked = 0;
  s->waiting = 0;
  s->exited = 1; /* ← 发布屏障：必须是槽位里最后一次写 */
  slot_unlock();

  slot_put(s);
  return HM_OK;
}

/* ------------------------------------------------------------------ *
 * Getter 族（Win32）—— 与 supervisor.c 的同名函数逐条对应
 * ------------------------------------------------------------------ */

hm_status hm_proc_get_state(hm_handle proc, int32_t *out_state) {
  win_slot *s;
  int32_t v;

  if (!out_state) return HM_E_INVALID_ARG;
  s = slot_acquire(proc);
  if (!s) return HM_E_HANDLE_STALE;
  slot_lock();
  v = s->exited ? (int32_t)HM_PROC_REAPED : (int32_t)HM_PROC_RUNNING;
  slot_unlock();
  slot_put(s);
  *out_state = v;
  return HM_OK;
}

hm_status hm_proc_get_term(hm_handle proc, uint32_t *out_bits,
                           int32_t *out_exit_code, int32_t *out_term_signal,
                           int32_t *out_killed_pgid) {
  win_slot *s;
  hm_status st;
  uint32_t bits;
  int32_t exit_code;
  int32_t killed_pgid;

  if (!out_bits || !out_exit_code || !out_term_signal || !out_killed_pgid) {
    return HM_E_INVALID_ARG; /* 全有或全无 */
  }
  s = getter_acquire_reaped(proc, &st);
  if (!s) return st;

  /* 一次读、统一写（D2）。Windows 语义（§四 表）：
   *   EXITED      恒置（Win32 的 wait 只返回"进程结束了"，没有信号概念）；
   *   SIGNALED    = cancelled；
   *   ESCALATED   = cancelled（TerminateJobObject 就是这个平台的"升级"）；
   *   term_signal 恒 0（Win32 无 POSIX 信号）。 */
  slot_lock();
  bits = HM_TERM_EXITED;
  exit_code = (int32_t)s->exit_code;
  if (s->cancelled) {
    bits |= HM_TERM_SIGNALED;
    bits |= HM_TERM_ESCALATED;
  }
  if (s->deadline_hit) {
    bits |= HM_TERM_DEADLINE_HIT;
  }
  killed_pgid = (s->cancelled || s->deadline_hit) ? (int32_t)s->pid : 0;
  slot_unlock();
  slot_put(s);

  *out_bits = bits;
  *out_exit_code = exit_code;
  *out_term_signal = 0;
  *out_killed_pgid = killed_pgid;
  return HM_OK;
}

hm_status hm_proc_get_deadline(hm_handle proc, uint64_t *out_fired_cycles) {
  win_slot *s;
  hm_status st;

  if (!out_fired_cycles) return HM_E_INVALID_ARG;
  s = getter_acquire_reaped(proc, &st);
  if (!s) return st;
  slot_put(s);
  /* win_slot 没有周期计数（Windows 只有 wall 兜底）—— 恒 0 是诚实的答案，
   * 不是"没测量"：有 CHANNEL_MEASURED 那一类事实位负责区分这两件事。 */
  *out_fired_cycles = 0;
  return HM_OK;
}

hm_status hm_proc_copy_deadline_source(hm_handle proc, char *buf, size_t cap,
                                       size_t *out_len) {
  win_slot *s;
  hm_status st;
  size_t len;
  int hit;

  if (!out_len) return HM_E_INVALID_ARG;
  s = getter_acquire_reaped(proc, &st);
  if (!s) return st;
  slot_lock();
  hit = s->deadline_hit;
  slot_unlock();
  slot_put(s);

  /* 静态串，但走**拷贝式**接口：返回 const char* 会引入借用期规则，
   * 与 R4 的 hm_view_borrow 混淆。字符串容量语义见 hermes_abi.h（+1）。 */
  len = hit ? 4u : 0u; /* "wall" 或 "" */
  if (!buf) {
    *out_len = len;
    return HM_OK;
  }
  if (cap < len + 1) {
    *out_len = len + 1; /* 所需 cap */
    return HM_E_TOO_LARGE;
  }
  if (hit) {
    memcpy(buf, "wall", 4);
  }
  buf[len] = '\0';
  *out_len = len;
  return HM_OK;
}

hm_status hm_proc_get_setup(hm_handle proc, uint32_t *out_bits,
                            int32_t *out_setup_errno) {
  win_slot *s;
  hm_status st;
  uint32_t bits;

  if (!out_bits || !out_setup_errno) return HM_E_INVALID_ARG;
  s = getter_acquire_reaped(proc, &st);
  if (!s) return st;
  slot_lock();
  bits = s->setup_bits; /* 恒 0：不置 CHANNEL_MEASURED，也不置其余三位 */
  slot_unlock();
  slot_put(s);
  *out_bits = bits;
  *out_setup_errno = 0;
  return HM_OK;
}

hm_status hm_proc_get_sb_requested(hm_handle proc, uint32_t *out_flags) {
  win_slot *s;
  hm_status st;
  uint32_t v;

  if (!out_flags) return HM_E_INVALID_ARG;
  s = getter_acquire_reaped(proc, &st);
  if (!s) return st;
  slot_lock();
  v = s->sb_requested; /* 恒 0：非零请求在 hm_spawn 就被拒 */
  slot_unlock();
  slot_put(s);
  *out_flags = v;
  return HM_OK;
}

hm_status hm_proc_get_sb_acked(hm_handle proc, uint32_t *out_flags) {
  win_slot *s;
  hm_status st;
  uint32_t v;

  if (!out_flags) return HM_E_INVALID_ARG;
  s = getter_acquire_reaped(proc, &st);
  if (!s) return st;
  slot_lock();
  v = s->sb_acked; /* 恒 0：没有 hermes-shim，就没有 ACK 可言 */
  slot_unlock();
  slot_put(s);
  *out_flags = v;
  return HM_OK;
}

hm_status hm_proc_get_sb_ignored(hm_handle proc, uint32_t *out_flags) {
  win_slot *s;
  hm_status st;

  if (!out_flags) return HM_E_INVALID_ARG;
  s = getter_acquire_reaped(proc, &st);
  if (!s) return st;
  slot_put(s);
  /* 恒 0：走得到 wait 的请求 sandbox_flags 必为 0，"没请求"自然"没忽略"。 */
  *out_flags = 0;
  return HM_OK;
}

/* 字节流两阶段拷贝（与 supervisor.c 的 copy_stream 同语义）。 */
static hm_status copy_stream(hm_handle proc, int which_stderr, uint8_t *buf,
                             size_t cap, size_t *out_len) {
  win_slot *s;
  hm_status st;
  const uint8_t *src;
  size_t need;

  if (!out_len) return HM_E_INVALID_ARG;
  s = getter_acquire_reaped(proc, &st);
  if (!s) return st;
  slot_lock();
  if (which_stderr) {
    src = s->err_buf;
    need = s->err_len;
  } else {
    src = s->out_buf;
    need = s->out_len;
  }
  slot_unlock();

  if (!buf) {
    slot_put(s);
    *out_len = need;
    return HM_OK;
  }
  if (cap < need) {
    slot_put(s);
    *out_len = need;
    return HM_E_TOO_LARGE;
  }
  if (need > 0) memcpy(buf, src, need);
  *out_len = need;
  slot_put(s);
  return HM_OK;
}

hm_status hm_proc_copy_stdout(hm_handle proc, uint8_t *buf, size_t cap,
                              size_t *out_len) {
  return copy_stream(proc, 0, buf, cap, out_len);
}

hm_status hm_proc_copy_stderr(hm_handle proc, uint8_t *buf, size_t cap,
                              size_t *out_len) {
  return copy_stream(proc, 1, buf, cap, out_len);
}

/* refcnt > 0（getter 在飞）或 waiting（wait 未返回）⇒ HM_E_INVALID_ARG。
 * 这是**调用顺序错**，不是泄漏；也绝不能"反正没人看"就 free 掉。 */
hm_status hm_proc_free(hm_handle proc) {
  win_slot *s;
  slot_lock();
  s = slot_of(proc, NULL);
  if (!s) {
    /*
     * 幂等：已释放 / 非法句柄 = 无操作，返回 HM_OK（与 POSIX 的
     * hm_proc_free 语义一致，契约见 hermes_abi.h）。注意这与
     * hm_proc_wait / hm_proc_cancel / getter 的 HM_E_HANDLE_STALE 刻意相反：
     * 清理可以重复，读取必须报错。 */
    slot_unlock();
    return HM_OK;
  }
  if (s->refcnt > 0 || s->waiting) {
    slot_unlock();
    return HM_E_INVALID_ARG;
  }
  /*
   * F2：wait 之前就 free —— 与 POSIX 的 hm_proc_free 逐句对应
   * （supervisor.c 的 `if (!p->reaped && p->pid > 0)` 分支）。
   *
   * 少了这一段就会漏孤儿：清掉槽位后进程还活着，既没有句柄也没有收尸人。
   * 契约是"wait 之前 free 等价于取消 + 收尸"，所以这里必须真的杀掉并等它死
   * （不是"发个信号就走"—— 不等就关掉 s->proc 会让 WaitForSingleObject 失去
   * 对象，进程照样跑）。
   *
   * ⚠ 本机无 MinGW，本段**未编译、未执行**，仅代码审查。
   */
  if (!s->exited && s->proc) {
    if (s->job) {
      /* 与 hm_proc_cancel 同一手段：Job Object 一次性带走整棵进程树。 */
      (void)TerminateJobObject(s->job, 1);
    } else {
      (void)TerminateProcess(s->proc, 1);
    }
    (void)WaitForSingleObject(s->proc, INFINITE);
    s->exited = 1;
    s->exit_code = 1;
  }
  if (s->out_thread) CloseHandle(s->out_thread);
  if (s->err_thread) CloseHandle(s->err_thread);
  close_pipe(&s->out_rd);
  close_pipe(&s->err_rd);
  if (s->proc) CloseHandle(s->proc);
  if (s->job) CloseHandle(s->job);
  free(s->out_buf);
  free(s->err_buf);
  s->out_buf = NULL;
  s->err_buf = NULL;
  s->out_len = s->err_len = 0;
  s->proc = NULL;
  s->job = NULL;
  s->out_thread = NULL;
  s->err_thread = NULL;
  s->used = 0;
  s->gen += 1;
  slot_unlock();
  return HM_OK;
}

#endif /* _WIN32 */
