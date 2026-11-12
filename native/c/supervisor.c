/* Windows 上这两个文件整体不参与编译：它们建立在 POSIX 进程模型上
 * （posix_spawn / 进程组 / 信号）。Windows 的实现见 native/c/win_spawn.c，
 * 对外导出同一组 hm_spawn_* 符号，所以调用方不需要区分平台。 */
#if !defined(_WIN32)
/*
 * supervisor.c —— hm_spawn_wait / hm_spawn_cancel / 进程表。
 *
 * 这里是"硬超时"真正落地的地方（R-CB-02 / R-NA-01）。
 *
 * 为什么用轮询（waitpid(WNOHANG) + nanosleep(2ms)）而不是 SIGALRM/timerfd：
 *   SIGALRM 需要一个信号处理器，而 Go runtime 自己大量使用信号（SIGURG 抢占、
 *   SIGPROF）。在 cgo 里安装信号处理器会与 runtime 的 sigtramp 交互，是已知的
 *   雷区。轮询的代价：每 2 ms 一次 hm_cycle_now()（~10 ns）+ 一次 waitpid，
 *   完全可忽略，换来"零信号交互"。
 *
 * 超时后的动作序列（两平台一致）：
 *   1. kill(-pgid, SIGTERM)           ← 负 pgid 才是"发整个组"
 *   2. 等 kill_grace_ms
 *   3. kill(-pgid, SIGKILL) 并置 escalated=1
 *   4. 循环 waitpid 直到收割
 *   5. **仍然读到管道 EOF**（第 5 步最容易被忘：只杀不读，孙进程仍持有管道
 *      写端，父进程会在 read 上永久等待）
 */

#include "hm_internal.h"

#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <pthread.h>
#include <signal.h>
#include <stdlib.h>
#include <string.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

/* ------------------------------------------------------------------ *
 * 进程表
 * ------------------------------------------------------------------ */

static hm_proc g_procs[HM_MAX_PROCS];
static pthread_mutex_t g_lock = PTHREAD_MUTEX_INITIALIZER;
static hm_proc_stats g_stats;

void hermes_proc_table_lock(void) { (void)pthread_mutex_lock(&g_lock); }
void hermes_proc_table_unlock(void) { (void)pthread_mutex_unlock(&g_lock); }

hm_proc *hermes_proc_storage(int slot) {
  if (slot <= 0 || slot >= HM_MAX_PROCS) {
    return NULL;
  }
  return &g_procs[slot];
}

int hermes_proc_alloc_locked(void) {
  int i;
  for (i = 1; i < HM_MAX_PROCS; i++) {
    uint32_t gen;
    if (g_procs[i].used) {
      continue;
    }
    /* generation 已经到顶 = 该槽位退役，永不复用（ABA 防护）。 */
    if (g_procs[i].gen == UINT32_MAX) {
      continue;
    }
    gen = g_procs[i].gen;
    if (gen == 0) {
      gen = 1;
    }
    memset(&g_procs[i], 0, sizeof(g_procs[i]));
    g_procs[i].gen = gen;
    g_procs[i].used = 1;
    g_procs[i].in_fd = -1;
    g_procs[i].out_fd = -1;
    g_procs[i].err_fd = -1;
    g_procs[i].setup_fd = -1;
    g_stats.live_slots++;
    g_stats.total_spawns++;
    return i;
  }
  return -1;
}

void hermes_proc_release_locked(int slot) {
  uint32_t gen;
  if (slot <= 0 || slot >= HM_MAX_PROCS) {
    return;
  }
  gen = g_procs[slot].gen;
  if (gen == 0) {
    gen = 1;
  }
  /*
   * generation 加一。加满到 UINT32_MAX 时把槽位**退役**：
   * 之后 hermes_proc_alloc_locked 永远跳过它。
   * 这是 ABA 的最终防线 —— 一个 2^32 次复用循环后，旧 handle 不可能再撞上
   * 一个"看起来一样但其实是新对象"的槽位。
   */
  if (gen == UINT32_MAX) {
    gen = UINT32_MAX;
  } else {
    gen++;
  }
  memset(&g_procs[slot], 0, sizeof(g_procs[slot]));
  g_procs[slot].gen = gen;
  g_procs[slot].used = 0;
  if (g_stats.live_slots > 0) {
    g_stats.live_slots--;
  }
}

hm_proc *hermes_proc_acquire(hm_handle h) {
  uint32_t slot;
  uint32_t gen;
  hm_proc *p;

  if (h == HM_HANDLE_INVALID) {
    return NULL;
  }
  slot = hermes_handle_slot(h);
  gen = hermes_handle_gen(h);

  hermes_proc_table_lock();
  if (slot == 0 || slot >= HM_MAX_PROCS) {
    hermes_proc_table_unlock();
    return NULL;
  }
  p = &g_procs[slot];
  if (!p->used || p->gen != gen) {
    hermes_proc_table_unlock();
    return NULL;
  }
  p->refcnt++;
  hermes_proc_table_unlock();
  return p;
}

void hermes_proc_put(hm_proc *p) {
  if (p == NULL) {
    return;
  }
  hermes_proc_table_lock();
  if (p->refcnt > 0) {
    p->refcnt--;
  }
  hermes_proc_table_unlock();
}

void hermes_proc_stats_read(hm_proc_stats *out) {
  if (out == NULL) {
    return;
  }
  hermes_proc_table_lock();
  *out = g_stats;
  hermes_proc_table_unlock();
}

/* ------------------------------------------------------------------ *
 * 内部工具
 * ------------------------------------------------------------------ */

static uint64_t mono_ns(void) {
  struct timespec ts;
  if (clock_gettime(CLOCK_MONOTONIC, &ts) != 0) {
    return 0;
  }
  return (uint64_t)ts.tv_sec * 1000000000ull + (uint64_t)ts.tv_nsec;
}

static void sleep_ms(unsigned ms) {
  struct timespec req;
  req.tv_sec = (time_t)(ms / 1000u);
  req.tv_nsec = (long)(ms % 1000u) * 1000000L;
  (void)nanosleep(&req, NULL);
}

/* 从 fd 读一块，写进可增长缓冲；返回 1=EOF，0=暂无数据，-1=错误。 */
static int pump_fd(int fd, uint8_t **buf, size_t *len, size_t *alloced,
                   size_t cap) {
  uint8_t tmp[8192];

  if (fd < 0) {
    return 1;
  }
  for (;;) {
    ssize_t n = read(fd, tmp, sizeof(tmp));
    if (n > 0) {
      size_t want;
      if (*len < cap) {
        want = *len + (size_t)n;
        if (want > cap) {
          want = cap;
        }
        if (want > *alloced) {
          size_t ncap = (*alloced == 0) ? 4096 : *alloced;
          while (ncap < want) {
            ncap *= 2;
          }
          if (ncap > cap) {
            ncap = cap;
          }
          {
            uint8_t *nb = (uint8_t *)realloc(*buf, ncap);
            if (nb == NULL) {
              return -1;
            }
            *buf = nb;
            *alloced = ncap;
          }
        }
        {
          size_t room = want - *len;
          if (room > 0) {
            memcpy(*buf + *len, tmp, room);
            *len += room;
          }
        }
      }
      /*
       * 即使已经到达 cap 也继续读并丢弃：否则工具写 stdout 时会把管道
       * 写满然后永久阻塞，我们就是在观测上把工具卡死了。
       * 这是 R-TR-06 / R-NF-01 "上界" 的正确实现方式。
       */
      continue;
    }
    if (n == 0) {
      return 1; /* EOF */
    }
    if (errno == EINTR) {
      continue;
    }
    if (errno == EAGAIN || errno == EWOULDBLOCK) {
      return 0;
    }
    return -1;
  }
}

static void pump_setup(hm_proc *p) {
  uint8_t b[HM_SHIM_ERRFD_PAYLOAD + 4];
  ssize_t n;

  if (p->setup_closed || p->setup_fd < 0) {
    return;
  }
  n = read(p->setup_fd, b, sizeof(b));
  if (n == 0) {
    p->setup_closed = 1;
    (void)close(p->setup_fd);
    p->setup_fd = -1;
    return;
  }
  if (n > 0) {
    if (n >= HM_SHIM_ERRFD_PAYLOAD && p->setup_errno == 0) {
      p->setup_errno = (int)b[0] | ((int)b[1] << 8) | ((int)b[2] << 16) |
                       ((int)b[3] << 24);
    }
    return;
  }
  if (errno != EAGAIN && errno != EWOULDBLOCK && errno != EINTR) {
    p->setup_closed = 1;
    (void)close(p->setup_fd);
    p->setup_fd = -1;
  }
}

static void pump_stdin(hm_proc *p) {
  if (p->in_fd < 0) {
    return;
  }
  while (p->in_off < p->in_len) {
    ssize_t n = write(p->in_fd, p->in_buf + p->in_off,
                      p->in_len - p->in_off);
    if (n > 0) {
      p->in_off += (size_t)n;
      continue;
    }
    if (n < 0 && (errno == EAGAIN || errno == EWOULDBLOCK || errno == EINTR)) {
      return;
    }
    /* 写失败或对端已关闭：放弃剩余字节。 */
    break;
  }
  (void)close(p->in_fd);
  p->in_fd = -1;
}

static void kill_group(hm_proc *p, int sig) {
  if (p->pgid <= 0) {
    return;
  }
  (void)kill(-p->pgid, sig);
}

/* ------------------------------------------------------------------ *
 * hm_spawn_wait
 * ------------------------------------------------------------------ */

hm_status hm_spawn_wait(hm_handle proc, hm_spawn_res *out) {
  hm_proc *p;
  hm_spawn_res tmp;
  size_t nbytes;
  uint64_t freq;
  uint64_t start_cycles;
  uint64_t deadline_at = 0;
  uint64_t wall_start_ns;
  uint64_t term_sent_ns = 0;
  uint64_t grace_ns;
  const char *deadline_source = "";
  uint64_t fired_cycles = 0;
  int deadline_hit = 0;
  int escalated = 0;
  size_t out_alloced = 0;
  size_t err_alloced = 0;
  int loops = 0;

  if (out == NULL) {
    return HM_E_INVALID_ARG;
  }
  nbytes = sizeof(*out);
  if (out->abi_size != 0 && (size_t)out->abi_size < nbytes) {
    nbytes = (size_t)out->abi_size;
  }
  memset(&tmp, 0, sizeof(tmp));
  tmp.deadline_source = "";

  p = hermes_proc_acquire(proc);
  if (p == NULL) {
    memset(out, 0, nbytes);
    return HM_E_HANDLE_STALE;
  }

  hermes_proc_table_lock();
  if (p->waiting) {
    hermes_proc_table_unlock();
    hermes_proc_put(p);
    memset(out, 0, nbytes);
    return HM_E_INVALID_ARG;
  }
  p->waiting = 1;
  start_cycles = hermes_hw_counter();
  p->start_cycles = start_cycles;
  if (p->deadline_cycles > 0) {
    deadline_at = start_cycles + p->deadline_cycles;
  }
  grace_ns = (uint64_t)p->kill_grace_ms * 1000000ull;
  hermes_proc_table_unlock();

  freq = hermes_hw_counter_freq(); /* 0 = 未知 → 只用 wall backstop */
  wall_start_ns = mono_ns();

  for (;;) {
    int st = 0;
    pid_t r;

    pump_stdin(p);
    (void)pump_fd(p->out_fd, &p->out_buf, &p->out_len,
                  &out_alloced, p->out_cap);
    (void)pump_fd(p->err_fd, &p->err_buf, &p->err_len,
                  &err_alloced, p->err_cap);
    pump_setup(p);

    r = waitpid(p->pid, &st, WNOHANG);
    if (r == p->pid) {
      hermes_proc_table_lock();
      p->status = st;
      p->reaped = 1;
      hermes_proc_table_unlock();
      break;
    }
    if (r < 0 && errno != EINTR) {
      hermes_proc_table_lock();
      p->reaped = 1;
      p->status = 0;
      hermes_proc_table_unlock();
      break;
    }

    /* --- 触发判定：取消 / deadline / wall 兜底 --- */
    {
      int trigger = 0;
      const char *why = "";
      uint64_t now_cycles = hermes_hw_counter();
      uint64_t now_ns = mono_ns();

      hermes_proc_table_lock();
      if (!p->term_sent) {
        if (p->cancelled) {
          trigger = 1;
          why = ""; /* 取消不是 deadline；上层自己知道它取消了 */
        } else if (deadline_at != 0 && now_cycles >= deadline_at) {
          trigger = 1;
          why = "cycle";
        } else if (p->wall_backstop_ns != 0 &&
                   (now_ns - wall_start_ns) >= p->wall_backstop_ns) {
          trigger = 1;
          why = "wall";
        }
        if (trigger) {
          p->term_sent = 1;
          term_sent_ns = now_ns;
          deadline_hit = 1;
          deadline_source = why;
          fired_cycles = now_cycles;
        }
      }
      hermes_proc_table_unlock();

      if (trigger) {
        kill_group(p, SIGTERM);
      } else {
        /* --- 宽限期到：升级 SIGKILL(-pgid) --- */
        int need_kill = 0;
        hermes_proc_table_lock();
        if (p->term_sent && !p->killed && term_sent_ns != 0 &&
            (mono_ns() - term_sent_ns) >= grace_ns) {
          need_kill = 1;
        }
        if (need_kill) {
          p->killed = 1;
        }
        hermes_proc_table_unlock();
        if (need_kill) {
          escalated = 1;
          kill_group(p, SIGKILL);
        }
      }
    }

    /*
     * 轮询周期 2 ms。用 nanosleep 而不是 poll 等待：我们是"每 2 ms 看一眼"，
     * 不是"等到 fd 可读"，后者在子进程静默时会永远不返回。
     */
    sleep_ms(2);
    loops++;
    if (loops > 3600000) { /* ~2 小时硬上限，防病态配置把线程钉死 */
      hermes_proc_table_lock();
      p->killed = 1;
      hermes_proc_table_unlock();
      escalated = 1;
      kill_group(p, SIGKILL);
    }
  }

  /*
   * 第 5 步：收割之后仍然要把管道读到 EOF。
   *
   * 如果工具派生了一个持有 stdout 写端的孙进程，直接 close 会丢掉尾部数据；
   * 如果我们在 read 上无限等，run 会卡死。折中：有界轮询（最多 200 × 10ms），
   * 若我们确实发过信号则先补一记 SIGKILL 把残留组员清掉，让 EOF 立刻到来。
   */
  if (escalated) {
    kill_group(p, SIGKILL);
  }
  {
    int i;
    for (i = 0; i < 200; i++) {
      struct pollfd pfd[2];
      int done = 1;
      int nfds = 0;

      if (p->out_fd >= 0) {
        pfd[nfds].fd = p->out_fd;
        pfd[nfds].events = POLLIN;
        pfd[nfds].revents = 0;
        nfds++;
      }
      if (p->err_fd >= 0) {
        pfd[nfds].fd = p->err_fd;
        pfd[nfds].events = POLLIN;
        pfd[nfds].revents = 0;
        nfds++;
      }
      if (nfds == 0) {
        break;
      }
      (void)poll(pfd, (nfds_t)nfds, 10);
      if (p->out_fd >= 0) {
        if (pump_fd(p->out_fd, &p->out_buf, &p->out_len,
                    &out_alloced, p->out_cap) == 1) {
          (void)close(p->out_fd);
          p->out_fd = -1;
        }
      }
      if (p->err_fd >= 0) {
        if (pump_fd(p->err_fd, &p->err_buf, &p->err_len,
                    &err_alloced, p->err_cap) == 1) {
          (void)close(p->err_fd);
          p->err_fd = -1;
        }
      }
      if (p->setup_fd >= 0) {
        pump_setup(p);
      }
      if (p->out_fd < 0 && p->err_fd < 0) {
        done = 1;
        break;
      }
      (void)done;
    }
  }
  pump_setup(p);

  /* --- 填结果 --- */
  {
    int st = p->status;
    hermes_proc_table_lock();
    tmp.exited = WIFEXITED(st) ? 1 : 0;
    tmp.exit_code = WIFEXITED(st) ? WEXITSTATUS(st) : -1;
    tmp.signaled = WIFSIGNALED(st) ? 1 : 0;
    tmp.term_signal = WIFSIGNALED(st) ? WTERMSIG(st) : 0;
    tmp.escalated = escalated;
    tmp.deadline_hit = deadline_hit;
    /* 取消也要记录被杀进程组：trace 里"我们杀了谁"不能因为触发原因不同就缺失。
     * deadline_hit 只表示"因 deadline/兜底而杀"，取消走 cancelled 标志。 */
    tmp.killed_pgid = (deadline_hit || p->cancelled) ? p->pgid : 0;
    tmp.deadline_source = deadline_source;
    tmp.fired_cycles = fired_cycles;
    tmp.setup_errno = p->setup_errno;
    tmp.stdout_bytes = p->out_buf;
    tmp.stdout_len = p->out_len;
    tmp.stderr_bytes = p->err_buf;
    tmp.stderr_len = p->err_len;
    p->out_buf = NULL;
    p->err_buf = NULL;
    p->waiting = 0;
    hermes_proc_table_unlock();
  }

  /* 关掉还没关的 fd，释放 stdin 缓冲 */
  if (p->out_fd >= 0) {
    (void)close(p->out_fd);
    p->out_fd = -1;
  }
  if (p->err_fd >= 0) {
    (void)close(p->err_fd);
    p->err_fd = -1;
  }
  if (p->setup_fd >= 0) {
    (void)close(p->setup_fd);
    p->setup_fd = -1;
  }
  if (p->in_fd >= 0) {
    (void)close(p->in_fd);
    p->in_fd = -1;
  }
  free(p->in_buf);
  p->in_buf = NULL;
  p->in_len = 0;
  p->in_off = 0;

  (void)freq;
  tmp.abi_size = (uint32_t)sizeof(hm_spawn_res);
  memcpy(out, &tmp, nbytes);
  if (nbytes >= sizeof(uint32_t)) {
    out->abi_size = (uint32_t)sizeof(hm_spawn_res);
  }

  hermes_proc_put(p);
  return HM_OK;
}

/* ------------------------------------------------------------------ *
 * hm_spawn_cancel —— 线程安全，可从任意 goroutine 调
 *
 * 立即向 -pgid 发 SIGTERM，不等 wait 循环下一个 tick：取消的语义是
 * "现在停"，让调用方等 2 ms 是不能接受的。
 * ------------------------------------------------------------------ */

hm_status hm_spawn_cancel(hm_handle proc) {
  hm_proc *p = hermes_proc_acquire(proc);
  pid_t pgid;
  int send_now = 0;

  if (p == NULL) {
    return HM_E_HANDLE_STALE;
  }

  hermes_proc_table_lock();
  if (!p->cancelled) {
    p->cancelled = 1;
    g_stats.total_cancels++;
    if (!p->term_sent && p->pgid > 0) {
      p->term_sent = 1;
      send_now = 1;
    }
    pgid = p->pgid;
  } else {
    pgid = p->pgid;
  }
  hermes_proc_table_unlock();

  if (send_now && pgid > 0) {
    (void)kill(-pgid, SIGTERM);
  }
  hermes_proc_put(p);
  return HM_OK;
}

/* ------------------------------------------------------------------ *
 * hm_spawn_free / hm_spawn_res_free
 * ------------------------------------------------------------------ */

hm_status hm_spawn_free(hm_handle proc) {
  uint32_t slot = hermes_handle_slot(proc);
  uint32_t gen = hermes_handle_gen(proc);
  hm_proc *p;
  hm_status st = HM_OK;

  if (proc == HM_HANDLE_INVALID) {
    return HM_E_INVALID_ARG;
  }

  hermes_proc_table_lock();
  p = hermes_proc_storage((int)slot);
  if (p == NULL || !p->used || p->gen != gen) {
    hermes_proc_table_unlock();
    return HM_E_HANDLE_STALE;
  }
  if (p->refcnt > 0 || p->waiting) {
    /* 还有人在用（通常是 wait 还没返回）。调用方必须先 wait 再 free。 */
    hermes_proc_table_unlock();
    return HM_E_INVALID_ARG;
  }
  if (!p->reaped && p->pid > 0) {
    /*
     * 没 wait 就 free：不能让子进程变成僵尸，也不能泄漏进程组。
     * 杀掉整组并收割。
     */
    (void)kill(-p->pgid, SIGKILL);
    (void)waitpid(p->pid, NULL, 0);
  }
  if (p->in_fd >= 0) {
    (void)close(p->in_fd);
    p->in_fd = -1;
  }
  if (p->out_fd >= 0) {
    (void)close(p->out_fd);
    p->out_fd = -1;
  }
  if (p->err_fd >= 0) {
    (void)close(p->err_fd);
    p->err_fd = -1;
  }
  if (p->setup_fd >= 0) {
    (void)close(p->setup_fd);
    p->setup_fd = -1;
  }
  free(p->in_buf);
  free(p->out_buf);
  free(p->err_buf);
  p->in_buf = NULL;
  p->out_buf = NULL;
  p->err_buf = NULL;
  hermes_proc_release_locked((int)slot);
  hermes_proc_table_unlock();
  return st;
}

void hm_spawn_res_free(hm_spawn_res *res) {
  if (res == NULL) {
    return;
  }
  /* R5：C malloc 的内存只能由这里释放。 */
  free(res->stdout_bytes);
  free(res->stderr_bytes);
  {
    size_t n = sizeof(*res);
    if (res->abi_size != 0 && (size_t)res->abi_size < n) {
      n = (size_t)res->abi_size;
    }
    memset(res, 0, n);
  }
  res->abi_size = (uint32_t)sizeof(hm_spawn_res);
}

#endif /* !_WIN32 */
