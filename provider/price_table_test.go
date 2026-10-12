package provider

import "testing"

// TestPriceTableRefreshInvariants 价表刷新（2026-10-12）的回归护栏。
//
// 背景：providers.json 的价表此前停留在 pi 离线快照（2026-08），与端点现值漂移。
// 刷新按四类口径逐字段更新（见 scripts/gen_providers_json.py 的 PRICE_OVERRIDES）：
// 缺口保留 / 快照过旧 / 分时取 off-peak / 平台自报价优先。
// 本测试钉两件事：① 抽样值确为刷新后的口径；② 「上游字段缺失 = 缺口 → 保留现值」不被
// 反向覆盖成 0（该 bug 曾真实发生：openrouter 5 条 cache_read 被钉成 0）。
func TestPriceTableRefreshInvariants(t *testing.T) {
	reg := NewRegistry()

	// ① 抽样：刷新后的代表值（跨 4 个 provider、覆盖 4 类口径）
	samples := []struct {
		provider, model          string
		input, cacheRead, output float64
		why                      string
	}{
		{"deepseek", "deepseek-v4-flash", 0.15, 0.003, 0.6, "官方退役旧名由 V4.1-Flash 承接（off-peak）"},
		{"opencode", "deepseek-v4-flash", 0.15, 0.003, 0.6, "zen/go 按官方价转售（旧批发价 0.22/0.007/0.66 已过期）"},
		{"zhipu", "glm-5", 1.0, 0.2, 3.2, "zai 与 zhipuai 两源一致才采用"},
		{"openrouter", "deepseek/deepseek-chat", 0.32, 0.0, 0.89, "平台自 API 优先于聚合目录"},
	}
	for _, s := range samples {
		p, ok := reg.PriceFor(s.provider, s.model)
		if !ok {
			t.Errorf("PriceFor(%s, %s) 应有价（%s）", s.provider, s.model, s.why)
			continue
		}
		if p.Input != s.input || p.CacheRead != s.cacheRead || p.Output != s.output {
			t.Errorf("%s/%s = %+v, want input=%v cache_read=%v output=%v（%s）",
				s.provider, s.model, p, s.input, s.cacheRead, s.output, s.why)
		}
	}

	// ② 缺口口径：上游不返回 input_cache_read（字段缺失，非显式 0）→ 保留 repo 现值。
	// 这 5 条曾被刷新脚本反向钉成 0，等于凭「缺口」判成「无缓存价」。
	gapKept := []string{
		"deepseek/deepseek-v3.1-terminus",
		"nvidia/nemotron-3-nano-30b-a3b",
		"qwen/qwen3-235b-a22b-2507",
		"qwen/qwen3.6-27b",
		"z-ai/glm-4.7-flash",
	}
	for _, m := range gapKept {
		p, ok := reg.PriceFor("openrouter", m)
		if !ok {
			t.Errorf("openrouter/%s 应命中价表", m)
			continue
		}
		if p.CacheRead <= 0 {
			t.Errorf("openrouter/%s cache_read = %v：上游是「字段缺失」（缺口）应保留现值，不得钉 0", m, p.CacheRead)
		}
	}
}
