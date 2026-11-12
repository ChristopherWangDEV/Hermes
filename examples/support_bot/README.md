# support_bot —— 首个验收样例（必须逐字编译通过）

这个目录是 `LANGUAGE.md` §1.4 的黄金样例落盘版，也是 `DECISIONS.md` D-014 修订后完整示例的实现。
**`hermes build` 与 `hermes run` 都必须能跑通**；任何语法改动只要让它编不过，就是回归。

## 1. 四个文件，各自对应 C 的什么

| 文件 | 对应 | 内容 | 谁写 |
|---|---|---|---|
| `models/order.h` | `.h` | `export struct OrderInfo` | 手写或 `hermes header` 生成 |
| `models/intent.h` | `.h` | `#include "order.h"` + `export struct Intent` | 同上 |
| `support.h` | `.h` | 全部对外契约：`#include`、`export backend homonoia;`、`SupportInput`、workflow 签名 | `hermes header support_bot.hbc` 生成后手改 |
| `support.hermes` | `.c` | `#link`、backend 地址定义、workflow 体 | 人写 |

`support.h` 里**没有** `#link`，`support.hermes` 里**没有** `#include <std/*.h>` 的重复——
后者通过 `#include "support.h"` 传递得到。这不是风格问题：

- `#include` 只给**声明**（类型信息），等价于"我知道它长什么样"。
- `#link` 才给**实现**，等价于"我用到它的代码"。写在 `.hermes` 里，链接输入就是 root unit 自己的选择；
  写在 `.h` 里，每个 include 者都会继承它没选择的链接依赖——那正是 `.hbc` 静态库模型要消灭的 C 式耦合。
- 因此 `#link` 出现在 `.h` 里是编译错误（`E3017`），`#include "x.hermes"` 也是编译错误（`E3015`，用 `#link` 引库）。

没有 include guard：include-once 是语言的语义保证（按解析后真实路径去重），不是靠预处理宏。

## 2. 验收命令

```bash
# ① 编译。产物 support_bot.hbc（静态库）+ 由它生成的 support.h
./build/hermes build examples/support_bot/support.hermes -o build/support_bot.hbc

# ② 看链接输入被编进了归档（D-014.3：看源码/看归档就知道依赖哪些库）
./build/hermes ar t build/support_bot.hbc
#   support_bot
#   requires: <std/net> <std/model> <std/runtime>
#   abi_version: 2   ir_version: 2

# ③ 断网跑（fake server 在 127.0.0.1:<port>，由 tests/fakeserver 启动）
HOMONOIA_URL=http://127.0.0.1:18080 ./build/hermes run build/support_bot.hbc \
    --input examples/support_bot/input.json \
    --trace build/trace.json
#   省略 --workflow：本 unit 恰好一个 entry（support_bot）

# ④ 两个可静态断言的事实，不需要运行
./build/hermes explain build/support_bot.hbc
#   workflow support_bot  (entry)
#     bounds: max_model_calls=2  max_tool_calls=2  max_steps=... max_concurrency=2
#     prompt #1 依赖: input.text
#     prompt #2 依赖: order.amount
```

`②` 里的 `requires` 就是链接器据以校验版本的表：目标 `.hbc` 的 `abi_version` 必须与编译器**完全相等**，
`ir_version` 主版本必须相同，否则 `E_LINK_ABI_MISMATCH`（`IR.md` §9.5）。

## 3. 为什么 `parallel` 里 `history` 没用还不删

它不产生诊断（Phase 1 无 liveness 分析，`LANGUAGE.md` §12.1 L7）。理由：`parallel` 的分支常常
**为了副作用而取值**（HTTP 调用本身就是要发生的），把值丢掉是合法写法；逼作者写 `_` 反而丢位置信息。
上界计算不受影响——`max_tool_calls=2` 由 fork 宽度决定，与是否使用无关。

## 4. `order.status` 不是 HTTP 状态码（这是本样例专门保留的坑）

`tool.call` 默认契约下，**非 2xx 直接抛 `HttpError`，不返回业务值**（D-010.2）。
所以：

```h
// ✗ 永远不成立：404 时 order 根本没被绑定，执行流在 tool.call 那行就跳走了
if (order.status == 404) { ... }
```

`OrderInfo.status` 是**业务**字段（404 这个值只可能来自一次 200 响应里的业务码，语义上说不通）。
要处理"订单不存在"，必须捕获：

```h
try {
    auto order = tool.call<OrderInfo>(url = "...", method = "GET");
} catch (HttpError e) {
    if (e.status == 404) { return "订单不存在，请核对订单号"; }
}
```

这就是 `HttpError` **必须**携带 `status` 字段的原因（`LANGUAGE.md` §7.1）：否则"订单不存在"这个业务上
最常见、最需要给用户一句人话的分支，在语言里根本写不出来。
编译器对上面那个 `if` 只能发启发式警告 `W5004`，不能报错——它无法区分"业务字段恰好叫 status"。
这条松点写在 `LANGUAGE.md` §12.1 L3，不假装能静态判定。

上面这段在 `hermes run`（解释执行）与 `hermes compile`（AOT / C 后端）下**都可用**：两条路径的 `e.status` 都读到**真实 HTTP 状态码**。AOT 侧剩余的限制只有一条——规范里写了、但运行期还没带上的字段（如 `e.url`）仍按 `E4034` 拒绝，不会静默给 `null`。规范见 `docs/LANGUAGE.md` §7.1.1 与 `docs/AOT-ERROR-OBJECT.md`。

如果**不想接异常**、只想把特定状态码当成正常返回值，仍可用 `allow_status = [404]` 把它变成 `HttpResult<T>`（§5.6）——这是「正常返回值」的另一种写法，不是 AOT 下唯一的出路。

## 5. 期望的 trace 形状（编译期已知，不需要跑两遍比对）

```
run                                 1
├── workflow support_bot            1
│   ├── model.generate (exact)      1
│   └── match/refund
│       ├── parallel                1
│       │   ├── tool.call GET       1
│       │   └── tool.call GET       1
│       └── model.generate (fast)   1
```

每个 IR 节点在运行期恰好对应一个 span，所以这个树形是 `economics` 与 `max_steps` 的同一份事实
（`IR.md` §11）。`hermes run --trace` 输出的 `spans` 数组条数必须落在 `bounds.max_spans` 内——
这是测试可以直接断言的不变量，不需要 golden 文件。
