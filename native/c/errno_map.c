/*
 * errno_map.c —— errno → hm_status，以及沙箱拒绝的专有映射。
 *
 * 为什么要单独一个文件而不是每个调用点自己 if：
 * 归因闭集（PRD §4.1.2 / D-008）要求"任何失败精确落到 6 值之一，没有 other"。
 * 如果 errno → 状态码的映射散落在 spawn / shim / wait 三处，就会出现
 * "同一个 ENOENT 在一处是 HM_E_SPAWN、在另一处是 HM_E_IO"的漂移，
 * 上层再把它映射成不同的归因 —— 这正是 D-008 要消灭的东西。
 * 单点映射 + 穷尽 switch，让"漏一个 errno"变成可审计的清单问题。
 */

#include "hm_internal.h"

#include <errno.h>

/*
 * 启动失败类 errno：这些意味着"我们没能把工具跑起来"，上层归因 transport，
 * 但要能和"工具自己跑了并失败"区分开。
 */
hm_status hermes_errno_to_spawn_status(int err) {
  switch (err) {
    case 0:
      return HM_OK;
    case ENOENT:   /* 解释器 / 可执行文件不存在 */
    case ENOTDIR:  /* 路径中间某段不是目录 */
    case EACCES:   /* 权限不足 */
    case EPERM:
    case ENOEXEC:  /* 文件存在但不是可执行格式 */
    case EISDIR:
    case ELOOP:
    case ENAMETOOLONG:
    case ETXTBSY:
      return HM_E_SPAWN;
    case EAGAIN:   /* 进程/内存资源暂时不足 */
    case ENOMEM:
    case EMFILE:
    case ENFILE:
      return HM_E_NOMEM;
    case EINVAL:
      return HM_E_INVALID_ARG;
    default:
      return HM_E_IO;
  }
}

/* 通用 errno → 状态码（shim 上报的 setup 失败走这条）。 */
hm_status hermes_errno_to_status(int err) {
  switch (err) {
    case 0:
      return HM_OK;
    case EINVAL:
      /*
       * macOS 对 RLIMIT_AS / RLIMIT_DATA / RLIMIT_RSS 的 setrlimit 返回 EINVAL
       * （内核不实现这些资源）。这不是"参数错了"，而是"平台不支持"。
       * 区分它很重要：前者应该 fail loud，后者应该降级并在 stderr 说明。
       * shim 已经把它单独识别为"unsupported"，走到这里只可能是别的 EINVAL。
       */
      return HM_E_RLIMIT_SETUP;
    case EPERM:
      return HM_E_SANDBOX_DENIED;
    case ENOENT:
    case EACCES:
      return HM_E_SPAWN;
    case ENOMEM:
      return HM_E_NOMEM;
    default:
      return HM_E_IO;
  }
}
