# C ABI 约定

生成的头（`hermes header --c`）里只有一份核心头：`hermes/hermes_core.h`。
本文写清楚**谁分配、谁释放、什么能跨线程**——C API 最容易被踩的就是这三件事。

## 所有权表

| 对象 | 谁分配 | 谁释放 | 备注 |
|---|---|---|---|
| `char*`（`hermes_rt_run` / `hermes_rt_run_file` 的 `out_ptr` / `err_ptr`） | **库** | 调用方，用 `hermes_rt_free` | 用 `free()` 是错的：分配器不一定相同 |
| 错误文本 | 由 `err_ptr` 返回，属于**本次调用** | 用 `hermes_rt_free` 归还 | 错误不再有线程局部缓冲，避免跨线程生命周期问题 |
| `hermes_buf input_json` | 调用方 | 调用方 | 库只读，不持有 |
| `hermes_app_*`（不透明类型） | 库 | 库（通过 `*_free`） | 只有 `@frozen` 的结构体会被展平 |

**一句话**：从库里出来的字节，必须用库的 `hermes_rt_free` 还。
这跟 `malloc`/`free` 不能跨分配器是同一个道理。

## 错误处理

- 所有函数返回 `hermes_status`：`HERMES_OK == 0`，其余见 `hermes_core.h` 的枚举
- 失败时 `out_json` **不被赋值**（不是置为半成品）
- 失败详情由 `hermes_rt_run` 的 `err_ptr` 返回，**属于本次调用**，调用方自己持有

## 字符串

`hermes_str` 带 `len`，不是裸 `const char*`。因为值里可以有 NUL，
而"跨语言边界上被静默截断"是最难查的一类 bug。

## 线程安全

| 对象 | 线程安全 |
|---|---|
| `hermes_rt_free` | 是（对不同的指针） |
| `hermes_rt_run` | 是（无全局可变状态） |
| 同一个 unit 的并发调用 | **否** —— unit 内部有可变的执行状态 |
| 不同 unit 之间 | 是 |

需要并发就开多个 unit，或者用 `hermes serve` 走 HTTP 面。

## 兼容性承诺

- **ABI major 相等即可**（`hermes version` 的 `abi` 字段），不要求严格相等
  （库里没有 `hermes_abi_major()`：两个来源必然漂移）
- `@frozen` 结构体是**布局承诺**：加字段就是 ABI break，必须重新编译所有使用方
- `@opaque` 类型（默认）只有指针对过边界，加字段安全

## 已知的坑

**`#include` 报错一次，用户就走了。** 所以生成的头**只** include `hermes/hermes_core.h`，
不依赖任何系统头（`<stddef.h>` / `<stdint.h>` / `<stdbool.h>` 都不用）。
如果你在嵌入式工具链上发现缺某个 typedef，那是一个 bug，报给我们。

**不要在外面再包 `extern "C"`。** 生成的头已经包了；嵌套在标准里合法，
但某些 clang 版本在 `-pedantic` 下会警告。

## 测试状态

| 项 | 状态 |
|---|---|
| C89 + `clang -std=c89 -pedantic -Werror` | ✅ 实测（`examples/c-minimal`） |
| `hermes cc` 链接与符号自检 | ✅ 实测 |
| CMake 集成（`cmake/hermesConfig.cmake`） | ✅ 实测：`cmake -S . -B build -DHERMES_ROOT=…` → 构建 → 运行 |
| `-fno-exceptions` 的 C++ 头 | ⚠️ 未实测 |
| 交叉编译（ARM / RISC-V） | ❌ 未支持，也没试过 |


## CMake 集成为什么不能直接链静态库

`hermes cc` 会**为每个 unit 生成一份 C shim**（运行期入口、错误串转换、
错误转换的实现都在 shim 里），然后把它和你的源码一起编译。
所以 `target_link_libraries(app hermes::runtime)` 是不完整的 ——
手写 clang 会在链接期报 `_hermes_last_error not found`。

`cmake/hermesConfig.cmake` 因此提供两个函数：

```cmake
hermes_add_unit(headers units/label.hermes)   # 编 .hbc + 生成 C 头 + include 路径
hermes_link("${CMAKE_CURRENT_BINARY_DIR}/app" main.c units/label.hermes)  # 走 hermes cc
```

`examples/c-minimal/` 是完整的可运行例子（实测通过）。

## IR 兼容性 ≠ ABI 兼容性

这两件事容易混，混了就会算错版本：

| 变化 | 影响 ABI major 吗 | 影响什么 |
|---|---|---|
| IR 里新增字段/节点（如 `functions[]`、`call_fn`） | **不影响** | `ir_version` |
| `hermes_core.h` 新增导出函数 | 影响（1.0 → 1.1） | ABI |
| `hermes_str` / `hermes_buf` 加字段 | 影响（`@frozen`） | ABI |

为什么第一阶段不影响 ABI：C 面看到的是**字节**，IR 的结构变化对调用方不可见。
但**旧运行时读到新 IR 会报"未知节点种类"** —— 那是 IR 兼容性问题，
由 `ir_version` 与加载器的校验答复，不是 ABI major。

所以加函数时：

- `include/hermes/hermes_core.h` **不改**，`hermes version` 的 `abi` 保持 `1.0`
- `ir_version` 提一档，并在本文件记录"旧运行时的行为"
- 若将来需要 `hermes_run_function` 这类**新入口**，才谈 ABI bump

## 真实导出符号（唯一事实源）

```c
void hermes_rt_free(char* p);
int  hermes_rt_run(const char* ir_data, size_t ir_len,
                   const char* input_data, size_t input_len,
                   char** out_ptr, char** err_ptr);
int  hermes_rt_run_file(const char* unit_path,
                        const char* input_data, size_t input_len,
                        char** out_ptr, char** err_ptr);
```

这三条来自 `cmd/hermesrt/main.go` 的 `//export`，是 `libhermesrt.a` 里实际存在的符号。
更早的文档里写过 `hermes_buf_free` / `hermes_last_error` / `hermes_abi_major`——
**它们从未被实现**，按那些名字写代码会链接失败。已删除，不留假 API。
