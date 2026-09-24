package responses

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"

	"github.com/sashabaranov/go-openai"
)

// sessionIDCtxKey context 传递 session id（Stream 注入；transport 读它加 affinity 头）。
type sessionIDCtxKey struct{}

// sessionAffinityTransport 按 context 里的 session id 注入 affinity 头（对齐 pi：
// openai 格式 session_id + x-client-request-id；openrouter 格式 x-session-id）。
// 仅当 baseURL 含 openrouter.ai 用 openrouter 格式，否则 openai 格式。
type sessionAffinityTransport struct {
	base http.RoundTripper
	isOR bool
}

func (t *sessionAffinityTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if sid, ok := req.Context().Value(sessionIDCtxKey{}).(string); ok && sid != "" {
		if t.isOR {
			req.Header.Set("x-session-id", sid)
		} else {
			req.Header.Set("session_id", sid)
			req.Header.Set("x-client-request-id", sid)
		}
	}
	return t.base.RoundTrip(req)
}

// ResponsesProvider OpenAI Responses API（/v1/responses）。
type ResponsesProvider struct {
	client   *openai.Client
	baseURL  string
	apiKey   string
	registry *provider.Registry // 依赖注入
}

// NewResponsesProvider 依赖注入构造（deps.Registry / deps.HTTPClient 可空 = 默认）。
func NewResponsesProvider(deps provider.Dependencies, baseURL, apiKey string) *ResponsesProvider {
	cfg := openai.DefaultConfig(apiKey)
	if baseURL != "" {
		cfg.BaseURL = baseURL
	}
	// 无条件包 session affinity transport（对齐 pi：sessionId 存在时发 affinity 头）。
	// base = deps.HTTPClient.Transport（OAuth 注入链）或默认 transport。
	base := http.DefaultTransport
	if deps.HTTPClient != nil {
		if deps.HTTPClient.Transport != nil {
			base = deps.HTTPClient.Transport
		}
	}
	// session affinity transport 恒包（对齐 pi：sessionId 存在时发 affinity 头）。
	// base = deps.HTTPClient.Transport（OAuth 注入链）或默认 transport。
	inner := &sessionAffinityTransport{
		base: base,
		isOR: strings.Contains(strings.ToLower(baseURL), "openrouter.ai"),
	}
	// OpenCode Go 网关（opencode.ai/zen/go/v1）：网关准入要求 x-opencode-session +
	// 自有 UA → 包在 affinity 之外最外层（各写各的头，互不覆盖）。
	if provider.IsOpenCodeGateway(cfg.BaseURL) {
		cfg.HTTPClient = provider.OpenCodeGatewayClient(&http.Client{Transport: inner})
	} else {
		cfg.HTTPClient = &http.Client{Transport: inner}
	}
	reg := deps.Registry
	if reg == nil {
		reg = provider.NewRegistry()
	}
	return &ResponsesProvider{client: openai.NewClientWithConfig(cfg), baseURL: strings.TrimRight(cfg.BaseURL, "/"), apiKey: apiKey, registry: reg}
}

func (p *ResponsesProvider) Stream(ctx context.Context, req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
	startTime := time.Now()
	started := false

	cfg := req.Config
	if cfg == nil {
		cfg = &provider.RequestConfig{}
	}

	// session id → context（sessionAffinityTransport 读它注入 affinity 头）
	if req.SessionID != "" {
		ctx = context.WithValue(ctx, sessionIDCtxKey{}, req.SessionID)
	}
	// 同一会话 id 经 provider 包的 ctx 键下传（OpenCode Go 网关 transport 读它写
	// x-opencode-session；非网关端点无 transport 读取方，无副作用）。
	ctx = provider.WithOpenCodeSession(ctx, req.SessionID)

	// 注册表（零值构造兜底：测试 mock 不走构造函数）；成本折算与错误路径都用它。
	reg := p.registry
	if reg == nil {
		reg = provider.NewRegistry()
	}

	// 思考（注册表：默认关/档位 clamp/toggle-only）
	td := p.registry.ResolveThinking(req.Provider, req.Model, cfg)

	// 图片降级（注册表 SupportsImage）
	messages := p.registry.DowngradeImages(req.Provider, req.Model, req.Messages)

	// 系统提示 → instructions；翻译消息为 input items（developer role：仅 openai 官方推理模型）
	instructions, msgs := translateMessages(messages, p.registry.SupportsDeveloperRole(req.Provider, req.Model))

	apiReq := openai.CreateResponseRequest{
		Model:        req.Model,
		Input:        msgs,
		Instructions: instructions,
		Stream:       true,
		Store:        boolPtr(false), // 对齐 pi：恒显式 store:false（不存服务端历史）
	}
	// prompt cache（对齐 pi openai-responses.ts buildParams）：
	//   prompt_cache_key = sessionId（截 64 字符）——同一会话多轮共享缓存前缀；
	//   prompt_cache_retention = "24h" —— **官方端点**开长缓存（2026-09-21 决策，与
	//   Anthropic 侧 cache_control ttl=1h 同一动机：服务端默认短缓存（5~10 分钟）对本
	//   Harness 的工具批 / 后台任务 / 用户思考时长太短，跨轮必过期，等于每轮冷启动重算）。
	//   OpenAI 的长缓存**不加价**（命中仍按折扣价计费），唯一代价是缓存条目最长 24h、
	//   理论上更"陈旧"——对按前缀匹配的 prompt cache 无正确性影响（模型看到的始终是本
	//   请求发出的前缀）。pi 的 cacheRetention 默认 "short"、本仓此前也不发该字段，
	//   现按用户决策改为长缓存。
	//   兼容端点（第三方向 Responses API 代理）不发：该字段是 OpenAI 扩展，严格 schema
	//   的网关可能 400（与 Anthropic 侧 compat 不发 ttl 同一取舍）。
	if req.SessionID != "" {
		apiReq.PromptCacheKey = clampPromptCacheKey(req.SessionID)
	}
	if isOfficialOpenAI(p.baseURL) {
		apiReq.PromptCacheRetention = "24h"
	}
	// 输出上限（≥16 钳制，pi issue #6265）
	maxOutput := cfg.MaxTokens
	if maxOutput == 0 {
		maxOutput = p.registry.MaxTokensDefaultFor(req.Provider, req.Model)
	}
	if maxOutput < 16 {
		maxOutput = 16
	}
	apiReq.MaxOutputTokens = int(maxOutput)

	// 采样
	if cfg.Temperature != nil {
		apiReq.Temperature = float32Ptr(*cfg.Temperature)
	}
	if cfg.TopP != nil {
		apiReq.TopP = float32Ptr(*cfg.TopP)
	}

	// 思考：开启且非 toggle-only → effort 档位 + summary（对齐 pi：
	// reasoning={effort, summary:"auto"} + include:["reasoning.encrypted_content"]）。
	// encrypted_content 是加密思考签名，多轮同模型回传必需（reasoning item 回传）。
	// summary 而不是 generate_summary：后者是官方 OpenAPI 里标注 **deprecated**
	// （"use summary instead"）的旧字段，严格校验的网关直接 400：
	//   [unknown_parameter] Unknown parameter: 'reasoning.generate_summary'
	// （2026-09 OpenCode Go 网关实测；宽松网关如 api.aicodewith 两者都收，不能当依据）。
	if td.Enabled && !td.ToggleOnly {
		effort := p.registry.MapEffort(req.Provider, req.Model, td.Effort)
		apiReq.Reasoning = &openai.ResponseReasoning{
			Effort:  string(effort),
			Summary: "auto",
		}
		apiReq.Include = []openai.ResponseInclude{openai.ResponseIncludeReasoningEncryptedContent}
	} else if !td.Enabled && !td.ThinkingUnsupported {
		// 思考关闭：显式 effort=none（对齐 pi thinkingLevelMap.off ?? "none"）
		// ThinkingUnsupported（模型本身没有思考能力）不发：任何思考参数都无意义。
		apiReq.Reasoning = &openai.ResponseReasoning{Effort: string(provider.ReasoningEffortLevelNone)}
	}

	// 工具
	if tools := translateTools(req.Tools); len(tools) > 0 {
		apiReq.Tools = tools
	}

	stream, err := p.client.CreateResponseStream(ctx, apiReq)
	if err != nil {
		return classifyResponsesError(err)
	}
	defer stream.Close()

	var (
		requestId             string
		content, reasoning    strings.Builder
		finishReason          core.FinishReason
		usage                 core.Usage
		pending               = make(map[int64]*core.ToolCall)
		contentSeq, reasonSeq int64
		reasoningSignature    string // 响应侧捕获的 reasoning item JSON（多轮思考回传）
	)

	// ensureToolCall 取/建 output_index 对应的 pending 工具调用（created=true 表示新建，
	// 调用方据此发一次 LLMToolCallStartEvent）。id/name 只出现在带 item 的事件里
	// （output_item.added / output_item.done）——参数增量事件普遍没有 item，谁先带谁补全，
	// 不用空值覆盖已有值。id 取 call_id：下游 function_call_output 按 call_id 配对
	// （item.ID 是 fc_ 前缀的 item id，不能当 call_id 用）。
	ensureToolCall := func(idx int64, item *openai.ResponseOutputItem) (*core.ToolCall, bool) {
		call, ok := pending[idx]
		if !ok {
			call = &core.ToolCall{Index: idx, RequestId: requestId}
			pending[idx] = call
		}
		if item != nil {
			if item.CallID != "" {
				call.Id = item.CallID
			} else if call.Id == "" {
				call.Id = item.ID
			}
			if item.Name != "" {
				call.Name = item.Name
			}
		}
		return call, !ok
	}

	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// 失败尝试的已产生用量随错误上抛（折价后包 —— 同 openai/anthropic 口径；
			// responses 的 usage 只在 response.completed 回，中途断流通常拿不到）。
			reg.FillUsageCost(req.Provider, req.Model, &usage)
			return provider.WithPartialUsage(classifyResponsesError(err), usage)
		}

		if requestId == "" && ev.Response != nil && ev.Response.ID != "" {
			requestId = ev.Response.ID
		}
		if !started {
			started = true
			if err := handler(provider.LLMStartEvent{RequestId: requestId, Model: req.Model, Timestamp: startTime}); err != nil {
				return err
			}
		}

		switch ev.Type {
		case openai.ResponseStreamEventOutputTextDelta:
			content.WriteString(ev.Delta)
			if err := handler(provider.LLMContentDeltaEvent{RequestId: requestId, Delta: ev.Delta, Index: contentSeq}); err != nil {
				return err
			}
			contentSeq++
		case openai.ResponseStreamEventReasoningSummaryTextDelta, openai.ResponseStreamEventReasoningTextDelta:
			reasoning.WriteString(ev.Delta)
			if err := handler(provider.LLMReasoningDeltaEvent{RequestId: requestId, Delta: ev.Delta, Index: reasonSeq}); err != nil {
				return err
			}
			reasonSeq++
		case openai.ResponseStreamEventReasoningSummaryPartAdded:
			// 捕获 reasoning item（含 encrypted_content —— 多轮同模型思考回传必需）。
			// SDK 的 ResponseOutputContent 无 encrypted 字段 → 从 Raw 解析。
			// 对齐 pi：params.include=["reasoning.encrypted_content"] 时 API 返回
			// reasoning item JSON，回传时原样塞进 input。
			captureReasoningItem(ev.Raw, &reasoningSignature)
		case openai.ResponseStreamEventFunctionArgumentsDelta:
			// 参数增量：上游在 delta 字段里给片段（实测 api.aicodewith/官方形态），
			// 本事件的 item 通常缺省（只有 item_id/output_index）→ 名字要从
			// output_item.added 那条补（见下）。
			idx := int64(ev.OutputIndex)
			call, created := ensureToolCall(idx, ev.Item)
			if created {
				if err := handler(provider.LLMToolCallStartEvent{Id: call.Id, Index: idx, Name: call.Name}); err != nil {
					return err
				}
			}
			delta := ev.Delta
			if delta == "" {
				delta = ev.Arguments // 少数网关把增量放 arguments 字段
			}
			call.Arguments += delta
			if err := handler(provider.LLMToolCallDeltaEvent{Id: call.Id, Index: idx, Name: call.Name, Delta: delta}); err != nil {
				return err
			}
		case openai.ResponseStreamEventFunctionArgumentsDone:
			// 参数收尾：arguments 是**权威整串**。只补发「还没发过的那截」增量，
			// 不整串重发（下游按增量累计，重发会翻倍）。
			idx := int64(ev.OutputIndex)
			call, created := ensureToolCall(idx, ev.Item)
			if created {
				if err := handler(provider.LLMToolCallStartEvent{Id: call.Id, Index: idx, Name: call.Name}); err != nil {
					return err
				}
			}
			if tail := applyToolArguments(call, ev.Arguments); tail != "" {
				if err := handler(provider.LLMToolCallDeltaEvent{Id: call.Id, Index: idx, Name: call.Name, Delta: tail}); err != nil {
					return err
				}
			}
		case openai.ResponseStreamEventOutputItemAdded:
			captureReasoningItem(ev.Raw, &reasoningSignature) // reasoning item（加密思考）也走这条
			// function_call item 首次出现：id（fc_）/call_id/name 只在这里有 —— 不在此时建
			// pending 就拿不到名字（实测缺 name → 下一轮 400 "Missing required parameter:
			// 'input[1].name'"）。arguments 此刻为空，增量随后到。
			if ev.Item != nil && ev.Item.Type == "function_call" {
				idx := int64(ev.OutputIndex)
				if call, created := ensureToolCall(idx, ev.Item); created {
					if err := handler(provider.LLMToolCallStartEvent{Id: call.Id, Index: idx, Name: call.Name}); err != nil {
						return err
					}
				}
			}
		case openai.ResponseStreamEventOutputItemDone:
			captureReasoningItem(ev.Raw, &reasoningSignature) // 收尾态 reasoning item 覆盖 added 态
			// 收尾事件带完整 item（id/call_id/name/arguments 权威值）：非流式参数或缺事件的
			// 上游在这里补齐；也把 arguments 归一到权威值。
			if ev.Item != nil && ev.Item.Type == "function_call" {
				idx := int64(ev.OutputIndex)
				call, created := ensureToolCall(idx, ev.Item)
				if created {
					if err := handler(provider.LLMToolCallStartEvent{Id: call.Id, Index: idx, Name: call.Name}); err != nil {
						return err
					}
				}
				if tail := applyToolArguments(call, ev.Item.Arguments); tail != "" {
					if err := handler(provider.LLMToolCallDeltaEvent{Id: call.Id, Index: idx, Name: call.Name, Delta: tail}); err != nil {
						return err
					}
				}
			}
		case openai.ResponseStreamEventCompleted:
			if ev.Response != nil {
				if ev.Response.Usage != nil {
					usage = toCoreUsage(*ev.Response.Usage)
				}
				switch ev.Response.Status {
				case openai.ResponseStatusCompleted:
					finishReason = core.FinishReasonStop
				case openai.ResponseStatusIncomplete:
					finishReason = core.FinishReasonLength
				case openai.ResponseStatusFailed:
					finishReason = core.FinishReasonError
				}
			}
		case openai.ResponseStreamEventFailed:
			// 失败帧：已收到的用量一并上抛（上游可能已计费）。
			reg.FillUsageCost(req.Provider, req.Model, &usage)
			if ev.Error != nil {
				return provider.WithPartialUsage(
					provider.DetectContextExceeded(fmt.Errorf("responses error: %s: %s", ev.Error.Code, ev.Error.Message)), usage)
			}
			return provider.WithPartialUsage(fmt.Errorf("responses stream failed"), usage)
		}
	}

	toolCalls := make([]core.ToolCall, 0, len(pending))
	for _, call := range pending {
		toolCalls = append(toolCalls, *call)
	}
	sort.Slice(toolCalls, func(i, j int) bool { return toolCalls[i].Index < toolCalls[j].Index })

	// 归一化：Usage 从 provider 吐出即完整（Input 总输入口径 + Cost 实际支出 µUSD）
	// —— 下游零协议特判。价表经注册表（bridge 覆盖注入）。
	reg.FillUsageCost(req.Provider, req.Model, &usage)

	return handler(provider.LLMEndEvent{
		RequestId:          requestId,
		Model:              req.Model,
		Content:            content.String(),
		Reasoning:          reasoning.String(),
		ReasoningSignature: reasoningSignature, // responses：reasoning item JSON（多轮回传）
		ToolCalls:          toolCalls,
		FinishReason:       finishReason,
		Usage:              usage,
		Timestamp:          time.Now(),
	})
}

func float32Ptr(f float64) *float32 { v := float32(f); return &v }

func boolPtr(b bool) *bool { return &b }

// isOfficialOpenAI 判断 baseURL 是否为官方 OpenAI 端点（api.openai.com 含子域）。
// 用途：只在官方端点发 OpenAI 专属扩展字段（prompt_cache_retention 等）——第三方
// Responses 代理只实现基础 schema，扩展字段可能 400。与 provider/anthropic 的
// isOfficialAnthropic 同一判定形态。
func isOfficialOpenAI(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "api.openai.com" || strings.HasSuffix(h, ".api.openai.com")
}

// clampPromptCacheKey 截断 prompt cache key 到 64 字符（对齐 pi
// clampOpenAIPromptCacheKey：OPENAI_PROMPT_CACHE_KEY_MAX_LENGTH=64）。
func clampPromptCacheKey(key string) string {
	if len(key) <= 64 {
		return key
	}
	return key[:64]
}

func toCoreUsage(u openai.ResponseUsage) core.Usage {
	cu := core.Usage{
		Input:       int64(u.InputTokens),
		Output:      int64(u.OutputTokens),
		TotalTokens: int64(u.TotalTokens),
		// Responses 协议默认：input_tokens **已含** cached_tokens（自证字段）。
		InputSemantics: core.UsageSemanticsProtocolResponses,
	}
	if u.InputTokensDetails != nil {
		cu.CacheRead = int64(u.InputTokensDetails.CachedTokens)
		cu.CacheWrite = int64(u.InputTokensDetails.CacheWriteTokens)
	}
	if u.OutputTokensDetails != nil {
		cu.Reasoning = int64(u.OutputTokensDetails.ReasoningTokens)
	}
	return cu
}

// applyToolArguments 把「权威整串」工具参数并入 call，返回需要补发给下游的增量尾巴
// （无需补发时为空串）。function_call_arguments.done 与 output_item.done 带的是完整
// arguments，而增量事件可能已经把同一串发过一遍 —— 这里按前缀比对只补发尾巴，
// 避免下游把整串二次累计（LLMEnd 用 call.Arguments，事件流服务展示，两侧必须同口径）。
func applyToolArguments(call *core.ToolCall, arg string) string {
	if arg == "" {
		return ""
	}
	if call.Arguments == "" {
		call.Arguments = arg
		return arg
	}
	// arg 以已累计值为前缀 → 只补发尾巴；完全相等时尾巴为空（不补发、不改动）
	if tail := strings.TrimPrefix(arg, call.Arguments); tail != arg {
		call.Arguments = arg
		return tail
	}
	// 与已累计值不构成前缀关系（上游重发/修正）：以权威整串为准
	call.Arguments = arg
	return arg
}

// captureReasoningItem 从事件原文里抓 reasoning item（含 encrypted_content）→ 原样存进
// sig（core.Message.ReasoningSignature），多轮同模型原样回传。
//
// 为什么从 Raw 里抓：SDK 的 ResponseOutputItem/ResponseOutputContent 没有 encrypted_content
// 字段；且 item **只出现在带 item 的事件里** —— 实测 OpenCode Go 网关 /responses 把 reasoning
// item 放在 `response.output_item.added` / `output_item.done`（官方另有
// reasoning_summary_part.added 一脉），只认后者会永远抓不到（旧实现即如此，两轮实测
// sigLen 恒 0）。
//
// 存**原样 item**（不重构字段）：回放形态与上游给的一致，不赌哪些字段可省
// （实测原样回放 [reasoning, function_call, function_call_output] → 200）。
// added / done 各来一次时后者覆盖（done 是收尾态）；新一轮 reasoning item 直接取代旧的
// （ReasoningSignature 是单值字段）。
func captureReasoningItem(raw json.RawMessage, sig *string) {
	if len(raw) == 0 {
		return
	}
	var ev struct {
		Item json.RawMessage `json:"item"`
	}
	if json.Unmarshal(raw, &ev) != nil || len(ev.Item) == 0 {
		return
	}
	var probe struct {
		Type      string `json:"type"`
		Encrypted string `json:"encrypted_content"`
	}
	if json.Unmarshal(ev.Item, &probe) != nil || probe.Type != "reasoning" || probe.Encrypted == "" {
		return
	}
	*sig = string(ev.Item)
}
