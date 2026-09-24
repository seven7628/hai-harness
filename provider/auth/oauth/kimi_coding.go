package oauth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/seven7628/hai-harness/provider/auth"
)

// Kimi Coding OAuth flow（Kimi Code 订阅）—— device flow（RFC 8628，JSON 响应）。
// 对齐 pi auth/oauth/kimi-coding.ts。

// Kimi Coding 端点与客户端常量。
const (
	KimiCodingClientID = "17e5f671-d194-4dfb-9706-5516cb48c098"
	// KimiCodingOAuthHost 默认认证主机（可用环境变量 KIMI_CODE_OAUTH_HOST / KIMI_OAUTH_HOST 覆盖）。
	KimiCodingOAuthHost = "https://auth.kimi.com"
	// KimiCodingAPIBase 对话 API 端点。
	KimiCodingAPIBase = "https://api.kimi.com/coding"
	// KimiCodingDeviceTimeout 设备码超时（15 分钟，对齐 pi）。
	KimiCodingDeviceTimeout = 15 * 60
	// KimiCodingRefreshMaxRetries 刷新最大重试（对齐 pi REFRESH_MAX_RETRIES）。
	KimiCodingRefreshMaxRetries = 3
)

// KimiCodingFlow Kimi Code 订阅 OAuth。
type KimiCodingFlow struct{}

// NewKimiCodingFlow 构造 Kimi Coding OAuth flow。
func NewKimiCodingFlow() *KimiCodingFlow { return &KimiCodingFlow{} }

// Name 实现 auth.Flow。
func (f *KimiCodingFlow) Name() string { return "Kimi Code (subscription)" }

// IsSubscription 实现 auth.Flow。
func (f *KimiCodingFlow) IsSubscription() bool { return true }

// oauthHost 认证主机（env 覆盖，对齐 pi getOauthHost）。
func (f *KimiCodingFlow) oauthHost() string {
	host := os.Getenv(KimiCodingOAuthHostEnv)
	if host == "" {
		host = os.Getenv("KIMI_OAUTH_HOST")
	}
	if host == "" {
		return KimiCodingOAuthHost
	}
	return strings.TrimSuffix(host, "/")
}

// 环境变量名。
const KimiCodingOAuthHostEnv = "KIMI_CODE_OAUTH_HOST"

// Login 实现 auth.Flow：device flow。
func (f *KimiCodingFlow) Login(ctx context.Context, interaction auth.Interaction) (cred *auth.OAuthCredential, err error) {
	cfg := DeviceFlowConfig{
		DeviceCodeURL:     f.oauthHost() + "/device/code",
		TokenURL:          f.oauthHost() + "/oauth/token",
		ClientID:          KimiCodingClientID,
		DeviceCodeTimeout: KimiCodingDeviceTimeout,
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

// Refresh 实现 auth.Flow：refresh_token 换新凭证（对齐 pi：最多重试 3 次）。
func (f *KimiCodingFlow) Refresh(ctx context.Context, cred *auth.OAuthCredential) (*auth.OAuthCredential, error) {
	if cred == nil || cred.Refresh == "" {
		return nil, errors.New("no kimi refresh token")
	}
	var lastErr error
	for i := 0; i < KimiCodingRefreshMaxRetries; i++ {
		body, err := postJSON(ctx, HTTPClient, f.oauthHost()+"/oauth/token", map[string]any{
			"grant_type":    "refresh_token",
			"client_id":     KimiCodingClientID,
			"refresh_token": cred.Refresh,
		}, nil)
		if err == nil {
			return parseTokenResponse(body)
		}
		lastErr = err
	}
	return nil, fmt.Errorf("kimi token refresh failed: %w", lastErr)
}

// ToAuth 实现 auth.Flow：access token → Bearer。
func (f *KimiCodingFlow) ToAuth(cred *auth.OAuthCredential) (auth.ModelAuth, error) {
	if cred == nil || cred.Access == "" {
		return auth.ModelAuth{}, auth.ErrNotLoggedIn
	}
	return auth.Bearer(cred.Access), nil
}
