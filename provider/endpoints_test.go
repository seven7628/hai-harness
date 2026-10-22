package provider

import "testing"

// 端点指纹：输出上限字段名由**端点**决定，不是模型名（2026-10 实测口径）。
// 数据依据：providers.json 18 家 chat-completions provider 每家内部字段名 100% 一致；
// 同名模型跨 provider 57 个里 50 个不一致。
func TestDetectMaxTokensField(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		baseURL  string
		model    string
		want     string
	}{
		// ① provider 名即内置 provider
		{"deepseek 官方端点", "deepseek", "https://api.deepseek.com/v1", "deepseek-v4.1-flash", "max_tokens"},
		{"zhipu 官方端点", "zhipu", "https://open.bigmodel.cn/api/paas/v4", "glm-5.3", "max_tokens"},
		{"openai 官方端点", "openai", "https://api.openai.com/v1", "gpt-6-luna", "max_completion_tokens"},
		// ② base_url host 指纹：自起名字但端点是同一家
		{"opendesign2 命中 host", "opendesign2", "https://amr-link.open-design.ai/v1", "deepseek-v4.1-flash", "max_tokens"},
		{"opendesign3qq 命中 host", "opendesign3qq", "https://amr-link.open-design.ai/v1", "mimo-v2.6-pro", "max_tokens"},
		{"opencode2 命中 host", "opencode2", "https://opencode.ai/zen/go/v1", "kimi-k2.7-code", "max_tokens"},
		{"host 带端口/尾点", "opendesign2", "https://AMR-LINK.open-design.ai:443/v1", "glm-5.3-flash", "max_tokens"},
		// provider 名优先于模型名（qwen 代理 deepseek 模型：字段名按 qwen 端点口径）
		{"provider 覆盖模型族", "qwen", "", "deepseek-v4-flash", "max_completion_tokens"},
		// ③ 端点完全未知 → 模型名 vendor 前缀兜底
		{"未知端点 + deepseek 族", "my-gateway", "https://api.example.com/v1", "deepseek-v4-pro", "max_tokens"},
		{"未知端点 + 带 vendor 路径", "my-gateway", "https://api.example.com/v1", "deepseek/deepseek-v4.1-flash", "max_tokens"},
		// 无信号：不认识的名字一律返回 ""（调用方保持默认 max_completion_tokens）
		{"未知端点 + 未知模型", "my-gateway", "https://api.example.com/v1", "totally-new-model", ""},
		{"anthropic 无 chat 条目", "anthropic", "https://api.anthropic.com", "claude-haiku-5.5", ""},
		{"空入参", "", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DetectMaxTokensField(c.provider, c.baseURL, c.model); got != c.want {
				t.Fatalf("DetectMaxTokensField(%q, %q, %q) = %q, want %q",
					c.provider, c.baseURL, c.model, got, c.want)
			}
		})
	}
}

// base_url → 内置预设 id（host 比较，忽略协议/路径/端口/大小写）。
func TestProviderIDForBaseURL(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"https://amr-link.open-design.ai/v1", "opendesign"},
		{"https://amr-link.open-design.ai", "opendesign"},
		{"https://opencode.ai/zen/go/v1", "opencode"},
		{"https://API.DeepSeek.com/v1", "deepseek"},
		{"https://api.deepseek.com.evil.example/v1", ""}, // 不做后缀模糊匹配
		{"https://api.example.com/v1", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got, _ := ProviderIDForBaseURL(c.raw); got != c.want {
			t.Fatalf("ProviderIDForBaseURL(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

// opendesign 渠道的模型条目必须自带 chat-completions 口径的字段名与思考元数据
// （否则会掉进「自定义 provider 零值 → max_completion_tokens / 档位被钳成 high」）。
func TestOpenDesignChannelMetadata(t *testing.T) {
	preset := FindProviderPreset("opendesign")
	if preset == nil {
		t.Fatal("opendesign 预设缺失")
	}
	if preset.BaseURL != "https://amr-link.open-design.ai/v1" || preset.Protocol != ProtocolChatCompletions {
		t.Fatalf("opendesign 预设端点/协议不符: %+v", preset)
	}
	if len(preset.Models) < 10 {
		t.Fatalf("opendesign 预设精选模型 = %d, want >= 10", len(preset.Models))
	}
	reg := NewRegistry()
	// Max 档位不得被钳成 high（deepseek 族档位表要能查到）
	if got := reg.MapEffort("opendesign", "deepseek-v4.1-flash", ReasoningEffortLevelMax); got != ReasoningEffortLevelMax {
		t.Fatalf("opendesign/deepseek-v4.1-flash Max 被改为 %q，want max", got)
	}
	if got := reg.MaxTokensFieldFor("opendesign", "deepseek-v4.1-flash"); got != "max_tokens" {
		t.Fatalf("字段名 = %q, want max_tokens", got)
	}
}
