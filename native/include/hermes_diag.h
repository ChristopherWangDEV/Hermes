#ifndef HERMES_DIAG_H
#define HERMES_DIAG_H

/* hermes_diag.h —— 双语诊断渲染的 C ABI（阶段 2 新增的 additive entry）。
 *
 * 分工铁律：C++ 按 lang 参数从自己的常量表渲染完整文案，**绝不**读环境变量、
 * 不碰终端；宿主语言选择（HERMES_LANG）与着色（NO_COLOR/TERM/isatty）仍在 Go。
 * 所有输出写调用方 buffer，前端零 IO。
 */

#include <stddef.h>
#include "hermes_abi.h"

#ifdef __cplusplus
extern "C" {
#endif

typedef struct hm_diag_arg {
  const char *name;
  const char *value;
} hm_diag_arg;

typedef struct hm_diag_note {
  const char *module;
  const char *path;
  int line;
  int col;
  const char *message;
} hm_diag_note;

typedef struct hm_diag {
  const char *code;       /* 诊断码，如 E_IR_DANGLING_REF */
  const char *severity;   /* "error" / "warning" / "note" */
  const char *module;     /* 文件名 */
  const char *path;       /* 可选的 JSON 路径 */
  int line;
  int col;
  int end_line;
  int end_col;
  const char *snippet;    /* 可选的源码片段，已由宿主生成 */
  const hm_diag_arg *args;
  size_t n_args;
  const hm_diag_note *notes;
  size_t n_notes;
} hm_diag;

/* 渲染一条诊断的完整人读文本（error[CODE]: … / why / help / note）。
 * lang 只接受 "en" / "zh"；未知值回落英文。返回 HM_OK 或 HM_E_INVALID_ARG。 */
HM_EXPORT hm_status hm_diag_render(const hm_diag *d, const char *lang,
                                   char *out, size_t cap, size_t *out_len);

/* 批量渲染，条目之间空一行（与 Go RenderAll 的骨架一致）。 */
HM_EXPORT hm_status hm_diag_render_all(const hm_diag *ds, size_t n,
                                       const char *lang,
                                       char *out, size_t cap, size_t *out_len);

/* 离线说明：文案中的 {arg} 换成 <arg>，末尾附 docs: 锚点。未登记码返回非 0。 */
HM_EXPORT hm_status hm_diag_explain(const char *code, const char *lang,
                                    char *out, size_t cap, size_t *out_len);

/* 机器可读旁路：explain 的 JSON 形态（code/message/why/fixes/docs）。
 * 与 hm_diag_explain 共用同一份 C++ 模板，Go 侧 --json 直接透传，不再二次渲染。 */
HM_EXPORT hm_status hm_diag_explain_json(const char *code, const char *lang,
                                         char *out, size_t cap, size_t *out_len);

/* 机器可读旁路：一条已渲染诊断的字段（code/severity/message/why/fixes/text）。
 * Go 侧的 Diag 结构据此落值，避免再 subst 一次造成双份文案。 */
HM_EXPORT hm_status hm_diag_render_json(const hm_diag *d, const char *lang,
                                        char *out, size_t cap, size_t *out_len);

#ifdef __cplusplus
}
#endif

#endif /* HERMES_DIAG_H */
