/* C 侧最小示例：调一个 Hermes unit，读回结果。
 *
 * 编译：
 *   ./build.sh
 * 运行：
 *   ./app
 *
 * 这里刻意只用一个文件、一个 include —— 第一次试用时，用户应该看到
 * "包含头文件、调用、释放" 三件事，而不是一整套构建系统。
 */
#include <stdio.h>
#include <string.h>

#include "label.h" /* 由 hermes header 生成 */

int main(void) {
  const char* input = "{\"title\":\"printer is broken\",\"priority\":3}";

  hermes_buf in;
  hermes_buf out;
  hermes_status st;
  hermes_str err;

  in.ptr = (char*)input;
  in.len = strlen(input);
  out.ptr = NULL;
  out.len = 0;

  st = hermes_label_label(&in, &out);
  if (st != HERMES_OK) {
    /* hermes_last_error() 返回的是**共享静态错误槽的视图**（hermes_str{ptr,len}）：
     * ptr 恒非 NULL、按 len 读，**不要 free**，也不要留到下次调用之后再用 ——
     * 每个入口调用都会先清槽。槽归库所有，Python/Go 等 FFI 按 len 取字节即可。 */
    err = hermes_last_error();
    fprintf(stderr, "hermes error %d: %.*s\n", (int)st, (int)err.len, err.ptr);
    return 1;
  }

  printf("%.*s\n", (int)out.len, out.ptr);

  /* 谁分配谁释放：out 的字节是**库**分配的，必须用 hermes_buf_free 归还。
   * 用 free() 是错的 —— 分配器不一定相同。 */
  hermes_buf_free(&out);
  return 0;
}
