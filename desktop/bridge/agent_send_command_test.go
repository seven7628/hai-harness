package main

import (
	"context"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/subagent"
)

// —— 用户直接对子智能体说话（agent_send 命令）——
//
// 背景（用户 2026-10 要求）：子会话窗口里的输入框应该发给**正在看的那个子智能体**，
// 而不是一律发主会话。落地上它是 bridge 命令 agent_send → subagent.Registry.Send，
// 与模型调用 agent_send 工具**同一条通道**（同一份 Task.inbox、同一套运行中判定）。
//
// 这个命令的关键不是「能发」，而是**三种情况要能分开**：
//
//	· 任务已结束 → 自动原地续跑（agent_resume 同链路）后再投，不是让用户先手动恢复
//	· 队列满     → 稍后再发（消息没有丢，是被拒绝）
//	· 找不到任务 → 这个 id 根本不存在（视图可能指向了已清理的任务）
//
// 都回同一个 "failed" 的话，前端只能显示「发送失败」，用户无从判断下一步做什么。
func TestAgentSendCommand(t *testing.T) {
	ws := t.TempDir()
	m, buf := testManager(t)
	sid := newSessionID(t, m, buf, ws)
	bs := m.get(ws, sid)
	if bs == nil {
		t.Fatal("session not found")
	}

	// 一个跑着的任务：Send 应当成功（消息进 inbox，等它的 stop 轮消费）
	release := make(chan struct{})
	task, err := bs.s.Registry().Start(context.Background(), "review", func(_ context.Context, _ *subagent.Task) (string, core.Usage, error) {
		<-release
		return "done", core.Usage{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			close(release)
		}
		_, _ = bs.s.Registry().Wait(task.ID)
		_ = bs.s.Shutdown(context.Background())
		bs.cancel()
		waitStoreSettled(t, ws, bs.s.ID())
	}()

	respOf := func(id int) map[string]any {
		return findFirst(buf.String(), func(mm map[string]any) bool {
			return mm["event_type"] == "command_response" && mm["id"] == float64(id)
		})
	}

	// ① 运行中 → ok
	buf.Reset()
	m.dispatch(cmd(1, "agent_send", map[string]any{"workspace": ws, "session_id": sid, "task_id": task.ID, "text": "把返回 401 的分支也改掉"}))
	if r := respOf(1); r == nil || r["ok"] != true {
		t.Fatalf("agent_send 到运行中的任务应当成功: %+v", r)
	}

	// ② 缺参数 → bad_request（不是 task_not_found：先要说清是调用方漏了字段）
	buf.Reset()
	m.dispatch(cmd(2, "agent_send", map[string]any{"workspace": ws, "session_id": sid, "task_id": task.ID}))
	if r := respOf(2); r == nil || r["ok"] != false || str(r, "code") != "bad_request" {
		t.Fatalf("缺 text 应当回 bad_request: %+v", r)
	}

	// ③ 未知任务 → task_not_found
	buf.Reset()
	m.dispatch(cmd(3, "agent_send", map[string]any{"workspace": ws, "session_id": sid, "task_id": "task-does-not-exist", "text": "hi"}))
	if r := respOf(3); r == nil || r["ok"] != false || str(r, "code") != "task_not_found" {
		t.Fatalf("未知任务应当回 task_not_found: %+v", r)
	}

	// ④ 会话不存在 → session_not_found
	buf.Reset()
	m.dispatch(cmd(4, "agent_send", map[string]any{"workspace": ws, "session_id": "no-such-session", "task_id": task.ID, "text": "hi"}))
	if r := respOf(4); r == nil || r["ok"] != false || str(r, "code") != "session_not_found" {
		t.Fatalf("会话不存在应当回 session_not_found: %+v", r)
	}

	// ⑤ 任务结束后 → 自动原地续跑再投（不再让前端先拦「已结束」）
	if bs.resumeTask == nil {
		t.Fatal("agent 工具已装配，但 bridgeSession.resumeTask 未接线")
	}
	var resumedTask, resumedText string
	bs.resumeTask = func(_ context.Context, taskID, message string) (string, error) {
		resumedTask, resumedText = taskID, message
		return taskID, nil
	}
	close(release)
	stopped = true // defer 不再重复 close（会 panic）
	if _, err := bs.s.Registry().Wait(task.ID); err != nil {
		t.Fatalf("等任务结束: %v", err)
	}
	buf.Reset()
	m.dispatch(cmd(5, "agent_send", map[string]any{"workspace": ws, "session_id": sid, "task_id": task.ID, "text": "还在吗"}))
	r5 := respOf(5)
	if r5 == nil || r5["ok"] != true {
		t.Fatalf("任务已结束应自动续跑并成功: %+v", r5)
	}
	data5, _ := r5["data"].(map[string]any)
	if !boolVal(data5["resumed"]) {
		t.Fatalf("响应应标记 resumed=true: %+v", r5)
	}
	if resumedTask != task.ID || resumedText != "还在吗" {
		t.Fatalf("续跑入口应收到原 task 与原消息，got task=%q text=%q", resumedTask, resumedText)
	}

	// ⑥ 没有 resume 入口（Work/旧会话）→ 仍明确回 task_finished，不静默丢弃
	bs.resumeTask = nil
	buf.Reset()
	m.dispatch(cmd(6, "agent_send", map[string]any{"workspace": ws, "session_id": sid, "task_id": task.ID, "text": "还在吗"}))
	if r := respOf(6); r == nil || r["ok"] != false || str(r, "code") != "task_finished" {
		t.Fatalf("无续跑入口时应回 task_finished: %+v", r)
	}
}

// TestAgentSendShowsInTranscript 用户对子智能体说的话，必须出现在**它自己的转录**里。
//
// 用户 2026-10 的质疑：「子智能体发送消息，不能等同于主智能体吗？理论上就是不同的 session
// 管理，只不过子智能体的 session 有父级的 sessionId 而已」。判断正确 —— 在它之前
// agent_send 只往 Task.inbox 投一条 core.Message，宿主看不见，「我发了一句话，屏幕上
// 什么都不显示」。现在由 subagent 的 Poll（所有投递源的唯一汇合点）发
// events.AgentUserMessage → reducer 追加一条 user 项。
//
// 本测试钉住的是**数据链路**（事件 → 卡片 item）：Poll 的触发时机由子 loop 决定，
// 这里直接投事件验证 reducer 侧，避免依赖真实 LLM 轮次。
func TestAgentUserMessageEntersAgentTranscript(t *testing.T) {
	r := NewViewReducer()
	_ = r.ApplyPlain(&events.AgentStart{RunId: "sub-1", ParentRunId: "main-1", Depth: 1, TaskId: "task-1", Name: "auth 审查", Timestamp: time.Now(), EventType: events.AgentStartType})
	_ = r.ApplyPlain(&events.AgentUserMessage{TaskId: "task-1", Text: "把返回 401 的分支也改掉", Source: "user", Timestamp: time.Now(), EventType: events.AgentUserMessageType})

	var card *Block
	for i := range r.blocks {
		if r.blocks[i].Kind == blockKindAgent && r.blocks[i].TaskID == "task-1" {
			card = &r.blocks[i]
		}
	}
	if card == nil {
		t.Fatal("agent 卡片不存在")
	}
	found := false
	for _, it := range card.Items {
		if it.Kind == agentItemUser && it.Text == "把返回 401 的分支也改掉" {
			found = true
		}
	}
	if !found {
		t.Fatalf("用户消息应当进子运行的转录（items=%+v）", card.Items)
	}

	// 未知任务：静默丢弃（消息已在上下文里，只是没有卡片可挂；不能凭空造孤儿卡片）
	before := len(r.blocks)
	_ = r.ApplyPlain(&events.AgentUserMessage{TaskId: "task-nope", Text: "hello", Timestamp: time.Now(), EventType: events.AgentUserMessageType})
	if len(r.blocks) != before {
		t.Fatalf("未知任务不得凭空建卡片，blocks %d → %d", before, len(r.blocks))
	}
}

// TestSubAgentTurnMetaReachesItems 子 agent 每轮的消息指标（模型 / 时刻 / TTFT · LLM 耗时 /
// tokens / Agent 总耗时）必须落到它的转录项上。
//
// 用户 2026-10 报障：「子智能体没有模型信息、消息事件信息、也没有（LLM 总耗时 43.8s ·
// Agent 总耗时 16m 19s · 15.88 tokens/s）」。子会话页复用的是主会话的渲染原语，它按
// assistant 块上的 model / ts / ttftMs / durMs / outputTokens / agentDurMs 渲染这一行；
// 而子 run 的这些值一直只在 reducer 里算进用量、**没落到 items** —— 于是整行都不出现。
func TestSubAgentTurnMetaReachesItems(t *testing.T) {
	r := NewViewReducer()
	base := time.Now()
	_ = r.ApplyPlain(&events.AgentStart{RunId: "sub-1", ParentRunId: "main-1", Depth: 1, TaskId: "task-1",
		Name: "auth 审查", Timestamp: base, EventType: events.AgentStartType})
	_ = r.ApplyPlain(&events.LLMStart{RunId: "sub-1", RequestId: "req-1", Model: "deepseek-v4-flash",
		Timestamp: base.Add(10 * time.Millisecond), EventType: events.LLMStartType})
	_ = r.ApplyPlain(&events.ReasoningChunk{RunId: "sub-1", RequestId: "req-1", Content: "先看目录结构。",
		Timestamp: base.Add(400 * time.Millisecond), EventType: events.ReasoningChunkType})
	_ = r.ApplyPlain(&events.ContentChunk{RunId: "sub-1", RequestId: "req-1", Content: "我先找到工作目录。",
		Timestamp: base.Add(600 * time.Millisecond), EventType: events.ContentChunkType})
	_ = r.ApplyPlain(&events.LLMEnd{RunId: "sub-1", RequestId: "req-1", Model: "deepseek-v4-flash",
		Content: "我先找到工作目录。", Usage: &core.Usage{Input: 1200, Output: 300, Reasoning: 80},
		Timestamp: base.Add(2 * time.Second), EventType: events.LLMEndType})
	_ = r.ApplyPlain(&events.AgentEnd{RunId: "sub-1", FinishReason: core.FinishReasonStop, DurationMS: 979_000,
		Usage: &core.Usage{Input: 1200, Output: 300, Reasoning: 80}, Model: "deepseek-v4-flash",
		Timestamp: base.Add(980 * time.Second), EventType: events.AgentEndType})

	var card *Block
	for i := range r.blocks {
		if r.blocks[i].Kind == blockKindAgent {
			card = &r.blocks[i]
		}
	}
	if card == nil {
		t.Fatal("agent 卡片不存在")
	}
	var text *AgentItem
	for i := range card.Items {
		if card.Items[i].Kind == agentItemText {
			text = &card.Items[i]
		}
	}
	if text == nil {
		t.Fatalf("正文项不存在（items=%+v）", card.Items)
	}
	if text.Model != "deepseek-v4-flash" {
		t.Errorf("模型名没落到项上（got %q）——子会话里就没有「用的是哪个模型」", text.Model)
	}
	if text.Ts == 0 {
		t.Error("时刻没落到项上 —— 子会话里每条消息缺时间")
	}
	if text.DurMs <= 0 {
		t.Errorf("本轮 LLM 耗时没落到项上（got %d）——「LLM 总耗时」那一段会消失", text.DurMs)
	}
	if text.TTFTMs <= 0 {
		t.Errorf("TTFT 没落到项上（got %d）", text.TTFTMs)
	}
	if text.OutputTokens != 300 || text.ReasoningTokens != 80 {
		t.Errorf("输出 token 没落到项上（got out=%d reason=%d）——tokens/s 算不出来",
			text.OutputTokens, text.ReasoningTokens)
	}
	if text.AgentDurMs != 979_000 {
		t.Errorf("Agent 总耗时没落到项上（got %d）——「Agent 总耗时 16m 19s」那一段会消失", text.AgentDurMs)
	}
}
