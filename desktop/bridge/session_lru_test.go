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
