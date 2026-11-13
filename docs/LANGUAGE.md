# HermesScript 语言规范 2.0.0

| 项   | 内容                                                                                                                                                                                                                                                            |
| --- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 文档  | LANGUAGE 2.0.0（Phase 1 语言基线；内部修订号 v3.1 基础上并入 2.0.0 的 `fallback` 与 IR `decl`/`const`）                                                                                                                                                                          |
| 状态  | **规范性**。语言语义以本文为准 |                                                                                                                                                                                               |
| 上游  | `docs/IR.md`（产物形态）、`include/hermes/hermes_core.h`（C 侧契约） |
| 实现  | 词法/语法/类型检查/codegen：`native/cpp/*`（C++，静态库 `libhermes_parser`，C ABI `hm_parse_*`）；业务校验/归档/链接：Go                                                                                                                                                                |
| 配套  | `docs/IR.md`（IR 与归档格式）、`docs/ARCH.md`（系统架构）                                                                                                                                                                                                                   |
| 一句话 | 一种**无用户函数、无递归、无全局可变状态**的强类型编排语言。控制流是结构化树（有 `for` / `while`，但都只有单一出口），所以最大步骤数、最大并发数、最大调用次数**可静态计算**——**前提是树里没有不可定界的构造**：`while`、步进推不出次数的 `for`、`max_steps = unlimited` 都会让这个前提失效，此时产物显式标 `bounds.unbounded: true`（§6.4、`IR.md` §5.3）。这是语言设计对预算与反压的真实回报，也是它的边界。 |

### 标准库的实现可选性（g*/G* 机制）

> 规范性细则见 **`docs/STD-VARIANT.md`**（唯一规范源，被 `internal/std/variant.go` 引用）。

- **语言主体 = LLVM / Rust / C / C++，不是 Go**。Go 只用于工具链与标准库的**可选实现**。
- **标准库是 App 层可选件**：每个 `std` 成员提供多语言实现；涉及 Go 特性且 Go 版更优时用 **g*/G* 版**（Go 实现，`gnet` / `gmodel` / `gruntime` / `gjson`），同时始终提供**非 Go 版**（纯 C / Rust 主体实现，`net` / `model` / `runtime` / `json`）。App 层经 `HERMES_STD_VARIANT` 在运行期二选一，**默认 `go` = 今天的行为**。
- **绝不能写成「App 层 = Go」或「全仓 Go 化」**：实现语言是选修课，不是必修课。
- 该开关**不写进 IR、不改 `requires` 哈希**（否则破坏供应链防投毒与可重现性）；链接层仍按所选变体校验 `std` 成员确实被提供（`E_LINK_STD_VARIANT`）。

### 变更记录

| 版本（产品 semver） | 改动                                                                                                                                     |
| ------------- | -------------------------------------------------------------------------------------------------------------------------------------- |
| v3.0          | 首版。五阶段编译链路、`<T>` 消歧 R-TY-1..4、`parallel` 语义、四个静态上界、幂等证明、错误层级、诊断质量                                                                      |
| **v3.1**      | **并入 D-014（语法随库模型调整），另并入 D-023 末节的 `auth` 子句。11 处改动，全部是语法/契约层，不动 v3.0 的语义结论**                                                          |
| **v2.0.0**    | **新增 `fallback` 错误边界表达式（§5.4.1）；`fallback` 成为保留字（破坏性变更）；`E4025` / `E4026` 两个新码；IR 节点 `let`→`decl`、`mut`→`const`（IR `ir_version` = 2）** |



> `v3.0`–`v3.1` 是**语言规范的内部修订号**，早于产品 2.0.0 的版本体系；**产品 2.0.0 起，本表一律用产品版本号（semver）**。  
> 两套编号不是同一根轴：内部修订号记录"规范文本改了几轮"，semver 记录"发布了什么"。本行 `v2.0.0` 之后的每一行都只写 semver。



> **产品版本 2.0.0 是长期支持版本（LTS）。** 2.x 只保证与 **≥2.0.0** 的 IR 互读：  
> `ir_version` 在 2.x 期间**恒为 2**（新增节点靠 kind 的可选字段扩展，不靠版本号区分），  
> 只有要破兼容时才升 3。**不做向前兼容**：1.x 的 IR 一律硬拒绝（`E_IR_VERSION`），  
> 不提供迁移路径 —— 按 2.0 的字段语义去解释一份 1.x 产物，得到的是"看起来对的错答案"。

v3.1 的 11 处改动，逐条给出「为什么它不是风格问题」：

| #  | 改动                                                                                                     | 章节            | 来源                                |
| -- | ------------------------------------------------------------------------------------------------------ | ------------- | --------------------------------- |
| 1  | `#include` **只吃 `.h`**；`#include "x.hermes"` 变编译错误（`E3015`）；实现靠 `#link`                                | §2.7 §9       | D-014.1                           |
| 2  | 顶层声明**默认私有**，加 `export`；符号表只认 `export`                                                                 | §3.1 §3.5     | D-014.2                           |
| 3  | 新增 `#link <std/net>` / `#link "models"`，依赖写进源码、编进 `.hbc` 的 `requires`                                  | §2.7 §9.5     | D-014.3                           |
| 4  | `backend` 拆声明（`.h`）/定义（`.hermes`）；地址走 `env(名, 字面量默认值)`                                                 | §3.1 §8.5     | D-014.4                           |
| 5  | 新增 `entry` 标注：恰好一个才可省略 `--workflow`                                                                    | §3.1 §3.6     | D-014.5                           |
| 6  | **`input` 必须显式类型**，且其类型必须 `export`；请求 schema 由它生成                                                      | §3.1 §4.8     | D-014.6                           |
| 7  | **`method` 只接受 6 个字面量**；省略时默认 `"GET"`（只读）                                                              | §3.3 §5.6     | D-014.7                           |
| 8  | `workflow` 名从**字符串字面量**改为**标识符**：`workflow "support_bot"` → `workflow support_bot` | §3.1          | D-014 示例                          |
| 9  | `HttpError` 必须携带 `status` 字段（否则"订单不存在"分支写不出来）；`W5004` 降级为**启发式**警告                                     | §7.1 §12.1 L3 | D-014 末段                          |
| 10 | **撤销 v3.0 的「`catch` 体内不能 `return`」**（`E2012` 废弃）                                                       | §5.4          | 本版自裁 |
| 11 | `backend` 定义可带 **`auth` 子句**（密钥只从 `env` 来、无默认值）；`env()` 的**元数按位置定**（地址位 2 个、`auth` 位 1 个）              | §3.1 §2 §8    | D-023 末节                          |

> 第 8 条要单独说一句：`workflow "support_bot"` 变 `workflow support_bot` 不是排版差异。名字进符号表后就是**标识符**，  
> 于是 `--workflow support_bot` 的取值来自符号表而不是字符串匹配，`hermes ar t` / `E_LINK_*` 报的符号名也就没有  
> "带引号还是不带"的二义。字符串字面量名会让符号表同时接受 `"support_bot"` 与 `support_bot` 两种拼写。
>
> 第 10 条是**本版唯一一处撤销既有规则**的地方，理由见 §5.4：v3.0 说"允许 `return` 会让 trace 父子关系与实际执行不符"，  
> 但同一套论证会让 `if`/`match` 分支里也不能 `return`。而 D-014 末段要求的正是  
> `catch (HttpError e) { if (e.status == 404) { ... } }`——没有 `return` 的话，恢复出来的值无处安放。
>
> 另修三处 v3.0 自身的缺陷（与 D-014 无关，但同批改掉，避免工程师照错的写）：  
> ① §4.7 的注解示例写成 `string currency;  @enum("USD","CNY");`（双分号），与 §3.1 的  
> `Field = Type Ident ["?"] {Annotation} ";"` 矛盾；  
> ② §11.2 ④ 的 `idem_key = "{intent.order.order_id}"` 违反 §5.7 的 `Pure(e)`（`intent` 来自 `model.generate`，不纯）；  
> ③ §3.4 的 R-TY-2 写成"其余一切情形 `<` 是 LT"，漏了作用域——`#link <std/net>` 里的 `<` 由词法器吃掉，永不到达表达式解析器；  
> 且 §2.8 的 token 表漏登记了 `->`（ARROW）与 `:`（COLON）两个 v3.0 已经用到的 token。

---

## 1. 定位与范围

### 1.1 它是什么

HermesScript 描述"一次 AI 工作流执行"：读入一个强类型输入，串/并联若干次模型调用与 HTTP 工具调用，最后返回一个值。文件模型照搬 C，但**声明与实现的分界线是强制的**：

| 文件         | 对应 C | 内容                                                                         | 谁产出                            |
| ---------- | ---- | -------------------------------------------------------------------------- | ------------------------------ |
| `.hermes`  | `.c` | 实现：`#link` + `backend` 定义 + 带体的 `workflow` + 本 unit 私有 `struct`            | 人写                             |
| `.h`       | `.h` | 接口：`#include` + **`export`** 的 `struct` / `backend` 声明 / `workflow` 签名（无体） | `hermes header` 生成，可手改         |
| `.hbc`     | `.a` | 归档：可链接单元（多个 unit + 符号表 + 类型表 + `requires` + 签名）                            | `hermes build` / `hermes link` |
| `#link` 指令 | `-l` | **链接输入**，写在 `.hermes` 源码里而不是命令行                                            | 人写                             |

两个方向各自单向：

```
   源码方向（人读的东西）                     二进制方向（机器读的东西）
   #include "x.h"   ── 只给声明 ──┐
   #link "x"        ── 只给实现 ──┤
                                  ├──►  compile  ──►  x.hbc ──►  link  ──►  可执行 IR 集合
   .hermes 的实现   ── 给出实现 ──┘                    （requires 在此校验）
```

**`#include` 与 `#link` 不是一回事，不许互换**：前者回答"它长什么样"，后者回答"它的代码在哪"。把两者合成一个 `#include`（v3.0 的做法）会得到一个无法回答的问题——"我 include 的到底是声明还是实现"，而这个问题只能在链接期回答，不能在词法期假装回答。

### 1.2 编译链路（五阶段，D-011）与语言规范的对应

| # | 阶段                           | 语言  | 产物                                   | 本文对应章节                     |
| - | ---------------------------- | --- | ------------------------------------ | -------------------------- |
| 1 | Lexer + Parser（含 include 解析） | C++ | AST + 诊断                             | §2 §3 §9                   |
| 2 | Type Checker（含幂等证明、上界计算）     | C++ | 带类型标注的 AST                           | §4 §5 §6 §7                |
| 3 | Codegen → JSON DAG           | C++ | **IR（JSON）**                         | §10 + `IR.md`              |
| 4 | Static Analyzer（业务校验）        | Go  | 校验报告 + warning                       | §8（能力/预算可行性）               |
| 5 | Emit / Sign / Archive        | Go  | `.hbc` + `dist/<name>.sealed.hermes` | `IR.md` §7、`ARCH.md` §5.11 |

**前端不依赖运行时**：`libhermes_parser` 是纯 CPU、纯内存、**无 IO**、**无全局可变状态**、**`-fno-exceptions -fno-rtti`** 的静态库。所有 IO（读文件、展开 include 的候选路径、写产物）在 Go 侧；C++ 收字节、还字节。这条边界的收益：前端可被 Python 绑定或编辑器插件单独复用；且异常不会在 cgo 边界上变成未定义行为。

### 1.3 它的边界是什么（每条都是设计裁决，不是缺陷）

下表第一列是**语言层的规则**——既包括"不做什么"，也包括几条"做了、但只做到这里"（`for` / `while`、可变绑定）。第二列写清每条规则换来了什么性质；性质写不出来，这条规则就该被重新讨论。

| 规则                                                                                                                       | 因此可证的性质（这不是缺陷，是收益）                                                                                                                      |
| ------------------------------------------------------------------------------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------- |
| `model.agent` 的 `max_parallel_tools` 必须是常量；`max_steps` 是常量**或** `unlimited`（§6.4，后者让该 workflow 变 `unbounded` 并发 `W2004`） | 只要没有 `unlimited`，最大步骤数、最大调用次数可静态计算（工具上界 = `max_parallel_tools × max_steps`）；出现 `unlimited` 时产物显式标 `unbounded`，不再假装有上界                   |
| 用户函数（`fn` + `call_fn`，2.0.0 已支持）                                                                   | 非递归函数在 codegen 期**内联展开**为普通节点（路线 A/B），`max_spans` 仍可静态定界；仅 `call_fn` 一种动态调用路径（递归），运行期深度上限 512（`internal/engine/run.go:837`），超界回普通错误而非崩溃 |
| 递归仅限 `call_fn` 动态路径，深度上限 512                                                                                             | 非递归函数编译期内联展开，调用图主体仍是静态可算的树/展开；递归调用次数不可静态定界，由运行期 `maxCallDepth=512` 兜底（§FUNCTIONS、§6），不写进 IR、不作预算依据                                      |
| 绑定默认**可变**：`auto` 是可变绑定、`const` 才不可变，`i++` / `x = expr` 都是语句                                                             | 变量类型在声明处由初始化式**静态**确定（`auto` 是编译期推导，不是动态类型），所以可变绑定不带来动态派发；上界核算按"循环体 × 次数"算（§6.1），次数可证才有 `max_spans`                                     |
| `parallel` 分支里声明的名字出块即关闭（`E5002`）；体外绑定在体内只读（`E5010` 编译期硬错）                                                               | 分支的产物只能经 `auto [a, b] = parallel { … }` 解构取出，不能靠"分支里定义、块外引用"串数据；也不能靠写体外变量串回去。体内只读已强制，见 §5.2 与 `KNOWN-GAPS.md` 的决定记录                     |
| 无类/方法/接口                                                                                                                 | 类型集合封闭，`catch` 可穷尽检查                                                                                                                    |
| 无浮点进入金额路径                                                                                                                | D-010.1，编译期拒                                                                                                                            |
| 无可变全局（语言层）：所有绑定都在 workflow / 块作用域内                                                                                       | 没有可被两个 workflow 实例共享的状态。**注意**：有 `for` / `while` 之后，trace 会重复经过同一个节点，所以「IR 与 trace 序列同构」不再成立；成立的只有「节点集合确定」与「上界按一次迭代 × 次数核算」（§6.1）       |

> **代价必须承认**：`parallel` 的元组不能存储、不能跨块传递；用户函数已在 2.0.0 支持（非递归函数 codegen 期内联展开，递归仅 `call_fn` 动态路径、深度上限 512），"重复逻辑只能复制"已不再是必要代价；`for`/`while` 与可变绑定见 §6 / §1.3 规则行。

### 1.4 黄金样例（本规范的验收基线，必须原样编译通过）

**权威副本在 `examples/support_bot/`**（四个文件 + `input.json` + `README.md`）。本节逐字重复一遍，因为规范不能引用一个可能被改坏的外部文件；`tests/compiler/golden_support_bot_test.go` 负责断言两边**代码逐字节相等**——用 `go:embed` 读 `examples/` 下的真文件，**剥掉全部注释与空行**后与本节的代码块比对。剥注释是刻意的：注释是给人读的，若它进断言，规范里每补一句说明就要同步改样例，最后两边都会被逼着少写注释。**代码一行都不许差。**

#### ① `models/order.h`

```h
export struct OrderInfo {
    string order_id;
    int status;         // 业务状态码。注意：这不是 HTTP 状态码（见 README §4）
    cost amount;       // D-010.1：金额必须用 cost，浮点（double）不进金额路径
}
```

#### ② `models/intent.h`

```h
#include "order.h"

export struct Intent {
    string action;      // "refund" / "logistics" / 其他
    OrderInfo order;
}
```

#### ③ `support.h`（对外契约，分发到无源码环境时只需要它 + `support_bot.hbc`）

```h
#include <std/net.h>
#include <std/model.h>
#include "models/intent.h"
#include "models/order.h"

export backend homonoia;

export struct SupportInput {
    string text;
    string user_id;
}

export workflow support_bot(input: SupportInput, backend = homonoia) entry -> string;
```

#### ④ `support.hermes`（实现；编译入口是它）

```h
#include "support.h"

#link <std/net>
#link <std/model>

backend homonoia = env("HOMONOIA_URL", "http://127.0.0.1:8080");

workflow support_bot(input: SupportInput, backend = homonoia) entry {
    auto intent = model.generate<Intent>(
        prompt = "识别用户意图：{input.text}",
        precision = "exact"
    );
    match (intent.action) {
        case "refund": {
            auto [order, history] = parallel {
                auto o = tool.call<OrderInfo>(
                    url = "https://api.internal.com/orders/{intent.order.order_id}",
                    method = "GET"
                );
                auto h = tool.call(url = "https://api.internal.com/history/{input.user_id}");
                return [o, h];
            };
            return model.generate(
                prompt = "根据订单金额 {order.amount}，生成退款话术。",
                precision = "fast"
            );
        }
        default: { return "抱歉，我无法处理该请求。"; }
    }
}
```

#### 这个样例刻意固定下来的 8 个语法事实

| # | 事实                                                                                                              | 若写错                                                               |
| - | --------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------- |
| 1 | `workflow` 名是**标识符** `support_bot`，不是字符串 `"support_bot"`                                                        | `E2002`                                                           |
| 2 | 顶层声明**必须 `export`** 才能被外部/链接看见；`.hermes` 里的 `workflow` 与 `backend` 定义**不带** `export`（`export` 已在 `.h` 的声明上出现一次） | `E3012`（重复 export）或 `W3013`（私有 workflow 恒为死代码——语言没有跨 workflow 调用） |
| 3 | `input` **带类型** `SupportInput`，且该类型必须 `export`                                                                  | `E3023` / `E3013`                                                 |
| 4 | `#include` 只吃 `.h`；`#link` 只吃模块名；`tool.call`/`model.generate` 用了就必须 `#link` 对应 std                              | `E3015` / `E3016`                                                 |
| 5 | `env("名","默认值")` 只出现在 `backend` 定义的右值位置                                                                         | `E3018`                                                           |
| 6 | `entry` 在本 unit 恰好一个                                                                                            | `E3021`/`E3022`                                                   |
| 7 | `method` 只写字面量；`h` 那次调用省略 `method` ⇒ 默认 `"GET"` ⇒ 只读                                                            | `E2016`                                                           |
| 8 | `match` 必须有 `default`；`intent.action` 是 `string`                                                                | `E4018`                                                           |

#### 与用户原始语法参考的 5 处刻意差异

| # | 原文                                            | 本文                                                                   | 依据                                                                            |
| - | --------------------------------------------- | -------------------------------------------------------------------- | ----------------------------------------------------------------------------- |
| 1 | `float amount`（旧类型名；v2.0.0 起为 `double amount`）                                | `cost amount`                                                        | D-010.1：金额禁止浮点（§4.3）                                                          |
| 2 | `if (order.status == 404)`                    | 删掉                                                                   | D-010.2：404 已抛 `HttpError`，该分支不成立（`examples/support_bot/README.md` §4 给出正确写法） |
| 3 | `auto user_input = input.text;`（`input` 无声明）  | `input: SupportInput`                                                | D-014.6：强类型不能从入口第一行就漏                                                         |
| 4 | `workflow "support_bot" (backend = homonoia)` | `workflow support_bot(input: ..., backend = homonoia) entry`         | D-014.2/5/6 + 变更记录第 8 条                                                       |
| 5 | `backend homonoia = "http://..."`             | `.h` 声明 + `.hermes` 里 `env("HOMONOIA_URL", "http://127.0.0.1:8080")` | D-014.4：硬编码地址的库无法分发                                                           |

---

## 2. 词法

### 2.1 源文件

- UTF-8，允许 BOM（解析前剥除）；非法 UTF-8 → `E1001`（报字节偏移）。
- 行尾 `\n` 或 `\r\n`；`hermes fmt` 统一输出 `\n`。
- 单文件上限 8 MiB（`E1002`）；include 展开后总 token 数上限 2,000,000（`E1003`）；模块数上限 256（`E3004`）；include 深度上限 32（`E3003`）。


### 2.2 注释

```h
// 行注释
/* 块注释，不嵌套（与 C 一致） */
```

不嵌套是**故意**的：嵌套注释让 `/* /* */` 的解释依赖实现，而 `fmt` 必须语义不变地重排注释。注释在 AST 上以"附着到下一个 token"的方式保留，供 `fmt` 还原（§13）。注释**不进 IR**（IR 是执行计划，注释无执行语义）。

### 2.3 空白与换行

空白仅用于分隔 token；**换行不是语句终结符**（语句以 `;` 或 `}` 终结），因此 `fmt` 可自由折行而不改语义——这是 `fmt` 幂等性的基础。

换行有语义的唯三处：`//` 注释到行尾；`#include` 是**行指令**（`#` 必须是行内第一个非空白字符，路径必须同行闭合）；`#link` 也是行指令（必须同行闭合；行末的 `;` **可选**——带了仅触发 `W2003` 警告，见 §2.7、附录 D，理由是可判定性 + 与 C++ 指令行对齐，不是历史包袱）。

### 2.4 标识符与保留字

```
Ident = ( Letter | '_' ) { Letter | Digit | '_' } ;   Letter = A-Z a-z ;  Digit = 0-9
```

| 类别 | 词 |
|---|---|
| 关键字（29） | `struct` `backend` `workflow` `if` `else` `match` `case` `default` `while` `for` `in` `break` `continue` `retry` `delay` `try` `catch` `return` `parallel` `security` `allow` `deny` `with` `const` `auto` **`export`** **`extern`** **`unlimited`**（见 §6.4） **`fallback`**（见 §5.4.1） |
| 字面量关键字（2） | `true` `false` |
| 类型名（6，仅类型位置） | `string` `int` `double` `bool` `json` `cost` |
| **保留命名空间（6）** | `model` `tool` `json` `input` `memory` `vector` |
| 错误类型名（6，仅 `catch` 位置） | `Error` `NetworkError` `HttpError` `ModelError` `ContractError` `CircuitOpenError` |
| **上下文关键字（3）** | `entry`（仅 workflow 签名的 `)` 之后）、`env`（仅 `backend` 定义的 `=` 之后）、`auth`（仅 `BackendDef` 的地址（`env(...)` 或字符串字面量）之后，且后随 `env`） |
| 通配（1） | `_`（仅 `auto [a, _] = …` 的解构丢弃位）。兜底分支用 `default:`，不再用 `_` |

**保留命名空间是硬约束**（`E1004`）：`model`/`tool`/`json`/`input` 不能作变量名、struct 名、字段名、backend 名。理由见 §3.4——泛型消歧的正确性依赖"内建被调者集合封闭且可判定"。

**`export` 与 `extern` 正交**：`export` 说我提供（进符号表）、`extern` 说我使用、体在别处（native 归档）。两者独立——一个符号可同时 `export extern`（对外暴露、体在 native），也可只 `export`（体在本 unit）或只 `extern`（内部使用的 native 绑定）。它不是命名空间，是**链接来源**修饰（§3.8）。

**为什么 `entry` / `env` / `auth` 是上下文关键字而不是保留字**：三者都出现在**唯一**的语法位置，所以判定只需定长前瞻——
`)` 之后若见 `IDENT("entry")` 且其后是 `{` 或 `->`，就是标注（那里本来不允许出现标识符）；`backend X =` 之后若见 `IDENT("env")` 且后随 `(`，就是环境读取（那里本来只允许字符串字面量）；`backend X = <地址>` 之后若见 `IDENT("auth")` 且后随 `IDENT("env")`，就是 `auth` 子句（那里本来只允许 `;`，见下）。语法上完全可判定。
而把这三个词设为保留字会白拿走三个高概率标识符：配置 struct 里的 `env` 字段、审计模型里的 `entry` 字段、认证配置里的 `auth` 字段都很常见——为一个定长前瞻就能解决的问题付这个代价不值得。**判定规则必须是规则，不是"实现自己看着办"**，所以上表把它写成规范。

**`unlimited` 反过来是保留字，不是上下文关键字**（与上一条不是双标，判据相同：代价 vs 收益）。它出现在一个**表达式**位置（`max_steps = unlimited` 的右值），而那里任何 `IDENT` 都合法 —— 所以没有"那里本来不允许标识符"可以用，判成上下文关键字只能靠"看它是不是恰好叫 unlimited"，那就是把语义塞进字符串比较。更要紧的是含义不能被遮蔽：若允许 `auto unlimited = 3;`，同一个词在同一个文件里就会代表两件事（"没有编译期上界"与一个整数），而变量会**静默盖住**标记 —— 那种 bug 只在某次改动后表现为"上界突然算出了 3"，无从追起。所以它被保留，代价是拿走一个在 Hermes 里本来就很具体的词。它的合法位置见 §6.4。

**`auth` 这一处单独写清，免得下次还要重新推一遍**：`BackendDef` 的地址部分读完（`env(...)` 的 `)` 或字符串字面量）之后，按文法合法的下一个 token **只有 `;`**——产生式里已经没有任何还能吃标识符的位置了。于是"这里冒出一个标识符"本身就说明它不是普通标识符，判据与 `entry` / `env` 完全同类：**那里本来不允许标识符**。再看它为何不必成为关键字——`auth` 是配置结构里最普通不过的字段名（`.homo` 的 `gateway` 就叫 `auth`），设成保留字会把它从每个 struct 字段、变量名、backend 名里拿走，而这个代价换不到任何判定能力：「地址之后见 `IDENT("auth")` 且后随 `IDENT("env")`」是 2 token 定长前瞻，且只可能在这一个位置成立。**所以它是上下文关键字，§2.4 关键字表那一行一个字不变。**


### 2.5 字面量

| 字面量 | 语法 | 语义 |
|---|---|---|
| 整数 | `0` \| `[1-9][0-9_]*` \| `0x[0-9a-fA-F_]+` | `int`（int64）。**无八进制**：`0755` 是十进制 755（与 C 的刻意差异，前导零在配置里常见） |
| 浮点 | `D.D` \| `D.D e[±]D` \| `D e[±]D` | 词法上识别为 `double`（IEEE-754 binary64）。**小数点后必须跟数字**：`1.5` / `1.5e2` / `1e3` 合法；`1.` 会被拆成 `INT 1` + `.`（随后 `E1001`），`.5` 非法（`E1005`，避免与路径 `.` 混淆）—— 旧版写「`1.` 合法」与 `lexer.cpp::lex_number()` 不符，按实测校准。**2.0.0：字面量已放开**，编成 IR 位模式 `{"k":"double","bits":"<16 位小写 hex>"}`；IR 字节里绝不放 JSON 浮点数字，V-7 / canonical / 签名链都不受影响。`E4029` 现在只是**旧 `float` 标签的纵深防线**（三道：check_type 全 AST 扫描 + codegen `gen_expr` + 参数默认值折叠），不再是「所有浮点字面量被拒」。语言里没有 `float` 类型；`json.as_float(j)` 保留 API 原名、窄化到 `double`（cast 节点的 `to` 写 `"double"`）。见 §4.1、附录 A 与 `KNOWN-GAPS.md` |
| 金额 | `$` `D` `[.D{1,6}]` | `cost`，单位 **micro-USD**。`$1.50` = 1,500,000。>6 位小数 → `E1006`（§12.2） |
| 字符串 | `"..."`（单行，含插值） | `string` |
| 布尔 | `true` `false` | `bool` |

**字符串必须单行**：字面换行 → `E1007`。这条让"未闭合字符串"立刻在行末失败，而不是吞掉后面几百行——错误恢复质量的关键。

转义：`\\` `\"` `\n` `\r` `\t` `\u{XXXX}`（1–6 位十六进制，必须合法标量值，不接受代理对）。未知转义 → `E1008` 并列出全部合法转义。

### 2.6 字符串插值（设计裁决 Q2）

```h
"orders/{user_id}"
"意图：{input.text}"
"a{{b"                     // 字面 "a{b"
"金额 {order.amount_cents} 元"
```

**规范规则（5 条，全部可测）**

| # | 规则 |
|---|---|
| I-1 | 字符串内 `{` 开启插值，到同层第一个 `}` 结束 |
| I-2 | 字面 `{` 写作 `{{`。**`}` 不需转义**：插值之外的 `}` 是普通字符（刻意非对称：`{` 有歧义，`}` 没有） |
| I-3 | 插值内容**只允许路径** `ident ( "." ident )*`。不允许任意表达式、调用、算术、嵌套插值、索引 |
| I-4 | 静态类型必须 **text-able**：`string`/`int`/`double`/`bool`/`cost`/`json`（`double` 用 shortest round-trip 文本）。**struct 不允许**（必须取到叶子） |
| I-5 | 插值依赖**静态可见**：编译期把每个字面量的插值路径记入 IR（`interp_deps`），`hermes explain` 直接打印"此 prompt 依赖 `input.text`、`intent.order.order_id`"，无需运行 |

**为什么 I-3 限制为路径（这是本文最容易被质疑的一条）**

1. **消除第二个 `{` 歧义源**。`{` 同时是块分隔符。若插值内允许完整表达式，则 `{` 之后要重新进入表达式解析器，而表达式解析器又要处理 `match` 臂里的 `case ...: {`——两个 `{` 语法位置互相嵌套，错误恢复会给出荒谬定位（用户看到"第 87 行 `}` 少了"，实际错在第 3 行）。
2. **`explain` 的核心价值**是"这个 prompt 依赖哪些输入"。允许任意表达式时这变成不可静态回答的问题。
3. **需要计算时有更清晰的写法**：`auto amount = order.amount_cents;` 然后 `{amount}`。符合"无隐式魔法"。

**`{` 的两种角色如何彻底消除歧义**：由**词法状态**决定，不由语法分析决定。进入 `"` 后切到 `STRING` 模式；该模式内 `{` 只有插值含义、`"` 退出、换行报 `E1007`。块分隔符 `{` 只可能出现在 `NORMAL` 模式。**不存在需要回溯的情形**。

### 2.7 `#include` 与 `#link` 的词法

两条指令都是**单 token**：识别行首的 `#` 后要求紧跟 `include` 或 `link`，按下一非空白字符分流 `<...>` 或 `"..."`，整条内容作为载荷。因此 `<` 在指令行内**不会**被当作泛型或关系运算符。

| 指令 | 形态 | 载荷 | 行尾 | 语义 |
|---|---|---|---|---|
| include | `#include <std/net.h>` | 角度路径 | **无** `;`（路径自行闭合） | 取声明（类型信息） |
| include | `#include "models/order.h"` | 引号路径，相对当前文件 | 无 `;` | 同上 |
| link | `#link <std/net>` | 角度模块名，仅 `std` 域 | 可选（带则 `W2003`） | 取实现（内置 `stdlib.hbc` 成员） |
| link | `#link "models"` | 引号模块名 | 可选（带则 `W2003`） | 取实现（`models.hbc`） |
| link native | `#link native "归档名" from "纯 C 头"` | 两个字符串：`"归档名"` → `lib<名>.a`/`.so`（同 `-l<名>`，D-020）；`"纯 C 头"` 必须是**纯 C ABI 头**（cgo 只解析 C） | 可选（带则 `W2003`，与 `#link` 同规则） | 取实现（用户自己的 C 归档；`extern` 符号只从它解析，§3.8） |

为什么 `#include` / `#link` 的 `;` **可选**（带了只警告）：`#include` 的载荷在 `>` 或 `"` 处**已经自闭合**，加分号纯属冗余；`#link` 的引号形式之后虽无闭合字符，但 `fmt` 在指令区只做稳定分区（include 全在前、link 全在后，见附录 B），"下一行是 `#include`"的歧义本就不成立。于是两条指令的分号都**可选**：不写是默认写法；写了仅触发 `W2003`（"指令行末多余的分号"，**不阻塞构建**；clang 对 `#include <x>;` 同理：`extra tokens at end of #include directive`）。`#link native …`（§2.7、§3.8）沿用同一条规则：分号可选、带则 `W2003`。**刻意的可选，理由是与 C++ 指令行对齐、且不让 `fmt` 去猜"这行到底结没结束"。**

约束：

- `<...>` 内禁止 `>`、换行、空字节；include 的两种形式都禁止路径分量 `.` / `..`（`E1009`，防穿越）。
- `#link "..."` 的载荷**不是路径**，是模块名：必须匹配 `Ident ( "/" Ident )*`（`E1010`），且**不得**带 `.hbc` 后缀（`#link "models.hbc"` → `E1010`，文案："模块名不是文件名，`#link "models"` 会解析到 `models.hbc`"）。这条防的是"看起来像路径、以为是路径，于是以为能写绝对路径"。
- `#link` 只允许出现在 `.hermes` 里（`E3017`）。理由见 §9.1。
- 内置域之外的 `<>` → `E3001`（`#link <net>` 报"内置模块名必须形如 `<std/名字>`"）。

### 2.8 Token 种类（实现契约）

```
EOF IDENT INT FLOAT MONEY STRING(parts) INCLUDE(path,mode) LINK(spec,mode)
STRUCT BACKEND WORKFLOW LET IF ELSE MATCH RETRY DELAY TRY CATCH RETURN PARALLEL EXPORT
UNLIMITED
TRUE FALSE TYPE(which) RESERVED_NS(which) ERR_TYPE(which) UNDERSCORE
LBRACE RBRACE LPAREN RPAREN LBRACKET RBRACKET SEMI COMMA DOT COLON AT
LT LE GT GE EQEQ NE ANDAND OROR BANG PLUS MINUS STAR SLASH PERCENT ARROW COLON
```

`FLOAT` / `TY_FLOAT` 是**实现侧 token 枚举的沿用旧名**（§2.8 是「实现契约」，不是类型名表）；自 v2.0.0 起它承载的是 `double`（IEEE-754 binary64）字面量，语言里没有 `float` 类型（§2.5、§4.1）。

> `mode ∈ {angle, quote}`。`ARROW` 是 `->`（workflow 返回类型），`COLON` 是 `:`（`case`/`default` 臂与字段声明共用）。
> `FATARROW`（`=>`）在内部修订号 v3.1 存在过，**产品 2.0.0 起取消**（当时记为内部修订 v3.2，两者是同一个版本）：`match` 臂改为 C 风格的 `case X:` / `default:`。
> 取消理由：它只出现在一个语法位置，没有任何第二种用途；而 `case`/`default` 与语言整体 C 风格一致，
> 且 `default` 这个词本身就说清了「这是兜底」，比 `_` 更难被误读。


---

## 3. 语法

### 3.1 EBNF（完整，无省略；`.hermes` 与 `.h` 各一套顶层产生式）

```ebnf
; ---- 两套顶层：实现文件与接口文件 ----
ImplFile    = { Directive } { ImplDecl } EOF .
HeaderFile  = { Directive } { HdrDecl  } EOF .

Directive   = INCLUDE | LINK .

ImplDecl    = DeclVis StructDecl | DeclVis BackendDef | DeclVis WorkflowDecl .
HdrDecl     = DeclVis StructDecl | DeclVis BackendSig | DeclVis WorkflowSig .
DeclVis     = [ "export" ] .

StructDecl  = "struct" Ident "{" { Field } "}" .
Field       = Type Ident [ "?" ] { Annotation } ";" .
Type        = BaseType { "[" "]" } .
BaseType    = "string" | "int" | "double" | "bool" | "json" | "cost" | Ident .
Annotation  = "@" Ident [ "(" Lit { "," Lit } ")" ] .

BackendSig  = "backend" Ident ";" .
BackendDef  = "backend" Ident "=" ( StringLit | EnvExpr ) [ "auth" EnvExpr ] ";" .
EnvExpr     = "env" "(" StringLit [ "," StringLit ] ")" .  ; 仅允许出现在此处（§2.4）。
                                                           ; 元数按位置定：地址位 2 个（缺默认值 → E3018），auth 位 1 个（多给默认值非法，见 §3.1 注 8）

WorkflowDecl= "workflow" Ident "(" [ ParamList ] ")" [ "entry" ] [ "->" Type ] Block .
WorkflowSig = "workflow" Ident "(" [ ParamList ] ")" [ "entry" ] "->" Type ";" .
ParamList   = InputParam { "," Param } .
InputParam  = "input" ":" Type .                           ; 固定标签，不是普通标识符
Param       = Ident "=" Expr .

; ---- 语句 ----
Block       = "{" { Stmt } "}" .
Stmt        = VarDecl | AssignStmt | UpdateStmt | IfStmt | WhileStmt | ForStmt
            | BreakStmt | ContinueStmt | MatchStmt | RetryStmt | TryStmt
            | ReturnStmt | ExprStmt .

; 绑定：auto = 由初始化式推导（编译期），写了类型就必须与初始化式一致。
; 默认可变，const 才不可变 —— 可变性写在**声明处**，赋值处不重复声明。
; 作用域：workflow 体 / if / else / while / for / try / catch / retry / parallel
; 分支各是一个独立词法作用域。**允许内层同名遮蔽**：内层声明是另一个绑定，
; 出块即关闭；`for` 的循环变量同理（出循环即回收，E4028 就是它的守卫）；块内
; 引用一律取**最内层**同名绑定。这条不只是语法糖：遮蔽在 IR 里必须是两个槽位
; （`vars[]` 因此允许重名，见 IR.md §4.1），否则运行期会读到另一层的值。
VarDecl     = [ "const" ] DeclType Pattern "=" Expr ";" .
DeclType    = "auto" | BaseType .
Pattern     = Ident | "[" [ DestrElem { "," DestrElem } ] "]" .
DestrElem   = Ident | "_" .

AssignStmt  = Ident "=" Expr ";" .                         ; 目标只能是裸标识符：没有字段赋值、没有解构赋值
UpdateStmt  = Ident ( "++" | "--" ) ";" .

WhileStmt   = "while" "(" Expr ")" Block .
ForStmt     = "for" "(" ( "int" | "auto" ) Ident "=" Expr ";" Expr ";" Post ")" Block .
Post        = Ident ( "++" | "--" | "+=" IntLit | "-=" IntLit )
            | ( "++" | "--" ) Ident .                      ; 步长必须是字面量：次数要能在编译期算
BreakStmt   = "break" ";" .
ContinueStmt= "continue" ";" .

IfStmt      = "if" "(" Expr ")" Block { "else" "if" "(" Expr ")" Block } [ "else" Block ] .

MatchStmt   = "match" "(" Expr ")" "{" { MatchArm } "}" .
MatchArm    = "case" StringLit ":" Block | "default" ":" Block .

RetryStmt   = "retry" "(" IntLit [ "," "delay" "=" IntLit ] ")" Block .

TryStmt     = "try" Block { "catch" "(" CatchType Ident ")" Block } .
CatchType   = "NetworkError" | "HttpError" | "ModelError" | "ContractError"
            | "CircuitOpenError" | "Error" .

ReturnStmt  = "return" Expr ";" .
ExprStmt    = Expr ";" .

; ---- 表达式 ----
Expr        = OrExpr .
OrExpr      = AndExpr { "||" AndExpr } .
AndExpr     = EqExpr { "&&" EqExpr } .
EqExpr      = RelExpr { ( "==" | "!=" ) RelExpr } .
RelExpr     = AddExpr { ( "<" | "<=" | ">" | ">=" ) AddExpr } .
AddExpr     = MulExpr { ( "+" | "-" ) MulExpr } .
MulExpr     = UnaryExpr { ( "*" | "/" | "%" ) UnaryExpr } .
UnaryExpr   = [ "!" | "-" ] PostfixExpr .
PostfixExpr = Primary { "." Ident | "[" StringLit "]" } .

Primary     = Lit | Path | CallExpr | ParallelExpr | "(" Expr ")" .
Path        = Ident { "." Ident } .
CallExpr    = Callee [ "<" Type ">" ] "(" [ Arg { "," Arg } ] ")" .
Callee      = ( "model" | "tool" | "json" ) "." Ident .
Arg         = Ident "=" Expr .

ParallelExpr= "parallel" "{" { Stmt } RetArray "}" .
RetArray    = "return" "[" [ Expr { "," Expr } ] "]" ";" .

Lit         = IntLit | DoubleLit | MoneyLit | StringLit | "true" | "false" .
```


**注**：
1. `PostfixExpr` 的 `"[" StringLit "]"` 只允许**字符串常量**键（`json` 字段访问）；`j[expr]` 非法（`E2010`）。
2. `Callee` 只允许 3 个命名空间 ⇒ **没有一等函数**，`model.generate` 不能赋值给变量。
3. `.h` 里出现带体的 `workflow` → `E3005`；出现语句 → `E3006`；出现 `#link` → `E3017`。
4. `DeclVis` 是**可见性**，不是语法装饰：只有 `export` 的符号进符号表（§3.5）。`.hermes` 与 `.h` 都可写 `export`，但同一符号**只能写一次**（`E3012`）。惯例：`export` 写在 `.h` 的声明上，`.hermes` 的定义不重复；单文件法（没有 `.h`）时直接写在 `.hermes` 上。
5. `WorkflowSig` 的 `-> Type` **必填**（头文件没有体，没有东西可供推断）；`WorkflowDecl` 的 `-> Type` **可省**，省略时由各 `return` 推断（§3.7）。定义写了就与推断结果比对，不一致 → `E3014`。
6. `InputParam` 是**固定标签** `input`，不是普通标识符：因此不存在"参数名叫 input 的普通参数"，也不需要为此把 `input` 做成保留字（它本来就在保留命名空间里，§2.4）。`input` 可整体省略；省略时该 workflow 无输入，请求体必须是空对象 `{}`。
7. **地址位**的 `env()` 只在 `BackendDef` 右值位置合法；两个实参都必须是字符串字面量（`E3018`）。这是**静态审计**要求：不解析任何东西就能读出"这个 backend 在没设变量时去哪里"。
8. **`auth` 子句**：`backend X = <地址> auth env("KEY");` —— 密钥的来源是一个**子句**，既不是地址的一部分，也不是地址 `env(...)` 的第三个实参。它的右值**必须是 `env(名)` 单实参形式**；给了两个实参（即给了默认值）→ **`E2018`**，文案必须说清"密钥没有安全默认值：给了默认就等于允许没配也能跑，而那只会在运行期变成一个 401"。三条边界：
   - **允许与地址的变量名相同**（`backend h = env("H", "http://127.0.0.1:8080") auth env("H");` 不报错）。这不是可静态判定的错——同一个环境变量被用来装两个值，只有到了运行期解析出来才知道，语言不做这种语义检查，由运行期失败暴露。（注意地址位的 `env` 是**两**参，见第 7 条；`env("H")` 单独出现在地址位本身就是 `E3018`，与 `auth` 无关。）
   - 一个 `backend` 定义**最多一个** `auth` 子句；写第二个 → 同一个 `E2018`（不为"重复"单开错误码：两者都是"这个位置上的 `auth` 子句不合法"）。
   - 理由与第 7 条同构：**不解析任何东西就能读出这个 backend 用哪个环境变量装密钥**，而"有没有默认值"正是审计结论的差别所在——有默认值意味着"没配也能跑"。
9. `Lit` 的 `DoubleLit` 就是 §2.5 的 double 字面量（`1.5` / `1.5e2` / `1e3`），编成 IR 位模式；实现侧 token 枚举 `Tok::FLOAT` / `TY_FLOAT` 沿用旧名，但语义自 v2.0.0 起是 binary64 `double`，语言里没有 `float` 类型。


### 3.2 运算符优先级（紧 → 松）

| 级 | 运算符 | 结合 | 说明 |
|---|---|---|---|
| 1 | `.` `[]` `()` 后缀 | 左 | 字段 / 常量键 / 调用 |
| 2 | `!` `-` 一元 | 右 | 无一元 `+` |
| 3 | `*` `/` `%` | 左 | `%` 仅 `int` |
| 4 | `+` `-` | 左 | string 禁止 `+`（§4.4） |
| 5 | `<` `<=` `>` `>=` | 左 | `a<b<c` ⇒ `(a<b)<c` ⇒ 类型错 |
| 6 | `==` `!=` | 左 | 仅同类型 |
| 7 | `&&` | 左 | 两侧 `bool` |
| 8 | `\|\|` | 左 | 两侧 `bool` |

|  |  |
| --- | --- |
**无位运算、无三元**：`&` / `|` 不是合法字符（`E1001`），`a ? b : c` 解析不出来 —— `match` 取代三元。

**但赋值与 `++` / `--` 是有的**（这一点容易记反）：`auto` 是可变绑定、`const` 才不可变，`x = expr`、`x++`、`x--` 都是合法语句，编译成 `assign` 节点（`for` / `while` 的计数靠它们）。**没有**的是两样东西：① 复合赋值 `+=` / `-=` / `*=` / `/=` 作为**独立语句** —— 这些 token 词法层认得出，但语句位置不接受（`E1001`），语句里要写 `x = x + 1`；唯一的例外是 `for` 的步进子句，那里 `i += 2` / `i -= 2` 合法且推荐（步长必须是字面量，因为次数要能在编译期算出来）；② 表达式里赋值 —— 赋值只能是语句，所以 `if (x = 1)` 这类笔误在语法层就不成立（`E1001`，而不是静默地当条件用）。

### 3.3 语句形态约束

| 约束 | 理由 |
|---|---|
| `if`/`match` 是**语句**不是表达式 | 于是不存在"分支产出值"⇒ 无 φ 节点/合流值 ⇒ IR 是纯控制树，无需 SSA。要产出值就在分支里 `return` |
| `parallel { }` 体内 `return` 只允许最外层一个，且必须是数组字面量 | 元组唯一入口；其他位置 `return` → `E2011` |
| `workflow` 体必须**所有路径**都 `return` | 否则可能不返回值（`E4001`）。判定是出口可达性分析，不是"数一数有没有 return"：`match` 各臂全 `return` 且必须有 `default` 时出口不可达——黄金样例正是这个形状 |
| 同一 `try` 内子类型必须写在父类型之前 | 否则后者不可达 → `E4002`（硬错：写反的 catch 顺序几乎总是 bug，且会让专用分支的错误落到通用分支，静默降级） |
| `match` 必须有 `default` 分支 | 无 `default` 时"全不命中"路径无定义（`E4018`） |
| `retry` 的次数与 `delay` 必须是整数字面量 | 上界与成本必须编译期可算（`E2015`） |
| `precision` 必须是字符串字面量 | 同上（`E2014`） |
| **`method` 必须是 6 个字面量之一**：`"GET"` `"HEAD"` `"POST"` `"PUT"` `"PATCH"` `"DELETE"`；省略时默认 `"GET"` | 非字面量 → `E2016`；字面量但不在集合内 → `E2017`。**D-014.7**：幂等证明（§5.7）依赖"编译期知道这是只读方法"。若 `method` 可由运行期表达式决定，`retry` 包着 POST 也拦不住——证明就是假的。限定字面量后，"只读"成为编译期事实 |
| 所有内建调用的参数必须**命名**且**纯** | 求值顺序确定 + 上界可算 + trace 树不重叠（§5.1 E-3） |
| 顶层声明必须显式 `export` 才进符号表 | 默认私有。理由见 §3.5 |

### 3.4 `<T>` 与 `<` 的消歧（设计裁决 Q1）

解析器最容易写错的地方。给出**确定性规则 + 形式化理由 + 退化边界**。

#### 规则

```
R-TY-1  「类型实参」判定（四条同时成立）：
        (a) `<` 左侧的 token 序列恰好构成 Callee，即 `model.<ident>` / `tool.<ident>` / `json.<ident>`；
        (b) `<` 之后、候选 Type 之后必须紧跟 `>`；
        (c) `>` 之后必须紧跟 `(`；
        (d) 候选 Type 必须能被 Type 产生式**完整**解析。
        满足 → 类型实参列表，不存在第二种解释。

R-TY-2  其余一切**表达式上下文**中的情形：`<` 是关系运算符 LT，`>` 是 GT。
        注意作用域：`#include <std/net.h>` / `#link <std/net>` 里的 `<...>` 已由
        词法器整体吃成一个 token（§2.7），**永不到达表达式解析器**。因此 R-TY-2 的
        前提是"已经进入表达式解析"，不是"任何地方看到 `<` 就当 LT"。v3.0 把它写成
        "其余一切情形"是漏了这条作用域，v3.1 补上。

R-TY-3  Type 的形态是 `BaseType ( "[]" )*`，BaseType 为 6 个类型名或一个 Ident。
        因此 R-TY-1 最多只需 3 token 前瞻（Ident, ">", "("）。实现是 LL(3)，
        **不需要回溯、不需要 mark/reset**。

R-TY-4  词法器不产生 `>>` token（语言无移位运算），因此没有 C++ 的 `>>` 拆分问题。
        `<=` / `>=` 是合法 token，但不可能出现在类型实参内部；
        在 `Callee <` 之后读到 `<=` 时直接判 R-TY-1 失败 → 按 R-TY-2 处理。
```

#### 为什么充分（形式化理由）

> `model`/`tool`/`json` 是**保留命名空间**，不能作变量、字段名或 struct 名。因此任何形如 `model . X` 的表达式**只可能是内建被调者**，不可能是"变量 `model` 的字段 X"。而 `model.generate` / `tool.call` / `json.*` 的集合**封闭且有限**（无用户函数、无一等函数、无别名）。于是"左侧是不是内建被调者"是**词法 + 一张内建表**就能回答的问题，与作用域无关、与类型推导无关——这就是 R-TY-1(a) 可以只靠语法判定的原因。

#### 退化边界（必须知道的四条）

| # | 退化情形 | 当前可达？ | Phase 2 修补 |
|---|---|---|---|
| D1 | 用户给内建命名空间起别名（`using g = model.generate;`） | **不可达**（无别名语法） | 禁止别名指向泛型内建，或改全量回溯解析 |
| D2 | 嵌套泛型（`List<OrderInfo>`），`>` `>` 相邻需拆分 | **不可达**（无嵌套泛型类型） | 类型实参上下文内把 `>>` 拆成两个 `>`（需上下文相关词法） |
| D3 | 用户函数/用户泛型（`foo<Bar>(x)`） | **不可达**（无用户函数） | R-TY-1(a) 扩为"被调者可能是用户函数"，必须 speculative parse + 64 token 预算 |
| D4 | `a < b` 中 `a` 恰是内建被调者（`model.generate < 5`） | 可达，但**必然类型错**（内建调用无值语义） | 诊断应指向"内建调用缺少 `(`"，而不是"类型不匹配" |

#### 诊断质量要求（验收项）

对 `model.generate<Intent(x);`（漏 `>`）必须给出：

```
error[E2001]: unterminated type argument list
  --> support.hermes L12:30
   |
12 |     auto intent = model.generate<Intent(
   |                                    ^ 期望 '>'，读到 '('
   = 为什么: model.generate<T>(...) 的形态是 '<' 类型 '>' 紧接 '('。
   = 怎么改: model.generate<Intent>(prompt = "...", precision = "fast")
```

不是 "unexpected token"。

### 3.5 可见性与符号表

**规则**：顶层声明（`struct` / `backend` / `workflow`）默认**私有**；只有带 `export` 的符号进符号表。

| 概念 | 定义 |
|---|---|
| **编译单元（unit）** | root `.hermes` + 它 `#include` 的闭包（所有 `.h`）。`.hbc` 里的一个 member 就是一个 unit |
| **私有** | 只在本 unit 内可见；不进 IR 的 `exports`、不进 `.hbc` 的符号表、链接后消失 |
| **`export`** | 进符号表 ⇒ 可被 `hermes ar`/`header` 看见、可被别的 `.hbc` 通过 `#include 它的.h` + `#link 它` 使用、是死代码剥离的**根集合** |
| `export` 写两次 | `E3012`：同一符号的声明（`.h`）与定义（`.hermes`）都写 `export`。**惯例是只写在声明上**；写两次不是更保险，是让"这个符号到底由谁发布"变成两处事实 |
| `export` 的符号无人定义 | **编译期不报**，链接期必须被满足，否则 `E_LINK_UNRESOLVED`。等价于 C 的 `extern`：声明是承诺，兑现是链接期的事 |
| 私有 `struct` 重名 | 仍报 `E4014`（同 unit 内 struct 名必须唯一，不论可见性）。这不是为可见性让步的规则：IR 的类型表用**非限定名**做 `$defs` 键（§4.6），两个同名 struct 会让生成的 schema 二义，而 schema 字节进请求哈希 |

**为什么默认私有（两个具体失效模式，不是洁癖）**

1. **符号空间污染**：每个库的内部辅助类型（`_RetryState`、`_Page`）都进符号表后，两个库同时被链接就会产生命名冲突——而冲突的双方根本不该被外人看见，作者也没有动机预先给它们加前缀。
2. **死代码剥离会剥错**：剥离器必须知道"哪些是根"。若一切默认导出，则**所有**符号都是根，剥离等于不剥（`.hbc` 虚胖）；反过来若剥离器自己猜"没人用就剥"，它会把 `support.h` 声明、只被外部消费者用到的 `SupportInput` 剥掉——于是分发出去的库里没有请求契约。显式 `export` 让根集合成为**语法事实**，剥离器不需要猜。

**这条规则与 `#link` 是一对**：`export` 决定"我提供什么"，`#link` 决定"我使用什么"。两侧都是源码里的显式事实，所以 `hermes ar t` 能同时列出它们，链接器能在**不读任何源码**的前提下完成符号解析与版本校验。

### 3.6 `entry`：默认入口

| 情形 | 行为 |
|---|---|
| root unit 里恰好 1 个 `entry`，命令行没给 `--workflow` | 用它 |
| root unit 里 0 个 `entry`，没给 `--workflow` | `E3021`：列出本 unit 全部可运行的 export workflow，要求 `--workflow` 指定 |
| root unit 里 ≥2 个 `entry`，没给 `--workflow` | `E3022`：列出全部 entry 及其位置，**不猜**（"第一个声明的"是坏默认值：重排声明就会换入口，而重排是 `fmt` 允许的操作） |
| 给了 `--workflow X` | 用 X；若 X 不是本 unit 的 export workflow → `E3025` |
| `entry` 但该符号没被 `export` | `E3024`：入口必须在符号表里，否则 `hermes run` 按名字解析时根本看不到它 |

`entry` 是 workflow 签名的一部分：`.h` 与 `.hermes` 上的 `entry` 必须一致（`E3014`），`hermes header` 会把它写进生成的 `.h`。
`entry` 在非 root unit（被 `#link` 进来的库）里没有程序入口含义，**允许存在但不参与选择**——一个库可以有"建议入口"，那是文档，不是控制流。

### 3.7 返回类型：定义处推断，声明处必填

| 文件 | 规则 |
|---|---|
| `.hermes`（定义） | `-> Type` 可省略；省略时由**所有** `return` 表达式统一（unify）得到。无法统一 → `E4021`，列出每个 `return` 的类型与位置 |
| `.hermes`（定义，写了 `-> Type`） | 推断结果必须与它相同，否则 `E3014` |
| `.h`（签名） | `-> Type` **必填**。没有体就没有东西可推断，头文件不能靠猜 |

推断结果写进 IR 的 `workflows[].returns`，也是 `hermes header` 生成 `-> Type` 的来源。

`return` 表达式的类型不能是 **tuple**（`E4022`）：元组只能被立即解构，不能存储、不能跨块传递、不能进 JSON。允许从 workflow 返回元组会让"返回值"变成语言里唯一能把元组带出 `parallel` 的通道，而 `parallel` 的元组语义（§5.2）明确禁止这件事。

---

### 3.8 `extern`：声明体在 native 归档的函数 / workflow（2026-11-09 已批准）

`extern` 关键字声明"这个符号的体在别处"——具体在一条 `#link native` 指向的 C 归档里（§2.7）。它与既有的 `export`（可见性）、`#link "…"`（`.hbc` 实现）**正交**：三者都是源码里的显式事实，链接器不读源码也能核（§3.5）。

**三种"来源"的正交关系**：

| 修饰 / 指令 | 它说什么 | 体的去处 |
|---|---|---|
| `export` | "我提供这个符号"（进符号表，别的 unit 能 `#include` 它的 `.h` 并 `#link` 它的实现） | 本 unit 的 `.hermes` 体，或（workflow）本 unit 的 workflow 体 |
| `#link "models"` | "我这个 unit 的实现来自 `models.hbc`" | `.hbc`（Hermes 自己写的库） |
| `extern` + `#link native "m" from "m.h"` | "这个符号的体只在 native 归档里，别去 `.hbc` 找" | `libm.a` / `libm.so` 的 C ABI 符号 |

> 一句话：`export` 说"我提供什么"，`#link` 说"我用谁的实现"，`extern` 说"这个体由 native 提供、别去别处找"。三者可任意组合：`extern` 的 workflow 通常也 `export`（对外暴露、体在 native）；`extern` 的用户函数不必 `export`。

**`#link native` 绑定规则**（语法见 §2.7）：

- 一个符号由**声明它的那条 `#link`** 满足：`extern` 符号**只**从 `#link native` 的归档里解析，非 `extern` 的空体导出**只**从 `#link "…"` 的 `.hbc` 里解析。**两条路不互相回退**——"链的是哪份实现"是源码里的显式事实，不取决于文件系统上恰好存在什么（这正是 `#include`/`#link` 分离要保护的属性）。
- 同一符号被两处提供 → 重复定义，沿用 `E3012` 的"同一符号只能定义一次"纪律。
- 没有任何一条 `#link` 能提供 → **链接期** `E_LINK_UNRESOLVED`（沿用既有 unresolved 路径），**不留"运行期才 500"的余地**。

**无体 `extern workflow` 的六条定义（措辞当判据）**：

1. **形态**：`export extern workflow net_get(url: string) -> NetResp;`，无 `{}` 体，以 `;` 终结。`fmt` 对有体与无体两种形态都幂等（不补体、不删 `;`）。
2. **`entry` 允许**：无体 `extern workflow` 可以带 `entry`——整个 unit 的入口就是 native 实现的情形（§3.6）。
3. **exports 形状与有体一致**：`hermes header` 仍生成 `hermes_<unit>_<wf>` 声明；shim 把 C 调用转成 native 调用，**对外符号面不变**（调用方 `#include` 到的 `.h` 与有体 workflow 完全相同）。
4. **`security` / `allow` / `deny` / `with` 在无体 workflow 上硬错**：这些策略修饰语义上要求一个 Hermes 体去执行（定义见 §5.4.1）；无体 workflow 没有体，写了会让读代码的人以为生效，而运行期毫无作用。属"声明了不会被执行的策略"这一类拒绝。
5. **恒 `unbounded`**（终止性不可证）：native 体是不透明盒子（可能阻塞、循环、卡在 I/O），Hermes 在盒内没有任何 checkpoint；与含 `call_native` 的 workflow 是**同一成因**——都走 §6.4 / `IR.md` §5.3 的那条"不可定界即标 `unbounded`"的标记路径。
6. **调用方纪律**：无体 workflow 可出现在 `parallel` 分支里（与调普通 workflow 同纪律，分支不引入额外约束，§5.2）；但 **`retry` 它按幂等证明（§5.7）处理**——native 体无法静态证明幂等，故除非该符号在 `from` 头里显式标注 `idempotent = true`（仿 `tool.call` 的写法），`retry` 包着它 → `E5007`/`E5009` 硬错。

**「`extern` 不是 `extern "C"`」辨析**：`extern "C"` 管的是**链接约定**（关 name mangling、C 调用规约），而 Hermes 的 `extern` 管的是**体在别处**（"这个符号的实现不在本编译单元"）。本语言只有 C ABI 这一种链接约定，所以不需要、也不存在 `extern "C"` 这个修饰——名字里的 `extern` 表达的是"来源在 native 归档"，链接约定已经由 `#link native` 的 `from` 头（纯 C）定死。把两者混为一谈，会误以为 `extern` 在调 C++——它不调；它调的是作者写的那层 `extern "C"` 薄封装。



---

## 4. 类型系统（设计裁决 Q5）

### 4.1 类型集合（封闭，无用户定义类型）

| 类型 | 表示 | 用途与禁止 |
|---|---|---|
| `string` | UTF-8 字节串 | 通用文本 |
| `int` | **int64** | 计数、状态码、金额分值。选 64 位因为与 Go 预算账本同为 int64，避免"编译期过、运行期溢出" |
| `double` | **IEEE-754 binary64**（8 字节）；IR 常量是位模式对象 `{"k":"double","bits":"<16 位小写 hex>"}`，IR 字节里没有 JSON 浮点数字 | 分数、温度、阈值。**唯一的浮点类型，没有 32 位 `float`**；字面量 `1.5` / `1e3` / `1.5e2` 已允许（§2.5）。**禁止进入金额/预算路径**，定点量用 `cost` / `int` 微单位；`json.as_float(j)` 保留 API 原名、窄化到 `double`。**仍被拒**：`double == double` / `!=`（`E4033`）、`int`/`cost` 与 `double` 混用（无隐式提升，`E4006` / `E4008`）、`tool.call<double>`（`call_tool` 的 T 必须是 JSON 结构类型）；`call_native` 的返回类型可以是 `double`（真编译真跑已验证） |
| `bool` | — | 条件 |
| `json` | 动态 JSON | 逃逸口（§4.5） |
| `cost` | int64 @ 1e-6 USD | 金额（§4.3） |
| `T[]` | 数组 | 元素为 `BaseType`；**不支持嵌套数组**（`T[][]` → `E4003`，schema 的 `items` 只生成一层） |
| `struct S` | 具名记录 | 字段顺序进入 schema 字节（§8.3） |
| `HttpResult<T>` | 内置 `{ int status; T body; json headers; }` | 仅由 `tool.call` + `allow_status` 产生（§5.6） |
| `Tuple(T1..Tk)` | 仅由 `parallel` 产生 | **不可命名、不可存储、不可跨块传递**；只能立即解构 |

**没有**：`null` 类型（可空字段写成 `T name?`，例如 `string maybe?`，`?` 贴在字段名之后，见 §3.1 与 §4.2）、`any`（用 `json`）、具名 tuple、函数类型、`unit`（`return` 必须带值）。

### 4.2 可空性

只有 **struct 字段**可空，且 `?` 贴在字段名之后，写成 `Type name?`（例如 `string maybe?`，见 §3.1）；变量、参数、返回值都不可空。

- `auto x = s.maybe;`（`s.maybe` 取自声明为 `string maybe?` 的可空字段，取出值类型为可空 `string`）→ `E4004`："可空字段必须显式收窄"。
- Phase 1 无 `null` 字面量，故**无收窄语法**。因此规则是：**可空字段只能用于插值与 `json.encode`**，不能参与算术或作参数。
- 这条限制很小（可空字段主要表达"模型可省略此字段"），却让空指针类问题在语言层消失。Phase 2 加 `null` 字面量与 `if (x != null)`。

### 4.3 `cost`：为什么是 micro-USD 而非 8 位小数（§12.2 的偏差项）

| 项 | 值 |
|---|---|
| 表示 | int64，1 单位 = 1e-6 USD |
| 字面量 | `$` + 十进制，≤6 位小数；`$1.5` = 1,500,000 |
| 范围 | ±9.22e12 USD，远超任何预算 |
| 算术 | `cost ± cost → cost`；`cost * int → cost`；`cost / int → cost`（向零截断）；`cost + int` → `E4005` |
| 精度 | 结果不足 1 单位的余数**向零截断**，`explain` 标注（不四舍五入：四舍五入会让"最坏情况总花费"可能超预算上界） |
| 与账本 | 直接就是 ARCH §5.4 的 micro-USD，**零转换、零舍入** |

**理由**：若用 8 位小数（1e-8），每个值进预算账本时都要缩放+舍入，而这个舍入点正是 D-010.1 要消灭的漏洞。6 位对 token 单价完全够（典型 15 USD/Mtok ⇒ 每 token 15 µUSD）。若确需 8 位，应新增 `decimal`（Phase 2 任意精度），而不是把 `cost` 改成 int128。

### 4.4 运算类型规则（含刻意禁止项）

| 运算 | 允许 | 禁止（编译错 + 码） |
|---|---|---|
| `+ - * /` | `int⊗int→int`、`double⊗double→double`、`cost±cost`、`cost*int`、`cost/int` | `int + double`（`E4006`，**无隐式提升**：隐式提升与金额漏洞同源）；`string + string`（`E4007`，改用插值）；`cost + double`（`E4008`） |
| `%` | `int⊗int→int` | 其余含 double 一律 `E4009`（源码层只允许 int 取余） |
| `== !=` | 同类型；`json⊗json` | 跨类型（`E4010`）；**`double == double` / `double != double`（`E4033`）** —— 浮点精确相等的答案依赖最后一位，编译期证不出来，故**硬错**（旧版写「允许但 `W4001`」，而 `W4001` 从未实现，2026-11-10 改为硬错） |
| `< <= > >=` | `int`/`double`/`cost`/`string`（UTF-8 字节序） | `bool`/`json`/`struct`（`E4011`） |
| `! && \|\|` | `bool` | 其他（`E4012`，无 truthiness） |

**整数除法与取模语义**：`int` 的 `/`（整除）与 `%`（取模）按 **C 语义向零截断**（与 Go/实现一致），不是向下取整；除数为 `0` 是**运行期 `ContractError`**（消息分别为 `integer division by zero` / `modulo by zero`，见 `internal/engine/run.go` 的 `binop`），不是编译期错误，也不会崩溃。（旧版此处把消息写成中文，与"产品默认英文"的口径不符，按代码校准。）

**`NaN` / `Inf` 的边界（2026-11-10 裁决）**：只在**序列化边界**拒绝。IR 内部、构造、传递、比较都不拒 —— `0/0`、`Inf - Inf` 这类中间结果必须能存在，拒在中间等于把这些表达式本身禁掉；写出去的那一刻才拒（进结果 JSON / 进 trace 时是运行期错误，带**位置**与改法，实现见 `cmd/hermes/jsonboundary.go`）。判据是**位置**不是值：`tests/guard/nonfinite_boundary_test.go` 扫全仓，`math.IsNaN` / `math.IsInf` 只许出现在那一个文件。

### 4.5 `json` 逃逸口的静态/动态边界

| 允许 | 语义 |
|---|---|
| `auto j = input;` | `input` 的类型**恒为**签名里声明的 `S`，因此 `j` 是 struct、`j.field` 是**静态检查**的字段访问。要动态视图必须显式 `json.encode` 再 `json.decode`——没有隐式通道。v3.0 的"未声明 `input` 即 `json`"已废除（`E3023`） |
| `j.field` / `j["field"]` | 结果仍是 `json`；运行时不存在 → `ContractError` |
| `json.encode(x)` | 任意可编码类型 → `string`（canonical JSON，规则见 ARCH §5.3） |
| `json.decode(s)` | `string` → `json`；非法 → `ContractError` |
| `json.as_int/ as_string/as_bool/as_float(j)` | 显式窄化，失败 → `ContractError`（带 `expect`/`got`）；`as_float` **有意保留 API 原名**，窄化到 `double`（cast 节点的 `to` 是 `"double"`） |
| 插值 `{j}` | 运行时是 string → **原样文本**（不加引号）；其余 → canonical JSON 文本 |

| 禁止 | 原因 |
|---|---|
| `json` 参与算术 | 类型不安全，必须显式 `json.as_int` |
| `json` 作 `catch` 类型 | 无意义 |
| 隐式 `json → string` | 必须 `json.as_string` 或插值（插值是唯一的隐式通道，且只用于文本生产） |

**这条边界的意义**：程序里**只有一个地方**能引入运行时类型错误——`json.as_*` 与 `json` 字段访问。每处都在 IR 里标记为 `dyn_site`、trace 里也标记，所以"线上那个 ContractError 从哪来"永远一眼可定位。

### 4.6 struct → JSON Schema 映射表（Q5 后半）

| HermesScript | 生成的 JSON Schema | 备注 |
|---|---|---|
| `string` | `{"type":"string"}` | |
| `int` | `{"type":"integer"}` | **不是** `number`：`grammar.py` 的 `_INT` 与 `_NUM` 是两个节点 |
| `double` | `{"type":"number"}` | 唯一的浮点类型（IEEE-754 binary64），没有 32 位 `float` |
| `bool` | `{"type":"boolean"}` | |
| `cost` | `{"type":"integer","x-hermes-unit":"micro_usd"}` | 不用 string（模型会输出 `"1.20"`/`"￥1.2"` 变体）；不用 number（精度）。`x-` 扩展键不被 `grammar.py` 当作已知关键字，故不会触发它的拒绝逻辑 |
| `json` | `{}`（any） | 若后端不接受空 schema，降级为 7 类型 union（§8.4） |
| `T[]` | `{"type":"array","items":<T>}` | |
| 嵌套 `struct S` | `{"$ref":"#/$defs/S"}`，`S` 挂在根 `$defs` | `grammar.py` 只支持**本地** `$def` 引用 |
| `T name?`（如 `string maybe?`）| `{"anyOf":[{"type":T},{"type":"null"}]}`，且字段名**仍进 `required`** | OpenAI strict 与 `to_strict_json_schema` 的形状；把可选字段移出 `required` 会被 OpenAI 拒绝 |
| `@enum("a","b")`（仅 `string`） | `{"type":"string","enum":["a","b"]}` | §4.7 |
| `@pattern("^INV-[0-9]{6}$")`（仅 `string`） | `{"type":"string","pattern":"..."}` | 走 `internal/schema/pattern.go` 的 RE2 子集翻译（ARCH §5.6 的差异归一仍有效） |
| `@max/@min/@maxLen/@format` | **不支持 → `E4013`** | `grammar.py` 明确拒绝这些关键字；生成一个上游会拒的 schema 比不生成更坏（build 绿、运行红） |

**根 schema 与 `$defs` 命名（四条）**

**①** 每个 `model.generate<T>` 的 `<T>` 生成一份完整 schema 文档，作为 `response_format` 发出。三种情形要分清：

| 写法 | 发 `response_format` | 返回类型 | 诊断 |
|---|---|---|---|
| `model.generate<Intent>(...)` | 是，`schema(Intent)` | `Intent` | — |
| `model.generate<json>(...)` | 否（`{}` 不是有约束的 schema） | `json` | `W4002`：无输出结构约束， 做事后校验 |
| `model.generate(...)`（无类型实参） | 否 | **`string`**（原始文本，引擎不做任何解析） | `W4002` |

无类型实参返回 `string` 而不是 `json` 是刻意的：没有 schema 时**没有任何理由相信输出是合法 JSON**，把它标成 `json` 会让每个字段访问都变成运行期 `ContractError`，而那正是强类型要消灭的东西。想要动态视图就写 `<json>`，那是显式声明"我知道它可能不是 JSON"。

**②** `$defs` 键 = struct **非限定名**。

**③** 两个不同文件的 struct 同名 → **编译错 `E4014`**，给出两处定义位置并建议改名。**不自动加前缀**：schema 字节进请求哈希，自动改名会让"改 A 文件导致 B 工作流请求哈希变化"，同时破坏缓存与可重现性。

**④** `$defs` 键顺序 = 类型表 id 升序（声明顺序）；字段顺序 = 声明顺序。

### 4.7 注解（唯一的元数据通道）

```h
export struct Invoice {
    string id;                              // → {"type":"string"}
    string currency @enum("USD","CNY");
    string code     @pattern("^INV-[0-9]{6}$");
}
```

注解写在**字段名之后、分号之前**（`Field = Type Ident ["?"] {Annotation} ";"`，§3.1）。v3.0 的示例误写成 `string currency; @enum(...);`（分号在注解前），与产生式矛盾——已按产生式修正：一个字段只有一个终结分号，`fmt` 才能在不理解注解的情况下安全重排。

注解只能出现在字段上、只能作用于 `string`（`@enum`/`@pattern`）、参数只能是字面量。**注解不改变类型**，只改变生成的 schema。注解集合封闭（未知注解 → `E4015` 并列出全部合法注解）。

### 4.8 `input` 的类型就是 HTTP 请求契约

`hermes serve` 的 API 契约不是另写一份 OpenAPI 文档，而是**从 `input` 的类型生成**：

| 项 | 规定 |
|---|---|
| 请求体 | `SupportInput` 的**实例本身**：`{"text":"...","user_id":"u_1"}`。**不**嵌在 `"input"` 键下 |
| 生成的 schema | 与 §4.6 完全同一套映射与同一份字节稳定性规则（§8.3）；根 schema = `schema(SupportInput)` |
| `required` | 所有**非** `T?` 字段。可选字段仍进 `required`、类型写成 `anyOf[T,null]`（§4.6） |
| 无 `input` 参数 | 请求体必须是 `{}`；非空 → 运行期 `ContractError`（HTTP 400） |
| `input: json` | 允许（逃逸口），请求体任意 JSON，但打 `W5005`："入口没有结构契约，`hermes serve` 无法为你生成文档，也无法在入口挡住畸形请求" |
| 生成时机 | **编译期**，随 IR 一起落进 `.hbc` 的 `schemas`。运行期只读不算 |

**为什么不嵌一层 `{"input": {...}}`**：`hermes run --input file.json` 读的文件与 HTTP 请求体必须**字节同形**。若 CLI 读 `{"text":...}` 而 HTTP 收 `{"input":{"text":...}}`，则"本地测试通过"与"线上可用"验的不是同一个东西——而 `input.json` 与请求体同形，是能用同一个 fixture 跑通两条路径的唯一方式。

**为什么在编译期生成而不是运行期**：运行期生成意味着 `serve` 用来校验的 schema 与它对外宣称的文档来自两条代码路径，二者会漂移；而漂移的表现是"文档说必填、实际不校验"，这比没有文档更坏。编译期生成让二者是**同一份字节**。

---

## 5. 求值语义

### 5.1 基本规则

| 规则 | 内容 |
|---|---|
| E-1 | 语句按出现顺序求值。变量声明在初始化表达式求值**完成后**绑定名字 |
| E-2 | 参数求值顺序 = **签名声明顺序**（不是书写顺序）。命名参数可乱序书写，但求值按声明顺序 ⇒ trace 里同一次调用的 span 顺序在所有调用点一致 |
| E-3 | **参数表达式必须纯**（`E5001`）：参数位置不允许 `model.generate`/`tool.call`/`json.decode` |
| E-4 | `if`/`match` 恰好执行一条分支；条件必须 `bool` |
| E-5 | `retry(n, delay=D) { B }` 最多执行 `n+1` 次 |
| E-6 | `try { B } catch (T e) { C }` 至多匹配一个 catch（按书写顺序，第一个类型匹配者胜出） |
| E-7 | `parallel { ... return [e1..ek]; }` 值为长度 k 的元组，与调用侧 `auto [a1..ak]` **按位置**配对 |

**E-3 的理由**：参数里的嵌套调用会让三件事同时失去确定性——(a) 预算预留顺序（父调用的预留必须发生在其依赖的子调用之前，嵌套则无法在预留前知道要预留多少）；(b) trace 树（父 span 早于子 span 开始，但父请求体依赖子结果 ⇒ 区间重叠，时间线失去意义）；(c) 静态上界（"一次调用"内部还藏调用）。**所以先把子调用绑定出来（`auto sub = tool.call(...);`），再作为参数传进去。**

### 5.2 `parallel` 完整语义（设计裁决 Q3）

#### 作用域
- 独立词法作用域；体内声明的名字出块即关闭（`E5002`）。
- 体外变量在体内**可见且只读**——这正是 `parallel` 不需要锁、且 §6 的并发上界可证的全部理由。
- **只读已由编译期硬错 `E5010` 强制**：分支里对体外绑定的 `x++` / `x = …` 一律拒绝，无迁移期警告、无 opt-in 开关。运行期每个分支拿到的是父作用域的**副本**（`runBranches` / `cloneFrames`），这类写入落在副本上、出分支即丢弃，恒等于零效果；在强制之前它是「不报错 + 不生效」的静默失效，现在则是编译期拒。理由与「为什么不提供开关」见 `KNOWN-GAPS.md` 的决定记录。这一条与 `max_steps = unlimited` 的 `W2004` 不同：那是明说了代价的合法写法，写体外绑定则是**可证无效**。
- 判据落在**源码 AST 的类型检查**（`native/cpp/check_type.cpp` 的 `parallel_`），**不在 codegen**：以进入**最内层** `parallel` 时的作用域水位为界，写目标绑定的下标小于水位就报错；分支自己声明的绑定（含嵌套内层分支自己声明的）可写。嵌套只看最内层——内层分支同样拿到外层分支作用域的副本，所以「外层分支里声明的名字」对内层也是体外绑定。**读体外变量继续合法**，那才是 §5.2 真正成立的部分。
- 为什么不能在 IR / codegen 上判：路线 A/B 内联会给被内联的函数体合成指向 `__hm_inline_r_N` 的 `assign` 节点；在 IR 上判「目标必须声明在本分支内」，会把 `auto v = pick(3);`（`pick` 是 if/return 决策树）这种用户一行赋值都没写的正常源码判错。源码 AST 里 `pick(3)` 只是 `CallExpr`，`assign` 还不存在。

#### 元组配对
- `parallel` 值类型 = `Tuple(T1..Tk)`，`Ti` 为体内 `return` 数组第 i 元素类型。
- `auto [a1..am] = parallel {...}`：**`m` 必须等于 `k`**，否则 `E5003`（编译期——两个长度都是静态常量）。
- `k == 0`（`return [];`）合法，调用侧须 `auto [] = ...`。
- 丢弃某值写 `_`：`auto [order, _] = parallel {...}`。

#### 并发上限
三者最小值，前两个是编译期常量：

```
effective_concurrency = min( k,                     // 静态分支数
                             backend.max_parallel,  // backend 声明，默认 8
                             policy.bulkhead )      // 运行期配置
```

`k` 静态 ⇒ §6 的 `max_concurrency` 上界可算。`k > backend.max_parallel` 时编译期 `W5001`（"分支数超过 backend 并发上限，实际并发 N"）——**不报错**，工作流仍正确，只是部分串行化。

#### 错误传播（这条决定 trace 形状与预算行为）

**快速失败 + 确定性选择 + 全部收尸。**

1. 任一分支失败 → 立即 `cancel` 其余分支的 `context`。
2. 等待**所有**分支真正结束（不许"抛错就返回"，否则 goroutine/子进程泄漏，违反 ARCH R-NF-04）。
3. 抛出的错误 = **失败分支中索引最小者**；其余失败挂 `.suppressed[]`。
4. 被取消的分支 span = `cancelled`（不是 `failed`，也不是 `ok`）。
5. 同时有失败与取消时，`parallel` 节点 span = `failed`，归因取索引最小失败分支。

**为什么按索引而非到达顺序**：到达顺序依赖调度 ⇒ 同一输入两次运行会报不同的错 ⇒ replay 与 live 不一致，trace 的 `failure` 字段失去可比性。按索引是纯函数。代价是"可能报一个不是根因的错"，用 `suppressed[]` 弥补——审计者能看到全部失败。

#### 预算行为（与 ARCH INV-1 的接缝）

`parallel` 在**启动任何分支前**，把 k 个分支的估算上界**一次性全部预留**（all-or-nothing）。任一预留失败 → 一个分支都不启动，抛 `BudgetError`。

- 代价：预留量 = 各分支上界之和，实际用量可能远小于此（短分支提前完成）；差额在 `Settle` 归还。
- 为什么必须原子：否则出现"分支 1 已在跑、分支 2 预留失败"的中间态；要么杀掉分支 1（浪费已花的钱），要么让分支 2 无预留执行（违反 INV-1）。两者都不可接受。

#### 嵌套

| 项 | 规则 |
|---|---|
| 允许嵌套 | 是 |
| 深度上限 | **4**（`E5004`） |
| 静态分支积上限 | **256**（`E5005`）——所有路径上 `k` 之积的最大值。它同时是 `max_concurrency` 上界与预留扇出上界，必须有硬顶 |
| `parallel` 内 `retry` | 允许；幂等证明（§5.7）跨层级生效 |
| `retry` 内 `parallel` | 允许；此时 `parallel` 的**每个分支**都要满足幂等要求（整块会重入） |


### 5.3 `if` / `match`

- `if` 条件必须 `bool`；`else if` 链按顺序求值，首个为真者执行。
- `match` 主表达式的类型决定臂的合法性：允许 **`string` 或 `int`** 作主表达式（其他类型 → `E4019`）；`json` 主语虽能写出，但臂类型一致性不校验，登记为已知松点（§12.1 L10）；臂为字符串或整数字面量，必须与主语同类型（`E4019`）；`default` 必须有（`E4018`）；重复字面量 → `E4020`。
- `match` 臂内必须 `return`（因为 `match` 是语句且不产出值）。

### 5.4 `try` / `catch`

- 从内向外匹配；内层已捕获的错误不会传到外层。
- **catch 体内允许 `return`**（v3.1 修订，理由见下）；也允许变量声明 / `if` / `match` / `retry` / `parallel` / 嵌套 `try`。
- 捕获后若 catch 体没有 `return`，执行继续到 `try` 语句之后。
- `catch (T e)` 里的 `e` 是一个**只读**结构值，字段按 §7.1 静态检查（`e.status` 只在 `HttpError` 上有；写成 `NetworkError` 上访问 `status` 就是 `E4016`）。

**v3.1 撤销了 v3.0 的「catch 体内不能 return」。** v3.0 的理由是"catch 的职责是恢复；允许 `return` 会让控制流出现第二出口，trace 里 `try` 节点的父子关系与实际执行不符"。这个理由站不住：

1. 同一套论证会让 `if`/`match` 分支里也不能 `return`——它们同样"只执行一条路径"。分支能 return，catch 就能 return，这不是新问题。
2. **"降级答案"是最常见的恢复形式**：`catch (HttpError e) { if (e.status == 404) { return "订单不存在"; } }`。禁止它，等于要求作者在 catch 里"无值恢复"；而语言没有可变绑定、也没有从块里带值出来的语法，恢复出来的东西无处安放——结果是**错误处理根本写不出来**。这不是严格，是缺功能。
3. 对 trace 无害：未被执行的 `return` 节点不产生 span，与未命中的 `if` 分支完全同构。`max_steps` 本来就是**上界**（§6.3），从不要求等于实际 span 数。

**剩下的要求**：catch 里的 `return` 类型必须与 workflow 返回类型统一（`E4021`）；catch 体的 `return` 与 body 的 `return` 一起参与 §3.7 的推断。

### 5.4.1 `fallback`：错误边界（不是异常）

```hermes
auto result = model.generate(prompt = "hi") fallback { return "降级回复"; };
auto data   = tool.call(url = "api")       fallback { auto d = cache.read(); return d; };
tool.call(url = "api")                     fallback { log.warn("API failed"); };
```

- **语法**：`postfix-expr "fallback" block`。`fallback` 是**保留字**（§2.4），因此 `auto fallback = 1;` 在 2.0.0 起是语法错（`E1001`）。这是本版的**破坏性变更**，也是 2.0.0 存在的理由之一。
- **语义**：先求主表达式；**失败**（C ABI 状态码 `err != 0`）时改跑降级块，取块里 `return` 的值作为整个表达式的值。主表达式成功时**降级块不执行**（不白花预算）。
- **零栈展开**：失败一直是**返回值**，不是 `throw`。前端本来就以 `-fno-exceptions` 编译，这里没有任何一处依赖异常机制 —— `fallback` 是控制流，不是异常。
- **类型**：整个表达式的类型 = 主表达式的类型。降级块里每个 `return e` 的 `e` 必须与之同类型，否则 `E4025`。理由：不同的话，降级路径只在**真的出错那天**才暴露类型错误 —— 那正是错误边界最不该有的性质（正常路径测试全绿，线上第一次超时才发现降级返回了 `string` 而调用方按 `int` 用）。
- **降级块里可以读外部变量**，但**不许给 `const` 绑定重新赋值**（`E4026`）。非 `const` 的普通绑定可以写。
- **块可以没有 `return`**（第三条示例：只要副作用的形态）。此时表达式本身没有值；用在值位置会得到 `json` 的 `null`。
- **预算**：`max_spans` / 模型调用 / 工具调用按**主表达式 + 降级块相加**计，不取 `max` —— 主表达式可能已经花掉一部分预算才失败，降级块再跑一遍。取 `max` 会低估上界，而低估的上界会让运行期把合法行为判成超界（§6.1）。

**与 `try` / `catch` 的区别**（两者都存在，不要互相替代）：

| | `fallback` | `try` / `catch` |
|---|---|---|
| 形态 | **表达式**，产出一个值 | **语句**，不产出值 |
| 粒度 | 一行调用 + 它的降级路径 | 一整段代码 + 按错误**类型**分派 |
| 分派 | 只有一条降级路径 | 多个 `catch`，按错误层级匹配（§7.2） |
| 绑定 | 不绑定错误对象 | `catch (T e)` 绑定只读错误值，可按字段分支 |
| 典型用法 | "这个调用失败了就用这个值继续" | "按错误类型做不同的恢复" |

`fallback` 内**不绑定错误对象**：要在降级路径里看状态码，用 `try` / `catch`。这条限制是有意的 —— 一旦 `fallback` 也能读错误，它就会长成第二个 `catch`，两套错误分派并存比只有一套更难讲清。

这不表示 AOT 产物里没有错误对象：产物内部有一个统一的标准错误对象，供 `try` / `catch` / `fallback` / `retry` 的判定与清错使用（规范见 `docs/AOT-ERROR-OBJECT.md`），只是**不向降级块暴露绑定**；要在降级路径里看错误字段，仍请用 `try` / `catch`。

### 5.5 `retry`

```
attempt 1 → 失败 → sleep(delay_1) → attempt 2 → ... → attempt n+1
```

- 退避公式（**全抖动**，与 ARCH §5.7 的 `retry.go` 一致）：

```
cap_i   = min(delay_cap_ms, delay × 2^(i-1))     // delay_cap_ms 来自 policy，默认 8000
sleep_i = uniform_int( 0, cap_i )                // 由引擎的可注入 RNG 产生（可复现）
下界     = max(sleep_i, Retry-After 头 × 1000)    // 上游明确要求等更久时听上游
```

- 重试耗尽后抛**最后一次**的错误，并在其 detail 里带 `attempts = n+1`。
- 只重试"可重试"类错误：`NetworkError`、`HttpError`(5xx/429/503/529)、`ModelError`(输出不合法)。`ContractError`/`CircuitOpenError` 不重试（重试不会改变结果）。
- **`retry` 块的幂等前提由编译期证明**（§5.7）。

### 5.6 `tool.call` 的契约

| 参数 | 必填 | 语义 |
|---|---|---|
| `url` | 是 | 字符串（可插值） |
| `method` | 否（默认 `"GET"`） | **必须是 6 个字面量之一**：`"GET"`/`"HEAD"`/`"POST"`/`"PUT"`/`"PATCH"`/`"DELETE"`。非字面量 → `E2016`，字面量但不在集合内 → `E2017`（§3.3、D-014.7） |
| `timeout` | 否 | 整数字面量，毫秒；默认取 policy |
| `allow_status` | 否 | 整数字面量数组，如 `[404, 409]` |
| `idempotent` | 否 | `true`/`false` 字面量 |
| `idem_key` | 否 | 字符串（可插值），**必须纯**（§5.7） |
| `header` | 否 | `json` 或 struct（生成 header） |
| `body` | 否 | struct/`json`/`string` |

**返回类型（这条是 D-010.2 的核心）**：

| 情形 | 返回类型 | 非 2xx 的行为 |
|---|---|---|
| 无 `allow_status`（默认） | `T`（`<T>` 指定；未指定则为 `json`） | **抛 `HttpError`**，不返回业务值 |
| 有 `allow_status = [404]` | `HttpResult<T>` = `{ int status; T body; json headers; }` | 列表内的状态码作为正常返回（`status` = 该码，`body` 尽力解析；解析失败为 `json` 空对象）；列表外仍抛 `HttpError` |

**因此"订单不存在"必须写成捕获，不能写成字段比较**：

```h
try {
    auto order = tool.call<OrderInfo>(url = "...", method = "GET");
    auto msg = "订单 {order.order_id} 已找到";
} catch (HttpError e) {
    if (e.status == 404) { auto msg = "订单不存在，请核对订单号"; }
}
```

`if (order.status == 404)` 在默认契约下**永远不成立**：404 时 `order` 根本没被绑定，执行流在 `tool.call` 那一行就跳走了。这也是 `HttpError` **必须**携带 `status` 字段的原因（§7.1）——否则"订单不存在"这个业务上最常见、最需要给用户一句人话的分支，在语言里根本写不出来。

编译器对 `if (order.status == 404)` 只能发**启发式**警告 `W5004`，不能报错：它无法区分"业务字段恰好叫 `status`，且在 200 响应里等于 404"（罕见但类型上完全合法）与"作者误以为拿到了 HTTP 状态码"。这条松点如实登记在 §12.1 L3，不假装能静态判定。

**为什么不做"自动把 404 当正常返回"**：那会让"HTTP 层失败"和"业务层返回"混在一个类型里，调用方永远无法确定 `order` 是"订单"还是"错误响应"。用类型区分（`T` vs `HttpResult<T>`）是唯一能让调用点自解释的做法。

### 5.7 `retry` 幂等证明（设计裁决 Q8）

**位置**：类型检查之后、codegen 之前的独立 pass（`native/cpp/check_idem.cpp`）。它需要类型信息（区分 `tool.call` 与其他调用）和词法嵌套关系（`retry` 块），但不需要 IR。

**规则**：对**每一个** `retry` 节点 R，R 子树内（含更深层的 `retry`）的**每一个** `tool.call` 必须满足三者之一：

| # | 条件 | 引擎行为 |
|---|---|---|
| ① | `idempotent = true` | 原样重发 |
| ② | `idem_key = <纯表达式>` | 每次注入同名 `Idempotency-Key` 头 |
| ③ | `method` ∈ {`GET`, `HEAD`} | 只读，天然幂等 |

不满足 → `E5007`（§11.2 ④）。

**`idem_key` 的表达式约束**：必须是**纯表达式**（`Pure(e)` 可判定）：

```
Pure(lit)                    = true
Pure(input.path)             = true        // input 在一次 run 内固定
Pure(x)  where x is a declared name whose init is Pure  = true
Pure(model.generate(...))    = false       // 每次调用结果可能不同
Pure(tool.call(...))         = false       // 响应可能不同
Pure(arith/cmp/interp of Pure args) = true
Pure(其他)                    = false
```

`idem_key` 不纯 → `E5009`："idempotency key must be derived from input; if it comes from model output or a tool response, the key changes on every retry and deduplication fails"。

**为什么放在编译期**：运行期无法知道"这个调用点了能不能安全重试"——它能拿到的只有 URL 和方法字符串，而服务端的幂等性是一个**声明性事实**，只能由作者声明或由方法语义推定。语言能拿到静态调用点，就该在 build 时判死，让"重试导致重复扣款"这类事故不可能上线（R-CB-06 的编译期化）。

`model.generate` 在 retry 块内**视为可重入**（有预算预留保护，重试计费可核销），无需声明。

**实现约束（2.0.0 落地）**：`check_idempotency()` 挂在 `api.cpp::run_parse` 的类型检查之后，所有 `hm_parse_*` / `hm_codegen_*` 入口共用 —— 少挂一个入口就等于给「换个子命令绕过重复扣款检查」留一条路。`Pure(e)` 认不出来的形状一律判不纯；同名变量若在任意一处绑到不纯初始化式，该名字整体判不纯（作用域信息不足时宁可误报，不可漏过）。`tool.call` 目前不允许出现在用户函数体内（codegen 明确拒绝），因此证明是过程内的，不需要跨调用图。

**D-014.7 的 `E2016` / `E2017` 也在本 pass 判**：语法器看到的是一串通用实参、不知道哪个是 `tool.call`，而「method 必须是编译期已知的字面量」正是 `readonly` 这条推论的前提。附录 A 把两码登记为语法码是历史分类，判据落在真正读 `method` 的这一层。

与 IR 的对应关系：`readonly` / `idem_true` / `idem_key` 三种证明各自落到 `tools[].idem.mode`；**没有任何证明的调用发 `none`**，绝不回落到 `readonly`（IR.md §3.4）。`idem_key` 同时发 `tools[].idem.key` 的结构化片段与 `call_tool.args.idem_key` 的表达式节点，引擎用后者注入 `Idempotency-Key` 头 —— 只有标签没有注入，等于证明是假的。

---

## 6. 编译期静态上界（设计裁决 Q7）

控制流是结构化树（`for` / `while` 也只有单一出口、`parallel` 是定长 fork），所以**没有不可判定的图** ⇒ 以下四个量是**可判定**的，不是估算 ——但"可判定"的前提是树里没有"次数不可静态定界"的构造。这个前提不是总能满足：`while`、步进推不出次数的 `for`、递归函数的 `call_fn`、以及 `max_steps = unlimited` 的 agent 都会让它失效，命中任意一个，workflow 就标 `bounds.unbounded: true`（见 §6.4）。§6.1 给的是 v1 那四个量的代数；agent 与"不可定界"的构造在 §6.4 与 `IR.md` §5.1 补齐。

> **整数算术的边界（与 §4.4 一致）**：`int` 的 `/` 与 `%` 按 C 语义向零截断，除数为 `0` 是运行期 `ContractError`，不进入静态上界核算。

### 6.1 递归定义（规范）

记 `M`/`T`/`S`/`W` 为"最大模型调用数 / 最大工具调用数 / 最大执行步数 / 最大并发分支数"：

```
M(call_model)=1  T=0  S=1        M(call_tool)=0  T=1  S=1
S(vardecl)=1  S(return)=1  其他表达式：M=T=S=0

seq        M=ΣM(c)   T=ΣT(c)   S=ΣS(c)   W=max W(c)
if/match   M=max     T=max     S=max     W=max        （分支互斥）
try        M=M(body)+max M(catch_i)   S=S(body)+max S(catch_i)
           W=max(W(body), max W(catch_i))
retry(n)   M=(n+1)×M(B)  T=(n+1)×T(B)  S=(n+1)×S(B)  W=W(B)   （顺序重入，不加并发）
parallel   M=ΣM(Bi)  T=ΣT(Bi)  S=ΣS(Bi)  W=ΣW(Bi)
```

`workflow` 上界 = body 上界。块缺失 `return` 时仍按上述算——上界只需**不小于**真值。

**上面这张表是 v1 的七种构造，不含 `for` / `while` / agent / `call_fn`**：它们的项写在 `IR.md` §5.1（规范副本在那边，这里不复制——同一套式子写两处，早晚会有一处停止更新）。三条口径记住就不容易错：① 循环按"**一次迭代 × 次数**"记，次数推不出来就不乘，改标 `bounds.unbounded`；② agent 的项按 `(1 + k) × max_steps`，`unlimited` 时按一次迭代记并标 `unbounded`（§6.4）；③ 标了 `unbounded` 的 workflow，这些数字**不是上界**，别拿去做预算。

### 6.2 上界的四个去处

| 上界 | 去处 | 违反时 |
|---|---|---|
| `max_model_calls` | 运行期预算预留的扇出上界（`parallel` 预留 = 分支 `M` × 单次 `max_tokens` 上界） | — |
| `max_tool_calls` | 执行器并发子进程上界 ⇒ 决定 cgo 线程预算（ARCH §6.2） | — |
| `max_steps` | trace 的 span 数上界 ⇒ trace 字节上界；可静态判定 `max_bytes` 是否够 | 不够时 `W6001` |
| `max_concurrency` | 反压窗口静态上界 `min(上界, backend.max_parallel, policy.bulkhead)` | 不可能超过 |

全部写入 `.hbc` 成员元数据（`bounds`），`hermes run` 读出直接用，**运行期不重算**。

### 6.3 哪一条不是"可判定"而是"上界"

> **prompt 字节长度（及由此推出的 token 数）不是可判定量，只能是上界。**
> 因为插值 `{input.text}` 的长度由运行时输入决定。

`economics.prompt_bytes_upper` 基于**声明的输入规模上限** `assumptions.input_bytes_upper`（默认 1 MiB）计算。算法、"为什么它是上界而非预测"的论证、以及 `cost_upper_micro_usd` 的公式见 `docs/IR.md` §6。

**一句话**：本语言能给出的是**成本上界**（可证），不是**成本预测**（不可证）。宣传时必须写"上界"。用它做两件事是安全的：① 判断"这个工作流**理论上能不能**装进预算"；② 作为 `parallel` 预留的扇出上界。用它做"这次大概花多少钱"是错的。

### 6.4 `unlimited`：明说"没有编译期上界"

`unlimited` 是保留字（§2.4），**唯一合法位置**是 `model.agent` 的 `max_steps`：

```hermes
auto answer = model.agent(
    goal = "查订单 {intent.order_id} 的状态，然后告诉用户能不能退款",
    tools = ["lookup_order"],
    max_steps = unlimited        // 想跑到完成，而不是跑到第 4 步
);
```

它**编得出来**，但**必须**带一条编译期警告 `W2004`。任何其他位置都是错误，不是警告：

| 写法 | 结果 |
|---|---|
| `max_steps = unlimited` | ✅ 编译通过 + `W2004`（不阻塞构建） |
| `max_steps = unlimited` 与常量同时写 | ❌ 硬错（上界要么可证明、要么明说没有，两句话不能同时成立） |
| `retry(unlimited)` | ❌ `E2015` 硬错（见下） |
| `auto x = unlimited;` 等任何**值**位置 | ❌ `E4024` 硬错（类型检查先报；codegen 的 `gen_expr` 另有一道独立的拒绝，两处都不许它通过） |

#### 为什么是警告而不是错误

因为它**编得出来**：`unlimited` 不是"不支持的构造"，它是一句可以兑现的话 —— 作者确实想跑到完成。但接受它要付出两条**编译期保证**，这两条正是本语言存在的理由，所以必须说出来，不能静默：

1. **`max_spans` 不再是 trace 长度的可证明上界。** agent 跑几步由模型决定，编译期看不见。§6.2 里"`max_steps` ⇒ trace 字节上界 ⇒ 可静态判定 `max_bytes` 是否够（`W6001`）"这条判定因此失效：`trace_bytes_upper` 仍然算得出一个数，但它不再是上界，只是一个占位。
2. **预算不能静态预留。** `economics` 里的 `model_calls_upper` / `cost_upper_micro_usd` 建立在"模型调用次数可算"之上（§6.3）。`unlimited` 之后调用次数是"至多 = 步数"，而步数未知，于是"预算是预留而不是事后统计"这句话在这个 workflow 上不再成立。

反过来，**报错**在这里是错的：报错等于宣称"这个程序没有意义"，而它的意义很清楚；同时它会把唯一诚实的选择（承认没有上界）逼回到"编一个大数字"——那才是真正的谎。

#### 产物形状（这句是给读 IR 的人看的）

- agent 规格里**没有 `max_steps` 键**（缺席 = 没有静态上界）。不写 `null`、更不写一个大数：`null` 要给每个消费者多一种形状，大数则会被 `max_spans ≤ 64 × max_steps + 1` 与复算一致性检查判成漂移 —— 一个假的上界等于给整份产物埋了个假前提。
- `bounds.unbounded = true`。它与 `while` 语句、递归 `call_fn` 走的是**同一条**标记路径（四种成因列全在 `IR.md` §5.3）：不是装饰，而是告诉读的人"bounds 里那些数字不全是可证明的上界"。
- `max_spans` / `max_model_calls` / `max_tool_calls` 只按**一次迭代**记（`span = 1 + k`、`model = 1`、`tool = k`，`k = max(1, max_parallel_tools)`），与 `while` 语句"只按一次迭代记、绝不乘一个假想上限"同一个先例。**不要**用它们做预算或容量规划。
- `hermes explain` 对这个 workflow 的 `max_steps` / `max_spans` 显示 **unknown**（文本模式）与 `"unbounded": true`（`--json`），不给数字。


> **运行期还有一道保险，但它不是上界。** 运行期另有一道硬性迭代上限兜底，所以 `unlimited` 不会真的"无限跑"；这个值不写进 IR，也**不能**当作预算依据。
> **状态**：这道保险目前只有设计、没有实现 —— 现在 `hermes run` 遇到没有静态上界的 agent 会直接拒绝执行（`max_steps 必须 > 0`）。落地之前，本段描述的是运行期契约而不是可观察行为，缺口记在 `KNOWN-GAPS.md`「运行期 agent 硬上限」。

#### 为什么 `retry(unlimited)` 是**错误**（故意与 `max_steps` 不对称）

两条理由，都不是"顺手不一致"：

1. **重试次数是成本的乘数，不是次数本身。** `SP(retry) = 1 + (n+1) × SP(body)`（`IR.md` §5.1）。`n` 无界时这个式子没有值，`max_spans` / `max_model_calls` 同时失去意义 —— 而它们是"预算可以预留"的全部依据。换句话说：`max_steps` 增大是"这次可能多花几步"，`retry` 增大是"整棵子树乘 17 倍"，两者的量纲不同。
2. **无界重试在任何语义下都不是想要的结果。** 重试解决的是**暂时性失败**；"永远失败"重试多少次还是失败，只是把钱花得更彻底。真要"跑到成功"，条件必须写出来 —— 写成 `while`。

所以 `retry(unlimited)` 报 `E2015`，文案直接给出替代写法：

```hermes
auto attempt = 0;
while (attempt < 3) {          // 条件里写清停止依据
    ...
    attempt++;
}
```

**一句话**：`unlimited` 合法，因为那条路径上运行期有硬上限、且作者要的就是"跑到完成"；`retry` 不合法，因为无界乘数让整份产物失去上界，而"无界重试"本身不成立。

---

## 7. 内置错误类型层级（设计裁决 Q9）

### 7.1 层级（封闭、名义、单一根）

```
Error                      ← 唯一根；catch (Error e) 兜住全部可捕获者
├── NetworkError           ← 连接失败/超时/对端重置（无 HTTP 响应）
├── HttpError              ← 收到响应但状态码不被接受
├── ModelError             ← 模型层失败（输出无法解析/不满足契约/上游模型错误）
├── ContractError          ← 契约破裂（schema 校验失败、json.as_* 窄化失败、工具响应不符）
├── CircuitOpenError       ← 熔断 open，请求未发出
└── BudgetError            ← 预算不足（**唯一不可捕获**，见 7.3）
```

无用户自定义错误类型（无用户函数，无处声明），无多重继承，无包装语法。层级写死在编译器里，`catch (T e)` 的类型集合封闭。

#### 7.1.1 每个错误类型携带的字段（`e.field` 的静态契约）

`catch (T e)` 的 `e` 是只读结构值，字段访问与 struct 字段一样**静态检查**。这是 D-014 的直接要求：`catch (HttpError e) { if (e.status == 404) ... }` 必须写得出来，而它依赖 `HttpError` 有 `status`。字段集合在 `check_type.cpp` 的一张表里，是**静态**契约：解释执行（`hermes run`）与 AOT（`hermes compile`，C 后端）下**一致生效**，两条路径的差别与 AOT 侧的实现形态见下面第 7 条。

| 类型 | 字段 | 说明 |
|---|---|---|
| `NetworkError` | `kind: string`（`timeout`/`refused`/`reset`/`dns`）、`url: string`、`elapsed_ms: int` | 无 HTTP 响应 |
| `HttpError` | **`status: int`**、`url: string`、`method: string`、`body_prefix: string`（≤1 KiB，按 policy 脱敏）、`attempts: int` | 有响应但状态码未被接受 |
| `ModelError` | `provider: string`、`model: string`、`kind: string`（`upstream`/`invalid_output`/`truncated`）、`status: int`（上游无状态码时为 0） | |
| `ContractError` | `where: string`（`model_output`/`tool_body`/`json_narrow`）、`expect: string`、`got: string`、`path: string` | 契约破裂 |
| `CircuitOpenError` | `provider: string`、`model: string`、`open_until_ms: int` | 请求未发出 |
| `BudgetError` | `limit_micro_usd: int`、`spent_micro_usd: int`、`requested_micro_usd: int` | 不可捕获（§7.3），字段只为 trace 与 `--explain` |

约束：

1. **只读**：错误对象没有构造语法，也没有字段赋值语法 —— `e.status = 500` 解析不出来（赋值的目标只能是裸标识符，`E1001`）。错误的产生权只属于引擎。
2. **不可存**：`auto s = e.status;` 合法（`int` 是值类型）；`auto x = e;` → `E4023`——错误对象不能离开 `catch` 块，否则它的生命周期跨出了产生它的那次失败，trace 里"这个错误已被恢复"的判断会失效。
3. **字段集合封闭**：新增字段是 `ir_version` 的 minor 变更；删除或改类型是 major（`IR.md` §10）。程序里访问不存在的字段 → `E4016`，与 struct 走同一条诊断路径。
4. `body_prefix` 必须脱敏后截断（Authorization/Set-Cookie/长 token 按 policy 打码），且**永不进 trace 全文**——它进 trace 的是 `body_sha256` 与前缀。
5. **规范字段 ≠ 运行期已有字段**（2026-11-10）：上表是**目标**契约，执行器的错误值目前只填了 `HttpError.status`。读其余字段 → `E4034`（"规范有、运行期还没带上"），而不是 `E4016`（那是在说"没有这个字段"，会把人赶去查拼写），也不是放行（编得过、运行期死在 `field does not exist` 上，而那是普通 `error`，`catch` 接不住）。实现上这是 `check_type.cpp` 字段表的 `ready` 一列 —— **运行期真把某字段填上时只翻这一位**，`tests/compiler/parser_test.cpp::test_err_fields` 两侧都钉住。
6. `catch (Error e)` 的 `e` 是根类型，而上表的字段集合按子类型分，**没有任何字段是全部子类型共有的**（`status` 在 `HttpError`/`ModelError` 上有、`NetworkError` 上没有）⇒ `e.status` 在根类型上报 `E4016` 并给出"换成具体类型"的改法，不猜。
7. **AOT（C 后端）同样兑现静态契约**（2026-11-10）：`catch (HttpError e) { if (e.status == 404) ... }` 在 `hermes run`（解释执行）与 `hermes compile`（C 后端）下**都能读到真实 HTTP 状态码**，两条路径一致生效。AOT 产物内部有一个**标准错误对象**（固定宽度整数 `code` / `status` + 定长字符数组 `msg`，**无指针成员**，C 编译期布局断言钉死其大小与字段偏移），由 `hermes_net_*` 的「返回状态码 + 出参」边界填充；`try` 的 catch 臂、`fallback`、`retry` **三处清错点共享同一个对象并统一清空**，catch 臂在清错**前**按值快照供 `e.status` 读。完整规范见 `docs/AOT-ERROR-OBJECT.md`。几条边界必须讲清：
   - **`fallback` 闭环不等于让 `fallback` 绑定错误对象**：§5.4.1「`fallback` 内不绑定错误对象」这条语言设计不变；闭环只发生在**清错语义**（三处清错点都清整个对象），要看错误字段仍请用 `try` / `catch`。
   - **源码层字段集合未变**：不新增 `e.code` / `e.msg`（§7.1.1 字段集合封闭，新增字段属 `ir_version` minor 变更，当前实现不升）。`code` / `msg` / 结构体布局只是 AOT 产物**内部**的 ABI 字段，不是源码可访问的字段；源码层仍只有 `HttpError.status` 是 ready。
   - **规范有但运行期未带上的字段仍拒绝**：`e.url` 等字段按第 5 条报 `E4034`；AOT 侧也**显式拒绝**而非静默给 `null`（「静默错值比拒绝坏得多」这条历史论证保留在 `KNOWN-GAPS`）。
   - **对外错误文本已闭合（v2.0.0 收口）**：AOT `main.c` 仍是独立可执行文件，但 unit AOT 成员与 `hermes cc` 的 shim 现在把错误文本发布到共享错误槽，宿主可用**既有**的 `hermes_last_error()` 读到真实原因；**不改公开 ABI 签名、不动冻结头与 `hermes_core.h`、不新增导出函数**，7 语言 SDK 仍走 `err` 出参（错误文本本就完整，未改）。**新限制**：`-std=c89` / `-std=c99` 下 `HERMES_THREAD_LOCAL` 展开为空，槽退化为进程级全局，并发调用会串错误（见 `docs/AOT-ERROR-OBJECT.md` §9 与 `docs/C-ABI.md`）。**仍剩**：**32 位 target 从未实机编译 / 链接 / 运行**，只有「按构造可移植 + 编译期断言 + 双 target 编译探针（若 clang 支持）」这一层保障，不能当成实机验证（见 `docs/AOT-ERROR-OBJECT.md` §6）。
   - 判据：`internal/backend` 的生成结构守卫与端到端 AOT 测试（放行 `status`、拒绝未 ready 字段、真实 404 / 503 分流），不再以单一「拒绝读错误对象字段」的守卫为准。

### 7.2 `catch` 匹配规则

- `catch (T e)` 匹配 `T` 及**其所有子类型**。
- 兄弟互不匹配：`catch (HttpError e)` **不会**接住 `NetworkError`。
- 同一 `try` 内，若前一个 catch 的类型是后一个的父类型，后者不可达 → `E4002`（硬错）。不可达 catch 几乎必然是把顺序写反了，而它会让本该走专用分支的错误落到通用分支，静默降级。
- `catch (Error e)` 捕获除 `BudgetError` 外全部。

### 7.3 `BudgetError` 不可捕获（`E5006`）

`catch (BudgetError e)` 编译失败。理由：预算是 **run 级**不变量（ARCH INV-1：`spent ≤ limit` 恒成立）。允许捕获即允许程序在预算耗尽后继续花钱，INV-1 当场失效——而 ARCH §5.4 整个账本设计（预留必须先于请求）就是为保证这条。预算耗尽的正确语义是**run 以 partial 结果 halt**（退出码 3，ARCH §7.2），由引擎决定，不由被编排的程序决定。

替代做法：把大 workflow 拆成多个 workflow，由外层调度器按预算分别发起。

### 7.4 到 PRD 6 值归因闭集的映射

映射在**引擎侧**（`internal/errs/table.go`），不在语言侧——语言只负责把错误**分类正确**。判定顺序**从上到下，首个命中者胜**（顺序敏感：429 既是 4xx 又是容量）：

| 错误类型 | 附加条件 | Attribution | Subtype | 计熔断 |
|---|---|---|---|---|
| `CircuitOpenError` | — | `circuit` | — | ❌ |
| `BudgetError` | — | `budget` | — | ❌ |
| `HttpError` | status = 429 | `policy` | `quota` | ❌ |
| `HttpError` | status ∈ {503, 529} | `policy` | `capacity` | ❌ |
| `HttpError` | status ∈ {502, 504} | `transport` | `timeout` | ✅ |
| `HttpError` | 其他 4xx | `upstream_contract_drift` | — | ✅ |
| `HttpError` | 其他 5xx | `transport` | — | ✅ |
| `NetworkError` | — | `transport` | `timeout`/`refused` | ✅ |
| `ModelError` | 输出不满足 schema / 无法解析 | `model_contract_violation` | — | ❌ |
| `ModelError` | 上游拒绝（模型不存在等） | `upstream_contract_drift` | — | ✅ |
| `ContractError` | 工具响应体不符 | `upstream_contract_drift` | — | ✅ |
| `ContractError` | `json.as_*` 窄化失败 | `model_contract_violation` | — | ❌ |

> `catch` 捕获错误**不改写** trace 里已记录的归因：该 span 仍是 `status=failed` + 原归因，父 span 可以 `ok`。审计者因此能看到"这里失败过但被恢复了"，而不是"什么都没发生"。这是 R-TR-03 契约证据的一部分。

---

## 8. backend、precision 与 schema 编码（设计裁决 Q10）

### 8.1 `precision` 映射

| HermesScript | 上游 `tier` | `response_format` | 语义 |
|---|---|---|---|
| `"exact"` | `exact` | `{"type":"json_schema","json_schema":{"name":T,"strict":true,"schema":S}}` | 逐位一致（约束解码，非法字符不可生成） |
| `"fast"` | `tolerance` | 同上 | 结构受约束，数值/字符串不保证逐位一致 |
| `"perf"` | `speed` | 同上 | 同上，优先吞吐 |

上游的 tier 集合恰为 `(exact, tolerance, speed)`，故是 1:1 映射，不是发明新词。

`precision` 必须是字符串字面量（`E2014`），因为它参与静态上界与 build 报告。

### 8.2 不支持约束解码的 backend 上 `exact` 的降级（必须显式）

| 情形 | 行为 |
|---|---|
| `kind = "homonoia"` 且 `supports_tier = true` | `exact` 正常 |
| 其他 backend + `exact` + 未声明 `exact_fallback` | **编译错 `E5008`** |
| 其他 backend + `exact_fallback = "fast"` | 降级为 `fast`；build 打 `W5002`；trace 记 `precision.effective="fast"` 与 `precision.degraded_from="exact"` |
| 其他 backend + `fast`/`perf` | 正常；若连 `response_format` 都不支持则 `constraint.mode="post_hoc"` 并打 `W5002` |

`E5008` 文案见 §11.2 ⑧ 之后（同码族）。核心："不许静默降级"——静默降级比报错更坏，因为用户会以为拿到了强约束。

### 8.3 schema 字节稳定性（六条）

生成的 schema 会进入请求体 ⇒ 其字节进入 `request_sha256`。为保证同一源码永远生成同一字节：

1. 键序固定：`type` → `properties` → `required` → `additionalProperties`，`$defs` 在根末尾。
2. `properties` 内字段顺序 = struct **声明顺序**。
3. `$defs` 键顺序 = 类型 id 升序。
4. 无多余空白（canonical JSON，复用 ARCH §5.3）。
5. `additionalProperties: false` **总是**生成（OpenAI strict 要求；`grammar.py` 支持）。
6. 可选字段仍进 `required`，类型写成 `anyOf[T, null]`（§4.6）。

### 8.4 `{}` 的降级

`json` 字段生成 `{}`（空 schema = any）。若后端不接受空 schema，降级为显式 7 类型 union：

```json
{"type":["object","array","string","integer","number","boolean","null"]}
```

降级是 **build 期决定**（`--probe` 结果或 `backend.schema_any = "union"` 显式声明），**不写进 `.hbc`**；运行期读配置现算。默认 `union`——因为它是**确定能工作**的那个；拿运行期稳定性换 schema 体积不划算。

### 8.5 `backend`：声明/定义分离 + `env()` 与 `auth` 子句

| 位置 | 写法 | 含义 |
|---|---|---|
| `.h` | `export backend homonoia;` | **声明**：这个 unit 对外承诺有一个叫 `homonoia` 的 backend。没有地址 |
| `.hermes` | `backend homonoia = env("HOMONOIA_URL", "http://127.0.0.1:8080");` | **定义**：地址来自环境变量，缺省走本机 |
| `.hermes` | `backend homonoia = env("HOMONOIA_URL", "http://127.0.0.1:8080") auth env("HOMONOIA_KEY");` | **定义 + 密钥来源**：`auth` 子句只给变量名。地址有本地默认，密钥没有默认值 |
| `.hermes` | `backend local = "http://127.0.0.1:11434";` | 定义且字面量（仅用于确实不该变的场景，如本机固定端口） |
| 两处都没有 | — | backend 不存在 → `E3007` |

规则：

1. **`env()` 的第二参数必填且必须是字符串字面量**（`E3018`）。空串是有语义的：表示"必须由外部设置，没有默认值"，此时缺变量在 `run` 起跑前失败（退出码 2，`E_ENV_MISSING`），**报错发生在花第一分钱之前**。
2. **地址里不允许出现凭据**：`"http://user:pass@host"` → `E3019`（"凭据不进源码：源码要进 git、进归档、进审计"）。凭据由引擎侧 policy 注入 header。
3. 定义的 backend 若在 `.h` 里没有对应声明 ⇒ 它是**私有** backend，仅本 unit 可用（合法，常见于只在本文件里用的本地模型）。反之声明了没有定义、也没有 `#link` 提供 ⇒ 链接期 `E_LINK_UNRESOLVED`。
4. **求值时机**：`hermes run` 启动时读一次；解析结果写进 trace 的 `backend.resolved`，只记 `scheme + host + port`，不记 userinfo、不记 query。
5. **密钥来源是 `auth` 子句，不是地址的一部分**：`auth` 位只接受 `env(名)` **单实参**（`E2018`），地址位是**两**参（`E3018`）——**元数按位置定**，这是两个错误码分家的根据（禁默认值的理由写在 §3.1 注 8，本节只管规则与产物）。产物侧：密钥值**不进 IR、不进 `.hbc`、不进签名产物**；trace 只记 `env` 变量名，不记值、不记头长度。`auth` 与字面量地址可以同时出现——地址来源与密钥来源是两件正交的事。

**为什么必须分离（三个都成立才叫理由）**

1. **硬编码地址的库无法分发**：`.hbc` 给别人用时，`127.0.0.1:8080` 是**我的**机器，不是他的。
2. **离线与零费用要是默认行为**：不设任何变量就走本机默认 backend，这是 D-007"不依赖外部服务"能在部署层成立的前提。若地址必须由命令行给，忘给就崩，人会写死一个地址来"省事"——那才是真正的不可重现。
3. **默认值必须是字面量，才能静态审计**：审计者不需要执行任何东西、不需要知道环境，就能回答"这个 workflow 在没设变量时会把用户数据发到哪个主机"。若默认值可以是表达式，这个问题就只能靠跑一遍回答，而"跑一遍"在审计场景里是不允许的。

### 8.6 `entry` 与 `--workflow` 的解析顺序

见 §3.6。这里只补一条与 backend 相关的：`entry` workflow 的 `backend` 参数默认值在 `.h` 与 `.hermes` 上必须一致（`E3014`），因为它是"不指定时用哪个 backend"的唯一声明——两处不一致时，`hermes run` 与 `hermes serve` 可能选到不同的 backend，而这件事没有任何运行期信号能暴露。

---

## 9. include 与三种文件（设计裁决 Q6，已 修订）

### 9.1 三种文件的分工 + `#include` 只吃接口

见 §1.1 表。一句话记住：**`#include` 回答"它长什么样"，`#link` 回答"它的代码在哪"。**

| 写法 | 结果 |
|---|---|
| `#include "models/order.h"` | 取声明。**唯一合法的 include 目标** |
| `#include <std/net.h>` | 取内置标准库的声明 |
| `#include "models/order.hermes"` | **`E3015`**："不能包含实现文件。要引入它的实现请写 `#link "models/order"`，只需要它的类型请包含 `.h`" |
| `#link "models/order"` | 取实现（`models/order.hbc`），并把它记进 `requires` |
| `#link <std/net>` | 取内置实现（`stdlib.hbc` 的 `net` 成员） |

不存在"按字面找、找不到回落 `.hermes`"的规则，也**不再**允许 `#include "x.hermes"` 内联实现（v3.0 曾允许，v3.1 废除）。理由不是洁癖，是两个只有 `#link` 能给出的性质：

1. **接口可见、实现可替换**：`.h` 是类型信息，`.hbc` 是代码。源码不在手上时，前者能给你、后者能给你——但只有分开，才能"只给前者"（写集成测试、做代码审阅）或"只给后者"（分发到生产）。
2. **依赖是源码里的事实**：`#include "x.hermes"` 把实现拉进当前 unit，于是"这个 unit 依赖什么"只能靠读全部源码才知道；`#link` 把依赖写成一行声明，编进 `.hbc` 的 `requires`，链接器据此校验版本（§9.5），`hermes ar t` 一眼列出。

若 `#include "x.h"` 找不到，错误必须列出：尝试过的每个路径 + **"若你要的是实现，请写 `#link "x"`"**——因为这是 v3.0 迁移过来的人 100% 会踩的一脚。

### 9.2 搜索路径（include 与 link 是两套，不共用）

| 形式 | 搜索顺序 |
|---|---|
| `#include "..."` | ① 当前文件所在目录；② `-I` 目录（按命令行顺序）；③ 项目根（entry 文件所在目录）。**不搜索**系统目录、不读环境变量、不联网 |
| `#include <std/...>` | **只**查内置 `stdlib.hbc` 随附的 `.h`（`go:embed` 进二进制）。找不到 → `E3001`，消息列出全部可用模块 |
| `#link "mod"` | ① `-L` 目录（按命令行顺序）；② 项目根；解析为 `<dir>/mod.hbc`。找不到 → `E_LINK_NOT_FOUND`，列出尝试路径与 `hermes ar t` 能看到的候选 |
| `#link <std/net>` | **只**查内置 `stdlib.hbc` 的成员表。找不到 → `E3001` |

两套路径刻意不共用：include 找 `.h`（可能来自源码树），link 找 `.hbc`（一定来自构建产物）。共用会让"我在源码树里改了 `.h` 但链接的是三天前构建的 `.hbc`"变成默认行为——那是 C 里 `-l` 顺序问题的翻版。

安全约束：include 路径分量禁止 `.`/`..`（`E1009`）；link 模块名禁止 `.`/`..`/绝对路径/`.hbc` 后缀（`E1010`）；符号链接按解析后真实路径去重；IR 里记录**相对 include root 的 POSIX 相对路径**与**相对 link root 的模块名**（可重现构建要求）。

### 9.3 循环与重复

- **include-once 语义**（不是 C 的文本包含）：键 = 解析后绝对真实路径，重复 include 是 no-op。
- 循环 → `E3002`，打印**完整环** `a.h → b.h → c.h → a.h`，每跳带行号。
- 深度上限 32（`E3003`）；模块数上限 256（`E3004`）。环检测用迭代 DFS + 显式栈 + 三色标记（不用递归，避免深链爆栈；前端 `-fno-exceptions` 下栈溢出无法捕获）。
- **`#link` 重复是 no-op**（按解析后的模块标识去重），且 `#link` 不参与 include 的环检测——它是另一个图（链接图），环形依赖留给链接器报 `E_LINK_CYCLE`（`IR.md` §9）。
- `#link` 的**传递性**：A link B、B link C ⇒ A 的 `requires` 含 B 与 C（闭包），但 A 的**类型可见性**不含 C 的声明——要用 C 的类型，A 必须自己 `#include "c.h"`。这正是"实现可传递、声明不传递"的分离，与 C 的 `-l` + `#include` 一致。


### 9.4 各文件的语法限制

| 文件 | 允许的顶层声明 | 违反 |
|---|---|---|
| `.hermes`（entry） | `#include`、`#link`、`[export] struct`、`backend` 定义、带体 `workflow` | — |
| `.h` | `#include`、`[export] struct`、`backend` 声明（无地址）、**`workflow` 签名**（`-> Type;`，无体） | 带体 → `E3005`；语句 → `E3006`；`#link` → `E3017`；`backend x = ...` → `E3020` |

`workflow` 签名的语义作用：**链接期校验**。若某 `.h` 声明 `export workflow support_bot(input: SupportInput, backend = homonoia) entry -> string;` 而链接进来的 `.hbc` 里同名 workflow 的返回类型/参数/`entry` 标记不同，链接失败（`E_LINK_SIG_MISMATCH`，`IR.md` §9.3）。它不是可调用接口（语言无跨 workflow 调用），而是"分发到没有源码的环境"时的接口契约。

### 9.5 `requires`：链接器据以校验版本的表

每一条 `#link` 在 `.hbc` 的 unit 元数据里留一条记录：

| 字段 | 例 | 校验规则 |
|---|---|---|
| `spec` | `<std/net>` / `models` | 原样保留，供 `hermes ar t` 打印 |
| `kind` | `std` / `local` | `std` 只在本编译器版本内解析 |
| `abi_version` | `2` | 必须与消费者**完全相等**，否则 `E_LINK_ABI_MISMATCH` |
| `ir_version` | `2` | 链接时主版本必须相同（`IR.md` §9.5）；但**产物顶层的 `ir_version` / `language_version` 在 2.0.0 起必须完全等于 `2`**，1.x 一律 `E_IR_VERSION`（不做向前兼容） |
| `member_sha256` | `ab12…` | 被 link 的 member 的字节哈希；与归档实际内容不符 → `E_LINK_TAMPERED` |

**版本约束是推导出来的，不是写出来的**：`#link` 的语法里**没有**版本号。理由是加版本号会立刻产生"声明 `>=2` 但实际需要 `>=3`"这类无法验证的谎话，而 `abi_version` 必须严格相等、`ir_version` 主版本必须相同这两条，已经能把"版本错配"这一类事故全部挡住。真正需要细粒度约束时（Phase 2 的语义化版本范围），再加语法。

---

## 10. IR（设计裁决 Q4，详见 `docs/IR.md`）

**结论：确认团队负责人的判断，并加强为"IR 与 trace 树同构"。** 这个"同构"要限定清楚，否则循环一来就是假话：

- 无用户函数、无递归；`for` / `while` 也是**单一出口**的结构化构造 ⇒ 控制流是**结构化树**（节点图无回边）。注意区分两件事：**节点图**无环，而**执行序列**可以重复——循环体就是"同一个节点被执行 N 次"。
- 因此 IR **不是** CFG：无基本块、无 φ 节点、无 SSA。`if`/`match` 是分支+汇聚，`parallel` 是显式 fork/join，`for`/`while` 是带体子的循环节点。
- span 只落在**控制流块与调用**上（引擎的 `block()` 与调用路径），不是每个节点都落；`parallel` fork 对应 1 父 + k 子。所以"节点数与 span 数一一对应"只在**无循环**时成立；有循环时是"节点数 × 迭代次数"。因此 `max_steps` 只在迭代次数可证时才是 span 数上界——迭代次数不可证（`while`、步进推不出次数的 `for`、`max_steps = unlimited`）时，workflow 标 `bounds.unbounded: true` 并放弃这条声称（§6.4、`IR.md` §5.3）。
- IR 是**扁平节点数组 + id 引用**（不是嵌套 JSON）：便于 diff/审阅/增量工具；id 引用让变量绑定的值被多处引用（真 DAG 而非树）。
- 每个节点**必带 source map**（`loc:{m,l,c,el,ec}`，m = 模块索引）。这是"IR 可独立审计"的硬要求（Q12）。
- IR 顶层是 **unit 列表**：每个 unit 有自己的名字、导出符号集、DAG、source map；`types` 与 `economics` 的归属见 IR.md §3。
- 每个 unit 还带三项 v3.1 元数据：**`exports`**（符号表，来自 `export`，§3.5）、**`requires`**（来自 `#link`，§9.5）、**`entry`**（默认入口名，可空，§3.6）。三项都是**语法事实**——`hermes ar t` 打印它们，链接器只读它们，不读源码。
- `input` 类型生成的请求 schema 与 `model.generate<T>` 的输出 schema 走同一套映射（§4.6/§4.8），都存在 `schemas` 表里，按 `schema_sha256` 去重；生成的字节稳定性规则见 §8.3。
- **私有声明进 IR 但不进 `exports`**：它们的节点仍在 DAG 里（私有 workflow 可以被 `entry` 选中；私有 struct 被导出的签名引用），只是不被符号表列出，链接期不可见、可被死代码剥离（`IR.md` §9.4）。



---

## 11. 诊断质量（设计裁决 Q11）

### 11.1 诊断结构

所有诊断（错误/警告/note）同一形状，C++ 前端产出、Go 渲染：

```json
{"severity":"error|warning|note",
 "code":"E4016",
 "message":"struct Intent 没有字段 \"oder_id\"",
 "line":27,"col":24,"end_line":27,"end_col":31,
 "module":"support.hermes",
 "why":"...",
 "fix":["...","..."],
 "notes":[{"module":"support.hermes","line":14,"col":8,"message":"struct Intent 在此声明"}],
 "docs":"docs/LANGUAGE.md#7.4"}
```

**硬要求（验收项）**：`message` 必须是一句**可独立读懂**的话（禁止 "invalid token" 这类无主语短语）；`fix` 至少一条；跨文件问题时 `notes` 必须指回定义处。

### 11.2 十一条真实诊断（逐字，可直接作为测试断言）

#### ① 未声明字段 `E4016`

```
error[E4016]: struct Intent has no field "oder_id"
  --> support.hermes
      line 27, col 31

  |
27 |                 url = "https://api.internal.com/orders/{intent.oder_id}",
  |                                                        ^^^^^^^
   why: 强类型的意义就是拼错的字段在 build 时红，而不是在线上把 "{undefined}" 发进 URL。
  help: struct Intent 的字段是 action, order；你要的大概是 intent.order.order_id
  note: struct Intent is declared here (support.hermes line 14)
```

#### ② 类型不匹配 `E4017`

```
error[E4017]: type mismatch: expected string, got int?
  --> support.hermes
      line 31, col 24

  |
31 |         prompt = "订单 {order.note} 已创建",
  |                        ^^^^^^^^^^^
   why: 可空字段 string? 不能直接插值——"没说"和"空字符串"是两件事，
        直接插值会把 null 写成 "null" 发进 prompt，而模型会把它当字面量。
  help: 怎么改（二选一）：① 让模型必填该字段（把它从 T? 改成 T，并在 schema 里进 required）；② 先在工具侧补默认值：让 .h 里声明 string note; 而不是 string? note;
```

#### ③ `match` 缺兜底 `E4018`

```
error[E4018]: match has no default arm
  --> support.hermes
      line 35, col 5

  |
35 |     match (intent.action) {
  |     ^^^^^
   why: intent.action 的类型是 string，取值集合不受 struct 约束（模型能输出任何字符串）。没有 "default" 时"两个分支都没命中"的路径无定义——那是未定义行为唯一的入口。
  help: 怎么改：match (intent.action) { case "refund": { return "退款"; }, case "logistics": { return "查物流"; }, default: { return "未知操作：{intent.action}"; } }
  help: 参考: docs/LANGUAGE.md §3.1 MatchArm（"default" 是必选产生式）
```

#### ④ `retry` 内有非幂等调用 `E5007`

```
error[E5007]: tool call inside retry has no idempotency proof
  --> support.hermes
      line 44, col 19

  |
44 |         auto pay = tool.call(url = "https://api.internal.com/pay",
  |                   ^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
   why: POST 在重试时会重复提交。运行期无法判断"这个调用能不能安全重试"——幂等性是声明性事实。语言能拿到静态调用点，所以在这里判死，而不是让线上出现重复扣款。
  help: 怎么改（三选一）：① 上游确实幂等: method = "POST", idempotent = true；② 让引擎注入幂等键: method = "POST", idem_key = "{input.user_id}"（idem_key 必须只由 input 决定，不能含模型输出或工具响应——否则重试时键会变）；③ 方法本身只读: method = "GET"
  help: 参考: docs/LANGUAGE.md §5.7
  note: this call sits inside retry(2) and may run up to 3 times (support.hermes line 43)
```

#### ⑤ `parallel` 元组长度不匹配 `E5003`

```
error[E5003]: parallel returns 2 values, but the left side destructures 3 names
  --> support.hermes
      line 28, col 14

  |
28 |         auto [order, history, user] = parallel {
  |              ^^^^^
   why: 元组按位置配对，两侧长度都是静态常量，不等一定是写错了。若允许运行期报错，就会"跑到第 3 个才失败"，前面两次调用白花钱。
  help: 怎么改: 删掉第 3 个名字，或在 parallel 体内再取一个值：return [o, h, u];
  note: parallel block at L48:13 returns 2 values (support.hermes line 48)
```

#### ⑥ 未知 backend `E3007`

```
error[E3007]: workflow references undeclared backend "hononoia"
  --> support.hermes
      line 9, col 26

  |
 9 | workflow support_bot(input: SupportInput, backend = hononoia) entry {
   |                                                    ^^^^^^^^
   why: backend 必须在同一 unit 内可见（本文件或 #include 进来的 .h 里的声明）。若拼错时"自动回落到默认 backend"，你会以为走了本机 backend，实际走了别处。backend 有声明但没有定义时，报的是链接期的 E_LINK_UNRESOLVED，不是这里。
  help: 怎么改（二选一）：① backend = homonoia —— 改拼写；② 在 support.h 里加 export backend homonoia; —— 确实要新增一个，并在 support.hermes 里给它地址: backend hononoia = env("HONONOIA_URL", "http://127.0.0.1:8080");
  note: backends declared in this unit: homonoia (support.h line 11)
```

#### ⑦ `<std/...>` 不存在 `E3001`

```
error[E3001]: built-in standard module <std/vect.h> not found
  --> support.hermes
      line 1, col 1

  |
 1 | #include <std/vect.h>
   | ^^^^^^^^^^^^^^^^^^^^^
   why: <...> 只解析随二进制内置的标准库（stdlib.hbc + 其 .h），不搜索文件系统、不联网——这是"零费用可重现"的要求。
  help: 可用的标准模块（本版本）：<std/net.h> tool.call 能力声明 + NetworkError / HttpError；<std/model.h> model.generate 能力 + ModelError / ContractError；<std/json.h> json.encode / json.decode / json.as_*；<std/runtime.h> BudgetError / CircuitOpenError（默认已隐式包含与隐式链接）。实现由 #link 取: #link <std/net> / #link <std/model>
  help: 怎么改: 若你要的是项目内的文件，用引号形式: #include "lib/vect.h"
  help: 注意: 写成 <std/net.hermes> 会得到 E3015（不能 include 实现文件），不是 E3001。
```

#### ⑨ `#include` 了实现文件 `E3015`

```
error[E3015]: cannot include an implementation file
  --> support.hermes
      line 1, col 1

  |
 1 | #include "models/order.hermes"
   | ^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
   why: #include 只给声明（类型信息），实现必须走 #link。允许 include 实现会让"这个 unit 依赖谁的代码"只能靠通读源码才知道，也就无法替换实现——而"接口可见、实现可替换"正是 .hbc 存在的理由。
  help: 怎么改（二选一）：① 只要 OrderInfo 这个类型: #include "models/order.h"；② 还要它的实现: #include "models/order.h" + #link "models/order"
  help: 参考: docs/LANGUAGE.md §9.1
```

#### ⑩ 用了内建但没 `#link` `E3016`

```
error[E3016]: tool.call used but this unit has no #link <std/net>
  --> support.hermes
      line 16, col 17

  |
16 |                 auto o = tool.call<OrderInfo>(
   |                         ^^^^^^^^^
   why: #include 给声明，#link 给实现。少了 #link，编出来的 .hbc 在链接期才会缺符号，而那时诊断里已经看不到你写的这一行。编译期报出来代价最小。
  help: 怎么改: 在 support.hermes 顶部的指令区加一行 #link <std/net>
  note: the declaration exists; the implementation is missing (support.h line 4)
```

#### ⑪ `input` 参数没写类型 `E3023`

```
error[E3023]: input parameter must have a type
  --> support.h
      line 17, col 24

  |
17 | export workflow support_bot(input, backend = homonoia) entry -> string;
   |                                 ^^^^^
   why: input 的类型同时就是这个 workflow 的 HTTP 请求契约——hermes serve 的 API schema 由它生成。没有类型时 input.text 无法校验、请求体也无法生成 schema：强类型从入口第一行就漏了，后面再严也没用。
  help: 怎么改: export struct SupportInput { string text; string user_id; } export workflow support_bot(input: SupportInput, backend = homonoia) entry -> string;
  help: 参考: docs/LANGUAGE.md §4.8
```

#### ⑧ `precision` 非法值 `E2014`

```
error[E2014]: precision must be one of the literals "exact" / "fast" / "perf"
  --> support.hermes
      line 13, col 37

  |
13 |         prompt = "...", precision = "fastest"
   |                                     ^^^^^^^^^
   why: precision 决定解码模式，也参与成本上界计算（IR economics），必须是编译期常量；拼错若回落到默认值，你会以为拿到了 exact。
  help: 怎么改: precision = "fast" （三个合法值: exact / fast / perf）
  help: 参考: docs/LANGUAGE.md §8.1
```

### 11.3 诊断质量门禁（验收项）

1. 全部诊断经 `schemas/diag.schema.json` 校验。
2. 上表 11 条逐字进 `tests/compiler/diagnostics_golden.txt`，CI 逐字比对（`message`/`fix` 改动必须显式改 golden，防退化）。
3. 每个诊断码必须在附录 A 索引中出现（防"有码无文档"）。**由 `TestEveryCodeAppearsInAppendixA` 执行**——规范里写"必须"而没有测试跑它，等于没有这条门禁。
4. 前端**可恢复解析**：一轮解析把该阶段能恢复的诊断全部产出（`-fno-exceptions` 下本来也只能如此），**渲染几条由 CLI 决定**：`hermes build` 默认渲染到第一条 error 即停（warnings 若排在 error 之前仍会显示，绝不把 error 藏起来），`--keep-going` 则把这一轮能恢复的诊断全部渲染出来（上限 50 条，超出部分以「还有 N 条」提示）。两者都不跨阶段续报。`--deny-warnings` 把 warning 升级为构建失败（退出码 4，且不写归档）。

   截断是**渲染策略**，不是解析策略：前端仍一次产出该轮全部诊断（`tests/compiler/parser_test.cpp` 的 `test_keep_going` 锁定的正是这件事），所以"默认只报第一条"与"错误恢复"不冲突——它们在不同的层。实现见 `cmd/hermes/ir_build.go` 的 `truncateDiags`，判据 `TestTruncateDiags` + `TestBuildKeepGoing`。

5. **同一处问题只说一遍**：渲染前先按「模块 + 严重级 + 码 + 行列区间 + 消息」逐字合并，再截断（顺序不能反，否则 50 条上限会被重复项吃掉）。这不是洁癖：同一个模块会被前端解析好几遍（每个 C 入口各跑一次完整 lex + parse + check_types + check_idempotency，且每趟都把整袋诊断交回来），模块级诊断因此会各出好几份。合并只针对**逐字相同**的诊断 —— 位置、码、严重级任一不同都各留一条（判据 `TestDedupeDiagsKeepsDistinct` 的 5 个反例）。

### 11.4 离线查错：`hermes explain <CODE>`

编译当场给出的诊断是**内联**的（头行 + 定位 + 源码片段 + why + fix），那是"错在哪"。
错过现场之后（日志、CI 输出、同事贴来的一行 `error[E4016]:`）需要的是"这类错是什么"——
`hermes explain` 提供这一层，与 `rustc --explain` 同形：

```
$ hermes explain E4016
error[E4016]: the type has no such field
   why: Field access reads a slot at a fixed offset; a field the type does not declare
        would read bytes that were never laid out, so the access cannot be compiled.
  help: use one of the type's declared fields
  help: or add the field to the struct declaration
  docs: docs/LANGUAGE.md#4.1
```

契约（每条都有测试）：

| 事 | 规定 |
|---|---|
| 参数形态 | 数字码（`E4016`/`W2003`）与语义码（`E_IR_DUP_ID`/`W_IR_PANEL_TRUNCATED`）都接受；不是码时按原来的 IR / `.hbc` 路径解释 workflow 契约 |
| 头行 | 严重程度由**码的首字母**定：`W…` 打 `warning[…]`，其余打 `error[…]`。不允许 `warning[E4016]` 这种自相矛盾的输出，所以 `RenderExplain` 不收 severity 参数 |
| 定位行 | **不打** `  --> `：explain 解释的是一类错误，没有具体源码位置，空箭头只会让人以为丢了信息 |
| 占位符 | 模板里的 `{id}` 渲染成**元变量** `<id>`（与文法书的记法一致）：既不填样例值（那会把一类说成一个例子），也不留 `{id}`（那会被读成替换坏了）。`TestExplainHasNoLeftoverPlaceholders` 对每条码 × 两种语言断言"一个 `{name}` 都不许剩" |
| 语言 | 跟随 `HERMES_LANG`（`zh*` → 中文），与编译期诊断同一套文案表 |
| `--json` | 输出 `{code,message,why,fixes,docs}`，供编辑器 / 文档生成读取 |
| `--color auto\|always\|never` | 与 `Render` 同一套判据（`NO_COLOR`、`TERM=dumb`、TTY 检测，零第三方依赖） |
| 未登记的码 | 退出码 **4**，并提示"该码由编译器内联产出 why/fix，重跑构建可见"——**不编造**一条说明 |
| 锚点 | 来自 `internal/diag/catalog.go` 的 `docsAnchorEN`，必须指向本文件真实存在的小节（`TestDocsAnchorsPointToRealSections` 守） |

**单一真相**：编译期内联的 `message/why/fix/docs` 与 `explain` 打印的是**同一张表**
（`internal/diag` 的 catalog）。两边分开写必然会漂——同一条码在构建里说"字段不存在"、
在 explain 里说"检查拼写"，用户就不知道该信哪句。新增诊断码时**只加一次**，
编译期与离线查错同时具备。

---

## 12. 边界、已知松点与偏差

### 12.1 已知语义松点

| # | 松点 | 为什么接受 | 何时收紧 |
|---|---|---|---|
| L1 | ~~`input` 未声明类型时是 `json`~~ **（v3.1 已关闭）** | D-014.6 把 `input` 的类型变成必填，`input.text` 从此静态检查 | 已收紧：`E3023`（§3.1 注 6） |
| L2 | `{j}` 插值 `json` 时走"string 原样、其余 canonical JSON"的动态规则 | 否则 `{input.text}` 输出带引号的 `"hi"`，与直觉相反 | Phase 2 加 `json.as_string` 强制 |
| L3 | `if (order.status == 404)` 默认契约下永不成立，但编译器只能发**启发式**警告 | 无法区分"业务字段恰好叫 status 且在 200 响应里等于 404"（类型上合法）与"作者误以为拿到了 HTTP 状态码"。**这里必须承认判定不了，不能假装判定** | **`W5004` 已实现**（2026-11-10，启发式：`==`/`!=` 一侧是 400–599 的整数字面量、另一侧是宿主为**用户 struct** 的 status/status_code/httpStatus 字段；`catch` 里的错误对象与 `HttpResult<T>.status` 都放行）。彻底解决要靠类型分层：把响应体与状态码放进同一个值（`HttpResult<T>`），而 `allow_status` 已经提供了那个类型 |
| L4 | 字符串不能跨行、不能 `+` 拼接 | 换行有语义会让未闭合字符串吞掉整个文件；`+` 拼接会诱导在代码里拼 prompt | Phase 2 加 `"""` |
| L5 | 无可空收窄（`if (x != null)`） | 无 `null` 字面量；可空字段只用于插值与编码已够 | Phase 2 |
| L6 | ~~`catch` 体内不能 `return`~~ **（v3.1 已关闭）** | v3.0 的理由（"trace 父子关系会不符"）站不住：`if`/`match` 也只走一条路径。禁止它会让错误恢复无值可产出 | 已收紧：允许 `return`（§5.4）。`E2012` 保留但不再触发 |
| L7 | 未使用的绑定不产生诊断 | 没有 liveness 分析；而 `parallel` 分支常是"为了副作用而取值"（HTTP 调用本身要发生），逼作者写 `_` 反而丢位置信息。上界不受影响（由 fork 宽度决定） | Phase 2：加 `--warn-unused`，默认关 |
| L8 | `#link` 语法里没有版本约束 | 加版本号会立刻出现"声明 `>=2` 实际需要 `>=3`"这类无法验证的谎话；`abi_version` 严格相等 + `ir_version` 主版本相同已能挡住全部实际事故（§9.5） | Phase 2：语义化版本范围，需配套"约束可验证"的方案 |
| L9 | workflow 返回类型在 `.hermes` 里是推断的 | 用户语法参考本就没有返回类型，强制写是无谓破坏；而 `.h` 里必填、且与推断比对，所以**对外契约仍是显式的** | Phase 2：可选 `--require-return-type` |
| L10 | `match` 的 `json` 主语虽能写出，但各臂类型一致性不校验 | `json` 主语的臂合法类型无法静态确定，强校验会误伤或漏报；`string`/`int` 主语已严格按同类型校验（`E4019`） | 收紧需先给 `json` 主语定义一套臂类型规则 |

### 12.2 与 DECISIONS 的偏差（唯一一处，需签字）

**D-010.1 的文字内部矛盾**："`cost`（定点，int64 微单位，8 位小数）"——int64 微单位 = 6 位小数；8 位需 int64@1e-8 或 int128。本文按 **6 位（micro-USD）**落地，理由见 §4.3。

- 若坚持 8 位：必须同时把 ARCH §5.4 的账本单位从 micro-USD 改为 1e-8 USD，并重审 `max_micro_usd ≤ 1e15` 上限（1e15 × 1e-8 = 1e7 USD，偏小）。**不建议**：钱在账本里多一次缩放，就是 D-010.1 想消灭的那个漏洞。
- 需要：团队负责人签字确认按 6 位落地，或明确要求改 8 位。

### 12.3 Phase 2 预留（不做，但留好接口）

| 项 | 预留 |
|---|---|
| `for`/`while` | `max_steps` 从常量变为"Σ + 循环上界乘法"，需要 `@bound(n)` |
| 用户函数 | 泛型消歧退化到 speculative parse（§3.4 D3）；调用图从树变图，幂等证明需跨过程 |
| `decimal` | `cost` 保持 int64；`decimal` 单独类型，不进账本 |
| `null` 字面量 | `T?` 收窄语法 |
| 多行字符串 | `"""..."""` |
| 与部署描述文件的互操作 | **不做**：`.hermes` 是编排 DAG，部署描述文件描述"起什么进程、怎么连"，两者正交。语言层不增加任何部署描述语法 —— 在 Hermes 里再实现一份，只会养出与真实部署必然分叉的第二个事实源 |

> 本表只列**还没做**的预留。曾在此的「诊断旗标（`--keep-going` / `--deny-warnings`）」已于 2026-11-10 落地（默认渲染到第一条 error 即停，`--keep-going` 最多 50 条），故移出本表，见 §11.3 第 4 条。

### 12.4 本规范在 D-014 之上补的 11 条裁决（需复核）

D-014 定了 7 处，但每处落地都会遇到它没回答的边界。下面是我补的裁决，逐条给"不这么做会更差"的理由。**这些是本规范里唯一需要你签字的部分**（其余都是 D-014/D-010/D-011 的直接落地）。

| # | 裁决 | 若反过来做，会坏在哪 |
|---|---|---|
| 1 | `#link` 只允许在 `.hermes`（`E3017`） | 允许写在 `.h`，则每个 include 该头文件的人都继承了一个他没选择的链接依赖；`#include` 与 `#link` 的分工当场作废 |
| 2 | `#link` 不带版本约束 | 加上版本号就是"无法验证的承诺"：没人能在编译期证明"这段代码只需要 >=2"，而错配的两种真实形态（abi 不等、ir 主版本不等）已由严格比较挡住 |
| 3 | 用了内建却没 `#link` ⇒ **编译期** `E3016` | 留到链接期报，诊断里只剩符号名，看不到写错的那一行；而"编译期能早报的错不该留到链接期"是 T08 的门禁原则 |
| 4 | `.h` 的 workflow 签名 `-> Type` 必填，`.hermes` 可省（推断） | 两边都可省 ⇒ 头文件里的返回类型只能靠猜，等于契约缺一角；两边都必填 ⇒ 与用户语法参考冲突，且定义处是重复信息 |
| 5 | `entry` 必须落在符号表里（`E3024`） | `hermes run` 按名字从符号表解析入口，非导出的 entry 会让"明明标了 entry 却找不到入口"变成运行期谜题 |
| 6 | `catch` 内允许 `return`（撤销 v3.0 L6） | 语言没有可变绑定，恢复出来的值无处安放 ⇒ 错误处理不可写；且原理由对 `if`/`match` 同样成立，会推出荒谬结论 |
| 7 | workflow 名是**标识符** | 保留字符串字面量会让符号表同时接受带引号与不带引号两种拼写，`E_LINK_*` 的符号名出现二义 |
| 8 | `.h` 里**允许**非 `export` 声明（内部头文件） | 强制 `.h` 全部 export，则"两个 `.hermes` 共享一个私有类型"只能靠复制定义，而复制会让 ODR 检查失去意义 |
| 9 | `struct` 名在整 unit 内唯一（含私有） | 私有 struct 可以重名 ⇒ `$defs` 键（非限定名）出现二义 ⇒ 生成的 schema 二义 ⇒ 请求哈希不稳定 |
| 10 | 请求体是 input 类型的**实例本身**，不嵌 `{"input":…}` | 嵌一层后 CLI 的 `--input` 文件与 HTTP 体不同形，本地测试与线上验的不是同一件事 |
| 11 | `model.generate` 无类型实参返回 `string`（不是 `json`） | 返回 `json` 等于承认"没有 schema 也能拿到结构化结果"，每个字段访问都变成潜在 `ContractError`——正是强类型要消灭的东西 |

---

## 附录 A：诊断码索引

| 码段 | 类别 | 内容 |
|---|---|---|
| `E1001`–`E1008` | 词法 | 编码/BOM/长度/保留名/浮点/金额/字符串/转义 |
| `E1009`–`E1010` | 路径与指令 | include 路径穿越（`.`/`..`）/ `#link` 模块名不合法（含 `.hbc` 后缀、绝对路径） |
| `E2001`–`E2015` | 语法 | 括号/类型实参/提前 return/参数纯性/precision 字面量/retry 字面量 |
| `E2016`–`E2017` | 语法 | **`method` 不是字面量 / 不在 6 个合法方法内** |
| `E2018` | 语法 | **`auth` 子句的右值不是 `env(名)` 单实参形式** |
| `E3001`–`E3010` | 模块 | std 不存在/循环/深度/模块数/`.h` 限制/未知 backend |
| `E3012` | 可见性 | 同一符号 `export` 两次（声明 + 定义都写了） |
| `E3013` | 契约 | `input` 的类型没有 `export` |
| `E3014` | 契约 | 声明与定义不一致（返回类型/参数/`entry`/`backend` 默认值） |
| `E3015` | 指令 | `#include` 了 `.hermes`（实现文件），应用 `#link` |
| `E3016` | 指令 | 用了 `tool.call`/`model.generate`/`json.*` 但没 `#link` 对应 std |
| `E3017` | 指令 | `#link` 出现在 `.h` 里 |
| `E3018` | backend | `env()` 的实参不是字符串字面量（含默认值缺失） |
| `E3019` | backend | backend 地址含凭据（`user:pass@`） |
| `E3020` | backend | `.h` 里写了 backend **定义**（带地址），只能写声明 |
| `E3021`–`E3022` | entry | 本 unit 没有 `entry` / 有多个 `entry`（均需 `--workflow`） |
| `E3023` | entry | `input` 参数没写类型 |
| `E3024`–`E3025` | entry | `entry` 未进符号表 / `--workflow` 指定的名字不存在 |
| `E3030`–`E3032` | 反向融合 | 类型不能跨 C ABI 边界（`json`/`vector`/泛型）/ `extern` 没有匹配的 `#link native` / `from` 头不是纯 C ABI |
| `E3101` | workflow | 一个编译单元只能有一个 `workflow`（声明与定义各写一次才合法） |
| `E4001`–`E4020` | 类型 | 返回缺失/可空/算术/比较/注解/字段/重复 struct 名/match 主表达式 |
| `E4021`–`E4023` | 类型 | 各 `return` 类型无法统一 / `return` 了 tuple / 错误对象逃出 `catch` |
| `E4024` | 类型 | `unlimited` 出现在**值**位置：它只是上界标记，唯一合法位置是 `model.agent(max_steps = unlimited)`（§6.4）。**阻塞构建** |
| `E4025` | 类型 | `fallback` 降级块里的 `return` 类型与主表达式不一致（§5.4.1）。降级路径的类型错只会在出错那天暴露，所以必须在编译期拒。**阻塞构建** |
| `E4026` | 类型 | `fallback` 降级块里给 `const` 绑定重新赋值（§5.4.1）：`const` 在整个作用域内只读，降级路径不是例外。**阻塞构建** |
| `E4027` | 类型 | `if` / `while` 的条件不是 `bool`（§5.3，无 truthiness）：`if (1)` / `if (json.as_int(x))` 编译期拒。编译器原先不查，直到执行器才因"条件非 bool"失败。**阻塞构建** |
| `E4028` | 类型 | 在 `for` 循环**已结束后**引用循环变量（循环变量只在循环体内存在，出循环即回收，§3.1）。**阻塞构建** |
| `E4029` | 类型 | **旧 `float` 标签的纵深防线**（v2.0.0 起**不再是**「所有浮点字面量被拒」）。正常路径的浮点类型名只有 `double`，字面量 `1.5` / `1.5e2` / `1e3` 编成 IR 位模式 `{"k":"double","bits":"<16 位小写 hex>"}`；IR 字节里没有 JSON 浮点数字，V-7 / canonical / 签名链都不受影响。这道码只盯 `LitExpr.text == "float"` 的**旧标签**：手工把标签改回 `"float"`（或直调 C ABI 绕过诊断）时，codegen 的字符串兜底会产出 `{"t":"string","v":"float"}` —— 合法 IR、校验全过、运行期返回字符串 `"float"`，是纯静默损坏。**三道防线**：`check_type.cpp::scan_float_` 全 AST 扫描（含 `subs`）+ `codegen.cpp::gen_expr` + workflow 参数默认值折叠；因为 `hm_codegen*` 不因 DiagBag 里有 error 而短路。**阻塞构建**（§2.5、§4.1） |
| `E4030` | 类型 | 用户函数调用的**实参类型**与形参声明类型不匹配（语言无隐式参数转换）：`add("x", 1)` 这类调用在编译期被拒。**元数不一致**不在此码范围，它由装配期 `E_IR_CODEGEN`（`function add takes 2 argument(s), got 1`）报，避免与装配期诊断重复。**阻塞构建** |
| `E4031` | 类型 | 数组下标表达式的类型不是整数（`auto k = "a"; xs[k]`）；字符串字面量下标 `xs["k"]` 走 json 常量键路径、不在此列。此前误用 `E4023`（与「错误对象逃出 catch」冲突），现归还。**阻塞构建** |
| `E4032` | 类型 | 被索引的值不是数组也不是 json（`auto n = 5; n[0]`）。此前误用 `E4022`（与「`return` 了 tuple」冲突），现归还。**阻塞构建** |
| `E4033` | 类型 | `double` 的 `==` / `!=`（`input.x == input.x`）：二进制浮点的精确相等不是稳定谓词 —— 舍入让数学上相等的两个值比出不相等，NaN 与自己也不相等（`x == x` 可能为假，`x != x` 悄悄成了语言里唯一的 NaN 判据）。编译期证不出来就拒，而不是让它静默返回 false。`< <= > >=` 与 `double` 的算术不受影响；`int`/`cost` 与 `double` 混用另由 `E4006` / `E4008` 拒。**阻塞构建**（§4.4） |
| `E4034` | 类型 | 错误字段**规范里有、运行期还没带上**（`catch (HttpError e) { e.url }`）：§7.1.1 声明了这个字段，但执行器的错误值目前只填了 `status`。既不报 `E4016`（那是在说"没有这个字段"，会把人赶去查拼写），也不放行（编得过、运行期死在 `field does not exist`，而那是普通 error，`catch` 接不住）。运行期真把某字段填上时只改 `check_type.cpp` 字段表的 `ready` 标记。**阻塞构建**（§7.1.1） |
| `E5001`–`E5010` | 语义 | 不纯参数/作用域/元组长度/嵌套上限/预算不可捕获/幂等/精度降级/idem_key 不纯/`parallel` 体内写体外绑定 |
| `E5007` | 语义 | `retry` 子树内的 `tool.call` 无法证明幂等：既不是 `GET`/`HEAD`、也没有 `idempotent = true`、也没有纯 `idem_key`（§5.7，由 `check_idem.cpp` 在类型检查后判）。**阻塞构建** |
| `E5009` | 语义 | `idem_key` 不是纯表达式（含模型输出、工具响应或任何非 `input` 派生的值）：重试时键会变，服务端去重失效（§5.7）。与所在位置无关，凡是写了 `idem_key` 就查。**阻塞构建** |
| `E5010` | 语义 | `parallel` 分支内对体外绑定赋值 / 自增（`x = …` / `x++`）：写入落在分支的父作用域副本上，恒不生效 ⇒ 编译期硬错，无迁移期、无开关（§5.2）。**阻塞构建** |
| `E6001`–`E6003` | 上界 | 扇出积超限/步数超限/预算不可覆盖 |
| `W2004` | 警告 | `model.agent(max_steps = unlimited)`：这个 agent 的步骤上界不再是编译期常量 ⇒ `max_spans` 不再是 trace 长度的可证明上界，预算也不能静态预留（§6.4）。**不阻塞构建** |
| `W2003` | 警告 | `#include` / `#link` 行末写了多余的分号：分号对指令行**可选**，带了仅冗余，发此警告以引导去掉（与 C++ 对 `#include <x>;` 的 `extra tokens at end of #include directive` 对齐）。**不阻塞构建**（§2.7、附录 D） |
| `W3013` | 警告 | 私有 `workflow` 恒为死代码（语言无跨 workflow 调用）：要么 `export`，要么删 |
| `W4002` | 警告 | `model.generate` 无类型实参（无输出结构约束， 做事后校验） |
| `W5001`–`W5002` | 警告 | 并发超 backend / 精度降级 |
| `W5004`–`W5005` | 警告 | HTTP 状态码比较（启发式，§12.1 L3）/ `input: json`（入口无结构契约） |
| `W6001` | 警告 | trace 上界可能超 `max_bytes` |
| `E_LINK_*` | 链接 | 由 Go 链接器产出（`ARCH` §5.10、`IR.md` §9） |
| `E_IR_*` | IR 产物 | 由 `internal/ir/validate.go` 在**加载期**产出（`IR.md` §12 V-1…V-12），独立于前端诊断码 |
| `E_IR_RETRY_UNPROVEN` | IR 产物 | **V-12**：从 `retry.body` 沿节点引用边可达的 `call_tool`，其 tool 的 `idem.mode` 不是 `readonly` / `idem_true` / `idem_key`（含 `none`、字段被删、未知值）⇒ 拒绝加载。这是 `E5007` 的**产物侧**防线：手写或篡改 IR 不能绕过编译期幂等证明（`§5.7`、`IR.md` §12）。**阻塞 `hermes run`（退出码 4）** |
| `W_IR_PANEL_TRUNCATED` | IR 产物 | 加载期诊断条数超出面板上限，**后面的被丢弃**（面板有界才能保证报错输出本身不炸内存）。只丢展示、不丢判定。**不阻塞**，但"少报了几条"会写在这条警告里 |

> **警告码的实现状态（2026-11-10 核实，W5005 于 v2.0.0 收口实装）**：本表里**真正会被产出的警告是 `W2003`、`W2004`、`W5004`、`W5005` 与 `W_IR_PANEL_TRUNCATED`**（全仓 `"[EW][0-9]{4}"` 字面量的机器清点为证）。`W3013`、`W4002`、`W5001`–`W5003`、`W6001` 是**规范预留、尚无产出点** —— 它们描述的是"将来要发的诊断"，不是"现在会看到的诊断"。写在这里而不是删掉，是因为码段本身是契约（§11.3 第 3 条要求每个码有索引）；但**别拿这张表判断"某个写法会不会被告警"**，那个问题的答案只在编译器里。`--deny-warnings` 目前能升级的就是 `W2003`/`W2004`/`W5004`/**W5005**。

**已废弃的码（不复用，防止旧 golden 与新含义撞车）**：

| 码 | 原含义 | 废弃原因 |
|---|---|---|
| `E2012` | catch 体内不允许 `return` | v3.1 撤销（§5.4），catch 体与普通块同权 |
| `W5003` | `input` 未声明类型 | v3.1 改为硬错 `E3023` |
| `W4001` | 浮点相等（「允许但警告」） | **从未实现**：全仓只有文档提到它；改名前的 `float == float` 曾静默通过（产出两个十进制文本的字符串比较）。2026-11-10 拍板改为硬错 `E4033`（v2.0.0 起即 `double == double`）。保留此行是为了不复用该号 |

## 附录 B：`hermes fmt` 的语义（明确降级）

`fmt` **不做** AST 重排（不做"把 `if` 拆行、把长调用折行"这类重写）。它做**token 流规范化**：

| 做 | 不做 |
|---|---|
| 统一 4 空格缩进（按 `{`/`(`/`[` 深度） | 折行/合并行 |
| 运算符两侧单空格、逗号后单空格 | 重排声明顺序 |
| `;` `{` 前无空格 | 删/改注释内容（只调整其行内位置） |
| 行尾统一 `\n`，去行尾空白，文件末尾恰好一个 `\n` | 改写字符串内容（含插值） |
| 保留所有注释 | 重排 `struct` 字段 |
| **指令区规范化**：`#` 顶格；`#include` 全部在前、`#link` 全部在后（各保持原有相对顺序）；`#link` 的 `;` 前不留空格 | 不展开 include、不改 `<>`/`""` 形式、不规范化模块名/路径 |

指令区排序是 v3.1 新增的**唯一**一处"重排"：它是**两个不相交的稳定分区**（include 之间、link 之间各自保序），所以幂等性仍是构造性的——第二次格式化的输入已经是"include 全在前、link 全在后"，分区不再变化。而声明与注释的保序重排做不到这个保证，这也是它仍然不做的原因。

**幂等性的形式化保证**：`fmt` 的输出经词法分析得到的 token 序列与原 token 序列**恒等**（含注释与位置关系），而 `fmt` 是"token 序列 + 缩进深度"的纯函数 ⇒ `fmt(fmt(x)) == fmt(x)` 必然成立，不依赖测试。测试仍然写，但它验证的是实现、不是定理。

**为什么现在不做 AST 级格式化**：AST 级格式化需要在 AST 上保留注释的**全部**位置信息（前置/后置/行尾/独立行），并在重排时重新分配——这是大约 400 行的细节工程，且它的失败模式（注释跑到错的地方）比"不格式化"更糟。Phase 2 再做。Phase 1 的 `fmt` 已经是"两次输出字节相等"可验收的，R-HF-01 不会开天窗。

## 附录 B：`memory` / `vector`（存储内建，D-035）

四个内建，两个保留命名空间。它们是**求值即副作用**的调用，可以出现在变量声明的右侧，
也可以直接作为语句（`exprstmt`）。

| 调用 | 返回 | 语义 |
|---|---|---|
| `memory.get(key = s)` | 任意 JSON 或 `null` | 键不存在返回 `null`，**不是错误** |
| `memory.put(key = s, value = v)` | `bool` | 原子写（临时文件 + rename） |
| `vector.put(id = s, text = s)` | `bool` | 调 `/v1/embeddings` 后存向量与原文 |
| `vector.query(text = s, k = n)` | 数组 `[{id, text, score}]` | 余弦相似度，按分数降序，同分按 id |

**存储位置是部署事实，不是语法**：`hermes run --store <dir>`、`serve --store <dir>`，
或环境变量 `HERMES_STORE`。不用这些内建的 workflow 不会被要求配存储 —— 惰性打开。

**为什么不做成声明**：声明会把部署参数写进源码，而 `backend` 用 `env()` 注入正是为了
让产物可分发。存储是同一类事实，所以走同一条路（运行时配置）。

**上界**：一次存储操作记 1 个 span；`vector.put` / `vector.query` 各记 1 次模型调用
（embedding 也是一次模型调用，要算钱）。

**embeddings 的地址**：优先用 workflow 的默认 backend（地址与鉴权都从它来），
否则回落到 `HERMES_EMBED_URL` / `GATEWAY_URL`。模型名默认 `text-embedding-3-small`，
可用 `HERMES_EMBED_MODEL` 覆盖。


## 附录 C：`match` 的臂

臂接受**字符串或整数**字面量，必须与主语同类型（`E4019`）：

```hermes
match (order.status) {
    case 200: { return "ok"; }
    case 404: { return "not found"; }
    default:  { return "error"; }
}
```

- **同类型才比较**：拿字符串臂去匹配整数永远不命中，而那种静默不匹配要到运行期
  才表现为"走了 default"。所以类型不一致在编译期就报错。
- **default 的判定是 `is_nullable`**，不是"字面量为空"。这一点改过一次：
  整数臂没有字面量，用"空"判定会把每个整数臂都当成 default。
- IR 里字符串臂写 `lit`、整数臂写 `num`，两者都没有才是 default 臂
  （见 `schemas/ir.schema.json` 的 `matchArm`）。


## 附录 D：分号规则（对齐 C++）

| 构造 | 分号 | 说明 |
|---|---|---|
| `#include` / `#link` | **可选**（带则 `W2003`） | 预处理指令，与 C++ 的 `#include <x>` 一致；`#link <std/net>;` 仍能编，但报 `W2003`（见 §2.7） |
| 变量声明 / 赋值 / `return` / 表达式语句 | **必须** | 与 C++ 的语句一致 |
| `struct` / `workflow` / `backend` / `tool` 定义 | 不带 | 块结构，与 C++ 的函数定义一致 |

`#link <std/net>;` 仍然能编，但会报 **`W2003` 警告**（clang 对
`#include <x>;` 也是警告：*extra tokens at end of #include directive*）。
警告不阻塞构建 —— 这是"能编，但建议改"的表达方式。
