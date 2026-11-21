/* Windows 上这两个文件整体不参与编译：它们建立在 POSIX 进程模型上
 * （posix_spawn / 进程组 / 信号）。Windows 的实现见 native/c/win_spawn.c，
 * 对外导出同一组 hm_proc_* 符号（含全部 getter），所以调用方不需要区分平台。 */
#if !defined(_WIN32)
/*
 * supervisor.c —— hm_proc_wait / hm_proc_cancel / hm_proc_free / 进程表 /
 * 全部 hm_proc_get_* 与 hm_proc_copy_* getter。
 *
 * F-11b（v4）：一次调用发生了什么**只**通过 getter 从 handle 读出。槽位是
 * 唯一真相，hm_proc_wait 就地写槽位，getter 是只读投影。因此：
 *   - hm_proc_wait 没有出参、没有 abi_size、没有大小协商；
 *   - `p->reaped = 1` 是**发布屏障**，必须是槽位里最后一次写；
 *   - getter 一律 acquire（refcnt++）→ 表锁内校验 → 读到局部 → 解锁 → put，
 *     于是 hm_proc_free 在 getter 在飞时返回 HM_E_INVALID_ARG，
 *     不存在 free-during-getter 的 use-after-free。
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

/* fd 3 记录里的小端 u32 解码。 */
static uint32_t setup_read_u32_le(const uint8_t *b) {
  return (uint32_t)b[0] | ((uint32_t)b[1] << 8) |
         ((uint32_t)b[2] << 16) | ((uint32_t)b[3] << 24);
}

/* 消费累加缓冲开头 n 字节，把剩余部分前移。 */
static void setup_consume(hm_proc *p, size_t n) {
  if (n >= p->setup_acc_len) {
    p->setup_acc_len = 0;
    return;
  }
  memmove(p->setup_acc, p->setup_acc + n, p->setup_acc_len - n);
  p->setup_acc_len -= n;
}

/*
 * fd 3 的**小累加解析**（F-11 + F-11b）。
 *
 * shim 会写四类记录，可能一次 read 粘连、也可能跨多次 read 拆开：
 *   - 8 字节 [HM_SHIM_SETUP_TAG_IGNORED][ignored_flags] → 按位累加；
 *   - 8 字节 [HM_SHIM_SETUP_TAG_ACKED ][applied_flags] → 按位累加（F-11b）；
 *   - 8 字节 [HM_SHIM_SETUP_TAG_FAILED][errno]         → setup_failed=1；
 *   - 4 字节 [errno]（无 tag）                          → 首个非零者胜出，
 *                                                        且置 setup_exec_failed。
 * 只消费"完整记录"；残缺的 tag 头留在累加缓冲里等下一次 read。
 * 这同时覆盖"成功 ignored/ACK 记录 + execve 失败二次写 errno"：两者粘在一次
 * read 里也不会把 errno 丢掉。n == 0 仍然表示 fd 3 关闭（EOF）。
 *
 * 关键区分（E2 的根因）：裸 errno 那条分支**不置** setup_failed。execve 失败
 * 时 setup 是成功的，把它算成 setup 失败会让业务自己的 exit(40) 被误拒。
 */
static void pump_setup(hm_proc *p) {
  if (p->setup_closed || p->setup_fd < 0) {
    return;
  }
  for (;;) {
    size_t room = sizeof(p->setup_acc) - p->setup_acc_len;
    ssize_t n;

    if (room == 0) {
      return; /* 防御性：正常路径下解析循环已经把完整记录消费掉 */
    }
    n = read(p->setup_fd, p->setup_acc + p->setup_acc_len, room);
    if (n == 0) {
      p->setup_closed = 1;
      /* F-11b：EOF 本身不是 execve 成功的证据（shim 在 execve 之前被
       * SIGKILL 时内核也会关掉 fd 3），只是"exec 时序判据"的一个输入。
       * 谁把它当成功证据，E3 就会绿。 */
      p->setup_eof = 1;
      (void)close(p->setup_fd);
      p->setup_fd = -1;
      return;
    }
    if (n < 0) {
      if (errno == EINTR) {
        continue;
      }
      if (errno == EAGAIN || errno == EWOULDBLOCK) {
        return;
      }
      /* read 出错 ≠ EOF：不置 setup_eof，否则会把 I/O 故障当成"exec 成功"。 */
      p->setup_closed = 1;
      (void)close(p->setup_fd);
      p->setup_fd = -1;
      return;
    }
    p->setup_acc_len += (size_t)n;

    while (p->setup_acc_len >= (size_t)HM_SHIM_ERRFD_PAYLOAD) {
      uint32_t head = setup_read_u32_le(p->setup_acc);
      if (head == HM_SHIM_SETUP_TAG_IGNORED) {
        if (p->setup_acc_len < (size_t)HM_SHIM_IGNORED_RECORD) {
          break; /* 记录被 read 拆开，等下半截 */
        }
        p->sandbox_ignored |=
            setup_read_u32_le(p->setup_acc + HM_SHIM_ERRFD_PAYLOAD);
        setup_consume(p, HM_SHIM_IGNORED_RECORD);
        continue;
      }
      if (head == HM_SHIM_SETUP_TAG_ACKED) {
        if (p->setup_acc_len < (size_t)HM_SHIM_ACKED_RECORD) {
          break; /* 同上：等后半截 */
        }
        /* F-11b 正面证据：shim 实际施加成功的位。按位或累加，
         * 多条 ACK（跨 exec 阶段重写）也不会丢掉任何一位。 */
        p->sb_acked |=
            setup_read_u32_le(p->setup_acc + HM_SHIM_ERRFD_PAYLOAD);
        setup_consume(p, HM_SHIM_ACKED_RECORD);
        continue;
      }
      if (head == HM_SHIM_SETUP_TAG_FAILED) {
        if (p->setup_acc_len < (size_t)HM_SHIM_FAILED_RECORD) {
          break;
        }
        {
          uint32_t fe = setup_read_u32_le(p->setup_acc + HM_SHIM_ERRFD_PAYLOAD);
          p->setup_failed = 1;
          if (fe != 0 && p->setup_errno == 0) {
            p->setup_errno = (int)fe;
          }
        }
        setup_consume(p, HM_SHIM_FAILED_RECORD);
        continue;
      }
      /* 裸 4 字节 errno：execve 失败（既有语义）。**不置** setup_failed ——
       * execve 失败时 setup 是成功的，把它算成 setup 失败会让业务自己的
       * exit(40) 被误拒（E2 的根因）。改置 setup_exec_failed：它是"目标
       * 从未启动"的语义信号，Go 侧据此放行而不是拒绝。 */
      p->setup_exec_failed = 1;
      if (head != 0 && p->setup_errno == 0) {
        p->setup_errno = (int)head;
      }
      setup_consume(p, HM_SHIM_ERRFD_PAYLOAD);
    }
    /* 不 return：继续读到 EAGAIN / EOF，把粘连的后续记录一并排空。 */
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
 * hm_proc_wait
 *
 * F-11b（v4）：**无出参**。结果就地写槽位，调用方随后用 getter 读。
 * 这是"没有固定大小结构体承载返回值"的落地点 —— 没有 out、没有 abi_size、
 * 没有 memcpy、没有需要在别处配对的 free。
 * ------------------------------------------------------------------ */

hm_status hm_proc_wait(hm_handle proc) {
  hm_proc *p;
  uint64_t freq;
  uint64_t start_cycles;
  uint64_t deadline_at = 0;
  uint64_t wall_start_ns;
  uint64_t term_sent_ns = 0;
  uint64_t grace_ns;
  int deadline_source_kind = 0; /* 0="" 1="cycle" 2="wall" */
  uint64_t fired_cycles = 0;
  int deadline_hit = 0;
  int escalated = 0;
  size_t out_alloced = 0;
  size_t err_alloced = 0;
  int loops = 0;
  int eof_while_alive = 0; /* F-11b：fd 3 EOF 时子进程仍存活 = execve 已发生 */

  p = hermes_proc_acquire(proc);
  if (p == NULL) {
    return HM_E_HANDLE_STALE;
  }

  hermes_proc_table_lock();
  if (p->waiting) {
    hermes_proc_table_unlock();
    hermes_proc_put(p);
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
    int eof_now;
    pid_t r;

    pump_stdin(p);
    (void)pump_fd(p->out_fd, &p->out_buf, &p->out_len,
                  &out_alloced, p->out_cap);
    (void)pump_fd(p->err_fd, &p->err_buf, &p->err_len,
                  &err_alloced, p->err_cap);
    pump_setup(p);

    /*
     * F-11b 的 exec 时序判据（E3/E4 的消歧点）。
     *
     * fd 3 通道上"execve 成功"与"execve 之前被 SIGKILL"**都**表现为 EOF：
     * shim 给 fd 3 打了 FD_CLOEXEC，两种情况内核都会关掉写端。唯一还能区分
     * 它们的信息是**时序**：execve 成功时，EOF 到来的那一刻目标进程还活着
     * （它刚刚继承了这个 pid）；被 SIGKILL 时 EOF 与死亡是同一件事。
     *
     * 所以顺序必须是 pump_setup → 读 setup_eof → waitpid(WNOHANG)：
     *   eof_now && waitpid(...) == 0  →  fd 3 已关但子进程仍在 → exec 已发生。
     * 反过来先 waitpid 再读，被杀的 shim 也会因为"我们先看到死"而拿到 0，
     * 判据就退化成"永远不确定"，E4 会被误拒。
     */
    eof_now = p->setup_eof;

    r = waitpid(p->pid, &st, WNOHANG);
    if (r == 0 && eof_now) {
      eof_while_alive = 1;
    }
    if (r == p->pid) {
      hermes_proc_table_lock();
      p->status = st;
      /* F-11b：**不在这里**置 reaped。结果还没排空、派生值还没算完，
       * 提前发布会让并发 getter 看到 REAPED + 半成品数据（"用 0 冒充"的
       * 另一种形态）。reaped=1 移到填结果块的最后一行。 */
      hermes_proc_table_unlock();
      break;
    }
    if (r < 0 && errno != EINTR) {
      hermes_proc_table_lock();
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
          if (!p->cancelled) {
            deadline_hit = 1;
            /* 存成枚举而不是 const char*：槽位里不放指针，getter 再映射成
             * "cycle" / "wall" / "" 的静态字符串。 */
            deadline_source_kind = (why[0] == 'c') ? 1 : ((why[0] == 'w') ? 2 : 0);
          }
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

  /* --- 填结果：就地写槽位，reaped=1 是**最后一次写**（发布屏障） ---
   *
   * 顺序即正确性：派生值 → setup/sb 面 → waiting=0 → reaped=1。
   * 任何 getter 都在表锁内校验 reaped，所以它要么看到"未收割"，要么看到
   * 一份完整的结果；中间态不可观测。 */
  hermes_proc_table_lock();
  /* exited/exit_code/signaled/term_signal 由 p->status 现算（getter 里同样
   * 用 WIFEXITED/WIFSIGNALED 派生），不必在槽位里存 4 份冗余副本。 */
  p->deadline_hit = deadline_hit;
  /* 取消也要记录被杀进程组：trace 里"我们杀了谁"不能因为触发原因不同就缺失。
   * deadline_hit 只表示"因 deadline/兜底而杀"，取消走 cancelled 标志。 */
  p->killed_pgid = (deadline_hit || p->cancelled) ? p->pgid : 0;
  p->fired_cycles = fired_cycles;
  p->deadline_source_kind = deadline_source_kind;
  /* F-11b：exec 时序判据的结果落槽位；只在 setup_channel 置位时有意义。 */
  p->setup_eof_while_alive = eof_while_alive ? 1u : 0u;
  p->waiting = 0;
  p->reaped = 1; /* ← 发布屏障：必须是槽位里最后一次写 */
  hermes_proc_table_unlock();

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
  /* v4：不再有 out / nbytes / memcpy。stdout/stderr 缓冲区**留在槽位里**，
   * 由 hm_proc_copy_stdout / hm_proc_copy_stderr 拷出，由 hm_proc_free 释放。
   * R5 的"跨分配器 free"整条规则随之消失（没有需要配对的 free 函数）。 */
  hermes_proc_put(p);
  return HM_OK;
}

/* ------------------------------------------------------------------ *
 * Getter 族（F-11b / v4）
 *
 * 全部 out-of-line：必须是**真实导出符号**，否则"新 Go + 旧库"就从链接期
 * 硬失败退化成静默误读（见 hermes_abi.h 里 R8 第 2 条末句与 G1 守卫）。
 *
 * 统一形态（§3.4）：
 *   acquire（refcnt++）→ 表锁内校验 reaped → 读到局部变量 → 解锁 → put。
 * refcnt > 0 期间 hm_proc_free 会拒绝（supervisor.c 的 hm_proc_free 检查），
 * 所以"getter 在飞时被 free"不会产生 use-after-free。
 *
 * 出参 NULL 规则（§3.3，统一无例外）：任一标量出参为 NULL ⇒ HM_E_INVALID_ARG
 * 且**一个出参都不写**；唯一例外是 copy_* 的 buf（NULL = 两阶段第一阶段）。
 * 未收割 ⇒ HM_E_NOT_READY 且一个出参都不写 —— "还没算出来"绝不表现为 0。
 * ------------------------------------------------------------------ */

/* 见上：成功返回持 refcnt 的槽位指针，失败返回 NULL 并给出错误码。 */
static hm_proc *getter_acquire_reaped(hm_handle proc, hm_status *st) {
  hm_proc *p = hermes_proc_acquire(proc);

  if (p == NULL) {
    *st = HM_E_HANDLE_STALE;
    return NULL;
  }
  hermes_proc_table_lock();
  if (!p->reaped) {
    hermes_proc_table_unlock();
    hermes_proc_put(p);
    *st = HM_E_NOT_READY;
    return NULL;
  }
  hermes_proc_table_unlock();
  *st = HM_OK;
  return p;
}

/* deadline_source 的三种取值。用静态串 + 拷贝式接口，不返回 const char*：
 * 那会引入"借用期"规则，与 R4 的 hm_view_borrow 混淆。 */
static const char k_deadline_src_none[] = "";
static const char k_deadline_src_cycle[] = "cycle";
static const char k_deadline_src_wall[] = "wall";

hm_status hm_proc_get_state(hm_handle proc, int32_t *out_state) {
  hm_proc *p;
  int32_t v;

  if (out_state == NULL) {
    return HM_E_INVALID_ARG;
  }
  /* 唯一**不要求**已收割的 getter：RUNNING 是一个合法答案。 */
  p = hermes_proc_acquire(proc);
  if (p == NULL) {
    return HM_E_HANDLE_STALE;
  }
  hermes_proc_table_lock();
  v = p->reaped ? (int32_t)HM_PROC_REAPED : (int32_t)HM_PROC_RUNNING;
  hermes_proc_table_unlock();
  hermes_proc_put(p);

  *out_state = v;
  return HM_OK;
}

hm_status hm_proc_get_term(hm_handle proc, uint32_t *out_bits,
                           int32_t *out_exit_code, int32_t *out_term_signal,
                           int32_t *out_killed_pgid) {
  hm_proc *p;
  hm_status st;
  uint32_t bits;
  int32_t exit_code;
  int32_t term_signal;
  int32_t killed_pgid;

  /* 全有或全无：任一 NULL ⇒ 整体拒绝，一个出参都不写。 */
  if (out_bits == NULL || out_exit_code == NULL || out_term_signal == NULL ||
      out_killed_pgid == NULL) {
    return HM_E_INVALID_ARG;
  }
  p = getter_acquire_reaped(proc, &st);
  if (p == NULL) {
    return st;
  }

  /* D2：四个值必须来自**同一次槽位读取** —— 先全部取到局部变量，再统一
   * 写出。边读边写会让调用方看到"跨字段撕裂"的组合（例如 EXITED 配一个
   * 属于另一次 wait 的 exit_code），那比缺字段更难诊断。 */
  hermes_proc_table_lock();
  {
    int s = p->status;
    bits = 0;
    exit_code = WIFEXITED(s) ? (int32_t)WEXITSTATUS(s) : -1;
    term_signal = WIFSIGNALED(s) ? (int32_t)WTERMSIG(s) : 0;
    /* escalated 不另存字段：p->killed 的语义就是"已向进程组发过 SIGKILL"，
     * 与旧 escalated 的置位点逐一对应（含 loops 超硬的兜底那一处）。 */
    if (WIFEXITED(s)) {
      bits |= HM_TERM_EXITED;
    }
    if (WIFSIGNALED(s)) {
      bits |= HM_TERM_SIGNALED;
    }
    if (p->killed) {
      bits |= HM_TERM_ESCALATED;
    }
    if (p->deadline_hit) {
      bits |= HM_TERM_DEADLINE_HIT;
    }
    killed_pgid = (int32_t)p->killed_pgid;
  }
  hermes_proc_table_unlock();
  hermes_proc_put(p);

  *out_bits = bits;
  *out_exit_code = exit_code;
  *out_term_signal = term_signal;
  *out_killed_pgid = killed_pgid;
  return HM_OK;
}

hm_status hm_proc_get_deadline(hm_handle proc, uint64_t *out_fired_cycles) {
  hm_proc *p;
  hm_status st;
  uint64_t v;

  if (out_fired_cycles == NULL) {
    return HM_E_INVALID_ARG;
  }
  p = getter_acquire_reaped(proc, &st);
  if (p == NULL) {
    return st;
  }
  hermes_proc_table_lock();
  v = p->fired_cycles;
  hermes_proc_table_unlock();
  hermes_proc_put(p);

  *out_fired_cycles = v;
  return HM_OK;
}

hm_status hm_proc_copy_deadline_source(hm_handle proc, char *buf, size_t cap,
                                       size_t *out_len) {
  hm_proc *p;
  hm_status st;
  const char *src;
  size_t len;
  int kind;

  if (out_len == NULL) {
    return HM_E_INVALID_ARG;
  }
  p = getter_acquire_reaped(proc, &st);
  if (p == NULL) {
    /* 未就绪/句柄过期：*out_len 不写（真值表 §3.2）。 */
    return st;
  }
  hermes_proc_table_lock();
  kind = p->deadline_source_kind;
  hermes_proc_table_unlock();
  hermes_proc_put(p);

  src = (kind == 1) ? k_deadline_src_cycle
                    : ((kind == 2) ? k_deadline_src_wall : k_deadline_src_none);
  len = strlen(src);

  /* 字符串语义（D6）：*out_len 成功时 = 字符数（不含 '\0'），需要
   * cap = len + 1；cap 不足时 *out_len 写"所需 cap"（len + 1）并返回
   * HM_E_TOO_LARGE，buf 一个字节都不写（R3：绝不截断）。
   * 与下面字节流两条规则**分开**，不共用一段说明 —— 混用就是差一个字节的
   * 缓冲区溢出。 */
  if (buf == NULL) {
    *out_len = len;
    return HM_OK;
  }
  if (cap < len + 1) {
    *out_len = len + 1;
    return HM_E_TOO_LARGE;
  }
  if (len > 0) {
    memcpy(buf, src, len);
  }
  buf[len] = '\0';
  *out_len = len;
  return HM_OK;
}

hm_status hm_proc_get_setup(hm_handle proc, uint32_t *out_bits,
                            int32_t *out_setup_errno) {
  hm_proc *p;
  hm_status st;
  uint32_t bits;
  int32_t err;

  if (out_bits == NULL || out_setup_errno == NULL) {
    return HM_E_INVALID_ARG;
  }
  p = getter_acquire_reaped(proc, &st);
  if (p == NULL) {
    return st;
  }
  hermes_proc_table_lock();
  bits = 0;
  if (p->setup_failed) {
    bits |= HM_SETUP_FAILED;
  }
  if (p->setup_exec_failed) {
    bits |= HM_SETUP_EXEC_FAILED;
  }
  if (p->setup_eof_while_alive) {
    bits |= HM_SETUP_EXEC_CONFIRMED;
  }
  /* 粘住的 "本次 spawn 具备 fd3 通道" 事实位。POSIX 上 hm_proc_wait 收尾时
   * 会把 setup_fd 关掉并置 -1，所以**绝不能**在这里按 setup_fd >= 0 现算。 */
  if (p->setup_channel) {
    bits |= HM_SETUP_CHANNEL_MEASURED;
  }
  err = (int32_t)p->setup_errno;
  hermes_proc_table_unlock();
  hermes_proc_put(p);

  *out_bits = bits;
  *out_setup_errno = err;
  return HM_OK;
}

hm_status hm_proc_get_sb_requested(hm_handle proc, uint32_t *out_flags) {
  hm_proc *p;
  hm_status st;
  uint32_t v;

  if (out_flags == NULL) {
    return HM_E_INVALID_ARG;
  }
  p = getter_acquire_reaped(proc, &st);
  if (p == NULL) {
    return st;
  }
  hermes_proc_table_lock();
  v = p->sb_requested;
  hermes_proc_table_unlock();
  hermes_proc_put(p);

  *out_flags = v;
  return HM_OK;
}

hm_status hm_proc_get_sb_acked(hm_handle proc, uint32_t *out_flags) {
  hm_proc *p;
  hm_status st;
  uint32_t v;

  if (out_flags == NULL) {
    return HM_E_INVALID_ARG;
  }
  p = getter_acquire_reaped(proc, &st);
  if (p == NULL) {
    return st;
  }
  hermes_proc_table_lock();
  v = p->sb_acked;
  hermes_proc_table_unlock();
  hermes_proc_put(p);

  *out_flags = v;
  return HM_OK;
}

hm_status hm_proc_get_sb_ignored(hm_handle proc, uint32_t *out_flags) {
  hm_proc *p;
  hm_status st;
  uint32_t v;

  if (out_flags == NULL) {
    return HM_E_INVALID_ARG;
  }
  p = getter_acquire_reaped(proc, &st);
  if (p == NULL) {
    return st;
  }
  hermes_proc_table_lock();
  v = p->sandbox_ignored;
  hermes_proc_table_unlock();
  hermes_proc_put(p);

  *out_flags = v;
  return HM_OK;
}

/* 字节流两阶段拷贝的共用实现。规则（与字符串那条**不同**）：
 *   out_len == NULL            → HM_E_INVALID_ARG，不写任何东西
 *   buf == NULL                → 只写 *out_len = 需要的字节数，HM_OK
 *   buf != NULL && cap < 需要   → *out_len = 需要，HM_E_TOO_LARGE，buf 不写
 *   否则                        → memcpy，*out_len = 需要，HM_OK
 * *out_len 在所有路径上都是同一个意思：需要的/已写的字节数（没有 +1）。
 * 内存始终在 C 侧槽位里：调用方只往里 memcpy，从不接手所有权。 */
static hm_status copy_stream(hm_handle proc, int which_stderr, uint8_t *buf,
                             size_t cap, size_t *out_len) {
  hm_proc *p;
  hm_status st;
  const uint8_t *src;
  size_t need;

  if (out_len == NULL) {
    return HM_E_INVALID_ARG;
  }
  p = getter_acquire_reaped(proc, &st);
  if (p == NULL) {
    return st; /* 未就绪：*out_len 不写 */
  }
  hermes_proc_table_lock();
  if (which_stderr) {
    src = p->err_buf;
    need = p->err_len;
  } else {
    src = p->out_buf;
    need = p->out_len;
  }
  hermes_proc_table_unlock();

  if (buf == NULL) {
    hermes_proc_put(p);
    *out_len = need;
    return HM_OK;
  }
  if (cap < need) {
    hermes_proc_put(p);
    *out_len = need;
    return HM_E_TOO_LARGE;
  }
  /* src 在 put 之前始终有效：refcnt > 0 让 hm_proc_free 拒绝归还槽位，
   * 而缓冲区只在 hm_proc_free 里 free。 */
  if (need > 0) {
    memcpy(buf, src, need);
  }
  *out_len = need;
  hermes_proc_put(p);
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

/* ------------------------------------------------------------------ *
 * hm_proc_cancel —— 线程安全，可从任意 goroutine 调
 *
 * 立即向 -pgid 发 SIGTERM，不等 wait 循环下一个 tick：取消的语义是
 * "现在停"，让调用方等 2 ms 是不能接受的。
 * ------------------------------------------------------------------ */

hm_status hm_proc_cancel(hm_handle proc) {
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
 * hm_proc_free —— 归还槽位
 *
 * refcnt > 0（有 getter 在飞）或 waiting（wait 未返回）时返回
 * HM_E_INVALID_ARG：那是**调用顺序错**，不是资源泄漏，更不能拿"反正没人看"
 * 当理由去 free —— 那正是 free-during-getter 的 use-after-free。
 *
 * 其余两种"没有东西可释放"的情形（已释放的陈旧句柄、HM_HANDLE_INVALID）
 * 都是**无操作、返回 HM_OK**：清理可以重复，读取不可以（契约见 hermes_abi.h）。
 * ------------------------------------------------------------------ */

hm_status hm_proc_free(hm_handle proc) {
  uint32_t slot = hermes_handle_slot(proc);
  uint32_t gen = hermes_handle_gen(proc);
  hm_proc *p;
  hm_status st = HM_OK;

  if (proc == HM_HANDLE_INVALID) {
    /*
     * `HM_HANDLE_INVALID`（0）同样落在"不是本表签发的句柄"里 ⇒ 无操作、HM_OK，
     * 与下面那段 stale 分支同一语义（hermes_abi.h 的契约：已释放 **或压根不是
     * 本表签发的**句柄都是无操作）。
     *
     * 依据三条：
     *   1. C 标准里 `free(NULL)` 就是合法 no-op，`hm_proc_free(0)` 沿用同一惯例；
     *   2. 契约是"**清理可以重复**"，而"清理一个不存在的进程"正是重复的极端情形；
     *   3. 读取面**不享受**这个宽容：hm_proc_wait / hm_proc_cancel / 13 个 getter
     *      对 0 仍然是错误（HM_E_HANDLE_STALE / HM_E_INVALID_ARG），不许跟着放宽。
     *
     * 为什么不直接删掉这个前置分支靠下面的槽位查找兜：`slot_of(0)` 恰好因为
     * gen 规则落到 stale ⇒ 也是 HM_OK，但那是**耦合出来的巧合**（依赖"槽位 0 的
     * gen 永不为 0"这条实现细节），不该被依赖。显式分支把语义写在脸上。
     */
    return HM_OK;
  }

  hermes_proc_table_lock();
  p = hermes_proc_storage((int)slot);
  if (p == NULL || !p->used || p->gen != gen) {
    /*
     * 幂等（契约见 hermes_abi.h 的 hm_proc_free）：已释放 / 非本表签发的
     * 句柄 = "没有东西可释放" = 无操作，返回 HM_OK。
     *
     * 这与同文件里 wait / cancel / getter 的 HM_E_HANDLE_STALE **刻意相反**：
     * 清理可以重复（finalizer + 显式 defer 各调一次），读取不可以
     * （拿零值当答案是"用 0 冒充"）。两个方向都要是对的，缺一个就自相矛盾。
     */
    hermes_proc_table_unlock();
    return HM_OK;
  }
  if (p->refcnt > 0 || p->waiting) {
    /* 还有人在用（通常是 wait 还没返回）。调用方必须先 wait 再 free。 */
    hermes_proc_table_unlock();
    return HM_E_INVALID_ARG;
  }
  if (!p->reaped && p->pid > 0) {
    /*
     * F2：wait 之前就 free = "取消 + 收尸"，返回 HM_OK（契约见 hermes_abi.h
     * 里 hm_proc_free 的注释）。POSIX 侧一直就是这个行为，这里只把它写成
     * 契约的一部分，别让 Windows 侧以为可以不杀。
     *
     * 不能让子进程变成僵尸，也不能泄漏进程组：杀整组 + waitpid 收割。
     * 用 SIGKILL 而不是 SIGTERM：free 的语义是"我不要这个结果了"，此时
     * 没有机会再等一次优雅退出 —— 那正是 wait 该做的事。
     *
     * 为什么要分两支：`kill(-p->pgid, ...)` 在 pgid == 0 时是 kill(0, SIGKILL)
     * ——把我们**自己的**进程组全杀了。pgid 正常由 spawn 填成子进程 pid，
     * 但清理路径不该依赖"上游一定填过"。
     */
    if (p->pgid > 0) {
      kill_group(p, SIGKILL); /* 整组：连派生出来的孙进程一起带走 */
    } else {
      (void)kill(p->pid, SIGKILL); /* 兜底：只杀直接子进程 */
    }
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

#endif /* !_WIN32 */
