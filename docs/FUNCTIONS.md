# 用户函数（2.0.0）

> 状态（2026-11-07，最终）：**非递归函数在 codegen 编译期展开（Inline）**。
> 调用点直接变成函数体的表达式，产物里没有 `call_fn`、运行期没有动态派发，
> 上界完全静态。**递归函数**是唯一保留 `call_fn` 的情况 —— 它的调用次数
> 不可静态定界，运行期由深度上限 512 兜底，workflow 标 `unbounded`。
>
> 非递归但**不是单一 `return <expr>;`** 的函数（含 if/循环/多语句）目前
> 会在调用点报编译错误：编译模式不提供调用帧，也不退回动态派发。
> 形参出现在**插值字符串**里也会被拒绝（`interp` 存的是路径名，内联没有
> 临时变量可绑；改用 `+` 拼接）。函数体里的 `model/tool/memory/vector`
> 调用仍编译期拒绝（调用次数要进上界）。

## 语法：与 C++ 一致

```cpp
// 返回类型开头，定义即声明
string greet(string name) {
    return "hi, " + name;
}

// 递归是允许的
int fib(int n) {
    if (n < 2) { return n; }
    return fib(n - 1) + fib(n - 2);
}

export workflow a() entry -> int {
    return fib(10);
}
```

规则：

- **定义即声明**：两遍扫描，函数在文件中任意位置可见，不需要前向声明。
- **不做重载**：同名同参数个数即冲突，编译期报错。重载会让 `<T>` 消歧从封闭表
  变成完整重载解析，那是另一个量级的改动。
- **参数是值传递**：与现有 `struct` 语义一致，没有引用/指针。

## 静态判定规则（"尽量都静态"）

| 情况 | 上界 |
|---|---|
| 函数**不递归**，且调用链上的参数不参与循环次数 | **静态**：按内联展开计算 `max_spans`，仍是常量 |
| 函数递归（直接或间接） | **unbounded**：`explain` 显示未知，运行期由深度上限兜底 |
| 函数体含 `while` / 运行期上界的 `for` | 调用它的 workflow 也是 unbounded |

判定方式：在 codegen 前对**调用图**做一次可达性分析。

- 从每个 workflow 出发，沿调用边做 DFS；
- 遇到已在栈上的函数 → 该 workflow 标 `unbounded`；
- 否则按**拓扑序**计算每个函数的 span，workflow 的 span 加上调用点展开的份额。

这样"能算的仍然算"，只有真递归才降级，且降级是可见的。

## IR 形状

单元级新增：

```json
"functions": [
  {"name":"fib","params":[{"name":"n","type":{"k":"int"}}],
   "returns":{"k":"int"},"root":3,"vars":[...],"recursive":true}
]
```

节点新增 `call_fn`：

```json
{"id":7,"k":"call_fn","loc":{...},"fn":"fib","args":[8]}
```

`recursive` 是编译期算出来的事实，不是提示——`explain` 直接读它。

## 执行模型：非递归展开，递归留调用帧

- **非递归**：codegen 把 `return <expr>` 在调用点展开，形参直接代换为实参
  节点。实参必须已确认是纯表达式（span/model/tool 全 0）——否则形参被引用
  多次时会重复求值，那是语义错误，所以编译期拒绝并提示先绑定到变量。
- **递归**：保留 `call_fn`，引擎用新作用域执行，参数按名字绑定。
- **深度上限 512**：超了回 `stack depth exceeded`，不是崩溃。
- 与现有 `break`/`continue` 的交互：跳转**不能跨函数边界**，parser 已经保证。

## 验收

| # | 验收项 | 状态 |
|---|---|---|
| 1 | `fib(10)` 跑出 55 | ✅ `functions_test.go`（`main` = `double_it(20)+fib(10)` = 95） |
| 2 | 非递归函数不影响 `max_spans` 的常量性 | ✅ `mild` 产物无 `call_fn`、`unbounded` 不出现、`max_spans=0` |
| 3 | 递归函数的 workflow：`explain` 显示 unknown | ✅ `max_steps` / `max_spans` 都显示 unknown |
| 4 | 深度 100000 的递归 → `stack depth exceeded`，进程不崩 | ✅ `down(100000)` 在第 513 层停下 |
| 5 | 同名同参数个数的两个函数 → 编译报错 | ✅ 同名即报错（不支持重载） |
| 6 | `call_fn` 出现在 refs 校验里；schema 有 `nodeCallFn` | ✅ |

## 分层与顺序

| 步 | 文件 | 完成标志 |
|---|---|---|
| 1 | `native/cpp/{lexer,parser,ast}` | ✅ 已完成：`string f(int x) { }` 解析成 `FunctionDecl`（参数是 `Type name`，与 workflow 的 `name: Type` 不同，独立解析分支） |
| 2 | `native/cpp/codegen` | ✅ 非递归函数编译期展开；递归判定（调用图）+ `call_fn` 只留给递归 |
| 3 | `internal/ir` | ✅ `Doc.Functions` + `refs` + schema（`function` / `nodeCallFn`）+ `SP(call_fn)=1` |
| 4 | `internal/engine` | ✅ 只服务递归：调用帧 + 深度上限 512（parallel 分支复制当前帧） |
| 5 | 测试与文档 | ✅ golden 覆盖内联/递归/深度/静态上界；`hermes compile` 有"无非递归 call_fn"断言 |

**每一步都必须先加探针确认数据流**（这是本项目已经付过三次学费的地方：
`unbounded` 挂错对象、`policy` 漏一个创建点、`policy_` 晚于使用）。

## 已确认的事实（下一步可直接用）

探针实测：`FunctionDecl` 确实在 `module->child` 里，`text` 是函数名，
`child` 是参数链（`Param`，`text`=参数名、`type_args`=类型），`body` 是块。

```cpp
[string greet] params=0x... body=0x...
```

所以第 2 步不需要改 parser 或模块结构，只在 codegen 里加：
按 `FunctionDecl` 逐个 `Gen` 生成 `functions[]`，并在 `gen_expr` 的调用点
把"单段标识符 + 已知函数名"解析成 `call_fn`。

## JVal 语义与上次失败的真因（已确认，别再猜）

`native/cpp/json_write.h`：

```cpp
JVal& push(JVal v);                        // 数组尾部追加
JVal& set(const std::string& key, JVal v); // 对象插入/覆盖（**按值**，会拷贝）
static JVal array();                       // 造一个空数组（kArr）
```

所以"塞进对象"用 `w.set("functions", functions_)` 是合法的**拷贝**，
不存在移动语义的坑。上次失败不是这里。

## 上一次失败：结论**收回**

我先前写在这里的"真因是 JSON 撑爆输出缓冲"**证据不足，已收回**。

反证来自 `internal/native/parse.go`：Go 侧本来就有重试扩容——

```go
for attempt := 0; attempt < 8; attempt++ {
    if jl >= jsonCap { jsonCap *= 2 }   // 初始 len(src)*8 + 8192
}
return nil, fmt.Errorf("native: codegen output exceeded %d grow attempts", maxAttempts)
```

初始值已经很大，再翻倍 8 次约 2000 倍，几 KB 源码不可能撑爆。而且实际报错是
**`codegen 产出的 workflow 不是合法 JSON:`**（内容为空），不是
`exceeded 8 grow attempts`。两条信息对不上，说明问题是**产物被弄坏**，不是容量不够。

最可能的方向：`JVal` 对象在拼接时被破坏（`set` 收到 `kNull` 被忽略、或
`nodes_` 被 mov 走后再用）。

## 下一次的正确开场：先看产物

在 `run_function` 里加探针，打印 `o.str()` 的**长度与前 200 字节**：

```cpp
std::string dbg = o.str();
fprintf(stderr, "[probe] fn json len=%zu head=%.200s\n", dbg.size(), dbg.c_str());
```

先看清生成的 JSON 长什么样，再谈缓冲或结构。**不要照着未经证实的结论改架构。**

## 下一步的准确做法

0. **先用探针看清 `run_function` 生成的 JSON**（见上一节），确认它是否被破坏
1. 若确实需要：`hm_codegen_ext` 增加一个输出参数（`functions` 缓冲），与 `wf` / `types` 并列
2. Go 侧 `ir.Assembly` 增加 `FunctionsJSON`，装配时解码
3. `ir.Doc.Functions []Function`

## 2026-11-07 落地记录（接手的人先看这里）

**做成了什么**

- `native/cpp/codegen.cpp` 新增 `Gen::run_function`：把 `FunctionDecl` 编成
  `{name, params, returns, root, nodes, vars, recursive}`。
- 新增 `hm_codegen_functions`（**新函数**，`hm_codegen_ext` 的签名一个字没动）。
  形状：`(src, len, filename, is_header, ext_types, ext_len, out, cap, out_len,
  diag, diag_cap, diag_len)`。
- Go：`internal/native.CodegenFunctions` → `ir.Assembly.FunctionsJSON` →
  `ir.resolveFunctions` → `Doc.Functions`。没有函数时 `functions` 键**不出现**，
  1.x 的 golden 字节不变。
- `call_fn` 进 IR/schema/refs；`fn` 是**名字引用**（`TableFunction`），
  `args` 是位置参数节点。

**为什么这轮没有四次失败**

签名整段写在新函数里，没有在既有文本里找插入点；探针先确认了
`FunctionDecl.child/type_args/body` 与 `CallExpr` 的实参形状。

**执行模型（最终）**

- 非递归函数：`gen_expr` 在调用点展开 `return <expr>`，形参映射到实参节点；
  产物里没有 `call_fn`，上界静态。
- 实参必须纯（span/model/tool 全 0）；否则报
  `argument N to ... is not pure`，要求先绑定变量。
- 形参出现在插值字符串里：编译期拒绝（没有临时变量可绑，会变成运行期
  "变量不存在"）；提示改成 `+` 拼接。
- 非递归但非单一 return 的函数：调用点报
  `cannot be inlined at compile time`，指到调用行/列（Rust 风格诊断）。
- 递归函数：保留 `call_fn`，引擎新作用域执行，深度上限 512；
  含递归调用的 workflow 标 `unbounded`，`explain` 的 `max_steps`/`max_spans`
  都显示 unknown。
- `cmd/hermes/cmd_compile.go` 的 `checkCompiledStatic`：编译产物里一旦出现
  指向非递归函数的 `call_fn` 就拒绝构建 —— 动态派发不许溜进 AOT。

**已知缺口**

1. **非单一 return 的非递归函数**：目前编译期拒绝。要支持它们，需要把
   if/循环控制流也展开（例如结果变量 + 早退标志），这是下一步的 inliner。
   **难点的具体位置见下一节**，别重新发现一遍。
2. **函数体里的外部调用**：`model/tool/memory/vector` 仍编译期拒绝。
3. **函数体引用的字段级 V-2**：装配期已查名字唯一、参数个数、类型、
   局部变量绑定；字段级悬空引用还没进 `collectRefs`。

---

## 非单一 return 的展开：难点与路线（探针结论，2026-11-07）

### 现状的拒绝点

`native/cpp/codegen.cpp:566-574`：

```cpp
const Node* only = (body && body->kind == Kind::Block) ? body->child : nullptr;
if (only != nullptr && only->next == nullptr && only->kind == Kind::ReturnStmt) {
  retExpr = only->child;                       // 唯一 return 的表达式
}
if (retExpr == nullptr) {
  fail_at(e, "function `X` cannot be inlined at compile time: ...");
}
```

即：**要求函数体恰好一条 `return <expr>;`**，然后把那个表达式节点直接搬到调用点。

### 核心约束：内联发生在 `gen_expr`，而它插不进语句

内联点在 `Gen::gen_expr`（`codegen.cpp:260` 起，函数签名 `int gen_expr(const Node*)`）。
它的契约是**返回一个表达式节点的 id**，不是"生成一串语句"。

而 `if` 展开需要语句（`Gen::gen_stmt`，`codegen.cpp:1211` 起）。

**这就是难点的全部**：调用点可能嵌在表达式任意深度——

```c
return pick(3);          // 调用点是 return 的子表达式
let x = pick(3) + 1;     // 调用点是 binop 的操作数
let y = f(pick(3));      // 嵌套调用
```

外层语句在调 `gen_expr` 时**不知道自己需要为内联腾出语句位置**。

所以"把内联搬到 `gen_stmt`"不是搬一个函数调用那么简单，而是要让
**表达式生成具备"提升语句到外层"的能力**（CPS 化或两趟）。这是架构级改动。

### 路线 A（小步，先做这个）：if/else 归约，不引入早退标志

**适用形态**：函数体的每条路径都是直接 `return`，没有中间语句、没有循环。

```c
int pick(int x) {
    if (x > 0) { return 1; }
    return 0;
}
```

可以无早退地归约成**一个** `if` 语句 + 一个结果变量：

```
let __r = <零值>;              // 类型已知，取零值
if (x > 0) { __r = 1; } else { __r = 0; }
// 表达式位置引用 __r
```

**前提改动**：`gen_expr` 需要能"往当前正在生成的语句列表里追加一条 `let`"。
这要求 `Gen` 持有一个"当前语句出口"（`std::vector<JVal>* cur_stmts`），
`gen_stmt` 进入块时压栈、退出时弹栈。**这是一个可控的小改动**，且对现有
单 return 路径无影响（那条路径不追加语句）。

**判定**：写一个 `bool flatten_if_to_result(const Node* body, ...)`，
只接受"if 的 then/else 都是单条 return（或链式 if）"的形状；不满足就仍然报
现在这条 `E_IR_CODEGEN`。**先让能归约的归约，不能归约的保持拒绝** —— 不要
为了"支持所有情况"把语义做糊。

**上界**：归约后语句数增加有限（每个 if 两三个节点），`max_spans` 仍静态。

### 路线 B（后续）：结果变量 + 早退标志

适用于中间语句、循环、`break`/`continue`：

```
let __r = <零值>;
let __early = false;
if (c) { __r = 1; __early = true; }
if (!__early) { <后续语句> }        // 每段后续语句都要包
```

**代价**：IR 结构从线性变成嵌套 if；语句数随"return 出现位置"增长；
含 `while` 的函数体展开后，**调用方的上界从静态变成 `unbounded`**
（`explain` 必须如实标注，不能编假数字）。

这条路要改 `gen_stmt` 的 `return` 语义（在函数展开上下文里，`return`
不是 workflow 返回，而是"写结果 + 置标志"）——建议用一个模式标志
（`inlining_fn_`）而不是复制一份 `gen_stmt`。

### 两条路共同的纪律

1. **先探针**：改之前先 `./build/hermes ir /tmp/f.hermes -o /tmp/f.json` 看
   现在的诊断和产物。凡跳过探针的改动，这个项目全部回滚过。
2. **不要用文本位置推断语法边界**：声明跨行、参数里也有分号。
3. **golden 会变**：`schemas/golden/ir-*.json` 有 17 份，展开逻辑一变就要
   重生成并逐份 review 差异（不是无脑覆盖）。
4. **`hm_codegen_ext` 的签名一个字都不许动**：四次失败全出在这上面。
   要加能力就**新增一个函数**（`hm_codegen_functions` 就是这么来的）。
