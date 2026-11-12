package main

import (
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/seven7628/hai-harness/provider"
)

// priceRefreshTestEnv 隔离全局价表（userPriceTable）与价源地址，返回构造好的
// manager + 输出缓冲（provCfg 指向给定 providerConfig）。测试结束自动还原。
func priceRefreshTestEnv(t *testing.T, pc *providerConfig) (*manager, *syncBuf) {
	t.Helper()
	priceTableMu.Lock()
	saved := userPriceTable
	userPriceTable = map[string]map[string]provider.ModelPrice{}
	priceTableMu.Unlock()
	t.Cleanup(func() {
		priceTableMu.Lock()
		userPriceTable = saved
		priceTableMu.Unlock()
	})

	m, buf := testManager(t)
	if pc.registry == nil {
		pc.registry = provider.NewRegistry()
	}
	m.provCfg = pc
	pc.syncRegistry()
	return m, buf
}

// respondJSON 起一个返回固定 JSON 的 httptest 服务。
func respondJSON(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// refreshResp 跑一次 refresh_prices 命令，返回响应的 data 段。
func refreshResp(t *testing.T, m *manager, buf *syncBuf, payload map[string]any) (map[string]any, bool, string) {
	t.Helper()
	m.dispatch(cmd(1, "refresh_prices", payload))
	lines := readLines(buf)
	if len(lines) == 0 {
		t.Fatal("refresh_prices 无响应")
	}
	l := lines[len(lines)-1]
	data, _ := l["data"].(map[string]any)
	ok, _ := l["ok"].(bool)
	errMsg, _ := l["error"].(string)
	return data, ok, errMsg
}

// modelsDevFixture 最小 models.dev 快照：opencode-go（带价 + 免费条目）+ togetherai。
const modelsDevFixture = `{
  "opencode-go": {
    "api": "https://opencode.ai/zen/go/v1",
    "models": {
      "deepseek-v4.1-flash": {"cost": {"input": 0.15, "output": 0.6, "cache_read": 0.003, "cache_write": 0}},
      "minimax/minimax-m2.7:free": {"cost": {"input": 0, "output": 0, "cache_read": 0, "cache_write": 0}}
    }
  },
  "togetherai": {
    "api": "https://api.together.ai/v1",
    "models": {"deepseek-v4.1-flash": {"cost": {"input": 0.2, "output": 0.8}}}
  }
}`

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// fakeModelsDev 把 models.dev 地址指向 httptest 服务（测试结束还原）。
func fakeModelsDev(t *testing.T) {
	t.Helper()
	srv := respondJSON(t, modelsDevFixture)
	modelsDevPricesURL = srv.URL
	t.Cleanup(func() { modelsDevPricesURL = "https://models.dev/api.json" })
}

// TestRefreshPricesFillsGapFromModelsDev 刷新按 base_url 匹配 models.dev provider，
// 只补「当前无价」的已配置模型（免费条目、价源没有的模型都跳过），并即刻作用于注册表。
func TestRefreshPricesFillsGapFromModelsDev(t *testing.T) {
	fakeModelsDev(t)
	pc := &providerConfig{
		// 自定义名 opencode2 与 models.dev 的 opencode-go 同 base_url → 按端点匹配
		baseURL: map[string]string{"opencode2": "https://opencode.ai/zen/go/v1"},
		models: map[string]map[string]int64{"opencode2": {
			"deepseek-v4.1-flash":       1_000_000,
			"minimax/minimax-m2.7:free": 1_000_000, // 免费条目：不写入（保持无价，不虚增）
			"private-model":             1_000_000, // 价源没有：跳过
		}},
	}
	m, buf := priceRefreshTestEnv(t, pc)

	data, ok, errMsg := refreshResp(t, m, buf, map[string]any{"provider": "opencode2"})
	if !ok {
		t.Fatalf("refresh_prices 应成功: %s", errMsg)
	}
	if data["source"] != "models.dev:opencode-go" {
		t.Fatalf("价源判定 = %v, want models.dev:opencode-go", data["source"])
	}
	filled, _ := data["filled"].([]any)
	if len(filled) != 1 || filled[0] != "deepseek-v4.1-flash" {
		t.Fatalf("filled = %v, want [deepseek-v4.1-flash]", filled)
	}
	if data["skipped_count"].(float64) != 2 {
		t.Fatalf("skipped = %v, want 2（免费条目 + 价源没有的模型）", data["skipped_count"])
	}
	// 内存即刻生效：注册表查价
	p, okp := pc.registry.PriceFor("opencode2", "deepseek-v4.1-flash")
	if !okp || !approx(p.Input, 0.15) || !approx(p.Output, 0.6) || !approx(p.CacheRead, 0.003) {
		t.Fatalf("注册表价 = %+v ok=%v, want 0.15/0.6/0.003", p, okp)
	}
	if _, ok := pc.registry.PriceFor("opencode2", "private-model"); ok {
		t.Fatal("价源没有的模型不应被写入价表")
	}
}

// TestRefreshPricesKeepsExistingUnlessOverwrite 默认不覆盖已有价（内置价/手填价），
// overwrite=true 才强制刷成实时价。
func TestRefreshPricesKeepsExistingUnlessOverwrite(t *testing.T) {
	fakeModelsDev(t)
	pc := &providerConfig{
		baseURL: map[string]string{"opencode2": "https://opencode.ai/zen/go/v1"},
		models:  map[string]map[string]int64{"opencode2": {"deepseek-v4.1-flash": 1_000_000}},
	}
	m, buf := priceRefreshTestEnv(t, pc)
	// 手填价 9/9（模拟用户在设置面板填过）
	pc.setPrices("opencode2", map[string]provider.ModelPrice{
		"deepseek-v4.1-flash": {Input: 9, Output: 9},
	})

	data, ok, errMsg := refreshResp(t, m, buf, map[string]any{"provider": "opencode2"})
	if !ok {
		t.Fatalf("refresh_prices 失败: %s", errMsg)
	}
	if data["filled_count"].(float64) != 0 {
		t.Fatalf("默认不应覆盖已有手填价: %v", data)
	}
	if p, _ := priceFor("opencode2", "deepseek-v4.1-flash"); !approx(p.Input, 9) {
		t.Fatalf("手填价被改动: %+v", p)
	}

	data, ok, errMsg = refreshResp(t, m, buf, map[string]any{"provider": "opencode2", "overwrite": true})
	if !ok {
		t.Fatalf("overwrite 刷新失败: %s", errMsg)
	}
	if data["filled_count"].(float64) != 1 {
		t.Fatalf("overwrite=true 应覆盖: %v", data)
	}
	if p, _ := priceFor("opencode2", "deepseek-v4.1-flash"); !approx(p.Input, 0.15) {
		t.Fatalf("覆盖后价 = %+v, want 0.15", p)
	}
}

// TestRefreshPricesOpenRouterPerTokenConversion OpenRouter 自报价的 pricing 是**每 token**
// 的 USD 字符串 → 必须 ×1e6 换成 USD/1M（否则成本被低估六个数量级）。
func TestRefreshPricesOpenRouterPerTokenConversion(t *testing.T) {
	srv := respondJSON(t, `{"data":[{"id":"deepseek/deepseek-v4-pro",
		"pricing":{"prompt":"0.0000011154","completion":"0.0000033462","input_cache_read":"0.00000003718"}},
		{"id":"poolside/laguna-s-2.1:free","pricing":{"prompt":"0","completion":"0"}}]}`)
	openRouterPricesURL = srv.URL
	t.Cleanup(func() { openRouterPricesURL = "https://openrouter.ai/api/v1/models" })

	pc := &providerConfig{
		baseURL: map[string]string{"openrouter": "https://openrouter.ai/api/v1"},
		models:  map[string]map[string]int64{"openrouter": {"deepseek/deepseek-v4-pro": 1_000_000}},
	}
	m, buf := priceRefreshTestEnv(t, pc)

	data, ok, errMsg := refreshResp(t, m, buf, map[string]any{"provider": "openrouter"})
	if !ok {
		t.Fatalf("refresh_prices 失败: %s", errMsg)
	}
	if data["source"] != "openrouter" {
		t.Fatalf("openrouter 端点应走自报价, got %v", data["source"])
	}
	p, okp := priceFor("openrouter", "deepseek/deepseek-v4-pro")
	if !okp || !approx(p.Input, 1.1154) || !approx(p.Output, 3.3462) || !approx(p.CacheRead, 0.03718) {
		t.Fatalf("价 = %+v ok=%v, want 1.1154/3.3462/0.03718（每 token ×1e6）", p, okp)
	}
}

// TestRefreshPricesNoSourceForUnknownEndpoint 无实时价源的端点（models.dev 未收录的私有
// 网关）→ 明确失败，且不留下任何价（不虚增成本）。
func TestRefreshPricesNoSourceForUnknownEndpoint(t *testing.T) {
	fakeModelsDev(t)
	pc := &providerConfig{
		baseURL: map[string]string{"command-code": "https://api.commandcode.ai/provider/v1"},
		models:  map[string]map[string]int64{"command-code": {"deepseek/deepseek-v4-flash": 1_000_000}},
	}
	m, buf := priceRefreshTestEnv(t, pc)

	_, ok, errMsg := refreshResp(t, m, buf, map[string]any{"provider": "command-code"})
	if ok {
		t.Fatal("无价源应失败")
	}
	if errMsg == "" {
		t.Fatal("失败应带原因（便于前端提示手填价）")
	}
	if _, ok := priceFor("command-code", "deepseek/deepseek-v4-flash"); ok {
		t.Fatal("失败不应写入任何价")
	}
}

// TestRefreshPricesEndpointBeatsName 端点优先于同名：models.dev 里 "opencode"（/zen/v1）
// 与 "opencode-go"（/zen/go/v1）是两条不同端点、价也不同；本地 provider 名恰好叫
// "opencode" 但 base_url 是 go 端点 → 必须取 opencode-go。
// 回归：曾按名先命中，给整条链套了另一条端点的价（0.14/0.028/0.28 vs 0.15/0.003/0.6）。
func TestRefreshPricesEndpointBeatsName(t *testing.T) {
	const fixture = `{
	  "opencode": {"api": "https://opencode.ai/zen/v1",
	    "models": {"deepseek-v4-flash": {"cost": {"input": 0.14, "output": 0.28, "cache_read": 0.028}}}},
	  "opencode-go": {"api": "https://opencode.ai/zen/go/v1",
	    "models": {"deepseek-v4-flash": {"cost": {"input": 0.15, "output": 0.6, "cache_read": 0.003}}}}
	}`
	srv := respondJSON(t, fixture)
	modelsDevPricesURL = srv.URL
	t.Cleanup(func() { modelsDevPricesURL = "https://models.dev/api.json" })

	pc := &providerConfig{
		baseURL: map[string]string{"opencode": "https://opencode.ai/zen/go/v1"}, // go 端点
		models:  map[string]map[string]int64{"opencode": {"deepseek-v4-flash": 1_000_000}},
	}
	m, buf := priceRefreshTestEnv(t, pc)

	data, ok, errMsg := refreshResp(t, m, buf, map[string]any{"provider": "opencode"})
	if !ok {
		t.Fatalf("refresh_prices 失败: %s", errMsg)
	}
	if data["source"] != "models.dev:opencode-go" {
		t.Fatalf("应按端点命中 opencode-go, got %v", data["source"])
	}
	// 默认「缺价才填」：opencode 在宿主内置价表已有价 → 本轮不覆盖
	if data["filled_count"].(float64) != 0 {
		t.Fatalf("已有价默认不覆盖, got filled=%v", data["filled"])
	}
	// overwrite=true 时必须套 go 端点价（0.15/0.6），而不是同名 opencode（0.14/0.28）
	if _, ok, _ := refreshResp(t, m, buf, map[string]any{"provider": "opencode", "overwrite": true}); !ok {
		t.Fatal("overwrite 刷新失败")
	}
	if p, _ := priceFor("opencode", "deepseek-v4-flash"); !approx(p.Input, 0.15) || !approx(p.Output, 0.6) {
		t.Fatalf("价 = %+v, want 0.15/0.6（go 端点价）", p)
	}
}

// TestRefreshPricesByNameFallback provider 名与 models.dev 同名时按名命中（不依赖 base_url）。
func TestRefreshPricesByNameFallback(t *testing.T) {
	fakeModelsDev(t)
	pc := &providerConfig{
		baseURL: map[string]string{"togetherai": ""},
		models:  map[string]map[string]int64{"togetherai": {"deepseek-v4.1-flash": 1_000_000}},
	}
	m, buf := priceRefreshTestEnv(t, pc)

	data, ok, errMsg := refreshResp(t, m, buf, map[string]any{"provider": "togetherai"})
	if !ok {
		t.Fatalf("refresh_prices 失败: %s", errMsg)
	}
	if data["source"] != "models.dev:togetherai" {
		t.Fatalf("同名 provider 应按名命中, got %v", data["source"])
	}
}
