package main

// hub_background.go：Hub 后台任务的生命周期（P2.5-06）。
//
// pumpHubQueue / expireSessionNeeds 原先都是 fire-and-forget goroutine，进程
// shutdown 时既不取消也不等待：P3b 群会话频繁创建/关闭后，旧泵可能在新会话
// 装配完成后仍在跑，甚至在关停后继续写账本。
//
// 这里给 manager 一个 Hub 级 ctx + WaitGroup：
//   - spawnHubBackground：登记后再起 goroutine，关闭后直接拒绝；
//   - stopHubBackground：先 cancel 再 Wait，超时返回 false（调用方告警）；
//   - hubBackgroundContext：同步调用路径（测试/诊断）复用同一 ctx。

import (
	"context"
	"time"
)

func (m *manager) hubBackgroundContext() (context.Context, bool) {
	if m == nil {
		return nil, false
	}
	m.hubBgMu.Lock()
	defer m.hubBgMu.Unlock()
	if m.hubBgClosing {
		return nil, false
	}
	if m.hubBgCtx == nil {
		m.hubBgCtx, m.hubBgCancel = context.WithCancel(context.Background())
	}
	return m.hubBgCtx, true
}

func (m *manager) spawnHubBackground(fn func(context.Context)) {
	if m == nil || fn == nil {
		return
	}
	m.hubBgMu.Lock()
	if m.hubBgClosing {
		m.hubBgMu.Unlock()
		return
	}
	if m.hubBgCtx == nil {
		m.hubBgCtx, m.hubBgCancel = context.WithCancel(context.Background())
	}
	ctx := m.hubBgCtx
	m.hubBgWG.Add(1)
	m.hubBgMu.Unlock()

	go func() {
		defer m.hubBgWG.Done()
		fn(ctx)
	}()
}

// stopHubBackground 取消并等待 Hub 后台任务；返回 false = 超时仍未退出。
// 幂等：重复调用只是再次 Wait；关闭后 spawnHubBackground 不再启动新任务。
func (m *manager) stopHubBackground(timeout time.Duration) bool {
	if m == nil {
		return true
	}
	m.hubBgMu.Lock()
	m.hubBgClosing = true
	cancel := m.hubBgCancel
	m.hubBgMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	done := make(chan struct{})
	go func() {
		m.hubBgWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}
