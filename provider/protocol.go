package provider

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
)

// Protocol 接入协议枚举。
type Protocol string

const (
	ProtocolChatCompletions Protocol = "chat_completions"
	ProtocolResponses       Protocol = "responses"
	ProtocolAnthropic       Protocol = "anthropic"
)

// Dependencies 各协议 provider 构造所需的依赖（依赖注入，便于测试替身）。
// 零值 = 各协议默认实现（真实 HTTP client / 内置 Registry）。
type Dependencies struct {
	// HTTPClient 可注入 mock/限流/日志 transport；nil = http.DefaultClient。
	HTTPClient *http.Client

	// Registry 模型元数据注册表（窗口/上限/价/推理/模态）；nil = 内置默认。
	Registry *Registry

	// Logger 可选日志；nil = 静默。
	Logger *slog.Logger
}

// UnsupportedProtocolProvider 非法协议占位 provider：Stream 时返回永久错误（不重试），
// 让上层 LLM 请求报出明确原因（而非静默当 chat-completions 产生 endpoint 错误）。
func UnsupportedProtocolProvider(err error) Provider {
	return &unsupportedProvider{err: MarkPermanent(err)}
}

type unsupportedProvider struct {
	err error
}

func (u *unsupportedProvider) Stream(context.Context, *StreamRequest, func(StreamEvent) error) error {
	return u.err
}

// ParseProtocol 从字符串解析协议（bridge settings.json 用）。
// 空串 → chat_completions（默认）；非空非法值 → error（拒绝静默回退，
// 避免用户填错协议时被当 chat-completions 导致 endpoint/参数错误难诊断）。
func ParseProtocol(s string) (Protocol, error) {
	switch Protocol(s) {
	case "":
		return ProtocolChatCompletions, nil
	case ProtocolChatCompletions, ProtocolResponses, ProtocolAnthropic:
		return Protocol(s), nil
	default:
		return "", fmt.Errorf("unknown provider protocol %q (支持: chat_completions/responses/anthropic)", s)
	}
}

// NewProvider 按协议构造 provider 实现（依赖注入）。
// 仅 chat-completions 在根包构造（无循环依赖）；responses/anthropic 由 bridge 直调
// 子包构造（子包 import 根包契约层，工厂放根包会循环依赖——见 providers.go newProvider）。
// 非法协议返回错误（ParseProtocol 严格版，拒绝静默回退）。
func NewProvider(protocol Protocol, deps Dependencies, baseURL, apiKey string, caps Capabilities) (Provider, error) {
	switch protocol {
	case ProtocolChatCompletions:
		cc := FullOpenAICompatCapabilities()
		if caps.OpenAI != nil {
			cc = *caps.OpenAI
		}
		return NewOpenAIProviderWithDeps(deps, baseURL, apiKey, cc), nil
	case ProtocolResponses, ProtocolAnthropic:
		return nil, fmt.Errorf("provider protocol %q: 请由 desktop/bridge 构造（根包工厂仅支持 chat_completions）", protocol)
	default:
		return nil, fmt.Errorf("unknown provider protocol %q", protocol)
	}
}

// Capabilities 聚合各协议能力（NewProvider 参数；nil 字段 = 各协议默认）。
type Capabilities struct {
	OpenAI    *OpenAICompatCapabilities
	Anthropic *AnthropicCapabilities
	Responses *ResponsesCapabilities
}
