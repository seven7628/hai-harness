package provider

import "strings"

// RequestConfig 厂商无关的请求级配置：语义统一，映射由各 provider 实现。
//
// 参数类别是有限的（思考开关 / 思考强度 / 输出上限 / 用量上报 / 推理回传），
// 差异在厂商格式与档位映射 —— 调用方只传统一语义，nil 字段 = 厂商默认。
// OpenAICompatCapabilities 描述一个 OpenAI-compatible chat-completions 端点实际
// 接受的可选字段。零值保持完整的 OpenAI 请求能力；Minimal=true 适用于只保证
// model/messages/stream 的代理端点。
type OpenAICompatCapabilities struct {
	Minimal  bool   `json:"minimal,omitempty"`
	BasePath string `json:"base_path,omitempty"`

	// 下列指针区分未配置与明确关闭，便于设置文件热更新。
	ReasoningEffort     *bool `json:"reasoning_effort,omitempty"`
	MaxCompletionTokens *bool `json:"max_completion_tokens,omitempty"`
	StreamUsage         *bool `json:"stream_usage,omitempty"`
	Tools               *bool `json:"tools,omitempty"`

	// ThinkingFormat 思考参数格式（对齐 pi OpenAICompletionsCompat.thinkingFormat）：
	// 零值/空 = "openai"（reasoning_effort）；其余厂商自定义：
	//   "deepseek"  → thinking:{type:"enabled"|"disabled"} + reasoning_effort
	//   "zai"       → thinking:{type} + reasoning_effort（智谱/零一）
	//   "qwen"      → enable_thinking: bool + reasoning_effort
	//   "openrouter"→ reasoning:{effort}
	//   "together"  → reasoning:{enabled} + reasoning_effort
	//   "ant-ling"  → reasoning:{effort}（仅映射档位非 null 时发）
	//   "string-thinking" → thinking: string（档位映射文本）
	// 注册表模型数据（models_*.go）按 pi 标注入；settings.json openai_compat 可覆盖。
	ThinkingFormat string `json:"thinking_format,omitempty"`

	// 下列兼容开关（对齐 pi OpenAICompletionsCompat；nil = 按默认语义，见各自方法）：
	SupportsDeveloperRole *bool `json:"supports_developer_role,omitempty"` // 推理模型用 developer role（默认：推理模型开）
	// RequiresAssistantAfterToolResult 某些端点要求 tool_result 后必须跟 assistant 消息
	//（默认 false；按模型标 true 时，tool 后直接 user 会插入空 assistant）。
	RequiresAssistantAfterToolResult *bool `json:"requires_assistant_after_tool_result,omitempty"`
	// RequiresToolResultName 某些端点要求 tool_result 消息带 name 字段（默认 false）。
	RequiresToolResultName *bool `json:"requires_tool_result_name,omitempty"`
	// RequiresThinkingAsText 某些端点要求 thinking 块转成 <thinking> 文本块（默认 false）。
	RequiresThinkingAsText *bool `json:"requires_thinking_as_text,omitempty"`

	// 厂商扩展（2026-09 补齐；对齐 pi OpenAICompletionsCompat）：
	// ZaiToolStream z.ai 端点工具流式（tool_stream:true；zai-coding-cn 系）。
	// 默认 false（不发 tool_stream 字段）。
	ZaiToolStream *bool `json:"zai_tool_stream,omitempty"`
	// ChatTemplateArgs baseten 端点的 chat_template_args（thinking 开关等）；
	// 与 ThinkingFormat="baseten" 联动。非 nil 时按模板变量注入。
	ChatTemplateArgs map[string]string `json:"chat_template_args,omitempty"`
	// DeferredToolsMode kimi 延迟工具模式（"kimi"）；tool_result 带 added_tool_names
	// 时把延迟工具转为正式 tools（大工具集优化）。空 = 不启用。
	DeferredToolsMode string `json:"deferred_tools_mode,omitempty"`
}

// FullOpenAICompatCapabilities 保持历史默认行为：发送所有已支持的可选字段。
func FullOpenAICompatCapabilities() OpenAICompatCapabilities {
	return OpenAICompatCapabilities{}
}

// CustomOpenAICompatCapabilities 是自定义 OpenAI-compatible 端点的安全默认：
// 保留完整 chat-completions 能力，但把未带版本路径的 Host 归一到 /v1。
func CustomOpenAICompatCapabilities() OpenAICompatCapabilities {
	return OpenAICompatCapabilities{BasePath: "/v1"}
}

// MinimalOpenAICompatCapabilities 返回最保守的**格式类**可选字段策略：保留标准
// OpenAI function tools（Agent 的读写/命令/子 agent 能力）与 reasoning_effort
// （思考档位是语义参数，见 supportsReasoningEffort 注释），仅省略容易被代理网关
// 拒绝的 max_completion_tokens 与 stream_options。
// OpenAI-compatible 服务的公开约定通常以 /v1 为版本根；已有 /v1 路径不会重复追加。
func MinimalOpenAICompatCapabilities() OpenAICompatCapabilities {
	return OpenAICompatCapabilities{Minimal: true, BasePath: "/v1"}
}

// ZhipuOpenAICompatCapabilities 智谱（Zhipu/ZAI）/api/paas/v4 端点的兼容能力：
// 保留 function tools（Agent 必需）与 stream_usage（成本核算），省略
// max_completion_tokens（GLM 用 max_tokens 字段名）；BasePath=/v4 修正版本根
// （默认 /v1 会拼出 /api/paas/v4/v1 错误路径）。
//
// reasoning_effort 不再省略（2026-09 用户决策：档位由模型能力消化，不由端点能力吞掉）：
// 注册表里 GLM-5.2/5.3 声明了 thinking_levels（low/high/max）→ 发 clamp 后的档位；
// glm-4.6 等 ThinkingToggleOnly 模型本就不发档位（只发 thinking.type），故不受影响。
// 若真机验证发现 /v4 拒该字段（400），settings.json 的
// openai_compat.reasoning_effort=false 可显式关掉（或在此处恢复显式 false）。
func ZhipuOpenAICompatCapabilities() OpenAICompatCapabilities {
	tools, usage, max := true, true, false
	return OpenAICompatCapabilities{
		BasePath:            "/v4",
		Tools:               &tools,
		StreamUsage:         &usage,
		MaxCompletionTokens: &max,
	}
}

// supportsReasoningEffort 是否发送 reasoning_effort（思考档位）。
//
// 默认发：档位是**模型能力**问题（注册表 ThinkingLevels / ThinkingSupportedEfforts
// 决定 clamp 到哪一档），不是端点能力问题 —— 早先由 Minimal 派生为 false 会让
// 「用户选了档位但请求体里什么都没有」（静默 no-op），与「Provider 内部消化档位选择」
// 的决策冲突（2026-09）。真正拒绝该字段的端点用显式 false 关闭（settings.json
// openai_compat.reasoning_effort=false）。
//
// 注意无档位可表达的模型不会因此多发包：ThinkingToggleOnly（glm-4.6/kimi-k2.6 等）
// 只发 thinking 开关，ThinkingDefaultOff（非推理模型）直接不发。
func (c OpenAICompatCapabilities) supportsReasoningEffort() bool {
	if c.ReasoningEffort != nil {
		return *c.ReasoningEffort
	}
	return true
}

func (c OpenAICompatCapabilities) supportsMaxCompletionTokens() bool {
	if c.MaxCompletionTokens != nil {
		return *c.MaxCompletionTokens
	}
	return !c.Minimal
}

func (c OpenAICompatCapabilities) supportsStreamUsage() bool {
	if c.StreamUsage != nil {
		return *c.StreamUsage
	}
	return !c.Minimal
}

func (c OpenAICompatCapabilities) supportsTools() bool {
	if c.Tools != nil {
		return *c.Tools
	}
	return true // function tools 是 go-code Agent 的基础能力，minimal 也必须默认保留。
}

type RequestConfig struct {
	// Thinking 思考模式开关：nil = 厂商默认（多数默认开）；false = 关闭
	// （OpenAI 兼容系映射 effort=none，Anthropic 映射 thinking.type=disabled）。
	Thinking *bool

	// Effort 思考强度（统一档位）：none|low|medium|high|xhigh|max。
	// "" = 厂商默认（通常 high）；实际档位按厂商/模型映射表换算
	// （如 DeepSeek flash 最高 high、pro 的 xhigh→max）。
	Effort ReasoningEffortLevel

	// Temperature 温度：nil = 厂商默认（推理模型可能不支持，透传由 SDK 校验）。
	// 采样默认值由宿主（bridge）决定——DeepSeek 构造 loop 时显式传 1.0。
	Temperature *float64

	// TopP 核采样概率：nil = 厂商默认；透传 OpenAI top_p。采样默认值由宿主
	// （bridge）决定——DeepSeek 构造 loop 时显式传 0.95。
	// （DeepSeek 思考模式下 temperature/top_p 会被忽略：文档口径「设置不报错但不生效」。）
	TopP *float64

	// MaxTokens 单次输出上限：0 = 厂商默认。
	// 语义统一：OpenAI chat max_tokens / Anthropic max_tokens / Responses max_output_tokens。
	MaxTokens int64

	// IncludeUsage 是否请求用量上报：nil = true（成本核算依赖 usage）。
	// OpenAI 映射 stream_options.include_usage。
	IncludeUsage *bool

	// RetainReasoning 是否把 assistant 的推理内容作为上下文回传：
	// nil = 按模型名自动判断 —— deepseek/kimi/glm/qwen/minimax 等推理模型工具调用必须
	// 回传（否则 400），openai 系（o1/gpt-5）无需回传（省 token）。
	RetainReasoning *bool

	// ThinkingBudgetTokens Anthropic thinking 预算（tokens），仅 thinking 开启时生效。
	// SDK 要求 ≥1024 且 < max_tokens；nil = 默认 1024。
	ThinkingBudgetTokens *int64

	// OpenAICompat 控制 OpenAI-compatible chat-completions 请求的可选字段。
	// nil/零值保持历史完整请求；Minimal=true 用于只支持最小 OpenAI 兼容集的网关。
	OpenAICompat *OpenAICompatCapabilities
}

// ShouldIncludeUsage 用量上报开关（默认 true）。
func (c *RequestConfig) ShouldIncludeUsage() bool {
	if c == nil || c.IncludeUsage == nil {
		return true
	}
	return *c.IncludeUsage
}

// ShouldRetainReasoning 推理回传决策：显式配置优先，否则按模型名自动判断。
func (c *RequestConfig) ShouldRetainReasoning(model string) bool {
	if c != nil && c.RetainReasoning != nil {
		return *c.RetainReasoning
	}
	return autoRetainReasoning(model)
}

// autoRetainReasoning 推理回传自动判断：DeepSeek/Kimi/GLM/Qwen 等推理模型必须回传
// （工具调用场景缺失 reasoning_content 上游 400 实证——DeepSeek 官方错误
// "The reasoning_content in the thinking mode must be passed back to the API"，
// opencode 网关同协议转发）；OpenAI 系（o1/gpt-5）无需回传（省 token，且厂商自行
// 管理思考状态，回传可能被拒）。
//
// 覆盖面 = 已确认思考模型族（含 opencode 网关模型清单 2026-08）。未知模型不自动
// 回传（保守：宁可少传省 token，回传必要性由宿主经 RequestConfig.RetainReasoning
// 显式指定——见 bridge buildLoop 对 opencode 网关的全量强制回传）。
func autoRetainReasoning(model string) bool {
	m := strings.ToLower(model)
	for _, family := range []string{"deepseek", "kimi", "glm", "qwen", "minimax", "mimo", "grok", "hy3"} {
		if strings.Contains(m, family) {
			return true
		}
	}
	return false
}

// AutoRetainReasoning 上表族判断的导出入口：宿主（bridge）为「内置表没有元数据」的
// 模型（网关别名 provider + "deepseek/..." 模型名、自定义 provider 手加模型）补全
// 注册表元数据时复用同一张族表，避免两处口径漂移。
func AutoRetainReasoning(model string) bool { return autoRetainReasoning(model) }

// RequiresReasoningContentByName 名称族兜底：assistant 消息必须恒带 reasoning_content
// 字段（空串被上游拒绝 → 调用方用空格占位，见 translateMessages）。
//
// 覆盖 deepseek 族：内置表里该族全部 requires_reasoning_content=true（deepseek/
// opencode/nvidia/openrouter 各端点），网关把上游模型以 "deepseek/..." 别名暴露、
// 内置表无该 provider 条目时靠本函数兜底（否则零值元数据盖掉兜底 → 字段缺失 →
// 上游 400 "The `reasoning_content` in the thinking mode must be passed back to the
// API"；2026-09-10 command-code-2 + deepseek/deepseek-v4.1-flash 实证：缺字段 6/6 400，
// 空格占位 200，空串 400）。
//
// 小米 MiMo 族同理（2026-09-23 对齐）：官方「深度思考」文档明确「Agent 多轮会话开启
// 深度思考且历史存在工具调用时，后续轮次回传的 assistant 必须完整回传 reasoning_content，
// 否则 400」；原生端点与 zen/go 代理的注册表条目都是 true，但网关别名（command-code 的
// "xiaomi/mimo-*"）与用户自建模型没有内置条目 → 靠本族兜底。OpenRouter 的
// "xiaomi/mimo-*" 条目是精确命中（false：平台自己处理回传），不经过本兜底。
func RequiresReasoningContentByName(model string) bool {
	m := strings.ToLower(model)
	return strings.Contains(m, "deepseek") || strings.Contains(m, "mimo")
}

// mapEffort 厂商 effort 映射：统一档位 → 厂商实际档位。
// DeepSeek 官方映射表（2026-08）：
//
//	请求传入     deepseek-v4-flash 实际    deepseek-v4-pro 实际
//	low         low                       low
//	high        high                      high
//	xhigh       high                      max
//	max         max                       max
//
// 其余模型档位原样透传（含 none/minimal/medium 由 SDK 校验）。
// 无任何档位声明的模型由 MapEffort 的最后一层兜底钳制（见 clampCompatEffort）。
func mapEffort(model string, e ReasoningEffortLevel) ReasoningEffortLevel {
	switch {
	case strings.HasPrefix(model, "deepseek-v4-flash") && e == ReasoningEffortLevelXHigh:
		return ReasoningEffortLevelHigh // flash 最高档 = high
	case strings.HasPrefix(model, "deepseek-v4-pro") && e == ReasoningEffortLevelXHigh:
		return ReasoningEffortLevelMax // pro 把 xhigh 升到 max
	}
	return e
}

// clampCompatEffort 无档位数据的模型在兼容端点上的档位兜底：只发经典集合
// {none, low, medium, high} —— minimal→low、xhigh/max→high。
//
// 为什么需要（2026-09-22 实测）：minimal/xhigh/max 是较新的档位，兼容端点大多不认。
// 同一请求体下 opencode zen + mimo-v2.6-flash（settings 里自建、注册表无收录）：
// low/medium/high/none → 200；minimal/xhigh/max → 400 "Streaming response failed:
// [400] Invalid request parameters"（整轮失败，重试无用）。钳到 high 只略降档，不打断会话。
func clampCompatEffort(e ReasoningEffortLevel) ReasoningEffortLevel {
	switch e {
	case ReasoningEffortLevelMinimal:
		return ReasoningEffortLevelLow
	case ReasoningEffortLevelXHigh, ReasoningEffortLevelMax:
		return ReasoningEffortLevelHigh
	}
	return e
}
