/* win_spawn.c —— Windows 上的进程派发（hm_spawn_* 的 Win32 实现）。
 *
 * 为什么单独一个文件而不是往 exec.c 里塞 #ifdef：exec.c 的语义是
 * "posix_spawn + 进程组 + 信号"，Windows 上这三样都不存在。硬塞会让两份
 * 逻辑互相干扰；分文件之后，对外导出的符号集合完全一致，调用方不区分平台。
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
  if (req->abi_size < sizeof(hm_spawn_req) - 64) return HM_E_ABI_VERSION;
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

hm_status hm_spawn_cancel(hm_handle proc) {
  slot_lock();
  win_slot *s = slot_of(proc, NULL);
  if (!s) {
    slot_unlock();
    return HM_E_HANDLE_STALE;
  }
  s->cancelled = 1;
  if (s->job) TerminateJobObject(s->job, 1);
  else if (s->proc) TerminateProcess(s->proc, 1);
  slot_unlock();
  return HM_OK;
}

hm_status hm_spawn_wait(hm_handle proc, hm_spawn_res *out) {
  if (!out || out->abi_size < sizeof(hm_spawn_res) - 64) return HM_E_INVALID_ARG;
  slot_lock();
  win_slot *s = slot_of(proc, NULL);
  slot_unlock();
  if (!s) return HM_E_HANDLE_STALE;

  /* deadline：先按 wall 兜底等，超时就杀掉整棵树。 */
  DWORD timeout = INFINITE;
  int deadline_hit = 0;
  if (s->wall_backstop_ms > 0 && s->wall_backstop_ms < 0x7fffffff) {
    timeout = (DWORD)s->wall_backstop_ms;
  }
  DWORD wr = WaitForSingleObject(s->proc, timeout);
  if (wr == WAIT_TIMEOUT) {
    deadline_hit = 1;
    if (s->job) TerminateJobObject(s->job, 1);
    else TerminateProcess(s->proc, 1);
    WaitForSingleObject(s->proc, 5000);
  }

  DWORD code = 0;
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

  memset(out, 0, sizeof(*out));
  out->abi_size = sizeof(hm_spawn_res);
  out->exited = 1;
  out->exit_code = (int32_t)code;
  out->signaled = s->cancelled ? 1 : 0;
  out->escalated = s->cancelled ? 1 : 0;
  out->deadline_hit = deadline_hit;
  out->killed_pgid = (s->cancelled || deadline_hit) ? (int32_t)s->pid : 0;
  out->deadline_source = deadline_hit ? "wall" : "";
  out->setup_errno = 0;
  out->stdout_bytes = s->out_buf;
  out->stdout_len = s->out_len;
  out->stderr_bytes = s->err_buf;
  out->stderr_len = s->err_len;
  s->out_buf = NULL; s->out_len = s->out_cap = 0;
  s->err_buf = NULL; s->err_len = s->err_cap = 0;
  s->exited = 1;
  s->exit_code = code;
  return HM_OK;
}

void hm_spawn_res_free(hm_spawn_res *res) {
  if (!res) return;
  free(res->stdout_bytes);
  free(res->stderr_bytes);
  res->stdout_bytes = NULL;
  res->stderr_bytes = NULL;
  res->stdout_len = res->stderr_len = 0;
}

hm_status hm_spawn_free(hm_handle proc) {
  slot_lock();
  win_slot *s = slot_of(proc, NULL);
  if (!s) {
    slot_unlock();
    return HM_E_HANDLE_STALE;
  }
  if (s->out_thread) CloseHandle(s->out_thread);
  if (s->err_thread) CloseHandle(s->err_thread);
  close_pipe(&s->out_rd);
  close_pipe(&s->err_rd);
  if (s->proc) CloseHandle(s->proc);
  if (s->job) CloseHandle(s->job);
  free(s->out_buf);
  free(s->err_buf);
  s->out_buf = NULL; s->err_buf = NULL;
  s->out_len = s->err_len = 0;
  s->proc = NULL; s->job = NULL;
  s->used = 0;
  s->gen += 1;
  slot_unlock();
  return HM_OK;
}

#endif /* _WIN32 */
