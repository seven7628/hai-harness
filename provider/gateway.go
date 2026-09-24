package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"time"
)

// OpenCode Go 网关协议要求（https://opencode.ai/docs/go/#where-can-i-use-it，2026-09 对齐）。
// 官网 "Your client should" 三条，其中两条是传输层可判定的硬要求：
//
//  1. 「Send a stable session ID in x-opencode-session for each conversation so we can
//     optimize routing and prompt caching」——缺失即 400
//     "Request is missing x-opencode-session and cannot be routed efficiently"（2026-09
//     用户实测；非流式 curl 同端点同形态复现）。
//  2. 「Identify itself with its own user agent, such as my-coding-agent/1.0, rather than a
//     generic SDK or HTTP-library name」——Go 默认 "Go-http-client/1.1"、
//     anthropic-sdk-go 的 "Anthropic/Go x.y.z" 都属于被明确点名的通用库名。
//
// 注入点选在传输层（RoundTripper）而非各协议请求构造处，原因：三协议
// （chat-completions / responses / anthropic）各自经不同 SDK 建请求，且
// chat-completions 有「SDK 通道 + minimal 自建请求通道」两条路径 —— 只有传输层能一次覆盖，
// 且天然带上 redirect/retry 后的重发请求。各 provider 构造期按 baseURL 判定是否包装
// （IsOpenCodeGateway），对其他厂商零影响。
const (
	// OpenCodeSessionHeader 会话稳定标识头。同一会话多轮必须恒同值：网关据此路由到
	// 同一副本并复用 prompt cache 前缀；每轮换值等于放弃缓存（请求仍成功）。
	OpenCodeSessionHeader = "x-opencode-session"

	// OpenCodeUserAgent 客户端自报身份（官网要求的 "my-coding-agent/1.0" 形态）。
	// 版本号仅供网关侧流量归因，不参与请求语义。
	OpenCodeUserAgent = "github.com/seven7628/hai-harness/0.1.0"
)

// IsOpenCodeGateway baseURL 是否指向 OpenCode Go 网关（域名含 opencode.ai）。
// 与 desktop/bridge isOpenCodeGateway（推理内容全量回传判定）同一口径：判据是
// 端点而非 provider 名 —— 用户把同一网关配成自定义名（如 opencode2）同样适用。
func IsOpenCodeGateway(baseURL string) bool {
	return strings.Contains(strings.ToLower(baseURL), "opencode.ai")
}

// openCodeSessionCtxKey context 传递会话 id（provider Stream 注入；transport 读它写头）。
// 子包（responses/anthropic）经 WithOpenCodeSession 写入。
type openCodeSessionCtxKey struct{}

// WithOpenCodeSession 把会话 id 放进 ctx（空 id 原样返回：交由 transport 的兜底 id 接管）。
// 供 provider 包内的 chat-completions 实现与 responses/anthropic 子包共用 ——
// 让「会话 id」沿 ctx 而非各 SDK 的请求结构体流动。
func WithOpenCodeSession(ctx context.Context, sid string) context.Context {
	if sid == "" {
		return ctx
	}
	return context.WithValue(ctx, openCodeSessionCtxKey{}, sid)
}

// OpenCodeSessionFromContext 读 ctx 里的会话 id（无则空串）。
func OpenCodeSessionFromContext(ctx context.Context) string {
	sid, _ := ctx.Value(openCodeSessionCtxKey{}).(string)
	return sid
}

// openCodeGatewayTransport 按 ctx 注入会话 id，并统一客户端 UA。
type openCodeGatewayTransport struct {
	base http.RoundTripper

	// fallbackSession 会话 id 缺省时的兜底（每个 provider 实例构造期生成一次）。
	// 网关对缺该头的请求直接 400 拒绝整轮对话，而 SessionID 并非所有调用路径都下发
	//（标题生成、压缩调用等旁路请求）。宁可送一个路由次优但稳定的 id，也不让请求失败。
	fallbackSession string
}

func (t *openCodeGatewayTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	sid := OpenCodeSessionFromContext(req.Context())
	if sid == "" {
		sid = t.fallbackSession
	}
	if sid != "" {
		req.Header.Set(OpenCodeSessionHeader, sid)
	}
	// 无条件覆盖 UA：SDK 自带的通用名正是官网要求避免的形态，请求链路上没有需要
	// 保留的合法既有值（调用方显式设置的 UA 亦让位于网关准入要求）。
	req.Header.Set("User-Agent", OpenCodeUserAgent)
	return t.base.RoundTrip(req)
}

// OpenCodeGatewayClient 用协议头注入 transport 包装 client（仅网关端点调用；
// 非网关端点由调用方提前判定，对其他厂商零影响）。
// 保留入参 client 的 Timeout（默认 client 为 0 = 不限，与流式请求语义一致 ——
// 请求级时限由上层 ctx 与 HTTP ResponseHeaderTimeout 控制）。
func OpenCodeGatewayClient(client *http.Client) *http.Client {
	base := http.DefaultTransport
	var timeout time.Duration
	if client != nil {
		timeout = client.Timeout
		if client.Transport != nil {
			base = client.Transport
		}
	}
	return &http.Client{
		Transport: &openCodeGatewayTransport{base: base, fallbackSession: NewOpenCodeSessionID()},
		Timeout:   timeout,
	}
}

// NewOpenCodeSessionID 生成会话 id（随机；跨进程不重复即可满足网关「stable per
// conversation」要求）。bridge 的真实会话 id 由上层经 StreamRequest.SessionID 下发，
// 本函数只服务缺省兜底。
func NewOpenCodeSessionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "go-code-session-unknown" // 熵源异常：仍给出稳定非空值（网关只要求非空）
	}
	return "go-code-" + hex.EncodeToString(b)
}
