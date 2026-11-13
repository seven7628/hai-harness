package main

// P6-15 / B1：waiting_human 之后队列必须还能泵动。
//
// 复现（Review B1）：PairLimit=1，A@B 成功 → B@A 命中 pair 限 → 全部 skipped →
// waiting_human；随后另一条在飞 run 回来 @D，Offer 成功入队，但状态停在
// waiting_human 而调度器只认 running → D 永远不唤醒。
//
// 两条独立的回归：
//  1. Offer 只要真的入队就把 waiting_human 拉回 running（状态回归）；
//  2. 调度判据放宽为「running / waiting_human 且队列非空」（第二道保险）——
//     429 退避把 wake 退回队列时状态仍是 waiting_human，这时也必须投得出去。

import (
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
)

// waitingHumanFixture 搭一个「pair 到限 → waiting_human」的群：
// A→B 成功投递一次（吃掉 PairLimit=1 的额度），B→A 再 @ 就全部 skipped。
func waitingHumanFixture(t *testing.T) (m *manager, g *agenthub.Group, a, b, d agenthub.AgentProfile, fd *convProvider) {
	t.Helper()
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{
		Cooldown: -1, PairLimit: 1, PairWindow: 10 * time.Minute, IdleReset: -1,
	})
	a = createHubAgent(t, m, buf, 1, "等A")
	b = createHubAgent(t, m, buf, 2, "等B")
	d = createHubAgent(t, m, buf, 3, "等D")
	g = convGroupWith(t, m, buf, 10, "等待群", nil, a, b, d)
	fd = newConvProvider("收到")
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(d.ID), string(g.ID)), g, string(d.ID), fd)
	return m, g, a, b, d, fd
}

// usePairLocked 把 a↔b 的额度用满一次（Offer + BeginDelivery + CommitDelivery）。
func usePairLocked(t *testing.T, ctrl *agenthub.ConversationController, g *agenthub.Group, a, b agenthub.AgentProfile) {
	t.Helper()
	wakes, err := ctrl.Offer(agenthub.ConversationOffer{
		GroupID: g.ID, Sender: a.ID, Targets: []agenthub.AgentID{b.ID},
		Text: "第一句", Depth: 2, TriggerSeq: 3, RoundID: "r1",
	})
	if err != nil || len(wakes) != 1 {
		t.Fatalf("首次 @ 应入队: %v %+v", err, wakes)
	}
	w, ok := ctrl.BeginDelivery(wakes[0], time.Time{})
	if !ok {
		t.Fatal("首次投递应成功")
	}
	ctrl.CommitDelivery(w, time.Time{})
	if n := ctrl.PairCount(g.ID, a.ID, b.ID); n != 1 {
		t.Fatalf("pair 额度没被用掉: %d", n)
	}
}

// skipToWaitingHuman 让 b→a 命中 pair 限 → 全部 skipped → waiting_human。
func skipToWaitingHuman(t *testing.T, ctrl *agenthub.ConversationController, g *agenthub.Group, a, b agenthub.AgentProfile) {
	t.Helper()
	if wakes, err := ctrl.Offer(agenthub.ConversationOffer{
		GroupID: g.ID, Sender: b.ID, Targets: []agenthub.AgentID{a.ID},
		Text: "回一句", Depth: 2, TriggerSeq: 5, RoundID: "r3",
	}); err != nil || len(wakes) != 0 {
		t.Fatalf("到限必须全部跳过: %v %+v", err, wakes)
	}
}

// TestHubConversationWaitingHumanStillPumps：waiting_human + 队列非空（429 退避
// 退回队列）时，泵必须真的把队首投出去（B1 第二道保险）。
func TestHubConversationWaitingHumanStillPumps(t *testing.T) {
	m, g, a, b, d, fd := waitingHumanFixture(t)
	ctrl := m.hubConversation()
	ctrl.BeginHuman(g.ID, 1, time.Time{})
	ctrl.StartHumanRound(g.ID, 1, time.Time{})
	usePairLocked(t, ctrl, g, a, b)

	// D 的 wake 先进队并投成 running（等下用它做「429 退回队列」）。
	dwakes, err := ctrl.Offer(agenthub.ConversationOffer{
		GroupID: g.ID, Sender: a.ID, Targets: []agenthub.AgentID{d.ID},
		Text: "给 D 的活", Depth: 2, TriggerSeq: 4, RoundID: "r2",
	})
	if err != nil || len(dwakes) != 1 {
		t.Fatalf("给 D 的 @ 应入队: %v %+v", err, dwakes)
	}
	dw, ok := ctrl.BeginDelivery(dwakes[0], time.Time{})
	if !ok {
		t.Fatal("D 的首次投递应成功")
	}

	skipToWaitingHuman(t, ctrl, g, a, b)
	if v := convView(m, g.ID); v.Status != agenthub.ConversationWaitingHuman {
		t.Fatalf("status = %q, want waiting_human", v.Status)
	}

	// 429 退避把 D 退回队列（ReadyAt 已过 = 立刻可跑）：状态仍是 waiting_human，
	// 但队列非空 —— 调度器必须取得到它。
	if !ctrl.RequeueDelivery(dw, agenthub.DeliveryReasonRateLimit, time.Now().Add(-time.Second), time.Time{}) {
		t.Fatal("退回队列失败")
	}
	if v := convView(m, g.ID); v.Status != agenthub.ConversationWaitingHuman || v.QueueLen != 1 {
		t.Fatalf("退回队列后 = %+v, want waiting_human/queue=1", v)
	}
	if wakes := ctrl.DueWakes(g.ID, time.Now()); len(wakes) != 1 {
		t.Fatalf("B1：waiting_human 且队列非空时 DueWakes 必须给出 wake: %+v", wakes)
	}
	if _, ok := ctrl.NextReadyAt(g.ID); !ok {
		t.Fatal("B1：NextReadyAt 必须给得出（否则泵不会挂定时器）")
	}
	if _, ok := ctrl.Peek(g.ID); !ok {
		t.Fatal("B1：Peek 必须看得到队首")
	}

	m.pumpHubConversation(g.ID)
	convWait(t, "waiting_human 队列里的 wake 必须被真的投出去", func() bool { return fd.callCount() >= 1 })
	convWait(t, "投出去之后队列排空", func() bool { return convView(m, g.ID).QueueLen == 0 })
}

// TestHubConversationOfferAfterWaitingHumanWakesTarget：waiting_human 之后另一条
// 在飞 run 回来 @D（走 bridge 的真实入口 offerGroupWakes）必须把状态拉回 running
// 并真的唤醒 D（B1 状态回归 + 泵）。
func TestHubConversationOfferAfterWaitingHumanWakesTarget(t *testing.T) {
	m, g, a, b, d, fd := waitingHumanFixture(t)
	ctrl := m.hubConversation()
	ctrl.BeginHuman(g.ID, 1, time.Time{})
	ctrl.StartHumanRound(g.ID, 1, time.Time{})
	usePairLocked(t, ctrl, g, a, b)
	skipToWaitingHuman(t, ctrl, g, a, b)
	if v := convView(m, g.ID); v.Status != agenthub.ConversationWaitingHuman || v.QueueLen != 0 {
		t.Fatalf("到限后 = %+v, want waiting_human/queue=0", v)
	}

	// 另一条在飞 run（发起者 A）回来 @D：这就是 AgentEnd → finishHubReply 的入口。
	trigger := &agenthub.Message{GroupID: g.ID, Seq: 9, Text: "换个对手"}
	wakes, err := m.offerGroupWakes(g, agenthub.Round{
		GroupID: g.ID, ID: "r4", TriggerSeq: 9, Targets: []agenthub.AgentID{d.ID},
	}, trigger, a.ID, 2, "")
	if err != nil || len(wakes) != 1 {
		t.Fatalf("waiting_human 之后必须仍能入队: %v %+v", err, wakes)
	}
	v := convView(m, g.ID)
	if v.Status != agenthub.ConversationRunning || v.QueueLen != 1 {
		t.Fatalf("入队后 = %+v, want running/queue=1（waiting_human 必须被拉回）", v)
	}
	// hubOnRunEnd 在 AgentEnd 之后补一次泵。
	m.pumpHubConversation(g.ID)
	convWait(t, "waiting_human 之后 @D 必须真的唤醒 D", func() bool { return fd.callCount() >= 1 })
	convWait(t, "D 投出去之后队列排空", func() bool { return convView(m, g.ID).QueueLen == 0 })
}

// TestHubRunTimeoutTimersClearedOnShutdown：N6 —— 关停要撤掉所有 run 超时定时器并
// 清空 map（定时器闭包持有 manager，关停后再 fire 只会白跑一轮）。
func TestHubRunTimeoutTimersClearedOnShutdown(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1, RunTimeout: time.Minute})
	a := createHubAgent(t, m, buf, 1, "超时A")
	b := createHubAgent(t, m, buf, 2, "超时B")
	g := convGroupWith(t, m, buf, 10, "超时群", nil, a, b)

	count := func() int {
		m.hubRunTimerMu.Lock()
		defer m.hubRunTimerMu.Unlock()
		return len(m.hubRunTimers)
	}
	m.armHubRunTimeout(g.ID, a.ID, "wake_timeout_a")
	m.armHubRunTimeout(g.ID, b.ID, "wake_timeout_b")
	if n := count(); n != 2 {
		t.Fatalf("挂上的定时器 = %d, want 2", n)
	}
	// 直接清：map 空 + 到点不再 fire。
	m.clearAllHubRunTimeouts()
	if n := count(); n != 0 {
		t.Fatalf("clearAllHubRunTimeouts 之后 = %d, want 0", n)
	}
	// 关停路径也要清（closeHubRuntimes 的第一件事）。
	m.armHubRunTimeout(g.ID, a.ID, "wake_timeout_c")
	if n := count(); n != 1 {
		t.Fatalf("重挂后 = %d, want 1", n)
	}
	m.closeHubRuntimes()
	if n := count(); n != 0 {
		t.Fatalf("N6：关停后定时器 map 没清空（%d 条）", n)
	}
}
