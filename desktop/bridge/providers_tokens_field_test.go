package main

import (
	"testing"

	"github.com/seven7628/hai-harness/provider"
)

// 端点指纹在宿主侧的接线（2026-10）：自定义 provider（用户在 UI 手加端点）的模型元数据
// 是零值，MaxTokensField 为空 → 请求一律发 max_completion_tokens。DeepSeek 官方对该字段
// **静默忽略**（实测 cap=4 仍输出 85 token、无任何报错），上限形同虚设。
//
// 断言：overrideRegistryLocked 按 provider 名 / base_url host / 模型名族补齐字段名；
// 内置条目的既有字段名不被改写。
func TestSyncRegistryFillsMaxTokensFieldByEndpoint(t *testing.T) {
	pc := &providerConfig{
		registry: provider.NewRegistry(),
		baseURL: map[string]string{
			"opendesign2": "https://amr-link.open-design.ai/v1", // host 命中内置 opendesign 渠道
			"my-gateway":  "https://api.example.com/v1",         // 完全未知端点
			"deepseek":    "https://api.deepseek.com/v1",        // 内置 provider
			"anthropic-x": "https://api.anthropic.com",          // 无 chat-completions 条目
		},
		models: map[string]map[string]int64{
			"opendesign2": {"deepseek-v4.1-flash": 1_000_000},
			// 未知端点 + 已知模型族 → 按厂商口径兜底
			"my-gateway":  {"deepseek-v4-pro": 1_000_000, "brand-new-model": 128_000},
			"deepseek":    {"deepseek-v4-flash": 1_000_000},
			"anthropic-x": {"claude-haiku-5.5": 200_000},
		},
		maxTokens: map[string]map[string]int64{
			"opendesign2": {"deepseek-v4.1-flash": 384_000},
			"my-gateway":  {"deepseek-v4-pro": 384_000, "brand-new-model": 8_192},
			"deepseek":    {"deepseek-v4-flash": 384_000},
			"anthropic-x": {"claude-haiku-5.5": 8_192},
		},
	}
	pc.syncRegistry()

	reg := pc.registry
	if got := reg.MaxTokensFieldFor("opendesign2", "deepseek-v4.1-flash"); got != "max_tokens" {
		t.Fatalf("opendesign2（host 命中渠道）字段名 = %q, want max_tokens", got)
	}
	if got := reg.MaxTokensFieldFor("my-gateway", "deepseek-v4-pro"); got != "max_tokens" {
		t.Fatalf("未知端点 + deepseek 族字段名 = %q, want max_tokens", got)
	}
	if got := reg.MaxTokensFieldFor("my-gateway", "brand-new-model"); got != "max_completion_tokens" {
		t.Fatalf("无任何信号时应保持默认, got %q", got)
	}
	if got := reg.MaxTokensFieldFor("deepseek", "deepseek-v4-flash"); got != "max_tokens" {
		t.Fatalf("内置条目字段名被改写, got %q", got)
	}
	if got := reg.MaxTokensFieldFor("anthropic-x", "claude-haiku-5.5"); got != "max_completion_tokens" {
		t.Fatalf("anthropic 端点没有 chat-completions 口径，应保持默认, got %q", got)
	}
	// 顺带守住：渠道化之后 Max 档位不能被钳成 high（deepseek 族档位表可用）
	if got := reg.MapEffort("opendesign2", "deepseek-v4.1-flash", provider.ReasoningEffortLevelMax); got != provider.ReasoningEffortLevelMax {
		t.Fatalf("opendesign2 Max 档位被改为 %q", got)
	}
}
