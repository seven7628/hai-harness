package agenthub

// P6-04：全局对话限额 + 无序 PairLimit 的**滚动窗口**与五类清零/恢复时机。
//
// 口径（§1.1/§1.2）：
//   - 只统计最近 pair_window 内的**成功投递**；更早的计数惰性过期；
//   - A↔B 到限只 skip 该 target，其他 target 继续；窗口滑过后自动恢复；
//   - 清零：①人类新输入 ②pair 窗口过期 ③空闲 idle_reset 回 idle ④手动重置；
//   - A→C 不清 A↔B；人类发送不计 pair。

import (
	"errors"
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
	// P6-05：手动重置和「人类新输入 / 空闲 30 分钟」一样，必须把 depth 也清零 ——
	// 否则重置后第一条 @ 会带着旧链的深度（新链第一条就被判 depth_limit）。
	if v.Status != ConversationIdle || len(v.PairCount) != 0 || v.QueueLen != 0 || v.StopReason != "" || v.Depth != 0 || v.TurnsUsed != 0 {
		t.Fatalf("手动重置后 = %+v, want idle/清零（含 depth/turns）", v)
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

// TestConversationWaitingHumanStillSchedules：B1 回归 —— pair 到限把对话推到
// waiting_human 之后，下一批 @（另一条在飞 run 回来 @D / 人类插话）必须**既能入队
// 也能被调度器取到**，否则队列永久泵不动（Offer 成功、DueWakes 却返回空）。
func TestConversationWaitingHumanStillSchedules(t *testing.T) {
	c, clk := newTestController(t, ConversationLimits{
		Cooldown: -1, PairLimit: 1, PairWindow: 10 * time.Minute, IdleReset: -1,
	})
	c.BeginHuman("group_1", 1, clk.Now())
	// 用满 a↔b（1 次成功投递）。
	if _, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"), Text: "一句", Depth: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.BeginTurn("group_1", clk.Now()); !ok {
		t.Fatal("begin")
	}
	// b→a 命中 pair 限 → 全部 skipped → waiting_human。
	if wakes, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_b", Targets: convIDs("agent_a"), Text: "两句", Depth: 2,
	}); err != nil || len(wakes) != 0 {
		t.Fatalf("到限必须跳过: %v %+v", err, wakes)
	}
	if v := c.View("group_1"); v.Status != ConversationWaitingHuman || v.QueueLen != 0 {
		t.Fatalf("到限后 = %+v, want waiting_human/queue=0", v)
	}
	// 软状态不是终态：waiting_human + 空队列时调度器什么都不给（不会自己复活）。
	if wakes := c.DueWakes("group_1", clk.Now()); len(wakes) != 0 {
		t.Fatalf("空队列不该有可投 wake: %+v", wakes)
	}
	if _, ok := c.NextReadyAt("group_1"); ok {
		t.Fatal("空队列不该有 NextReadyAt")
	}
	// 另一条在飞 run 回来 @D（不同 pair，不命中限）：必须入队 + 立刻可调度。
	wakes, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_c", Targets: convIDs("agent_d"), Text: "换个对手", Depth: 2,
	})
	if err != nil || len(wakes) != 1 {
		t.Fatalf("waiting_human 之后必须仍能入队: %v %+v", err, wakes)
	}
	v := c.View("group_1")
	if v.Status != ConversationRunning || v.QueueLen != 1 {
		t.Fatalf("入队后 = %+v, want running/queue=1（waiting_human 必须被拉回）", v)
	}
	if v.StopReason != "" {
		t.Fatalf("拉回 running 后不该残留等待原因: %q", v.StopReason)
	}
	if due := c.DueWakes("group_1", clk.Now()); len(due) != 1 || due[0].Target != "agent_d" {
		t.Fatalf("DueWakes = %+v, want [agent_d]（B1：队列必须泵得动）", due)
	}
	if _, ok := c.NextReadyAt("group_1"); !ok {
		t.Fatal("B1：waiting_human 拉回后 NextReadyAt 必须给得出")
	}
	if _, ok := c.Peek("group_1"); !ok {
		t.Fatal("B1：Peek 必须看得到队首")
	}
	if w, ok := c.BeginTurn("group_1", clk.Now()); !ok || w.Target != "agent_d" {
		t.Fatalf("BeginTurn = %+v/%v, want agent_d", w, ok)
	}
}

// TestConversationWaitingHumanKeepsWhenAllSkipped：B1 回归 —— 全部 skipped 时
// **仍然**保持 waiting_human（不能因为放宽调度判据就翻回 running）。
func TestConversationWaitingHumanKeepsWhenAllSkipped(t *testing.T) {
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
	// 连续两次全部跳过：状态必须稳定停在 waiting_human，队列/调度一律为空。
	for i := 0; i < 2; i++ {
		if wakes, err := c.Offer(ConversationOffer{
			GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"), Text: "再一句", Depth: 2,
		}); err != nil || len(wakes) != 0 {
			t.Fatalf("第 %d 次到限必须跳过: %v %+v", i+1, err, wakes)
		}
		v := c.View("group_1")
		if v.Status != ConversationWaitingHuman || v.QueueLen != 0 || v.StopReason == "" {
			t.Fatalf("第 %d 次全跳过之后 = %+v, want waiting_human/queue=0/有原因", i+1, v)
		}
		if wakes := c.DueWakes("group_1", clk.Now()); len(wakes) != 0 {
			t.Fatalf("全跳过之后不该有可投 wake: %+v", wakes)
		}
	}
}

// TestConversationTerminalNeverSchedules：B1 放宽调度判据之后，stopped/exhausted
// 仍然一条都不放（队列里有东西也不能投）。
func TestConversationTerminalNeverSchedules(t *testing.T) {
	c, clk := newTestController(t, ConversationLimits{
		Cooldown: -1, PairLimit: 99, PairWindow: time.Hour, IdleReset: -1,
	})
	c.BeginHuman("group_1", 1, clk.Now())
	if _, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"), Text: "一句", Depth: 2,
	}); err != nil {
		t.Fatal(err)
	}
	c.Stop("group_1", "人类停止", clk.Now())
	if wakes := c.DueWakes("group_1", clk.Now()); len(wakes) != 0 {
		t.Fatalf("stopped 之后不该有可投 wake: %+v", wakes)
	}
	if v := c.View("group_1"); v.Status != ConversationStopped {
		t.Fatalf("stopped 之后状态 = %q", v.Status)
	}
	// exhausted：同一对重复同一段内容 → 整批熔断（保留原有语义）。
	c2, clk2 := newTestController(t, ConversationLimits{
		Cooldown: -1, PairLimit: 99, PairWindow: time.Hour, IdleReset: -1,
	})
	c2.BeginHuman("group_2", 1, clk2.Now())
	if _, err := c2.Offer(ConversationOffer{
		GroupID: "group_2", Sender: "agent_a", Targets: convIDs("agent_b"),
		Text: "同一句话", Depth: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := c2.BeginTurn("group_2", clk2.Now()); !ok {
		t.Fatal("begin")
	}
	if _, err := c2.Offer(ConversationOffer{
		GroupID: "group_2", Sender: "agent_a", Targets: convIDs("agent_b"),
		Text: "同一句话", Depth: 2,
	}); !errors.Is(err, ErrConversationExhausted) {
		t.Fatalf("同内容应 exhausted: %v", err)
	}
	if wakes := c2.DueWakes("group_2", clk2.Now()); len(wakes) != 0 {
		t.Fatalf("exhausted 之后不该有可投 wake: %+v", wakes)
	}
	if v := c2.View("group_2"); v.Status != ConversationExhausted {
		t.Fatalf("exhausted 之后状态 = %q", v.Status)
	}
}

// TestConversationExhaustMarksQueuedSkipped：N1 —— exhausted 清队列时账本里的
// queued 记录也要收口，不能留下永远「排队中」的记录。
func TestConversationExhaustMarksQueuedSkipped(t *testing.T) {
	c, clk := newTestController(t, ConversationLimits{
		Cooldown: -1, PairLimit: 99, PairWindow: time.Hour, IdleReset: -1,
	})
	c.BeginHuman("group_1", 1, clk.Now())
	c.StartHumanRound("group_1", 1, clk.Now())
	// 先排一条正常的 @。
	wakes, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"),
		Text: "同一句", Depth: 2, TriggerSeq: 3,
	})
	if err != nil || len(wakes) != 1 {
		t.Fatalf("offer: %v %+v", err, wakes)
	}
	if v := c.View("group_1"); v.QueueLen != 1 {
		t.Fatalf("前置队列 = %d, want 1", v.QueueLen)
	}
	// 同内容再来一次 → 整批熔断 → exhausted 并清队列。
	if _, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"),
		Text: "同一句", Depth: 2, TriggerSeq: 4,
	}); !errors.Is(err, ErrConversationExhausted) {
		t.Fatalf("同内容应 exhausted: %v", err)
	}
	v := c.View("group_1")
	if v.Status != ConversationExhausted || v.QueueLen != 0 {
		t.Fatalf("exhausted 后 = %+v, want exhausted/queue=0", v)
	}
	for _, d := range c.Deliveries("group_1") {
		if d.Status == DeliveryQueued {
			t.Fatalf("N1：队列清了，账本还留着「排队中」: %+v", d)
		}
		if d.Status == DeliverySkipped && d.Reason != DeliveryReasonExhausted {
			t.Fatalf("N1：跳过原因 = %q, want %q", d.Reason, DeliveryReasonExhausted)
		}
	}
}

// TestConversationQueueFullIgnoresSkippedPlans：N3 —— queue_full 只统计真要入队的
// plan；已经因互踢被跳过的目标不该把「队列没满」的一条 @ 判成 queue_full。
func TestConversationQueueFullIgnoresSkippedPlans(t *testing.T) {
	c, clk := newTestController(t, ConversationLimits{
		Cooldown: -1, PairLimit: 1, PairWindow: time.Hour, IdleReset: -1, MaxPendingWakes: 1,
	})
	c.BeginHuman("group_1", 1, clk.Now())
	c.StartHumanRound("group_1", 1, clk.Now())
	// 用满 a↔b。
	if _, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"), Text: "一句", Depth: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.BeginTurn("group_1", clk.Now()); !ok {
		t.Fatal("begin")
	}
	// a→[b, c]：b 命中 pair 限（跳过，不入队），c 正常。队列上限 1 应该刚好放行 c。
	wakes, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b", "agent_c"),
		Text: "两句", Depth: 2,
	})
	if err != nil {
		t.Fatalf("offer: %v", err)
	}
	if len(wakes) != 1 || wakes[0].Target != "agent_c" {
		t.Fatalf("N3：只该放行 c（b 已跳过，不占队列额度），得到 %+v", wakes)
	}
	for _, d := range c.Deliveries("group_1") {
		if d.Reason == DeliveryReasonQueueFull {
			t.Fatalf("N3：不该出现 queue_full（被跳过的目标不算队列额度）: %+v", d)
		}
	}
}

// TestConversationResetBumpsGeneration：N2 —— 重置/空闲超时是换代：重置前那条在飞
// run 的 final @ 不能把状态从 idle 翻回 running。
func TestConversationResetBumpsGeneration(t *testing.T) {
	c, clk := newTestController(t, ConversationLimits{
		Cooldown: -1, PairLimit: 99, PairWindow: time.Hour, IdleReset: -1,
	})
	c.BeginHuman("group_1", 1, clk.Now())
	c.StartHumanRound("group_1", 1, clk.Now())
	before := c.View("group_1")
	v := c.Reset("group_1", clk.Now())
	if v.Status != ConversationIdle {
		t.Fatalf("重置后 = %q, want idle", v.Status)
	}
	if v.ID == before.ID || v.ID == "" {
		t.Fatalf("N2：重置必须换代（旧 %q / 新 %q）", before.ID, v.ID)
	}
	// 旧 run 的 final @ 带着旧 conversation id：记 skipped(stale)，状态保持 idle。
	wakes, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"),
		Text: "旧 run 收尾", Depth: 2, TriggerSeq: 3, ConversationID: before.ID,
	})
	if err != nil || len(wakes) != 0 {
		t.Fatalf("旧代 @ 必须丢弃: %v %+v", err, wakes)
	}
	if v := c.View("group_1"); v.Status != ConversationIdle || v.QueueLen != 0 {
		t.Fatalf("N2：旧 run 的 @ 把状态翻回了 %q（队列 %d）", v.Status, v.QueueLen)
	}
	var stale bool
	for _, d := range c.Deliveries("group_1") {
		if d.Status == DeliverySkipped && d.Reason == DeliveryReasonStale {
			stale = true
		}
	}
	if !stale {
		t.Fatal("N2：旧代 @ 应记 skipped(stale)")
	}
	// 用当前 id 的 @ 照常入队（换代判据不是「一律拒绝」）。
	wakes, err = c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"),
		Text: "新代", Depth: 2, TriggerSeq: 4, ConversationID: v.ID,
	})
	if err != nil || len(wakes) != 1 {
		t.Fatalf("新代 @ 应入队: %v %+v", err, wakes)
	}
}
