# 1.2.2：C++ 风格的 `for`

> 状态：**已实现并跑通**（`for` 头四段 + `i++`/`++i`/`i--`/`--i`/`i += k`/`i -= k`）。
> 下面「上界规则」里 `unbounded` 那条还没接进 `explain` 的输出。

这份文件记的是**还没实现**的东西。写下来是为了让"为什么这样设计"和"怎么验收"
在动手之前先定下来 —— 不是给已经能用的语法写文档。

## 语法

```hermes
for (int i = 0; i < 10; i = i + 1) { ... }   // 显式步进
for (int i = 0; i < 10; i++) { ... }         // 糖，等价于上一行
for (int i = 0; i < 10; ++i) { ... }         // 前缀形式，语义相同
for (int i = 10; i > 0; i--) { ... }         // 递减
```

## 一条硬规则：循环变量归 `for` 所有

`i` 由 `for` 节点拥有，**只有循环头**能改它。循环体里写 `i = i + 1` 仍然报错。

这不是限制，是让设计 A 成立的前提：

- IR 的 `var.decl` 唯一性不破 —— 每个变量仍只有一个声明点
- `let` 与 `var` 一一对应的不变量不破
- 不需要引入 `auto mut`，也不需要通用赋值语句

`while` 才需要通用可变变量（`auto mut` + 赋值语句）。那是 1.2.3 的事。

## `++` / `--` 的语义

只在**循环头的步进子句**里合法：

| 写法 | 等价 |
|---|---|
| `i++` / `++i` | `i = i + 1` |
| `i--` / `--i` | `i = i - 1` |

前缀与后缀在这里**没有区别**：步进发生在迭代末尾，表达式值不被使用。
在表达式里写 `x = i++` 这种用法不存在（语言没有通用赋值），所以也不必处理
"前缀与后缀取值不同"这个 C++ 陷阱。

## 上界规则

| 条件 | 结果 |
|---|---|
| `init`、`cond` 的上界都是整数字面量 | `max_spans` 仍是**编译期常量**，`explain` 给出数字 |
| `cond` 里出现运行期值（比如输入字段） | 该 workflow 标 **`unbounded`**，`explain` 明说 |

`unbounded` 的 workflow：`max_spans` 字段写成 `null`（而不是编一个假数字），
运行期由 `--max-steps` 兜底。这是**可见的降级**，不是静默的近似。

## IR 形状

```json
{"id": 7, "k": "for", "loc": {...}, "var": 0, "init": 3, "cond": 4, "step": 5, "body": 6}
```

`var` 是变量下标；`init`/`cond`/`step`/`body` 是节点编号。

## 验收

1. `for (int i = 0; i < 3; i++) { }` 编过、跑通、迭代 3 次
2. 字面量上界的 workflow：`hermes explain` 给出具体 `max_spans`
3. 运行期上界的 workflow：`explain` 显示 `unbounded`，`max_spans` 为 `null`
4. 循环体里写 `i = i + 1` 报错，错误信息说清"循环变量只由 for 头拥有"
5. `i++` / `i--` / `++i` / `--i` 四种写法行为一致
6. IR schema 增加 `nodeFor`，refs 检查覆盖 `var`/`init`/`cond`/`step`/`body`

## 依赖

零新增依赖。改动范围：`native/cpp/{lexer,parser,codegen,check_type}`、
`internal/{ir,engine}`、`schemas/ir.schema.json`、`tests/`。
构建仍是 `clang + libc++ + lld`，门禁继续拦 libstdc++ 与 GNU ld。

## 落点（下一轮直接照着改）

| 文件 | 改什么 |
|---|---|
| `native/cpp/ast.h` | `Tok` 加 `KW_FOR` / `KW_INC`(`++`) / `KW_DEC`(`--`) / `PLUS_EQ`(`+=`) / `MINUS_EQ`(`-=`)；`Kind` 加 `ForStmt`；`Node` 加 `body` 指针 |
| `native/cpp/lexer.cpp` | 关键字表加 `for`；`+`/`-` 后面跟 `+`/`-`/`=` 时合成对应 token |
| `native/cpp/parser.cpp` | `parse_stmt` 分派 `KW_FOR` → `parse_for()`；`parse_for` 读 `( auto i = e ; cond ; step )`，`step` 只接受 `i++` / `++i` / `i--` / `--i` / `i += k` / `i -= k` / `i = i ± k` |
| `native/cpp/codegen.cpp` | `case Kind::ForStmt` → `alloc("for")`，写 `var/init/cond/step/body`；`SP(for) = 1 + N × SP(body)`，N 由字面量上下界算出；算不出就把 workflow 标 `unbounded` |
| `native/cpp/check_type.cpp` | `for` 的 `init` 与 `body` 里 `i` 的类型必须是 `int`；`cond` 必须是 `bool` |
| `internal/ir/types.go` | `Node` 加 `Var/Init/Cond/Step/Body`；`Workflow` 加 `Unbounded bool`（或 `MaxSpans *int`） |
| `internal/ir/bounds.go` | `RecomputeMaxSpans` 增加 `for` 分支；`unbounded` 时跳过等式校验 |
| `internal/engine/run.go` | `case "for"`：绑定 `var`，按 `step` 更新，`cond` 为假或迭代到上界时退出 |
| `schemas/ir.schema.json` | 加 `nodeFor`；`max_spans` 允许 `null` |
| `tests/compiler/`、`internal/engine/` | 上面"验收"的 6 条 |
