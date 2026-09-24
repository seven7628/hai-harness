// Package auth 提供 OAuth 订阅登录的基础设施（对齐 pi 开源库 packages/ai/src/auth/）。
//
// 设计原则：
//   - 只依赖 stdlib（net/http / crypto / encoding/json），不 import 任何厂商 SDK；
//   - 厂商差异收敛在 auth/oauth/<id>.go 的 Flow 实现里（client_id / 端点 / scope）；
//   - 上层（provider 子包）通过 ModelAuth 拿到请求鉴权（Bearer / 专用 header / 永久 key），
//     经 auth.Transport 注入 HTTP 层，三协议子包零改动。
package auth

import (
	"context"
	"errors"
	"time"
)

// OAuthCredential 存储的 OAuth 凭证（对齐 pi OAuthCredential：type/refresh/access/expires）。
// 持久化于 ~/.go-code/oauth.json（0600），与 settings.json 分离。
type OAuthCredential struct {
	Type    string `json:"type"`              // 恒 "oauth"
	Refresh string `json:"refresh,omitempty"` // refresh token（OpenRouter 为空）
	Access  string `json:"access"`            // access token / 永久 key
	Expires int64  `json:"expires"`           // Unix 毫秒；MaxInt64 = 永不过期（OpenRouter 永久 key）

	// Extra 厂商扩展字段（如 GitHub Copilot 的 enterpriseUrl）。
	Extra map[string]string `json:"extra,omitempty"`
}

// Valid 凭证是否结构完整。
func (c *OAuthCredential) Valid() bool {
	return c != nil && c.Type == "oauth" && c.Access != ""
}

// ExpiresSoon 是否在 minValidity 内过期（默认提前 5 分钟触发刷新）。
func (c *OAuthCredential) ExpiresSoon(minValidity time.Duration) bool {
	if c.Expires >= int64(^uint64(0)>>1) { // MaxInt64 = 永不过期
		return false
	}
	if minValidity <= 0 {
		minValidity = DefaultOAuthMinValidity
	}
	return time.Now().Add(minValidity).UnixMilli() >= c.Expires
}

// DefaultOAuthMinValidity OAuth 刷新提前量（pi DEFAULT_OAUTH_MINIMUM_VALIDITY_MS = 5min）。
const DefaultOAuthMinValidity = 5 * time.Minute

// ModelAuth 请求鉴权结果（对齐 pi ModelAuth：apiKey/headers/baseUrl）。
// 有值字段覆盖 provider 默认；空字段不参与。
type ModelAuth struct {
	// APIKey 有值 → 作为 API key 传给 provider 构造（OpenRouter 永久 key 走此通道）。
	APIKey string
	// Headers 有值 → 注入请求头（Copilot 专用 header 组；Authorization 也在此表达）。
	Headers map[string]string
	// BaseURL 有值 → 覆盖 provider baseURL（Copilot proxy-ep 动态端点）。
	BaseURL string
}

// Bearer 便捷构造：Authorization: Bearer <token>。
func Bearer(token string) ModelAuth {
	return ModelAuth{Headers: map[string]string{"Authorization": "Bearer " + token}}
}

// Flow 一个厂商的 OAuth 流程（对齐 pi OAuthAuth：login/refresh/toAuth）。
type Flow interface {
	// Name 显示名（如 "Anthropic (Claude Pro/Max)"）。
	Name() string
	// IsSubscription 是否订阅制登录。
	IsSubscription() bool
	// Login 发起登录（PKCE + loopback 或 device code），返回新凭证。
	Login(ctx context.Context, interaction Interaction) (*OAuthCredential, error)
	// Refresh 用 refresh token 换新凭证；不支持刷新返回 (nil, nil)。
	Refresh(ctx context.Context, cred *OAuthCredential) (*OAuthCredential, error)
	// ToAuth 由凭证推导请求鉴权（多数厂商 = Bearer；Copilot = 专用 header + 动态 baseURL）。
	ToAuth(cred *OAuthCredential) (ModelAuth, error)
}

// Interaction 登录交互回调（bridge 实现：IPC 事件推送 + 弹窗双通道）。
// 对齐 pi AuthInteraction：prompt 返回输入（manual_code 粘贴重定向 URL 等）；取消返回 ErrCancelled。
type Interaction interface {
	// Notify 向用户展示事件（auth_url / device_code / progress / info）。
	Notify(event AuthEvent) error
	// Prompt 向用户询问；ctx 取消或用户取消返回 ErrCancelled。
	Prompt(ctx context.Context, p Prompt) (string, error)
}

// PromptType 询问类型。
type PromptType string

const (
	// PromptManualCode 手动粘贴授权码 / 重定向 URL（浏览器无法回环时的通道）。
	PromptManualCode PromptType = "manual_code"
	// PromptText 普通文本输入。
	PromptText PromptType = "text"
	// PromptSecret 密文输入。
	PromptSecret PromptType = "secret"
)

// Prompt 一次用户询问（对齐 pi AuthPrompt）。
type Prompt struct {
	Type        PromptType `json:"type"`
	Message     string     `json:"message"`
	Placeholder string     `json:"placeholder,omitempty"`
}

// EventType 登录事件类型。
type EventType string

const (
	// EventAuthURL 需要用户打开授权链接（PKCE flow）。
	EventAuthURL EventType = "auth_url"
	// EventDeviceCode 需要用户在浏览器输入设备码（device flow）。
	EventDeviceCode EventType = "device_code"
	// EventProgress 进度更新。
	EventProgress EventType = "progress"
	// EventInfo 普通信息（含可点链接）。
	EventInfo EventType = "info"
)

// AuthEvent 登录过程事件（对齐 pi AuthEvent）。
type AuthEvent struct {
	Type             EventType `json:"type"`
	URL              string    `json:"url,omitempty"`                       // auth_url 的授权链接
	Instructions     string    `json:"instructions,omitempty"`              // 操作指引
	UserCode         string    `json:"user_code,omitempty"`                 // device_code 用户码
	VerificationURI  string    `json:"verification_uri,omitempty"`          // device 验证页
	VerificationURIC string    `json:"verification_uri_complete,omitempty"` // 含用户码的完整链接
	IntervalSeconds  int       `json:"interval_seconds,omitempty"`          // 轮询间隔
	ExpiresInSeconds int       `json:"expires_in_seconds,omitempty"`
	Message          string    `json:"message,omitempty"`
}

// 错误语义（对齐 pi ModelsError code "oauth"/"auth"）。
var (
	// ErrNotLoggedIn 未登录（无凭证）。
	ErrNotLoggedIn = errors.New("oauth: not logged in")
	// ErrCancelled 用户取消 / ctx 取消。
	ErrCancelled = errors.New("oauth: cancelled")
	// ErrRefreshFailed 刷新失败（携带原始错误；不静默回退 env key）。
	ErrRefreshFailed = errors.New("oauth: refresh failed")
	// ErrTokenExpired 刷新后 token 仍不满足最小有效期。
	ErrTokenExpired = errors.New("oauth: token expires too soon")
)
