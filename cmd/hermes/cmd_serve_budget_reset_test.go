package main

// 「租户预算的可选周期性重置」的回归测试。
//
// 这个文件里最要紧的一条不是"重置能工作"，而是 **不配置时什么都没变**：
// 累计用完仍然 402，跨月不会自己好起来，而且连时钟都不读一次。
// 其余用例证明：窗口边界按 UTC、读写两侧都惰性归零、租户级覆盖 CLI、
// 非法枚举是 usage error、并发下账本不撕裂。

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"hermes/internal/budget"
	"hermes/internal/runtime"
)

// utc 解析测试用的固定时刻。不用 time.Now()，也不需要 sleep。
func utc(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad fixture %q: %v", s, err)
	}
	return v.UTC()
}

// fixedClock 返回一个可以顺序推进的时钟。内部加锁，并发读也不会触发 race；
// 设值只在没有 goroutine 在跑的阶段做（各用例都遵守这一点）。
func fixedClock(start time.Time) (func() time.Time, func(time.Time)) {
	var mu sync.Mutex
	cur := start.UTC()
	read := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return cur
	}
	set := func(v time.Time) {
		mu.Lock()
		cur = v.UTC()
		mu.Unlock()
	}
	return read, set
}

// ledgerWithSpent 造一个"本次请求实际花了 microUSD"的账本，供 settleBudget 结算。
func ledgerWithSpent(t *testing.T, microUSD int64) *budget.Ledger {
	t.Helper()
	l := budget.New(budget.Limits3{})
	r, err := l.Reserve(budget.Scope{Workflow: "test", Step: "settle"}, budget.Upper{MicroUSD: microUSD})
	if err != nil {
		t.Fatalf("Reserve(%d): %v", microUSD, err)
	}
	if err := r.Settle(budget.Upper{MicroUSD: microUSD}); err != nil {
		t.Fatalf("Settle(%d): %v", microUSD, err)
	}
	if got := l.Spent().MicroUSD; got != microUSD {
		t.Fatalf("账本用量应 %d，实际 %d", microUSD, got)
	}
	return l
}

// budgetServer 造一个只测预算账本的服务。
// rt 必须是 *runtime.Runtime：runtimeFor 在类型断言失败时会提前返回，测不到 402。
func budgetServer(t *testing.T, globalReset string, now func() time.Time) *server {
	t.Helper()
	s := newTestServer(t)
	s.rt = &runtime.Runtime{}
	s.budgetReset = globalReset
	s.now = now
	return s
}

func isBudgetExhausted(err error) bool {
	var be *budgetExhausted
	return errors.As(err, &be)
}

// assertBudget402 走真正的 HTTP 出口，守住那条唯一的 402 文案。
func assertBudget402(t *testing.T, s *server) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"hello"}`))
	req.Header.Set("Authorization", "Bearer tok")
	s.chatCompletions(rec, req)
	if rec.Code != 402 {
		t.Fatalf("预算用完应回 402，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "tenant budget exhausted; contact the operator") ||
		!strings.Contains(rec.Body.String(), "budget_error") {
		t.Fatalf("402 文案/类型应保持原样：%s", rec.Body.String())
	}
}

// TestBudgetResetDefaultNeverResets 是"纯 opt-in"的守门测试：
// 什么都不配时，与改动前逐字节一致 —— 不重置、仍 402、不读时钟、不写窗口。
func TestBudgetResetDefaultNeverResets(t *testing.T) {
	clock, setTime := fixedClock(utc(t, "2026-01-31T12:00:00Z"))
	clockReads := 0
	s := budgetServer(t, "", func() time.Time {
		clockReads++
		return clock()
	})
	tenant := &Tenant{Name: "acme", BudgetMicroUSD: 100}
	s.tenants = &TenantSet{byToken: map[string]*Tenant{"tok": tenant}}

	s.settleBudget(tenant, ledgerWithSpent(t, 60))
	s.settleBudget(tenant, ledgerWithSpent(t, 50))
	if got := s.spentBy(tenant); got != 110 {
		t.Fatalf("默认路径应纯累计到 110，实际 %d", got)
	}
	assertBudget402(t, s)

	// 跨月、跨年都不该让预算自己恢复。
	setTime(utc(t, "2027-03-01T00:00:00Z"))
	if got := s.spentBy(tenant); got != 110 {
		t.Fatalf("默认 none 永不重置，跨月后应仍为 110，实际 %d", got)
	}
	assertBudget402(t, s)

	if clockReads != 0 {
		t.Fatalf("默认路径不该计算窗口（不该读时钟），实际读了 %d 次", clockReads)
	}
	st, ok := s.spent["acme"]
	if !ok || st.window != "" {
		t.Fatalf("默认路径不该写窗口 key，实际 %+v", st)
	}
}

// TestBudgetResetWindowKeys 守住对外说明过的窗口 key 格式与 UTC 语义。
func TestBudgetResetWindowKeys(t *testing.T) {
	cases := []struct {
		period string
		at     time.Time
		want   string
	}{
		{budgetResetDaily, utc(t, "2026-02-03T23:59:59Z"), "2026-02-03"},
		{budgetResetWeekly, utc(t, "2026-11-05T12:00:00Z"), "2026-W45"},
		{budgetResetMonthly, utc(t, "2026-02-03T23:59:59Z"), "2026-02"},
		{budgetResetNone, utc(t, "2026-02-03T23:59:59Z"), ""},
		{"", utc(t, "2026-02-03T23:59:59Z"), ""},
		{"yearly", utc(t, "2026-02-03T23:59:59Z"), ""},
	}
	for _, c := range cases {
		if got := budgetWindowKey(c.period, c.at); got != c.want {
			t.Errorf("budgetWindowKey(%q, %s) = %q，期望 %q",
				c.period, c.at.Format(time.RFC3339), got, c.want)
		}
	}

	// 本地 2026-02-01 00:30（UTC+8）在 UTC 仍是 1 月 31 日：
	// 换 TZ 不能把"这一天/这个月"挪走。
	shanghai := time.FixedZone("UTC+8", 8*3600)
	local := time.Date(2026, 2, 1, 0, 30, 0, 0, shanghai)
	if got := budgetWindowKey(budgetResetDaily, local); got != "2026-01-31" {
		t.Fatalf("daily 应按 UTC，期望 2026-01-31，实际 %q", got)
	}
	if got := budgetWindowKey(budgetResetMonthly, local); got != "2026-01" {
		t.Fatalf("monthly 应按 UTC，期望 2026-01，实际 %q", got)
	}

	// ISO 周年与日历年可以不同：2027-01-01 属于 2026-W53。
	// 这条保证用的是 t.ISOWeek()，不是拿 Format("2006") 拼周数。
	if got := budgetWindowKey(budgetResetWeekly, utc(t, "2027-01-01T12:00:00Z")); got != "2026-W53" {
		t.Fatalf("weekly 应使用 ISOWeek 的年份，期望 2026-W53，实际 %q", got)
	}
}

// TestBudgetResetRollsOnWindowChange 覆盖三种周期：同窗口累计、跨窗口归零、
// 归零后预算恢复、新窗口重新累计。
func TestBudgetResetRollsOnWindowChange(t *testing.T) {
	cases := []struct {
		period    string
		first     string
		second    string
		firstKey  string
		secondKey string
	}{
		{budgetResetDaily, "2026-01-31T12:00:00Z", "2026-02-01T12:00:00Z", "2026-01-31", "2026-02-01"},
		{budgetResetWeekly, "2026-02-01T12:00:00Z", "2026-02-02T12:00:00Z", "2026-W05", "2026-W06"},
		{budgetResetMonthly, "2026-01-31T12:00:00Z", "2026-02-02T12:00:00Z", "2026-01", "2026-02"},
	}
	for _, c := range cases {
		t.Run(c.period, func(t *testing.T) {
			clock, setTime := fixedClock(utc(t, c.first))
			s := budgetServer(t, "", clock)
			tenant := &Tenant{Name: "acme", BudgetMicroUSD: 100, BudgetReset: c.period}

			s.settleBudget(tenant, ledgerWithSpent(t, 40))
			s.settleBudget(tenant, ledgerWithSpent(t, 20))
			if got := s.spentBy(tenant); got != 60 {
				t.Fatalf("同窗口内应累计到 60，实际 %d", got)
			}
			if got := s.spent["acme"].window; got != c.firstKey {
				t.Fatalf("窗口 key 应 %q，实际 %q", c.firstKey, got)
			}
			// 同窗口里继续花到用完，预算必须仍然拦住。
			s.settleBudget(tenant, ledgerWithSpent(t, 40))
			if _, _, err := s.runtimeFor(tenant); !isBudgetExhausted(err) {
				t.Fatalf("同窗口用完应报 budgetExhausted，实际 %v", err)
			}

			// 读侧惰性重置：只读 spentBy 也要把账翻到新窗口。
			setTime(utc(t, c.second))
			if got := s.spentBy(tenant); got != 0 {
				t.Fatalf("跨窗口后应归零，实际 %d", got)
			}
			if got := s.spent["acme"].window; got != c.secondKey {
				t.Fatalf("窗口 key 应 %q，实际 %q", c.secondKey, got)
			}
			_, l, err := s.runtimeFor(tenant)
			if err != nil || l == nil {
				t.Fatalf("跨窗口后预算应恢复，实际 err=%v ledger=%v", err, l)
			}

			// 写侧惰性重置也要能独立触发（把账本直接删掉，模拟只有写路径到达）。
			delete(s.spent, "acme")
			s.settleBudget(tenant, ledgerWithSpent(t, 7))
			if got := s.spentBy(tenant); got != 7 {
				t.Fatalf("新窗口内应只记本次 7，实际 %d", got)
			}
			if got := s.spent["acme"].window; got != c.secondKey {
				t.Fatalf("写侧重置后窗口 key 应 %q，实际 %q", c.secondKey, got)
			}
		})
	}
}

// TestBudgetResetTenantOverridesCLIDefault 守优先级：租户级 > CLI > none。
// t1=2026-01-31、t2=2026-02-01：跨日 + 跨月，但仍在同一个 ISO 周 2026-W05。
func TestBudgetResetTenantOverridesCLIDefault(t *testing.T) {
	clock, setTime := fixedClock(utc(t, "2026-01-31T12:00:00Z"))
	s := budgetServer(t, budgetResetMonthly, clock)

	inherit := &Tenant{Name: "inherit", BudgetMicroUSD: 100}
	explicitNone := &Tenant{Name: "explicit-none", BudgetMicroUSD: 100, BudgetReset: budgetResetNone}
	explicitDaily := &Tenant{Name: "explicit-daily", BudgetMicroUSD: 100, BudgetReset: budgetResetDaily}
	explicitWeekly := &Tenant{Name: "explicit-weekly", BudgetMicroUSD: 100, BudgetReset: budgetResetWeekly}
	all := []*Tenant{inherit, explicitNone, explicitDaily, explicitWeekly}

	wantPeriod := map[*Tenant]string{
		inherit:        budgetResetMonthly,
		explicitNone:   budgetResetNone,
		explicitDaily:  budgetResetDaily,
		explicitWeekly: budgetResetWeekly,
	}
	for _, tn := range all {
		if got := s.resetPeriodFor(tn); got != wantPeriod[tn] {
			t.Fatalf("%s 的生效周期应 %q，实际 %q", tn.Name, wantPeriod[tn], got)
		}
		s.settleBudget(tn, ledgerWithSpent(t, 100))
		if _, _, err := s.runtimeFor(tn); !isBudgetExhausted(err) {
			t.Fatalf("%s 首窗口用完应报 budgetExhausted，实际 %v", tn.Name, err)
		}
	}

	setTime(utc(t, "2026-02-01T12:00:00Z"))
	wantUsed := map[*Tenant]int64{
		inherit:        0,   // 继承 CLI 的 monthly，跨月归零
		explicitNone:   100, // 租户显式 none 压过 CLI，永不重置
		explicitDaily:  0,   // 租户显式 daily，跨日归零
		explicitWeekly: 100, // 仍是 2026-W05，不归零
	}
	for _, tn := range all {
		if got := s.spentBy(tn); got != wantUsed[tn] {
			t.Errorf("%s 跨窗口后应为 %d，实际 %d", tn.Name, wantUsed[tn], got)
		}
	}
	if _, _, err := s.runtimeFor(explicitNone); !isBudgetExhausted(err) {
		t.Fatalf("显式 none 的租户跨月后仍应 402，实际 %v", err)
	}
	if _, _, err := s.runtimeFor(inherit); err != nil {
		t.Fatalf("继承 monthly 的租户跨月后应恢复，实际 %v", err)
	}
}

// TestBudgetResetCLIValidation 守 usage error：非法枚举与缺参数都必须是 2。
func TestBudgetResetCLIValidation(t *testing.T) {
	for _, bad := range []string{"yearly", "MONTHLY", "monthyl", "1", "none "} {
		t.Run("bad:"+bad, func(t *testing.T) {
			if got := cmdServe([]string{"--budget-reset", bad, "dummy.ir.json"}); got != 2 {
				t.Fatalf("非法枚举 %q 应退出 2，实际 %d", bad, got)
			}
		})
	}
	if got := cmdServe([]string{"--budget-reset"}); got != 2 {
		t.Fatalf("--budget-reset 缺值应退出 2，实际 %d", got)
	}

	// 合法枚举要能穿过参数解析：随后读不到 IR 会走 4，而不是 2。
	for _, good := range []string{"none", "daily", "weekly", "monthly", ""} {
		t.Run("ok:"+good, func(t *testing.T) {
			missing := filepath.Join(t.TempDir(), "does-not-exist.ir.json")
			if got := cmdServe([]string{"--budget-reset", good, missing}); got != 4 {
				t.Fatalf("合法枚举 %q 不该是 usage error，实际退出 %d", good, got)
			}
		})
	}
}

// TestBudgetResetSchemaStrictnessAndLoading 用真实调用证明 schema.Validate 的
// 严格程度，并覆盖配置层的枚举校验与空串折叠。
func TestBudgetResetSchemaStrictnessAndLoading(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// 与 internal/ir/schema.go 生成的形状一致：additionalProperties:false，
	// 且**所有声明字段都进 required**。
	declared := []byte(`{
		"type":"object",
		"required":["name","tokens","budget_reset"],
		"properties":{
			"name":{"type":"string"},
			"tokens":{"type":"array","items":{"type":"string"}},
			"budget_reset":{"type":"string"}
		},
		"additionalProperties":false
	}`)
	undeclared := []byte(`{
		"type":"object",
		"required":["name","tokens"],
		"properties":{
			"name":{"type":"string"},
			"tokens":{"type":"array","items":{"type":"string"}}
		},
		"additionalProperties":false
	}`)

	// ① 结构体里声明了字段：JSON 能过校验，且 Go 侧读得到枚举值。
	p := write("ok.json", `{"tenants":[{"name":"acme","tokens":["t"],"budget_reset":"monthly"}]}`)
	ts, err := LoadTenantsWithSchema(p, declared)
	if err != nil {
		t.Fatalf("声明了 budget_reset 的配置应能加载：%v", err)
	}
	if got := ts.Lookup("t").BudgetReset; got != budgetResetMonthly {
		t.Fatalf("budget_reset 应反序列化为 monthly，实际 %q", got)
	}

	// ② struct 未声明但 JSON 里出现：**严格拒绝**（不是宽松放过）。
	p = write("extra.json", `{"tenants":[{"name":"acme","tokens":["t"],"budget_reset":"monthly"}]}`)
	_, err = LoadTenantsWithSchema(p, undeclared)
	if err == nil || !strings.Contains(err.Error(), "undeclared fields: budget_reset") {
		t.Fatalf("未声明字段应被 schema.Validate 拒绝，实际 %v", err)
	}

	// ③ 声明了字段但 JSON 没给：required 也拒绝。这就是文档要求
	//    「每条租户 JSON 都要给出该字段（可填空串）」的依据。
	p = write("missing.json", `{"tenants":[{"name":"acme","tokens":["t"]}]}`)
	_, err = LoadTenantsWithSchema(p, declared)
	if err == nil || !strings.Contains(err.Error(), "missing required field budget_reset") {
		t.Fatalf("缺失声明字段应被拒绝，实际 %v", err)
	}

	// ④ 空串等价于未声明（随后由 CLI 默认值接管），且能正常加载。
	p = write("empty.json", `{"tenants":[{"name":"acme","tokens":["t"],"budget_reset":""}]}`)
	ts, err = LoadTenantsWithSchema(p, declared)
	if err != nil {
		t.Fatalf("空串 budget_reset 应合法：%v", err)
	}
	if norm, ok := normalizeBudgetReset(ts.Lookup("t").BudgetReset); !ok || norm != budgetResetNone {
		t.Fatalf("空串应折叠成 none，实际 (%q, %v)", norm, ok)
	}

	// ⑤ 非法枚举不能悄悄退化成不重置，配置加载阶段就要报错。
	p = write("bad.json", `{"tenants":[{"name":"acme","tokens":["t"],"budget_reset":"yearly"}]}`)
	_, err = LoadTenantsWithSchema(p, declared)
	if err == nil || !strings.Contains(err.Error(), "invalid budget_reset") {
		t.Fatalf("非法枚举应在加载时报错，实际 %v", err)
	}

	// ⑥ 没有声明 struct 时，只在租户 JSON 里写这个字段就够了。
	p = write("noschema.json", `{"tenants":[{"name":"acme","tokens":["t"],"budget_reset":"weekly"}]}`)
	ts, err = LoadTenants(p)
	if err != nil {
		t.Fatalf("无 struct 声明时应宽松加载：%v", err)
	}
	if got := ts.Lookup("t").BudgetReset; got != budgetResetWeekly {
		t.Fatalf("无 schema 时字段也应读到，实际 %q", got)
	}
}

// TestBudgetResetConcurrentReadWrite 让 -race 证明：读侧与写侧的窗口判定、
// 归零、累加全部在 spentMu 同一临界区内，账本不会撕裂。
func TestBudgetResetConcurrentReadWrite(t *testing.T) {
	for _, period := range []string{budgetResetNone, budgetResetMonthly} {
		t.Run(period, func(t *testing.T) {
			clock, _ := fixedClock(utc(t, "2026-05-10T08:00:00Z"))
			s := budgetServer(t, period, clock)
			tenant := &Tenant{Name: "acme", BudgetMicroUSD: 1 << 60, BudgetReset: period}

			const n = 64
			ledgers := make([]*budget.Ledger, n)
			for i := range ledgers {
				ledgers[i] = ledgerWithSpent(t, 2)
			}
			var wg sync.WaitGroup
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					_ = s.spentBy(tenant)
					s.settleBudget(tenant, ledgers[i])
					_ = s.spentBy(tenant)
					if _, _, err := s.runtimeFor(tenant); err != nil {
						t.Errorf("并发 runtimeFor 不该失败：%v", err)
					}
				}(i)
			}
			wg.Wait()

			if got, want := s.spentBy(tenant), int64(n*2); got != want {
				t.Fatalf("并发结算应一笔不漏：期望 %d，实际 %d", want, got)
			}
			wantWindow := ""
			if period == budgetResetMonthly {
				wantWindow = "2026-05"
			}
			if got := s.spent["acme"].window; got != wantWindow {
				t.Fatalf("窗口 key 应 %q，实际 %q", wantWindow, got)
			}
		})
	}
}
