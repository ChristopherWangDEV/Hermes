# 1.2.3 计划：`security` 策略（Allow / Deny）

> 状态：**部分实现**。已做：声明 / `with` / IR / 名字校验 / `tool.call` 编译期白名单 / `network.outbound` 域名（字面量 URL）。
> 未做：`file.*`（语言里没有这个能力，写出来会**编译报错**，不是静默忽略）、插值 URL 的运行期拦截。

## 目标形态

```hermes
export security AgentGuard {
    allow tool.call      = ["search_web", "get_weather"];
    deny  file.write     = ["/etc/passwd", "/var/"];
    allow network.outbound = ["api.openai.com"];
}

export workflow safe_agent() with AgentGuard entry -> string {
    auto res = tool.call("search_web", query = "最新新闻");
    return res;
}
```

默认**全部拒绝**（default deny）：没写 `allow` 的能力一律不可用。

## 关键界线：哪些能编译期拦，哪些只能运行期拦

| 能力 | 判定时机 | 为什么 |
|---|---|---|
| `tool.call` | **编译期** | `export tool` 是**封闭集合**，编译期就知道每个 tool 的名字 |
| `file.write` / `file.read` | **编译期**（字面量路径）+ 运行期（拼接路径） | 路径是字面量时能比；运行期拼出来的只能运行期比 |
| `network.outbound` | **运行期** | 域名在 `http.post(url = ...)` 里，URL 可能是运行期值 |
| `model.call` | 编译期 | backend 是声明的，名字封闭 |

这条界线必须写进文档：**"编译期拦不住的，运行期必须拦，而且拦的时候要报清楚"**。
只做编译期 = 一个 `network.outbound` 策略形同虚设。

## 编译期怎么拦

1. `security` 声明进 IR（新节点/新表：`security[]` + workflow 的 `with` 引用）
2. 类型检查阶段遍历 workflow 的调用点：
   - `tool.call("x")` 的 `x` 不在 `allow tool.call` 里 → **编译错误**（不是警告）
   - `file.write("/etc/passwd")` 命中 `deny` → 编译错误
   - `file.write(运行期值)` → 允许编译，但标记"需要运行期校验"
3. 未声明 `with` 的 workflow 不受策略约束（但可以全局 `--strict-security` 要求每个 workflow 都带策略）

## 运行期怎么拦

- 运行时（`hermesrt`）在**执行工具/网络/文件操作前**查同一份策略
- 命中 deny → 返回明确的 `ContractError`，trace 里记一条 `denied` span
- 不做"静默降级"：拒绝就是拒绝，带策略名与被拒的值

## IR 形状（草案）

```json
"security": [
  {"name":"AgentGuard",
   "rules":[{"effect":"allow","cap":"tool.call","values":["search_web","get_weather"]},
            {"effect":"deny","cap":"file.write","values":["/etc/passwd","/var/"]}]}
]
```

workflow 上加 `"security": "AgentGuard"`。

## 验收

1. 声明了 `allow tool.call = ["search_web"]` 后，调用 `tool.call("delete_database")` → **编译错误**
2. `file.write("/etc/passwd")` → 编译错误
3. `network.outbound` 只允许列出的域名，运行期访问别的域名 → 运行期错误 + trace 记录
4. 没声明 `with` 的 workflow 行为不变（向后兼容）
5. 策略名拼错（`with NoSuchGuard`）→ 编译错误，而不是静默放行

## 与红线的关系

零新增依赖。策略解析、检查、运行期判定全部是自有代码。
不引入外部分析器或沙箱框架 —— 运行期拦截复用已有的 exec/sandbox 路径。
