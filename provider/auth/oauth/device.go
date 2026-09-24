package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/seven7628/hai-harness/provider/auth"
)

// DeviceFlowConfig device code flow 的厂商参数。
type DeviceFlowConfig struct {
	// DeviceCodeURL 设备授权端点（POST，返回 device_code/user_code/verification_uri/...）。
	DeviceCodeURL string
	// TokenURL 轮询端点（POST）。
	TokenURL string
	// ClientID OAuth 客户端 id。
	ClientID string
	// Scope 请求的 scope（可为空）。
	Scope string
	// ExtraForm 附加表单字段。
	ExtraForm url.Values
	// ExtraHeaders 附加请求头（如 Copilot 的专用 UA）。
	ExtraHeaders map[string]string
	// DeviceCodeTimeout 设备码总超时（秒）；0 = 15 分钟。
	DeviceCodeTimeout int
	// PollInterval 服务端建议间隔（秒）；0 = 默认 5s。
	PollInterval int
}

// StartDeviceFlow 发起设备授权（RFC 8628 §3.1），返回 device_code。
func StartDeviceFlow(ctx context.Context, cfg DeviceFlowConfig, interaction auth.Interaction) (deviceCode string, err error) {
	form := url.Values{}
	form.Set("client_id", cfg.ClientID)
	if cfg.Scope != "" {
		form.Set("scope", cfg.Scope)
	}
	for k, v := range cfg.ExtraForm {
		form.Set(k, v[0])
	}

	body, err := postForm(ctx, HTTPClient, cfg.DeviceCodeURL, form, cfg.ExtraHeaders)
	if err != nil {
		return "", fmt.Errorf("device code request failed: %w", err)
	}
	var d struct {
		DeviceCode       string `json:"device_code"`
		UserCode         string `json:"user_code"`
		VerificationURI  string `json:"verification_uri"`
		VerificationURIC string `json:"verification_uri_complete"`
		Interval         int    `json:"interval"`
		ExpiresIn        int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return "", fmt.Errorf("invalid device code response JSON: %w", err)
	}
	if d.DeviceCode == "" || d.UserCode == "" || d.VerificationURI == "" {
		return "", errors.New("invalid device code response fields")
	}
	// 验证 verification_uri 必须 http(s)（防恶意端点让 open 执行可执行文件，对齐 pi）
	if !trustedHTTPURL(d.VerificationURI) {
		return "", errors.New("untrusted verification_uri in device code response")
	}

	if err := interaction.Notify(auth.AuthEvent{
		Type:             auth.EventDeviceCode,
		UserCode:         d.UserCode,
		VerificationURI:  d.VerificationURI,
		VerificationURIC: d.VerificationURIC,
		IntervalSeconds:  d.Interval,
		ExpiresInSeconds: d.ExpiresIn,
		Instructions:     "Open the verification URL in your browser and enter the code.",
	}); err != nil {
		return "", err
	}
	return d.DeviceCode, nil
}

// PollDeviceToken 轮询 token 端点（RFC 8628 §3.3）直到 complete/failed/超时。
// 返回（access_token, refresh_token, expires_in_seconds）。
func PollDeviceToken(ctx context.Context, cfg DeviceFlowConfig, deviceCode string) (access, refresh string, expiresIn int, err error) {
	timeout := cfg.DeviceCodeTimeout
	if timeout <= 0 {
		timeout = 15 * 60
	}
	interval := cfg.PollInterval
	if interval <= 0 {
		interval = 5
	}

	// PollDeviceFlow 的 Poll 回调在单 goroutine 内串行调用，闭包捕获安全。
	token, err := auth.PollDeviceFlow(ctx, auth.DevicePollOptions{
		IntervalSeconds:     interval,
		ExpiresInSeconds:    timeout,
		WaitBeforeFirstPoll: true,
		Poll: func(ctx context.Context) (auth.DevicePollResult, error) {
			form := url.Values{}
			form.Set("client_id", cfg.ClientID)
			form.Set("device_code", deviceCode)
			form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
			body, err := postForm(ctx, HTTPClient, cfg.TokenURL, form, cfg.ExtraHeaders)
			if err != nil {
				var he *httpError
				if errors.As(err, &he) && he.Status == http.StatusBadRequest {
					// 400 通常是 pending/slow_down/denied（RFC 8628 §3.5）
					return auth.DevicePollResult{Status: "failed", Message: he.Message}, nil
				}
				return auth.DevicePollResult{}, err
			}
			var t struct {
				AccessToken  string `json:"access_token"`
				RefreshToken string `json:"refresh_token"`
				ExpiresIn    int    `json:"expires_in"`
				Error        string `json:"error"`
				ErrorDesc    string `json:"error_description"`
				Interval     int    `json:"interval"`
			}
			if err := json.Unmarshal(body, &t); err != nil {
				return auth.DevicePollResult{}, err
			}
			if t.AccessToken != "" {
				access, refresh, expiresIn = t.AccessToken, t.RefreshToken, t.ExpiresIn
				return auth.DevicePollResult{Status: "complete", Value: t.AccessToken}, nil
			}
			switch t.Error {
			case "authorization_pending":
				return auth.DevicePollResult{Status: "pending"}, nil
			case "slow_down":
				return auth.DevicePollResult{Status: "slow_down", IntervalSeconds: t.Interval}, nil
			case "":
				return auth.DevicePollResult{Status: "failed", Message: "invalid device token response"}, nil
			default:
				msg := "Device flow failed: " + t.Error
				if t.ErrorDesc != "" {
					msg += ": " + t.ErrorDesc
				}
				return auth.DevicePollResult{Status: "failed", Message: msg}, nil
			}
		},
	})
	if err != nil {
		return "", "", 0, err
	}
	_ = token
	return access, refresh, expiresIn, nil
}

// httpError 带状态码的 HTTP 错误。
type httpError struct {
	Status  int
	Message string
}

func (e *httpError) Error() string { return e.Message }

// trustedHTTPURL 校验 URL 为 http(s)。
func trustedHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return u.Scheme == "https" || u.Scheme == "http"
}
