# Hermes IR 与归档格式规范 v1.5

| 项 | 内容 |
|---|---|
| 文档 | IR v1.5（Phase 1 IR / 容器 / 签名基线；v1.0 → D-014 → D-023 → D-026 → D-026 编号规则 → 2.0.0 函数） |
| 状态 | **规范性**。IR 字段名与二进制布局以本文为准 |
| 上游 | `DECISIONS.md` D-009 / D-011 / D-012 / D-013 / **D-014**；`docs/LANGUAGE.md`（语义） |
| 机器校验 | `schemas/ir.schema.json`（本文 §12 是它的字段清单）、`schemas/hbc-index.schema.json` |
| 一句话 | IR 是**可独立审计的执行计划**：JSON、无浮点、每个节点可映射回源码行号；`.hbc` 是**可链接的归档**，签的是索引与成员哈希，不是成员全文。 |

### 变更记录

| 版本 | 改动 |
|---|---|
| v1.0 | 首版。三身份、三条硬性质、20 种节点、四轴上界、`economics`、归档布局、签名事务、链接 |
| **v1.1** | **并入 D-014**：① 新增 `requires[]`（`#link` 的落地，§2/§7.2/§9.5）；② `workflow` 新增 `exported` / `is_entry` / `returns`，`input` 变为「struct 或 null」；③ 字段 `entry`（根节点 id）**改名为 `root`**（`entry` 现指语言级标注，同词两义会让审阅者与链接器都读错）；④ `backend` 的 `base_url` 换成 `addr{src,env,default}`，**存地址来源而非解析结果**（§3.3）；⑤ 剥离的根集合按产物分两套（§9.4）；⑥ `abi_version` 改为严格相等（§9.5） |
| **v1.2** | **并入 D-023 末节**：`backend` 新增可选 `auth{src:"env",env}`，**禁止 `default` 键**（与 `addr` 唯一差别，§3.3）；`auth` 与 `addr` 正交，`literal` 地址也可带 `auth`；加载期见 `default` → `E_IR_BAD_AUTH`。**注意 `ir_version` 仍是 1**：字段是**可选新增**，不写 `auth` 的 IR 与 v1.1 逐字节相同，**所以就 `auth` 这一项而言 golden 无需重生**。（`schemas/golden/ir-*.json` 仍待整体重生，但那是 D-020 的 `requires[].origin` 造成的——它是**必填**字段，与这里相反。别把两件事混成一件） |

| **v1.3** | **并入 D-026**：新增**第 21 种节点** `destructure`（tuple 解构）。`vars: [int\|null…]` 按位置接 parallel 的 k 元组，**`null` = `_` 丢弃位**；`SP(destructure)=SP(e)`（与 `let` 同构）。同时登记两种**读兼容**形态：A（`let.vars`）与 C（`parallel.bind`）。**写方只出 `destructure`**：三种形状语义相同而字节不同，若都能写出则同一程序有三个 `ir_sha256`，可重现性与 golden 比对当场失效。`ir_version` 仍为 1（节点种类是新增而非改名，且 v1.0 后没有任何 `.hbc` 流出） |


| **v1.4** | **补 §4.2「节点编号规则」**：`id == 下标` 只说编号连续，没说分配顺序；顺序不定则同一份源码可编出两份字节不同的 IR（`ir_sha256` 不同、golden 不可比、链接期 ODR 退化）。现钉死为**前序 DFS + 固定字段序**（每个 kind 的顺序见该表）。**这不是新特性，是把既有的隐式约定写成可判定的规格**；`ir_version` 仍为 1（约束的是 codegen 的输出，不是消费者的读法） |
| **v1.5** | **2.0.0 函数**：顶层新增 `functions[]`（unit 级函数表，**没有函数时键不出现**，保证 1.x 的 golden 字节不变）；新增第 22 种节点 `call_fn`（`fn` 是名字引用，`args` 是位置参数）。`SP(call_fn)=1`；只有**递归**函数保留 `call_fn`，含它的 workflow 标 `unbounded`（调用次数不可静态定界，`max_steps` / `max_spans` 都不是真正的上界）。非递归函数在 codegen 展开，产物里没有 `call_fn`。函数体里的 `model/tool/memory/vector` 调用**编译期拒绝**（否则 workflow 的调用次数上界会少算）。`ir_version` 仍为 1：新增字段与节点种类，不改变已有字段语义 |

> `ir_version` **仍是 1**：IR 格式在 v1.0 之后尚未发布过任何 `.hbc`（T09–T11 已作废重做），所以字段改名与新增不构成破坏性变更。若已有归档流出，`root` 这个改名就必须走 major+1。

---

## 1. 定位与三条硬性质

IR 有三个身份，缺一不可：

| 身份 | 要求 | 谁消费 |
|---|---|---|
| **执行计划** | 运行期不需要源码、不需要重新解析；`if`/`match`/`parallel` 的语义完全显式 | `internal/engine` |
| **审计对象** | 人能读懂；每个节点带源码位置；schema 字节可见；上界与成本可见 | 企业审计、`hermes explain` |
| **分发产物** | canonical JSON（字节稳定）；可签名；可链接；跨机器可重现 | `internal/archive`、`internal/link` |

三条硬性质（任何一条被破坏，设计即失效）：

| # | 性质 | 后果 | 强制手段 |
|---|---|---|---|
| P-1 | **无浮点** | canonical JSON 的字节稳定性（ARCH §5.3）。浮点的最短往返表示在 Go/Rust/C++ 之间有真实分歧 | `float` 字面量在 IR 里编码为**字符串**（`{"t":"float","v":"1.5"}`）；canonical 化遇 JSON 数字带小数点即失败 |
| P-2 | **每个节点带 source map** | "IR 可独立审计"不是口号：审计者要能从任意节点跳到源码行 | schema 里 `loc` 是 `required`；codegen 逐节点断言 `loc` 已填（缺失即编译器内部错误，不是可忽略警告） |
| P-3 | **节点图无环** | 控制流是结构化树（LANGUAGE §1.3） | codegen 后跑一次 DFS 环检测（`E_IR_CYCLE`，编译器 bug 兜底）；链接期再对所有成员跑一次 |

---

## 2. 顶层结构

一份 IR 文档 = **一个 unit**（一次 `hermes build` 的产物单元）。`.hbc` 归档装若干 unit。

```json
{
  "ir_version": 1,
  "unit": {
    "name": "support",
    "entry_path": "support.hermes",
    "modules": [
      {"path": "support.hermes",       "sha256": "…", "builtin": false},
      {"path": "models/intent.h",      "sha256": "…", "builtin": false},
      {"path": "std/net.h",            "sha256": "…", "builtin": true}
    ],
    "source_sha256": "…",
    "language_version": 1
  },
  "compiler": {"name": "hermes-parse", "version": "1.0.0", "parser_abi": 1, "opt": "O2"},
  "types":      [ /* §3.1 类型表，unit 内全部 struct */ ],
  "backends":   [ /* §3.3 */ ],
  "tools":      [ /* §3.4 */ ],
  "schemas":    [ /* §3.5 已生成的 JSON Schema（字节进请求，必须可审计） */ ],
  "workflows":  [ /* §4 每个 workflow 一个 DAG */ ],
  "functions":  [ /* §4.3 unit 级函数表；没有函数时整个键不出现 */ ],
  "exports":    {"workflows": [...], "types": [...], "backends": [...], "tools": [...]},
  "imports":    [ {"sym": "...", "kind": "workflow|type|backend|tool",
                   "loc": {"m": 0, "l": 3, "c": 1, "el": 3, "ec": 24}} ],
  "requires":   [ {"spec": "<std/net>", "kind": "std",  "abi_version": 1, "ir_version": 1, "origin": "source", "member_sha256": "…"},
                  {"spec": "models",    "kind": "local","abi_version": 1, "ir_version": 1, "origin": "source", "member_sha256": "…"},
                  {"spec": "<std/runtime>", "kind": "std", "implicit": true, "abi_version": 1, "ir_version": 1, "origin": "source"} ],
  "diag_counts": {"error": 0, "warning": 2}
}
```

| 字段 | 类型 | 说明 |
|---|---|---|
| `ir_version` | int | IR schema 的 major。**1**。见 §10 |
| `unit.name` | string | unit 名 = entry 文件名去扩展名，`[A-Za-z0-9_.-]{1,64}`。同一归档内 unit 名唯一 |
| `unit.entry_path` | string | 相对 include root 的 POSIX 路径。**不含绝对路径**（可重现） |
| `unit.modules[]` | array | 展开后的全部模块，**按 include 到达顺序**；`m` 索引即此数组下标。`.h` 与 `.hermes` 都在里面；`#link` 的模块**不在**里面（它不是被编译的源码） |
| `unit.source_sha256` | hex64 | `sha256( 按 modules 顺序的 (path\0bytes) )`，只含 `builtin=false` 的模块 |
| `unit.language_version` | int | 语言版本（= LANGUAGE.md 的 major）。**1** |
| `compiler.*` | object | 前端版本。不含时间戳（可重现） |
| `exports` / `imports` | object/array | 符号表来源。链接器只读这两个字段，**不读 `workflows` 内部**（解耦） |
| `requires[]` | array | **D-014.3 + D-020**：`#link`（或命令行 `-l`）的落地。链接器据此校验版本（§9.5）。`implicit: true` 只用于 `<std/runtime>`（错误类型与预算错误是语言的一部分，不可缺失，因此隐式 include + 隐式 link）。`origin: "source"\|"cli"` 记录依赖来自源码还是命令行（`--audit-strict` 拒绝任何 `cli` 来源，含输入成员自带的 `cli` 传染） |
| `diag_counts` | object | 只统计，不含文案（文案走诊断通道，不进 IR） |

**为什么 `imports` 存在 IR 里**：链接期要能回答"这个 unit 引用了哪些它自己没有的符号，各在哪一行"。若只在编译期内存里有，链接一个只有 `.hbc` 没有源码的环境就无法定位。

**`requires` 与 `imports` 的区别（别混）**：
- `imports` = **符号级**：我要用别人提供的 workflow/type，是谁、在哪一行。
- `requires` = **模块级**：我要链接哪些 `.hbc`/`stdlib` 成员，各是什么版本。

前者决定"能不能解析到符号"，后者决定"链接谁、版本对不对"。一个 `#link` 可以带来零个 `imports`（只用它的实现、不碰它的类型），这正是 `#include`/`#link` 分离后的正常形态。

---

## 3. 类型表、backend、工具、schema

### 3.1 类型表（`types`，独立于 unit 存在，D-013）

D-013 要求类型表独立：ODR 合并在**类型层**发生。因此 `types` 是 unit 顶层数组，不是 workflow 的子结构。

```json
{"id": 1, "name": "OrderInfo", "module": 1,
 "loc": {"m": 1, "l": 5, "c": 1, "el": 9, "ec": 2},
 "fields": [
   {"n": "order_id",     "t": {"k": "string"}, "opt": false, "ann": []},
   {"n": "amount_cents", "t": {"k": "int"},    "opt": false, "ann": []},
   {"n": "currency",     "t": {"k": "string"}, "opt": false,
    "ann": [{"name": "enum", "args": ["USD", "CNY"]}]},
   {"n": "note", "t": {"k": "string"}, "opt": true, "ann": [], "loc": {"m":1,"l":9,"c":5,"el":9,"ec":15}}
 ],
 "sha256": "…"}
```

| 字段 | 说明 |
|---|---|
| `id` | unit 内唯一，从 1 递增，**按声明顺序分配**（决定 `$defs` 键序，LANGUAGE §4.6） |
| `name` | 非限定名。跨 unit 同名 ⇒ 链接期走 ODR（§9.2） |
| `module` | 声明所在模块下标 |
| `fields[]` | 声明顺序（决定 schema 字节） |
| `fields[].t` | 类型表达式，见下表 |
| `fields[].ann` | 注解。`enum`/`pattern` 的 `args` 是字符串字面量 |
| `sha256` | **类型规范序列化**的哈希：`sha256(canonical_json({"name":…,"fields":[…]})`，**不含 `id`/`module`/`loc`**。ODR 比较用它 |

**类型表达式 `t`（递归但深度受限）**：

| `k` | 附加字段 | 说明 |
|---|---|---|
| `string` `int` `float` `bool` `json` `cost` | — | 基础类型 |
| `array` | `of`: 类型表达式 | 元素只能是 `BaseType`（不支持嵌套数组，LANGUAGE §4.1） |
| `struct` | `id`: 类型表下标 | 引用本 unit 的类型 |
| `httpresult` | `of`: 类型表达式 | 仅由 `tool.call` + `allow_status` 产生 |
| `tuple` | `of`: [类型表达式…] | 仅由 `parallel` 产生；不可命名/存储 |

### 3.2 大小与资源约束（防恶意/失控 IR）

| 项 | 上限 | 错误 |
|---|---|---|
| 单份 IR 字节 | 32 MiB | `E_IR_TOO_LARGE` |
| 节点数 | 200,000 / unit | 同上 |
| 嵌套深度 | 64 | `E_IR_TOO_DEEP` |
| 类型数 | 4,096 / unit | `E_IR_TOO_MANY_TYPES` |
| 字符串字面量总长 | 8 MiB / unit | `E_IR_TOO_LARGE` |

IR 是**可信度低于源码**的输入（可能来自第三方 `.hbc`），因此加载期必须校验：字段集、类型、上限、`loc` 边界（`l ≥ 1`、`el ≥ l`、`m < len(modules)`）、`id` 引用存在且无环。

### 3.3 backend

```json
{"name": "homonoia", "kind": "openai",
 "model": "qwen2.5-7b-instruct",
 "supports_tier": true, "supports_response_format": true, "max_parallel": 8,
 "exact_fallback": null, "schema_any": "union",
 "exported": true,
 "addr": {"src": "env", "env": "HOMONOIA_URL", "default": "http://127.0.0.1:8080"},
 "decl_loc": {"m": 0, "l": 11, "c": 16, "el": 11, "ec": 23},
 "def_loc":  {"m": 0, "l": 9,  "c": 1,  "el": 9,  "ec": 76}}
```

> 上面是 `examples/support_bot` 的真实 backend（无 `auth` 子句，所以没有 `auth` 键）。带密钥时多一个 `auth`，见下文。

`kind` ∈ `openai` | `anthropic` | `ollama` | `homonoia` | `custom`。`supports_*` 是**声明值**（可来自 `.h` 手写或 `hermes header` 生成）；`hermes probe` 的实测结果**不进 IR**（D-012），只在 build 时与声明值比对并发 warning。

**`addr` 存的是地址的「来源」，不是解析后的地址**（D-014.4）：

| `addr.src` | 形态 | 谁的地址 |
|---|---|---|
| `env` | `{"src":"env","env":"HOMONOIA_URL","default":"http://…"}` | 运行时由环境变量决定；`default` 是**字面量**（LANGUAGE §8.5），可为 `""` 表示"必须外部设置" |
| `literal` | `{"src":"literal","value":"http://127.0.0.1:11434"}` | 源码里写死的地址 |

**为什么 IR 里存环境变量名而不存解析结果**：IR 是**可重现构建**的产物，它的字节必须与机器无关。若把 `HOMONOIA_URL` 解析后的值写进 IR，同一份源码在两台机器上产出不同的 `.hbc`，`source_sha256`/签名/`request_sha256` 全部跟着变——可重现性当场失效，而且失效方式是"看起来只是 diff 了一个 URL"。解析后的值属于**运行事实**，进 trace（`backend.resolved`：只记 `scheme+host+port`），不进 IR。

**`auth`：密钥来源，与 `addr` 同形状但禁止 `default`**（D-023 末节）：

```json
{"name": "homonoia", "kind": "openai",
 "addr": {"src": "env", "env": "HOMONOIA_URL", "default": "http://127.0.0.1:8080"},
 "auth": {"src": "env", "env": "HOMONOIA_KEY"}}
```

| 字段 | 必须？ | 约束 |
|---|---|---|
| `auth` | 省略 = 该 backend 不带密钥 | `src` 只能是 `"env"`；**`default` 键不得出现**（`E2018` 在语法层就拦掉 `auth env("K","d")`），加载期再校验一次：见到 `default` 即 `E_IR_BAD_AUTH` |

三条与 `addr` 的差别，各有理由：

1. **`src` 只有 `env`**，没有 `literal`。密钥字面量没有安全的落盘方式——写进 IR 就写进了 `.hbc` 与签名产物，而那两样是要分发的。
2. **没有 `default`**。地址的本地默认是安全的（`127.0.0.1`），密钥没有安全默认值；给了默认就等于允许"没配也能跑"，而那只会在运行期变成一个 401。
3. **缺失时不是 warning 而是错误**：运行期解析失败 → `policy/denied` + `Retry:none`，且**一个请求都不发**（D-023 末节第 3 条）。不许悄悄发一个不带头的请求去换一个 401——那会把"忘了配密钥"变成"上游拒绝了我"，而这两件事的修复动作完全不同。

**`addr` 与 `auth` 是正交的**：`{"src":"literal"}` 的地址也可以带 `auth`。地址从哪来、密钥从哪来是两件事，禁止组合只会逼人为了挂个密钥去凭空发明一个装地址的环境变量。

两个 `loc`：`decl_loc` 指 `.h` 里的声明，`def_loc` 指 `.hermes` 里的定义。只写 `def_loc` 时说明该 backend 是私有的（没有导出声明）。

### 3.4 工具

```json
{"name": "erp.get_invoice",
 "method": "GET",
 "url_template": [{"lit": "https://api.internal.com/invoice/"}, {"path": ["invoice_id"]}],
 "allow_status": [],
 "timeout_ms": 5000,
 "idem": {"mode": "readonly"},
 "decl_loc": {...}}
```

`idem.mode` ∈ `readonly` | `idem_true` | `idem_key`（后者带 `"key": [parts]`）。

**`method` 恒为 6 个合法字面量之一**（`"GET"`/`"HEAD"`/`"POST"`/`"PUT"`/`"PATCH"`/`"DELETE"`，LANGUAGE §3.3 / D-014.7），且**省略时上游已在 codegen 里折叠成 `"GET"`**——所以 IR 里不存在"method 待定"。这条不是格式约定：`idem.mode="readonly"` 的正确性完全建立在"编译期已知这是只读方法"上。若 `method` 可以是运行期表达式，`readonly` 就只是一个注释，`retry` 包着 POST 也拦不住，D-010.3 的幂等证明当场失效。

`idem` 由 `native/cpp/check_idem.cpp` 在类型检查后计算（LANGUAGE §5.7），是**编译期事实**；运行期只读不算。

### 3.5 已生成的 JSON Schema（`schemas`）

```json
{"id": "Intent", "root": "Intent",
 "json": {"type": "object",
          "properties": {"action": {"type": "string"},
                         "order": {"$ref": "#/$defs/OrderInfo"}},
          "required": ["action", "order"],
          "additionalProperties": false,
          "$defs": {"OrderInfo": { … }}}}
```

**为什么 schema 原文进 IR 而不是运行期现算**：

1. 审计者要能看到"到底发给上游的是什么"。运行期现算则需重跑 codegen，审计不可复现。
2. schema 字节进请求体 ⇒ 进 `request_sha256`（trace 契约证据）。IR 里存一份，trace 里就能引用其哈希而不必存全文。
3. 运行期不依赖 codegen 逻辑（前端是 C++，运行器是 Go；少一次跨语言依赖）。

键序与 `$defs` 顺序由 LANGUAGE §8.3 定死；`json` 字段本身在 IR 里是普通 JSON 对象，**但参与 canonical 化时必须与生成时的字节一致**——因此 codegen 直接按规范键序写出，Go 侧 canonical 化只是复核（不一致即 `E_IR_SCHEMA_UNSTABLE`）。

---

## 4. workflow 与节点

### 4.1 workflow

```json
{"name": "support_bot",
 "exported": true,
 "is_entry": true,
 "backend": "homonoia",
 "input":  {"k": "struct", "id": 7},
 "returns": {"k": "string"},
 "params": [ {"n": "backend", "v": {"k": "backend", "v": "homonoia"}} ],
 "root": 0,
 "nodes": [ … §4.2 … ],
 "vars":  [ {"id": 0, "name": "intent", "t": {"k": "struct", "id": 8}, "decl": 3, "scope": 2} ],
 "bounds":     { … §5 … },
 "economics":  { … §6 … },
 "interp_deps": [ {"node": 7, "paths": [["input","text"], ["intent","action"]]} ],
 "loc": {"m": 0, "l": 9, "c": 1, "el": 30, "ec": 2}}
```

| 字段 | 说明 |
|---|---|
| `name` | **标识符**（LANGUAGE §3.1），`[A-Za-z_][A-Za-z0-9_]*`。跨 unit 同名 ⇒ 链接期错误（不是 ODR，见 §9.2） |
| `exported` | bool。**决定它是否出现在 `exports.workflows` 里**（D-014.2）。`exported=false` 的 workflow 链接期不可见、可被死代码剥离（§9.4） |
| `is_entry` | bool。本 unit 的默认入口（D-014.5）。**必须 `exported=true`**（`E3024`） |
| `backend` | 默认 backend 名（来自参数默认值 `backend = homonoia`） |
| `input` | `{"k":"struct","id":N}`，或 **`null`**（该 workflow 没有 `input` 参数）。`{"k":"json"}` 已不可能——`input` 的类型必填（`E3023`，D-014.6） |
| `returns` | 返回类型（§3.7 推断结果）。`.h` 里的 `-> Type` 与它比对 |
| `params[]` | 其余参数默认值；`v` 是**已折叠的常量**（`{"k":"string","v":"..."}`/`{"k":"int","v":8}`/`{"k":"backend","v":"homonoia"}`） |
| `root` | 根节点 id（永远是 `seq`）。**v3.1 把 v1 的字段名 `entry` 改成 `root`**：`entry` 现在是语言级标注（`is_entry`），同一个词表示两件事会让 IR 审阅者与链接器都读错 |
| `vars[]` | 变量表。`decl` = 声明该变量的 `let` 节点 id；`scope` = 作用域 `seq` 节点 id |
| `bounds` / `economics` | 每个 workflow 各一份（D-013：库内多个 workflow 各有各的上界） |

**`vars` 为什么必要**：IR 是扁平图，`var` 节点只带 `var.id`；没有变量表就无法回答"这个 id 的类型是什么、在哪声明、作用域多大"。审计者靠它读懂一段 DAG。

**`input` 的 schema 从哪来**：`input.id` 指向 `types` 里的 struct；它对应的请求 schema 在 `schemas` 里有一份，`id` 就是该 struct 的非限定名（LANGUAGE §4.8）。所以 `hermes serve` 不需要重新生成，直接取 `schemas` 里的那份字节——文档与校验是**同一份字节**。

### 4.2 节点（20 种，完整）

每个节点：`{"id": N, "k": "<kind>", "loc": {"m","l","c","el","ec"}, …kind 专属…}`。
**`loc` 对所有节点必填**（P-2）。`m` 是模块下标。

#### 语句/结构节点（10）

| `k` | 字段 | 语义 |
|---|---|---|
| `seq` | `kids: [id…]` | 顺序块。空块合法。**不产生 span** |
| `let` | `var: int`, `e: id` | 绑定。不产生 span（其值若为调用，span 归属该调用） |
| `if` | `c: id`, `then: id`, `elifs: [{c: id, body: id}]`, `else: id \| null` | 条件分支。**产生 1 个 span（`kind=block`）** |
| `match` | `subj: id`, `arms: [{lit: string, body: id}]`, `default: id` | 枚举分支。产生 1 个 span |
| `parallel` | `branches: [id…]`, `join: [id…]`, `join_k: int`, `max_par: int` | fork/join。**`join` 是按位置配对的元组元素表达式**（`join_k == len(join)`，D-031）。产生 `1 + k` 个 span（父 + 每分支） |
| `retry` | `n: int`, `delay_ms: int`, `body: id` | 重入块。产生 1 个 span + 每次尝试 1 个子 span |
| `try` | `body: id`, `catches: [{t: string, v: int, body: id}]` | 产生 1 个 span |
| `return` | `e: id` | 不产生 span |
| `nop` | — | 空语句占位（`;`）。不产生 span |
| `destructure` | `vars: [int\|null…]`, `e: id` | 元组解构（D-026）。`vars[i]` 接 `e` 所指 parallel 的第 i 个元素；**`null` = `_` 丢弃位**。产生 0 个 span（`SP(destructure)=SP(e)`，与 `let` 同构）。 |

> ### tuple 解构：三种编码，写方唯一（D-026）
>
> `auto [order, history] = parallel { … }` 是语言的核心语义（LANGUAGE §5.2 / E-7 / `E5003`），
> 而 v1.1 的 20 种节点里没有任何一种能表达"把 k 个变量按位置绑定到 k 元组"。
> v1.3 补上，并把它做成**读宽容、写唯一**：
>
> | 形态 | 形状 | 谁用 |
> |---|---|---|
> | **B（规范形）** | `{"k":"destructure","vars":[v0,v1],"e":<parallel id>}` | **codegen 只写这一种** |
> | A（读兼容） | `let` 上带 `vars:[v0,v1]` 而不是 `var` | 只可能出现在第三方 IR |
> | C（读兼容） | `parallel` 上带 `bind:[v0,v1]` | 同上 |
>
> **为什么写方必须唯一**：三种形状语义相同而字节不同。若都能写出，同一份程序就有三种
> `ir_sha256`，可重现性、golden 逐字节比对、链接期 ODR 全部失效。**读宽容是为了能读别人的
> IR，写唯一是为了自己的 IR 可比对**——两者不冲突，但也不能反过来。
>
> **为什么用 `destructure` 而不给 `let` 加字段**：`let` 的语义是"绑定一个值"，解构是"绑定 k 个
> 位置"。合并会让 `let` 出现两种形状，而"同一个概念两种形状"正是本项目反复出问题的地方
> （`call_model.args` 与 `call_json.args` 就因此被迫延迟解析）。
>
> **丢弃位用 JSON `null`，不用哨兵整数**：`-1` 已经表示 `input` 了。再造一个 `-2` 只会让两种
> 哨兵在代码里互相冒充，而它们的含义毫无关系。
>
> 长度一致性（`m == k`）仍是编译期 `E5003` 的事，不是加载期检查——加载期只知道形状。

#### 表达式节点（11）

| `k` | 字段 | 说明 |
|---|---|---|
| `lit` | `t: "int"\|"float"\|"bool"\|"string"\|"cost"`, `v` | `float` 的 `v` 是**字符串**（P-1）；`cost` 的 `v` 是**整数 micro-USD**；`int` 是整数（int64）；`bool` 是 true/false；`string` 是字符串 |
| `var` | `var: int` | 引用变量表 |
| `field` | `of: id`, `name: string` | struct/json 字段 |
| `index` | `of: id`, `key: string` | `j["k"]`（key 必须是字面量） |
| `agent` | `agent: {goal: id, tools: [string], max_steps: int}` | D-034：有界 agent 循环。`max_steps` 必须是常量 |
| `binop` | `op: string`, `l: id`, `r: id` | `add sub mul div mod eq ne lt le gt ge and or` |
| `unop` | `op: "neg"\|"not"`, `e: id` | |
| `interp` | `parts: [{lit: string} \| {path: [string…]}]` | 字符串插值。`path` 已解析为字段路径 |
| `call_model` | 见下 | 产生 1 个 span |
| `call_tool` | 见下 | 产生 1 个 span |
| `call_json` | `fn: string`, `args: [id…]` | `json` 内建，位置参数。不产生 span（纯本地） |
| `call_fn` | `fn: string`, `args: [id…]` | **递归**函数调用（2.0.0）。非递归函数在编译期展开，不会出现这个节点。`fn` 指向 `functions[]`（名字）；参数按形参顺序。产生 1 个 span，workflow 标 `unbounded` |
| `cast` | `to: 类型表达式`, `e: id` | 仅 `json.as_*` 的显式窄化结果占位（`dyn_site=true`） |

`call_model`：
```json
{"k": "call_model", "schema": 0,            // 指向 schemas[] 的下标；null = 无约束（json）
 "args": {"prompt": 7,                       // 节点 id（string 类型）
          "precision": "exact",              // 字面量，已折叠
          "max_tokens": 1024,                // 字面量或 -1（取 policy）
          "temperature": "0.8"},             // 字符串（禁浮点）
 "attempt_ctx": {"retry_node": 12, "attempt_index_upper": 3}}
```

`call_tool`：
```json
{"k": "call_tool", "tool": "erp.get_invoice",
 "args": {"url": 9, "method": "GET", "timeout_ms": 5000,
          "allow_status": [], "header": null, "body": null, "idem_key": null},
 "ret": {"k": "struct", "id": 1},            // T 或 httpresult<T>
 "attempt_ctx": {"retry_node": 12, "attempt_index_upper": 3}}
```

> `attempt_ctx` 是**编译期算好的**：让运行期不必回溯"我在第几层 retry 里"，也让 trace 能直接标 `attempt=2/3`。它同时是幂等证明（LANGUAGE §5.7）与上界乘法（§5）的落点。

#### 节点编号规则（codegen 的硬规则，不是实现自由）

`id == 下标` 只规定了"编号必须是连续的"，**没有规定分配顺序**。而顺序一旦不定，
同一份源码就能编出两份字节不同的 IR —— `ir_sha256` 不同、golden 无法逐字节比对、
`hermes link` 的 ODR 判定退化。所以在 v1.4 把它钉死：

**前序 DFS（父先于子），从 `root` 出发，按下列字段顺序递归。**

| kind | 访问顺序 |
|---|---|
| `seq` | `kids` 左→右 |
| `let` / `destructure` | 先 `e` |
| `if` | `c` → `then` → 各 `elifs[i]` 的 `c`、`body`（按数组序）→ `else` |
| `match` | `subj` → 各 `arms[i].body`（按数组序）→ `default` |
| `parallel` | `branches` 左→右 |
| `retry` | `body` |
| `try` | `body` → 各 `catches[i].body`（按数组序） |
| `return` | `e` |
| `field` / `index` | `of` |
| `binop` | `l` → `r` |
| `unop` / `cast` | `e` |
| `call_model` | `args.prompt` |
| `call_tool` | `args.url` → `args.header` → `args.body` → `args.idem_key`（null 跳过） |
| `call_json` | `args` 左→右 |
| `lit` / `var` / `interp` / `nop` | 无子节点（`interp` 的 part 不是节点） |

**丢弃位不占号**：`destructure.vars` 里的 `null` 不是节点，不参与编号。

为什么必须写死：编号顺序属于**产物字节**的一部分，和键序、空白同等重要。
不写死的话，C++ 前端换一种递归顺序就是一次静默的格式破坏 —— 没有任何测试会红，
因为两边都"合法"。

> 对照检查：`schemas/golden/ir-support_bot.json` 的节点顺序必须能由这条规则
> 逐条复现；否则 golden 记录的就不是规格，而是某次实现的偶然。

### 4.3 一个完整的 workflow（黄金样例的 IR，节选）

```json
{"name": "support_bot", "backend": "homonoia", "input": {"k": "json"}, "entry": 0,
 "nodes": [
  {"id": 0, "k": "seq", "loc": {"m":0,"l":18,"c":40,"el":41,"ec":2}, "kids": [1, 4, 5, 12]},
  {"id": 1, "k": "let", "loc": {"m":0,"l":19,"c":5,"el":19,"ec":35}, "var": 0,
   "e": 2},
  {"id": 2, "k": "field", "loc": {"m":0,"l":19,"c":21,"el":19,"ec":31},
   "of": 3, "name": "text"},
  {"id": 3, "k": "var", "loc": {"m":0,"l":19,"c":21,"el":19,"ec":26}, "var": -1},
  {"id": 4, "k": "let", "loc": {"m":0,"l":20,"c":5,"el":23,"ec":6}, "var": 1, "e": 7},
  {"id": 7, "k": "call_model", "loc": {"m":0,"l":20,"c":18,"el":23,"ec":6},
   "schema": 0,
   "args": {"prompt": 8, "precision": "exact", "max_tokens": -1, "temperature": "0.8"},
   "attempt_ctx": null},
  {"id": 8, "k": "interp", "loc": {"m":0,"l":21,"c":18,"el":21,"ec":40},
   "parts": [{"lit": "识别用户意图："}, {"path": ["user_input"]}]},
  {"id": 12, "k": "return", "loc": {"m":0,"l":39,"c":9,"el":39,"ec":45}, "e": 13}
 ],
 "vars": [{"id": 0, "name": "user_input", "t": {"k":"json"}, "decl": 1, "scope": 0},
          {"id": 1, "name": "intent",     "t": {"k":"struct","id":2}, "decl": 4, "scope": 0}],
 "bounds": {"max_steps": 9, "max_spans": 7, "max_model_calls": 2, "max_tool_calls": 2,
            "max_concurrency": 2, "max_retry_expansion": 1},
 "economics": { … }}
```

> `var: -1` 表示 `input`（隐式根变量，不在 `vars[]` 里）。这是一个刻意的特殊值：`input` 由 workflow 的 `input` 字段定义，不需要 `let`。

---

## 5. 上界（`bounds`）

定义见 LANGUAGE §6.1。IR 里**算好写死**，运行期只读不重算。

| 字段 | 含义 | 用途 |
|---|---|---|
| `max_steps` | 结构性节点数上界（人读） | build 报告 |
| `max_spans` | **span 数上界**（运行期断言用） | trace 字节上界（§5.1）、`W6001` |
| `max_model_calls` | 模型调用次数上界 | `parallel` 预留扇出上界 |
| `max_tool_calls` | 工具调用次数上界 | exec 并发上界（ARCH §6.2） |
| `max_concurrency` | 同时存活分支数上界 | 反压窗口静态上界 |
| `max_retry_expansion` | 最坏重试放大倍数 | 预算上界乘子 |

### 5.1 `max_spans` 的代数（与 `max_steps` 不同，是运行期断言）

```
SP(call_model)=1   SP(call_tool)=1   SP(call_fn)=1
SP(lit|var|field|index|binop|unop|interp|cast)=0
SP(nop)=0          SP(let)=SP(e)     SP(return)=SP(e)   SP(destructure)=SP(e)
SP(seq)     = Σ SP(kid)
SP(if)      = 1 + max(SP(then), max SP(elifs), SP(else))
SP(match)   = 1 + max(SP(arms), SP(default))
SP(try)     = 1 + SP(body) + max SP(catch_i)
SP(retry)   = 1 + (n+1) × SP(body)
SP(parallel)= 1 + Σ SP(branch_i)
```

`max_spans ≤ 64 × max_steps + 1` 恒成立（因为 `retry(n)` 的 n 有上限 16，`SP` 的放大不超过 `17^k`；实际取 `max_spans` 直接由代数算出，不用这个不等式，它只用于 sanity 断言）。

`max_steps` 用 LANGUAGE §6.1 的 `S`（每个调用/let/return 计 1）。

**运行期断言**：`len(trace.spans) ≤ max_spans`。若违反，写 `internal.invariant_violation` 并终止——这是编译器 bug，不是用户 bug，必须响亮。

### 5.2 trace 字节上界的静态判定

```
trace_bytes_upper ≈ max_spans × 平均 span 字节（保守 2048）+ 头部开销（4096）
```

build 时若 `trace_bytes_upper > policy.trace.max_bytes`，打 `W6001`（"trace 上界 N 字节超过配置 M，运行期会被截断"）。这是**静态可判定**的——因为 `max_spans` 是常量。这条警告的价值：让"trace 被截断"从运行期意外变成 build 期已知。

---

## 6. `economics`：成本上界（设计裁决 Q14）

### 6.1 它是上界，不是预测（先把话说死）

**能算出**：一段 prompt 模板的字节数上界、调用次数上界。
**不能算出**：这次运行实际会花多少钱。

原因只有一个：插值 `{input.text}` 的长度由运行时输入决定，语言无法知道。**任何声称"预估成本"的实现都是在猜**。本规范只提供可证的**上界**。

### 6.2 算法（整数运算，不许浮点）

```
假设（写在 assumptions 里，可被 workflow 参数覆盖）：
  input_bytes_upper          = 1 MiB      每个 string/json 值来自 input 时的长度上界
  tokens_per_byte_milli      = 1000       token/字节 上界，千分之（1000 = 1.0 token/byte）
  max_tokens_default         = 1024       调用未显式给 max_tokens 时

每个字符串字面量/插值节点：
  bytes_upper(lit)          = len(utf8)
  bytes_upper({path p})     = max_text_bytes(type(p))
      string → input_bytes_upper      json → input_bytes_upper
      int    → 20                      cost → 24        （"-9223372036854775.808" 含负号）
      float  → 24                      bool  → 5

每个 call_model 节点 i：
  prompt_bytes_upper(i)  = Σ 各 part 的 bytes_upper
  tokens_in_upper(i)     = ceil(prompt_bytes_upper(i) × tokens_per_byte_milli / 1000)
  tokens_out_upper(i)    = max_tokens(i)（−1 时取 max_tokens_default）

workflow 聚合（与 §5.1 的代数同构，因为分支互斥）：
  PB(seq)=Σ   PB(if/match)=max   PB(parallel)=Σ   PB(try)=body+max catch   PB(retry)=(n+1)×
  tokens_in_upper  = ceil(PB × milli / 1000)
  tokens_out_upper = OB（同代数，每 call 用其 max_tokens）

成本（仅当 price 已知）：
  in_units  = ceil_div(tokens_in_upper  × in_price_per_mtok,  1_000_000)   ← 向上取整
  out_units = ceil_div(tokens_out_upper × out_price_per_mtok, 1_000_000)
  cost_upper_micro_usd = Σ over call sites (in_units + out_units)
```

**向上取整是硬要求**：向下取整会让"上界"小于真值，上界性质当场失效（`ceil_div(a,b) = (a + b - 1) / b`，须防 `a+b` 溢出）。

### 6.3 `economics` 字段

```json
{"assumptions": {"input_bytes_upper": 1048576, "tokens_per_byte_milli": 1000,
                 "max_tokens_default": 1024},
 "prompt_bytes_upper": 41200,
 "tokens_in_upper": 41200,
 "tokens_out_upper": 3072,
 "model_calls_upper": 3,
 "cost_upper_micro_usd": 0,
 "pricing_source": "declared",     // declared | builtin | unknown
 "overflow": false}
```

| 情形 | 行为 |
|---|---|
| `pricing_source = "unknown"` | **省略 `cost_upper_micro_usd`**（不是写 0——写 0 会被误读成"免费"），build 打印"成本上界不可算：模型单价未知，仅给出 token 上界" |
| 溢出（乘算超 int64） | `overflow: true` + 省略成本 + `E6003`（build 失败：说明上界不可靠，不能用来做预算校验） |
| `cost_upper > budget.max_micro_usd` | `E6003`：**build 失败**——这个工作流理论上就装不进预算，不必等到运行期 |
| `tokens_in_upper + tokens_out_upper > budget.max_tokens` | 同上 |

### 6.4 运行期怎么用

| 用途 | 用法 |
|---|---|
| `parallel` 预留 | 用各分支的 `tokens_in_upper`（而非 costs），因为预算账本按 token + micro-USD 双轨 |
| 成本告警 | 运行结束把 `spent` 与 `cost_upper` 一起写 trace，**永不**把二者混算 |
| **禁止** | 用 `cost_upper` 作为"预计花费"展示给最终用户（那是错的；只在 `hermes build` 与审计报告里出现，且必须标注"上界"字样） |

---

## 7. 归档 `.hbc`（D-013）

### 7.1 二进制布局

```
偏移    长度   内容
0       4      magic "HBC\x1a"
4       2      u16le archive_version (= 1)
6       2      u16le flags           bit0 = stripped
8       4      u32le member_count
12      8      u64le payload_off
20      8      u64le payload_len
28      8      u64le index_off
36      8      u64le index_len
44      32     u64le 之外的 index_sha256（32 字节原始摘要，便于不解析 index 就能比对）
76      52     reserved（必须全 0；非 0 即 E_HBC_RESERVED）
128     …      payload 区：各成员 IR 字节，每成员 8 字节对齐
        …      index 区：canonical JSON（§7.2）
末尾-84  84     footer：u8 alg(=1) | u8[3] rsvd | u8[16] key_id | u8[64] sig
```

- `footer` 固定 84 字节，位置 = `file_len − 84`。`key_id` = `hex(sha256(pubkey))` 的前 16 个 ASCII 字符。
- 大小上限 512 MiB；`member_count` ≤ 4096。
- 加载顺序：magic → version → 各段边界合法性（区间不重叠、不越界、按 8 对齐）→ `index_sha256` 比对 → 验签 → 逐成员校验 `ir_sha256` → 解析成员 IR。**先结构、后密码学、最后语义**（结构错时报"第 k 个成员长度越界"比"签名不对"有用得多）。

### 7.2 index（canonical JSON）

```json
{"archive_version": 1,
 "created_by": "hermes/1.0.0",
 "flags": 0,
 "members": [
   {"name": "support", "ir_off": 0, "ir_len": 18342, "ir_sha256": "…",
    "ir_version": 1, "abi_version": 1, "language_version": 1,
    "source_sha256": "…",
    "exports": {"workflows": ["support_bot"], "types": ["Intent", "OrderInfo"],
                "backends": ["homonoia"], "tools": []},
    "requires": [{"spec": "<std/net>", "kind": "std", "abi_version": 1, "ir_version": 1, "origin": "source", "member_sha256": "…"},
                 {"spec": "<std/model>", "kind": "std", "abi_version": 1, "ir_version": 1, "origin": "source", "member_sha256": "…"}],
    "imports": [{"sym": "models.UserInfo", "kind": "type",
                 "loc": {"m": 1, "l": 3, "c": 1, "el": 3, "ec": 24}}]}
 ],
 "types":   [{"name": "Intent",    "sha256": "…", "member": "support"},
             {"name": "OrderInfo", "sha256": "…", "member": "support"}],
 "symbols": [{"name": "support_bot", "kind": "workflow", "member": "support"},
             {"name": "Intent",      "kind": "type",     "member": "support"}],
 "entries": ["support_bot"],
 "provenance": [{"member": "base", "archive_sha256": "…", "key_id": "…"}]}
```

`kind` ∈ `workflow` | `type` | `backend` | `tool`。

**index 里没有任何哈希指向自身**——`index_sha256` 在**头部**（文件偏移 44），不在 index 里。这是刻意的：见 §8.1 的自引用陷阱。

### 7.3 `flags`

| 位 | 含义 |
|---|---|
| 0 | `stripped`：已做死代码剥离（`--no-strip` 时清零） |

未知位被置位 → `E_HBC_FLAGS`（拒绝加载）。这比"忽略未知位"安全：位通常意味着语义变化。

---

## 8. 签名事务（设计裁决 Q13）

### 8.1 自引用陷阱（先说这个，因为最容易做错）

`.hbc` 的 index 里**不能**有"本文件的 sha256"：index 参与了自身内容的哈希，方程无解。

因此：
- `.hbc` 的签名消息 = `"hermes.hbc.v1\0" ‖ index_canonical_bytes`。覆盖 index（其中含每个成员的 `ir_sha256`），**不覆盖 footer 自身**。
- `hbc_sha256`（整文件哈希）**只存在于 `.sealed.hermes` 的 manifest 里**——不同文件，无自引用。

### 8.2 两个产物、两个签名、一次事务

D-011 写"签名只签一次"，在归档形态下**不可实现**（自引用）。修订为：**一次事务、同密钥、两个签名、互相绑定**。这是对 D-011 的必要修正，已在此声明。

| 产物 | 签名消息 | 覆盖什么 |
|---|---|---|
| `app.hbc` footer | `"hermes.hbc.v1\0" ‖ index_bytes` | 归档索引 + 全部成员的 `ir_sha256` |
| `dist/app.sealed.hermes` footer | `"hermes.sealed.v1\0" ‖ manifest_bytes ‖ source_bytes` | 源码全文 + manifest（内含 `hbc_sha256`、`index_sha256`、`ir_sha256`、bounds、economics） |

**为什么 `.hbc` 不签成员全文**（D-013 的硬需求）：`hermes link` 必须能复用他人已签名的成员库，**而链接方没有成员库的私钥**。若签名覆盖成员全文，链接后必须用原私钥重签，或整个"分发到没有源码的生产环境"的链条断裂。签"索引 + 成员哈希"就够：索引断言"这些确切字节的成员在我的归档里"，而每个 `ir_sha256` 由原始归档的签名背书，可逐级回溯。

### 8.3 `sealed.hermes` 布局

```
偏移    内容
0       magic "SHRM\x1a" (4) | u16le seal_version(=1) (2) | u16le flags (2)
8       u64 source_off, u64 source_len
24      u64 ir_off,     u64 ir_len
40      u64 manifest_off, u64 manifest_len
56      reserved（8 字节，全 0）
64      source bytes（.hermes 源文件原文，UTF-8，逐字节）
…       ir bytes（entry unit 的 IR 原文，= .hbc 里那份字节）
…       manifest（canonical JSON，§8.4）
末尾-84  footer：u8 alg | u8[3] rsvd | u8[16] key_id | u8[64] sig
```

### 8.4 manifest

```json
{"seal_version": 1,
 "source_path": "support.hermes", "source_sha256": "…", "source_bytes": 4218,
 "ir_sha256": "…", "ir_bytes": 18342,
 "hbc_sha256": "…", "hbc_bytes": 20480,
 "index_sha256": "…",
 "entry_workflows": ["support_bot"],
 "bounds": {"max_spans": 7, "max_model_calls": 2, "max_tool_calls": 2, "max_concurrency": 2},
 "economics": {"prompt_bytes_upper": 41200, "tokens_in_upper": 41200,
               "tokens_out_upper": 3072, "pricing_source": "declared",
               "cost_upper_micro_usd": 0},
 "key_id": "…"}
```

**manifest 里没有时间戳、没有绝对路径、没有主机名**——否则同一源码在两台机器上产出不同字节（D-012 的可重现要求）。

### 8.5 "如何证明两者同源"（Q13 的核心问题）

四个哈希构成闭环：

```
sealed.manifest.source_sha256  ==  sha256(sealed 内 embed 的 source bytes)
sealed.manifest.ir_sha256      ==  sha256(sealed 内 embed 的 ir bytes)
                                ==  .hbc index.members[0].ir_sha256
sealed.manifest.index_sha256   ==  .hbc 头部 offset 44 的 index_sha256
sealed.manifest.hbc_sha256     ==  sha256(整个 .hbc 文件，含其 footer)
```

**验证流程 `hermes verify app.hbc dist/app.sealed.hermes`（五步，任一步失败即整体失败）**：

1. 结构：两个容器各自的 magic/版本/段边界/保留字节。
2. 验签：`sealed` 的签名（覆盖 manifest + source）；`.hbc` 的签名（覆盖 index）。
3. 交叉：`manifest.index_sha256 == hbc 头部 index_sha256`；`sha256(index_bytes) == 同一值`。
4. 交叉：`manifest.ir_sha256 == sha256(sealed 内 ir bytes) == sha256(.hbc 成员 0 的 ir bytes)`。
5. 交叉：`manifest.hbc_sha256 == sha256(.hbc 全文件)`；`manifest.source_sha256 == sha256(sealed 内 source bytes)`。

**为什么这构成证明**：`hbc_sha256` 覆盖了 `.hbc` 的 footer（含其签名），所以想让"sealed 指向另一个 `.hbc`"必须改 manifest，改 manifest 就破坏 sealed 的签名。反之想让"`.hbc` 换成另一个"必须同时改 manifest 与 sealed 签名。攻击者需要私钥。**而两个签名由同一次 build、同一密钥产出，因此不需要第三个字段来"声明它们同源"——哈希关系本身就是声明。**

**只拿到一个文件时能证明什么**：

| 只有 | 能证明 | 不能证明 |
|---|---|---|
| `.hbc` | 归档完整性 + 签名者身份 + "成员来自 source_sha256 这份源码"（若信任签名者） | 无法独立核对源码（不含源码） |
| `sealed.hermes` | 源码与 IR 的对应关系 + 签名者身份 + 当时的 bounds/economics | 无法核对分发出去的 `.hbc` 是否是同一份（不含 `.hbc` 全文，只有哈希） |

**这就是为什么必须两个产物都分发**——单靠任一个都有一半的链断了。D-011 说"二者缺一不可"，这里是它的形式化理由。

### 8.6 事务性（两个文件怎么算"一次成功"）

POSIX 无法原子地重命名两个文件。规则：

1. 写 `app.hbc.tmp` 与 `dist/app.sealed.hermes.tmp`，各自 `fsync`。
2. `rename(.hbc.tmp → .hbc)`，`fsync(dir)`。
3. `rename(sealed.tmp → sealed)`，`fsync(dir)`。
4. 顺序**不可交换**：先 `.hbc` 后 `sealed`。

崩溃在两个 rename 之间的结果是"新 `.hbc` + 旧/无 sealed"⇒ `hermes verify` **必然失败**（`hbc_sha256` 不符）。中间态只可能表现为"验证不过"，**不可能表现为"验证通过但两者不一致"**——这是安全的方向。

`--no-seal`（仅内部调试）跳过 sealed，此时 `hermes doctor` 与 `hermes run` 在生产模式（`HERMES_ENV=prod`）拒绝加载无 sealed 的 `.hbc`。

---

## 9. 链接（D-013）与"链接器为什么在 Go"

### 9.1 符号解析

输入：若干 `.hbc`（`-l` 或位置参数）+ 待解析的符号需求（来自各成员的 `imports`）。

```
符号命名空间：workflow / type / backend / tool 四类，**彼此独立**
（即一个 workflow 和一个 type 可以同名，不冲突；但同类内同名即冲突）
```

算法（迭代到不动点，最多 16 轮）：

```
need = ∪ 所有成员 imports
avail = 所有归档的 symbols
未解析 = need − avail            → E_LINK_UNRESOLVED（§9.3）
再对"已解析但可能引新需求"的成员并入 need，重复直到不再变化
```

### 9.2 重复符号

| 情形 | 行为 |
|---|---|
| 两个库导出**同名 workflow** | `E_LINK_DUP_WORKFLOW`：点名两个来源归档与成员。**不覆盖、不警告后继续**（静默覆盖会让"我链接的到底是谁"变成不可知） |
| 两个库导出**同名 backend / tool** 且**配置不同** | `E_LINK_DUP_TOOL` / `E_LINK_DUP_BACKEND`，给出两边的配置 diff |
| 两个库导出**同名 struct** | **ODR**：比较 `types[].sha256`（LANGUAGE §4.6 的规范序列化，不含 id/module/loc）。相同 ⇒ 合并为一份（保留先出现者的 id）；不同 ⇒ `E_LINK_ODR_MISMATCH` 并给**字段级 diff** |

ODR diff 的形状（验收项）：

```
error[E_LINK_ODR_MISMATCH]: struct OrderInfo 在两处定义不同
  成员 a.hbc:models/order.h
  成员 b.hbc:models/order.h
  字段级差异：
    amount_cents : int    (a.hbc)  ≠  cost  (b.hbc)
    currency     : string (a.hbc)  =  string (b.hbc)
  字段集合差异：a.hbc 独有 note;   b.hbc 独有 external_id;
  = 为什么: 两个库对同一个名字的结构有不同理解。若静默选一个，
             另一个库生成的 JSON Schema 与它自己的读取代码就对不上——
             这会在运行期表现为"字段莫名消失"，无法定位。
  = 怎么改: 重命名其中一个 struct，或让两个库依赖同一个 .h
```

### 9.3 未解析符号

```
error[E_LINK_UNRESOLVED]: 未解析的符号 "UserInfo"
  被引用位置：unit "support" (support.hermes L3:10)
  符号类别：type
  已链接的库导出：Intent, OrderInfo, support_bot
  = 怎么改（三选一）:
     ① hermes link models.hbc app.hbc -o out.hbc   —— 补上提供该符号的库
     ② hermes build app.hermes -l models -o app.hbc
     ③ 若 UserInfo 其实应在本 unit 内，检查是否漏了 #include "models/user.h"
```

### 9.4 死代码剥离

**两种产物、两套根集合**（v3.1 按 D-014.2 分清；v1 只有一套，会把库剥空）：

| 产物 | 根集合 | 剥离什么 |
|---|---|---|
| `hermes build`（一个 unit ⇒ 库） | **全部 `exported` 符号** | 只剥**私有且不可达**的符号（典型：没有 `export`、又不是 `is_entry` 的 workflow——语言没有跨 workflow 调用，它必然是死代码，`W3013`） |
| `hermes link`（多库 ⇒ 可执行） | **`entries`**（`is_entry=true` 的 workflow） | 剥一切从 entry 不可达的东西，**包括未被用到的 export**。这是"我只付我用到的那部分"的来源 |

```
reachable(sym) = ∃ root r：sym 在 r 的传递闭包内（含 type/backend/tool 引用）
```

- 顺序：先算可达集，再按**成员名字典序**重排 payload 区（保证 `hermes link` 的字节可重现）。
- 剥离后 `flags.bit0 = 1`；`--no-strip` 清零。
- **`hermes build` 不能剥 export**：一个库的导出符号是给别人用的，本 unit 内没人引用它完全正常。v1 的规则（"不被 entry 可达就剥"）在这个场景下会把库剥成空壳，而症状是"链接时 E_LINK_UNRESOLVED"——离原因很远。
- 审计含义（D-013 明说）：审计的是**被链接进去的东西**，不是整个库。因此 `hermes ar t` 必须能列出"已剥离的成员/符号及其原因"（`--why` 参数）。

### 9.5 版本检查

**两条来源不同的检查，不能混在一起**：`archive_version` 在**加载**归档时查；`ir_version` / `abi_version` 在**链接**时按 `requires` 逐条查（D-014.3）。

| 轴 | 规则 | 违反 |
|---|---|---|
| `archive_version` | 必须被本版本支持（当前 = 1） | `E_HBC_VERSION`（**加载**失败，早于任何链接决策） |
| `abi_version` | **必须完全相等**（不是 major 相等） | `E_LINK_ABI_MISMATCH` |
| `ir_version` | 主版本相等，且被链接方 ≤ 链接方 | `E_LINK_IR_MISMATCH` |
| `language_version` | 被链接方 ≤ 链接方（语言无破坏性变更时不升 major） | warning |
| `requires[].member_sha256` | 必须与归档里该成员的 `ir_sha256` 相等 | `E_LINK_TAMPERED`（"你声明的依赖和实际链进来的不是同一份字节"） |

> `member_sha256` **缺省即不校验**，只允许出现在 `implicit:true` 的项（目前仅 `<std/runtime>`）——它是编译器内建、不来自任何归档成员。**本地库（`kind:"local"`）缺省 `member_sha256` 视为错误**（`E_LINK_REQUIRES_INCOMPLETE`）：声明了要链接一个库却不说要它的哪份字节，等于没声明。

**为什么 `abi_version` 要求完全相等而不是 major 相等**：ABI 的 minor 在实际工程里从来不安全——加一个 `uint64` 句柄代际位、改一个结构体字段顺序，都能让 minor 变化而 major 不动，而后果是 cgo 边界上的内存解释错误。既然 `abi_version` 由我们自己维护、且只在前端与 Go 之间使用，**严格相等是最省心的正确解**：升级就两边一起升。

**为什么 `requires[].member_sha256` 值得存**：它是"供应链"那一环的唯一硬证据。`#link <std/net>` 的语义是"我要那个确切版本的 net 实现"，而不是"我要某个叫 net 的东西"。少了它，投毒的 `stdlib.hbc` 与正版在版本检查上完全无法区分。

### 9.6 链接器为什么在 Go（团队负责人倾向 Go；我同意，给理由）

| 维度 | 论据 |
|---|---|
| **JSON 读取** | 链接器要读任意（可能敌意的）成员 IR。Go 有 `encoding/json`（stdlib，严格，可配 `DisallowUnknownFields`）；C++ 侧我们只有 200 行的 **writer**，没有 reader。要在 C++ 做链接，得再写一个 400+ 行的 reader + 全套错误处理，**直接违反"零第三方 C++ 库"且把一个已验证的组件变成两个**。 |
| **canonical 序列化** | ODR 比较与 index 生成都需要 canonical JSON。canonical writer 已在 Go（`internal/canon`，被 trace 与签名复用）。放 C++ 就是**第二份实现**，正是 ARCH §5.3 用"单一实现点 + golden"消灭的那类漂移。 |
| **文件 IO** | 链接要跨多文件读、写临时文件、`fsync`、`rename`。C++ 侧被定义为**无 IO**（LANGUAGE §1.2）。破例即边界失效。 |
| **性能** | 无关。链接是每次构建一次的操作，目标 50 MiB IR < 2 s；Go 的 `encoding/json` 在这量级轻松达标。热路径是运行期，不是链接期。 |
| **"前端需要符号表全貌"是否成立** | **不成立**。`hermes build` 一次只编一个 unit；未解析符号**故意**推迟到链接期——这正是静态库的意义。前端只需要 `.h` 里的类型，那是源码级的，不走 IR。 |

**唯一放 C++ 的理由（以及为什么不够）**：如果要做"链接期代码内联/跨 unit 常量折叠"这类优化，需要重新生成 IR，那才需要 codegen。Phase 1 不做任何链接期优化（`--no-strip` 之外），所以不需要。

**结论**：链接器在 Go（`internal/link`），归档容器读写也在 Go（`internal/archive`）。C++ 前端**只做** 词法/语法/类型/codegen，**不做** 任何跨文件、任何 IO、任何签名。

---

## 10. 版本策略（四条轴，互不牵连）

| 轴 | 位置 | 谁读 | 变更规则 |
|---|---|---|---|
| `ir_version` | IR 顶层 | 运行期、链接器 | 加字段 = 不改（消费者必须容忍未知字段）；改字段语义/类型 = major+1，旧运行期拒绝 |
| `archive_version` | `.hbc` 偏移 4 | 归档加载器 | 布局变更 = major+1 |
| `seal_version` | `.sealed.hermes` 偏移 4 | verify | manifest 字段语义变更 = major+1 |
| `abi_version` | `hm_abi_version()` / `hm_parse_version()` | cgo 初始化 | 原生层 ABI（ARCH §11.5） |

**"消费者必须容忍未知字段"的实现要求**：Go 侧 `internal/ir` 的每个结构体带 `Extra map[string]json.RawMessage`，canonical 重编码时按 key 排序写回。否则"新版前端 + 旧版 Go"会静默丢字段，把 minor 兼容变成静默数据损坏。

---

## 11. IR ↔ trace 的映射（"trace 形状编译期已知"的落地）

| IR 节点 | 产生的 span | `span_id` 生成规则 |
|---|---|---|
| `workflow` 根 | 1 个 `kind=run` | `<run_id>` |
| `call_model` / `call_tool` 作为 `let` 的值 | 1 个 `kind=call` | `<run_id>/<var_name>` ← **变量名，可在源码 grep 到**（R-TR-01） |
| `call_*` 在 `return` 里 | 1 个 | `<run_id>/L<line>C<col>` |
| `parallel` | 1 个 `kind=block` + 每分支 1 个（分支内再递归） | `<run_id>/parallel@L<line>/b<i>` |
| `if` / `match` / `try` | 1 个 `kind=block` | `<run_id>/L<line>` |
| `retry` | 1 个 `kind=block` + 每次尝试 1 个 `kind=attempt` | `<run_id>/retry@L<line>/a<n>` |

因此：**`max_spans` 就是 trace 的 span 数上界**，`hermes trace` 的形状与 IR 同构，`replay` 只需重放 IR（不需要源码、不需要快照）。

> 无 span 的节点（`seq`/`let`/`return`/表达式）只产生 trace **事件**（`trace.event`），不产生 span。这条规则保证"span 数 ≤ max_spans"可断言，也保证 trace 文件不会被无信息的结构性节点撑大。

---

## 12. `schemas/ir.schema.json` 的字段设计（Q12 的机器可读版）

本文 §2–§6 是权威；JSON Schema 是它在机器上的投影。结构：

```
$defs:
  loc          { required: [m,l,c,el,ec]; 5 × integer, minimum 0/1 }
  typeExpr     { oneOf: 6 个基础 k | array{of} | struct{id} | httpresult{of} | tuple{of[]} }
  annotation   { required: [name, args]; name enum[enum, pattern] }
  field        { required: [n, t, opt, ann]; + loc 可选 }
  typeDef      { required: [id, name, module, loc, fields, sha256] }
  backend      { required: [name, kind, addr];
                 optional: [auth, model, supports_tier, supports_response_format,
                            exact_fallback, schema_any, max_parallel, exported];
                 if exported == true then required: [decl_loc, def_loc]
                 addr: oneOf { {required:[src,env,default]} src=="env" |
                               {required:[src,value]}      src=="literal" }
                 # D-023 末节：auth 与 addr 同形状，但**禁止 default**
                 auth: { required: [src,env]; src must == "env"; no `default` key }
                       | absent   # 省略 = 该 backend 不带密钥 }
  requireItem  { required: [spec, kind, abi_version, ir_version, origin];
                 optional: [member_sha256, implicit];
                 origin: enum["source","cli"]  # D-020：source=源码 #link，cli=命令行 -l/-L }

  tool         { required: [name, method, url_template, allow_status, timeout_ms, idem, decl_loc] }
  schemaDoc    { required: [id, root, json] }
  var          { required: [id, name, t, decl, scope] }
  bounds       { required: [max_steps, max_spans, max_model_calls, max_tool_calls,
                            max_concurrency, max_retry_expansion]; 全部 integer, minimum 0 }
  economics    { required: [assumptions, prompt_bytes_upper, tokens_in_upper,
                            tokens_out_upper, model_calls_upper, pricing_source];
                 optional: [cost_upper_micro_usd, overflow] }
  node         { required: [id, k, loc];
                 if k == "seq"    then required: [kids]
                 if k == "let"    then required: [var, e]
                 if k == "if"     then required: [c, then]
                 if k == "match"  then required: [subj, arms, default]
                 if k == "parallel" then required: [branches, join_k, max_par]
                 if k == "retry"  then required: [n, delay_ms, body]
                 if k == "try"    then required: [body, catches]
                 if k == "return" then required: [e]
                 if k == "lit"    then required: [t, v]
                 … （20 种逐一给出，oneOf + discriminator = "k"）
               }
  workflow     { required: [name, exported, is_entry, backend, input, returns, root,
                            nodes, vars, bounds, economics, interp_deps, loc] }
  irDoc        { required: [ir_version, unit, compiler, types, backends, tools,
                            schemas, workflows, exports, imports, requires, diag_counts] }

顶层约束（json schema 表达不了、由加载器额外校验）：
  V-1  all node.id 唯一且 ∈ [0, len(nodes))
  V-2  all 引用（kids/then/c/e/of/…）指向存在的 id
  V-3  图无环（P-3）
  V-4  workflow.root 存在且 k == "seq"
  V-5  var.decl 指向 k == "let" 且 let.var 回指（双向一致）
  V-6  每个 node.loc.m < len(unit.modules)
  V-7  IR 里不存在带小数点的 JSON 数字（P-1）
  V-8  schemas[].json 重新 canonical 化后字节等于原文（§3.5）
  V-9  workflow.exported 与 exports.workflows 含该 name **双向一致**（一方有另一方无 ⇒ E_IR_EXPORT_MISMATCH）
  V-10 至多一个 workflow.is_entry == true；若存在则它必须 exported == true（E3022/E3024 的加载期复查）
  V-11 exports.* / requires[] 的名字在 links 上下文里唯一（type/workflow/backend/tool 四个命名空间各自独立）
```

**V-9 为什么必须双向**：`exported` 是工作流上的标量，`exports.workflows` 是符号表。两者是同一个事实的两份表示，只在一处出现就意味着"哪个才算数"要靠约定——而链接器只读 `exports`，前端只写 `exported`，一旦不一致，剥离器与链接器的判断会分叉（一边认为可达、一边认为不存在）。加载期直接判死，比在链接期追查便宜得多。

**为什么 V-1…V-11 不放进 JSON Schema**：JSON Schema 表达不了图约束（引用存在性、无环、双向一致）。硬塞进去只会得到一个又慢又难读的 schema。分工：**schema 管形状，加载器管图不变量**，两者都有独立测试。

校验器实现归属：`internal/ir/load.go`（Go）。加载任何 `.hbc` 成员 IR 前必跑全部 V-1…V-11 —— **包括自己刚产出的**（自举信任从零开始，这样"编译器产出了坏 IR"能被自己的加载器抓住）。
