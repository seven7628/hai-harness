package main

// hub_task_ack.go：群内智能体拒绝被指派的任务（task_ack，P3b-05，方案 §6.7.9）。
//
// 语义：assign_task 把任务派给 B 之后，B 的**第一次发言**可以 accept=true（继续做，
// 纯确认）或 accept=false（在开始工作前拒绝）。拒绝后任务落 declined，派发者 A
// 收到结果卡与 task_result 回推，一次，不自动重试。
//
// 只能拒绝「本会话这一轮绑定的任务」（hubTaskIDs，由 hub_task header 绑定），
// 且只有目标任务本人能拒绝；已经开始跑的任务（running）没有 declined 出边。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/tools"
)

func registerHubTaskAckTool(eng *tools.Engine, m *manager, agentID agenthub.AgentID, groupID agenthub.GroupID, sid string) {
	if eng == nil || m == nil || agentID == "" || groupID == "" {
		return
	}
	eng.RegisterTool(context.Background(), newHubTaskAckTool(m, agentID, groupID, sid))
}

type hubTaskAckArgs struct {
	GroupID string `json:"group_id"`
	Accept  *bool  `json:"accept"`
	Reason  string `json:"reason"`
}

func parseHubTaskAckArgs(arguments string) (hubTaskAckArgs, error) {
	var a hubTaskAckArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return a, fmt.Errorf("解析参数失败: %w", err)
	}
	if a.Accept == nil {
		// 默认 false 太危险：漏传 accept 会被当成「拒绝任务」。
		return a, fmt.Errorf("%w: accept 不能为空（true=接受，false=拒绝）", agenthub.ErrInvalid)
	}
	return a, nil
}

type hubTaskAckTool struct {
	tools.BaseTool
	m       *manager
	agentID agenthub.AgentID
	groupID agenthub.GroupID
	sid     string
}

func newHubTaskAckTool(m *manager, agentID agenthub.AgentID, groupID agenthub.GroupID, sid string) *hubTaskAckTool {
	return &hubTaskAckTool{
		BaseTool: tools.BaseTool{
			Name_: "task_ack",
			Description_: "Decline an assigned task before starting it; the dispatcher will be notified. " +
				"accept=true simply confirms you are working on the task. " +
				"accept=false transitions the current assigned task to declined with your reason and writes a " +
				"result card to the group; it cannot be used once work has started. " +
				"Only the agent the task was assigned to can call this.",
			Params_: tools.Obj(map[string]any{
				"group_id": tools.Str("Group id (defaults to this session's group)"),
				"accept":   tools.Bool("true = accept and work on it; false = decline before starting"),
				"reason":   tools.Str("Why you decline (shown to the dispatcher)"),
			}, "accept"),
			CanParallel_: false,
			ReadOnly_:    false,
		},
		m: m, agentID: agentID, groupID: groupID, sid: sid,
	}
}

func (t *hubTaskAckTool) ValidParams(_ context.Context, _, arguments string) error {
	_, err := parseHubTaskAckArgs(arguments)
	return err
}

func (t *hubTaskAckTool) Call(ctx context.Context, _, arguments string) (string, error) {
	args, err := parseHubTaskAckArgs(arguments)
	if err != nil {
		return "", err
	}
	if gid := strings.TrimSpace(args.GroupID); gid != "" && gid != string(t.groupID) {
		return "", fmt.Errorf("%w: group_id %q 不是本会话所在的群 %q", agenthub.ErrInvalid, gid, t.groupID)
	}
	task, err := t.m.hubTaskAckTarget(t.sid, t.agentID, t.groupID)
	if err != nil {
		return "", err
	}
	if *args.Accept {
		// 接受是纯确认：不迁移状态、不写卡、不回推（任务本来就在跑）。
		return marshalHubToolResult(map[string]any{"task": task, "acknowledged": true, "status": task.Status})
	}
	reason := strings.TrimSpace(args.Reason)
	if reason == "" {
		reason = "目标智能体拒绝了这个任务"
	}
	if task.Status == agenthub.TaskDeclined {
		return marshalHubToolResult(map[string]any{
			"task": task, "acknowledged": false, "status": task.Status, "note": "任务此前已被拒绝",
		})
	}
	switch task.Status {
	case agenthub.TaskRunning:
		return "", fmt.Errorf("%w: 任务已开始运行，不能拒绝；decline 只能在开始工作前使用（要用请 task_cancel）", agenthub.ErrInvalid)
	case agenthub.TaskQueued, agenthub.TaskDispatched:
		// 允许拒绝（queued：派发落账窗口里本轮已经拿到任务；dispatched：正常在跑）。
	default:
		return "", fmt.Errorf("%w: 任务 %s 状态 %q，不能拒绝", agenthub.ErrInvalid, task.ID, task.Status)
	}
	st, err := t.m.hubStore()
	if err != nil {
		return "", err
	}
	ledger := agenthub.NewLedger(st)
	declined, err := ledger.Transition(ctx, task.ID, agenthub.TaskDeclined, agenthub.TransitionOptions{
		At: time.Now().UTC(), Error: reason,
	})
	if err != nil {
		return "", fmt.Errorf("%w: 拒绝任务失败: %v", agenthub.ErrInvalid, err)
	}
	t.m.emitHubTaskUpdate(declined)
	// 与结算同一条通知路径：群内结果卡 + 只唤醒派发者（幂等，重复拒绝不会双写）。
	t.m.notifyAgentTaskResult(ledger, declined)
	return marshalHubToolResult(map[string]any{
		"task": declined, "acknowledged": false, "status": declined.Status, "reason": reason,
	})
}

// hubTaskAckTarget 解析调用方**当前绑定**的智能体任务。
// sid 是工具所属会话（group agent 会话），hubTaskIDs 由 hub_task header 绑定；
// 没有绑定 = 这不是一次被指派的运行。目标不匹配 = 不是你的任务（不泄露存在性）。
func (m *manager) hubTaskAckTarget(sid string, agentID agenthub.AgentID, groupID agenthub.GroupID) (*agenthub.Task, error) {
	if m == nil {
		return nil, fmt.Errorf("%w: manager 不可用", agenthub.ErrInvalid)
	}
	bs := m.sessionByID(sid)
	if bs == nil {
		return nil, fmt.Errorf("%w: 当前会话未装配，无法确认任务", agenthub.ErrInvalid)
	}
	id := bs.currentHubTaskID()
	if id == "" {
		return nil, fmt.Errorf("%w: 当前这一轮没有绑定任务（task_ack 只用于被指派任务的运行）", agenthub.ErrInvalid)
	}
	st, err := m.hubStore()
	if err != nil {
		return nil, err
	}
	task, err := st.LoadTask(context.Background(), id)
	if err != nil {
		return nil, err
	}
	if task.Target.AgentID != agentID || (task.Target.GroupID != "" && task.Target.GroupID != groupID) {
		return nil, fmt.Errorf("%w: 只有该任务的目标智能体可以 ack", agenthub.ErrInvalid)
	}
	return task, nil
}
