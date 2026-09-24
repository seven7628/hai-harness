package auth

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// 双检锁刷新（对齐 pi resolve.ts resolveStoredOAuth）：
// 过期判定 → 进 per-provider 锁 → 锁内复查 → 仍过期才调 Flow.Refresh → 原子持久化 → 释放。
// 并发请求只刷新一次。

// RefreshTimeout 单次刷新网络超时（pi DEFAULT_OAUTH_REFRESH_TIMEOUT_MS = 15s）。
const RefreshTimeout = 15 * time.Second

// ResolveAuth 解析某 provider 的 OAuth 请求鉴权（每次请求前调用；有 TokenSource 缓存则走缓存）。
// 未登录 → ErrNotLoggedIn；刷新失败 → ErrRefreshFailed（不静默回退 env key）。
// forceRefresh 为 true 时忽略过期判定直接刷新（401 失效场景）。
func ResolveAuth(ctx context.Context, store *Store, flow Flow, providerID string) (ModelAuth, error) {
	return resolveAuth(ctx, store, flow, providerID, false)
}

// ResolveAuthForce 强制刷新后返回鉴权（OAuth token 被服务端拒绝（401）时用）。
func ResolveAuthForce(ctx context.Context, store *Store, flow Flow, providerID string) (ModelAuth, error) {
	return resolveAuth(ctx, store, flow, providerID, true)
}

func resolveAuth(ctx context.Context, store *Store, flow Flow, providerID string, force bool) (ModelAuth, error) {
	cred, err := store.Read(providerID)
	if err != nil {
		return ModelAuth{}, err
	}
	if cred == nil || !cred.Valid() {
		return ModelAuth{}, ErrNotLoggedIn
	}
	if force || cred.ExpiresSoon(0) {
		// 锁内复查（双检锁）：只有仍过期（或强制）才真正刷新。
		err := store.Modify(providerID, func(cur *OAuthCredential) (*OAuthCredential, error) {
			if cur == nil || !cur.Valid() {
				return nil, nil // 期间被登出
			}
			if !force && !cur.ExpiresSoon(0) {
				return nil, nil // 别的请求已刷新
			}
			refreshCtx, cancel := context.WithTimeout(ctx, RefreshTimeout)
			defer cancel()
			next, err := flow.Refresh(refreshCtx, cur)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrRefreshFailed, err)
			}
			if next == nil {
				// flow 不支持刷新（如 OpenRouter 永久 key）：token 被服务端拒绝 = 已失效，需重新登录
				return nil, fmt.Errorf("%w: 登录已失效，请重新登录", ErrRefreshFailed)
			}
			return next, nil
		})
		if err != nil {
			return ModelAuth{}, err
		}
		// 刷新后（无论本请求刷新还是别的请求已刷）重读最新凭证
		cur, rerr := store.Read(providerID)
		if rerr != nil {
			return ModelAuth{}, rerr
		}
		if cur == nil || !cur.Valid() {
			return ModelAuth{}, ErrNotLoggedIn
		}
		cred = cur
	}
	return flow.ToAuth(cred)
}

// TokenSource 进程内 token 缓存：解析成功后缓存至过期前 1 分钟，
// 避免每次 Stream 都读盘 + 刷新判断；缓存失效才走 ResolveAuth 双检锁路径。
type TokenSource struct {
	store  *Store
	flow   Flow
	provID string

	mu     sync.Mutex
	cached *ModelAuth
	until  time.Time
}

// NewTokenSource 构造 TokenSource。
func NewTokenSource(store *Store, flow Flow, providerID string) *TokenSource {
	return &TokenSource{store: store, flow: flow, provID: providerID}
}

// Auth 返回请求鉴权（缓存优先）。
func (ts *TokenSource) Auth(ctx context.Context) (ModelAuth, error) {
	return ts.auth(ctx, false)
}

// AuthForce 强制刷新后返回鉴权（401 失效场景：忽略有效期直接刷新）。
func (ts *TokenSource) AuthForce(ctx context.Context) (ModelAuth, error) {
	return ts.auth(ctx, true)
}

func (ts *TokenSource) auth(ctx context.Context, force bool) (ModelAuth, error) {
	ts.mu.Lock()
	if !force && ts.cached != nil && time.Now().Before(ts.until) {
		ma := *ts.cached
		ts.mu.Unlock()
		return ma, nil
	}
	ts.mu.Unlock()

	ma, err := resolveAuth(ctx, ts.store, ts.flow, ts.provID, force)
	if err != nil {
		return ModelAuth{}, err
	}
	ts.mu.Lock()
	// 缓存至过期前 1 分钟（永不过期 token 缓存 1 小时）
	ttl := time.Hour
	if cred, _ := ts.store.Read(ts.provID); cred != nil && cred.Expires < int64(^uint64(0)>>1) {
		remaining := time.Until(time.UnixMilli(cred.Expires))
		ttl = remaining - time.Minute
		if ttl <= 0 {
			ttl = time.Second // 即将过期：立即失效走刷新
		}
	}
	ts.cached = &ma
	ts.until = time.Now().Add(ttl)
	ts.mu.Unlock()
	return ma, nil
}

// Invalidate 使缓存失效（401 时调用，下次请求走刷新路径）。
func (ts *TokenSource) Invalidate() {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.cached = nil
	ts.until = time.Time{}
}
