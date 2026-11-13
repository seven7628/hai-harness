package agenthub

// P3b-04：对话控制器（防止无限互踢）——预算/深度/踢皮球/冷却/同内容/人类优先。

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type convClock struct {
	mu sync.Mutex
	t  time.Time
}

func newConvClock() *convClock {
	return &convClock{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *convClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *convClock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	return c.t
}

func newTestController(t *testing.T, limits ConversationLimits) (*ConversationController, *convClock) {
	t.Helper()
	clk := newConvClock()
	return NewConversationController(ConversationOptions{Limits: limits, Now: clk.Now}), clk
}

func convIDs(ids ...string) []AgentID {
	out := make([]AgentID, 0, len(ids))
	for _, id := range ids {
		out = append(out, AgentID(id))
	}
	return out
}

func TestConversationLimitsDefaults(t *testing.T) {
	c := NewConversationController(ConversationOptions{})
	got := c.Limits()
	// D2/D3：pair 默认 10（滚动窗口 10 分钟）、每链深度默认 1000、空闲重置 30 分钟。
	if got.TurnBudget != 8 || got.MaxDepth != 1000 || got.PairLimit != 10 ||
		got.PairWindow != 10*time.Minute || got.IdleReset != 30*time.Minute ||
		got.MaxPendingWakes != 200 || got.Cooldown != 0 {
		t.Fatalf("默认限额 = %+v, want 8/1000/10/10m/30m/200/0（D8 默认无冷却）", got)
	}
	// 零值限额的控制器与显式默认值同义；负数冷却 = 关闭。
	c2 := NewConversationController(ConversationOptions{Limits: ConversationLimits{Cooldown: -1}})
	if c2.Limits().Cooldown != 0 {
		t.Fatalf("负冷却应关闭冷却，得到 %v", c2.Limits().Cooldown)
	}
	// UnlimitedTurnBudget 必须原样保留：withDefaults 不能把 -1 补回 8。
	c3 := NewConversationController(ConversationOptions{Limits: ConversationLimits{TurnBudget: UnlimitedTurnBudget}})
	if got := c3.Limits().TurnBudget; got != UnlimitedTurnBudget {
		t.Fatalf("TurnBudget = %d, want %d（-1 不能被补成 8）", got, UnlimitedTurnBudget)
	}
	if v := c3.View("group_1"); v.TurnBudget != UnlimitedTurnBudget || v.Status != ConversationIdle {
		t.Fatalf("空对话视图 = %+v", v)
	}
}

func TestConversationBeginHumanResetsEverything(t *testing.T) {
	c, clk := newTestController(t, ConversationLimits{Cooldown: -1})
	c.BeginHuman("group_1", 1, clk.Now())
	if _, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"),
		Text: "我先说一句", Depth: 2, TriggerSeq: 1,
	}); err != nil {
		t.Fatalf("offer: %v", err)
	}
	w, ok := c.NextDue("group_1", clk.Now())
	if !ok {
		t.Fatal("新鲜对话的第一个 wake 必须立即可跑（冷却不能拖住人类消息）")
	}
	if _, ok := c.BeginTurn("group_1", clk.Now()); !ok {
		t.Fatal("begin turn")
	}
	if _, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_b", Targets: convIDs("agent_a"),
		Text: "回一句", Depth: 3, TriggerSeq: 2,
	}); err != nil {
		t.Fatalf("offer 2: %v", err)
	}
	before := c.View("group_1")
	if before.TurnsUsed != 1 || before.QueueLen != 1 || before.Depth != w.Depth {
		t.Fatalf("前态 = %+v", before)
	}
	if before.PairCount[ConversationPairKey("agent_a", "agent_b")] != 1 {
		t.Fatalf("PairCount = %v, want a→b = 1", before.PairCount)
	}

	clk.Advance(time.Second)
	view := c.BeginHuman("group_1", 2, clk.Now())
	if view.Status != ConversationRunning {
		t.Fatalf("BeginHuman 后 status = %q, want running", view.Status)
	}
	if view.TurnsUsed != 0 || view.Depth != 0 || view.QueueLen != 0 || len(view.PairCount) != 0 {
		t.Fatalf("人类消息必须清空计数与队列：%+v", view)
	}
	if view.HumanSeq != 2 {
		t.Fatalf("human_seq = %d", view.HumanSeq)
	}
	if _, ok := c.NextDue("group_1", clk.Now()); ok {
		t.Fatal("人类消息清掉的队列不能被再取出")
	}
	// 新对话重新计数：A→B 次数从 0 开始。
	if n := c.PairCount("group_1", "agent_a", "agent_b"); n != 0 {
		t.Fatalf("新对话 pair count = %d, want 0", n)
	}
}

func TestConversationTurnBudgetExactlyEight(t *testing.T) {
	c, clk := newTestController(t, ConversationLimits{TurnBudget: 8, PairLimit: 99, MaxDepth: 99, Cooldown: -1})
	c.BeginHuman("group_1", 1, clk.Now())
	for i := 1; i <= 8; i++ {
		if _, err := c.Offer(ConversationOffer{
			GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"),
			Text: "第 " + string(rune('0'+i)) + " 句不同的话", Depth: 2, TriggerSeq: int64(i),
		}); err != nil {
			t.Fatalf("第 %d 轮 offer 不该被拒: %v", i, err)
		}
		w, ok := c.NextDue("group_1", clk.Now())
		if !ok {
			t.Fatalf("第 %d 轮 NextDue 失败", i)
		}
		if w.Turn != i {
			t.Fatalf("turn = %d, want %d", w.Turn, i)
		}
		got, ok := c.BeginTurn("group_1", clk.Now())
		if !ok || got.Turn != i {
			t.Fatalf("第 %d 轮 BeginTurn 失败: %+v %v", i, got, ok)
		}
		clk.Advance(time.Second)
	}
	view := c.View("group_1")
	if view.TurnsUsed != 8 || view.Status != ConversationRunning {
		t.Fatalf("8 轮后 = %+v", view)
	}
	_, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"),
		Text: "第 9 句不同的话", Depth: 2,
	})
	if !errors.Is(err, ErrConversationExhausted) {
		t.Fatalf("第 9 轮必须 exhausted，得到 %v", err)
	}
	var ee *ExhaustedError
	if !errors.As(err, &ee) || !strings.Contains(ee.Reason, "8") {
		t.Fatalf("exhausted 原因缺预算数字: %+v", err)
	}
	if v := c.View("group_1"); v.Status != ConversationExhausted || v.TurnsUsed != 8 || v.QueueLen != 0 {
		t.Fatalf("exhausted 视图 = %+v", v)
	}
}

func TestConversationQueuedWakeCannotExceedBudget(t *testing.T) {
	// 预算只剩 1 轮但队列里有 2 条：第 8 轮之后必须停下，第 9 条不能跑。
	c, clk := newTestController(t, ConversationLimits{TurnBudget: 8, PairLimit: 99, MaxDepth: 99, Cooldown: -1})
	c.BeginHuman("group_1", 1, clk.Now())
	for i := 0; i < 7; i++ {
		if _, err := c.Offer(ConversationOffer{
			GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"),
			Text: "预置 " + string(rune('a'+i)), Depth: 2,
		}); err != nil {
			t.Fatalf("预置第 %d 轮: %v", i+1, err)
		}
		if _, ok := c.BeginTurn("group_1", clk.Now()); !ok {
			t.Fatalf("预置第 %d 轮 begin", i+1)
		}
	}
	if _, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b", "agent_c"),
		Text: "最后一轮两条", Depth: 2,
	}); err != nil {
		t.Fatalf("offer 两条: %v", err)
	}
	if v := c.View("group_1"); v.QueueLen != 2 || v.TurnsUsed != 7 {
		t.Fatalf("前态 = %+v", v)
	}
	if _, ok := c.BeginTurn("group_1", clk.Now()); !ok {
		t.Fatal("第 8 轮应当可跑")
	}
	if _, ok := c.NextDue("group_1", clk.Now()); ok {
		t.Fatal("预算已满：队列里的第 9 条不能被取出")
	}
	v := c.View("group_1")
	if v.Status != ConversationExhausted || v.QueueLen != 0 || v.TurnsUsed != 8 {
		t.Fatalf("预算耗尽后 = %+v", v)
	}
}

// 生产默认（UnlimitedTurnBudget）：20 轮自动接龙照跑，不再有第 9 条闸门。
func TestConversationUnlimitedTurnBudgetKeepsTwentyTurns(t *testing.T) {
	c, clk := newTestController(t, ConversationLimits{
		TurnBudget: UnlimitedTurnBudget, PairLimit: 99, MaxDepth: 99, Cooldown: -1,
	})
	c.BeginHuman("group_1", 1, clk.Now())
	for i := 1; i <= 20; i++ {
		if _, err := c.Offer(ConversationOffer{
			GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"),
			Text: fmt.Sprintf("第 %d 条不同的发言", i), Depth: 2, TriggerSeq: int64(i),
		}); err != nil {
			t.Fatalf("第 %d 轮不该被轮次预算拦下: %v", i, err)
		}
		if _, ok := c.NextDue("group_1", clk.Now()); !ok {
			t.Fatalf("第 %d 轮 NextDue 失败（无限预算不能在出队处误杀）", i)
		}
		got, ok := c.BeginTurn("group_1", clk.Now())
		if !ok || got.Turn != i {
			t.Fatalf("第 %d 轮 BeginTurn = %+v %v", i, got, ok)
		}
		clk.Advance(time.Second)
	}
	v := c.View("group_1")
	if v.TurnsUsed != 20 || v.Status != ConversationRunning || v.QueueLen != 0 {
		t.Fatalf("20 轮后 = %+v", v)
	}
	if v.TurnBudget != UnlimitedTurnBudget {
		t.Fatalf("视图 turn_budget = %d, want %d", v.TurnBudget, UnlimitedTurnBudget)
	}
}

// 取消轮次闸门后，其余安全阀一个都不能松：深度、同对、同内容指纹照样停。
func TestConversationUnlimitedTurnBudgetStillStopsOnSafetyValves(t *testing.T) {
	t.Run("深度", func(t *testing.T) {
		c, clk := newTestController(t, ConversationLimits{TurnBudget: UnlimitedTurnBudget, Cooldown: -1, MaxDepth: 4})
		c.BeginHuman("group_1", 1, clk.Now())
		for depth := 1; depth <= 4; depth++ {
			if _, err := c.Offer(ConversationOffer{
				GroupID: "group_1", Sender: AgentID("agent_" + string(rune('a'+depth))),
				Targets: convIDs("agent_z"), Text: fmt.Sprintf("深度 %d", depth), Depth: depth,
			}); err != nil {
				t.Fatalf("深度 %d 应当允许: %v", depth, err)
			}
			if _, ok := c.BeginTurn("group_1", clk.Now()); !ok {
				t.Fatalf("深度 %d begin", depth)
			}
		}
		// P6-04：到限**只跳过该 target**（账本 skipped/depth_limit），不整批熔断。
		wakes, err := c.Offer(ConversationOffer{
			GroupID: "group_1", Sender: "agent_x", Targets: convIDs("agent_y"),
			Text: "第 5 跳", Depth: 5,
		})
		if err != nil {
			t.Fatalf("超深不该报错（只跳过该 target）: %v", err)
		}
		if len(wakes) != 0 {
			t.Fatalf("超深仍入队: %+v", wakes)
		}
		if v := c.View("group_1"); v.Status != ConversationWaitingHuman {
			t.Fatalf("全部跳过 → status = %q, want waiting_human（不是 exhausted）", v.Status)
		}
		var skipped *Delivery
		for _, d := range c.Deliveries("group_1") {
			if d.Status == DeliverySkipped && d.Reason == DeliveryReasonDepthLimit {
				dd := d
				skipped = &dd
			}
		}
		if skipped == nil || !strings.Contains(skipped.Note, "深度") {
			t.Fatalf("缺 depth_limit 的 skipped 记录: %+v", c.Deliveries("group_1"))
		}
	})

	t.Run("同对", func(t *testing.T) {
		c, clk := newTestController(t, ConversationLimits{TurnBudget: UnlimitedTurnBudget, Cooldown: -1, PairLimit: 2})
		c.BeginHuman("group_1", 1, clk.Now())
		for i := 0; i < 2; i++ {
			if _, err := c.Offer(ConversationOffer{
				GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"),
				Text: fmt.Sprintf("第 %d 次不同说法", i+1), Depth: 2,
			}); err != nil {
				t.Fatalf("第 %d 次 a→b 应当允许: %v", i+1, err)
			}
			if _, ok := c.BeginTurn("group_1", clk.Now()); !ok {
				t.Fatalf("第 %d 次 begin", i+1)
			}
		}
		wakes, err := c.Offer(ConversationOffer{
			GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"),
			Text: "第三次", Depth: 2,
		})
		if err != nil || len(wakes) != 0 {
			t.Fatalf("同对超限只跳过该 target（不报错、不入队）: %v %+v", err, wakes)
		}
	})

	t.Run("同内容指纹", func(t *testing.T) {
		c, clk := newTestController(t, ConversationLimits{
			TurnBudget: UnlimitedTurnBudget, PairLimit: 99, Cooldown: -1,
		})
		c.BeginHuman("group_1", 1, clk.Now())
		text := "好的，我来处理：把报告写好，然后发给你。"
		if _, err := c.Offer(ConversationOffer{
			GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"), Text: text, Depth: 2,
		}); err != nil {
			t.Fatalf("offer: %v", err)
		}
		if _, ok := c.BeginTurn("group_1", clk.Now()); !ok {
			t.Fatal("begin")
		}
		_, err := c.Offer(ConversationOffer{
			GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"),
			Text: "好的 我来处理！把报告写好，然后发给你", Depth: 2,
		})
		if !errors.Is(err, ErrConversationExhausted) || !strings.Contains(err.Error(), "循环") {
			t.Fatalf("同内容必须按循环 exhausted，得到 %v", err)
		}
	})
}

func TestConversationDepthCap(t *testing.T) {
	// P6-04/P6-05：到限**只拒绝这条链继续 @**（skip 该 target），不把整个群判死。
	c, clk := newTestController(t, ConversationLimits{Cooldown: -1, MaxDepth: 4})
	c.BeginHuman("group_1", 1, clk.Now())
	for depth := 1; depth <= 4; depth++ {
		if _, err := c.Offer(ConversationOffer{
			GroupID: "group_1", Sender: AgentID("agent_" + string(rune('a'+depth))),
			Targets: convIDs("agent_z"), Text: "深度 " + string(rune('0'+depth)), Depth: depth,
		}); err != nil {
			t.Fatalf("深度 %d 应当允许: %v", depth, err)
		}
		if _, ok := c.BeginTurn("group_1", clk.Now()); !ok {
			t.Fatalf("深度 %d begin", depth)
		}
	}
	if v := c.View("group_1"); v.Depth != 4 {
		t.Fatalf("depth = %d, want 4", v.Depth)
	}
	wakes, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_x", Targets: convIDs("agent_y"), Text: "第 5 跳", Depth: 5,
	})
	if err != nil {
		t.Fatalf("超深不该整批报错（只跳过该 target）: %v", err)
	}
	if len(wakes) != 0 {
		t.Fatalf("超深仍入队: %+v", wakes)
	}
	if v := c.View("group_1"); v.Status != ConversationWaitingHuman || v.QueueLen != 0 {
		t.Fatalf("全部跳过后的视图 = %+v, want waiting_human/queue=0", v)
	}
}

func TestConversationPairLimit(t *testing.T) {
	// P6-04：无序 pair、成功投递才计数、到限只跳过该 target。
	c, clk := newTestController(t, ConversationLimits{Cooldown: -1, PairLimit: 2})
	c.BeginHuman("group_1", 1, clk.Now())
	for i := 0; i < 2; i++ {
		if _, err := c.Offer(ConversationOffer{
			GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"),
			Text: "第 " + string(rune('a'+i)) + " 次不同说法", Depth: 2,
		}); err != nil {
			t.Fatalf("第 %d 次 a↔b 应当允许: %v", i+1, err)
		}
		if _, ok := c.BeginTurn("group_1", clk.Now()); !ok {
			t.Fatalf("第 %d 次 begin", i+1)
		}
	}
	if n := c.PairCount("group_1", "agent_a", "agent_b"); n != 2 {
		t.Fatalf("a↔b = %d, want 2", n)
	}
	// 无序：反过来查同一个键。
	if n := c.PairCount("group_1", "agent_b", "agent_a"); n != 2 {
		t.Fatalf("b↔a = %d, want 2（无序 pair）", n)
	}
	wakes, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"), Text: "第三次", Depth: 2,
	})
	if err != nil || len(wakes) != 0 {
		t.Fatalf("第 3 次 a↔b 只跳过该 target，得到 %v %+v", err, wakes)
	}
	// 人类发送不计 PairCount：同一对目标的既有人数不因人的消息增长。
	c.BeginHuman("group_1", 2, clk.Now())
	for i := 0; i < 5; i++ {
		if _, err := c.Offer(ConversationOffer{
			GroupID: "group_1", Human: true, Targets: convIDs("agent_a", "agent_b"),
			Text: "人类说第 " + string(rune('a'+i)) + " 句", Depth: 1,
		}); err != nil {
			t.Fatalf("人类 offer: %v", err)
		}
		c.ClearQueue("group_1")
	}
	if n := c.PairCount("group_1", "", "agent_a"); n != 0 {
		t.Fatalf("人类发送计入 pair count = %d", n)
	}
	if n := c.PairCount("group_1", "agent_a", "agent_b"); n != 0 {
		t.Fatalf("人类发送不该增长 a↔b: %d", n)
	}
}

func TestConversationCooldownBetweenAgentWakes(t *testing.T) {
	c, clk := newTestController(t, ConversationLimits{Cooldown: 50 * time.Millisecond})
	c.BeginHuman("group_1", 1, clk.Now())
	if _, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b", "agent_c"),
		Text: "@B @C 你们看看", Depth: 2,
	}); err != nil {
		t.Fatalf("offer: %v", err)
	}
	// 新对话的第一条立即可跑。
	first, ok := c.NextDue("group_1", clk.Now())
	if !ok {
		t.Fatal("新对话第一条 wake 必须立即可跑")
	}
	if _, ok := c.BeginTurn("group_1", clk.Now()); !ok {
		t.Fatal("begin first")
	}
	clk.Advance(10 * time.Millisecond)
	if _, ok := c.NextDue("group_1", clk.Now()); ok {
		t.Fatal("冷却中（10ms < 50ms）不能取出下一条")
	}
	head, ok := c.Peek("group_1")
	if !ok {
		t.Fatal("冷却中的 wake 仍在队列里")
	}
	if want := first.ReadyAt.Add(0); head.ReadyAt.Before(want) {
		t.Fatalf("ReadyAt = %v, want >= %v", head.ReadyAt, want)
	}
	if d := head.ReadyAt.Sub(clk.Now()); d <= 0 || d > 50*time.Millisecond {
		t.Fatalf("冷却剩余 = %v，应在 (0,50ms]", d)
	}
	clk.Advance(40 * time.Millisecond) // 累计 50ms
	if _, ok := c.NextDue("group_1", clk.Now()); !ok {
		t.Fatal("冷却结束后必须可跑")
	}
	if _, ok := c.BeginTurn("group_1", clk.Now()); !ok {
		t.Fatal("begin second")
	}
	if v := c.View("group_1"); v.TurnsUsed != 2 || v.QueueLen != 0 {
		t.Fatalf("两次唤醒后 = %+v", v)
	}
}

func TestConversationFingerprintLoop(t *testing.T) {
	c, clk := newTestController(t, ConversationLimits{PairLimit: 99, Cooldown: -1})
	c.BeginHuman("group_1", 1, clk.Now())
	text := "好的，我来处理：把报告写好，然后发给你。"
	if _, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"),
		Text: text, Depth: 2,
	}); err != nil {
		t.Fatalf("offer: %v", err)
	}
	if _, ok := c.BeginTurn("group_1", clk.Now()); !ok {
		t.Fatal("begin")
	}
	// 同样的内容、标点/空白变体：判定为循环，且一条都不入队。
	_, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"),
		Text: "好的 我来处理！把报告写好，然后发给你", Depth: 2,
	})
	if !errors.Is(err, ErrConversationExhausted) || !strings.Contains(err.Error(), "循环") {
		t.Fatalf("同内容必须按循环 exhausted，得到 %v", err)
	}
	if v := c.View("group_1"); v.Status != ConversationExhausted || v.QueueLen != 0 || v.StopReason == "" {
		t.Fatalf("exhausted 视图 = %+v", v)
	}
}

// TestConversationOfferSkipsOnlyAtLimitTarget（P6-04）：一条消息 @ 两个人，其中一个
// 已经到 pair 上限 → **只跳过它**，另一个照常入队；不整批熔断、不报错。
func TestConversationOfferSkipsOnlyAtLimitTarget(t *testing.T) {
	c, clk := newTestController(t, ConversationLimits{Cooldown: -1, PairLimit: 2})
	c.BeginHuman("group_1", 1, clk.Now())
	// 先把 a↔c 用满（2 次成功投递）。
	for i := 0; i < 2; i++ {
		if _, err := c.Offer(ConversationOffer{
			GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_c"),
			Text: "c 第 " + string(rune('a'+i)) + " 句", Depth: 2,
		}); err != nil {
			t.Fatalf("预置 a↔c: %v", err)
		}
		if _, ok := c.BeginTurn("group_1", clk.Now()); !ok {
			t.Fatal("begin 预置")
		}
	}
	wakes, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b", "agent_c"),
		Text: "一条消息同时 @ 两个人", Depth: 2,
	})
	if err != nil {
		t.Fatalf("到限不该整批报错: %v", err)
	}
	if len(wakes) != 1 || wakes[0].Target != "agent_b" {
		t.Fatalf("wakes = %+v, want 只留 agent_b", wakes)
	}
	if v := c.View("group_1"); v.QueueLen != 1 {
		t.Fatalf("queue = %d, want 1（只跳过命中的那条）", v.QueueLen)
	}
	var skipped bool
	for _, d := range c.Deliveries("group_1") {
		if d.Target == "agent_c" && d.Status == DeliverySkipped && d.Reason == DeliveryReasonPairLimit {
			skipped = true
		}
	}
	if !skipped {
		t.Fatalf("缺 a↔c 的 skipped(pair_limit) 记录: %+v", c.Deliveries("group_1"))
	}
	// 被跳过的那条不算投递成功 → 不增长 pair 计数。
	if n := c.PairCount("group_1", "agent_a", "agent_c"); n != 2 {
		t.Fatalf("a↔c = %d, want 2（skip 不该再计数）", n)
	}
	if n := c.PairCount("group_1", "agent_a", "agent_b"); n != 0 {
		t.Fatalf("a↔b = %d, want 0（还没投递）", n)
	}
}

// TestConversationDepthFollowsChainNotGroup（P6-04/P6-05）：depth 由 wake 自带，
// 不再按「群级当前 depth」夹紧 —— 否则并行分支的深浅会互相踩。
func TestConversationDepthFollowsChainNotGroup(t *testing.T) {
	c, clk := newTestController(t, ConversationLimits{Cooldown: -1, MaxDepth: 1000})
	c.BeginHuman("group_1", 1, clk.Now())
	// 深分支：depth 5。
	wakes, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_deep", Targets: convIDs("agent_x"),
		Text: "深链", Depth: 5,
	})
	if err != nil || len(wakes) != 1 || wakes[0].Depth != 5 {
		t.Fatalf("深链 wake = %+v, %v（depth 必须原样保留）", wakes, err)
	}
	if _, ok := c.BeginTurn("group_1", clk.Now()); !ok {
		t.Fatal("begin 深链")
	}
	// 浅分支随后到：不能因为「群级 depth 已经是 5」把它的 depth 抬到 6。
	wakes, err = c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_shallow", Targets: convIDs("agent_y"),
		Text: "浅链", Depth: 2,
	})
	if err != nil || len(wakes) != 1 || wakes[0].Depth != 2 {
		t.Fatalf("浅链 wake = %+v, %v（不能被群级 depth 污染）", wakes, err)
	}
	if _, ok := c.BeginTurn("group_1", clk.Now()); !ok {
		t.Fatal("begin 浅链")
	}
	if v := c.View("group_1"); v.Depth != 5 {
		t.Fatalf("视图 depth = %d, want 5（取见过的最大跳数）", v.Depth)
	}
}

// TestConversationEmptyOfferKeepsIdle 锁住 N3：没有 @ 的 Offer 不能建对话/换 id。
func TestConversationEmptyOfferKeepsIdle(t *testing.T) {
	c, clk := newTestController(t, ConversationLimits{Cooldown: -1})
	if wakes, err := c.Offer(ConversationOffer{GroupID: "group_1", Sender: "agent_a"}); err != nil || len(wakes) != 0 {
		t.Fatalf("空 offer = %v, %v", wakes, err)
	}
	if v := c.View("group_1"); v.Status != ConversationIdle || v.ID != "" || v.TurnsUsed != 0 {
		t.Fatalf("空 offer 造出了对话: %+v", v)
	}
	// 已有对话：空 offer 不改 id/状态/轮次/队列。
	c.BeginHuman("group_1", 1, clk.Now())
	c.StartHumanRound("group_1", 1, clk.Now())
	before := c.View("group_1")
	if _, err := c.Offer(ConversationOffer{GroupID: "group_1", Sender: "agent_a", Text: "没有 @"}); err != nil {
		t.Fatalf("空 offer: %v", err)
	}
	after := c.View("group_1")
	if after.ID != before.ID || after.Status != before.Status ||
		after.TurnsUsed != before.TurnsUsed || after.QueueLen != before.QueueLen {
		t.Fatalf("空 offer 改了对话：前 %+v / 后 %+v", before, after)
	}
}

func TestConversationSelfMentionExcluded(t *testing.T) {
	c, clk := newTestController(t, ConversationLimits{Cooldown: -1})
	c.BeginHuman("group_1", 1, clk.Now())
	wakes, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_a", "agent_b", "agent_b", ""),
		Text: "我顺便 @ 自己一下", Depth: 2,
	})
	if err != nil {
		t.Fatalf("offer: %v", err)
	}
	if len(wakes) != 1 || wakes[0].Target != "agent_b" {
		t.Fatalf("自我 @ 与重复目标必须被去掉：%+v", wakes)
	}
}

func TestConversationStopIsIdempotent(t *testing.T) {
	c, clk := newTestController(t, ConversationLimits{Cooldown: -1})
	c.BeginHuman("group_1", 1, clk.Now())
	if _, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"), Text: "排队一条", Depth: 2,
	}); err != nil {
		t.Fatalf("offer: %v", err)
	}
	v := c.Stop("group_1", "人类停止了自动对话", clk.Now())
	if v.Status != ConversationStopped || v.QueueLen != 0 || v.StopReason == "" {
		t.Fatalf("stop 视图 = %+v", v)
	}
	if _, ok := c.NextDue("group_1", clk.Now()); ok {
		t.Fatal("stopped 之后不能再取 wake")
	}
	again := c.Stop("group_1", "再来一次", clk.Now())
	if again.Status != ConversationStopped || again.StopReason != v.StopReason {
		t.Fatalf("Stop 不幂等：%+v", again)
	}
	// stopped 之后智能体再 @：静默丢弃（人的意图，不是错误），不重新开对话。
	wakes, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"), Text: "还想说", Depth: 2,
	})
	if err != nil || len(wakes) != 0 {
		t.Fatalf("stopped 后 offer = %v, %v；want 静默丢弃", wakes, err)
	}
	if v := c.View("group_1"); v.Status != ConversationStopped {
		t.Fatalf("offer 不该复活 stopped 对话：%+v", v)
	}
}

func TestConversationImplicitBeginOnAgentOffer(t *testing.T) {
	// 没有人类消息就被 @（assign_task 直接唤醒）：隐式开对话，护栏照样生效。
	c, clk := newTestController(t, ConversationLimits{Cooldown: -1})
	if _, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"), Text: "先干活", Depth: 1,
	}); err != nil {
		t.Fatalf("隐式开对话的 offer: %v", err)
	}
	v := c.View("group_1")
	if v.Status != ConversationRunning || v.ID == "" || v.QueueLen != 1 {
		t.Fatalf("隐式开对话视图 = %+v", v)
	}
	if _, ok := c.NextDue("group_1", clk.Now()); !ok {
		t.Fatal("隐式对话的第一条 wake 应当立即可跑")
	}
}

func TestConversationConcurrentUse(t *testing.T) {
	c, _ := newTestController(t, ConversationLimits{Cooldown: time.Millisecond})
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		group := GroupID("group_" + string(rune('a'+g)))
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				switch i % 5 {
				case 0:
					c.BeginHuman(group, int64(i), time.Time{})
				case 1:
					_, _ = c.Offer(ConversationOffer{
						GroupID: group, Sender: "agent_a", Targets: convIDs("agent_b"),
						Text: "并发 offer " + string(rune('a'+i)), Depth: 2,
					})
				case 2:
					// 两段式契约：NextDue 只看状态、BeginTurn 才出队。两者之间
					// 被人类消息/停止插队时 BeginTurn 返回 false = 这条 wake 已被丢弃。
					if _, ok := c.NextDue(group, time.Time{}); ok {
						_, _ = c.BeginTurn(group, time.Time{})
					}
				case 3:
					_ = c.View(group)
				case 4:
					if i%2 == 0 {
						c.Stop(group, "", time.Time{})
					}
				}
			}(i)
		}
	}
	wg.Wait()
	for g := 0; g < 4; g++ {
		v := c.View(GroupID("group_" + string(rune('a'+g))))
		if v.TurnsUsed > v.TurnBudget {
			t.Fatalf("并发下超出预算：%+v", v)
		}
		if v.Terminal() && v.QueueLen != 0 {
			t.Fatalf("终态仍留队列：%+v", v)
		}
	}
}

func TestNormalizeFingerprint(t *testing.T) {
	cases := []struct{ in, want string }{
		{"  你好，世界！ ", "你好世界"},
		{"OK, I'll do it.", "okilldoit"},
		{"重复\n重复\t重复", "重复重复重复"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := NormalizeFingerprint(tc.in); got != tc.want {
			t.Errorf("NormalizeFingerprint(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	long := strings.Repeat("好", 300)
	if got := []rune(NormalizeFingerprint(long)); len(got) != ConversationFingerprintRunes {
		t.Fatalf("长指纹长度 = %d, want %d", len(got), ConversationFingerprintRunes)
	}
}
