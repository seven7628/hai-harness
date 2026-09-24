package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/seven7628/hai-harness/provider/auth"
)

// GitHub Copilot OAuth flow（Copilot 订阅）—— device flow + copilot_internal 刷新。
// 对齐 pi auth/oauth/github-copilot.ts。

// Copilot 端点与客户端常量。
const (
	CopilotClientID = "Iv1.b507a08c87ecfe98"
	// CopilotAPIVersion 对齐 pi COPILOT_API_VERSION。
	CopilotAPIVersion = "2026-06-01"
	// CopilotUserAgent 对齐 pi "GitHubCopilotChat/0.35.0"。
	CopilotUserAgent = "GitHubCopilotChat/0.35.0"
)

// CopilotHeaders 请求 Copilot API 的专用 header 组（对齐 pi COPILOT_HEADERS）。
var CopilotHeaders = map[string]string{
	"User-Agent":             CopilotUserAgent,
	"Editor-Version":         "vscode/1.107.0",
	"Editor-Plugin-Version":  "copilot-chat/0.35.0",
	"Copilot-Integration-Id": "vscode-chat",
}

// GitHubCopilotFlow GitHub Copilot 订阅 OAuth。
type GitHubCopilotFlow struct{}

// NewGitHubCopilotFlow 构造 Copilot OAuth flow。
func NewGitHubCopilotFlow() *GitHubCopilotFlow { return &GitHubCopilotFlow{} }

// Name 实现 auth.Flow。
func (f *GitHubCopilotFlow) Name() string { return "GitHub Copilot" }

// IsSubscription 实现 auth.Flow。
func (f *GitHubCopilotFlow) IsSubscription() bool { return true }

// Login 实现 auth.Flow：device flow。
func (f *GitHubCopilotFlow) Login(ctx context.Context, interaction auth.Interaction) (cred *auth.OAuthCredential, err error) {
	cfg := DeviceFlowConfig{
		DeviceCodeURL: "https://github.com/login/device/code",
		TokenURL:      "https://github.com/login/oauth/access_token",
		ClientID:      CopilotClientID,
		Scope:         "read:user",
		ExtraHeaders: map[string]string{
			"User-Agent": CopilotUserAgent,
		},
	}
	deviceCode, err := StartDeviceFlow(ctx, cfg, interaction)
	if err != nil {
		return nil, err
	}
	// GitHub device flow：轮询 access_token → 拿 GitHub OAuth token（作为 refresh）
	// → copilot_internal/v2/token 换 Copilot access token（对齐 pi 两段式）。
	ghToken, _, _, err := PollDeviceToken(ctx, cfg, deviceCode)
	if err != nil {
		return nil, err
	}
	if err := interaction.Notify(auth.AuthEvent{Type: auth.EventProgress, Message: "Exchanging GitHub token for Copilot access..."}); err != nil {
		return nil, err
	}
	return f.refreshFromGitHubToken(ctx, ghToken, "")
}

// refreshFromGitHubToken 用 GitHub OAuth token 换 Copilot token（对齐 pi refreshGitHubCopilotAccessToken）。
func (f *GitHubCopilotFlow) refreshFromGitHubToken(ctx context.Context, ghToken, enterpriseDomain string) (*auth.OAuthCredential, error) {
	baseURL := copilotAPIBase(ghToken, enterpriseDomain)
	body, err := getJSON(ctx, HTTPClient, baseURL+"/copilot_internal/v2/token", map[string]string{
		"Authorization": "Bearer " + ghToken,
	})
	if err != nil {
		return nil, fmt.Errorf("copilot token request failed: %w", err)
	}
	var t struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"` // Unix 秒
	}
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, fmt.Errorf("invalid copilot token response: %w", err)
	}
	if t.Token == "" {
		return nil, errors.New("invalid copilot token response fields")
	}
	expires := int64(^uint64(0) >> 1)
	if t.ExpiresAt > 0 {
		expires = t.ExpiresAt*1000 - 5*60*1000 // 提前 5 分钟
	}
	return &auth.OAuthCredential{
		Type:    "oauth",
		Refresh: ghToken, // Copilot 的 refresh 是长期 GitHub token（对齐 pi）
		Access:  t.Token,
		Expires: expires,
		Extra:   map[string]string{"enterprise_url": enterpriseDomain},
	}, nil
}

// copilotAPIBase 从 token 解析 proxy-ep → API base URL（对齐 pi getGitHubCopilotBaseUrl）。
// token 格式: tid=...;exp=...;proxy-ep=proxy.individual.githubcopilot.com;...
func copilotAPIBase(token, enterpriseDomain string) string {
	if token != "" {
		for _, part := range strings.Split(token, ";") {
			if k, v, ok := strings.Cut(part, "="); ok && strings.TrimSpace(k) == "proxy-ep" {
				host := strings.TrimSpace(v)
				if host != "" {
					// proxy.xxx → api.xxx（对齐 pi：proxyHost.replace(/^proxy\./, "api.")）
					apiHost := "api." + strings.TrimPrefix(host, "proxy.")
					return "https://" + apiHost
				}
			}
		}
	}
	if enterpriseDomain != "" {
		return "https://copilot-api." + enterpriseDomain
	}
	return "https://api.individual.githubcopilot.com"
}

// Refresh 实现 auth.Flow：GitHub token 直接换新 Copilot token（refresh 本身不变）。
func (f *GitHubCopilotFlow) Refresh(ctx context.Context, cred *auth.OAuthCredential) (*auth.OAuthCredential, error) {
	if cred == nil || cred.Refresh == "" {
		return nil, errors.New("no copilot refresh token")
	}
	enterprise := ""
	if cred.Extra != nil {
		enterprise = cred.Extra["enterprise_url"]
	}
	return f.refreshFromGitHubToken(ctx, cred.Refresh, enterprise)
}

// ToAuth 实现 auth.Flow：Copilot 专用 header + 动态 baseURL。
func (f *GitHubCopilotFlow) ToAuth(cred *auth.OAuthCredential) (auth.ModelAuth, error) {
	if cred == nil || cred.Access == "" {
		return auth.ModelAuth{}, auth.ErrNotLoggedIn
	}
	headers := map[string]string{}
	for k, v := range CopilotHeaders {
		headers[k] = v
	}
	headers["Authorization"] = "Bearer " + cred.Access
	headers["X-GitHub-Api-Version"] = CopilotAPIVersion
	enterprise := ""
	if cred.Extra != nil {
		enterprise = cred.Extra["enterprise_url"]
	}
	return auth.ModelAuth{
		Headers: headers,
		BaseURL: copilotAPIBase(cred.Access, enterprise),
	}, nil
}

// FetchCopilotModels 拉取 Copilot 可用模型（登录/刷新后同步模型清单；对齐 pi fetchGitHubCopilotModels）。
// 返回模型 id 列表（仅 picker_enabled 且 policy 非 disabled）。
// baseURL 为空时从凭证解析（proxy-ep / enterprise）；显式传入可覆盖（测试/网关场景）。
func FetchCopilotModels(ctx context.Context, cred *auth.OAuthCredential, baseURL string) ([]string, error) {
	ma, err := (&GitHubCopilotFlow{}).ToAuth(cred)
	if err != nil {
		return nil, err
	}
	if baseURL == "" {
		baseURL = ma.BaseURL
	}
	headers := map[string]string{
		"Accept":                 "application/json",
		"Authorization":          ma.Headers["Authorization"],
		"User-Agent":             CopilotUserAgent,
		"Editor-Version":         "vscode/1.107.0",
		"Editor-Plugin-Version":  "copilot-chat/0.35.0",
		"Copilot-Integration-Id": "vscode-chat",
		"X-GitHub-Api-Version":   CopilotAPIVersion,
	}
	body, err := getJSON(ctx, HTTPClient, baseURL+"/models", headers)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data []struct {
			ID     string `json:"id"`
			Picker bool   `json:"model_picker_enabled"`
			Policy struct {
				State string `json:"state"`
			} `json:"policy"`
			Capabilities struct {
				Supports struct {
					ToolCalls *bool `json:"tool_calls"`
				} `json:"supports"`
			} `json:"capabilities"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("invalid copilot models response: %w", err)
	}
	out := []string{}
	for _, m := range resp.Data {
		if m.ID == "" {
			continue
		}
		if m.Capabilities.Supports.ToolCalls != nil && !*m.Capabilities.Supports.ToolCalls {
			continue // 不支持工具调用的模型（Agent 必需）
		}
		if m.Picker && m.Policy.State != "disabled" {
			out = append(out, m.ID)
		}
	}
	return out, nil
}
