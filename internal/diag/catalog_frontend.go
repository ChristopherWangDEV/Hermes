package diag

// catalog_frontend.go 补齐**编译器前端**（native/cpp）诊断码的长说明，供
// `hermes explain <code>`（rustc --explain 等价物）使用。
//
// 分工：
//   - catalog.go 持有这些码的**常量**与**文档锚点**（docsAnchorEN，含真实小节号）；
//   - 本文件持有它们的**文案**（Text/Why/Fix），借 init() 并入 catalog。
//
// 单文件的语法/类型错误由 C++ 前端产出（它读源码），其 code 在 native/cpp 里以字面量
// 内联出现，既不在 IR 也不在 include 图里；catalog.go 里原有的 E_IR_* 与 include/link/CABI
// 码覆盖不到它们，所以这里单独登记。已在 catalog.go 内建条目的码（E1009/E1010/
// E3001..E3004/E3015/E3030..E3032）**不**在此重复，避免 map 出现重复键。

func init() {
	for lang, m := range catalogFrontend {
		for code, mm := range m {
			catalog[lang][code] = mm
		}
	}
}

var catalogFrontend = map[Lang]map[string]msg{
	LangEN: catalogFrontendEN,
	LangZH: catalogFrontendZH,
}

var catalogFrontendEN = map[string]msg{
	CodeSyntax: {
		Text: "syntax error: the parser could not match a declaration or statement here",
		Why:  "The grammar expects a specific token at this point; a missing separator, an unbalanced brace, or a misspelled keyword breaks the match, and parsing stops until it is fixed.",
		Fix: []string{
			"check the punctuation and keywords around the reported position against LANGUAGE.md §3",
			"if the message names a token kind, look for a typo just before it",
		},
	},
	CodeFileTooLarge: {
		Text: "source file exceeds the 8 MiB limit",
		Why:  "A single file is capped at 8 MiB so the compiler's memory stays bounded on hostile or runaway input; include expansion is counted separately.",
		Fix: []string{
			"split the file into smaller modules",
			"trim its contents, or move part of it behind a #link",
		},
	},
	CodeTokenLimit: {
		Text: "token count exceeds the 2,000,000 limit",
		Why:  "The total token count after include expansion is capped so parse time and memory stay bounded and build time remains answerable from the source tree.",
		Fix: []string{
			"split the file or reduce its includes",
			"move rarely used declarations behind a #link so they are not re-parsed",
		},
	},
	CodeFloatLiteral: {
		Text: "a double literal must be written as D.D ('.5' is invalid)",
		Why:  "A leading dot is confused with the '.' of field access; requiring D.D keeps the lexer unambiguous.",
		Fix: []string{
			"write 0.5 instead of .5",
			"use `double` for fractional values; keep money in `cost` / int micro-units",
		},
	},
	CodeCostPrecision: {
		Text: "amount has more than 6 decimal places (micro-USD limit)",
		Why:  "cost is micro-USD and allows at most 6 decimal places; anything finer cannot be represented and would be silently rounded.",
		Fix: []string{
			"round the amount to 6 decimal places",
			"or express it in micro-USD integers directly",
		},
	},
	CodeUnterminatedString: {
		Text: "unterminated string (must be single-line)",
		Why:  "An unterminated or line-broken string swallows the following code, turning one typo into a cascade of unrelated errors.",
		Fix: []string{
			"close the string on the same line",
			"use \\n for a newline, or split the content into multiple segments",
		},
	},
	CodeStringEscape: {
		Text: "invalid string escape or interpolation",
		Why:  "Escapes and interpolation must follow the grammar; a malformed one cannot be decoded, and a line-broken string would swallow the code after it.",
		Fix: []string{
			"valid escapes: \\\\ \\\" \\n \\r \\t, and \\u escapes with 4 hex digits",
			"interpolation must be a plain path such as input.text",
		},
	},
	CodeGenerics: {
		Text: "expected '(' after a type-argument list",
		Why:  "A generic callee written as name<T> must be followed by its call parentheses; leaving them off makes the <T> read as a less-than comparison.",
		Fix: []string{
			"write name<T>(...)",
			"or drop the <T> and let the type be inferred",
		},
	},
	CodeWorkflowName: {
		Text: "a workflow name is an identifier, not a quoted string",
		Why:  "Workflow names are identifiers so they can be linked and exported by symbol; a quoted string cannot be a symbol name.",
		Fix: []string{
			"write the name without quotes: workflow Name",
			"if you meant a string value, move it into the body",
		},
	},
	CodeJsonFieldAccess: {
		Text: "expected ']' after json field access",
		Why:  "Json field access is bracket syntax; a missing ']' leaves the access unterminated and the rest of the expression misparsed.",
		Fix: []string{
			"close the access: obj[\"key\"]",
			"check the key string and its closing bracket",
		},
	},
	CodeRetryUnlimited: {
		Text: "the retry count and delay must be integer literals",
		Why:  "The retry bound is baked into the IR's cost upper bound; a non-literal count or delay cannot be statically bounded, so the strip and trace limits would be wrong.",
		Fix: []string{
			"write literal integers: retry 3 times delay 100",
			"retry bounds are compile-time — a runtime value cannot be used here",
		},
	},
	CodeToolCallMethodLit: {
		Text: "tool.call method must be a string literal",
		Why:  "The HTTP method decides idempotency (GET/HEAD are safe to retry); it must be readable at compile time, so a computed method is rejected.",
		Fix: []string{
			"pass a literal: tool.call(\"GET\", ...)",
			"if the method varies, split into separate call sites",
		},
	},
	CodeToolCallMethodValid: {
		Text: "tool.call method is not a valid HTTP method",
		Why:  "Only the standard methods are recognized; an unknown method cannot be mapped to an idempotency class, so retry safety cannot be proven.",
		Fix: []string{
			"use GET, HEAD, POST, PUT, PATCH, or DELETE",
			"check for a typo or wrong casing",
		},
	},
	CodeAuthClause: {
		Text: "an auth clause must be auth env(\"NAME\"): a secret comes only from the environment",
		Why:  "The secret source must be statically auditable; allowing an expression or a default lets the backend start unconfigured and fail only with a 401 at runtime.",
		Fix: []string{
			"write auth env(\"NAME\") with a literal variable name",
			"remove any default — a secret has no safe default",
		},
	},
	CodeHeaderWorkflowBody: {
		Text: "a workflow in a header (.h) cannot have a body",
		Why:  "Headers declare interfaces only; a body in a header would be duplicated into every includer, so which implementation runs would depend on the include graph instead of #link.",
		Fix: []string{
			"move the body to the matching .hermes implementation",
			"keep only the signature in the header",
		},
	},
	CodeToolDecl: {
		Text: "a tool declaration field must have the form name = \"value\";",
		Why:  "Tool metadata (endpoint, method, idempotency) is a flat set of string fields; a malformed field means the tool cannot be described to the runtime.",
		Fix: []string{
			"write each field as name = \"value\";",
			"check that the value is a quoted string literal",
		},
	},
	CodeMissingLink: {
		Text: "tool.call needs #link <std/net>; model.generate needs #link <std/model>",
		Why:  "Built-in capabilities are not ambient: the networking and model primitives only exist if the unit links their stdlib module, so an unlinked call has no implementation.",
		Fix: []string{
			"add #link <std/net>; for tool.call",
			"add #link <std/model>; for model.generate",
		},
	},
	CodeLinkInHeader: {
		Text: "#link is only allowed in .hermes files",
		Why:  "Linking chooses an implementation; headers describe interfaces and are included many times, so a #link there would pull one implementation into every includer.",
		Fix: []string{
			"move the #link into the .hermes implementation",
			"keep only #include in the header",
		},
	},
	CodeEnvConst: {
		Text: "env() arguments must be constant strings: interpolation is not a constant",
		Why:  "A backend endpoint or credential name is read at build time; an interpolated value is only known at runtime, so it cannot be baked into the configuration.",
		Fix: []string{
			"pass a plain string literal to env()",
			"move any interpolation into runtime code instead",
		},
	},
	CodeBackendCreds: {
		Text: "a backend address must not carry credentials (user:pass@host)",
		Why:  "Credentials in a URL end up in logs, traces, and the IR; they must come from env() so they are not written into the artifact or leaked.",
		Fix: []string{
			"remove user:pass@ from the address",
			"supply the secret via auth env(\"NAME\")",
		},
	},
	CodeHeaderBackend: {
		Text: "a header (.h) cannot contain a backend definition",
		Why:  "A backend definition names an address and a secret source — implementation, not interface; putting it in a header gives every includer its own copy.",
		Fix: []string{
			"move the backend definition to the .hermes implementation",
			"keep only a declaration in the header if needed",
		},
	},
	CodeInputNoType: {
		Text: "the input argument must have a type",
		Why:  "input's type is the request contract; without it the decoder has no schema to check the incoming body against.",
		Fix: []string{
			"annotate the type: input: Req",
			"give the struct a matching schema",
		},
	},
	CodeOneWorkflow: {
		Text: "a compilation unit may contain only one workflow (the rest would be dropped)",
		Why:  "The assembly stage builds IR around a single workflow; an extra one never reaches the output and looks perfectly normal — data silently dropped.",
		Fix: []string{
			"split each workflow into its own .hermes file",
			"build each one separately",
		},
	},
	CodeUnreachableCatch: {
		Text: "this catch is unreachable: an earlier catch (Error e) already catches everything",
		Why:  "Catch arms are tried in order; a catch (Error) before a narrower catch swallows every error, so the later arm can never run.",
		Fix: []string{
			"move the broad catch (Error) to the end",
			"or remove the unreachable arm",
		},
	},
	CodeCostIntArith: {
		Text: "cost cannot be added to or subtracted by a bare int",
		Why:  "cost is micro-USD; adding a bare int treats that int as micro-USD by accident, a unit error that silently changes money by a factor of 10^6.",
		Fix: []string{
			"wrap the int as a cost explicitly",
			"or keep the amount in one unit throughout",
		},
	},
	CodeNoAutoPromote: {
		Text: "int and double do not auto-promote",
		Why:  "There is no implicit numeric promotion; automatic int to double is the classic source of silent precision loss and rounding bugs.",
		Fix: []string{
			"convert explicitly to the type you want",
			"or keep both operands the same type",
		},
	},
	CodeStringArith: {
		Text: "an arithmetic operator was applied to a non-numeric operand (string, bool, json)",
		Why:  "Strings have no arithmetic and there is no operator overloading; concatenation is deliberately disallowed because it hides encoding and locale, and unary minus is only defined on numbers.",
		Fix: []string{
			"combine strings with interpolation (put the value inside the string) instead of an operator",
			"narrow json to a numeric type before arithmetic (json.as_int)",
		},
	},
	CodeCostFloatMix: {
		Text: "cost and double do not mix in arithmetic",
		Why:  "cost is integer micro-USD; mixing it with a double reintroduces the exact rounding error cost was designed to avoid.",
		Fix: []string{
			"keep money math in cost (integers)",
			"or convert deliberately and round explicitly",
		},
	},
	CodeDivInt: {
		Text: "operator % requires int operands",
		Why:  "Remainder is only meaningful on integers; a remainder on cost, double, string, json, or struct has no well-defined result.",
		Fix: []string{
			"use int operands for %",
			"for cost, work in micro-USD ints first",
		},
	},
	CodeCompareTypes: {
		Text: "cannot compare two values of different types with == or !=",
		Why:  "Equality requires both operands to be the same type; comparing across types has no single meaning and would need a hidden coercion.",
		Fix: []string{
			"compare values of the same type",
			"convert one side explicitly to match",
		},
	},
	CodeOrderCompare: {
		Text: "cannot order-compare these operand types",
		Why:  "Ordering (< <= > >=) is only defined for int, double, cost, and string; bool, json, and struct have no total order.",
		Fix: []string{
			"compare only int, double, cost, or string",
			"for json, compare a specific extracted field",
		},
	},
	CodeLogicalOp: {
		Text: "logical &&, ||, and ! require bool operands",
		Why:  "The language has no truthiness; &&, ||, ! are boolean connectives, so an int/string/json operand would need an implicit nonzero-means-true rule that does not exist.",
		Fix: []string{
			"compare explicitly: x != 0, or s != a quoted empty string",
			"or change the operand to a bool",
		},
	},
	CodeDupStruct: {
		Text: "duplicate struct declaration in this file",
		Why:  "Each struct name maps to one layout; a second declaration in the same file makes the type ambiguous and the field order depend on which one wins.",
		Fix: []string{
			"rename one of the two structs",
			"or remove the duplicate if it is a leftover",
		},
	},
	CodeFieldAccess: {
		Text: "the type has no such field",
		Why:  "Field access reads a slot at a fixed offset; a field the type does not declare would read bytes that were never laid out, so the access cannot be compiled.",
		Fix: []string{
			"use one of the type's declared fields",
			"or add the field to the struct declaration",
		},
	},
	CodeMatchNoDefault: {
		Text: "`match` has no `default` branch",
		Why:  "Exhaustiveness cannot be proven for string subjects, so a match without default would have an undefined result for an unmatched value.",
		Fix: []string{
			"add a default: arm",
			"or narrow the subject so the cases are provably exhaustive",
		},
	},
	CodeMatchCaseType: {
		Text: "a `case` label has a different type than the `match` subject",
		Why:  "Case labels are compared to the subject; an int label on a string subject (or vice versa) can never match and signals a type confusion.",
		Fix: []string{
			"make the label the same type as the subject",
			"or convert the subject before matching",
		},
	},
	CodeDupCase: {
		Text: "the same case label appears more than once",
		Why:  "A duplicated label makes the second arm dead and hides that the author probably meant a different value.",
		Fix: []string{
			"remove the duplicate arm",
			"or change it to the value you actually meant",
		},
	},
	CodeReturnTypeMismatch: {
		Text: "return type mismatch: the returned value does not match the declared return type",
		Why:  "The declared return type is the workflow's contract — callers, the linker, and the IR schema all rely on it; a different value breaks all of them.",
		Fix: []string{
			"return a value of the declared type",
			"or change the declaration to the type you return",
		},
	},
	CodeReturnTuple: {
		Text: "cannot return a tuple",
		Why:  "A workflow's return type must be a single named type with a schema; a tuple has no name, so it cannot be exported or described to callers.",
		Fix: []string{
			"wrap the values in a struct and return that",
			"or return one value and pass the rest via input",
		},
	},
	CodeBindErrorObj: {
		Text: "cannot bind the error object to a variable",
		Why:  "The catch error object lives only inside its catch block and is borrowed, not owned; binding it out would let it outlive the error it points to.",
		Fix: []string{
			"use the error object inline inside the catch block",
			"if you need its data later, copy the fields out explicitly",
		},
	},
	CodeUnlimitedValue: {
		Text: "`unlimited` is not a value: it may only appear as max_steps = unlimited",
		Why:  "`unlimited` means no compile-time upper bound; it is a bound marker, not a constant. Allowing it in a value position would create a variable with no value and break every bound.",
		Fix: []string{
			"use unlimited only in model.agent(max_steps = unlimited)",
			"assign a concrete value elsewhere",
		},
	},
	CodeFallbackReturn: {
		Text: "fallback return type does not match the main expression",
		Why:  "The fallback is the value used when the main expression raises; if its type differs, callers would get one of two incompatible types at runtime.",
		Fix: []string{
			"make the fallback return the same type as the main expression",
			"or convert the fallback value to that type",
		},
	},
	CodeConstReassignFallback: {
		Text: "cannot reassign a const variable inside a fallback block",
		Why:  "A const is immutable for the whole scope; a fallback is not an exception — reassigning there would make const mean different things in different blocks.",
		Fix: []string{
			"use a non-const variable if it must change",
			"or move the assignment out of the fallback",
		},
	},
	CodeConditionNotBool: {
		Text: "an if/while condition must be bool",
		Why:  "A condition is a control-flow branch point and the language has no truthiness; int/string/json/cost have no notion of being true, so an implicit rule would be undefined at runtime.",
		Fix: []string{
			"compare explicitly: x != 0, or s != a quoted empty string",
			"or produce a bool from the expression",
		},
	},
	CodeLoopVarEnded: {
		Text: "the loop variable of a `for` loop is used after the loop has ended",
		Why:  "The loop variable is owned by the for header and exists only inside the body; its slot is reclaimed when the loop ends, so referencing it outside reads a variable that no longer exists.",
		Fix: []string{
			"move the use inside the loop body",
			"or copy the value to a variable declared before the loop",
		},
	},
	CodeFloatNoIR: {
		Text: "legacy float label has no IR value representation",
		Why: "This is a legacy-tag defense line, not a ban on floating point. Since v2.0.0 the only floating type is `double` (IEEE-754 binary64) and literals are emitted as the bit pattern {{\"k\":\"double\",\"bits\":\"<16 lowercase hex digits>\"}}; IR bytes contain no JSON floating-point numbers, so V-7 / canonical / signing all hold. " +
			"A LitExpr tagged \"float\" is the pre-v2.0.0 shape: it falls into the string fallback and produces {{\"t\":\"string\",\"v\":\"float\"}} — valid IR that silently returns the string \"float\" at runtime. The three scanners stay as defense in depth against C ABI callers that ignore diagnostics.",
		Fix: []string{
			"use `double` for floating-point values (literals 1.5 / 1.5e2 / 1e3 are supported)",
			"keep money in `cost` / int micro-units; the `json.as_float` API name is unchanged",
		},
	},
	CodeCallArgType: {
		Text: "argument type mismatch in a call",
		Why:  "A call passes a value of one type where the callee declares another; with no implicit conversion, the call cannot be compiled to the declared signature.",
		Fix: []string{
			"pass a value of the declared parameter type",
			"or convert the argument explicitly",
		},
	},
	CodeIndexNotInt: {
		Text: "an array index must be an integer",
		Why:  "Array indexing is an integer offset; a non-int index has no defined position and would need a hidden truncation.",
		Fix: []string{
			"use an int index",
			"or convert the index to int explicitly",
		},
	},
	CodeIndexNonArray: {
		Text: "cannot index a value that is not an array",
		Why:  "Only arrays support indexed access; indexing an int/string/json/struct has no element to return.",
		Fix: []string{
			"index an array value",
			"for json, extract a typed array first (json.as_array)",
		},
	},
	CodeFloatEq: {
		Text: "cannot compare two doubles with == or !=",
		Why: "Exact equality on binary floating point is not a stable predicate, so it cannot be proven at compile time and is rejected instead of silently returning false. " +
			"Rounding makes two mathematically equal values compare unequal, and NaN is unequal to itself: `x == x` can be false, and `x != x` quietly becomes the only NaN test in the language.",
		Fix: []string{
			"keep the value in exact units (int micro-units, or `cost`) and compare that",
			"or compare with an explicit tolerance: use a `double` literal or a value from the input, e.g. `json.as_float(input.eps)`; equality itself is still rejected",
		},
	},
	CodeErrFieldNotYet: {
		Text: "this error field is specified but the runtime does not produce it yet",
		Why: "LANGUAGE.md 7.1.1 declares the field, so it is not a spelling mistake — but the executor's error value does not carry it yet. " +
			"Accepting the access would compile and then die at run time with `field does not exist`, which is a plain error and cannot be caught: a static checker that passes while the program fails is worse than a refusal.",
		Fix: []string{
			"read a field the runtime already carries (E4016's help lists them per error type)",
			"or read the outcome as a normal value: with `allow_status = [404]` the status comes back on HttpResult<T>, so `r.status == 404` needs no error object at all",
		},
	},
	CodeParallelScoped: {
		Text: "a value declared inside a `parallel` block is not visible outside it",
		Why:  "Each branch of parallel runs concurrently; a value produced inside the block has no single well-defined value outside, so it is scoped to the block.",
		Fix: []string{
			"use the value inside the parallel block",
			"or return it from the branch and merge the results",
		},
	},
	CodeRetryNoIdem: {
		Text: "a tool call inside `retry` has no idempotency proof",
		Why:  "A retry re-runs its whole body on a RunError; a call_tool without an idempotency proof repeats the same POST — duplicate charges, duplicate side effects.",
		Fix: []string{
			"move the call outside the retry subtree",
			"or give the tool a real proof (GET/HEAD, idempotent = true, or a pure idem_key)",
		},
	},
	CodeIdemKeyDerived: {
		Text: "idempotency key must be derived from the input",
		Why:  "An idem key that is constant or random is not idempotent: a constant key collapses distinct requests, a random one changes every retry and defeats dedup.",
		Fix: []string{
			"derive the key from request fields (e.g. the order id)",
			"or omit idem_key if the tool is idempotent another way",
		},
	},
	CodeParallelWrite: {
		Text: "cannot write to an outer variable inside `parallel`",
		Why:  "Parallel branches run concurrently; writing a variable declared outside the block is a data race whose result depends on scheduling, so it is rejected.",
		Fix: []string{
			"return a value from each branch and merge after parallel",
			"or use a variable declared inside the branch",
		},
	},
	CodeLinkSemicolon: {
		Text: "#link does not need a trailing semicolon (like #include)",
		Why:  "A directive is not a statement: #include and #link both end at the line end, so a trailing ';' is stray.",
		Fix: []string{
			"remove the trailing ';'",
			"leave the directive as #link <std/net>",
		},
	},
	CodeMaxStepsUnlimited: {
		Text: "max_steps = unlimited: this agent has no statically known upper bound on steps",
		Why:  "With unlimited steps, max_spans is no longer a provable trace bound and budgets cannot be pre-reserved; only the runtime step cap remains.",
		Fix: []string{
			"set a concrete max_steps if you need provable bounds",
			"accept it only when the runtime cap is the intended guard",
		},
	},
	CodeHTTPStatusCmp: {
		Text: "a struct field named like a status code is compared with an HTTP status, but the default tool.call contract never binds it for a non-2xx response",
		Why: "Without allow_status, tool.call returns T only on success and throws HttpError otherwise, so on a 404 the flow leaves at the tool.call line and the field is never bound — the comparison can never be true. " +
			"The HTTP status only reaches the call site through allow_status (HttpResult<T>.status). This is a heuristic warning, not an error: a business field that happens to be named status and really equals 404 is legal, and the compiler cannot tell the two apart (LANGUAGE.md 12.1 L3).",
		Fix: []string{
			"list the code in the call's allow_status and compare the result's status: `tool.call<T>(..., allow_status = [404])` then `r.status == 404`",
			"or read the outcome from the response body instead: a 200 response carries T, so check a business field on T (e.g. `found = false`) rather than the HTTP status",
		},
	},
	CodeInputJsonNoContract: {
		Text: "entry has no structural contract: input is declared as json, so hermes serve cannot generate documentation from it nor reject malformed requests at the entry point",
		Why: "The input type is the HTTP request contract. When declared as json the request body is arbitrary JSON, so serve can neither generate an API schema nor validate the fields at the entry point. This is a sanctioned escape hatch (LANGUAGE.md §4.8), but it loses compile-time contract protection.",
		Fix: []string{
			"declare a struct as the contract: `input: SupportInput`, where the fields are the shape of the request body and non-`T?` fields become required",
			"if arbitrary JSON is truly needed, keep json but accept this warning, or upgrade it to an error with --deny-warnings",
		},
	},
	CodeIRCodegen: {
		Text: "code generation failed: the workflow could not be lowered to IR",
		Why:  "Codegen rejects shapes it cannot lower faithfully (for example an if/return decision tree that does not match its expected form); emitting a wrong IR would be worse than failing.",
		Fix: []string{
			"fix the construct the message names",
			"or report it — an internal codegen error is a compiler bug, not your input",
		},
	},
}

var catalogFrontendZH = map[string]msg{
	CodeSyntax: {
		Text: "语法错误：解析器在这里无法匹配任何声明或语句",
		Why:  "语法在此处要求特定记号；缺分隔符、括号不配对或拼错关键字都会打断匹配，在修好前解析无法继续。",
		Fix: []string{
			"对照 LANGUAGE.md §3 检查报错位置前后的标点与关键字",
			"若消息点了记号种类，留意它前面是否有拼写错误",
		},
	},
	CodeFileTooLarge: {
		Text: "源文件超过 8 MiB 上限",
		Why:  "单个文件被限制在 8 MiB，好让编译器在恶意或失控输入下内存有界；include 展开另行计数。",
		Fix: []string{
			"把文件拆成更小的模块",
			"精简内容，或把一部分挪到 #link 后面",
		},
	},
	CodeTokenLimit: {
		Text: "token 数超过 2,000,000 上限",
		Why:  "include 展开后的总 token 数被限制，好让解析时间与内存有界、构建耗时能从源码树本身回答。",
		Fix: []string{
			"拆分文件或减少 include",
			"把不常用的声明挪到 #link 后面，避免被重新解析",
		},
	},
	CodeFloatLiteral: {
		Text: "double 字面量必须写成 D.D（'.5' 非法）",
		Why:  "前导点会与字段访问的 '.' 混淆；要求写成 D.D 才能让词法无歧义。",
		Fix: []string{
			"写成 0.5 而不是 .5",
			"小数用 `double`；金额仍留在 `cost` / int 微单位",
		},
	},
	CodeCostPrecision: {
		Text: "金额超过 6 位小数（微美元上限）",
		Why:  "cost 是微美元，最多保留 6 位小数；更细的精度无法表示，只会被静默舍入。",
		Fix: []string{
			"把金额四舍五入到 6 位小数",
			"或者直接用微美元整数表示",
		},
	},
	CodeUnterminatedString: {
		Text: "字符串未闭合（必须单行）",
		Why:  "未闭合或跨行的字符串会把后面的代码吞进去，一个笔误变成一串不相关的错误。",
		Fix: []string{
			"在同一行内闭合字符串",
			"用 \\n 表示换行，或把内容拆成多段",
		},
	},
	CodeStringEscape: {
		Text: "非法的字符串转义或插值",
		Why:  "转义与插值必须符合语法；写坏了一个就解码不出来，跨行的字符串还会吞掉后面的代码。",
		Fix: []string{
			"合法转义：\\\\ \\\" \\n \\r \\t，以及带 4 位十六进制的 \\u 转义",
			"插值必须是纯路径，如 input.text",
		},
	},
	CodeGenerics: {
		Text: "类型实参列表后缺少 '('",
		Why:  "写成 name<T> 的泛型调用必须紧跟调用括号；省掉会让 <T> 被当成小于比较。",
		Fix: []string{
			"写成 name<T>(...)",
			"或者去掉 <T>，让类型被推断",
		},
	},
	CodeWorkflowName: {
		Text: "workflow 名是标识符，不是带引号的字符串",
		Why:  "workflow 名是标识符，才能按符号被链接与导出；字符串不能充当符号名。",
		Fix: []string{
			"去掉引号：workflow Name",
			"如果你要的是字符串值，把它放进函数体",
		},
	},
	CodeJsonFieldAccess: {
		Text: "json 字段访问后缺少 ']'",
		Why:  "json 字段访问是方括号语法；缺 ']' 会让访问没闭合、后面的表达式被误解析。",
		Fix: []string{
			"闭合访问：obj[\"key\"]",
			"检查键字符串与它的右括号",
		},
	},
	CodeRetryUnlimited: {
		Text: "retry 次数与 delay 必须是整数字面量",
		Why:  "retry 上界会写进 IR 的代价上界；非字面量的次数或延迟无法静态定界，剥离与 trace 限额都会算错。",
		Fix: []string{
			"写成整数字面量：retry 3 times delay 100",
			"retry 上界是编译期的，这里不能用运行期的值",
		},
	},
	CodeToolCallMethodLit: {
		Text: "tool.call 的 method 必须是字符串字面量",
		Why:  "HTTP 方法决定幂等性（GET/HEAD 可安全重试）；它必须在编译期可读，所以拒绝计算出来的方法。",
		Fix: []string{
			"传字面量：tool.call(\"GET\", ...)",
			"方法若会变，拆成几个独立调用点",
		},
	},
	CodeToolCallMethodValid: {
		Text: "tool.call 的 method 不是合法的 HTTP 方法",
		Why:  "只识别标准方法；未知方法无法归入幂等类别，重试安全性就无法证明。",
		Fix: []string{
			"用 GET、HEAD、POST、PUT、PATCH 或 DELETE",
			"检查是否拼错或大小写不对",
		},
	},
	CodeAuthClause: {
		Text: "auth 子句必须是 auth env(\"NAME\")：密钥只能来自环境",
		Why:  "密钥来源必须可静态审计；允许表达式或默认值会让后端在未配置时启动，只在运行期以 401 失败。",
		Fix: []string{
			"写成 auth env(\"NAME\")，变量名用字面量",
			"去掉任何默认值——密钥没有安全的默认",
		},
	},
	CodeHeaderWorkflowBody: {
		Text: ".h 头文件里的 workflow 不能带函数体",
		Why:  "头文件只声明接口；头里写函数体会被复制进每个包含者，于是跑的是哪份实现取决于 include 图而非 #link。",
		Fix: []string{
			"把函数体挪到对应的 .hermes 实现里",
			"头文件只保留签名",
		},
	},
	CodeToolDecl: {
		Text: "tool 声明字段必须是 name = \"value\"; 的形式",
		Why:  "工具元信息（端点、方法、幂等性）是一组扁平字符串字段；字段写坏就无法把工具描述给运行时。",
		Fix: []string{
			"每个字段写成 name = \"value\";",
			"确认值是用引号括起来的字符串字面量",
		},
	},
	CodeMissingLink: {
		Text: "tool.call 需要 #link <std/net>；model.generate 需要 #link <std/model>",
		Why:  "内置能力不是默认存在的：网络与模型原语只有在本单元链接了对应 stdlib 模块时才可用，未链接的调用没有实现。",
		Fix: []string{
			"tool.call 加 #link <std/net>;",
			"model.generate 加 #link <std/model>;",
		},
	},
	CodeLinkInHeader: {
		Text: "#link 只允许出现在 .hermes 文件里",
		Why:  "#link 选择实现；头文件描述接口且会被多次包含，放在头里会把同一实现拉进每个包含者。",
		Fix: []string{
			"把 #link 挪进 .hermes 实现",
			"头文件里只保留 #include",
		},
	},
	CodeEnvConst: {
		Text: "env() 的参数必须是常量字符串：插值不是常量",
		Why:  "后端端点或凭据名在构建期读取；插值出来的值要到运行期才知道，无法写进配置。",
		Fix: []string{
			"给 env() 传纯字符串字面量",
			"把插值挪到运行期代码里",
		},
	},
	CodeBackendCreds: {
		Text: "后端地址不能内嵌凭据（user:pass@host）",
		Why:  "URL 里的凭据会进日志、trace 和 IR；必须从 env() 来，才不会写进产物或泄漏。",
		Fix: []string{
			"从地址里去掉 user:pass@",
			"用 auth env(\"NAME\") 提供密钥",
		},
	},
	CodeHeaderBackend: {
		Text: ".h 头文件里不能包含 backend 定义",
		Why:  "backend 定义含地址与密钥来源，属于实现而非接口；写进头文件会让每个包含者各得一份。",
		Fix: []string{
			"把 backend 定义挪到 .hermes 实现",
			"必要时头里只留声明",
		},
	},
	CodeInputNoType: {
		Text: "input 参数必须带类型",
		Why:  "input 的类型就是请求契约；没有它，解码器没有 schema 去校验进来的 body。",
		Fix: []string{
			"标注类型：input: Req",
			"给该 struct 配上对应的 schema",
		},
	},
	CodeOneWorkflow: {
		Text: "一个编译单元只能包含一个 workflow（其余会被丢弃）",
		Why:  "汇编阶段围绕单个 workflow 构建 IR；多出的那个永远进不了产物，看起来却完全正常——数据被静默丢弃。",
		Fix: []string{
			"把每个 workflow 拆到各自的 .hermes 文件",
			"分别构建",
		},
	},
	CodeUnreachableCatch: {
		Text: "这个 catch 不可达：前面的 catch (Error e) 已经能捕获一切",
		Why:  "catch 分支按顺序匹配；catch (Error) 放在更窄的分支之前会吞掉所有错误，后面的分支永远跑不到。",
		Fix: []string{
			"把宽的 catch (Error) 挪到最后",
			"或者删掉这个不可达的分支",
		},
	},
	CodeCostIntArith: {
		Text: "cost 不能与裸 int 相加或相减",
		Why:  "cost 是微美元；拿裸 int 去加等于默认那个 int 也是微美元，一个单位错误会把金额悄悄差 10^6 倍。",
		Fix: []string{
			"把 int 显式转成 cost",
			"或者全程用同一种单位",
		},
	},
	CodeNoAutoPromote: {
		Text: "int 与 double 不会自动提升",
		Why:  "没有隐式数值提升；int 到 double 的自动转换是静默精度损失与舍入 bug 的经典来源。",
		Fix: []string{
			"显式转换成你要的类型",
			"或者让两个操作数同类型",
		},
	},
	CodeStringArith: {
		Text: "算术运算符用在了非数值操作数上（string、bool、json）",
		Why:  "字符串没有算术，也没有运算符重载；拼接被刻意禁止因为它会掩盖编码与区域设置，一元负号只对数字有定义。",
		Fix: []string{
			"用字符串插值（把值写进字符串里）而不是运算符",
			"算术前把 json 收窄成数值类型（json.as_int）",
		},
	},
	CodeCostFloatMix: {
		Text: "cost 与 double 不能在算术里混用",
		Why:  "cost 是整数微美元；与 double 混算会重新引入 cost 本想避免的舍入误差。",
		Fix: []string{
			"金额运算留在 cost（整数）里",
			"或显式转换并明确舍入",
		},
	},
	CodeDivInt: {
		Text: "运算符 % 需要 int 操作数",
		Why:  "取余只对整数有意义；对 cost、double、string、json 或 struct 取余没有良定义。",
		Fix: []string{
			"% 用 int 操作数",
			"对 cost 先换算成微美元整数",
		},
	},
	CodeCompareTypes: {
		Text: "不能用 == 或 != 比较两个不同类型的值",
		Why:  "相等要求两侧同类型；跨类型比较没有唯一含义，只能靠隐藏的强制转换。",
		Fix: []string{
			"比较同类型的值",
			"显式把一边转换成相同类型",
		},
	},
	CodeOrderCompare: {
		Text: "这些操作数类型不能做大小比较",
		Why:  "大小比较（< <= > >=）只对 int、double、cost、string 有定义；bool、json、struct 没有全序。",
		Fix: []string{
			"只比较 int、double、cost 或 string",
			"对 json，先取出具体字段再比",
		},
	},
	CodeLogicalOp: {
		Text: "逻辑 &&、||、! 需要 bool 操作数",
		Why:  "语言没有真值性；&&、||、! 是布尔联结词，int/string/json 操作数需要并不存在的非零即真隐式规则。",
		Fix: []string{
			"显式比较：x != 0，或 s != 空字符串字面量",
			"或者把操作数改成 bool",
		},
	},
	CodeDupStruct: {
		Text: "同一文件里重复声明了同名 struct",
		Why:  "每个 struct 名对应一种布局；同文件里第二次声明会让类型二义，字段顺序取决于哪份胜出。",
		Fix: []string{
			"给其中一个改名",
			"若是残留，删掉重复的那份",
		},
	},
	CodeFieldAccess: {
		Text: "该类型没有这个字段",
		Why:  "字段访问按固定偏移读槽位；类型没声明的字段会读到从未布局的字节，因此无法编译。",
		Fix: []string{
			"使用该类型已声明的字段",
			"或者把该字段加进 struct 声明",
		},
	},
	CodeMatchNoDefault: {
		Text: "`match` 缺少 `default` 分支",
		Why:  "字符串主题无法证明穷尽，缺 default 的 match 遇到不匹配的值时结果未定义。",
		Fix: []string{
			"加一个 default: 分支",
			"或者收窄主题使各分支可证明穷尽",
		},
	},
	CodeMatchCaseType: {
		Text: "`case` 标签与 `match` 主题的类型不一致",
		Why:  "case 标签要跟主题比较；字符串主题配 int 标签（或反之）永远匹配不上，说明类型搞混了。",
		Fix: []string{
			"让标签与主题同类型",
			"或者在 match 前先转换主题",
		},
	},
	CodeDupCase: {
		Text: "同一个 case 标签出现了多次",
		Why:  "重复标签让第二个分支成为死代码，也掩盖了作者本想写另一个值的事实。",
		Fix: []string{
			"删掉重复的分支",
			"或者改成你真正想要的值",
		},
	},
	CodeReturnTypeMismatch: {
		Text: "返回类型不匹配：返回的值与声明的返回类型不符",
		Why:  "声明的返回类型是 workflow 的契约——调用方、链接器和 IR schema 都依赖它；返回别的类型会同时破坏三者。",
		Fix: []string{
			"返回声明类型的值",
			"或者把声明改成你实际返回的类型",
		},
	},
	CodeReturnTuple: {
		Text: "不能返回元组",
		Why:  "workflow 的返回类型必须是有 schema 的单一具名类型；元组没有名字，无法导出或描述给调用方。",
		Fix: []string{
			"把值包进 struct 再返回",
			"或者只返回一个值，其余通过 input 传",
		},
	},
	CodeBindErrorObj: {
		Text: "不能把 error 对象绑定到变量",
		Why:  "catch 的 error 对象只活在它自己的 catch 块里且是借用的，不归你所有；绑出去会让它比所指向的错误活得更久。",
		Fix: []string{
			"在 catch 块内联使用 error 对象",
			"若之后要用它的数据，显式把字段拷出来",
		},
	},
	CodeUnlimitedValue: {
		Text: "`unlimited` 不是值：只能出现在 max_steps = unlimited 的位置",
		Why:  "`unlimited` 意为没有编译期上界，是上界标记而非常量；放进值位置会造出没有值的变量并破坏所有上界。",
		Fix: []string{
			"只在 model.agent(max_steps = unlimited) 里用 unlimited",
			"其他位置赋具体值",
		},
	},
	CodeFallbackReturn: {
		Text: "fallback 的返回类型与主表达式不一致",
		Why:  "fallback 是主表达式抛错时采用的值；类型不同会让调用方在运行期拿到两种不兼容类型之一。",
		Fix: []string{
			"让 fallback 返回与主表达式相同的类型",
			"或者把 fallback 的值转换成该类型",
		},
	},
	CodeConstReassignFallback: {
		Text: "不能在 fallback 块里重新赋值 const 变量",
		Why:  "const 在整个作用域内不可变；fallback 不是例外——在里面改会让 const 在不同块里含义不同。",
		Fix: []string{
			"需要可变就用非 const 变量",
			"或者把赋值移出 fallback",
		},
	},
	CodeConditionNotBool: {
		Text: "if/while 的条件必须是 bool",
		Why:  "条件是控制流分支点，语言没有真值性；int/string/json/cost 没有为真的概念，隐式规则在运行期是未定义的。",
		Fix: []string{
			"显式比较：x != 0，或 s != 空字符串字面量",
			"或者让表达式产出 bool",
		},
	},
	CodeLoopVarEnded: {
		Text: "在 `for` 循环结束后使用了循环变量",
		Why:  "循环变量归 for 头所有，只在循环体内存在；循环结束后槽位被回收，在外面引用就是读一个已经不存在的变量。",
		Fix: []string{
			"把使用挪进循环体",
			"或者在循环前声明一个变量把值存下来",
		},
	},
	CodeFloatNoIR: {
		Text: "旧 float 标签在 IR 里没有表示",
		Why: "这是旧标签的纵深防线，不是「所有浮点字面量被拒」。v2.0.0 起唯一浮点类型是 `double`（IEEE-754 binary64），字面量编成位模式 {{\"k\":\"double\",\"bits\":\"<16 位小写 hex>\"}}，IR 字节里没有 JSON 浮点数字，V-7 / canonical / 签名三件事都不受影响。" +
			"带 \"float\" 标签的 LitExpr 是 v2.0.0 之前的形态：它会落进字符串兜底，产出 {{\"t\":\"string\",\"v\":\"float\"}} —— 合法 IR，运行期却静默返回字符串 \"float\"。三道扫描器保留下来，防的是忽略诊断直接调 C ABI 的宿主。",
		Fix: []string{
			"浮点用 `double`（字面量 1.5 / 1.5e2 / 1e3 都支持）",
			"金额仍留在 `cost` / int 微单位；`json.as_float` 这个 API 名保留原名",
		},
	},
	CodeCallArgType: {
		Text: "调用实参类型不匹配",
		Why:  "实参类型与被调方声明的形参类型不同；没有隐式转换，调用无法按声明签名编译。",
		Fix: []string{
			"传声明形参类型的值",
			"或者显式转换实参",
		},
	},
	CodeIndexNotInt: {
		Text: "数组下标必须是整数",
		Why:  "数组下标是整数偏移；非 int 下标没有确定位置，需要隐藏的截断。",
		Fix: []string{
			"用 int 下标",
			"或者把下标显式转成 int",
		},
	},
	CodeIndexNonArray: {
		Text: "不能对非数组的值取下标",
		Why:  "只有数组支持按下标访问；对 int/string/json/struct 取下标没有元素可取。",
		Fix: []string{
			"对数组值取下标",
			"对 json，先取出类型化数组（json.as_array）",
		},
	},
	CodeFloatEq: {
		Text: "不能用 == 或 != 比较两个 double",
		Why: "二进制浮点的精确相等不是一个稳定谓词，编译期证不出来，所以直接拒，而不是让它静默返回 false。" +
			"舍入会让两个数学上相等的值比出不相等，NaN 与自己也不相等：`x == x` 可能为假，而 `x != x` 悄悄成了语言里唯一的 NaN 判据。",
		Fix: []string{
			"把值保持在精确单位上（int 微单位或 `cost`）再比",
			"或者用显式容差比较：可以写 `double` 字面量，也可以取自输入（如 `json.as_float(input.eps)`）；相等比较本身仍然被拒",
		},
	},
	CodeErrFieldNotYet: {
		Text: "该错误字段规范里有，但运行期还没有带上",
		Why: "LANGUAGE.md 7.1.1 声明了这个字段，所以它不是拼写错误——但执行器的错误值目前还没有填它。" +
			"放行的话会编得过、运行期死在 `field does not exist` 上，而那是普通 error，catch 接不住：静态检查通过而程序失败，比直接拒绝更坏。",
		Fix: []string{
			"改读运行期已经带上的字段（E4016 的 help 按错误类型列出了它们）",
			"或者把结果当正常值读：`allow_status = [404]` 时状态码从 HttpResult<T> 回来，`r.status == 404` 根本不需要错误对象",
		},
	},
	CodeParallelScoped: {
		Text: "在 `parallel` 块内声明的值在块外不可见",
		Why:  "parallel 的每个分支并发执行；块内产生的值在块外没有唯一确定的值，因此作用域限于块内。",
		Fix: []string{
			"在 parallel 块内使用该值",
			"或者从分支返回再合并结果",
		},
	},
	CodeRetryNoIdem: {
		Text: "`retry` 内的工具调用没有幂等证明",
		Why:  "retry 在遇到 RunError 时重跑整个 body；未证明幂等的 call_tool 会把同一个 POST 再发一遍——重复扣款、重复副作用。",
		Fix: []string{
			"把该调用移出 retry 子树",
			"或者给工具真证明（GET/HEAD、idempotent = true，或纯 idem_key）",
		},
	},
	CodeIdemKeyDerived: {
		Text: "幂等键必须由输入派生",
		Why:  "常量或随机的 idem 键并不幂等：常量会把不同请求塌缩成一个，随机键每次重试都变，等于没去重。",
		Fix: []string{
			"用请求字段派生键（如订单号）",
			"或者工具本就幂等，索性省掉 idem_key",
		},
	},
	CodeParallelWrite: {
		Text: "不能在 `parallel` 内写外部变量",
		Why:  "parallel 分支并发执行；写块外声明的变量是数据竞争，结果取决于调度，因此被拒。",
		Fix: []string{
			"每个分支返回值，parallel 之后再合并",
			"或者用分支内声明的变量",
		},
	},
	CodeLinkSemicolon: {
		Text: "#link 不需要结尾分号（和 #include 一样）",
		Why:  "指令不是语句：#include 和 #link 都止于行末，结尾多出的 ';' 是多余的。",
		Fix: []string{
			"去掉结尾的 ';'",
			"指令保持 #link <std/net>",
		},
	},
	CodeMaxStepsUnlimited: {
		Text: "max_steps = unlimited：该 agent 的步数没有静态可证的上界",
		Why:  "步数无限时 max_spans 不再是可证的 trace 上界、预算无法预留，只剩运行期硬上限。",
		Fix: []string{
			"需要可证上界就设具体 max_steps",
			"只有当你本就以运行期上限为闸时才接受它",
		},
	},
	CodeHTTPStatusCmp: {
		Text: "拿形如状态码的 struct 字段去比 HTTP 状态码，但默认 tool.call 契约下非 2xx 根本不会绑定它",
		Why: "没有 allow_status 时，tool.call 成功才返回 T，否则抛 HttpError —— 404 时执行流在 tool.call 那行就跳走了，字段从未被绑定，这个比较永远不可能为真。" +
			"HTTP 状态只有通过 allow_status（HttpResult<T>.status）才到得了调用点。这是**启发式警告**不是错误：业务字段恰好叫 status 且真的等于 404 是合法的，编译器分不出这两种情况（LANGUAGE.md 12.1 L3）。",
		Fix: []string{
			"把该码写进调用的 allow_status，再比结果的状态：`tool.call<T>(..., allow_status = [404])` 然后 `r.status == 404`",
			"或者从响应体判断：200 响应才带 T，所以检查 T 上的业务字段（如 `found = false`），而不是 HTTP 状态码",
		},
	},
	CodeInputJsonNoContract: {
		Text: "入口没有结构契约：input 被声明为 json，hermes serve 无法据此生成文档，也无法在入口挡住畸形请求",
		Why:  "input 的类型就是 HTTP 请求契约。声明为 json 时请求体是任意 JSON，serve 既生成不出 API schema，也无法在入口校验字段。这是规范允许的逃逸口（LANGUAGE.md §4.8），但会失去编译期契约保护。",
		Fix: []string{
			"声明一个 struct 作为契约：input: SupportInput，其中字段即为请求体形状，非 T? 字段会被纳入 required",
			"若确实需要任意 JSON，保留 json 但接受此警告；也可使用 --deny-warnings 将其升级为错误",
		},
	},
	CodeIRCodegen: {
		Text: "代码生成失败：该 workflow 无法降到 IR",
		Why:  "codegen 会拒绝无法忠实下降的形状（如形状不符的 if/return 决策树）；产出错误的 IR 比失败更糟。",
		Fix: []string{
			"修正消息点名的那处构造",
			"或者上报——内部 codegen 错误是编译器 bug，不是你的输入",
		},
	},
}
