package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	"github.com/seven7628/hai-harness/provider/auth"
)

// OpenRouter OAuth flow（OpenRouter 账号）—— PKCE + loopback。
// 特殊：token 交换返回【永久 API key】（非 access/refresh 对）→ ToAuth 落 api_key 通道。
// 对齐 pi auth/oauth/openrouter.ts。

// OpenRouter 端点与常量。
const (
	OpenRouterAuthorize = "https://openrouter.ai/auth"
	// OpenRouterLoginTimeout 登录总超时（5 分钟，对齐 pi）。
	OpenRouterLoginTimeout = 5 * 60
	// OpenRouterExchangeTimeout token 交换超时（30s，对齐 pi）。
	OpenRouterExchangeTimeout = 30
)

// OpenRouterTokenURL token 端点（var 便于测试替换）。
var OpenRouterTokenURL = "https://openrouter.ai/api/v1/auth/keys"

// OpenRouterFlow OpenRouter OAuth。
type OpenRouterFlow struct{}

// NewOpenRouterFlow 构造 OpenRouter OAuth flow。
func NewOpenRouterFlow() *OpenRouterFlow { return &OpenRouterFlow{} }

// Name 实现 auth.Flow。
func (f *OpenRouterFlow) Name() string { return "OpenRouter" }

// IsSubscription 实现 auth.Flow。
func (f *OpenRouterFlow) IsSubscription() bool { return false }

// Login 实现 auth.Flow：PKCE + loopback（回调处理 token 交换，对齐 pi startCallbackServer 内嵌交换）。
func (f *OpenRouterFlow) Login(ctx context.Context, interaction auth.Interaction) (cred *auth.OAuthCredential, err error) {
	verifier, challenge, err := auth.GeneratePKCE()
	if err != nil {
		return nil, err
	}
	cs, err := auth.NewCallbackServer(ctx, verifier)
	if err != nil {
		return nil, err
	}
	defer cs.Close()

	// 手动通道竞争（对齐 pi：浏览器无法回环时粘贴授权码）
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
	authParams.Set("code_challenge", challenge)
	authParams.Set("code_challenge_method", "S256")
	authParams.Set("redirect_uri", cs.RedirectURI)
	authParams.Set("state", verifier)
	authURL := OpenRouterAuthorize + "?" + authParams.Encode()

	if err := interaction.Notify(auth.AuthEvent{
		Type:         auth.EventAuthURL,
		URL:          authURL,
		Instructions: "Complete login in your browser. If the browser is on another machine, paste the final redirect URL here.",
	}); err != nil {
		return nil, err
	}

	code, state, werr := cs.WaitCode()
	if werr != nil {
		// 回调失败 → 手动输入通道
		select {
		case input := <-manualCh:
			if input == "" {
				return nil, auth.ErrCancelled
			}
			code, _ = auth.ParseAuthorizationInput(input)
			if code == "" {
				return nil, errors.New("missing authorization code")
			}
		case <-ctx.Done():
			return nil, auth.ErrCancelled
		}
	} else if state != verifier {
		return nil, errors.New("oauth state mismatch")
	}

	if err := interaction.Notify(auth.AuthEvent{Type: auth.EventProgress, Message: "Exchanging authorization code for API key..."}); err != nil {
		return nil, err
	}
	return f.exchange(ctx, code, verifier)
}

// exchange 用授权码换永久 key（对齐 pi exchangeAuthorizationCode）。
func (f *OpenRouterFlow) exchange(ctx context.Context, code, verifier string) (*auth.OAuthCredential, error) {
	body, err := postJSON(ctx, HTTPClient, OpenRouterTokenURL, map[string]any{
		"code":                  code,
		"code_verifier":         verifier,
		"code_challenge_method": "S256",
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("openrouter key exchange failed: %w", err)
	}
	var t struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, errors.New("openrouter OAuth returned invalid JSON")
	}
	if t.Key == "" {
		return nil, errors.New(`openrouter OAuth response carries no "key"`)
	}
	return &auth.OAuthCredential{
		Type:    "oauth",
		Access:  t.Key,
		Expires: int64(^uint64(0) >> 1), // 永久 key，永不过期
	}, nil
}

// Refresh 实现 auth.Flow：OpenRouter key 永久有效，不支持刷新。
func (f *OpenRouterFlow) Refresh(ctx context.Context, cred *auth.OAuthCredential) (*auth.OAuthCredential, error) {
	return nil, nil
}

// ToAuth 实现 auth.Flow：永久 key → API key 通道（KeyInstead 语义，上层落 api_key 字段）。
func (f *OpenRouterFlow) ToAuth(cred *auth.OAuthCredential) (auth.ModelAuth, error) {
	if cred == nil || cred.Access == "" {
		return auth.ModelAuth{}, auth.ErrNotLoggedIn
	}
	return auth.ModelAuth{APIKey: cred.Access}, nil
}
