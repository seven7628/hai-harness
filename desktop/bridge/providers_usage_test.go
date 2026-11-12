package main

// usage 口径 / 缓存 TTL 标注（2026-09-21）：协议字段 + Provider 面板显式标注 → 注册表 → provider。
//
// 背景：Anthropic 官方语义 input_tokens **未含**缓存（总输入 = input + cache_read +
// cache_write），而 new-api 类网关把 OpenAI 计数搬过来（input_tokens = prompt_tokens，
// 已含缓存）。判错一次，上下文占用/命中率/成本整体偏一倍。本轮把判定从「只看响应形状
// 嗅探」升级为三级：显式标注 > 响应形状 > 协议默认 —— 本测试锁 settings.json 字段
// 落注册表、热应用与清除（回自动）三条路径。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeSettingsProvider 写 settings.json 的 provider 段（测试隔离 HOME）。
func writeSettingsProvider(t *testing.T, cfg map[string]any) {
	t.Helper()
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".go-code")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"provider": cfg})
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestUsageInclFromSettingsAndHotApply(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeSettingsProvider(t, map[string]any{
		"active": "aggregator",
		// 标注 true：该网关 input_tokens 已含缓存（OpenAI 形状）
		"aggregator": map[string]any{
			"base_url":                   "https://gateway.example.com",
			"protocol":                   "anthropic",
			"usage_input_includes_cache": true,
			"models":                     map[string]any{"claude-opus-4-6": 1000000},
		},
		// 标注 false：端点按 Anthropic 官方语义（未含）——覆盖响应形状嗅探
		"zhipu": map[string]any{
			"base_url":                   "https://open.bigmodel.cn/api/anthropic",
			"protocol":                   "anthropic",
			"usage_input_includes_cache": false,
			"models":                     map[string]any{"glm-5.3": 1000000},
		},
		// 未标注：自动（协议默认 + 响应形状）
		"minimax": map[string]any{
			"base_url": "https://api.minimax.io/anthropic",
			"protocol": "anthropic",
		},
		// 缓存 TTL 标注：minimax（兼容端点，默认 5m）显式开 1h；aggregator 显式关（官方默认 1h → 5m）
		"aggregator_ttl": map[string]any{
			"base_url":     "https://gateway.example.com",
			"protocol":     "anthropic",
			"cache_ttl_1h": false,
			"models":       map[string]any{"claude-opus-4-6": 1000000},
		},
		"minimax_ttl": map[string]any{
			"base_url":     "https://api.minimax.io/anthropic",
			"protocol":     "anthropic",
			"cache_ttl_1h": true,
		},
	})

	pc := loadProviderConfig()

	// ① settings → 内存旁路表
	if v, ok := pc.usageInclOf("aggregator"); !ok || !v {
		t.Fatalf("aggregator 标注 = (%v,%v), want (true,true)", v, ok)
	}
	if v, ok := pc.usageInclOf("zhipu"); !ok || v {
		t.Fatalf("zhipu 标注 = (%v,%v), want (false,true)", v, ok)
	}
	if _, ok := pc.usageInclOf("minimax"); ok {
		t.Fatal("minimax 未配置 → 不应有标注（自动判定）")
	}

	// ② 内存旁路表 → 注册表（provider 级，provider 在请求期按 req.Provider 查它）
	if v, ok := pc.registry.UsageInputIncludesCache("aggregator", "claude-opus-4-6"); !ok || !v {
		t.Fatalf("注册表 aggregator = (%v,%v), want (true,true)", v, ok)
	}
	if v, ok := pc.registry.UsageInputIncludesCache("zhipu", "glm-5.3"); !ok || v {
		t.Fatalf("注册表 zhipu = (%v,%v), want (false,true)", v, ok)
	}
	if _, ok := pc.registry.UsageInputIncludesCache("minimax", "MiniMax-M2.7"); ok {
		t.Fatal("注册表 minimax 不应有标注")
	}

	// ③ 热应用：面板改标注（set_provider payload 路径）→ 注册表立即生效
	yes := true
	pc.setUsageIncl("minimax", &yes)
	if v, ok := pc.registry.UsageInputIncludesCache("minimax", "MiniMax-M2.7"); !ok || !v {
		t.Fatalf("热应用后 minimax = (%v,%v), want (true,true)", v, ok)
	}
	pc.setUsageIncl("aggregator", nil) // 清除 → 回自动
	if _, ok := pc.usageInclOf("aggregator"); ok {
		t.Fatal("清除后不该有标注（回自动判定）")
	}
	if _, ok := pc.registry.UsageInputIncludesCache("aggregator", "claude-opus-4-6"); ok {
		t.Fatal("清除后注册表不该残留标注")
	}

	// ③b 缓存 TTL 标注：settings → 内存 → 注册表；热应用；清除回端点默认
	if v, ok := pc.cacheTTL1hOf("aggregator_ttl"); !ok || v {
		t.Fatalf("aggregator_ttl TTL 标注 = (%v,%v), want (false,true)", v, ok)
	}
	if v, ok := pc.registry.CacheTTL1h("minimax_ttl", "MiniMax-M2.7"); !ok || !v {
		t.Fatalf("注册表 minimax_ttl TTL = (%v,%v), want (true,true)", v, ok)
	}
	if _, ok := pc.registry.CacheTTL1h("minimax", "MiniMax-M2.7"); ok {
		t.Fatal("未标注的 provider 不应有 TTL 标注（回端点默认）")
	}
	oneHour := true
	pc.setCacheTTL1h("minimax", &oneHour)
	if v, ok := pc.registry.CacheTTL1h("minimax", "MiniMax-M2.7"); !ok || !v {
		t.Fatalf("热应用后 minimax TTL = (%v,%v), want (true,true)", v, ok)
	}
	pc.setCacheTTL1h("minimax", nil)
	if _, ok := pc.registry.CacheTTL1h("minimax", "MiniMax-M2.7"); ok {
		t.Fatal("清除后不该有 TTL 标注")
	}

	// ④ 设置里删掉标注字段（provider 仍在，重新 loadSettings）→ 注册表同步清除（不粘住旧标注）
	writeSettingsProvider(t, map[string]any{
		"active": "aggregator",
		"aggregator": map[string]any{
			"base_url": "https://gateway.example.com",
			"protocol": "anthropic",
			"models":   map[string]any{"claude-opus-4-6": 1000000},
		},
		"aggregator_ttl": map[string]any{
			"base_url": "https://gateway.example.com",
			"protocol": "anthropic",
			"models":   map[string]any{"claude-opus-4-6": 1000000},
		},
		"zhipu": map[string]any{
			"base_url":                   "https://open.bigmodel.cn/api/anthropic",
			"protocol":                   "anthropic",
			"usage_input_includes_cache": false,
			"models":                     map[string]any{"glm-5.3": 1000000},
		},
	})
	pc.loadSettings()
	pc.syncRegistry()
	if _, ok := pc.registry.UsageInputIncludesCache("aggregator", "claude-opus-4-6"); ok {
		t.Fatal("settings 删字段后注册表必须清除标注")
	}
	if _, ok := pc.registry.CacheTTL1h("aggregator_ttl", "claude-opus-4-6"); ok {
		t.Fatal("settings 删 cache_ttl_1h 字段后注册表必须清除 TTL 标注")
	}
	if v, ok := pc.registry.UsageInputIncludesCache("zhipu", "glm-5.3"); !ok || v {
		t.Fatalf("zhipu 标注应保留 = (%v,%v), want (false,true)", v, ok)
	}
}
