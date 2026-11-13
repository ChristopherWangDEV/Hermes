package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"hermes/internal/archive"
	"hermes/internal/budget"
	"hermes/internal/engine"
	"hermes/internal/ir"
	"hermes/internal/runtime"
)

// serve 子命令：把 unit 的导出 workflow 暴露成 HTTP 面（T28）。
//
// D-021 定死了四个端点必须与 Homonoia **同形**：
//
//	GET  /health               存活与能力
//	GET  /v1/models            只列 exported workflow，owned_by = "hermes"
//	POST /v1/chat/completions  OpenAI 面
//	POST /v1/responses         Responses 面
//
// 为什么不能少一个：客户端要能"无感切换"两台后端。少一个端点，切换就成了
// "改客户端配置"，而配置一改，两者就不再是可替换的。
func cmdServe(args []string) int {
	var irPath, addr, token string
	maxInflight := 0
	maxPerModel := 0
	tenantsPath := ""
	// 纯 opt-in：默认 none，与引入该参数之前的累计语义完全一致。
	budgetReset := budgetResetNone
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--addr":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, tr("hermes serve: --addr needs an address\n\n")+serveUsage)
				return 2
			}
			addr = args[i+1]
			i++
		case "--tenants":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, "hermes serve: --tenants needs a file\n\n"+serveUsage)
				return 2
			}
			tenantsPath = args[i+1]
			i++
		case "--max-inflight-per-model":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, "hermes serve: --max-inflight-per-model needs a number\n\n"+serveUsage)
				return 2
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n < 0 {
				fmt.Fprintf(os.Stderr, "hermes serve: --max-inflight-per-model must be a non-negative integer, got %q\n", args[i+1])
				return 2
			}
			maxPerModel = n
			i++
		case "--max-inflight":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, "hermes serve: --max-inflight needs a number\n\n"+serveUsage)
				return 2
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n < 0 {
				fmt.Fprintf(os.Stderr, "hermes serve: --max-inflight must be a non-negative integer, got %q\n", args[i+1])
				return 2
			}
			maxInflight = n
			i++
		case "--token":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, "hermes serve: --token needs a value\n\n"+serveUsage)
				return 2
			}
			token = args[i+1]
			i++
		case "--budget-reset":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, "hermes serve: --budget-reset needs one of none|daily|weekly|monthly\n\n"+serveUsage)
				return 2
			}
			v, ok := normalizeBudgetReset(args[i+1])
			if !ok {
				fmt.Fprintf(os.Stderr, "hermes serve: --budget-reset must be one of none|daily|weekly|monthly, got %q\n", args[i+1])
				return 2
			}
			budgetReset = v
			i++
		case "-h", "--help", "help":
			fmt.Print(serveUsage)
			return 0
		default:
			if irPath == "" && a != "" && a[0] != '-' {
				irPath = a
				continue
			}
			fmt.Fprintf(os.Stderr, "hermes serve: %s\n\n%s", fmt.Sprintf(tr("unknown argument %q"), a), serveUsage)
			return 2
		}
	}
	if irPath == "" {
		fmt.Fprint(os.Stderr, tr("hermes serve: missing IR / .hbc\n\n")+serveUsage)
		return 2
	}
	if addr == "" {
		addr = "127.0.0.1:8080"
	}

	data, err := os.ReadFile(irPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes serve: cannot read %s: %v\n"), irPath, err)
		return 4
	}
	if len(data) >= 4 && string(data[0:4]) == "HBC\x1a" {
		data, err = extractMemberIR(data)
		if err != nil {
			fmt.Fprintf(os.Stderr, tr("hermes serve: %v\n"), err)
			return 4
		}
	}
	doc, ds := ir.Load(data)
	if len(ds) != 0 {
		fmt.Fprintf(os.Stderr, tr("hermes serve: IR is invalid (%d diagnostics)\n"), len(ds))
		return 4
	}

	var rt engine.Runtime
	if len(doc.Backends) > 0 {
		byName := make(map[string]ir.Backend, len(doc.Backends))
		for _, b := range doc.Backends {
			byName[b.Name] = b
		}
		rt = &runtime.Runtime{Backends: byName}
	}

	if token == "" {
		token = os.Getenv("HERMES_SERVE_TOKEN")
	}
	var tenants *TenantSet
	if tenantsPath != "" {
		// 如果 unit 声明了 `export struct Tenant`，用它校验配置 ——
		// 租户因此是**一个类型**，而不是一堆没人管形状的键值对。
		var schemaJSON []byte
		for i := range doc.Schemas {
			if doc.Schemas[i].Root == "Tenant" || doc.Schemas[i].ID == "Tenant" {
				schemaJSON = doc.Schemas[i].JSON
				break
			}
		}
		ts, err := LoadTenantsWithSchema(tenantsPath, schemaJSON)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hermes serve: %v\n", err)
			return 4
		}
		tenants = ts
	}
	srv := &server{
		doc: doc, rt: rt, token: token, maxInflight: maxInflight, perModel: maxPerModel,
		tenants: tenants, budgetReset: budgetReset, now: time.Now,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", srv.health)
	mux.HandleFunc("/v1/models", srv.models)
	mux.HandleFunc("/v1/chat/completions", srv.chatCompletions)
	mux.HandleFunc("/v1/responses", srv.responses)

	fmt.Fprintf(os.Stderr, tr("hermes serve: %s (%d workflows, %d backends)\n"),
		addr, len(doc.Workflows), len(doc.Backends))
	if tenants != nil {
		fmt.Fprintf(os.Stderr, "hermes serve: %s\n", fmt.Sprintf(
			tr("tenant mode (%d tenants: %s)"),
			len(tenants.Names()), strings.Join(tenants.Names(), ", ")))
	}
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes serve: %v\n"), err)
		return 4
	}
	return 0
}

type server struct {
	doc *ir.Doc
	rt  engine.Runtime
	// token 非空时，除 /health 外都要求 `Authorization: Bearer <token>`。
	//
	// 为什么不是"默认没鉴权、可选开启"：没有鉴权的默认会被人直接部署到内网，
	// 而内网不等于可信。这里反过来 —— 有 token 才鉴权，但**启动时大声说明**
	// 当前是不是裸的，让人不可能在不知情的情况下暴露。
	token string
	// maxInflight 是同时在跑的请求数上限（0 表示不限）。
	//
	// 为什么必须有：每个请求都会花模型的钱，而 HTTP 面本身没有天然背压 ——
	// 一千个并发到达时，全部会进入模型调用，账单跟着并发数走。
	// 超限直接 429，让客户端自己退避，而不是让我们的上游过载。
	maxInflight int
	inflight    atomic.Int32
	// perModel 是**单个 workflow（模型）**的在跑上限（0 表示不限）。
	//
	// 为什么需要它：全局上限只能回答"总共别过载"，不能回答"一个热模型会不会
	// 把其他模型饿死"。一个慢 workflow 占满名额时，其他 workflow 的请求会
	// 全部 429 —— 而它们本来可以正常跑完。
	perModel int
	perMu    sync.Mutex
	perCount map[string]*atomic.Int32
	// tenants 非空时，token 决定**身份与配额**（D-039）。
	tenants *TenantSet
	// tenantCount 按 "租户/模型" 计并发。
	tenantMu    sync.Mutex
	tenantCount map[string]*atomic.Int32
	// spent 是每个租户**累计已花**的 micro-USD。
	//
	// 为什么按累计而不是"每次请求一个账本"：预算是**跨请求**的事实。
	// 单请求账本只能回答"这一次花了多少"，回答不了"这个月还剩多少"。
	//
	// 窗口是**可选**的：全局默认值与租户级字段都是 none（或空）时，这里退化成
	// 纯粹的累计值 —— 不读时钟、不写 window、不产生任何额外输出，与引入本特性
	// 之前逐字节一致。只有显式 opt-in 某个周期，才会在读/写时惰性判定窗口。
	spentMu sync.Mutex
	spent   map[string]*windowedSpent
	// budgetReset 是 `--budget-reset` 给的**全局默认值**（默认 none）。
	// 租户自己声明了 budget_reset 时以租户为准：租户级 > CLI > none。
	budgetReset string
	// now 是可注入的时钟：生产用 time.Now，测试用固定时刻，不必真的等到跨月。
	// 约定在开服前设好、之后不再修改，因此读取不需要额外的锁。
	now func() time.Time
}

// windowedSpent 是一个租户在某个重置窗口内的累计花费。
type windowedSpent struct {
	window string
	used   int64
}

// clock 返回当前 UTC 时刻。now 为 nil 时回落到真实时钟，让直接构造 server
// 的调用方（含既有测试）保持默认行为。
func (s *server) clock() time.Time {
	if s.now == nil {
		return time.Now().UTC()
	}
	return s.now().UTC()
}

// budgetWindowKey 计算 now 所属的 **UTC** 日历窗口。
//
// 用 UTC 而不是本地时区：同一份部署换个 TZ 环境变量，不该让"这个月的预算"
// 悄悄换一天开始。period 为 none 或未知值时返回空串，表示不计算窗口。
func budgetWindowKey(period string, now time.Time) string {
	u := now.UTC()
	switch period {
	case budgetResetDaily:
		return u.Format("2006-01-02")
	case budgetResetWeekly:
		y, w := u.ISOWeek()
		return fmt.Sprintf("%04d-W%02d", y, w)
	case budgetResetMonthly:
		return u.Format("2006-01")
	default:
		return ""
	}
}

// resetPeriodFor 返回该租户实际生效的重置周期。
// 优先级：租户级字段 > CLI 全局默认值 > none；空串视为"未声明"而非显式 none。
func (s *server) resetPeriodFor(t *Tenant) string {
	v := s.budgetReset
	if t != nil && t.BudgetReset != "" {
		v = t.BudgetReset
	}
	period, ok := normalizeBudgetReset(v)
	if !ok {
		// 正常路径到不了这里（CLI 与配置加载都已校验）。保守地退化成不重置：
		// 绝不因为一个拼错的周期就悄悄把别人的账清零。
		return budgetResetNone
	}
	return period
}

// rollWindowLocked 在窗口变化时把账本归零。调用方**必须**持有 s.spentMu。
// 读（spentBy）与写（settleBudget）都可能触发重置，两次判定必须原子，
// 否则一个刚跨窗口的读会看见旧值、紧接着的写又会把它写回去。
func (s *server) rollWindowLocked(st *windowedSpent, period string) {
	if period == budgetResetNone {
		return
	}
	w := budgetWindowKey(period, s.clock())
	if w == "" {
		return
	}
	if st.window != w {
		st.window = w
		st.used = 0
	}
}

// runtimeFor 返回一个带租户预算的执行运行时。
//
// 每个请求一份：Runtime 结构体里有 Budget 指针，共用会让两个请求的预留
// 落进同一个账本，结算时互相污染。
func (s *server) runtimeFor(t *Tenant) (engine.Runtime, *budget.Ledger, error) {
	base, ok := s.rt.(*runtime.Runtime)
	if !ok || base == nil {
		return s.rt, nil, nil
	}
	clone := *base
	if t == nil || t.BudgetMicroUSD <= 0 {
		return &clone, nil, nil
	}
	remaining := t.BudgetMicroUSD - s.spentBy(t)
	if remaining <= 0 {
		return nil, nil, &budgetExhausted{Tenant: t.Name}
	}
	l := budget.New(budget.Limits3{Global: budget.Limits{MicroUSD: remaining}})
	clone.Budget = l
	return &clone, l, nil
}

// spentBy 返回租户当前窗口内的累计花费。
//
// 读侧也必须判定窗口：否则跨窗口后的第一个请求会看到旧值而继续回 402，
// 直到某次 settleBudget 顺手把窗口翻过去 —— "预算什么时候恢复"不能靠运气。
func (s *server) spentBy(t *Tenant) int64 {
	if t == nil {
		return 0
	}
	period := s.resetPeriodFor(t)
	s.spentMu.Lock()
	defer s.spentMu.Unlock()
	st, ok := s.spent[t.Name]
	if !ok {
		return 0
	}
	s.rollWindowLocked(st, period)
	return st.used
}

// settleBudget 把这一次的真实花费记到租户账上。
// 懒初始化保留在锁内（原逻辑在 s.spent == nil 时建表），窗口判定与累加
// 也都在同一临界区内，读写两侧因此看到同一个窗口。
func (s *server) settleBudget(t *Tenant, l *budget.Ledger) {
	if t == nil || l == nil {
		return
	}
	used := l.Spent().MicroUSD
	period := s.resetPeriodFor(t)
	s.spentMu.Lock()
	defer s.spentMu.Unlock()
	if s.spent == nil {
		s.spent = map[string]*windowedSpent{}
	}
	st, ok := s.spent[t.Name]
	if !ok {
		st = &windowedSpent{}
		s.spent[t.Name] = st
	}
	s.rollWindowLocked(st, period)
	st.used += used
}

// budgetExhausted 表示租户的预算已经用完。
type budgetExhausted struct{ Tenant string }

func (e *budgetExhausted) Error() string { return "budget exhausted: " + e.Tenant }

// acquireTenant 按租户与其单模型上限拿名额。返回 0 表示放行。
func (s *server) acquireTenant(w http.ResponseWriter, t *Tenant, model string) bool {
	if t == nil {
		return true
	}
	if t.MaxInflight > 0 && !s.bumpTenant(t.Name, "", t.MaxInflight) {
		w.Header().Set("Retry-After", "1")
		writeErr(w, 429, "too many in-flight requests for this tenant; retry later", "rate_limit_error")
		return false
	}
	if t.MaxPerModel > 0 && !s.bumpTenant(t.Name, model, t.MaxPerModel) {
		s.bumpTenantDown(t.Name, "")
		w.Header().Set("Retry-After", "1")
		writeErr(w, 429, "too many in-flight requests for this model in this tenant; retry later", "rate_limit_error")
		return false
	}
	return true
}

func (s *server) releaseTenant(t *Tenant, model string) {
	if t == nil {
		return
	}
	if t.MaxPerModel > 0 {
		s.bumpTenantDown(t.Name, model)
	}
	if t.MaxInflight > 0 {
		s.bumpTenantDown(t.Name, "")
	}
}

// bumpTenant 在 key 上加一并检查上限；超限时回退并返回 false。
func (s *server) bumpTenant(tenant, model string, limit int) bool {
	key := tenant + "/" + model
	s.tenantMu.Lock()
	if s.tenantCount == nil {
		s.tenantCount = map[string]*atomic.Int32{}
	}
	c, ok := s.tenantCount[key]
	if !ok {
		c = &atomic.Int32{}
		s.tenantCount[key] = c
	}
	s.tenantMu.Unlock()
	if int(c.Add(1)) > limit {
		c.Add(-1)
		return false
	}
	return true
}

func (s *server) bumpTenantDown(tenant, model string) {
	key := tenant + "/" + model
	s.tenantMu.Lock()
	c := s.tenantCount[key]
	s.tenantMu.Unlock()
	if c != nil {
		c.Add(-1)
	}
}

// acquireModel 拿一个**按模型**的名额。满了返回 false（调用方回 429）。
func (s *server) acquireModel(w http.ResponseWriter, model string) bool {
	if s.perModel <= 0 {
		return true
	}
	s.perMu.Lock()
	if s.perCount == nil {
		s.perCount = map[string]*atomic.Int32{}
	}
	c, ok := s.perCount[model]
	if !ok {
		c = &atomic.Int32{}
		s.perCount[model] = c
	}
	s.perMu.Unlock()
	if int(c.Add(1)) > s.perModel {
		c.Add(-1)
		w.Header().Set("Retry-After", "1")
		writeErr(w, 429, "too many in-flight requests for this model; retry later", "rate_limit_error")
		return false
	}
	return true
}

func (s *server) releaseModel(model string) {
	if s.perModel <= 0 {
		return
	}
	s.perMu.Lock()
	c := s.perCount[model]
	s.perMu.Unlock()
	if c != nil {
		c.Add(-1)
	}
}

// acquire 拿一个并发名额。满了返回 false（调用方回 429）。
func (s *server) acquireInflight(w http.ResponseWriter) bool {
	if s.maxInflight <= 0 {
		return true
	}
	if int(s.inflight.Add(1)) > s.maxInflight {
		s.inflight.Add(-1)
		// 429 而不是排队：排队会让客户端以为请求已受理，而实际在等一个
		// 可能永远不会空出来的名额。带上 Retry-After，让退避有依据。
		w.Header().Set("Retry-After", "1")
		writeErr(w, 429, "too many in-flight requests; retry later", "rate_limit_error")
		return false
	}
	return true
}

func (s *server) releaseInflight() {
	if s.maxInflight > 0 {
		s.inflight.Add(-1)
	}
}

// authorize 检查鉴权。返回 true 表示放行。
func (s *server) authorize(w http.ResponseWriter, r *http.Request) (*Tenant, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	tok := ""
	if strings.HasPrefix(h, prefix) {
		tok = h[len(prefix):]
	}

	// 租户模式：身份由 token 决定，配额跟着租户走。
	if s.tenants != nil {
		if t := s.tenants.Lookup(tok); t != nil {
			return t, true
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="hermes"`)
		// 不区分"token 不存在"与"租户被停用"：区分等于告诉攻击者哪个 token 曾经有效。
		writeErr(w, 401, "invalid credentials", "authentication_error")
		return nil, false
	}

	if s.token == "" {
		return nil, true
	}
	if !subtleCompare(tok, s.token) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="hermes"`)
		writeErr(w, 401, "invalid or missing bearer token", "authentication_error")
		return nil, false
	}
	return nil, true
}

// subtleCompare 是**定长比较**：用 != 比较会让响应时间随匹配前缀长度变化，
// 而那是可测量的 —— 逐字节试 token 在局域网里完全可行。
func subtleCompare(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

func (s *server) exported() []string {
	out := make([]string, 0, len(s.doc.Workflows))
	for i := range s.doc.Workflows {
		if s.doc.Workflows[i].Exported {
			out = append(out, s.doc.Workflows[i].Name)
		}
	}
	sort.Strings(out)
	return out
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"status":    "ok",
		"workflows": len(s.exported()),
	})
}

func (s *server) models(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorize(w, r); !ok {
		return
	}
	names := s.exported()
	list := make([]any, 0, len(names))
	for _, n := range names {
		list = append(list, map[string]any{
			"id": n, "object": "model", "owned_by": "hermes",
		})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": list})
}

// chatRequest 只取我们用得到的字段。
//
// `input` 是**显式**的（非 OpenAI 标准）：workflow 的输入契约由 `.h` 里的
// struct 定义，直接映射到 messages 会变成猜。客户端要传什么，看 `hermes ir --json`
// 里的 schemas[]。
type chatRequest struct {
	Model    string         `json:"model"`
	Messages []chatMessage  `json:"messages"`
	Input    map[string]any `json:"input"`
	// Stream 请求 SSE 流式返回。注意：**engine 是整段返回的**，
	// 所以这是"按 SSE 协议分块发送"，不是逐 token 生成 —— 见 streamChat 的注释。
	Stream bool `json:"stream"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (s *server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	// 同上：这个端点花模型的钱，鉴权不能省。
	tenant, ok := s.authorize(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, 405, tr("method not allowed"), "invalid_request_error")
		return
	}
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, tr("request body is not valid JSON: ")+err.Error(), "invalid_request_error")
		return
	}

	name, err := s.pickWorkflow(req.Model)
	if err != nil {
		// 模型名不认识 → 404 + 明确列出可用的，而不是回一个空结果。
		writeErr(w, 404, err.Error(), "model_not_found")
		return
	}
	// 限流放在**确定要真跑**之后：404/400 不消耗名额，否则一堆打错模型的请求
	// 会把真正有效的请求挤掉。
	if !s.acquireInflight(w) {
		return
	}
	defer s.releaseInflight()
	if !s.acquireModel(w, name) {
		return
	}
	defer s.releaseModel(name)
	if !s.acquireTenant(w, tenant, name) {
		return
	}
	defer s.releaseTenant(tenant, name)
	input := req.Input
	if input == nil {
		// 没给 input 时，把最后一条 user 消息当作 text —— 这是最接近
		// "对话式调用"的映射，且**只在没有 input 时**发生，不覆盖显式传的。
		for i := len(req.Messages) - 1; i >= 0; i-- {
			if req.Messages[i].Role == "user" {
				input = map[string]any{"text": req.Messages[i].Content}
				break
			}
		}
	}

	// 每个请求一份带租户预算的运行时（见 runtimeFor 的注释）。
	reqRT, ledger, rerr := s.runtimeFor(tenant)
	if rerr != nil {
		writeErr(w, 402, "tenant budget exhausted; contact the operator", "budget_error")
		return
	}
	if ledger != nil {
		defer s.settleBudget(tenant, ledger)
	}
	opts := engine.RunOptions{Runtime: reqRT}
	if req.Stream {
		// **真流式**：模型每吐一块就转给客户端，而不是等整段算完再分块。
		// 头部与 SseWriter 在 run 之前就要就绪 —— 回调会在运行期间被调用。
		sw, err := beginSSE(w, name)
		if err != nil {
			writeErr(w, 500, err.Error(), "server_error")
			return
		}
		opts.OnModelChunk = sw.delta
		res, runErr := engine.RunWith(s.doc, name, input, opts)
		if runErr != nil {
			// 已经发过头了，只能在流里报错再收尾。
			sw.fail(runErr.Error())
			return
		}
		text, err := renderValue(res.Value)
		if err != nil {
			// 已经发过头了，只能在流里报错再收尾（与 runErr 分支同形）。
			sw.fail(err.Error())
			return
		}
		sw.finish(text)
		return
	}
	res, runErr := engine.RunWith(s.doc, name, input, opts)
	if runErr != nil {
		// 运行失败是 503（上游/预算/熔断），不是 400：请求本身没错。
		writeErr(w, 503, runErr.Error(), "server_error")
		return
	}

	content, err := renderValue(res.Value)
	if err != nil {
		writeErr(w, 500, err.Error(), "server_error")
		return
	}
	if req.Stream {
		s.streamChat(w, name, content)
		return
	}
	writeJSON(w, 200, map[string]any{
		"id":     "chatcmpl-hermes",
		"object": "chat.completion",
		"model":  name,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
	})
}

func (s *server) responses(w http.ResponseWriter, r *http.Request) {
	// 鉴权必须**每个端点都做**。之前只有 /v1/models 校验了 token，
	// 而真正花钱的 /v1/chat/completions 与 /v1/responses 是裸的 ——
	// 带 --token 启动时，聊天接口仍然对任何人开放。
	tenant, ok := s.authorize(w, r)
	if !ok {
		return
	}
	_ = tenant
	if r.Method != http.MethodPost {
		writeErr(w, 405, tr("method not allowed"), "invalid_request_error")
		return
	}
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, tr("request body is not valid JSON: ")+err.Error(), "invalid_request_error")
		return
	}
	name, err := s.pickWorkflow(req.Model)
	if err != nil {
		writeErr(w, 404, err.Error(), "model_not_found")
		return
	}
	if !s.acquireInflight(w) {
		return
	}
	defer s.releaseInflight()
	res, runErr := engine.RunWith(s.doc, name, req.Input, engine.RunOptions{Runtime: s.rt})
	if runErr != nil {
		writeErr(w, 503, runErr.Error(), "server_error")
		return
	}
	text, err := renderValue(res.Value)
	if err != nil {
		writeErr(w, 500, err.Error(), "server_error")
		return
	}
	writeJSON(w, 200, map[string]any{
		"id":     "resp-hermes",
		"object": "response",
		"model":  name,
		"output": []any{map[string]any{
			"type": "message",
			"role": "assistant",
			"content": []any{map[string]any{
				"type": "output_text", "text": text,
			}},
		}},
	})
}

// pickWorkflow 决定跑哪个：显式 model 名优先，否则用 is_entry。
//
// 「否则用 entry」不是便利，是契约：单 workflow 的 unit 调用方不该被迫知道名字。
func (s *server) pickWorkflow(model string) (string, error) {
	if model != "" {
		for _, n := range s.exported() {
			if n == model {
				return n, nil
			}
		}
		return "", fmt.Errorf(tr("no workflow named %q (available: %s)"), model, strings.Join(s.exported(), ", "))
	}
	for i := range s.doc.Workflows {
		if s.doc.Workflows[i].IsEntry {
			return s.doc.Workflows[i].Name, nil
		}
	}
	return "", fmt.Errorf(tr("this IR has no entry workflow; the model must be given explicitly"))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg, typ string) {
	writeJSON(w, code, map[string]any{
		"error": map[string]any{"message": msg, "type": typ},
	})
}

// renderValue 把 workflow 的返回值转成文本。字符串原样，其余走 JSON。
//
// NaN / ±Inf 只在**序列化边界**拒（与 `hermes run` 的 cmd_run.go 同一口径）：
// 这里必须复用同包的 rejectNonFinite，而不是在 json.Marshal 失败时
// `return fmt.Sprint(v)` —— 那会把 NaN 静默变成字符串 "NaN"，等于伪造成功。
// 也不在这里新增第二道非有限值判定：只在序列化边界统一拒，结构守卫
// tests/guard/nonfinite_boundary_test.go 只允许判定点出现在 jsonboundary.go。
func renderValue(v any) (string, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	if err := rejectNonFinite(v); err != nil {
		return "", err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("cannot serialise result: %w", err)
	}
	return string(b), nil
}

var serveUsage = pickUsage(`hermes serve - expose workflows over HTTP (T28 / D-021)

Usage:
  hermes serve <unit.ir.json | unit.hbc> [--addr 127.0.0.1:8080]
               [--token <bearer>] [--max-inflight <n>] [--max-inflight-per-model <n>]
               [--budget-reset <none|daily|weekly|monthly>]

Endpoints (all four must exist; with one missing a client cannot switch backends silently):
  GET  /health
  GET  /v1/models
  POST /v1/chat/completions
  POST /v1/responses

Notes:
  The request field "input" is explicit (not OpenAI standard): the workflow input contract
  is the struct declared in .h, and mapping it onto messages would be guesswork. Without
  "input", the last user message becomes "text".

Notes:
  --max-inflight caps total concurrent requests; --max-inflight-per-model caps
  concurrent requests **for one workflow**, so a slow model cannot starve the others.

Notes:
  --budget-reset is **pure opt-in**: the default "none" (or omitting the flag) never
  resets anything. Cumulative spend keeps growing and an exhausted budget stays 402,
  byte-for-byte the old behaviour. daily / weekly / monthly lazily zero the counter
  at UTC calendar boundaries (window keys: 2006-01-02, 2026-W45, 2006-01). A
  tenant's own "budget_reset" field overrides this process-wide default. This is a
  configuration shape, not a tenancy feature: local, air-gapped and single-machine
  deployments can use it just as well.

Exit codes:
  0  clean exit
  2  usage error
  4  IR unreadable/invalid, port in use
`,
	`hermes serve —— 把 workflow 暴露成 HTTP 面（T28 / D-021）

用法:
  hermes serve <unit.ir.json | unit.hbc> [--addr 127.0.0.1:8080]
               [--token <bearer>] [--max-inflight <n>] [--max-inflight-per-model <n>]
               [--budget-reset <none|daily|weekly|monthly>]

端点（四个都必须存在，缺一个客户端就无法无感切换）:
  GET  /health
  GET  /v1/models
  POST /v1/chat/completions
  POST /v1/responses

请求体里的 input 是**显式**的（非 OpenAI 标准）：workflow 的输入契约由 .h 里的
struct 定义，把它映射到 messages 会变成猜。不给 input 时，最后一条 user 消息
会当作 text —— 只在没有 input 时发生。

说明:
  --budget-reset 是**纯 opt-in**：默认 "none"（不写这个参数也一样）永不重置。
  累计花费只增不减，预算用完依旧回 402，与旧版逐字节一致。daily / weekly /
  monthly 按 UTC 日历边界惰性归零（窗口 key：2006-01-02、2026-W45、2006-01）。
  租户 JSON 里自己的 "budget_reset" 优先于这个全局默认值。它只是配置形状，
  不绑定「租户」语义：本地部署、内网封闭环境、单机配置同样适用。

退出码:
  0  正常结束
  2  用法错误
  4  IR 读不到/不合格、端口占用
`)

// extractMemberIR 从 .hbc 里取第一个成员的 IR。
func extractMemberIR(data []byte) ([]byte, error) {
	a, err := archive.Read(data)
	if err != nil {
		return nil, err
	}
	if len(a.Members) == 0 {
		return nil, fmt.Errorf(tr("archive has no members"))
	}
	return a.Members[0].IR, nil
}

// streamChat 用 SSE 把结果发出去。
//
// **诚实说明**：engine 是整段返回的（没有 token 级回调），所以这里是把
// 已经算好的结果**按 SSE 协议分块发送**，首块给 role、次块给 content、
// 末块给 finish_reason，然后 [DONE]。
//
// 为什么仍然值得做：客户端按 SSE 解析，一次性给全文会让它们卡在等第一块上
// （有些实现有首字节超时）。协议对了，用户体验就不是"卡住"。
// **不假装是真流式**：真流式要在 provider 层接 chunk，那是另一件事。
func (s *server) streamChat(w http.ResponseWriter, model, content string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported by this server", "server_error")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)

	send := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}

	chunk := func(delta map[string]any, finish any) {
		send(map[string]any{
			"id": "chatcmpl-hermes", "object": "chat.completion.chunk", "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		})
	}
	chunk(map[string]any{"role": "assistant"}, nil)
	chunk(map[string]any{"content": content}, nil)
	chunk(map[string]any{}, "stop")
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// sseWriter 是流式响应的写入器。
//
// 为什么不在 run 之后一次性分块：那样客户端要等到**整段算完**才看到第一个字节，
// 而首字节超时是很多客户端会踩的坑。真流式把每个模型增量立刻转出去。
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	model   string
	started bool
}

func beginSSE(w http.ResponseWriter, model string) (*sseWriter, error) {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("this server cannot stream (no Flusher)")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)
	sw := &sseWriter{w: w, flusher: f, model: model}
	sw.emit(map[string]any{"role": "assistant"}, nil)
	return sw, nil
}

func (s *sseWriter) emit(delta map[string]any, finish any) {
	b, err := json.Marshal(map[string]any{
		"id": "chatcmpl-hermes", "object": "chat.completion.chunk", "model": s.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	})
	if err != nil {
		return
	}
	fmt.Fprintf(s.w, "data: %s\n\n", b)
	s.flusher.Flush()
}

// delta 是 OnModelChunk 的回调：模型吐一块就发一块。
func (s *sseWriter) delta(d string) error {
	s.started = true
	s.emit(map[string]any{"content": d}, nil)
	return nil
}

// finish 收尾。注意：如果已经有增量，**不要再发完整内容** —— 那会让客户端
// 把同一段话看两遍。没有增量时（上游不支持流式）才补发一次。
func (s *sseWriter) finish(full string) {
	if !s.started {
		s.emit(map[string]any{"content": full}, nil)
	}
	s.emit(map[string]any{}, "stop")
	fmt.Fprint(s.w, "data: [DONE]\n\n")
	s.flusher.Flush()
}

// fail 在流里报错：过已经发过头，HTTP 状态码改不了了。
func (s *sseWriter) fail(msg string) {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": "server_error"}})
	fmt.Fprintf(s.w, "data: %s\n\n", b)
	fmt.Fprint(s.w, "data: [DONE]\n\n")
	s.flusher.Flush()
}
