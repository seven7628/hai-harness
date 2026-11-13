package agenthub

// P6-08：按 agent 停止 —— 只作废该目标的排队唤醒，其他目标一条都不动。

import "testing"

// TestConversationSkipAgentQueuedOnlyThatAgent：按 agent 停止只清它的 queued，
// 其他目标的队列/账本原样保留；它的 running 记录也不动（由 run 收尾/中断路径收口）。
func TestConversationSkipAgentQueuedOnlyThatAgent(t *testing.T) {
	c, clk := newTestController(t, ConversationLimits{Cooldown: -1})
	c.BeginHuman("group_1", 1, clk.Now())
	wakes, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b", "agent_c"),
		Text: "@B @C 一起来", Depth: 2, TriggerSeq: 2,
	})
	if err != nil || len(wakes) != 2 {
		t.Fatalf("offer: %v %v", wakes, err)
	}
	// B 的那条已经在跑（模拟「已经投给 B，B 正在跑」）。
	var bWake Wake
	for _, w := range wakes {
		if w.Target == "agent_b" {
			bWake = w
		}
	}
	if begun, fresh := c.BeginDelivery(bWake, clk.Now()); !fresh {
		t.Fatalf("B 的投递没能开始：%+v", begun)
	}

	n := c.SkipAgentQueuedDeliveries("group_1", "agent_b", DeliveryReasonStopped, clk.Now())
	if n != 0 {
		t.Fatalf("按 agent 停止不该动 running 记录：跳过了 %d 条", n)
	}
	if d, ok := c.Delivery("group_1", bWake.ID); !ok || d.Status != DeliveryRunning {
		t.Fatalf("B 的 running 记录 = %+v ok=%v, want running", d, ok)
	}

	// 再排队一条 B 的和一条 C 的：按 agent 停止只清 B 的那条。
	more, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b", "agent_c"),
		Text: "@B @C 第二轮", Depth: 2, TriggerSeq: 3,
	})
	if err != nil || len(more) != 2 {
		t.Fatalf("offer 2: %v %v", more, err)
	}
	// 队列里现在有 C 的第一条（没被投）+ 第二批的 B/C 两条。
	if got := c.View("group_1").QueueLen; got != 3 {
		t.Fatalf("queue = %d, want 3", got)
	}
	if n := c.SkipAgentQueuedDeliveries("group_1", "agent_b", DeliveryReasonStopped, clk.Now()); n != 1 {
		t.Fatalf("跳过条数 = %d, want 1（只有 B 那条）", n)
	}
	if got := c.View("group_1").QueueLen; got != 2 {
		t.Fatalf("queue = %d, want 2（C 的两条必须留着）", got)
	}
	due := c.DueWakes("group_1", clk.Now())
	if len(due) != 1 || due[0].Target != "agent_c" {
		t.Fatalf("队列内容 = %+v, want 只有 agent_c", due)
	}
	// 账本：B 的新一条是 skipped(stopped)，C 的仍然 queued。
	var bSkipped, cQueued bool
	for _, d := range c.Deliveries("group_1") {
		if d.Target == "agent_b" && d.TriggerSeq == 3 && d.Status == DeliverySkipped && d.Reason == DeliveryReasonStopped {
			bSkipped = true
		}
		if d.Target == "agent_c" && d.TriggerSeq == 3 && d.Status == DeliveryQueued {
			cQueued = true
		}
	}
	if !bSkipped || !cQueued {
		t.Fatalf("账本 = %+v, want B skipped(stopped) + C queued", c.Deliveries("group_1"))
	}
}
