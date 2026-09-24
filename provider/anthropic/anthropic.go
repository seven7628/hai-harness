package anthropic

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/respjson"
)

// AnthropicProvider Anthropic Messages API（https://api.anthropic.com/v1/messages）。
// 也服务第三方 Anthropic 兼容端点（智谱 GLM / MiniMax / DeepSeek / Copilot 等）——
// 这些端点只实现基础 Messages API，不认识 Claude 的扩展特性（beta 头、工具缓存、
// eager tool streaming），compatMode=true 时降级发送（见 Stream）。
type AnthropicProvider struct {
	client   anthropic.Client
	baseURL  string
	apiKey   string
	registry *provider.Registry // 依赖注入
	compat   bool               // 兼容端点模式（非官方 api.anthropic.com）

	// cacheControl 是否发送 cache_control 缓存断点（system 末块 + 工具末项 + 对话尾部，
	// 合计 ≤3 个）。构造期恒 true（见 NewAnthropicProvider 注释），真正的闸门是注册表
	// 每模型标注：SupportsCacheControl=false 的模型（GLM/Kimi 官方直连，发该字段会 400）
	// 在 Stream 里被裁掉；WithCacheControl 仅测试与兜底用。
	// TTL 档位不在这里：官方端点 = 1h（extended TTL，带 beta 头）、兼容端点 = 5m 默认，
	// 逐请求由 compat 判定（见 Stream 的 oneHourTTL 与 translate.go cacheControlEphemeral）。
	cacheControl bool
}

// ProviderOption 构造期可选配置（WithCacheControl 等）。
type ProviderOption func(*AnthropicProvider)

// WithCacheControl 强制开启/关闭 cache_control 发送（system + 最后一个工具）。
// 默认：官方端点开、兼容端点关。第三方聚合平台（支持 prompt cache 的 DeepSeek
// 等）应显式开启。
func WithCacheControl(on bool) ProviderOption {
	return func(p *AnthropicProvider) { p.cacheControl = on }
}

// NewAnthropicProvider 依赖注入构造（deps.Registry / deps.HTTPClient 可空 = 默认）。
// baseURL 指向官方 Anthropic 端点（api.anthropic.com）时保持全部扩展能力；
// 指向第三方兼容端点时自动降级（compatMode），无需调用方显式配置。
//
// 重要：禁用 SDK 内部自动重试（WithMaxRetries(0)）—— anthropic-sdk-go 默认
// MaxRetries=2，会在 429/5xx 上静默重试（内部退避），错误上抛时已耗尽多次尝试，
// 外部 agent_loop 的 streamWithRetry 与 LLMError 事件（WillRetry/RetryDelayMs 展示、
// 用户可见的重试反馈）完全失效。重试统一交给上层：错误立即分类上抛 →
// streamWithRetry 按档位退避重试并逐次发 LLMError 事件。（对齐 openai/responses
// 协议：go-openai 无内置重试，429 直接上抛。）
func NewAnthropicProvider(deps provider.Dependencies, baseURL, apiKey string, opts ...ProviderOption) *AnthropicProvider {
	// baseURL 归一（尾部 /v1 去重，见 normalizeAnthropicBaseURL）：只改 path、不动 host，
	// 下面的网关（IsOpenCodeGateway）与官方端点（isOfficialAnthropic）判定都按 host 判，
	// 归一在这里做一次，全构造路径同一口径。
	baseURL = normalizeAnthropicBaseURL(baseURL)
	ropts := []option.RequestOption{}
	if baseURL != "" {
		ropts = append(ropts, option.WithBaseURL(baseURL))
	}
	if apiKey != "" {
		ropts = append(ropts, option.WithAPIKey(apiKey))
	}
	// 显式传 baseURL/apiKey（bridge 总是显式传：settings/env keyLocked 已解析）→
	// 禁用 SDK 环境变量默认链（DefaultClientOptions 读 ANTHROPIC_BASE_URL /
	// ANTHROPIC_AUTH_TOKEN / ANTHROPIC_API_KEY）。否则宿主机若配了 Claude Code
	// 代理环境（ANTHROPIC_AUTH_TOKEN=PROXY_xxx）会附加无效 Authorization:
	// Bearer 头，GLM 兼容端点优先认它 → 401「令牌已过期或验证不正确」（2026-09 实证）。
	if baseURL != "" || apiKey != "" {
		ropts = append(ropts, option.WithoutEnvironmentDefaults())
	}
	// HTTP client：OpenCode Go 网关（opencode.ai/... —— 网关同域暴露 anthropic 兼容
	// 端点）的准入要求是 x-opencode-session + 自有 UA，包装后注入；deps.HTTPClient
	// 为空时同样要包装（否则 SDK 自带 client 不带这些头）。
	httpClient := deps.HTTPClient
	if provider.IsOpenCodeGateway(baseURL) {
		httpClient = provider.OpenCodeGatewayClient(httpClient)
	}
	if httpClient != nil {
		ropts = append(ropts, option.WithHTTPClient(httpClient))
	}
	// 禁用 SDK 内部重试（见上注释）：重试由 agent_loop 统一决策/展示。
	ropts = append(ropts, option.WithMaxRetries(0))
	reg := deps.Registry
	if reg == nil {
		reg = provider.NewRegistry()
	}
	p := &AnthropicProvider{
		client:   anthropic.NewClient(ropts...),
		baseURL:  baseURL,
		apiKey:   apiKey,
		registry: reg,
		compat:   !isOfficialAnthropic(baseURL),
	}
	// 默认：开启 cache_control 缓存断点（官方 + 第三方聚合平台均接受；
	// GLM/Kimi 官方直连不支持由注册表 per-model 标注关闭，见 SupportsCacheControl）。
	// 注意：**没有** per-provider 的 settings 开关 —— 真正的闸门只有注册表标注与
	// WithCacheControl（测试/兜底），改语义时别只看注释。
	p.cacheControl = true
	for _, o := range opts {
		o(p)
	}
	return p
}

// normalizeAnthropicBaseURL 归一 baseURL：剥掉路径尾部的版本段 v1，避免 SDK 再拼一次。
//
// 成因：SDK 的 Messages 请求路径是**相对路径** "v1/messages"，由 option.WithBaseURL 的
// base 解析而来；而 WithBaseURL 会给非空 path 补尾斜杠（option/requestoption.go:
// u.Path += "/"），于是自带 /v1 的 base 解析成 .../v1/v1/messages —— 聚合端点直接 404
// 「路径错误」。典型用户路径：OpenCode Go 网关的 base_url 是 OpenAI 风格的
// https://opencode.ai/zen/go/v1（chat-completions / responses 都用它），协议面板切到
// Anthropic 时原样传进来。实测（2026-09）：.../zen/go/v1/messages = 401（存在，待鉴权），
// .../zen/go/v1/v1/messages = 404。
//
// 归一后用户填 https://opencode.ai/zen/go/v1 或 https://opencode.ai/zen/go 最终请求
// 同一路径 .../zen/go/v1/messages；无 /v1 尾段的 base（官方 api.anthropic.com、
// 智谱 /api/anthropic、Kimi /coding 等）原样返回，零影响。
func normalizeAnthropicBaseURL(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return baseURL // 非法 URL：原样交给 SDK 报错（不吞错误）
	}
	path := strings.TrimRight(u.Path, "/")
	slash := strings.LastIndex(path, "/")
	if !strings.EqualFold(path[slash+1:], "v1") {
		return baseURL
	}
	u.Path, u.RawPath = path[:slash], ""
	return u.String()
}

// isOfficialAnthropic 判断 baseURL 是否为官方 Anthropic 端点。
// 官方：api.anthropic.com（含子域 *.api.anthropic.com）。其余（智谱 /api/anthropic、
// minimax /anthropic、deepseek /anthropic、copilot 等）均为兼容端点。
func isOfficialAnthropic(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "api.anthropic.com" || strings.HasSuffix(h, ".api.anthropic.com")
}

func (p *AnthropicProvider) Stream(ctx context.Context, req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
	startTime := time.Now()
	started := false

	cfg := req.Config
	if cfg == nil {
		cfg = &provider.RequestConfig{}
	}

	// OpenCode Go 网关：会话 id 经 ctx 下传（transport 读它写 x-opencode-session；
	// 非网关端点无读取方，无副作用）。
	ctx = provider.WithOpenCodeSession(ctx, req.SessionID)

	// 图片降级（注册表 SupportsImage；Anthropic 支持 URL + base64，非视觉模型降级）
	messages := p.registry.DowngradeImages(req.Provider, req.Model, req.Messages)

	// 系统提示 → 顶层 system 参数（不是 system role 消息）。
	// cache_control 决策：构造默认开（WithCacheControl 可强制开关），再按模型
	// 注册表裁剪（GLM/Kimi 官方直连不支持 → SupportsCacheControl=false 关闭）。
	sendCacheControl := p.cacheControl && p.registry.SupportsCacheControl(req.Provider, req.Model)
	// 缓存 TTL：显式标注（Provider 面板「缓存 TTL」= cache_ttl_1h）> 端点默认
	// （官方 1h extended TTL；兼容端点 5m —— 它们只实现 Messages 基础协议、不认识 ttl 字段）。
	// 默认值的动机：5 分钟对本 Harness 的工具批 / 后台任务 / 用户思考太短，跨轮缓存必过期。
	// 见 translate.go cacheControlEphemeral。
	ttl1h := !p.compat
	if v, ok := p.registry.CacheTTL1h(req.Provider, req.Model); ok {
		ttl1h = v
	}
	oneHourTTL := sendCacheControl && ttl1h
	system, msgs := translateMessages(messages, req.Model, sendCacheControl, oneHourTTL)
	// 对话尾部滚动断点（第三处断点，见 markConversationTail）：没有它，缓存前缀只剩
	// 「系统提示词 + 工具 schema」，对话增长部分每轮全价。断点合计 ≤3（< 官方 4 上限）。
	if sendCacheControl {
		markConversationTail(msgs, oneHourTTL)
	}

	// max_tokens 必填（注册表真实上限；未知模型 8192）
	maxTokens := cfg.MaxTokens
	if maxTokens == 0 {
		maxTokens = p.registry.MaxTokensDefaultFor(req.Provider, req.Model)
	}
	if maxTokens <= 0 {
		maxTokens = 8192
	}
	// max_tokens clamp 到模型「内置真实上限」（RegistryLookupBuiltin 不含 settings override）：
	// 用户 settings 的 max_tokens 表可能配超厂商上限（实测 GLM anthropic 端点 max_tokens
	// 上限 131072，settings 曾配 256000 → 每次请求 400 1210「max_tokens参数非法」）。
	// override 值只允许调低、不允许调高超过内置真实上限——发请求前统一钳制。
	if info, ok := provider.RegistryLookupBuiltin(req.Provider, req.Model); ok && info.MaxTokens > 0 && maxTokens > info.MaxTokens {
		maxTokens = info.MaxTokens
	}

	// 思考：ResolveThinking（注册表：toggle-only / 档位 clamp / 默认关）
	// 对齐 pi anthropic-messages.ts：
	//   - adaptive 模型（新 claude 系，注册表 SupportsAdaptiveThinking）：
	//     thinking={type:"adaptive", display:"summarized"} + output_config.effort（归一档位映射厂商档位）
	//   - 旧模型：thinking={type:"enabled", budget_tokens, display:"summarized"}
	//     budget 按档位（pi DEFAULT_THINKING_BUDGETS），max_tokens 为 thinking 扩容，
	//     再 clamp 到上下文剩余空间（pi adjustMaxTokensForThinking + clampMaxTokensToContext）
	//   - 关闭：thinking={type:"disabled"}
	td := p.registry.ResolveThinking(req.Provider, req.Model, cfg)

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(req.Model),
		MaxTokens: maxTokens,
		Messages:  msgs,
		System:    system,
	}

	// 兼容端点（智谱 GLM 等）降级：
	//   1) GLM-5.3 系列强制思考（ThinkingForceOn）：thinking:{type:"disabled"} 会
	//      400 invalid_request_params —— 显式关思考被忽略（省略 thinking = 默认开）。
	//   2) 开启思考且非 adaptive：兼容端点不认识 budget_tokens 形式（智谱官方示例
	//      只用最基础字段）——省略 thinking 字段 = 端点默认行为（GLM 默认开思考），
	//      最稳妥。adaptive 形式仅官方 Anthropic 新模型支持，兼容端点恒非 adaptive。
	//   omitThinking 为 true 时整个 thinking 块不发送（既不 enabled 也不 disabled）：
	//   force-on 模型显式关思考 + 兼容端点非 adaptive 开思考，都走省略。
	omitThinking := false
	if p.compat {
		if p.registry.ThinkingForceOn(req.Provider, req.Model) {
			// 强制思考：disabled 会 400 → 无论开关都省略（省略 = 默认开）
			omitThinking = true
		} else if td.Enabled && !p.registry.SupportsAdaptiveThinking(req.Provider, req.Model) {
			// 兼容端点开思考且非 adaptive：不发送 budget 形式 → 省略（依赖端点默认）
			omitThinking = true
		}
	}
	sendThinking := !omitThinking && td.Enabled
	// ThinkingUnsupported（模型本身没有思考能力）：不发 disabled —— 该字段只用于表达
	//「显式关闭思考」的意图，对没有思考的模型无意义（严格端点可能拒绝）。
	sendDisabled := !omitThinking && !td.Enabled && !td.ThinkingUnsupported
	if sendDisabled {
		// 官方端点或兼容端点支持 disabled 的模型（minimax 等）显式关思考
		params.Thinking = anthropic.ThinkingConfigParamUnion{
			OfDisabled: &anthropic.ThinkingConfigDisabledParam{},
		}
	}
	if sendThinking {
		if p.registry.SupportsAdaptiveThinking(req.Provider, req.Model) {
			params.Thinking = anthropic.ThinkingConfigParamUnion{
				OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{
					Display: anthropic.ThinkingConfigAdaptiveDisplaySummarized,
				},
			}
			// 档位：归一档位 → 模型 ThinkingLevels 映射（如 claude 系 low→low/medium→medium…），
			// 透传到 output_config.effort（Anthropic 新模型 effort 档位；SDK 类型含 xhigh/max）
			if eff := p.registry.MapEffort(req.Provider, req.Model, td.Effort); eff != "" {
				params.OutputConfig = anthropic.OutputConfigParam{
					Effort: anthropic.OutputConfigEffort(eff),
				}
			}
		} else {
			// 旧模型 budget-based thinking（对齐 pi streamSimple 非 adaptive 分支）：
			//   1) max_tokens 为 thinking 扩容（base + budget 钳到模型上限）
			//   2) budget 按档位（minimal 1024 / low 2048 / medium 8192 / high 16384）
			//   3) max_tokens clamp 到上下文剩余（window - 已用 - 4096）
			//   4) budget 钳到「max_tokens - 答案保底 1024」
			info, _ := p.registry.Lookup(req.Provider, req.Model)
			modelCap := info.MaxTokens
			if modelCap <= 0 {
				modelCap = maxTokens
			}
			adjMax, adjBudget := provider.AdjustMaxTokensForThinking(maxTokens, modelCap, td.Effort)
			used := provider.EstimateContextTokens(req.Messages)
			adjMax = provider.ClampMaxTokensToContext(modelCap, info.ContextWindow, used, adjMax)
			params.MaxTokens = adjMax
			budget := adjBudget
			if room := adjMax - provider.MinAnswerTokens; room > 0 && budget > room {
				budget = room
			}
			// S7 修复：budget 必须 < max_tokens 且 ≥1（SDK 硬性要求）。极端场景
			//（maxTokens 被 clamp 到 1）时 budget 钳到 maxTokens-1 可能为 0 →
			// 再拉回 1024 会 ≥ maxTokens → 400。正确处理：先保证 < maxTokens，
			// 再保证 ≥1024（若 maxTokens≤1024，budget 只能取 maxTokens-1，思考
			// 退化为最小预算；总比 400 好）。
			if budget >= params.MaxTokens {
				budget = params.MaxTokens - 1
				if budget < 1 {
					budget = 1
				}
			}
			if budget < 1024 && budget < params.MaxTokens {
				budget = 1024
				if budget >= params.MaxTokens {
					budget = params.MaxTokens - 1
				}
			}
			params.Thinking = anthropic.ThinkingConfigParamOfEnabled(budget)
			params.Thinking.OfEnabled.Display = anthropic.ThinkingConfigEnabledDisplaySummarized
		}
	}

	// 温度：Anthropic 无 top_p；部分模型不支持 temperature —— 注册表 SupportsTemperature。
	// 对齐 pi：thinking 开启时 temperature 与 extended thinking 不兼容 → 不发（否则 400）。
	if cfg.Temperature != nil && !td.Enabled && p.registry.SupportsTemperature(req.Provider, req.Model) {
		params.Temperature = anthropic.Float(*cfg.Temperature)
	}

	// 工具
	if tools := translateTools(req.Tools, p.compat, sendCacheControl, oneHourTTL); len(tools) > 0 {
		params.Tools = tools
	}

	// 对齐 pi：流式思考交错 beta 头（interleaved-thinking-2025-05-14）——非 adaptive 思考模型
	// 需要该 beta 才能把 thinking/text/tool_use 交错流式输出（否则 400/顺序错乱）。
	// adaptive 模型内置交错，无需该头（pi 同逻辑）。
	// 工具流式 beta（fine-grained-tool-streaming-2025-05-14）：supportsEagerToolInputStreaming
	// 默认 true → 有工具时也发（eager_input_streaming 依赖该 beta）。
	// 兼容端点（智谱 GLM 等）不认识这些 Claude beta 特性 → 一律不发（否则 400）。
	var streamOpts []option.RequestOption
	var betas []string
	if !p.compat {
		if td.Enabled && !p.registry.SupportsAdaptiveThinking(req.Provider, req.Model) {
			betas = append(betas, "interleaved-thinking-2025-05-14")
		}
		if len(req.Tools) > 0 {
			betas = append(betas, "fine-grained-tool-streaming-2025-05-14")
		}
		// 1h extended cache TTL 需要显式 beta 头（SDK 常量 AnthropicBetaExtendedCacheTTL2025_04_11，
		// Messages 路径不自动注入）。只在真的发了 ttl=1h 时带 —— 不带的多余 beta 头是噪音，
		// 兼容端点更是完全不发（见 oneHourTTL）。
		if oneHourTTL {
			betas = append(betas, "extended-cache-ttl-2025-04-11")
		}
	}
	if len(betas) > 0 {
		streamOpts = append(streamOpts, option.WithHeader("anthropic-beta", strings.Join(betas, ",")))
	}

	// 流式
	stream := p.client.Messages.NewStreaming(ctx, params, streamOpts...)
	defer stream.Close() // 释放连接（含 handler 提前返回的异常路径）

	var (
		requestId             string
		content, reasoning    strings.Builder
		finishReason          core.FinishReason
		usage                 core.Usage
		pending               = make(map[int64]*core.ToolCall)
		contentSeq, reasonSeq int64
		thinkingSignature     string // thinking 块 signature（多轮回传必需）
		openAIShapedUsage     bool   // 响应 usage 带 OpenAI/DeepSeek 形状字段 → input_tokens 已含缓存
	)

	for stream.Next() {
		ev := stream.Current()

		// 事件归一（AsAny 类型安全分发）
		switch variant := ev.AsAny().(type) {
		case anthropic.MessageStartEvent:
			requestId = variant.Message.ID
			if !started {
				started = true
				if err := handler(provider.LLMStartEvent{RequestId: requestId, Model: req.Model, Timestamp: startTime}); err != nil {
					return err
				}
			}
			// message_start.usage：input / cache_read / cache_creation
			usage.Input = variant.Message.Usage.InputTokens
			usage.CacheRead = variant.Message.Usage.CacheReadInputTokens
			usage.CacheWrite = variant.Message.Usage.CacheCreationInputTokens
			// 思考 token：Anthropic 的 output_tokens 含思考（output_tokens_details.thinking_tokens
			// 是其子集，且反映**完整**思考量 —— 流里回的是 display=summarized 的摘要文本）。
			// 不填这个字段，展示层的输出速度就没法把「思考时间」从分母里排除（见
			// ReasoningSummarized 与前端 genMsOf）。
			if t := variant.Message.Usage.OutputTokensDetails.ThinkingTokens; t > 0 {
				usage.Reasoning = t
			}
			if looksOpenAIUsageShape(variant.Message.Usage.JSON.ExtraFields) {
				openAIShapedUsage = true
			}

		case anthropic.ContentBlockStartEvent:
			idx := variant.Index
			switch variant.ContentBlock.Type {
			case "text":
				if variant.ContentBlock.Text != "" {
					content.WriteString(variant.ContentBlock.Text)
					if err := handler(provider.LLMContentDeltaEvent{RequestId: requestId, Delta: variant.ContentBlock.Text, Index: contentSeq}); err != nil {
						return err
					}
					contentSeq++
				}
			case "thinking":
				if variant.ContentBlock.Thinking != "" {
					reasoning.WriteString(variant.ContentBlock.Thinking)
					if err := handler(provider.LLMReasoningDeltaEvent{RequestId: requestId, Delta: variant.ContentBlock.Thinking, Index: reasonSeq}); err != nil {
						return err
					}
					reasonSeq++
				}
				// thinking 块 signature（多轮回传必需；signature_delta 会增量追加）
				if sig := variant.ContentBlock.Signature; sig != "" {
					thinkingSignature = sig
				}
			case "tool_use":
				call := &core.ToolCall{Index: idx, Id: variant.ContentBlock.ID, Name: variant.ContentBlock.Name, RequestId: requestId}
				pending[idx] = call
				if err := handler(provider.LLMToolCallStartEvent{Id: call.Id, Index: idx, Name: call.Name}); err != nil {
					return err
				}
			}

		case anthropic.ContentBlockDeltaEvent:
			idx := variant.Index
			switch delta := variant.Delta.AsAny().(type) {
			case anthropic.TextDelta:
				content.WriteString(delta.Text)
				if err := handler(provider.LLMContentDeltaEvent{RequestId: requestId, Delta: delta.Text, Index: contentSeq}); err != nil {
					return err
				}
				contentSeq++
			case anthropic.ThinkingDelta:
				reasoning.WriteString(delta.Thinking)
				if err := handler(provider.LLMReasoningDeltaEvent{RequestId: requestId, Delta: delta.Thinking, Index: reasonSeq}); err != nil {
					return err
				}
				reasonSeq++
			case anthropic.InputJSONDelta:
				if call, ok := pending[idx]; ok {
					call.Arguments += delta.PartialJSON
					if err := handler(provider.LLMToolCallDeltaEvent{Id: call.Id, Index: idx, Name: call.Name, Delta: delta.PartialJSON}); err != nil {
						return err
					}
				}
			case anthropic.SignatureDelta:
				thinkingSignature += delta.Signature // 增量追加 thinking signature
			}

		case anthropic.MessageDeltaEvent:
			// usage：delta 是累计全量（官方 SDK 注释：Total input = input_tokens +
			// cache_creation + cache_read）。官方端点在 message_start 已带 cache 字段，
			// 但第三方兼容层（new-api 等）只在 message_delta 返回完整 usage（含
			// cache_creation_input_tokens / cache_read_input_tokens），message_start
			// 只有 input —— 这里必须读取才能拿到缓存 token。
			usage.Output = variant.Usage.OutputTokens
			if u := variant.Usage.InputTokens; u > 0 {
				usage.Input = u
			}
			if u := variant.Usage.CacheReadInputTokens; u > 0 {
				usage.CacheRead = u
			}
			if u := variant.Usage.CacheCreationInputTokens; u > 0 {
				usage.CacheWrite = u
			}
			if t := variant.Usage.OutputTokensDetails.ThinkingTokens; t > 0 {
				usage.Reasoning = t
			}
			if looksOpenAIUsageShape(variant.Usage.JSON.ExtraFields) {
				openAIShapedUsage = true
			}
			// stop_reason
			switch variant.Delta.StopReason {
			case anthropic.StopReasonEndTurn:
				finishReason = core.FinishReasonStop
			case anthropic.StopReasonMaxTokens:
				finishReason = core.FinishReasonLength
			case anthropic.StopReasonToolUse:
				finishReason = core.FinishReasonToolCall
			case anthropic.StopReasonModelContextWindowExceeded:
				// 上下文超限 → 语义错误（不可重试）。用量一并上抛（超限请求通常未计费，
				// 但已收到的部分照记不丢 —— 与流末错误同一口径）。
				p.finalizeUsage(req, &usage, openAIShapedUsage, sendThinking, oneHourTTL)
				return provider.WithPartialUsage(
					provider.DetectContextExceeded(errors.New("anthropic: model context window exceeded")), usage)
			}

		case anthropic.MessageStopEvent:
			// 流结束（usage 已累计）
		}
	}
	if err := stream.Err(); err != nil {
		// 失败尝试的已产生用量（官方端点 message_start 就带 input/cache；流中途失败时
		// output 可能为 0）也要进账：先归一 + 折价，再随错误上抛（2026-09-23 决策）。
		p.finalizeUsage(req, &usage, openAIShapedUsage, sendThinking, oneHourTTL)
		return provider.WithPartialUsage(classifyAnthropicError(err), usage)
	}

	// 归一（Input 口径 + InputSemantics + 成本，含 1h 写入价修正）走 finalizeUsage：
	// 成功与失败（部分用量随错误上抛）两条路径共用同一实现，口径不允许漂移。
	p.finalizeUsage(req, &usage, openAIShapedUsage, sendThinking, oneHourTTL)

	toolCalls := make([]core.ToolCall, 0, len(pending))
	for _, call := range pending {
		toolCalls = append(toolCalls, *call)
	}
	sort.Slice(toolCalls, func(i, j int) bool { return toolCalls[i].Index < toolCalls[j].Index })

	return handler(provider.LLMEndEvent{
		RequestId:          requestId,
		Model:              req.Model,
		Content:            content.String(),
		Reasoning:          reasoning.String(),
		ReasoningSignature: thinkingSignature,
		ToolCalls:          toolCalls,
		FinishReason:       finishReason,
		Usage:              usage,
		Timestamp:          time.Now(),
	})
}

// finalizeUsage 流结束（**成功或失败**）时的统一归一：Input 总输入口径 + InputSemantics +
// TotalTokens + ReasoningSummarized + 成本（含 1h 缓存写入价修正）。
//
// 为什么抽成方法（2026-09-23）：失败尝试的部分用量也要进账（上游已按已生成 token 计费），
// 两条路径必须共用同一套归一 —— 否则失败路径的 Input 口径/成本会与成功路径漂移。
// 参数：openAIShapedUsage = 流中是否见到 OpenAI/DeepSeek 形状计数；sendThinking =
// 本轮是否请求了思考；oneHourTTL = 本轮缓存断点是否 1h 档（写入价 2× 输入价）。
func (p *AnthropicProvider) finalizeUsage(req *provider.StreamRequest, u *core.Usage, openAIShapedUsage, sendThinking, oneHourTTL bool) {
	// 统一口径（流结束统一归一，覆盖官方端点 message_start 带缓存、第三方
	// message_delta 带缓存的两种形态）：
	// core.Usage.Input = 总输入 token（含缓存命中/写入）—— 与 OpenAI 协议
	// （prompt_tokens 已含 cached_tokens）一致。Anthropic 官方语义
	// Total input = input_tokens + cache_read + cache_creation（input_tokens 只是
	// 未命中部分），此处归一为总输入：
	//   - 客户端命中率 cacheRead/Input 两协议统一（不再 >100% 荒谬）
	//   - 上下文占用/压缩锚点使用总输入（Anthropic 协议此前低估）
	//   - 成本按 (Input-CacheRead-CacheWrite)*全价 + CacheRead*低价 计算（costUsd）
	// TotalTokens = 总输入 + 输出（各协议一致，不重复加缓存）。
	//
	// 形状判定（2026-09-20）：官方语义之外还有一类 **OpenAI 形状的兼容网关**（new-api
	// 等）——它们把 OpenAI 的计数直接搬到 Anthropic 字段上，input_tokens 就是
	// prompt_tokens（**已含缓存命中**），并常在响应里额外带 prompt_tokens_details.
	// cached_tokens / prompt_cache_hit_tokens 这类 OpenAI/DeepSeek 字段。对这类端点再做
	// 上面的加法会重复计数：ctx 锚点虚高近 2×、缓存命中率腰斩（120/1080 而真值
	// 120/540）、成本里未命中部分多算。
	//
	// 口径来源三级优先（2026-09-21 定，配合协议字段）：
	//   ① 注册表**显式标注**（Provider 面板「usage 口径」= usage_input_includes_cache /
	//      providers.json 的模型字段）—— 端点违背所声明协议时唯一可靠的判据，且用户可自纠；
	//   ② 响应**自报形状**（openAIShapedUsage，仅在未标注时作证据）：带 OpenAI/DeepSeek
	//      计数明细 ⇒ 计数形状是 OpenAI 的，跳过相加；
	//   ③ **协议默认**：anthropic 协议 = Anthropic 官方语义（input_tokens 未含缓存）→ 相加。
	// 注册表（零值构造兜底：测试 mock 不走构造函数）—— 归一/成本只走这一份引用。
	reg := p.registry
	if reg == nil {
		reg = provider.NewRegistry()
	}
	inclusiveUsage, usageAnnotated := reg.UsageInputIncludesCache(req.Provider, req.Model)
	addCachedToInput := true
	// InputSemantics：把「这次的口径是怎么判出来的」落到数据里（自证字段，见 core.Usage）。
	// 端点违背所声明协议时（Anthropic 端点回 OpenAI 形状计数），事后能从事后数据区分
	// 「端点错了」还是「本地判错了」，不必重放请求。
	semantics := core.UsageSemanticsProtocolAnthropic
	switch {
	case usageAnnotated:
		addCachedToInput = !inclusiveUsage
		if inclusiveUsage {
			semantics = core.UsageSemanticsAnnotationInclusive
		} else {
			semantics = core.UsageSemanticsAnnotationExclusive
		}
	case openAIShapedUsage:
		addCachedToInput = false
		semantics = core.UsageSemanticsShapeInclusive
	}
	u.InputSemantics = semantics
	if addCachedToInput {
		u.Input += u.CacheRead + u.CacheWrite
	}
	u.TotalTokens = u.Input + u.Output
	// 思考「摘要化」标记：本 provider 恒请求 display=summarized（adaptive 与
	// budget 两条路径都显式设置）——流里到的是摘要，而 usage 里记的是完整思考 token 量，
	// 产出这些 token 的时间不在「首个流式 token 之后」的窗口内。展示层的 tokens/s 分母
	// 据此改用整轮 durMs（否则分子含全部思考、分母只剩正文窗口 → 速度虚高数倍）。
	if sendThinking && u.Reasoning > 0 {
		u.ReasoningSummarized = true
	}

	// 1h 档写入量（token 维度，2026-09-23 补齐）：本轮断点全是 1h（per-request 已知）→
	// 写入量全归 1h 档；5m 档恒 0。成本侧仍走下面的 2× 修正（价格表形态统一后置）。
	if oneHourTTL && u.CacheWrite > 0 {
		u.CacheWrite1h = u.CacheWrite
	}

	// 归一化：Usage 从 provider 吐出即完整（Input 总输入口径已在上方归一 +
	// Cost 实际支出 µUSD）——下游零协议特判。价表经注册表（bridge 覆盖注入）。
	reg.FillUsageCost(req.Provider, req.Model, u)
	if oneHourTTL && u.CacheWrite > 0 {
		// 1h 档的缓存写入按 **2× 基础输入价** 计费（5m 档才是价表 `cache_write` 的 1.25×，
		// 上游数据只有这一个字段）。这里在**知道 TTL 的这一层**把写入段改成 2×：下游一律以
		// core.Usage.Cost 为准（bridge costUsd 优先用它、metrics 聚合也复用它），于是官方端点
		// （1h）与兼容端点 / 标注为 5m 的网关各自成本都准确，且无需维护第二份价表。
		if price, ok := reg.PriceFor(req.Provider, req.Model); ok && price.Input > 0 {
			write1h := int64(float64(u.CacheWrite) * 2 * price.Input)
			u.Cost.Total += write1h - u.Cost.CacheWrite
			u.Cost.CacheWrite = write1h
		}
	}
}

// openAIUsageShapeFields 兼容网关把 OpenAI / DeepSeek 形状的计数一并塞进 Anthropic
// usage 时会出现这些字段。出现即说明端点的字节是 OpenAI 语义：input_tokens 与
// prompt_tokens 同义（**含**缓存命中），不能再往上加 cache_read/cache_creation。
var openAIUsageShapeFields = []string{
	"prompt_tokens_details",    // OpenAI：cached_tokens 明细
	"prompt_cache_hit_tokens",  // DeepSeek：缓存命中
	"prompt_cache_miss_tokens", // DeepSeek：未命中
}

// looksOpenAIUsageShape 判定 usage 是否带 OpenAI/DeepSeek 形状字段（SDK 把未建模字段
// 收集进 JSON.ExtraFields）。注意：这些字段是「未建模」收集的，SDK 用 invalid 状态装它们
// —— Field.Valid() 恒为 false，只能用 Raw() 判存在（Raw()=="" 才是真的没有；
// Raw()=="null" 是显式 null，也不算存在）。
func looksOpenAIUsageShape(extra map[string]respjson.Field) bool {
	for _, k := range openAIUsageShapeFields {
		f, ok := extra[k]
		if !ok {
			continue
		}
		if raw := f.Raw(); raw != "" && raw != respjson.Null {
			return true
		}
	}
	return false
}
