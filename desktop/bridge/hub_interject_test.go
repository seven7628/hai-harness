package main

// P6-07：人类插话 = 开新 generation，但**不中断在飞 run**；旧 generation 尚未投递的
// auto wake 丢弃、旧 run 的 @ 不得进入新 generation；人类自己的「下一个输入」必须留住
//（目标在跑 → 当前 run 结束后投递）。
//
// 全部用确定性 fake provider（门闩 + 轮询），不碰网络、不靠长 sleep 猜时序。

import (
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"
)

// convRequestText 取一次 LLM 请求里所有正文块拼起来（断言触发消息用）。
func convRequestText(req *provider.StreamRequest) string {
	if req == nil {
		return ""
	}
	var b strings.Builder
	for _, msg := range req.Messages {
		for _, c := range msg.Content {
			if c.Type == core.ContentTypeText {
				b.WriteString(c.Content)
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

// convDeliveryFor 取账本里某 target 最近一条记录。
func convDeliveryFor(m *manager, gid agenthub.GroupID, target agenthub.AgentID) (agenthub.Delivery, bool) {
	var out agenthub.Delivery
	found := false
	for _, d := range m.hubConversation().Deliveries(gid) {
		if d.Target == target {
			out, found = d, true
		}
	}
	return out, found
}

// TestHumanInterjectKeepsNextInputForBusyTarget：人类连着 @ 同一个在跑的成员两次 ——
// 第二条是「下一个输入」，再插一句无 @ 的话换代也不能把它丢掉；目标这一轮结束后
// 它必须真的被投出去（模型看到的是第二件事，不是被吞掉）。
func TestHumanInterjectKeepsNextInputForBusyTarget(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1})

	a := createHubAgent(t, m, buf, 1, "下一输入A")
	b := createHubAgent(t, m, buf, 2, "下一输入B")
	g := convGroupWith(t, m, buf, 10, "下一输入群", map[string]any{"max_turns_per_human": 100}, a, b)
	fb := newConvProvider("B 第一轮跑完", "B 第二件跑完")
	release := fb.gated()
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(b.ID), string(g.ID)), g, string(b.ID), fb)

	// ① 人类 @B：B 开始跑并卡在门闩上。
	if ok, errMsg, _ := hubCmd(t, m, buf, 11, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@下一输入B 第一件事",
	}); !ok {
		t.Fatalf("human send #1: %s", errMsg)
	}
	fb.waitStarted(t)

	// ② 人类再 @B：B 在跑 → 这条进入「下一个输入」队列，不丢、不报 failed。
	if ok, errMsg, data := hubCmd(t, m, buf, 12, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@下一输入B 第二件事",
	}); !ok {
		t.Fatalf("human send #2: %s", errMsg)
	} else if ds, _ := data["deliveries"].([]agenthub.Delivery); len(ds) > 0 && ds[0].Status != agenthub.DeliveryQueued {
		t.Fatalf("忙目标的人类下一输入 = %+v, want queued", ds[0])
	}
	convWait(t, "第二件事排在 B 的下一输入", func() bool {
		v := convView(m, g.ID)
		return v.QueueLen == 1 && v.TurnsUsed == 1
	})

	// ③ 人类插话（无 @）：开新 generation。第二件事属于「下一个输入」，不能被换代清掉。
	if ok, errMsg, _ := hubCmd(t, m, buf, 13, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "先停一下，等我看完",
	}); !ok {
		t.Fatalf("human interject: %s", errMsg)
	}
	if v := convView(m, g.ID); v.QueueLen != 1 {
		t.Fatalf("换代丢掉了人类的下一输入：queue=%d want 1 (%+v)", v.QueueLen, v)
	}
	if d, ok := convDeliveryFor(m, g.ID, b.ID); !ok || d.Status != agenthub.DeliveryQueued {
		t.Fatalf("第二件事的账本 = %+v ok=%v, want queued（不能是 skipped）", d, ok)
	}

	// ④ B 第一轮结束 → 排队的那条必须被投出去，模型看到的是「第二件事」。
	close(release)
	convWait(t, "B 的第二件事被投递", func() bool { return fb.callCount() >= 2 })
	if text := convRequestText(fb.lastRequest()); !strings.Contains(text, "第二件事") {
		t.Fatalf("B 第二次被唤醒看到的不是第二件事：%q", text)
	}
	convWait(t, "第二件事跑完收口", func() bool { return convView(m, g.ID).QueueLen == 0 })
}

// TestHumanInterjectDoesNotInterruptInflightRun：插话只换代，不腰斩在飞 run ——
// 旧 run 的 final 照常落账（正文属于旧 generation，人已经看到了）。
func TestHumanInterjectDoesNotInterruptInflightRun(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1})

	a := createHubAgent(t, m, buf, 1, "不中断A")
	b := createHubAgent(t, m, buf, 2, "不中断B")
	g := convGroupWith(t, m, buf, 10, "不中断群", map[string]any{"max_turns_per_human": 100}, a, b)
	fa := newConvProvider("A 想完了")
	release := fa.gated()
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(a.ID), string(g.ID)), g, string(a.ID), fa)

	if ok, errMsg, _ := hubCmd(t, m, buf, 11, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@不中断A 慢慢想",
	}); !ok {
		t.Fatalf("human send: %s", errMsg)
	}
	fa.waitStarted(t)
	bsA := mustGroupSession(t, m, string(a.ID), string(g.ID))

	if ok, errMsg, _ := hubCmd(t, m, buf, 12, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "补充一句：别急",
	}); !ok {
		t.Fatalf("human interject: %s", errMsg)
	}
	// 在飞 run 没有被中断：会话还在跑，群里也还没有 A 的发言。
	if !bsA.running.Load() || !bsA.hubGroupBusy.Load() {
		t.Fatal("人类插话中断了在飞 run（run 必须继续）")
	}
	if n := convAgentMessages(convGroupMessages(t, m, g.ID), a.ID); n != 0 {
		t.Fatalf("插话之后还没收尾就冒出 %d 条 A 的发言", n)
	}

	close(release)
	convWait(t, "旧 run 的 final 照常落账", func() bool {
		return convAgentMessages(convGroupMessages(t, m, g.ID), a.ID) == 1
	})
	// 旧 run 的投递记录收口成 done（它确实跑完了，不是被中断）。
	// 收口挂在 emit 末尾的 hubOnRunEnd，比 final 落账晚一拍，所以轮询等它。
	var got agenthub.Delivery
	convWait(t, "旧 run 的投递收口成 done", func() bool {
		d, ok := convDeliveryFor(m, g.ID, a.ID)
		got = d
		return ok && d.Status == agenthub.DeliveryDone
	})
	if got.Status != agenthub.DeliveryDone {
		t.Fatalf("旧 run 的账本 = %+v, want done", got)
	}
}

// TestHumanInterjectOldRunFinalDoesNotRoute：旧 generation 的 run 收尾时正文里的 @
// 不得把新 generation 的成员叫醒（正文照发，路由丢弃并记 skipped(stale)）。
func TestHumanInterjectOldRunFinalDoesNotRoute(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1, MaxDepth: 99})

	a := createHubAgent(t, m, buf, 1, "终稿A")
	c := createHubAgent(t, m, buf, 2, "终稿C")
	g := convGroupWith(t, m, buf, 10, "终稿群", map[string]any{"max_turns_per_human": 100}, a, c)
	fa := newConvProvider("@终稿C 旧一轮的终稿")
	release := fa.gated()
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(a.ID), string(g.ID)), g, string(a.ID), fa)
	fc := newConvProvider("C 不该被叫醒")
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(c.ID), string(g.ID)), g, string(c.ID), fc)

	if ok, errMsg, _ := hubCmd(t, m, buf, 11, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@终稿A 你说说看",
	}); !ok {
		t.Fatalf("human send: %s", errMsg)
	}
	fa.waitStarted(t)
	if ok, errMsg, _ := hubCmd(t, m, buf, 12, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "等一下，我改主意了",
	}); !ok {
		t.Fatalf("human interject: %s", errMsg)
	}
	newConv := convView(m, g.ID).ID

	// 旧 run 收尾：正文照发（旧 generation 的产出），@ 不路由。
	close(release)
	waitGroupMessages(t, m, g.ID, func(msgs []agenthub.Message) bool {
		return convAgentMessages(msgs, a.ID) == 1
	})
	time.Sleep(150 * time.Millisecond) // 给可能的错误路由留出投递窗口
	if n := fc.callCount(); n != 0 {
		t.Fatalf("旧 generation 的 final 把 C 叫醒了 %d 次", n)
	}
	d, ok := convDeliveryFor(m, g.ID, c.ID)
	if !ok || d.Status != agenthub.DeliverySkipped || d.Reason != agenthub.DeliveryReasonStale {
		t.Fatalf("C 的账本 = %+v ok=%v, want skipped(stale)", d, ok)
	}
	// 新 generation 没有被旧 run 污染。
	v := convView(m, g.ID)
	if v.ID != newConv || v.TurnsUsed != 0 || v.Depth != 0 {
		t.Fatalf("新 generation 被旧 run 污染：%+v", v)
	}
}
