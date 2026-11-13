package main

import (
	"fmt"
	"testing"
	"time"
)

// TestMaxLiveSessions 预算判据：256MB ÷ 1.5MB 会算出 170，必须被 64 的句柄上限截住。
func TestMaxLiveSessions(t *testing.T) {
	if got := maxLiveSessions(); got != maxLiveSessionsHard {
		t.Fatalf("maxLiveSessions=%d want %d", got, maxLiveSessionsHard)
	}
}

// TestEvictIdleSessionsLocked 淘汰策略：
//   - 只淘汰「空闲且超过 grace」的，按最久未访问优先；
//   - 运行中的、刚访问过的、keepID 本身一律不动；
//   - 淘汰后数量降到上限。
func TestEvictIdleSessionsLocked(t *testing.T) {
	m := &manager{} // hub == nil：不触发 Session Mesh
	ws := &workspaceRuntime{sessions: map[string]*bridgeSession{}}
	old := time.Now().Add(-time.Minute).UnixNano()

	const n = maxLiveSessionsHard + 6
	for i := 0; i < n; i++ {
		bs := &bridgeSession{id: fmt.Sprintf("s%02d", i)}
		bs.lastUsed.Store(old + int64(i)) // 越大越新
		bs.cancel = func() {}
		ws.sessions[bs.id] = bs
	}
	// 一个运行中的（最旧），一个刚访问过的（最旧），都必须留下
	running := &bridgeSession{id: "running"}
	running.lastUsed.Store(old)
	running.running.Store(true)
	running.cancel = func() {}
	ws.sessions[running.id] = running

	recent := &bridgeSession{id: "recent"}
	recent.lastUsed.Store(time.Now().UnixNano())
	recent.cancel = func() {}
	ws.sessions[recent.id] = recent

	keep := ws.sessions["s05"]
	keep.lastUsed.Store(old) // 最旧，但它是本次新建的
	keep.cancel = func() {}

	m.evictIdleSessionsLocked(ws, keep.id)

	if len(ws.sessions) > maxLiveSessions() {
		t.Fatalf("淘汰后仍超上限: %d > %d", len(ws.sessions), maxLiveSessions())
	}
	for _, id := range []string{"running", "recent", keep.id} {
		if _, ok := ws.sessions[id]; !ok {
			t.Fatalf("%s 不应被淘汰", id)
		}
	}
	// 最旧的空闲会话应被淘汰
	if _, ok := ws.sessions["s00"]; ok {
		t.Fatal("最久未访问的空闲会话应被淘汰")
	}
}

// TestEvictSkipsHubBusySessions 合并主分支 LRU 后的回归：Hub 的「已 Ask 未开跑/
// 群轮未收/结算中」都不在 running 里，淘汰它们会直接丢任务。
func TestEvictSkipsHubBusySessions(t *testing.T) {
	m := &manager{}
	ws := &workspaceRuntime{sessions: map[string]*bridgeSession{}}
	old := time.Now().Add(-time.Hour).UnixNano()
	mk := func(id string, mark func(*bridgeSession)) *bridgeSession {
		bs := &bridgeSession{id: id}
		bs.lastUsed.Store(old)
		bs.cancel = func() {}
		mark(bs)
		ws.sessions[id] = bs
		return bs
	}
	mk("dispatch-pending", func(bs *bridgeSession) { bs.hubDispatchPending.Store(true) })
	mk("group-busy", func(bs *bridgeSession) { bs.hubGroupBusy.Store(true) })
	mk("settling", func(bs *bridgeSession) { bs.hubSettling.Store(true) })
	for i := 0; i < maxLiveSessions()+4; i++ {
		mk(fmt.Sprintf("idle%02d", i), func(*bridgeSession) {})
	}
	m.evictIdleSessionsLocked(ws, "")
	for _, id := range []string{"dispatch-pending", "group-busy", "settling"} {
		if _, ok := ws.sessions[id]; !ok {
			t.Fatalf("Hub 忙会话 %s 被 LRU 淘汰（任务会丢失）", id)
		}
	}
	if len(ws.sessions) > maxLiveSessions() {
		t.Fatalf("淘汰后仍超上限: %d > %d", len(ws.sessions), maxLiveSessions())
	}
}

// TestEvictSkipsWhenAllBusy 全部在跑时允许超预算，而不是打断 run。
func TestEvictSkipsWhenAllBusy(t *testing.T) {
	m := &manager{}
	ws := &workspaceRuntime{sessions: map[string]*bridgeSession{}}
	for i := 0; i < maxLiveSessions()+3; i++ {
		bs := &bridgeSession{id: fmt.Sprintf("b%02d", i)}
		bs.lastUsed.Store(time.Now().Add(-time.Hour).UnixNano())
		bs.running.Store(true)
		bs.cancel = func() {}
		ws.sessions[bs.id] = bs
	}
	before := len(ws.sessions)
	m.evictIdleSessionsLocked(ws, "")
	if len(ws.sessions) != before {
		t.Fatalf("全在跑时不应淘汰任何会话: %d -> %d", before, len(ws.sessions))
	}
}
