package oauth

import (
	"context"
	"errors"
	"fmt"

	"github.com/seven7628/hai-harness/provider/auth"
)

// xAI OAuth flow（SuperGrok / X Premium 订阅）—— device flow。
// 对齐 pi auth/oauth/xai.ts。

// xAI 端点与客户端常量。
const (
	XAIClientID      = "b1a00492-073a-47ea-816f-4c329264a828"
	XAIScope         = "openid profile email offline_access grok-cli:access api:access"
	XAIDeviceTimeout = 15 * 60
)

// xAI 端点（var 便于测试替换）。
var (
	XAIDeviceURL = "https://auth.x.ai/oauth2/device/code"
	XAITokenURL  = "https://auth.x.ai/oauth2/token"
)

// XAIFlow xAI 订阅 OAuth。
type XAIFlow struct{}

// NewXAIFlow 构造 xAI OAuth flow。
func NewXAIFlow() *XAIFlow { return &XAIFlow{} }

// Name 实现 auth.Flow。
func (f *XAIFlow) Name() string { return "xAI (Grok/X subscription)" }

// IsSubscription 实现 auth.Flow。
func (f *XAIFlow) IsSubscription() bool { return true }

// Login 实现 auth.Flow：device flow。
func (f *XAIFlow) Login(ctx context.Context, interaction auth.Interaction) (cred *auth.OAuthCredential, err error) {
	cfg := DeviceFlowConfig{
		DeviceCodeURL:     XAIDeviceURL,
		TokenURL:          XAITokenURL,
		ClientID:          XAIClientID,
		Scope:             XAIScope,
		DeviceCodeTimeout: XAIDeviceTimeout,
	}
	deviceCode, err := StartDeviceFlow(ctx, cfg, interaction)
	if err != nil {
		return nil, err
	}
	access, refresh, expiresIn, err := PollDeviceToken(ctx, cfg, deviceCode)
	if err != nil {
		return nil, err
	}
	return newCredential(refresh, access, expiresIn), nil
}

// Refresh 实现 auth.Flow：refresh_token 换新凭证。
func (f *XAIFlow) Refresh(ctx context.Context, cred *auth.OAuthCredential) (*auth.OAuthCredential, error) {
	if cred == nil || cred.Refresh == "" {
		return nil, errors.New("no xai refresh token")
	}
	body, err := postJSON(ctx, HTTPClient, XAITokenURL, map[string]any{
		"grant_type":    "refresh_token",
		"client_id":     XAIClientID,
		"refresh_token": cred.Refresh,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("xai token refresh failed: %w", err)
	}
	return parseTokenResponse(body)
}

// ToAuth 实现 auth.Flow：access token → Bearer。
func (f *XAIFlow) ToAuth(cred *auth.OAuthCredential) (auth.ModelAuth, error) {
	if cred == nil || cred.Access == "" {
		return auth.ModelAuth{}, auth.ErrNotLoggedIn
	}
	return auth.Bearer(cred.Access), nil
}
