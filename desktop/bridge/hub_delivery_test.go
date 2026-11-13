package main

// P6-01：投递账本的 bridge 接线 —— 命令回执的 per-target deliveries、忙目标排队
// 不丢、重复投递幂等、skipped/failed 带原因、视图/事件带账本。

import (
	"context"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
)

// mustDelivery 从 hub_group_send 的回执里取某 target 的投递记录。
// 回执里的 deliveries 经过 JSON 编码（map[string]any），这里按字段还原成结构体。
func mustDelivery(t *testing.T, data map[string]any, target agenthub.AgentID) agenthub.Delivery {
	t.Helper()
	raw, _ := data["deliveries"].([]any)
	if len(raw) == 0 {
		t.Fatalf("回执缺 deliveries: %+v", data)
	}
	for _, item := range raw {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		if s, _ := m["target"].(string); s != string(target) {
			continue
		}
		return deliveryFromMap(m)
	}
	t.Fatalf("deliveries 里没有 target %s: %+v", target, raw)
	return agenthub.Delivery{}
}

// deliveryFromMap 把 JSON 解码后的 map 还原成 Delivery（只取测试关心的字段）。
func deliveryFromMap(m map[string]any) agenthub.Delivery {
	str := func(k string) string { s, _ := m[k].(string); return s }
	num := func(k string) int {
		if f, ok := m[k].(float64); ok {
			return int(f)
		}
		if n, ok := m[k].(int); ok {
			return n
		}
		return 0
	}
	d := agenthub.Delivery{
		WakeID:         str("wake_id"),
		ConversationID: str("conversation_id"),
		GroupID:        agenthub.GroupID(str("group_id")),
		Target:         agenthub.AgentID(str("target")),
		Sender:         agenthub.AgentID(str("sender")),
		Human:          m["human"] == true,
		Depth:          num("depth"),
		Turn:           num("turn"),
		Status:         agenthub.DeliveryStatus(str("status")),
		Reason:         str("reason"),
		Attempts:       num("attempts"),
	}
	if f, ok := m["trigger_seq"].(float64); ok {
		d.TriggerSeq = int64(f)
	}
	return d
}

// TestHubGroupSendReturnsPerTargetDeliveries：人 @ 两个空闲成员 → 回执里每个 target
// 一条 running 记录（含 wake_id/conversation_id/depth），命令返回 per-target deliveries。
func TestHubGroupSendReturnsPerTargetDeliveries(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1})
	a := createHubAgent(t, m, buf, 1, "投递账本A")
	b := createHubAgent(t, m, buf, 2, "投递账本B")
	c := createHubAgent(t, m, buf, 3, "投递账本C")
	g := convGroupWith(t, m, buf, 10, "投递账本群", nil, a, b, c)
	// 门闩式 fake：让两条投递都停在 running（不靠 sleep 猜时序）。
	fb := newConvProvider("B 回复")
	rb := fb.gated()
	defer close(rb)
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(b.ID), string(g.ID)), g, string(b.ID), fb)
	fc := newConvProvider("C 回复")
	rc := fc.gated()
	defer close(rc)
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(c.ID), string(g.ID)), g, string(c.ID), fc)

	ok, errMsg, data := hubCmd(t, m, buf, 20, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@投递账本B @投递账本C 一起看",
	})
	if !ok {
		t.Fatalf("send: %s", errMsg)
	}
	raw, _ := data["deliveries"].([]any)
	if len(raw) != 2 {
		t.Fatalf("deliveries = %d 条, want 2（每 target 一条）: %+v", len(raw), data)
	}
	for _, target := range []agenthub.AgentID{b.ID, c.ID} {
		d := mustDelivery(t, data, target)
		if d.Status != agenthub.DeliveryRunning {
			t.Fatalf("%s 投递状态 = %s, want running", target, d.Status)
		}
		if d.WakeID == "" || d.ConversationID == "" {
			t.Fatalf("%s 缺幂等键/对话 id: %+v", target, d)
		}
		if d.Depth != 1 || d.Turn != 1 || !d.Human {
			t.Fatalf("%s 记账字段不对: %+v", target, d)
		}
	}
	// 视图里也能看到同一本账（UI 走 hub_conversation 事件）。
	convWait(t, "视图带投递账本", func() bool {
		v := m.hubConversationView(g.ID)
		return len(v.Deliveries) == 2
	})
}

// TestHubGroupWakeDeliveryIsIdempotent：同一个 wake id 投两次只跑一轮。
// 这是「重复投递幂等」的端到端反证 —— 两个泵/断线重试同时看到空闲。
func TestHubGroupWakeDeliveryIsIdempotent(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1})
	a := createHubAgent(t, m, buf, 1, "幂等A")
	b := createHubAgent(t, m, buf, 2, "幂等B")
	g := convGroupWith(t, m, buf, 10, "幂等群", nil, a, b)
	fb := newConvProvider("B 只该跑一次")
	release := fb.gated()
	defer close(release)
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(b.ID), string(g.ID)), g, string(b.ID), fb)

	st := mustHubStore(t, m)
	ctx := context.Background()
	ctrl := m.hubConversation()
	ctrl.BeginHuman(g.ID, 1, time.Time{})
	ctrl.StartHumanRound(g.ID, 1, time.Time{})
	wakes, err := ctrl.Offer(agenthub.ConversationOffer{
		GroupID: g.ID, Human: true, Targets: []agenthub.AgentID{b.ID},
		Text: "@幂等B 只跑一次", Depth: 1, TriggerSeq: 1, RoundID: "round_idem",
	})
	if err != nil || len(wakes) != 1 {
		t.Fatalf("offer: %v (%d wakes)", err, len(wakes))
	}
	w := wakes[0]
	if _, err := m.wakeGroupAgent(ctx, st, g, w); err != nil {
		t.Fatalf("第一次投递: %v", err)
	}
	fb.waitStarted(t)
	// 第二次：同一条 wake（同 id）→ no-op，绝不能给目标会话塞第二份快照。
	if _, err := m.wakeGroupAgent(ctx, st, g, w); err != nil {
		t.Fatalf("重复投递必须是 no-op，得到错误: %v", err)
	}
	d, ok := ctrl.Delivery(g.ID, w.ID)
	if !ok || d.Status != agenthub.DeliveryRunning || d.Attempts != 1 {
		t.Fatalf("重复投递后记录 = %+v (found=%v), want running/attempts=1", d, ok)
	}
	// 目标会话只被问过一次（run 正在跑，count 恒为 1）。
	if n := fb.callCount(); n != 1 {
		t.Fatalf("provider 调用 %d 次, want 1（重复投递不能变成第二轮）", n)
	}
}

// TestHubGroupDeliveryDoneOnRunEnd：run 收尾把 running 推进到 done（AgentEnd 之后
// 的 hubOnRunEnd 收口），不留「永远在跑」的记录。
func TestHubGroupDeliveryDoneOnRunEnd(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1})
	a := createHubAgent(t, m, buf, 1, "收口A")
	b := createHubAgent(t, m, buf, 2, "收口B")
	g := convGroupWith(t, m, buf, 10, "收口群", nil, a, b)
	p3bWireFake(t, m, g, string(b.ID), "B 收到")

	ok, errMsg, data := hubCmd(t, m, buf, 20, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@收口B 跑一轮",
	})
	if !ok {
		t.Fatalf("send: %s", errMsg)
	}
	d0 := mustDelivery(t, data, b.ID)
	convWait(t, "投递记录收口到 done", func() bool {
		d, found := m.hubConversation().Delivery(g.ID, d0.WakeID)
		return found && d.Status == agenthub.DeliveryDone
	})
}

// TestHubGroupDeliveryStopReasons：停止后排队记 skipped(stopped)、在跑记
// failed(interrupted) —— skipped/failed 必须有可读原因。
func TestHubGroupDeliveryStopReasons(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1})
	a := createHubAgent(t, m, buf, 1, "停账A")
	b := createHubAgent(t, m, buf, 2, "停账B")
	c := createHubAgent(t, m, buf, 3, "停账C")
	g := convGroupWith(t, m, buf, 10, "停账群", nil, a, b, c)
	// B 卡在 run 里；C 排队（第二个 @ 由 A 发出，撞上 C 忙？不 —— 让 C 直接忙）。
	fb := newConvProvider("B 卡住")
	release := fb.gated()
	defer close(release)
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(b.ID), string(g.ID)), g, string(b.ID), fb)
	bsC := mustGroupSession(t, m, string(c.ID), string(g.ID))
	bsC.hubGroupBusy.Store(true) // C 正忙：它的 wake 只能排队
	defer bsC.hubGroupBusy.Store(false)

	ok, errMsg, data := hubCmd(t, m, buf, 20, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@停账B @停账C 都听着",
	})
	if !ok {
		t.Fatalf("send: %s", errMsg)
	}
	db := mustDelivery(t, data, b.ID)
	dc := mustDelivery(t, data, c.ID)
	if db.Status != agenthub.DeliveryRunning {
		t.Fatalf("B = %s, want running", db.Status)
	}
	if dc.Status != agenthub.DeliveryQueued || dc.Reason != agenthub.DeliveryReasonBusy {
		t.Fatalf("C = %+v, want queued/target_busy", dc)
	}

	ok, errMsg, _ = hubCmd(t, m, buf, 21, "hub_conversation_stop", map[string]any{
		"group_id": string(g.ID),
	})
	if !ok {
		t.Fatalf("stop: %s", errMsg)
	}
	convWait(t, "停止后账本收口", func() bool {
		d1, ok1 := m.hubConversation().Delivery(g.ID, db.WakeID)
		d2, ok2 := m.hubConversation().Delivery(g.ID, dc.WakeID)
		return ok1 && ok2 && d1.Status.Terminal() && d2.Status.Terminal()
	})
	d1, _ := m.hubConversation().Delivery(g.ID, db.WakeID)
	d2, _ := m.hubConversation().Delivery(g.ID, dc.WakeID)
	if d2.Status != agenthub.DeliverySkipped || d2.Reason != agenthub.DeliveryReasonStopped {
		t.Fatalf("排队的 C = %+v, want skipped/stopped", d2)
	}
	if d1.Status != agenthub.DeliveryFailed || d1.Reason != agenthub.DeliveryReasonInterrupted {
		t.Fatalf("在跑的 B = %+v, want failed/interrupted", d1)
	}
}
