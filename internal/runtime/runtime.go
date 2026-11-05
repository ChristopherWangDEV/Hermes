// Package runtime 把 engine 的 Runtime 接口接到真实的外部世界：
// 模型调用走 internal/provider，工具调用走 HTTP。
//
// 为什么单独一层而不是让 engine 直接用 provider：
//   - engine 的测试要能注入 fake（T25 的"不该联网"）；
//   - 地址解析（env / literal）、密钥来源、错误分类都属于"运行期策略"，
//     不该混进执行器的控制流里。
package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"hermes/internal/budget"
	"hermes/internal/engine"
	"hermes/internal/errs"
	"hermes/internal/ir"
	"hermes/internal/provider"
	"hermes/internal/resilience"
)

// Runtime 是生产实现的 Runtime。
type Runtime struct {
	// StoreHandle 是可选的存储句柄；nil 时按 HERMES_STORE / --store 惰性打开。
	StoreHandle *Store
	// Backends 按名字索引（来自 IR 的 backends[]）。
	Backends map[string]ir.Backend
	// Client 是 provider 客户端；HTTP 字段为空时用默认值。
	Client *provider.Client
	// HTTPClient 用于工具调用。
	HTTPClient *http.Client
	// Getenv 允许测试注入环境；为空走 os.Getenv。
	Getenv func(string) string
	// ToolTimeout 是单次工具调用的硬上限。
	ToolTimeout time.Duration
	// Budget 是一次 run 的账本。为 nil 表示不设预算。
	Budget *budget.Ledger
	// Breaker 是 per-target 熔断器。为 nil 表示不熔断。
	Breaker *resilience.Breaker
	// Now 允许测试注入时钟（熔断的冷却依赖时间）。
	Now func() time.Time
	// AIMD 是自适应并发窗口（按 target 分别记）。为 nil 表示不调整。
	AIMD *resilience.AIMD
}

// Concurrency 实现 engine.RunOptions.Concurrency：把 IR 的 max_par 压到
// AIMD 当前窗口以内。
//
// 为什么只在**变小**时生效：AIMD 的窗口可以涨到 ceil，但那段上限由 IR 的
// max_par 决定 —— 语言里写的上界是**承诺**，运行时不该把它撑大。
func (r *Runtime) Concurrency(want int) int {
	if r.AIMD == nil {
		return want
	}
	w := r.AIMD.Window("")
	if w <= 0 || w > want {
		return want
	}
	return w
}

func (r *Runtime) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runtime) getenv(k string) string {
	if r.Getenv != nil {
		return r.Getenv(k)
	}
	return os.Getenv(k)
}

// resolveAddr 把 backend 的**来源**解析成真实地址（IR.md §3.3）。
//
// 密钥与地址都只存"来源"不存解析结果，所以解析只发生在这里、只发生一次。
// 缺变量且没有默认值时**明确报错**，不许退回一个"看起来能用"的默认值。
func (r *Runtime) resolveAddr(name string) (addr, key string, err error) {
	b, ok := r.Backends[name]
	if !ok {
		return "", "", fmt.Errorf("没有名为 %q 的 backend（IR 里 backends[] 为空或不含它）", name)
	}
	switch b.Addr.Src {
	case "literal":
		if b.Addr.Value == nil {
			return "", "", fmt.Errorf("backend %q 的 addr.src=literal 但缺 value", name)
		}
		addr = *b.Addr.Value
	case "env":
		if b.Addr.Env == "" {
			return "", "", fmt.Errorf("backend %q 的 addr.src=env 但缺 env 名", name)
		}
		v := r.getenv(b.Addr.Env)
		if v == "" {
			if b.Addr.Default != nil {
				v = *b.Addr.Default
			} else {
				return "", "", fmt.Errorf("环境变量 %s 未设置，且该 backend 没有默认地址", b.Addr.Env)
			}
		}
		addr = v
	default:
		return "", "", fmt.Errorf("backend %q 的 addr.src=%q 不认识", name, b.Addr.Src)
	}

	// 密钥：只有 auth 子句给来源；**没有默认值**（D-023 末节）。
	// 缺密钥时一个请求都不发：把"忘了配密钥"伪装成"上游拒绝我"，
	// 会让这两件事的修复动作混在一起。
	if b.Auth != nil {
		if b.Auth.Src != "env" || b.Auth.Env == "" {
			return "", "", fmt.Errorf("backend %q 的 auth 子句形状非法", name)
		}
		key = r.getenv(b.Auth.Env)
		if key == "" {
			return "", "", fmt.Errorf("环境变量 %s 未设置：backend %q 需要密钥，而密钥没有默认值", b.Auth.Env, name)
		}
	}
	return addr, key, nil
}

// Model 实现 engine.Runtime。
func (r *Runtime) Model(ctx context.Context, req engine.ModelRequest) (any, error) {
	name := req.Backend
	if name == "" {
		if len(r.Backends) != 1 {
			return nil, fmt.Errorf("workflow %q 没有默认 backend，而可用的 backend 有 %d 个——运行期不替你挑",
				req.Workflow, len(r.Backends))
		}
		for k := range r.Backends {
			name = k
		}
	}
	addr, key, err := r.resolveAddr(name)
	if err != nil {
		return nil, &engine.RunError{Kind: "Error", Msg: err.Error()}
	}

	c := r.Client
	if c == nil {
		c = &provider.Client{}
	}
	c.BaseURL = addr
	c.APIKey = key

	b := r.Backends[name]
	target := provider.Target{Provider: string(b.Kind), Model: b.Model, EndpointKind: "chat"}
	if b.Kind == "homonoia" {
		// Homonoia 的网关面是 OpenAI 兼容的（D-021 的四端点同形）。
		target.Provider = "openai"
	}
	if b.Kind == "custom" {
		// `custom` 的定义就是"**别的** OpenAI 兼容服务"：vLLM、LM Studio、Ollama 的
		// OpenAI 面、OpenRouter、LiteLLM、以及绝大多数国内厂商的 /v1 面。
		// IR 规格里它是合法 kind，而 provider 之前没有对应实现 —— 写出来会在运行期
		// 报"未知 provider"，而错误信息看起来像"你拼错了"，实际是**没实现**。
		//
		// 不另开一套协议：真需要非 OpenAI 协议时应该加新的 kind，而不是让 custom
		// 变成"什么都行"——那会把"我连的是什么"变成运行期才知道的事。
		target.Provider = "openai"
	}

	// 熔断：开闸期间**不发请求**。这是"第 N 次调用上游计数不增长"的实现方式，
	// 不是靠调用方自觉。
	if r.Breaker != nil && !r.Breaker.Allow(r.now()) {
		return nil, &engine.RunError{Kind: "CircuitOpenError",
			Msg: "熔断器已开闸（" + target.String() + "），本次调用不发送"}
	}

	// 预算：先按**上界**预留，再按实际用量结算。
	// 上界用 prompt 字节数（字节数 ≥ token 数），下界估计会让"上界"失效。
	var resv *budget.Reservation
	if r.Budget != nil {
		up := budget.Upper{In: int64(len(req.Prompt)), Out: int64(maxInt(req.MaxTokens, 0))}
		scope := budget.Scope{Workflow: req.Workflow, Step: "model"}
		rv, err := r.Budget.Reserve(scope, up)
		if err != nil {
			return nil, &engine.RunError{Kind: "Error", Msg: "预算拒绝: " + err.Error()}
		}
		resv = rv
	}

	// agent 循环走多轮：Messages 非空时用它（含历史工具结果），不用 Prompt。
	messages := req.Messages
	if len(messages) == 0 {
		messages = []provider.Message{{Role: "user", Content: req.Prompt}}
	}
	resp, err := c.Chat(ctx, target, provider.Request{
		OnChunk:        req.OnChunk,
		Model:          b.Model,
		Messages:       messages,
		MaxTokens:      req.MaxTokens,
		ResponseFormat: req.Schema,
		Tools:          req.Tools,
	})
	if err != nil {
		if resv != nil {
			_ = resv.Settle(budget.Upper{})
		}
		if r.Breaker != nil {
			r.Breaker.Report(r.now(), &errs.Error{
				Attr: errs.AttrTransport, Sub: errs.SubNone,
				Op: "provider.Chat", StepID: "model", Target: target.String(),
			})
		}
		if r.AIMD != nil {
			// 拥塞（而不是任意错误）才收缩窗口：把 4xx 也当拥塞会让窗口
			// 因为"密钥忘了配"而缩到 1，而那根本不是容量问题。
			if re, ok := err.(*engine.RunError); ok && re.Kind == "NetworkError" {
				r.AIMD.OnCongestion("")
			}
		}
		return nil, &engine.RunError{Kind: "ModelError", Msg: err.Error()}
	}
	if resv != nil {
		_ = resv.Settle(budget.Upper{In: resp.Usage.In, Out: resp.Usage.Out})
	}
	if r.Breaker != nil {
		r.Breaker.Report(r.now(), nil)
	}
	if r.AIMD != nil {
		r.AIMD.OnSuccess("")
	}

	// 有 schema 时把响应解析成 JSON 值；没有 schema 就返回原文。
	// 多轮（agent）时把"文字 + 工具调用请求"一起交回去：
	// 上层要能分辨"这句话"与"它还要调工具"，而不是从一段文本里猜。
	if len(req.Messages) > 0 {
		return &engine.ModelReply{Content: resp.Content, ToolCalls: resp.ToolCalls}, nil
	}
	if len(req.Schema) > 0 {
		var v any
		if uerr := jsonUnmarshalNumber([]byte(resp.Content), &v); uerr == nil {
			return v, nil
		}
		// 解析失败是**契约违约**（模型没按 schema 说话），不是网络错误。
		return nil, &engine.RunError{Kind: "ContractError", Msg: "响应不符合 schema: " + trim(resp.Content)}
	}
	return resp.Content, nil
}

// Tool 实现 engine.Runtime：一次 HTTP 调用。
func (r *Runtime) Tool(ctx context.Context, req engine.ToolRequest) (any, error) {
	if r.ToolTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.ToolTimeout)
		defer cancel()
	}
	method := req.Method
	if method == "" {
		method = "GET"
	}
	// body：非 nil 时按 JSON 编码。**不猜**用 POST —— method 缺省仍是 GET，
	// 而 GET 带 body 是合法但不常见的，悄悄改成 POST 会让幂等性变掉。
	var bodyReader io.Reader
	if req.Body != nil {
		enc, err := json.Marshal(req.Body)
		if err != nil {
			return nil, &engine.RunError{Kind: "HttpError", Msg: "请求体无法序列化: " + err.Error()}
		}
		bodyReader = bytes.NewReader(enc)
	}
	if req.TimeoutMS > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutMS)*time.Millisecond)
		defer cancel()
	}
	hreq, err := http.NewRequestWithContext(ctx, method, req.URL, bodyReader)
	if err != nil {
		return nil, &engine.RunError{Kind: "NetworkError", Msg: err.Error()}
	}
	// 头必须在 Do **之前**设置 —— 放在后面代码一样编过、测试一样不报错，
	// 只是服务端收到的是空头。这类错误只有"看服务端收到了什么"才抓得住。
	for k, v := range req.Header {
		hreq.Header.Set(k, v)
	}
	if req.Body != nil {
		// 只有我们自己序列化了 body 时才设 Content-Type：用户显式给了就不覆盖。
		if hreq.Header.Get("Content-Type") == "" {
			hreq.Header.Set("Content-Type", "application/json")
		}
	}

	c := r.HTTPClient
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	res, err := c.Do(hreq)
	if err != nil {
		return nil, &engine.RunError{Kind: "NetworkError", Msg: err.Error()}
	}
	defer res.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	allowed := false
	for _, code := range req.AllowStatus {
		if res.StatusCode == code {
			allowed = true
			break
		}
	}
	if allowed {
		// allow_status 命中：这是**正常返回**，不是一个错误。
		// 返回 {status, body, headers} 三元组（LANGUAGE §5.6 的 HttpResult<T>），
		// 调用方才能写 if (r.status == 404) 这样的分支。
		var payload any
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		if err := dec.Decode(&payload); err != nil {
			payload = string(body)
		}
		hdrs := make(map[string]any, len(res.Header))
		for k := range res.Header {
			hdrs[k] = res.Header.Get(k)
		}
		return map[string]any{
			"status":  int64(res.StatusCode),
			"body":    payload,
			"headers": hdrs,
		}, nil
	}
	if res.StatusCode >= 400 {
		// 非 2xx 且不在 allow_status 里 → HttpError（D-010.2）。
		return nil, &engine.RunError{
			Kind: "HttpError", Status: res.StatusCode,
			Msg: trim(string(body)),
		}
	}
	var v any
	if err := jsonUnmarshalNumber(body, &v); err != nil {
		return string(body), nil
	}
	return v, nil
}

func trim(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Store 实现 engine.StoreRuntime：把 memory/vector 内建落到文件存储 + embeddings。
// nsKey 给存储键加 unit 命名空间。
//
// 为什么必须有：两个不同的 unit 共用同一个 stores 目录时，同名键会互相覆盖 ——
// 那是**数据串号**，比性能问题严重得多。命名空间取自 unit 名，编译期就确定。
func nsKey(unit, key string) string {
	if unit == "" {
		return key
	}
	return unit + "/" + key
}

func (r *Runtime) Store(ctx context.Context, req engine.StoreRequest) (any, error) {
	s := r.StoreHandle
	if s == nil {
		opened, err := OpenStore("")
		if err != nil {
			return nil, err
		}
		s = opened
		r.StoreHandle = s
	}
	switch req.Op {
	case "memory.get":
		v, ok, err := s.MemoryGet(nsKey(req.Unit, NormalizeKey(req.Key)))
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, nil // 没有就是 nil：调用方据此用默认值，而不是把"缺失"当错误
		}
		return v, nil
	case "memory.put":
		return true, s.MemoryPut(nsKey(req.Unit, NormalizeKey(req.Key)), req.Value, req.TTL)
	case "memory.delete":
		// 删除也走命名空间：删别人的键不该成功。
		return s.MemoryDelete(nsKey(req.Unit, NormalizeKey(req.Key)))
	case "vector.delete":
		return s.VectorDelete(nsKey(req.Unit, req.Key))
	case "vector.put":
		vec, model, err := r.embed(req.Text, req.Backend)
		if err != nil {
			return nil, err
		}
		return true, s.VectorPut(nsKey(req.Unit, req.Key), req.Text, vec, model)
	case "vector.query":
		vec, _, err := r.embed(req.Text, req.Backend)
		if err != nil {
			return nil, err
		}
		m, err := s.VectorQuery(nsKey(req.Unit, ""), vec, req.K)
		if err != nil {
			return nil, err
		}
		return m, nil
	}
	return nil, fmt.Errorf("runtime: 不认识的存储操作 %q", req.Op)
}

func (r *Runtime) embed(text, backend string) ([]float32, string, error) {
	model := os.Getenv("HERMES_EMBED_MODEL")
	if model == "" {
		model = "text-embedding-3-small"
	}
	base := os.Getenv("HERMES_EMBED_URL")
	key := os.Getenv("HERMES_API_KEY")
	// 有 backend 就优先用它的地址与鉴权：存储不该绕过 workflow 已经选定的上游。
	if b, ok := r.Backends[backend]; ok && b.Addr.Value != nil {
		base = *b.Addr.Value
		if b.Auth != nil {
			key = os.Getenv(b.Auth.Env)
		}
	}
	vec, err := Embed(base, key, model, text)
	if err != nil {
		return nil, "", err
	}
	return vec, model, nil
}
