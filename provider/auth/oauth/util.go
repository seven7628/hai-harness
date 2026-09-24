// Package oauth 各厂商 OAuth 登录流程实现（对齐 pi packages/ai/src/auth/oauth/）。
//
// 每个厂商一个文件：client_id / 端点 / scope 常量内聚；只依赖 provider/auth 基础设施
// （PKCE / loopback 回调 / device 轮询），不依赖任何厂商 SDK。
package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/provider/auth"
)

// --- 通用 HTTP 工具（各 flow 复用）---

// postForm POST application/x-www-form-urlencoded，返回响应体。
func postForm(ctx context.Context, client *http.Client, tokenURL string, form url.Values, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return doRequest(client, req)
}

// postJSON POST application/json，返回响应体。
func postJSON(ctx context.Context, client *http.Client, url string, body any, headers map[string]string) ([]byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(b)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return doRequest(client, req)
}

// getJSON GET 请求，返回响应体。
func getJSON(ctx context.Context, client *http.Client, url string, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return doRequest(client, req)
}

func doRequest(client *http.Client, req *http.Request) ([]byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB 上限
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, &httpError{Status: resp.StatusCode, Message: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))}
	}
	return body, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// newCredential 构造凭证：expires_in 秒 → Unix 毫秒，提前 5 分钟（对齐 pi 统一 -5min）。
func newCredential(refresh, access string, expiresIn int) *auth.OAuthCredential {
	expires := int64(^uint64(0) >> 1) // MaxInt64 = 永不过期
	if expiresIn > 0 {
		expires = time.Now().Add(time.Duration(expiresIn)*time.Second - 5*time.Minute).UnixMilli()
	}
	return &auth.OAuthCredential{
		Type:    "oauth",
		Refresh: refresh,
		Access:  access,
		Expires: expires,
	}
}

// parseTokenResponse 解析标准 token 端点 JSON（access_token/refresh_token/expires_in）。
func parseTokenResponse(body []byte) (*auth.OAuthCredential, error) {
	var t struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, fmt.Errorf("token exchange returned invalid JSON: %v", err)
	}
	if t.AccessToken == "" {
		return nil, errors.New("token exchange returned no access_token")
	}
	return newCredential(t.RefreshToken, t.AccessToken, t.ExpiresIn), nil
}

// HTTPClient 各 flow 共用的 HTTP 客户端（30s 超时；var 便于测试替换为 TLS 信任 client）。
var HTTPClient = func() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}()

// FormatError 对齐 pi formatErrorDetails：带 code/errno/cause 的错误详情。
func FormatError(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
