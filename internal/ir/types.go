// Package ir 是 Hermes IR（canonical JSON）在 Go 侧的只读视图与加载期校验。
//
// 两层分工：
//
//	types.go     纯数据结构。**只描述形状**，不做任何校验 —— 能 Unmarshal 成功
//	             不代表 IR 合法（字段齐了但引用可能悬空）。
//	refs.go      把散落在 30 多处、形态各异的引用归一化成 []Ref。
//	validate.go  三阶段校验（形状 → 图不变量 → 派生复算），产出 []diag.Diag。
//
// 为什么引用要归一化：IR 里同一个"引用"有两种形态 ——
//
//	下标   typeExpr.struct.id → types[]、decl.var → vars[]、seq.kids[] → nodes[]
//	名字   call_tool.tool → tools[]、workflow.backend → backends[]
//
// 按字段逐个写 if 检查，**一定会在新增 node kind 时漏掉**（新 kind 的作者
// 不会记得去改校验器）。collectRefs 是新 kind 的必须实现项，漏了会体现为
// 引用条数断言失败（golden IR 的引用总数是固定的）。
package ir

import "encoding/json"

// IRVersionSupported 是本编译器**唯一接受**的 ir_version。
//
// 2.x 期间恒为 2，**不随 minor 升**：2.1 / 2.2 新增节点时靠节点 kind 的
// 可选字段扩展，不靠版本号区分。理由很实际 —— 一旦拿 minor 去升 ir_version，
// 2.0 与 2.1 就互读不了，而 2.0.0 是 LTS，那等于自己把 LTS 承诺打掉。
//
// 只有**要破兼容**（删字段、改字段含义）才升 3，同时明确放弃 2.x 互读。
// 也不做向前兼容：1.x 的产物直接拒绝（E_IR_VERSION），不写迁移路径。
const IRVersionSupported = 2

// Doc 对应 IR.md §12 的 irDoc。
type Doc struct {
	// IRVersion 必须等于 IRVersionSupported，否则 E_IR_VERSION 硬拒绝。
	IRVersion int         `json:"ir_version"`
	Unit      Unit        `json:"unit"`
	Compiler  Compiler    `json:"compiler"`
	Types     []TypeDef   `json:"types"`
	Backends  []Backend   `json:"backends"`
	Tools     []Tool      `json:"tools"`
	Schemas   []SchemaDoc `json:"schemas"`
	Workflows []Workflow  `json:"workflows"`
	// Functions 是 unit 级函数表（2.0.0）。
	//
	// omitempty 是**硬要求**：没有函数的 IR 必须与 1.x 的字节完全一致，
	// 否则 17 份 golden IR 与所有已签名的 .hbc 会同时变哈希。
	Functions  []Function    `json:"functions,omitempty"`
	Exports    Exports       `json:"exports"`
	Imports    []ImportRef   `json:"imports"`
	Requires   []RequireItem `json:"requires"`
	DiagCounts DiagCounts    `json:"diag_counts"`
}

// Unit 是一个编译单元。modules 的**下标**就是 loc.m 的值域（V-6）。
type Unit struct {
	Name      string   `json:"name"`
	EntryPath string   `json:"entry_path"`
	Modules   []Module `json:"modules"`
	// SourceSHA256 只在 sealed 产物里出现；成员 IR 里没有。
	SourceSHA256 string `json:"source_sha256,omitempty"`
	// LanguageVersion 属于 unit 而不属于 compiler：它描述的是**这份源码**讲哪一版语言，
	// 而 compiler.parser_abi 描述的是**谁编的它**。两条轴独立，见 ARCH §11（六轴）。
	LanguageVersion int `json:"language_version"`
}

// Module 按 include 到达顺序排列。`#link` 的模块**不在**里面。
//
// 判据是 builtin 而不是文件后缀：内置 std 模块没有可寻址的源文件，它的完整性由
// requires[].member_sha256 背书，所以 builtin=true 时**故意没有 sha256**
// （没有的东西不编一个出来，见 schemas/ir.schema.json 的 module 定义）。
type Module struct {
	Path    string `json:"path"`
	SHA256  string `json:"sha256,omitempty"`
	Builtin bool   `json:"builtin"`
}

type Compiler struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	ParserABI int    `json:"parser_abi"`
	Opt       string `json:"opt,omitempty"`
}

// Loc 是源码位置。m 是 modules 下标（V-6 校验它）。
type Loc struct {
	M  int `json:"m"`
	L  int `json:"l"`
	C  int `json:"c"`
	EL int `json:"el"`
	EC int `json:"ec"`
}

// TypeDef 是 struct 定义。
type TypeDef struct {
	ID       int     `json:"id"`
	Name     string  `json:"name"`
	Module   int     `json:"module"`
	Loc      Loc     `json:"loc"`
	Fields   []Field `json:"fields"`
	SHA256   string  `json:"sha256"`
	Exported bool    `json:"exported,omitempty"`
}

// Field 的 T 是类型表达式；Ann 是注解（enum/pattern）。
//
// Ann 是**切片而不是指针**：IR.md §3.1 的样例里每个字段都有 ann 键，哪怕是 []。
// 恒写出来是为了让"没有注解"与"忘了生成"在 diff 里可区分——用 omitempty 会把
// 两者压成同一种字节，而这两件事的修复动作完全不同。
type Field struct {
	N   string       `json:"n"`
	T   TypeExpr     `json:"t"`
	Opt bool         `json:"opt"`
	Ann []Annotation `json:"ann"`
	Loc *Loc         `json:"loc,omitempty"`
}

type Annotation struct {
	Name string   `json:"name"` // "enum" | "pattern"
	Args []string `json:"args"`
}

// TypeExpr 是**递归**的类型表达式。Of 的形态由 K 决定，所以先原样收下：
//
//	K == "array" | "httpresult"  → Of 是单个 TypeExpr
//	K == "tuple"                 → Of 是 []TypeExpr
//	K == "struct"                → ID 是 types[] 下标
//	其余（6 个基础类型）          → 无附加字段
//
// 用 json.RawMessage 而不是 interface{}：延迟解析只发生一次（collectRefs），
// 且不会把 map[string]any 的浮点风险带进来（P-1）。
type TypeExpr struct {
	K  string          `json:"k"`
	ID *int            `json:"id,omitempty"`
	Of json.RawMessage `json:"of,omitempty"`
	// Ref 只在**装配中途**出现：codegen 只认识名字（`struct` 类型的 ref），
	// 装配阶段换成 ID 后必须清空 —— 留在 Doc 里会让 emit 写出一份 schema
	// 不认的 IR。它进结构体是因为函数表直接解码到 Doc，而不是走 map 转手。
	Ref string `json:"ref,omitempty"`
}

// Backend 的地址与密钥都是**来源**，不是解析值（D-014 / D-023）。
type Backend struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	Addr Addr   `json:"addr"`
	// Auth 省略 = 该 backend 不带密钥。有值时**禁止** default 键（D-023 末节）。
	Auth     *Auth  `json:"auth,omitempty"`
	Model    string `json:"model,omitempty"`
	Exported bool   `json:"exported,omitempty"`
	DeclLoc  *Loc   `json:"decl_loc,omitempty"`
	DefLoc   *Loc   `json:"def_loc,omitempty"`
}

type Addr struct {
	Src     string  `json:"src"` // "env" | "literal"
	Env     string  `json:"env,omitempty"`
	Default *string `json:"default,omitempty"`
	Value   *string `json:"value,omitempty"`
}

// Auth 与 Addr 同形状但**没有 default**：密钥没有安全默认值（D-023）。
type Auth struct {
	Src string `json:"src"` // 恒为 "env"
	Env string `json:"env"`
}

// Tool 的 URL 模板是**结构化片段**，不是字符串：运行期不该再解析一次 URL，
// 审计者也要能直接看出这个 URL 依赖了哪些字段。
// AgentSpec 是一次 agent 循环的规格（D-034）。
//
// `MaxSteps` 通常是**常量**，这是允许循环的前提：模型调几次工具由它决定，
// 但上界在编译期就写死了，所以 `max_model_calls` / `max_tool_calls` 仍然可算，
// 预算仍然是预留而不是事后统计。
//
// 唯一的例外是源码里写了 `max_steps = unlimited`（v1.2.2）：那种情况下这个键
// **整个不存在**，于是 `max_spans` / 调用次数在加载期都不可验证（见
// Bounds.Unbounded），运行期退回一个硬性迭代上限（engine 的
// maxUnboundedAgentSteps）。作者放弃了编译期上界，但没有放弃"一定会停"。
type AgentSpec struct {
	// Goal 是给模型的初始指令（表达式节点；通常是一次插值）。
	Goal *int `json:"goal"`
	// Tools 是 tools[] 表里的名字。**按名字引用而不是内联**：工具定义只该有一份，
	// 内联会让"agent 用的 schema"和"call_tool 用的 schema"变成两处维护。
	Tools []string `json:"tools"`
	// MaxSteps 是循环上界（常量，>0）；**nil 表示没有编译期上界**
	// （源码写的是 `max_steps = unlimited`）。
	//
	// 为什么必须是指针 + omitempty：`max_steps = unlimited` 时前端**刻意不写
	// 这个键**（写 null 要第二套形状，写一个大数则是撒谎 —— 那会撞上
	// `max_spans ≤ 64×max_steps+1`）。若用非指针 int，缺键会被解成 0、再被
	// 重新编码成 `"max_steps":0` 写进 .hbc —— 一个**凭空出现的假数字**，
	// 正是这门语言拒绝做的事。指针让"缺失"能活着穿过 IR 往返。
	MaxSteps *int `json:"max_steps,omitempty"`
	// MaxParallelTools 是一轮里模型最多能要求多少个工具调用（默认 1）。
	//
	// 为什么必须显式：OpenAI 与 Anthropic 都会在一轮响应里并行发多个 tool_call。
	// 上界若写死"每步一次工具调用"，模型一并行，max_tool_calls 当场不成立 ——
	// 而上界不成立，整个"预算是预留"的叙事就跟着不成立。
	MaxParallelTools int `json:"max_parallel_tools,omitempty"`
	// Backend 是默认 backend 名；空串表示交给 workflow 的默认值。
	Backend string `json:"backend,omitempty"`
}

type Tool struct {
	Name string `json:"name"`
	// Description 与 Params 是给 **模型** 看的（function calling 的 schema）。
	// 它们不是给运行时看的 —— 运行时只关心 Method 与 URLTemplate。
	Description string          `json:"description,omitempty"`
	Params      json.RawMessage `json:"params,omitempty"`
	Method      string          `json:"method"`
	URLTemplate []TemplatePart  `json:"url_template"`
	AllowStatus []int           `json:"allow_status"`
	TimeoutMS   int             `json:"timeout_ms"`
	Idem        Idem            `json:"idem"`
	DeclLoc     Loc             `json:"decl_loc"`
}

// TemplatePart 二选一：字面文本，或一个已解析的字段路径。
type TemplatePart struct {
	Lit  string   `json:"lit,omitempty"`
	Path []string `json:"path,omitempty"`
}

// Idem 是编译期事实（check_idem.cpp 算），运行期只读不算。
// none = 该调用点没有幂等证明，不可安全重试；给未证明的调用贴 readonly 就是
// 一份说谎的 IR（重试会重复扣款），所以 codegen 对非只读且无声明的调用发 none。
type Idem struct {
	Mode string         `json:"mode"` // readonly | idem_true | idem_key | none
	Key  []TemplatePart `json:"key,omitempty"`
}

// SchemaDoc 的 JSON 是**原样嵌入**的字节，参与类型哈希（V-8）。
//
// ID 是**名字**不是下标：它是 struct 的非限定名，也是 hermes serve 的请求契约名。
// 引用它的地方（call_model.schema）用数组下标，两者靠这个 id 关联。
type SchemaDoc struct {
	ID   string          `json:"id"`
	Root string          `json:"root"`
	JSON json.RawMessage `json:"json"`
}

// Workflow 是执行单位。
// SecurityPolicy 是 workflow 上内联的安全策略（D-041）。
type SecurityPolicy struct {
	Name  string         `json:"name"`
	Rules []SecurityRule `json:"rules"`
}

type SecurityRule struct {
	Effect string   `json:"effect"`
	Cap    string   `json:"cap"`
	Values []string `json:"values"`
}

type Workflow struct {
	Name     string    `json:"name"`
	Exported bool      `json:"exported"`
	IsEntry  bool      `json:"is_entry"`
	Backend  string    `json:"backend"`
	Input    *TypeExpr `json:"input"`
	Returns  TypeExpr  `json:"returns"`
	// Params 是其余参数的默认值（已折叠的常量）。input 不在里面 —— 它的类型是必填的。
	Params   []Param         `json:"params"`
	Root     int             `json:"root"`
	Nodes    []Node          `json:"nodes"`
	Vars     []Var           `json:"vars"`
	Bounds   Bounds          `json:"bounds"`
	Security *SecurityPolicy `json:"security,omitempty"`
	// Economics 与 InterpDeps 的细节由 §5 定义，校验器不碰它们的内部，
	// 只用 economics 做派生复算（见 validate.go 阶段 3）。
	Economics  json.RawMessage `json:"economics"`
	InterpDeps []InterpDep     `json:"interp_deps"`
	Loc        Loc             `json:"loc"`
}

// Function 是用户函数（2.0.0）。函数体与 workflow 共用节点/变量形状，
// 但它是 unit 级定义：不绑定 workflow，也没有 bounds —— 调用点的上界
// 由 workflow.bounds 负责（当前版本对含 call_fn 的 workflow 标 unbounded）。
type Function struct {
	Name      string      `json:"name"`
	Params    []FuncParam `json:"params"`
	Returns   TypeExpr    `json:"returns"`
	Root      int         `json:"root"`
	Nodes     []Node      `json:"nodes"`
	Vars      []Var       `json:"vars"`
	Recursive bool        `json:"recursive"`
}

// FuncParam 是函数形参。T 是**静态类型**（与 workflow.Param 的"已折叠默认值"
// 不同：函数参数由调用方在运行期绑定，没有默认值这个概念）。
type FuncParam struct {
	N string   `json:"n"`
	T TypeExpr `json:"t"`
}

// Param 的 V 是**已折叠的常量**：运行期不该再求值一次。
type Param struct {
	N string          `json:"n"`
	V json.RawMessage `json:"v"`
}

// InterpDep 记录"这个节点的 prompt 依赖哪些字段路径"（LANGUAGE I-5）。
// 编译期静态可见，所以 hermes explain 无需运行就能打印。
type InterpDep struct {
	Node  int        `json:"node"`
	Paths [][]string `json:"paths"`
}

// Var 的 Decl 是绑定它的 `decl` 节点 id（V-5 双向校验的一端）。
type Var struct {
	ID    int      `json:"id"`
	Name  string   `json:"name"`
	T     TypeExpr `json:"t"`
	Decl  int      `json:"decl"`
	Scope int      `json:"scope"`
}

// Bounds 是**派生字段**：它由前端算出，加载期必须复算比对。
// 不复算的后果不是"数字不好看"，而是伪造的 bounds 能绕过执行上限。
type Bounds struct {
	MaxSteps int `json:"max_steps"`
	MaxSpans int `json:"max_spans"`
	// Unbounded 为 true 时，max_steps / max_spans **都不是真正的上界**：
	// 递归函数的调用次数与未知循环的迭代次数都无法静态定界。运行期的闸是
	// 调用深度上限（512）与循环上限，不是这两个数字。
	Unbounded         bool `json:"unbounded,omitempty"`
	MaxModelCalls     int  `json:"max_model_calls"`
	MaxToolCalls      int  `json:"max_tool_calls"`
	MaxConcurrency    int  `json:"max_concurrency"`
	MaxRetryExpansion int  `json:"max_retry_expansion"`
}

// Node 是 tagged union：K 决定哪些字段有效。
//
// **所有可空整数用 *int**：IR 里 id 0 是合法值，用 int 会把"缺字段"和
// "id 恰好是 0"混成一个 —— 那正是"期望 vs 实际"说不清的经典 bug。
//
// 特例：**Var == -1 表示 input**（隐式根变量，不在 vars[] 里，IR.md §4.3）。
// 校验 V-2 时必须放行 -1，否则所有用到输入参数的 IR 都会被判悬空。
type Node struct {
	ID  int    `json:"id"`
	K   string `json:"k"`
	Loc Loc    `json:"loc"`

	// 语句节点
	Kids []int `json:"kids,omitempty"`
	// Var 是单绑定（`auto x = …`）。Vars/Bind 是**解构绑定**的两种形状，
	// 与 Var 互斥或并存的关系由一个统一规则决定：见 boundVarIDs。
	Var *int `json:"var,omitempty"`
	// Vars 是解构绑定的变量表下标（A 形态：`auto [a,b] = …`）。
	// **nil 元素表示 `_` 丢弃位**（B/C 形态允许；写成 JSON 的 null，不用哨兵整数——
	// -1 已经表示 input 了，再造一个 -2 只会让两种哨兵在代码里互相冒充）。
	Vars []*int `json:"vars,omitempty"`
	// Bind 是挂在 parallel 上的绑定（C 形态）。
	Bind     []*int     `json:"bind,omitempty"`
	E        *int       `json:"e,omitempty"`
	C        *int       `json:"c,omitempty"`
	Then     *int       `json:"then,omitempty"`
	Elifs    []ElifArm  `json:"elifs,omitempty"`
	Else     *int       `json:"else,omitempty"`
	Subj     *int       `json:"subj,omitempty"`
	Arms     []MatchArm `json:"arms,omitempty"`
	Default  *int       `json:"default,omitempty"`
	Branches []int      `json:"branches,omitempty"`
	// Join 是 parallel 按位置配对的元组元素（D-031）。只有 join_k 的话，
	// 执行器知道"几元"却不知道"是哪几个表达式"。
	Join   []int `json:"join,omitempty"`
	JoinK  *int  `json:"join_k,omitempty"`
	MaxPar *int  `json:"max_par,omitempty"`
	N      *int  `json:"n,omitempty"`
	// I 是数组下标节点的下标表达式（`xs[i]` → of=数组, i=下标）。
	I       *int `json:"i,omitempty"`
	DelayMS *int `json:"delay_ms,omitempty"`
	Body    *int `json:"body,omitempty"`
	// Main 是 fallback 节点的**主表达式**（2.0.0）。
	//
	// 与 try 的区别：try 只有 body + catches（控制流），fallback 有两个
	// **表达式**——main 先算，状态码非零时改算 body 并取它的返回值。
	// 因此 main 单独占一个槽位，不塞进 kids：kids 的语义是语句序列。
	Main    *int       `json:"main,omitempty"`
	Catches []CatchArm `json:"catches,omitempty"`

	// 表达式节点
	Of     *int         `json:"of,omitempty"`
	Name   *string      `json:"name,omitempty"`
	Key    *string      `json:"key,omitempty"`
	Op     *string      `json:"op,omitempty"`
	L      *int         `json:"l,omitempty"`
	R      *int         `json:"r,omitempty"`
	Parts  []InterpPart `json:"parts,omitempty"`
	Schema *int         `json:"schema,omitempty"`
	// Args 的形态由 K 决定，**必须延迟解析**：
	//   call_model / call_tool → 对象（ModelArgs / ToolArgs）
	//   call_json             → 数组 []int（位置参数）
	// 同一个 JSON 键两种类型，用具体结构体会让 Unmarshal 直接报错。
	Args json.RawMessage `json:"args,omitempty"`
	// ArgTypes 是 call_native 的**位置形参静态类型表**，与 args 一一对应。
	// args 只有节点 id，AOT 的 C 后端靠它生成编译期写死的 extern 原型；
	// struct 形参的 ref 由装配层解析成 id（见 resolveSchemaRefs）。
	// 其余 kind 不写这个字段（omitempty），golden 因此不受影响。
	ArgTypes []TypeExpr  `json:"arg_types,omitempty"`
	Att      *AttemptCtx `json:"attempt_ctx,omitempty"`
	Tool     *string     `json:"tool,omitempty"`
	// Agent 是 `agent { … }` 的规格（D-034）。
	Agent *AgentSpec      `json:"agent,omitempty"`
	Ret   *TypeExpr       `json:"ret,omitempty"`
	To    *TypeExpr       `json:"to,omitempty"`
	Fn    *string         `json:"fn,omitempty"`
	LitT  *string         `json:"t,omitempty"`
	LitV  json.RawMessage `json:"v,omitempty"`
	// for 节点（D-040）额外使用的槽位。Var/Body 复用已有字段。
	Init  *int   `json:"init,omitempty"`
	Cond  *int   `json:"cond,omitempty"`
	Delta *int64 `json:"delta,omitempty"`
	// Const 为 true 表示这个绑定不可再赋值（对应源码的 `const auto`）。
	//
	// 2.0.0 起 IR **只用 `const`** 表达可变性，不再有 `mut` 字段：
	// 「缺字段 = 可变」会让"忘了写"和"故意可变"长得一模一样，而 IR 是
	// 要能被人和工具读的 —— 写清楚 true/false 比省略更便宜。
	//
	// 用 `*bool` 而不是 `bool` 是必须的：`omitempty` 对 `bool` 会把 `false`
	// 也省掉，于是 codegen 写出的 `"const":false` 会在重新序列化时消失，
	// 而 Node 是所有 kind 共用的扁平结构 —— 去掉 omitempty 又会让
	// `seq` / `lit` 这些根本没有可变性的节点凭空多一个 `"const":false`。
	// 指针的 omitempty 只省 `nil`（= 该 kind 没有这个概念），`&false` 照常写出。
	Const *bool `json:"const,omitempty"`

	// ---- 2.0.1 控制流扩展 ---------------------------------------------------
	// Signal 是 emit / on 引用的信号名。信号名进 IR 而不是进符号表：运行期
	// 需要它来配对槽与发射，而名字本来就是源码里的字符串。
	Signal *string `json:"signal,omitempty"`
	// Data 是 stop 的负载表达式（可选）。与 E 分开而不是复用 R：stop 的
	// reason 与 data 是两个**并列**的东西，用左右操作数表达会让读 IR 的人
	// 以为它们有先后运算关系。
	Data *int `json:"data,omitempty"`
	// Route 是 emit 的路由模式：0=全局冒泡、1=频道、2=定向。数字是 IR 契约。
	Route *int `json:"route,omitempty"`
	// Chan 是频道名的 **FNV-1a 64 编译期哈希**。运行期只做整数比较，
	// 不做字符串比较、更不建哈希表。
	Chan *uint64 `json:"chan,omitempty"`
	// 定向路由的目标上下文表达式。JSON 键用 target 而不是 to：`to` 已经是
	// call_tool 的**类型**字段（TypeExpr），两个字段共用同一个 JSON 标签会让
	// encoding/json 静默丢字段，症状是"节点缺参数"——离真正原因很远。
	Target *int `json:"target,omitempty"`
	// MS / TimeoutMS 是 sleep 与 until 的时长（毫秒，编译期已折算）。
	// Captures 是 on 槽体引用的外层局部变量名（闭包转换用）。
	// 存**名字**而不是变量下标：下标是各函数自己的编号空间，跨函数没有意义。
	Captures []string `json:"captures,omitempty"`
	// Exported 表示这个 on 监听的信号是 `export signal`（对外可见）。
	// 只有这种槽才进全局表；普通局部信号留在单元内做编译期配对。
	Exported *bool `json:"exported,omitempty"`
	MS        *int `json:"ms,omitempty"`
	TimeoutMS *int `json:"timeout_ms,omitempty"`
}

type ElifArm struct {
	C    int `json:"c"`
	Body int `json:"body"`
}

type MatchArm struct {
	// Lit 是字符串臂的字面量。**default 臂靠"两者都不存在"表示**，
	// 而不是靠空串 —— 否则 `case ""` 会被当成 default，静默改语义。
	Lit string `json:"lit,omitempty"`
	// Num 是整数臂的字面量。与 Lit 互斥。
	Num  *int64 `json:"num,omitempty"`
	Body int    `json:"body"`
}

type CatchArm struct {
	T    string `json:"t"`
	V    int    `json:"v"`
	Body int    `json:"body"`
}

// InterpPart 二选一：Lit（字面文本）或 Path（已解析的字段路径）。
type InterpPart struct {
	Lit  string   `json:"lit,omitempty"`
	Path []string `json:"path,omitempty"`
}

// ModelArgs 是 call_model 的 args（IR.md §4.2）。
// temperature 是**字符串**（P-1 无浮点），不是数字。
type ModelArgs struct {
	Prompt      *int   `json:"prompt"`
	Precision   string `json:"precision"`
	MaxTokens   *int   `json:"max_tokens"` // -1 = 取 policy
	Temperature string `json:"temperature"`
}

// ToolArgs 是 call_tool 的 args。header / body / idem_key 可空。
type ToolArgs struct {
	URL         *int   `json:"url"`
	Method      string `json:"method"`
	TimeoutMS   *int   `json:"timeout_ms"`
	AllowStatus []int  `json:"allow_status"`
	Header      *int   `json:"header"`
	Body        *int   `json:"body"`
	IdemKey     *int   `json:"idem_key"`
}

type AttemptCtx struct {
	RetryNode    int `json:"retry_node"`
	AttemptIndex int `json:"attempt_index_upper"`
}

// Exports 是**符号表**：与各定义上的 exported 标量是同一事实的两份表示（V-9）。
type Exports struct {
	Workflows []string `json:"workflows"`
	Types     []string `json:"types"`
	Backends  []string `json:"backends"`
	Tools     []string `json:"tools"`
}

type ImportRef struct {
	Sym  string `json:"sym"`
	Kind string `json:"kind"` // workflow | type | backend | tool | native
	Loc  Loc    `json:"loc"`
}

// RequireItem 是模块级依赖（D-014.3 / D-020）。
type RequireItem struct {
	Spec         string `json:"spec"`
	Kind         string `json:"kind"` // std | local | native
	ABIVersion   int    `json:"abi_version"`
	IRVersion    int    `json:"ir_version"`
	Origin       string `json:"origin"` // source | cli（D-020 必填）
	MemberSHA256 string `json:"member_sha256,omitempty"`
	Implicit     bool   `json:"implicit,omitempty"`
	// Name / Archive / Header 是反向融合（extern / #link native / call_native）
	// 专用的可选字段。反向融合把"我依赖一份 C 归档"写进 requires，而 C 归档
	// 的契约由头文件（Header）给出，产物名（Archive，如 libmath.a）与名字
	// （Name，通常等于归档名）用于链接期解析。它们只在 kind=="native" 时出现。
	Name    string `json:"name,omitempty"`
	Archive string `json:"archive,omitempty"`
	Header  string `json:"header,omitempty"`
}

// DiagCounts 只统计，不含文案（文案走诊断通道，不进 IR）。
// 键名是单数 error/warning —— 与 IR.md §2 的样例一致，别"顺手改成复数"。
type DiagCounts struct {
	Error   int `json:"error"`
	Warning int `json:"warning"`
}
