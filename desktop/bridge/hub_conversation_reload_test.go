package main

// P7-04 / I1 收尾：设置保存后立即刷新状态条 + 已挂 run 超时热更新。
//
// 修复前的现状：
//   - `reload_settings` 只让限额在下一次 hubConversation() 时生效，状态条（hub_conversation
//     事件）要等群里下一次有人说话才回显新分母 —— 用户改完设置看不到任何变化；
//   - run_timeout_ms 只影响**之后**挂上的定时器，已经挂着的 10 分钟定时器照旧跑完，
//     「改成 100ms 立刻生效」是句空话。
//
// 反证价值：把 refreshHubConversationLimits 从 reload_settings 摘掉 → 测试 1 红；
// 把 rearmHubRunTimeouts 摘掉 → 测试 2 的「定时器被换掉」与测试 3 的「100ms 真的
// 中断」都红。

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
)

// convReloadSettings 发 reload_settings，返回**这条命令自己**新增的 hub_conversation
// 事件（用命令前的输出长度当游标：历史事件不能冒充「刚刷新过」）。
func convReloadSettings(t *testing.T, m *manager, buf *syncBuf, id int) []map[string]any {
	t.Helper()
	before := buf.Len()
	m.dispatch(cmd(id, "reload_settings", map[string]any{}))
	out := buf.String()
	if len(out) < before {
		t.Fatalf("输出缓冲变短了（%d → %d），无法按游标取事件", before, len(out))
	}
	delta := out[before:]
	okResp := false
	var events []map[string]any
	for _, line := range strings.Split(delta, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var msg map[string]any
		if json.Unmarshal([]byte(line), &msg) != nil {
			continue
		}
		switch msg["event_type"] {
		case "hub_conversation":
			events = append(events, msg)
		case "command_response":
			if got, _ := msg["id"].(float64); int(got) == id {
				okResp = msg["ok"] == true
			}
		}
	}
	if !okResp {
		t.Fatalf("reload_settings 没有成功回执（新增输出:\n%s）", delta)
	}
	return events
}

// convEventInt 取事件里 conversation 的数值字段（JSON 解出来是 float64）。
func convEventInt(t *testing.T, conv map[string]any, key string) int {
	t.Helper()
	f, ok := conv[key].(float64)
	if !ok {
		t.Fatalf("conversation.%s 不是数值: %+v", key, conv)
	}
	return int(f)
}

// convRunTimer 取某 (group, agent) 当前挂着的 run 超时定时器（nil = 没挂）。
func convRunTimer(m *manager, gid agenthub.GroupID, aid agenthub.AgentID) *hubRunTimer {
	m.hubRunTimerMu.Lock()
	defer m.hubRunTimerMu.Unlock()
	return m.hubRunTimers[hubRunTimerKey(gid, aid)]
}

// convSeedRunningDelivery 在控制器里造一条「已投递成功、还停在 running」的账本记录，
// 返回它的 wake id。用来给 run 超时定时器一个真实的复核对象。
func convSeedRunningDelivery(t *testing.T, ctrl *agenthub.ConversationController, gid agenthub.GroupID, sender, target agenthub.AgentID) string {
	t.Helper()
	ctrl.BeginHuman(gid, 1, time.Time{})
	ctrl.StartHumanRound(gid, 1, time.Time{})
	wakes, err := ctrl.Offer(agenthub.ConversationOffer{
		GroupID: gid, Sender: sender, Targets: []agenthub.AgentID{target},
		Text: "@" + string(target) + " 你先来", Depth: 2, TriggerSeq: 2, RoundID: "r1",
	})
	if err != nil || len(wakes) != 1 {
		t.Fatalf("首次 @ 应入队: %v %+v", err, wakes)
	}
	w, ok := ctrl.BeginDelivery(wakes[0], time.Time{})
	if !ok {
		t.Fatal("首次投递应成功")
	}
	ctrl.CommitDelivery(w, time.Time{})
	return w.ID
}

// TestHubReloadSettingsRefreshesConversationBar：reload_settings 一返回就要有新的
// hub_conversation 事件（新 pair_limit / max_depth / run_timeout_ms），且旧对话状态
// （对话 id / 轮次 / pair 计数）一条都不丢。
func TestHubReloadSettingsRefreshesConversationBar(t *testing.T) {
	m, buf := testManager(t)
	writeHubConvSettings(t, `{"hub":{"conversation":{"pair_limit":1,"max_depth":3,"run_timeout_ms":60000}}}`)

	a := createHubAgent(t, m, buf, 1, "刷新A")
	b := createHubAgent(t, m, buf, 2, "刷新B")
	g := convGroupWith(t, m, buf, 10, "刷新群", nil, a, b)

	ctrl := m.hubConversation() // 首次读取：pair 1 / 深度 3 / 超时 60s
	if got := ctrl.Limits(); got.PairLimit != 1 || got.MaxDepth != 3 || got.RunTimeout != time.Minute {
		t.Fatalf("初始限额 = %+v, want pair=1/depth=3/timeout=1m", got)
	}
	convSeedRunningDelivery(t, ctrl, g.ID, a.ID, b.ID)
	before := ctrl.View(g.ID)
	if before.PairCount[agenthub.ConversationPairKey(a.ID, b.ID)] != 1 {
		t.Fatalf("前置 pair 计数 = %+v, want 1", before.PairCount)
	}
	m.clearHubRunTimeout(g.ID, b.ID) // 这个用例只看事件，不需要定时器

	// 保存新设置（内容长度也变了，指纹一定不同）→ reload_settings 立即刷新状态条。
	time.Sleep(20 * time.Millisecond)
	writeHubConvSettings(t, `{"hub":{"conversation":{"pair_limit":5,"max_depth":9,"run_timeout_ms":250}}}`)
	events := convReloadSettings(t, m, buf, 20)

	var ev map[string]any
	for _, e := range events {
		if gid, _ := e["group_id"].(string); gid == string(g.ID) {
			ev = e
			break
		}
	}
	if ev == nil {
		t.Fatalf("reload_settings 没有为群 %s 广播 hub_conversation（只拿到 %d 条事件）", g.ID, len(events))
	}
	conv, _ := ev["conversation"].(map[string]any)
	if conv == nil {
		t.Fatalf("hub_conversation 事件缺 conversation: %+v", ev)
	}
	if got := convEventInt(t, conv, "pair_limit"); got != 5 {
		t.Fatalf("刷新后的 pair_limit = %d, want 5", got)
	}
	if got := convEventInt(t, conv, "max_depth"); got != 9 {
		t.Fatalf("刷新后的 max_depth = %d, want 9", got)
	}
	if got := convEventInt(t, conv, "run_timeout_ms"); got != 250 {
		t.Fatalf("刷新后的 run_timeout_ms = %d, want 250", got)
	}
	// 旧对话状态不丢：事件里的 id / 轮次 / pair 计数与命令前一致。
	if id, _ := conv["id"].(string); id != before.ID || id == "" {
		t.Fatalf("刷新把对话 id 弄丢了: got=%q want=%q", conv["id"], before.ID)
	}
	if got := convEventInt(t, conv, "turns_used"); got != before.TurnsUsed {
		t.Fatalf("刷新把轮次弄丢了: got=%d want=%d", got, before.TurnsUsed)
	}
	pairs, _ := conv["pair_count"].(map[string]any)
	key := agenthub.ConversationPairKey(a.ID, b.ID)
	if n, ok := pairs[key].(float64); !ok || int(n) != 1 {
		t.Fatalf("刷新把 pair 计数弄丢了: %+v", conv["pair_count"])
	}
	// 控制器本身也真的换成了新限额（事件不是拼出来的假值）。
	if got := ctrl.Limits(); got.PairLimit != 5 || got.MaxDepth != 9 || got.RunTimeout != 250*time.Millisecond {
		t.Fatalf("热更新后限额 = %+v, want pair=5/depth=9/timeout=250ms", got)
	}
}

// TestHubReloadSettingsRearmsRunTimeout：已挂的 1 分钟定时器在 run_timeout_ms 改成 5s
// 后被**换掉**（新值从现在起算），旧定时器被 Stop，账本仍停在 running。
func TestHubReloadSettingsRearmsRunTimeout(t *testing.T) {
	m, buf := testManager(t)
	writeHubConvSettings(t, `{"hub":{"conversation":{"run_timeout_ms":60000}}}`)

	a := createHubAgent(t, m, buf, 1, "重挂A")
	b := createHubAgent(t, m, buf, 2, "重挂B")
	g := convGroupWith(t, m, buf, 10, "重挂群", nil, a, b)

	ctrl := m.hubConversation()
	wakeID := convSeedRunningDelivery(t, ctrl, g.ID, a.ID, b.ID)
	m.armHubRunTimeout(g.ID, b.ID, wakeID)
	old := convRunTimer(m, g.ID, b.ID)
	if old == nil || old.timeout != time.Minute {
		t.Fatalf("前置 run 超时 = %+v, want 1m", old)
	}

	time.Sleep(20 * time.Millisecond)
	writeHubConvSettings(t, `{"hub":{"conversation":{"run_timeout_ms":5000}}}`)
	convReloadSettings(t, m, buf, 20)

	cur := convRunTimer(m, g.ID, b.ID)
	if cur == nil {
		t.Fatal("热更新后没有重挂 run 超时定时器")
	}
	if cur == old {
		t.Fatal("run 超时定时器没重挂（还是同一个，旧值照旧跑完）")
	}
	if cur.timeout != 5*time.Second {
		t.Fatalf("重挂后的 timeout = %v, want 5s（从现在起算）", cur.timeout)
	}
	// 旧定时器必须已被 Stop：再 Stop 一次返回 false 就是证据（1 分钟的旧定时器不可能自己到点）。
	if old.timer.Stop() {
		t.Fatal("旧 run 超时定时器没被 Stop（它还会在旧值到点时误杀这一轮）")
	}
	// 账本没被弄坏：还在 running，重挂不该收口投递。
	if d, ok := ctrl.Delivery(g.ID, wakeID); !ok || d.Status != agenthub.DeliveryRunning {
		t.Fatalf("重挂把账本改了: %+v ok=%v", d, ok)
	}

	// 上限没变时**不该**重挂：保存主题/语言之类无关设置不能把倒计时重置。
	time.Sleep(20 * time.Millisecond)
	writeHubConvSettings(t, `{"hub":{"conversation":{"run_timeout_ms":5000,"pair_limit":7}}}`)
	convReloadSettings(t, m, buf, 21)
	if again := convRunTimer(m, g.ID, b.ID); again != cur {
		t.Fatal("run_timeout 没变却重挂了定时器（无关设置的保存会重置倒计时）")
	}
	m.clearHubRunTimeout(g.ID, b.ID)
}

// TestHubReloadSettingsRunTimeoutFiresWithNewValue：行为级证明 —— 一轮本来挂着 10 分钟
// 上限、卡在门闩上永不自己结束；改成 100ms 并 reload 后，这一轮必须被**新的**上限中断
// （账本 failed(run_timeout) + 系统说明），而不是等满 10 分钟。
func TestHubReloadSettingsRunTimeoutFiresWithNewValue(t *testing.T) {
	m, buf := testManager(t)
	writeHubConvSettings(t, `{"hub":{"conversation":{"run_timeout_ms":600000}}}`)

	a := createHubAgent(t, m, buf, 1, "热超时A")
	b := createHubAgent(t, m, buf, 2, "热超时B")
	g := convGroupWith(t, m, buf, 10, "热超时群", map[string]any{"max_turns_per_human": 100}, a, b)
	fa := newConvProvider("不该跑到这里")
	release := releaseOnce(fa.gated())
	defer release()
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(a.ID), string(g.ID)), g, string(a.ID), fa)

	if ok, errMsg, _ := hubCmd(t, m, buf, 11, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@热超时A 你先跑着",
	}); !ok {
		t.Fatalf("human send: %s", errMsg)
	}
	fa.waitStarted(t) // A 卡在门闩上（永远不自己结束）

	old := convRunTimer(m, g.ID, a.ID)
	if old == nil || old.timeout != 10*time.Minute {
		t.Fatalf("前置 run 超时 = %+v, want 10m", old)
	}

	time.Sleep(20 * time.Millisecond)
	writeHubConvSettings(t, `{"hub":{"conversation":{"run_timeout_ms":100}}}`)
	convReloadSettings(t, m, buf, 20)
	if old.timer.Stop() {
		t.Fatal("旧（10 分钟）定时器没被 Stop：热更新只挂了一个新的，旧的还会再杀一次")
	}

	convWait(t, "热更新后的 100ms 上限中断了 A 的 run", func() bool {
		bs, err := m.hubGroupSession(string(a.ID), string(g.ID))
		return err == nil && !groupSessionActive(bs)
	})
	ds := convDeliveriesFor(m, g.ID, a.ID)
	if len(ds) == 0 || ds[0].Status != agenthub.DeliveryFailed || ds[0].Reason != agenthub.DeliveryReasonTimeout {
		t.Fatalf("超时那一轮的账本 = %+v, want failed(run_timeout)", ds)
	}
	msgs := waitConvSystemMessage(t, m, g.ID, "已按单轮上限中断")
	sys := convSystemMessages(msgs)
	if !strings.Contains(sys[len(sys)-1].Text, "不到 1 秒") {
		t.Fatalf("超时说明没提新上限（应说「不到 1 秒」）: %+v", sys[len(sys)-1])
	}
}
