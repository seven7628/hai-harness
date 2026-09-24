package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/seven7628/hai-harness/tools"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultHTTPMaxBytes = 20000

// HTTPTool 发起 HTTP GET 请求（带超时，响应体截断）。
type HTTPTool struct {
	tools.BaseTool
	Timeout  time.Duration // 请求超时（默认 15s）
	MaxBytes int64         // 响应体上限（默认 20000）
	// AllowPrivate 允许访问私网/loopback 地址（默认 false）。生产关闭；测试注入
	// true 以用 httptest（127.0.0.1）验证 HTTP 功能本身。
	AllowPrivate bool
}

// NewHTTPTool 创建 HTTP GET 工具（需注册到引擎）。
func NewHTTPTool(timeout time.Duration) *HTTPTool {
	return &HTTPTool{
		BaseTool: tools.BaseTool{
			Name_:        "http_get",
			Description_: "Make an HTTP GET request and return the status code and response body — fetch URL content / call public APIs. Has a timeout (default 15s); the response body is truncated to 20KB (prompts truncated when exceeded).",
			Params_:      tools.Obj(map[string]any{"url": tools.Str("Request URL (http/https)")}, "url"),
			CanParallel_: true,
			ReadOnly_:    true,
		},
		Timeout:  timeout,
		MaxBytes: defaultHTTPMaxBytes,
	}
}

// ToolTimeout 实现 tools.ToolTimeoutProvider：声明请求时限；
// 0（未配置）交给引擎默认兜底（WithToolTimeout）。
func (t *HTTPTool) ToolTimeout() time.Duration { return t.Timeout }

type httpArgs struct {
	URL string `json:"url"`
}

func (t *HTTPTool) ValidParams(_ context.Context, _, arguments string) error {
	var a httpArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return invalidJSON("http_get", err.Error())
	}
	if !strings.HasPrefix(a.URL, "http://") && !strings.HasPrefix(a.URL, "https://") {
		return recoveryError("invalid_url", "correct_arguments", "url must start with http:// or https://", "Provide a valid HTTP(S) URL and retry.")
	}
	return nil
}

func (t *HTTPTool) Call(ctx context.Context, _, arguments string) (string, error) {
	var a httpArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", err
	}
	// SSRF 防护（V2 SECURITY-09）：解析目标 host，拒绝私网/loopback/link-local/
	// 云 metadata 地址（DNS rebinding 的第一道防线：请求前按解析结果校验）。
	// AllowPrivate=true（测试）跳过。
	if !t.AllowPrivate {
		if err := validateHTTPTarget(a.URL); err != nil {
			return "", err
		}
	}
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	maxBytes := t.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultHTTPMaxBytes
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return "", err
	}
	if int64(len(body)) > maxBytes {
		return fmt.Sprintf("HTTP %d\n%s\n...(truncated)", resp.StatusCode, body[:maxBytes]), nil
	}
	return fmt.Sprintf("HTTP %d\n%s", resp.StatusCode, body), nil
}

// validateHTTPTarget 校验 HTTP 目标不指向内网/本机（SSRF 防护）：
//   - 仅 http/https；
//   - host 解析后的所有 IP 都必须是公网（拒绝 loopback/私网/link-local/unspecified/
//     multicast 及云 metadata 保留段 169.254.169.254）。
//
// 注意：这是请求前的静态校验（DNS 解析时刻），无法完全防 DNS rebinding
// （解析后到连接间的二次解析）；完整防护需连接期校验（DialContext 里再查一次），
// 此处先补第一道防线（V2 建议的 IP 校验）。
func validateHTTPTarget(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("url must be http(s): %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("url missing host")
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("dns lookup %q: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("dns lookup %q: no addresses", host)
	}
	for _, ip := range ips {
		if isBlockedIP(ip) {
			return fmt.Errorf("目标地址 %q 解析到内网/本机地址 %s，已拒绝（SSRF 防护）", host, ip)
		}
	}
	return nil
}

// isBlockedIP 判断 IP 是否属禁止访问段（loopback/私网/link-local/unspecified/multicast）。
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	// 云 metadata 保留段（169.254.169.254 是 link-local 子集，但显式列出防误判）
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 169 && ip4[1] == 254 {
		return true
	}
	return false
}
