# TRACE-SCHEMA —— trace 文件的字段语义

> 权威定义：`schemas/trace.schema.json`。本文解释**为什么是这些字段**，不复述字段表。
> 实现：`internal/trace/{span.go,writer.go}`、`internal/canon/json.go`。

## 1 一句话

一份 trace 必须能**单独回答**三个问题，不触网、不读源码：

1. 哪一步失败了？（`failed_at` + `step_id`）
2. 为什么？（`attr` ∈ 6 值闭集）
3. 花了多少、调了几次？（`micro_usd` / `tokens_*` / `provider` / `tool` 计数）

做不到这三条，trace 就只是日志的美化版。

## 2 三个不能动的约束

| 约束 | 原因 | 违反了会怎样 |
|---|---|---|
| **没有浮点** | 浮点的十进制表示与解析在不同实现间不保证一致 | 哈希跨机器漂移，验签随机失败 |
| **时间用纳秒整数** | `time.Time` 带时区与单调时钟信息 | 同一份数据在两地写出不同字节 |
| **`step_id` 必须能在源文件里 grep 到** | 否则 trace 指不回代码 | 排查时只能靠猜 |

## 3 块 = 一个事件

哈希链的块边界**就是事件边界**，不是定长切块。所以"第 k 块"= 第 k 个事件，
`hermes trace verify` 报"第 3 块被改"时，人能直接数到第 3 个 span。

代价：单个 span 的 canonical 字节不得超过 `native.ChainBlockMax`（1 MiB）。
超了**报错不截断**——截断会让"链对不上"这个信号消失，而那是防篡改的全部意义。
真需要放大 payload 时，应该拆成子 span，而不是把阈值调大。

## 4 为什么 `chain.block_hashes` 必须存

只有 `root` 时，verify 只能回答"变了"；存了逐块哈希，才能回答"**第几块**变了"。

这两个问题的价值差一个数量级：前者让你重跑，后者让你定位。

代价是文件变大（每个事件 64 字节十六进制）。对 trace 这种读多写一次的东西，值得。

## 5 截断：不完整 ≠ 被篡改

`max_bytes` 到达时写入 `trace.truncated` 事件并收口，然后：

- 文件仍是**合法 JSON**（否则等于没有 trace）
- 仍能通过 `verify`（证明"这份不完整的记录确实是我写的"）
- `chain.truncated == true`，读者不会把"缺了后半段"误读成"执行到这就结束了"

## 6 CLI

```
hermes trace verify  <trace.json> [--json]   # 0 通过 / 4 被篡改或无法解析
hermes trace summary <trace.json> [--json]   # 聚合失败步骤、归因、花费、调用次数
```

`--json` 可以放在路径**前后任意位置**（手工解析，不用 `flag` 包——
Go 的 `flag` 遇到第一个位置参数就停止解析，会把它当成第二个文件路径）。

## 7 已知缺口

- `schemas/trace.schema.json` 目前**没有**自动校验器。现在的守卫是
  `internal/trace` 的 `TestShapeMatchesSchema`（断言字段集合与数值类型），
  它验的是"我们产出的形状没漂"，不是"任意文件都符合 schema"。
  自写的 schema 校验器（T12 族）到位后应替换这段。
- `trace diff`（T21 ④）未实现。
