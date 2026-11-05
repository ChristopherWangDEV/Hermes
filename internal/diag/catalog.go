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
	CodeVarMismatch    = "E_IR_VAR_MISMATCH"    // V-5  var.decl ↔ let.var 双向
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
	Args []string
}

func lookup(code string, lang Lang) (msg, bool) {
	if m, ok := catalog[lang][code]; ok {
		return m, true
	}
	// 请求的语言缺这条时回落到英文：宁可给出英文诊断，也不要给出一条
	// "no message" 的内部错误把人挡在门外。守卫测试保证这是死路径。
	m, ok := catalog[LangEN][code]
	return m, ok
}

var catalog = map[Lang]map[string]msg{
	LangEN: catalogEN,
	LangZH: catalogZH,
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
		Text: "var {name} (id {var}) and the `let` node binding it disagree: {detail}",
		Why: "vars[] and let nodes are two views of one binding. The executor reserves the slot from vars[] " +
			"but assigns it in the let node; if they disagree, the variable is read from a slot that was never written.",
		Fix: []string{
			"make var {var}.decl point at the `let` node that assigns it",
			"or drop the vars[] entry if the let node no longer exists",
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
			"encode it as a string: {{\"t\":\"float\",\"v\":\"1.5\"}}",
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
		Text: "变量 {name}（id {var}）与绑定它的 `let` 节点不一致：{detail}",
		Why: "vars[] 和 let 节点是同一个绑定的两种视图。执行器按 vars[] 预留槽位、在 let 节点里赋值；" +
			"两者不一致时，变量会从一个从没被写过的槽里读出来。",
		Fix: []string{
			"让 var {var}.decl 指向给它赋值的那个 `let` 节点",
			"或者如果 let 节点已经不存在，删掉这条 vars[] 记录",
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
			"编码成字符串：{{\"t\":\"float\",\"v\":\"1.5\"}}",
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
}
