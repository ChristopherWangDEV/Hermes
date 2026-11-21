# C minimal

从零到"跑起来"三步。这是 C/C++ 团队评估时第一眼看的东西。

```sh
./build.sh      # 编 unit → 生成头 → 编译链接
./app           # printer is broken #3
```

## 三份文件

| 文件 | 是什么 |
|---|---|
| `units/label.hermes` | 一个不需要网络的 workflow |
| `main.c` | 你的 C 程序：include 生成的头、调用、释放 |
| `build.sh` | 三步：`hermes build` → `hermes header` → `hermes cc` |

## 几个会绊住人的地方

**① 头是生成的，不是手写的。** `label.h` 由 `hermes header label.hbc --c` 产出，
调用前必须先跑一次。改了 `.hermes` 要重新生成 —— 类型变了而不重新生成，
症状是链接期报 `undefined symbol`，而不是一条"你的头过期了"。

**② 用 `hermes cc`，不要手写 clang。** 它知道要链接运行时静态库、生成的 shim、
`hermes_core_impl.c` 与平台框架，并在链接后自检符号表。手写 clang 会漏掉 shim
与 `hermes_core_impl.c`，报 `hermes_<unit>_<wf>` / `hermes_last_error` 找不到 ——
这些符号不在运行时库里，而在 `hermes cc` 每次编进产物的生成 C 里。

**③ 谁分配谁释放。** `out` 的字节是**库**分配的，必须 `hermes_buf_free(&out)`。
用 `free()` 是错的：分配器不一定相同，跨分配器释放是未定义行为。
**例外**：`hermes_last_error()` 返回的 `hermes_str` 借的是库里的静态错误槽，
**不 free**、也不长期持有；按 `len` 读 `ptr`，视图在下次调用前有效。

**④ C89 严格。** 示例用 `clang -std=c89 -pedantic -Werror` 编译，
所以声明必须集中在块首（`-Wdeclaration-after-statement` 会拦）。

## CMake

```sh
cmake -S . -B build -DHERMES_ROOT=$HOME/.local/hermes
cmake --build build
./build/app
```

两步分工：`hermes_add_unit` 负责编 `.hbc` 与生成头，`hermes_link` 负责调
`hermes cc` 完成编译与链接（每个 unit 需要一份生成的 C shim，CMake 自己链静态库不完整）。

## 跑通了意味着什么

- 生成的头**只** include 了 `hermes/hermes_core.h`，没有系统头依赖
- 状态码是 enum，不是 `int`（传错类型编不过）
- 字符串视图带 `len`，含 NUL 的值不会被截断
