/*
 * sandbox.c —— syscall 级沙箱的最小可信落点。
 *
 * 位置很重要：沙箱**只在 hermes-shim 里**生效，永远不影响 Go 引擎进程。
 * 这是"两个二进制"（D-005）换来的最大好处之一 —— 沙箱代码跑了、跑错了，
 * 死的是工具子进程，不是编排引擎。
 *
 * macOS：sandbox_init(3)（DEPRECATED 但仍是唯一可用的用户态入口）。
 *        profile 以 (allow default) 打底再逐项 deny，而不是 (deny default)
 *        加白名单。原因：Phase 1 没有能力穷举一个任意第三方工具需要哪些
 *        mach-lookup / sysctl / 文件路径，deny-default 会让所有工具直接
 *        起不来，于是使用者会把沙箱整体关掉 —— 那比"允许多、精确禁"
 *        更不安全。等到 Phase 3 有 capability 清单再切 deny-default。
 * Linux：PR_SET_NO_NEW_PRIVS + rlimit 兜底；seccomp 留 Phase 2。
 */

#include "hm_internal.h"

#include <errno.h>
#include <stdio.h>

#if defined(__APPLE__)
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"
#include <sandbox.h>
#pragma clang diagnostic pop
#endif

#if defined(__linux__)
#include <sys/prctl.h>
#endif

#define HM_SANDBOX_PROFILE_CAP 2048

hm_status hermes_sandbox_apply(uint32_t flags, uint32_t *out_ignored) {
  uint32_t ignored = 0;

  if (out_ignored != NULL) {
    *out_ignored = 0;
  }

  if (flags == 0) {
    return HM_OK;
  }

#if defined(_WIN32)
  /* Windows：沙箱由进程派发层的 Job Object 承担（杀整棵树 + 资源限制），
   * 这一层没有额外的 seccomp/sandbox 可施加。把标志按"未应用"如实回报 ——
   * 假装成功会让审计报告说谎。 */
  ignored |= flags;
  if (out_ignored != NULL) {
    *out_ignored = ignored;
  }
  return HM_OK;
#elif defined(__APPLE__)
  {
    char profile[HM_SANDBOX_PROFILE_CAP];
    size_t used = 0;
    char *errbuf = NULL;
    int rc;

    used += (size_t)snprintf(profile + used, sizeof(profile) - used,
                             "(version 1)(allow default)");
    if (used >= sizeof(profile)) {
      return HM_E_INVALID_ARG;
    }

    if ((flags & HM_SB_NO_NET) != 0) {
      used += (size_t)snprintf(profile + used, sizeof(profile) - used,
                               "(deny network*)");
      if (used >= sizeof(profile)) {
        return HM_E_INVALID_ARG;
      }
    }
    if ((flags & HM_SB_READONLY_TMP) != 0) {
      used += (size_t)snprintf(profile + used, sizeof(profile) - used,
                               "(deny file-write* (subpath \"/tmp\")"
                               "(subpath \"/private/tmp\"))");
      if (used >= sizeof(profile)) {
        return HM_E_INVALID_ARG;
      }
    }
    if ((flags & HM_SB_NO_NEW_PRIVS) != 0) {
      /*
       * macOS 上没有 PR_SET_NO_NEW_PRIVS 的对应物：setuid/setgid 位由
       * 文件系统与 code signing 决定，沙箱 profile 无法表达。
       * 明确记为 "ignored" 而不是假装成功 —— 假装成功会让审计报告说谎。
       */
      ignored |= HM_SB_NO_NEW_PRIVS;
    }

    /*
     * sandbox_init(3) 自 macOS 10.8 起被标 deprecated，但它仍是用户态唯一
     * 可用的入口（没有公开的替代 API）。这里就地压制告警，并把范围限制到
     * 两次调用本身 —— 不在文件级关闭 -Wdeprecated-declarations，否则将来
     * 真正的废弃调用也会被一起吞掉。
     */
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"
    rc = sandbox_init(profile, 0, &errbuf);
#pragma clang diagnostic pop
    if (rc != 0) {
      if (errbuf != NULL) {
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"
        sandbox_free_error(errbuf);
#pragma clang diagnostic pop
      }
      if (out_ignored != NULL) {
        *out_ignored = ignored;
      }
      return HM_E_SANDBOX_DENIED;
    }
    if (errbuf != NULL) {
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"
      sandbox_free_error(errbuf);
#pragma clang diagnostic pop
    }
    if (out_ignored != NULL) {
      *out_ignored = ignored;
    }
    return HM_OK;
  }
#elif defined(__linux__)
  {
    if ((flags & HM_SB_NO_NEW_PRIVS) != 0) {
      if (prctl(PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != 0) {
        return HM_E_SANDBOX_DENIED;
      }
    } else {
      ignored |= HM_SB_NO_NEW_PRIVS;
    }
    /* 网络与 /tmp 只读在 Linux 上需要 seccomp / mount namespace，
     * 属 Phase 2；这里诚实记录为 ignored，不静默假装。 */
    ignored |= (flags & (HM_SB_NO_NET | HM_SB_READONLY_TMP));
    if (out_ignored != NULL) {
      *out_ignored = ignored;
    }
    return HM_OK;
  }
#else
  (void)flags;
  if (out_ignored != NULL) {
    *out_ignored = flags;
  }
  return HM_OK;
#endif
}
