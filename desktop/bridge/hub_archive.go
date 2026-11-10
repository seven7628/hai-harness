package main

// hub_archive.go：Hub 冷归档后台触发（P4-04b-2b）。
//
// 只在 hubStore() 打开存储后启动；第一轮立即跑，之后按 interval（默认 5 分钟）
// 周期跑。归档失败每个不同错误只发一条 hub_alert（同一错误持续时去重），成功
// 不发 UI 事件。归档是存储维护，不新增工具，也不删除任何消息。
//
// 生命周期复用 hub_background.go：spawnHubBackground 登记 goroutine，
// closeHubRuntimes → stopHubBackground(5s) 取消并等待，退出时清 running。

import (
	"context"
	"time"
)

const hubArchiveDefaultInterval = 5 * time.Minute

// ensureHubArchiveLoop 幂等启动归档循环：同一个 FileStore 生命周期内只跑一个。
// 注意：spawnHubBackground 可能因关停返回 false，此时必须把 running 复位，
// 否则同进程重开 hub runtime 后永远起不来。
func (m *manager) ensureHubArchiveLoop() {
	if m == nil {
		return
	}
	m.hubArchiveMu.Lock()
	if m.hubArchiveRunning {
		m.hubArchiveMu.Unlock()
		return
	}
	m.hubArchiveRunning = true
	m.hubArchiveMu.Unlock()

	started := m.spawnHubBackground(func(ctx context.Context) {
		defer func() {
			m.hubArchiveMu.Lock()
			m.hubArchiveRunning = false
			m.hubArchiveMu.Unlock()
		}()
		m.runHubArchiveOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		ticker := time.NewTicker(m.hubArchiveIntervalValue())
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.runHubArchiveOnce(ctx)
			}
		}
	})
	if !started {
		m.hubArchiveMu.Lock()
		m.hubArchiveRunning = false
		m.hubArchiveMu.Unlock()
	}
}

func (m *manager) hubArchiveIntervalValue() time.Duration {
	m.hubArchiveMu.Lock()
	d := m.hubArchiveInterval
	m.hubArchiveMu.Unlock()
	if d <= 0 {
		return hubArchiveDefaultInterval
	}
	return d
}

// runHubArchiveOnce 直接读 m.hubFS（不调用 hubStore，避免从后台循环递归启动自己）。
func (m *manager) runHubArchiveOnce(ctx context.Context) {
	if m == nil || ctx.Err() != nil {
		return
	}
	m.mu.Lock()
	st := m.hubFS
	m.mu.Unlock()
	if st == nil {
		return
	}
	if _, err := st.ArchiveIfNeeded(ctx); err != nil {
		if ctx.Err() != nil {
			return // 关停取消不是归档故障，不发告警
		}
		m.reportHubArchiveFailure(err)
		return
	}
	m.hubArchiveMu.Lock()
	m.hubArchiveLastErr = ""
	m.hubArchiveMu.Unlock()
}

// reportHubArchiveFailure 按错误字符串去重：同一次故障只发一条 hub_alert。
func (m *manager) reportHubArchiveFailure(err error) {
	if m == nil || err == nil {
		return
	}
	msg := err.Error()
	m.hubArchiveMu.Lock()
	if m.hubArchiveLastErr == msg {
		m.hubArchiveMu.Unlock()
		return
	}
	m.hubArchiveLastErr = msg
	m.hubArchiveMu.Unlock()
	m.emitHubAlert(hubAlertEvent{
		Scope:    "hub",
		Code:     "hub_archive_failed",
		Severity: "error",
		Message:  "Hub 冷归档失败: " + msg,
	})
}
