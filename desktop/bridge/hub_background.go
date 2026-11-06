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
	"sync"
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

// resetHubBackgroundIfClosed 供 closeHubRuntimes 之后新建 hub runtime 时调用：
// 关闭只应结束旧一代后台任务，不应让同一进程永久失去泵/过期能力（P2.5-06 复审 N2）。
func (m *manager) resetHubBackgroundIfClosed() {
	if m == nil || m.hubShutdown.Load() {
		return
	}
	m.hubBgMu.Lock()
	defer m.hubBgMu.Unlock()
	if !m.hubBgClosing {
		return
	}
	m.hubBgClosing = false
	m.hubBgCtx, m.hubBgCancel = context.WithCancel(context.Background())
	m.hubBgWG = &sync.WaitGroup{} // 新 generation 独立 WG；旧 waiter 仍等旧 WG
	m.hubBgDone = nil
}

func (m *manager) spawnHubBackground(fn func(context.Context)) {
	if m == nil || fn == nil {
		return
	}
	m.hubBgMu.Lock()
	if m.hubBgClosing || m.hubShutdown.Load() {
		m.hubBgMu.Unlock()
		return
	}
	if m.hubBgCtx == nil {
		m.hubBgCtx, m.hubBgCancel = context.WithCancel(context.Background())
	}
	if m.hubBgWG == nil {
		m.hubBgWG = &sync.WaitGroup{}
	}
	ctx := m.hubBgCtx
	wg := m.hubBgWG
	wg.Add(1)
	m.hubBgMu.Unlock()

	go func() {
		defer wg.Done()
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
	// 每个 generation 只建一个 waiter：重复 stop 不累积 goroutine（P2.5-06 复审 N4）。
	m.hubBgMu.Lock()
	if m.hubBgWG == nil {
		m.hubBgWG = &sync.WaitGroup{}
	}
	wg := m.hubBgWG
	if m.hubBgDone == nil {
		m.hubBgDone = make(chan struct{})
		done := m.hubBgDone
		go func(wg *sync.WaitGroup) {
			wg.Wait()
			close(done)
		}(wg)
	}
	done := m.hubBgDone
	m.hubBgMu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}
