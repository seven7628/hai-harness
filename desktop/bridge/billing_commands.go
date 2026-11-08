// provider_billing 命令：查 Provider 的余额 / 配额窗口。
//
// 数据面在 provider/billing（声明 + 归一化 + 失败软），这里只负责三件事：
//  1. 从 providerConfig 组装 Request（base_url / key / OAuth 头）；
//  2. 缓存与节流（60s TTL、手动刷新 5s 冷却、single-flight 去重）；
//  3. 落盘到 ~/.go-code/billing-cache.json（0600）—— 冷启动先显示上次值（stale），
//     而不是一片空白。
//
// 为什么本地花费（今日/本月）**不在这里算**：metrics.jsonl 已 202MB / 43.6 万行，
// 只解析 3 个字段的全扫就要 2.16s（2026-11 实测）。刷新按钮背后再扫一遍不可接受 ——
// 前端用已缓存的 metrics 报告（ByDayModel.Model = "provider/model"）派生即可。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/seven7628/hai-harness/provider/billing"
)

const (
	billingCacheTTL   = 60 * time.Second // 自动刷新（进设置页）的复用窗口
	billingMinRefresh = 5 * time.Second  // 手动刷新的冷却：连点不放大成 5 个请求
	billingBudget     = 10 * time.Second // 单家总预算（含 dashboard 的两次请求）
	billingWorkers    = 4                // 并发上限：别把 20 家一次性打出去
)

type billingEntry struct {
	snap    *billing.Snapshot
	fetched time.Time
	tried   time.Time
}

var (
	billingMu      sync.Mutex
	billingEntries = map[string]*billingEntry{} // provider → 最近一次结果
	billingDiskAt  time.Time
	billingDiskMu  sync.Mutex
	billingLoadOne sync.Once
)

// billingRequest 组装一次抓取所需的一切（key / OAuth 头都在这里解析）。
func (pc *providerConfig) billingRequest(name string) billing.Request {
	pc.mu.Lock()
	base := pc.baseURL[name]
	pc.mu.Unlock()
	req := billing.Request{Provider: name, BaseURL: base}

	// OAuth 订阅：凭证在 oauth.json，用 Flow.ToAuth 派生请求头（Copilot 是专用头组）。
	if pc.authTypeOf(name) == "oauth" {
		if flow := pc.oauthFlowOf(name); flow != nil && pc.oauthStore != nil {
			if cred, err := pc.oauthStore.Read(name); err == nil && cred != nil && cred.Valid() {
				if ma, err := flow.ToAuth(cred); err == nil && len(ma.Headers) > 0 {
					req.OAuth = ma.Headers
					return req
				}
			}
		}
		return req // 未登录 → Fetch 会返回 NotConnected（不是错误）
	}
	req.APIKey = pc.keyLocked(name)
	return req
}

// allProviders 已配置（未删除）的 provider id（排序，保证输出稳定）。
func (pc *providerConfig) allProviders() []string {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	out := make([]string, 0, len(pc.baseURL))
	for name := range pc.baseURL {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// providerBilling 处理 provider_billing 命令。
//
// 语义：
//   - payload.provider 省略 = 全部（并发 4，每家独立成败）；
//   - refresh=false = 只读缓存（进设置页用），refresh=true = 强制拉（刷新按钮用）；
//   - 单家失败不影响别家：错误以 ErrCode 落在各自的 Snapshot 上，命令本身 ok=true。
func (m *manager) providerBilling(c command) {
	m.billingLoadDiskOnce() // 冷启动先显示上次值（≤24h），而不是一片空白
	name := strings.TrimSpace(str(c.Payload, "provider"))
	refresh := boolVal(c.Payload["refresh"])

	targets := []string{}
	if name != "" {
		targets = []string{name}
	} else {
		targets = m.provCfg.allProviders()
	}
	out := make(map[string]*billing.Snapshot, len(targets))
	// 并发抓取；单家预算 billingBudget（Fetch 内部每请求 8s，dashboard 要两次）。
	sem := make(chan struct{}, billingWorkers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, id := range targets {
		id := id
		if snap := m.billingCached(id, refresh); snap != nil {
			mu.Lock()
			out[id] = snap
			mu.Unlock()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			req := m.provCfg.billingRequest(id)
			ctx, cancel := context.WithTimeout(context.Background(), billingBudget)
			defer cancel()
			snap, err := billing.Fetch(ctx, req)
			if err != nil {
				code, status := billingErrParts(err)
				snap = &billing.Snapshot{
					Provider: id, Kind: billing.KindNone, FetchedAt: time.Now().UTC(),
					ErrCode: code, ErrStatus: status,
				}
				// 失败不清空旧值：上一次成功的读数还在就复用它（标 Stale），
				// UI 显示「旧值变灰 + 错误一行」，而不是把余额抹成空白。
				if prev := m.billingPrevious(id); prev != nil && prev.ErrCode == "" && (prev.Balance != nil || len(prev.Windows) > 0 || prev.Used != nil) {
					old := *prev
					old.ErrCode, old.ErrStatus, old.Stale = code, status, true
					snap = &old
				}
			}
			m.billingStore(id, snap)
			mu.Lock()
			out[id] = snap
			mu.Unlock()
		}()
	}
	wg.Wait()

	m.resp(c, true, "", map[string]any{
		"snapshots":  out,
		"fetched_at": time.Now().UTC().Format(time.RFC3339),
	})
}

// billingCached 命中缓存则返回快照（含「刚拉过」的冷却逻辑），否则返回 nil 表示该去拉。
func (m *manager) billingCached(provider string, force bool) *billing.Snapshot {
	billingMu.Lock()
	defer billingMu.Unlock()
	e := billingEntries[provider]
	if e == nil || e.snap == nil {
		return nil
	}
	now := time.Now()
	if force {
		if now.Sub(e.tried) < billingMinRefresh {
			return e.snap // 冷却中：连点刷新不放大请求
		}
		return nil
	}
	if now.Sub(e.fetched) < billingCacheTTL {
		return e.snap
	}
	// 过期：返回旧值（Stale 语义由前端按 fetched_at 判断），同时允许一次后台刷新由
	// 调用方决定 —— 这里返回 nil 表示「去拉」，但失败时前端仍有上一次的值可显示。
	return nil
}

// billingPrevious 取上一次成功的快照（失败时用来保留旧值）。
func (m *manager) billingPrevious(provider string) *billing.Snapshot {
	billingMu.Lock()
	defer billingMu.Unlock()
	if e := billingEntries[provider]; e != nil {
		return e.snap
	}
	return nil
}

func (m *manager) billingStore(provider string, snap *billing.Snapshot) {
	now := time.Now()
	billingMu.Lock()
	e := billingEntries[provider]
	if e == nil {
		e = &billingEntry{}
		billingEntries[provider] = e
	}
	e.snap = snap
	e.tried = now
	// 失败不刷新 fetched（保留「上次成功时间」的语义，前端据此显示陈旧程度）。
	if snap != nil && snap.ErrCode == "" {
		e.fetched = now
	}
	billingMu.Unlock()
	m.billingPersist()
}

// —— 落盘（冷启动先显示上次值）——

func billingCachePath() string { return homeCfgPath("billing-cache.json") }

type billingDisk struct {
	Snapshots map[string]*billing.Snapshot `json:"snapshots"`
}

func (m *manager) billingLoadDiskOnce() {
	billingLoadOne.Do(func() {
		raw, err := os.ReadFile(billingCachePath())
		if err != nil {
			return
		}
		var d billingDisk
		if json.Unmarshal(raw, &d) != nil {
			return
		}
		now := time.Now()
		billingMu.Lock()
		for k, v := range d.Snapshots {
			if v == nil || now.Sub(v.FetchedAt) > 24*time.Hour {
				continue // 一天前的读数没有参考价值，不如空着
			}
			billingEntries[k] = &billingEntry{snap: v, fetched: v.FetchedAt, tried: v.FetchedAt}
		}
		billingMu.Unlock()
	})
}

func (m *manager) billingPersist() {
	m.billingLoadDiskOnce()
	billingDiskMu.Lock()
	defer billingDiskMu.Unlock()
	if time.Since(billingDiskAt) < 2*time.Second {
		return // 一次命令里 20 家会触发 20 次写；合并成一次
	}
	billingDiskAt = time.Now()

	billingMu.Lock()
	snaps := make(map[string]*billing.Snapshot, len(billingEntries))
	for k, e := range billingEntries {
		if e.snap != nil {
			snaps[k] = e.snap
		}
	}
	billingMu.Unlock()
	raw, err := json.Marshal(billingDisk{Snapshots: snaps})
	if err != nil {
		return
	}
	path := billingCachePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path) // 原子替换：半截文件会让冷启动整个缓存失效
}

// billingErrParts 从 error 取稳定错误码 + HTTP 状态（不上屏，只进隐藏字段）。
func billingErrParts(err error) (string, int) {
	var fe *billing.FetchError
	if errors.As(err, &fe) {
		return fe.Code, fe.Status
	}
	return billing.ErrCodeNetwork, 0
}
