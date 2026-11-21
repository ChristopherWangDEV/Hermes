package diag

// 错误码常量（阶段 2c：只保留标识符，文案已全部迁到 C++ catalog）。
//
// 这些常量的值就是用户看到的码，**不随语言变化**；why/help/文案在
// native/cpp/diag_catalog.cpp。此处保留是因为 Go 侧的校验器、LSP、CLI
// 到处引用它们，且它们是"这个码存在"的编译期证据。

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
	CodeParamDefaultNotConst  = "E4035"        // workflow 参数默认值折不成字面量
	CodeParamNotSupported     = "E3026"        // 有体 workflow 的具名形参只有 input / backend
	CodeParallelScoped        = "E5002"        // parallel 内声明对外不可见
	CodeRetryNoIdem           = "E5007"        // retry 内无幂等证明
	CodeIdemKeyDerived        = "E5009"        // 幂等键必须来自输入
	CodeParallelWrite         = "E5010"        // parallel 内写外部变量
	CodeLinkSemicolon         = "W2003"        // #link 不需要分号
	CodeMaxStepsUnlimited     = "W2004"        // max_steps = unlimited 警告
	CodeHTTPStatusCmp         = "W5004"        // 拿业务字段比 HTTP 状态码（启发式）
	CodeInputJsonNoContract   = "W5005"        // input: json 入口无结构契约
	CodeIRCodegen             = "E_IR_CODEGEN" // codegen 失败

	// 编译器诊断：**工具链链接面**（internal/toolchain 的 Refusal）。
	//
	// 这些码的发射点是 Go 侧的链接计划构造器，不是 C++ 前端也不是 IR 加载器：
	// 它们判的是"这台机器的目标/链接形态/许可证/命名空间允不允许"，而前端
	// 看不见 target、IR 也还没产出来。文案在发射点里是**已定稿的英文整句**
	// （含 "hermes compile: " 前缀，逐字打印），这里的 Text 逐字照抄它，
	// 中文条目只是 explain 的译文 —— 与本文件里 C++ 前端那批码同一个规矩。
	CodeDynamicBareMetal   = "E3102" // 裸机目标上用 --link-dynamic
	CodeNetLibNamespace    = "E3103" // 带 Go runtime 的归档不在 g* 命名空间
	CodeCrossNetArchive    = "E3104" // 宿主构建的 Go 归档要进交叉编译产物
	CodeCppABIRuntime      = "E3105" // 直接链 C++ ABI 运行时
	CodeBareMetalLinkShape = "E3106" // 裸机的 --link-static 不是 .a
	CodeNetLibMissing      = "E3107" // 需要网络归档但一个都没解析到
	CodeForbiddenRuntime   = "E3108" // 许可证闭集在链接面上命中
	CodeHostOnlyNode       = "E3109" // 裸机上用了宿主 OS 专属节点
	// CodeLinkInputShape 是 --link-* 值不是本机库文件（system 库名 / 链接器选项 /
	// 不存在 / 不是普通文件）。与 E3105（C++ ABI）、E3108（许可闭集）分开：
	// 同一个码管两件事时，grep 到它无法判断用户遇到的是哪一种。
	CodeLinkInputShape = "E3111"

	// CodeGoRuntimeBareMetalLink 是拆号后的裸机 Go runtime 归档拒绝码。
	//
	// 为什么必须拆出来：E3101 在 C++ 前端（api.cpp:282）和这里都表示
	// "一个 unit 只能一个 workflow"。一个码管两件事，grep 到 E3101 时
	// 无法判断用户遇到的是哪一种，报错质量门禁也就无从断言。
	// 发射点仍在 internal/toolchain/link.go:16（常量名保留，只改码值）。
	CodeGoRuntimeBareMetalLink = "E3110"

	// 编译器诊断：C++ 前端 2.0.1 的**控制流与内建**（native/cpp/check_type.cpp）。
	// 与上面那批 C++ 码一样，文案在发射点里逐字存在，这里只做登记与译文。
	CodeVoidAsValue      = "E4101" // void 调用被当成值用
	CodeSnapshotRequired = "E4102" // memory.rollback/commit 要的不是 snapshot
	CodeVoidReturn       = "E4103" // return 了一个 void 调用
	CodeMissingReturnVal = "E4104" // 声明了非 void 返回类型却写裸 `return;`
	CodeBreakOutsideLoop = "E4105" // break 不在循环里
	CodeContinueOutside  = "E4106" // continue 不在循环里
	CodeStopOutsideWf    = "E4107" // stop 不在 workflow 体内
	CodeStopDataType     = "E4108" // stop 的 data 与声明的返回类型不符
	CodeSignalUndeclared = "E4109" // on/wait/emit 用了没声明的信号
	CodeSignalDupHandler = "E4110" // 同一作用域里同一个信号有两个 handler
	CodeSleepDuration    = "E4111" // sleep/until timeout 的实参类型不对

	// 契约编程（D-036）：require / ensure 子句的四条 Sema 规则。
	// 一条一因——把"非布尔"和"子句里调了有副作用的函数"合成一个码，grep 到之后
	// 就无法判断用户遇到的是哪一种。
	CodeContractNotBool    = "E4116" // 子句不是布尔判据
	CodeContractResultUse  = "E4117" // require 里引用了 result
	CodeContractImpure     = "E4118" // 子句调用了可能有副作用的函数
	CodeContractInBody     = "E4119" // 子句写在体内
	CodeContractOnFunction = "E4120" // 函数上的契约子句尚未支持（2.0.1）
	CodeEmitNoHandler    = "W4112" // 频道路由的 emit 没有任何 handler
	CodeUntilNoTimeout   = "W4113" // until 没有 timeout，可以永久阻塞

	// CodeAssignImmutable 是**装配期**的码：IR 里一个 `assign` 节点指向了
	// 由 `decl` 标成 const:true 的变量（IR.md §4.2）。它和 CodeConstReassignFallback
	// 不是一件事 —— 后者判的是源码 AST 里 fallback 块内的重赋值，前者判的是
	// 已经到 IR 的这一层，同样能拦住手改过的产物。
	CodeAssignImmutable = "E_IR_ASSIGN_IMMUTABLE"
	// #bind（语法糖：一行说清“谁的实现 / 按什么哈希验 / 函数签名长什么样”）。
	// 三条规则各有独立码：可见性与供给方是两件事（E4036），接口文件不决定
	// 实现（E3027），指令必须在任何函数体之前确定（E3028）。
	CodeBindInHeader   = "E3027"
	CodeBindNotTopLevel = "E3028"
	CodeBindNoNames    = "E4036"
)
