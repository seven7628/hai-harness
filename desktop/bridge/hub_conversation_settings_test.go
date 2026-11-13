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
