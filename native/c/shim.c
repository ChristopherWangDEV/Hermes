/*
 * shim.c —— hermes-shim 的 main。
 *
 * 这个程序存在的唯一理由（ARCH §5.2 / D-005）：
 *   macOS 的 posix_spawn 无法直接给子进程设 rlimit（没有
 *   POSIX_SPAWN_SETRLIMIT），所以必须先起一个 shim，让 shim 给自己
 *   setrlimit + 上沙箱，然后再 execve 目标工具。
 *
 * 为什么它值得多占一个二进制：
 *   1. rlimit 与沙箱只影响 shim → 工具这条进程链，**永不改 Go 引擎自身的
 *      rlimit**（rlimit 是进程级资源，在引擎进程里设会毒化整个产品）。
 *   2. 沙箱有一个明确的、可审的、能单独单测的落点（--self-test）。
 *   3. 它提供 setup 失败的**独立上报通道**：fd 3。
 *          · setup 全成功 → shim 给 fd 3 打 FD_CLOEXEC，execve 成功后内核
 *            自动关闭它 → 父进程读到 **EOF**。
 *          · setup/execve 失败 → 向 fd 3 写 4 字节小端 errno 后 _exit。
 *      没有这条通道，就分不清"工具自己 exit(1)"和"我们没把 rlimit 设上"，
 *      归因会把基础设施失败误报成工具失败。
 *
 * 用法：
 *   hermes-shim --self-test
 *   hermes-shim [--as=N] [--cpu=N] [--nofile=N] [--nproc=N] [--fsize=N]
 *               [--sandbox=N] [--alloc-test=N]
 *   hermes-shim [flags] -- <exe> [argv0] [arg1] ...
 *       → execve(exe, {argv0, arg1, ...}, environ)
 *       argv0 缺省时用 exe。
 */

#include "hm_internal.h"

#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/resource.h>
#include <sys/stat.h> /* umask */
#include <unistd.h>

extern char **environ;

/* 被跳过的 rlimit（平台不支持），用于 --alloc-test 的判定 */
#define HM_LIM_AS 0x1u
#define HM_LIM_DATA 0x2u
static uint32_t g_limits_unsupported = 0;

static void shim_warn(const char *msg) {
  fprintf(stderr, "hermes-shim: warn: %s\n", msg);
  fflush(stderr);
}

/*
 * 把 errno 写到 fd 3 然后立刻 _exit。
 * 用 write(2) 而不是 stdio：_exit 不刷 stdio，且此刻不允许再分配内存。
 */
static void report_and_exit(int err, int code) {
  uint8_t payload[HM_SHIM_ERRFD_PAYLOAD];
  payload[0] = (uint8_t)(err & 0xff);
  payload[1] = (uint8_t)((err >> 8) & 0xff);
  payload[2] = (uint8_t)((err >> 16) & 0xff);
  payload[3] = (uint8_t)((err >> 24) & 0xff);

  /* 尽力写满 4 字节；失败也无法再做什么，只能退出。 */
  {
    ssize_t off = 0;
    while (off < (ssize_t)sizeof(payload)) {
      ssize_t w = write(HM_SHIM_ERRFD, payload + off,
                        (size_t)((ssize_t)sizeof(payload) - off));
      if (w <= 0) {
        break;
      }
      off += w;
    }
  }
  _exit(code);
}

/*
 * 设置一项 rlimit。
 *
 * 平台不支持（Darwin 对 RLIMIT_AS / RLIMIT_DATA / RLIMIT_RSS 返回 EINVAL）
 * 时**降级**而不失败：把该项记进 g_limits_unsupported 并往 stderr 写一行
 * 明确的 warn。这条 warn 会被父进程当作 stderr 捕获，因此"限制没生效"
 * 是可见的、可断言的，而不是静默的。
 */
static void apply_limit(const char *name, int resource, unsigned long long value,
                        uint32_t bit) {
  struct rlimit rl;
  struct rlimit rb;

  if (value == 0) {
    return; /* 0 = 不设置 */
  }
  if (getrlimit(resource, &rl) != 0) {
    report_and_exit(errno, HM_SHIM_EXIT_SETUP);
  }
  rl.rlim_cur = (rlim_t)value;
  if (rl.rlim_max != RLIM_INFINITY && rl.rlim_max < (rlim_t)value) {
    rl.rlim_max = (rlim_t)value;
  }
  if (setrlimit(resource, &rl) != 0) {
    int e = errno;
    if (e == EINVAL) {
      /* Darwin 用 EINVAL 表示"该资源不被实现"。 */
      char msg[160];
      g_limits_unsupported |= bit;
      snprintf(msg, sizeof(msg),
               "rlimit %s unsupported on this platform (errno=EINVAL); "
               "the limit was NOT applied",
               name);
      shim_warn(msg);
      return;
    }
    report_and_exit(e, HM_SHIM_EXIT_SETUP);
  }
  if (getrlimit(resource, &rb) != 0) {
    report_and_exit(errno, HM_SHIM_EXIT_SETUP);
  }
  if (rb.rlim_cur != (rlim_t)value) {
    report_and_exit(EINVAL, HM_SHIM_EXIT_SETUP);
  }
}

/* 关闭 4 号以上的所有 fd（fd 3 是错误通道，必须留着）。 */
static void close_from_4(void) {
  int max_fd = (int)sysconf(_SC_OPEN_MAX);
  int fd;

  if (max_fd < 0 || max_fd > 4096) {
    max_fd = 4096;
  }
  for (fd = 4; fd < max_fd; fd++) {
    (void)close(fd);
  }
}

static int parse_u64(const char *s, unsigned long long *out) {
  char *end = NULL;
  unsigned long long v;

  if (s == NULL || *s == '\0') {
    return 0;
  }
  errno = 0;
  v = strtoull(s, &end, 10);
  if (errno != 0 || end == NULL || *end != '\0') {
    return 0;
  }
  *out = v;
  return 1;
}

/* 匹配 "--key=value"；命中返回 value 指针，否则 NULL。 */
static const char *match_opt(const char *arg, const char *key) {
  size_t n = strlen(key);
  if (strncmp(arg, key, n) != 0) {
    return NULL;
  }
  if (arg[n] != '=') {
    return NULL;
  }
  return arg + n + 1;
}

/* ------------------------------------------------------------------ *
 * --self-test
 *
 * 这是"shim 可以被单独信任"的证据：不 spawn 任何东西，只验证它自己的
 * 那几个基础件（errno 映射、rlimit 往返、fd 通道、沙箱参数拼接）。
 * ------------------------------------------------------------------ */
static int self_test(void) {
  struct rlimit rl;
  struct rlimit orig;
  int pipefd[2];
  uint8_t buf[HM_SHIM_ERRFD_PAYLOAD];
  ssize_t n;
  int failures = 0;

  /* 1. errno 映射 */
  if (hermes_errno_to_spawn_status(ENOENT) != HM_E_SPAWN) {
    fprintf(stderr, "self-test: ENOENT should map to HM_E_SPAWN\n");
    failures++;
  }
  if (hermes_errno_to_spawn_status(0) != HM_OK) {
    fprintf(stderr, "self-test: errno 0 should map to HM_OK\n");
    failures++;
  }
  if (hermes_errno_to_status(0) != HM_OK) {
    fprintf(stderr, "self-test: errno 0 should map to HM_OK (generic)\n");
    failures++;
  }

  /* 2. rlimit 往返：把 NOFILE 降到 128 再读回，然后恢复 */
  if (getrlimit(RLIMIT_NOFILE, &orig) != 0) {
    fprintf(stderr, "self-test: getrlimit(NOFILE) failed\n");
    failures++;
  } else {
    rl = orig;
    rl.rlim_cur = 128;
    if (rl.rlim_max != RLIM_INFINITY && rl.rlim_max < 128) {
      rl.rlim_max = 128;
    }
    if (setrlimit(RLIMIT_NOFILE, &rl) != 0) {
      fprintf(stderr, "self-test: setrlimit(NOFILE,128) failed errno=%d\n", errno);
      failures++;
    } else {
      struct rlimit rb;
      if (getrlimit(RLIMIT_NOFILE, &rb) != 0 || rb.rlim_cur != 128) {
        fprintf(stderr, "self-test: NOFILE readback mismatch\n");
        failures++;
      }
      if (setrlimit(RLIMIT_NOFILE, &orig) != 0) {
        fprintf(stderr, "self-test: failed to restore NOFILE\n");
        failures++;
      }
    }
  }

  /* 3. fd 3 错误通道的编码：写 4 字节小端、读回来必须一致 */
  if (pipe(pipefd) != 0) {
    fprintf(stderr, "self-test: pipe() failed\n");
    failures++;
  } else {
    uint8_t payload[HM_SHIM_ERRFD_PAYLOAD] = {0x0d, 0x00, 0x00, 0x00}; /* EACCES */
    if (write(pipefd[1], payload, sizeof(payload)) !=
        (ssize_t)sizeof(payload)) {
      fprintf(stderr, "self-test: fd-channel write failed\n");
      failures++;
    }
    n = read(pipefd[0], buf, sizeof(buf));
    if (n != (ssize_t)sizeof(buf) || buf[0] != 0x0d || buf[1] != 0 ||
        buf[2] != 0 || buf[3] != 0) {
      fprintf(stderr, "self-test: fd-channel roundtrip mismatch (n=%ld)\n",
              (long)n);
      failures++;
    }
    close(pipefd[0]);
    close(pipefd[1]);
  }

  /* 4. 沙箱：flags=0 必须是 no-op 且不算 ignored */
  {
    uint32_t ignored = 0xffffffffu;
    if (hermes_sandbox_apply(0, &ignored) != HM_OK || ignored != 0) {
      fprintf(stderr, "self-test: sandbox_apply(0) not a clean no-op\n");
      failures++;
    }
  }

  /* 5. 布局指纹非零（ABI 头被改过却没提升 minor 时，指纹会变，doctor 会看出来） */
  if (hermes_abi_layout_fingerprint() == 0) {
    fprintf(stderr, "self-test: layout fingerprint is zero\n");
    failures++;
  }

  printf("hermes-shim self-test: abi=%u.%u fingerprint=%08x %s\n", HM_ABI_MAJOR,
         HM_ABI_MINOR, hermes_abi_layout_fingerprint(),
         failures == 0 ? "OK" : "FAIL");
  fflush(stdout);
  return failures == 0 ? HM_SHIM_EXIT_OK : HM_SHIM_EXIT_SELFTEST_FAIL;
}

/* ------------------------------------------------------------------ *
 * --alloc-test：证明 rlimit 真的作用到了这条进程链上
 * ------------------------------------------------------------------ */
static int alloc_test(unsigned long long bytes) {
  void *p;

  if (bytes == 0) {
    bytes = 256ull << 20;
  }
  p = malloc((size_t)bytes);
  if (p == NULL) {
    printf("hermes-shim alloc-test: malloc(%llu) blocked (limit enforced)\n",
           bytes);
    fflush(stdout);
    return HM_SHIM_EXIT_ALLOC_BLOCKED;
  }
  free(p);

  if ((g_limits_unsupported & HM_LIM_AS) != 0 ||
      (g_limits_unsupported & HM_LIM_DATA) != 0) {
    /*
     * 平台根本不实现地址空间限制（macOS 就是这种情况）。
     * 大声报告"不支持"，绝不假装 limit 生效了。
     */
    printf("hermes-shim alloc-test: malloc(%llu) succeeded; "
           "address-space limit is UNSUPPORTED on this platform\n",
           bytes);
    fflush(stdout);
    return HM_SHIM_EXIT_LIMIT_UNSUPPORTED;
  }
  printf("hermes-shim alloc-test: malloc(%llu) succeeded; "
         "limit did NOT take effect\n",
         bytes);
  fflush(stdout);
  return HM_SHIM_EXIT_ALLOC_OK;
}

int main(int argc, char **argv) {
  unsigned long long rl_as = 0;
  unsigned long long rl_cpu = 0;
  unsigned long long rl_nofile = 0;
  unsigned long long rl_nproc = 0;
  unsigned long long rl_fsize = 0;
  unsigned long long sandbox_flags = 0;
  unsigned long long alloc_bytes = 0;
  int have_alloc_test = 0;
  int argi = 1;
  int dashdash = -1;
  uint32_t ignored_flags = 0;

  for (; argi < argc; argi++) {
    const char *a = argv[argi];
    const char *v;

    if (strcmp(a, "--") == 0) {
      dashdash = argi;
      break;
    }
    if (strcmp(a, "--self-test") == 0) {
      return self_test();
    }
    if (strcmp(a, "--version") == 0) {
      printf("hermes-shim abi=%u.%u fingerprint=%08x\n", HM_ABI_MAJOR,
             HM_ABI_MINOR, hermes_abi_layout_fingerprint());
      return HM_SHIM_EXIT_OK;
    }
    if ((v = match_opt(a, "--as")) != NULL) {
      if (!parse_u64(v, &rl_as)) {
        return HM_SHIM_EXIT_SETUP;
      }
      continue;
    }
    if ((v = match_opt(a, "--cpu")) != NULL) {
      if (!parse_u64(v, &rl_cpu)) {
        return HM_SHIM_EXIT_SETUP;
      }
      continue;
    }
    if ((v = match_opt(a, "--nofile")) != NULL) {
      if (!parse_u64(v, &rl_nofile)) {
        return HM_SHIM_EXIT_SETUP;
      }
      continue;
    }
    if ((v = match_opt(a, "--nproc")) != NULL) {
      if (!parse_u64(v, &rl_nproc)) {
        return HM_SHIM_EXIT_SETUP;
      }
      continue;
    }
    if ((v = match_opt(a, "--fsize")) != NULL) {
      if (!parse_u64(v, &rl_fsize)) {
        return HM_SHIM_EXIT_SETUP;
      }
      continue;
    }
    if ((v = match_opt(a, "--sandbox")) != NULL) {
      if (!parse_u64(v, &sandbox_flags)) {
        return HM_SHIM_EXIT_SETUP;
      }
      continue;
    }
    if ((v = match_opt(a, "--alloc-test")) != NULL) {
      if (!parse_u64(v, &alloc_bytes)) {
        return HM_SHIM_EXIT_SETUP;
      }
      have_alloc_test = 1;
      continue;
    }
    fprintf(stderr, "hermes-shim: unknown option: %s\n", a);
    return HM_SHIM_EXIT_SETUP;
  }

  /* ---- setup ---- */
  umask(077);

  apply_limit("as", RLIMIT_AS, rl_as, HM_LIM_AS);
  apply_limit("cpu", RLIMIT_CPU, rl_cpu, 0);
  apply_limit("fsize", RLIMIT_FSIZE, rl_fsize, 0);
  apply_limit("nofile", RLIMIT_NOFILE, rl_nofile, 0);
  apply_limit("nproc", RLIMIT_NPROC, rl_nproc, 0);
#if defined(__linux__)
  apply_limit("data", RLIMIT_DATA, rl_as, HM_LIM_DATA);
#endif

  if (sandbox_flags != 0) {
    hm_status st = hermes_sandbox_apply((uint32_t)sandbox_flags, &ignored_flags);
    if (st != HM_OK) {
      report_and_exit(EPERM, HM_SHIM_EXIT_SETUP);
    }
    if (ignored_flags != 0) {
      shim_warn("some sandbox flags are not supported on this platform");
    }
  }

  if (have_alloc_test) {
    /*
     * 分配测试必须在 execve 之前做：它测的就是"这条进程链的限制"。
     * 注意 alloc-test 不出现在生产路径上，它是一个只读的可观测探针。
     */
    return alloc_test(alloc_bytes);
  }

  if (dashdash < 0 || dashdash + 1 >= argc) {
    fprintf(stderr,
            "usage: hermes-shim [flags] -- <exe> [argv0] [args...]\n"
            "       hermes-shim --self-test\n");
    return HM_SHIM_EXIT_SETUP;
  }

  {
    int exe_index = dashdash + 1;
    char **child_argv;

    if (argc - exe_index - 1 <= 0) {
      /* 没给 argv0：用 exe 自己 */
      child_argv = &argv[exe_index];
    } else {
      child_argv = &argv[exe_index + 1];
    }

    /*
     * fd 3 打 CLOEXEC：execve **成功**时内核自动关闭它 → 父进程读到 EOF，
     * 据此判定 "setup 全成功、工具已经开始跑"。
     * execve 失败时 fd 3 仍然开着，我们照样能把 errno 写出去。
     */
    (void)fcntl(HM_SHIM_ERRFD, F_SETFD, FD_CLOEXEC);

    close_from_4();

    execve(argv[exe_index], child_argv, environ);

    /* 走到这里 = execve 失败 */
    report_and_exit(errno, HM_SHIM_EXIT_EXEC);
  }

  return HM_SHIM_EXIT_EXEC;
}
