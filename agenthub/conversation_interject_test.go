package agenthub

// P6-07：人类插话的「下一输入」队列与 generation 隔离（§1.5）。
//
// 契约只把**旧 generation 尚未投递的 auto wake** 判作废：人类自己已经说出口、
// 只是目标当时在跑的那条指令属于「下一个输入」，换代不能把它一起丢掉。

import (
	"testing"
	"time"
)

// TestConversationHumanNextInputSurvivesGenerationChange：人类 @B（B 在跑）→ 再发
// 一条不 @ 人的消息 → B 的「下一输入」必须还在队里，并且在新代里可投。
func TestConversationHumanNextInputSurvivesGenerationChange(t *testing.T) {
	c, clk := newTestController(t, ConversationLimits{Cooldown: -1})
	c.BeginHuman("group_1", 1, clk.Now())
	wakes, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Human: true, Targets: convIDs("agent_b"),
		Text: "@B 先看这个", Depth: 1, TriggerSeq: 2,
	})
	if err != nil || len(wakes) != 1 {
		t.Fatalf("人类 wake 入队失败：wakes=%v err=%v", wakes, err)
	}
	firstID := c.View("group_1").ID
	if v := c.View("group_1"); v.QueueLen != 1 {
		t.Fatalf("人类 wake 应排队：%+v", v)
	}

	clk.Advance(time.Second)
	v := c.BeginHuman("group_1", 3, clk.Now())
	if v.ID == firstID {
		t.Fatal("人类消息必须开新 generation")
	}
	if v.QueueLen != 1 {
		t.Fatalf("换代丢掉了人类的下一输入：queue=%d want 1", v.QueueLen)
	}
	// 新代里仍然可投：交出来的是**新**对话 id，BeginDelivery 不能判成 stale。
	due := c.DueWakes("group_1", clk.Now())
	if len(due) != 1 {
		t.Fatalf("新代里取不到人类的下一输入：%+v", due)
	}
	if due[0].Target != "agent_b" || due[0].ConversationID != v.ID {
		t.Fatalf("下一输入 = %+v, want target=agent_b conversation=%s", due[0], v.ID)
	}
	begun, fresh := c.BeginDelivery(due[0], clk.Now())
	if !fresh {
		t.Fatalf("人类的下一输入被判成不可投（stale/已终结）：%+v", begun)
	}
	if d, ok := c.Delivery("group_1", wakes[0].ID); !ok || d.Status != DeliveryRunning {
		t.Fatalf("账本记录 = %+v ok=%v, want running", d, ok)
	}
	// 人类 wake 不吃轮次：新代仍然是「第 0 轮自动接龙」。
	if got := c.View("group_1").TurnsUsed; got != 0 {
		t.Fatalf("turns = %d, want 0（人类 wake 不累加 TurnsUsed）", got)
	}
}

// TestConversationBeginHumanDropsAutoWakesKeepsHuman：同一代里既有 auto wake 又有
// 人类下一输入时，换代只丢 auto 那条（账本记 skipped 并出队）。
func TestConversationBeginHumanDropsAutoWakesKeepsHuman(t *testing.T) {
	c, clk := newTestController(t, ConversationLimits{Cooldown: -1})
	c.BeginHuman("group_1", 1, clk.Now())
	human, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Human: true, Targets: convIDs("agent_b"),
		Text: "@B 你接着做", Depth: 1, TriggerSeq: 2,
	})
	if err != nil || len(human) != 1 {
		t.Fatalf("人类 wake: %v %v", human, err)
	}
	auto, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_c"),
		Text: "@C 帮个忙", Depth: 2, TriggerSeq: 3,
	})
	if err != nil || len(auto) != 1 {
		t.Fatalf("auto wake: %v %v", auto, err)
	}
	v := c.BeginHuman("group_1", 4, clk.Now())
	if v.QueueLen != 1 {
		t.Fatalf("换代后队列 = %d, want 1（只剩人类那条）", v.QueueLen)
	}
	due := c.DueWakes("group_1", clk.Now())
	if len(due) != 1 || due[0].Target != "agent_b" {
		t.Fatalf("队列内容 = %+v, want 只有 agent_b", due)
	}
	// 旧 auto wake 的记录被丢掉（不再是一条永远 queued 的幽灵账）。
	if d, ok := c.Delivery("group_1", auto[0].ID); ok {
		t.Fatalf("旧 generation 的 auto wake 还在账本里：%+v", d)
	}
}

// TestConversationHumanNextInputNewestPerTarget：「下一个输入」每 target 一条 ——
// 人类连着 @ 同一个目标时最新那条顶上（正文都在群日志里），不同 target 各排各的。
func TestConversationHumanNextInputNewestPerTarget(t *testing.T) {
	c, clk := newTestController(t, ConversationLimits{Cooldown: -1})
	c.BeginHuman("group_1", 1, clk.Now())
	if _, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Human: true, Targets: convIDs("agent_b", "agent_c"),
		Text: "@B @C 一人一件事", Depth: 1, TriggerSeq: 2,
	}); err != nil {
		t.Fatalf("offer: %v", err)
	}
	if _, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Human: true, Targets: convIDs("agent_b"),
		Text: "@B 第二件", Depth: 1, TriggerSeq: 3,
	}); err != nil {
		t.Fatalf("offer 2: %v", err)
	}
	v := c.BeginHuman("group_1", 4, clk.Now())
	if v.QueueLen != 2 {
		t.Fatalf("queue = %d, want 2（B 只剩最新一条 + C 一条）", v.QueueLen)
	}
	// 每 target 一条：B 拿到最新那条，C 拿它自己的（互不阻塞）。
	due := c.DueWakes("group_1", clk.Now())
	byTarget := map[AgentID]Wake{}
	for _, w := range due {
		byTarget[w.Target] = w
	}
	if len(byTarget) != 2 {
		t.Fatalf("一次 pass 的目标 = %+v, want B/C 各一条", due)
	}
	if got := byTarget["agent_b"].TriggerSeq; got != 3 {
		t.Fatalf("B 拿到的是 seq=%d, want 3（最新一条顶上）", got)
	}
	if got := byTarget["agent_c"].TriggerSeq; got != 2 {
		t.Fatalf("C 拿到的是 seq=%d, want 2（没被 B 的新消息牵连）", got)
	}
	// 被顶掉的那条不能留成幽灵账：账本里只剩 B 最新一条 + C 一条。
	active := 0
	for _, d := range c.Deliveries("group_1") {
		if d.Status == DeliveryQueued {
			active++
		}
	}
	if active != 2 {
		t.Fatalf("queued 账本 = %d 条, want 2（被取代的 pending 要一起清掉）", active)
	}
}
