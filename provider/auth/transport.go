package auth

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// Transport OAuth 注入 transport：在每次请求前解析 ModelAuth 并注入请求头。
// 对齐方案 §5.2：三协议子包只看到 apiKey/HTTPClient，token 由本层透明注入。
//
// 用法：
//
//	base := http.DefaultTransport
//	tr := auth.NewTransport(store, flow, "anthropic", base)
//	deps.HTTPClient = &http.Client{Transport: tr}
//
// 行为：
//   - 未登录 / 刷新失败 → 不注入鉴权头（下游 401 由错误分类层提示「OAuth 未登录/已过期」），
//     且 LastError() 记录原因供 oauth_status 暴露；
//   - 401 响应 → 缓存失效 + 重试一次（仅 GET/无 body 请求可安全重放；刷新后带新 token 重发）。
type Transport struct {
	store  *Store
	flow   Flow
	provID string
	base   http.RoundTripper

	source *TokenSource

	mu        sync.RWMutex
	lastError error // 最近一次鉴权解析错误（未登录/刷新失败）；oauth_status 暴露用
}

// NewTransport 构造 OAuth 注入 transport。base 为 nil 时用 http.DefaultTransport。
func NewTransport(store *Store, flow Flow, providerID string, base http.RoundTripper) *Transport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &Transport{
		store:  store,
		flow:   flow,
		provID: providerID,
		base:   base,
		source: NewTokenSource(store, flow, providerID),
	}
}

// TokenSource 暴露进程内缓存（供外部失效/状态查询）。
func (t *Transport) TokenSource() *TokenSource { return t.source }

// LastError 最近一次鉴权解析错误（未登录 / 刷新失败）；无错误返回 nil。
func (t *Transport) LastError() error {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.lastError
}

func (t *Transport) setLastError(err error) {
	t.mu.Lock()
	t.lastError = err
	t.mu.Unlock()
}

// RoundTrip 实现 http.RoundTripper：解析 OAuth 鉴权 → 注入请求头 → 发送。
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	ma, err := t.source.Auth(req.Context())
	if err != nil {
		// 未登录 / 刷新失败：不注入鉴权头，让下游拿到 401 由错误分类层提示。
		// 这保证「刷新失败不静默回退 env key」（对齐 pi resolve.ts 语义）。
		// LastError 记录原因（oauth_status 可暴露「请重新登录」）。
		t.setLastError(err)
		return t.base.RoundTrip(req)
	}
	t.setLastError(nil)

	// 注入鉴权头（不覆盖调用方显式设置的头）
	for k, v := range ma.Headers {
		if req.Header.Get(k) == "" {
			req.Header.Set(k, v)
		}
	}

	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	// 401 → 缓存失效 + 强制刷新重试一次（token 可能在请求途中过期/被吊销）
	if resp.StatusCode == http.StatusUnauthorized {
		t.source.Invalidate()
		ma2, aerr := t.source.AuthForce(req.Context())
		if aerr != nil {
			// 强制刷新失败（刷新端点不可达 / token 已吊销）：记录原因供上层提示重新登录。
			// 同时把响应体替换为语义化错误——下游三协议 classify 会把它作为 APIError
			// message 呈现给用户（"OAuth 登录已过期，请重新登录"），而不是裸 401。
			t.setLastError(aerr)
			resp.Body.Close()
			body := fmt.Sprintf(`{"error":{"message":"OAuth 登录已过期或已失效（%v），请在设置中重新登录","type":"oauth_expired"}}`, oauthHint(aerr))
			resp.Body = io.NopCloser(strings.NewReader(body))
			resp.ContentLength = int64(len(body))
			resp.Header.Set("Content-Type", "application/json")
			return resp, nil
		}
		if req.Body == nil {
			resp.Body.Close()
			req2 := req.Clone(req.Context())
			// 先清除旧注入头，再写入新值（Clone 保留了第一次注入的 Authorization）
			for k := range ma.Headers {
				req2.Header.Del(k)
			}
			for k, v := range ma2.Headers {
				req2.Header.Set(k, v)
			}
			return t.base.RoundTrip(req2)
		}
	}
	return resp, nil
}

// oauthHint 把 OAuth 鉴权错误映射为用户可读的短提示（避免把内部 error 长串全量暴露）。
func oauthHint(err error) string {
	if err == nil {
		return "未登录"
	}
	msg := err.Error()
	// 只取第一行（错误链前缀），避免刷屏
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	return msg
}
