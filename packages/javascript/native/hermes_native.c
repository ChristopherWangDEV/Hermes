/* Hermes 的 Node 原生插件（N-API）。
 *
 * 为什么需要它：Node 没有内建 FFI（不像 Python 的 ctypes / Java 的 FFM /
 * C# 的 P/Invoke）。要在进程内调 C ABI，只有 N-API 插件这一条零依赖的路 ——
 * 它由 Node 官方提供头文件，我们自己写绑定，不引 node-addon-api / node-gyp。
 *
 * 编译（一条 clang 命令，无构建系统）：
 *   cc -shared -fPIC -I"$(node -p 'process.config.variables.nodedir')/include/node" \
 *      -o hermes.node hermes_native.c
 * 见 scripts/build_node_addon.sh。
 */
#define NAPI_VERSION 8
#include <node_api.h>
#include <dlfcn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

typedef int (*run_file_fn)(const char*, const char*, size_t, char**, char**);
typedef void (*free_fn)(char*);

static void* g_lib = NULL;
static run_file_fn g_run_file = NULL;
static free_fn g_free = NULL;

static napi_value throw_error(napi_env env, const char* msg) {
  napi_throw_error(env, NULL, msg);
  return NULL;
}

/* load(libraryPath) —— 加载运行时动态库。 */
static napi_value js_load(napi_env env, napi_callback_info info) {
  size_t argc = 1;
  napi_value argv[1];
  napi_get_cb_info(env, info, &argc, argv, NULL, NULL);
  if (argc < 1) return throw_error(env, "load 需要库路径");

  char path[4096];
  size_t len = 0;
  napi_get_value_string_utf8(env, argv[0], path, sizeof(path), &len);

  if (g_lib != NULL) {
    /* 重复 load 是常见用法（多个 unit 共用一个库），直接忽略。 */
    napi_value ok;
    napi_get_boolean(env, true, &ok);
    return ok;
  }
  g_lib = dlopen(path, RTLD_NOW | RTLD_LOCAL);
  if (!g_lib) {
    char msg[4600];
    snprintf(msg, sizeof(msg), "加载 %s 失败: %s", path, dlerror());
    return throw_error(env, msg);
  }
  g_run_file = (run_file_fn)dlsym(g_lib, "hermes_rt_run_file");
  g_free = (free_fn)dlsym(g_lib, "hermes_rt_free");
  if (!g_run_file || !g_free) {
    return throw_error(env, "动态库里缺 hermes_rt_run_file / hermes_rt_free");
  }
  napi_value ok;
  napi_get_boolean(env, true, &ok);
  return ok;
}

/* runFile(unitPath, inputJson) —— 执行并返回字符串。 */
static napi_value js_run_file(napi_env env, napi_callback_info info) {
  size_t argc = 2;
  napi_value argv[2];
  napi_get_cb_info(env, info, &argc, argv, NULL, NULL);
  if (argc < 2) return throw_error(env, "runFile 需要 (unitPath, inputJson)");
  if (!g_run_file) return throw_error(env, "先调用 load(libraryPath)");

  char unit[4096];
  size_t unitLen = 0;
  napi_get_value_string_utf8(env, argv[0], unit, sizeof(unit), &unitLen);

  char* input = NULL;
  size_t inputLen = 0;
  napi_valuetype t;
  napi_typeof(env, argv[1], &t);
  if (t == napi_string) {
    napi_get_value_string_utf8(env, argv[1], NULL, 0, &inputLen);
    input = (char*)malloc(inputLen + 1);
    if (!input) return throw_error(env, "内存不足");
    napi_get_value_string_utf8(env, argv[1], input, inputLen + 1, &inputLen);
  }

  char* out = NULL;
  char* err = NULL;
  int code = g_run_file(unit, input, inputLen, &out, &err);
  free(input);

  napi_value result = NULL;
  if (code != 0) {
    napi_throw_error(env, NULL, err ? err : "执行失败");
  } else {
    napi_create_string_utf8(env, out ? out : "", NAPI_AUTO_LENGTH, &result);
  }
  /* 两块内存都由原生层分配，必须用它的 free 归还。 */
  if (out) g_free(out);
  if (err) g_free(err);
  return result;
}

static napi_value init(napi_env env, napi_value exports) {
  napi_value fn;
  napi_create_function(env, "load", NAPI_AUTO_LENGTH, js_load, NULL, &fn);
  napi_set_named_property(env, exports, "load", fn);
  napi_create_function(env, "runFile", NAPI_AUTO_LENGTH, js_run_file, NULL, &fn);
  napi_set_named_property(env, exports, "runFile", fn);
  return exports;
}

NAPI_MODULE(NODE_GYP_MODULE_NAME, init)
