package builtin

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"github.com/seven7628/hai-harness/tools"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
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

	// client 缓存（惰性建）：连接池跨调用复用 ⇒ 连续调用不必每次重新
	// TCP + TLS 握手（改造前每次 new 一个 Client ⇒ 每次都是新连接池）。
	clientOnce sync.Once
	client     *http.Client
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
	//
	// 解析结果**直接用于连接**（不走 transport 的二次解析）：
	//   - 只解析**一次** DNS（改造前：校验解析一次 + transport 连的时候再解析一次
	//     ⇒ 每次调用多一个完整 DNS RTT，冷缓存 10–100 ms）；
	//   - 顺带**关掉 DNS rebinding 的窗口**：连接只可能落到刚校验过的那个 IP
	//     （改造前校验与连接之间有一次重新解析，攻击者能在那个窗口里换 IP）；
	//   - 解析走 ctx（net.Resolver.LookupIPAddr）⇒ DNS 卡死时 ctx 能打断
	//     （改造前的 net.LookupIP **不可中断**，工具声明的时限管不住它）。
	var dialIPs []net.IP
	if !t.AllowPrivate {
		ips, err := resolveAndValidateHTTPTarget(ctx, a.URL)
		if err != nil {
			return "", err
		}
		dialIPs = ips
	}
	// 把校验结果带进 ctx：共享的 transport 在拨号时从 ctx 取（每次调用的 IP 不同，
	// 而连接池要跨调用复用 ⇒ 两者只能这样分开）。
	ctx = context.WithValue(ctx, httpDialIPsKey{}, dialIPs)
	ctx = context.WithValue(ctx, httpHostKey{}, hostOf(a.URL))
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
	// 工具级时限落到 ctx（client 缓存 ⇒ 不能靠 client.Timeout 逐次设置）。
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	client := t.httpClient()
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	// **drain 剩余响应体**（≤ 64 KB）：不 drain 的话连接不能归还连接池 ⇒
	// 下一次调用又要重新 TCP + TLS 握手（连续 10 次调用 = 10 次完整握手）。
	// 超过 64 KB 的残余直接放弃（连接随之关闭、不复用）—— 省流量比复用更重要。
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	}()

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
	_, err := resolveAndValidateHTTPTarget(context.Background(), rawURL)
	return err
}

// resolveAndValidateHTTPTarget 解析并校验目标（SSRF 防护），返回**校验过的 IP**。
//
// 解析走 ctx（可中断）；返回的 IP 直接用于连接 ⇒ 只解析一次，且连接只可能落到
// 刚校验过的地址（DNS rebinding 的窗口被关掉）。
func resolveAndValidateHTTPTarget(ctx context.Context, rawURL string) ([]net.IP, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("url must be http(s): %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("url missing host")
	}
	// ctx 可中断的解析（net.LookupIP 不可中断 ⇒ DNS 卡死时工具的时限管不住它）。
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("dns lookup %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("dns lookup %q: no addresses", host)
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		if isBlockedIP(a.IP) {
			return nil, fmt.Errorf("目标地址 %q 解析到内网/本机地址 %s，已拒绝（SSRF 防护）", host, a.IP)
		}
		ips = append(ips, a.IP)
	}
	return ips, nil
}

// hostOf 取 URL 的 host（TLS SNI / 证书校验用）。
func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// httpDialIPsKey / httpHostKey 把「本次调用校验过的 IP」与「原始 host」带进 ctx
// （共享的 transport 在拨号时从 ctx 取 —— 连接池要跨调用复用，而每次校验出的
// IP 可能不同，两者只能这样分开）。
type httpDialIPsKey struct{}
type httpHostKey struct{}

// httpClient 返回共享的 HTTP 客户端（连接池跨调用复用）。
//
// 两个要点：
//   - **连接只落到已校验的 IP**：DialContext / DialTLSContext 从 ctx 取本次
//     校验过的 IP（不是自己再解析一次 ⇒ 只解析一次 DNS，且 DNS rebinding 的
//     窗口被关掉）；TLS 的 SNI/证书校验仍用原始 host（直连 IP 时证书名必须是域名）。
//   - **连接池跨调用复用**：改造前每次 new 一个 Client ⇒ 每次都是新连接池 ⇒
//     连续调用每次都要重新 TCP + TLS 握手。
func (t *HTTPTool) httpClient() *http.Client {
	t.clientOnce.Do(func() {
		dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		tr := &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				ip, err := validatedIPFrom(ctx, addr)
				if err != nil {
					return nil, err
				}
				return dialer.DialContext(ctx, network, ip)
			},
			// HTTPS：自己拨号 + TLS 握手（ServerName 用原始 host）。
			DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				ip, err := validatedIPFrom(ctx, addr)
				if err != nil {
					return nil, err
				}
				host, _ := ctx.Value(httpHostKey{}).(string)
				conn, err := dialer.DialContext(ctx, network, ip)
				if err != nil {
					return nil, err
				}
				tlsConn := tls.Client(conn, &tls.Config{ServerName: host})
				if err := tlsConn.HandshakeContext(ctx); err != nil {
					conn.Close()
					return nil, err
				}
				return tlsConn, nil
			},
			MaxIdleConns:        8,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		}
		t.client = &http.Client{Transport: tr}
	})
	return t.client
}

// validatedIPFrom 从 ctx 里取本次校验过的 IP，把 host:port 换成 ip:port。
// 没有校验结果（AllowPrivate 测试路径）⇒ 原样返回 addr（走系统解析）。
func validatedIPFrom(ctx context.Context, addr string) (string, error) {
	ips, _ := ctx.Value(httpDialIPsKey{}).([]net.IP)
	if len(ips) == 0 {
		return addr, nil
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", err
	}
	ip := pickDialIP(ips, host)
	if ip == nil {
		return "", fmt.Errorf("http: no validated address for host %q", host)
	}
	return net.JoinHostPort(ip.String(), port), nil
}

// pickDialIP 从已校验的 IP 里挑一个（host 匹配优先；否则取第一个）。
func pickDialIP(ips []net.IP, host string) net.IP {
	for _, ip := range ips {
		if ip.String() == host {
			return ip
		}
	}
	if len(ips) > 0 {
		return ips[0]
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
