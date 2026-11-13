package main

// Review 场景 3 N1：Project（workspace 目标）任务的结果回推会唤醒发起者的群会话。
// 这一轮 run 没有 hub_group_wake —— 修复前 finishHubReply 直接 return，模型产生的
// 「收到项目结果，我来总结」只留在会话历史里，群里只剩一张结果卡。
//
// 这里锚定修复后的口径：
//   - 回推 run 的非空正文 = 群里**恰好一条**发起者气泡（sender = 发起者，
//     round = nil，无 mentions/context）；
//   - 不唤醒旁观者、不产生 @ 路由、不启动自动对话、不写第二张结果卡；
//   - 普通无 wake 的群 Ask 仍然静默（防止「任何无 wake 都落账」的宽条件）；
//   - 空正文 / 被中断的 run 不写（不造空气泡、不写半截）；
//   - 标记一次性：同一条结果不会被第二个 run 再认领一遍。

import (
	"context"
	"testing"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

// gwAgentBubbles 取某智能体在群里的普通正文气泡（结果卡是 KindTaskResult，不计入）。
func gwAgentBubbles(msgs []agenthub.Message, id agenthub.AgentID) []agenthub.Message {
	out := make([]agenthub.Message, 0, 1)
	for _, m := range msgs {
		if m.Kind == agenthub.KindText && m.Sender.Kind == agenthub.SenderAgent && m.Sender.AgentID == id {
			out = append(out, m)
		}
	}
	return out
}

// TestGroupTaskResultFollowUpLandsSummaryBubble 是 N1 的主回归：复用会话 3 的
// 派发 fixture（群内 A 用 session_send 把任务派给 Project 会话），断言 Project 主
// 会话回推之后，A 的总结正文真的进了群日志。
func TestGroupTaskResultFollowUpLandsSummaryBubble(t *testing.T) {
	f := newGroupWorkspaceFixture(t)
	task := f.task

	convWait(t, "发起者的总结落成群气泡", func() bool {
		return len(gwAgentBubbles(p3bMessages(t, f.m, f.g.ID), f.a.ID)) >= 1
	})
	// 等这轮回推 run 的 agent_end 落进事件流再数：气泡是在 emit 里写的，事件行在
	// emit 末尾 —— 有了它才能断定「不会再多第二条」，而不是恰好抢在第二条之前。
	convWait(t, "回推 run 的 agent_end", func() bool {
		for _, ev := range readAllLines(f.buf) {
			if ev["event_type"] == string(events.AgentEndType) && ev["session_id"] == f.bsA.id {
				return true
			}
		}
		return false
	})
	msgs := p3bMessages(t, f.m, f.g.ID)
	bubbles := gwAgentBubbles(msgs, f.a.ID)
	if len(bubbles) != 1 {
		t.Fatalf("发起者群气泡 = %d, want 1（msgs=%+v）", len(bubbles), msgs)
	}
	b := bubbles[0]
	// 正文就是模型这一轮的输出（fake provider 的回复）。
	if b.Text != "收到项目结果，我来总结" {
		t.Fatalf("总结气泡正文 = %q, want 模型输出", b.Text)
	}
	// 不路由 / 不 @ / 无 round：这条气泡不对应任何轮次。
	if len(b.Mentions) != 0 || b.MentionAll || b.RoundID != "" || b.ReplyToSeq != 0 {
		t.Fatalf("总结气泡带了 mentions/round/回执: %+v", b)
	}
	if b.Context != nil {
		t.Fatalf("总结气泡不该有 TurnContext: %+v", b.Context)
	}
	// 结果卡仍然只有一张（回推只写卡、总结只写气泡，互不重复）。
	if n := len(p3bTaskResultCards(msgs, task.ID)); n != 1 {
		t.Fatalf("结果卡 = %d, want 1（msgs=%+v）", n, msgs)
	}
	// 旁观者 B 不被唤醒，也没有任何 @ 路由 / 自动对话被启动。
	if n := f.fb.callCount(); n != 0 {
		t.Fatalf("旁观者被唤醒 %d 次（总结气泡不能 @ 任何人）", n)
	}
	if n := gwGroupRounds(f.buf, f.g.ID); n != 0 {
		t.Fatalf("总结气泡产生了 %d 条 @ 路由（hub_round）, want 0", n)
	}
	if v := convView(f.m, f.g.ID); v.TurnsUsed != 0 || v.Status == agenthub.ConversationRunning {
		t.Fatalf("总结气泡启动了自动对话: %+v", v)
	}
	// 回落：这条气泡必须出现在群消息事件流里（UI 才画得出来），而不只是落盘。
	found := false
	for _, ev := range readAllLines(f.buf) {
		if ev["event_type"] == "hub_message" && ev["group_id"] == string(f.g.ID) {
			if msg, ok := ev["message"].(map[string]any); ok {
				if msg["text"] == b.Text && msg["id"] == b.ID {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatalf("群里没有发出总结气泡的 hub_message 事件: %+v", b)
	}
	// 只推一次结果（不能因为落账又触发一次回推）。
	if n := p3bEventCount(t, f.buf, string(events.TaskResultDeliveredType), task.ID); n != 1 {
		t.Fatalf("task_result_delivered = %d, want 1", n)
	}
}

// TestGroupPlainAskWithoutWakeStaysSilent 是负对照：群会话里**普通**的无 wake Ask
// （例如宿主直接调 API、或没有唤醒头的进程内消息）仍然保持原「静默」语义。
// 放宽成「wake == nil 就落账」会让这类 run 的正文也变成群气泡 —— 必须失败。
//
// 用一个独立群/会话而不是会话 3 的 fixture：这里的回复必须与任何回推总结不同，
// 否则落账去重（sameGroupReply）会先把变异盖住，负对照就失去意义。
func TestGroupPlainAskWithoutWakeStaysSilent(t *testing.T) {
	m, buf := testManager(t)
	a := createDispatchAgent(t, m, buf, 1, "静默A", true)
	b := createDispatchAgent(t, m, buf, 2, "旁观B", false)
	g := assignGroup(t, m, buf, 3, "静默群", map[agenthub.AgentID]string{
		a.ID: "owner", b.ID: "member",
	})
	bs := mustGroupSession(t, m, string(a.ID), string(g.ID))
	fa := newConvProvider("普通无 wake 的正文不该进群")
	setGroupFakeLoop(t, m, bs, g, string(a.ID), fa)
	bsB := mustGroupSession(t, m, string(b.ID), string(g.ID))
	fb := newConvProvider("我不该被叫醒")
	setGroupFakeLoop(t, m, bsB, g, string(b.ID), fb)

	// 无 hub_group_wake、无任务结果：直接把一段用户文本投进群会话（宿主 API 语义）。
	if err := bs.s.Ask(context.Background(), core.NewUserMessage(
		core.Content{Type: core.ContentTypeText, Content: "普通 API 调用"},
	)); err != nil {
		t.Fatal(err)
	}
	bs.wakeRun()
	convWait(t, "普通 Ask 跑完", func() bool { return fa.callCount() >= 1 })
	// 确定性锚点：等这轮的 agent_end 落进事件流（emit 里 finishHubReply 之后才写），
	// 而不是 sleep 猜延迟 —— 否则「没写气泡」可能只是还没收尾。
	convWait(t, "普通 Ask 的 agent_end", func() bool {
		for _, ev := range readAllLines(buf) {
			if ev["event_type"] == string(events.AgentEndType) && ev["session_id"] == bs.id {
				return true
			}
		}
		return false
	})

	if n := p3bKindCount(p3bMessages(t, m, g.ID), agenthub.KindText); n != 0 {
		t.Fatalf("普通无 wake Ask 写了 %d 条气泡：静默语义被放宽了", n)
	}
	if n := fb.callCount(); n != 0 {
		t.Fatalf("旁观者被唤醒 %d 次", n)
	}
	if n := gwGroupRounds(buf, g.ID); n != 0 {
		t.Fatalf("产生了 %d 条 @ 路由, want 0", n)
	}
}

// TestGroupTaskResultFollowUpSkipsEmptyAndAborted：模型不输出（空正文）不造空气泡；
// 被中断/失败/截断的 run 不写半截总结（结果卡已经承载终态）。
func TestGroupTaskResultFollowUpSkipsEmptyAndAborted(t *testing.T) {
	m, buf := testManager(t)
	a := createDispatchAgent(t, m, buf, 1, "总结A", true)
	g := assignGroup(t, m, buf, 3, "总结群", map[agenthub.AgentID]string{a.ID: "owner"})
	bs := mustGroupSession(t, m, string(a.ID), string(g.ID))
	spec := p3bGroupSpec(t, m, g, a.ID)

	const id = agenthub.TaskID("task_summary_skip")
	start := core.NewTaskResultMessageWithStatus(string(id), "做一份对比表", "done", "completed", nil)
	claim := func() {
		t.Helper()
		bs.markHubTaskResultPush(id)
		if !bs.claimHubTaskResultPush(start.Content[0].Content) {
			t.Fatal("回推标记没被认领：AgentStart 的起始正文匹配不上")
		}
	}

	// 1) 空正文 + 正常结束：一条都不写（全空段也不写空串）。
	claim()
	m.finishHubReply(bs, spec, &events.AgentEnd{Content: "   ", FinishReason: core.FinishReasonStop}, nil)
	if msgs := p3bMessages(t, m, g.ID); len(gwAgentBubbles(msgs, a.ID)) != 0 {
		t.Fatalf("空正文写了气泡: %+v", msgs)
	}

	// 2) 有正文但 run 被中断：不写半截（结果卡承载「已中断」）。
	claim()
	m.finishHubReply(bs, spec, &events.AgentEnd{
		Content: "收到项目结果，我来总", FinishReason: core.FinishReasonAbort,
	}, nil)
	if msgs := p3bMessages(t, m, g.ID); len(gwAgentBubbles(msgs, a.ID)) != 0 {
		t.Fatalf("被中断的 run 写了半截气泡: %+v", msgs)
	}

	// 3) 标记一次性：正常结束后同一轮再收尾（重复 AgentEnd）不再写第二条。
	claim()
	m.finishHubReply(bs, spec, &events.AgentEnd{Content: "总结：三家都支持导出", FinishReason: core.FinishReasonStop}, nil)
	m.finishHubReply(bs, spec, &events.AgentEnd{Content: "总结：三家都支持导出", FinishReason: core.FinishReasonStop}, nil)
	bubbles := gwAgentBubbles(p3bMessages(t, m, g.ID), a.ID)
	if len(bubbles) != 1 {
		t.Fatalf("气泡 = %d, want 1（重复收尾不能写第二条）", len(bubbles))
	}
	if bubbles[0].Sender.AgentID != a.ID || bubbles[0].RoundID != "" {
		t.Fatalf("气泡归属不对: %+v", bubbles[0])
	}
}

// TestClaimHubTaskResultPushIsStrict 锁定认领规则本身：只有「已登记 id + 起始正文
// 恰好是那条 task_result」才认领；别的正文/别的 id 一概不认（否则任何无 wake 的
// run 都可能被当成回推 run）。
func TestClaimHubTaskResultPushIsStrict(t *testing.T) {
	bs := &bridgeSession{}
	other := core.NewTaskResultMessageWithStatus("task_other", "", "done", "completed", nil)
	// 没登记过：即便是 task_result 正文也不认（普通子 agent 结果回推不走这条路）。
	if bs.claimHubTaskResultPush(other.Content[0].Content) {
		t.Fatal("未登记的 task_result 被认领")
	}
	bs.markHubTaskResultPush("task_wanted")
	// 普通用户文本、别的任务 id 都不认。
	for _, content := range []string{"普通 API 调用", other.Content[0].Content, ""} {
		if bs.claimHubTaskResultPush(content) {
			t.Fatalf("错误认领了 %q", content)
		}
	}
	wanted := core.NewTaskResultMessageWithStatus("task_wanted", "", "done", "completed", nil)
	if !bs.claimHubTaskResultPush(wanted.Content[0].Content) {
		t.Fatal("登记的 task_result 没被认领")
	}
	// 一次性：同一条结果不会被第二个 run 再认领。
	if bs.claimHubTaskResultPush(wanted.Content[0].Content) {
		t.Fatal("同一条结果被认领了两次")
	}
	// Ask 失败撤回后也不再认领。
	bs.markHubTaskResultPush("task_wanted")
	bs.unmarkHubTaskResultPush("task_wanted")
	if bs.claimHubTaskResultPush(wanted.Content[0].Content) {
		t.Fatal("撤回的登记仍被认领")
	}
	// take 之后清空，不会外溢到下一个 run。
	bs.markHubTaskResultPush("task_wanted")
	if !bs.claimHubTaskResultPush(wanted.Content[0].Content) {
		t.Fatal("重新登记后认领失败")
	}
	if id := bs.takeHubTaskResultTask(); id != "task_wanted" {
		t.Fatalf("take = %q", id)
	}
	if id := bs.takeHubTaskResultTask(); id != "" {
		t.Fatalf("take 一次没清空: %q", id)
	}
}
