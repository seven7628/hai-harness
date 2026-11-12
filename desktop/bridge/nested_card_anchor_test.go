package main

import (
	"testing"
	"time"

	"github.com/seven7628/hai-harness/events"
)

// TestAgentStartCarriesParentTaskID 钉住「卡片的归属锚随运行事件走」。
//
// 背景（2026-11 用户报障）：孙任务卡由 agent_start 建立，而父**任务** id 原先只挂在
// task_started 的占位卡上（events.AgentStart 没有这个字段）—— 前端在升级占位卡时没把
// 锚带过来，于是孙任务的车道长在主会话的时间线上，派发它的子智能体页面里反而空着。
//
// 判据：**故意不发 task_started**，只发带 ParentTaskId 的 agent_start —— 卡片仍必须带
// 父任务锚（不再依赖占位卡在不在窗口里）。
func TestAgentStartCarriesParentTaskID(t *testing.T) {
	r := NewViewReducer()
	if err := r.ApplyPlain(&events.AgentStart{
		RunId: "run-child", ParentRunId: "run-parent", ParentTaskId: "task-parent",
		Depth: 2, TaskId: "task-child", Name: "hygiene: interfaces B",
		Timestamp: time.Now(), EventType: events.AgentStartType,
	}); err != nil {
		t.Fatalf("agent_start: %v", err)
	}

	var card *Block
	for _, b := range r.Snapshot().Blocks {
		if b.Kind == blockKindAgent && b.TaskID == "task-child" {
			bb := b
			card = &bb
		}
	}
	if card == nil {
		t.Fatal("agent 卡片应已建立")
	}
	if card.ParentTaskID != "task-parent" {
		t.Errorf("Block.ParentTaskID = %q，期望 %q（否则孙卡在主转录里没有归属锚）",
			card.ParentTaskID, "task-parent")
	}
}

// TestAgentStartInheritsParentTaskIDFromPlaceholder 锁住旧事件的兼容路径：
// ParentTaskId 为空（字段加入之前的事件日志 / 旧 checkpoint 回放）时，仍从占位卡继承。
func TestAgentStartInheritsParentTaskIDFromPlaceholder(t *testing.T) {
	r := NewViewReducer()
	base := time.Now()
	if err := r.ApplyPlain(&events.TaskStarted{
		TaskId: "task-child", Name: "hygiene: interfaces B", ParentTaskId: "task-parent",
		Status: "running", Timestamp: base, EventType: events.TaskStartedType,
	}); err != nil {
		t.Fatalf("task_started: %v", err)
	}
	if err := r.ApplyPlain(&events.AgentStart{
		RunId: "run-child", ParentRunId: "run-parent", Depth: 2, TaskId: "task-child",
		Name: "hygiene: interfaces B", Timestamp: base.Add(time.Second), EventType: events.AgentStartType,
	}); err != nil {
		t.Fatalf("agent_start: %v", err)
	}

	var card *Block
	for _, b := range r.Snapshot().Blocks {
		if b.Kind == blockKindAgent && b.TaskID == "task-child" {
			bb := b
			card = &bb
		}
	}
	if card == nil {
		t.Fatal("agent 卡片应已建立")
	}
	if card.ParentTaskID != "task-parent" {
		t.Errorf("旧事件回退失败：Block.ParentTaskID = %q，期望从占位卡继承 %q",
			card.ParentTaskID, "task-parent")
	}
}
