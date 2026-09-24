package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	"github.com/seven7628/hai-harness/provider/auth"
)

// OpenAI Codex OAuth flow（ChatGPT Plus/Pro 订阅）—— PKCE + loopback，device 兜底。
// 对齐 pi auth/oauth/openai-codex.ts。

// OpenAI Codex 端点与客户端常量。
const (
	OpenAICodexClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	OpenAICodexAuthBase = "https://auth.openai.com"
	// OpenAICodexAuthorize 授权端点。
	OpenAICodexAuthorize = OpenAICodexAuthBase + "/oauth/authorize"
	// OpenAICodexTokenURL token 端点。
	OpenAICodexTokenURL = OpenAICodexAuthBase + "/oauth/token"
	// OpenAICodexDeviceUserCode device 授权端点。
	OpenAICodexDeviceUserCode = OpenAICodexAuthBase + "/api/accounts/deviceauth/usercode"
	// OpenAICodexDeviceToken device 轮询端点。
	OpenAICodexDeviceToken = OpenAICodexAuthBase + "/api/accounts/deviceauth/token"
	// OpenAICodexDeviceVerify device 验证页。
	OpenAICodexDeviceVerify = OpenAICodexAuthBase + "/codex/device"
	// OpenAICodexScope 请求 scope。
	OpenAICodexScope = "openid profile email offline_access"
	// OpenAICodexDeviceTimeout device 超时（15 分钟，对齐 pi）。
	OpenAICodexDeviceTimeout = 15 * 60
	// OpenAICodexAPIBase 对话 API 端点。
	OpenAICodexAPIBase = "https://chatgpt.com/backend-api"
)

// OpenAICodexFlow OpenAI Codex 订阅 OAuth。
type OpenAICodexFlow struct{}

// NewOpenAICodexFlow 构造 OpenAI Codex OAuth flow。
func NewOpenAICodexFlow() *OpenAICodexFlow { return &OpenAICodexFlow{} }

// Name 实现 auth.Flow。
func (f *OpenAICodexFlow) Name() string { return "OpenAI (ChatGPT Plus/Pro)" }

// IsSubscription 实现 auth.Flow。
func (f *OpenAICodexFlow) IsSubscription() bool { return true }

// Login 实现 auth.Flow：PKCE + loopback 优先，manual 通道兜底。
// （pi 还有 device 双通道；本实现以 PKCE 为主，manual 覆盖浏览器不可达场景。）
func (f *OpenAICodexFlow) Login(ctx context.Context, interaction auth.Interaction) (cred *auth.OAuthCredential, err error) {
	verifier, challenge, err := auth.GeneratePKCE()
	if err != nil {
		return nil, err
	}
	cs, err := auth.NewCallbackServer(ctx, verifier)
	if err != nil {
		return nil, err
	}
	defer cs.Close()

	manualCh := make(chan string, 1)
	manualCtx, manualCancel := context.WithCancel(ctx)
	defer manualCancel()
	go func() {
		input, perr := interaction.Prompt(manualCtx, auth.Prompt{
			Type:        auth.PromptManualCode,
			Message:     "Complete login in your browser, or paste the authorization code / redirect URL here:",
			Placeholder: cs.RedirectURI,
		})
		if perr == nil {
			manualCh <- input
		}
		cs.Cancel()
	}()

	authParams := url.Values{}
	authParams.Set("client_id", OpenAICodexClientID)
	authParams.Set("response_type", "code")
	authParams.Set("redirect_uri", cs.RedirectURI)
	authParams.Set("scope", OpenAICodexScope)
	authParams.Set("code_challenge", challenge)
	authParams.Set("code_challenge_method", "S256")
	authParams.Set("state", verifier)
	authURL := OpenAICodexAuthorize + "?" + authParams.Encode()

	if err := interaction.Notify(auth.AuthEvent{
		Type:         auth.EventAuthURL,
		URL:          authURL,
		Instructions: "Complete login in your browser. If the browser is on another machine, paste the final redirect URL here.",
	}); err != nil {
		return nil, err
	}

	code, state, werr := cs.WaitCode()
	if werr != nil {
		select {
		case input := <-manualCh:
			if input == "" {
				return nil, auth.ErrCancelled
			}
			code, state = auth.ParseAuthorizationInput(input)
			if code == "" {
				return nil, errors.New("missing authorization code")
			}
		case <-ctx.Done():
			return nil, auth.ErrCancelled
		}
	}
	if state != "" && state != verifier {
		return nil, errors.New("oauth state mismatch")
	}
	if state == "" {
		state = verifier
	}

	if err := interaction.Notify(auth.AuthEvent{Type: auth.EventProgress, Message: "Exchanging authorization code for tokens..."}); err != nil {
		return nil, err
	}
	return f.exchange(ctx, code, state, verifier, cs.RedirectURI)
}

// exchange 授权码换 token（对齐 pi exchangeAuthorizationCode：标准 JSON 字符串字段）。
func (f *OpenAICodexFlow) exchange(ctx context.Context, code, state, verifier, redirectURI string) (*auth.OAuthCredential, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", OpenAICodexClientID)
	form.Set("code", code)
	form.Set("state", state)
	form.Set("redirect_uri", redirectURI)
	form.Set("code_verifier", verifier)

	body, err := postForm(ctx, HTTPClient, OpenAICodexTokenURL, form, nil)
	if err != nil {
		return nil, fmt.Errorf("token exchange request failed: %w", err)
	}
	var t struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, errors.New("token exchange returned invalid JSON")
	}
	if t.AccessToken == "" {
		return nil, errors.New("token exchange returned no access_token")
	}
	return newCredential(t.RefreshToken, t.AccessToken, t.ExpiresIn), nil
}

// Refresh 实现 auth.Flow：refresh_token 换新凭证。
func (f *OpenAICodexFlow) Refresh(ctx context.Context, cred *auth.OAuthCredential) (*auth.OAuthCredential, error) {
	if cred == nil || cred.Refresh == "" {
		return nil, errors.New("no codex refresh token")
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", OpenAICodexClientID)
	form.Set("refresh_token", cred.Refresh)

	body, err := postForm(ctx, HTTPClient, OpenAICodexTokenURL, form, nil)
	if err != nil {
		return nil, fmt.Errorf("token refresh request failed: %w", err)
	}
	var t struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, errors.New("token refresh returned invalid JSON")
	}
	if t.AccessToken == "" {
		return nil, errors.New("token refresh returned no access_token")
	}
	if t.RefreshToken == "" {
		t.RefreshToken = cred.Refresh
	}
	return newCredential(t.RefreshToken, t.AccessToken, t.ExpiresIn), nil
}

// ToAuth 实现 auth.Flow：access token → Bearer。
func (f *OpenAICodexFlow) ToAuth(cred *auth.OAuthCredential) (auth.ModelAuth, error) {
	if cred == nil || cred.Access == "" {
		return auth.ModelAuth{}, auth.ErrNotLoggedIn
	}
	return auth.Bearer(cred.Access), nil
}
