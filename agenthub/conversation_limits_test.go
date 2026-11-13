package agenthub

// P6-04：全局对话限额 + 无序 PairLimit 的**滚动窗口**与五类清零/恢复时机。
//
// 口径（§1.1/§1.2）：
//   - 只统计最近 pair_window 内的**成功投递**；更早的计数惰性过期；
//   - A↔B 到限只 skip 该 target，其他 target 继续；窗口滑过后自动恢复；
//   - 清零：①人类新输入 ②pair 窗口过期 ③空闲 idle_reset 回 idle ④手动重置；
//   - A→C 不清 A↔B；人类发送不计 pair。

import (
	"testing"
	"time"
)

// TestConversationPairWindowExpiresLazily：窗口滑过之后该 pair 自动恢复（不需要人类操作）。
func TestConversationPairWindowExpiresLazily(t *testing.T) {
	const window = 10 * time.Minute
	c, clk := newTestController(t, ConversationLimits{
		Cooldown: -1, PairLimit: 2, PairWindow: window, IdleReset: -1,
	})
	c.BeginHuman("group_1", 1, clk.Now())
	// 用满 a↔b（2 次成功投递）。
	for i := 0; i < 2; i++ {
		if _, err := c.Offer(ConversationOffer{
			GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"),
			Text: "第 " + string(rune('a'+i)) + " 句", Depth: 2,
		}); err != nil {
			t.Fatalf("第 %d 次: %v", i+1, err)
		}
		if _, ok := c.BeginTurn("group_1", clk.Now()); !ok {
			t.Fatal("begin")
		}
	}
	if n := c.PairCount("group_1", "agent_a", "agent_b"); n != 2 {
		t.Fatalf("窗口内 a↔b = %d, want 2", n)
	}
	// 到限：只跳过。
	if wakes, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"), Text: "第三次", Depth: 2,
	}); err != nil || len(wakes) != 0 {
		t.Fatalf("到限必须只跳过该 target: %v %+v", err, wakes)
	}
	// 窗口滑过（10 分钟 + 1 秒）：计数惰性过期，自动恢复。
	clk.Advance(window + time.Second)
	if n := c.PairCount("group_1", "agent_a", "agent_b"); n != 0 {
		t.Fatalf("窗口外计数 = %d, want 0（惰性过期）", n)
	}
	if wakes, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"), Text: "窗口后再说", Depth: 2,
	}); err != nil || len(wakes) != 1 {
		t.Fatalf("窗口滑过后必须自动恢复: %v %+v", err, wakes)
	}
}

// TestConversationPairWindowViewReset：视图给出 pair 窗口与「最早恢复时刻」（UI 的
// 「约 N 分钟后自动恢复」数据源）。
func TestConversationPairWindowViewReset(t *testing.T) {
	const window = 10 * time.Minute
	c, clk := newTestController(t, ConversationLimits{
		Cooldown: -1, PairLimit: 2, PairWindow: window, IdleReset: -1,
	})
	c.BeginHuman("group_1", 1, clk.Now())
	if _, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"), Text: "一句", Depth: 2,
	}); err != nil {
		t.Fatal(err)
	}
	t0 := clk.Now()
	if _, ok := c.BeginTurn("group_1", clk.Now()); !ok {
		t.Fatal("begin")
	}
	v := c.View("group_1")
	if v.PairWindowMS != window.Milliseconds() {
		t.Fatalf("pair_window_ms = %d, want %d", v.PairWindowMS, window.Milliseconds())
	}
	if v.PairResetAt == nil || !v.PairResetAt.Equal(t0.Add(window)) {
		t.Fatalf("pair_reset_at = %v, want %v", v.PairResetAt, t0.Add(window))
	}
	if v.MaxPendingWakes != DefaultConversationMaxPendingWakes {
		t.Fatalf("视图缺 max_pending_wakes 回显: %+v", v)
	}
	// 默认限额下 idle_reset/run_timeout 也要回显（UI 显示用）。
	def := NewConversationController(ConversationOptions{}).View("group_x")
	if def.IdleResetMS != DefaultConversationIdleReset.Milliseconds() ||
		def.RunTimeoutMS != DefaultConversationRunTimeout.Milliseconds() ||
		def.PairWindowMS != DefaultConversationPairWindow.Milliseconds() {
		t.Fatalf("默认视图缺全局设置回显: %+v", def)
	}
}

// TestConversationPairOtherTargetDoesNotReset：A↔B 不因 A 去跟 C 说话而清零（否则
// 换目标就能洗白循环）。
func TestConversationPairOtherTargetDoesNotReset(t *testing.T) {
	c, clk := newTestController(t, ConversationLimits{
		Cooldown: -1, PairLimit: 2, PairWindow: 10 * time.Minute, IdleReset: -1,
	})
	c.BeginHuman("group_1", 1, clk.Now())
	for i := 0; i < 2; i++ {
		if _, err := c.Offer(ConversationOffer{
			GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"),
			Text: "a 对 b 第 " + string(rune('a'+i)) + " 句", Depth: 2,
		}); err != nil {
			t.Fatal(err)
		}
		if _, ok := c.BeginTurn("group_1", clk.Now()); !ok {
			t.Fatal("begin")
		}
	}
	// A 去跟 C 说话（新键，从 0 开始）。
	if _, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_c"),
		Text: "a 对 c 第一句", Depth: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.BeginTurn("group_1", clk.Now()); !ok {
		t.Fatal("begin a↔c")
	}
	if n := c.PairCount("group_1", "agent_a", "agent_b"); n != 2 {
		t.Fatalf("a↔b = %d, want 2（不该被 a↔c 清零）", n)
	}
	if n := c.PairCount("group_1", "agent_a", "agent_c"); n != 1 {
		t.Fatalf("a↔c = %d, want 1（新键从 0 开始）", n)
	}
}

// TestConversationPairCountsOnlySuccessfulDelivery：忙/失败退回队列的尝试不计数 ——
// 否则目标一忙，额度就被白吃光，队列里的合法接龙永远轮不到。
func TestConversationPairCountsOnlySuccessfulDelivery(t *testing.T) {
	c, clk := newTestController(t, ConversationLimits{
		Cooldown: -1, PairLimit: 2, PairWindow: 10 * time.Minute, IdleReset: -1,
	})
	c.BeginHuman("group_1", 1, clk.Now())
	wakes, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"), Text: "第一句", Depth: 2,
	})
	if err != nil || len(wakes) != 1 {
		t.Fatalf("offer: %v %+v", err, wakes)
	}
	// 投递 → 目标忙 → 退回队列（Attempts=1，但没成功）。
	begun, fresh := c.BeginDelivery(wakes[0], clk.Now())
	if !fresh {
		t.Fatal("首次投递必须生效")
	}
	c.RequeueDelivery(begun, DeliveryReasonBusy, time.Time{}, clk.Now())
	if n := c.PairCount("group_1", "agent_a", "agent_b"); n != 0 {
		t.Fatalf("忙退回也算 pair = %d, want 0（只有成功投递才 +1）", n)
	}
	// 再投一次并成功（CommitDelivery）→ 计数 +1。
	begun, fresh = c.BeginDelivery(wakes[0], clk.Now())
	if !fresh {
		t.Fatal("回队后必须能再投")
	}
	c.CommitDelivery(begun, clk.Now())
	if n := c.PairCount("group_1", "agent_a", "agent_b"); n != 1 {
		t.Fatalf("成功投递后 a↔b = %d, want 1", n)
	}
}

// TestConversationIdleResetClears：空闲 idle_reset 之后回 idle 并清零计数/深度/队列。
func TestConversationIdleResetClears(t *testing.T) {
	const idle = 30 * time.Minute
	c, clk := newTestController(t, ConversationLimits{
		Cooldown: -1, PairLimit: 10, PairWindow: time.Hour, IdleReset: idle,
	})
	c.BeginHuman("group_1", 1, clk.Now())
	if _, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"),
		Text: "说一句", Depth: 3,
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.BeginTurn("group_1", clk.Now()); !ok {
		t.Fatal("begin")
	}
	if v := c.View("group_1"); v.TurnsUsed != 1 || v.Depth != 3 || len(v.PairCount) != 1 {
		t.Fatalf("空闲前 = %+v", v)
	}
	// 还没到空闲阈值：状态保持。
	clk.Advance(idle - time.Minute)
	if v := c.View("group_1"); v.Status != ConversationRunning {
		t.Fatalf("未到空闲阈值就重置了: %+v", v)
	}
	// 越过阈值：惰性结算（View 就会触发）。
	clk.Advance(2 * time.Minute)
	v := c.View("group_1")
	if v.Status != ConversationIdle || v.TurnsUsed != 0 || v.Depth != 0 || v.QueueLen != 0 || len(v.PairCount) != 0 {
		t.Fatalf("空闲重置后 = %+v, want idle/全清零", v)
	}
}

// TestConversationManualResetLiftsWaitingHuman：手动重置立即清零并解除 waiting_human，
// 不自动唤醒任何人（队列清空、下一次 @ 立刻生效）。
func TestConversationManualResetLiftsWaitingHuman(t *testing.T) {
	c, clk := newTestController(t, ConversationLimits{
		Cooldown: -1, PairLimit: 1, PairWindow: 10 * time.Minute, IdleReset: -1,
	})
	c.BeginHuman("group_1", 1, clk.Now())
	// 用满 a↔b（1 次），再 @ 一次 → 全部跳过 → waiting_human。
	if _, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"), Text: "一句", Depth: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.BeginTurn("group_1", clk.Now()); !ok {
		t.Fatal("begin")
	}
	if wakes, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"), Text: "两句", Depth: 2,
	}); err != nil || len(wakes) != 0 {
		t.Fatalf("到限必须跳过: %v %+v", err, wakes)
	}
	if v := c.View("group_1"); v.Status != ConversationWaitingHuman {
		t.Fatalf("status = %q, want waiting_human", v.Status)
	}
	v := c.Reset("group_1", clk.Now())
	if v.Status != ConversationIdle || len(v.PairCount) != 0 || v.QueueLen != 0 || v.StopReason != "" {
		t.Fatalf("手动重置后 = %+v, want idle/清零", v)
	}
	// 重置之后同一对立刻能再 @（不自动唤醒，但下一次 @ 立即生效）。
	wakes, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"), Text: "重置后", Depth: 2,
	})
	if err != nil || len(wakes) != 1 {
		t.Fatalf("重置后应能立即入队: %v %+v", err, wakes)
	}
	if v := c.View("group_1"); v.Status != ConversationRunning || v.QueueLen != 1 {
		t.Fatalf("重置后视图 = %+v, want running/queue=1", v)
	}
}

// TestConversationAllTargetsSkippedWaitsHuman：全部目标都被跳过 → waiting_human
// （不是 exhausted），并留下可读的说明。
func TestConversationAllTargetsSkippedWaitsHuman(t *testing.T) {
	c, clk := newTestController(t, ConversationLimits{
		Cooldown: -1, PairLimit: 1, PairWindow: 10 * time.Minute, IdleReset: -1,
	})
	c.BeginHuman("group_1", 1, clk.Now())
	if _, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"), Text: "一句", Depth: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.BeginTurn("group_1", clk.Now()); !ok {
		t.Fatal("begin")
	}
	if wakes, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"), Text: "两句", Depth: 2,
	}); err != nil || len(wakes) != 0 {
		t.Fatalf("到限必须跳过: %v %+v", err, wakes)
	}
	v := c.View("group_1")
	if v.Status != ConversationWaitingHuman {
		t.Fatalf("status = %q, want waiting_human（不是 exhausted）", v.Status)
	}
	if v.StopReason == "" {
		t.Fatal("缺等待人类的原因说明")
	}
}

// TestConversationMaxPendingWakes：队列上限是兜底护栏，超出的新唤醒记 queue_full。
func TestConversationMaxPendingWakes(t *testing.T) {
	c, clk := newTestController(t, ConversationLimits{
		Cooldown: -1, PairLimit: 99, PairWindow: time.Hour, IdleReset: -1, MaxPendingWakes: 2,
	})
	c.BeginHuman("group_1", 1, clk.Now())
	// 第一条 @ 两个目标 → 队列 2 条（刚好到上限）。
	if _, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b", "agent_c"),
		Text: "先来两句", Depth: 2,
	}); err != nil {
		t.Fatal(err)
	}
	// 再来一条 → 超过上限，整条被 skip(queue_full)。
	wakes, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_d"), Text: "第三条", Depth: 2,
	})
	if err != nil || len(wakes) != 0 {
		t.Fatalf("超过 max_pending_wakes 必须跳过: %v %+v", err, wakes)
	}
	var full bool
	for _, d := range c.Deliveries("group_1") {
		if d.Status == DeliverySkipped && d.Reason == DeliveryReasonQueueFull {
			full = true
		}
	}
	if !full {
		t.Fatalf("缺 queue_full 记录: %+v", c.Deliveries("group_1"))
	}
	if v := c.View("group_1"); v.QueueLen != 2 || v.MaxPendingWakes != 2 {
		t.Fatalf("视图 = %+v, want queue=2/max=2", v)
	}
}
