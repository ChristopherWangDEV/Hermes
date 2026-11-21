// Package netcall 把"一次外部调用"包成 **JSON 进 / JSON 出**，供 C ABI 使用。
//
// 为什么单独一层而不是直接 //export 到 c-archive：c-archive 的导出函数不能在
// `go test` 里跑（buildmode=c-archive 是个库，不是测试目标）。把逻辑放在这个
// 普通包里，就能用 httptest 真走一遍 HTTP，而 cmd/hermesnet 只做薄封装。
//
// 这是 S2 的"能力层"：编译产物通过 libhermesnet 调到这里，跑的是和 `hermes run`
// **同一份** Runtime.Tool / Runtime.Model 实现 —— 语义不靠复制粘贴来保持一致。
package netcall

import (
	"context"
	"encoding/json"

	"hermes/internal/engine"
	"hermes/internal/runtime"
)

func marshal(v any) ([]byte, string) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, "result not serializable: " + err.Error()
	}
	return b, ""
}

// ToolReq 是一次工具调用的 JSON 形态（对应 IR 的 call_tool 参数）。
type ToolReq struct {
	URL         string            `json:"url"`
	Method      string            `json:"method"`
	Header      map[string]string `json:"header"`
	Body        any               `json:"body"`
	AllowStatus []int             `json:"allow_status"`
	TimeoutMS   int               `json:"timeout_ms"`
	Workflow    string            `json:"workflow"`
	Tool        string            `json:"tool"`
}

// Status 是 C ABI 的状态码，与 hermes_status 的前几档同义。
type Status int

const (
	OK       Status = 0
	Contract Status = 1
	Network  Status = 2
	HTTP     Status = 3
	ModelErr Status = 4
	Circuit  Status = 5
	Internal Status = 8
)

// kindToStatus 把 engine.RunError.Kind 映射成状态码（对应 CatchType）。
func kindToStatus(kind string) Status {
	switch kind {
	case "ContractError":
		return Contract
	case "NetworkError":
		return Network
	case "HttpError":
		return HTTP
	case "ModelError":
		return ModelErr
	case "CircuitOpenError":
		return Circuit
	case "":
		return OK
	default: // "Error" 及未映射
		return Internal
	}
}

// Tool 执行一次工具调用。返回结果 JSON（ok 时）或错误信息（失败时）。
func Tool(rt *runtime.Runtime, req ToolReq) (out []byte, errMsg string, st Status) {
	treq := engine.ToolRequest{
		Workflow:    req.Workflow,
		Tool:        req.Tool,
		URL:         req.URL,
		Method:      req.Method,
		Header:      req.Header,
		Body:        req.Body,
		AllowStatus: req.AllowStatus,
		TimeoutMS:   req.TimeoutMS,
	}
	v, err := rt.Tool(context.Background(), treq)
	if err != nil {
		if re, ok := err.(*engine.RunError); ok {
			// 纯文本 err 里必须带 status：RunError.Error() 在 Status != 0 时
			// 输出 "(status N)"，C 侧 hm_parse_status 据此还原 e.status。
			// 用 re.Msg 会丢掉 Status，让 AOT 的 e.status 静默恒 0。
			return nil, re.Error(), kindToStatus(re.Kind)
		}
		return nil, err.Error(), Internal
	}
	b, mErr := marshal(v)
	if mErr != "" {
		return nil, mErr, Contract
	}
	return b, "", OK
}

// ModelReq 是一次模型调用的 JSON 形态（对应 IR 的 call_model 参数）。
type ModelReq struct {
	Workflow    string          `json:"workflow"`
	Backend     string          `json:"backend"`
	Prompt      string          `json:"prompt"`
	Precision   string          `json:"precision"`
	MaxTokens   int             `json:"max_tokens"`
	Temperature string          `json:"temperature"`
	Schema      json.RawMessage `json:"schema"` // 原始 JSON Schema；null/空表示无约束
}

// Model 执行一次模型调用，返回回复 JSON（内容或 tool_calls）。
func Model(rt *runtime.Runtime, req ModelReq) (out []byte, errMsg string, st Status) {
	var schema []byte
	if len(req.Schema) > 0 && string(req.Schema) != "null" {
		schema = []byte(req.Schema)
	}
	mreq := engine.ModelRequest{
		Workflow:    req.Workflow,
		Backend:     req.Backend,
		Prompt:      req.Prompt,
		Precision:   req.Precision,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		Schema:      schema,
	}
	reply, err := rt.Model(context.Background(), mreq)
	if err != nil {
		if re, ok := err.(*engine.RunError); ok {
			return nil, re.Error(), kindToStatus(re.Kind)
		}
		return nil, err.Error(), Internal
	}
	b, mErr := marshal(reply)
	if mErr != "" {
		return nil, mErr, Contract
	}
	return b, "", OK
}
