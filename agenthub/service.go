package agenthub

// service.go：Hub 对「工作区/会话」的服务层（方案 §6.6 / §7.5）。
//
// 工具层（desktop/bridge/hub_workspace_tools.go）只做参数解析与 JSON 渲染，
// 真正的校验/派发都走这里：可以在 agenthub 单测里用假 WorkspaceOps 跑起来，
// 不必起 bridge。P2-03 的任务账本会在这里绑定 task_id（现在先返回一次性 id）。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// SendResult session_send 的返回：task_id 供后续 task_status/task_result 引用。
type SendResult struct {
	TaskID    string `json:"task_id"`
	SessionID string `json:"session_id"`
	Accepted  bool   `json:"accepted"`
	Message   string `json:"message,omitempty"`
}

// WorkspaceService 是工具层的能力面（组合 WorkspaceOps，注入 AgentID）。
type WorkspaceService struct {
	Ops     WorkspaceOps
	AgentID AgentID
	// NewTaskID 可注入（测试/账本）；nil = 进程内随机 task_<hex>。
	NewTaskID func() string
}

func NewWorkspaceService(ops WorkspaceOps, agentID AgentID) *WorkspaceService {
	return &WorkspaceService{Ops: ops, AgentID: agentID}
}

func (s *WorkspaceService) ops() (WorkspaceOps, error) {
	if s == nil || s.Ops == nil {
		return nil, fmt.Errorf("%w: workspace ops 未配置", ErrInvalid)
	}
	return s.Ops, nil
}

// ListWorkspaces 列出可派发的项目工作区。
func (s *WorkspaceService) ListWorkspaces(ctx context.Context) ([]WorkspaceInfo, error) {
	ops, err := s.ops()
	if err != nil {
		return nil, err
	}
	return ops.ListWorkspaces(ctx)
}

// CreateWorkspace 在 workspace_roots 下新建工作区并登记。
func (s *WorkspaceService) CreateWorkspace(ctx context.Context, parent, name string) (WorkspaceInfo, error) {
	ops, err := s.ops()
	if err != nil {
		return WorkspaceInfo{}, err
	}
	parent = strings.TrimSpace(parent)
	name = strings.TrimSpace(name)
	if parent == "" {
		return WorkspaceInfo{}, fmt.Errorf("%w: parent 不能为空（必须是 policy.workspace_roots 下的目录）", ErrInvalid)
	}
	if name == "" {
		return WorkspaceInfo{}, fmt.Errorf("%w: name 不能为空", ErrInvalid)
	}
	return ops.CreateWorkspace(ctx, CreateWorkspaceReq{AgentID: s.AgentID, Parent: parent, Name: name})
}

// ListSessions 列出工作区里的会话（调用前应先用 workspace_list 找到工作区）。
func (s *WorkspaceService) ListSessions(ctx context.Context, workspace string) ([]SessionInfo, error) {
	ops, err := s.ops()
	if err != nil {
		return nil, err
	}
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return nil, fmt.Errorf("%w: workspace 不能为空（先 workspace_list 再 session_list）", ErrInvalid)
	}
	return ops.ListSessions(ctx, workspace)
}

// CreateSession 在工作区里开一个项目会话。
func (s *WorkspaceService) CreateSession(ctx context.Context, workspace, title string) (SessionInfo, error) {
	ops, err := s.ops()
	if err != nil {
		return SessionInfo{}, err
	}
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return SessionInfo{}, fmt.Errorf("%w: workspace 不能为空（先 workspace_list 再 session_create）", ErrInvalid)
	}
	return ops.CreateSession(ctx, CreateSessionReq{
		AgentID: s.AgentID, Workspace: workspace, Title: strings.TrimSpace(title),
	})
}

// Send 把任务派给项目会话。目标必须是已登记项目工作区里的会话（WorkspaceOps 负责校验）。
func (s *WorkspaceService) Send(ctx context.Context, workspace, sessionID, text, clientMsgID string) (SendResult, error) {
	ops, err := s.ops()
	if err != nil {
		return SendResult{}, err
	}
	workspace = strings.TrimSpace(workspace)
	sessionID = strings.TrimSpace(sessionID)
	text = strings.TrimSpace(text)
	if workspace == "" {
		return SendResult{}, fmt.Errorf("%w: workspace 不能为空（先 session_list 再 session_send）", ErrInvalid)
	}
	if sessionID == "" {
		return SendResult{}, fmt.Errorf("%w: session_id 不能为空（先 session_list 再 session_send）", ErrInvalid)
	}
	if text == "" {
		return SendResult{}, fmt.Errorf("%w: text 不能为空", ErrInvalid)
	}
	res, err := ops.Dispatch(ctx, DispatchReq{
		AgentID: s.AgentID, Workspace: workspace, SessionID: sessionID,
		Text: text, ClientMsgID: strings.TrimSpace(clientMsgID),
	})
	if err != nil {
		return SendResult{}, err
	}
	taskID := ""
	if s.NewTaskID != nil {
		taskID = s.NewTaskID()
	} else {
		taskID = defaultTaskID()
	}
	return SendResult{
		TaskID: taskID, SessionID: res.SessionID, Accepted: res.Accepted, Message: res.Message,
	}, nil
}

// defaultTaskID 生成一次性任务 id（P2-03 起改由账本分配并持久化）。
func defaultTaskID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "task_unknown"
	}
	return "task_" + hex.EncodeToString(b[:])
}
