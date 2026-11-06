package agenthub

// workspace.go：Hub 与「工作区」之间的依赖倒置接口（方案 §3.2 / §6.6）。
//
// 为什么放在 agenthub 而不是 bridge：上层的路由/任务账本/护栏要在 agenthub 单测里
// 跑起来，不能为了「列一个工作区」去起整个 bridge。bridge 实现这些接口（
// desktop/bridge/hub_workspace.go），agenthub 只依赖行为契约。
//
// 信任边界（§9.1）：智能体只是**派发**任务，不能直接读写工作区文件；真正落盘由
// 工作区主会话执行。因此接口里所有路径参数都要在 bridge 侧做根目录校验
//（policy.workspace_roots + 敏感目录 + 符号链接归一），领域包不提供路径旁路。

import (
	"context"
	"time"
)

// WorkspaceInfo 一个可派发的工作区（工作区 = 用户已有的项目目录）。
type WorkspaceInfo struct {
	Path      string     `json:"path"`
	Name      string     `json:"name"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"` // nil = 未知（避免 0001-01-01 出现在工具输出里）
}

// CreateWorkspaceReq 新建工作区目录。
//
// 只允许在**智能体自己的** policy.workspace_roots 下新建；Parent 必须是已存在的
// 允许根（或其子目录），Name 是单段目录名。已存在的目录一律拒绝 —— 不接管用户目录。
type CreateWorkspaceReq struct {
	AgentID AgentID `json:"agent_id"`
	Parent  string  `json:"parent"`
	Name    string  `json:"name"`
}

// SessionInfo 工作区里的一个会话（Hub 只关心「派给谁/在跑什么」）。
type SessionInfo struct {
	ID        string `json:"id"`
	Workspace string `json:"workspace"`
	Title     string `json:"title"`
	Running   bool   `json:"running"`
	// Pending = Session.inbox 积压 + 未搬进 inbox 的恢复积压（空闲闸门的数据源）。
	Pending   int        `json:"pending"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// CreateSessionReq 在工作区里开会话（工作区主会话，不是 Hub 会话）。
type CreateSessionReq struct {
	AgentID   AgentID `json:"agent_id"`
	Workspace string  `json:"workspace"`
	Title     string  `json:"title,omitempty"`
}

// DispatchReq 把一条任务消息派给工作区的某个会话。
type DispatchReq struct {
	AgentID     AgentID `json:"agent_id"`
	Workspace   string  `json:"workspace"`
	SessionID   string  `json:"session_id"`
	Text        string  `json:"text"`
	ClientMsgID string  `json:"client_msg_id,omitempty"`
	// TaskHeader 非空 = 在正文前加一个 hub_task 块（EncodeTaskHeader 的 JSON）。
	// 目标会话的 UserInputsConsumed 据此绑定任务；正文仍是独立 text 块。
	TaskHeader string `json:"task_header,omitempty"`
}

// DispatchDuplicateMessage 是目标侧去重命中时的 Message（同一幂等键已投递过）。
const DispatchDuplicateMessage = "duplicate"

// DispatchResult 派发结果（P2-03 起 task 账本会引用这里的 run/session）。
type DispatchResult struct {
	SessionID string `json:"session_id"`
	Accepted  bool   `json:"accepted"`
	Message   string `json:"message,omitempty"`
}

// WorkspaceOps 是 Hub 对工作区的全部能力面（bridge 实现）。
type WorkspaceOps interface {
	ListWorkspaces(ctx context.Context) ([]WorkspaceInfo, error)
	CreateWorkspace(ctx context.Context, req CreateWorkspaceReq) (WorkspaceInfo, error)
	ListSessions(ctx context.Context, workspace string) ([]SessionInfo, error)
	// SessionStatus 单会话空闲闸门探针（Running/Pending）；不存在 → ErrNotFound。
	SessionStatus(ctx context.Context, workspace, sessionID string) (SessionInfo, error)
	CreateSession(ctx context.Context, req CreateSessionReq) (SessionInfo, error)
	Dispatch(ctx context.Context, req DispatchReq) (DispatchResult, error)
	InterruptSession(ctx context.Context, sid string) error
}

// SessionSpec / SessionHandle / SessionStatus 是 Hub 会话运行器的最小契约
// （P2 的回推与 P3 的群路由都要用；先定形状，bridge 在 P2-02 起实现）。
type SessionSpec struct {
	AgentID   AgentID `json:"agent_id"`
	SessionID string  `json:"session_id"`
	Scope     Scope   `json:"scope"`
}

type SessionHandle struct {
	SessionID string `json:"session_id"`
}

type SessionStatus struct {
	SessionID string `json:"session_id"`
	Running   bool   `json:"running"`
	Pending   int    `json:"pending"`
}

// SessionMessage 送进 Hub 会话的一条用户消息（领域包不 import core，保持窄依赖）。
type SessionMessage struct {
	Text        string `json:"text"`
	ClientMsgID string `json:"client_msg_id,omitempty"`
}

// SessionRunner 是 Hub 对自己会话的能力面。
type SessionRunner interface {
	Ensure(ctx context.Context, spec SessionSpec) (SessionHandle, error)
	Ask(ctx context.Context, sid string, msg SessionMessage) error
	Interrupt(ctx context.Context, sid string) error
	Status(ctx context.Context, sid string) (SessionStatus, error)
}
