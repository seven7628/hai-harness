package main

// P6-04：全局对话设置（settings.json hub.conversation）+ 手动重置命令。

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
)

// writeHubConvSettings 写用户层 settings.json（testManager 已把 HOME 指到临时目录）。
func writeHubConvSettings(t *testing.T, raw string) {
	t.Helper()
	path := filepath.Join(os.Getenv("HOME"), ".go-code", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestLoadHubConversationLimitsDefaults：没有配置段时用默认值（pair 10 / 深度 1000 /
// 窗口 10 分钟 / 空闲 30 分钟 / 队列 200 / 冷却 0）。
func TestLoadHubConversationLimitsDefaults(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	got := loadHubConversationLimits()
	if got.PairLimit != 10 || agenthub.DefaultConversationPairLimit != 10 {
		t.Fatalf("pair_limit = %d (默认 %d), want 10", got.PairLimit, agenthub.DefaultConversationPairLimit)
	}
	if got.MaxDepth != 1000 {
		t.Fatalf("max_depth = %d, want 1000", got.MaxDepth)
	}
	if got.PairWindow != 10*time.Minute || got.IdleReset != 30*time.Minute {
		t.Fatalf("窗口 = %v/%v, want 10m/30m", got.PairWindow, got.IdleReset)
	}
	if got.Cooldown != 0 {
		t.Fatalf("cooldown = %v, want 0（默认无冷却）", got.Cooldown)
	}
	if got.MaxPendingWakes != 200 || got.RunTimeout != 10*time.Minute {
		t.Fatalf("queue/timeout = %d/%v, want 200/10m", got.MaxPendingWakes, got.RunTimeout)
	}
	if got.TurnBudget != agenthub.UnlimitedTurnBudget {
		t.Fatalf("turn_budget = %d, want 不限", got.TurnBudget)
	}
}

// TestLoadHubConversationLimitsFromSettings：settings.json 的 hub.conversation 生效。
func TestLoadHubConversationLimitsFromSettings(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeHubConvSettings(t, `{"hub":{"conversation":{
		"max_depth": 42, "pair_limit": 3, "pair_window_ms": 120000,
		"idle_reset_ms": 600000, "cooldown_ms": 250, "run_timeout_ms": 300000,
		"max_pending_wakes": 7
	}}}`)
	got := loadHubConversationLimits()
	if got.MaxDepth != 42 || got.PairLimit != 3 {
		t.Fatalf("depth/pair = %d/%d, want 42/3", got.MaxDepth, got.PairLimit)
	}
	if got.PairWindow != 2*time.Minute || got.IdleReset != 10*time.Minute {
		t.Fatalf("窗口 = %v/%v, want 2m/10m", got.PairWindow, got.IdleReset)
	}
	if got.Cooldown != 250*time.Millisecond || got.RunTimeout != 5*time.Minute {
		t.Fatalf("cooldown/timeout = %v/%v", got.Cooldown, got.RunTimeout)
	}
	if got.MaxPendingWakes != 7 {
		t.Fatalf("queue = %d, want 7", got.MaxPendingWakes)
	}
}

// TestLoadHubConversationLimitsClamps：坏值/越界回落默认或钳制，不能把群聊卡死。
func TestLoadHubConversationLimitsClamps(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// 0/负数在这些字段里都不是合法业务值 → 回落默认（不是「关掉护栏」）。
	writeHubConvSettings(t, `{"hub":{"conversation":{"max_depth":0,"pair_limit":-3,"pair_window_ms":0}}}`)
	got := loadHubConversationLimits()
	if got.MaxDepth != agenthub.DefaultConversationMaxDepth || got.PairLimit != agenthub.DefaultConversationPairLimit {
		t.Fatalf("坏值没有回落默认: %+v", got)
	}
	if got.PairWindow != agenthub.DefaultConversationPairWindow {
		t.Fatalf("pair_window = %v, want 默认", got.PairWindow)
	}
	// 上限钳制：防误配成天文数字（等于关掉护栏）。
	writeHubConvSettings(t, `{"hub":{"conversation":{"max_depth":999999999,"pair_limit":99999,"max_pending_wakes":999999}}}`)
	got = loadHubConversationLimits()
	if got.MaxDepth != hubConvMaxDepthLimit || got.PairLimit != hubConvPairLimitMax ||
		got.MaxPendingWakes != hubConvMaxPendingLimit {
		t.Fatalf("上限没有钳制: %+v", got)
	}
	// 坏 JSON：整段回落默认，不 panic。
	writeHubConvSettings(t, `{"hub":`)
	if got := loadHubConversationLimits(); got.PairLimit != agenthub.DefaultConversationPairLimit {
		t.Fatalf("坏 JSON 没有回落默认: %+v", got)
	}
}

// TestHubConversationResetCommand：手动重置立即清零并解除 waiting_human，不唤醒任何人。
func TestHubConversationResetCommand(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1, PairLimit: 1, PairWindow: time.Hour, IdleReset: -1})
	a := createHubAgent(t, m, buf, 1, "重置A")
	b := createHubAgent(t, m, buf, 2, "重置B")
	g := convGroupWith(t, m, buf, 10, "重置群", nil, a, b)
	fa := newConvProvider("@重置B 我说一句")
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(a.ID), string(g.ID)), g, string(a.ID), fa)
	fb := newConvProvider("@重置A 我回一句")
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(b.ID), string(g.ID)), g, string(b.ID), fb)

	// 人类 @A → A @B（pair=1）→ B @A（到限，skip）→ waiting_human。
	if ok, errMsg, _ := hubCmd(t, m, buf, 20, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@重置A 你们聊聊",
	}); !ok {
		t.Fatalf("send: %s", errMsg)
	}
	convWait(t, "对话进入 waiting_human", func() bool {
		return convView(m, g.ID).Status == agenthub.ConversationWaitingHuman
	})
	if n := fb.callCount(); n != 1 {
		t.Fatalf("B 调用 = %d, want 1", n)
	}

	ok, errMsg, data := hubCmd(t, m, buf, 21, "hub_conversation_reset", map[string]any{
		"group_id": string(g.ID),
	})
	if !ok {
		t.Fatalf("reset: %s", errMsg)
	}
	conv, _ := data["conversation"].(map[string]any)
	if conv == nil || conv["status"] != string(agenthub.ConversationIdle) {
		t.Fatalf("重置后视图 = %+v, want idle", data)
	}
	if v := convView(m, g.ID); v.QueueLen != 0 || v.TurnsUsed != 0 || len(v.PairCount) != 0 {
		t.Fatalf("重置后计数没清: %+v", v)
	}
	// 不自动唤醒任何人。
	before := fb.callCount()
	time.Sleep(80 * time.Millisecond)
	if fb.callCount() != before {
		t.Fatalf("重置唤醒了智能体（%d → %d）", before, fb.callCount())
	}
	// 重置之后同一对立刻能再 @（额度已还）。
	if ok, errMsg, data := hubCmd(t, m, buf, 22, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@重置B 重置后再说",
	}); !ok {
		t.Fatalf("send after reset: %s", errMsg)
	} else if d := mustDelivery(t, data, b.ID); d.Status != agenthub.DeliveryRunning {
		t.Fatalf("重置后投递 = %+v, want running", d)
	}
}

// TestHubConversationResetRejectsShutdown：关停后重置命令不能动存储。
func TestHubConversationResetRejectsShutdown(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "关停A")
	b := createHubAgent(t, m, buf, 2, "关停B")
	g := convGroupWith(t, m, buf, 10, "关停群", nil, a, b)
	m.hubShutdown.Store(true)
	defer m.hubShutdown.Store(false)
	ok, errMsg, _ := hubCmd(t, m, buf, 20, "hub_conversation_reset", map[string]any{
		"group_id": string(g.ID),
	})
	if ok || errMsg == "" {
		t.Fatalf("关停后重置必须被拒: ok=%v err=%q", ok, errMsg)
	}
}

// TestHubConversationLimitsHotReload：I1 回归 —— 设置保存后**无需重启**立即生效：
// 新限额用于 Offer / View（状态条分母）/ 挂 run 超时，且旧对话状态一条都不丢。
func TestHubConversationLimitsHotReload(t *testing.T) {
	m, buf := testManager(t)
	writeHubConvSettings(t, `{"hub":{"conversation":{"pair_limit":1,"max_depth":3,"run_timeout_ms":1234}}}`)
	a := createHubAgent(t, m, buf, 1, "热更A")
	b := createHubAgent(t, m, buf, 2, "热更B")
	g := convGroupWith(t, m, buf, 10, "热更群", nil, a, b)

	ctrl := m.hubConversation()
	if got := ctrl.Limits(); got.PairLimit != 1 || got.MaxDepth != 3 || got.RunTimeout != 1234*time.Millisecond {
		t.Fatalf("首读限额 = %+v, want 1/3/1234ms", got)
	}
	// 造一点对话状态：人类开新对话 + 一次成功投递（pair 用掉 1 次）。
	ctrl.BeginHuman(g.ID, 1, time.Time{})
	ctrl.StartHumanRound(g.ID, 1, time.Time{})
	wakes, err := ctrl.Offer(agenthub.ConversationOffer{
		GroupID: g.ID, Sender: a.ID, Targets: []agenthub.AgentID{b.ID},
		Text: "第一句", Depth: 2, TriggerSeq: 3, RoundID: "r1",
	})
	if err != nil || len(wakes) != 1 {
		t.Fatalf("offer: %v %+v", err, wakes)
	}
	w, ok := ctrl.BeginDelivery(wakes[0], time.Time{})
	if !ok {
		t.Fatal("投递应成功")
	}
	ctrl.CommitDelivery(w, time.Time{})
	before := ctrl.View(g.ID)
	// 人类首轮占 1 轮，这次自动投递再 +1 → 2 轮。
	if before.TurnsUsed != 2 || before.HumanSeq != 1 || before.PairCount[agenthub.ConversationPairKey(a.ID, b.ID)] != 1 {
		t.Fatalf("前置状态 = %+v", before)
	}
	// 旧限额下同一对再 @ 会被跳过（pair_limit=1）。
	if w2, err := ctrl.Offer(agenthub.ConversationOffer{
		GroupID: g.ID, Sender: a.ID, Targets: []agenthub.AgentID{b.ID},
		Text: "第二句", Depth: 2, TriggerSeq: 4, RoundID: "r2",
	}); err != nil || len(w2) != 0 {
		t.Fatalf("旧限额应跳过同一对: %v %+v", err, w2)
	}

	// 保存新设置（不重启）：指纹（mtime+size）变化 → 下一次 hubConversation() 重载。
	time.Sleep(20 * time.Millisecond)
	writeHubConvSettings(t, `{"hub":{"conversation":{"pair_limit":2,"max_depth":7,"run_timeout_ms":2500,"pair_window_ms":600000,"idle_reset_ms":600000}}}`)
	ctrl2 := m.hubConversation()
	if ctrl2 != ctrl {
		t.Fatal("热更新不该重建控制器（对话状态会丢）")
	}
	if got := ctrl2.Limits(); got.PairLimit != 2 || got.MaxDepth != 7 || got.RunTimeout != 2500*time.Millisecond {
		t.Fatalf("热更新后限额 = %+v, want 2/7/2500ms", got)
	}
	// 状态条分母同步（View 直接读新限额）。
	v := ctrl2.View(g.ID)
	if v.PairLimit != 2 || v.MaxDepth != 7 || v.RunTimeoutMS != 2500 {
		t.Fatalf("视图分母 = %+v, want pair=2/depth=7/timeout=2500", v)
	}
	// 旧对话状态不丢。
	if v.ID != before.ID || v.HumanSeq != before.HumanSeq || v.TurnsUsed != before.TurnsUsed {
		t.Fatalf("热更新把对话状态弄丢了: before=%+v after=%+v", before, v)
	}
	if n := v.PairCount[agenthub.ConversationPairKey(a.ID, b.ID)]; n != 1 {
		t.Fatalf("pair 计数丢了: %d", n)
	}
	// 新限额立即用于 Offer：pair_limit=2 时同一对（已用 1 次）还能再投。
	if w3, err := ctrl2.Offer(agenthub.ConversationOffer{
		GroupID: g.ID, Sender: a.ID, Targets: []agenthub.AgentID{b.ID},
		Text: "第三句", Depth: 2, TriggerSeq: 5, RoundID: "r3",
	}); err != nil || len(w3) != 1 {
		t.Fatalf("新 pair_limit 应立即生效: %v %+v", err, w3)
	}
	// 新 run_timeout 立即用于挂超时（旧定时器按旧值跑完是已知语义）。
	m.armHubRunTimeout(g.ID, a.ID, "wake_hot_reload")
	m.hubRunTimerMu.Lock()
	rt := m.hubRunTimers[hubRunTimerKey(g.ID, a.ID)]
	m.hubRunTimerMu.Unlock()
	if rt == nil || rt.timeout != 2500*time.Millisecond {
		t.Fatalf("run 超时没用新值: %+v", rt)
	}
	m.clearHubRunTimeout(g.ID, a.ID)
}
