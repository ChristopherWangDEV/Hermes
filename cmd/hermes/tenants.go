package main

// 租户身份（D-039）：**静态配置的多 token**，不引入外部 IdP。
//
// 为什么不做 OIDC：项目红线是"离线可用、零依赖、零费用"。接 IdP 意味着
// 启动要拉 JWKS，可用性被外部服务绑住；而绝大多数自建部署的规模（几个团队）
// 用一张 token 表就够。需要企业 SSO 的部署，把它放在前面做网关即可 ——
// 那是 `--trust-tenant-header` 这条兼容路径要解决的问题。

import (
	"encoding/json"
	"fmt"
	"os"

	"hermes/internal/schema"
)

// Tenant 是一个身份单元 + 它的配额。
type Tenant struct {
	Name string `json:"name"`
	// Tokens 是这个租户可用的 bearer token。**放文件不放命令行**：
	// `ps` 能看到命令行参数，而 token 出现在进程列表里就等于泄露。
	Tokens []string `json:"tokens"`
	// MaxInflight 是该租户同时在跑的请求上限（0 表示沿用全局）。
	MaxInflight int `json:"max_inflight"`
	// MaxPerModel 是该租户在**单个模型**上的并发上限（0 表示沿用全局）。
	MaxPerModel int `json:"max_per_model"`
	// BudgetMicroUSD 是该租户的累计预算（0 表示不设预算）。
	// 单位与 runtime 的 cost 一致：micro-USD。
	BudgetMicroUSD int64 `json:"budget_micro_usd"`
}

// TenantSet 是 token → 租户的索引。
type TenantSet struct {
	byToken map[string]*Tenant
	list    []Tenant
}

// LoadTenants 读一份租户配置。
func LoadTenants(path string) (*TenantSet, error) {
	return LoadTenantsWithSchema(path, nil)
}

// LoadTenantsWithSchema 读配置，并用**声明的结构体**校验每一条租户。
//
// 为什么让租户变成"结构体概念"：租户有名字、配额、预算这些字段，而它们的
// 约束（必须非空、上限为正）现在只写在 Go 的解析代码里 —— 写错了要到
// `serve` 启动时才炸。把它声明成 `.h` 里的 struct，配置就能在启动时
// 按**同一份契约**校验，错误还带字段路径。
//
// tenantSchema 为 nil 时退化为只做结构性检查（旧行为）。
func LoadTenantsWithSchema(path string, tenantSchema []byte) (*TenantSet, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rawDoc map[string]any
	if err := json.Unmarshal(raw, &rawDoc); err != nil {
		return nil, fmt.Errorf("%s 不是合法 JSON: %w", path, err)
	}
	// **先按声明的契约校验，再反序列化**。顺序反过来会先撞上 Go 的类型错误
	// （"cannot unmarshal string into int"），而那条信息既没有字段路径，
	// 也不告诉你"这是 Tenant 结构体要求的"。
	if tenantSchema != nil {
		items, _ := rawDoc["tenants"].([]any)
		for i, item := range items {
			if err := schema.Validate(tenantSchema, item); err != nil {
				return nil, fmt.Errorf("%s: 第 %d 个租户不合契约（Tenant）: %v", path, i+1, err)
			}
		}
	}
	var doc struct {
		Tenants []Tenant `json:"tenants"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s 不是合法 JSON: %w", path, err)
	}
	ts := &TenantSet{byToken: map[string]*Tenant{}}
	for i := range doc.Tenants {
		t := &doc.Tenants[i]
		if t.Name == "" {
			return nil, fmt.Errorf("%s: 第 %d 个租户没有 name", path, i+1)
		}
		if len(t.Tokens) == 0 {
			return nil, fmt.Errorf("%s: 租户 %q 没有任何 token", path, t.Name)
		}
		for _, tok := range t.Tokens {
			if tok == "" {
				return nil, fmt.Errorf("%s: 租户 %q 里有空 token", path, t.Name)
			}
			if prev, ok := ts.byToken[tok]; ok {
				// 同一个 token 属于两个租户会让"这是谁"变成运气。
				return nil, fmt.Errorf("%s: token 同时属于 %q 与 %q", path, prev.Name, t.Name)
			}
			ts.byToken[tok] = t
		}
	}
	ts.list = doc.Tenants
	return ts, nil
}

// Lookup 按 token 找租户。找不到返回 nil。
func (ts *TenantSet) Lookup(token string) *Tenant {
	if ts == nil {
		return nil
	}
	return ts.byToken[token]
}

// Names 列出所有租户名（启动日志用）。
func (ts *TenantSet) Names() []string {
	if ts == nil {
		return nil
	}
	out := make([]string, 0, len(ts.list))
	for i := range ts.list {
		out = append(out, ts.list[i].Name)
	}
	return out
}
