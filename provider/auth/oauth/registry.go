// Package oauth 各厂商 OAuth 登录流程实现（对齐 pi packages/ai/src/auth/oauth/）。
//
// 每个厂商一个文件：client_id / 端点 / scope 常量内聚；只依赖 provider/auth 基础设施
// （PKCE / loopback 回调 / device 轮询），不依赖任何厂商 SDK。
package oauth

import (
	"fmt"
	"sync"

	"github.com/seven7628/hai-harness/provider/auth"
)

// FlowID 内置 flow 标识（与 ProviderPreset.OAuth.Flow 对应）。
type FlowID string

const (
	FlowAnthropic     FlowID = "anthropic"
	FlowOpenAICodex   FlowID = "openai-codex"
	FlowGitHubCopilot FlowID = "github-copilot"
	FlowKimiCoding    FlowID = "kimi-coding"
	FlowOpenRouter    FlowID = "openrouter"
	FlowXAI           FlowID = "xai"
)

// registry flow 构造器注册表（对齐 pi auth/oauth/load.ts 的懒加载 loaders）。
var registry = struct {
	sync.Mutex
	flows map[FlowID]func() auth.Flow
}{
	flows: map[FlowID]func() auth.Flow{
		FlowAnthropic:     func() auth.Flow { return NewAnthropicFlow() },
		FlowOpenAICodex:   func() auth.Flow { return NewOpenAICodexFlow() },
		FlowGitHubCopilot: func() auth.Flow { return NewGitHubCopilotFlow() },
		FlowKimiCoding:    func() auth.Flow { return NewKimiCodingFlow() },
		FlowOpenRouter:    func() auth.Flow { return NewOpenRouterFlow() },
		FlowXAI:           func() auth.Flow { return NewXAIFlow() },
	},
}

// NewFlow 按 flow id 构造 flow；未知 id 返回错误。
func NewFlow(id string) (auth.Flow, error) {
	registry.Lock()
	defer registry.Unlock()
	c, ok := registry.flows[FlowID(id)]
	if !ok {
		return nil, fmt.Errorf("unknown oauth flow: %s", id)
	}
	return c(), nil
}

// HasFlow 是否支持某 flow id。
func HasFlow(id string) bool {
	registry.Lock()
	defer registry.Unlock()
	_, ok := registry.flows[FlowID(id)]
	return ok
}
