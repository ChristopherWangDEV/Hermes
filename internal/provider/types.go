// Package provider 是"和上游说话"的唯一地方。
//
// 三条纪律：
//   - 失败分类只走 errs 的映射表（本包不写第二份"什么算失败"的判断）；
//   - 请求体一旦 marshalled 就记下 sha256，重试与重放才有据可查（R-IO-06）；
//   - 计数要么来自上游权威端点，要么明确标成 upper_bound —— 绝不拿本地估算冒充精确值（§1.7）。
package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Target 是一次调用的目标。AIMD 窗口、熔断器、trace 的 target 字段都以它为键，
// 三处必须是同一个字符串形式，否则"同一个 target"在三处会算出三种身份。
type Target struct {
	Provider     string // openai | anthropic | ollama
	Model        string
	EndpointKind string // chat | responses | messages | generate
}

func (t Target) String() string {
	return t.Provider + "/" + t.Model + "/" + t.EndpointKind
}

// Message 是最小对话单元。刻意不叫 ChatMessage：它在三套协议间是同构的。
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// ToolCalls 只在 `role:"assistant"` 且模型要求调用时非空。
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID 只在 `role:"tool"` 时非空：它把结果**配回**某次调用。
	// 不回填这个 ID，模型无法知道哪条结果对应哪个请求 —— 顺序猜错就张冠李戴。
	ToolCallID string `json:"tool_call_id,omitempty"`
	// Name 是工具名（部分上游要求 role:"tool" 时带上）。
	Name string `json:"name,omitempty"`
}

// Request 是一次生成请求。
//
// 没有 Temperature 之类的浮点参数：D-010.1 的"禁浮点"虽针对金额，
// 但把浮点挡在 API 面之外能省掉一整类"两边序列化不一样"的跨语言麻烦。
// 需要采样控制时用整数刻度（如 top_k），由上游映射。
type Request struct {
	Model     string
	Messages  []Message
	MaxTokens int
	Stream    bool
	// OnChunk 非空时走**真流式**：每个增量文本块调用一次。
	// 返回 error 表示调用方不再要了（比如客户端断开），此时应中止上游请求。
	//
	// 为什么是回调而不是返回 channel：回调能把"中止"表达成 error，
	// 而 channel 需要额外一条控制通道，且容易忘记关闭。
	OnChunk        func(delta string) error
	ResponseFormat json.RawMessage // 直接来自 IR 的 schemas[]，运行期不重新生成
	Tools          json.RawMessage
	ToolChoice     string // "" | "auto" | "none" | "required"
}

// Usage 是本次调用的用量。Source 说明 token 数是怎么来的。
type Usage struct {
	In     int64
	Out    int64
	Source string // "provider" | "upper_bound"
}

// Response 是归一化后的结果。三套上游的差异在这一层被吃掉，
// 上层（engine/trace）不该看见 "choices" 或 "content_block" 这类字眼。
type Response struct {
	ID            string
	Content       string
	FinishReason  string // stop | length | tool_calls | content_filter
	Usage         Usage
	RequestSHA256 string // 发出去的那份字节的哈希，用于"同输入同请求"的断言
	// ToolCalls 是模型**要求**调的调用（OpenAI function calling）。
	// 有序：多个调用可以并行，但结果必须按原顺序回填 —— 顺序错了模型会张冠李戴。
	ToolCalls []ToolCall
}

// ToolCall 是一次函数调用请求。
//
// Arguments 保留**原始 JSON 文本**，不预先解析成 map：解析失败时那串字节
// 正是最有用的证据，而提前解析会把"模型给了坏 JSON"和"我们解析器有 bug"
// 混成同一个错误。
type ToolCall struct {
	ID        string
	Name      string
	Arguments string // JSON 文本
}

// Capabilities 描述"这个目标能干什么"。
//
// 关键点：**不支持也要有明确的条目**（T22 ⑨）。缺席会被上层读成"没问过"，
// 而"没问过"和"不支持"在运行期是两种完全不同的处理。
type Capabilities struct {
	Target      Target
	Endpoints   map[string]bool // endpoint_kind → 是否支持
	Streaming   bool
	Tools       bool
	JSONSchema  bool     // 是否支持 response_format.json_schema
	CountTokens string   // "provider" | "upper_bound"
	Unsupported []string // 明确不做的端点，带理由
}

// sha256Hex 是"请求字节指纹"的唯一实现。
func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
