package provider

import "testing"

// newModelsHasEffort 本地档位包含判定：刻意不从 registry_json_test.go 复用 hasEffort ——
// 那个文件被本机 .git/info/exclude 排除（不入库），干净检出里不存在，复用会让本文件编译不过。
func newModelsHasEffort(es []ReasoningEffortLevel, e ReasoningEffortLevel) bool {
	for _, x := range es {
		if x == e {
			return true
		}
	}
	return false
}

// 2026-10 新增基础模型（deepseek-v4.1-flash / Claude 5.5 / GPT-6 系）的注册表回归。
//
// 这些模型由 scripts/gen_providers_json.py 的 NEW_BASE_MODELS 表补齐（pi 离线快照
// 2026-08 不含它们）—— 若有人重生成时漏了该表，或手改 providers.json 时写错档位/
// 模态，本文件即失败。数据口径见该脚本表头注释（官方端点 + models.dev 双源核对）。

// TestNewBaseModels_DeepSeekV41Flash：V4.1 Flash 官方口径与旧 flash 条目同族同档。
// 覆盖官方正式名 `deepseek-flash`（GET /models 返回的 id）—— 不登记它时用户
// `list_models` 拉到官方名并选中会得到 窗口=0 / 无价 / max_tokens_field 错（实测确认）。
func TestNewBaseModels_DeepSeekV41Flash(t *testing.T) {
	r := NewRegistry()
	for _, mid := range []string{"deepseek-flash", "deepseek-v4.1-flash"} {
		info, ok := r.Lookup("deepseek", mid)
		if !ok {
			t.Fatalf("deepseek/%s 不在注册表（官方正式名必须可解析）", mid)
		}
		if info.ContextWindow != 1000000 || info.MaxTokens != 384000 {
			t.Errorf("%s 窗口/上限 = %d/%d, want 1000000/384000", mid, info.ContextWindow, info.MaxTokens)
		}
		if !info.Reasoning || !info.RequiresReasoningContent {
			t.Errorf("%s 应 reasoning + requires_reasoning_content（官方多轮回传要求）: %+v", mid, info)
		}
		if info.ThinkingFormat != "deepseek" {
			t.Errorf("%s thinking_format = %q, want deepseek", mid, info.ThinkingFormat)
		}
		// 官方档位只有 low/high/max；xhigh 归 high（与旧 flash 条目一致）
		if got := r.MapEffort("deepseek", mid, ReasoningEffortLevelXHigh); got != ReasoningEffortLevelHigh {
			t.Errorf("%s xhigh = %s, want high", mid, got)
		}
		if got := r.MapEffort("deepseek", mid, ReasoningEffortLevelMax); got != ReasoningEffortLevelMax {
			t.Errorf("%s max = %s, want max（不得降档）", mid, got)
		}
		// 价：官方 off-peak $0.15 / 缓存命中 $0.003 / 输出 $0.6
		if p, ok := r.PriceFor("deepseek", mid); !ok || p.Input != 0.15 || p.Output != 0.6 || p.CacheRead != 0.003 {
			t.Errorf("%s 价 = %+v (ok=%v), want input .15 / cache .003 / output .6", mid, p, ok)
		}
		if !r.SupportsImageInput("deepseek", mid) {
			t.Errorf("deepseek/%s 应支持图片输入（官方 vision ✓）", mid)
		}
	}
	// 官方名与旧形态名必须同元数据（否则上下文预算/成本随所选名字漂移）
	a, _ := r.Lookup("deepseek", "deepseek-flash")
	b, _ := r.Lookup("deepseek", "deepseek-v4.1-flash")
	if a.ContextWindow != b.ContextWindow || a.MaxTokens != b.MaxTokens || a.Cost != b.Cost {
		t.Errorf("deepseek-flash 与 deepseek-v4.1-flash 元数据不一致: %+v vs %+v", a, b)
	}
}

// TestNewBaseModels_Claude55：Claude 5.5 三款 adaptive thinking + 官方价。
func TestNewBaseModels_Claude55(t *testing.T) {
	r := NewRegistry()
	costs := map[string][2]float64{
		"claude-opus-5-5":   {4, 20},
		"claude-sonnet-5-5": {2, 10},
		"claude-haiku-5-5":  {0.1, 0.5},
	}
	for mid, c := range costs {
		info, ok := r.Lookup("anthropic", mid)
		if !ok {
			t.Fatalf("anthropic/%s 不在注册表", mid)
		}
		if info.ContextWindow != 1000000 || info.MaxTokens != 128000 {
			t.Errorf("%s 窗口/上限 = %d/%d, want 1000000/128000", mid, info.ContextWindow, info.MaxTokens)
		}
		if !info.Reasoning {
			t.Errorf("%s 应 reasoning", mid)
		}
		// 官方 models/overview：5.5 三款 adaptive（opus=always-on、sonnet/haiku=yes）
		if !r.SupportsAdaptiveThinking("anthropic", mid) {
			t.Errorf("%s 应 adaptive thinking（官方 thinking.adaptive）", mid)
		}
		// adaptive 模型 supported_efforts 必须含基础档，否则 low/medium/high 被 clamp 到 xhigh
		for _, e := range []ReasoningEffortLevel{ReasoningEffortLevelLow, ReasoningEffortLevelMedium, ReasoningEffortLevelHigh} {
			if !newModelsHasEffort(info.ThinkingSupportedEfforts, e) {
				t.Errorf("%s supported_efforts 缺基础档 %s: %v", mid, e, info.ThinkingSupportedEfforts)
			}
		}
		// 基础档必须原样透传（不得 clamp 到 xhigh）
		if got := r.MapEffort("anthropic", mid, ReasoningEffortLevelHigh); got != ReasoningEffortLevelHigh {
			t.Errorf("%s high = %s, want high", mid, got)
		}
		if p, ok := r.PriceFor("anthropic", mid); !ok || p.Input != c[0] || p.Output != c[1] {
			t.Errorf("%s 价 = %+v (ok=%v), want input %v / output %v", mid, p, ok, c[0], c[1])
		}
	}
}

// TestNewBaseModels_GPT6：GPT-6 系跑 responses 协议（与 gpt-5.6 同族）。
func TestNewBaseModels_GPT6(t *testing.T) {
	r := NewRegistry()
	costs := map[string][2]float64{
		"gpt-6-astra": {10, 50},
		"gpt-6-sol":   {2, 10},
		"gpt-6-luna":  {0.1, 0.5},
		"gpt-6.1-sol": {2, 10},
	}
	for mid, c := range costs {
		info, ok := r.Lookup("openai", mid)
		if !ok {
			t.Fatalf("openai/%s 不在注册表", mid)
		}
		if info.Protocol != ProtocolResponses {
			t.Errorf("%s protocol = %s, want responses（gpt-5.6 同族）", mid, info.Protocol)
		}
		if info.MaxTokensField != "max_output_tokens" {
			t.Errorf("%s max_tokens_field = %q, want max_output_tokens", mid, info.MaxTokensField)
		}
		if info.ContextWindow != 1050000 || info.MaxTokens != 128000 {
			t.Errorf("%s 窗口/上限 = %d/%d, want 1050000/128000", mid, info.ContextWindow, info.MaxTokens)
		}
		if !info.Reasoning {
			t.Errorf("%s 应 reasoning", mid)
		}
		if p, ok := r.PriceFor("openai", mid); !ok || p.Input != c[0] || p.Output != c[1] {
			t.Errorf("%s 价 = %+v (ok=%v), want input %v / output %v", mid, p, ok, c[0], c[1])
		}
	}
}

// TestNewBaseModels_Aggregators：第三方聚合端点也兼容这批基础模型（任务要求）。
// 每个聚合商按自己的端点形态声明（vendor 前缀 id / thinking_format / 各自价）。
func TestNewBaseModels_Aggregators(t *testing.T) {
	r := NewRegistry()
	checks := []struct {
		prov, mid     string
		wantImage     bool
		wantReasoning bool
	}{
		// opencode（zen/go 网关）：只有该端点实际提供的三款
		{"opencode", "deepseek-v4.1-flash", true, true},
		{"opencode", "claude-haiku-5-5", true, true},
		{"opencode", "gpt-6-luna", true, true},
		// github-copilot：gpt-6 四款（responses）+ Claude 5.5 三款（anthropic，点号 id）
		{"github-copilot", "gpt-6-astra", true, true},
		{"github-copilot", "gpt-6.1-sol", true, true},
		{"github-copilot", "claude-opus-5.5", true, true},
		{"github-copilot", "claude-sonnet-5.5", true, true},
		{"github-copilot", "claude-haiku-5.5", true, true},
		// openrouter（vendor 前缀 id）
		{"openrouter", "deepseek/deepseek-v4.1-flash", true, true},
		{"openrouter", "openai/gpt-6-astra", true, true},
		{"openrouter", "openai/gpt-6.1-sol", true, true},
		// openrouter 的 "-pro" 变体（平台路由变体，同价同窗口；仓库既有 gpt-5.6-*-pro 同口径）
		{"openrouter", "openai/gpt-6-astra-pro", true, true},
		{"openrouter", "openai/gpt-6-luna-pro", true, true},
		// openrouter 的 Claude 5.5（点号 id；thinking_format=openrouter）
		{"openrouter", "anthropic/claude-opus-5.5", true, true},
		{"openrouter", "anthropic/claude-sonnet-5.5", true, true},
		{"openrouter", "anthropic/claude-haiku-5.5", true, true},
		// nvidia（"deepseek-ai/" 前缀，免费端点）
		{"nvidia", "deepseek-ai/deepseek-v4.1-flash", true, true},
		// 阿里 token-plan
		{"qwen", "deepseek-v4.1-flash", true, true},
		// 其余聚合商的 DeepSeek V4.1（各家 vendor 专有 id）
		{"together", "deepseek-ai/DeepSeek-V4.1-Flash", true, true},
		{"fireworks", "accounts/fireworks/models/deepseek-v4p1-flash", true, true},
		{"baseten", "deepseek-ai/DeepSeek-V4.1-Flash", true, true},
		{"huggingface", "deepseek-ai/DeepSeek-V4.1-Flash", true, true},
	}
	for _, c := range checks {
		info, ok := r.Lookup(c.prov, c.mid)
		if !ok {
			t.Errorf("%s/%s 不在注册表（聚合端点未兼容）", c.prov, c.mid)
			continue
		}
		if info.Reasoning != c.wantReasoning {
			t.Errorf("%s/%s reasoning = %v, want %v", c.prov, c.mid, info.Reasoning, c.wantReasoning)
		}
		if r.SupportsImageInput(c.prov, c.mid) != c.wantImage {
			t.Errorf("%s/%s 图片支持 = %v, want %v", c.prov, c.mid, r.SupportsImageInput(c.prov, c.mid), c.wantImage)
		}
		if info.ContextWindow <= 0 || info.MaxTokens <= 0 {
			t.Errorf("%s/%s 元数据不全: %+v", c.prov, c.mid, info)
		}
	}
	// openrouter 的 gpt-6 走平台归一思考接口（不能当成 OpenRouter 原生的 reasoning_effort）
	if info, ok := r.Lookup("openrouter", "openai/gpt-6-astra"); !ok || info.ThinkingFormat != "openrouter" {
		t.Errorf("openrouter/openai/gpt-6-astra thinking_format = %q, want openrouter", info.ThinkingFormat)
	}
}

// TestNewBaseModels_PresetVisible：新增模型进了预设默认清单（首屏可选）。
func TestNewBaseModels_PresetVisible(t *testing.T) {
	want := map[string][]string{
		"deepseek":  {"deepseek-v4.1-flash"},
		"anthropic": {"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5"},
		"openai":    {"gpt-6-astra", "gpt-6.1-sol", "gpt-6-sol", "gpt-6-luna"},
		"opencode":  {"deepseek-v4.1-flash", "gpt-6-luna"},
	}
	for pid, mids := range want {
		p := FindProviderPreset(pid)
		if p == nil {
			t.Fatalf("预设 %s 缺失", pid)
		}
		have := map[string]bool{}
		for _, m := range p.Models {
			have[m] = true
		}
		for _, m := range mids {
			if !have[m] {
				t.Errorf("预设 %s 默认清单缺 %s（首屏不可选）: %v", pid, m, p.Models)
			}
		}
	}
}

// TestNewBaseModels_FamilyFallbackUnchanged：新条目不得破坏名称族兜底
// （网关把上游以新版本号暴露时，仍继承 deepseek 族档位表；见 models_test.go 的族测试）。
func TestNewBaseModels_FamilyFallbackUnchanged(t *testing.T) {
	r := NewRegistry()
	// 未登记的自建 provider + 别名 → 名称族兜底：Max 原样发出
	if got := r.MapEffort("my-gateway", "deepseek-v4.1-flash", ReasoningEffortLevelMax); got != ReasoningEffortLevelMax {
		t.Errorf("my-gateway deepseek-v4.1-flash max = %s, want max（族兜底）", got)
	}
	// deepseek-v4.1-flash 与 deepseek-v4-flash / deepseek-flash 同族（去版本号）→ 声明一致
	if !AutoRetainReasoning("deepseek/deepseek-v4.1-flash") {
		t.Error("deepseek-v4.1-flash 应判定为思考模型族")
	}
	if !RequiresReasoningContentByName("deepseek/deepseek-v4.1-flash") {
		t.Error("deepseek-v4.1-flash 别名必须恒带 reasoning_content")
	}
}
