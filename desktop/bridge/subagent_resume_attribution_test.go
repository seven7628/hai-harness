package main

import (
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

// TestAgentResumeKeepsPerRunCards 钉住「一张卡 = 一次运行」的 task 事件归属：
//
//   - agent_resume 复用 task_id，但每次运行是一张新卡；
//   - task_started 不许把上一轮已结算的卡翻回 running；
//   - agent_user_message 必须进**当前 run** 的卡（RunId 优先），没有 RunId 时进最新卡；
//   - task_end / task_result_delivered 只写最新卡，旧卡的时长/结果/已交付标记不变；
//   - 每条 text/tool/user item 都带它自己那次运行的 run_id（前端按 run 切 Worked 盒）。
func TestAgentResumeKeepsPerRunCards(t *testing.T) {
	r := NewViewReducer()
	base := time.Now().Add(-time.Minute)

	subStart := func(runID string, at time.Time) {
		t.Helper()
		if err := r.ApplyPlain(&events.AgentStart{
			RunId: runID, ParentRunId: "main-1", Depth: 1, TaskId: "task-resume",
			Name: "review", Timestamp: at, EventType: events.AgentStartType,
		}); err != nil {
			t.Fatalf("agent_start(%s): %v", runID, err)
		}
	}
	subEnd := func(runID string, dur int64, at time.Time) {
		t.Helper()
		if err := r.ApplyPlain(&events.AgentEnd{
			RunId: runID, ParentRunId: "main-1", TaskId: "task-resume",
			FinishReason: core.FinishReasonStop, DurationMS: dur,
			Timestamp: at, EventType: events.AgentEndType,
		}); err != nil {
			t.Fatalf("agent_end(%s): %v", runID, err)
		}
	}
	deliver := func(result string, at time.Time) {
		t.Helper()
		if err := r.ApplyPlain(&events.TaskEnd{
			TaskId: "task-resume", Status: "completed", Result: result,
			Timestamp: at, EventType: events.TaskEndType,
		}); err != nil {
			t.Fatalf("task_end: %v", err)
		}
		if err := r.ApplyPlain(&events.TaskResultDelivered{
			SessionId: "s1", TaskId: "task-resume", Status: "completed", Result: result,
			Timestamp: at, EventType: events.TaskResultDeliveredType,
		}); err != nil {
			t.Fatalf("task_result_delivered: %v", err)
		}
	}
	agentCards := func() []Block {
		t.Helper()
		var out []Block
		for _, b := range r.Snapshot().Blocks {
			if b.Kind == blockKindAgent && b.TaskID == "task-resume" {
				out = append(out, b)
			}
		}
		return out
	}
	itemTexts := func(b Block) []string {
		var out []string
		for _, it := range b.Items {
			out = append(out, it.Text)
		}
		return out
	}

	// —— run 1 ——
	if err := r.ApplyPlain(&events.TaskStarted{
		TaskId: "task-resume", Name: "review", Timestamp: base, EventType: events.TaskStartedType,
	}); err != nil {
		t.Fatalf("task_started(run1): %v", err)
	}
	subStart("sub-1", base.Add(time.Second))
	if err := r.ApplyPlain(&events.ContentChunk{
		RunId: "sub-1", RequestId: "req-1", Content: "run 1 answer",
		Timestamp: base.Add(2 * time.Second), EventType: events.ContentChunkType,
	}); err != nil {
		t.Fatalf("content_chunk(run1): %v", err)
	}
	if err := r.ApplyPlain(&events.ToolStart{
		RunId: "sub-1", Id: "tool-1", Name: "bash",
		Timestamp: base.Add(3 * time.Second), EventType: events.ToolRunStartType,
	}); err != nil {
		t.Fatalf("tool_start(run1): %v", err)
	}
	subEnd("sub-1", 1000, base.Add(4*time.Second))
	deliver("R1", base.Add(5*time.Second))

	cards := agentCards()
	if len(cards) != 1 {
		t.Fatalf("run1 后应只有一张卡，得到 %d", len(cards))
	}
	if cards[0].DurMs != 1000 || cards[0].TaskResult != "R1" || !cards[0].DeliveredToMain {
		t.Fatalf("run1 卡结算错误: dur=%d result=%q delivered=%v", cards[0].DurMs, cards[0].TaskResult, cards[0].DeliveredToMain)
	}

	// —— 续跑：task_started 不许把已结算的旧卡翻回 running ——
	if err := r.ApplyPlain(&events.TaskStarted{
		TaskId: "task-resume", Name: "review", Timestamp: base.Add(10 * time.Second),
		EventType: events.TaskStartedType,
	}); err != nil {
		t.Fatalf("task_started(run2): %v", err)
	}
	cards = agentCards()
	if cards[0].Status != statusCompleted {
		t.Fatalf("task_started 把已完成的旧卡翻成了 %q（续跑应由 agent_start 新建卡）", cards[0].Status)
	}
	if cards[0].DurMs != 1000 || cards[0].TaskResult != "R1" {
		t.Fatalf("task_started 改写了旧卡: dur=%d result=%q", cards[0].DurMs, cards[0].TaskResult)
	}

	// —— run 2：新卡 + prompt 必须进新卡 ——
	subStart("sub-2", base.Add(11*time.Second))
	if err := r.ApplyPlain(&events.AgentUserMessage{
		TaskId: "task-resume", RunId: "sub-2", Text: "resume prompt for run 2",
		Timestamp: base.Add(12 * time.Second), EventType: events.AgentUserMessageType,
	}); err != nil {
		t.Fatalf("agent_user_message(run2): %v", err)
	}
	if err := r.ApplyPlain(&events.ContentChunk{
		RunId: "sub-2", RequestId: "req-2", Content: "run 2 answer",
		Timestamp: base.Add(13 * time.Second), EventType: events.ContentChunkType,
	}); err != nil {
		t.Fatalf("content_chunk(run2): %v", err)
	}
	if err := r.ApplyPlain(&events.ToolStart{
		RunId: "sub-2", Id: "tool-2", Name: "bash",
		Timestamp: base.Add(14 * time.Second), EventType: events.ToolRunStartType,
	}); err != nil {
		t.Fatalf("tool_start(run2): %v", err)
	}
	// 没有 RunId 的 inbox 消息（Poll 路径）：必须进最新卡，而不是第一张。
	if err := r.ApplyPlain(&events.AgentUserMessage{
		TaskId: "task-resume", Text: "inbox follow-up",
		Timestamp: base.Add(15 * time.Second), EventType: events.AgentUserMessageType,
	}); err != nil {
		t.Fatalf("agent_user_message(inbox): %v", err)
	}
	subEnd("sub-2", 2000, base.Add(16*time.Second))
	deliver("R2", base.Add(17*time.Second))

	cards = agentCards()
	if len(cards) != 2 {
		t.Fatalf("续跑后应有 2 张卡（run1/run2），得到 %d", len(cards))
	}
	old, cur := cards[0], cards[1]

	// 旧卡：自己的时长/结果/交付，不被 run2 的 task_end/delivery 改写。
	if old.DurMs != 1000 || old.TaskResult != "R1" || !old.DeliveredToMain {
		t.Fatalf("run2 改写了旧卡: dur=%d result=%q delivered=%v", old.DurMs, old.TaskResult, old.DeliveredToMain)
	}
	// 新卡：AgentEnd 的 2000ms 必须保留；task_end 不是用它自己的 ts-spawnedAt 覆盖。
	if cur.DurMs != 2000 {
		t.Fatalf("新卡时长被 task_end 覆盖: got %d want 2000", cur.DurMs)
	}
	if cur.TaskResult != "R2" || !cur.DeliveredToMain {
		t.Fatalf("新卡结果/交付错误: result=%q delivered=%v", cur.TaskResult, cur.DeliveredToMain)
	}

	// prompt 归属：run1 卡里没有 run2 的 prompt；run2 卡里有两条（RunId 精确 + 最新卡兜底）。
	for _, text := range itemTexts(old) {
		if text == "resume prompt for run 2" || text == "inbox follow-up" {
			t.Fatalf("续跑 prompt 写进了旧卡: %q", text)
		}
	}
	var gotPrompt, gotInbox bool
	for _, it := range cur.Items {
		if it.Kind != agentItemUser {
			continue
		}
		if it.Text == "resume prompt for run 2" {
			gotPrompt = it.RunID == "sub-2"
		}
		if it.Text == "inbox follow-up" {
			gotInbox = it.RunID == "sub-2"
		}
	}
	if !gotPrompt || !gotInbox {
		t.Fatalf("续跑 prompt 未进新卡或 run_id 错误: prompt=%v inbox=%v", gotPrompt, gotInbox)
	}

	// 每条 item 都带自己的 run_id（前端按 run 切段/结算的根数据）。
	for _, it := range old.Items {
		if it.RunID != "sub-1" {
			t.Fatalf("run1 卡里的 item 缺/错 run_id: kind=%s run=%q", it.Kind, it.RunID)
		}
	}
	for _, it := range cur.Items {
		if it.RunID != "sub-2" {
			t.Fatalf("run2 卡里的 item 缺/错 run_id: kind=%s run=%q", it.Kind, it.RunID)
		}
	}
}
