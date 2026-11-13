package diag

// 错误码常量。**一个 V 检查项对应一个码**，不允许一个码覆盖两件事：
// 错误码是给人 grep 的，混用会让"搜这个码搜到两种问题"。
//
// 命名规则：E_IR_<什么坏了>。E_ 前缀表示这是错误（W_ 是警告）。
const (
	CodeDupID          = "E_IR_DUP_ID"          // V-1  node id 唯一
	CodeDanglingRef    = "E_IR_DANGLING_REF"    // V-2  引用存在
	CodeCycle          = "E_IR_CYCLE"           // V-3  图无环
	CodeBadRoot        = "E_IR_BAD_ROOT"        // V-4  root 存在且是 seq
	CodeVarMismatch    = "E_IR_VAR_MISMATCH"    // V-5  var.decl ↔ decl.var 双向
	CodeBadLoc         = "E_IR_BAD_LOC"         // V-6  loc.m 在范围内
	CodeFloat          = "E_IR_FLOAT"           // V-7  无浮点
	CodeSchemaUnstable = "E_IR_SCHEMA_UNSTABLE" // V-8  schema 重编码字节相等
	CodeExportMismatch = "E_IR_EXPORT_MISMATCH" // V-9  exported ↔ exports 双向
	CodeEntry          = "E_IR_ENTRY"           // V-10 至多一个 entry 且必须 exported
	CodeSymConflict    = "E_IR_SYM_CONFLICT"    // V-11 四命名空间各自唯一
	CodePanelTruncated = "W_IR_PANEL_TRUNCATED" // 诊断数量超上限，后面的被丢弃
	CodeRecomputeDrift = "E_IR_RECOMPUTE_DRIFT" // 派生字段与复算不符（bounds/sha256）
	CodeLoadFailed     = "E_IR_LOAD_FAILED"     // 字节反序列化失败（形状层面就坏了）
	CodeDynamicCall    = "E_IR_DYNAMIC_CALL"    // 非递归函数必须编译期展开，不许 call_fn
	// CodeRetryUnproven 是 E5007 的**产物侧**对应物：判据落在 IR 节点图上
	// （从 retry.body 可达的 call_tool），不是源码 AST。编译器闸门只保护
	// hermes ir 产出的 IR；hermes run 接受任意 IR 文件，而 engine 的 retry
	// 不读 tools[].idem.mode —— 手改 / 篡改的产物能把未证明的 POST 重试成
	// 重复扣款。一个码一件事：这里只管「retry 内没有证明」，mode 取值是否
	// 属于四值枚举是 schema 的事。
	CodeRetryUnproven = "E_IR_RETRY_UNPROVEN" // retry 子树内有无幂等证明的 call_tool
	// CodeIRVersion 是**版本闸**：ir_version / language_version 只接受 2。
	//
	// 为什么两个字段共用一个码而不是各来一个：这是**同一件事**（"这份产物来自
	// 我不支持的版本"），只是坏在哪个字段上不同 —— 用 {field} 参数区分即可。
	// 拆成两个码会让人 grep 到一半以为漏了一种情况。
	CodeIRVersion = "E_IR_VERSION"

	// 编译器诊断：跨文件部分（include 图）。用 LANGUAGE.md §11.2 的数字码，
	// 因为那些码在文档里有锚点、可 grep。**单文件**的语法/类型错误由 C++ 前端产出
	// （它读源码），跨文件的由 Go 侧产出（它读文件系统）—— 分工按"谁看得见"。
	CodeIncludeCycle   = "E3002" // include 环
	CodeIncludeHermes  = "E3015" // #include 了 .hermes
	CodePathEscape     = "E1009" // 路径含 . / ..
	CodeIncludeDepth   = "E3003" // include 深度超上限
	CodeIncludeTooMany = "E3004" // 模块数超上限
	CodeLinkBadModule  = "E1010" // #link 模块名不合法
	CodeLinkNotStd     = "E3001" // #link <> 不是内置模块

	// 反向融合（extern / #link native / call_native）相关的诊断码。
	CodeCABITypeBoundary = "E3030" // 类型不能跨 C ABI 边界（json/vector/泛型）
	CodeExternNoLink     = "E3031" // extern 没有匹配的 #link native
	CodeCABINotPureC     = "E3032" // from 头不是纯 C ABI

	// 编译器诊断：C++ 前端（词法 / 语法 / 类型 / 求值 / retry）产出的数字码。
	// 锚点统一在 docsAnchorEN 里指向 docs/LANGUAGE.md 的相应小节。
	CodeSyntax                = "E1001"        // 通用语法错误
	CodeFileTooLarge          = "E1002"        // 源文件超过 8 MiB
	CodeTokenLimit            = "E1003"        // token 数超过上限
	CodeFloatLiteral          = "E1005"        // 浮点字面量写法（'.5' 非法）
	CodeCostPrecision         = "E1006"        // cost 超过 6 位小数（micro-USD）
	CodeUnterminatedString    = "E1007"        // 未闭合字符串
	CodeStringEscape          = "E1008"        // 转义 / 插值错误
	CodeGenerics              = "E2001"        // model.generate<T> 类型实参
	CodeWorkflowName          = "E2002"        // workflow 名是标识符不是字符串
	CodeJsonFieldAccess       = "E2010"        // json 字段访问缺 ']'
	CodeRetryUnlimited        = "E2015"        // retry(unlimited) 不允许
	CodeToolCallMethodLit     = "E2016"        // tool.call method 必须是字符串字面量
	CodeToolCallMethodValid   = "E2017"        // tool.call method 不是合法 HTTP 方法
	CodeAuthClause            = "E2018"        // auth 子句错误
	CodeHeaderWorkflowBody    = "E3005"        // 头文件里的 workflow 不能有体
	CodeToolDecl              = "E3009"        // tool 声明字段错误
	CodeMissingLink           = "E3016"        // 内建需要 #link
	CodeLinkInHeader          = "E3017"        // #link 只能在 .hermes
	CodeEnvConst              = "E3018"        // env() 必须是常量字符串
	CodeBackendCreds          = "E3019"        // backend 地址带凭据
	CodeHeaderBackend         = "E3020"        // 头文件不能有 backend 定义
	CodeInputNoType           = "E3023"        // input 参数必须有类型
	CodeOneWorkflow           = "E3101"        // 一个编译单元只能有一个 workflow
	CodeCompareTypes          = "E4010"        // 比较运算类型不匹配
	CodeOrderCompare          = "E4011"        // 排序比较类型不匹配
	CodeLogicalOp             = "E4012"        // 逻辑运算需要 bool
	CodeDupStruct             = "E4014"        // 重复 struct 声明
	CodeFieldAccess           = "E4016"        // 字段不存在
	CodeMatchNoDefault        = "E4018"        // match 缺 default
	CodeMatchCaseType         = "E4019"        // case 类型不对
	CodeDupCase               = "E4020"        // match 臂重复
	CodeReturnTypeMismatch    = "E4021"        // 返回类型不匹配
	CodeReturnTuple           = "E4022"        // 不能返回元组
	CodeBindErrorObj          = "E4023"        // 不能把错误对象绑定到变量
	CodeUnlimitedValue        = "E4024"        // unlimited 不是值
	CodeFallbackReturn        = "E4025"        // fallback 返回类型不匹配
	CodeConstReassignFallback = "E4026"        // fallback 内不能改 const
	CodeConditionNotBool      = "E4027"        // 条件必须是 bool
	CodeLoopVarEnded          = "E4028"        // for 循环变量越界引用
	CodeFloatNoIR             = "E4029"        // 旧 float 标签纵深防线（v2.0.0）
	CodeUnreachableCatch      = "E4002"        // catch 不可达
	CodeCostIntArith          = "E4005"        // cost 不能与裸 int 加减
	CodeNoAutoPromote         = "E4006"        // int 与 double 不自动提升
	CodeStringArith           = "E4007"        // 字符串不能参与算术
	CodeCostFloatMix          = "E4008"        // cost 与 double 不能混算
	CodeDivInt                = "E4009"        // % 需要 int 操作数
	CodeCallArgType           = "E4030"        // 调用实参类型不匹配
	CodeIndexNotInt           = "E4031"        // 数组下标必须是整数
	CodeIndexNonArray         = "E4032"        // 不能对非数组取下标
	CodeFloatEq               = "E4033"        // double 的 == / != 硬拒（不可证）
	CodeErrFieldNotYet        = "E4034"        // 错误字段规范有、运行期还没带上
	CodeParallelScoped        = "E5002"        // parallel 内声明对外不可见
	CodeRetryNoIdem           = "E5007"        // retry 内无幂等证明
	CodeIdemKeyDerived        = "E5009"        // 幂等键必须来自输入
	CodeParallelWrite         = "E5010"        // parallel 内写外部变量
	CodeLinkSemicolon         = "W2003"        // #link 不需要分号
	CodeMaxStepsUnlimited     = "W2004"        // max_steps = unlimited 警告
	CodeHTTPStatusCmp         = "W5004"        // 拿业务字段比 HTTP 状态码（启发式）
	CodeInputJsonNoContract   = "W5005"        // input: json 入口无结构契约
	CodeIRCodegen             = "E_IR_CODEGEN" // codegen 失败
)

// msg 是一条诊断的文案模板。
//
// Args 是**声明**这个模板接受哪些占位符。守卫测试做双向校验：
// 模板里出现的 {x} 必须在 Args 里；Args 里的每项也必须在模板里被用到。
// 单向校验会漏掉"声明了参数却忘了在文案里用"（翻译时看不到这个参数，
// 也就不会翻译它），以及"文案里写了个没人传的参数"（渲染出字面 {x}）。
type msg struct {
	Text string
	Why  string
	Fix  []string
	// Docs 是相关文档锚点（rustc --explain 的"相关文档"行），如
	// "docs/LANGUAGE.md#11.2"。hermes explain <code> 会打印它。为空表示暂无锚点。
	Docs string
	Args []string
}

// Lookup 按诊断码取长说明（Text/Why/Fix/Docs）。
//
// 找不到时先回落英文（catalog[LangEN]），再找不到返回 ok=false。
// 包外（如 hermes explain）用它实现 rustc --explain 等价物——所以必须导出。
func Lookup(code string, lang Lang) (msg, bool) {
	m, ok := catalog[lang][code]
	if !ok {
		// 请求的语言缺这条时回落到英文：宁可给出英文诊断，也不要给出一条
		// "no message" 的内部错误把人挡在门外。守卫测试保证这是死路径。
		m, ok = catalog[LangEN][code]
		if !ok {
			return m, false
		}
	}
	if a, ok := docsAnchor[lang][code]; ok {
		m.Docs = a
	}
	return m, true
}

var catalog = map[Lang]map[string]msg{
	LangEN: catalogEN,
	LangZH: catalogZH,
}

// docsAnchor 给每个诊断码一个文档锚点。锚点会随文档重组而变，集中放一处好维护，
// 且 URL 不分语言。hermes explain <code> 打印它作为"相关文档"行（rustc --explain 的
// "further reading"）。放这里而不是塞进每条 msg，是为了让锚点改动不污染文案评审。
var docsAnchor = map[Lang]map[string]string{
	LangEN: docsAnchorEN,
	LangZH: docsAnchorEN,
}

var docsAnchorEN = map[string]string{
	// 词法 / include / #link（LANGUAGE.md §2.7）；include 环与展开上限见 §9.3。
	CodeIncludeCycle:   "docs/LANGUAGE.md#9.3",
	CodeIncludeHermes:  "docs/LANGUAGE.md#2.7",
	CodePathEscape:     "docs/LANGUAGE.md#2.7",
	CodeIncludeDepth:   "docs/LANGUAGE.md#9.3",
	CodeIncludeTooMany: "docs/LANGUAGE.md#9.3",
	CodeLinkBadModule:  "docs/LANGUAGE.md#2.7",
	CodeLinkNotStd:     "docs/LANGUAGE.md#2.7",
	// 反向融合（extern / #link native / call_native）：规范在 LANGUAGE.md §2.7 与 IR.md。
	CodeCABITypeBoundary: "docs/LANGUAGE.md#2.7",
	CodeExternNoLink:     "docs/LANGUAGE.md#2.7",
	CodeCABINotPureC:     "docs/LANGUAGE.md#2.7",
	// IR 加载期语义码：规则在语言规范里，先指到顶层，后续可细化到具体小节。
	CodeDupID:          "docs/LANGUAGE.md",
	CodeDanglingRef:    "docs/LANGUAGE.md",
	CodeCycle:          "docs/LANGUAGE.md",
	CodeBadRoot:        "docs/LANGUAGE.md",
	CodeVarMismatch:    "docs/LANGUAGE.md",
	CodeBadLoc:         "docs/LANGUAGE.md",
	CodeFloat:          "docs/LANGUAGE.md",
	CodeSchemaUnstable: "docs/LANGUAGE.md",
	CodeExportMismatch: "docs/LANGUAGE.md",
	CodeEntry:          "docs/LANGUAGE.md",
	CodeSymConflict:    "docs/LANGUAGE.md",
	CodePanelTruncated: "docs/LANGUAGE.md",
	CodeLoadFailed:     "docs/LANGUAGE.md",
	CodeRecomputeDrift: "docs/LANGUAGE.md",
	CodeIRVersion:      "docs/LANGUAGE.md",
	CodeDynamicCall:    "docs/LANGUAGE.md",
	CodeRetryUnproven:  "docs/LANGUAGE.md",

	// C++ 前端数字码锚点
	CodeSyntax:                "docs/LANGUAGE.md#3",
	CodeFileTooLarge:          "docs/LANGUAGE.md#2.1",
	CodeTokenLimit:            "docs/LANGUAGE.md#2.1",
	CodeFloatLiteral:          "docs/LANGUAGE.md#2.5",
	CodeCostPrecision:         "docs/LANGUAGE.md#4.3",
	CodeUnterminatedString:    "docs/LANGUAGE.md#2.5",
	CodeStringEscape:          "docs/LANGUAGE.md#2.6",
	CodeGenerics:              "docs/LANGUAGE.md#3.4",
	CodeWorkflowName:          "docs/LANGUAGE.md#3",
	CodeJsonFieldAccess:       "docs/LANGUAGE.md#4.5",
	CodeRetryUnlimited:        "docs/LANGUAGE.md#5.5",
	CodeToolCallMethodLit:     "docs/LANGUAGE.md#5.6",
	CodeToolCallMethodValid:   "docs/LANGUAGE.md#5.6",
	CodeAuthClause:            "docs/LANGUAGE.md#8.5",
	CodeHeaderWorkflowBody:    "docs/LANGUAGE.md#9.4",
	CodeToolDecl:              "docs/LANGUAGE.md#3",
	CodeMissingLink:           "docs/LANGUAGE.md#2.7",
	CodeLinkInHeader:          "docs/LANGUAGE.md#9.1",
	CodeEnvConst:              "docs/LANGUAGE.md#8.5",
	CodeBackendCreds:          "docs/LANGUAGE.md#8.5",
	CodeHeaderBackend:         "docs/LANGUAGE.md#9.4",
	CodeInputNoType:           "docs/LANGUAGE.md#4.8",
	CodeOneWorkflow:           "docs/LANGUAGE.md", // 无专门小节
	CodeCompareTypes:          "docs/LANGUAGE.md#4.4",
	CodeOrderCompare:          "docs/LANGUAGE.md#4.4",
	CodeLogicalOp:             "docs/LANGUAGE.md#4.4",
	CodeDupStruct:             "docs/LANGUAGE.md#4.1",
	CodeFieldAccess:           "docs/LANGUAGE.md#4.1",
	CodeMatchNoDefault:        "docs/LANGUAGE.md#5.3",
	CodeMatchCaseType:         "docs/LANGUAGE.md#5.3",
	CodeDupCase:               "docs/LANGUAGE.md#5.3",
	CodeReturnTypeMismatch:    "docs/LANGUAGE.md#3.7",
	CodeReturnTuple:           "docs/LANGUAGE.md#3.7",
	CodeBindErrorObj:          "docs/LANGUAGE.md#5.4",
	CodeUnlimitedValue:        "docs/LANGUAGE.md#6.4",
	CodeFallbackReturn:        "docs/LANGUAGE.md#5.4.1",
	CodeConstReassignFallback: "docs/LANGUAGE.md#5.4.1",
	CodeConditionNotBool:      "docs/LANGUAGE.md#4.4",
	CodeLoopVarEnded:          "docs/LANGUAGE.md#3",
	CodeFloatNoIR:             "docs/LANGUAGE.md#4.4",
	CodeUnreachableCatch:      "docs/LANGUAGE.md#5.4",
	CodeCostIntArith:          "docs/LANGUAGE.md#4.4",
	// E4030–E4032 都出在"类型集合封闭 + 无隐式转换"这条上（§4.1）：
	// 调用实参不给隐式转换、下标只接受整数、只有数组可被索引。
	CodeCallArgType:   "docs/LANGUAGE.md#4.1",
	CodeIndexNotInt:   "docs/LANGUAGE.md#4.1",
	CodeIndexNonArray: "docs/LANGUAGE.md#4.1",
	// E4033 不是"类型集合封闭"那一族（§4.1），而是 §4.4「运算符的禁止列」：
	// 浮点相等的答案依赖最后一位，编译期证不出来，所以直接拒。
	CodeFloatEq: "docs/LANGUAGE.md#4.4",
	// E4034 是「字段在 §7.1.1 的表里，但运行期还没往里填」—— 判据与解释都在
	// §7.1.1 那张表上（约束 3 说明了字段集合封闭与 E4016 的关系）。
	CodeErrFieldNotYet:    "docs/LANGUAGE.md#7.1.1",
	CodeNoAutoPromote:     "docs/LANGUAGE.md#4.4",
	CodeStringArith:       "docs/LANGUAGE.md#4.4",
	CodeCostFloatMix:      "docs/LANGUAGE.md#4.4",
	CodeDivInt:            "docs/LANGUAGE.md#4.4",
	CodeParallelScoped:    "docs/LANGUAGE.md#5.2",
	CodeRetryNoIdem:       "docs/LANGUAGE.md#5.7",
	CodeIdemKeyDerived:    "docs/LANGUAGE.md#5.7",
	CodeParallelWrite:     "docs/LANGUAGE.md#5.2",
	CodeLinkSemicolon:     "docs/LANGUAGE.md#2.7",
	CodeMaxStepsUnlimited: "docs/LANGUAGE.md#6.4",
	// W5004 锚到 §12.1 而不是 §7.1：§7.1 讲的是 HttpError/allow_status 的设计，
	// 而"这条只是启发式、承认判不了"的登记在松点表 L3。用户点进去要看到的是
	// "为什么不能报错"，不是"HTTP 契约怎么设计"。
	CodeHTTPStatusCmp: "docs/LANGUAGE.md#12.1",
	// W5005 锚到 §4.8：那节登记了 input: json 是规范允许的逃逸口，以及
	// "编译器打 W5005、serve 无结构契约"的来龙去脉。
	CodeInputJsonNoContract: "docs/LANGUAGE.md#4.8",
	CodeIRCodegen:           "docs/LANGUAGE.md", // 通用 codegen 失败
}

var catalogEN = map[string]msg{
	CodeDupID: {
		Text: "duplicate node id {id} in workflow {wf}: first at {first}, again at {second}",
		Why: "Node ids are array indices (IR.md §4). Two nodes sharing one id make every reference to it ambiguous — a kid that says {id} could mean either of them, " +
			"and which one you get depends on traversal order.",
		Fix: []string{
			"renumber the node at {second} (ids must stay contiguous, 0..{last})",
			"or delete one of the two nodes if it is a leftover",
		},
		Args: []string{"id", "wf", "first", "second", "last"},
	},

	CodeDanglingRef: {
		// 句式刻意做成"{what} points at {id}, but {table} has {range}"：
		// 表名与范围都由调用方拼好传进来，因为只有它知道自己在查哪张表。
		// 第一版把 "node(s)" 写死在模板里，结果查 vars 表时输出
		// "vars index ... has only 0 node(s) (0..-1)" —— 单位错、空表还显示负区间。
		Text: "{what} points at {id}, but {table} has {range}",
		Why: "Every id in the IR's reference positions must index into the table it names. " +
			"A dangling id means the IR was truncated, hand-edited, or produced by a frontend that is one version out of step.",
		Fix: []string{
			"remove the reference at {path}",
			"or add the missing entry — the table must stay consistent with its references",
		},
		Args: []string{"what", "id", "table", "range", "path"},
	},

	CodeDynamicCall: {
		Text: "non-recursive function {fn} is called through call_fn at {where}: compiled mode must inline it",
		Why: "Hermes is a compiled language: non-recursive functions are expanded at compile time so the runtime has no dynamic dispatch. " +
			"A call_fn to a non-recursive function means the IR came from an older compiler, was hand-edited, or codegen has a bug. " +
			"Allowing it would silently reintroduce dynamic dispatch exactly where the bounds are supposed to be static.",
		Fix: []string{
			"rebuild the IR with the current compiler (non-recursive calls are inlined)",
			"if {fn} really recurses, mark it recursive — the depth limit 512 is the only runtime guard",
		},
		Args: []string{"fn", "where"},
	},

	CodeRetryUnproven: {
		Text: "retry node {retry} can execute call_tool {call} more than once, but tool {tool} declares idem.mode={mode} — there is no idempotency proof",
		Why:  "A retry re-runs its whole body whenever a RunError comes back. If that body contains a tool call without a proof of idempotency, every 5xx repeats the same POST: duplicate charges, duplicate side effects. E5007 is a compile-time gate on the source AST, so it cannot protect an IR file that was hand-edited, produced by an older compiler, or rearranged by a codegen bug. This check runs on the node graph at load time, where 'inside retry' means reachable from retry.body.",
		Fix: []string{
			"move call_tool {call} out of the retry subtree — keep only proven calls inside it",
			"give {tool} a real proof (GET/HEAD, idempotent = true, or a pure idem_key) and rebuild the IR with hermes ir; do not hand-edit idem.mode",
		},
		Args: []string{"retry", "call", "tool", "mode"},
	},

	CodeCycle: {
		Text: "workflow {wf}: the node graph has a cycle — {chain}",
		Why: "The IR must be a DAG (IR.md V-3). With a cycle the executor can re-enter the same node forever, " +
			"so neither a deadline nor a budget can bound the run — it would be killed from outside instead of stopping itself.",
		Fix: []string{
			"break the cycle by removing the edge {edge}",
			"if you need repetition, use `retry` — it is bounded, and the graph itself must stay acyclic",
		},
		Args: []string{"wf", "chain", "edge"},
	},

	CodeBadRoot: {
		Text: "workflow {wf}: root={root}, but {reason}",
		Why: "The executor starts at root and expects a statement list there, i.e. a node with k=\"seq\". " +
			"Any other kind has no defined entry semantics, so the first step of the run would be undefined.",
		Fix: []string{
			"point root at the first `seq` node",
			"or wrap the body in a `seq` node if the workflow has a single statement",
		},
		Args: []string{"wf", "root", "reason"},
	},

	CodeVarMismatch: {
		Text: "var {name} (id {var}) and the `decl` node binding it disagree: {detail}",
		Why: "vars[] and decl nodes are two views of one binding. The executor reserves the slot from vars[] " +
			"but assigns it in the decl node; if they disagree, the variable is read from a slot that was never written.",
		Fix: []string{
			"make var {var}.decl point at the `decl` node that assigns it",
			"or drop the vars[] entry if the decl node no longer exists",
		},
		Args: []string{"name", "var", "detail"},
	},

	CodeBadLoc: {
		Text: "{what}: loc.m={m}, but the unit has only {count} module(s) (0..{last})",
		Why: "loc.m indexes unit.modules[]. An out-of-range value crashes the diagnostic renderer itself — " +
			"the loader would fail while trying to report a failure, and you would see a panic instead of this message.",
		Fix: []string{
			"fix loc.m in the producer — this is almost always a frontend bug, not hand-edited IR",
			"or regenerate the IR if it was edited by hand",
		},
		Args: []string{"what", "m", "count", "last"},
	},

	CodeFloat: {
		Text: "IR contains a floating-point JSON number at {path} (starts with {preview})",
		Why: "Canonical JSON forbids floats (IR.md P-1). The shortest round-trip representation of a float differs " +
			"between Go, Rust and C++, so a float anywhere in the hashed bytes makes cross-language verification fail at random, " +
			"and only on some machines.",
		Fix: []string{
			"encode it as a string: {{\"k\":\"double\",\"bits\":\"3ff8000000000000\"}}",
			"money and probabilities are integers — use micro-USD (1e-6) and basis points (1e-4)",
		},
		Args: []string{"path", "preview"},
	},

	CodeSchemaUnstable: {
		Text: "schema at {path} does not re-canonicalise to the same bytes: {detail}",
		Why: "schemas[].json is embedded verbatim and participates in the type hash. If re-encoding changes even one byte, " +
			"an offline build and a signature check disagree about which schema was signed.",
		Fix: []string{
			"emit the schema with keys in byte order and no whitespace",
			"or report the frontend that produced it — this is normally a codegen bug",
		},
		Args: []string{"path", "detail"},
	},

	CodeExportMismatch: {
		Text: "workflow {name} has exported={exported}, but {where} {verb} it",
		Why: "`exported` on the workflow and exports.workflows (the symbol table) are two representations of one fact. " +
			"The stripper reads the scalar; the linker reads the table. When they disagree, one believes the symbol is reachable " +
			"and the other believes it does not exist — the artifact then works or breaks depending on which tool ran first.",
		Fix: []string{
			"set exported={want} on the workflow",
			"or {action} \"{name}\" {target}",
		},
		Args: []string{"name", "exported", "where", "verb", "want", "action", "target"},
	},

	CodeEntry: {
		Text: "{reason} (workflow {name})",
		Why: "Entry selection must be unambiguous: `hermes run` without --workflow picks the single exported entry. " +
			"Zero candidates means there is nothing to run; two means the choice is a coin flip and the artifact is not reproducible.",
		Fix: []string{
			"mark exactly one exported workflow with `entry`",
			"or pass --workflow <name> explicitly at run time",
		},
		Args: []string{"reason", "name"},
	},

	CodeSymConflict: {
		Text: "symbol {name} appears {count} times in {namespace}: {places}",
		Why: "Each of the four namespaces (type / workflow / backend / tool) must hold unique names. " +
			"With duplicates, export lookup becomes order-dependent — the linker resolves whichever it scans first, " +
			"so the same archive can link differently on two machines.",
		Fix: []string{
			"rename one of them",
			"or drop the duplicate export if it is a leftover",
		},
		Args: []string{"name", "count", "namespace", "places"},
	},

	CodePanelTruncated: {
		Text: "stopped after {shown} diagnostics; {dropped} more were suppressed",
		Why: "A single bad input can produce thousands of follow-on errors, and the useful ones are always at the top. " +
			"The cap keeps the report readable and bounds memory on hostile IR.",
		Fix: []string{
			"fix the first {shown} and re-run — the rest are usually consequences",
			"or raise the cap with --max-diags if you really want the full list",
		},
		Args: []string{"shown", "dropped"},
	},

	CodeLoadFailed: {
		Text: "the IR bytes are not well-formed JSON for this schema: {detail}",
		Why: "The loader decodes into a fixed shape. When a field has the wrong JSON type " +
			"(an object where an integer was expected, a string where a list was expected), decoding stops at the first mismatch " +
			"and nothing after it is checked — so this is the only diagnostic you get until it is fixed.",
		Fix: []string{
			"fix the reported field; the surrounding IR is probably fine",
			"or regenerate the artifact if it was hand-edited",
		},
		Args: []string{"detail"},
	},

	CodeRecomputeDrift: {
		Text: "{field} says {claimed}, but recomputing from the IR gives {actual}",
		Why: "This field is derived, not authoritative. It is written at build time and must be recomputed on load — " +
			"otherwise a tampered value would silently raise the execution ceiling, and bounds exist precisely to cap what a run may do.",
		Fix: []string{
			"regenerate the artifact — the producer wrote an inconsistent value",
			"if it was edited by hand, do not: derived fields are recomputed, not trusted",
		},
		Args: []string{"field", "claimed", "actual"},
	},

	CodeIRVersion: {
		Text: "{field} is {got}, but this build only accepts {want} (Hermes 2.0.0 and later)",
		Why: "IR is deliberately not forward-compatible: 2.x reads only what 2.0.0 and later produce. " +
			"Accepting an older artifact would mean interpreting fields whose meaning has changed since — a wrong answer that looks like a right one. " +
			"There is no migration path on purpose: 1.x artifacts are rejected loudly instead of being silently misread.",
		Fix: []string{
			"recompile the source with this compiler (2.0.0+)",
			"if you must run a 1.x artifact, use a 1.x binary — do not hand-edit the version field",
		},
		Args: []string{"field", "got", "want"},
	},

	CodeIncludeCycle: {
		Text: "include cycle: {chain}",
		Why: "Includes are expanded depth-first, so a cycle never terminates — the compiler would keep reopening the same header. " +
			"Note this is a *module* cycle, not a runtime one: breaking it changes which declarations are visible, not the execution order.",
		Fix: []string{
			"move the shared declarations into a third header that both sides include",
			"or drop one of the two includes if it is only needed for a type that can be forward-declared",
		},
		Args: []string{"chain"},
	},

	CodeIncludeHermes: {
		Text: "#include {path} targets an implementation file; only .h may be included",
		Why: "A .hermes file contains bodies. Including one would give every includer its own copy of the implementation " +
			"and make \"which implementation is linked\" depend on the include graph instead of on #link — which is exactly " +
			"the property #include/#link separation exists to protect.",
		Fix: []string{
			"to use its implementation, write: #link \"{mod}\";",
			"to use only its types, include its interface: #include \"{name}.h\"",
		},
		Args: []string{"path", "mod", "name"},
	},

	CodePathEscape: {
		Text: "include path {path} escapes the include root",
		Why: "Include paths must stay inside the project. A path containing '.' or '..' lets a header reach outside the tree — " +
			"which breaks reproducible builds (the resolved path depends on where the source tree happens to sit) and makes an " +
			"offline build read files nobody declared.",
		Fix: []string{
			"use a path relative to the include root: {suggestion}",
			"or add the directory with -I and include it by its name inside that root",
		},
		Args: []string{"path", "suggestion"},
	},

	CodeIncludeDepth: {
		Text: "include chain is {depth} levels deep, exceeding the limit of {limit}",
		Why: "The limit exists so a hostile or accidental chain cannot exhaust the stack. It is far above any readable design — " +
			"if you hit it, the header layout is the problem, not the limit.",
		Fix: []string{
			"flatten the hierarchy: put shared declarations in one leaf header",
			"or split the deepest header into two that are siblings rather than nested",
		},
		Args: []string{"depth", "limit"},
	},

	CodeIncludeTooMany: {
		Text: "the unit expands to {count} modules, exceeding the limit of {limit}",
		Why: "Every module is parsed and kept for the whole compile. The cap bounds memory on hostile input and keeps " +
			"\"how long does this build take\" answerable from the source tree alone.",
		Fix: []string{
			"split the unit: move part of it behind a #link so its modules are not re-parsed",
			"or consolidate small headers that are only ever included together",
		},
		Args: []string{"count", "limit"},
	},

	CodeLinkBadModule: {
		Text: "#link {spec} is not a valid module name",
		Why: "A #link payload is a *module name*, not a file name — it resolves to <name>.hbc. Writing a suffix or a path " +
			"makes people think absolute paths work, and then builds that work on one machine fail on another.",
		Fix: []string{
			"write the module name only: #link \"{suggestion}\";",
			"if the library lives elsewhere, point -L at its directory instead of putting a path in the source",
		},
		Args: []string{"spec", "suggestion"},
	},

	CodeLinkNotStd: {
		Text: "#link {spec} does not resolve: built-in modules are named <std/NAME>",
		Why: "Angle-bracket links only resolve against the embedded stdlib, which has a fixed, small member list. " +
			"Anything else must come from a .hbc on the -L path, and that form uses quotes, not angle brackets.",
		Fix: []string{
			"use a built-in module: {available}",
			"or link your own archive by module name: #link \"yourlib\";",
		},
		Args: []string{"spec", "available"},
	},

	CodeCABITypeBoundary: {
		Text: "type {type} cannot cross the C ABI boundary",
		Why:  "only int, bool, string, double, cost, and opaque structs have a stable C layout; json, vector<T>, and generics do not",
		Fix: []string{
			"use a supported C-ABI type: int, bool, string, double, cost, or an opaque struct",
			"pass the value inside a struct and cross the boundary by opaque pointer",
		},
		Args: []string{"type"},
	},

	CodeExternNoLink: {
		Text: "extern declaration {sym} has no matching #link native",
		Why:  "an extern symbol's implementation must come from a native archive named by #link native; without it the linker cannot resolve the symbol",
		Fix: []string{
			`add '#link native "<archive>" from "<header>";' in the same compilation unit`,
			"make sure the extern symbol name matches a symbol exported by that archive",
		},
		Args: []string{"sym"},
	},

	CodeCABINotPureC: {
		Text: "native header {header} is not a pure-C ABI header",
		Why:  "cgo only parses C; a C++ header (.hpp/.cpp/.cc) cannot serve as the native contract",
		Fix: []string{
			"provide a pure-C header (.h) for the archive",
			`wrap the C++ API in a thin extern "C" shim and point #link native at the .h shim`,
		},
		Args: []string{"header"},
	},
}

var catalogZH = map[string]msg{
	CodeDupID: {
		Text: "workflow {wf} 里 node id {id} 重复：先出现在 {first}，又出现在 {second}",
		Why: "node id 就是数组下标（IR.md §4）。两个节点共用一个 id 会让所有指向它的引用变成二义 —— " +
			"一个声称指向 {id} 的 kid 到底指哪一个，取决于遍历顺序。",
		Fix: []string{
			"把 {second} 处的节点重新编号（id 必须连续，0..{last}）",
			"或者删掉两个里的一个（如果是残留的话）",
		},
		Args: []string{"id", "wf", "first", "second", "last"},
	},

	CodeDanglingRef: {
		Text: "{what} 指向 {id}，但 {table} 只有 {range}",
		Why: "IR 里每个引用位置上的 id 都必须落在它所指的那张表内。" +
			"悬空意味着这份 IR 被截断过、被手改过，或者产它的前端差了一个版本。",
		Fix: []string{
			"删掉 {path} 处的这个引用",
			"或者补上缺失的条目 —— 表必须与它的引用保持一致",
		},
		Args: []string{"what", "id", "table", "range", "path"},
	},

	CodeDynamicCall: {
		Text: "非递归函数 {fn} 在 {where} 处通过 call_fn 调用：编译模式必须把它展开",
		Why: "Hermes 是编译型语言：非递归函数在编译期展开，运行期没有动态派发。" +
			"指向非递归函数的 call_fn 说明 IR 来自旧编译器、被手改过，或者 codegen 有 bug。" +
			"放行它等于在上界本该静态的地方悄悄恢复动态派发。",
		Fix: []string{
			"用当前编译器重新生成 IR（非递归调用会被内联）",
			"如果 {fn} 确实递归，把它标成 recursive —— 深度上限 512 是唯一的运行期闸",
		},
		Args: []string{"fn", "where"},
	},

	CodeRetryUnproven: {
		Text: "retry 节点 {retry} 会把 call_tool {call} 执行多次，但工具 {tool} 的 idem.mode={mode} —— 没有任何幂等证明",
		Why:  "retry 在遇到 RunError 时会重跑整个 body。如果 body 里有未证明幂等的工具调用，每一次 5xx 都会把同一个 POST 再发一遍：重复扣款、重复副作用。E5007 是源码 AST 上的编译期闸门，保护不了被手改过的、旧编译器产出的、或被 codegen bug 搬过位置的 IR 文件。这条校验在加载期落在节点图上 ——「retry 内」的准确含义是「从 retry.body 可达」。",
		Fix: []string{
			"把 call_tool {call} 移出 retry 子树，只把已证明幂等的调用留在里面",
			"给 {tool} 一个真证明（GET/HEAD、idempotent = true 或纯 idem_key），用 hermes ir 重新生成 IR；不要手改 idem.mode",
		},
		Args: []string{"retry", "call", "tool", "mode"},
	},

	CodeCycle: {
		Text: "workflow {wf} 的节点图有环 —— {chain}",
		Why: "IR 必须是有向无环图（IR.md V-3）。有环意味着执行器可以永远重新进入同一个节点，" +
			"于是超时和预算都兜不住 —— 它只能被外部杀掉，而不是自己停下来。",
		Fix: []string{
			"删掉 {edge} 这条边来打破环",
			"如果要重复执行，用 `retry` —— 它是有界的，而图本身必须无环",
		},
		Args: []string{"wf", "chain", "edge"},
	},

	CodeBadRoot: {
		Text: "workflow {wf}：root={root}，但 {reason}",
		Why: "执行器从 root 开始，并在那里期待一个语句列表，也就是 k=\"seq\" 的节点。" +
			"其他 kind 没有定义好的入口语义，跑起来第一步就是未定义行为。",
		Fix: []string{
			"把 root 指向第一个 `seq` 节点",
			"或者如果 workflow 只有一条语句，把它包进一个 `seq` 节点",
		},
		Args: []string{"wf", "root", "reason"},
	},

	CodeVarMismatch: {
		Text: "变量 {name}（id {var}）与绑定它的 `decl` 节点不一致：{detail}",
		Why: "vars[] 和 decl 节点是同一个绑定的两种视图。执行器按 vars[] 预留槽位、在 decl 节点里赋值；" +
			"两者不一致时，变量会从一个从没被写过的槽里读出来。",
		Fix: []string{
			"让 var {var}.decl 指向给它赋值的那个 `decl` 节点",
			"或者如果 decl 节点已经不存在，删掉这条 vars[] 记录",
		},
		Args: []string{"name", "var", "detail"},
	},

	CodeBadLoc: {
		Text: "{what}：loc.m={m}，但 unit 只有 {count} 个模块（0..{last}）",
		Why: "loc.m 是 unit.modules[] 的下标。越界会让**诊断渲染器自己崩掉** —— " +
			"加载器在试图报告一个失败时失败，你看到的会是 panic 而不是这条消息。",
		Fix: []string{
			"修产生方的 loc.m —— 这几乎总是前端 bug，不是手改 IR",
			"如果是手工编辑过的 IR，重新生成一份",
		},
		Args: []string{"what", "m", "count", "last"},
	},

	CodeFloat: {
		Text: "IR 在 {path} 处出现浮点 JSON 数字（开头是 {preview}）",
		Why: "canonical JSON 禁止浮点（IR.md P-1）。浮点的最短往返表示在 Go / Rust / C++ 之间有真实分歧，" +
			"只要它落进被哈希的字节里，跨语言校验就会随机失败，而且只在某些机器上失败。",
		Fix: []string{
			"编码成字符串：{{\"k\":\"double\",\"bits\":\"3ff8000000000000\"}}",
			"金额与概率用整数 —— 微美元（1e-6）和基点（1e-4）",
		},
		Args: []string{"path", "preview"},
	},

	CodeSchemaUnstable: {
		Text: "{path} 处的 schema 重新 canonical 化后字节不同：{detail}",
		Why: "schemas[].json 是原样嵌入的，并参与类型哈希。只要重编码改了一个字节，" +
			"离线构建与验签就会对\"签的是哪份 schema\"产生分歧。",
		Fix: []string{
			"按字节序输出键、不留空白",
			"或者报告产它的前端 —— 这一般是 codegen 的 bug",
		},
		Args: []string{"path", "detail"},
	},

	CodeExportMismatch: {
		Text: "workflow {name} 的 exported={exported}，但 {where} {verb} 它",
		Why: "workflow 上的 `exported` 和 exports.workflows（符号表）是同一个事实的两份表示。" +
			"剥离器读标量，链接器读符号表。两者不一致时，一边认为符号可达、另一边认为它不存在 —— " +
			"于是产物能不能用取决于哪个工具先跑。",
		Fix: []string{
			"把 workflow 的 exported 设为 {want}",
			"或者 {action} \"{name}\" {target}",
		},
		Args: []string{"name", "exported", "where", "verb", "want", "action", "target"},
	},

	CodeEntry: {
		Text: "{reason}（workflow {name}）",
		Why: "入口选择必须唯一：不带 --workflow 的 `hermes run` 会选唯一的那个导出入口。" +
			"零个候选意味着没东西可跑；两个意味着选择是抛硬币，产物不可重现。",
		Fix: []string{
			"给恰好一个导出的 workflow 打上 `entry`",
			"或者在运行时显式传 --workflow <name>",
		},
		Args: []string{"reason", "name"},
	},

	CodeSymConflict: {
		Text: "符号 {name} 在 {namespace} 里出现 {count} 次：{places}",
		Why: "四个命名空间（type / workflow / backend / tool）各自必须名称唯一。" +
			"有重复时导出查找就依赖顺序 —— 链接器解析它先扫到的那个，" +
			"于是同一份归档在两台机器上可能链出不同结果。",
		Fix: []string{
			"把其中一个改名",
			"或者如果是残留，删掉重复的导出",
		},
		Args: []string{"name", "count", "namespace", "places"},
	},

	CodePanelTruncated: {
		Text: "在 {shown} 条诊断后停止；另有 {dropped} 条被抑制",
		Why: "一份坏输入可以产生上千条连带的错误，而有用的永远在最上面。" +
			"上限让报告保持可读，也给恶意 IR 的产出的内存定了界。",
		Fix: []string{
			"先修前 {shown} 条再重跑 —— 其余通常是连带结果",
			"确实想看全量的话用 --max-diags 提高上限",
		},
		Args: []string{"shown", "dropped"},
	},

	CodeLoadFailed: {
		Text: "IR 字节不是本 schema 期望的合法 JSON：{detail}",
		Why: "加载器按固定形状解码。某个字段的 JSON 类型不对时（该是整数的地方给了对象、该是列表的地方给了字符串），" +
			"解码会在第一处不匹配处停下，之后的内容一概不检查 —— 所以在修好它之前你只会看到这一条。",
		Fix: []string{
			"修报出来的那个字段；周围的内容大概率是好的",
			"如果这份产物是手改的，重新生成一份",
		},
		Args: []string{"detail"},
	},

	CodeRecomputeDrift: {
		Text: "{field} 声称 {claimed}，但从 IR 复算得到 {actual}",
		Why: "这个字段是派生的，不是权威值。它在构建时写入、必须在加载时复算 —— " +
			"否则一个被篡改的值就能悄悄抬高执行上限，而 bounds 存在的意义恰恰是封住一次运行能做什么。",
		Fix: []string{
			"重新生成产物 —— 产生方写了不一致的值",
			"如果是手改的：不要改，派生字段是复算出来的，不是信来的",
		},
		Args: []string{"field", "claimed", "actual"},
	},

	CodeIRVersion: {
		Text: "{field} 是 {got}，但本版本只接受 {want}（Hermes 2.0.0 及之后）",
		Why: "IR 有意不做向前兼容：2.x 只读 2.0.0 及之后产出的东西。" +
			"接受更旧的产物，等于按现在的含义去解释语义已经变过的字段 —— 一个看起来像正确答案的错误答案。" +
			"没有迁移路径是有意的：1.x 的产物被明确拒绝，而不是被静默误读。",
		Fix: []string{
			"用这个编译器（2.0.0+）重新编译源码",
			"确实要跑 1.x 产物的话，用 1.x 的二进制 —— 不要手改版本字段",
		},
		Args: []string{"field", "got", "want"},
	},

	CodeIncludeCycle: {
		Text: "include 成环：{chain}",
		Why: "include 是深度优先展开的，成环就永不终止 —— 编译器会反复重新打开同一个头文件。" +
			"注意这是**模块**环，不是运行期环：打破它改变的是\"哪些声明可见\"，不是执行顺序。",
		Fix: []string{
			"把共用的声明挪进第三个头文件，两边都 include 它",
			"或者如果某个 include 只是为了一个可以前向声明的类型，删掉它",
		},
		Args: []string{"chain"},
	},

	CodeIncludeHermes: {
		Text: "#include {path} 指向的是实现文件；只能 include .h",
		Why: ".hermes 里有函数体。include 它等于让每个包含者都拿到一份自己的实现副本，" +
			"并把\"链的是哪份实现\"从 #link 手里挪到 include 图上 —— 而 #include/#link 分离存在的意义正是守住这个性质。",
		Fix: []string{
			"要用它的实现，写：#link \"{mod}\";",
			"只要它的类型，include 它的接口：#include \"{name}.h\"",
		},
		Args: []string{"path", "mod", "name"},
	},

	CodePathEscape: {
		Text: "include 路径 {path} 逃出了 include 根目录",
		Why: "include 路径必须留在项目内。含 '.' 或 '..' 的路径能让头文件伸到源码树外面 —— " +
			"这会破坏可重现构建（解析结果取决于源码树恰好放在哪），也让离线构建去读没人声明过的文件。",
		Fix: []string{
			"用相对 include 根的路径：{suggestion}",
			"或者用 -I 把那个目录加进来，再按它在那个根内的名字 include",
		},
		Args: []string{"path", "suggestion"},
	},

	CodeIncludeDepth: {
		Text: "include 链深达 {depth} 层，超过上限 {limit}",
		Why: "上限存在是为了让恶意或误写的链条耗尽不了栈。它远高于任何可读的设计 —— " +
			"真撞到上限，说明头文件层次有问题，不是上限有问题。",
		Fix: []string{
			"压平层次：把共用声明放进一个叶子头文件",
			"或者把最深的那个头拆成两个平级的，而不是嵌套的",
		},
		Args: []string{"depth", "limit"},
	},

	CodeIncludeTooMany: {
		Text: "本单元展开出 {count} 个模块，超过上限 {limit}",
		Why: "每个模块都要解析并保留到编译结束。上限给恶意输入的内存定了界，" +
			"也让\"这个构建要多久\"能从源码树本身回答。",
		Fix: []string{
			"拆分单元：把一部分挪到 #link 后面，那样它的模块不会被重新解析",
			"或者合并那些总是一起被 include 的小头文件",
		},
		Args: []string{"count", "limit"},
	},

	CodeLinkBadModule: {
		Text: "#link {spec} 不是合法的模块名",
		Why: "#link 的载荷是**模块名**不是文件名 —— 它解析成 <name>.hbc。写后缀或路径会让人以为绝对路径可行，" +
			"然后一台机器上能过的构建在另一台上失败。",
		Fix: []string{
			"只写模块名：#link \"{suggestion}\";",
			"库在别处的话，把 -L 指向它的目录，而不是把路径写进源码",
		},
		Args: []string{"spec", "suggestion"},
	},

	CodeLinkNotStd: {
		Text: "#link {spec} 无法解析：内置模块的名字是 <std/NAME>",
		Why: "尖括号形式的链接只在内嵌 stdlib 里解析，而它的成员表是固定且很小的。" +
			"其他一切必须来自 -L 路径上的 .hbc，而那种形式用引号不用尖括号。",
		Fix: []string{
			"用内置模块：{available}",
			"或者按模块名链接你自己的归档：#link \"yourlib\";",
		},
		Args: []string{"spec", "available"},
	},

	CodeCABITypeBoundary: {
		Text: "类型 {type} 不能跨 C ABI 边界",
		Why:  "只有 int、bool、string、double、cost 和不透明结构体在 C 里有稳定布局；json、vector<T>、泛型都没有",
		Fix: []string{
			"改用受支持的 C-ABI 类型：int、bool、string、double、cost 或不透明结构体",
			"把值包进结构体，以不透明指针跨边界",
		},
		Args: []string{"type"},
	},

	CodeExternNoLink: {
		Text: "extern 声明 {sym} 没有匹配的 #link native",
		Why:  "extern 符号的实现必须来自 #link native 指名的原生归档；缺它链接器无法解析该符号",
		Fix: []string{
			"在同一编译单元里加 '#link native \"<归档>\" from \"<头>\";'",
			"确认 extern 符号名与该归档导出的符号一致",
		},
		Args: []string{"sym"},
	},

	CodeCABINotPureC: {
		Text: "原生头 {header} 不是纯 C ABI 头",
		Why:  "cgo 只解析 C；C++ 头（.hpp/.cpp/.cc）不能充当原生契约",
		Fix: []string{
			"为归档提供纯 C 头（.h）",
			"把 C++ API 包一层 extern \"C\" 薄封装，让 #link native 指向 .h 封装头",
		},
		Args: []string{"header"},
	},
}
