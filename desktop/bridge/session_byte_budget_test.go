package main

// 会话内存预算的**字节口径**（2026-11-18，bridge 内存爬山第 3 步）。
//
// 问题：预算以前按**会话个数**算（假设 96MB/会话），而会话成本按事件数缩放
// —— 实测一个 60 万事件的大会话常驻 248MB，是假设的 2.6 倍
// ⇒ 「5 个会话 = 480MB」的预算当场失效（实测 bridge 同时驻留 6 个会话 ≈ 1.2GB，
// 把 16G 机器的 swap 撑到 91%）。
//
// 这里钉住三件事：
//  1. ApproxBytes 的口径随内容增长（不是常数、不是零）；
//  2. **字节超预算时，即使会话数没超上限也要淘汰**（新行为）；
//  3. 会话数上限的行为不变（小会话场景下不额外淘汰）。

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/events"
)

// newTestReducerWithText 造一个持有若干文本块的 reducer。
func newTestReducerWithText(t *testing.T, blocks int, textLen int) *ViewReducer {
	t.Helper()
	red := NewViewReducer()
	body := strings.Repeat("x", textLen)
	for i := 0; i < blocks; i++ {
		runID := fmt.Sprintf("run-%d", i)
		reqID := fmt.Sprintf("req-%d", i)
		now := time.Now()
		_ = red.Apply(&events.LLMStart{
			Model: "m", RunId: runID, RequestId: reqID, Timestamp: now,
		}, EventMeta{})
		_ = red.Apply(&events.LLMEnd{
			Model: "m", Content: body, RunId: runID, RequestId: reqID, Timestamp: now,
		}, EventMeta{})
	}
	return red
}

// TestApproxBytesGrowsWithContent：估算值必须随内容增长。
func TestApproxBytesGrowsWithContent(t *testing.T) {
	small := newTestReducerWithText(t, 2, 64)
	big := newTestReducerWithText(t, 2, 64*1024)

	s := small.ApproxBytes()
	b := big.ApproxBytes()
	if s <= 0 {
		t.Fatalf("小会话估算值 = %d，应当 > 0", s)
	}
	if b <= s {
		t.Fatalf("大会话估算值 %d 应当大于小会话 %d", b, s)
	}
	t.Logf("估算: 2×64B 文本 = %d 字节；2×64KB 文本 = %d 字节", s, b)
}

// TestEvictByByteBudget：字节超预算 ⇒ 淘汰（哪怕会话数没超上限）。
func TestEvictByByteBudget(t *testing.T) {
	orig := sessionMemBudgetBytes
	sessionMemBudgetBytes = 1 // 1 字节：任何有内容的会话都超预算
	t.Cleanup(func() { sessionMemBudgetBytes = orig })

	m := &manager{}
	ws := &workspaceRuntime{sessions: map[string]*bridgeSession{}}
	old := time.Now().Add(-time.Hour).UnixNano()

	// 3 个会话（3 ≤ maxLiveSessions()=5 ⇒ 按个数不会淘汰），每个都带内容。
	for i := 0; i < 3; i++ {
		bs := &bridgeSession{id: fmt.Sprintf("b%02d", i)}
		bs.lastUsed.Store(old + int64(i))
		bs.cancel = func() {}
		bs.reducer = newTestReducerWithText(t, 1, 4096)
		ws.sessions[bs.id] = bs
	}

	m.evictIdleSessionsLocked(ws, "")

	if len(ws.sessions) >= 3 {
		t.Fatalf("字节超预算时未淘汰任何会话（会话数 %d ≤ 上限 %d，但字节账超了）",
			len(ws.sessions), maxLiveSessions())
	}
	// 最旧的空闲会话应被淘汰。
	if _, ok := ws.sessions["b00"]; ok {
		t.Fatal("最久未访问的空闲会话应被淘汰")
	}
}

// TestEvictByteBudgetKeepsBusyAndRecent：字节预算**不**越过运行中/刚访问的会话。
func TestEvictByteBudgetKeepsBusyAndRecent(t *testing.T) {
	orig := sessionMemBudgetBytes
	sessionMemBudgetBytes = 1
	t.Cleanup(func() { sessionMemBudgetBytes = orig })

	m := &manager{}
	ws := &workspaceRuntime{sessions: map[string]*bridgeSession{}}
	old := time.Now().Add(-time.Hour).UnixNano()

	mk := func(id string, mark func(*bridgeSession)) *bridgeSession {
		bs := &bridgeSession{id: id}
		bs.lastUsed.Store(old)
		bs.cancel = func() {}
		bs.reducer = newTestReducerWithText(t, 1, 4096)
		mark(bs)
		ws.sessions[id] = bs
		return bs
	}
	mk("running", func(bs *bridgeSession) { bs.running.Store(true) })
	mk("dispatch-pending", func(bs *bridgeSession) { bs.hubDispatchPending.Store(true) })
	recent := mk("recent", func(*bridgeSession) {})
	recent.lastUsed.Store(time.Now().UnixNano())

	m.evictIdleSessionsLocked(ws, "")

	for _, id := range []string{"running", "dispatch-pending", "recent"} {
		if _, ok := ws.sessions[id]; !ok {
			t.Fatalf("%s 不应被淘汰（运行中/刚访问/待派发）", id)
		}
	}
}

// TestEvictByteBudgetUnderLimitKeepsAll：字节账没超 ⇒ 不额外淘汰（小会话行为不变）。
func TestEvictByteBudgetUnderLimitKeepsAll(t *testing.T) {
	orig := sessionMemBudgetBytes
	sessionMemBudgetBytes = 1 << 30 // 1GB：远大于这些小会话
	t.Cleanup(func() { sessionMemBudgetBytes = orig })

	m := &manager{}
	ws := &workspaceRuntime{sessions: map[string]*bridgeSession{}}
	old := time.Now().Add(-time.Hour).UnixNano()
	for i := 0; i < 3; i++ {
		bs := &bridgeSession{id: fmt.Sprintf("s%02d", i)}
		bs.lastUsed.Store(old + int64(i))
		bs.cancel = func() {}
		bs.reducer = newTestReducerWithText(t, 1, 1024)
		ws.sessions[bs.id] = bs
	}
	m.evictIdleSessionsLocked(ws, "")
	if len(ws.sessions) != 3 {
		t.Fatalf("字节账没超预算时不应淘汰：剩 %d 个（期望 3）", len(ws.sessions))
	}
}
