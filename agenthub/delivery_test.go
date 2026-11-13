package agenthub

// P6-01：投递账本 —— 每 target queued/running/done/skipped/failed、幂等、换代丢弃。

import (
	"testing"
	"time"
)

// deliveryByID 取账本里的一条（测试用）。
func deliveryByID(t *testing.T, c *ConversationController, gid GroupID, id string) Delivery {
	t.Helper()
	d, ok := c.Delivery(gid, id)
	if !ok {
		t.Fatalf("账本里没有 wake %s", id)
	}
	return d
}

func mustOffer(t *testing.T, c *ConversationController, group GroupID, sender AgentID, targets []AgentID, depth int) []Wake {
	t.Helper()
	wakes, err := c.Offer(ConversationOffer{
		GroupID: group, Sender: sender, Targets: targets, Text: "接着说", Depth: depth,
		TriggerSeq: 7, RoundID: "round_1",
	})
	if err != nil {
		t.Fatalf("Offer: %v", err)
	}
	return wakes
}

// TestDeliveryLedgerLifecycle：一条 wake 走完 queued → running → done，
// 每一步都能从账本读到，且 Offer 就分配了幂等键。
func TestDeliveryLedgerLifecycle(t *testing.T) {
	c, _ := newTestController(t, ConversationLimits{Cooldown: -1, PairLimit: 100, MaxDepth: 100})
	gid := GroupID("g_delivery_life")
	c.BeginHuman(gid, 1, time.Time{})
	c.StartHumanRound(gid, 1, time.Time{})
	wakes := mustOffer(t, c, gid, "agent_a", []AgentID{"agent_b"}, 1)
	if len(wakes) != 1 || wakes[0].ID == "" {
		t.Fatalf("Offer 必须给每条 wake 分配 id，得到 %+v", wakes)
	}
	id := wakes[0].ID
	if d := deliveryByID(t, c, gid, id); d.Status != DeliveryQueued || d.Target != "agent_b" || d.Sender != "agent_a" {
		t.Fatalf("入队后 = %+v, want queued/agent_b/agent_a", d)
	}
	if v := c.View(gid); len(v.Deliveries) != 1 || v.Deliveries[0].WakeID != id {
		t.Fatalf("视图 deliveries = %+v, want 1 条 %s", v.Deliveries, id)
	}

	begun, fresh := c.BeginDelivery(wakes[0], time.Time{})
	if !fresh || begun.ID != id {
		t.Fatalf("首次投递必须生效：fresh=%v id=%s", fresh, begun.ID)
	}
	if d := deliveryByID(t, c, gid, id); d.Status != DeliveryRunning || d.Attempts != 1 {
		t.Fatalf("投递后 = %+v, want running/attempts=1", d)
	}

	if !c.FinishDelivery(gid, id, DeliveryDone, "", time.Time{}) {
		t.Fatal("running → done 必须生效")
	}
	if d := deliveryByID(t, c, gid, id); d.Status != DeliveryDone {
		t.Fatalf("收尾后 = %+v, want done", d)
	}
	// 终态幂等：再来一次不改写、不重复计数。
	if c.FinishDelivery(gid, id, DeliveryDone, "", time.Time{}) {
		t.Fatal("已终结的记录不该再被推进")
	}
}

// TestDeliveryBeginIsIdempotent：同一条 wake 投第二次必须 no-op —— 这是「重复投递
// 幂等」的核心保证（两个泵/断线重试同时看到空闲时，目标会话只能收到一份快照）。
func TestDeliveryBeginIsIdempotent(t *testing.T) {
	c, _ := newTestController(t, ConversationLimits{Cooldown: -1, PairLimit: 100, MaxDepth: 100})
	gid := GroupID("g_delivery_idem")
	c.BeginHuman(gid, 1, time.Time{})
	c.StartHumanRound(gid, 1, time.Time{})
	wakes := mustOffer(t, c, gid, "agent_a", []AgentID{"agent_b"}, 1)
	w := wakes[0]

	if _, fresh := c.BeginDelivery(w, time.Time{}); !fresh {
		t.Fatal("第一次投递必须生效")
	}
	// 还在跑（running）：第二次 no-op。
	if _, fresh := c.BeginDelivery(w, time.Time{}); fresh {
		t.Fatal("running 状态的重复投递必须被挡下")
	}
	// 已收尾（done）：第三次也 no-op。
	c.FinishDelivery(gid, w.ID, DeliveryDone, "", time.Time{})
	if _, fresh := c.BeginDelivery(w, time.Time{}); fresh {
		t.Fatal("done 状态的重复投递必须被挡下")
	}
	if d := deliveryByID(t, c, gid, w.ID); d.Status != DeliveryDone || d.Attempts != 1 {
		t.Fatalf("重复投递后 = %+v, want done/attempts=1（不重复计数）", d)
	}
}

// TestDeliveryBusyRequeueKeepsWake：目标忙 → 记录退回 queued（不是 failed）、
// wake 回队、attempts 累加；下一次 BeginDelivery 仍然生效。
func TestDeliveryBusyRequeueKeepsWake(t *testing.T) {
	c, _ := newTestController(t, ConversationLimits{Cooldown: -1, PairLimit: 100, MaxDepth: 100})
	gid := GroupID("g_delivery_busy")
	c.BeginHuman(gid, 1, time.Time{})
	c.StartHumanRound(gid, 1, time.Time{})
	w := mustOffer(t, c, gid, "agent_a", []AgentID{"agent_b"}, 1)[0]

	begun, fresh := c.BeginDelivery(w, time.Time{})
	if !fresh {
		t.Fatal("首次投递必须生效")
	}
	if !c.RequeueDelivery(begun, DeliveryReasonBusy, time.Time{}, time.Time{}) {
		t.Fatal("busy 必须能退回队列")
	}
	d := deliveryByID(t, c, gid, w.ID)
	if d.Status != DeliveryQueued || d.Reason != DeliveryReasonBusy || d.Attempts != 1 {
		t.Fatalf("忙退回后 = %+v, want queued/target_busy/attempts=1", d)
	}
	if v := c.View(gid); v.QueueLen != 1 {
		t.Fatalf("queue_len = %d, want 1（wake 必须回队，不能丢）", v.QueueLen)
	}
	// 再投一次：还是同一条 id，attempts 变成 2。
	if _, fresh := c.BeginDelivery(w, time.Time{}); !fresh {
		t.Fatal("回队后的 wake 必须能再次投递")
	}
	if d := deliveryByID(t, c, gid, w.ID); d.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", d.Attempts)
	}
}

// TestDeliveryStaleGenerationSkipped：旧 generation 的 wake 不得进入新对话，
// 记 skipped(stale) 且不出现在队列里（D5 / P6-05 的「旧 run 不消耗新链额度」）。
func TestDeliveryStaleGenerationSkipped(t *testing.T) {
	c, _ := newTestController(t, ConversationLimits{Cooldown: -1, PairLimit: 100, MaxDepth: 100})
	gid := GroupID("g_delivery_stale")
	c.BeginHuman(gid, 1, time.Time{})
	c.StartHumanRound(gid, 1, time.Time{})
	w := mustOffer(t, c, gid, "agent_a", []AgentID{"agent_b"}, 1)[0]

	// 人类插话：新 generation，旧 wake 作废（排队记录被丢弃）。
	c.BeginHuman(gid, 2, time.Time{})
	c.StartHumanRound(gid, 1, time.Time{})
	if v := c.View(gid); v.QueueLen != 0 {
		t.Fatalf("换代后队列 = %d, want 0", v.QueueLen)
	}
	if d, ok := c.Delivery(gid, w.ID); ok {
		t.Fatalf("换代后旧 queued 记录应被丢弃，得到 %+v", d)
	}
	// 有人硬把旧 wake 再投一次（旧 run 的 group_say 回来）：必须被挡下，并记成
	// skipped(stale_conversation) —— 不静默消失，UI 能看到「旧链被丢弃」。
	if _, fresh := c.BeginDelivery(w, time.Time{}); fresh {
		t.Fatal("旧 generation 的 wake 不该投出去")
	}
	d := deliveryByID(t, c, gid, w.ID)
	if d.Status != DeliverySkipped || d.Reason != DeliveryReasonStale {
		t.Fatalf("旧 wake 记录 = %+v, want skipped/stale_conversation", d)
	}
}

// TestDeliveryStopAndInterrupt：停止把排队记 skipped(stopped)、在跑记 failed(interrupted)。
func TestDeliveryStopAndInterrupt(t *testing.T) {
	c, _ := newTestController(t, ConversationLimits{Cooldown: -1, PairLimit: 100, MaxDepth: 100})
	gid := GroupID("g_delivery_stop")
	c.BeginHuman(gid, 1, time.Time{})
	c.StartHumanRound(gid, 2, time.Time{})
	wakes := mustOffer(t, c, gid, "agent_a", []AgentID{"agent_b", "agent_c"}, 1)
	if _, fresh := c.BeginDelivery(wakes[0], time.Time{}); !fresh {
		t.Fatal("B 投递必须生效")
	}
	c.Stop(gid, "人类停止了自动对话，等你继续", time.Time{})
	if n := c.SkipQueuedDeliveries(gid, DeliveryReasonStopped, time.Time{}); n != 1 {
		t.Fatalf("skipped queued = %d, want 1", n)
	}
	if n := c.InterruptRunningDeliveries(gid, DeliveryReasonInterrupted, time.Time{}); n != 1 {
		t.Fatalf("interrupted running = %d, want 1", n)
	}
	if d := deliveryByID(t, c, gid, wakes[0].ID); d.Status != DeliveryFailed || d.Reason != DeliveryReasonInterrupted {
		t.Fatalf("在跑的 = %+v, want failed/interrupted", d)
	}
	if d := deliveryByID(t, c, gid, wakes[1].ID); d.Status != DeliverySkipped || d.Reason != DeliveryReasonStopped {
		t.Fatalf("排队的 = %+v, want skipped/stopped", d)
	}
}

// TestDeliveryLedgerPrune：账本有界，进行中的记录永不被截掉。
func TestDeliveryLedgerPrune(t *testing.T) {
	c, _ := newTestController(t, ConversationLimits{Cooldown: -1, PairLimit: 1000, MaxDepth: 1000, TurnBudget: -1})
	gid := GroupID("g_delivery_prune")
	c.BeginHuman(gid, 1, time.Time{})
	c.StartHumanRound(gid, 1, time.Time{})
	var ids []string
	for i := 0; i < MaxConversationDeliveries+10; i++ {
		// 每条正文不同：同内容指纹护栏会把「重复同一段话」判成循环对话。
		ws, err := c.Offer(ConversationOffer{
			GroupID: gid, Sender: "agent_a", Targets: []AgentID{"agent_b"},
			Text: "第 " + time.Duration(i).String() + " 条", Depth: 1, TriggerSeq: int64(i + 1), RoundID: "round_x",
		})
		if err != nil {
			t.Fatalf("offer #%d: %v", i, err)
		}
		if len(ws) != 1 {
			t.Fatalf("offer #%d = %d wakes", i, len(ws))
		}
		ids = append(ids, ws[0].ID)
		// 每条都收尾（终态），否则队列会堆到 pair 上限。
		if _, fresh := c.BeginDelivery(ws[0], time.Time{}); fresh {
			c.FinishDelivery(gid, ws[0].ID, DeliveryDone, "", time.Time{})
		}
	}
	all := c.Deliveries(gid)
	if len(all) > MaxConversationDeliveries {
		t.Fatalf("账本 %d 条超过上限 %d", len(all), MaxConversationDeliveries)
	}
	// 最新的一定还在。
	if _, ok := c.Delivery(gid, ids[len(ids)-1]); !ok {
		t.Fatal("最新记录不该被截掉")
	}
	// 最旧的终态记录应已丢弃。
	if _, ok := c.Delivery(gid, ids[0]); ok {
		t.Fatal("最旧的终态记录应被截掉")
	}
	// 视图只带最近若干条（有界），且进行中的永不丢。
	v := c.View(gid)
	if len(v.Deliveries) > ConversationViewDeliveries {
		t.Fatalf("视图 deliveries = %d 条，超过 %d", len(v.Deliveries), ConversationViewDeliveries)
	}
}

// TestDeliveryFinishAgentOnlyTouchesRunning：run 收尾只收口该目标正在跑的记录，
// 不动别人、不动排队。
func TestDeliveryFinishAgentOnlyTouchesRunning(t *testing.T) {
	c, _ := newTestController(t, ConversationLimits{Cooldown: -1, PairLimit: 100, MaxDepth: 100})
	gid := GroupID("g_delivery_agent")
	c.BeginHuman(gid, 1, time.Time{})
	c.StartHumanRound(gid, 2, time.Time{})
	wakes := mustOffer(t, c, gid, "agent_a", []AgentID{"agent_b", "agent_c"}, 1)
	c.BeginDelivery(wakes[0], time.Time{})
	if n := c.FinishAgentDeliveries(gid, "agent_b", DeliveryDone, "", time.Time{}); n != 1 {
		t.Fatalf("收口条数 = %d, want 1", n)
	}
	if d := deliveryByID(t, c, gid, wakes[0].ID); d.Status != DeliveryDone {
		t.Fatalf("B = %+v, want done", d)
	}
	if d := deliveryByID(t, c, gid, wakes[1].ID); d.Status != DeliveryQueued {
		t.Fatalf("C 不该被动到：%+v", d)
	}
	// 再收一次：没有 running 了，返回 0（幂等）。
	if n := c.FinishAgentDeliveries(gid, "agent_b", DeliveryDone, "", time.Time{}); n != 0 {
		t.Fatalf("重复收口 = %d, want 0", n)
	}
}

// TestDeliveryFinishRequiresRunning：B2 回归 —— 账本状态机只允许
// running → 终态。queued（还没投 / 429 退回队列等退避）被 FinishDelivery 收口
// 会把退避中的 wake 覆写成终态：退避到期后 BeginDelivery no-op，这条 wake 永久
// 占住队首。running → done/failed 仍然生效（fail() 的路径不能坏）。
func TestDeliveryFinishRequiresRunning(t *testing.T) {
	c, clk := newTestController(t, ConversationLimits{
		Cooldown: -1, PairLimit: 100, MaxDepth: 100, IdleReset: -1,
	})
	gid := GroupID("g_finish_running")
	c.BeginHuman(gid, 1, time.Time{})
	c.StartHumanRound(gid, 1, time.Time{})
	wakes := mustOffer(t, c, gid, "agent_a", []AgentID{"agent_b"}, 1)
	id := wakes[0].ID

	// queued → done：必须拒绝，状态原样。
	if c.FinishDelivery(gid, id, DeliveryDone, "", time.Time{}) {
		t.Fatal("queued 不能被 FinishDelivery 直接改成 done")
	}
	if d := deliveryByID(t, c, gid, id); d.Status != DeliveryQueued {
		t.Fatalf("queued 被改写了: %+v", d)
	}
	// queued → failed：同样拒绝。
	if c.FinishDelivery(gid, id, DeliveryFailed, "x", time.Time{}) {
		t.Fatal("queued 不能被 FinishDelivery 直接改成 failed")
	}

	// running → done：允许。
	if _, fresh := c.BeginDelivery(wakes[0], time.Time{}); !fresh {
		t.Fatal("首次投递必须生效")
	}
	if !c.FinishDelivery(gid, id, DeliveryDone, "", time.Time{}) {
		t.Fatal("running → done 必须生效")
	}
	if d := deliveryByID(t, c, gid, id); d.Status != DeliveryDone {
		t.Fatalf("running → done 后 = %+v", d)
	}
	// 终态幂等：再来一次 false。
	if c.FinishDelivery(gid, id, DeliveryDone, "", time.Time{}) {
		t.Fatal("终态再收口必须是 no-op")
	}

	// 429 退避：running → queued(+ReadyAt) 之后，run 收尾（FinishDelivery done）
	// 必须拒绝 —— 这正是 Review B2 的复现形状。
	wakes2 := mustOffer(t, c, gid, "agent_c", []AgentID{"agent_d"}, 1)
	id2 := wakes2[0].ID
	w2, fresh := c.BeginDelivery(wakes2[0], time.Time{})
	if !fresh {
		t.Fatal("第二次投递必须生效")
	}
	ready := clk.Now().Add(500 * time.Millisecond)
	if !c.RequeueDelivery(w2, DeliveryReasonRateLimit, ready, time.Time{}) {
		t.Fatal("429 退回队列必须生效")
	}
	if d := deliveryByID(t, c, gid, id2); d.Status != DeliveryQueued || d.ReadyAt == nil || d.Attempts != 1 {
		t.Fatalf("退回队列后 = %+v, want queued/ReadyAt/attempts=1", d)
	}
	if c.FinishDelivery(gid, id2, DeliveryDone, "", time.Time{}) {
		t.Fatal("退避中的 queued 记录不能被 run 收尾覆写成 done")
	}
	if d := deliveryByID(t, c, gid, id2); d.Status != DeliveryQueued || d.ReadyAt == nil {
		t.Fatalf("收尾之后退避记录被改写: %+v", d)
	}
	// 退避到期后必须还能真的重投一次（attempts 增加）。
	clk.Advance(time.Second)
	due := c.DueWakes(gid, clk.Now())
	var again bool
	for _, w := range due {
		if w.ID == id2 {
			again = true
		}
	}
	if !again {
		t.Fatalf("退避到期后必须回到可投集合: %+v", due)
	}
	if _, fresh := c.BeginDelivery(w2, clk.Now()); !fresh {
		t.Fatal("退避到期后 BeginDelivery 必须生效（B2：不能被终态占住）")
	}
	if d := deliveryByID(t, c, gid, id2); d.Status != DeliveryRunning || d.Attempts != 2 {
		t.Fatalf("重投后 = %+v, want running/attempts=2", d)
	}
	// running → failed：fail() 的收口路径仍然生效。
	if !c.FinishDelivery(gid, id2, DeliveryFailed, "boom", time.Time{}) {
		t.Fatal("running → failed 必须生效")
	}
	if d := deliveryByID(t, c, gid, id2); d.Status != DeliveryFailed || d.Reason != "boom" {
		t.Fatalf("running → failed 后 = %+v", d)
	}
}
