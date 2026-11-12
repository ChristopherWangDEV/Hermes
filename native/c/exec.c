/* Windows 上这两个文件整体不参与编译：它们建立在 POSIX 进程模型上
 * （posix_spawn / 进程组 / 信号）。Windows 的实现见 native/c/win_spawn.c，
 * 对外导出同一组 hm_spawn_* 符号，所以调用方不需要区分平台。 */
#if !defined(_WIN32)
/*
 * exec.c —— hm_spawn：posix_spawn + 新进程组 + 四条管道。
 *
 * 为什么是 posix_spawn 而不是 fork（ARCH §5.2 方案 B 已被否决）：
 *   Go runtime 是多线程的。fork 只复制调用线程，子进程继承了其它线程已持有
 *   的 malloc 锁 / Rust allocator 状态；fork 之后只要跑过一次分配就可能死锁
 *   或崩溃，而 cgo 里唯一安全的只有 async-signal-safe 调用。
 *   posix_spawn 在 macOS 走 SYS_posix_spawn、在 Linux 走
 *   clone(CLONE_VM|CLONE_VFORK)+exec，子进程**不跑我们的代码**。
 *
 * 进程组：posix_spawnattr_setpgroup(&attr, 0) 让子进程成为新进程组的组长
 * （pgid == 子进程 pid）。这是"杀掉卡死的工具"能成立的前提 ——
 * 只 kill(pid) 会漏掉工具派生的孙子进程，kill(-pgid) 不会。
 *
 * 四条管道：
 *   0/1/2 = stdin/stdout/stderr（业务数据）
 *   3     = shim 的 setup 错误通道（见 hm_internal.h）
 */

/* 与 cycle.c 同一个原因：glibc 在 -std=c11 下默认隐藏 POSIX 符号。
 * `posix_spawn_file_actions_addchdir_np` 是 glibc 扩展，还要 _GNU_SOURCE。
 * 两个宏都必须在**任何 include 之前**定义。 */
#if defined(__linux__)
#ifndef _GNU_SOURCE
#define _GNU_SOURCE 1
#endif
#ifndef _POSIX_C_SOURCE
#define _POSIX_C_SOURCE 200809L
#endif
#endif

#include "hm_internal.h"

#include <errno.h>
#include <fcntl.h>
#include <signal.h>
#include <spawn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/wait.h>
#include <unistd.h>

extern char **environ;

#define HM_SPAWN_MAX_ARGS 512
#define HM_STDIN_MAX (1u << 20)

/* 把 fd 设成非阻塞（父进程侧读端 / 写端都要）。 */
static int set_nonblock(int fd) {
  int flags = fcntl(fd, F_GETFL, 0);
  if (flags < 0) {
    return -1;
  }
  return fcntl(fd, F_SETFL, flags | O_NONBLOCK);
}

static void close_if_open(int *fd) {
  if (*fd >= 0) {
    (void)close(*fd);
    *fd = -1;
  }
}

/* 读受 abi_size 约束的字段（R8：按 min(自身, 传入) 读）。 */
static int req_has(const hm_spawn_req *req, size_t off, size_t size) {
  return req->abi_size == 0 || (off + size) <= (size_t)req->abi_size;
}

hm_status hm_spawn(const hm_spawn_req *req, hm_handle *out_proc) {
  int in_pipe[2] = {-1, -1};
  int out_pipe[2] = {-1, -1};
  int err_pipe[2] = {-1, -1};
  int setup_pipe[2] = {-1, -1};
  posix_spawn_file_actions_t fa;
  posix_spawnattr_t attr;
  int fa_ready = 0;
  int attr_ready = 0;
  char *argbuf[8];
  char *child_argv[HM_SPAWN_MAX_ARGS + 16];
  int argc = 0;
  int nargbuf = 0;
  pid_t pid = -1;
  int rc;
  int slot = -1;
  hm_proc *p = NULL;
  hm_status status = HM_OK;
  const char *shim_path;
  int i;

  if (req == NULL || out_proc == NULL) {
    return HM_E_INVALID_ARG;
  }
  *out_proc = HM_HANDLE_INVALID;

  /* --- 校验 --- */
  if (!req_has(req, offsetof(hm_spawn_req, exe), sizeof(const char *)) ||
      req->exe == NULL || req->exe[0] == '\0') {
    return HM_E_INVALID_ARG;
  }

  shim_path = req_has(req, offsetof(hm_spawn_req, shim), sizeof(const char *))
                  ? req->shim
                  : NULL;
  if (shim_path == NULL || shim_path[0] == '\0') {
    /*
     * 没有显式 shim 路径时，从环境变量退到 "同目录 hermes-shim"。
     * 生产路径由 Go 侧显式传入（T08 要求错误信息含期望路径）。
     */
    shim_path = getenv("HERMES_SHIM");
  }
  if (shim_path == NULL || shim_path[0] == '\0') {
    return HM_E_SPAWN;
  }
  if (access(shim_path, X_OK) != 0) {
    return HM_E_SPAWN;
  }

  /* --- 管道 --- */
  if (pipe(in_pipe) != 0 || pipe(out_pipe) != 0 || pipe(err_pipe) != 0 ||
      pipe(setup_pipe) != 0) {
    status = HM_E_NOMEM;
    goto fail;
  }

  /* --- argv：[shim] [flags...] -- <exe> <argv[0]> <argv[1]> ... --- */
  child_argv[argc++] = (char *)shim_path;

#define HM_PUSH_FLAG(fmt, val)                                              \
  do {                                                                      \
    if (nargbuf >= (int)(sizeof(argbuf) / sizeof(argbuf[0]))) {             \
      status = HM_E_TOO_LARGE;                                              \
      goto fail;                                                            \
    }                                                                       \
    argbuf[nargbuf] = (char *)malloc(64);                                   \
    if (argbuf[nargbuf] == NULL) {                                          \
      status = HM_E_NOMEM;                                                  \
      goto fail;                                                            \
    }                                                                       \
    (void)snprintf(argbuf[nargbuf], 64, fmt, (unsigned long long)(val));    \
    child_argv[argc++] = argbuf[nargbuf];                                   \
    nargbuf++;                                                              \
  } while (0)

  if (req_has(req, offsetof(hm_spawn_req, rl_as_bytes), sizeof(uint64_t)) &&
      req->rl_as_bytes != 0) {
    HM_PUSH_FLAG("--as=%llu", req->rl_as_bytes);
  }
  if (req_has(req, offsetof(hm_spawn_req, rl_cpu_sec), sizeof(uint64_t)) &&
      req->rl_cpu_sec != 0) {
    HM_PUSH_FLAG("--cpu=%llu", req->rl_cpu_sec);
  }
  if (req_has(req, offsetof(hm_spawn_req, rl_nofile), sizeof(uint64_t)) &&
      req->rl_nofile != 0) {
    HM_PUSH_FLAG("--nofile=%llu", req->rl_nofile);
  }
  if (req_has(req, offsetof(hm_spawn_req, rl_nproc), sizeof(uint64_t)) &&
      req->rl_nproc != 0) {
    HM_PUSH_FLAG("--nproc=%llu", req->rl_nproc);
  }
  if (req_has(req, offsetof(hm_spawn_req, rl_fsize), sizeof(uint64_t)) &&
      req->rl_fsize != 0) {
    HM_PUSH_FLAG("--fsize=%llu", req->rl_fsize);
  }
  if (req_has(req, offsetof(hm_spawn_req, sandbox_flags), sizeof(uint32_t)) &&
      req->sandbox_flags != 0) {
    HM_PUSH_FLAG("--sandbox=%llu", (unsigned long long)req->sandbox_flags);
  }

  child_argv[argc++] = (char *)"--";
  child_argv[argc++] = (char *)req->exe;

  {
    int have_argv0 = 0;
    for (i = 0; req->argv != NULL && req->argv[i] != NULL; i++) {
      if (argc >= HM_SPAWN_MAX_ARGS) {
        status = HM_E_TOO_LARGE;
        goto fail;
      }
      child_argv[argc++] = (char *)req->argv[i];
      have_argv0 = 1;
    }
    if (!have_argv0) {
      child_argv[argc++] = (char *)req->exe;
    }
  }
  child_argv[argc] = NULL;

  /* --- file_actions --- */
  if ((rc = posix_spawn_file_actions_init(&fa)) != 0) {
    status = hermes_errno_to_spawn_status(rc);
    goto fail;
  }
  fa_ready = 1;

#define HM_FA_GOTO(expr)          \
  do {                            \
    if ((rc = (expr)) != 0) {     \
      status = hermes_errno_to_spawn_status(rc); \
      goto fail;                  \
    }                             \
  } while (0)

  HM_FA_GOTO(posix_spawn_file_actions_adddup2(&fa, in_pipe[0], 0));
  HM_FA_GOTO(posix_spawn_file_actions_adddup2(&fa, out_pipe[1], 1));
  HM_FA_GOTO(posix_spawn_file_actions_adddup2(&fa, err_pipe[1], 2));
  HM_FA_GOTO(posix_spawn_file_actions_adddup2(&fa, setup_pipe[1], HM_SHIM_ERRFD));

  /*
   * 关掉管道在子进程里的原始 fd（dup2 之后它们已经多余）。
   * 跳过 0..3：如果某个管道 fd 恰好是 3，addclose(3) 会把刚 dup 好的
   * 错误通道关掉。
   */
  {
    int fds[8];
    int nfds = 0;
    fds[nfds++] = in_pipe[0];
    fds[nfds++] = in_pipe[1];
    fds[nfds++] = out_pipe[0];
    fds[nfds++] = out_pipe[1];
    fds[nfds++] = err_pipe[0];
    fds[nfds++] = err_pipe[1];
    fds[nfds++] = setup_pipe[0];
    fds[nfds++] = setup_pipe[1];
    for (i = 0; i < nfds; i++) {
      if (fds[i] > HM_SHIM_ERRFD) {
        HM_FA_GOTO(posix_spawn_file_actions_addclose(&fa, fds[i]));
      }
    }
  }

  if (req_has(req, offsetof(hm_spawn_req, cwd), sizeof(const char *)) &&
      req->cwd != NULL && req->cwd[0] != '\0') {
#if defined(__APPLE__)
    /*
     * macOS 26 起 posix_spawn_file_actions_addchdir(3) 取代了 _np 变体，
     * 而 _np 变体在老 SDK 上仍是唯一可用入口。按 SDK 版本二选一，而不是
     * 全局压制告警 —— 后者会把将来真正的废弃调用一起吞掉。
     */
#if defined(__MAC_OS_X_VERSION_MAX_ALLOWED) && \
    __MAC_OS_X_VERSION_MAX_ALLOWED >= 260000
    HM_FA_GOTO(posix_spawn_file_actions_addchdir(&fa, req->cwd));
#else
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"
    HM_FA_GOTO(posix_spawn_file_actions_addchdir_np(&fa, req->cwd));
#pragma clang diagnostic pop
#endif
#else
    /* Linux 上同名函数从 glibc 2.29 起可用 */
    HM_FA_GOTO(posix_spawn_file_actions_addchdir_np(&fa, req->cwd));
#endif
  }

  /* --- attr：新进程组 + 重置信号处置 --- */
  if ((rc = posix_spawnattr_init(&attr)) != 0) {
    status = hermes_errno_to_spawn_status(rc);
    goto fail;
  }
  attr_ready = 1;
  {
    short flags = POSIX_SPAWN_SETPGROUP | POSIX_SPAWN_SETSIGDEF |
                  POSIX_SPAWN_SETSIGMASK;
#ifdef POSIX_SPAWN_CLOEXEC_DEFAULT
    /*
     * Darwin 扩展：子进程除 file_actions 里 dup2 过的 fd 外，其余全部关闭。
     * 这比 shim 里的 closefrom(4) 更彻底 —— closefrom 有 TOCTOU 窗口
     * （spawn 到 shim 运行之间新开的 fd 会漏进来）。
     */
    flags = (short)(flags | POSIX_SPAWN_CLOEXEC_DEFAULT);
#endif
    HM_FA_GOTO((int)posix_spawnattr_setflags(&attr, flags));

    /* pgid 0 = 让子进程自己成为新进程组的组长 */
    HM_FA_GOTO((int)posix_spawnattr_setpgroup(&attr, 0));

    {
      sigset_t def;
      sigset_t empty;
      sigemptyset(&def);
      sigaddset(&def, SIGTERM);
      sigaddset(&def, SIGINT);
      sigaddset(&def, SIGQUIT);
      sigaddset(&def, SIGPIPE);
      sigaddset(&def, SIGHUP);
      HM_FA_GOTO((int)posix_spawnattr_setsigdefault(&attr, &def));

      sigemptyset(&empty);
      HM_FA_GOTO((int)posix_spawnattr_setsigmask(&attr, &empty));
    }
  }

  /* --- 槽位 --- */
  hermes_proc_table_lock();
  slot = hermes_proc_alloc_locked();
  hermes_proc_table_unlock();
  if (slot < 0) {
    status = HM_E_NOMEM;
    goto fail;
  }

  /* --- spawn --- */
  {
    const char *const *envp = req->envp;
    static const char *const empty_env[1] = {NULL};
    if (envp == NULL) {
      /* 契约：NULL = 空环境，不是继承。 */
      envp = empty_env;
    }
    rc = posix_spawn(&pid, shim_path, &fa, &attr, child_argv,
                     (char *const *)envp);
    if (rc != 0) {
      status = hermes_errno_to_spawn_status(rc);
      goto fail;
    }
  }

  /* --- 父进程侧收尾 --- */
  close_if_open(&in_pipe[0]);
  close_if_open(&out_pipe[1]);
  close_if_open(&err_pipe[1]);
  close_if_open(&setup_pipe[1]);

  if (set_nonblock(out_pipe[0]) != 0 || set_nonblock(err_pipe[0]) != 0 ||
      set_nonblock(setup_pipe[0]) != 0 || set_nonblock(in_pipe[1]) != 0) {
    status = HM_E_IO;
    goto fail;
  }

  /* --- 填槽位 --- */
  hermes_proc_table_lock();
  p = hermes_proc_storage(slot);
  p->pid = pid;
  p->pgid = pid; /* PGROUP(0) 保证 pgid == pid */
  p->in_fd = in_pipe[1];
  p->out_fd = out_pipe[0];
  p->err_fd = err_pipe[0];
  p->setup_fd = setup_pipe[0];
  in_pipe[1] = -1;
  out_pipe[0] = -1;
  err_pipe[0] = -1;
  setup_pipe[0] = -1;

  p->deadline_cycles =
      req_has(req, offsetof(hm_spawn_req, deadline_cycles), sizeof(uint64_t))
          ? req->deadline_cycles
          : 0;
  {
    uint64_t backstop_ms =
        req_has(req, offsetof(hm_spawn_req, wall_backstop_ms), sizeof(uint64_t))
            ? req->wall_backstop_ms
            : 0;
    p->wall_backstop_ns = backstop_ms * 1000000ull;
  }
  p->kill_grace_ms =
      req_has(req, offsetof(hm_spawn_req, kill_grace_ms), sizeof(uint32_t)) &&
              req->kill_grace_ms != 0
          ? req->kill_grace_ms
          : HM_DEFAULT_KILL_GRACE_MS;

  {
    size_t out_cap =
        req_has(req, offsetof(hm_spawn_req, stdout_cap), sizeof(size_t)) &&
                req->stdout_cap != 0
            ? req->stdout_cap
            : HM_DEFAULT_STDOUT_CAP;
    size_t err_cap =
        req_has(req, offsetof(hm_spawn_req, stderr_cap), sizeof(size_t)) &&
                req->stderr_cap != 0
            ? req->stderr_cap
            : HM_DEFAULT_STDERR_CAP;
    if (out_cap > HM_VIEW_MAX) {
      out_cap = HM_VIEW_MAX;
    }
    if (err_cap > HM_VIEW_MAX) {
      err_cap = HM_VIEW_MAX;
    }
    p->out_cap = out_cap;
    p->err_cap = err_cap;
  }

  /* stdin：复制进槽位，等 wait 循环非阻塞写完 */
  if (req_has(req, offsetof(hm_spawn_req, stdin_bytes), sizeof(const uint8_t *)) &&
      req->stdin_bytes != NULL && req->stdin_len > 0) {
    size_t n = req->stdin_len;
    if (n > HM_STDIN_MAX) {
      hermes_proc_table_unlock();
      status = HM_E_TOO_LARGE;
      goto fail;
    }
    p->in_buf = (uint8_t *)malloc(n);
    if (p->in_buf == NULL) {
      hermes_proc_table_unlock();
      status = HM_E_NOMEM;
      goto fail;
    }
    memcpy(p->in_buf, req->stdin_bytes, n);
    p->in_len = n;
    p->in_off = 0;
  } else {
    close_if_open(&p->in_fd);
  }

  {
    hm_handle h = hermes_handle_pack((uint32_t)slot, p->gen);
    *out_proc = h;
  }
  hermes_proc_table_unlock();
  p = NULL;

  if (fa_ready) {
    posix_spawn_file_actions_destroy(&fa);
  }
  if (attr_ready) {
    posix_spawnattr_destroy(&attr);
  }
  for (i = 0; i < nargbuf; i++) {
    free(argbuf[i]);
  }
  return HM_OK;

fail:
  if (pid > 0) {
    (void)kill(-pid, SIGKILL);
    (void)kill(pid, SIGKILL);
    (void)waitpid(pid, NULL, 0);
  }
  if (slot >= 0) {
    hermes_proc_table_lock();
    {
      hm_proc *q = hermes_proc_storage(slot);
      /* 关掉可能已经挂上的 fd 与缓冲 */
      if (q->in_fd > 0) {
        (void)close(q->in_fd);
      }
      if (q->out_fd > 0) {
        (void)close(q->out_fd);
      }
      if (q->err_fd > 0) {
        (void)close(q->err_fd);
      }
      if (q->setup_fd > 0) {
        (void)close(q->setup_fd);
      }
      free(q->in_buf);
      free(q->out_buf);
      free(q->err_buf);
      hermes_proc_release_locked(slot);
    }
    hermes_proc_table_unlock();
  }
  close_if_open(&in_pipe[0]);
  close_if_open(&in_pipe[1]);
  close_if_open(&out_pipe[0]);
  close_if_open(&out_pipe[1]);
  close_if_open(&err_pipe[0]);
  close_if_open(&err_pipe[1]);
  close_if_open(&setup_pipe[0]);
  close_if_open(&setup_pipe[1]);
  if (fa_ready) {
    posix_spawn_file_actions_destroy(&fa);
  }
  if (attr_ready) {
    posix_spawnattr_destroy(&attr);
  }
  for (i = 0; i < nargbuf; i++) {
    free(argbuf[i]);
  }
  return status;
}

#endif /* !_WIN32 */
