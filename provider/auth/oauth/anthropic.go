package oauth

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/seven7628/hai-harness/provider/auth"
)

// Anthropic OAuth flow（Claude Pro/Max 订阅）—— PKCE + loopback 回调。
// 对齐 pi auth/oauth/anthropic.ts。

// Anthropic 端点与客户端常量（client_id 为 pi 公开客户端标识的明文，非机密）。
const (
	AnthropicClientID  = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	AnthropicAuthorize = "https://claude.ai/oauth/authorize"
	AnthropicScopes    = "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
)

// AnthropicTokenURL token 端点（var 便于测试替换）。
var AnthropicTokenURL = "https://platform.claude.com/v1/oauth/token"

// AnthropicFlow Anthropic 订阅 OAuth。
type AnthropicFlow struct{}

// NewAnthropicFlow 构造 Anthropic OAuth flow。
func NewAnthropicFlow() *AnthropicFlow { return &AnthropicFlow{} }

// Name 实现 auth.Flow。
func (f *AnthropicFlow) Name() string { return "Anthropic (Claude Pro/Max)" }

// IsSubscription 实现 auth.Flow。
func (f *AnthropicFlow) IsSubscription() bool { return true }

// Login 实现 auth.Flow：PKCE + loopback 回调，与 manual_code 手动粘贴竞争。
func (f *AnthropicFlow) Login(ctx context.Context, interaction auth.Interaction) (cred *auth.OAuthCredential, err error) {
	verifier, challenge, err := auth.GeneratePKCE()
	if err != nil {
		return nil, err
	}
	cs, err := auth.NewCallbackServer(ctx, verifier) // state = verifier（对齐 pi）
	if err != nil {
		return nil, err
	}
	defer cs.Close()

	// 手动通道（浏览器无法回环时粘贴授权码/重定向 URL）
	type manualResult struct {
		input string
		err   error
	}
	manualCh := make(chan manualResult, 1)
	manualCtx, manualCancel := context.WithCancel(ctx)
	defer manualCancel()
	go func() {
		input, perr := interaction.Prompt(manualCtx, auth.Prompt{
			Type:        auth.PromptManualCode,
			Message:     "Complete login in your browser, or paste the authorization code / redirect URL here:",
			Placeholder: cs.RedirectURI,
		})
		manualCh <- manualResult{input: input, err: perr}
		cs.Cancel() // 手动接管 → 取消回调等待
	}()

	authParams := url.Values{}
	authParams.Set("code", "true")
	authParams.Set("client_id", AnthropicClientID)
	authParams.Set("response_type", "code")
	authParams.Set("redirect_uri", cs.RedirectURI)
	authParams.Set("scope", AnthropicScopes)
	authParams.Set("code_challenge", challenge)
	authParams.Set("code_challenge_method", "S256")
	authParams.Set("state", verifier)
	authURL := AnthropicAuthorize + "?" + authParams.Encode()

	if err := interaction.Notify(auth.AuthEvent{
		Type:         auth.EventAuthURL,
		URL:          authURL,
		Instructions: "Complete login in your browser. If the browser is on another machine, paste the final redirect URL here.",
	}); err != nil {
		return nil, err
	}

	code, state, werr := cs.WaitCode()
	if werr != nil {
		// 回调失败（未打开浏览器/被 Cancel）→ 等 manual 输入兜底；select 防阻塞死锁
		select {
		case mr := <-manualCh:
			if mr.err != nil {
				return nil, mr.err
			}
			return f.exchangeManual(ctx, mr.input, verifier, cs.RedirectURI)
		case <-ctx.Done():
			return nil, auth.ErrCancelled
		}
	}
	if state != verifier {
		return nil, errors.New("oauth state mismatch")
	}
	if err := interaction.Notify(auth.AuthEvent{Type: auth.EventProgress, Message: "Exchanging authorization code for tokens..."}); err != nil {
		return nil, err
	}
	cred, err = f.exchange(ctx, code, state, verifier, cs.RedirectURI)
	if err != nil {
		return nil, err
	}
	manualCancel()
	return cred, nil
}

// exchangeManual 处理手动粘贴的授权码/重定向 URL。
func (f *AnthropicFlow) exchangeManual(ctx context.Context, input, verifier, redirectURI string) (*auth.OAuthCredential, error) {
	code, state := auth.ParseAuthorizationInput(input)
	if code == "" {
		return nil, errors.New("missing authorization code")
	}
	if state != "" && state != verifier {
		return nil, errors.New("oauth state mismatch")
	}
	if state == "" {
		state = verifier
	}
	return f.exchange(ctx, code, state, verifier, redirectURI)
}

// exchange 用授权码换 token（对齐 pi exchangeAuthorizationCode）。
func (f *AnthropicFlow) exchange(ctx context.Context, code, state, verifier, redirectURI string) (*auth.OAuthCredential, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", AnthropicClientID)
	form.Set("code", code)
	form.Set("state", state)
	form.Set("redirect_uri", redirectURI)
	form.Set("code_verifier", verifier)

	body, err := postForm(ctx, HTTPClient, AnthropicTokenURL, form, nil)
	if err != nil {
		return nil, fmt.Errorf("token exchange request failed: %w", err)
	}
	return parseTokenResponse(body)
}

// Refresh 实现 auth.Flow：refresh_token 换新凭证。
func (f *AnthropicFlow) Refresh(ctx context.Context, cred *auth.OAuthCredential) (*auth.OAuthCredential, error) {
	if cred == nil || cred.Refresh == "" {
		return nil, errors.New("no refresh token")
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", AnthropicClientID)
	form.Set("refresh_token", cred.Refresh)

	body, err := postForm(ctx, HTTPClient, AnthropicTokenURL, form, nil)
	if err != nil {
		return nil, fmt.Errorf("token refresh request failed: %w", err)
	}
	return parseTokenResponse(body)
}

// ToAuth 实现 auth.Flow：access token → Bearer + Claude Code identity 头。
// 对齐 pi anthropic-messages.ts OAuth 分支：claude-code 身份头让订阅 token 以
// Claude Code 客户端身份调用（否则平台侧拒绝/降级）。
func (f *AnthropicFlow) ToAuth(cred *auth.OAuthCredential) (auth.ModelAuth, error) {
	if cred == nil || cred.Access == "" {
		return auth.ModelAuth{}, auth.ErrNotLoggedIn
	}
	return auth.ModelAuth{Headers: map[string]string{
		"Authorization":  "Bearer " + cred.Access,
		"anthropic-beta": "claude-code-20250219,oauth-2025-04-20",
		"user-agent":     "claude-cli/2.1.75",
		"x-app":          "cli",
	}}, nil
}
