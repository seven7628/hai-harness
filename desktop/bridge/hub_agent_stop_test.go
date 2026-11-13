package main

// P6-08：按 agent 停止 + run 超时。
//
//   - hub_conversation_stop 带 agent_id 时只中断该成员的群 run、丢掉它排队的唤醒，
//     其他成员照常跑（不带 agent_id 仍是群级停止）。
//   - run_timeout_ms 真的中断超时的一轮，账本记 failed(run_timeout) + 一条系统说明，
//     队列继续（排在那位成员后面的唤醒照常投递）。

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
)

// releaseOnce 让「显式放行 + defer 兜底」不会 double close（门闩 fake 的收尾）。
func releaseOnce(ch chan struct{}) func() {
	var once sync.Once
	return func() { once.Do(func() { close(ch) }) }
}

// convDeliveriesFor 取账本里某 target 的全部记录（旧 → 新）。
func convDeliveriesFor(m *manager, gid agenthub.GroupID, target agenthub.AgentID) []agenthub.Delivery {
	var out []agenthub.Delivery
	for _, d := range m.hubConversation().Deliveries(gid) {
		if d.Target == target {
			out = append(out, d)
		}
	}
	return out
}

// TestHubConversationStopAgentOnlyThatAgent：按 agent 停止只停一个成员，另一个继续跑；
// 对话状态保持 running（不是群级 stopped），停止说明点名字。
func TestHubConversationStopAgentOnlyThatAgent(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1})

	a := createHubAgent(t, m, buf, 1, "单停A")
	b := createHubAgent(t, m, buf, 2, "单停B")
	g := convGroupWith(t, m, buf, 10, "单停群", map[string]any{"max_turns_per_human": 100}, a, b)
	fa := newConvProvider("A 不该写出来")
	releaseA := releaseOnce(fa.gated())
	defer releaseA()
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(a.ID), string(g.ID)), g, string(a.ID), fa)
	fb := newConvProvider("B 继续跑完")
	releaseB := releaseOnce(fb.gated())
	defer releaseB()
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(b.ID), string(g.ID)), g, string(b.ID), fb)

	// 人类同时 @A @B：两个并行跑，都卡在门闩上。
	if ok, errMsg, _ := hubCmd(t, m, buf, 11, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@单停A @单停B 你们各跑各的",
	}); !ok {
		t.Fatalf("human send: %s", errMsg)
	}
	fa.waitStarted(t)
	fb.waitStarted(t)

	ok, errMsg, data := hubCmd(t, m, buf, 12, "hub_conversation_stop", map[string]any{
		"group_id": string(g.ID), "agent_id": string(a.ID),
	})
	if !ok {
		t.Fatalf("按 agent 停止: %s", errMsg)
	}
	if got, _ := data["agent_id"].(string); got != string(a.ID) {
		t.Fatalf("回执 agent_id = %q, want %q", got, a.ID)
	}
	if n, _ := data["interrupted"].(float64); n < 1 {
		t.Fatalf("interrupted = %v, want >= 1", data["interrupted"])
	}
	conv, _ := data["conversation"].(map[string]any)
	if conv == nil || conv["status"] == string(agenthub.ConversationStopped) {
		t.Fatalf("按 agent 停止不该把整个对话标成 stopped：%+v", data["conversation"])
	}
	if conv["status"] != string(agenthub.ConversationRunning) {
		t.Fatalf("对话状态 = %v, want running（其他成员还要继续）", conv["status"])
	}

	// A 的 run 被中断；B 的 run 不受影响（还在跑）。
	convWait(t, "A 的 run 被中断", func() bool {
		bsA, err := m.hubGroupSession(string(a.ID), string(g.ID))
		return err == nil && !bsA.running.Load() && !bsA.hubGroupBusy.Load()
	})
	bsB := mustGroupSession(t, m, string(b.ID), string(g.ID))
	if !bsB.running.Load() || !bsB.hubGroupBusy.Load() {
		t.Fatal("按 agent 停止把 B 的 run 也打断了")
	}
	// 账本：A failed(interrupted)，B 仍是 running。
	dsA := convDeliveriesFor(m, g.ID, a.ID)
	if len(dsA) != 1 || dsA[0].Status != agenthub.DeliveryFailed || dsA[0].Reason != agenthub.DeliveryReasonInterrupted {
		t.Fatalf("A 的账本 = %+v, want failed(interrupted)", dsA)
	}
	dsB := convDeliveriesFor(m, g.ID, b.ID)
	if len(dsB) != 1 || dsB[0].Status != agenthub.DeliveryRunning {
		t.Fatalf("B 的账本 = %+v, want running", dsB)
	}
	// 系统说明点名 A，并且不是群级「停止了自动对话」那句。
	msgs := waitConvSystemMessage(t, m, g.ID, "已停止")
	found := false
	for _, msg := range convSystemMessages(msgs) {
		if strings.Contains(msg.Text, "单停A") && strings.Contains(msg.Text, "其他成员继续") {
			found = true
		}
		if strings.Contains(msg.Text, "人类停止了自动对话") {
			t.Fatalf("按 agent 停止写了群级停止说明：%q", msg.Text)
		}
	}
	if !found {
		t.Fatalf("系统说明没点名被停的成员：%+v", convSystemMessages(msgs))
	}

	// B 照常收尾落账；A 一条都不写（被中断的 run 不写半截正文）。
	releaseB()
	convWait(t, "B 的 final 落账", func() bool {
		return convAgentMessages(convGroupMessages(t, m, g.ID), b.ID) == 1
	})
	if n := convAgentMessages(convGroupMessages(t, m, g.ID), a.ID); n != 0 {
		t.Fatalf("被按 agent 停止的 A 仍写了 %d 条发言", n)
	}
}

// TestHubConversationStopAgentDropsItsQueuedWake：按 agent 停止要把该成员排队的
// 「下一个输入」一起作废（skipped），这一轮结束后不能再冒出来跑。
func TestHubConversationStopAgentDropsItsQueuedWake(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1})

	a := createHubAgent(t, m, buf, 1, "排队A")
	b := createHubAgent(t, m, buf, 2, "排队B")
	g := convGroupWith(t, m, buf, 10, "排队群", map[string]any{"max_turns_per_human": 100}, a, b)
	fa := newConvProvider("A 第一轮", "A 第二轮不该跑")
	release := releaseOnce(fa.gated())
	defer release()
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(a.ID), string(g.ID)), g, string(a.ID), fa)

	if ok, errMsg, _ := hubCmd(t, m, buf, 11, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@排队A 第一件事",
	}); !ok {
		t.Fatalf("human send #1: %s", errMsg)
	}
	fa.waitStarted(t)
	if ok, errMsg, _ := hubCmd(t, m, buf, 12, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@排队A 第二件事",
	}); !ok {
		t.Fatalf("human send #2: %s", errMsg)
	}
	convWait(t, "第二件事排队", func() bool { return convView(m, g.ID).QueueLen == 1 })

	ok, errMsg, data := hubCmd(t, m, buf, 13, "hub_conversation_stop", map[string]any{
		"group_id": string(g.ID), "agent_id": string(a.ID),
	})
	if !ok {
		t.Fatalf("按 agent 停止: %s", errMsg)
	}
	if n, _ := data["skipped"].(float64); n != 1 {
		t.Fatalf("skipped = %v, want 1（排队的第二件事）", data["skipped"])
	}
	if v := convView(m, g.ID); v.QueueLen != 0 {
		t.Fatalf("停止后队列 = %d, want 0", v.QueueLen)
	}
	release()
	time.Sleep(150 * time.Millisecond)
	if n := fa.callCount(); n != 1 {
		t.Fatalf("A 被投递了 %d 次，want 1（排队的唤醒必须作废）", n)
	}
	// 被作废的那条记 skipped(stopped)，不是 failed。
	var last agenthub.Delivery
	ds := convDeliveriesFor(m, g.ID, a.ID)
	if len(ds) > 0 {
		last = ds[len(ds)-1]
	}
	if last.Status != agenthub.DeliverySkipped || last.Reason != agenthub.DeliveryReasonStopped {
		t.Fatalf("排队那条的账本 = %+v, want skipped(stopped)", last)
	}
}

// TestHubConversationRunTimeoutInterruptsAndContinues：run_timeout_ms 到点中断这一轮
// （账本 failed(run_timeout) + 系统说明），并把排在后面的唤醒继续投出去。
func TestHubConversationRunTimeoutInterruptsAndContinues(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1, RunTimeout: 500 * time.Millisecond})

	a := createHubAgent(t, m, buf, 1, "超时A")
	b := createHubAgent(t, m, buf, 2, "超时B")
	g := convGroupWith(t, m, buf, 10, "超时群", map[string]any{"max_turns_per_human": 100}, a, b)
	fa := newConvProvider("A 第二轮")
	release := releaseOnce(fa.gated())
	defer release()
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(a.ID), string(g.ID)), g, string(a.ID), fa)
	fb := newConvProvider("@超时A 该你了")
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(b.ID), string(g.ID)), g, string(b.ID), fb)

	// ① 人类 @A：A 卡在门闩上（永远不自己结束）。
	if ok, errMsg, _ := hubCmd(t, m, buf, 11, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@超时A 你先来",
	}); !ok {
		t.Fatalf("human send #1: %s", errMsg)
	}
	fa.waitStarted(t)

	// ② 人类 @B：B 秒回并 @A —— 这条排队等 A（A 还在跑）。
	if ok, errMsg, _ := hubCmd(t, m, buf, 12, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@超时B 叫一下 A",
	}); !ok {
		t.Fatalf("human send #2: %s", errMsg)
	}
	convWait(t, "B 的 @ 排在 A 后面", func() bool { return convView(m, g.ID).QueueLen == 1 })

	// ③ 超时：A 的 run 被中断（不用等人手动停）。
	convWait(t, "A 的 run 超时被中断", func() bool {
		bs, err := m.hubGroupSession(string(a.ID), string(g.ID))
		return err == nil && !bs.running.Load() && !bs.hubGroupBusy.Load()
	})
	ds := convDeliveriesFor(m, g.ID, a.ID)
	if len(ds) == 0 || ds[0].Status != agenthub.DeliveryFailed || ds[0].Reason != agenthub.DeliveryReasonTimeout {
		t.Fatalf("超时那一轮的账本 = %+v, want failed(run_timeout)", ds)
	}
	msgs := waitConvSystemMessage(t, m, g.ID, "超时A")
	if !strings.Contains(convSystemMessages(msgs)[len(convSystemMessages(msgs))-1].Text, "已按单轮上限中断") {
		t.Fatalf("超时说明没说清怎么处理的：%+v", convSystemMessages(msgs))
	}
	// 队列继续：排在 A 后面的那条唤醒照常投出去（第二轮真的开跑）。
	convWait(t, "队列继续（A 的第二轮开跑）", func() bool { return fa.callCount() >= 2 })
	// 第二轮立刻放行收尾（它自己也有超时定时器；收尾时该定时器要被撤掉）。
	release()

	// 已经收尾的 run 不会再被迟到的定时器补一刀：只留一条超时说明。
	time.Sleep(250 * time.Millisecond)
	n := 0
	for _, msg := range convSystemMessages(convGroupMessages(t, m, g.ID)) {
		if strings.Contains(msg.Text, "超时A") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("超时A 的说明 = %d 条, want 1（收尾后的定时器必须 no-op）", n)
	}
}
