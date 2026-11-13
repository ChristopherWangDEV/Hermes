package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"

	"hermes/internal/ir"
	"strings"
	"testing"
)

// newTestServer 造一个只含内存状态的服务实例（不起监听端口）。
func newTestServer(t *testing.T) *server {
	t.Helper()
	data, err := os.ReadFile("../../schemas/golden/ir-hello.json")
	if err != nil {
		t.Fatal(err)
	}
	doc, ds := ir.Load(data)
	if len(ds) != 0 {
		t.Fatalf("golden 不合格: %d 条", len(ds))
	}
	return &server{doc: doc}
}

// TestServeHealthAndModels 守 D-021 的两个只读端点。
func TestServeHealthAndModels(t *testing.T) {
	s := newTestServer(t)

	rec := httptest.NewRecorder()
	s.health(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != 200 {
		t.Fatalf("health 应 200，实际 %d", rec.Code)
	}
	var h map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	if h["status"] != "ok" {
		t.Fatalf("health.status 应是 ok，实际 %v", h["status"])
	}

	rec = httptest.NewRecorder()
	s.models(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("models 应 200，实际 %d", rec.Code)
	}
	var m struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Data) != 1 || m.Data[0].ID != "hello" || m.Data[0].OwnedBy != "hermes" {
		t.Fatalf("/v1/models 应只列 exported workflow 且 owned_by=hermes，实际 %+v", m.Data)
	}
}

// TestServeChatCompletions 走一次真实的 chat 调用（无外部依赖）。
func TestServeChatCompletions(t *testing.T) {
	s := newTestServer(t)
	body := `{"model":"hello","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	s.chatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("应 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Object != "chat.completion" {
		t.Fatalf("object 应是 chat.completion，实际 %q", resp.Object)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "hi" {
		t.Fatalf("应返回 workflow 的值 \"hi\"，实际 %+v", resp.Choices)
	}
}

// TestServeUnknownModel 守"模型名不认识要 404 并列可用项"，而不是回空结果。
func TestServeUnknownModel(t *testing.T) {
	s := newTestServer(t)
	body := `{"model":"nope"}`
	rec := httptest.NewRecorder()
	s.chatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != 404 {
		t.Fatalf("未知 model 应 404，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "hello") {
		t.Fatalf("错误里应列出可用 workflow：%s", rec.Body.String())
	}
}

// TestServeResponses 守 Responses 面（D-021 要求两面同形）。
//
// 少一个端点，客户端"无感切换"就不成立——切换变成改配置，而改过配置之后
// 两台后端就不再是可替换的。
func TestServeResponses(t *testing.T) {
	s := newTestServer(t)
	body := `{"model":"hello","input":{"text":"hi"}}`
	rec := httptest.NewRecorder()
	s.responses(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("应 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Object string `json:"object"`
		Output []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Object != "response" {
		t.Fatalf("object 应是 response，实际 %q", resp.Object)
	}
	if len(resp.Output) != 1 || len(resp.Output[0].Content) != 1 {
		t.Fatalf("output 形状不对: %+v", resp.Output)
	}
}

// TestServeAuth 守"配了 token 就必须带"。
//
// 401 而不是 403：调用方要能区分"没带凭证"与"带了但没权限"，
// 前者该去补 Authorization，后者该去找管理员。
func TestServeAuth(t *testing.T) {
	data, err := os.ReadFile("../../schemas/golden/ir-hello.json")
	if err != nil {
		t.Fatal(err)
	}
	doc, ds := ir.Load(data)
	if len(ds) != 0 {
		t.Fatalf("golden 不合格: %d 条", len(ds))
	}
	s := &server{doc: doc, token: "s3cret"}

	cases := []struct {
		name string
		hdr  string
		want int
	}{
		{"没带凭证", "", 401},
		{"凭证错", "Bearer nope", 401},
		{"前缀对但短", "Bearer s3cre", 401},
		{"凭证对", "Bearer s3cret", 200},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			if c.hdr != "" {
				req.Header.Set("Authorization", c.hdr)
			}
			s.models(rec, req)
			if rec.Code != c.want {
				t.Fatalf("期望 %d，实际 %d：%s", c.want, rec.Code, rec.Body.String())
			}
		})
	}

	// /health 不要求凭证：探活落在负载均衡上，给它配 token 会让健康检查
	// 变成"凭证过期就整片摘流量"。
	rec := httptest.NewRecorder()
	s.health(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != 200 {
		t.Fatalf("/health 不该要凭证，实际 %d", rec.Code)
	}
}

// TestServeStreaming 守 SSE 的线格式：客户端按这个格式解析。
func TestServeStreaming(t *testing.T) {
	s := newTestServer(t)
	body := `{"model":"hello","input":{"text":"hi"},"stream":true}`
	rec := httptest.NewRecorder()
	s.chatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != 200 {
		t.Fatalf("应 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type 应是 text/event-stream，实际 %q", ct)
	}
	out := rec.Body.String()
	for _, want := range []string{
		`"delta":{"role":"assistant"}`,
		`"delta":{"content":"hi"}`,
		`"finish_reason":"stop"`,
		"data: [DONE]",
	} {
		if !strings.Contains(strings.ReplaceAll(out, " ", ""), strings.ReplaceAll(want, " ", "")) {
			t.Fatalf("SSE 输出缺少 %q：\n%s", want, out)
		}
	}
}

// TestServeRateLimit 守并发上限：超限回 429，且名额用完能还回去。
//
// 为什么必须限：每个请求都花模型的钱，而 HTTP 面没有天然背压 ——
// 一千个并发到达时全部会进模型调用，账单跟着并发数走。
func TestServeRateLimit(t *testing.T) {
	data, err := os.ReadFile("../../schemas/golden/ir-hello.json")
	if err != nil {
		t.Fatal(err)
	}
	doc, ds := ir.Load(data)
	if len(ds) != 0 {
		t.Fatalf("golden 不合格: %d 条", len(ds))
	}
	s := &server{doc: doc, maxInflight: 1}

	// 拿满一个名额。
	rec := httptest.NewRecorder()
	if !s.acquireInflight(rec) {
		t.Fatal("第一个名额应该拿得到")
	}

	// 第二个必须被拒，且是 429 + Retry-After。
	rec2 := httptest.NewRecorder()
	if s.acquireInflight(rec2) {
		t.Fatal("超过上限应该被拒")
	}
	if rec2.Code != 429 {
		t.Fatalf("超限应回 429，实际 %d", rec2.Code)
	}
	if rec2.Header().Get("Retry-After") == "" {
		t.Fatal("429 应带 Retry-After")
	}

	// 还回去之后又能拿。
	s.releaseInflight()
	rec3 := httptest.NewRecorder()
	if !s.acquireInflight(rec3) {
		t.Fatal("释放后应能再拿到名额")
	}
}

// TestSSEWriterStreamsDeltas 守真流式的三件事：
// 先发 role、增量逐块发、收尾不再重复整段。
//
// "收尾不重复"这条特别要紧：如果 finish 无条件补发完整内容，客户端会把
// 同一段话看两遍 —— 而那在界面上看起来像模型复读了。
func TestSSEWriterStreamsDeltas(t *testing.T) {
	rec := httptest.NewRecorder()
	sw, err := beginSSE(rec, "hello")
	if err != nil {
		t.Fatal(err)
	}
	sw.delta("你")
	sw.delta("好")
	sw.finish("你好")

	out := rec.Body.String()
	if !strings.Contains(out, `"delta":{"role":"assistant"}`) {
		t.Fatalf("首块应带 role：\n%s", out)
	}
	if strings.Count(out, `"content":"你好"`) != 0 {
		t.Fatalf("有增量时不该再补发整段：\n%s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("应以 finish_reason=stop 收尾：\n%s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("应发 [DONE]：\n%s", out)
	}
}

// TestSSEWriterWithoutDeltasSendsFull 守"上游不支持流式时的回退"。
func TestSSEWriterWithoutDeltasSendsFull(t *testing.T) {
	rec := httptest.NewRecorder()
	sw, _ := beginSSE(rec, "hello")
	sw.finish("整段结果")
	if !strings.Contains(rec.Body.String(), `"content":"整段结果"`) {
		t.Fatalf("没有增量时应补发完整内容：\n%s", rec.Body.String())
	}
}

// TestPerModelInflightIsolatesModels 守"一个热模型不会饿死其他模型"。
//
// 全局上限只能回答"总共别过载"；它不能回答"慢的那个会不会占满名额"。
// 没有按模型上限时，一个卡住的 workflow 会让所有其他 workflow 一起 429。
func TestPerModelInflightIsolatesModels(t *testing.T) {
	s := &server{perModel: 1}
	rec := httptest.NewRecorder()

	if !s.acquireModel(rec, "slow") {
		t.Fatal("第一个名额应该拿到")
	}
	// 同一个模型再要一个 → 拒绝
	rec2 := httptest.NewRecorder()
	if s.acquireModel(rec2, "slow") {
		t.Fatal("同一模型的第二个名额应被拒绝")
	}
	if rec2.Code != 429 {
		t.Fatalf("应回 429，实际 %d", rec2.Code)
	}
	if rec2.Header().Get("Retry-After") == "" {
		t.Fatal("429 应带 Retry-After")
	}
	// 换一个模型 → 不受影响
	rec3 := httptest.NewRecorder()
	if !s.acquireModel(rec3, "fast") {
		t.Fatal("另一个模型不该被这个模型的配额挡住")
	}

	s.releaseModel("slow")
	if !s.acquireModel(httptest.NewRecorder(), "slow") {
		t.Fatal("释放后应能再拿到名额")
	}
}

// TestChatRequiresAuth 守一个**安全修复**：/v1/chat/completions 曾经完全没鉴权。
//
// 只有 /v1/models 校验了 token，而真正花模型钱的聊天接口是裸的 ——
// 带 --token 启动时，任何人仍然可以调用它。
func TestChatRequiresAuth(t *testing.T) {
	s := &server{token: "secret"}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"x"}`))
	s.chatCompletions(rec, req)
	if rec.Code != 401 {
		t.Fatalf("没有 token 时应 401，实际 %d", rec.Code)
	}

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"x"}`))
	req2.Header.Set("Authorization", "Bearer wrong")
	s.chatCompletions(rec2, req2)
	if rec2.Code != 401 {
		t.Fatalf("错误 token 应 401，实际 %d", rec2.Code)
	}
}

// TestTenantLookupRejectsAmbiguousToken 守"一个 token 不能属于两个租户"。
//
// 那会让"这是谁"变成运气，而配额与审计都建立在这个身份上。
func TestTenantLookupRejectsAmbiguousToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tenants.json")
	body := `{"tenants":[
		{"name":"a","tokens":["same"]},
		{"name":"b","tokens":["same"]}
	]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTenants(path); err == nil {
		t.Fatal("同一个 token 属于两个租户时应报错")
	}
}

// TestTenantConfigCheckedAgainstDeclaredStruct 守"租户是一个类型，不是一堆键值对"。
//
// 声明 `export struct Tenant {...}` 之后，配置文件会按**同一份 schema** 校验，
// 错误带字段路径；没有声明时才退化为仅做结构性检查。
func TestTenantConfigCheckedAgainstDeclaredStruct(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tenants.json")
	body := `{"tenants":[{"name":"acme","max_inflight":"two","tokens":["t"]}]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	schemaJSON := []byte(`{
		"type":"object",
		"required":["name","max_inflight"],
		"properties":{
			"name":{"type":"string"},
			"max_inflight":{"type":"integer"},
			"tokens":{"type":"array","items":{"type":"string"}}
		}}`)
	_, err := LoadTenantsWithSchema(path, schemaJSON)
	if err == nil {
		t.Fatal("字段类型不合契约时应报错")
	}
	if !strings.Contains(err.Error(), "max_inflight") {
		t.Fatalf("错误里应指出字段路径：%v", err)
	}

	// 不带 schema 时只做结构性检查：**类型正确的**配置能过。
	okPath := filepath.Join(dir, "ok.json")
	okBody := `{"tenants":[{"name":"acme","tokens":["t"]}]}`
	if err := os.WriteFile(okPath, []byte(okBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTenants(okPath); err != nil {
		t.Fatalf("合法配置应能加载: %v", err)
	}
}
