package provider

// OpenAICompatCapabilities 留在 config.go（bridge settings JSON 反序列化依赖，不动）。

// AnthropicCapabilities 描述 Anthropic Messages 端点能力（首期全默认，预留裁剪位）。
type AnthropicCapabilities struct {
	SupportsThinking    *bool // nil = 按模型自适应（adaptive thinking）
	SupportsTemperature *bool // nil = true
	SupportsImages      *bool // nil = true
	// SupportsCacheControl 端点是否接受 system/tools 的 cache_control 缓存断点。
	// nil = 按官方端点（api.anthropic.com 开，兼容端点关）；true = 兼容端点也发
	// （new-api 类转换层 / 第三方 DeepSeek 平台接受该字段并回显缓存计数）。
	SupportsCacheControl *bool
	MaxTokens            int64 // 0 = 默认 8192
}

// ResponsesCapabilities 描述 OpenAI Responses 端点能力。
type ResponsesCapabilities struct {
	SupportsReasoningSummary *bool // nil = true（gpt-5 系）
	SupportsDeveloperRole    *bool // nil = false（首期用 instructions）
	SupportsImages           *bool // nil = true
	MaxOutputTokens          int64 // 0 = 厂商默认；钳制 ≥16
}
