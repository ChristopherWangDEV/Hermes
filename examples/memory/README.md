# Memory / Vector

`memory.*` 与 `vector.*` 是内建，不是库。零依赖，位置由运行时给。

```sh
hermes build examples/memory/write.hermes -o write.hbc
hermes build examples/memory/read.hermes  -o read.hbc
echo '{"user":"u1","text":"我想退款"}' > in.json

hermes run write.hbc --input in.json --store /tmp/hermes-store   # 写
hermes run read.hbc  --input in.json --store /tmp/hermes-store   # 换进程读，还在
```

## 为什么是两个文件

**一个 unit 只能有一个 entry**。这里拆成两个 unit 是为了演示"写入 / 读取"两件事；
同一 unit 里放多个 workflow 是支持的（D-037），只是 entry 不能重复。
所以"写"和"读"分成两个 unit —— 顺带也更接近真实用法：读者本来就会在不同命令里调它们。

## 向量

```hermes
#link <std/model>

export struct Turn { string user; string text; }

export backend embedder : openai = env("EMBED_URL", "http://127.0.0.1:8080");

export workflow index(input: Turn, backend = embedder) entry -> json {
    vector.put(id = "{input.user}", text = input.text);
    return vector.query(text = input.text, k = 3);
}
```

`vector.query` 返回 `[{id, text, score}]`，按余弦相似度降序，同分按 id —— **稳定**比"随便"重要，
否则同一个查询两次返回不同顺序，用的人会以为是 bug。

## 边界

- `memory.get` 缺键返回 `null`，不抛异常：第一次对话本来就没有历史。
- 没有文档加载器：把 PDF / 网页变成文本这一步不在这里做，`vector.put` 只吃字符串。
- 上界：一次存储操作 1 个 span；`vector.put` / `vector.query` 各记 1 次模型调用。
