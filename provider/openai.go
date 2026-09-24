package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/seven7628/hai-harness/core"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/sashabaranov/go-openai"
)

// OpenAIProvider 基于 sashabaranov/go-openai，兼容 OpenAI / DeepSeek / vLLM 等
// 实现了 OpenAI Chat Completions 协议的厂商（baseURL 指向 /v1 端点）。
type OpenAIProvider struct {
	client       *openai.Client
	capabilities OpenAICompatCapabilities
	baseURL      string
	apiKey       string
	httpClient   *http.Client
	registry     *Registry // 依赖注入：模型元数据（nil = 内置默认）
}

// NewOpenAIProvider 创建完整 OpenAI-compatible provider；apiKey 为空时库会读取
// OPENAI_API_KEY 环境变量。完整能力配置保持既有 OpenAI / DeepSeek 行为。
func NewOpenAIProvider(baseURL, apiKey string) *OpenAIProvider {
	return NewOpenAIProviderWithCapabilities(baseURL, apiKey, FullOpenAICompatCapabilities())
}

// NewOpenAIProviderWithCapabilities 创建 OpenAI-compatible provider，并按端点能力
// 选择请求可选字段。Minimal 能让只保证 model/messages/stream 的兼容网关绕过
// go-openai 对 gpt-5 等 OpenAI 原生模型名的本地参数校验。
func NewOpenAIProviderWithCapabilities(baseURL, apiKey string, capabilities OpenAICompatCapabilities) *OpenAIProvider {
	return newOpenAIProviderWithClient(baseURL, apiKey, capabilities, nil, nil)
}

// NewOpenAIProviderWithDeps 依赖注入版构造（bridge/测试用；deps.Registry/HTTPClient 可空 = 默认）。
func NewOpenAIProviderWithDeps(deps Dependencies, baseURL, apiKey string, capabilities OpenAICompatCapabilities) *OpenAIProvider {
	return newOpenAIProviderWithClient(baseURL, apiKey, capabilities, deps.Registry, deps.HTTPClient)
}

// newOpenAIProviderWithClient 两个公开构造函数的共同实现。
//
// 两个发送通道各自使用的 client 不同：
//   - SDK 通道（CreateChatCompletionStream）只认 openai.ClientConfig.HTTPClient；
//   - minimal 通道（createMinimalChatCompletionStream 自建请求）只认 p.httpClient。
//
// 注入 client（bridge 的 llmHTTPClient 首包超时 / OAuth transport）历史上只作用于
// minimal 通道 —— SDK 通道恒用 SDK 默认 client。此差异对普通端点无影响（两者都是
// 默认 transport），但 OpenCode Go 网关要求两个通道都带 x-opencode-session，
// 故网关端点把两通道统一到一个包装 client；非网关端点维持既有分工不变。
func newOpenAIProviderWithClient(baseURL, apiKey string, capabilities OpenAICompatCapabilities, reg *Registry, injected *http.Client) *OpenAIProvider {
	cfg := openai.DefaultConfig(apiKey)
	if baseURL != "" {
		cfg.BaseURL = baseURL
	}
	sdkClient := http.DefaultClient
	if c, ok := cfg.HTTPClient.(*http.Client); ok && c != nil {
		sdkClient = c
	}
	minimalClient := injected
	if minimalClient == nil {
		minimalClient = sdkClient
	}
	if IsOpenCodeGateway(cfg.BaseURL) {
		// 网关准入：协议头注入。统一一个包装 client（同一兜底会话 id），两通道共用。
		base := injected
		if base == nil {
			base = sdkClient
		}
		sdkClient = OpenCodeGatewayClient(base)
		minimalClient = sdkClient
		cfg.HTTPClient = sdkClient
	}
	if reg == nil {
		reg = NewRegistry()
	}
	return &OpenAIProvider{client: openai.NewClientWithConfig(cfg), capabilities: capabilities, baseURL: strings.TrimRight(cfg.BaseURL, "/"), apiKey: apiKey, httpClient: minimalClient, registry: reg}
}

// NewDeepSeekProvider 创建 DeepSeek provider（OpenAI 兼容协议，思考模式默认开启，
// 经 reasoning_effort 控制强度，思考内容经 reasoning_content 流式返回）。
func NewDeepSeekProvider(apiKey string) *OpenAIProvider {
	return NewOpenAIProvider("https://api.deepseek.com/v1", apiKey)
}

// NewKimiProvider 创建 Kimi（月之暗面 / Moonshot）provider：OpenAI 兼容 chat/completions，
// 默认接入境内端点 api.moonshot.cn/v1（thinking 模型的思考内容经 reasoning_content 流式返回；
// 若用境外端点可在设置里改 base_url 为 https://api.moonshot.ai/v1）。
func NewKimiProvider(apiKey string) *OpenAIProvider {
	return NewOpenAIProvider("https://api.moonshot.cn/v1", apiKey)
}

// NewZhipuProvider 创建智谱 Zhipu（ZAI / BigModel）provider：OpenAI 兼容 chat/completions，
// 版本根为 /api/paas/v4（非 /v1），BasePath 指向 /v4 以保证 /chat/completions 落在正确路径。
func NewZhipuProvider(apiKey string) *OpenAIProvider {
	return NewOpenAIProviderWithCapabilities("https://open.bigmodel.cn/api/paas/v4", apiKey, ZhipuOpenAICompatCapabilities())
}

// chatRequestWithExtra 自定义请求结构：内嵌 SDK ChatCompletionRequest 标准字段，
// ExtraBody 提供厂商扩展字段（thinkingFormat 的 thinking/enable_thinking/reasoning 等）。
// MarshalJSON 先输出 SDK 标准 JSON，再把扩展字段平铺合并（扩展键覆盖同名字段，对齐 pi
// Object.assign 语义）。
type chatRequestWithExtra struct {
	openai.ChatCompletionRequest
	ExtraBody map[string]any `json:"-"`
}

func (r chatRequestWithExtra) MarshalJSON() ([]byte, error) {
	base, err := json.Marshal(r.ChatCompletionRequest)
	if err != nil {
		return nil, err
	}
	if len(r.ExtraBody) == 0 {
		return base, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(base, &obj); err != nil {
		return nil, err
	}
	for k, v := range r.ExtraBody {
		obj[k] = v
	}
	return json.Marshal(obj)
}

// createMinimalChatCompletionStream sends the smallest useful chat-completions request
// directly through net/http. The SDK's public method validates gpt-5* names as if they
// were OpenAI-native models; a custom gateway may expose those names while accepting the
// compatibility shape, so this path skips that client-side validator and parses the same
// SSE response schema locally.
// extraBody 非空时一并合并进请求体（厂商 thinking 扩展字段）。
func (p *OpenAIProvider) createMinimalChatCompletionStream(ctx context.Context, request openai.ChatCompletionRequest, extraBody map[string]any) (*minimalChatStream, error) {
	request.Stream = true
	body, err := json.Marshal(chatRequestWithExtra{ChatCompletionRequest: request, ExtraBody: extraBody})
	if err != nil {
		return nil, err
	}
	base := strings.TrimRight(p.baseURL, "/")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	path := p.capabilities.BasePath
	if path == "" {
		path = "/v1"
	}
	path = "/" + strings.Trim(path, "/")
	if strings.HasSuffix(base, path) {
		path = ""
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Connection", "keep-alive")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	client := p.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusBadRequest {
		defer resp.Body.Close()
		b, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return nil, readErr
		}
		return nil, classifyMinimalHTTPError(resp, b)
	}
	return &minimalChatStream{resp: resp, reader: bufio.NewReader(resp.Body)}, nil
}

type minimalChatStream struct {
	resp   *http.Response
	reader *bufio.Reader
	done   bool
}

func (s *minimalChatStream) Close() error { return s.resp.Body.Close() }

func (s *minimalChatStream) Recv() (openai.ChatCompletionStreamResponse, error) {
	var out openai.ChatCompletionStreamResponse
	if s.done {
		return out, io.EOF
	}
	for {
		line, err := s.reader.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return out, err
		}
		// 流末尾：ReadBytes 会把**已读到但没有尾换行的最后一行**一并返回（err=io.EOF +
		// len(line)>0）。这一行往往正是携带 finish_reason / usage 的收尾事件（llm_end 的
		// 全部依据）—— 旧实现直接 return err 把它丢掉，表现为「流看似正常结束，本轮却
		// 没有收尾信息（finish_reason 缺失 / usage 为 0）」。此处先解析残留行，EOF 留给
		// 下一次 Recv（done 标记），对端行为无损：有尾换行的流走的分支完全相同。
		last := errors.Is(err, io.EOF)
		if last {
			s.done = true
		}
		if len(line) == 0 {
			if last {
				return out, io.EOF
			}
			continue
		}
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 || !bytes.HasPrefix(trimmed, []byte("data:")) {
			if last {
				return out, io.EOF // 末尾残留不是事件（空行等）→ 流到此结束
			}
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
		if bytes.Equal(payload, []byte("[DONE]")) {
			s.done = true
			return out, io.EOF
		}
		if bytes.HasPrefix(payload, []byte("{\"error\":")) {
			return out, classifyMinimalHTTPError(s.resp, payload)
		}
		if uerr := json.Unmarshal(payload, &out); uerr != nil {
			return out, uerr
		}
		return out, nil
	}
}

func classifyMinimalHTTPError(resp *http.Response, body []byte) error {
	var wire struct {
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	makeErr := func() error {
		if json.Unmarshal(body, &wire) == nil {
			msg := wire.Message
			typ := wire.Code
			if wire.Error != nil {
				msg = wire.Error.Message
				typ = wire.Error.Type
			}
			if msg != "" {
				return fmt.Errorf("error, status code: %d, status: %s, type: %s, message: %s", resp.StatusCode, resp.Status, typ, msg)
			}
		}
		return fmt.Errorf("error, status code: %d, status: %s, body: %s", resp.StatusCode, resp.Status, strings.TrimSpace(string(body)))
	}
	err := makeErr()
	if detected := DetectContextExceeded(err); detected != err {
		// 上下文超限（可能是 200 + 流中 error frame 的网关形态）：不可重试 + 语义化错误
		return detected
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return &RateLimitError{StatusCode: resp.StatusCode, Err: err}
	}
	// 流中段 200 + error frame 的限流：状态码拿不到（恒 200），按文本识别为 RateLimitError
	// （宽退避重试）；仅对 200 响应做（非 200 时上面状态码分支已覆盖）。
	if resp.StatusCode == http.StatusOK && IsRateLimitText(err.Error()) {
		return &RateLimitError{StatusCode: 0, Err: err}
	}
	if resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusTooEarly || resp.StatusCode >= http.StatusInternalServerError {
		return err
	}
	return MarkPermanent(err)
}

// Provider 设置面板「拉取模型列表」/「测试连接」；ctx 供调用方限时（测试连接带超时
// 防挂死）；baseURL 空回退默认 OpenAI。
func ListModels(ctx context.Context, baseURL, apiKey string) ([]string, error) {
	cfg := openai.DefaultConfig(apiKey)
	if baseURL != "" {
		cfg.BaseURL = baseURL
	}
	// OpenCode Go 网关：与对话端点同口径自报身份（见 listModelsWithClient 注释）。
	if IsOpenCodeGateway(cfg.BaseURL) {
		if c, ok := cfg.HTTPClient.(*http.Client); ok {
			cfg.HTTPClient = OpenCodeGatewayClient(c)
		} else {
			cfg.HTTPClient = OpenCodeGatewayClient(nil)
		}
	}
	c := openai.NewClientWithConfig(cfg)
	ls, err := c.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ls.Models))
	for _, m := range ls.Models {
		out = append(out, m.ID)
	}
	sort.Strings(out)
	return out, nil
}

// ListModelsWithCapabilities 列出兼容端点模型。Minimal 模式绕过 SDK 的默认路径/模型
// 校验，仅用于需要自定义版本根的网关；普通模式保持 go-openai 实现。
func ListModelsWithCapabilities(ctx context.Context, baseURL, apiKey string, capabilities OpenAICompatCapabilities) ([]string, error) {
	return listModelsWithClient(ctx, http.DefaultClient, baseURL, apiKey, capabilities)
}

// ListModelsWithClient 同 ListModelsWithCapabilities，但使用自定义 HTTP client
// （OAuth 模式传注入 Bearer 的 transport，见 desktop/bridge OAuth 登录链路）。
func ListModelsWithClient(ctx context.Context, client *http.Client, baseURL string, capabilities OpenAICompatCapabilities) ([]string, error) {
	return listModelsWithClient(ctx, client, baseURL, "", capabilities)
}

func listModelsWithClient(ctx context.Context, client *http.Client, baseURL, apiKey string, capabilities OpenAICompatCapabilities) ([]string, error) {
	// OpenCode Go 网关：/models 实测不校验 x-opencode-session（与对话端点不同），
	// 但官网要求客户端自报身份、全部流量可归因，故一并包装（其他厂商零影响）。
	if IsOpenCodeGateway(baseURL) {
		client = OpenCodeGatewayClient(client)
	}
	if capabilities.BasePath == "" {
		capabilities.BasePath = "/v1"
	}
	if !capabilities.Minimal && strings.HasSuffix(strings.TrimRight(baseURL, "/"), "/"+strings.Trim(capabilities.BasePath, "/")) {
		return ListModels(ctx, baseURL, apiKey)
	}
	base := strings.TrimRight(baseURL, "/")
	path := capabilities.BasePath
	if path == "" {
		path = "/v1"
	}
	path = "/" + strings.Trim(path, "/")
	if strings.HasSuffix(base, path) {
		path = ""
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusBadRequest {
		return nil, classifyMinimalHTTPError(resp, body)
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (p *OpenAIProvider) Stream(ctx context.Context, req *StreamRequest, handler func(e StreamEvent) error) error {
	// LLMStart 延迟到首个 chunk 后发出（拿到厂商 requestId——事件自包含原则：
	// LLMStart 需与 LLMEnd/chunk 字段级关联；原实现 Stream 入口即发、无
	// requestId，trace 审计 2026-08-10 发现）。Timestamp 保留真实发起时刻，
	// 首字耗时（首个 chunk - LLMStart）不受影响。建连失败/空流时无 LLMStart。
	startTime := time.Now()
	started := false

	cfg := req.Config
	if cfg == nil {
		cfg = &RequestConfig{}
	}

	// OpenCode Go 网关：会话 id 经 ctx 下传到 transport（写 x-opencode-session）。
	// 非网关端点 ctx 不变；空 SessionID 由 transport 兜底 id 接管。
	ctx = WithOpenCodeSession(ctx, req.SessionID)

	// 思考开关与强度（统一语义 → 模型能力）：
	// ResolveThinking 按注册表：非推理模型默认关、档位 clamp 到模型支持范围、toggle-only 只发开关。
	reg := p.registry
	if reg == nil {
		reg = NewRegistry() // 零值构造兜底（测试 mock 不走构造函数）
	}
	td := reg.ResolveThinking(req.Provider, req.Model, cfg)

	coreMsgs := reg.DowngradeImages(req.Provider, req.Model, req.Messages)
	msgs, err := translateMessages(coreMsgs,
		reg.ShouldRetainReasoning(req.Provider, req.Model, cfg),
		reg.SupportsDeveloperRole(req.Provider, req.Model),
		reg.RequiresReasoningContent(req.Provider, req.Model),
		reg.RequiresAssistantAfterToolResult(req.Provider, req.Model),
		reg.RequiresToolResultName(req.Provider, req.Model),
		reg.RequiresThinkingAsText(req.Provider, req.Model))
	if err != nil {
		return err
	}

	apiReq := openai.ChatCompletionRequest{
		Model:    req.Model,
		Messages: msgs,
	}
	if p.capabilities.supportsTools() {
		apiReq.Tools = translateTools(req.Tools)
	}
	// 思考参数格式：compat 显式配置优先 → 注册表模型数据（pi thinkingFormat 对齐）。
	// 用 Resolve（含跨 provider 按名 + 名称族）：自建 provider 下的已知模型名 / 网关新版本号
	// 也走同一格式分支（否则 deepseek 系模型只会发 reasoning_effort、丢掉 thinking 开关）。
	thinkingFormat := p.capabilities.ThinkingFormat
	if thinkingFormat == "" {
		if info, ok := reg.Resolve(req.Provider, req.Model); ok {
			thinkingFormat = info.ThinkingFormat
		}
	}
	// 厂商扩展字段（thinkingFormat 非 openai 时承载 thinking/enable_thinking/reasoning 等）
	var extraBody map[string]any

	// 思考开关/档位 → 按厂商格式发送（对齐 pi openai-completions.ts thinkingFormat 分支）：
	//   - openai（默认）  : reasoning_effort（关 → "none"）
	//   - deepseek        : thinking:{type:"enabled"|"disabled"} + reasoning_effort（支持时）
	//   - zai             : thinking:{type} + reasoning_effort（支持时）
	//   - qwen            : enable_thinking: bool + reasoning_effort（支持时）
	//   - openrouter      : reasoning:{effort}（关 → {effort:"none"}）
	//   - together        : reasoning:{enabled} + reasoning_effort（支持时）
	//   - ant-ling        : reasoning:{effort}（仅映射档位非 null 时发）
	//   - string-thinking : thinking: string（档位映射文本；关 → off 映射）
	//
	// 前置闸门：非推理模型（td.ThinkingUnsupported）整个思考块与档位都不发 —— 模型没有
	// 思考能力，任何思考参数都无意义（且严格端点可能对不支持的模型 400）。
	if td.ThinkingUnsupported {
		// 什么都不发：extraBody 保持 nil、apiReq.ReasoningEffort 保持空。
	} else {
		switch thinkingFormat {
		case "deepseek", "zai":
			extraBody = map[string]any{"thinking": map[string]any{"type": "disabled"}}
			if td.Enabled {
				extraBody["thinking"] = map[string]any{"type": "enabled"}
			}
			if p.capabilities.supportsReasoningEffort() && !td.ToggleOnly {
				if td.Enabled {
					effort := reg.MapEffort(req.Provider, req.Model, td.Effort)
					apiReq.ReasoningEffort = string(effort)
				} else {
					apiReq.ReasoningEffort = string(ReasoningEffortLevelNone)
				}
			}
			// z.ai 工具流式（对齐 pi zaiToolStream）：工具参数经 tool_stream 流式返回
			if thinkingFormat == "zai" && reg.ZaiToolStream(req.Provider, req.Model) && len(req.Tools) > 0 {
				extraBody["tool_stream"] = true
			}
		case "qwen":
			extraBody = map[string]any{"enable_thinking": td.Enabled}
			if p.capabilities.supportsReasoningEffort() && !td.ToggleOnly && td.Enabled {
				effort := reg.MapEffort(req.Provider, req.Model, td.Effort)
				apiReq.ReasoningEffort = string(effort)
			}
		case "openrouter":
			// toggle-only 模型（小米 MiMo 等：只有开/关、没有档位）走 reasoning:{enabled}：
			// OpenRouter 的 /api/v1/models 对这类模型只给 {"mandatory": false}（无
			// supported_efforts），发 effort 无意义；有档位声明的模型才发 reasoning:{effort}。
			// 关思考时同理（enabled:false 而不是 effort:"none"）—— 模型没有档位可关。
			toggleOnly := td.ToggleOnly
			if !toggleOnly {
				if info, ok := reg.Resolve(req.Provider, req.Model); ok {
					toggleOnly = info.ThinkingToggleOnly
				}
			}
			if toggleOnly {
				extraBody = map[string]any{"reasoning": map[string]any{"enabled": td.Enabled}}
			} else if td.Enabled {
				effort := reg.MapEffort(req.Provider, req.Model, td.Effort)
				extraBody = map[string]any{"reasoning": map[string]any{"effort": string(effort)}}
			} else {
				extraBody = map[string]any{"reasoning": map[string]any{"effort": "none"}}
			}
		case "together":
			extraBody = map[string]any{"reasoning": map[string]any{"enabled": td.Enabled}}
			if p.capabilities.supportsReasoningEffort() && !td.ToggleOnly && td.Enabled {
				effort := reg.MapEffort(req.Provider, req.Model, td.Effort)
				apiReq.ReasoningEffort = string(effort)
			}
		case "ant-ling":
			// ant-ling：仅当映射档位非 null（ThinkingLevels 命中）时发 reasoning:{effort}
			if td.Enabled {
				if info, ok := reg.Resolve(req.Provider, req.Model); ok {
					if mapped, ok := info.ThinkingLevels[td.Effort]; ok && mapped != "" {
						extraBody = map[string]any{"reasoning": map[string]any{"effort": mapped}}
					}
				}
			}
		case "string-thinking":
			if td.Enabled {
				effort := reg.MapEffort(req.Provider, req.Model, td.Effort)
				extraBody = map[string]any{"thinking": string(effort)}
			} else if info, ok := reg.Resolve(req.Provider, req.Model); ok {
				if off, ok := info.ThinkingLevels[ReasoningEffortLevelNone]; ok && off != "" {
					extraBody = map[string]any{"thinking": off}
				}
			}
		case "baseten", "chat-template":
			// baseten/chat-template 端点：thinking 开关经 chat_template_args/chat_template_kwargs
			// 注入（对齐 pi chatTemplateArgs —— { "$var": "thinking.enabled" } 模板变量）。
			// 模板变量：thinking.enabled（开关）、thinking.effort（档位）、thinking.budget（预算）。
			templateArgs := reg.ChatTemplateArgs(req.Provider, req.Model)
			args := map[string]any{}
			for k, v := range templateArgs {
				switch v {
				case "thinking.enabled":
					args[k] = td.Enabled
				case "thinking.effort":
					if td.Enabled && !td.ToggleOnly {
						args[k] = string(reg.MapEffort(req.Provider, req.Model, td.Effort))
					}
				case "thinking.budget":
					if td.Enabled {
						args[k] = ThinkingBudgetForLevel(td.Effort)
					}
				}
			}
			if len(args) > 0 {
				if thinkingFormat == "baseten" {
					extraBody = map[string]any{"chat_template_args": args}
				} else {
					extraBody = map[string]any{"chat_template_kwargs": args}
				}
			}
			// baseten 也支持 reasoning_effort（compat 支持时）
			if thinkingFormat == "baseten" && p.capabilities.supportsReasoningEffort() && !td.ToggleOnly {
				if td.Enabled {
					effort := reg.MapEffort(req.Provider, req.Model, td.Effort)
					apiReq.ReasoningEffort = string(effort)
				} else {
					apiReq.ReasoningEffort = string(ReasoningEffortLevelNone)
				}
			}
		default: // openai（默认）
			if !td.Enabled {
				if p.capabilities.supportsReasoningEffort() {
					apiReq.ReasoningEffort = string(ReasoningEffortLevelNone)
				}
			} else if !td.ToggleOnly {
				if p.capabilities.supportsReasoningEffort() {
					effort := reg.MapEffort(req.Provider, req.Model, td.Effort)
					apiReq.ReasoningEffort = string(effort)
				}
			}
		}
	}
	if p.capabilities.supportsMaxCompletionTokens() {
		maxTokens := cfg.MaxTokens
		if maxTokens == 0 {
			maxTokens = reg.MaxTokensDefaultFor(req.Provider, req.Model)
		}
		if reg.MaxTokensFieldFor(req.Provider, req.Model) == "max_tokens" {
			apiReq.MaxTokens = int(maxTokens)
		} else {
			apiReq.MaxCompletionTokens = int(maxTokens)
		}
	}
	if p.capabilities.supportsStreamUsage() {
		apiReq.StreamOptions = &openai.StreamOptions{IncludeUsage: cfg.ShouldIncludeUsage()}
	}
	// 采样参数透传：显式指定才带上（nil = 厂商默认）。采样默认值由宿主（bridge）
	// 在构造 loop 时决定（DeepSeek top_p=0.95 / temperature=1.0），provider 不内置。
	if cfg.Temperature != nil {
		apiReq.Temperature = float32(*cfg.Temperature)
	}
	if cfg.TopP != nil {
		apiReq.TopP = float32(*cfg.TopP)
	}

	type chatCompletionStream interface {
		Recv() (openai.ChatCompletionStreamResponse, error)
		Close() error
	}
	var stream chatCompletionStream
	if p.capabilities.Minimal || len(extraBody) > 0 {
		// Minimal 或带厂商扩展字段（thinkingFormat 的 thinking/enable_thinking/reasoning 等）：
		// 走自定义 marshal 通道（SDK 结构体没有这些扩展字段，json 序列化会丢弃）。
		stream, err = p.createMinimalChatCompletionStream(ctx, apiReq, extraBody)
	} else {
		stream, err = p.client.CreateChatCompletionStream(ctx, apiReq)
	}
	if err != nil {
		return classifyAPIError(err)
	}
	defer stream.Close()

	var (
		requestId          string // 请求 ID（厂商响应 id，首个 chunk 起）
		content, reasoning strings.Builder
		finishReason       core.FinishReason
		usage              core.Usage
		pending            = make(map[int]*core.ToolCall)
		// 流式块序号（content/reasoning 各自从 0 自增）：宿主流式渲染与审计
		// 用（丢块检测、首字耗时定位）。曾取 choices[].index —— SDK 恒单 choice
		// 恒 0，信息量为零（真实环境验证 2026-08-10 trace 审计发现）。
		contentSeq, reasoningSeq int64
	)

	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// 失败尝试的已产生用量随错误上抛（上游按已生成 token 计费）：先折价再包 ——
			// 注意 chat-completions 的 usage 只在收尾帧回，中途断流通常拿不到（记录不到 ≠ 没花钱，
			// 但宁可如实为零也不伪造估算；Anthropic 协议 message_start 带 input，能拿到）。
			reg.FillUsageCost(req.Provider, req.Model, &usage)
			return WithPartialUsage(classifyAPIError(err), usage)
		}

		if requestId == "" && chunk.ID != "" {
			requestId = chunk.ID
		}
		if !started {
			started = true
			if err := handler(LLMStartEvent{
				RequestId: requestId,
				Model:     req.Model,
				Timestamp: startTime,
			}); err != nil {
				return err
			}
		}
		if chunk.Usage != nil {
			usage = toCoreUsage(*chunk.Usage)
		}
		for _, choice := range chunk.Choices {
			delta := choice.Delta

			if delta.ReasoningContent != "" {
				reasoning.WriteString(delta.ReasoningContent)
				if err := handler(LLMReasoningDeltaEvent{
					RequestId: requestId,
					Delta:     delta.ReasoningContent,
					Index:     reasoningSeq,
				}); err != nil {
					return err
				}
				reasoningSeq++
			}
			if delta.Content != "" {
				content.WriteString(delta.Content)
				if err := handler(LLMContentDeltaEvent{
					RequestId: requestId,
					Delta:     delta.Content,
					Index:     contentSeq,
				}); err != nil {
					return err
				}
				contentSeq++
			}
			for _, tc := range delta.ToolCalls {
				index := 0
				if tc.Index != nil {
					index = *tc.Index
				}
				call, ok := pending[index]
				if !ok {
					call = &core.ToolCall{Index: int64(index), RequestId: requestId}
					pending[index] = call
					if err := handler(LLMToolCallStartEvent{
						Id:    tc.ID,
						Index: int64(index),
						Name:  tc.Function.Name,
					}); err != nil {
						return err
					}
				}
				if tc.ID != "" {
					call.Id = tc.ID
				}
				if tc.Function.Name != "" {
					call.Name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					call.Arguments += tc.Function.Arguments
					if err := handler(LLMToolCallDeltaEvent{
						Id:    call.Id,
						Index: call.Index,
						Name:  call.Name,
						Delta: tc.Function.Arguments,
					}); err != nil {
						return err
					}
				}
			}
			if choice.FinishReason != "" {
				finishReason = mapFinishReason(choice.FinishReason)
			}
		}
	}

	// 按 Index 排序输出完整工具调用
	toolCalls := make([]core.ToolCall, 0, len(pending))
	for _, call := range pending {
		toolCalls = append(toolCalls, *call)
	}
	sort.Slice(toolCalls, func(i, j int) bool { return toolCalls[i].Index < toolCalls[j].Index })

	// 归一化：LLMEndEvent.Usage 从 provider 吐出即完整（Input 总输入口径 +
	// Cost 实际支出 µUSD）——下游零协议特判。价表经注册表（bridge 覆盖注入）。
	reg.FillUsageCost(req.Provider, req.Model, &usage)

	return handler(LLMEndEvent{
		RequestId:    requestId,
		Model:        req.Model,
		Content:      content.String(),
		Reasoning:    reasoning.String(),
		ToolCalls:    toolCalls,
		FinishReason: finishReason,
		Usage:        usage,
		Timestamp:    time.Now(),
	})
}

// translateMessages 把抽象消息翻译为 SDK 消息，内容块数组支持文本/图片混排；
// assistant 消息回传 ToolCalls（协议必需），ReasoningContent 按策略：
//   - 保留（DeepSeek/Kimi 等推理模型工具调用必须回传，否则 API 400）；
//   - 丢弃（OpenAI 系无需回传 —— 省 token，推理内容通常很长）。
//
// 兼容开关（对齐 pi openai-completions.ts convertMessages）：
//   - useDeveloperRole：推理模型 system → developer role
//   - requireReasoningContent：deepseek 系要求 assistant 恒带 reasoning_content（空串也行）
//   - requireAssistantAfterTool：tool_result 后直接 user → 插入空 assistant 桥接
//   - requireToolResultName：tool 消息带 name 字段
//   - requireThinkingAsText：thinking 块转纯文本（不带标签）
//
// 所有文本过 SanitizeSurrogates（未配对代理项移除，防 400）。
func translateMessages(messages []core.Message, retainReasoning bool, useDeveloperRole bool, requireReasoningContent bool, requireAssistantAfterTool bool, requireToolResultName bool, requireThinkingAsText bool) ([]openai.ChatCompletionMessage, error) {
	out := make([]openai.ChatCompletionMessage, 0, len(messages))
	lastRoleWasTool := false
	// pendingImages 缓冲 tool 消息的图片块：OpenAI 系 tool 角色 content 只能是字符串，
	// 图片无处安放 → 等 tool 段结束后合并成一条 user 消息（多工具批只发一条，
	// 避免在两条 tool 结果之间插入 user 破坏 tool_call 配对——严格端点会 400）。
	var pendingImages []core.Content
	flushImages := func() {
		if len(pendingImages) == 0 {
			return
		}
		imgs := pendingImages
		pendingImages = nil
		// 上一条已是 user（源消息紧随 user，或多次 flush）→ 并入，避免连续 user。
		if n := len(out); n > 0 && out[n-1].Role == string(openai.ChatMessageRoleUser) {
			appendImagesAsUserContent(&out[n-1], imgs)
			return
		}
		if requireAssistantAfterTool && lastRoleWasTool {
			out = append(out, openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleAssistant,
				Content: "I have processed the tool results.",
			})
			lastRoleWasTool = false
		}
		msg := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser}
		appendImagesAsUserContent(&msg, imgs)
		out = append(out, msg)
		lastRoleWasTool = false
	}
	for _, m := range messages {
		if m.Role != core.Tool {
			flushImages() // 工具段结束：图片先落地（下一轮/下一段之前模型必须看到）
		}
		// tool_result 后直接 user（无 assistant 桥接）→ 插入空 assistant（部分端点要求）
		if requireAssistantAfterTool && lastRoleWasTool && m.Role == core.User {
			out = append(out, openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleAssistant,
				Content: "I have processed the tool results.",
			})
		}
		lastRoleWasTool = m.Role == core.Tool
		// 收集 tool 消息的图片块（downgrade 已在翻译层入口把非视觉模型的图片换成
		// 占位文本 —— 此处剩下的图片块必然是「模型支持图片」的，可安全送出）。
		if m.Role == core.Tool {
			for _, c := range m.Content {
				if c.Type == core.ContentTypeImage && c.Content != "" {
					pendingImages = append(pendingImages, c)
				}
			}
		}

		// 空 assistant 消息过滤（2026-08-17 opencode 网关 400 实证）：无可见文本且
		// 无工具调用的 assistant 消息（纯思考 / 全空文本轮）序列化后既无 content 也
		// 无 tool_calls —— 上游严格 schema 校验 "Invalid assistant message: content or
		// tool_calls must be set" 直接 400。这类消息对模型零信息量，剔除即可
		//（两侧不会夹 tool 回执，不破坏 user/assistant/tool 交错）。
		if isEmptyAssistantMessage(m) {
			continue
		}
		if err := m.Validate(); err != nil {
			return nil, fmt.Errorf("invalid message: %w", err)
		}
		reasoning := m.Reasoning
		if !retainReasoning {
			reasoning = "" // 丢弃推理内容（节省回传 token）
		}
		role := string(m.Role)
		if m.Role == core.System && useDeveloperRole {
			role = string(openai.ChatMessageRoleDeveloper)
		}
		msg := openai.ChatCompletionMessage{
			Role:             role,
			ToolCallID:       m.ToolCallId,
			ReasoningContent: SanitizeSurrogates(reasoning),
		}
		// deepseek 系端点要求 assistant 消息恒带 reasoning_content（思考开启时缺失 400）：
		// 对齐 pi requiresReasoningContentOnAssistantMessages && model.reasoning →
		// reasoning_content = ""。go-openai 的 omitempty 会丢弃空串 → 用空格占位
		//（DeepSeek 官方文档 reasoning_content 允许空字符串；空格等价且不改变语义）。
		// S2 修复：占位在 thinking-as-text 分支**之后**设置（该分支会清空
		// ReasoningContent —— 两者互斥时占位优先，保证 deepseek 系字段恒在）。
		// tool 消息：requiresToolResultName → 带 name 字段
		if m.Role == core.Tool && requireToolResultName && m.ToolName != "" {
			msg.Name = m.ToolName
		}
		// kimi 延迟工具（pi deferredToolsMode="kimi"）：toolResult 的 addedToolNames 触发
		// 延迟工具加载。go 的 Agent 工具集小且每轮全量下发（req.Tools），无延迟加载
		// 需求 → 不实现（AddedToolNames 字段保留供未来大工具集场景）。
		// thinking 块 → 纯文本（requiresThinkingAsText，不带标签避免模型模仿）。
		// S3 修复：用 msg.ReasoningContent（sanitize 后）而非 m.Reasoning（原始）；
		// 正文多块拼接在此一并处理（translateContent 的 assistant 分支只补空 Content，
		// 否则带图片块时正文丢失）。
		if m.Role == core.Assistant && msg.ReasoningContent != "" && requireThinkingAsText {
			thinkingText := SanitizeSurrogates(msg.ReasoningContent)
			var textParts []string
			for _, c := range m.Content {
				if c.Type == core.ContentTypeText {
					textParts = append(textParts, c.Content)
				}
			}
			bodyText := strings.Join(textParts, "\n")
			if bodyText != "" {
				msg.Content = thinkingText + "\n\n" + bodyText
			} else {
				msg.Content = thinkingText
			}
			msg.ReasoningContent = ""
		}
		// requiresReasoningContent 占位（thinking-as-text 之后，保证字段恒在）
		if requireReasoningContent && m.Role == core.Assistant && msg.ReasoningContent == "" {
			msg.ReasoningContent = " "
		}
		translateContent(&msg, m.Role, m.Content)
		for _, tc := range m.ToolCalls {
			msg.ToolCalls = append(msg.ToolCalls, openai.ToolCall{
				ID:       tc.Id,
				Type:     openai.ToolTypeFunction,
				Function: openai.FunctionCall{Name: tc.Name, Arguments: tc.Arguments},
			})
		}
		out = append(out, msg)
	}
	flushImages() // 尾部 tool 段（正常每轮都会命中）
	return out, nil
}

// appendImagesAsUserContent 把图片块并入一条 user 消息的内容数组（已有多块则追加）。
// 已有纯文本 Content 时先搬进 MultiContent，避免 Content/MultiContent 双写
// （SDK 序列化以 MultiContent 为准，但保留文本顺序语义）。
func appendImagesAsUserContent(msg *openai.ChatCompletionMessage, images []core.Content) {
	parts := msg.MultiContent
	if len(parts) == 0 && msg.Content != "" {
		parts = []openai.ChatMessagePart{{Type: openai.ChatMessagePartTypeText, Text: msg.Content}}
		msg.Content = ""
	}
	for _, c := range images {
		parts = append(parts, openai.ChatMessagePart{
			Type:     openai.ChatMessagePartTypeImageURL,
			ImageURL: &openai.ChatMessageImageURL{URL: c.Content},
		})
	}
	msg.MultiContent = parts
}

// isEmptyAssistantMessage 判断 assistant 消息是否「空」：无工具调用且所有内容块都是
// 空白文本（纯思考 / 全空轮）。空 assistant 消息在 OpenAI 兼容协议里既无 content 也
// 无 tool_calls，上游严格 schema 会 400（见 translateMessages 注释）。仅文本块参与判断
// —— 有可见文本 / 图片 / 工具调用的 assistant 消息都不是空（图片输出块少见但语义非空）。
func isEmptyAssistantMessage(m core.Message) bool {
	if m.Role != core.Assistant || len(m.ToolCalls) > 0 {
		return false
	}
	for _, c := range m.Content {
		if c.Type != core.ContentTypeText || strings.TrimSpace(c.Content) != "" {
			return false
		}
	}
	return true
}

// translateContent 把内容块数组翻译为 SDK 内容：
// tool 消息拼接文本（协议要求字符串）；其余角色单文本块直接映射，多块/含图走内容数组。
// 空 content 语义（真实 API 验证 2026-08-10，curl 逐形态实测 DeepSeek）：
//   - assistant 纯 tool_call 轮省略 content 字段 → DeepSeek 接受（容忍），原样省略；
//     （数组兜底不可行：空文本块 Text 空串被 omitempty 省略 → 400 missing field `text`；
//     故不引入 MultiContent 兜底）
//   - tool 消息空结果 → 400 missing field content（严格 schema）→ 占位文本兜底
func translateContent(msg *openai.ChatCompletionMessage, role core.MessageRole, blocks []core.Content) {
	if role == core.Tool {
		var sb strings.Builder
		for _, c := range blocks {
			if c.Type != core.ContentTypeImage {
				sb.WriteString(c.Content)
			}
		}
		if sb.Len() == 0 {
			sb.WriteString("[empty tool result]") // 防御：tool 消息 content 字段必须存在（DeepSeek 400 实证）
		}
		msg.Content = SanitizeSurrogates(sb.String())
		return
	}
	if len(blocks) == 0 {
		return
	}
	// assistant 消息：多文本块拼接为纯字符串（对齐 pi —— 数组 content 会让部分端点
	//（NVIDIA NIM 等）镜像 content-block 结构，产出递归嵌套如 [{'type':'text',...}]）。
	// requiresThinkingAsText 已在 translateMessages 设过 msg.Content（thinking+正文）→ 保留。
	if role == core.Assistant {
		if msg.Content == "" {
			var sb strings.Builder
			for _, c := range blocks {
				if c.Type == core.ContentTypeImage {
					continue // assistant 消息图片输出块少见；跳过（文本优先）
				}
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(c.Content)
			}
			msg.Content = SanitizeSurrogates(sb.String())
		}
		return
	}
	if len(blocks) == 1 && blocks[0].Type != "image" {
		msg.Content = SanitizeSurrogates(blocks[0].Content)
		return
	}
	parts := make([]openai.ChatMessagePart, 0, len(blocks))
	for _, c := range blocks {
		switch c.Type {
		case core.ContentTypeImage:
			parts = append(parts, openai.ChatMessagePart{
				Type:     openai.ChatMessagePartTypeImageURL,
				ImageURL: &openai.ChatMessageImageURL{URL: c.Content},
			})
		default:
			parts = append(parts, openai.ChatMessagePart{Type: openai.ChatMessagePartTypeText, Text: SanitizeSurrogates(c.Content)})
		}
	}
	msg.MultiContent = parts
}

// translateTools 把厂商无关的工具 schema 翻译为 SDK 工具定义。
func translateTools(schemas []core.ToolSchema) []openai.Tool {
	if len(schemas) == 0 {
		return nil
	}
	out := make([]openai.Tool, 0, len(schemas))
	for _, s := range schemas {
		out = append(out, openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        s.Name,
				Description: s.Description,
				Parameters:  s.Parameters,
			},
		})
	}
	return out
}

func mapFinishReason(fr openai.FinishReason) core.FinishReason {
	switch fr {
	case openai.FinishReasonStop:
		return core.FinishReasonStop
	case openai.FinishReasonLength:
		return core.FinishReasonLength
	case openai.FinishReasonToolCalls:
		return core.FinishReasonToolCall
	case openai.FinishReasonContentFilter:
		return core.FinishReasonContentFilter
	default:
		return core.FinishReasonStop
	}
}

// classifyAPIError 把 4xx 客户端错误分类（复用根包 ClassifyHTTPError 公共分类器）。
// 注意：仅建连路径调用（Stream 入口）；流中段错误帧（HTTP 200 + SSE error frame）
// 的 SDK 错误不含 HTTP 状态码——在此对文本做 429/限流识别（RateLimitError，宽退避
// 重试），其余仍走文本识别（上下文超限优先）。建连 4xx 有状态码，走状态码分类。
func classifyAPIError(err error) error {
	var apiErr *openai.APIError
	if errors.As(err, &apiErr) && apiErr.HTTPStatusCode >= 400 && apiErr.HTTPStatusCode < 500 {
		return ClassifyHTTPError(apiErr.HTTPStatusCode, UnwrapErrorBody(err))
	}
	// RequestError：错误体不是纯 JSON（SSE 帧 / HTML / 网关自定义）时 go-openai 走这条，
	// 类型不是 APIError —— 不带状态码会让 400 参数错误退化成「generic 可重试」而白试 11 次。
	// 状态码取自原错误，文案换成可读的上游信息（UnwrapErrorBody）。
	var reqErr *openai.RequestError
	if errors.As(err, &reqErr) && reqErr.HTTPStatusCode > 0 {
		return ClassifyHTTPError(reqErr.HTTPStatusCode, UnwrapErrorBody(err))
	}
	// 无状态码可用的形态（流中段 SSE 错误帧等）：先让文案可读，再做文本识别
	err = UnwrapErrorBody(err)
	// 先识别限流（部分网关 200 + error frame 表达 429 也走 SSE），再识别上下文超限；
	// 其余原样（可重试）。
	if IsRateLimitText(err.Error()) {
		return &RateLimitError{StatusCode: 0, Err: err}
	}
	return DetectContextExceeded(err)
}

// retryableClientErrors 已移至 errors.go（三协议共用）。
func toCoreUsage(u openai.Usage) core.Usage {
	cu := core.Usage{
		Input:       int64(u.PromptTokens),
		Output:      int64(u.CompletionTokens),
		TotalTokens: int64(u.TotalTokens),
		// chat-completions 协议默认：prompt_tokens **已含** cached_tokens（自证字段）。
		InputSemantics: core.UsageSemanticsProtocolOpenAI,
	}
	// 明细（Details）：只透传上游**真的报了**的维度（2026-09-23 补齐）——
	// 音频 / 预测输出 token 会影响成本归因，此前被整个丢弃；图片 token 该 SDK 不上报，
	// 不伪造（InputImage 恒 0，字段留给会报的网关）。
	var d core.UsageDetails
	if u.PromptTokensDetails != nil {
		cu.CacheRead = int64(u.PromptTokensDetails.CachedTokens)
		d.InputAudio = int64(u.PromptTokensDetails.AudioTokens)
	}
	if u.CompletionTokensDetails != nil {
		cu.Reasoning = int64(u.CompletionTokensDetails.ReasoningTokens)
		d.OutputAudio = int64(u.CompletionTokensDetails.AudioTokens)
		d.PredictionAccepted = int64(u.CompletionTokensDetails.AcceptedPredictionTokens)
		d.PredictionRejected = int64(u.CompletionTokensDetails.RejectedPredictionTokens)
	}
	if d != (core.UsageDetails{}) {
		cu.Details = &d // 纯文本轮不挂指针（不占展示位，也不给「0 个音频 token」的错觉）
	}
	return cu
}
