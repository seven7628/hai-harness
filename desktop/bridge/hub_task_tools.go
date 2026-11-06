package main

// hub_task_tools.go：Hub 智能体的任务查询/取消工具（方案 §6.3.1 / §6.7.6）。
//
// 与任务板命令共用同一账本与取消逻辑：task_status 只读，task_cancel 走
// cancelHubTask（queued 出队、dispatched/running 中断或空闲兜底）。
// 只允许操作本智能体发起的任务，避免用随机 id 读到别的智能体任务。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/tools"
)

func registerHubTaskTools(eng *tools.Engine, m *manager, agentID agenthub.AgentID) {
	if eng == nil || m == nil || agentID == "" {
		return
	}
	eng.RegisterTool(context.Background(), newHubTaskStatusTool(m, agentID))
	eng.RegisterTool(context.Background(), newHubTaskCancelTool(m, agentID))
}

type hubTaskToolArgs struct {
	TaskID string `json:"task_id"`
}

func parseHubTaskToolArgs(arguments string) (agenthub.TaskID, error) {
	var a hubTaskToolArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", fmt.Errorf("解析参数失败: %w", err)
	}
	id := agenthub.TaskID(strings.TrimSpace(a.TaskID))
	if id == "" {
		return "", fmt.Errorf("task_id 不能为空")
	}
	return id, nil
}

// hubTaskForAgent 读任务并校验归属；不属于本智能体的 id 一律 ErrNotFound，
// 不向外泄露「存在但不是你的」。
func (m *manager) hubTaskForAgent(ctx context.Context, id agenthub.TaskID, agentID agenthub.AgentID) (*agenthub.Task, error) {
	st, err := m.hubStore()
	if err != nil {
		return nil, err
	}
	t, err := st.LoadTask(ctx, id)
	if err != nil {
		return nil, err
	}
	if agentID != "" && t.Origin.AgentID != agentID {
		return nil, fmt.Errorf("%w: task %s", agenthub.ErrNotFound, id)
	}
	return t, nil
}

type hubTaskStatusTool struct {
	tools.BaseTool
	m       *manager
	agentID agenthub.AgentID
}

func newHubTaskStatusTool(m *manager, agentID agenthub.AgentID) *hubTaskStatusTool {
	return &hubTaskStatusTool{
		BaseTool: tools.BaseTool{
			Name_: "task_status",
			Description_: "Check one task you dispatched: current status, dispatch run_id, result/error, and its event log. " +
				"Use this after session_send (task_id returned) or when deciding whether to retry/cancel. " +
				"Only tasks originated by this agent are visible.",
			Params_: tools.Obj(map[string]any{
				"task_id": tools.Str("Task id returned by session_send"),
			}, "task_id"),
			CanParallel_: true,
			ReadOnly_:    true,
		},
		m: m, agentID: agentID,
	}
}

func (t *hubTaskStatusTool) ValidParams(_ context.Context, _, arguments string) error {
	_, err := parseHubTaskToolArgs(arguments)
	return err
}

func (t *hubTaskStatusTool) Call(ctx context.Context, _, arguments string) (string, error) {
	id, err := parseHubTaskToolArgs(arguments)
	if err != nil {
		return "", err
	}
	task, err := t.m.hubTaskForAgent(ctx, id, t.agentID)
	if err != nil {
		return "", err
	}
	st, err := t.m.hubStore()
	if err != nil {
		return "", err
	}
	events, err := st.ReadTaskEvents(ctx, id)
	if err != nil {
		return "", err
	}
	if events == nil {
		events = []agenthub.TaskEvent{}
	}
	return marshalHubToolResult(map[string]any{"task": task, "events": events})
}

type hubTaskCancelTool struct {
	tools.BaseTool
	m       *manager
	agentID agenthub.AgentID
}

func newHubTaskCancelTool(m *manager, agentID agenthub.AgentID) *hubTaskCancelTool {
	return &hubTaskCancelTool{
		BaseTool: tools.BaseTool{
			Name_: "task_cancel",
			Description_: "Cancel a task you dispatched. Queued tasks are removed before dispatch; " +
				"dispatched/running tasks interrupt the target session (or are canceled immediately if it is idle). " +
				"Returns the latest task snapshot and a note describing what happened.",
			Params_: tools.Obj(map[string]any{
				"task_id": tools.Str("Task id to cancel"),
			}, "task_id"),
			CanParallel_: false,
			ReadOnly_:    false,
		},
		m: m, agentID: agentID,
	}
}

func (t *hubTaskCancelTool) ValidParams(_ context.Context, _, arguments string) error {
	_, err := parseHubTaskToolArgs(arguments)
	return err
}

func (t *hubTaskCancelTool) Call(ctx context.Context, _, arguments string) (string, error) {
	id, err := parseHubTaskToolArgs(arguments)
	if err != nil {
		return "", err
	}
	if _, err := t.m.hubTaskForAgent(ctx, id, t.agentID); err != nil {
		return "", err
	}
	task, note, err := t.m.cancelHubTask(ctx, id)
	if err != nil {
		return "", err
	}
	return marshalHubToolResult(map[string]any{"task": task, "note": note})
}
