# Starter

最小但结构完整的 Hermes 工程。两份文件，各自只放该放的东西：

| 文件 | 放什么 | 为什么 |
|---|---|---|
| `app.h` | 类型、backend 声明、tool 声明、workflow 签名 | 这是**契约**：分发 `.hbc` 时对方只需要它 |
| `app.hermes` | `#link`、backend 地址、workflow 实现 | 这是**实现**：地址与链接输入是这一份的选择 |

## 为什么分成两个文件

- **地址不进头文件**：地址写死在库里，这份库就没法分发（`127.0.0.1` 是作者的机器，不是用户的）。
- **链接输入不进头文件**：`#link` 放在 `.h` 里，每个 include 的人都会继承他没选择的依赖 ——
  那正是 `#link` 要消灭的 C 式耦合。

## 它演示了什么

```sh
hermes build examples/starter/app.hermes -o app.hbc
hermes explain app.hbc        # 编译期就能看到上界
hermes run app.hbc --input '{"text":"…","user_id":"u1"}'
hermes header app.hbc --c -o app.h    # 生成对外 C ABI 头
```

- `model.generate<Intent>` —— 输出契约由 struct 生成，回答在进入编排前被校验
- `match` —— 分支
- `model.agent(goal, tools, max_steps)` —— 模型自己选工具，循环上界是常量
- `export tool` —— 一条声明同时喂给"模型看到的 schema"和"运行期执行的 URL"

`hermes build` 之后 `hermes explain app.hbc` 会告诉你：`max_model_calls 5`、
`max_tool_calls 4`、`max_spans 10`。这些数字是**编译期算出来的**，不是运行期统计的。
